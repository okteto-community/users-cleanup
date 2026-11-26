package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/okteto/users-cleanup/app/cleanup"
	"github.com/okteto/users-cleanup/app/demo"
	"github.com/okteto/users-cleanup/app/kube"
	"github.com/okteto/users-cleanup/app/okteto"
	"github.com/okteto/users-cleanup/app/service"
	"github.com/okteto/users-cleanup/app/store"
	"github.com/okteto/users-cleanup/app/web"
)

// keepAwake reads the persistent mark from Kubernetes and changes it through Okteto.
type keepAwake struct {
	*kube.Inventory
	*okteto.Client
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("users-cleanup stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	inactiveDays, err := intEnv("INACTIVE_DAYS", 90)
	if err != nil || inactiveDays < 1 {
		return errors.New("INACTIVE_DAYS must be a positive number of days")
	}
	port := envOr("PORT", "8080")
	dataDir := envOr("DATA_DIR", "/data")

	info := web.Info{
		Cluster:      os.Getenv("OKTETO_CONTEXT"),
		Namespace:    os.Getenv("OKTETO_NAMESPACE"),
		InactiveDays: inactiveDays,
	}
	engine := &cleanup.Engine{InactiveDays: inactiveDays}
	var awake service.KeepAwake

	if os.Getenv("DEMO") == "true" {
		cluster := demo.New()
		engine.Users, engine.Inventory, awake = cluster, cluster, cluster
		info.Demo, info.Cluster, info.Namespace = true, "demo.okteto.example", "users-cleanup"
		if dataDir == "/data" {
			dataDir = filepath.Join(os.TempDir(), "users-cleanup-demo")
		}
	} else {
		// OKTETO_CONTEXT and OKTETO_TOKEN are injected by Okteto in the pods with the
		// "dev.okteto.com/inject-token" annotation. The token acts as the owner of the namespace.
		token := os.Getenv("OKTETO_TOKEN")
		if info.Cluster == "" || token == "" || info.Namespace == "" {
			return errors.New("OKTETO_CONTEXT, OKTETO_TOKEN and OKTETO_NAMESPACE are required: deploy this service in Okteto with the 'dev.okteto.com/inject-token' annotation, or set DEMO=true to try it locally")
		}
		client := okteto.New(info.Cluster, token)
		inventory := &kube.Inventory{KubeconfigPath: envOr("KUBECONFIG_PATH", filepath.Join(os.TempDir(), "users-cleanup", "kubeconfig"))}
		engine.Users, engine.Inventory, awake = client, inventory, keepAwake{inventory, client}
	}

	st, err := store.New(dataDir)
	if err != nil {
		return err
	}
	manager, err := service.New(engine, st, awake, info.Namespace, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The first plan is ready by the time the admin opens the page.
	if err := manager.StartScan(); err != nil {
		logger.Warn("could not start the first scan", "err", err)
	}
	go manager.RunScheduler(ctx, 30*time.Second)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           (&web.Server{Manager: manager, Info: info, Logger: logger}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	logger.Info("users-cleanup started", "port", port, "cluster", info.Cluster, "namespace", info.Namespace, "inactiveDays", inactiveDays, "demo", info.Demo)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func intEnv(name string, fallback int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}

package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/okteto/users-cleanup/app/cleanup"
	"github.com/okteto/users-cleanup/app/demo"
	"github.com/okteto/users-cleanup/app/service"
	"github.com/okteto/users-cleanup/app/store"
)

func newServer(t *testing.T) (*httptest.Server, *service.Manager) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cluster := demo.New()
	m, err := service.New(&cleanup.Engine{Users: cluster, Inventory: cluster, InactiveDays: 90}, st, cluster, "ns", logger)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&Server{Manager: m, Info: Info{Cluster: "https://okteto.example", Namespace: "ns", InactiveDays: 90}, Logger: logger}).Handler())
	t.Cleanup(srv.Close)
	return srv, m
}

func post(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func waitIdle(t *testing.T, m *service.Manager) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for m.Status().Busy != nil {
		if time.Now().After(deadline) {
			t.Fatal("still busy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestActionsRequireTheHeaderOfThePage(t *testing.T) {
	srv, m := newServer(t)

	if res := post(t, srv.URL+"/api/run", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("without the header: got %d", res.StatusCode)
	}
	if res := post(t, srv.URL+"/api/run", "", map[string]string{actionHeader: "1", "Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Errorf("from another site: got %d", res.StatusCode)
	}
	if n := len(m.Status().History); n != 0 || m.Status().Busy != nil {
		t.Fatalf("a rejected request must not start anything: %d runs", n)
	}
}

func TestRunAndReport(t *testing.T) {
	srv, m := newServer(t)

	if res := post(t, srv.URL+"/api/run", "", map[string]string{actionHeader: "1", "Origin": srv.URL}); res.StatusCode != http.StatusAccepted {
		t.Fatalf("got %d", res.StatusCode)
	}
	if res := post(t, srv.URL+"/api/scan", "", map[string]string{actionHeader: "1"}); res.StatusCode != http.StatusConflict {
		t.Errorf("a second operation must be rejected while one is running: got %d", res.StatusCode)
	}
	waitIdle(t, m)

	res, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var status struct {
		Info    Info                 `json:"info"`
		History []service.RunSummary `json:"history"`
	}
	if err := json.NewDecoder(res.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Info.InactiveDays != 90 || len(status.History) != 1 || status.History[0].Totals.Deleted == 0 {
		t.Fatalf("unexpected status: %+v", status)
	}

	res, err = http.Get(srv.URL + "/api/runs/" + status.History[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var run cleanup.Run
	if err := json.NewDecoder(res.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if len(run.Deleted) != status.History[0].Totals.Deleted || len(run.Blocked) == 0 {
		t.Errorf("unexpected report: %+v", run.Totals)
	}

	if res, _ := http.Get(srv.URL + "/api/runs/..%2F..%2Fstate"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown runs are not found: got %d", res.StatusCode)
	}
}

func TestSchedule(t *testing.T) {
	srv, m := newServer(t)
	res := post(t, srv.URL+"/api/schedule", `{"enabled":true,"weekday":5,"hour":22}`, map[string]string{actionHeader: "1"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("got %d", res.StatusCode)
	}
	if s := m.Status().Schedule; !s.Enabled || s.Weekday != 5 || s.Hour != 22 || s.KeepAwake == nil || !*s.KeepAwake {
		t.Errorf("unexpected schedule: %+v", s)
	}
	if res := post(t, srv.URL+"/api/schedule", `{"enabled":true,"weekday":9,"hour":22}`, map[string]string{actionHeader: "1"}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid schedule: got %d", res.StatusCode)
	}
}

func TestPage(t *testing.T) {
	srv, _ := newServer(t)
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Users Cleanup") {
		t.Errorf("got %d", res.StatusCode)
	}

	icon, err := http.Get(srv.URL + "/favicon.png")
	if err != nil {
		t.Fatal(err)
	}
	defer icon.Body.Close()
	if icon.StatusCode != http.StatusOK || icon.Header.Get("Content-Type") != "image/png" || !strings.Contains(string(body), `href="/favicon.png"`) {
		t.Errorf("the page must link an icon that is served: got %d %s", icon.StatusCode, icon.Header.Get("Content-Type"))
	}
}

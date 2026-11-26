// Package kube reads namespaces and their resources from the Kubernetes API,
// using the kubeconfig that Okteto generates for the owner of this namespace.
package kube

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/okteto/users-cleanup/app/cleanup"
)

const (
	ownerLabel       = "dev.okteto.com/owner"
	memberPrefix     = "dev.okteto.com/member-"
	typeLabel        = "space.okteto.com/type"
	defaultLabel     = "dev.okteto.com/default-namespace"
	persistentKey    = "dev.okteto.com/persistent"
	oktetoAppLabel   = "dev.okteto.com/git-deploy=true"
	oktetoAppPrefix  = "okteto-git-"
	helmLabel        = "owner=helm"
	helmReleaseLabel = "name"
	pageSize         = 500
)

// kind is a type of resource that counts as "the user has something here".
// The objects that Okteto creates in every namespace (service accounts, quotas,
// network policies, registry secrets...) are deliberately not in the list.
type kind struct {
	gvr      schema.GroupVersionResource
	name     string
	selector string
	// skipOwned ignores the objects created by another object (the pods of a
	// deployment, the jobs of a cron job), which is already reported.
	skipOwned bool
	// display turns an object into the name shown in the report.
	display func(meta *metav1.PartialObjectMetadata) string
}

var kinds = []kind{
	{
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}, name: "Okteto app", selector: oktetoAppLabel,
		display: func(m *metav1.PartialObjectMetadata) string { return strings.TrimPrefix(m.Name, oktetoAppPrefix) },
	},
	{
		gvr: schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, name: "Helm release", selector: helmLabel,
		display: func(m *metav1.PartialObjectMetadata) string {
			if release := m.Labels[helmReleaseLabel]; release != "" {
				return release
			}
			return m.Name
		},
	},
	{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, name: "Deployment"},
	{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, name: "StatefulSet"},
	{gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, name: "DaemonSet"},
	{gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, name: "CronJob"},
	{gvr: schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, name: "Job", skipOwned: true},
	{gvr: schema.GroupVersionResource{Version: "v1", Resource: "pods"}, name: "Pod", skipOwned: true},
	{gvr: schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}, name: "Volume", skipOwned: true},
	{gvr: schema.GroupVersionResource{Version: "v1", Resource: "services"}, name: "Service"},
	{gvr: schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, name: "Ingress", skipOwned: true},
}

// Inventory implements cleanup.Inventory on top of the Kubernetes API.
type Inventory struct {
	// KubeconfigPath is where the kubeconfig is stored.
	KubeconfigPath string
	// Download fetches the kubeconfig. It defaults to running "okteto kubeconfig".
	Download func(ctx context.Context, path string) error

	mu       sync.Mutex
	core     kubernetes.Interface
	metadata metadata.Interface
}

// OktetoKubeconfig downloads the kubeconfig of the user the Okteto CLI is
// authenticated as. Inside a pod with the "dev.okteto.com/inject-token"
// annotation that is the owner of the namespace. The kubeconfig has no static
// credentials: it runs "okteto kubetoken" to get short-lived tokens.
func OktetoKubeconfig(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "okteto", "kubeconfig")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("downloading the kubeconfig with 'okteto kubeconfig': %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// clients returns the Kubernetes clients, downloading the kubeconfig on first use.
// A failure is not cached: the next call tries again.
func (i *Inventory) clients(ctx context.Context) (kubernetes.Interface, metadata.Interface, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.core != nil {
		return i.core, i.metadata, nil
	}

	download := i.Download
	if download == nil {
		download = OktetoKubeconfig
	}
	if err := download(ctx, i.KubeconfigPath); err != nil {
		return nil, nil, err
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: i.KubeconfigPath},
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("loading the kubeconfig: %w", err)
	}
	cfg.QPS = 50
	cfg.Burst = 100
	cfg.Timeout = 60 * time.Second

	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating the Kubernetes client: %w", err)
	}
	meta, err := metadata.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("creating the Kubernetes client: %w", err)
	}
	i.core, i.metadata = core, meta
	return core, meta, nil
}

// OwnedNamespaces returns every Okteto namespace grouped by the ID of its owner.
func (i *Inventory) OwnedNamespaces(ctx context.Context) (map[string][]cleanup.Namespace, error) {
	result := map[string][]cleanup.Namespace{}
	err := i.listNamespaces(ctx, ownerLabel, func(owner string, ns cleanup.Namespace) {
		result[owner] = append(result[owner], ns)
	})
	return result, err
}

// NamespacesOf returns the namespaces owned by one user.
func (i *Inventory) NamespacesOf(ctx context.Context, userID string) ([]cleanup.Namespace, error) {
	var result []cleanup.Namespace
	err := i.listNamespaces(ctx, ownerLabel+"="+userID, func(_ string, ns cleanup.Namespace) {
		result = append(result, ns)
	})
	return result, err
}

func (i *Inventory) listNamespaces(ctx context.Context, selector string, add func(owner string, ns cleanup.Namespace)) error {
	core, _, err := i.clients(ctx)
	if err != nil {
		return err
	}

	opts := metav1.ListOptions{LabelSelector: selector, Limit: pageSize}
	for {
		list, err := core.CoreV1().Namespaces().List(ctx, opts)
		if err != nil {
			return err
		}
		for _, n := range list.Items {
			owner := n.Labels[ownerLabel]
			if owner == "" {
				continue
			}
			ns := cleanup.Namespace{
				Name:     n.Name,
				Type:     n.Labels[typeLabel],
				Personal: n.Labels[defaultLabel] == "true",
			}
			for label, value := range n.Labels {
				if member, ok := strings.CutPrefix(label, memberPrefix); ok && value == "true" && member != owner {
					ns.OtherMembers++
				}
			}
			add(owner, ns)
		}
		if list.Continue == "" {
			return nil
		}
		opts.Continue = list.Continue
	}
}

// Resources returns what a user left in a namespace.
func (i *Inventory) Resources(ctx context.Context, namespace string) ([]cleanup.Resource, error) {
	_, meta, err := i.clients(ctx)
	if err != nil {
		return nil, err
	}

	var resources []cleanup.Resource
	seen := map[cleanup.Resource]bool{}
	for _, k := range kinds {
		opts := metav1.ListOptions{LabelSelector: k.selector, Limit: pageSize}
		for {
			list, err := meta.Resource(k.gvr).Namespace(namespace).List(ctx, opts)
			if apierrors.IsNotFound(err) {
				// The cluster does not serve this kind of resource.
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing %s: %w", k.gvr.Resource, err)
			}
			for idx := range list.Items {
				item := &list.Items[idx]
				if k.skipOwned && len(item.OwnerReferences) > 0 {
					continue
				}
				name := item.Name
				if k.display != nil {
					name = k.display(item)
				}
				r := cleanup.Resource{Kind: k.name, Name: name}
				if !seen[r] {
					seen[r] = true
					resources = append(resources, r)
				}
			}
			if list.Continue == "" {
				break
			}
			opts.Continue = list.Continue
		}
	}
	return resources, nil
}

// IsPersistent reports whether a namespace is marked as persistent ("Keep awake") in Okteto.
func (i *Inventory) IsPersistent(ctx context.Context, namespace string) (bool, error) {
	core, _, err := i.clients(ctx)
	if err != nil {
		return false, err
	}
	ns, err := core.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	_, inLabels := ns.Labels[persistentKey]
	_, inAnnotations := ns.Annotations[persistentKey]
	return inLabels || inAnnotations, nil
}

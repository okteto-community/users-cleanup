package kube

import (
	"context"
	"reflect"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/okteto/users-cleanup/app/cleanup"
)

func namespace(name string, labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func object(apiVersion, kind, namespace, name string, labels map[string]string, owned bool) *metav1.PartialObjectMetadata {
	o := &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
	}
	if owned {
		o.OwnerReferences = []metav1.OwnerReference{{Name: "parent"}}
	}
	return o
}

func TestNamespaces(t *testing.T) {
	inv := &Inventory{core: kubefake.NewSimpleClientset(
		namespace("ada", map[string]string{ownerLabel: "u1", memberPrefix + "u1": "true", defaultLabel: "true", typeLabel: "development"}),
		namespace("team", map[string]string{ownerLabel: "u1", memberPrefix + "u1": "true", memberPrefix + "u2": "true", memberPrefix + "u3": "true"}),
		namespace("preview-1", map[string]string{ownerLabel: "u2", memberPrefix + "u2": "true", typeLabel: "preview"}),
		namespace("kube-system", nil),
	)}

	owned, err := inv.OwnedNamespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(owned["u1"], func(a, b int) bool { return owned["u1"][a].Name < owned["u1"][b].Name })
	want := map[string][]cleanup.Namespace{
		"u1": {{Name: "ada", Type: "development", Personal: true}, {Name: "team", OtherMembers: 2}},
		"u2": {{Name: "preview-1", Type: "preview"}},
	}
	if !reflect.DeepEqual(owned, want) {
		t.Errorf("got %+v\nwant %+v", owned, want)
	}

	mine, err := inv.NamespacesOf(context.Background(), "u2")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mine, want["u2"]) {
		t.Errorf("got %+v, want %+v", mine, want["u2"])
	}
}

func TestResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	inv := &Inventory{
		core: kubefake.NewSimpleClientset(),
		metadata: metadatafake.NewSimpleMetadataClient(scheme,
			object("apps/v1", "Deployment", "busy", "api", nil, false),
			object("v1", "Pod", "busy", "api-123", nil, true),
			object("v1", "Pod", "busy", "debug", nil, false),
			object("batch/v1", "CronJob", "busy", "nightly", nil, false),
			object("batch/v1", "Job", "busy", "nightly-1", nil, true),
			object("v1", "PersistentVolumeClaim", "busy", "data", nil, false),
			object("v1", "ConfigMap", "busy", "okteto-git-movies", map[string]string{"dev.okteto.com/git-deploy": "true"}, false),
			object("v1", "ConfigMap", "busy", "kube-root-ca.crt", nil, false),
			object("v1", "Secret", "busy", "sh.helm.release.v1.redis.v1", map[string]string{"owner": "helm", "name": "redis"}, false),
			object("v1", "Secret", "busy", "sh.helm.release.v1.redis.v2", map[string]string{"owner": "helm", "name": "redis"}, false),
			object("v1", "Secret", "busy", "okteto-regcred", nil, false),
			object("apps/v1", "Deployment", "other", "elsewhere", nil, false),
		),
	}

	got, err := inv.Resources(context.Background(), "busy")
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(a, b int) bool { return got[a].Kind+got[a].Name < got[b].Kind+got[b].Name })
	want := []cleanup.Resource{
		{Kind: "CronJob", Name: "nightly"},
		{Kind: "Deployment", Name: "api"},
		{Kind: "Helm release", Name: "redis"},
		{Kind: "Okteto app", Name: "movies"},
		{Kind: "Pod", Name: "debug"},
		{Kind: "Volume", Name: "data"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}

	empty, err := inv.Resources(context.Background(), "empty")
	if err != nil || len(empty) != 0 {
		t.Errorf("an empty namespace has no resources: %v %v", empty, err)
	}
}

func TestIsPersistent(t *testing.T) {
	inv := &Inventory{core: kubefake.NewSimpleClientset(
		namespace("awake", map[string]string{persistentKey: "true"}),
		namespace("sleepy", nil),
	)}
	for name, want := range map[string]bool{"awake": true, "sleepy": false} {
		got, err := inv.IsPersistent(context.Background(), name)
		if err != nil || got != want {
			t.Errorf("%s: got %v %v, want %v", name, got, err, want)
		}
	}
}

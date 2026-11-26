// Package demo is an in-memory Okteto with made-up users. It lets you try the
// service on your machine (DEMO=true) without touching a real cluster.
package demo

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/okteto/users-cleanup/app/cleanup"
)

// Cluster implements the Okteto API, the inventory and the keep-awake switch.
type Cluster struct {
	mu         sync.Mutex
	users      []cleanup.User
	namespaces map[string][]cleanup.Namespace
	resources  map[string][]cleanup.Resource
	persistent bool
}

type seed struct {
	name       string
	role       string
	lastSeen   int // days ago; -1 means never
	created    int // days ago
	resources  []cleanup.Resource
	extra      string // name of a second namespace
	extraOther int    // other members of the second namespace
	extraRes   []cleanup.Resource
}

// New returns a cluster with a mix of active, deletable and blocked users.
func New() *Cluster {
	app := func(kind, name string) cleanup.Resource { return cleanup.Resource{Kind: kind, Name: name} }
	seeds := []seed{
		{name: "Ada Lovelace", role: "Admin", lastSeen: 0, created: 900},
		{name: "Grace Hopper", role: "Admin", lastSeen: 212, created: 800},
		{name: "Linus Torvalds", lastSeen: 3, created: 400, resources: []cleanup.Resource{app("Okteto app", "movies"), app("Deployment", "api")}},
		{name: "Margaret Hamilton", lastSeen: 41, created: 600},
		{name: "Dennis Ritchie", lastSeen: 95, created: 700},
		{name: "Barbara Liskov", lastSeen: 130, created: 500},
		{name: "Ken Thompson", lastSeen: 301, created: 950, extra: "ken-sandbox"},
		{name: "Radia Perlman", lastSeen: -1, created: 200},
		{name: "Donald Knuth", lastSeen: -1, created: 20},
		{name: "Edsger Dijkstra", lastSeen: 180, created: 720, resources: []cleanup.Resource{
			app("Okteto app", "shortest-path"), app("Deployment", "frontend"), app("Deployment", "worker"),
			app("StatefulSet", "postgres"), app("Volume", "data-postgres-0"), app("Service", "frontend"), app("Service", "postgres"),
		}},
		{name: "Frances Allen", lastSeen: 400, created: 1000, resources: []cleanup.Resource{app("Volume", "old-backup")}},
		{name: "Tim Berners-Lee", lastSeen: 150, created: 650, extra: "team-web", extraOther: 3},
		{name: "Alan Kay", lastSeen: 12, created: 300},
		{name: "John Backus", lastSeen: 97, created: 480},
		{name: "Shafi Goldwasser", lastSeen: 60, created: 365, resources: []cleanup.Resource{app("Helm release", "redis")}},
		{name: "Leslie Lamport", lastSeen: 260, created: 880,
			resources: []cleanup.Resource{app("CronJob", "nightly-report"), app("Deployment", "paxos")},
			extra:     "paxos-staging",
			extraRes:  []cleanup.Resource{app("Okteto app", "paxos"), app("StatefulSet", "etcd"), app("Volume", "data-etcd-0")}},
	}

	c := &Cluster{namespaces: map[string][]cleanup.Namespace{}, resources: map[string][]cleanup.Resource{}}
	now := time.Now().UTC()
	for i, s := range seeds {
		slug := slugify(s.name)
		id := fmt.Sprintf("user-%02d", i+1)
		created := now.AddDate(0, 0, -s.created)
		u := cleanup.User{ID: id, ExternalID: slug, Name: s.name, Email: slug + "@example.com", Role: "Developer", DefaultNamespace: slug, CreationDate: &created}
		if s.role != "" {
			u.Role = s.role
		}
		if s.lastSeen >= 0 {
			seen := now.AddDate(0, 0, -s.lastSeen).Add(-3 * time.Hour)
			u.LastSeen = &seen
		}
		c.users = append(c.users, u)

		c.namespaces[id] = []cleanup.Namespace{{Name: slug, Type: "development", Personal: true}}
		c.resources[slug] = s.resources
		if s.extra != "" {
			c.namespaces[id] = append(c.namespaces[id], cleanup.Namespace{Name: s.extra, Type: "development", OtherMembers: s.extraOther})
			c.resources[s.extra] = s.extraRes
		}
	}
	return c
}

func slugify(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		case r >= 'a' && r <= 'z':
			out = append(out, r)
		case r == ' ' || r == '-':
			out = append(out, '-')
		}
	}
	return string(out)
}

func (c *Cluster) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(60 * time.Millisecond):
	}
}

// ListUsers returns the users that have not been deleted.
func (c *Cluster) ListUsers(ctx context.Context) ([]cleanup.User, error) {
	c.pause(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cleanup.User{}, c.users...), nil
}

// DeleteUser removes a user and their namespaces.
func (c *Cluster) DeleteUser(ctx context.Context, id string) error {
	c.pause(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, u := range c.users {
		if u.ID == id {
			c.users = append(c.users[:i], c.users[i+1:]...)
			delete(c.namespaces, id)
			return nil
		}
	}
	return fmt.Errorf("User '%s' not found (HTTP 404)", id)
}

// OwnedNamespaces returns the namespaces of every user.
func (c *Cluster) OwnedNamespaces(ctx context.Context) (map[string][]cleanup.Namespace, error) {
	c.pause(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]cleanup.Namespace{}
	for id, list := range c.namespaces {
		out[id] = append([]cleanup.Namespace{}, list...)
	}
	return out, nil
}

// NamespacesOf returns the namespaces of a user.
func (c *Cluster) NamespacesOf(ctx context.Context, userID string) ([]cleanup.Namespace, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cleanup.Namespace{}, c.namespaces[userID]...), nil
}

// Resources returns the resources of a namespace.
func (c *Cluster) Resources(ctx context.Context, namespace string) ([]cleanup.Resource, error) {
	c.pause(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cleanup.Resource{}, c.resources[namespace]...), nil
}

// IsPersistent reports whether the demo namespace is persistent.
func (c *Cluster) IsPersistent(context.Context, string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persistent, nil
}

// SetPersistent marks the demo namespace as persistent.
func (c *Cluster) SetPersistent(_ context.Context, _ string, persistent bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistent = persistent
	return nil
}

package cleanup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// maxResourcesPerNamespace caps the resources kept per namespace in a report.
	maxResourcesPerNamespace = 40
	inspectWorkers           = 8
)

// Engine scans for inactive users and deletes the ones that are safe to delete.
type Engine struct {
	Users        UsersAPI
	Inventory    Inventory
	InactiveDays int
	// Now is replaceable in tests.
	Now func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

// Scan classifies every user without changing anything.
func (e *Engine) Scan(ctx context.Context, progress func(Progress)) (*Snapshot, error) {
	if progress == nil {
		progress = func(Progress) {}
	}

	progress(Progress{Phase: "Listing users"})
	users, err := e.Users.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}

	now := e.now()
	snap := &Snapshot{
		GeneratedAt:  now,
		InactiveDays: e.InactiveDays,
		Cutoff:       now.AddDate(0, 0, -e.InactiveDays),
		Users:        make([]UserReport, 0, len(users)),
	}
	for _, u := range users {
		snap.Users = append(snap.Users, classifyActivity(u, now, snap.Cutoff))
	}

	inactive := make([]*UserReport, 0)
	for i := range snap.Users {
		if snap.Users[i].Status != StatusActive {
			inactive = append(inactive, &snap.Users[i])
		}
	}

	if len(inactive) > 0 {
		progress(Progress{Phase: "Listing namespaces"})
		owned, err := e.Inventory.OwnedNamespaces(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing namespaces: %w", err)
		}
		for _, u := range inactive {
			u.Namespaces = owned[u.ID]
		}
		e.inspect(ctx, inactive, progress)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	sortUsers(snap.Users)
	snap.Totals = totals(snap.Users)
	return snap, nil
}

// classifyActivity decides whether a user is active, looking only at dates and role.
func classifyActivity(u User, now, cutoff time.Time) UserReport {
	r := UserReport{User: u, Status: StatusActive, InactiveDays: -1}

	reference := u.LastSeen
	if reference == nil {
		r.NeverSeen = true
		reference = u.CreationDate
	}
	if reference == nil {
		// Without dates there is no way to tell: never delete on a guess.
		r.addReason(ReasonNoActivityData)
	} else {
		r.InactiveDays = int(now.Sub(*reference).Hours() / 24)
		if r.InactiveDays < 0 {
			r.InactiveDays = 0
		}
		if reference.Before(cutoff) {
			r.Status = StatusDeletable
		}
	}

	if r.Status != StatusActive && u.Role == RoleAdmin {
		r.addReason(ReasonAdmin)
	}
	return r
}

// inspect fills the resources of the namespaces of each user and blocks the users
// who own something that the deletion would destroy.
func (e *Engine) inspect(ctx context.Context, users []*UserReport, progress func(Progress)) {
	total := len(users)
	var (
		mu   sync.Mutex
		done int
		wg   sync.WaitGroup
	)
	progress(Progress{Phase: "Inspecting namespaces", Total: total})

	queue := make(chan *UserReport)
	for w := 0; w < inspectWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range queue {
				e.inspectUser(ctx, u)
				mu.Lock()
				done++
				progress(Progress{Phase: "Inspecting namespaces", Done: done, Total: total})
				mu.Unlock()
			}
		}()
	}
	for _, u := range users {
		queue <- u
	}
	close(queue)
	wg.Wait()
}

func (e *Engine) inspectUser(ctx context.Context, u *UserReport) {
	for i := range u.Namespaces {
		ns := &u.Namespaces[i]
		if ns.OtherMembers > 0 {
			u.addReason(ReasonShared)
		}

		resources, err := e.Inventory.Resources(ctx, ns.Name)
		if err != nil {
			u.addReason(ReasonCheckFailed)
			u.Error = fmt.Sprintf("could not inspect namespace %q: %s", ns.Name, err)
			continue
		}
		sort.Slice(resources, func(a, b int) bool {
			if resources[a].Kind != resources[b].Kind {
				return resources[a].Kind < resources[b].Kind
			}
			return resources[a].Name < resources[b].Name
		})
		ns.ResourceCount = len(resources)
		if len(resources) > maxResourcesPerNamespace {
			resources = resources[:maxResourcesPerNamespace]
		}
		ns.Resources = resources
		if ns.ResourceCount > 0 {
			u.addReason(ReasonResources)
		}
	}
}

// Run scans and then deletes every deletable user. Each user is verified again
// right before the deletion, so that the run never acts on stale data.
func (e *Engine) Run(ctx context.Context, trigger Trigger, progress func(Progress)) *Run {
	if progress == nil {
		progress = func(Progress) {}
	}

	run := &Run{
		ID:           newRunID(e.now()),
		Trigger:      trigger,
		StartedAt:    e.now(),
		InactiveDays: e.InactiveDays,
		Deleted:      []UserReport{},
		Failed:       []UserReport{},
		Blocked:      []UserReport{},
	}
	defer func() { run.FinishedAt = e.now() }()

	snap, err := e.Scan(ctx, progress)
	if err != nil {
		run.Error = err.Error()
		return run
	}
	run.Cutoff = snap.Cutoff
	run.Totals.Users = snap.Totals.Users
	run.Totals.Active = snap.Totals.Active
	run.Totals.Inactive = snap.Totals.Inactive

	var candidates []UserReport
	for _, u := range snap.Users {
		switch u.Status {
		case StatusBlocked:
			run.Blocked = append(run.Blocked, u)
		case StatusDeletable:
			candidates = append(candidates, u)
		}
	}

	if len(candidates) > 0 {
		// The scan can take a while: read the users again so that somebody who just
		// came back is not deleted.
		fresh, err := e.Users.ListUsers(ctx)
		if err != nil {
			run.Error = fmt.Sprintf("refreshing users before deleting: %s", err)
			for _, u := range candidates {
				u.Error = "not attempted: " + run.Error
				run.Failed = append(run.Failed, u)
			}
			candidates = nil
		}
		freshByID := make(map[string]User, len(fresh))
		for _, u := range fresh {
			freshByID[u.ID] = u
		}

		for i, u := range candidates {
			progress(Progress{Phase: "Deleting users", Done: i, Total: len(candidates)})

			if err := ctx.Err(); err != nil {
				u.Error = "not attempted: the run was interrupted"
				run.Failed = append(run.Failed, u)
				continue
			}

			current, exists := freshByID[u.ID]
			if !exists {
				// Somebody else deleted the user in the meantime: nothing to do.
				continue
			}
			verified := e.verify(ctx, current, snap.Cutoff)
			if verified.Status != StatusDeletable {
				run.Blocked = append(run.Blocked, verified)
				continue
			}

			if err := e.Users.DeleteUser(ctx, u.ID); err != nil {
				verified.Error = err.Error()
				run.Failed = append(run.Failed, verified)
				continue
			}
			run.Deleted = append(run.Deleted, verified)
		}
		progress(Progress{Phase: "Deleting users", Done: len(candidates), Total: len(candidates)})
	}

	sortUsers(run.Blocked)
	run.Totals.Deleted = len(run.Deleted)
	run.Totals.Blocked = len(run.Blocked)
	run.Totals.Failed = len(run.Failed)
	return run
}

// verify classifies a single user from scratch, with fresh data.
func (e *Engine) verify(ctx context.Context, u User, cutoff time.Time) UserReport {
	r := classifyActivity(u, e.now(), cutoff)
	if r.Status == StatusActive {
		r.addReason(ReasonActiveAgain)
		return r
	}

	namespaces, err := e.Inventory.NamespacesOf(ctx, u.ID)
	if err != nil {
		r.addReason(ReasonCheckFailed)
		r.Error = fmt.Sprintf("could not list the namespaces of the user: %s", err)
		return r
	}
	r.Namespaces = namespaces
	e.inspectUser(ctx, &r)
	return r
}

func totals(users []UserReport) Totals {
	t := Totals{Users: len(users)}
	for _, u := range users {
		switch u.Status {
		case StatusActive:
			t.Active++
		case StatusDeletable:
			t.Deletable++
		case StatusBlocked:
			t.Blocked++
		}
	}
	t.Inactive = t.Deletable + t.Blocked
	return t
}

// sortUsers puts the users who have been inactive the longest first, and the
// users without activity data at the end.
func sortUsers(users []UserReport) {
	sort.SliceStable(users, func(a, b int) bool {
		ua, ub := users[a], users[b]
		if ua.InactiveDays != ub.InactiveDays {
			if ua.InactiveDays < 0 || ub.InactiveDays < 0 {
				return ub.InactiveDays < 0
			}
			return ua.InactiveDays > ub.InactiveDays
		}
		return strings.ToLower(ua.Name) < strings.ToLower(ub.Name)
	})
}

func newRunID(t time.Time) string {
	suffix := make([]byte, 3)
	_, _ = rand.Read(suffix)
	return t.Format("20060102-150405") + "-" + hex.EncodeToString(suffix)
}

package cleanup

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) *time.Time {
	t := now.AddDate(0, 0, -n)
	return &t
}

// fake is an in-memory Okteto.
type fake struct {
	users      []User
	namespaces map[string][]Namespace
	resources  map[string][]Resource

	listErr      error
	resourcesErr map[string]error
	deleteErr    map[string]error
	deleted      []string
	// onList runs on every ListUsers call, with the number of the call.
	onList func(call int, f *fake)
	lists  int
}

func (f *fake) ListUsers(context.Context) ([]User, error) {
	f.lists++
	if f.onList != nil {
		f.onList(f.lists, f)
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]User{}, f.users...), nil
}

func (f *fake) DeleteUser(_ context.Context, id string) error {
	if err := f.deleteErr[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fake) OwnedNamespaces(context.Context) (map[string][]Namespace, error) {
	out := map[string][]Namespace{}
	for id, list := range f.namespaces {
		out[id] = append([]Namespace{}, list...)
	}
	return out, nil
}

func (f *fake) NamespacesOf(_ context.Context, id string) ([]Namespace, error) {
	return append([]Namespace{}, f.namespaces[id]...), nil
}

func (f *fake) Resources(_ context.Context, ns string) ([]Resource, error) {
	if err := f.resourcesErr[ns]; err != nil {
		return nil, err
	}
	return append([]Resource{}, f.resources[ns]...), nil
}

func engine(f *fake) *Engine {
	return &Engine{Users: f, Inventory: f, InactiveDays: 90, Now: func() time.Time { return now }}
}

func byID(users []UserReport) map[string]UserReport {
	out := map[string]UserReport{}
	for _, u := range users {
		out[u.ID] = u
	}
	return out
}

func ids(users []UserReport) []string {
	out := []string{}
	for _, u := range users {
		out = append(out, u.ID)
	}
	sort.Strings(out)
	return out
}

func TestScanClassifiesUsers(t *testing.T) {
	f := &fake{
		users: []User{
			{ID: "active", LastSeen: daysAgo(10), CreationDate: daysAgo(500)},
			{ID: "edge-active", LastSeen: daysAgo(89), CreationDate: daysAgo(500)},
			{ID: "inactive-empty", LastSeen: daysAgo(91), CreationDate: daysAgo(500)},
			{ID: "inactive-no-namespace", LastSeen: daysAgo(200)},
			{ID: "inactive-resources", LastSeen: daysAgo(200)},
			{ID: "inactive-second-namespace", LastSeen: daysAgo(200)},
			{ID: "inactive-shared", LastSeen: daysAgo(200)},
			{ID: "inactive-admin", Role: RoleAdmin, LastSeen: daysAgo(200)},
			{ID: "active-admin", Role: RoleAdmin, LastSeen: daysAgo(1)},
			{ID: "never-seen-old", CreationDate: daysAgo(120)},
			{ID: "never-seen-new", CreationDate: daysAgo(5)},
			{ID: "no-dates"},
			{ID: "check-fails", LastSeen: daysAgo(200)},
		},
		namespaces: map[string][]Namespace{
			"active":                    {{Name: "active"}},
			"inactive-empty":            {{Name: "empty"}},
			"inactive-resources":        {{Name: "busy"}},
			"inactive-second-namespace": {{Name: "empty-2"}, {Name: "extra-busy"}},
			"inactive-shared":           {{Name: "team", OtherMembers: 2}},
			"inactive-admin":            {{Name: "admin-ns"}},
			"check-fails":               {{Name: "broken"}},
		},
		resources: map[string][]Resource{
			"active":     {{Kind: "Deployment", Name: "api"}},
			"busy":       {{Kind: "Deployment", Name: "api"}, {Kind: "Volume", Name: "data"}},
			"extra-busy": {{Kind: "Okteto app", Name: "movies"}},
		},
		resourcesErr: map[string]error{"broken": errors.New("forbidden")},
	}

	snap, err := engine(f).Scan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := byID(snap.Users)

	want := map[string]struct {
		status  Status
		reasons []Reason
	}{
		"active":                    {StatusActive, nil},
		"edge-active":               {StatusActive, nil},
		"active-admin":              {StatusActive, nil},
		"never-seen-new":            {StatusActive, nil},
		"inactive-empty":            {StatusDeletable, nil},
		"inactive-no-namespace":     {StatusDeletable, nil},
		"never-seen-old":            {StatusDeletable, nil},
		"inactive-resources":        {StatusBlocked, []Reason{ReasonResources}},
		"inactive-second-namespace": {StatusBlocked, []Reason{ReasonResources}},
		"inactive-shared":           {StatusBlocked, []Reason{ReasonShared}},
		"inactive-admin":            {StatusBlocked, []Reason{ReasonAdmin}},
		"no-dates":                  {StatusBlocked, []Reason{ReasonNoActivityData}},
		"check-fails":               {StatusBlocked, []Reason{ReasonCheckFailed}},
	}
	for id, w := range want {
		u := got[id]
		if u.Status != w.status || !reflect.DeepEqual(u.Reasons, w.reasons) {
			t.Errorf("%s: got %s %v, want %s %v", id, u.Status, u.Reasons, w.status, w.reasons)
		}
	}

	if u := got["inactive-resources"]; u.Namespaces[0].ResourceCount != 2 || len(u.Namespaces[0].Resources) != 2 {
		t.Errorf("the blocking resources are not reported: %+v", u.Namespaces)
	}
	if u := got["active"]; len(u.Namespaces) != 0 {
		t.Errorf("active users must not be inspected: %+v", u.Namespaces)
	}
	if u := got["never-seen-old"]; !u.NeverSeen || u.InactiveDays != 120 {
		t.Errorf("never seen users count from their creation: %+v", u)
	}
	if u := got["check-fails"]; u.Error == "" {
		t.Error("the error of a failed check must be reported")
	}

	wantTotals := Totals{Users: 13, Active: 4, Inactive: 9, Deletable: 3, Blocked: 6}
	if snap.Totals != wantTotals {
		t.Errorf("totals: got %+v, want %+v", snap.Totals, wantTotals)
	}
	if len(f.deleted) != 0 {
		t.Errorf("a scan must never delete: %v", f.deleted)
	}
}

func TestScanCapsTheResourcesOfANamespace(t *testing.T) {
	var many []Resource
	for i := 0; i < maxResourcesPerNamespace+25; i++ {
		many = append(many, Resource{Kind: "Pod", Name: string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	f := &fake{
		users:      []User{{ID: "u", LastSeen: daysAgo(200)}},
		namespaces: map[string][]Namespace{"u": {{Name: "big"}}},
		resources:  map[string][]Resource{"big": many},
	}
	snap, err := engine(f).Scan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ns := snap.Users[0].Namespaces[0]
	if ns.ResourceCount != len(many) || len(ns.Resources) != maxResourcesPerNamespace {
		t.Errorf("got %d resources listed and a count of %d", len(ns.Resources), ns.ResourceCount)
	}
}

func TestRunDeletesOnlyDeletableUsers(t *testing.T) {
	f := &fake{
		users: []User{
			{ID: "active", LastSeen: daysAgo(10)},
			{ID: "gone-1", LastSeen: daysAgo(100)},
			{ID: "gone-2", CreationDate: daysAgo(300)},
			{ID: "busy", LastSeen: daysAgo(100)},
			{ID: "admin", Role: RoleAdmin, LastSeen: daysAgo(100)},
			{ID: "api-error", LastSeen: daysAgo(100)},
		},
		namespaces: map[string][]Namespace{"busy": {{Name: "busy"}}, "gone-1": {{Name: "gone-1"}}},
		resources:  map[string][]Resource{"busy": {{Kind: "Deployment", Name: "api"}}},
		deleteErr:  map[string]error{"api-error": errors.New("boom (HTTP 500)")},
	}

	run := engine(f).Run(context.Background(), TriggerManual, nil)

	if run.Error != "" {
		t.Fatalf("unexpected error: %s", run.Error)
	}
	if got := ids(run.Deleted); !reflect.DeepEqual(got, []string{"gone-1", "gone-2"}) {
		t.Errorf("deleted: %v", got)
	}
	sort.Strings(f.deleted)
	if !reflect.DeepEqual(f.deleted, []string{"gone-1", "gone-2"}) {
		t.Errorf("the API deleted %v", f.deleted)
	}
	if got := ids(run.Blocked); !reflect.DeepEqual(got, []string{"admin", "busy"}) {
		t.Errorf("blocked: %v", got)
	}
	if got := ids(run.Failed); !reflect.DeepEqual(got, []string{"api-error"}) || run.Failed[0].Error != "boom (HTTP 500)" {
		t.Errorf("failed: %+v", run.Failed)
	}
	if run.Deleted[0].Namespaces == nil && run.Deleted[1].Namespaces == nil {
		t.Error("the report must list the namespaces deleted with the user")
	}
	want := RunTotals{Users: 6, Active: 1, Inactive: 5, Deleted: 2, Blocked: 2, Failed: 1}
	if run.Totals != want {
		t.Errorf("totals: got %+v, want %+v", run.Totals, want)
	}
}

func TestRunKeepsUsersWhoChangedSinceTheScan(t *testing.T) {
	f := &fake{
		users: []User{
			{ID: "came-back", LastSeen: daysAgo(100)},
			{ID: "got-resources", LastSeen: daysAgo(100)},
			{ID: "still-gone", LastSeen: daysAgo(100)},
			{ID: "deleted-by-someone-else", LastSeen: daysAgo(100)},
		},
		namespaces: map[string][]Namespace{"got-resources": {{Name: "late"}}},
		resources:  map[string][]Resource{},
	}
	// The first list is the scan. Things change before the second one, which the
	// run does right before deleting.
	f.onList = func(call int, f *fake) {
		if call != 2 {
			return
		}
		f.users = []User{
			{ID: "came-back", LastSeen: daysAgo(0)},
			{ID: "got-resources", LastSeen: daysAgo(100)},
			{ID: "still-gone", LastSeen: daysAgo(100)},
		}
		f.resources["late"] = []Resource{{Kind: "Deployment", Name: "new"}}
	}

	run := engine(f).Run(context.Background(), TriggerScheduled, nil)

	if !reflect.DeepEqual(f.deleted, []string{"still-gone"}) {
		t.Fatalf("the API deleted %v, want only still-gone", f.deleted)
	}
	blocked := byID(run.Blocked)
	if !blocked["came-back"].HasReason(ReasonActiveAgain) {
		t.Errorf("came-back: %+v", blocked["came-back"])
	}
	if !blocked["got-resources"].HasReason(ReasonResources) {
		t.Errorf("got-resources: %+v", blocked["got-resources"])
	}
	if len(run.Failed) != 0 {
		t.Errorf("failed: %+v", run.Failed)
	}
}

func TestRunDeletesNothingWhenTheUsersCannotBeRefreshed(t *testing.T) {
	f := &fake{users: []User{{ID: "gone", LastSeen: daysAgo(100)}}}
	f.onList = func(call int, f *fake) {
		if call == 2 {
			f.listErr = errors.New("api down")
		}
	}

	run := engine(f).Run(context.Background(), TriggerManual, nil)

	if len(f.deleted) != 0 {
		t.Fatalf("nothing must be deleted without fresh data: %v", f.deleted)
	}
	if run.Error == "" || len(run.Failed) != 1 {
		t.Errorf("the report must explain the failure: %+v", run)
	}
}

func TestRunReportsAScanFailure(t *testing.T) {
	f := &fake{listErr: errors.New("not an admin")}
	run := engine(f).Run(context.Background(), TriggerManual, nil)
	if run.Error == "" || len(f.deleted) != 0 {
		t.Errorf("got %+v", run)
	}
	if run.FinishedAt.IsZero() {
		t.Error("a failed run still has an end")
	}
}

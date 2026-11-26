package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/okteto/users-cleanup/app/cleanup"
	"github.com/okteto/users-cleanup/app/demo"
	"github.com/okteto/users-cleanup/app/store"
)

func TestNextRun(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name          string
		after         string
		weekday, hour int
		want          string
	}{
		// 2026-11-26 is a Thursday.
		{"later the same day", "2026-11-26 05:00", 4, 6, "2026-11-26 06:00"},
		{"the hour already passed", "2026-11-26 07:00", 4, 6, "2026-12-03 06:00"},
		{"exactly at the hour", "2026-11-26 06:00", 4, 6, "2026-12-03 06:00"},
		{"another day this week", "2026-11-26 07:00", 6, 0, "2026-11-28 00:00"},
		{"a day earlier in the week", "2026-11-26 07:00", 1, 6, "2026-11-30 06:00"},
		{"across the end of the year", "2026-12-30 23:30", 5, 23, "2027-01-01 23:00"},
	}
	for _, tt := range tests {
		if got := NextRun(at(tt.after), tt.weekday, tt.hour); !got.Equal(at(tt.want)) {
			t.Errorf("%s: got %s, want %s", tt.name, got.Format("2006-01-02 15:04"), tt.want)
		}
	}
}

type awake struct {
	persistent bool
	setErr     error
	calls      []bool
}

func (a *awake) IsPersistent(context.Context, string) (bool, error) { return a.persistent, nil }
func (a *awake) SetPersistent(_ context.Context, _ string, v bool) error {
	if a.setErr != nil {
		return a.setErr
	}
	a.calls = append(a.calls, v)
	a.persistent = v
	return nil
}

func newManager(t *testing.T, a KeepAwake, dir string) *Manager {
	t.Helper()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	cluster := demo.New()
	m, err := New(&cleanup.Engine{Users: cluster, Inventory: cluster, InactiveDays: 90}, st, a, "ns", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func waitIdle(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for m.Status().Busy != nil {
		if time.Now().After(deadline) {
			t.Fatal("the manager is still busy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestScheduleMarksTheNamespaceAsPersistent(t *testing.T) {
	a := &awake{}
	dir := t.TempDir()
	m := newManager(t, a, dir)

	if err := m.SetSchedule(context.Background(), true, 3, 9); err != nil {
		t.Fatal(err)
	}
	s := m.Status().Schedule
	if !s.Enabled || s.NextRun == nil || s.KeepAwake == nil || !*s.KeepAwake || !a.persistent {
		t.Fatalf("enabling must keep the namespace awake: %+v", s)
	}

	// The schedule survives a restart.
	if s := newManager(t, a, dir).Status().Schedule; !s.Enabled || s.Weekday != 3 || s.Hour != 9 || s.NextRun == nil {
		t.Fatalf("the schedule was not persisted: %+v", s)
	}

	if err := m.SetSchedule(context.Background(), false, 3, 9); err != nil {
		t.Fatal(err)
	}
	if s := m.Status().Schedule; s.Enabled || s.NextRun != nil || a.persistent {
		t.Fatalf("disabling must remove the mark it added: %+v persistent=%v", s, a.persistent)
	}
}

func TestScheduleLeavesAMarkItDidNotAdd(t *testing.T) {
	a := &awake{persistent: true}
	m := newManager(t, a, t.TempDir())

	if err := m.SetSchedule(context.Background(), true, 1, 6); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSchedule(context.Background(), false, 1, 6); err != nil {
		t.Fatal(err)
	}
	if !a.persistent || len(a.calls) != 0 {
		t.Fatalf("a namespace that was already persistent must stay persistent: calls=%v", a.calls)
	}
}

func TestScheduleReportsAKeepAwakeFailure(t *testing.T) {
	a := &awake{setErr: errors.New("not-authorized")}
	m := newManager(t, a, t.TempDir())

	if err := m.SetSchedule(context.Background(), true, 1, 6); err != nil {
		t.Fatal(err)
	}
	if s := m.Status().Schedule; !s.Enabled || s.KeepAwakeError == "" {
		t.Fatalf("the schedule stays on and the problem is reported: %+v", s)
	}
}

func TestInvalidSchedule(t *testing.T) {
	m := newManager(t, &awake{}, t.TempDir())
	if err := m.SetSchedule(context.Background(), true, 7, 6); err == nil {
		t.Error("weekday 7 is invalid")
	}
	if err := m.SetSchedule(context.Background(), true, 1, 24); err == nil {
		t.Error("hour 24 is invalid")
	}
}

func TestScheduledRun(t *testing.T) {
	m := newManager(t, &awake{}, t.TempDir())
	clock := time.Date(2026, 11, 26, 5, 0, 0, 0, time.UTC) // a Thursday
	m.now = func() time.Time { return clock }

	if err := m.SetSchedule(context.Background(), true, 4, 6); err != nil {
		t.Fatal(err)
	}

	m.tick()
	waitIdle(t, m)
	if n := len(m.Status().History); n != 0 {
		t.Fatalf("nothing runs before its time: %d runs", n)
	}

	clock = clock.Add(90 * time.Minute)
	m.tick()
	waitIdle(t, m)
	status := m.Status()
	if len(status.History) != 1 || status.History[0].Trigger != cleanup.TriggerScheduled {
		t.Fatalf("the scheduled run did not happen: %+v", status.History)
	}
	if status.History[0].Totals.Deleted == 0 {
		t.Errorf("the demo cluster has users to delete: %+v", status.History[0].Totals)
	}
	want := time.Date(2026, 12, 3, 6, 0, 0, 0, time.UTC)
	if status.Schedule.NextRun == nil || !status.Schedule.NextRun.Equal(want) {
		t.Errorf("next run: got %v, want %v", status.Schedule.NextRun, want)
	}

	// The run is not repeated, and its report is stored.
	m.tick()
	waitIdle(t, m)
	if n := len(m.Status().History); n != 1 {
		t.Fatalf("the run was repeated: %d runs", n)
	}
	run, err := m.Run(status.History[0].ID)
	if err != nil || run == nil || len(run.Deleted) != status.History[0].Totals.Deleted {
		t.Fatalf("the report was not stored: %v %+v", err, run)
	}
	if m.Plan() == nil || m.Plan().Totals.Deletable != 0 {
		t.Errorf("the plan is refreshed after a run: %+v", m.Plan())
	}
}

func TestOnlyOneOperationAtATime(t *testing.T) {
	m := newManager(t, &awake{}, t.TempDir())
	if err := m.StartScan(); err != nil {
		t.Fatal(err)
	}
	if err := m.StartRun(cleanup.TriggerManual); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	waitIdle(t, m)
}

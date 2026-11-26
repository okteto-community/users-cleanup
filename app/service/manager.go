// Package service coordinates scans, runs and the weekly schedule: only one
// thing happens at a time, and everything a page needs is available in memory.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/okteto/users-cleanup/app/cleanup"
	"github.com/okteto/users-cleanup/app/store"
)

// ErrBusy is returned when a scan or a run is already in progress.
var ErrBusy = errors.New("a scan or a cleanup is already running")

// KeepAwake controls whether Okteto may put the namespace of this service to
// sleep. A sleeping namespace would miss its weekly run.
type KeepAwake interface {
	IsPersistent(ctx context.Context, namespace string) (bool, error)
	SetPersistent(ctx context.Context, namespace string, persistent bool) error
}

// Busy describes the operation in progress.
type Busy struct {
	// Kind is "scan" or "run".
	Kind string `json:"kind"`
	cleanup.Progress
}

// ScheduleView is the schedule as shown in the page.
type ScheduleView struct {
	Enabled bool       `json:"enabled"`
	Weekday int        `json:"weekday"`
	Hour    int        `json:"hour"`
	NextRun *time.Time `json:"nextRun,omitempty"`
	// KeepAwake is whether the namespace is persistent. Nil when unknown.
	KeepAwake      *bool  `json:"keepAwake"`
	KeepAwakeError string `json:"keepAwakeError,omitempty"`
}

// RunSummary is a run without its lists of users.
type RunSummary struct {
	ID         string            `json:"id"`
	Trigger    cleanup.Trigger   `json:"trigger"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt time.Time         `json:"finishedAt"`
	Totals     cleanup.RunTotals `json:"totals"`
	Error      string            `json:"error,omitempty"`
}

// Status is the light state that the page polls.
type Status struct {
	Busy *Busy `json:"busy"`
	// PlanVersion changes every time a scan finishes.
	PlanVersion int          `json:"planVersion"`
	PlanAt      *time.Time   `json:"planAt,omitempty"`
	PlanError   string       `json:"planError,omitempty"`
	Schedule    ScheduleView `json:"schedule"`
	History     []RunSummary `json:"history"`
}

// Manager owns the state of the service.
type Manager struct {
	engine    *cleanup.Engine
	store     *store.Store
	keepAwake KeepAwake
	namespace string
	logger    *slog.Logger
	now       func() time.Time

	mu             sync.Mutex
	busy           *Busy
	plan           *cleanup.Snapshot
	planVersion    int
	planError      string
	schedule       store.Schedule
	persistent     *bool
	keepAwakeError string
	history        []RunSummary
}

// New loads the saved state and returns a manager.
func New(engine *cleanup.Engine, st *store.Store, keepAwake KeepAwake, namespace string, logger *slog.Logger) (*Manager, error) {
	state, err := st.LoadState()
	if err != nil {
		return nil, err
	}
	runs, err := st.Runs()
	if err != nil {
		return nil, err
	}
	m := &Manager{
		engine:    engine,
		store:     st,
		keepAwake: keepAwake,
		namespace: namespace,
		logger:    logger,
		now:       func() time.Time { return time.Now().UTC() },
		schedule:  state.Schedule,
	}
	for _, r := range runs {
		m.history = append(m.history, summarise(r))
	}
	return m, nil
}

func summarise(r *cleanup.Run) RunSummary {
	return RunSummary{ID: r.ID, Trigger: r.Trigger, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Totals: r.Totals, Error: r.Error}
}

// Status returns the light state of the service.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	s := Status{
		PlanVersion: m.planVersion,
		PlanError:   m.planError,
		History:     append([]RunSummary{}, m.history...),
		Schedule: ScheduleView{
			Enabled:        m.schedule.Enabled,
			Weekday:        m.schedule.Weekday,
			Hour:           m.schedule.Hour,
			NextRun:        m.schedule.NextRun,
			KeepAwake:      m.persistent,
			KeepAwakeError: m.keepAwakeError,
		},
	}
	if m.busy != nil {
		busy := *m.busy
		s.Busy = &busy
	}
	if m.plan != nil {
		at := m.plan.GeneratedAt
		s.PlanAt = &at
	}
	return s
}

// Plan returns the result of the last scan, or nil.
func (m *Manager) Plan() *cleanup.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plan
}

// Run returns the report of a run, or nil when it does not exist.
func (m *Manager) Run(id string) (*cleanup.Run, error) {
	return m.store.Run(id)
}

// StartScan refreshes the plan in the background.
func (m *Manager) StartScan() error {
	if err := m.begin("scan"); err != nil {
		return err
	}
	go func() {
		defer m.end()
		m.scan(context.Background())
	}()
	return nil
}

// StartRun starts a cleanup in the background.
func (m *Manager) StartRun(trigger cleanup.Trigger) error {
	if err := m.begin("run"); err != nil {
		return err
	}
	go func() {
		defer m.end()
		ctx := context.Background()

		m.logger.Info("cleanup started", "trigger", trigger)
		run := m.engine.Run(ctx, trigger, m.progress)
		m.logger.Info("cleanup finished", "trigger", trigger, "deleted", run.Totals.Deleted,
			"blocked", run.Totals.Blocked, "failed", run.Totals.Failed, "error", run.Error)
		for _, u := range run.Deleted {
			m.logger.Info("user deleted", "id", u.ID, "email", u.Email, "inactiveDays", u.InactiveDays)
		}

		if err := m.store.SaveRun(run); err != nil {
			m.logger.Error("could not save the report of the run", "err", err)
		}
		m.mu.Lock()
		m.history = append([]RunSummary{summarise(run)}, m.history...)
		m.mu.Unlock()

		// The deleted users are gone: refresh the plan so that the page shows it.
		m.setKind("scan")
		m.scan(ctx)
	}()
	return nil
}

func (m *Manager) scan(ctx context.Context) {
	snap, err := m.engine.Scan(ctx, m.progress)
	persistent, keepAwakeErr := m.keepAwake.IsPersistent(ctx, m.namespace)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.planVersion++
	if err != nil {
		m.logger.Error("scan failed", "err", err)
		m.planError = err.Error()
	} else {
		m.plan = snap
		m.planError = ""
	}
	if keepAwakeErr == nil {
		m.persistent = &persistent
	}
}

func (m *Manager) begin(kind string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy != nil {
		return ErrBusy
	}
	m.busy = &Busy{Kind: kind, Progress: cleanup.Progress{Phase: "Starting"}}
	return nil
}

func (m *Manager) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = nil
}

func (m *Manager) setKind(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy != nil {
		m.busy = &Busy{Kind: kind, Progress: cleanup.Progress{Phase: "Refreshing the plan"}}
	}
}

func (m *Manager) progress(p cleanup.Progress) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy != nil {
		m.busy.Progress = p
	}
}

// SetSchedule enables or disables the weekly cleanup. Enabling it marks the
// namespace of this service as persistent, so that Okteto does not put it to
// sleep; disabling it removes the mark, if this service was the one adding it.
func (m *Manager) SetSchedule(ctx context.Context, enabled bool, weekday, hour int) error {
	if weekday < 0 || weekday > 6 || hour < 0 || hour > 23 {
		return fmt.Errorf("invalid schedule: weekday must be 0-6 and hour 0-23")
	}

	m.mu.Lock()
	schedule := m.schedule
	m.mu.Unlock()

	schedule.Enabled = enabled
	schedule.Weekday = weekday
	schedule.Hour = hour
	schedule.NextRun = nil
	if enabled {
		next := NextRun(m.now(), weekday, hour)
		schedule.NextRun = &next
	}

	keepAwakeError := ""
	persistent, err := m.keepAwake.IsPersistent(ctx, m.namespace)
	switch {
	case err != nil:
		keepAwakeError = err.Error()
	case enabled && !persistent:
		if err := m.keepAwake.SetPersistent(ctx, m.namespace, true); err != nil {
			keepAwakeError = err.Error()
		} else {
			persistent = true
			schedule.KeepAwakeByUs = true
		}
	case !enabled && persistent && schedule.KeepAwakeByUs:
		if err := m.keepAwake.SetPersistent(ctx, m.namespace, false); err != nil {
			keepAwakeError = err.Error()
		} else {
			persistent = false
			schedule.KeepAwakeByUs = false
		}
	case !enabled:
		schedule.KeepAwakeByUs = false
	}
	if keepAwakeError != "" {
		m.logger.Error("could not change the keep-awake mark of the namespace", "err", keepAwakeError)
	}

	if err := m.store.SaveState(store.State{Schedule: schedule}); err != nil {
		return fmt.Errorf("saving the schedule: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedule = schedule
	m.keepAwakeError = keepAwakeError
	if err == nil {
		m.persistent = &persistent
	}
	m.logger.Info("schedule updated", "enabled", enabled, "weekday", weekday, "hour", hour)
	return nil
}

// RunScheduler starts the weekly cleanup when it is due. It blocks until ctx is done.
func (m *Manager) RunScheduler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.tick()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick starts a scheduled run when one is due. A run that was missed while the
// service was down starts as soon as the service is back.
func (m *Manager) tick() {
	m.mu.Lock()
	schedule := m.schedule
	m.mu.Unlock()

	now := m.now()
	if !schedule.Enabled || schedule.NextRun == nil || now.Before(*schedule.NextRun) {
		return
	}
	if err := m.StartRun(cleanup.TriggerScheduled); err != nil {
		// Something else is running: try again on the next tick.
		return
	}

	m.mu.Lock()
	// The schedule may have changed while the run was starting: only move it forward.
	if m.schedule.Enabled {
		next := NextRun(now, m.schedule.Weekday, m.schedule.Hour)
		m.schedule.NextRun = &next
	}
	schedule = m.schedule
	m.mu.Unlock()

	if err := m.store.SaveState(store.State{Schedule: schedule}); err != nil {
		m.logger.Error("could not save the next scheduled run", "err", err)
	}
}

// NextRun returns the first moment after "after" that falls on the given
// weekday and hour, in UTC.
func NextRun(after time.Time, weekday, hour int) time.Time {
	after = after.UTC()
	next := time.Date(after.Year(), after.Month(), after.Day(), hour, 0, 0, 0, time.UTC)
	next = next.AddDate(0, 0, (weekday-int(next.Weekday())+7)%7)
	if !next.After(after) {
		next = next.AddDate(0, 0, 7)
	}
	return next
}

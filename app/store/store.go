// Package store persists the schedule and the reports of the runs as JSON files.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/okteto/users-cleanup/app/cleanup"
)

// maxRuns is the number of run reports kept on disk.
const maxRuns = 50

// Schedule is the configuration of the weekly cleanup.
type Schedule struct {
	Enabled bool `json:"enabled"`
	// Weekday goes from 0 (Sunday) to 6 (Saturday).
	Weekday int `json:"weekday"`
	// Hour is the hour of the day, in UTC.
	Hour    int        `json:"hour"`
	NextRun *time.Time `json:"nextRun,omitempty"`
	// KeepAwakeByUs is true when this service marked its namespace as persistent,
	// so that it only removes the mark it added itself.
	KeepAwakeByUs bool `json:"keepAwakeByUs,omitempty"`
}

// State is everything that survives a restart, besides the runs.
type State struct {
	Schedule Schedule `json:"schedule"`
}

// Store reads and writes the state in a directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New returns a store that keeps its files in dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		return nil, fmt.Errorf("creating the data directory: %w", err)
	}
	return &Store{dir: dir}, nil
}

// LoadState returns the saved state, or the defaults when nothing was saved yet.
func (s *Store) LoadState() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Monday at 06:00 UTC, disabled.
	state := State{Schedule: Schedule{Weekday: 1, Hour: 6}}
	data, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("reading state.json: %w", err)
	}
	return state, nil
}

// SaveState persists the state.
func (s *Store) SaveState(state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(filepath.Join(s.dir, "state.json"), state)
}

// SaveRun persists the report of a run and removes the oldest reports.
func (s *Store) SaveRun(run *cleanup.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := writeJSON(s.runPath(run.ID), run); err != nil {
		return err
	}
	ids, err := s.runIDs()
	if err != nil {
		return err
	}
	for _, id := range ids[min(len(ids), maxRuns):] {
		_ = os.Remove(s.runPath(id))
	}
	return nil
}

// Run returns the report of a run, or nil when it does not exist.
func (s *Store) Run(id string) (*cleanup.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readRun(id)
}

// Runs returns the reports of every stored run, newest first.
func (s *Store) Runs() ([]*cleanup.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids, err := s.runIDs()
	if err != nil {
		return nil, err
	}
	runs := make([]*cleanup.Run, 0, len(ids))
	for _, id := range ids {
		run, err := s.readRun(id)
		if err != nil || run == nil {
			continue
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func (s *Store) readRun(id string) (*cleanup.Run, error) {
	// IDs come from URLs: never let them escape the runs directory.
	if id == "" || strings.ContainsAny(id, `/\.`) {
		return nil, nil
	}
	data, err := os.ReadFile(s.runPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var run cleanup.Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// runIDs returns the IDs of the stored runs, newest first. IDs start with a
// timestamp, so the alphabetical order is the chronological order.
func (s *Store) runIDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

func (s *Store) runPath(id string) string {
	return filepath.Join(s.dir, "runs", id+".json")
}

// writeJSON writes atomically, so that a crash never leaves a half-written file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

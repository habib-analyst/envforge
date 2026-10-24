// Package registry is envforge's source of truth: a JSON state file
// guarded by an flock, written atomically (tmp+rename+fsync) so a
// kill -9 at any moment leaves either the old or the new state, never
// a torn file. Teardown runs entirely from this registry, from a fresh
// process that never saw the provisioning run.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ProcRec records one spawned process. StartTime is the process start
// time from /proc/<pid>/stat (field 22); together with the PID it makes
// process identity immune to PID reuse.
type ProcRec struct {
	Service   string `json:"service"`
	Kind      string `json:"kind"` // "service" | "redis" | "postgres"
	PID       int    `json:"pid"`
	PGID      int    `json:"pgid"`
	StartTime uint64 `json:"startTime"`
	Port      int    `json:"port"`
}

// EnvRec is one environment's full record.
type EnvRec struct {
	Name       string         `json:"name"`
	State      string         `json:"state"` // provisioning | ready | failed | tearing-down
	Phase      string         `json:"phase"`
	SpecHash   string         `json:"specHash"`
	SpecPath   string         `json:"specPath"`
	Index      int            `json:"index"`
	BlockBase  int            `json:"blockBase"`
	Dir        string         `json:"dir"`
	CreatedAt  time.Time      `json:"createdAt"`
	DurationMs int64          `json:"durationMs,omitempty"`
	Ports      map[string]int `json:"ports"`
	Procs      []ProcRec      `json:"procs"`
}

// Tombstone remembers a torn-down env so audits can attribute its
// port block after the live record is gone.
type Tombstone struct {
	Name       string    `json:"name"`
	Index      int       `json:"index"`
	BlockBase  int       `json:"blockBase"`
	TornDownAt time.Time `json:"tornDownAt"`
}

// Registry is the on-disk state.
type Registry struct {
	Envs       map[string]*EnvRec `json:"envs"`
	Tombstones []Tombstone        `json:"tombstones"`
}

// Store manages one registry file under a home directory.
type Store struct {
	Home string
}

func (s *Store) file() string { return filepath.Join(s.Home, "registry.json") }
func (s *Store) lock() string { return filepath.Join(s.Home, "registry.lock") }

// Update loads the registry under an exclusive lock, applies fn, and
// atomically persists the result.
func (s *Store) Update(fn func(*Registry) error) error {
	if err := os.MkdirAll(s.Home, 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(s.lock(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	reg, err := s.loadLocked()
	if err != nil {
		return err
	}
	if err := fn(reg); err != nil {
		return err
	}
	return s.saveLocked(reg)
}

// Load reads the registry under a shared lock.
func (s *Store) Load() (*Registry, error) {
	lf, err := os.OpenFile(s.lock(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return s.loadLocked()
}

func (s *Store) loadLocked() (*Registry, error) {
	raw, err := os.ReadFile(s.file())
	if errors.Is(err, os.ErrNotExist) {
		return &Registry{Envs: map[string]*EnvRec{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var reg Registry
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("registry corrupt: %w", err)
	}
	if reg.Envs == nil {
		reg.Envs = map[string]*EnvRec{}
	}
	return &reg, nil
}

func (s *Store) saveLocked(reg *Registry) error {
	raw, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.file())
}

// Get returns a copy-free pointer to one env record (call under Load only
// for reads; mutations must go through Update).
func (r *Registry) Get(name string) (*EnvRec, bool) {
	e, ok := r.Envs[name]
	return e, ok
}

// FreeIndex returns the smallest port-block index not used by a live env.
func (r *Registry) FreeIndex() int {
	used := map[int]bool{}
	for _, e := range r.Envs {
		used[e.Index] = true
	}
	for i := 0; ; i++ {
		if !used[i] {
			return i
		}
	}
}

package engine

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"envforge/internal/procutil"
	"envforge/internal/registry"
)

// Teardown removes an environment completely: every process in its
// process groups (from the registry AND from the /proc marker scan),
// its data directories, and its registry entry. It runs entirely from
// durable state, so it works from a fresh process on an env it never
// provisioned — including envs whose provisioner was kill -9'd at any
// phase. It is idempotent: running it again after a partial or killed
// teardown finishes the job.
func (e *Engine) Teardown(envName string) error {
	reg, err := e.store.Load()
	if err != nil {
		return err
	}
	rec, registered := reg.Get(envName)
	dir := e.envDir(envName)
	if !registered {
		if _, err := os.Stat(dir); os.IsNotExist(err) && len(procutil.FindByMarker(envName)) == 0 {
			return nil // nothing exists: teardown is a no-op success
		}
		// Registry lost but artifacts remain: proceed marker/dir-only.
	}

	e.phase(envName, "down:kill")
	e.setState(envName, "tearing-down")

	// Collect process groups: registry records + marker scan (adopts
	// processes the provisioner died before recording).
	pgids := map[int]bool{}
	if registered {
		for _, pr := range rec.Procs {
			pgids[pr.PGID] = true
		}
	}
	for _, pi := range procutil.FindByMarker(envName) {
		pgids[pi.PGID] = true
	}
	for pgid := range pgids {
		_ = procutil.KillGroup(pgid, syscall.SIGTERM)
	}

	// Phase down:wait — poll until every marker process is gone;
	// escalate to SIGKILL after a grace period.
	e.phase(envName, "down:wait")
	deadline := time.Now().Add(10 * time.Second)
	escalated := false
	for {
		remaining := procutil.FindByMarker(envName)
		if registered {
			for _, pr := range rec.Procs {
				if procutil.Alive(pr.PID, pr.StartTime) {
					remaining = append(remaining, procutil.ProcInfo{PID: pr.PID, PGID: pr.PGID})
				}
			}
		}
		if len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("teardown %s: %d process(es) survived SIGKILL: %v", envName, len(remaining), remaining)
		}
		if !escalated && time.Since(deadline.Add(-10*time.Second)) > 3*time.Second {
			escalated = true
			for _, pi := range remaining {
				_ = procutil.KillGroup(pi.PGID, syscall.SIGKILL)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Phase down:rmdirs.
	e.phase(envName, "down:rmdirs")
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("teardown %s: remove dirs: %w", envName, err)
	}

	// Phase down:unregister.
	e.phase(envName, "down:unregister")
	return e.store.Update(func(r *registry.Registry) error {
		if rec, ok := r.Get(envName); ok {
			r.Tombstones = append(r.Tombstones, registry.Tombstone{
				Name: envName, Index: rec.Index, BlockBase: rec.BlockBase, TornDownAt: time.Now(),
			})
			delete(r.Envs, envName)
		}
		return nil
	})
}

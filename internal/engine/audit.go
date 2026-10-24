package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"envforge/internal/procutil"
	"envforge/internal/registry"
)

// Audit is the leak detector. Crucially it does NOT trust the
// provisioner's bookkeeping: it discovers reality from three
// independent sources — /proc environment markers, the kernel's
// listening-socket table, and the filesystem — and cross-checks them
// against the registry's set of live envs. Anything envforge-shaped
// that belongs to no live env is a leak.
//
// If envName is non-empty, findings are scoped to that env (used after
// tearing one env down while others live). Otherwise any leftover of
// any non-live env is reported.
func (e *Engine) Audit(envName string) ([]string, error) {
	reg, err := e.store.Load()
	if err != nil {
		return nil, err
	}
	live := map[string]*registry.EnvRec{}
	for name, rec := range reg.Envs {
		live[name] = rec
	}
	livePorts := map[int]string{} // port -> env
	for name, rec := range live {
		for _, p := range rec.Ports {
			livePorts[p] = name
		}
	}
	var leaks []string
	inScope := func(name string) bool { return envName == "" || name == envName }

	// 1. Processes: marker scan.
	for _, pi := range procutil.FindByMarker("") {
		if _, isLive := live[pi.Env]; isLive {
			continue
		}
		if inScope(pi.Env) {
			leaks = append(leaks, fmt.Sprintf("process: pid %d (pgid %d, service %q) of torn-down env %q still alive", pi.PID, pi.PGID, pi.Service, pi.Env))
		}
	}

	// 2. Ports: listeners inside the managed range owned by no live env.
	listening := procutil.ListeningPorts()
	for port := BlockBaseMin; port < BlockBaseMin+BlockSize*MaxIndex; port++ {
		if !listening[port] {
			continue
		}
		if owner, ok := livePorts[port]; ok {
			_ = owner
			continue
		}
		// Attribute by block: block index -> tombstone / unknown.
		idx := (port - BlockBaseMin) / BlockSize
		name := ""
		for _, ts := range reg.Tombstones {
			if ts.Index == idx {
				name = ts.Name
			}
		}
		if envName == "" || name == envName {
			leaks = append(leaks, fmt.Sprintf("port: %d listening in managed range, owned by no live env (block of %q)", port, name))
		}
	}

	// 3. Directories: envs/<name> present but not live.
	envsRoot := filepath.Join(e.cfg.Home, "envs")
	entries, _ := os.ReadDir(envsRoot)
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		if _, isLive := live[ent.Name()]; isLive {
			continue
		}
		if inScope(ent.Name()) {
			leaks = append(leaks, fmt.Sprintf("dir: %s exists but env %q is not live", filepath.Join(envsRoot, ent.Name()), ent.Name()))
		}
	}

	// 4. Registry residue for the scoped env.
	if envName != "" {
		if _, stillThere := live[envName]; stillThere {
			leaks = append(leaks, fmt.Sprintf("registry: env %q still registered after teardown", envName))
		}
	}
	return leaks, nil
}

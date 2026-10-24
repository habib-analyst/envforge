package engine

import (
	"fmt"
	"os"
	"path/filepath"

	"envforge/internal/procutil"
	"envforge/internal/spec"
)

// Verify compares an environment's live state against its spec and
// returns one line per drift item found (empty slice = no drift).
func (e *Engine) Verify(envName string) ([]string, error) {
	reg, err := e.store.Load()
	if err != nil {
		return nil, err
	}
	rec, ok := reg.Get(envName)
	if !ok {
		return nil, fmt.Errorf("env %q not found", envName)
	}
	var drift []string

	// 1. Spec file changed since provision?
	curHash, err := spec.HashFile(rec.SpecPath)
	if err != nil {
		drift = append(drift, fmt.Sprintf("spec: cannot read %s: %v", rec.SpecPath, err))
	} else if curHash != rec.SpecHash {
		drift = append(drift, "spec: file changed since provision (sha256 mismatch)")
	}
	sp, _, lerr := spec.Load(rec.SpecPath)
	if lerr != nil {
		drift = append(drift, fmt.Sprintf("spec: no longer parses: %v", lerr))
		return drift, nil
	}

	listening := procutil.ListeningPorts()
	ctx := e.renderCtx(envName, rec.Dir, rec.Ports, sp)
	byService := map[string]int{} // service name -> index into rec.Procs
	for i, pr := range rec.Procs {
		byService[pr.Service] = i
	}

	for _, svc := range sp.Services {
		idx, recorded := byService[svc.Name]
		if !recorded {
			drift = append(drift, fmt.Sprintf("%s: no process record in registry", svc.Name))
			continue
		}
		pr := rec.Procs[idx]

		// 2. Process identity (PID + start time, reuse-proof).
		if !procutil.Alive(pr.PID, pr.StartTime) {
			drift = append(drift, fmt.Sprintf("%s: process not running (pid %d gone or recycled)", svc.Name, pr.PID))
		} else {
			// 3. Listening port matches assignment.
			if !listening[pr.Port] {
				drift = append(drift, fmt.Sprintf("%s: assigned port %d is not listening", svc.Name, pr.Port))
			}
			// 4. Config read back over HTTP matches the rendered spec env.
			if svc.Health != "" {
				cfg, err := httpJSON(pr.Port, "/config")
				if err != nil {
					drift = append(drift, fmt.Sprintf("%s: /config read-back failed: %v", svc.Name, err))
				} else {
					_, senv, rerr := ctx.RenderService(svc)
					if rerr == nil {
						for k, want := range senv {
							if got, present := cfg[k]; present && got != want {
								drift = append(drift, fmt.Sprintf("%s: config %s = %q, spec says %q", svc.Name, k, got, want))
							}
						}
					}
				}
			}
		}

		// 5. Rendered env file on disk matches a fresh render.
		_, senv, rerr := ctx.RenderService(svc)
		if rerr == nil {
			want := renderEnvFile(managedEnv(envName, svc.Name, rec.Ports, sp), senv)
			got, rerr2 := os.ReadFile(filepath.Join(rec.Dir, svc.Name+".env"))
			if rerr2 != nil {
				drift = append(drift, fmt.Sprintf("%s: rendered env file missing: %v", svc.Name, rerr2))
			} else if string(got) != want {
				drift = append(drift, fmt.Sprintf("%s: rendered env file on disk differs from spec render", svc.Name))
			}
		}
	}

	// 6. Infra processes alive?
	for _, pr := range rec.Procs {
		if pr.Kind == "service" {
			continue
		}
		if !procutil.Alive(pr.PID, pr.StartTime) {
			drift = append(drift, fmt.Sprintf("%s: infra process not running (pid %d)", pr.Service, pr.PID))
		} else if !listening[pr.Port] {
			drift = append(drift, fmt.Sprintf("%s: infra port %d is not listening", pr.Service, pr.Port))
		}
	}
	return drift, nil
}

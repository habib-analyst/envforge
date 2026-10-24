package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"envforge/internal/procutil"
)

// repoRoot resolves the build root (internal/engine -> ../..).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func buildAppsvc(t *testing.T, root string) string {
	t.Helper()
	out := filepath.Join(root, "bin", "appsvc")
	if _, err := os.Stat(out); err != nil {
		cmd := exec.Command("go", "build", "-o", out, "./examples/appsvc")
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build appsvc: %v\n%s", err, b)
		}
	}
	return out
}

// Full lifecycle integration test: provision a redis + 2-service env,
// verify clean, kill a service, verify flags exactly that drift,
// tear down, audit must be empty.
func TestLifecycleDriftTeardownAudit(t *testing.T) {
	root := repoRoot(t)
	appsvc := buildAppsvc(t, root)
	cfg := DefaultConfig()
	home, err := os.MkdirTemp(root, "testhome-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	cfg.Home = home
	cfg.Appsvc = appsvc
	eng := New(cfg)

	specPath := filepath.Join(home, "spec.json")
	specBody := `{
	  "name": "pr-test",
	  "infra": {"redis": true},
	  "services": [
	    {"name": "api", "command": ["` + appsvc + `"], "env": {"FEATURE_FLAG": "on"}, "health": "/health"},
	    {"name": "web", "command": ["` + appsvc + `"], "env": {"FEATURE_FLAG": "on"}, "health": "/health"}
	  ],
	  "smoke": [{"service": "web", "path": "/call?svc=api", "expect": 200}]
	}`
	if err := os.WriteFile(specPath, []byte(specBody), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := eng.Provision("pr-test", specPath, false)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Self-clean even when an assertion below fails midway (a leaked
	// env would impersonate the next run's env on the same ports).
	defer func() { _ = New(cfg).Teardown("pr-test") }()
	if res.Ports["api"] == 0 || res.Ports["redis"] == 0 {
		t.Fatalf("ports not assigned: %v", res.Ports)
	}

	drift, err := eng.Verify("pr-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(drift) != 0 {
		t.Fatalf("fresh env has drift: %v", drift)
	}

	// Kill the api service's whole group; verify must flag exactly it.
	recs, _ := eng.List()
	var apiPGID int
	for _, pr := range recs[0].Procs {
		if pr.Service == "api" {
			apiPGID = pr.PGID
		}
	}
	if err := procutil.KillGroup(apiPGID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Signal delivery and reaping are async: poll until verify flags it.
	joined := ""
	for i := 0; i < 50; i++ {
		drift, err = eng.Verify("pr-test")
		if err != nil {
			t.Fatal(err)
		}
		joined = strings.Join(drift, "\n")
		if strings.Contains(joined, "not running") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(joined, "api") || !strings.Contains(joined, "not running") {
		t.Fatalf("killed api not flagged as drift: %v", drift)
	}
	if strings.Contains(joined, "web: process not running") {
		t.Fatalf("web falsely flagged: %v", drift)
	}

	// Teardown from a *fresh* engine (simulating a new process).
	eng2 := New(cfg)
	if err := eng2.Teardown("pr-test"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	leaks, err := eng2.Audit("")
	if err != nil {
		t.Fatal(err)
	}
	if len(leaks) != 0 {
		t.Fatalf("leaks after teardown: %v", leaks)
	}
	if _, err := os.Stat(filepath.Join(home, "envs", "pr-test")); !os.IsNotExist(err) {
		t.Fatal("env dir survived teardown")
	}
	if got := procutil.FindByMarker("pr-test"); len(got) != 0 {
		t.Fatalf("marker processes survived teardown: %v", got)
	}
	// Idempotency: second teardown is a clean no-op.
	if err := eng2.Teardown("pr-test"); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
}

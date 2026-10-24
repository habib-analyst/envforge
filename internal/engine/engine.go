// Package engine implements envforge's lifecycle: provision, teardown,
// drift verification, and the registry-independent leak audit.
//
// Design rules that make the guarantees possible:
//   - Registry first: the env record (with its port block) is created
//     before anything exists, and every spawned process is recorded
//     immediately after it starts.
//   - Marker second: every child process carries ENVFORGE_ENV in its
//     environment, so teardown and audit can discover processes by
//     scanning /proc — including processes a kill -9 prevented the
//     provisioner from recording.
//   - Teardown is idempotent and runs from the registry alone, from a
//     fresh process; killing it mid-run just means running it again.
package engine

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"envforge/internal/procutil"
	"envforge/internal/registry"
	"envforge/internal/spec"
)

const (
	BlockBaseMin = 20000
	BlockSize    = 100
	MaxIndex     = 1000
)

// Config locates the home directory and helper binaries.
type Config struct {
	Home     string // envforge state root (registry + envs/)
	RedisBin string
	PGBinDir string
	Appsvc   string
}

// DefaultConfig resolves paths from env vars, PATH, and the workspace
// tool locations used on this machine.
func DefaultConfig() Config {
	home := os.Getenv("ENVFORGE_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".envforge")
		}
	}
	cfg := Config{Home: home}
	cfg.RedisBin = os.Getenv("ENVFORGE_REDIS_BIN")
	if cfg.RedisBin == "" {
		if p, err := exec.LookPath("redis-server"); err == nil {
			cfg.RedisBin = p
		} else {
			cfg.RedisBin = "/home/hatch/workspace/tools/redis-src/redis-7.4.2/src/redis-server"
		}
	}
	cfg.PGBinDir = os.Getenv("ENVFORGE_PG_BIN")
	if cfg.PGBinDir == "" {
		cfg.PGBinDir = "/home/hatch/workspace/tools/pgsql/bin"
	}
	cfg.Appsvc = os.Getenv("ENVFORGE_APPSVC")
	if cfg.Appsvc == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.Appsvc = filepath.Join(filepath.Dir(exe), "appsvc")
		}
	}
	return cfg
}

// Engine is one envforge operator. It is stateless across calls: all
// durable state lives in the registry, so any process can operate on
// any environment.
type Engine struct {
	cfg   Config
	store *registry.Store
	slow  time.Duration // ENVFORGE_SLOW_PHASE_MS: chaos-injection phase delay
}

// New builds an Engine.
func New(cfg Config) *Engine {
	e := &Engine{cfg: cfg, store: &registry.Store{Home: cfg.Home}}
	if ms, err := strconv.Atoi(os.Getenv("ENVFORGE_SLOW_PHASE_MS")); err == nil && ms > 0 {
		e.slow = time.Duration(ms) * time.Millisecond
	}
	return e
}

func (e *Engine) envDir(name string) string {
	return filepath.Join(e.cfg.Home, "envs", name)
}

// phase records a lifecycle phase in the registry, mirrors it to the
// chaos phase file (ENVFORGE_PHASE_FILE), and optionally sleeps so a
// chaos killer can reliably hit the phase boundary.
func (e *Engine) phase(envName, phase string) {
	_ = e.store.Update(func(r *registry.Registry) error {
		if rec, ok := r.Get(envName); ok {
			rec.Phase = phase
		}
		return nil
	})
	if pf := os.Getenv("ENVFORGE_PHASE_FILE"); pf != "" {
		tmp := pf + ".tmp"
		if os.WriteFile(tmp, []byte(phase+"\n"), 0o644) == nil {
			_ = os.Rename(tmp, pf)
		}
	}
	if e.slow > 0 {
		time.Sleep(e.slow)
	}
}

func (e *Engine) setState(envName, state string) {
	_ = e.store.Update(func(r *registry.Registry) error {
		if rec, ok := r.Get(envName); ok {
			rec.State = state
		}
		return nil
	})
}

func (e *Engine) recordProc(envName string, pr registry.ProcRec) {
	_ = e.store.Update(func(r *registry.Registry) error {
		if rec, ok := r.Get(envName); ok {
			rec.Procs = append(rec.Procs, pr)
		}
		return nil
	})
}

// renderCtx builds the template context for an env.
func (e *Engine) renderCtx(envName, dir string, ports map[string]int, sp *spec.Spec) spec.Context {
	ctx := spec.Context{
		Env: envName, Dir: dir, Ports: ports,
		Appsvc: e.cfg.Appsvc, RedisBin: e.cfg.RedisBin, PGBinDir: e.cfg.PGBinDir,
		Vars: sp.Vars,
	}
	ctx.RedisPort = ports["redis"]
	ctx.PGPort = ports["postgres"]
	return ctx
}

// managedEnv computes the envforge-managed environment variables for a
// service (these are also written to its rendered .env file).
func managedEnv(envName, svcName string, ports map[string]int, sp *spec.Spec) map[string]string {
	m := map[string]string{
		"ENVFORGE_ENV": envName,
		"ENVFORGE_SVC": svcName,
		"PORT":         strconv.Itoa(ports[svcName]),
	}
	if p, ok := ports["redis"]; ok {
		m["REDIS_ADDR"] = "127.0.0.1:" + strconv.Itoa(p)
	}
	if p, ok := ports["postgres"]; ok {
		m["DATABASE_URL"] = fmt.Sprintf("postgres://envforge@127.0.0.1:%d/postgres", p)
	}
	for _, svc := range sp.Services {
		key := "EF_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(svc.Name)) + "_URL"
		m[key] = fmt.Sprintf("http://127.0.0.1:%d", ports[svc.Name])
	}
	return m
}

// renderEnvFile produces the deterministic content of a service's
// rendered env file: managed vars plus its (rendered) spec env, sorted.
func renderEnvFile(managed, specEnv map[string]string) string {
	all := map[string]string{}
	for k, v := range managed {
		all[k] = v
	}
	for k, v := range specEnv {
		all[k] = v
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, all[k])
	}
	return b.String()
}

// spawn starts one process in its own process group, logs to the env's
// log dir, stamps the marker env, and records it in the registry
// immediately.
func (e *Engine) spawn(envName, svcName, kind string, argv []string, extraEnv map[string]string, dir string, port int) (registry.ProcRec, error) {
	logPath := filepath.Join(dir, "logs", svcName+".log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return registry.ProcRec{}, err
	}
	defer lf.Close()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	env := os.Environ()
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return registry.ProcRec{}, fmt.Errorf("start %s: %w", svcName, err)
	}
	go func() { _ = cmd.Wait() }() // reap if it dies while we live

	_, startTime, _ := procutil.Stat(cmd.Process.Pid)
	pr := registry.ProcRec{
		Service: svcName, Kind: kind,
		PID: cmd.Process.Pid, PGID: cmd.Process.Pid,
		StartTime: startTime, Port: port,
	}
	e.recordProc(envName, pr)
	return pr, nil
}

// ProvisionResult summarises a completed provision.
type ProvisionResult struct {
	Name       string         `json:"name"`
	Ports      map[string]int `json:"ports"`
	DurationMs int64          `json:"duration_ms"`
}

// Provision creates an environment end to end.
func (e *Engine) Provision(envName, specPath string, hold bool) (*ProvisionResult, error) {
	start := time.Now()
	sp, specHash, err := spec.Load(specPath)
	if err != nil {
		return nil, err
	}
	if envName == "" {
		envName = sp.Name
	}
	if envName == "" {
		return nil, fmt.Errorf("env name required (spec has no name; pass --name)")
	}
	dir := e.envDir(envName)

	// Phase alloc: registry entry + port block, before anything exists.
	var ports map[string]int
	err = e.store.Update(func(r *registry.Registry) error {
		if _, exists := r.Get(envName); exists {
			return fmt.Errorf("env %q already exists; tear it down first", envName)
		}
		idx := r.FreeIndex()
		base := BlockBaseMin + idx*BlockSize
		ports = map[string]int{}
		next := 1
		if sp.Infra.Redis {
			ports["redis"] = base + next
			next++
		}
		if sp.Infra.Postgres {
			ports["postgres"] = base + next
			next++
		}
		next = 10 // leave infra room; services start at block+10
		for _, svc := range sp.Services {
			ports[svc.Name] = base + next
			next++
		}
		r.Envs[envName] = &registry.EnvRec{
			Name: envName, State: "provisioning", Phase: "alloc",
			SpecHash: specHash, SpecPath: specPath,
			Index: idx, BlockBase: base, Dir: dir,
			CreatedAt: time.Now(), Ports: ports,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.phase(envName, "alloc")

	fail := func(err error) (*ProvisionResult, error) {
		e.setState(envName, "failed")
		return nil, err
	}

	// Phase dirs: directory tree + env marker + rendered env files.
	e.phase(envName, "dirs")
	for _, d := range []string{dir, filepath.Join(dir, "logs"), filepath.Join(dir, "data")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fail(err)
		}
	}
	// Every port in the block shares the same base: port/BlockSize*BlockSize.
	blockBase := 0
	for _, p := range ports {
		blockBase = p / BlockSize * BlockSize
		break
	}
	marker, _ := json.MarshalIndent(map[string]any{
		"name": envName, "blockBase": blockBase, "specHash": specHash,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "env.json"), marker, 0o644); err != nil {
		return fail(err)
	}
	ctx := e.renderCtx(envName, dir, ports, sp)
	rendered := map[string][]string{} // svc -> argv
	renderedEnv := map[string]map[string]string{}
	for _, svc := range sp.Services {
		argv, senv, err := ctx.RenderService(svc)
		if err != nil {
			return fail(err)
		}
		rendered[svc.Name] = argv
		renderedEnv[svc.Name] = senv
		content := renderEnvFile(managedEnv(envName, svc.Name, ports, sp), senv)
		if err := os.WriteFile(filepath.Join(dir, svc.Name+".env"), []byte(content), 0o644); err != nil {
			return fail(err)
		}
		if svc.DataDir {
			if err := os.MkdirAll(filepath.Join(dir, "data", svc.Name), 0o755); err != nil {
				return fail(err)
			}
		}
	}

	// Infra: Postgres cluster init (sync, ~1s) lives in the dirs phase.
	if sp.Infra.Postgres {
		if err := e.initPostgres(dir); err != nil {
			return fail(fmt.Errorf("initdb: %w", err))
		}
	}

	// Pre-flight: every assigned port must be free, or health checks
	// could be answered by a squatter and "succeed" against the
	// wrong process.
	for name, p := range ports {
		if procutil.ListeningPorts()[p] {
			return fail(fmt.Errorf("port %d for %s is already listening (squatter?)", p, name))
		}
	}

	// Phase start:<name> per process, in dependency order.
	if sp.Infra.Redis {
		e.phase(envName, "start:redis")
		rdir := filepath.Join(dir, "data", "redis")
		if err := os.MkdirAll(rdir, 0o755); err != nil {
			return fail(err)
		}
		argv := []string{e.cfg.RedisBin,
			"--port", strconv.Itoa(ports["redis"]),
			"--bind", "127.0.0.1", "--protected-mode", "no",
			"--dir", rdir, "--dbfilename", "dump.rdb",
			"--save", "", "--appendonly", "no", "--daemonize", "no",
		}
		env := map[string]string{"ENVFORGE_ENV": envName, "ENVFORGE_SVC": "redis"}
		if _, err := e.spawn(envName, "redis", "redis", argv, env, dir, ports["redis"]); err != nil {
			return fail(err)
		}
	}
	if sp.Infra.Postgres {
		e.phase(envName, "start:postgres")
		argv := []string{"unshare", "-U", "--map-user=65534", "--map-group=65534",
			filepath.Join(e.cfg.PGBinDir, "postgres"),
			"-D", filepath.Join(dir, "data", "pgdata"),
			"-p", strconv.Itoa(ports["postgres"]),
			"-k", filepath.Join(dir, "data", "pgsock"),
			"-c", "listen_addresses=127.0.0.1",
			"-c", "shared_buffers=16MB",
			"-c", "max_connections=10",
		}
		env := map[string]string{"ENVFORGE_ENV": envName, "ENVFORGE_SVC": "postgres"}
		if _, err := e.spawn(envName, "postgres", "postgres", argv, env, dir, ports["postgres"]); err != nil {
			return fail(err)
		}
	}
	for _, svc := range sp.Services {
		e.phase(envName, "start:"+svc.Name)
		m := managedEnv(envName, svc.Name, ports, sp)
		for k, v := range renderedEnv[svc.Name] {
			m[k] = v
		}
		if _, err := e.spawn(envName, svc.Name, "service", rendered[svc.Name], m, dir, ports[svc.Name]); err != nil {
			return fail(err)
		}
	}

	// Phase health: every process must answer.
	e.phase(envName, "health")
	if err := e.waitHealthy(envName, sp, ports); err != nil {
		return fail(err)
	}

	// Phase smoke: per-service HTTP 200 + spec smoke checks.
	e.phase(envName, "smoke")
	if err := e.runSmoke(sp, ports); err != nil {
		return fail(err)
	}

	dur := time.Since(start).Milliseconds()
	_ = e.store.Update(func(r *registry.Registry) error {
		if rec, ok := r.Get(envName); ok {
			rec.State = "ready"
			rec.Phase = "ready"
			rec.DurationMs = dur
		}
		return nil
	})
	e.phase(envName, "ready")

	if hold {
		select {} // chaos "idle" phase: provisioner alive, env complete
	}
	return &ProvisionResult{Name: envName, Ports: ports, DurationMs: dur}, nil
}

// initPostgres runs initdb inside a user namespace (this host's shell
// is uid 0; Postgres refuses to run as root; inside the namespace the
// server sees uid 65534 (nobody), mapped to host uid 0. Using nobody
// rather than an arbitrary uid matters: initdb resolves the effective
// uid in /etc/passwd, and minimal container passwd files always carry
// nobody but may carry no uid-1000 user).
func (e *Engine) initPostgres(dir string) error {
	pgdata := filepath.Join(dir, "data", "pgdata")
	pgsock := filepath.Join(dir, "data", "pgsock")
	for _, d := range []string{pgdata, pgsock} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	cmd := exec.Command("unshare", "-U", "--map-user=65534", "--map-group=65534",
		filepath.Join(e.cfg.PGBinDir, "initdb"),
		"-D", pgdata, "-U", "envforge", "--auth=trust", "--no-instructions")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e *Engine) waitHealthy(envName string, sp *spec.Spec, ports map[string]int) error {
	type target struct {
		name string
		port int
		kind string
		path string
	}
	var targets []target
	if sp.Infra.Redis {
		targets = append(targets, target{"redis", ports["redis"], "redis", ""})
	}
	if sp.Infra.Postgres {
		targets = append(targets, target{"postgres", ports["postgres"], "postgres", ""})
	}
	for _, svc := range sp.Services {
		kind := "tcp"
		if svc.Health != "" {
			kind = "http"
		}
		targets = append(targets, target{svc.Name, ports[svc.Name], kind, svc.Health})
	}
	deadline := time.Now().Add(30 * time.Second)
	for _, t := range targets {
		for {
			ok := false
			switch t.kind {
			case "redis":
				ok = redisPing(t.port)
			case "postgres":
				ok = e.pgReady(t.port)
			case "http":
				ok = httpStatus(t.port, t.path) == 200
			default:
				ok = tcpOpen(t.port)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("env %s: %s (port %d) not healthy within 30s; see logs in %s", envName, t.name, t.port, e.envDir(envName))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

func (e *Engine) runSmoke(sp *spec.Spec, ports map[string]int) error {
	for _, svc := range sp.Services {
		if svc.Health == "" {
			continue
		}
		if got := httpStatus(ports[svc.Name], svc.Health); got != 200 {
			return fmt.Errorf("smoke: %s%s -> HTTP %d, want 200", svc.Name, svc.Health, got)
		}
	}
	for _, chk := range sp.Smoke {
		want := chk.Expect
		if want == 0 {
			want = 200
		}
		if got := httpStatus(ports[chk.Service], chk.Path); got != want {
			return fmt.Errorf("smoke: %s%s -> HTTP %d, want %d", chk.Service, chk.Path, got, want)
		}
	}
	return nil
}

func (e *Engine) pgReady(port int) bool {
	cmd := exec.Command(filepath.Join(e.cfg.PGBinDir, "pg_isready"),
		"-h", "127.0.0.1", "-p", strconv.Itoa(port), "-q")
	if err := cmd.Run(); err == nil {
		return true
	}
	return tcpOpen(port)
}

func tcpOpen(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func httpStatus(port int, path string) int {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// httpJSON fetches a JSON object (used by verify for /config read-back).
func httpJSON(port int, path string) (map[string]string, error) {
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// redisPing speaks one inline PING to a Redis port.
func redisPing(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "+PONG")
}

// List returns all live env records.
func (e *Engine) List() ([]*registry.EnvRec, error) {
	reg, err := e.store.Load()
	if err != nil {
		return nil, err
	}
	out := make([]*registry.EnvRec, 0, len(reg.Envs))
	for _, rec := range reg.Envs {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Package spec defines the envforge environment specification format,
// its canonical hash, and template rendering for commands and env vars.
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"text/template"
)

// Service is one process in the environment.
type Service struct {
	Name    string            `json:"name"`
	Command []string          `json:"command"`           // argv; every element is template-rendered
	Env     map[string]string `json:"env,omitempty"`     // values are template-rendered
	Health  string            `json:"health,omitempty"`  // HTTP path for health/config, e.g. "/health"
	DataDir bool              `json:"dataDir,omitempty"` // give the service its own data directory
}

// Infra selects built-in per-env infrastructure processes.
type Infra struct {
	Redis    bool `json:"redis,omitempty"`
	Postgres bool `json:"postgres,omitempty"`
}

// SmokeCheck is one HTTP request the provisioner makes after health passes.
type SmokeCheck struct {
	Service string `json:"service"`
	Path    string `json:"path"`
	Expect  int    `json:"expect"` // expected HTTP status; 0 means 200
}

// Spec is a full environment specification.
type Spec struct {
	Name     string            `json:"name,omitempty"` // default env name; CLI --name overrides
	Infra    Infra             `json:"infra,omitempty"`
	Services []Service         `json:"services"`
	Smoke    []SmokeCheck      `json:"smoke,omitempty"`
	Vars     map[string]string `json:"vars,omitempty"` // extra template variables
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Load reads and validates a spec file. It returns the spec and the
// SHA-256 of the raw file bytes (the drift-detection baseline).
func Load(path string) (*Spec, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, "", fmt.Errorf("parse spec: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, "", err
	}
	return &s, hex.EncodeToString(sum[:]), nil
}

// HashFile returns the SHA-256 of a file's raw bytes.
func HashFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Validate checks structural rules of the spec.
func (s *Spec) Validate() error {
	if len(s.Services) == 0 && !s.Infra.Redis && !s.Infra.Postgres {
		return fmt.Errorf("spec declares no services and no infra")
	}
	seen := map[string]bool{}
	for _, svc := range s.Services {
		if !nameRe.MatchString(svc.Name) {
			return fmt.Errorf("invalid service name %q", svc.Name)
		}
		if svc.Name == "redis" || svc.Name == "postgres" {
			return fmt.Errorf("service name %q is reserved for infra", svc.Name)
		}
		if seen[svc.Name] {
			return fmt.Errorf("duplicate service name %q", svc.Name)
		}
		seen[svc.Name] = true
		if len(svc.Command) == 0 {
			return fmt.Errorf("service %q has no command", svc.Name)
		}
	}
	for _, chk := range s.Smoke {
		if !seen[chk.Service] {
			return fmt.Errorf("smoke check references unknown service %q", chk.Service)
		}
	}
	return nil
}

// Context carries everything a template may reference while rendering.
type Context struct {
	Env       string            // environment name, e.g. "pr-123"
	Dir       string            // environment directory (absolute)
	Port      int               // port of the service being rendered (0 when N/A)
	Ports     map[string]int    // every service + infra ("redis"/"postgres") port
	Appsvc    string            // path to the bundled demo app binary
	RedisBin  string            // path to redis-server
	PGBinDir  string            // path to postgres bin directory
	Vars      map[string]string // spec-level vars
	RedisPort int
	PGPort    int
}

// PortFor is the template helper {{.PortFor "api"}}.
func (c Context) PortFor(name string) int { return c.Ports[name] }

// Render expands one template string in ctx.
func (c Context) Render(text string) (string, error) {
	t, err := template.New("v").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, c); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// RenderService renders a service's command and env with Port set to the
// service's own port.
func (c Context) RenderService(svc Service) (argv []string, env map[string]string, err error) {
	c.Port = c.Ports[svc.Name]
	argv = make([]string, len(svc.Command))
	for i, a := range svc.Command {
		if argv[i], err = c.Render(a); err != nil {
			return nil, nil, fmt.Errorf("service %s command: %w", svc.Name, err)
		}
	}
	env = make(map[string]string, len(svc.Env))
	for k, v := range svc.Env {
		if env[k], err = c.Render(v); err != nil {
			return nil, nil, fmt.Errorf("service %s env %s: %w", svc.Name, k, err)
		}
	}
	return argv, env, nil
}

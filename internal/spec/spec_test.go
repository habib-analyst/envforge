package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndHash(t *testing.T) {
	p := writeSpec(t, `{"name":"pr-1","services":[{"name":"api","command":["/bin/true"],"health":"/health"}]}`)
	s, h1, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "pr-1" || len(s.Services) != 1 {
		t.Fatalf("unexpected spec: %+v", s)
	}
	if len(h1) != 64 {
		t.Fatalf("hash not sha256 hex: %q", h1)
	}
	// Any byte change flips the hash (drift baseline).
	if err := os.WriteFile(p, []byte(`{"name":"pr-1","services":[{"name":"api","command":["/bin/false"],"health":"/health"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, h2, _ := Load(p)
	if h1 == h2 {
		t.Fatal("hash did not change after spec edit")
	}
}

func TestLoadRejects(t *testing.T) {
	for _, body := range []string{
		`{"services":[]}`, // nothing declared
		`{"services":[{"name":"redis","command":["x"]}]}`,                                  // reserved name
		`{"services":[{"name":"a","command":["x"]},{"name":"a","command":["y"]}]}`,         // duplicate
		`{"services":[{"name":"a","command":[]}]}`,                                         // no command
		`{"services":[{"name":"a","command":["x"]}],"smoke":[{"service":"b","path":"/"}]}`, // bad smoke ref
		`{"services":[{"name":"a","command":["x"]}],"bogus":1}`,                            // unknown field
	} {
		if _, _, err := Load(writeSpec(t, body)); err == nil {
			t.Fatalf("spec accepted, want error: %s", body)
		}
	}
}

func TestRender(t *testing.T) {
	ctx := Context{
		Env: "pr-9", Dir: "/tmp/x", Appsvc: "/bin/appsvc",
		Ports: map[string]int{"api": 20010, "web": 20012, "redis": 20001},
	}
	ctx.RedisPort = 20001
	svc := Service{
		Name:    "web",
		Command: []string{"{{.Appsvc}}", "--port", "{{.Port}}"},
		Env:     map[string]string{"API": "http://127.0.0.1:{{.PortFor \"api\"}}", "CACHE": "{{.RedisPort}}"},
	}
	argv, env, err := ctx.RenderService(svc)
	if err != nil {
		t.Fatal(err)
	}
	if argv[2] != "20012" {
		t.Fatalf("self port render: %v", argv)
	}
	if env["API"] != "http://127.0.0.1:20010" || env["CACHE"] != "20001" {
		t.Fatalf("env render: %v", env)
	}
	if _, _, err := ctx.RenderService(Service{Name: "x", Command: []string{"{{.Nope}}"}}); err == nil {
		t.Fatal("unknown template field accepted")
	}
	if !strings.Contains(argv[0], "appsvc") {
		t.Fatalf("appsvc render: %v", argv)
	}
}

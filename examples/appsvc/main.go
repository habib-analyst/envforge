// appsvc is envforge's demo application service. It is deliberately
// small but behaves like a real service in the ways the experiments
// need:
//
//   - /health returns 200 once serving.
//   - /config returns its full environment (config read-back for drift
//     detection).
//   - /kv is a key-value endpoint backed by the env's Redis (REDIS_ADDR)
//     with an in-memory fallback; it tags responses with its env name so
//     cross-env contamination is directly observable.
//   - /call?svc=api calls another service's /health via the EF_<SVC>_URL
//     env var envforge injects (cross-service smoke test).
//   - It spawns a child process (sleep) so teardown must kill the whole
//     process tree, not just the leader. Set EF_NO_CHILD=1 to disable.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	mu  sync.Mutex
	mem = map[string]string{}
)

func envName() string { return os.Getenv("ENVFORGE_ENV") }
func svcName() string { return os.Getenv("ENVFORGE_SVC") }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// --- minimal Redis (RESP) client: just GET/SET ---

func redisCmd(args ...string) (string, error) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		return "", fmt.Errorf("no redis")
	}
	c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := c.Write([]byte(b.String())); err != nil {
		return "", err
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	switch {
	case strings.HasPrefix(line, "+"):
		return strings.TrimSpace(line[1:]), nil
	case strings.HasPrefix(line, "-"):
		return "", fmt.Errorf("redis: %s", strings.TrimSpace(line[1:]))
	case strings.HasPrefix(line, "$"):
		n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
		if n < 0 {
			return "", nil // nil bulk string: key absent
		}
		buf := make([]byte, n+2)
		total := 0
		for total < len(buf) {
			m, err := r.Read(buf[total:])
			if err != nil {
				return "", err
			}
			total += m
		}
		return string(buf[:n]), nil
	}
	return "", fmt.Errorf("unexpected redis reply %q", line)
}

func kvGet(k string) (string, string, bool) {
	if os.Getenv("REDIS_ADDR") != "" {
		if v, err := redisCmd("GET", k); err == nil {
			return v, "redis", v != ""
		}
	}
	mu.Lock()
	defer mu.Unlock()
	v, ok := mem[k]
	return v, "memory", ok
}

func kvSet(k, v string) string {
	if os.Getenv("REDIS_ADDR") != "" {
		if _, err := redisCmd("SET", k, v); err == nil {
			return "redis"
		}
	}
	mu.Lock()
	defer mu.Unlock()
	mem[k] = v
	return "memory"
}

func main() {
	if os.Getenv("EF_NO_CHILD") == "" {
		child := exec.Command("sleep", "3600")
		_ = child.Start() // inherits env + process group on purpose
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": svcName(), "env": envName()})
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]string{}
		for _, kv := range os.Environ() {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("/kv", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body struct{ K, V string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.K == "" {
				writeJSON(w, 400, map[string]string{"error": "want JSON {\"K\":..,\"V\":..}"})
				return
			}
			via := kvSet(body.K, body.V)
			writeJSON(w, 200, map[string]string{"ok": "1", "via": via, "env": envName(), "service": svcName()})
		default:
			k := r.URL.Query().Get("k")
			v, via, found := kvGet(k)
			writeJSON(w, 200, map[string]any{"value": v, "found": found, "via": via, "env": envName(), "service": svcName()})
		}
	})
	mux.HandleFunc("/call", func(w http.ResponseWriter, r *http.Request) {
		svc := r.URL.Query().Get("svc")
		key := "EF_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(svc)) + "_URL"
		base := os.Getenv(key)
		if base == "" {
			writeJSON(w, 400, map[string]string{"error": "unknown svc " + svc})
			return
		}
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(base + "/health")
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		var body map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&body)
		writeJSON(w, 200, map[string]any{"caller": svcName(), "callee": svc, "callee_status": resp.StatusCode, "callee_body": body})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		// Debug surface for the isolation experiment: sorted key list.
		var keys []string
		if os.Getenv("REDIS_ADDR") != "" {
			if v, err := redisCmd("KEYS", "*"); err == nil {
				_ = v
			}
		}
		mu.Lock()
		for k := range mem {
			keys = append(keys, k)
		}
		mu.Unlock()
		sort.Strings(keys)
		writeJSON(w, 200, map[string]any{"keys": keys, "env": envName()})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "appsvc:", err)
		os.Exit(1)
	}
}

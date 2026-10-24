// Package procutil provides the /proc-based process discovery envforge
// uses as its second, registry-independent source of truth. Every
// process envforge spawns carries ENVFORGE_ENV / ENVFORGE_SVC markers in
// its environment; scanning /proc for those markers finds env processes
// even ones the registry never recorded (e.g. spawned a microsecond
// before the provisioner was kill -9'd).
package procutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ProcInfo is one discovered process.
type ProcInfo struct {
	PID       int
	PGID      int
	StartTime uint64
	Env       string // ENVFORGE_ENV value
	Service   string // ENVFORGE_SVC value
}

// parseStat extracts (state, pgid, starttime) from /proc/<pid>/stat
// content. comm sits in parentheses and may contain spaces/parens, so
// fields are counted from the last ')': the remainder starts at field 3
// (state, offset 0); pgrp is field 5 (offset 2), starttime is field 22
// (offset 19).
func parseStat(content string) (state string, pgid int, startTime uint64, err error) {
	i := strings.LastIndex(content, ")")
	if i < 0 {
		return "", 0, 0, fmt.Errorf("malformed stat")
	}
	fields := strings.Fields(content[i+1:])
	if len(fields) < 20 {
		return "", 0, 0, fmt.Errorf("short stat")
	}
	p, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, 0, err
	}
	st, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return "", 0, 0, err
	}
	return fields[0], p, st, nil
}

// Stat returns a process's pgid and start time.
func Stat(pid int) (pgid int, startTime uint64, err error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, err
	}
	_, pgid, startTime, err = parseStat(string(raw))
	return pgid, startTime, err
}

// State returns a process's state letter (R, S, Z, ...).
func State(pid int) (string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	state, _, _, err := parseStat(string(raw))
	return state, err
}

// Alive reports whether pid exists, is the same process instance
// (start time match guards against PID reuse), and is not a zombie —
// a killed-but-not-yet-reaped child keeps its /proc entry and start
// time, but holds no sockets and runs no code.
func Alive(pid int, startTime uint64) bool {
	state, _, st, err := func() (string, int, uint64, error) {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			return "", 0, 0, err
		}
		return parseStat(string(raw))
	}()
	if err != nil || st != startTime {
		return false
	}
	return state != "Z" && state != "X" && state != "x"
}

// Environ returns a process's environment as a map.
func Environ(pid int) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, kv := range strings.Split(string(raw), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

// FindByMarker scans /proc for processes whose environment contains
// ENVFORGE_ENV. If envName is non-empty, only that env's processes are
// returned; otherwise every envforge-managed process is returned.
func FindByMarker(envName string) []ProcInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []ProcInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := Environ(pid)
		if err != nil {
			continue
		}
		name, ok := env["ENVFORGE_ENV"]
		if !ok || name == "" {
			continue
		}
		if envName != "" && name != envName {
			continue
		}
		pgid, st, err := Stat(pid)
		if err != nil {
			continue
		}
		out = append(out, ProcInfo{PID: pid, PGID: pgid, StartTime: st, Env: name, Service: env["ENVFORGE_SVC"]})
	}
	return out
}

// ListeningPorts returns the set of TCP ports in LISTEN state, from
// /proc/net/tcp and /proc/net/tcp6 (state 0A).
func ListeningPorts() map[int]bool {
	out := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			_, portHex, ok := strings.Cut(fields[1], ":")
			if !ok {
				continue
			}
			p, err := strconv.ParseInt(portHex, 16, 32)
			if err == nil {
				out[int(p)] = true
			}
		}
	}
	return out
}

// KillGroup sends sig to a whole process group.
func KillGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("refusing to signal pgid %d", pgid)
	}
	return syscall.Kill(-pgid, sig)
}

package procutil

import (
	"net"
	"os"
	"testing"
)

func TestParseStat(t *testing.T) {
	// comm with spaces and parens is the classic /proc parsing trap.
	line := "1234 (my weird) proc) S 100 1234 1234 0 -1 4194304 100 0 0 0 0 0 0 0 20 0 1 0 987654 3400000 100"
	state, pgid, st, err := parseStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if state != "S" || pgid != 1234 || st != 987654 {
		t.Fatalf("got pgid=%d start=%d", pgid, st)
	}
}

func TestAliveSelf(t *testing.T) {
	_, st, err := Stat(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(os.Getpid(), st) {
		t.Fatal("self not alive with correct start time")
	}
	if Alive(os.Getpid(), st+1) {
		t.Fatal("start-time guard failed (PID-reuse protection broken)")
	}
	if Alive(1<<30, 1) {
		t.Fatal("nonexistent pid reported alive")
	}
}

func TestMarkerFindsSelf(t *testing.T) {
	// Our own test process has no marker; FindByMarker("") may find
	// unrelated envforge procs on a busy box, so just assert the call
	// shape works and self is excluded by pid.
	for _, pi := range FindByMarker("") {
		if pi.PID == os.Getpid() {
			t.Fatal("FindByMarker returned the scanning process itself")
		}
	}
}

func TestListeningPorts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if !ListeningPorts()[port] {
		t.Fatalf("listening port %d not reported", port)
	}
}

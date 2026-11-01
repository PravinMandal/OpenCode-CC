package native

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadyPort(t *testing.T) {
	cases := map[string]int{
		`{"time":"x","event":"listening","port":41234}`: 41234,
		`{"event":"listening","port":0}`:                0,
		`{"event":"usage","port":41234}`:                0,
		`not json`:                                      0,
	}
	for line, want := range cases {
		if got := readyPort(line); got != want {
			t.Errorf("readyPort(%q) = %d, want %d", line, got, want)
		}
	}
}

func TestManagerDisabledWithoutDataDir(t *testing.T) {
	m := New(Options{})
	if m.Enabled() {
		t.Fatal("manager without a data dir should be disabled")
	}
	if _, _, err := m.Endpoint(); err == nil {
		t.Fatal("expected an error from a disabled manager")
	}
	// Must not panic or block when the server was never started.
	m.Stop()
}

func TestManagerLifecycle(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-bun")
	script := "#!/bin/sh\n" +
		`printf '%s\n' '{"time":"x","event":"listening","port":45678,"identity":"test"}'` + "\n" +
		"sleep 30\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	m := New(Options{DataDir: dir, BunPath: fake, ReadyTimeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Start(ctx)

	if port := m.Port(); port != 45678 {
		t.Fatalf("port = %d, want 45678", port)
	}
	endpoint, token, err := m.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "http://127.0.0.1:45678/v1/chat/completions" {
		t.Errorf("endpoint = %q", endpoint)
	}
	if len(token) < 16 {
		t.Errorf("token too short: %q", token)
	}

	// The scripts must be staged where the child can read them.
	for _, name := range []string{"transport.mjs", "capture.mjs"} {
		if _, err := os.Stat(filepath.Join(dir, "native", name)); err != nil {
			t.Errorf("staged %s: %v", name, err)
		}
	}

	m.Stop()
	if port := m.Port(); port != 0 {
		t.Errorf("port after Stop = %d, want 0", port)
	}
	if _, _, err := m.Endpoint(); err == nil {
		t.Error("expected an error after Stop")
	}
}

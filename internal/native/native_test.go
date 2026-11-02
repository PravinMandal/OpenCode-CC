package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// The free tier's quota is per public IP/64, and rotation is an operator-only
// decision. The transport must never probe, retry, or spawn the reset engine
// on a 429, and must default to the canonical tool prefix: extra client tools
// make the request look like an unknown client and turn every turn into a
// shape 429 followed by a retry.
func TestTransportHasNoAutomaticQuotaRecovery(t *testing.T) {
	raw, err := assets.ReadFile("transport.mjs")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, forbidden := range []string{
		"recoverQuota(", "stockProbe(", "canonicalOnlyBody(", "request-reset.mjs",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("transport must not contain %q: automatic quota recovery/retry is forbidden", forbidden)
		}
	}
	for _, required := range []string{
		"tools_suppressed",
		"BRIDGE_CLIENT_TOOL_EXTRAS",
		`headers.set("x-oc-reset", "manual")`,
	} {
		if !strings.Contains(src, required) {
			t.Errorf("transport is missing %q", required)
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

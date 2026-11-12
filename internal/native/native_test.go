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

// The free tier's quota is per public IP/64. Rotation is approved only through
// the shared request-reset engine, which owns the radio and the reset lock. A
// request may try a bounded number of established lease ids, ask for exactly
// one reset, and mint exactly one fresh id after that reset; it must not probe
// the budget, spin, or invent its own egress rotation.
func TestTransportQuotaRecoveryIsBounded(t *testing.T) {
	raw, err := assets.ReadFile("transport.mjs")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, forbidden := range []string{
		"stockProbe(", "canonicalOnlyBody(",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("transport must not contain %q", forbidden)
		}
	}
	for _, required := range []string{
		"tools_suppressed",
		"tools_unavailable_pruned",
		"BRIDGE_CLIENT_TOOL_EXTRAS",
		"recoverQuota(",
		"request-reset.mjs",
		`RESET_ALLOW_AUTOMATIC: "1"`,
		// One reset and one fresh id after it; the lease pool is the only other
		// fallback and is bounded. The deadline stops a request that would spin,
		// and the reset wait is raced against that deadline so a 600s reset
		// cannot pin every inflight slot behind a 270s request budget.
		"if (!resetDone)",
		"if (!freshAfterResetDone)",
		"leaseFallbacks < LEASE_FALLBACK_ATTEMPTS",
		"Math.min(2, LEASE_POOL_LIMIT)",
		"ensureIdentityForCredential(",
		"identity_refresh_not_current",
		"leaseAccount",
		"identity_account_mismatch",
		"pruneOldToolResults",
		"context_pruned",
		"x-bridge-credential",
		"quota_reset_skipped_deadline",
		"Promise.race([recoverQuota(",
		"postResetGrace",
		"upstream_deadline",
		"upstream request deadline exceeded",
		`passThrough429(response, text, "failed")`,
		`passThrough429(response, text, "manual")`,
		`passThrough429(response, text, "shape")`,
		"automaticRecovery: true",
		"x-oc-native",
		"tool_index_rejected",
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
	for _, name := range []string{"transport.mjs", "capture.mjs", "gate.mjs"} {
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

// TestNativeTransportReviewFixes pins the fixes that are not reachable from
// Go: prune every dead canonical tool (not just the trailing suffix), rewrite
// the <env> block safely for paths with spaces/$ , and reclaim a stale reset
// lock instead of waiting out the full budget.
func TestNativeTransportReviewFixes(t *testing.T) {
	raw, err := assets.ReadFile("transport.mjs")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, required := range []string{
		// Prune all dead tools, not a prefix/suffix slice.
		"canonicalTools.filter((t, i) => runnableFlags[i])",
		"canonicalTools.filter((t, i) => !runnableFlags[i])",
		// Env rewrite: trust only system/developer, tolerate spaces, use
		// function replacers so $ in a path is not a replacement pattern.
		`m?.role !== "system" && m?.role !== "developer"`,
		"function cleanCwd(raw)",
		"block => block",
		"prefix) => prefix + cwd",
		"prefix) => prefix + repo",
		"BRIDGE_REWRITE_ENV",
		// Stale reset lock: PID liveness plus heartbeat age.
		"function resetLockStale()",
		"function resetLockOwnerAlive()",
		"RESET_LOCK_STALE_MS",
		"process.kill(pid, 0)",
		// Trace bodies are private.
		"writeFile(tracePath, body, { mode: 0o600 })",
		// Alias parity with the Go resolver.
		`exec_command: "shell"`,
		`view_file: "read"`,
		`create_file: "write"`,
		`str_replace: "edit"`,
	} {
		if !strings.Contains(src, required) {
			t.Errorf("transport is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"lastRunnable",             // suffix-only pruning
		"`$1${cwd}`",               // replacement-pattern interpolation
		"Bun.write(join(TRACE_DIR", // world-readable trace
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("transport still contains %q", forbidden)
		}
	}

	capture, err := assets.ReadFile("capture.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"await rename(tmp, target)",
		"mode: 0o600",
		"await rename(tmp, primary)",
	} {
		if !strings.Contains(string(capture), required) {
			t.Errorf("capture.mjs is missing %q", required)
		}
	}
}

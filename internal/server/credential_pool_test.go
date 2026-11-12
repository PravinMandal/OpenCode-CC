package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cfgpkg "github.com/PravinMandal/OpenCode-CC/internal/config"
)

type scriptedRoundTripper struct {
	mu     sync.Mutex
	calls  []string
	xapi   []string
	status []int
	bodies []string
}

func (s *scriptedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bearerToken(r.Header.Get("Authorization"))
	s.calls = append(s.calls, key)
	s.xapi = append(s.xapi, r.Header.Get("x-api-key"))
	idx := len(s.calls) - 1
	status := http.StatusOK
	if idx < len(s.status) {
		status = s.status[idx]
	}
	body := ""
	if idx < len(s.bodies) {
		body = s.bodies[idx]
	}
	return &http.Response{
		StatusCode:    status,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}, nil
}

func failoverRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"model":"x"}`))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"model":"x"}`)), nil
	}
	return req
}

func failoverConfig(keys ...string) *cfgpkg.Config {
	up := make([]cfgpkg.Upstream, 0, len(keys))
	for _, k := range keys {
		up = append(up, cfgpkg.Upstream{
			BaseURL: "https://opencode.ai/inference",
			APIKey:  k,
			Enabled: true,
		})
	}
	return &cfgpkg.Config{Upstreams: up}
}

// TestCredentialFailoverOnExpiredKey is the same-account key pool contract:
// an expired key is skipped, the next key serves the same request, and the
// request body is replayed intact.
func TestCredentialFailoverOnExpiredKey(t *testing.T) {
	rt := &scriptedRoundTripper{
		status: []int{http.StatusUnauthorized, http.StatusOK},
		bodies: []string{
			`{"error":{"type":"api_key_expired","message":"api key expired"}}`,
			`{"ok":true}`,
		},
	}
	pool := newCredentialPool(failoverConfig("expired-key", "good-key"))
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer expired-key")
	req.Header.Set("x-api-key", "expired-key")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(rt.calls) != 2 || rt.calls[0] != "expired-key" || rt.calls[1] != "good-key" {
		t.Fatalf("calls = %v, want [expired-key good-key]", rt.calls)
	}
	if len(rt.xapi) != 2 || rt.xapi[1] != "good-key" {
		t.Fatalf("x-api-key = %v, want the rotated key on the retry", rt.xapi)
	}
}

// TestCredentialFailoverOnModelList covers the GET catalog path: an expired
// key must not hide every model from the client.
func TestCredentialFailoverOnModelList(t *testing.T) {
	rt := &scriptedRoundTripper{status: []int{http.StatusUnauthorized, http.StatusOK}, bodies: []string{`{}`, `{"data":[]}`}}
	pool := newCredentialPool(failoverConfig("expired-key", "good-key"))
	tr := newCredentialFailoverTransport(rt, pool)
	req, _ := http.NewRequest(http.MethodGet, "https://opencode.ai/inference/v1/models", nil)
	req.Header.Set("Authorization", "Bearer expired-key")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(rt.calls) != 2 {
		t.Fatalf("status=%d calls=%v, want 200 via good-key", resp.StatusCode, rt.calls)
	}
}

// TestCredentialFailoverRotatesOn429 is the last-resort quota contract: the
// free-tier sidecar has already attempted an egress reset by the time the 429
// reaches this transport, so the next key is tried. Rotating must keep the
// request body and the x-api-key twin intact.
func TestCredentialFailoverRotatesOn429(t *testing.T) {
	rt := &scriptedRoundTripper{
		status: []int{http.StatusTooManyRequests, http.StatusOK},
		bodies: []string{
			`{"error":{"type":"FreeUsageLimitError","message":"free usage limit reached"}}`,
			`{"ok":true}`,
		},
	}
	pool := newCredentialPool(failoverConfig("key-one", "key-two"))
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer key-one")
	req.Header.Set("x-api-key", "key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the second key", resp.StatusCode)
	}
	if len(rt.calls) != 2 || rt.calls[0] != "key-one" || rt.calls[1] != "key-two" {
		t.Fatalf("calls = %v, want [key-one key-two]", rt.calls)
	}
	if len(rt.xapi) != 2 || rt.xapi[0] != "key-one" || rt.xapi[1] != "key-two" {
		t.Fatalf("x-api-key = %v, want the key to rotate in lockstep", rt.xapi)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q, want the successful retry body", body)
	}
}

// TestCredentialFailover429IsBounded proves a fully limited pool surfaces the
// real 429 after exactly one attempt per key instead of looping or replaying a
// consumed body.
func TestCredentialFailover429IsBounded(t *testing.T) {
	rt := &scriptedRoundTripper{
		status: []int{http.StatusTooManyRequests, http.StatusTooManyRequests, http.StatusTooManyRequests},
		bodies: []string{`{"error":"a"}`, `{"error":"b"}`, `{"error":"c"}`},
	}
	pool := newCredentialPool(failoverConfig("key-one", "key-two", "key-three"))
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if len(rt.calls) != 3 {
		t.Fatalf("calls = %d, want one attempt per key", len(rt.calls))
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"error":"c"}` {
		t.Fatalf("body = %q, want the last key's real error", body)
	}
}

// TestCredentialFailoverIgnoresQuota403 covers the billing boundary: a 403
// that says quota/billing is not an expired credential, so key rotation would
// only burn the remaining keys and hide the real error.
func TestCredentialFailoverIgnoresQuota403(t *testing.T) {
	rt := &scriptedRoundTripper{
		status: []int{http.StatusForbidden},
		bodies: []string{`{"error":{"type":"insufficient_quota","message":"billing quota exhausted for this workspace"}}`},
	}
	pool := newCredentialPool(failoverConfig("key-one", "key-two"))
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(rt.calls) != 1 {
		t.Fatalf("status=%d calls=%v, want the quota 403 surfaced without rotation", resp.StatusCode, rt.calls)
	}
}

// TestCredentialFailoverRestoresLargeErrorBody ensures a 403 page larger than
// the classification peek is not truncated on its way to the client.
func TestCredentialFailoverRestoresLargeErrorBody(t *testing.T) {
	big := strings.Repeat("denied ", 20_000) // ~140 KiB, past the 64 KiB peek
	rt := &scriptedRoundTripper{
		status: []int{http.StatusForbidden},
		bodies: []string{big},
	}
	pool := newCredentialPool(failoverConfig("key-one"))
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if len(body) != len(big) {
		t.Fatalf("body length = %d, want %d (error body truncated)", len(body), len(big))
	}
}

// TestCredentialFailoverLimitedKeyRecovers proves a 429 is not treated as a
// dead key: once the short limited window expires the same key serves traffic
// again without a restart.
func TestCredentialFailoverLimitedKeyRecovers(t *testing.T) {
	rt := &scriptedRoundTripper{status: []int{http.StatusOK}, bodies: []string{`{"ok":true}`}}
	pool := newCredentialPool(failoverConfig("key-one"))
	now := time.Now()
	pool.now = func() time.Time { return now }
	pool.markLimited("key-one")
	if !pool.isLimited("key-one") {
		t.Fatal("key should be limited")
	}
	now = now.Add(credentialLimitedTTL + time.Second)
	if pool.isLimited("key-one") {
		t.Fatal("limited key must recover after the short TTL")
	}
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer key-one")
	resp, err := tr.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%v err=%v, want 200 after recovery", resp, err)
	}
	resp.Body.Close()
}

// TestCredentialPoolNextCyclic covers the free-lane rotation contract: limited
// keys stay in the cycle (a new egress can clear them), dead keys are skipped,
// and a single-key pool returns that key so it can retry after a reset.
func TestCredentialPoolNextCyclic(t *testing.T) {
	pool := newCredentialPool(failoverConfig("key-one", "key-two"))
	pool.markLimited("key-one")
	next, ok := pool.nextCyclic("key-two", cfgpkg.DefaultUpstreamBase)
	if !ok || next != "key-one" {
		t.Fatalf("nextCyclic = %q, %v; want limited key-one back in the cycle", next, ok)
	}
	pool.markDead("key-one")
	next, ok = pool.nextCyclic("key-two", cfgpkg.DefaultUpstreamBase)
	if !ok || next != "key-two" {
		t.Fatalf("nextCyclic = %q, %v; want the only live key", next, ok)
	}
	single := newCredentialPool(failoverConfig("only"))
	next, ok = single.nextCyclic("only", cfgpkg.DefaultUpstreamBase)
	if !ok || next != "only" {
		t.Fatalf("single-key nextCyclic = %q, %v; want only", next, ok)
	}
	single.markDead("only")
	if _, ok := single.nextCyclic("only", cfgpkg.DefaultUpstreamBase); ok {
		t.Fatal("dead-only pool must not return a key")
	}
}

// TestCredentialProbeDoesNotMutatePool keeps warm-up probes from retiring a
// good key when the probe itself is unauthenticated or rate limited.
func TestCredentialProbeDoesNotMutatePool(t *testing.T) {
	rt := &scriptedRoundTripper{status: []int{http.StatusUnauthorized}, bodies: []string{`{"error":"api key expired"}`}}
	pool := newCredentialPool(failoverConfig("key-one", "key-two"))
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/models")
	req.Header.Set("Authorization", "Bearer key-one")
	req = markCredentialProbe(req)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	resp.Body.Close()
	if pool.isDead("key-one") || len(rt.calls) != 1 {
		t.Fatalf("probe mutated the pool: dead=%v calls=%v", pool.isDead("key-one"), rt.calls)
	}
}

// TestCredentialFailoverSkipsKnownDeadKey ensures a key rejected earlier is
// not tried first on the next request.
func TestCredentialFailoverSkipsKnownDeadKey(t *testing.T) {
	rt := &scriptedRoundTripper{status: []int{http.StatusOK}, bodies: []string{`{"ok":true}`}}
	pool := newCredentialPool(failoverConfig("dead-key", "live-key"))
	pool.markDead("dead-key")
	tr := newCredentialFailoverTransport(rt, pool)
	req := failoverRequest(t, "https://opencode.ai/inference/v1/chat/completions")
	req.Header.Set("Authorization", "Bearer dead-key")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if len(rt.calls) != 1 || rt.calls[0] != "live-key" {
		t.Fatalf("calls = %v, want [live-key]", rt.calls)
	}
}

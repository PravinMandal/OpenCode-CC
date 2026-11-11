package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cfgpkg "github.com/PravinMandal/OpenCode-CC/internal/config"
)

type scriptedRoundTripper struct {
	mu     sync.Mutex
	calls  []string
	status []int
	bodies []string
}

func (s *scriptedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := bearerToken(r.Header.Get("Authorization"))
	s.calls = append(s.calls, key)
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
}

// TestCredentialFailoverIgnores429 is the boundary: a quota error must never
// rotate keys or be interpreted as an expired credential.
func TestCredentialFailoverIgnores429(t *testing.T) {
	rt := &scriptedRoundTripper{
		status: []int{http.StatusTooManyRequests},
		bodies: []string{`{"error":{"type":"FreeUsageLimitError","message":"free usage limit reached"}}`},
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
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if len(rt.calls) != 1 {
		t.Fatalf("429 triggered %d calls; failover must never react to quota", len(rt.calls))
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

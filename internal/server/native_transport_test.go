package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeNativeEndpoint struct {
	url    string
	token  string
	enable bool
}

func (f fakeNativeEndpoint) Enabled() bool { return f.enable }

func (f fakeNativeEndpoint) Endpoint() (string, string, error) { return f.url, f.token, nil }

func nativeTestRequest(t *testing.T) *http.Request {
	t.Helper()
	body := `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "https://opencode.ai/inference/v1/chat/completions", strings.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }
	return req
}

// TestNativeFreeLaneRotatesOn429AfterReset is the free-lane quota contract:
// the sidecar has already attempted the egress reset (x-oc-reset: failed), so
// the proxy rotates to the next key and replays the body. The retry must not
// suppress the reset: quota is IP-wide, so each key gets its own attempt once
// the engine's cooldown expires.
func TestNativeFreeLaneRotatesOn429AfterReset(t *testing.T) {
	zeroFreeLaneCycleDelays(t)
	var (
		mu      sync.Mutex
		creds   []string
		noReset []string
	)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		creds = append(creds, r.Header.Get("x-bridge-credential"))
		noReset = append(noReset, r.Header.Get("x-bridge-no-reset"))
		n := len(creds)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("x-oc-reset", "failed")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"FreeUsageLimitError"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer sidecar.Close()

	pool := newCredentialPool(failoverConfig("key-one", "key-two"))
	tr := newNativeTransport(&scriptedRoundTripper{}, fakeNativeEndpoint{
		url: sidecar.URL + "/v1/chat/completions", token: "tok", enable: true,
	}, pool)
	req := nativeTestRequest(t)
	req.Header.Set("Authorization", "Bearer key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(creds) != 2 || creds[0] != "key-one" || creds[1] != "key-two" {
		t.Fatalf("credentials = %v, want [key-one key-two]", creds)
	}
	if len(noReset) != 2 || noReset[0] != "" || noReset[1] != "" {
		t.Fatalf("x-bridge-no-reset = %v, want no suppression: every key attempt may reset", noReset)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q, want the successful retry", body)
	}
}

// TestNativeFreeLaneDoesNotRotateOnManual429 keeps non-quota 429s (the sidecar
// marks them x-oc-reset: manual) from burning every key.
func TestNativeFreeLaneDoesNotRotateOnManual429(t *testing.T) {
	var mu sync.Mutex
	var creds []string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		creds = append(creds, r.Header.Get("x-bridge-credential"))
		mu.Unlock()
		w.Header().Set("x-oc-reset", "manual")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
	}))
	defer sidecar.Close()

	pool := newCredentialPool(failoverConfig("key-one", "key-two"))
	tr := newNativeTransport(&scriptedRoundTripper{}, fakeNativeEndpoint{
		url: sidecar.URL + "/v1/chat/completions", token: "tok", enable: true,
	}, pool)
	req := nativeTestRequest(t)
	req.Header.Set("Authorization", "Bearer key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if len(creds) != 1 {
		t.Fatalf("credentials = %v, want a single attempt for a non-quota 429", creds)
	}
}

// TestNativeFreeLaneCyclesKeysUntilSuccess proves the free lane does not stop
// after one pass over the pool: a reset may land later, so rotation wraps back
// to the first key and keeps trying until a key succeeds.
func TestNativeFreeLaneCyclesKeysUntilSuccess(t *testing.T) {
	zeroFreeLaneCycleDelays(t)
	var mu sync.Mutex
	var creds []string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		creds = append(creds, r.Header.Get("x-bridge-credential"))
		n := len(creds)
		mu.Unlock()
		if n < 5 {
			w.Header().Set("x-oc-reset", "failed")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"FreeUsageLimitError"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer sidecar.Close()

	pool := newCredentialPool(failoverConfig("key-one", "key-two", "key-three"))
	tr := newNativeTransport(&scriptedRoundTripper{}, fakeNativeEndpoint{
		url: sidecar.URL + "/v1/chat/completions", token: "tok", enable: true,
	}, pool)
	req := nativeTestRequest(t)
	req.Header.Set("Authorization", "Bearer key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the second pass", resp.StatusCode)
	}
	want := []string{"key-one", "key-two", "key-three", "key-one", "key-two"}
	if len(creds) != len(want) {
		t.Fatalf("credentials = %v, want %v", creds, want)
	}
	for i := range want {
		if creds[i] != want[i] {
			t.Fatalf("credentials = %v, want %v", creds, want)
		}
	}
}

// TestNativeFreeLaneCycleIsBounded proves a permanently limited upstream
// cannot pin a request forever: the cycle stops at the attempt ceiling and
// returns the real 429 rather than looping without end.
func TestNativeFreeLaneCycleIsBounded(t *testing.T) {
	zeroFreeLaneCycleDelays(t)
	var mu sync.Mutex
	var creds []string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		creds = append(creds, r.Header.Get("x-bridge-credential"))
		mu.Unlock()
		w.Header().Set("x-oc-reset", "skipped")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"FreeUsageLimitError"}}`)
	}))
	defer sidecar.Close()

	pool := newCredentialPool(failoverConfig("key-one", "key-two", "key-three"))
	tr := newNativeTransport(&scriptedRoundTripper{}, fakeNativeEndpoint{
		url: sidecar.URL + "/v1/chat/completions", token: "tok", enable: true,
	}, pool)
	req := nativeTestRequest(t)
	req.Header.Set("Authorization", "Bearer key-one")

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if len(creds) != freeLaneMaxAttempts {
		t.Fatalf("credentials = %d attempts, want %d", len(creds), freeLaneMaxAttempts)
	}
	keys := []string{"key-one", "key-two", "key-three"}
	for i, got := range creds {
		if want := keys[i%len(keys)]; got != want {
			t.Fatalf("attempt %d credential = %q, want %q", i, got, want)
		}
	}
}

func zeroFreeLaneCycleDelays(t *testing.T) {
	t.Helper()
	oldDelay, oldMax := freeLaneCycleDelay, freeLaneMaxDelay
	freeLaneCycleDelay, freeLaneMaxDelay = 0, 0
	t.Cleanup(func() {
		freeLaneCycleDelay, freeLaneMaxDelay = oldDelay, oldMax
	})
}

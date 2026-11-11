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
// the proxy rotates to the next key, replays the body, and tells the sidecar
// not to cycle the radio again for that retry.
func TestNativeFreeLaneRotatesOn429AfterReset(t *testing.T) {
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
	if len(noReset) != 2 || noReset[0] != "" || noReset[1] != "1" {
		t.Fatalf("x-bridge-no-reset = %v, want [\"\" \"1\"] (reset once, then rotate only)", noReset)
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

// TestNativeFreeLane429IsBounded proves an exhausted pool stops after one
// attempt per key instead of looping.
func TestNativeFreeLane429IsBounded(t *testing.T) {
	var mu sync.Mutex
	var creds []string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		creds = append(creds, r.Header.Get("x-bridge-credential"))
		n := len(creds)
		mu.Unlock()
		w.Header().Set("x-oc-reset", "skipped")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"limited `+string(rune('0'+n))+`"}`)
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
	if len(creds) != 3 {
		t.Fatalf("credentials = %v, want one attempt per key", creds)
	}
}

package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PravinMandal/OpenCode-CC/internal/config"
)

// stallOnceUpstream stalls past any short header timeout on the first request
// (closing the connection so the client sees a timeout, mimicking Zen's
// free-tier "never returns response headers" failure) and answers normally on
// the second.
func stallOnceUpstream(t *testing.T, firstStall time.Duration) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&hits, 1) == 1 {
			// Hijack and close without a response: a hard transport failure
			// that isTimeoutErr() classifies as retryable.
			if hj, ok := w.(http.Hijacker); ok {
				c, _, err := hj.Hijack()
				if err == nil {
					c.Close()
					return
				}
			}
			time.Sleep(firstStall)
		}
		body := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

func cfgFor(t *testing.T, base string) *config.Config {
	t.Helper()
	c := config.Default()
	c.Upstreams = []config.Upstream{{BaseURL: base, APIKey: "k", Enabled: true}}
	return c
}

// TestChatCompletionsRetriesUpstreamStall covers the reported failure:
// cli-proxyapi saw "auth_unavailable ... timeout awaiting response headers".
// The chat path must recover on retry instead of returning 502.
func TestChatCompletionsRetriesUpstreamStall(t *testing.T) {
	ts, hits := stallOnceUpstream(t, 0)
	srv := New(cfgFor(t, ts.URL), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.OpenAIProxy()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "recovered") {
		t.Errorf("body = %q, want the retried upstream answer", rr.Body.String())
	}
	if got := atomic.LoadInt64(hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (one failure + one retry)", got)
	}
}

// TestMessagesRetriesUpstreamStall is the Anthropic path — the one
// cli-proxyapi actually exercises for space-bunny-oc.
func TestMessagesRetriesUpstreamStall(t *testing.T) {
	ts, hits := stallOnceUpstream(t, 0)
	srv := New(cfgFor(t, ts.URL), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-fable-5-1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Proxy()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt64(hits); got < 2 {
		t.Errorf("upstream hits = %d, want >= 2 (retry after stall)", got)
	}
}

// TestResponsesRetriesUpstreamStall covers POST /v1/responses (Codex clients).
func TestResponsesRetriesUpstreamStall(t *testing.T) {
	ts, hits := stallOnceUpstream(t, 0)
	srv := New(cfgFor(t, ts.URL), nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ResponsesProxy()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := atomic.LoadInt64(hits); got < 2 {
		t.Errorf("upstream hits = %d, want >= 2 (retry after stall)", got)
	}
}

// TestRetryStopsWhenClientGone: once the downstream client disconnects, the
// proxy must stop hammering upstream.
func TestRetryStopsWhenClientGone(t *testing.T) {
	var hits int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		io.WriteString(w, `{}`)
	}))
	defer ts.Close()

	srv := New(cfgFor(t, ts.URL), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	cancel() // client already gone

	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.OpenAIProxy()(rr, req)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
}

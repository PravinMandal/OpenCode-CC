package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PravinMandal/OpenCode-CC/internal/config"
)

// rawHTTPTransport unwraps the credential-failover and free-tier wrappers so
// transport-level tests can assert the underlying connection pool policy.
func rawHTTPTransport(tr http.RoundTripper) *http.Transport {
	switch v := tr.(type) {
	case *http.Transport:
		return v
	case *credentialFailoverTransport:
		return rawHTTPTransport(v.base)
	case *nativeTransport:
		return rawHTTPTransport(v.base)
	default:
		return nil
	}
}

// TestUpstreamIdleTimeoutKeepsConnectionsWarm locks in the TTFT fix: the
// shared transport's IdleConnTimeout must be long enough that a request after
// a long thinking gap reuses a pooled socket instead of paying a fresh
// TCP+TLS handshake (measured ~610ms of added time-to-first-byte).
func TestUpstreamIdleTimeoutKeepsConnectionsWarm(t *testing.T) {
	srv := New(nil, nil)
	tr := rawHTTPTransport(srv.httpClient.Transport)
	if tr == nil {
		t.Fatalf("expected *http.Transport, got %T", srv.httpClient.Transport)
	}
	if tr.IdleConnTimeout < time.Hour {
		t.Errorf("IdleConnTimeout = %s, want >= 1h so idle gaps skip the cold handshake", tr.IdleConnTimeout)
	}
	if keepAliveInterval >= tr.IdleConnTimeout {
		t.Errorf("keepAliveInterval %s must be below IdleConnTimeout %s", keepAliveInterval, tr.IdleConnTimeout)
	}
	if tr.MaxIdleConnsPerHost < 16 {
		t.Errorf("MaxIdleConnsPerHost = %d, want >= 16", tr.MaxIdleConnsPerHost)
	}
	// Without an explicit dialer the transport falls back to net/http's
	// default, whose 30s TCP KeepAlive is shorter than our idle window.
	if tr.DialContext == nil {
		t.Error("DialContext is nil: transport would use net/http defaults")
	}
}

// TestKeepAlivePingsReusePooledConnection verifies the pinger keeps a socket
// in the idle pool (its whole purpose) instead of opening throwaway
// connections: 3 pings to one upstream must cost exactly 1 TCP dial.
func TestKeepAlivePingsReusePooledConnection(t *testing.T) {
	var dials, hits int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	srv := New(nil, nil)
	tr := rawHTTPTransport(srv.httpClient.Transport)
	base := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		atomic.AddInt64(&dials, 1)
		return base(ctx, network, addr)
	}

	for i := 0; i < 3; i++ {
		pingUpstream(srv.httpClient, ts.URL, "k")
	}

	if got := atomic.LoadInt64(&hits); got != 3 {
		t.Errorf("upstream hits = %d, want 3", got)
	}
	if got := atomic.LoadInt64(&dials); got != 1 {
		t.Errorf("TCP dials = %d, want 1 (pinging must reuse one pooled socket)", got)
	}
}

// TestKeepAliveUsesOpenCodeUA guards the free-tier gate: the pinger must
// present the same User-Agent shape as real traffic, or Zen rejects it and the
// pool never actually warms.
func TestKeepAliveUsesOpenCodeUA(t *testing.T) {
	got := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("User-Agent"):
		default:
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	srv := New(nil, nil)
	pingUpstream(srv.httpClient, ts.URL, "k")
	select {
	case ua := <-got:
		if !strings.HasPrefix(ua, "opencode/") {
			t.Errorf("keep-alive User-Agent = %q, want opencode/... shape", ua)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("keep-alive ping never reached upstream")
	}
}

// TestKeepAliveSendsAuthorization makes sure the pinger authenticates, so a
// 401 cannot silently leave the pool cold in production.
func TestKeepAliveSendsAuthorization(t *testing.T) {
	got := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("Authorization"):
		default:
		}
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	srv := New(nil, nil)
	pingUpstream(srv.httpClient, ts.URL, "sekret")
	select {
	case auth := <-got:
		if auth != "Bearer sekret" {
			t.Errorf("Authorization = %q, want %q", auth, "Bearer sekret")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("keep-alive ping never reached upstream")
	}
}

// TestKeepAliveHonorsTimeout ensures a slow/hung upstream cannot leak a
// goroutine or pile up on the shared transport.
func TestKeepAliveHonorsTimeout(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(release)

	srv := New(nil, nil)
	done := make(chan struct{})
	go func() {
		pingUpstream(srv.httpClient, ts.URL, "k")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(keepAliveTimeout + 5*time.Second):
		t.Fatalf("pingUpstream did not honor its %s timeout", keepAliveTimeout)
	}
}

// TestStartKeepAliveIsIdempotentAndStops guards double-start and shutdown.
func TestStartKeepAliveIsIdempotentAndStops(t *testing.T) {
	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{{BaseURL: "http://127.0.0.1:1", APIKey: "k", Enabled: true}}
	srv := New(cfg, nil)

	srv.StartKeepAlive()
	srv.StartKeepAlive() // must not spawn a second loop

	// Shutdown closes done; the loop must observe it and exit. If it did not,
	// this test would leak (caught by goleak-style checks in CI).
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-srv.done:
	default:
		t.Fatal("Shutdown did not close done")
	}
}

// TestPingUpstreamsSkipsDisabledUpstreams makes sure a disabled upstream in
// the pool is not pinged.
func TestPingUpstreamsSkipsDisabledUpstreams(t *testing.T) {
	var hits int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.Upstreams = []config.Upstream{
		{BaseURL: ts.URL, APIKey: "k", Enabled: false},
		{BaseURL: ts.URL, APIKey: "k", Enabled: true},
	}
	srv := New(cfg, nil)
	srv.pingUpstreams()

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1 (disabled upstream must be skipped)", got)
	}
}

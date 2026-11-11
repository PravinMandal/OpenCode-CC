package server

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/PravinMandal/OpenCode-CC/internal/api"
	"github.com/PravinMandal/OpenCode-CC/internal/config"
	"github.com/PravinMandal/OpenCode-CC/internal/native"
	"github.com/PravinMandal/OpenCode-CC/internal/proxy"
	"github.com/PravinMandal/OpenCode-CC/internal/store"
)

// Server is the top-level HTTP server. It owns the config, store, and the
// underlying *http.Server so it can be shut down cleanly.
type Server struct {
	cfg          *config.Config
	store        *store.Store
	httpClient   *http.Client
	native       *native.Manager
	srv          *http.Server
	effortLevels *EffortLevelsCache
	cooldowns    *cooldownTracker
	credPool     *credentialPool

	// done is closed by Shutdown to stop background loops (keep-alive pinger).
	done chan struct{}
	// keepAliveOnce guards against starting the pinger twice.
	keepAliveOnce sync.Once
	// maintenanceOnce guards against starting the retention loop twice.
	maintenanceOnce sync.Once
	// stopOnce guards closing done exactly once.
	stopOnce sync.Once
	// mu guards srv against the Start/Shutdown race: a signal can arrive
	// while Start is still bringing the sidecar up.
	mu sync.Mutex
	// nativeHealth caches the sidecar's /health verdict: "port open" is not
	// the same as "identity captured", and routing a free request to a
	// sidecar that answers 503 has no fallback.
	nativeHealthMu sync.Mutex
	nativeHealthAt time.Time
	nativeHealthOK bool
}

// New constructs a Server. The store may be nil if request logging is disabled.
func New(cfg *config.Config, st *store.Store) *Server {
	dataDir := ""
	if cfg != nil {
		dataDir = cfg.DataDir()
	}
	s := &Server{
		cfg:          cfg,
		store:        st,
		effortLevels: LoadEffortLevelsCache(dataDir),
		cooldowns:    newCooldownTracker(),
		done:         make(chan struct{}),
		native:       native.New(native.Options{DataDir: dataDir}),
		httpClient: &http.Client{
			Timeout: 0, // streaming: no global timeout (per-request override used)
			Transport: &http.Transport{
				// Honor explicit HTTP(S)_PROXY and NO_PROXY settings, including
				// an opt-in local WARP proxy. Loopback remains direct.
				Proxy:        http.ProxyFromEnvironment,
				MaxIdleConns: 256,
				// Concurrency headroom so agent bursts (parallel subagents +
				// retries) reuse pooled sockets instead of opening new ones.
				MaxIdleConnsPerHost: 32,
				// Explicit dialer. Without this the transport silently uses
				// net/http's default (30s KeepAlive, 30s dial timeout), which
				// is shorter than our idle window and lets the OS reap a warm
				// socket early. KeepAlive outlives upstreamIdleTimeout so a
				// pooled connection is never torn down underneath the pool;
				// Timeout bounds the TCP connect on a cold connection.
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 75 * time.Minute,
				}).DialContext,
				// Cold-connection cliff (measured Oct 2026): a request arriving
				// after >90s of upstream idleness must re-handshake TLS, which
				// costs ~600ms of TTFT (full handshake measured at 562ms; the
				// edge TTFB itself is only ~450ms). Paired A/B against the
				// upstream showed warm pooled connections are ~610ms faster
				// per request than fresh ones, and that HTTP/1.1 vs HTTP/2 is
				// irrelevant while warm (498ms vs 520ms TTFB) - so a long idle
				// timeout is strictly better than any protocol change.
				// keepWarmUpstreams() pings the pool well inside this window so
				// sockets stay usable across long thinking gaps.
				IdleConnTimeout: upstreamIdleTimeout,
				// Bound the wait for upstream response headers on every
				// request, streams included.
				ResponseHeaderTimeout: 30 * time.Second,
				// HTTP/1.1 only upstream. Sep 2026 forensics (SIGQUIT
				// goroutine dump) proved the wedge: one HTTP/2 connection
				// to a Cloudflare edge stalled in flow control (peer
				// stopped sending WINDOW_UPDATE; 75KB stuck in Send-Q) and
				// every route sharing this client queued on it forever —
				// Go's auto-H2 transport exposes no read-idle/ping
				// timeout knobs, and no request timeout can fire on a
				// stream. H1 isolates a stall to its own connection, which
				// the timeouts bound; fresh requests open fresh conns.
				ForceAttemptHTTP2: false,
				TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
			},
		},
	}
	if cfg != nil {
		s.credPool = newCredentialPool(cfg)
		paid := newCredentialFailoverTransport(s.httpClient.Transport, s.credPool)
		s.httpClient.Transport = newNativeTransport(paid, s.native, s.credPool)
	} else {
		s.httpClient.Transport = newNativeTransport(s.httpClient.Transport, s.native, nil)
	}
	return s
}

// Handler returns the root http.Handler wiring all routes. panelAssets serves
// the embedded SPA (nil = no embedded assets, e.g. dev mode).
func (s *Server) Handler(panelAssets http.FileSystem, panelMux http.Handler) http.Handler {
	mux := http.NewServeMux()

	// ---- Anthropic-compatible endpoints (gated by client API key) ----
	mux.Handle("/v1/messages", s.clientAuth(s.Proxy()))
	mux.Handle("/v1/messages/count_tokens", s.clientAuth(s.CountTokens()))
	// OpenAI-compatible clients can use the same server directly. Requests are
	// forwarded to Zen after applying the configured model mapping.
	mux.Handle("/v1/chat/completions", s.clientAuth(s.OpenAIProxy()))
	// Codex uses the Responses API. Zen /go only exposes Chat Completions, so
	// this route performs a bidirectional Responses <-> Chat translation.
	mux.Handle("/v1/responses", s.clientAuth(s.ResponsesProxy()))
	mux.Handle("/v1/models", s.clientAuth(proxy.ModelsHandlerWithUpstream(
		s.httpClient,
		func() (string, string) {
			base, key, _ := s.cfg.NextUpstream()
			return base, key
		},
	)))

	// ---- Panel API (mounted under /api) ----
	s.mountPanelAPI(mux)

	// ---- Panel SPA (everything else) ----
	if panelMux != nil {
		mux.Handle("/", panelMux)
	} else if panelAssets != nil {
		mux.Handle("/", http.FileServer(panelAssets))
	} else {
		mux.HandleFunc("/", s.panelPlaceholder)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Recover from panics so one bad request can't take the process down.
		// Logging middleware records basic request info for non-/api paths.
		s.withLogging(mux).ServeHTTP(w, r)
	})
}

// panelPlaceholder is shown when no embedded assets are present (dev mode).
func (s *Server) panelPlaceholder(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(devPanelPlaceholder))
}

// Start begins serving. It blocks until Shutdown is called.
func (s *Server) Start(handler http.Handler) error {
	srv := &http.Server{
		Addr:              s.cfg.Snapshot().ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout left at 0 so long streams are not killed.
	}
	// Publish the server before any slow startup step so Shutdown can drain
	// (or refuse to serve) even if a signal arrives during sidecar start.
	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()
	if s.native != nil {
		// Bounded: a sidecar that is slow or broken must not keep the paid
		// routes offline. Free requests get a clear 503 until it is ready.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s.native.Start(ctx)
		cancel()
	}
	// Hold upstream sockets warm so the first turn after an idle gap skips the
	// ~610ms cold-connection handshake.
	s.StartKeepAlive()
	// Bound the request log so SQLite does not grow without limit.
	s.StartMaintenance()
	return srv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() error {
	// Stop the keep-alive pinger first so it cannot open new upstream requests
	// while connections drain. sync.Once keeps a double Shutdown safe.
	s.stopOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
	})
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	var err error
	if srv != nil {
		// A nil context would panic on ctx.Done() as soon as one connection is
		// still open; give the drain a bounded budget instead.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err = srv.Shutdown(ctx)
		cancel()
		if err != nil {
			// Shutdown does not close connections whose handlers outlive the
			// deadline. Free-tier agent turns routinely run longer than 15s,
			// so without Close the sidecar would be stopped while a handler
			// is still writing to it — the 502 burst this ordering exists to
			// prevent.
			_ = srv.Close()
		}
	}
	// Drain in-flight HTTP requests BEFORE stopping the sidecar: a free-tier
	// request still on the wire needs the Bun process to finish. Stopping it
	// first turned every restart into a burst of 502s.
	if s.native != nil {
		s.native.Stop()
	}
	return err
}

// mountPanelAPI wires the panel's REST API onto mux via the api package.
func (s *Server) mountPanelAPI(mux *http.ServeMux) {
	api.SetInvalidateCache(InvalidateKeyCache)
	api.New(s.cfg, s.store).Mount(mux)
}

const devPanelPlaceholder = `<!doctype html><meta charset="utf-8">
<title>opencode-cc</title>
<body style="font-family:system-ui;background:#0b0f17;color:#e5e7eb;padding:2rem">
<h1>opencode-cc proxy is running ✅</h1>
<p>The web panel assets are not embedded in this build (dev mode).</p>
<p>Run <code>cd web &amp;&amp; npm run dev</code> and open the Vite dev server,
or build the panel with <code>npm run build</code> and rebuild the Go binary.</p>
<p>Set <code>ANTHROPIC_BASE_URL=http://` + "`HOST:8787`" + `</code> to start using it.</p>
</body>`

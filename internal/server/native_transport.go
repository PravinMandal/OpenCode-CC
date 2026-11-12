package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	cfgpkg "github.com/PravinMandal/OpenCode-CC/internal/config"

	"github.com/PravinMandal/OpenCode-CC/internal/proxy"
)

// nativeTransport routes every free-tier chat completion through the Bun
// sidecar owned by the native package. It deliberately matches on the model
// suffix instead of a fixed list: Zen adds and retires free models without
// notice, and a hardcoded name is how GLM ended up bypassing the transport and
// returning a bare FreeUsageLimitError.
//
// Paid models and every non-chat route keep using the hardened Go transport.
// nativeEndpoint is the slice of native.Manager the transport needs. Keeping
// it an interface lets tests exercise routing without spawning Bun.
type nativeEndpoint interface {
	Enabled() bool
	Endpoint() (string, string, error)
}

// Free-lane quota cycling. A free-limit 429 is an IP-wide condition, so the
// lane resets the egress through the sidecar and then moves through every key
// in the pool, wrapping back to the first and repeating. These are variables
// so tests can shrink the delays; the attempt ceiling is a safety valve for a
// pathological upstream (a client that disconnects cancels the request
// immediately and ends the loop).
var (
	freeLaneMaxAttempts = 40
	freeLaneCycleDelay  = 5 * time.Second
	freeLaneMaxDelay    = 30 * time.Second
)

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type nativeTransport struct {
	base  http.RoundTripper
	local *http.Transport
	mgr   nativeEndpoint
	pool  *credentialPool
}

func newNativeTransport(base http.RoundTripper, mgr nativeEndpoint, pool *credentialPool) http.RoundTripper {
	if mgr == nil || !mgr.Enabled() {
		return base
	}
	return &nativeTransport{
		base: base,
		mgr:  mgr,
		pool: pool,
		local: &http.Transport{
			// Loopback only: never inherit HTTP(S)_PROXY here.
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 32,
			// The sidecar may wait minutes for upstream headers and can spend
			// ~150s rotating an exhausted egress prefix, so this leg must not
			// impose a shorter deadline than the sidecar itself.
			ResponseHeaderTimeout: 0,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// nativeTransportReady reports whether the Bun sidecar is currently able to
// serve free-tier requests. When it is ready, the sidecar supplies the
// canonical tool set itself, so the Go layer must not inject free-tier marker
// tools: they would be indistinguishable from client declarations and would
// keep shell/read alive for a client that declared no tools.
//
// Readiness means the sidecar answered /health with ok=true, not merely that
// its port is open: on a first install (or after a corrupt capture) the
// sidecar announces its port before an identity exists and answers 503 to
// every completion.
func (s *Server) nativeTransportReady() bool {
	if s == nil || s.native == nil || !s.native.Enabled() {
		return false
	}
	endpoint, token, err := s.native.Endpoint()
	if err != nil {
		return false
	}
	s.nativeHealthMu.Lock()
	at, cached := s.nativeHealthAt, s.nativeHealthOK
	s.nativeHealthMu.Unlock()
	if !at.IsZero() && time.Since(at) < 2*time.Second {
		return cached
	}
	ok := false
	healthURL := strings.TrimSuffix(endpoint, "/v1/chat/completions") + "/health"
	if req, reqErr := http.NewRequest(http.MethodGet, healthURL, nil); reqErr == nil {
		if token != "" {
			req.Header.Set("x-bridge-token", token)
		}
		client := &http.Client{Timeout: 700 * time.Millisecond}
		if resp, doErr := client.Do(req); doErr == nil {
			var body struct {
				OK bool `json:"ok"`
			}
			if resp.StatusCode == http.StatusOK &&
				json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil {
				ok = body.OK
			}
			_ = resp.Body.Close()
		}
	}
	s.nativeHealthMu.Lock()
	s.nativeHealthAt, s.nativeHealthOK = time.Now(), ok
	s.nativeHealthMu.Unlock()
	return ok
}

func (t *nativeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !isZenChatCompletion(r) || r.GetBody == nil {
		return t.base.RoundTrip(r)
	}
	body, err := r.GetBody()
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Model string `json:"model"`
	}
	err = json.NewDecoder(io.LimitReader(body, maxRequestBytes)).Decode(&envelope)
	_ = body.Close()
	if err != nil || !proxy.IsFreeModel(envelope.Model) {
		return t.base.RoundTrip(r)
	}

	endpoint, token, err := t.mgr.Endpoint()
	if err != nil {
		return nativeUnavailable(r, err), nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nativeUnavailable(r, err), nil
	}

	local := r.Clone(r.Context())
	local.URL = u
	local.Host = u.Host
	local.Header = r.Header.Clone()
	local.Header.Set("x-bridge-token", token)
	// The sidecar resolves the live OpenCode credential itself when no
	// credential header is present (standalone use). When this request came
	// through the pool, pass the key the handler selected so key failover
	// applies to the free lane too. The inbound Authorization is stripped so
	// a client key never leaks into the sidecar leg.
	credential := bearerToken(r.Header.Get("Authorization"))
	if t.pool != nil {
		// Scope selection to the Zen base. A pool entry for another upstream
		// must never have its key sent to opencode.ai as a free-tier key.
		credential = t.pool.selectInitial(credential, cfgpkg.DefaultUpstreamBase)
	}
	local.Header.Del("Authorization")
	// x-api-key is the other Zen credential carrier. A client key must not ride
	// into the sidecar leg either.
	local.Header.Del("x-api-key")
	if credential != "" {
		local.Header.Set("x-bridge-credential", credential)
	}

	resp, err := t.local.RoundTrip(local)
	if err != nil {
		return nativeUnavailable(r, err), nil
	}
	// A rejected key is recoverable without changing request shape. A 429 is
	// recoverable by rotation too. The sidecar attempts an egress reset before
	// it surfaces a free-limit 429, so a failed reset means the IP bucket did
	// not move and another key is the next step. Do not suppress the reset on
	// the retry: the quota is IP-wide, and every key must get its own reset
	// attempt once the reset engine's cooldown expires. Rotation therefore
	// cycles key 1..N indefinitely rather than stopping after one pass; the
	// sidecar's reset engine collapses attempts inside its cooldown into one
	// radio cycle. The loop is bounded by the request context and a generous
	// attempt ceiling so a pathological upstream cannot pin the connection
	// forever. Auth failures keep the stricter nextAfter path: a dead key is
	// not revived by a reset.
	startCredential := credential
	cycles := 0
	attempts := 0
	for resp != nil && t.pool != nil && credential != "" {
		quota := false
		rotatable := false
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			prefix, _ := peekReplayBody(resp, credentialErrorPeek)
			rotatable = looksLikeCredentialError(resp.StatusCode, prefix)
			if rotatable {
				t.pool.markDead(credential)
			}
		case http.StatusTooManyRequests:
			// The sidecar sets x-oc-reset when it handled a free-limit 429.
			// "manual" means it is not a quota response at all; rotating keys
			// would only hide the real error. "shape" means a reset completed
			// but a fresh egress still returned the limit: the shape cache is
			// key-scoped and expires, so keep it in the quota cycle for a
			// later reset attempt instead of treating it as terminal.
			switch resp.Header.Get("x-oc-reset") {
			case "failed", "shape", "skipped":
				t.pool.markLimited(credential)
				rotatable, quota = true, true
			}
		}
		if !rotatable {
			break
		}
		var next string
		var ok bool
		if quota {
			if attempts+1 >= freeLaneMaxAttempts {
				break
			}
			next, ok = t.pool.nextCyclic(credential, cfgpkg.DefaultUpstreamBase)
		} else {
			next, ok = t.pool.nextAfter(credential, cfgpkg.DefaultUpstreamBase)
		}
		if !ok {
			break
		}
		drainAndClose(resp.Body)
		if quota {
			attempts++
			if next == startCredential {
				// A full pass over the pool produced no success. Back off so
				// the next pass starts after the reset cooldown and can
				// actually rotate the egress instead of replaying a cached
				// rejection.
				cycles++
				delay := freeLaneCycleDelay * time.Duration(cycles)
				if delay > freeLaneMaxDelay {
					delay = freeLaneMaxDelay
				}
				if !sleepContext(r.Context(), delay) {
					return nil, r.Context().Err()
				}
			}
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
		}
		body, bodyErr := r.GetBody()
		if bodyErr != nil {
			break
		}
		local.Body = body
		local.Header.Set("x-bridge-credential", next)
		// Every attempt (including a rotated key) may reset the egress: the
		// previous attempt's reset either failed or was too recent, and the
		// sidecar's cooldown decides whether a new radio cycle is needed.
		local.Header.Del("x-bridge-no-reset")
		credential = next
		resp, err = t.local.RoundTrip(local)
		if err != nil {
			return nativeUnavailable(r, err), nil
		}
	}
	if resp != nil {
		resp.Request = r
	}
	return resp, nil
}

func isZenChatCompletion(r *http.Request) bool {
	if r.Method != http.MethodPost || r.URL == nil || r.URL.Scheme != "https" || r.URL.Host != "opencode.ai" {
		return false
	}
	switch r.URL.Path {
	case "/inference/v1/chat/completions",
		"/inference/openai/v1/chat/completions",
		"/zen/v1/chat/completions":
		return true
	}
	return false
}

// nativeUnavailable turns a sidecar failure into a response the client can
// understand. Returning an error would surface as a generic 502; a 503 with a
// message names the actual problem.
func nativeUnavailable(r *http.Request, err error) *http.Response {
	msg := fmt.Sprintf("free-tier native transport unavailable: %v", err)
	body, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": msg},
	})
	return &http.Response{
		StatusCode:    http.StatusServiceUnavailable,
		Status:        "503 Service Unavailable",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}, "Retry-After": []string{"10"}},
		Body:          io.NopCloser(strings.NewReader(string(body))),
		ContentLength: int64(len(body)),
		Request:       r,
	}
}

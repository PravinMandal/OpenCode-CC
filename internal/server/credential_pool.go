package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	cfgpkg "github.com/PravinMandal/OpenCode-CC/internal/config"
)

const (
	// credentialDeadTTL is how long an expired/rejected key is skipped before
	// it is probed again. Expiry is usually not self-healing, but a five-minute
	// retry keeps a transient 401 (for example a clock-skewed edge) from
	// permanently removing a good key from the pool.
	credentialDeadTTL = 5 * time.Minute
	// credentialPoolTTL caches the resolved pool so a five-key configuration
	// does not read the OpenCode SQLite database on every request.
	credentialPoolTTL = 30 * time.Second
	// credentialErrorPeek bounds how much of a 401/403 body we inspect before
	// deciding whether the failure is credential-related. Auth errors are tiny.
	credentialErrorPeek = 64 * 1024
)

// credentialPool tracks which resolved upstream keys are currently usable and
// hands out the next key for auth failover. It deliberately reacts only to
// 401/403: a 429 is a quota signal and must never trigger key rotation.
type credentialPool struct {
	cfg *cfgpkg.Config

	mu       sync.Mutex
	dead     map[string]time.Time
	resolved []cfgpkg.ResolvedUpstream
	loadedAt time.Time
	now      func() time.Time
}

func newCredentialPool(cfg *cfgpkg.Config) *credentialPool {
	return &credentialPool{
		cfg:  cfg,
		dead: make(map[string]time.Time),
		now:  time.Now,
	}
}

func credentialID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:12])
}

// snapshot returns the resolved pool, refreshing it at most every
// credentialPoolTTL. force is used after an auth failure, when the caller wants
// to see a credential that was just rotated in or out of the config.
func (p *credentialPool) snapshot(force bool) []cfgpkg.ResolvedUpstream {
	if p == nil || p.cfg == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !force && len(p.resolved) > 0 && p.now().Sub(p.loadedAt) < credentialPoolTTL {
		out := make([]cfgpkg.ResolvedUpstream, len(p.resolved))
		copy(out, p.resolved)
		return out
	}
	out := p.cfg.ResolvedUpstreams()
	p.resolved = out
	p.loadedAt = p.now()
	cp := make([]cfgpkg.ResolvedUpstream, len(out))
	copy(cp, out)
	return cp
}

func (p *credentialPool) isDead(key string) bool {
	if p == nil || key == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.dead[credentialID(key)]
	if !ok {
		return false
	}
	if p.now().After(until) {
		delete(p.dead, credentialID(key))
		return false
	}
	return true
}

func (p *credentialPool) markDead(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	p.dead[credentialID(key)] = p.now().Add(credentialDeadTTL)
	p.mu.Unlock()
}

func (p *credentialPool) markAlive(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	delete(p.dead, credentialID(key))
	p.mu.Unlock()
}

// selectInitial keeps the handler's chosen key when it is healthy, otherwise
// returns the first healthy replacement. base is the upstream base URL the
// request will use; an empty base means "any key" (the free-tier sidecar
// always talks to Zen regardless of the loopback URL it is called on).
func (p *credentialPool) selectInitial(current, base string) string {
	if p == nil || current == "" || !p.isDead(current) {
		return current
	}
	if next, ok := p.nextAfter(current, base); ok {
		return next
	}
	// Every candidate is marked dead. Prefer the caller's original key over
	// inventing one; the caller can still surface the upstream error.
	return current
}

// nextAfter returns the next healthy key in configuration order, skipping
// current and every key already known bad. It returns false when no other key
// is available.
func (p *credentialPool) nextAfter(current, base string) (string, bool) {
	pool := p.snapshot(true)
	start := -1
	for i, u := range pool {
		if u.APIKey == current {
			start = i
			break
		}
	}
	if start < 0 {
		start = -1
	}
	for offset := 1; offset <= len(pool); offset++ {
		u := pool[(start+offset+len(pool))%len(pool)]
		if u.APIKey == current || !matchesBase(u.BaseURL, base) || p.isDead(u.APIKey) {
			continue
		}
		return u.APIKey, true
	}
	return "", false
}

// matchesBase reports whether an upstream base is the destination of this
// request. An empty base matches everything.
func matchesBase(upstreamBase, requestBase string) bool {
	if requestBase == "" {
		return true
	}
	upstreamBase = strings.TrimRight(upstreamBase, "/")
	requestBase = strings.TrimRight(requestBase, "/")
	return requestBase == upstreamBase || strings.HasPrefix(requestBase, upstreamBase+"/")
}

func bearerToken(header string) string {
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// looksLikeCredentialError distinguishes an expired/invalid key from other
// 401/403 responses (for example a workspace permission denial, which every key
// of the same account would hit). Failover is only useful for the former.
func looksLikeCredentialError(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	text := strings.ToLower(string(body))
	for _, marker := range []string{"api key", "api_key", "apikey", "credential", "expired", "invalid key", "unauthorized"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// replayBody returns a response body that can be read again after a bounded
// peek. 401/403 bodies are small; anything larger is truncated, which is
// acceptable because the caller is about to return an auth error.
type replayBody struct {
	*bytes.Reader
}

func (replayBody) Close() error { return nil }

func makeReplayBody(b []byte) io.ReadCloser {
	return replayBody{bytes.NewReader(b)}
}

func cloneWithCredential(r *http.Request, key string) (*http.Request, error) {
	clone := r.Clone(r.Context())
	if r.GetBody != nil {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		clone.Body = body
	}
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+key)
	return clone, nil
}

func requestBase(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return strings.TrimRight(r.URL.Scheme+"://"+r.URL.Host+r.URL.Path, "/")
}

// credentialFailoverTransport retries a paid-model request with another key
// only when the upstream rejects the credential (401/403). It never reacts to
// 429, so it cannot be used to spread quota across accounts.
type credentialFailoverTransport struct {
	base http.RoundTripper
	pool *credentialPool
}

func newCredentialFailoverTransport(base http.RoundTripper, pool *credentialPool) http.RoundTripper {
	if base == nil || pool == nil {
		return base
	}
	return &credentialFailoverTransport{base: base, pool: pool}
}

func (t *credentialFailoverTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	original := bearerToken(r.Header.Get("Authorization"))
	if original == "" || r.Method != http.MethodPost || r.GetBody == nil {
		return t.base.RoundTrip(r)
	}
	base := requestBase(r)
	current := t.pool.selectInitial(original, base)
	if current != original {
		var err error
		r, err = cloneWithCredential(r, current)
		if err != nil {
			return nil, err
		}
	}
	for attempts := 0; attempts < 8; attempts++ {
		resp, err := t.base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			if resp.StatusCode < http.StatusBadRequest {
				t.pool.markAlive(current)
			}
			return resp, nil
		}
		prefix, _ := io.ReadAll(io.LimitReader(resp.Body, credentialErrorPeek))
		_ = resp.Body.Close()
		resp.Body = makeReplayBody(prefix)
		resp.ContentLength = int64(len(prefix))
		if !looksLikeCredentialError(resp.StatusCode, prefix) {
			return resp, nil
		}
		t.pool.markDead(current)
		next, ok := t.pool.nextAfter(current, base)
		if !ok {
			return resp, nil
		}
		r, err = cloneWithCredential(r, next)
		if err != nil {
			return resp, nil
		}
		current = next
	}
	return t.base.RoundTrip(r)
}

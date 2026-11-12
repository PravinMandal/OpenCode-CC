package server

import (
	"bytes"
	"context"
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
	// credentialLimitedTTL is how long a key that answered 429 is skipped
	// before the pool prefers another key. Quota windows refill, and a 429 is
	// explicitly not proof that a key is invalid, so the skip is short. Its
	// only job is to stop a request from hammering a key that just ran out.
	credentialLimitedTTL = 60 * time.Second
	// credentialPoolTTL caches the resolved pool so a five-key configuration
	// does not read the OpenCode SQLite database on every request.
	credentialPoolTTL = 30 * time.Second
	// credentialErrorPeek bounds how much of a 401/403/429 body we inspect
	// before deciding whether failover is useful. Anything past this cap is
	// streamed through untouched instead of being replayed truncated.
	credentialErrorPeek = 64 * 1024
)

// credentialPool tracks which resolved upstream keys are currently usable and
// hands out the next key for auth or quota failover.
//
// A 401/403 that really is about the credential marks a key dead for a while.
// A 429 marks it limited for a much shorter time: the next key is tried, but
// the limited key is not discarded, because its quota window replenishes.
type credentialPool struct {
	cfg *cfgpkg.Config

	mu       sync.Mutex
	dead     map[string]time.Time
	limited  map[string]time.Time
	resolved []cfgpkg.ResolvedUpstream
	loadedAt time.Time
	now      func() time.Time
}

func newCredentialPool(cfg *cfgpkg.Config) *credentialPool {
	return &credentialPool{
		cfg:     cfg,
		dead:    make(map[string]time.Time),
		limited: make(map[string]time.Time),
		now:     time.Now,
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
	id := credentialID(key)
	until, ok := p.dead[id]
	if !ok {
		return false
	}
	if p.now().After(until) {
		delete(p.dead, id)
		return false
	}
	return true
}

func (p *credentialPool) isLimited(key string) bool {
	if p == nil || key == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	id := credentialID(key)
	until, ok := p.limited[id]
	if !ok {
		return false
	}
	if p.now().After(until) {
		delete(p.limited, id)
		return false
	}
	return true
}

func (p *credentialPool) markDead(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	delete(p.limited, credentialID(key))
	p.dead[credentialID(key)] = p.now().Add(credentialDeadTTL)
	p.mu.Unlock()
}

// markLimited records a 429 for a key. It never marks the key dead: quota is a
// transient condition and the same credential must be usable again once the
// window refills or another upstream rejects its replacement.
func (p *credentialPool) markLimited(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	p.limited[credentialID(key)] = p.now().Add(credentialLimitedTTL)
	p.mu.Unlock()
}

func (p *credentialPool) markAlive(key string) {
	if p == nil || key == "" {
		return
	}
	p.mu.Lock()
	id := credentialID(key)
	delete(p.dead, id)
	delete(p.limited, id)
	p.mu.Unlock()
}

// isUnavailable reports whether failover should look past this key right now.
func (p *credentialPool) isUnavailable(key string) bool {
	return p.isDead(key) || p.isLimited(key)
}

// selectInitial keeps the handler's chosen key when it is healthy, otherwise
// returns the first healthy replacement. base is the upstream base URL the
// request will use; an empty base means "any key".
func (p *credentialPool) selectInitial(current, base string) string {
	if p == nil || current == "" || !p.isUnavailable(current) {
		return current
	}
	if next, ok := p.nextAfter(current, base); ok {
		return next
	}
	// Every candidate is dead or limited. Prefer the caller's original key
	// over inventing one; the caller can still surface the upstream error.
	return current
}

// nextAfter returns the next usable key in configuration order, skipping
// current and every key already known bad or currently limited. It returns
// false when no other key is available, which is what bounds a request's
// failover loop: at most one attempt per configured key.
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
		if u.APIKey == current || !matchesBase(u.BaseURL, base) || p.isUnavailable(u.APIKey) {
			continue
		}
		return u.APIKey, true
	}
	return "", false
}

// nextCyclic returns the next key for the free lane's quota cycle. Unlike
// nextAfter it does not skip limited keys: a free-limit 429 is an IP-wide
// condition, and the reset the next attempt performs can clear it before the
// key is retried. Dead keys are still skipped, and reaching the current key
// again returns it so a single-key deployment keeps retrying instead of
// stopping after one pass. The caller bounds the cycle by request context.
func (p *credentialPool) nextCyclic(current, base string) (string, bool) {
	pool := p.snapshot(true)
	start := -1
	for i, u := range pool {
		if u.APIKey == current {
			start = i
			break
		}
	}
	for offset := 1; offset <= len(pool); offset++ {
		u := pool[(start+offset+len(pool))%len(pool)]
		if !matchesBase(u.BaseURL, base) || p.isDead(u.APIKey) {
			continue
		}
		return u.APIKey, true
	}
	return "", false
}

// count returns the number of configured keys for a base. The free lane uses
// it to recognise a full pass over the pool, including keys that are currently
// marked dead or limited.
func (p *credentialPool) count(base string) int {
	if p == nil {
		return 0
	}
	pool := p.snapshot(false)
	n := 0
	for _, u := range pool {
		if matchesBase(u.BaseURL, base) {
			n++
		}
	}
	return n
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
// 401/403 responses. Quota, billing, rate-limit and permission denials are
// never credential failures: every key of the same account would hit the same
// wall, so rotating only burns the remaining keys and hides the real error.
func looksLikeCredentialError(status int, body []byte) bool {
	text := strings.ToLower(string(body))
	for _, marker := range []string{
		"insufficient_quota", "insufficient quota", "quota",
		"billing", "payment", "rate limit", "rate_limit", "usage limit",
		"too many requests", "out of credits", "credit balance",
	} {
		if strings.Contains(text, marker) {
			return false
		}
	}
	if status == http.StatusUnauthorized {
		return true
	}
	// A workspace denial is key-specific when the pool holds keys from more
	// than one account (a general free-tier deployment does). Trying the next
	// key is correct; it must not be treated as an account-wide quota wall.
	for _, marker := range []string{
		"api key", "api_key", "apikey", "credential", "expired", "invalid key",
		"unauthorized", "workspace access denied", "wrong workspace", "access denied",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// replayBody returns a response body that can be read again after a bounded
// peek. The bytes are small auth/quota bodies that were fully buffered.
type replayBody struct {
	*bytes.Reader
}

func (replayBody) Close() error { return nil }

func makeReplayBody(b []byte) io.ReadCloser {
	return replayBody{bytes.NewReader(b)}
}

// peekReplayBody buffers up to limit bytes of resp.Body for classification and
// leaves resp.Body readable from the beginning. When the body is larger than
// limit, the unread remainder is streamed from the original body instead of
// being dropped, so a large error page is never truncated on the way out.
func peekReplayBody(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, nil
	}
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return prefix, err
	}
	rest := resp.Body
	resp.Body = makePeekReplayBody(prefix, rest)
	if int64(len(prefix)) >= limit {
		// The body may have more; do not let a stale length cut it short.
		resp.ContentLength = -1
	} else {
		resp.ContentLength = int64(len(prefix))
	}
	return prefix, nil
}

type restoredBody struct {
	io.Reader
	rest io.ReadCloser
}

func (b *restoredBody) Close() error {
	if b.rest != nil {
		return b.rest.Close()
	}
	return nil
}

func makePeekReplayBody(prefix []byte, rest io.ReadCloser) io.ReadCloser {
	return &restoredBody{Reader: io.MultiReader(bytes.NewReader(prefix), rest), rest: rest}
}

// drainAndClose discards a bounded prefix so the underlying connection can be
// reused, then closes the body.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, credentialErrorPeek))
	_ = body.Close()
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
	// The Zen handlers set both Authorization and x-api-key. Rotating only one
	// of them leaves the expired key in the other, and the upstream may prefer
	// x-api-key, so keep the pair in sync.
	if clone.Header.Get("x-api-key") != "" {
		clone.Header.Set("x-api-key", key)
	}
	return clone, nil
}

func requestBase(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return strings.TrimRight(r.URL.Scheme+"://"+r.URL.Host+r.URL.Path, "/")
}

// credentialFailoverTransport retries a request with another key when the
// upstream rejects the credential (401/403) or reports a quota limit (429).
//
// A 429 is tried last, after any IP reset the free-tier sidecar performed. The
// retry is bounded by the number of configured keys: nextAfter returns false
// once every other key has been tried, so the real upstream error is always
// surfaced and no request can loop forever.
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
	// Keep-alive probes must not mutate pool state: a probe is not user
	// traffic, and a single probe 401 would otherwise remove a good key.
	if isCredentialProbe(r) {
		return t.base.RoundTrip(r)
	}
	original := bearerToken(r.Header.Get("Authorization"))
	if original == "" || (r.Body != nil && r.GetBody == nil) {
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
	for {
		resp, err := t.base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			t.pool.markLimited(current)
			if next, ok := t.pool.nextAfter(current, base); ok {
				drainAndClose(resp.Body)
				r, err = cloneWithCredential(r, next)
				if err != nil {
					return resp, nil
				}
				current = next
				continue
			}
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			prefix, _ := peekReplayBody(resp, credentialErrorPeek)
			if !looksLikeCredentialError(resp.StatusCode, prefix) {
				return resp, nil
			}
			t.pool.markDead(current)
			next, ok := t.pool.nextAfter(current, base)
			if !ok {
				return resp, nil
			}
			drainAndClose(resp.Body)
			r, err = cloneWithCredential(r, next)
			if err != nil {
				return resp, nil
			}
			current = next
			continue
		default:
			if resp.StatusCode < http.StatusBadRequest {
				t.pool.markAlive(current)
			}
			return resp, nil
		}
	}
}

// credentialProbeKey marks warm-up/probe requests. They use the normal
// transport so they warm the same connection pool, but they are exempt from
// credential failover bookkeeping.
type credentialProbeKey struct{}

func markCredentialProbe(r *http.Request) *http.Request {
	if r == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), credentialProbeKey{}, true))
}

func isCredentialProbe(r *http.Request) bool {
	if r == nil {
		return false
	}
	v, _ := r.Context().Value(credentialProbeKey{}).(bool)
	return v
}

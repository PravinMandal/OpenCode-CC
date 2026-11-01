package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Kiowx/opencode-cc/internal/proxy"
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

type nativeTransport struct {
	base  http.RoundTripper
	local *http.Transport
	mgr   nativeEndpoint
}

func newNativeTransport(base http.RoundTripper, mgr nativeEndpoint) http.RoundTripper {
	if mgr == nil || !mgr.Enabled() {
		return base
	}
	return &nativeTransport{
		base: base,
		mgr:  mgr,
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
	// The sidecar resolves the live OpenCode credential itself; the inbound
	// key is for opencode-cc, not Zen, and must not leak into this leg.
	local.Header.Del("Authorization")

	resp, err := t.local.RoundTrip(local)
	if err != nil {
		return nativeUnavailable(r, err), nil
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

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

var errNoUpstream = errors.New("no upstream configured")

// AnthropicModelInfo describes one entry in the model list.
type AnthropicModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	Object      string `json:"object"`
	CreatedAt   int64  `json:"created_at,omitempty"`
	Created     int64  `json:"created,omitempty"`
	OwnedBy     string `json:"owned_by,omitempty"`
}

// AnthropicModelList is the body of GET /v1/models.
type AnthropicModelList struct {
	Data    []AnthropicModelInfo `json:"data"`
	Object  string               `json:"object"`
	FirstID string               `json:"first_id,omitempty"`
	LastID  string               `json:"last_id,omitempty"`
	HasMore bool                 `json:"has_more"`
}

// fetchZenModels queries the upstream <base>/v1/models and returns the ids. It
// is used by the /v1/models handler so Claude Code sees the real Zen model ids.
//
// ctx MUST carry a timeout (the caller sets it): a stalled upstream fetch must
// fail fast instead of parking the handler goroutine forever. In Sep 2026 a
// flow-control-stalled HTTP/2 upstream connection wedged every API route this
// way — see the transport notes in server.New.
func fetchZenModels(ctx context.Context, client *http.Client, base, apiKey string) ([]AnthropicModelInfo, error) {
	url := base + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, nil // fall back to empty list
	}

	var raw struct {
		Data []struct {
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]AnthropicModelInfo, 0, len(raw.Data))
	for _, m := range raw.Data {
		out = append(out, AnthropicModelInfo{
			ID:        m.ID,
			Type:      "model",
			Object:    "model",
			CreatedAt: m.Created,
			Created:   m.Created,
			OwnedBy:   "opencode-zen",
		})
	}
	return out, nil
}

// ModelsHandler returns an http.HandlerFunc that dynamically lists the upstream
// (Zen) models. It caches the result for a minute so we don't hit Zen on every
// call. If the upstream is unreachable it returns a minimal fallback list.
func ModelsHandler(client *http.Client, upstreamBase, apiKey func() string) http.HandlerFunc {
	return ModelsHandlerWithUpstream(client, func() (string, string) {
		return upstreamBase(), apiKey()
	})
}

// ModelsHandlerWithUpstream is like ModelsHandler but obtains the upstream base
// URL and API key as one pair. Use this when the caller selects from a rotating
// upstream pool, so the base URL and key cannot drift apart between callbacks.
//
// The cache is single-flight: Claude Code fans out many subagents, and without
// it a cold or failing upstream turns every concurrent /v1/models call into its
// own fetch. A failed fetch also backs off for modelsErrorBackoff so an
// upstream outage cannot pin the process in a request storm. Stale-but-known
// models are always preferred over an error or a made-up list.
func ModelsHandlerWithUpstream(client *http.Client, upstream func() (base, apiKey string)) http.HandlerFunc {
	const modelsErrorBackoff = 20 * time.Second

	var (
		mu       sync.Mutex
		cache    []AnthropicModelInfo
		cachedAt time.Time
		failedAt time.Time
		inflight chan struct{}
	)
	return func(w http.ResponseWriter, r *http.Request) {
		write := func(list []AnthropicModelInfo) {
			if len(list) == 0 {
				list = fallbackModels()
			}
			writeJSON(w, http.StatusOK, AnthropicModelList{Data: list, Object: "list", HasMore: false})
		}
		snapshot := func() []AnthropicModelInfo {
			out := make([]AnthropicModelInfo, len(cache))
			copy(out, cache)
			return out
		}

		mu.Lock()
		fresh := len(cache) > 0 && time.Since(cachedAt) < time.Minute
		backoff := !failedAt.IsZero() && time.Since(failedAt) < modelsErrorBackoff
		if fresh || backoff {
			list := snapshot()
			mu.Unlock()
			write(list)
			return
		}
		if inflight != nil {
			wait := inflight
			mu.Unlock()
			select {
			case <-wait:
			case <-r.Context().Done():
			}
			mu.Lock()
			list := snapshot()
			mu.Unlock()
			write(list)
			return
		}
		inflight = make(chan struct{})
		done := inflight
		mu.Unlock()

		// Use a background timeout rather than r.Context(): one canceled
		// caller must not abort the fetch every other caller is waiting on.
		fetchCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		base, key := upstream()
		var (
			models []AnthropicModelInfo
			err    error
		)
		if base != "" && client != nil {
			models, err = fetchZenModels(fetchCtx, client, base, key)
		} else {
			err = errNoUpstream
		}
		cancel()

		mu.Lock()
		if err == nil && len(models) > 0 {
			cache = models
			cachedAt = time.Now()
			failedAt = time.Time{}
		} else {
			failedAt = time.Now()
		}
		list := snapshot()
		inflight = nil
		close(done)
		mu.Unlock()
		write(list)
	}
}

// fallbackModels is only used when no successful catalog fetch has ever
// happened. It mirrors the current public Zen free set so a cold start with a
// briefly unavailable catalog still gives clients real, usable ids.
func fallbackModels() []AnthropicModelInfo {
	ids := []string{
		"deepseek-v4.1-flash-free",
		"glm-5.3-flash-free",
		"mimo-v2.6-flash-free",
		"nemotron-3.5-lightning-free",
		"nemotron-3.5-super-vl-free",
		"beam-free",
		"flint-40b-free",
		"big-pickle",
		"minimax-m3.1-flash-free",
	}
	out := make([]AnthropicModelInfo, 0, len(ids))
	for _, id := range ids {
		out = append(out, AnthropicModelInfo{
			ID: id, Type: "model", Object: "model", OwnedBy: "opencode-zen",
		})
	}
	return out
}

// writeJSON is shared here because the server package also needs it; to avoid
// an import cycle we keep a local copy in helpers.go for the proxy package and
// the server package has its own.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

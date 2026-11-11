package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PravinMandal/OpenCode-CC/internal/config"
	"github.com/PravinMandal/OpenCode-CC/internal/store"
)

// newEmptyRescueTestServer exposes /v1/chat/completions with the empty
// completion guard enabled or disabled.
func newEmptyRescueTestServer(t *testing.T, upstream http.Handler, enabled bool) *httptest.Server {
	t.Helper()
	zen := httptest.NewServer(upstream)
	t.Cleanup(zen.Close)

	cfg := config.Default()
	cfg.UpstreamBase = zen.URL
	cfg.ZenAPIKey = "zen-test-key"
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: "glm-5.3-flash-free"}}
	cfg.BridgeToollessResample = enabled

	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(cfg, st)
	httpSrv := httptest.NewServer(srv.Handler(nil, nil))
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

func writeEmptyChatStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"id":"empty","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`+"\n\n")
	fmt.Fprint(w, `data: {"id":"empty","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	fmt.Fprint(w, `data: {"id":"empty","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":26,"total_tokens":37}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeContentChatStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"id\":\"ok\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", text)
	fmt.Fprint(w, `data: {"id":"ok","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	fmt.Fprint(w, `data: {"id":"ok","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func chatRescueBody() string {
	return `{"model":"client-model","stream":true,"messages":[{"role":"user","content":"do the next step"}]}`
}

// An empty 200 stream must never reach the client when one retry produces
// visible text: T3's DSH adapter rejects an empty completed response.
func TestOpenAIEmptyChatCompletionRescuedIdenticalRetry(t *testing.T) {
	hits := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			writeEmptyChatStream(w)
			return
		}
		writeContentChatStream(w, "rescued after empty sample")
	})
	httpSrv := newEmptyRescueTestServer(t, upstream, true)

	resp, err := http.Post(httpSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatRescueBody()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if hits != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits)
	}
	if !strings.Contains(body, "rescued after empty sample") {
		t.Errorf("rescued text missing:\n%s", body)
	}
	if strings.Count(body, "[DONE]") != 1 {
		t.Errorf("want exactly one terminal frame:\n%s", body)
	}
	if strings.Contains(body, `"completion_tokens":26`) {
		t.Errorf("empty sample usage leaked into the answer:\n%s", body)
	}
}

// When the identical retry is also empty, the second retry must carry a user
// continuation nudge and replace the empty samples with its content.
func TestOpenAIEmptyChatCompletionNudgesAfterIdenticalRetry(t *testing.T) {
	var hits int
	var sawNudge bool
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			writeEmptyChatStream(w)
			return
		}
		// The third request is the nudged retry: the original body must still
		// be intact and one user continuation turn appended.
		var payload struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if len(payload.Messages) > 0 {
			last := payload.Messages[len(payload.Messages)-1]
			sawNudge = last.Role == "user" && strings.Contains(string(last.Content), "Continue")
		}
		writeContentChatStream(w, "content after nudge")
	})
	httpSrv := newEmptyRescueTestServer(t, upstream, true)

	resp, err := http.Post(httpSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatRescueBody()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if hits != 3 {
		t.Fatalf("upstream hits = %d, want 3 (empty, empty, nudge)", hits)
	}
	if !sawNudge {
		t.Error("third request did not end with a user continuation nudge")
	}
	if !strings.Contains(body, "content after nudge") {
		t.Errorf("nudged content missing:\n%s", body)
	}
	if strings.Count(body, "[DONE]") != 1 {
		t.Errorf("want exactly one terminal frame:\n%s", body)
	}
}

// Disabled guard preserves the old behavior: the empty response is relayed
// unchanged and no extra upstream request is made.
func TestOpenAIEmptyChatCompletionRescueDisabled(t *testing.T) {
	hits := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeEmptyChatStream(w)
	})
	httpSrv := newEmptyRescueTestServer(t, upstream, false)

	resp, err := http.Post(httpSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatRescueBody()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	if resp.StatusCode != http.StatusOK || hits != 1 {
		t.Fatalf("status=%d hits=%d body=%s", resp.StatusCode, hits, body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "[DONE]") {
		t.Errorf("empty response was not relayed intact:\n%s", body)
	}
}

// A rescued stream whose first frame is a role-only delta must keep that frame
// before the text. The hold buffer used to be dropped whenever any later frame
// had already reached the candidate buffer.
func TestOpenAIEmptyChatCompletionRescueKeepsHeldPrefix(t *testing.T) {
	hits := 0
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			writeEmptyChatStream(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"ok","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"ok","choices":[{"index":0,"delta":{"content":"held prefix kept"},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"ok","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	httpSrv := newEmptyRescueTestServer(t, upstream, true)

	resp, err := http.Post(httpSrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatRescueBody()))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	if !strings.Contains(body, "held prefix kept") {
		t.Fatalf("rescued text missing:\n%s", body)
	}
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("held role frame was dropped:\n%s", body)
	}
	if strings.Index(body, `"role":"assistant"`) > strings.Index(body, "held prefix kept") {
		t.Fatalf("role frame arrived after the text:\n%s", body)
	}
	if strings.Count(body, "[DONE]") != 1 {
		t.Errorf("want exactly one terminal frame:\n%s", body)
	}
}

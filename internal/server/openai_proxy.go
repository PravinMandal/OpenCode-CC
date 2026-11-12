package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/PravinMandal/OpenCode-CC/internal/proxy"
)

// OpenAIProxy handles POST /v1/chat/completions. Unlike Proxy, this endpoint
// accepts and returns OpenAI wire format, so OpenAI-compatible SDKs can point
// their base URL at opencode-cc directly.
func (s *Server) OpenAIProxy() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method must be POST")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				"could not read request body: "+err.Error())
			return
		}

		clientStream := openAIStreamRequested(body)
		upBody, incomingModel, targetModel, upstreamStream, err := s.prepareOpenAIRequest(body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		// Responses-native models are served ONLY on upstream /v1/responses;
		// the chat path would relay an opaque upstream 500, so fail fast.
		if proxy.IsResponsesNativeModel(targetModel) {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				targetModel+" is Responses-API-only; use POST /v1/responses")
			return
		}

		cfg := s.cfg.Snapshot()
		cacheKey := promptCacheKeyFromOpenAIBody(upBody)
		upstream, zenKey, ok := s.cfg.NextUpstreamForKey(cacheKey)
		if !ok {
			const msg = "no upstream API key configured. Set one in the web panel (Settings → upstreams)."
			writeOpenAIError(w, http.StatusUnauthorized, "authentication_error", msg)
			s.logFailed(r.Context(), r, incomingModel, targetModel, clientStream,
				http.StatusUnauthorized, "no upstream api key", body, time.Since(start))
			return
		}

		upURL := strings.TrimRight(upstream, "/") + "/v1/chat/completions"
		newUpReq := func(body []byte) (*http.Request, error) {
			req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+zenKey)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", ocUA())
			setZenSessionHeaders(req, r.Header, cacheKey)
			if upstreamStream {
				req.Header.Set("Accept", "text/event-stream")
			} else {
				req.Header.Set("Accept", "application/json")
			}
			return req, nil
		}
		upReq, err := newUpReq(upBody)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "api_error",
				"could not build upstream request: "+err.Error())
			return
		}

		httpClient := s.upstreamClient(upstreamStream, cfg.RequestTimeoutSeconds)
		// The empty-completion guard re-posts the same body when the free tier
		// answers 200 with no content and no tool call (reasoning-only turns).
		var resample func([]byte) (*http.Response, error)
		if cfg.BridgeToollessResample {
			resample = func(body []byte) (*http.Response, error) {
				// Rescue bodies are raw client requests (the original or a
				// nudged variant). Apply the same preparation as the first hop:
				// history restore, model resolution, prompt cache and the free
				// lane shape. Posting the raw body re-sent the unresolved alias
				// and skipped PrepareFreeChatBody, so the rescue failed exactly
				// on the free tier it exists for.
				prepared, _, _, _, err := s.prepareOpenAIRequest(body)
				if err != nil {
					return nil, err
				}
				req, err := newUpReq(prepared)
				if err != nil {
					return nil, err
				}
				return doUpstreamWithRetry(httpClient, req, prepared)
			}
		}
		// Retry a stalled/5xx upstream once before failing. Zen free-tier
		// intermittently stalls awaiting response headers (>30s) and 503s
		// under concurrency; without this the client sees a hard 502
		// (reported Oct 2026). Safe here because nothing has been written
		// downstream yet and the POST has no side effects.
		resp, err := doUpstreamWithRetry(httpClient, upReq, upBody)
		if err != nil {
			status := upstreamFailureStatus(w, err)
			writeOpenAIError(w, status, "api_error", "upstream request failed: "+err.Error())
			s.logFailed(r.Context(), r, incomingModel, targetModel, clientStream,
				status, err.Error(), body, time.Since(start))
			return
		}

		contentType := strings.ToLower(resp.Header.Get("Content-Type"))
		if upstreamStream && resp.StatusCode < http.StatusBadRequest &&
			(contentType == "" || strings.Contains(contentType, "text/event-stream")) {
			if clientStream {
				s.relayOpenAIStream(w, resp, r, incomingModel, targetModel, body, start, resample)
			} else {
				s.relayOpenAIAggregated(w, resp, r, incomingModel, targetModel, body, start)
			}
			return
		}
		s.relayOpenAIJSON(w, resp, r, incomingModel, targetModel, clientStream, body, start)
	}
}

// prepareOpenAIRequest validates the JSON object and rewrites only its model
// field, preserving extensions used by different OpenAI-compatible clients.
func (s *Server) prepareOpenAIRequest(body []byte) ([]byte, string, string, bool, error) {
	// Replayed history carries the client-facing tool names and argument keys.
	// Restore the canonical calls the model actually made before forwarding.
	body = proxy.RestoreChatHistory(body)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", "", false, fmt.Errorf("request body is not valid OpenAI JSON: %w", err)
	}
	if payload == nil {
		return nil, "", "", false, fmt.Errorf("request body must be a JSON object")
	}

	var incomingModel string
	if raw := payload["model"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &incomingModel); err != nil {
			return nil, "", "", false, fmt.Errorf("model must be a string")
		}
	}
	var stream bool
	if raw := payload["stream"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &stream); err != nil {
			return nil, "", "", false, fmt.Errorf("stream must be a boolean")
		}
	}
	if stream {
		// Zen supports OpenAI's usage trailer. Request it when the client did
		// not specify stream_options so API-key quotas and dashboard stats stay
		// accurate without overriding an explicit client preference.
		if raw, ok := payload["stream_options"]; !ok || string(raw) == "null" {
			payload["stream_options"] = json.RawMessage(`{"include_usage":true}`)
		}
	}

	targetModel := s.cfg.ResolveModel(incomingModel)
	payload["model"], _ = json.Marshal(targetModel)
	proxy.ApplyRawOpenAIPromptCache(payload, promptCacheOptionsFromConfig(s.cfg.Snapshot()))
	upBody, err := json.Marshal(payload)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("could not encode upstream request: %w", err)
	}
	if proxy.IsFreeModel(targetModel) {
		// When the Bun sidecar is ready it supplies the canonical tool set and
		// forces streaming itself. Injecting the free-tier marker tools here
		// would make the sidecar believe the client declared shell/read, so a
		// zero-tool client would keep tools it cannot run. The markers are only
		// needed on the direct (no-sidecar) fallback path.
		if !s.nativeTransportReady() {
			upBody, err = proxy.PrepareFreeChatBody(upBody)
			if err != nil {
				return nil, "", "", false, fmt.Errorf("could not prepare Zen free-tier request: %w", err)
			}
		}
		// Zen's free lane only accepts the official streaming/tool shape. The
		// handler still uses the original body to decide how to answer a
		// non-streaming client; this returned flag describes the upstream hop.
		stream = true
	}
	return upBody, incomingModel, targetModel, stream, nil
}

func openAIStreamRequested(body []byte) bool {
	var payload struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Stream
}

func filterUndeclaredOpenAITools(out *proxy.OpenAIResponse, reqBody []byte) {
	if out == nil {
		return
	}
	declared := declaredOpenAIToolNames(reqBody)
	schemas := declaredOpenAIToolSchemas(reqBody)
	for ci := range out.Choices {
		msg := out.Choices[ci].Message
		if msg == nil || len(msg.ToolCalls) == 0 {
			continue
		}
		kept := msg.ToolCalls[:0]
		for _, call := range msg.ToolCalls {
			resolved, translated, ok := proxy.ResolveDeclaredCallWithSchemas(
				call.Function.Name, call.Function.Arguments, declared, schemas)
			if !ok {
				continue
			}
			if resolved != call.Function.Name || translated != call.Function.Arguments {
				proxy.RememberTranslatedCall(call.ID, call.Function.Name, call.Function.Arguments, resolved, translated)
				call.Function.Name = resolved
				call.Function.Arguments = translated
			}
			kept = append(kept, call)
		}
		msg.ToolCalls = kept
		// A choice that lost every call must not advertise tool_calls as the
		// finish reason: OpenAI SDKs treat that as "there are calls to run"
		// and wait forever for tool results that will never arrive.
		if len(kept) == 0 && out.Choices[ci].FinishReason != nil &&
			*out.Choices[ci].FinishReason == "tool_calls" {
			stop := "stop"
			out.Choices[ci].FinishReason = &stop
		}
	}
}

// relayOpenAIAggregated adapts a forced upstream SSE response to the
// non-streaming wire format requested by ordinary OpenAI SDKs. It is used
// only for models whose free-tier gate requires stream:true upstream.
func (s *Server) relayOpenAIAggregated(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	reqBody []byte,
	start time.Time,
) {
	defer resp.Body.Close()
	reader := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error", "could not decompress upstream stream: "+err.Error())
			s.logFailed(r.Context(), r, incomingModel, targetModel, false, http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
			return
		}
		defer gz.Close()
		reader = gz
	}
	agg, err := proxy.AggregateOpenAIStream(io.LimitReader(reader, maxResponseBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "api_error", err.Error())
		s.logFailed(r.Context(), r, incomingModel, targetModel, false, http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	filterUndeclaredOpenAITools(agg, reqBody)
	writeJSON(w, http.StatusOK, agg)
	stopReason := ""
	if len(agg.Choices) > 0 && agg.Choices[0].FinishReason != nil {
		stopReason = *agg.Choices[0].FinishReason
	}
	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, false, http.StatusOK,
		agg.Usage.PromptTokens, agg.Usage.CompletionTokens, agg.Usage.CachedPromptTokens(), 0, stopReason,
		string(reqBody), mustJSON(agg), time.Since(start))
}

func promptCacheKeyFromOpenAIBody(body []byte) string {
	var payload struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.PromptCacheKey)
}

func (s *Server) relayOpenAIJSON(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	stream bool,
	reqBody []byte,
	start time.Time,
) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "api_error", "could not read upstream body: "+err.Error())
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	if len(raw) > maxResponseBytes {
		const msg = "upstream response exceeded the maximum allowed size"
		writeOpenAIError(w, http.StatusBadGateway, "api_error", msg)
		s.logFailed(r.Context(), r, incomingModel, targetModel, stream,
			http.StatusBadGateway, msg, reqBody, time.Since(start))
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		raw = filterUndeclaredOpenAIToolsRaw(raw, reqBody)
	}

	copyOpenAIHeaders(w.Header(), resp.Header, false)
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	// Stamp the cooldown before WriteHeader: headers set afterwards are
	// ignored. copyOpenAIHeaders already relayed an upstream Retry-After.
	if status == http.StatusTooManyRequests {
		s.stampCooldown(w, resp.Header, targetModel)
	}
	w.WriteHeader(status)
	_, _ = w.Write(raw)

	if status >= http.StatusBadRequest {
		msg := strings.TrimSpace(string(raw))
		if extracted := extractOpenAIError(raw); extracted != "" {
			msg = extracted
		}
		s.logFailedResponse(r.Context(), r, incomingModel, targetModel, stream,
			status, msg, reqBody, time.Since(start),
			&upstreamFailure{Status: status, Header: resp.Header, Body: raw})
		return
	}

	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage proxy.OpenAIUsage `json:"usage"`
	}
	_ = json.Unmarshal(raw, &out)
	stopReason := ""
	if len(out.Choices) > 0 {
		stopReason = out.Choices[0].FinishReason
	}
	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, stream, status,
		out.Usage.PromptTokens, out.Usage.CompletionTokens, out.Usage.CachedPromptTokens(), 0, stopReason,
		string(reqBody), string(raw), time.Since(start))
}

func (s *Server) relayOpenAIStream(
	w http.ResponseWriter,
	resp *http.Response,
	r *http.Request,
	incomingModel, targetModel string,
	reqBody []byte,
	start time.Time,
	resample func([]byte) (*http.Response, error),
) {
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error",
			"streaming not supported by this server")
		return
	}

	reader := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "api_error",
				"could not decompress upstream stream: "+err.Error())
			return
		}
		defer gz.Close()
		reader = gz
	}

	copyOpenAIHeaders(w.Header(), resp.Header, true)
	// An upstream 429 relayed verbatim keeps its own Retry-After (copied above);
	// when the upstream sent none, synthesise a real cooldown so the next hop
	// backs off instead of hot-looping.
	if resp.StatusCode == http.StatusTooManyRequests {
		s.stampCooldown(w, resp.Header, targetModel)
	}
	w.WriteHeader(resp.StatusCode)
	flusher.Flush()

	var responseLog strings.Builder
	// Phantom free-tier marker tools ("shell"/"read") are injected upstream but
	// never declared by this client. Resolve them to the client's real tools so
	// an OpenAI-compatible client never sees a tool it does not have.
	relay := &openAIStreamRelay{
		dst:      w,
		flusher:  flusher,
		log:      &responseLog,
		logLimit: s.cfg.Snapshot().MaxBodyLogBytes,
		filter:   newChatToolCallFilterWithSchemas(declaredOpenAIToolNames(reqBody), declaredOpenAIToolSchemas(reqBody)),
		hold:     true,
	}
	if _, err := io.Copy(relay, reader); err != nil {
		errPayload, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "upstream stream error: " + err.Error(),
				"type":    "api_error",
			},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", errPayload)
		flusher.Flush()
		s.logFailed(r.Context(), r, incomingModel, targetModel, true,
			http.StatusBadGateway, err.Error(), reqBody, time.Since(start))
		return
	}
	_ = relay.Flush()

	if !relay.hasVisible && !relay.wroteAny && resp.StatusCode == http.StatusOK {
		// A 200 stream with no text and no tool call is not a usable
		// completion; the free tier answers reasoning-only turns this way.
		// Text is never held: the first visible delta is streamed through, so
		// a normal answer keeps its token-by-token latency. A narrated stall
		// can no longer be retracted once streamed, so it is not rescued here.
		rescued := s.rescueEmptyOpenAIStream(w, flusher, reqBody, relay.logLimit,
			resample, incomingModel, targetModel)
		if rescued != nil {
			relay = rescued
		} else if relay.held.Len() > 0 {
			// Nothing rescued: release the original stream so the client still
			// receives a well-formed completion.
			_, _ = w.Write(relay.held.Bytes())
			relay.held.Reset()
			flusher.Flush()
		}
	}

	if summary := relay.filter.summary(); summary != "" {
		log.Printf("opencode-cc: chat stream tool filter %s model=%s target=%s",
			summary, incomingModel, targetModel)
	}

	s.logSuccessWithCache(r.Context(), r, incomingModel, targetModel, true, resp.StatusCode,
		relay.inputTokens, relay.outputTokens, relay.cachedInputTokens, 0, relay.stopReason,
		string(reqBody), relay.log.String(), time.Since(start))
}

// rescueEmptyOpenAIStream replaces an empty 200 chat completion with one
// retry. It buffers the candidate response fully before touching the client:
// a retry that turns out empty or errors midway must not append a second
// message to the open SSE response.
func (s *Server) rescueEmptyOpenAIStream(
	w http.ResponseWriter,
	flusher http.Flusher,
	reqBody []byte,
	logLimit int,
	resample func([]byte) (*http.Response, error),
	incomingModel, targetModel string,
) *openAIStreamRelay {
	if resample == nil {
		return nil
	}
	attempts := []struct {
		name string
		body []byte
	}{{"same", reqBody}}
	if nudged, ok := appendOpenAIContinueNudge(reqBody); ok {
		if forced, ok := forceOpenAIToolChoice(nudged); ok {
			nudged = forced
		}
		attempts = append(attempts, struct {
			name string
			body []byte
		}{"nudge", nudged})
	}
	for _, attempt := range attempts {
		resp, err := resample(attempt.body)
		if err != nil {
			log.Printf("opencode-cc: empty chat stream retry failed attempt=%s err=%v model=%s target=%s",
				attempt.name, err, incomingModel, targetModel)
			continue
		}
		if resp == nil {
			continue
		}
		if resp.StatusCode >= http.StatusBadRequest {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
			log.Printf("opencode-cc: empty chat stream retry status=%d attempt=%s model=%s target=%s",
				resp.StatusCode, attempt.name, incomingModel, targetModel)
			continue
		}
		reader := io.Reader(resp.Body)
		var gz *gzip.Reader
		if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
			gz, err = gzip.NewReader(resp.Body)
			if err != nil {
				_ = resp.Body.Close()
				continue
			}
			reader = gz
		}
		var buf bytes.Buffer
		candidate := &openAIStreamRelay{
			dst:      &buf,
			flusher:  noopStreamFlusher{},
			log:      &strings.Builder{},
			logLimit: logLimit,
			filter:   newChatToolCallFilterWithSchemas(declaredOpenAIToolNames(attempt.body), declaredOpenAIToolSchemas(attempt.body)),
			hold:     true,
		}
		n, cerr := io.Copy(candidate, io.LimitReader(reader, maxResponseBytes+1))
		_ = candidate.Flush()
		if gz != nil {
			_ = gz.Close()
		}
		_ = resp.Body.Close()
		if cerr != nil || n > maxResponseBytes {
			log.Printf("opencode-cc: empty chat stream retry unreadable attempt=%s err=%v model=%s target=%s",
				attempt.name, cerr, incomingModel, targetModel)
			continue
		}
		if !candidate.hasVisible {
			log.Printf("opencode-cc: empty chat stream retry still empty attempt=%s out=%d model=%s target=%s",
				attempt.name, candidate.outputTokens, incomingModel, targetModel)
			continue
		}
		// The candidate held its leading frames (role/empty delta) because
		// nothing was visible yet; once text arrives the relay writes later
		// frames straight to buf. Prepend the held prefix so the rescued
		// stream is complete, not a fragment starting at the first text delta.
		if candidate.held.Len() > 0 {
			var combined bytes.Buffer
			combined.Write(candidate.held.Bytes())
			combined.Write(buf.Bytes())
			candidate.held.Reset()
			buf = combined
		}
		if _, err := w.Write(buf.Bytes()); err != nil {
			return nil
		}
		flusher.Flush()
		log.Printf("opencode-cc: empty chat stream rescued attempt=%s out=%d model=%s target=%s",
			attempt.name, candidate.outputTokens, incomingModel, targetModel)
		return candidate
	}
	return nil
}

type noopStreamFlusher struct{}

func (noopStreamFlusher) Flush() {}

// forceOpenAIToolChoice turns an OpenAI Chat body into a forced tool call when
// it declares tools. Used only by rescue retries: the model has already
// narrated the next action and ended its turn, so a second text-only sample is
// useless. Bodies without tools are left unchanged.
func forceOpenAIToolChoice(body []byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, false
	}
	var tools []json.RawMessage
	if raw, ok := payload["tools"]; !ok || json.Unmarshal(raw, &tools) != nil || len(tools) == 0 {
		return nil, false
	}
	payload["tool_choice"] = json.RawMessage(`"required"`)
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}

// appendOpenAIContinueNudge appends one user turn asking for the next action.
// Used only after an identical retry of an empty completion is also empty.
func appendOpenAIContinueNudge(body []byte) ([]byte, bool) {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, false
	}
	var messages []json.RawMessage
	if raw, ok := payload["messages"]; ok {
		if json.Unmarshal(raw, &messages) != nil {
			return nil, false
		}
	}
	if len(messages) == 0 {
		return nil, false
	}
	nudge, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": "Continue with the next action now. Do not reply with an empty message.",
	})
	if err != nil {
		return nil, false
	}
	messages = append(messages, nudge)
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, false
	}
	payload["messages"] = raw
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false
	}
	return out, true
}

// openAIStreamRelay writes upstream bytes to the client and flushes every
// write. It re-encodes only tool-bearing SSE data lines (to resolve phantom
// free-tier marker tool names into the client's declared tools); every other
// byte is relayed untouched.
type openAIStreamRelay struct {
	dst      io.Writer
	flusher  http.Flusher
	log      *strings.Builder
	logLimit int
	filter   *chatToolCallFilter

	pending           []byte
	inputTokens       int
	outputTokens      int
	cachedInputTokens int
	stopReason        string

	// hold keeps the response off the wire until the first visible delta
	// (text or tool call). A 200 stream with no visible output is the free
	// tier's reasoning-only stop; holding lets the server replace it with a
	// rescued sample instead of handing the client an empty completion.
	hold        bool
	held        bytes.Buffer
	hasVisible  bool
	hasToolCall bool
	// wroteAny records that at least one byte reached the client. Once that
	// happens a rescue sample cannot be substituted (it would append a
	// second completion after [DONE]); the reasoning-only turn is passed
	// through instead.
	wroteAny bool
}

// heldStreamLimit bounds the reasoning-only prefix buffered before a visible
// delta. A text answer releases on its first delta, so this only caps a
// pathological reasoning stream.
const heldStreamLimit = 1 << 20

func (r *openAIStreamRelay) Write(p []byte) (int, error) {
	r.pending = append(r.pending, p...)
	for {
		i := bytes.IndexByte(r.pending, '\n')
		if i < 0 {
			break
		}
		line := r.pending[:i+1]
		r.pending = r.pending[i+1:]
		if err := r.emitLine(line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush forwards any trailing bytes that never got a newline terminator.
func (r *openAIStreamRelay) Flush() error {
	if len(r.pending) == 0 {
		return nil
	}
	line := r.pending
	r.pending = nil
	return r.emitLine(line)
}

// emitLine filters and forwards one complete SSE line (newline included).
func (r *openAIStreamRelay) emitLine(line []byte) error {
	body := line
	trailer := []byte("\n")
	if n := len(body); n > 0 && body[n-1] == '\n' {
		body = body[:n-1]
		trailer = line[n-1:]
	}

	out := body
	if r.filter != nil {
		// Rewrite only the JSON payload, keeping the "data:" prefix and the
		// original line terminator. Dropping either would corrupt the SSE
		// framing: the client would ignore the line as a nameless event.
		if data, ok := sseDataPayload(body); ok {
			if filtered, changed := r.filter.filterChatToolCalls(data); changed {
				rewritten := make([]byte, 0, len(body))
				rewritten = append(rewritten, body[:sseDataOffset(body)]...)
				rewritten = append(rewritten, filtered...)
				if rest := body[sseDataOffset(body)+len(data):]; len(rest) > 0 {
					rewritten = append(rewritten, rest...)
				}
				out = rewritten
			}
		}
	}

	r.observe(out)

	// Hold only until the first visible delta. A reasoning-only 200 stream
	// needs to be buffered so it can be replaced by a rescued sample, but a
	// text answer must stream token by token: holding it until a tool call
	// would turn every text response into a full-response buffer.
	if r.hold && !r.hasVisible {
		r.held.Write(out)
		r.held.Write(trailer)
		appendLimited(r.log, string(out), r.logLimit)
		appendLimited(r.log, string(trailer), r.logLimit)
		// Bound the buffer: a stream that emits only reasoning must not grow
		// without limit while we wait for a visible delta.
		if r.held.Len() > heldStreamLimit {
			r.hold = false
			if _, err := r.dst.Write(r.held.Bytes()); err != nil {
				return err
			}
			r.wroteAny = true
			r.held.Reset()
			r.flusher.Flush()
		}
		return nil
	}
	if r.hold && r.held.Len() > 0 {
		if _, err := r.dst.Write(r.held.Bytes()); err != nil {
			return err
		}
		r.wroteAny = true
		r.held.Reset()
	}
	if _, err := r.dst.Write(out); err != nil {
		return err
	}
	if _, err := r.dst.Write(trailer); err != nil {
		return err
	}
	r.wroteAny = true
	appendLimited(r.log, string(out), r.logLimit)
	appendLimited(r.log, string(trailer), r.logLimit)
	r.flusher.Flush()
	return nil
}

// sseDataPayload returns the JSON payload of an SSE "data:" line.
func sseDataPayload(line []byte) ([]byte, bool) {
	start := sseDataOffset(line)
	if start < 0 {
		return nil, false
	}
	end := bytes.IndexByte(line[start:], '\n')
	if end < 0 {
		end = len(line)
	} else {
		end += start
	}
	data := bytes.TrimSpace(bytes.TrimRight(line[start:end], "\r\n"))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil, false
	}
	return data, true
}

// sseDataOffset returns the index where an SSE "data:" line's payload begins,
// or -1 when the line is not a data line. The payload is everything after the
// "data:" field name and its single optional space, matching the SSE spec: a
// line that does not start with "data:" is a field-less event line.
func sseDataOffset(line []byte) int {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return -1
	}
	off := len("data:")
	if off < len(line) && line[off] == ' ' {
		off++
	}
	return off
}

func (r *openAIStreamRelay) observe(line []byte) {
	data, ok := sseDataPayload(line)
	if !ok {
		return
	}
	var chunk proxy.OpenAIStreamChunk
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	if chunk.Usage != nil {
		r.inputTokens = chunk.Usage.PromptTokens
		r.outputTokens = chunk.Usage.CompletionTokens
		r.cachedInputTokens = chunk.Usage.CachedPromptTokens()
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil {
			r.stopReason = *choice.FinishReason
		}
		// Reasoning alone is invisible to OpenAI chat clients: T3's DSH
		// adapter treats it as "completed response with no content". Only
		// text or a tool call makes a sample usable.
		if choice.Delta.Content != "" {
			r.hasVisible = true
		}
		if len(choice.Delta.ToolCalls) > 0 || choice.Delta.FunctionCall != nil {
			r.hasVisible = true
			r.hasToolCall = true
		}
	}
}

func copyOpenAIHeaders(dst, src http.Header, streaming bool) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if isHopByHopHeader(lower) || lower == "content-length" || lower == "content-encoding" {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	if dst.Get("Content-Type") == "" {
		if streaming {
			dst.Set("Content-Type", "text/event-stream")
		} else {
			dst.Set("Content-Type", "application/json")
		}
	}
	if streaming {
		dst.Set("Cache-Control", "no-cache")
		dst.Set("X-Accel-Buffering", "no")
	}
}

func isHopByHopHeader(lower string) bool {
	switch lower {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func appendLimited(dst *strings.Builder, value string, limit int) {
	if limit <= 0 {
		_, _ = dst.WriteString(value)
		return
	}
	if dst.Len() >= limit {
		return
	}
	remaining := limit - dst.Len()
	if len(value) > remaining {
		value = value[:remaining]
	}
	_, _ = dst.WriteString(value)
}

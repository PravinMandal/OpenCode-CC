package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCountTokensRejectsMalformedBody: a body that is not Anthropic JSON has no
// token count. The handler used to ignore both the read and unmarshal errors
// and answer 200 with an estimate of an empty request.
func TestCountTokensRejectsMalformedBody(t *testing.T) {
	h := (&Server{}).CountTokens()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"messages":`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, rec.Body.String())
	}
	if body.Type != "error" || body.Error.Type != "invalid_request_error" {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}

func TestCountTokensRejectsNonPost(t *testing.T) {
	h := (&Server{}).CountTokens()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}

func TestCountTokensValidRequest(t *testing.T) {
	h := (&Server{}).CountTokens()
	body := `{"model":"m","system":"be brief","messages":[{"role":"user","content":"hello world"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	if out.InputTokens <= 1 {
		t.Fatalf("input_tokens = %d, want an estimate above the empty floor", out.InputTokens)
	}
}

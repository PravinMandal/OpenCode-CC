package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeNative struct {
	url   string
	token string
	err   error
}

func (f fakeNative) Enabled() bool { return true }
func (f fakeNative) Endpoint() (string, string, error) {
	return f.url, f.token, f.err
}

type recorderTransport struct {
	mu    sync.Mutex
	calls []*http.Request
}

func (r *recorderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req.Clone(req.Context()))
	r.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    req,
	}, nil
}

func zenRequest(t *testing.T, model string) *http.Request {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, "https://opencode.ai/inference/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer client-key")
	return req
}

func TestNativeTransportRoutesEveryFreeModel(t *testing.T) {
	var gotPath, gotToken, gotAuth, gotBody string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotToken, gotAuth = r.URL.Path, r.Header.Get("x-bridge-token"), r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer sidecar.Close()

	for _, model := range []string{
		"deepseek-v4.1-flash-free",
		"glm-5.3-flash-free",
		"oc/nemotron-3.5-lightning-free",
		"brand-new-model-free",
		"big-pickle",
	} {
		t.Run(model, func(t *testing.T) {
			base := &recorderTransport{}
			tr := newNativeTransport(base, fakeNative{url: sidecar.URL + "/v1/chat/completions", token: "tok"})
			req := zenRequest(t, model)
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if len(base.calls) != 0 {
				t.Fatalf("free model went to the Go transport %d time(s)", len(base.calls))
			}
			if gotPath != "/v1/chat/completions" {
				t.Errorf("sidecar path = %q", gotPath)
			}
			if gotToken != "tok" {
				t.Errorf("bridge token = %q", gotToken)
			}
			if gotAuth != "" {
				t.Errorf("client Authorization leaked to sidecar: %q", gotAuth)
			}
			if !strings.Contains(gotBody, model) {
				t.Errorf("body did not preserve model: %q", gotBody)
			}
		})
	}
}

func TestNativeTransportLeavesPaidModelsAlone(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("paid model reached the sidecar")
	}))
	defer sidecar.Close()

	base := &recorderTransport{}
	tr := newNativeTransport(base, fakeNative{url: sidecar.URL + "/v1/chat/completions", token: "tok"})
	req := zenRequest(t, "claude-sonnet-5")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(base.calls) != 1 {
		t.Fatalf("base calls = %d, want 1", len(base.calls))
	}
}

func TestNativeTransportLeavesNonZenTrafficAlone(t *testing.T) {
	base := &recorderTransport{}
	tr := newNativeTransport(base, fakeNative{url: "http://127.0.0.1:1/v1/chat/completions", token: "tok"})
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"model":"glm-5.3-flash-free"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(base.calls) != 1 {
		t.Fatalf("base calls = %d, want 1", len(base.calls))
	}
}

func TestNativeTransportUnavailableIs503(t *testing.T) {
	base := &recorderTransport{}
	tr := newNativeTransport(base, fakeNative{err: errors.New("sidecar is still starting")})
	resp, err := tr.RoundTrip(zenRequest(t, "glm-5.3-flash-free"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "native transport unavailable") {
		t.Errorf("body = %s", body)
	}
	if len(base.calls) != 0 {
		t.Errorf("unavailable free request fell back to the Go transport")
	}
}

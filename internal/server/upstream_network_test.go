package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Reject actual SOCKS5 CONNECT handshakes with reply 4 (host unreachable),
// then forward connections. This reproduces net/http's real wrapped error.
func networkFaultProxy(t *testing.T, target string, failures int64) (string, *atomic.Int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	hits := &atomic.Int64{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var head [4]byte
				if _, err := io.ReadFull(conn, head[:2]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(head[1])); err != nil {
					return
				}
				conn.Write([]byte{5, 0})
				if _, err := io.ReadFull(conn, head[:]); err != nil {
					return
				}
				length := 4
				switch head[3] {
				case 3:
					var size [1]byte
					if _, err := io.ReadFull(conn, size[:]); err != nil {
						return
					}
					length = int(size[0])
				case 4:
					length = 16
				}
				if _, err := io.CopyN(io.Discard, conn, int64(length+2)); err != nil {
					return
				}
				if hits.Add(1) <= failures {
					conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				upstream, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer upstream.Close()
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				go io.Copy(upstream, conn)
				io.Copy(conn, upstream)
			}()
		}
	}()
	return "socks5://" + listener.Addr().String(), hits
}

func TestProxyHostUnreachableRecoversBeforeFailingClient(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		io.WriteString(w, `{"id":"test","choices":[{"index":0,"message":{"role":"assistant","content":"recovered"},"finish_reason":"stop"}]}`)
	}))
	defer target.Close()
	proxy, hits := networkFaultProxy(t, strings.TrimPrefix(target.URL, "http://"), 1)
	u, _ := url.Parse(proxy)
	srv := New(cfgFor(t, "http://upstream.invalid"), nil)
	srv.httpClient.Transport.(*http.Transport).Proxy = http.ProxyURL(u)
	defer srv.httpClient.CloseIdleConnections()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
	rr := httptest.NewRecorder()
	srv.OpenAIProxy()(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "recovered") || hits.Load() != 2 {
		t.Fatalf("status=%d attempts=%d body=%s", rr.Code, hits.Load(), rr.Body.String())
	}
}

func TestPersistentProxyFailureReturnsShortServiceRetry(t *testing.T) {
	proxy, hits := networkFaultProxy(t, "", 100)
	u, _ := url.Parse(proxy)
	srv := New(cfgFor(t, "http://upstream.invalid"), nil)
	srv.httpClient.Transport.(*http.Transport).Proxy = http.ProxyURL(u)
	defer srv.httpClient.CloseIdleConnections()
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
		if path == "/v1/messages" {
			srv.Proxy()(rr, req)
		} else {
			srv.OpenAIProxy()(rr, req)
		}
		if rr.Code != 503 || rr.Header().Get("Retry-After") != "2" {
			t.Fatalf("%s status=%d headers=%v", path, rr.Code, rr.Header())
		}
	}
	if hits.Load() != 6 {
		t.Fatalf("unbounded retry: %d attempts, want 3 per request", hits.Load())
	}
}

// The native sidecar applies its own bounded retry and deadline before it
// emits a 5xx. Retrying that response here doubled the user-facing stall and
// held a second inflight slot for the same terminal failure. Direct Zen 5xx
// still retries once (covered by TestProxyHostUnreachable...).
func TestUpstreamRetrySkipsNativeSidecar5xx(t *testing.T) {
	var hits atomic.Int64
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("x-oc-native", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":{"message":"upstream transport failed"}}`)
	}))
	defer sidecar.Close()

	req, _ := http.NewRequest(http.MethodPost, sidecar.URL, strings.NewReader(`{}`))
	started := time.Now()
	resp, err := doUpstreamWithRetry(sidecar.Client(), req, []byte(`{}`))
	elapsed := time.Since(started)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("native 502 should surface as an error, not a response")
	}
	if hits.Load() != 1 {
		t.Fatalf("native sidecar 502 attempted %d times, want exactly 1", hits.Load())
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("native sidecar 502 waited %s; the retry sleep should be skipped", elapsed)
	}
	var statusErr *upstreamStatusError
	if !errors.As(err, &statusErr) || statusErr.status != http.StatusBadGateway {
		t.Fatalf("error = %v, want upstreamStatusError 502", err)
	}
}

func TestProxyConnectivityClassificationBoundaries(t *testing.T) {
	for _, message := range []string{"socks connect tcp 127.0.0.1:40000->host:443: unknown error host unreachable", "proxyconnect tcp: connection refused"} {
		if !isProxyConnectivityFailure(errors.New(message)) {
			t.Fatalf("missed temporary connection failure: %s", message)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("socks connect: authentication failed"), errors.New("socks connect: connection not allowed"), errors.New("401 invalid key"), errors.New("429 quota exhausted"), errors.New("upstream network unreachable")} {
		if isProxyConnectivityFailure(err) {
			t.Fatalf("incorrectly classified: %v", err)
		}
		if got := upstreamFailureStatus(httptest.NewRecorder(), err); got != 502 {
			t.Fatalf("changed unrelated error status: %d", got)
		}
	}
}

func TestProxyRecoveryDoesNotReplayAuthOrQuotaResponses(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		var hits atomic.Int64
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"socks connect host unreachable in a provider response"}}`)
		}))
		req, _ := http.NewRequest(http.MethodPost, upstream.URL, strings.NewReader("{}"))
		response, err := doUpstreamWithRetry(upstream.Client(), req, []byte("{}"))
		if err != nil || response.StatusCode != status || response.Header.Get("Retry-After") != "3600" || hits.Load() != 1 {
			t.Fatalf("auth/quota response was retried or changed: status=%d hits=%d err=%v", status, hits.Load(), err)
		}
		response.Body.Close()
		upstream.Close()
	}
}

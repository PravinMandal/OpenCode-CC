package server

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/PravinMandal/OpenCode-CC/internal/config"
)

// TestShutdownPublishesServerBeforeStartup: a signal can arrive while Start is
// still bringing the sidecar up. The http.Server must already be reachable by
// Shutdown, and the second Shutdown must be safe.
func TestShutdownPublishesServerBeforeStartup(t *testing.T) {
	cfg := config.Default()
	cfg.ListenAddr = "127.0.0.1:0"
	srv := New(cfg, nil)

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(http.NotFoundHandler()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		srv.mu.Lock()
		published := srv.srv != nil
		srv.mu.Unlock()
		if published {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start never published the http.Server")
		}
		time.Sleep(time.Millisecond)
	}

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

// TestShutdownBeforeStartIsSafe covers the serve-error path: Shutdown may run
// before Start ever published a server.
func TestShutdownBeforeStartIsSafe(t *testing.T) {
	srv := &Server{done: make(chan struct{})}
	if err := srv.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-srv.done:
	default:
		t.Fatal("Shutdown did not close done")
	}
}

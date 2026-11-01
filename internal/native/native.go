// Package native owns the Bun process that carries free-tier traffic.
//
// Zen's free tier accepts a request only when it arrives over the same
// TLS/HTTP stack as the stock opencode CLI. Byte-for-byte replay of a request
// that Bun completes successfully is answered with FreeUsageLimitError when
// sent from Go, Node, Python or curl, so the transport cannot be ported into
// Go. Instead this package keeps the one Bun hop and hides it: opencode-cc
// starts the process on an ephemeral loopback port, keeps it alive, and stops
// it on shutdown. There is no fixed port and no separate service to manage.
package native

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

//go:embed transport.mjs capture.mjs
var assets embed.FS

// ErrDisabled is returned when the manager has no data directory to stage its
// scripts in, which is the case for unit tests that never start a server.
var ErrDisabled = errors.New("native free-tier transport is disabled")

// Options configures a Manager.
type Options struct {
	// DataDir is where the runtime scripts are written. Empty disables the
	// manager so constructing a Server in a test never spawns a process.
	DataDir string
	// IdentityDir overrides the captured opencode identity directory. The
	// default is ~/.local/share/opencode-native, shared with older installs so
	// an existing capture keeps working across upgrades.
	IdentityDir string
	// BunPath overrides Bun discovery. Empty uses OPENCODE_CC_BUN, then the
	// per-user install, then PATH.
	BunPath string
	// ReadyTimeout bounds how long Start waits for the first port announcement.
	ReadyTimeout time.Duration
}

// Manager supervises the Bun child process.
type Manager struct {
	dir         string
	identityDir string
	bun         string
	token       string
	readyWait   time.Duration

	mu      sync.RWMutex
	cmd     *exec.Cmd
	port    int
	running bool
	err     error

	stop      chan struct{}
	stopOnce  sync.Once
	started   chan struct{}
	startOnce sync.Once
	done      chan struct{}
	ready     chan struct{}
	readyOnce sync.Once
}

// New creates a manager. It does not spawn anything; call Start.
func New(opts Options) *Manager {
	dir := ""
	if opts.DataDir != "" {
		dir = filepath.Join(opts.DataDir, "native")
	}
	identityDir := opts.IdentityDir
	if identityDir == "" {
		identityDir = defaultIdentityDir()
	}
	bun := opts.BunPath
	if bun == "" {
		bun = findBun()
	}
	readyWait := opts.ReadyTimeout
	if readyWait <= 0 {
		readyWait = 10 * time.Second
	}
	m := &Manager{
		dir:         dir,
		identityDir: identityDir,
		bun:         bun,
		token:       randomToken(),
		readyWait:   readyWait,
		stop:        make(chan struct{}),
		started:     make(chan struct{}),
		done:        make(chan struct{}),
		ready:       make(chan struct{}),
	}
	if dir == "" {
		m.err = ErrDisabled
	}
	return m
}

// Enabled reports whether this manager can ever serve traffic.
func (m *Manager) Enabled() bool {
	return m != nil && m.dir != ""
}

// Start stages the scripts and launches the supervisor. It waits up to the
// ready timeout for the first successful spawn, then returns even if the
// sidecar is still retrying: paid models must keep working regardless.
func (m *Manager) Start(ctx context.Context) {
	if !m.Enabled() {
		return
	}
	m.startOnce.Do(func() {
		go m.run()
	})
	timer := time.NewTimer(m.readyWait)
	defer timer.Stop()
	select {
	case <-m.ready:
	case <-ctx.Done():
	case <-timer.C:
		if _, _, err := m.Endpoint(); err != nil {
			log.Printf("opencode-cc: native free-tier transport not ready: %v", err)
		}
	}
}

// Stop terminates the child and waits for the supervisor to exit.
func (m *Manager) Stop() {
	if !m.Enabled() {
		return
	}
	select {
	case <-m.started:
	default:
		return // never started
	}
	m.stopOnce.Do(func() { close(m.stop) })
	m.mu.RLock()
	cmd := m.cmd
	m.mu.RUnlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	select {
	case <-m.done:
	case <-time.After(6 * time.Second):
		if cmd != nil && cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-m.done:
		case <-time.After(2 * time.Second):
		}
	}
}

// Endpoint returns the loopback URL for the sidecar and the admission token.
func (m *Manager) Endpoint() (string, string, error) {
	if !m.Enabled() {
		return "", "", ErrDisabled
	}
	m.mu.RLock()
	port, err := m.port, m.err
	m.mu.RUnlock()
	if port == 0 {
		if err == nil {
			err = errors.New("sidecar is still starting")
		}
		return "", "", err
	}
	return fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), m.token, nil
}

// Port reports the current sidecar port, or zero when it is not running.
func (m *Manager) Port() int {
	if !m.Enabled() {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.port
}

func (m *Manager) run() {
	close(m.started)
	defer close(m.done)

	if m.bun == "" {
		err := errors.New("Bun is not installed; install it or set OPENCODE_CC_BUN")
		m.setErr(err)
		m.readyOnce.Do(func() { close(m.ready) })
		log.Printf("opencode-cc: %v", err)
		return
	}
	if err := m.stage(); err != nil {
		m.setErr(err)
		m.readyOnce.Do(func() { close(m.ready) })
		log.Printf("opencode-cc: native free-tier transport disabled: %v", err)
		return
	}

	backoff := time.Second
	for {
		select {
		case <-m.stop:
			return
		default:
		}

		cmd, port, err := m.spawn()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			m.setErr(err)
			log.Printf("opencode-cc: native transport start failed: %v (retrying in %s)", err, backoff)
			if !m.sleep(backoff) {
				return
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}

		backoff = time.Second
		m.setRunning(cmd, port)
		m.readyOnce.Do(func() { close(m.ready) })
		log.Printf("opencode-cc: native free-tier transport on 127.0.0.1:%d (pid %d)", port, cmd.Process.Pid)

		err = cmd.Wait()
		m.clearRunning(cmd)
		select {
		case <-m.stop:
			return
		default:
		}
		log.Printf("opencode-cc: native transport exited (%v); restarting", err)
		if !m.sleep(time.Second) {
			return
		}
	}
}

func (m *Manager) spawn() (*exec.Cmd, int, error) {
	cmd := exec.Command(m.bun, filepath.Join(m.dir, "transport.mjs"))
	cmd.Dir = m.dir
	cmd.Env = append(os.Environ(),
		"BRIDGE_PORT=0",
		"BRIDGE_HOST=127.0.0.1",
		"BRIDGE_TOKEN="+m.token,
		"BRIDGE_CAPTURE="+filepath.Join(m.dir, "capture.mjs"),
		"BRIDGE_IDENTITY_DIR="+m.identityDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}

	go drain(stderr, "native stderr")

	lines := make(chan string, 64)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return nil, 0, errors.New("sidecar exited before announcing its port")
			}
			if port := readyPort(line); port > 0 {
				go m.forward(lines)
				return cmd, port, nil
			}
			log.Printf("opencode-cc: native %s", line)
		case <-timeout.C:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, 0, errors.New("sidecar did not announce its port within 30s")
		case <-m.stop:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_ = cmd.Wait()
			return nil, 0, context.Canceled
		}
	}
}

// forward keeps draining stdout after readiness so a chatty sidecar can never
// block on a full pipe.
func (m *Manager) forward(lines <-chan string) {
	for line := range lines {
		log.Printf("opencode-cc: native %s", line)
	}
}

func drain(r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		log.Printf("opencode-cc: %s %s", prefix, scanner.Text())
	}
}

type readyEvent struct {
	Event string `json:"event"`
	Port  int    `json:"port"`
}

func readyPort(line string) int {
	var ev readyEvent
	if json.Unmarshal([]byte(line), &ev) != nil || ev.Event != "listening" || ev.Port <= 0 {
		return 0
	}
	return ev.Port
}

func (m *Manager) stage() error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(m.identityDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"transport.mjs", "capture.mjs"} {
		data, err := assets.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(m.dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-m.stop:
		return false
	}
}

func (m *Manager) setRunning(cmd *exec.Cmd, port int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cmd, m.port, m.running, m.err = cmd, port, true, nil
}

func (m *Manager) clearRunning(cmd *exec.Cmd) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == cmd {
		m.cmd, m.port, m.running = nil, 0, false
	}
}

func (m *Manager) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

func (m *Manager) unsafeErr() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.err
}

func defaultIdentityDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode-native")
}

func findBun() string {
	if p := os.Getenv("OPENCODE_CC_BUN"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".local", "lib", "opencode-native", "bun")
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("bun"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/local/bin/bun", "/usr/bin/bun"} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A predictable admission token is still better than refusing to run;
		// the listener is loopback-only, the token is defense in depth.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

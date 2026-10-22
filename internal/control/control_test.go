package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/api"
)

type fakeChecker struct {
	ok  bool
	msg string
}

func (f *fakeChecker) Healthy() (bool, string) { return f.ok, f.msg }

type fakeBackend struct {
	calls []string
	big   int
}

func (b *fakeBackend) Pause(scope string, d time.Duration) (string, error) {
	b.calls = append(b.calls, fmt.Sprintf("pause %s %s", scope, d))
	if scope == "bad/scope/x" {
		return "", fmt.Errorf("scope %q: want <project> or <project>/<service>", scope)
	}
	return "paused " + scope, nil
}
func (b *fakeBackend) Resume(scope string) (string, error) {
	b.calls = append(b.calls, "resume "+scope)
	return "", fmt.Errorf("%w: %s is not paused", ErrNothingToDo, scope)
}
func (b *fakeBackend) Unsuspend(scope string) (string, error) {
	b.calls = append(b.calls, "unsuspend "+scope)
	return "unsuspended " + scope, nil
}
func (b *fakeBackend) Status() any {
	return map[string]string{"filler": strings.Repeat("x", b.big)}
}

func parseFor(v string) (time.Duration, error) {
	if v == "" {
		return 2 * time.Hour, nil
	}
	return time.ParseDuration(v)
}

func serve(t *testing.T, h http.Handler) (sock string, stop func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wn") // short path: unix socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(dir, "w.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, h) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sock, func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(sock); err == nil {
			t.Error("socket not removed on shutdown")
		}
		os.RemoveAll(dir)
	}
}

func TestHealthOverUnixSocket(t *testing.T) {
	chk := &fakeChecker{ok: true, msg: "ok"}
	sock, stop := serve(t, healthOnly(chk))
	defer stop()
	ctx := context.Background()

	if err := Health(ctx, sock); err != nil {
		t.Fatalf("healthy daemon reported: %v", err)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi, err)
	}
	chk.ok, chk.msg = false, "docker events stream down for 5m0s"
	if err := Health(ctx, sock); err == nil {
		t.Fatal("unhealthy daemon reported healthy")
	}
	if code, _, _ := Get(ctx, sock, "/api/v1/status"); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
}

func TestCommandsOverTheSocket(t *testing.T) {
	be := &fakeBackend{}
	sock, stop := serve(t, Mux(api.HealthHandler(&fakeChecker{ok: true, msg: "ok"}), be, parseFor))
	defer stop()
	ctx := context.Background()

	if err := Health(ctx, sock); err != nil {
		t.Fatal(err)
	}
	msg, err := Command(ctx, sock, "pause", Request{Scope: "p/web", For: "90m"})
	if err != nil || msg != "paused p/web" {
		t.Fatalf("%q %v", msg, err)
	}
	if _, err := Command(ctx, sock, "pause", Request{Scope: "p"}); err != nil {
		t.Fatal(err)
	}
	if msg, err := Command(ctx, sock, "unsuspend", Request{Scope: "p"}); err != nil || msg != "unsuspended p" {
		t.Fatalf("%q %v", msg, err)
	}
	// A scope that matches nothing is a 404 with the reason, a malformed one a 400; both come back as errors.
	if _, err := Command(ctx, sock, "resume", Request{Scope: "p"}); err == nil || !strings.Contains(err.Error(), "is not paused") {
		t.Fatalf("%v", err)
	}
	if _, err := Command(ctx, sock, "pause", Request{Scope: "bad/scope/x"}); err == nil || !strings.Contains(err.Error(), "want <project>") {
		t.Fatalf("%v", err)
	}
	if _, err := Command(ctx, sock, "pause", Request{Scope: "p", For: "soon"}); err == nil {
		t.Fatal("a bad --for must be refused")
	}
	want := []string{"pause p/web 1h30m0s", "pause p 2h0m0s", "unsuspend p", "resume p", "pause bad/scope/x 2h0m0s"}
	if strings.Join(be.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("backend calls = %v, want %v", be.calls, want)
	}

	// Commands are POST only.
	if code, _, _ := Do(ctx, sock, http.MethodGet, "/v1/pause", nil); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/pause: %d", code)
	}
	if code, _, _ := Do(ctx, sock, http.MethodPost, "/v1/pause", nil); code != http.StatusBadRequest {
		t.Fatalf("POST without a body: %d", code)
	}
}

// The status reply is far larger than the 512 bytes the M1 client read.
func TestStatusIsNotTruncated(t *testing.T) {
	be := &fakeBackend{big: 200 << 10}
	sock, stop := serve(t, Mux(api.HealthHandler(&fakeChecker{ok: true}), be, parseFor))
	defer stop()
	code, body, err := Do(context.Background(), sock, http.MethodGet, "/v1/status", nil)
	if err != nil || code != 200 || len(body) < 200<<10 {
		t.Fatalf("code=%d len=%d err=%v", code, len(body), err)
	}
}

func TestCommandWithoutADaemonSaysSo(t *testing.T) {
	_, err := Command(context.Background(), filepath.Join(t.TempDir(), "none.sock"), "pause", Request{Scope: "p"})
	if err == nil || !strings.Contains(err.Error(), "cannot reach the whalenurse daemon") {
		t.Fatalf("%v", err)
	}
}

// healthOnly is a mux with /healthz and nothing else, like the TCP handler without a service (404 for the rest).
func healthOnly(c api.Checker) http.Handler {
	m := http.NewServeMux()
	m.Handle("/healthz", api.HealthHandler(c))
	return m
}

// A second whalenurse on the same socket path (a `docker exec ... whalenurse run --dry-run` beside the live daemon) must
// not take the path over: the CLI and the image HEALTHCHECK would reach the wrong process, and its exit would unlink the
// live daemon's socket.
func TestServeDoesNotTakeOverALiveSocket(t *testing.T) {
	sock, stop := serve(t, healthOnly(&fakeChecker{ok: true, msg: "one"}))
	defer stop()
	if err := CheckFree(sock); !errors.Is(err, ErrInUse) {
		t.Fatalf("CheckFree on a live socket: %v", err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx2, sock, healthOnly(&fakeChecker{ok: false, msg: "two"})) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInUse) {
			t.Fatalf("second Serve: %v, want ErrInUse", err)
		}
	case <-time.After(2 * time.Second):
		cancel2()
		<-done
		t.Fatal("the second Serve took over the live socket")
	}
	cancel2()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Health(ctx, sock); err != nil {
		t.Fatalf("the first daemon is no longer reachable: %v", err)
	}
}

// A stale socket (no process listening) is replaced, as before.
func TestServeReplacesAStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "wn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "w.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close() // what a killed process leaves behind
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, healthOnly(&fakeChecker{ok: true, msg: "ok"})) }()
	hctx, hcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer hcancel()
	for Health(hctx, sock) != nil && hctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if err := Health(hctx, sock); err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Something at the path that is not a socket is not deleted.
func TestServeLeavesARegularFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.sock")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, healthOnly(&fakeChecker{ok: true})) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve on a regular file: no error")
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("Serve replaced a regular file")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "x" {
		t.Fatalf("the file was changed: %q %v", b, err)
	}
}

// On shutdown, the path is removed only while it is still this process's socket.
func TestShutdownRemovesOnlyItsOwnSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "wn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "w.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sock, healthOnly(&fakeChecker{ok: true, msg: "A"})) }()
	hctx, hcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer hcancel()
	for Health(hctx, sock) != nil && hctx.Err() == nil {
		time.Sleep(10 * time.Millisecond)
	}
	// Someone else replaced the path (a newer process that found it, say, after a restart of A's container).
	if err := os.Remove(sock); err != nil {
		t.Fatal(err)
	}
	c, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c.(*net.UnixListener).SetUnlinkOnClose(false)
	defer c.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("A's shutdown removed the other listener's socket: %v", err)
	}
}

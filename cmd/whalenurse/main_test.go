package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/api"
	"github.com/jackspiering/whalenurse/internal/clock"
	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/daemon"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/notify"
)

type fakeDaemon struct{ calls []string }

func (f *fakeDaemon) Healthy() (bool, string) { return true, "ok" }
func (f *fakeDaemon) Pause(s string, d time.Duration) (string, error) {
	f.calls = append(f.calls, "pause "+s)
	return "paused", nil
}
func (f *fakeDaemon) Resume(s string) (string, error) {
	f.calls = append(f.calls, "resume "+s)
	return "resumed", nil
}
func (f *fakeDaemon) Unsuspend(s string) (string, error) {
	f.calls = append(f.calls, "unsuspend "+s)
	return "ok", nil
}
func (f *fakeDaemon) Status() any { f.calls = append(f.calls, "status"); return daemon.Status{} }

// There are no network-exposed write endpoints in v1: whatever the TCP listener is, it cannot pause,
// resume or unsuspend anything. Only the unix socket handler can.
func TestTCPHandlerHasNoWriteEndpoints(t *testing.T) {
	d := &fakeDaemon{}
	tcp, sock := handlers(d, api.Options{})

	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/pause"}, {"POST", "/v1/resume"}, {"POST", "/v1/unsuspend"}, {"GET", "/v1/status"},
		{"PUT", "/v1/pause"}, {"DELETE", "/v1/pause"},
	} {
		rec := httptest.NewRecorder()
		tcp.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"scope":"p"}`)))
		if rec.Code < 400 {
			t.Errorf("TCP handler answered %s %s with %d", tc.method, tc.path, rec.Code)
		}
	}
	if len(d.calls) != 0 {
		t.Fatalf("the TCP handler reached the daemon: %v", d.calls)
	}
	rec := httptest.NewRecorder()
	tcp.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	sock.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/pause", strings.NewReader(`{"scope":"p"}`)))
	if rec.Code != http.StatusOK || len(d.calls) != 1 {
		t.Fatalf("the socket handler should pause: %d %v", rec.Code, d.calls)
	}
}

func TestPrintStatus(t *testing.T) {
	var b bytes.Buffer
	printStatus(&b, daemon.Status{
		Host: "h", Mode: "label", Healthy: true, EventsUp: true, Settled: true,
		Engine: engine.Status{
			RateLimit: 10, ActionsLastH: 2,
			Pauses: []engine.Pause{{Scope: "p/web"}},
			Keys:   []engine.KeyStatus{{Key: "h/p/web/1", State: "SUSPENDED", Class: "unhealthy", SuspendReason: "ladder", Rung: 2, Flaps: 1}},
		},
	})
	out := b.String()
	for _, want := range []string{"host h", "2 actions in the last hour (limit 10)", "PAUSED", "p/web  until resume", "h/p/web/1", "SUSPENDED", "suspended: ladder", "rung 2 done"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
}

func TestSplitArgsAcceptsFlagsAfterThePositional(t *testing.T) {
	for _, args := range [][]string{{"p/web", "--for", "2h"}, {"--for", "2h", "p/web"}, {"--for=2h", "p/web"}} {
		fs := flag.NewFlagSet("pause", flag.ContinueOnError)
		dur := pauseFor(fs)
		pos, err := splitArgs(fs, args)
		if err != nil || len(pos) != 1 || pos[0] != "p/web" || *dur != "2h" {
			t.Errorf("%v: pos=%v dur=%q err=%v", args, pos, *dur, err)
		}
	}
}

// ---- notifications: the URLs are secrets (plan v3.7) ----------------------------------------------------------

// The tokens sit where real ones do: in the user info, in the path and in a query value, and in the host of a Discord URL.
var leakTokens = []string{"tok-USERINFO-81237", "tok-PATH-55120", "tok-QUERY-90421", "tok-DOWN-31337", "tok-PINGUUID-77777", "tok-HTTPTOKEN-66666",
	"reg-PASS-82002", "reg-IDTOKEN-82003", regAuthValue}

// The registry credentials of the pull rung (M7): the "auth" value is base64 of user:password.
var regAuthValue = base64.StdEncoding.EncodeToString([]byte("reg-USER-82001:reg-PASS-82002"))

func TestNoTokenReachesTheLogsStatusHealthzOrTheTestOutput(t *testing.T) {
	var statuses = map[string]int{"401": 401, "500": 500}
	var urls []string
	for name, code := range statuses {
		code := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope "+r.URL.Path, code) }))
		defer srv.Close()
		host := strings.TrimPrefix(srv.URL, "http://")
		switch name {
		case "401":
			urls = append(urls, "generic://u:tok-USERINFO-81237@"+host+"/hook/tok-PATH-55120?template=json&disabletls=yes&$k=tok-QUERY-90421")
		default:
			urls = append(urls, "generic://"+host+"/tok-PATH-55120/x?template=json&disabletls=yes&@Authorization=tok-QUERY-90421")
		}
	}
	// A connection that is refused (the client's error quotes the URL), a URL Shoutrrr rejects, and one that is not a URL.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadHost := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()
	urls = append(urls,
		"generic://"+deadHost+"/hook/tok-DOWN-31337?template=json&disabletls=yes",
		"nosuchservice://tok-USERINFO-81237@host/tok-PATH-55120",
		"tok-QUERY-90421-is-not-a-url")
	ping := "http://" + deadHost + "/ping/tok-PINGUUID-77777"

	regDir := t.TempDir()
	regFile := `{"auths": {"registry.example.lan:5000": {"auth": "` + regAuthValue + `"}, "ghcr.io": {"identitytoken": "reg-IDTOKEN-82003"}}}`
	if err := os.WriteFile(filepath.Join(regDir, "config.json"), []byte(regFile), 0o444); err != nil {
		t.Fatal(err)
	}
	env := []string{"WHALENURSE_REGISTRY_AUTH_DIR=" + regDir, "WHALENURSE_NOTIFY_URLS=" + strings.Join(urls, " "), "WHALENURSE_HEARTBEAT_URL=" + ping,
		"WHALENURSE_NOTIFY_TIMEOUT=2s", "WHALENURSE_HOST_ID=nas", "WHALENURSE_HTTP_TOKEN=tok-HTTPTOKEN-66666", "WHALENURSE_HTTP_ADDR=0.0.0.0:8484"}
	cfg, warns, err := config.FromEnv(env)
	if err != nil {
		t.Fatal(err)
	}

	notifyBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { notifyBackoff = nil })
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for _, w := range warns {
		log.Warn("config", "detail", w)
	}
	bus := &engine.Bus{}
	bus.Subscribe(daemon.LogSink(log))
	regAuth, err := loadRegistryAuth(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	nt, red := setupNotify(cfg, log, bus, nil, regAuth.secrets())
	// What compose's stderr can say about a refused login: the error text of a failed pull goes through the redactor.
	log.Warn("pull failed", "error", red.Redact("pull: unauthorized: user reg-USER-82001 password reg-PASS-82002 auth "+regAuthValue+" token reg-IDTOKEN-82003"))
	pinger := &notify.Pinger{URL: cfg.PingURL, Interval: time.Hour, Timeout: time.Second, Log: log, Redact: red}
	d := daemon.New(daemon.Options{Config: cfg, Reader: docker.NewFake(), Actor: docker.NewFake(), Clock: clock.Real{}, Bus: bus, Log: log,
		PullAuthHosts: regAuth.hosts(),
		NotifyStatus:  func() notify.Status { st := nt.Status(); ps := pinger.Status(); st.Ping = &ps; return st }})

	// Events that are sent, to every URL, and fail for each of the reasons above.
	key := "nas/p/web/1"
	bus.Publish(engine.Event{Host: "nas", Key: key, Container: "p-web-1", Type: engine.Suspended, Message: "every rung ran or was unavailable: unhealthy", Attrs: map[string]string{"reason": "ladder"}})
	bus.Publish(engine.Event{Host: "nas", Key: key, Container: "p-web-1", Type: engine.Recovered, Message: "healthy"})
	pinger.Tick(t.Context())
	nt.Shutdown(30 * time.Second) // delivers everything, retries included

	// `notify test`, and the status in every form it reaches a person.
	var test bytes.Buffer
	if err := runNotifyTest(t.Context(), &test, cfg); err == nil {
		t.Error("the test should report failures")
	}
	var text bytes.Buffer
	printStatus(&text, d.Status().(daemon.Status))
	js, _ := json.Marshal(d.Status())
	for _, w := range httpWarnings(cfg) {
		log.Warn("dashboard", "detail", w)
	}
	tcp, sock := handlers(d, api.Options{Token: cfg.HTTPToken})
	denied := httptest.NewRecorder()
	deniedReq := httptest.NewRequest("GET", "/api/v1/status", nil)
	deniedReq.Header.Set("Authorization", "Bearer wrong-"+cfg.HTTPToken)
	tcp.ServeHTTP(denied, deniedReq)
	if denied.Code != 401 {
		t.Errorf("a wrong token: %d", denied.Code)
	}
	healthz := httptest.NewRecorder()
	tcp.ServeHTTP(healthz, httptest.NewRequest("GET", "/healthz", nil))
	sockStatus := httptest.NewRecorder()
	sock.ServeHTTP(sockStatus, httptest.NewRequest("GET", "/v1/status", nil))

	outputs := map[string]string{
		"logs": logs.String(), "notify test": test.String(), "status text": text.String(), "status json": string(js),
		"healthz": healthz.Body.String(), "socket status": sockStatus.Body.String(),
		"401 body": denied.Body.String() + fmt.Sprint(denied.Header()),
	}
	for name, out := range outputs {
		if out == "" {
			t.Errorf("%s: no output to check", name)
		}
		for _, tok := range leakTokens {
			if strings.Contains(out, tok) {
				t.Errorf("%s leaks %q:\n%s", name, tok, out)
			}
		}
		for _, u := range append(urls, ping) {
			if strings.Contains(out, u) {
				t.Errorf("%s holds a whole URL: %s", name, u)
			}
		}
	}
	// The checks above mean something only if the failures happened and were reported.
	if !strings.Contains(test.String(), "FAILED") || !strings.Contains(test.String(), "#1 generic") || !strings.Contains(test.String(), "#4 nosuchservice") || !strings.Contains(test.String(), "#5 invalid") {
		t.Errorf("notify test:\n%s", test.String())
	}
	for name, out := range map[string]string{"status text": text.String(), "status json": string(js), "socket status": sockStatus.Body.String()} {
		if !strings.Contains(out, "registry.example.lan:5000") || !strings.Contains(out, "ghcr.io") {
			t.Errorf("%s does not name the registries:\n%s", name, out)
		}
		if strings.Contains(out, "reg-USER-82001") {
			t.Errorf("%s shows the registry user name:\n%s", name, out)
		}
	}
	if !strings.Contains(logs.String(), "registry credentials for the pull rung") || !strings.Contains(logs.String(), "***") {
		t.Errorf("the log lacks the registry line or the redacted pull error:\n%s", logs.String())
	}
	for _, want := range []string{"notification failed", "notification URL is invalid", "uptime ping failed"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, logs.String())
		}
	}
	if !strings.Contains(text.String(), "notifications:") || !strings.Contains(text.String(), "#1 generic") || !strings.Contains(text.String(), "uptime ping") {
		t.Errorf("status text shows no notification state:\n%s", text.String())
	}
}

func TestNotifyTestNeedsURLs(t *testing.T) {
	if err := runNotifyTest(t.Context(), &bytes.Buffer{}, config.Defaults()); err == nil {
		t.Fatal("no URLs: an error, not a silent success")
	}
}

func TestAllowedHostsAreIgnoredWithAToken(t *testing.T) {
	cfg := config.Defaults()
	cfg.HTTPAddr, cfg.HTTPToken, cfg.HTTPAllowedHosts = "127.0.0.1:8484", "a-long-random-token-0123456789", []string{"nas.example.org"}
	if got := httpWarnings(cfg); len(got) != 1 || !strings.Contains(got[0], "ALLOWED_HOSTS is ignored") {
		t.Fatalf("%v", got)
	}
}

func TestHTTPWarnings(t *testing.T) {
	for _, c := range []struct {
		addr, token string
		want        int
	}{
		{"127.0.0.1:8484", "", 0}, {"[::1]:8484", "", 0}, {"localhost:8484", "", 0},
		{":8484", "", 1}, {"0.0.0.0:8484", "", 1}, {"192.168.1.5:8484", "", 1},
		{":8484", "a-long-random-token-0123456789", 0}, {"127.0.0.1:8484", "xq7z", 1},
	} {
		cfg := config.Defaults()
		cfg.HTTPAddr, cfg.HTTPToken = c.addr, c.token
		if got := httpWarnings(cfg); len(got) != c.want {
			t.Errorf("%q token %q: %v", c.addr, c.token, got)
		}
		for _, w := range httpWarnings(cfg) {
			if c.token != "" && strings.Contains(w, c.token) {
				t.Errorf("the warning quotes the token: %s", w)
			}
		}
	}
}

func TestTrustedProxiesAreIgnoredWithoutAToken(t *testing.T) {
	cfg := config.Defaults()
	cfg.HTTPAddr, cfg.HTTPTrustedProxies = "127.0.0.1:8484", []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	if got := httpWarnings(cfg); len(got) != 1 || !strings.Contains(got[0], "TRUSTED_PROXIES is ignored without") {
		t.Fatalf("%v", got)
	}
	cfg.HTTPToken = "a-long-random-token-0123456789"
	if got := httpWarnings(cfg); len(got) != 0 {
		t.Fatalf("with a token the list is used: %v", got)
	}
}

// versionRunner answers `version --short` like a compose binary; nothing else may be run while the actors are picked.
type versionRunner struct{ t *testing.T }

func (r versionRunner) Run(_ context.Context, c compose.Command) (compose.Result, error) {
	if len(c.Args) == 2 && c.Args[0] == "version" && c.Args[1] == "--short" {
		return compose.Result{Stdout: []byte("5.6.0\n")}, nil
	}
	r.t.Errorf("unexpected compose command while wiring: %v", c.Args)
	return compose.Result{}, errors.New("unexpected")
}

type regularFile struct{ os.FileInfo }

func (regularFile) IsDir() bool { return false }

// Dry-run is structural: the daemon calls whatever actors the binary wires, so the binary's own wiring must give a dry run
// the no-op actors, for rung 1 and for the compose rungs, and a live run the real ones.
func TestDryRunWiresOnlyNoOpActors(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	lookup := compose.Lookup{
		Env: "/fake/docker-compose", Runner: versionRunner{t},
		Stat: func(string) (os.FileInfo, error) { return regularFile{}, nil },
	}
	cli := &docker.Fake{}
	for _, dry := range []bool{true, false} {
		cfg := config.Defaults()
		cfg.DryRun = dry
		cfg.ControlSocket = filepath.Join(t.TempDir(), "whalenurse.sock")

		rd, act, note, remove := composeFor(context.Background(), cfg, lookup, log)
		remove()
		if rd == nil || note != "" {
			t.Fatalf("dry=%v: no compose reader (note %q)", dry, note)
		}
		_, noop := act.(compose.DryRun)
		real, isReal := act.(*compose.Compose)
		if dry && !noop {
			t.Errorf("dry run: the compose actor is %T, want compose.DryRun", act)
		}
		if !dry && (!isReal || real != rd) {
			t.Errorf("live run: the compose actor is %T, want the *compose.Compose that is also the reader", act)
		}

		da := dockerActor(cfg, cli, log)
		_, noop = da.(docker.DryRun)
		if dry && !noop {
			t.Errorf("dry run: the Docker actor is %T, want docker.DryRun", da)
		}
		if !dry && da != docker.Actor(cli) {
			t.Errorf("live run: the Docker actor is %T, want the client", da)
		}
	}
}

// `run` takes --dry-run and nothing else. A positional argument stops Go's flag parsing, so `run dry-run` or
// `run -- --dry-run` would otherwise start a live healer during the evaluation the docs say to run as a dry run.
func TestRunRefusesPositionalArguments(t *testing.T) {
	for _, args := range [][]string{{"dry-run"}, {"--", "--dry-run"}, {"x", "--dry-run"}, {"--dry-run", "extra"}} {
		if dry, err := parseRunFlags(args); err == nil {
			t.Errorf("run %q: accepted (dry=%v), want an error", args, dry)
		}
	}
	for _, tc := range []struct {
		args []string
		dry  bool
	}{{nil, false}, {[]string{"--dry-run"}, true}, {[]string{"-dry-run"}, true}, {[]string{"--dry-run=false"}, false}} {
		dry, err := parseRunFlags(tc.args)
		if err != nil || dry != tc.dry {
			t.Errorf("run %q: dry=%v err=%v, want dry=%v", tc.args, dry, err, tc.dry)
		}
	}
}

// -h and --help print the usage and exit 0; they used to reach `run`'s flag set and exit 1.
func TestCommandDispatch(t *testing.T) {
	for _, tc := range []struct {
		args []string
		cmd  string
		rest int
	}{
		{nil, "run", 0}, {[]string{"--dry-run"}, "run", 1}, {[]string{"run", "--dry-run"}, "run", 1},
		{[]string{"-h"}, "help", 0}, {[]string{"--help"}, "help", 0}, {[]string{"-help"}, "help", 0},
		{[]string{"status", "--json"}, "status", 1}, {[]string{""}, "run", 1},
	} {
		cmd, rest := command(tc.args)
		if cmd != tc.cmd || len(rest) != tc.rest {
			t.Errorf("command(%q) = %q %q, want %q with %d arguments", tc.args, cmd, rest, tc.cmd, tc.rest)
		}
	}
}

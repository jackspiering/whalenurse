// Command whalenurse is a compose-aware self-healer for Docker hosts.
//
// `run` heals opted-in containers; `pause`, `resume`, `unsuspend` and `status`
// talk to the running daemon over its unix control socket; `healthcheck` is the
// image's HEALTHCHECK.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackspiering/whalenurse/internal/api"
	"github.com/jackspiering/whalenurse/internal/clock"
	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/control"
	"github.com/jackspiering/whalenurse/internal/daemon"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/notify"
	"github.com/jackspiering/whalenurse/internal/store"
	"github.com/jackspiering/whalenurse/internal/web"
)

// version is set with -ldflags "-X main.version=...".
var version = ""

func main() {
	cmd, args := command(os.Args[1:])
	var err error
	switch cmd {
	case "run":
		err = runCmd(args)
	case "healthcheck":
		err = healthCmd(args)
	case "pause", "resume", "unsuspend":
		err = scopeCmd(cmd, args)
	case "status":
		err = statusCmd(args)
	case "notify":
		err = notifyCmd(args)
	case "version":
		fmt.Println("whalenurse", buildVersion())
	case "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "whalenurse: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if errors.Is(err, flag.ErrHelp) {
		return // the flag set has printed its own usage
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "whalenurse:", err)
		os.Exit(1)
	}
}

// command splits the arguments into the command and its own arguments. The command is the first argument unless that
// is a flag (`whalenurse --dry-run` is `run --dry-run`); -h, -help and --help there ask for the usage.
func command(args []string) (string, []string) {
	if len(args) == 0 {
		return "run", nil
	}
	switch a := args[0]; {
	case a == "-h" || a == "-help" || a == "--help":
		return "help", args[1:]
	case a != "" && a[0] != '-':
		return a, args[1:]
	}
	return "run", args
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: whalenurse [run] [--dry-run]
       whalenurse pause <project>[/<service>] [--for 2h]
       whalenurse resume <project>[/<service>]
       whalenurse unsuspend <project>[/<service>]
       whalenurse status [--json]
       whalenurse notify test
       whalenurse healthcheck
       whalenurse version

run          watch Docker and heal opted-in containers (the default command)
pause        stop all actions for a project or service; --for takes 30m, 2h, 1d ... (default 2h; 0 = until resume)
resume       lift a pause
unsuspend    lift SUSPENDED so the ladder may run again
status       show incidents, SUSPENDED and PAUSED containers, flap counts and the rate-limit budget
notify test  send one test message to every WHALENURSE_NOTIFY_URLS URL and report each (needs no running daemon)
healthcheck  exit 0 if the running daemon reports healthy (for the image HEALTHCHECK)

pause, resume, unsuspend and status talk to the running daemon over its control socket
(WHALENURSE_CONTROL_SOCKET, default /run/whalenurse.sock): run them with docker exec.
`)
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}

// parseRunFlags reads the flags of `run`.
func parseRunFlags(args []string) (dry bool, err error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	d := fs.Bool("dry-run", false, "log decisions only; change nothing (same as WHALENURSE_DRY_RUN=true)")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	// Parsing stops at the first argument that is not a flag, so `run dry-run` or `run -- --dry-run` would leave the
	// flag unset and start a live healer.
	if fs.NArg() > 0 {
		return false, fmt.Errorf("run: unexpected argument %q (did you mean --dry-run?)", fs.Arg(0))
	}
	return *d, nil
}

func runCmd(args []string) error {
	dry, err := parseRunFlags(args)
	if err != nil {
		return err
	}
	cfg, warns, err := config.FromEnv(os.Environ())
	if err != nil {
		return err
	}
	if dry {
		cfg.DryRun = true
	}
	// A second `run` beside a live daemon (a dry run by docker exec, say) must not take its control socket over; the store
	// lock stops a second live run as well, but a dry run opens no store.
	if cfg.ControlSocket != "" {
		if err := control.CheckFree(cfg.ControlSocket); err != nil {
			return fmt.Errorf("%w: a whalenurse is already running here; give a second one its own WHALENURSE_CONTROL_SOCKET (and, for a live run, its own WHALENURSE_DATA_DIR)", err)
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	for _, w := range warns {
		log.Warn("config", "detail", w)
	}

	// The credentials of the pull rung (M7): checked first, like the rest of the configuration, so a bad file stops `run`
	// before it touches Docker, instead of failing the first pull.
	regAuth, err := loadRegistryAuth(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cli, err := docker.NewMoby()
	if err != nil {
		return err
	}
	defer cli.Close()
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = cli.Ping(pctx)
	cancel()
	if err != nil {
		return fmt.Errorf("cannot reach the Docker API (is the socket mounted, and is the user in its group?): %w", err)
	}

	selfID := ownContainerID()
	if selfID != "" {
		if me, err := cli.Inspect(ctx, selfID); err == nil && docker.ContainerRef(me.NetworkMode) != "" {
			log.Warn("whalenurse runs with network_mode container:<id>; it would become a dependent of a container it may heal. Do not do this.", "network_mode", me.NetworkMode)
		}
		log.Info("running in a container", "self", docker.ShortID(selfID))
	}
	if w := hostIDWarning(cfg.HostID, selfID); w != "" {
		log.Warn("config", "detail", w)
	}

	bus := &engine.Bus{}
	bus.Subscribe(daemon.LogSink(log))

	// State lives in SQLite under WHALENURSE_DATA_DIR. A dry run stores nothing: its
	// incidents and SUSPENDED states are not real, and must not be there for the next
	// real run to find.
	opts := daemon.Options{Config: cfg, Reader: cli, Clock: clock.Real{}, Bus: bus, Log: log, SelfID: selfID, Version: buildVersion()}
	var ledger notify.Ledger  // the notification dedupe is kept in the store too; a dry run has none
	var evlog engine.EventLog // what the API reads events from: the store, or a ring in memory in a dry run
	var history api.History
	hub := api.NewHub()
	if cfg.DryRun {
		log.Info("dry run: state is kept in memory only and nothing is written under the data directory")
		ring := engine.NewRing(0)
		bus.Subscribe(ring.Record)
		evlog = ring
	} else {
		st, snap, err := openStore(cfg, log, time.Now())
		if err != nil {
			return err
		}
		defer st.Close()
		opts.Persist, opts.Snapshot, opts.Meta = st, snap, st
		ledger, evlog, history = st, st, st
		// Event IDs are monotonic per host across restarts: the bus continues after the highest ID ever stored,
		// and the store records every event under the ID the bus gave it.
		bus.Seed(st.LastID())
		bus.Subscribe(st.RecordEvent)
		go pruneLoop(ctx, st, cfg, log)
	}
	bus.Subscribe(hub.Notify) // after the event log's own subscriber: the event is readable when the streams wake

	nt, red := setupNotify(cfg, log, bus, ledger, regAuth.secrets())
	defer nt.Shutdown(5 * time.Second) // the last message before a stop is often the one that matters

	opts.Actor = dockerActor(cfg, cli, log)
	if cfg.DryRun {
		log.Warn("DRY RUN: decisions are logged, no container is changed")
	}
	var removeConfigDir func()
	opts.PullAuthHosts = regAuth.hosts()
	opts.Compose, opts.ComposeActor, opts.ComposeNote, removeConfigDir = composeFor(ctx, cfg, compose.Lookup{Env: cfg.ComposeBin}, log)
	defer removeConfigDir()
	var pinger *notify.Pinger
	opts.NotifyStatus = func() notify.Status {
		st := nt.Status()
		if pinger != nil {
			ps := pinger.Status()
			st.Ping = &ps
		}
		return st
	}
	d := daemon.New(opts)
	if cfg.PingURL != "" {
		pinger = &notify.Pinger{
			URL: cfg.PingURL, Interval: cfg.PingInterval, Timeout: cfg.NotifyTimeout, Log: log, Redact: red,
			Healthy: func() bool { ok, _ := d.Healthy(); return ok },
		}
		go pinger.Run(ctx)
		log.Info("uptime ping on", "interval", cfg.PingInterval.String())
	}

	svc := daemon.NewAPI(d, evlog, history, hub, red.Redact)
	tcpHandler, sockHandler := handlers(d, api.Options{
		Service: svc, Token: cfg.HTTPToken, AllowedHosts: cfg.HTTPAllowedHosts, TrustedProxies: cfg.HTTPTrustedProxies, Web: web.Handler(svc),
		OnBlock: func(client string, d time.Duration) {
			log.Warn("dashboard: too many wrong tokens; the client is blocked", "client", client, "for", d.String())
		},
	})
	if cfg.ControlSocket != "" {
		go func() {
			if err := control.Serve(ctx, cfg.ControlSocket, sockHandler); err != nil {
				msg := "control socket unavailable; the image healthcheck will fail"
				if errors.Is(err, control.ErrInUse) {
					msg = "control socket in use by another whalenurse; this one has none (the CLI reaches the other)"
				}
				log.Error(msg, "path", cfg.ControlSocket, "error", err.Error())
			}
		}()
	}
	if cfg.HTTPAddr != "" {
		for _, w := range httpWarnings(cfg) {
			log.Warn("dashboard", "detail", w)
		}
		// No WriteTimeout: the event stream is open for as long as a browser tab is.
		srv := &http.Server{Addr: cfg.HTTPAddr, Handler: tcpHandler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 2 * time.Minute}
		srv.RegisterOnShutdown(hub.Close)
		go func() {
			log.Info("dashboard and read-only API listening", "addr", cfg.HTTPAddr, "token", cfg.HTTPToken != "", "metrics", cfg.Metrics, "extra_hosts_without_token", len(cfg.HTTPAllowedHosts), "trusted_proxies", len(cfg.HTTPTrustedProxies))
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http listener failed", "addr", cfg.HTTPAddr, "error", err.Error())
			}
		}()
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if srv.Shutdown(sctx) != nil {
				_ = srv.Close()
			}
		}()
	}

	if err := d.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("stopped")
	return nil
}

// hostIDWarning is the startup warning for a host ID that is the own container's ID: the default (the hostname) in a
// container without WHALENURSE_HOST_ID or hostname:, which changes whenever whalenurse is recreated.
func hostIDWarning(hostID, selfID string) string {
	if selfID == "" || hostID != docker.ShortID(selfID) {
		return ""
	}
	return "the host ID is this container's ID (its hostname, the default without WHALENURSE_HOST_ID), which changes whenever whalenurse is recreated: the state is carried over at startup, but keys, notifications and the API name another host after every upgrade. Set WHALENURSE_HOST_ID"
}

// openStore opens the state store, re-keys what it holds to the current host ID (plan v3.13 item 12: in a container the
// default host ID is the container's ID, which an upgrade changes) and loads it.
func openStore(cfg config.Global, log *slog.Logger, now time.Time) (*store.Store, engine.Snapshot, error) {
	st, err := store.Open(cfg.DataDir, log)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			return nil, engine.Snapshot{}, err
		}
		return nil, engine.Snapshot{}, fmt.Errorf("cannot open the state store in %s (is WHALENURSE_DATA_DIR a writable volume?): %w", cfg.DataDir, err)
	}
	n, err := st.Rekey(cfg.HostID, store.RekeyMeta{Keyed: daemon.MetaKeyed, WithKey: daemon.MetaWithKey})
	if err != nil {
		st.Close()
		return nil, engine.Snapshot{}, fmt.Errorf("cannot re-key the state store to the host ID %s: %w", cfg.HostID, err)
	}
	if n > 0 {
		log.Info("state store: moved the state of containers recorded under another host ID to this one", "keys", n, "host", cfg.HostID)
	}
	snap, err := st.Load(now)
	if err != nil {
		st.Close()
		return nil, engine.Snapshot{}, fmt.Errorf("cannot read the state store: %w", err)
	}
	log.Info("state store opened", "dir", cfg.DataDir, "keys", len(snap.Keys), "pauses", len(snap.Pauses))
	return st, snap, nil
}

// setupNotify makes the notifier one more subscriber of the event stream. The URLs hold tokens, so everything about them
// is logged by position and scheme ("#2 discord"), and every error goes through the redactor first. An invalid URL is
// reported and skipped: a notification problem never stops the healing.
// notifyBackoff is the wait before each retry of a failed delivery; nil means the notifier's default. Tests shorten it.
var notifyBackoff []time.Duration

func setupNotify(cfg config.Global, log *slog.Logger, bus *engine.Bus, ledger notify.Ledger, secrets []string) (*notify.Notifier, *notify.Redactor) {
	// The HTTP token and the registry credentials are redacted from everything the API shows as well.
	red := notify.NewRedactorWith(secrets, append(append([]string{}, cfg.NotifyURLs...), cfg.PingURL, cfg.HTTPToken)...)
	targets := notify.NewTargets(cfg.NotifyURLs, red, cfg.NotifyTimeout)
	for _, t := range targets {
		if t.Sender == nil {
			log.Error("notification URL is invalid and will not be used", "url", t.Label, "error", t.Err)
		}
	}
	nt := notify.New(notify.Options{
		Host: cfg.HostID, DryRun: cfg.DryRun, Log: log, Redact: red, Targets: targets,
		Timeout: cfg.NotifyTimeout, RateLimit: cfg.NotifyRateLimit, Backoff: notifyBackoff, Ledger: ledger,
	})
	bus.Subscribe(nt.Handle)
	if len(cfg.NotifyURLs) == 0 {
		log.Info("notifications are off: WHALENURSE_NOTIFY_URLS is not set")
	} else {
		log.Info("notifications", "urls", len(cfg.NotifyURLs), "per_url_limit_per_hour", cfg.NotifyRateLimit, "dry_run_marked", cfg.DryRun)
	}
	return nt, red
}

// composeFor finds the compose binary (plan §3.4: WHALENURSE_COMPOSE_BIN, then the host plugin mounted read-only, then
// the bundled copy) and builds the compose Reader and Actor. With none, rungs 2 and 3 are disabled by the version gate
// and the note says why. In dry-run mode the Actor is a no-op; the Reader still runs the drift guard, which only reads.
// The commands get an empty directory of their own as DOCKER_CONFIG, next to the control socket (writable in both
// deployments: /run is a tmpfs in the examples, the runtime directory under systemd) or else in the temp directory; the
// returned func removes it. The lookup is a parameter so a test can run this with a fake binary.
func composeFor(ctx context.Context, cfg config.Global, l compose.Lookup, log *slog.Logger) (compose.Reader, compose.Actor, string, func()) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bin, notes, err := compose.Resolve(rctx, l)
	if err != nil {
		log.Warn("no usable docker compose binary", "error", err.Error())
		return nil, nil, err.Error(), func() {}
	}
	log.Info("compose binary", "path", bin.Path, "source", string(bin.Source), "version", bin.VersionString(), "skipped", strings.Join(notes, "; "))
	var sockDir string
	if cfg.ControlSocket != "" {
		sockDir = filepath.Dir(cfg.ControlSocket)
	}
	dir, remove, err := compose.PrivateConfigDir(sockDir, os.TempDir())
	if err != nil {
		log.Warn("no private DOCKER_CONFIG directory for compose; compose before v5.6.0 then cannot dry-run and the drift guard uses config --hash",
			"error", err.Error(), "docker_config", compose.NoDockerConfig)
	}
	c := compose.New(*bin, compose.ExecRunner{ConfigDir: dir}).WithRegistryAuth(cfg.RegistryAuthDir)
	return c, composeActor(cfg, c, log), "", remove
}

// Dry-run is structural (m1-notes decision 8): the daemon calls its actors without asking whether this is a dry run, so
// these two functions are what keeps a dry run from changing anything. A test checks both.

// dockerActor is the Docker API actor of rung 1: the client itself, or in a dry run a no-op that logs.
func dockerActor(cfg config.Global, cli docker.Actor, log *slog.Logger) docker.Actor {
	if cfg.DryRun {
		return docker.DryRun{Would: func(verb, id string) { log.Info("dry-run: would act", "verb", verb, "id", docker.ShortID(id)) }}
	}
	return cli
}

// composeActor is the compose actor of rungs 2 and 3: c itself, or in a dry run a no-op that logs.
func composeActor(cfg config.Global, c *compose.Compose, log *slog.Logger) compose.Actor {
	if cfg.DryRun {
		return compose.DryRun{Would: func(verb string, p compose.Project) {
			log.Info("dry-run: would run compose", "verb", verb, "project", p.Name, "service", p.Service)
		}}
	}
	return c
}

func healthCmd(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _, err := config.FromEnv(os.Environ())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return control.Health(ctx, cfg.ControlSocket)
}

func ownContainerID() string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	return docker.SelfID(f)
}

// pruneLoop applies WHALENURSE_HISTORY_RETENTION once an hour.
func pruneLoop(ctx context.Context, st *store.Store, cfg config.Global, log *slog.Logger) {
	prune := func() {
		if n, err := st.Prune(time.Now(), cfg.HistoryRetention, cfg.FlapWindow); err != nil {
			log.Error("store prune failed", "error", err.Error())
		} else if n > 0 {
			log.Info("pruned old incident history", "incidents", n, "retention", cfg.HistoryRetention.String())
		}
		if err := st.PruneNotified(time.Now(), notify.DefaultDedupeFor); err != nil {
			log.Error("notification ledger prune failed", "error", err.Error())
		}
	}
	prune()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// splitArgs parses flags that may come before or after the positional arguments
// (`pause p/web --for 2h`), which flag.Parse alone does not.
func splitArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func socketPath() (string, error) {
	cfg, _, err := config.FromEnv(os.Environ())
	if err != nil {
		return "", err
	}
	return cfg.ControlSocket, nil
}

func scopeCmd(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var dur *string
	if name == "pause" {
		dur = pauseFor(fs)
	}
	pos, err := splitArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("usage: whalenurse %s <project>[/<service>]%s", name, map[bool]string{true: " [--for 2h]", false: ""}[name == "pause"])
	}
	sock, err := socketPath()
	if err != nil {
		return err
	}
	req := control.Request{Scope: pos[0]}
	if dur != nil {
		req.For = *dur
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	msg, err := control.Command(ctx, sock, name, req)
	if err != nil {
		return err
	}
	fmt.Println(msg)
	return nil
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw JSON")
	if _, err := splitArgs(fs, args); err != nil {
		return err
	}
	sock, err := socketPath()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, body, err := control.Do(ctx, sock, "GET", "/v1/status", nil)
	if err != nil {
		return fmt.Errorf("cannot reach the whalenurse daemon on %s (is it running, and are you in the same container?): %w", sock, err)
	}
	if code != 200 {
		return fmt.Errorf("status: HTTP %d: %.200s", code, body)
	}
	if *asJSON {
		fmt.Println(strings.TrimSpace(string(body)))
		return nil
	}
	var st daemon.Status
	if err := json.Unmarshal(body, &st); err != nil {
		return err
	}
	printStatus(os.Stdout, st)
	return nil
}

// notifyCmd is `whalenurse notify test`: one message to every URL, one line per URL. It reads the environment like the
// daemon does (so `docker exec` sees the container's), needs no daemon, and prints URLs by position and scheme only.
func notifyCmd(args []string) error {
	if len(args) != 1 || args[0] != "test" {
		return errors.New("usage: whalenurse notify test")
	}
	cfg, _, err := config.FromEnv(os.Environ())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runNotifyTest(ctx, os.Stdout, cfg)
}

// runNotifyTest sends the test message to every URL of cfg and prints one line per URL.
func runNotifyTest(ctx context.Context, w io.Writer, cfg config.Global) error {
	if len(cfg.NotifyURLs) == 0 {
		return errors.New("WHALENURSE_NOTIFY_URLS is not set: there is nothing to test")
	}
	red := notify.NewRedactor(cfg.NotifyURLs...)
	targets := notify.NewTargets(cfg.NotifyURLs, red, cfg.NotifyTimeout)
	defer func() {
		for _, t := range targets {
			if t.Sender != nil {
				_ = t.Sender.Close()
			}
		}
	}()
	res := notify.SendTest(ctx, targets, red, cfg.NotifyTimeout, notify.TestMessage(cfg.HostID, cfg.DryRun))
	if failed := notify.PrintTest(w, res); failed > 0 {
		return fmt.Errorf("%d of %d notification URLs failed", failed, len(res))
	}
	return nil
}

// regAuth is the checked WHALENURSE_REGISTRY_AUTH_DIR; the zero value (nil) is "off".
type regAuth struct{ a *compose.RegistryAuth }

func (r regAuth) secrets() []string {
	if r.a == nil {
		return nil
	}
	return r.a.Secrets
}

func (r regAuth) hosts() []string {
	if r.a == nil {
		return nil
	}
	return r.a.Hosts
}

// loadRegistryAuth checks WHALENURSE_REGISTRY_AUTH_DIR (plan v3.12 item 4). No message quotes the file's values.
func loadRegistryAuth(cfg config.Global, log *slog.Logger) (regAuth, error) {
	if cfg.RegistryAuthDir == "" {
		return regAuth{}, nil
	}
	a, err := compose.LoadRegistryAuth(cfg.RegistryAuthDir)
	if err != nil {
		return regAuth{}, fmt.Errorf("WHALENURSE_REGISTRY_AUTH_DIR: %w", err)
	}
	for _, w := range a.Warnings {
		log.Warn("WHALENURSE_REGISTRY_AUTH_DIR: " + w)
	}
	log.Info("registry credentials for the pull rung", "registries", strings.Join(a.Hosts, ", "))
	return regAuth{a}, nil
}

func printNotify(w io.Writer, n notify.Status) {
	fmt.Fprintf(w, "notifications: %s\n", n.Summary())
	if p := n.Ping; p != nil {
		line := fmt.Sprintf("uptime ping every %ds: %d sent", p.IntervalSec, p.Sent)
		if p.Failed > 0 {
			line += fmt.Sprintf(", %d failed (last: %s)", p.Failed, p.LastError)
		}
		if p.Skipped > 0 {
			line += fmt.Sprintf(", %d skipped while unhealthy", p.Skipped)
		}
		fmt.Fprintln(w, line)
	}
}

func printStatus(w io.Writer, st daemon.Status) {
	flags := []string{}
	if st.DryRun {
		flags = append(flags, "DRY RUN (nothing is stored or changed)")
	}
	if !st.Healthy {
		flags = append(flags, "UNHEALTHY: "+st.HealthDetail)
	}
	if !st.EventsUp {
		flags = append(flags, "docker events stream down")
	}
	if !st.Settled {
		flags = append(flags, "still in the startup settle period")
	}
	fmt.Fprintf(w, "host %s, mode %s", st.Host, st.Mode)
	if len(flags) > 0 {
		fmt.Fprintf(w, " [%s]", strings.Join(flags, "; "))
	}
	fmt.Fprintln(w)
	e := st.Engine
	rate := fmt.Sprintf("%d actions in the last hour", e.ActionsLastH)
	if e.RateLimit > 0 {
		rate += fmt.Sprintf(" (limit %d)", e.RateLimit)
	}
	fmt.Fprintln(w, rate)
	printCompose(w, st.Compose)
	printNotify(w, st.Notify)
	if len(st.PullAuth) > 0 {
		fmt.Fprintf(w, "pull credentials: %s\n", strings.Join(st.PullAuth, ", "))
	}
	if !e.RestoreUntil.IsZero() {
		fmt.Fprintf(w, "restore window open until %s: starts are restores, kills set no marker\n", e.RestoreUntil.Local().Format(time.TimeOnly))
	}
	if len(e.Pauses) > 0 {
		fmt.Fprintln(w, "\nPAUSED")
		for _, p := range e.Pauses {
			until := "until resume"
			if !p.Until.IsZero() {
				until = "until " + p.Until.Local().Format(time.RFC1123)
			}
			fmt.Fprintf(w, "  %s  %s\n", p.Scope, until)
		}
	}
	if len(e.Keys) == 0 {
		fmt.Fprintln(w, "\nno container has an incident, a flap count or a marker")
		return
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSTATE\tDETAIL\tFLAPS")
	for _, k := range e.Keys {
		var detail []string
		if k.Class != "" {
			detail = append(detail, k.Class)
		}
		if k.SuspendReason != "" {
			detail = append(detail, "suspended: "+k.SuspendReason)
		}
		if k.Rung > 0 {
			detail = append(detail, fmt.Sprintf("rung %d done", k.Rung))
		}
		if !k.CooldownUntil.IsZero() && k.CooldownUntil.After(time.Now()) {
			detail = append(detail, "cooldown until "+k.CooldownUntil.Local().Format(time.TimeOnly))
		}
		if !k.PausedUntil.IsZero() {
			detail = append(detail, "paused until "+k.PausedUntil.Local().Format(time.TimeOnly))
		}
		if k.ManualStop {
			detail = append(detail, "manual-stop marker")
		}
		if k.LoopIncreases > 0 {
			detail = append(detail, fmt.Sprintf("%d restarts counted", k.LoopIncreases))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", k.Key, k.State, strings.Join(detail, ", "), k.Flaps)
	}
	tw.Flush()
}

func printCompose(w io.Writer, c daemon.ComposeStatus) {
	bin := "no compose binary"
	if c.Binary != "" {
		bin = fmt.Sprintf("compose %s (%s, %s)", c.Version, c.Binary, c.Source)
	}
	engineV := "Engine unknown"
	if c.Engine != "" {
		engineV = "Engine " + c.Engine
	}
	if c.Rungs {
		fmt.Fprintf(w, "%s, %s; rungs 2 and 3 enabled; drift guard %s\n", bin, engineV, c.DriftGuard)
		return
	}
	fmt.Fprintf(w, "%s, %s; rungs 2 and 3 DISABLED (the ladder ends after rung 1): %s\n", bin, engineV, c.Reason)
}

// daemonLike is what the two listeners need from the daemon.
type daemonLike interface {
	api.Checker
	control.Backend
}

// handlers returns the handler for the TCP listener and the one for the unix
// socket. The TCP one is read-only (/healthz); only the socket can pause,
// resume or unsuspend, because it is reachable only by someone who can exec
// into the container (plan §2: no network-exposed write endpoints in v1).
func handlers(d daemonLike, o api.Options) (tcp, sock http.Handler) {
	o.Health = d
	tcp = api.Handler(o)
	sock = control.Mux(api.HealthHandler(d), d, func(v string) (time.Duration, error) { return daemon.ParseFor(v, config.ParseDuration) })
	return tcp, sock
}

// httpWarnings are the startup warnings about the HTTP listener: it is read-only, but it shows container names,
// states and event history, so a listener on every interface without a token deserves a word.
func httpWarnings(cfg config.Global) []string {
	var w []string
	host, _, err := net.SplitHostPort(cfg.HTTPAddr)
	loopback := err == nil && (host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()))
	if cfg.HTTPToken == "" && !loopback {
		w = append(w, "the dashboard listens on "+cfg.HTTPAddr+" without WHALENURSE_HTTP_TOKEN: anyone who can reach the port can read container names, states and the event history. Set a token, or publish the port on 127.0.0.1 or behind a reverse proxy or Tailscale (never network_mode)")
	}
	if cfg.HTTPToken != "" && len(cfg.HTTPAllowedHosts) > 0 {
		w = append(w, "WHALENURSE_HTTP_ALLOWED_HOSTS is ignored while WHALENURSE_HTTP_TOKEN is set")
	}
	if cfg.HTTPToken == "" && len(cfg.HTTPTrustedProxies) > 0 {
		w = append(w, "WHALENURSE_HTTP_TRUSTED_PROXIES is ignored without WHALENURSE_HTTP_TOKEN (it only changes the wrong-token limit)")
	}
	if cfg.HTTPToken != "" && len(cfg.HTTPToken) < 16 {
		w = append(w, "WHALENURSE_HTTP_TOKEN is shorter than 16 characters; use a long random token")
	}
	return w
}

func pauseFor(fs *flag.FlagSet) *string {
	return fs.String("for", "", "how long to pause: 30m, 2h, 1d ... (default 2h; 0 = until resume)")
}

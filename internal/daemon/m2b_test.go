package daemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/clock"
	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/store"
)

// M2b: rung 2 through compose, the drift guard, the version gate and the dependents step, against a fake Docker and a fake
// compose. Nothing here runs a process.

type okInfo struct{}

func (okInfo) Name() string       { return "f" }
func (okInfo) Size() int64        { return 1 }
func (okInfo) Mode() os.FileMode  { return 0o755 }
func (okInfo) ModTime() time.Time { return time.Time{} }
func (okInfo) IsDir() bool        { return true }
func (okInfo) Sys() any           { return nil }

func okStat(string) (os.FileInfo, error) { return okInfo{}, nil }

// newCompose builds a harness with a compose fake wired in. The Docker fake reports server (zero: Engine 29.9.1).
func newCompose(t *testing.T, tweak func(*config.Global), cf *compose.Fake, server docker.ServerInfo, more ...func(*Options)) *cfH {
	t.Helper()
	if cf == nil {
		cf = compose.NewFake()
	}
	logbuf := &bytes.Buffer{}
	cfg := config.Defaults()
	cfg.HostID = host
	cfg.StartupSettle = 0
	cfg.RateLimit = 100
	if tweak != nil {
		tweak(&cfg)
	}
	h := &harness{t: t, fake: docker.NewFake(), clk: clock.NewFake(time.Time{}), rec: &engine.Recorder{}, ctx: t.Context(), cfg: cfg}
	h.act = h.fake
	if server.Version != "" {
		h.fake.SetServer(server)
	}
	h.start(func(o *Options) {
		o.Compose, o.ComposeActor, o.Stat = cf, cf, okStat
		if cfg.DryRun {
			o.Actor = docker.DryRun{}
			o.ComposeActor = compose.DryRun{}
		}
		o.Log = slog.New(slog.NewTextHandler(logbuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		for _, fn := range more {
			fn(o)
		}
	})
	return &cfH{harness: h, cf: cf, log: logbuf}
}

type cfH struct {
	*harness
	cf  *compose.Fake
	log *bytes.Buffer
	n   int
}

func imgLabel(service string) string {
	return "sha256:" + service + strings.Repeat("0", 64-len(service))
}

// svc is a running compose container with every label compose writes and the image the drift guard compares.
func (h *cfH) svc(id, project, service string, extra ...string) docker.Container {
	h.t.Helper()
	defaults := []string{
		compose.LabelWorkingDir, "/opt/stacks/" + project, compose.LabelConfigFiles, "/opt/stacks/" + project + "/compose.yaml",
		compose.LabelConfigHash, "h-" + service, compose.LabelImage, imgLabel(service), compose.LabelVersion, "5.6.0"}
	extra = append(defaults, extra...)
	c := h.running(id, project, service, extra...)
	h.set(id, func(c *docker.Container) { c.Image = service + ":1" })
	h.fake.SetImage(service+":1", imgLabel(service))
	c, _ = h.fake.Inspect(h.ctx, id)
	return c
}

// replace stands in for what compose does: the container of the service is removed and a new one, with the same labels and
// a new ID, runs in its place.
func (h *cfH) replace(project, service string, fn func(*docker.Container)) docker.Container {
	h.t.Helper()
	list, _ := h.fake.List(h.ctx)
	for _, s := range list {
		if s.Labels[docker.LabelProject] != project || s.Labels[docker.LabelService] != service {
			continue
		}
		old, _ := h.fake.Inspect(h.ctx, s.ID)
		h.fake.Remove(old.ID)
		h.n++
		nc := old
		nc.ID = hexID(fmt.Sprintf("e%d", h.n))
		nc.Status, nc.Running, nc.Health, nc.ExitCode, nc.Error = "running", true, "", 0, ""
		nc.StartedAt, nc.Created, nc.RestartCount = h.clk.Now(), h.clk.Now(), 0
		if fn != nil {
			fn(&nc)
		}
		h.fake.Put(nc)
		return nc
	}
	h.t.Fatalf("no container for %s/%s", project, service)
	return docker.Container{}
}

func (h *cfH) verbs() string {
	var out []string
	for _, c := range h.cf.Calls() {
		out = append(out, c.Verb+" "+c.Project.Service)
	}
	return strings.Join(out, ", ")
}

func (h *cfH) current(project, service string) docker.Container {
	h.t.Helper()
	list, _ := h.fake.List(h.ctx)
	for _, s := range list {
		if s.Labels[docker.LabelProject] == project && s.Labels[docker.LabelService] == service {
			c, _ := h.fake.Inspect(h.ctx, s.ID)
			return c
		}
	}
	h.t.Fatalf("no container for %s/%s", project, service)
	return docker.Container{}
}

func (h *cfH) events(typ engine.EventType) []engine.Event {
	var out []engine.Event
	for _, e := range h.rec.Events() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func key(project, service string) string { return host + "/" + project + "/" + service + "/1" }

// ---- rung 2 -----------------------------------------------------------------------------------------------------

func TestLadderRestartThenRecreateThenSuspended(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.cf.OnRecreate = func(p compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "unhealthy" })
		return nil
	}
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)

	h.wantCalls("restart " + web.ID) // rung 1 through the API
	if got := h.verbs(); got != "dry-run web, recreate web" {
		t.Fatalf("compose calls = %q; the drift guard runs before the one recreate", got)
	}
	if now := h.current("p", "web"); now.ID == web.ID {
		t.Fatal("the container should have been recreated (new ID)")
	}
	if h.count(engine.ActionStarted) != 2 || h.count(engine.Suspended) != 1 || h.count(engine.RungUnavailable) != 0 {
		t.Fatalf("restart, recreate, SUSPENDED: %v", h.rec.Types())
	}
	// One pass per incident: nothing more, however long it stays unhealthy.
	h.advance(30 * time.Minute)
	if h.count(engine.ActionStarted) != 2 || len(h.cf.Calls()) != 2 {
		t.Fatalf("SUSPENDED means no more attempts: %v / %s", h.rec.Types(), h.verbs())
	}
}

func TestRecreateThatHelpsRecoversTheIncident(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.cf.OnRecreate = func(p compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(15 * time.Minute)
	if h.count(engine.Suspended) != 0 || h.count(engine.Recovered) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
	h.wantCalls("restart " + web.ID)
	if h.cf.Count("recreate") != 1 {
		t.Fatalf("%s", h.verbs())
	}
}

func TestDriftGuardBlocksARecreate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *cfH, web docker.Container)
		want  string
	}{
		{"compose file edited (the oracle says Recreate)", func(h *cfH, web docker.Container) {
			h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil }
		}, "config:"},
		{"tag moved (the oracle says Recreate and the image changed)", func(h *cfH, web docker.Container) {
			h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil }
			h.fake.SetImage("web:1", "sha256:other")
		}, "image: image web:1 now resolves to"},
		{"compose file edited, oracle unusable: the hash fallback", func(h *cfH, web docker.Container) {
			h.cf.OracleFn = func(compose.Project, string) (compose.Oracle, error) { return compose.Oracle{}, compose.ErrUnparseable }
			h.cf.HashFn = func(compose.Project) (string, error) { return "h-edited", nil }
		}, "config: config --hash is h-edited"},
		{"tag moved, oracle unusable: the explicit comparison", func(h *cfH, web docker.Container) {
			h.cf.OracleFn = func(compose.Project, string) (compose.Oracle, error) { return compose.Oracle{}, compose.ErrUnparseable }
			h.cf.HashFn = func(compose.Project) (string, error) { return "h-web", nil }
			h.fake.SetImage("web:1", "sha256:other")
		}, "image:"},
		{"image gone from the local store", func(h *cfH, web docker.Container) {
			h.cf.OracleFn = func(compose.Project, string) (compose.Oracle, error) { return compose.Oracle{}, compose.ErrUnparseable }
			h.cf.HashFn = func(compose.Project) (string, error) { return "h-web", nil }
			h.fake.SetImage("web:1", "")
		}, "unverified: image web:1 cannot be resolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompose(t, nil, nil, docker.ServerInfo{})
			web := h.svc(hexID("a1"), "p", "web")
			tc.setup(h, web)
			h.tick()
			h.set(web.ID, unhealthy)
			h.advance(6 * time.Minute)

			if h.cf.Count("recreate") != 0 || h.cf.Count("up") != 0 {
				t.Fatalf("a blocked recreate must not run: %s", h.verbs())
			}
			h.wantCalls("restart " + web.ID)
			blocked := h.events(engine.DriftBlocked)
			if len(blocked) != 1 || !strings.Contains(blocked[0].Message, tc.want) {
				t.Fatalf("want one DriftBlocked containing %q: %+v", tc.want, blocked)
			}
			if h.count(engine.Suspended) != 1 || h.count(engine.ActionStarted) != 1 {
				t.Fatalf("restart, then blocked, then SUSPENDED: %v", h.rec.Types())
			}
			for _, e := range h.events(engine.Suspended) {
				if e.Attrs["reason"] != engine.ReasonDrift {
					t.Fatalf("%+v", e)
				}
			}
			h.advance(10 * time.Minute)
			if h.cf.Count("recreate") != 0 || h.count(engine.Suspended) != 1 {
				t.Fatalf("SUSPENDED stays: %v / %s", h.rec.Types(), h.verbs())
			}
		})
	}
}

func TestWarnModeRecreatesAnywayAndSaysSo(t *testing.T) {
	h := newCompose(t, func(c *config.Global) { c.DriftGuard = config.DriftWarn }, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil }
	h.cf.OnRecreate = func(compose.Project) error { h.replace("p", "web", nil); return nil }
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	if h.cf.Count("recreate") != 1 || h.count(engine.DriftWarned) != 1 || h.count(engine.DriftBlocked) != 0 {
		t.Fatalf("%v / %s", h.rec.Types(), h.verbs())
	}
}

func TestVersionSkewBlocksOnlyTheBundledCompose(t *testing.T) {
	bundled := compose.NewFake()
	bundled.Bin = compose.Binary{Path: compose.BundledPath, Source: compose.SourceBundled, Version: compose.Version{Major: 5, Minor: 6}}
	bundled.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil }
	h := newCompose(t, nil, bundled, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web", compose.LabelVersion, "5.2.0")
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	b := h.events(engine.DriftBlocked)
	if len(b) != 1 || !strings.Contains(b[0].Message, "can't verify drift (compose 5.2.0 on host vs 5.6.0 in whalenurse)") {
		t.Fatalf("%+v", b)
	}
}

func TestDryRunNeverRecreates(t *testing.T) {
	h := newCompose(t, func(c *config.Global) { c.DryRun = true }, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(10 * time.Minute)
	h.wantCalls() // not even a restart: the actor is the no-op
	for _, c := range h.cf.Calls() {
		if c.Verb == "recreate" || c.Verb == "up" {
			t.Fatalf("dry-run ran a mutating compose command: %s", h.verbs())
		}
	}
	// It ran the whole ladder in its head, and the guard (which only reads) said it would have gone ahead.
	if h.count(engine.ActionResult) != 2 || h.count(engine.Suspended) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
	for _, e := range h.events(engine.ActionResult) {
		if e.Attrs["dry_run"] != "true" {
			t.Fatalf("%+v", e)
		}
	}
}

// ---- the version gate ------------------------------------------------------------------------------------------

func TestVersionGate(t *testing.T) {
	old := func(v string) *compose.Fake {
		cf := compose.NewFake()
		cf.Bin.Version, _ = compose.ParseVersion(v)
		cf.Bin.Raw = v
		return cf
	}
	for _, tc := range []struct {
		name   string
		cf     *compose.Fake
		server docker.ServerInfo
		off    bool
		why    string
	}{
		{"at the minimum", old("5.2.0"), docker.ServerInfo{Version: "28.1.0", Arch: "amd64"}, false, ""},
		{"latest", old("5.6.0"), docker.ServerInfo{Version: "29.9.1", Arch: "amd64"}, false, ""},
		{"Engine 28.0.4 (the GitHub runner's)", old("5.6.0"), docker.ServerInfo{Version: "28.0.4", Arch: "amd64"}, true, "Docker Engine 28.0.4 is older than 28.1.0"},
		{"Engine 25", old("5.6.0"), docker.ServerInfo{Version: "25.0.5", Arch: "amd64"}, true, "older than 28.1.0"},
		{"compose 5.1.0", old("5.1.0"), docker.ServerInfo{Version: "29.9.1", Arch: "amd64"}, true, "Docker Compose 5.1.0"},
		{"compose 2.38.2 (the runner's)", old("2.38.2"), docker.ServerInfo{Version: "29.9.1", Arch: "amd64"}, true, "Docker Compose 2.38.2"},
		{"compose version unreadable", func() *compose.Fake {
			cf := compose.NewFake()
			cf.Bin.Version, cf.Bin.Raw = compose.Version{}, "???"
			return cf
		}(), docker.ServerInfo{Version: "29.9.1", Arch: "amd64"}, true, "cannot be read"},
		{"Engine version unreadable", old("5.6.0"), docker.ServerInfo{Version: "dev", Arch: "amd64"}, true, "cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompose(t, nil, tc.cf, tc.server)
			web := h.svc(hexID("a1"), "p", "web")
			h.d.CheckVersions(h.ctx) // again: still one warning
			st := h.d.Status().(Status).Compose
			if st.Rungs == tc.off {
				t.Fatalf("status: %+v", st)
			}
			warnings := strings.Count(h.log.String(), "rungs 2 and 3 (recreate, pull) and the drift guard are disabled")
			if tc.off {
				if warnings != 1 || h.count(engine.RungsDisabled) != 1 || !strings.Contains(st.Reason, tc.why) || len(st.Disabled) != 2 {
					t.Fatalf("one warning, one event, the reason in status: %d %d %+v\n%s", warnings, h.count(engine.RungsDisabled), st, h.log.String())
				}
			} else if warnings != 0 || h.count(engine.RungsDisabled) != 0 || len(st.Disabled) != 0 {
				t.Fatalf("%d %+v", warnings, st)
			}

			h.tick()
			h.set(web.ID, unhealthy)
			h.advance(6 * time.Minute)
			h.wantCalls("restart " + web.ID)
			if tc.off {
				// The ladder ends at notify and SUSPENDED after rung 1, and compose is not touched at all.
				if len(h.cf.Calls()) != 0 || h.count(engine.Suspended) != 1 || h.count(engine.ActionStarted) != 1 || h.count(engine.RungUnavailable) != 1 {
					t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
				}
				if msg := h.events(engine.RungUnavailable)[0].Message; !strings.Contains(msg, "disabled") {
					t.Fatalf("%s", msg)
				}
			} else if h.cf.Count("recreate") != 1 {
				t.Fatalf("%s", h.verbs())
			}
		})
	}
}

func TestNoComposeBinaryDisablesTheRungsAndSaysWhy(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	h.start(func(o *Options) {
		o.ComposeNote = "no docker compose binary found (/host/docker-compose (host): not there)"
	})
	st := h.d.Status().(Status).Compose
	if st.Rungs || !strings.Contains(st.Reason, "not there") || st.Binary != "" {
		t.Fatalf("%+v", st)
	}
}

// The gate is read again before the sweep that follows a reconnect: the Engine may have been upgraded while the stream
// was down (plan v3.6 item 5).
func TestTheGateIsCheckedAgainAfterAReconnect(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{Version: "28.0.4", Arch: "amd64"})
	h.tick()
	h.d.StreamConnected()
	h.fake.SetServer(docker.ServerInfo{Version: "29.9.1", Arch: "amd64"})
	h.d.StreamLost(h.clk.Now())
	h.d.StreamConnected()
	h.advance(time.Second)
	if st := h.d.Status().(Status).Compose; !st.Rungs || st.Engine != "29.9.1" {
		t.Fatalf("gate not checked again after the reconnect: %+v", st)
	}
	if n := strings.Count(h.log.String(), "compose rungs enabled"); n != 1 {
		t.Fatalf("%d \"compose rungs enabled\" lines, want 1:\n%s", n, h.log.String())
	}
	// Another reconnect with nothing changed says nothing; a failed read keeps what was known.
	h.fake.ServerErr = fmt.Errorf("daemon restoring")
	h.d.StreamLost(h.clk.Now())
	h.d.StreamConnected()
	h.advance(time.Second)
	if st := h.d.Status().(Status).Compose; !st.Rungs {
		t.Fatalf("an API error during a restore must not disable the rungs: %+v", st)
	}
	if n := h.rec.Count(engine.RungsDisabled); n != 1 {
		t.Fatalf("%d RungsDisabled events, want the one from startup", n)
	}
}

// A failed first read is not a verdict on the versions: it is tried again until it succeeds.
func TestAFailedFirstVersionReadIsTriedAgain(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{Version: "29.9.1", Arch: "amd64"}, func(o *Options) {
		o.Reader.(*docker.Fake).ServerErr = fmt.Errorf("daemon restoring")
	})
	if st := h.d.Status().(Status).Compose; st.Rungs || !strings.Contains(st.Reason, "cannot read the Docker Engine version") {
		t.Fatalf("%+v", st)
	}
	h.tick() // the settle sweep: still failing
	h.fake.ServerErr = nil
	h.advance(h.cfg.PollInterval + time.Second)
	if st := h.d.Status().(Status).Compose; !st.Rungs || st.Engine != "29.9.1" {
		t.Fatalf("a failed first read was never tried again: %+v", st)
	}
}

func TestUnusableComposeInputsSkipRung2WithTheMountHint(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.start(func(o *Options) {
		o.Compose, o.ComposeActor = h.cf, h.cf
		o.Stat = func(path string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	})
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	ru := h.events(engine.RungUnavailable)
	if len(ru) != 1 || !strings.Contains(ru[0].Message, "identical host path") || !strings.Contains(ru[0].Message, "/opt/stacks/p") {
		t.Fatalf("%+v", ru)
	}
	if len(h.cf.Calls()) != 0 || h.count(engine.Suspended) != 1 {
		t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
	}
}

// WN-001: a service label shaped like a flag (an image can ship one) makes rungs 2 and 3 unavailable, live and in a dry
// run alike, and compose is never run for it: not even the drift guard's oracle, which it would turn into a real up.
func TestFlagShapedServiceLabelNeverReachesCompose(t *testing.T) {
	for _, dry := range []bool{true, false} {
		t.Run(fmt.Sprintf("dry-run=%v", dry), func(t *testing.T) {
			h := newCompose(t, func(c *config.Global) { c.DryRun = dry }, nil, docker.ServerInfo{})
			web := h.svc(hexID("a1"), "p", "web", compose.LabelService, "--dry-run=false", config.LabelHealPull, "true")
			h.tick()
			h.set(web.ID, unhealthy)
			h.advance(10 * time.Minute)
			if calls := h.cf.Calls(); len(calls) != 0 {
				t.Fatalf("compose ran: %s", h.verbs())
			}
			rungs := map[string]bool{}
			for _, e := range h.events(engine.RungUnavailable) {
				if !strings.Contains(e.Message, compose.LabelService) {
					t.Errorf("the refusal should name the label: %q", e.Message)
				}
				rungs[e.Attrs["rung"]] = true
			}
			if !rungs[config.RungRecreate] || !rungs[config.RungPull] || h.count(engine.Suspended) != 1 {
				t.Fatalf("rungs 2 and 3 should be unavailable, then SUSPENDED: %v %v", rungs, h.rec.Types())
			}
		})
	}
}

func TestNonComposeContainerGetsRung1Only(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	c := docker.Container{ID: hexID("a1"), Name: "plain", Labels: map[string]string{config.LabelEnable: "true"}, Status: "running", Running: true}
	h.fake.Put(c)
	h.tick()
	h.set(c.ID, unhealthy)
	h.advance(6 * time.Minute)
	h.wantCalls("restart " + c.ID)
	if ru := h.events(engine.RungUnavailable); len(ru) != 1 || !strings.Contains(ru[0].Message, "not a compose container") || len(h.cf.Calls()) != 0 {
		t.Fatalf("%+v %s", ru, h.verbs())
	}
}

// A volumes_from holder is a reference holder for the drift guard too: neither the oracle nor the hash is used for it.
func TestVolumesFromHolderSkipsCheck1(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.set(web.ID, func(c *docker.Container) { c.VolumesFrom = []string{"data-1"} })
	h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil } // would block, if asked
	h.cf.OnRecreate = func(compose.Project) error { h.replace("p", "web", nil); return nil }
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	if h.cf.Count("dry-run") != 0 || h.cf.Count("hash") != 0 || h.cf.Count("recreate") != 1 {
		t.Fatalf("%s", h.verbs())
	}
}

// ---- dependents (plan §3.5) ------------------------------------------------------------------------------------

// pair builds a parent and a network_mode dependent in project p.
func (h *cfH) pair() (parent, dep docker.Container) {
	parent = h.svc(hexID("c1"), "p", "parent")
	dep = h.svc(hexID("d1"), "p", "dep", compose.LabelDependsOn, "parent:service_started:true")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID })
	dep, _ = h.fake.Inspect(h.ctx, dep.ID)
	return parent, dep
}

// The parent is recreated by rung 2: its dependent now points at an ID that is gone. It is recreated by compose with the
// non-forced up, and never restarted (E1, E2).
func TestDependentIsRecreatedAfterItsParentIsRecreated(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent, dep := h.pair()
	var newParent docker.Container
	h.cf.OnRecreate = func(p compose.Project) error {
		newParent = h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	h.cf.OnUp = func(p compose.Project) error {
		h.replace("p", "dep", func(c *docker.Container) { c.NetworkMode = "container:" + newParent.ID })
		return nil
	}
	h.tick()
	h.set(parent.ID, unhealthy)
	h.advance(8 * time.Minute)

	h.wantCalls("restart " + parent.ID) // rung 1 on the parent; the dependent is never API-restarted
	if got := h.verbs(); got != "dry-run parent, recreate parent, up dep" {
		t.Fatalf("compose calls = %q (a dependent skips the oracle and the hash; its recreate is the non-forced up)", got)
	}
	now := h.current("p", "dep")
	if now.ID == dep.ID || now.NetworkMode != "container:"+newParent.ID || !now.Running {
		t.Fatalf("the dependent should be recreated against the new parent: %+v", now)
	}
	if h.count(engine.Suspended) != 0 || h.count(engine.DependentPostCheck) != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
	for _, c := range h.fake.Calls() {
		if c.ID == dep.ID {
			t.Fatalf("API call on a dependent whose parent was gone: %+v", c)
		}
	}
}

// If compose decides nothing changed and leaves the dependent stale, the post-check recreates it with --force-recreate.
func TestStaleDependentIsForceRecreatedWhenTheUpDidNothing(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent, dep := h.pair()
	var newParent docker.Container
	h.cf.OnRecreate = func(p compose.Project) error {
		if p.Service == "parent" {
			newParent = h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
		} else {
			h.replace("p", "dep", func(c *docker.Container) { c.NetworkMode = "container:" + newParent.ID })
		}
		return nil
	}
	h.cf.OnUp = func(compose.Project) error { return nil } // compose #13878's mirror image: it leaves the dependent alone
	h.tick()
	h.set(parent.ID, unhealthy)
	h.advance(8 * time.Minute)
	if got := h.verbs(); got != "dry-run parent, recreate parent, up dep, recreate dep" {
		t.Fatalf("%q", got)
	}
	if pc := h.events(engine.DependentPostCheck); len(pc) != 1 || !strings.Contains(pc[0].Message, "--force-recreate") {
		t.Fatalf("%v", h.rec.Types())
	}
	if now := h.current("p", "dep"); now.ID == dep.ID || now.NetworkMode != "container:"+newParent.ID {
		t.Fatalf("%+v", now)
	}
}

// A recreate by someone else (compose up by hand): the create/start events of the parent trigger the staleness sweep.
func TestExternalParentRecreateRecreatesTheDependent(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	_, dep := h.pair()
	var newParent docker.Container
	h.cf.OnUp = func(compose.Project) error {
		h.replace("p", "dep", func(c *docker.Container) { c.NetworkMode = "container:" + newParent.ID })
		return nil
	}
	h.tick()
	newParent = h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
	h.event(newParent, "create", "")
	h.event(newParent, "start", "")
	h.advance(3 * time.Minute)
	if got := h.verbs(); got != "up dep" {
		t.Fatalf("%q", got)
	}
	h.wantCalls()
	if h.current("p", "dep").ID == dep.ID {
		t.Fatal("not recreated")
	}
}

// The incident the dependents step opens for a dependent that is not opted in must close once the dependent has recovered.
// Found on a real host: it stayed open ("rung 2 done"), so the next time the parent was recreated the dependent went
// straight to SUSPENDED and was never repaired.
func TestDependentIncidentClosesAndTheNextParentRecreateRepairsItAgain(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	_, dep := h.pair()
	h.set(dep.ID, func(c *docker.Container) { delete(c.Labels, config.LabelEnable) }) // a sidecar with no opt-in of its own
	var newParent docker.Container
	h.cf.OnUp = func(compose.Project) error {
		h.replace("p", "dep", func(c *docker.Container) { c.NetworkMode = "container:" + newParent.ID })
		return nil
	}
	h.tick()
	for round := 1; round <= 2; round++ {
		before := h.current("p", "dep").ID
		newParent = h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
		h.event(newParent, "create", "")
		h.event(newParent, "start", "")
		h.advance(3 * time.Minute)
		if got := h.current("p", "dep"); got.ID == before || got.NetworkMode != "container:"+newParent.ID {
			t.Fatalf("round %d: the dependent was not recreated against the new parent: %+v", round, got)
		}
		h.advance(12 * time.Minute) // RECOVERY_PERIOD is 10 minutes
		if open, _ := h.d.Engine().IncidentOpen(key("p", "dep")); open {
			t.Fatalf("round %d: the dependent's incident is still open after it recovered", round)
		}
	}
	if h.count(engine.Suspended) != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
	_ = dep
}

// restartWithStore stands for a restart of whalenurse on the same store (and the compose fake of the test).
func (h *cfH) restartWithStore(st *store.Store) {
	h.t.Helper()
	with := h.withStore(st, "boot-A")
	h.start(func(o *Options) {
		o.Compose, o.ComposeActor, o.Stat = h.cf, h.cf, okStat
		with(o)
	})
}

// Compose writes an empty depends_on label on a service recreated with `up --no-deps` (create.go builds it from the services
// in scope), which is how whalenurse recreates a dependent. The parent of a dependent that is not opted in is then found
// only through what whalenurse learned, so the second parent recreate (here, after a restart of whalenurse) must still
// repair it. Found on a real host: before, the second repair never happened and nothing said so.
func TestDependentIsRepairedAgainAfterComposeEmptiedItsDependsOnLabel(t *testing.T) {
	st := openStore(t)
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	h.restartWithStore(st)
	_, dep := h.pair()
	h.set(dep.ID, func(c *docker.Container) { delete(c.Labels, config.LabelEnable) }) // a sidecar with no opt-in of its own
	var newParent docker.Container
	h.cf.OnUp = func(compose.Project) error {
		h.replace("p", "dep", func(c *docker.Container) {
			c.NetworkMode = "container:" + newParent.ID
			c.Labels = maps.Clone(c.Labels)
			delete(c.Labels, compose.LabelDependsOn) // what `up --no-deps` leaves
		})
		return nil
	}
	h.tick()
	h.advance(time.Minute) // the dependent is looked at, and its parent is learned from the label
	for round := 1; round <= 2; round++ {
		if round == 2 {
			h.restartWithStore(st)
			h.tick()
		}
		before := h.current("p", "dep").ID
		newParent = h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
		h.event(newParent, "create", "")
		h.event(newParent, "start", "")
		h.advance(3 * time.Minute)
		got := h.current("p", "dep")
		if got.ID == before || got.NetworkMode != "container:"+newParent.ID {
			t.Fatalf("round %d: the dependent was not recreated against the new parent: %+v (verbs %s)", round, got, h.verbs())
		}
		if round == 1 && got.Labels[compose.LabelDependsOn] != "" {
			t.Fatal("the fake should have emptied the label")
		}
		h.advance(12 * time.Minute)
	}
	if h.count(engine.Suspended) != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
}

// What is learned about a dependent's parents: the label replaces, a live container: reference adds and never shrinks.
func TestLearnedParentsFollowTheLabelAndOnlyGrowFromLiveReferences(t *testing.T) {
	st := openStore(t)
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	h.restartWithStore(st)
	parent := h.svc(hexID("c1"), "p", "parent")
	other := h.svc(hexID("c2"), "p", "other")
	dep := h.svc(hexID("d1"), "p", "dep", compose.LabelDependsOn, "other:service_started:true")
	h.set(dep.ID, func(c *docker.Container) {
		c.NetworkMode = "container:" + parent.ID
		delete(c.Labels, config.LabelEnable)
	})
	_ = other
	k := key("p", "dep")
	h.tick()
	h.advance(time.Minute)
	if got := strings.Join(h.d.knownParents(k), ","); got != "other" {
		t.Fatalf("from the label: %q", got)
	}
	h.set(dep.ID, func(c *docker.Container) { c.Labels = maps.Clone(c.Labels); delete(c.Labels, compose.LabelDependsOn) })
	h.advance(time.Minute)
	// an empty label forgets nothing, and the live container: reference adds the network parent
	if got := strings.Join(h.d.knownParents(k), ","); got != "other,parent" {
		t.Fatalf("an empty label must not shrink what was learned, and a live reference adds its service: %q", got)
	}
	if v, _ := st.Meta(metaParents + k); v != "other,parent" {
		t.Fatalf("not persisted: %q", v)
	}
	h.set(dep.ID, func(c *docker.Container) {
		c.Labels = maps.Clone(c.Labels)
		c.Labels[compose.LabelDependsOn] = "parent:service_started:true"
	})
	h.advance(time.Minute)
	if got := strings.Join(h.d.knownParents(k), ","); got != "parent" {
		t.Fatalf("a non-empty label is authoritative: %q", got)
	}
}

// The parent is still being healed: the stale dependent waits (its recreate would fail to join a parent that is not
// running), and goes ahead when the parent runs.
func TestStaleDependentWaitsForItsParent(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	_, dep := h.pair()
	newParent := h.replace("p", "parent", func(c *docker.Container) { c.Status, c.Running = "created", false })
	var up int
	h.cf.OnUp = func(compose.Project) error {
		up++
		h.replace("p", "dep", func(c *docker.Container) { c.NetworkMode = "container:" + newParent.ID })
		return nil
	}
	h.tick()
	h.advance(3 * time.Minute)
	if up != 0 || h.cf.Count("up") != 0 {
		t.Fatalf("recreated against a parent that is not running: %s", h.verbs())
	}
	h.set(newParent.ID, func(c *docker.Container) { c.Status, c.Running = "running", true; c.StartedAt = h.clk.Now() })
	h.advance(3 * time.Minute)
	if h.cf.Count("up") != 1 || h.current("p", "dep").ID == dep.ID {
		t.Fatalf("%s", h.verbs())
	}
}

func TestStaleDependentIsHeldBackWhileTheParentHasAnIncident(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent, _ := h.pair()
	h.cf.OnRecreate = func(p compose.Project) error {
		h.replace("p", "parent", func(c *docker.Container) { c.Health = "unhealthy" })
		return nil
	}
	h.tick()
	h.set(parent.ID, unhealthy)
	h.advance(61*time.Second + 35*time.Second) // rung 1 has run, the parent's incident is open
	if h.cf.Count("up") != 0 {
		t.Fatalf("the dependent was recreated while its parent was being healed: %s", h.verbs())
	}
}

// A parent restarted through the API: its dependents keep the old namespace, so they are restarted too, one at a time,
// each given time to come back before the next.
func TestDependentsAreRestartedOneAtATimeAfterTheParentIsRestarted(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent := h.svc(hexID("c1"), "p", "parent")
	d1 := h.svc(hexID("d1"), "p", "dep1")
	d2 := h.svc(hexID("d2"), "p", "dep2")
	for _, id := range []string{d1.ID, d2.ID} {
		h.set(id, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID; c.Health = "healthy" })
	}
	h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
		f.Update(id, func(c *docker.Container) {
			c.StartedAt = h.clk.Now()
			if id == parent.ID {
				c.Health = "healthy"
			}
			if id == d1.ID {
				c.Health = "starting" // d1 takes a while to be healthy
			}
		})
	}
	h.tick()
	h.set(parent.ID, unhealthy)
	h.advance(95 * time.Second)
	h.advance(40 * time.Second) // the parent's cooldown, then its dependents

	h.wantCalls("restart "+parent.ID, "restart "+d1.ID) // d2 waits for d1
	h.advance(20 * time.Second)
	h.wantCalls("restart "+parent.ID, "restart "+d1.ID)
	h.set(d1.ID, healthy)
	h.advance(10 * time.Second)
	h.wantCalls("restart "+parent.ID, "restart "+d1.ID, "restart "+d2.ID)
	if len(h.cf.Calls()) != 0 && h.cf.Count("recreate")+h.cf.Count("up") != 0 {
		t.Fatalf("a restart of the parent is repaired by restarts: %s", h.verbs())
	}
}

// The reverse case: a dependent that fails by itself never triggers an action on its parent (M2a keeps this).
func TestFailingDependentLeavesItsParentAlone(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent, dep := h.pair()
	h.tick()
	h.set(dep.ID, unhealthy)
	h.advance(10 * time.Minute)
	if n := len(h.fake.CallsOn(parent.ID)); n != 0 {
		t.Fatalf("%d calls on the parent", n)
	}
	for _, c := range h.cf.Calls() {
		if c.Project.Service == "parent" && (c.Verb == "recreate" || c.Verb == "up") {
			t.Fatalf("%s", h.verbs())
		}
	}
}

// A stale dependent is held to checks 2 and 3 of the drift guard: a re-tagged image blocks its recreate.
func TestStaleDependentWithAMovedTagIsBlocked(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	_, dep := h.pair()
	h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
	h.fake.SetImage("dep:1", "sha256:moved")
	h.tick()
	h.advance(3 * time.Minute)
	blocked := h.events(engine.DriftBlocked)
	if len(blocked) != 1 || blocked[0].Key != key("p", "dep") || !strings.Contains(blocked[0].Message, "the tag moved") {
		t.Fatalf("%+v / %v", blocked, h.rec.Types())
	}
	if h.cf.Count("up") != 0 || h.cf.Count("dry-run") != 0 || h.current("p", "dep").ID != dep.ID {
		t.Fatalf("%s", h.verbs())
	}
}

// With the compose rungs disabled a stale dependent stays what M2a made of it: advice, never a restart.
func TestStaleDependentWithTheGateOffIsOnlyAdvice(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{Version: "28.0.4", Arch: "amd64"})
	_, dep := h.pair()
	h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
	h.tick()
	h.advance(10 * time.Minute)
	if h.count(engine.NeedsRecreate) != 1 || h.count(engine.IncidentOpened) != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
	if m := h.events(engine.NeedsRecreate)[0].Message; !strings.Contains(m, "disabled") {
		t.Fatal(m)
	}
	h.wantCalls()
	if len(h.cf.Calls()) != 0 || h.current("p", "dep").ID != dep.ID {
		t.Fatalf("%s", h.verbs())
	}
}

// A `restart: "no"` parent stopped by a host shutdown stays down, and so do its dependents: one notification names them.
func TestParentLeftDownByPolicyGetsOneNotificationNamingItsDependents(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { settled(c); c.PollInterval = time.Minute }, nil)
	h.d.StreamConnected()
	h.advance(2*time.Minute + time.Second)
	parent := h.running(hexID("27"), "p", "parent")
	dep := h.running(hexID("d1"), "p", "dep")
	dep2 := h.running(hexID("d2"), "p", "dep2")
	for _, id := range []string{dep.ID, dep2.ID} {
		h.set(id, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID })
	}
	h.tick()
	h.clk.Advance(5 * time.Second)
	fin := h.clk.Now()
	h.set(parent.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 1, "no", fin
	})
	for _, id := range []string{dep.ID, dep2.ID} {
		h.set(id, func(c *docker.Container) {
			c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 137, "unless-stopped", fin
			c.Error = "cannot join network namespace of a non running container: container p-parent-1 is exited"
		})
	}
	h.d.StreamLost(h.clk.Now())
	h.clk.Advance(20 * time.Second)
	h.d.StreamConnected()
	h.advance(15 * time.Minute) // many polls

	h.wantCalls() // nothing is started
	var notes []engine.Event
	for _, e := range h.rec.Events() {
		if e.Type == engine.NotifyOnly {
			notes = append(notes, e)
		}
	}
	if len(notes) != 1 || notes[0].Key != key("p", "parent") {
		t.Fatalf("want exactly one notification, about the parent: %+v", notes)
	}
	if got := notes[0].Attrs["dependents"]; got != "p-dep-1,p-dep2-1" {
		t.Fatalf("dependents = %q", got)
	}
	if !strings.Contains(notes[0].Message, "p-dep-1, p-dep2-1") {
		t.Fatalf("the message should name them: %s", notes[0].Message)
	}
	if h.count(engine.IncidentOpened) != 0 {
		t.Fatalf("no ladder for any of them: %v", h.rec.Types())
	}
}

// heal-exited=always starts the parent, and then its dependents (the heal-the-parent-first path of rule 1).
func TestHealExitedAlwaysStartsTheParentThenItsDependents(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { settled(c); c.PollInterval = time.Minute }, nil)
	h.d.StreamConnected()
	h.advance(2*time.Minute + time.Second)
	parent := h.running(hexID("27"), "p", "parent", config.LabelHealExited, "always")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID })
	h.tick()
	h.clk.Advance(5 * time.Second)
	fin := h.clk.Now()
	h.set(parent.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 1, "no", fin
	})
	h.set(dep.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 137, "unless-stopped", fin
		c.Error = "cannot join network namespace of a non running container: container p-parent-1 is exited"
	})
	h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
		f.Update(id, func(c *docker.Container) {
			c.Status, c.Running, c.StartedAt, c.Error = "running", true, h.clk.Now(), ""
		})
	}
	h.d.StreamLost(h.clk.Now())
	h.clk.Advance(20 * time.Second)
	h.d.StreamConnected()
	h.advance(10 * time.Minute)
	h.wantCalls("start "+parent.ID, "start "+dep.ID)
}

// ---- plan v3.6 decisions ------------------------------------------------------------------------------------------

// noOptIn removes the opt-in label of a container.
func (h *cfH) noOptIn(id string) docker.Container {
	return h.set(id, func(c *docker.Container) { delete(c.Labels, config.LabelEnable) })
}

// The dependents step repairs the dependents of an opted-in parent even when they are not opted in themselves.
func TestSidecarThatIsNotOptedInFollowsItsOptedInParent(t *testing.T) {
	t.Run("parent recreated", func(t *testing.T) {
		h := newCompose(t, nil, nil, docker.ServerInfo{})
		_, dep := h.pair()
		h.noOptIn(dep.ID)
		var newParent docker.Container
		h.cf.OnUp = func(compose.Project) error {
			h.replace("p", "dep", func(c *docker.Container) { c.NetworkMode = "container:" + newParent.ID })
			return nil
		}
		h.tick()
		newParent = h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
		h.advance(3 * time.Minute)
		if got := h.verbs(); got != "up dep" {
			t.Fatalf("%q", got)
		}
		h.wantCalls()
	})
	t.Run("parent restarted", func(t *testing.T) {
		h := newCompose(t, nil, nil, docker.ServerInfo{})
		parent, dep := h.pair()
		h.noOptIn(dep.ID)
		h.set(parent.ID, func(c *docker.Container) { c.StartedAt = h.clk.Now().Add(-time.Minute) })
		h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
			f.Update(id, func(c *docker.Container) { c.StartedAt = h.clk.Now() })
		}
		h.tick()
		h.advance(3 * time.Minute)
		h.wantCalls("restart " + dep.ID)
	})
	t.Run("unhealthy only because the parent restarted", func(t *testing.T) {
		h := newCompose(t, nil, nil, docker.ServerInfo{})
		parent, dep := h.pair()
		h.noOptIn(dep.ID)
		h.set(parent.ID, func(c *docker.Container) { c.StartedAt = h.clk.Now().Add(-time.Minute) })
		h.set(dep.ID, unhealthy)
		h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
			f.Update(id, func(c *docker.Container) { c.StartedAt, c.Health = h.clk.Now(), "healthy" })
		}
		h.tick()
		h.advance(3 * time.Minute)
		h.wantCalls("restart " + dep.ID)
	})
}

func TestSidecarLabelledEnableFalseIsNeverTouched(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent, dep := h.pair()
	h.set(dep.ID, func(c *docker.Container) { c.Labels[config.LabelEnable] = "false" })
	h.set(parent.ID, func(c *docker.Container) { c.StartedAt = h.clk.Now().Add(-time.Minute) })
	h.tick()
	h.advance(3 * time.Minute)
	h.replace("p", "parent", func(c *docker.Container) { c.Health = "healthy" })
	h.advance(3 * time.Minute)
	h.wantCalls()
	if len(h.cf.Calls()) != 0 || h.count(engine.IncidentOpened) != 0 {
		t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
	}
}

// Its own failures need its own opt-in: the dependents step does not turn into a ladder for it.
func TestSidecarThatIsNotOptedInIsNotHealedForItsOwnFailures(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	_, dep := h.pair()
	h.noOptIn(dep.ID)
	h.tick()
	h.set(dep.ID, unhealthy)
	h.advance(10 * time.Minute)
	h.wantCalls()
	if len(h.cf.Calls()) != 0 || h.count(engine.IncidentOpened) != 0 {
		t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
	}
	h.set(dep.ID, func(c *docker.Container) { c.Health = ""; c.Status, c.Running, c.ExitCode = "exited", false, 1 })
	h.advance(10 * time.Minute)
	h.wantCalls()
}

func TestSidecarOfAParentThatIsNotOptedInIsNotTouched(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent, dep := h.pair()
	h.noOptIn(dep.ID)
	h.noOptIn(parent.ID)
	h.set(parent.ID, func(c *docker.Container) { c.StartedAt = h.clk.Now().Add(-time.Minute) })
	h.tick()
	h.advance(5 * time.Minute)
	h.wantCalls()
}

// Repairing a sidecar after each of its parent's restarts is not flapping.
func TestDependentRepairsDoNotFlapTheSidecar(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	parent, dep := h.pair()
	h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
		f.Update(id, func(c *docker.Container) { c.StartedAt = h.clk.Now() })
	}
	h.tick()
	for i := 0; i < 6; i++ { // FLAP_LIMIT is 3
		h.set(parent.ID, func(c *docker.Container) { c.StartedAt = h.clk.Now() }) // the parent restarted (by anything)
		h.advance(20 * time.Minute)
	}
	if got := len(h.fake.CallsOn(dep.ID)); got != 6 {
		t.Fatalf("the sidecar should be restarted after each of the 6 parent restarts, got %d", got)
	}
	if h.count(engine.Suspended) != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
}

// warn overrides verified drift only; "can't verify" stays blocking.
func TestWarnModeDoesNotOverrideWhatCouldNotBeVerified(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *cfH)
		want  string
	}{
		{"no check could be made", func(h *cfH) {
			h.cf.OracleFn = func(compose.Project, string) (compose.Oracle, error) { return compose.Oracle{}, compose.ErrUnparseable }
			h.cf.HashFn = func(compose.Project) (string, error) { return "", fmt.Errorf("boom") }
		}, "unverified:"},
		{"the image cannot be resolved", func(h *cfH) {
			h.cf.OracleFn = func(compose.Project, string) (compose.Oracle, error) { return compose.Oracle{}, compose.ErrUnparseable }
			h.cf.HashFn = func(compose.Project) (string, error) { return "h-web", nil }
			h.fake.SetImage("web:1", "")
		}, "unverified:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompose(t, func(c *config.Global) { c.DriftGuard = config.DriftWarn }, nil, docker.ServerInfo{})
			web := h.svc(hexID("a1"), "p", "web")
			tc.setup(h)
			h.tick()
			h.set(web.ID, unhealthy)
			h.advance(6 * time.Minute)
			b := h.events(engine.DriftBlocked)
			if len(b) != 1 || !strings.Contains(b[0].Message, tc.want) || !strings.Contains(b[0].Message, "does not override") {
				t.Fatalf("%+v", b)
			}
			if h.cf.Count("recreate") != 0 || h.count(engine.DriftWarned) != 0 {
				t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
			}
		})
	}
	// Version skew of the bundled compose is "can't verify" too.
	bundled := compose.NewFake()
	bundled.Bin = compose.Binary{Path: compose.BundledPath, Source: compose.SourceBundled, Version: compose.Version{Major: 5, Minor: 6}}
	bundled.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil }
	h := newCompose(t, func(c *config.Global) { c.DriftGuard = config.DriftWarn }, bundled, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web", compose.LabelVersion, "5.2.0")
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	if len(h.events(engine.DriftBlocked)) != 1 || bundled.Count("recreate") != 0 {
		t.Fatalf("skew must block under warn: %v", h.rec.Types())
	}
}

// Verified drift mixed with a check that could not be made still blocks under warn.
func TestWarnModeBlocksWhenAnyFindingIsUnverified(t *testing.T) {
	h := newCompose(t, func(c *config.Global) { c.DriftGuard = config.DriftWarn }, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.cf.OracleFn = func(compose.Project, string) (compose.Oracle, error) { return compose.Oracle{}, compose.ErrUnparseable }
	h.cf.HashFn = func(compose.Project) (string, error) { return "h-edited", nil } // verified config drift
	h.fake.SetImage("web:1", "")                                                   // and the image cannot be resolved
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	if len(h.events(engine.DriftBlocked)) != 1 || h.cf.Count("recreate") != 0 {
		t.Fatalf("%v / %s", h.rec.Types(), h.verbs())
	}
}

// An upgraded host plugin is information in the drift message.
func TestUpgradedHostPluginIsNotedInTheDriftMessage(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{}) // host plugin 5.6.0
	web := h.svc(hexID("a1"), "p", "web", compose.LabelVersion, "5.2.0")
	h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) { return compose.Recreated(name), nil }
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(6 * time.Minute)
	b := h.events(engine.DriftBlocked)
	if len(b) != 1 || !strings.Contains(b[0].Message, "config: compose would recreate") ||
		!strings.Contains(b[0].Message, "compose upgraded from 5.2.0 to 5.6.0 since this container was created") {
		t.Fatalf("%+v", b)
	}
	// ... and it is still verified drift: warn recreates.
	h2 := newCompose(t, func(c *config.Global) { c.DriftGuard = config.DriftWarn }, nil, docker.ServerInfo{})
	web2 := h2.svc(hexID("a1"), "p", "web", compose.LabelVersion, "5.2.0")
	h2.cf.OracleFn = h.cf.OracleFn
	h2.cf.OnRecreate = func(compose.Project) error { h2.replace("p", "web", nil); return nil }
	h2.tick()
	h2.set(web2.ID, unhealthy)
	h2.advance(6 * time.Minute)
	if h2.cf.Count("recreate") != 1 || h2.count(engine.DriftWarned) != 1 {
		t.Fatalf("%v", h2.rec.Types())
	}
}

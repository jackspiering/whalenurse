package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/api"
	"github.com/jackspiering/whalenurse/internal/clock"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/rules"
)

const host = "h"

type harness struct {
	t    *testing.T
	fake *docker.Fake
	clk  *clock.Fake
	rec  *engine.Recorder
	d    *Daemon
	ctx  context.Context
	cfg  config.Global
	act  docker.Actor
	// planted lists attribute keys a test puts on events on purpose (the dashboard's secrets test); every other
	// attribute the daemon or the engine emits must be one the API lets through, or the test fails.
	planted map[string]bool
}

// newHarness builds a daemon on a fake clock and a fake Docker. Startup settle
// is zero, so the first Tick runs the startup sweep.
func newHarness(t *testing.T, tweak func(*config.Global), actor func(*docker.Fake) docker.Actor) *harness {
	t.Helper()
	return newHarnessOpts(t, tweak, actor, nil)
}

// newHarnessOpts is newHarness with a hook on the daemon Options (a store, a boot id).
func newHarnessOpts(t *testing.T, tweak func(*config.Global), actor func(*docker.Fake) docker.Actor, opts func(*Options)) *harness {
	t.Helper()
	cfg := config.Defaults()
	cfg.HostID = host
	cfg.StartupSettle = 0
	cfg.RateLimit = 100
	if tweak != nil {
		tweak(&cfg)
	}
	h := &harness{t: t, fake: docker.NewFake(), clk: clock.NewFake(time.Time{}), rec: &engine.Recorder{}, ctx: context.Background(), cfg: cfg}
	h.act = h.fake
	if actor != nil {
		h.act = actor(h.fake)
	}
	h.start(opts)
	return h
}

// start (re)builds the daemon on the same fake Docker and clock, as a restart of the process would.
func (h *harness) start(opts func(*Options)) {
	bus := &engine.Bus{}
	bus.Subscribe(h.rec.Record)
	bus.Subscribe(func(e engine.Event) {
		for k := range e.Attrs {
			if !api.AttrAllowed(k) && !h.planted[k] {
				h.t.Errorf("event %s carries the attribute %q, which the API drops: add it to api.allowedAttrs if it belongs in the timeline", e.Type, k)
			}
		}
	})
	o := Options{Config: h.cfg, Reader: h.fake, Actor: h.act, Clock: h.clk, Bus: bus}
	if opts != nil {
		opts(&o)
	}
	h.d = New(o)
	h.d.CheckVersions(h.ctx)
}

// advance moves the clock and ticks once per second, as Run does.
func (h *harness) advance(total time.Duration) {
	for elapsed := time.Duration(0); elapsed < total; elapsed += time.Second {
		h.clk.Advance(time.Second)
		h.d.Tick(h.ctx)
	}
}

func (h *harness) tick() { h.d.Tick(h.ctx) }

func (h *harness) event(c docker.Container, action, detail string) {
	h.d.HandleEvent(h.ctx, docker.Event{Action: action, Detail: detail, ID: c.ID, Name: c.Name, Labels: c.Labels, Time: h.clk.Now()})
}

func labels(project, service string, extra ...string) map[string]string {
	m := map[string]string{
		docker.LabelProject: project, docker.LabelService: service, docker.LabelNumber: "1",
		docker.LabelOneOff: "False", config.LabelEnable: "true",
	}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

func (h *harness) running(id, project, service string, extra ...string) docker.Container {
	c := docker.Container{
		ID: id, Name: project + "-" + service + "-1", Labels: labels(project, service, extra...),
		Status: "running", Running: true, StartedAt: h.clk.Now().Add(-time.Hour), Created: h.clk.Now().Add(-time.Hour),
		RestartPolicy: "no", NetworkMode: project + "_default",
	}
	h.fake.Put(c)
	return c
}

func (h *harness) set(id string, fn func(*docker.Container)) docker.Container {
	h.fake.Update(id, fn)
	c, _ := h.fake.Inspect(h.ctx, id)
	return c
}

func (h *harness) count(typ engine.EventType) int { return h.rec.Count(typ) }

func (h *harness) wantCalls(want ...string) {
	h.t.Helper()
	var got []string
	for _, c := range h.fake.Calls() {
		got = append(got, c.Verb+" "+c.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		h.t.Fatalf("docker mutations = %v, want %v\nevents: %v", got, want, h.rec.Types())
	}
}

func unhealthy(c *docker.Container) { c.Health = "unhealthy" }
func healthy(c *docker.Container)   { c.Health = "healthy" }

func hexID(prefix string) string { return prefix + strings.Repeat("0", 64-len(prefix)) }

// ---- the basic ladder in M1: one restart, then give up ----------------------------

func TestUnhealthyIsRestartedOnceThenGivenUp(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick() // startup sweep: healthy, nothing to do
	h.wantCalls()

	c := h.set(a.ID, unhealthy)
	h.event(c, "health_status", "unhealthy")
	h.advance(29 * time.Second)
	h.wantCalls() // GRACE has not passed: events are only triggers
	h.advance(2 * time.Second)
	h.wantCalls("restart " + a.ID)
	if h.count(engine.IncidentOpened) != 1 || h.count(engine.ActionStarted) != 1 || h.count(engine.ActionResult) != 1 {
		t.Fatalf("events: %v", h.rec.Types())
	}

	// Still unhealthy after the cooldown: M1 has no rung 2, so it gives up and
	// the 60s poll must not restart it again and again.
	h.advance(10 * time.Minute)
	h.wantCalls("restart " + a.ID)
	if h.count(engine.RungUnavailable) != 1 || h.count(engine.Suspended) != 1 {
		t.Fatalf("want one RungUnavailable and one GaveUp: %v", h.rec.Types())
	}

	// It recovers by itself: the incident closes.
	h.set(a.ID, healthy)
	h.advance(2 * time.Minute)
	if h.count(engine.Unsuspended) != 1 || h.count(engine.Recovered) != 0 {
		t.Fatalf("healthy again: SUSPENDED clears at once, the incident only after RECOVERY_PERIOD: %v", h.rec.Types())
	}
	h.advance(10 * time.Minute)
	if h.count(engine.Recovered) != 1 {
		t.Fatalf("want Recovered: %v", h.rec.Types())
	}

	// A later failure is a new incident and gets a new restart.
	c = h.set(a.ID, unhealthy)
	h.event(c, "health_status", "unhealthy")
	h.advance(40 * time.Second)
	h.wantCalls("restart "+a.ID, "restart "+a.ID)
	if h.count(engine.IncidentOpened) != 2 {
		t.Fatalf("want two incidents: %v", h.rec.Types())
	}
}

func TestEventsAreOnlyTriggers(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	// A die event, but by the time GRACE is over the container is running and healthy.
	h.event(a, "die", "")
	h.advance(time.Minute)
	h.wantCalls()
	if h.count(engine.IncidentOpened) != 0 {
		t.Fatal("no incident should open for a transient die")
	}
}

func TestPollCatchesUnhealthyWithoutAnyEvent(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.set(a.ID, unhealthy) // health_status is edge-triggered; imagine we missed it
	h.advance(61*time.Second + 30*time.Second)
	h.wantCalls("restart " + a.ID)
}

func TestPollDetectsFailedRestartWithNoEvent(t *testing.T) {
	// E8: a failed restart-policy restart emits no event; only State.Error says so.
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "stuck")
	h.tick()
	h.set(a.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartCount, c.RestartPolicy = "exited", false, 1, 1, "unless-stopped"
		c.Error = "failed to create task for container: ... failed to fulfil mount request: open /x/src: no such file or directory"
	})
	h.advance(61*time.Second + 30*time.Second)
	h.wantCalls("start " + a.ID)
}

// A container that stays `restarting` with RestartCount never rising is not a crash loop under the §3.3
// rule: the loop signal is RestartCount rising, not the status (M1 treated it as one; M2a replaces that).
func TestRestartingWithoutRisingCountIsNotACrashLoop(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "loop")
	h.tick()
	h.set(a.ID, func(c *docker.Container) {
		c.Status, c.Running, c.Restarting, c.RestartPolicy = "restarting", true, true, "always"
	})
	h.advance(15 * time.Minute)
	h.wantCalls()
}

// A real crash loop is mostly `running`: the restart manager resets its backoff
// after a run of 10s, so these loops are rarely seen `restarting` for long.
// The crash-loop clock must survive the running observations.
func TestCrashLoopThatAlternatesRunningAndRestarting(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "loop", config.LabelHealExited, "never")
	h.tick()
	count := 0
	for i := 0; i < 12; i++ {
		count++
		c := h.set(a.ID, func(c *docker.Container) {
			c.Status, c.Running, c.Restarting, c.RestartPolicy, c.RestartCount = "restarting", true, true, "always", count
		})
		h.event(c, "die", "") // every cycle ends a run with a die
		h.advance(50 * time.Second)
		h.set(a.ID, func(c *docker.Container) { c.Status, c.Restarting = "running", false })
		h.advance(10 * time.Second)
		if i < 3 && len(h.fake.Calls()) != 0 {
			t.Fatalf("restarted after only %d minutes", i+1)
		}
	}
	if len(h.fake.CallsOn(a.ID)) == 0 {
		t.Fatalf("a loop that alternates running/restarting for 12 minutes was never healed: %v", h.rec.Types())
	}
}

func TestFastCrashLoopNeverSeenRestarting(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "loop")
	h.tick()
	count := 0
	var firstCall int
	for i := 0; i < 40 && firstCall == 0; i++ { // a die every 15s, always `running` when inspected
		count++
		c := h.set(a.ID, func(c *docker.Container) { c.RestartPolicy, c.RestartCount = "always", count })
		h.event(c, "die", "")
		h.advance(15 * time.Second)
		if len(h.fake.Calls()) > 0 {
			firstCall = i + 1
		}
	}
	if firstCall == 0 {
		t.Fatalf("a 15s crash loop was never healed: %v", h.rec.Types())
	}
	if elapsed := time.Duration(firstCall) * 15 * time.Second; elapsed < 5*time.Minute || elapsed > 8*time.Minute {
		t.Fatalf("healed after %s, want just after CRASHLOOP_AFTER (5m)", elapsed)
	}
}

func TestOneRestartIsNotACrashLoop(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	c := h.set(a.ID, func(c *docker.Container) { c.RestartPolicy, c.RestartCount = "always", 1 })
	h.event(c, "die", "")
	h.advance(15 * time.Minute)
	h.wantCalls()
}

// ---- dry-run is structural ------------------------------------------------------------

func TestDryRunPerformsNoMutation(t *testing.T) {
	var would []string
	h := newHarness(t, func(c *config.Global) { c.DryRun = true }, func(f *docker.Fake) docker.Actor {
		f.FailMutations = true // any call that reaches the fake would error and be recorded
		return docker.DryRun{Would: func(verb, id string) { would = append(would, verb+" "+id) }}
	})
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	c := h.set(a.ID, unhealthy)
	h.event(c, "health_status", "unhealthy")
	h.advance(5 * time.Minute)
	h.wantCalls() // nothing reached Docker
	if len(would) != 1 || would[0] != "restart "+a.ID {
		t.Fatalf("dry-run decisions = %v", would)
	}
	found := false
	for _, e := range h.rec.Events() {
		if e.Type == engine.ActionResult && e.Attrs["dry_run"] == "true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no dry-run ActionResult: %v", h.rec.Types())
	}
}

// ---- E1: never API-restart a dependent whose parent ID is gone --------------------------

func TestDependentWithParentGoneIsNotRestarted(t *testing.T) {
	h := newHarness(t, nil, nil)
	oldParent := hexID("48")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + oldParent })
	h.running(hexID("27"), "p", "parent") // the recreated parent, new ID
	h.tick()
	h.advance(3 * time.Minute)
	h.wantCalls()
	if h.count(engine.NeedsRecreate) != 1 {
		t.Fatalf("want exactly one NeedsRecreate (deduped across polls): %v", h.rec.Types())
	}
	if h.count(engine.IncidentOpened) != 0 {
		t.Fatal("a dependent that only needs a recreate must not open a restart incident")
	}

	// Even unhealthy, and even after its (failed) start left State.Error: still no restart.
	h.set(dep.ID, func(c *docker.Container) {
		c.Health = "unhealthy"
	})
	h.advance(3 * time.Minute)
	h.set(dep.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode = "exited", false, 137
		c.Error = "joining network namespace of container: No such container: " + oldParent
	})
	h.advance(3 * time.Minute)
	h.wantCalls()
}

func TestPreActionGuardCatchesVanishedParent(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { c.StartupSettle = time.Minute }, nil)
	parent := h.running(hexID("27"), "p", "parent")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID; c.Health = "unhealthy" })
	removed := false
	h.fake.AfterInspect = func(f *docker.Fake, ref string) {
		// The first time the dependent is inspected (the decision), the parent is recreated
		// right after: the decision was made against a world that no longer exists.
		if ref == dep.ID && !removed {
			removed = true
			f.Remove(parent.ID)
		}
	}
	h.advance(time.Minute + time.Second) // startup sweep
	h.wantCalls()
	if !removed {
		t.Fatal("hook never ran")
	}
	if h.count(engine.ActionRefused) != 1 {
		t.Fatalf("want ActionRefused: %v", h.rec.Types())
	}
}

// ---- "parent not running": act only on the parent ------------------------------------------

func TestParentStoppedActOnlyOnTheParent(t *testing.T) {
	h := newHarness(t, nil, nil)
	parent := h.running(hexID("27"), "p", "parent")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID })
	h.tick()

	// The parent crashed and the dependent failed to start behind it (E8e).
	h.set(parent.ID, func(c *docker.Container) { c.Status, c.Running, c.ExitCode = "exited", false, 1 })
	h.set(dep.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode = "exited", false, 137
		c.Error = "cannot join network namespace of a non running container: container p-parent-1 is exited"
	})
	h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
		if id == parent.ID { // starting the parent works
			f.Update(id, func(c *docker.Container) { c.Status, c.Running, c.StartedAt = "running", true, h.clk.Now() })
		}
	}
	h.advance(61*time.Second + 30*time.Second)

	// The parent is healed first. The dependent was never touched in the same pass.
	calls := h.fake.Calls()
	if len(calls) == 0 || calls[0].ID != parent.ID || calls[0].Verb != "start" {
		t.Fatalf("calls = %v; the first action must be the parent's start", calls)
	}
	if h.count(engine.WaitingForParent) == 0 {
		t.Fatalf("the dependent should have waited for its parent: %v", h.rec.Types())
	}
	// Then, with the parent running again under the same ID, the dependent is started.
	h.advance(2 * time.Minute)
	h.wantCalls("start "+parent.ID, "start "+dep.ID)
}

func TestDependentNeverTriggersActionOnItsParent(t *testing.T) {
	h := newHarness(t, nil, nil)
	parent := h.running(hexID("27"), "p", "parent", config.LabelHealExited, "never")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(parent.ID, func(c *docker.Container) { c.Status, c.Running, c.ExitCode = "exited", false, 137 }) // notify-only for the parent
	h.set(dep.ID, func(c *docker.Container) {
		c.NetworkMode = "container:" + parent.ID
		c.Status, c.Running, c.ExitCode = "exited", false, 137
		c.Error = "cannot join network namespace of a non running container: container p-parent-1 is exited"
	})
	h.tick()
	h.advance(5 * time.Minute)
	h.wantCalls() // no call at all, in particular none on the parent
	if n := len(h.fake.CallsOn(parent.ID)); n != 0 {
		t.Fatalf("%d calls on the parent", n)
	}
}

func TestParentWithOpenIncidentSuppressesDependent(t *testing.T) {
	h := newHarness(t, nil, nil)
	parent := h.running(hexID("27"), "p", "parent")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID })
	h.tick()
	h.set(parent.ID, unhealthy)
	h.set(dep.ID, unhealthy)
	h.advance(61*time.Second + 31*time.Second)
	// Both were evaluated in the same pass; the parent goes first (sorted key
	// order), and the dependent waits while the parent's incident is open.
	calls := h.fake.Calls()
	if len(calls) < 1 || calls[0].ID != parent.ID {
		t.Fatalf("calls = %v", calls)
	}
	for _, c := range calls {
		if c.ID == dep.ID {
			t.Fatalf("the dependent was restarted while its parent had an open incident: %v", calls)
		}
	}
}

// M2b: the dependents step. A dependent whose parent restarted later (same ID) is restarted through the API.
func TestStaleDependentAfterParentRestartIsRestarted(t *testing.T) {
	h := newHarness(t, nil, nil)
	parent := h.running(hexID("27"), "p", "parent")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID })
	h.set(parent.ID, func(c *docker.Container) { c.StartedAt = h.clk.Now().Add(-time.Minute) }) // restarted after the dependent
	h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
		f.Update(id, func(c *docker.Container) { c.StartedAt = h.clk.Now() })
	}
	h.tick()
	h.advance(3 * time.Minute)
	h.wantCalls("restart " + dep.ID)
}

// ---- in-flight window, manual stops ----------------------------------------------------------

func TestEventsDuringAnActionAreAttributedAndSetNoMarker(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.fake.OnMutate = func(f *docker.Fake, verb, id string) {
		// docker restart: kill, stop, die, start, restart (E9).
		for _, act := range []string{"kill", "die", "start"} {
			h.event(a, act, "")
		}
	}
	c := h.set(a.ID, unhealthy)
	h.event(c, "health_status", "unhealthy")
	h.advance(31 * time.Second)
	h.wantCalls("restart " + a.ID)
	if h.count(engine.ManualStopMarked) != 0 {
		t.Fatalf("our own restart must not look like a manual stop: %v", h.rec.Types())
	}
	if h.count(engine.EventAttributed) != 3 {
		t.Fatalf("want 3 attributed events: %v", h.rec.Types())
	}
	// A late event still stamped inside the window is attributed too...
	h.d.HandleEvent(h.ctx, docker.Event{Action: "kill", ID: a.ID, Name: a.Name, Labels: a.Labels, Time: h.clk.Now().Add(-time.Second)})
	if h.count(engine.ManualStopMarked) != 0 {
		t.Fatal("late event in the window set a marker")
	}
	// ...but one after the tail is interpreted.
	h.clk.Advance(10 * time.Second)
	h.d.HandleEvent(h.ctx, docker.Event{Action: "kill", ID: a.ID, Name: a.Name, Labels: a.Labels, Time: h.clk.Now()})
	if h.count(engine.ManualStopMarked) != 1 {
		t.Fatalf("a kill outside any window is a manual stop: %v", h.rec.Types())
	}
}

func TestManualStopIsLeftAloneAndStartClearsMarker(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	// docker stop of an app that exits 1 on SIGTERM: kill, die(1).
	h.set(a.ID, func(c *docker.Container) { c.Status, c.Running, c.ExitCode = "exited", false, 1 })
	h.event(a, "kill", "")
	h.event(a, "die", "")
	h.advance(3 * time.Minute)
	h.wantCalls()

	// Started by hand, then it crashes on its own: the marker was cleared, so it is a crash.
	h.set(a.ID, func(c *docker.Container) { c.Status, c.Running = "running", true })
	h.event(a, "start", "")
	h.set(a.ID, func(c *docker.Container) { c.Status, c.Running, c.ExitCode = "exited", false, 1 })
	h.event(a, "die", "")
	h.advance(40 * time.Second)
	h.wantCalls("start " + a.ID)
}

func TestExternalRecreateReArmsAfterGivingUp(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.set(a.ID, unhealthy)
	h.advance(10 * time.Minute)
	if h.count(engine.Suspended) != 1 {
		t.Fatalf("setup: %v", h.rec.Types())
	}
	h.wantCalls("restart " + a.ID)

	// Someone recreates it (outside any window) and it is unhealthy again.
	h.event(a, "create", "")
	h.advance(5 * time.Minute)
	if h.count(engine.Unsuspended) != 1 {
		t.Fatalf("want Unsuspended on an external create: %v", h.rec.Types())
	}
	h.wantCalls("restart "+a.ID, "restart "+a.ID)
}

// ---- skips, rate limit, startup, misc ---------------------------------------------------------

func TestSkips(t *testing.T) {
	h := newHarness(t, nil, nil)
	self := h.running(hexID("5e"), "p", "self")
	h.d.o.SelfID = self.ID
	oneoff := h.running(hexID("01"), "p", "run", docker.LabelOneOff, "True")
	swarm := h.running(hexID("02"), "p", "task", docker.LabelSwarmTaskID, "t1")
	off := h.running(hexID("03"), "p", "off", config.LabelEnable, "false")
	none := docker.Container{ID: hexID("04"), Name: "plain", Labels: map[string]string{}, Status: "running", Running: true, Health: "unhealthy"}
	h.fake.Put(none)
	h.tick()
	for _, c := range []docker.Container{self, oneoff, swarm, off} {
		h.set(c.ID, unhealthy)
	}
	h.advance(5 * time.Minute)
	h.wantCalls()
}

// WN-004: compose writes no container-number on a `docker compose run` container (compose's createContainer gets -1, and
// "one-off containers are not indexed"), so it falls back to the service's key. Being newer, it must still not take that
// key from the service's real container, in the loop or on the dashboard.
func TestComposeRunContainerDoesNotHideItsService(t *testing.T) {
	for _, tc := range []struct{ name, number, oneoff string }{
		{"no container-number", "", "True"}, {"container-number 1", "1", "True"}, {"lowercase oneoff", "", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil, nil)
			a := h.running(hexID("a1"), "p", "web")
			l := map[string]string{docker.LabelProject: "p", docker.LabelService: "web", docker.LabelOneOff: tc.oneoff, config.LabelEnable: "true"}
			if tc.number != "" {
				l[docker.LabelNumber] = tc.number
			}
			run := docker.Container{ID: hexID("b1"), Name: "p-web-run-1a2b3c", Status: "exited", Labels: l,
				Created: h.clk.Now(), RestartPolicy: "no", NetworkMode: "p_default"}
			h.fake.Put(run)
			h.tick()
			c := h.set(a.ID, unhealthy)
			h.event(c, "health_status", "unhealthy")
			h.advance(2 * time.Minute)
			h.wantCalls("restart " + a.ID)
			found := false
			for _, d := range h.d.containerDocs() {
				if d.Key == "h/p/web/1" {
					found = true
					if d.Name != a.Name {
						t.Fatalf("dashboard key shows %s, want the real container", d.Name)
					}
				}
			}
			if !found {
				t.Fatal("no dashboard row for h/p/web/1")
			}
		})
	}
}

// A key whose only container is a one-off container still resolves to it, and it is skipped.
func TestComposeRunContainerAloneIsShownAndSkipped(t *testing.T) {
	h := newHarness(t, nil, nil)
	run := docker.Container{ID: hexID("b1"), Name: "p-web-run-1a2b3c", Status: "running", Running: true,
		Labels:  map[string]string{docker.LabelProject: "p", docker.LabelService: "web", docker.LabelOneOff: "True", config.LabelEnable: "true"},
		Created: h.clk.Now(), RestartPolicy: "no", NetworkMode: "p_default"}
	h.fake.Put(run)
	h.tick()
	h.set(run.ID, unhealthy)
	h.advance(5 * time.Minute)
	h.wantCalls()
	docs := h.d.containerDocs()
	if len(docs) != 1 || docs[0].Name != run.Name {
		t.Fatalf("%+v", docs)
	}
}

func TestNonComposeContainerIsHealedByName(t *testing.T) {
	h := newHarness(t, nil, nil)
	c := docker.Container{ID: hexID("a1"), Name: "plain", Labels: map[string]string{config.LabelEnable: "true"}, Status: "running", Running: true}
	h.fake.Put(c)
	h.tick()
	h.set(c.ID, unhealthy)
	h.advance(2 * time.Minute)
	h.wantCalls("restart " + c.ID)
	for _, ev := range h.rec.Events() {
		if ev.Type == engine.IncidentOpened && ev.Key != "h/plain" {
			t.Fatalf("key = %q", ev.Key)
		}
	}
	if h.count(engine.IncidentOpened) != 1 {
		t.Fatal(h.rec.Types())
	}
}

func TestModeAllHealsEverythingExceptOptedOut(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { c.Mode = config.ModeAll }, nil)
	a := h.running(hexID("a1"), "p", "a")
	delete(a.Labels, config.LabelEnable)
	h.fake.Put(a)
	b := h.running(hexID("b1"), "p", "b", config.LabelEnable, "false")
	h.tick()
	h.set(a.ID, unhealthy)
	h.set(b.ID, unhealthy)
	h.advance(2 * time.Minute)
	h.wantCalls("restart " + a.ID)
}

func TestRateLimitHoldsThenRetries(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { c.RateLimit = 2 }, nil)
	var ids []string
	for i := 0; i < 3; i++ {
		c := h.running(hexID(fmt.Sprintf("a%d", i+1)), "p", fmt.Sprintf("s%d", i))
		ids = append(ids, c.ID)
	}
	h.tick()
	for _, id := range ids {
		h.set(id, unhealthy)
	}
	h.advance(2 * time.Minute)
	if got := len(h.fake.Calls()); got != 2 {
		t.Fatalf("rate limit of 2/hour allowed %d restarts", got)
	}
	if h.count(engine.RateLimited) != 1 {
		t.Fatalf("want one RateLimited: %v", h.rec.Types())
	}
	h.advance(time.Hour)
	if got := len(h.fake.Calls()); got != 3 {
		t.Fatalf("after the window, the held restart should run: %d", got)
	}
}

func TestStartupSettleDefersActing(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { c.StartupSettle = 2 * time.Minute }, nil)
	a := h.running(hexID("a1"), "p", "web", config.LabelHealExited, "always")
	h.set(a.ID, func(c *docker.Container) { c.Status, c.Running, c.ExitCode = "exited", false, 0 })
	h.event(a, "die", "") // recorded, but nothing is scheduled while the host is still booting
	h.advance(time.Minute + 50*time.Second)
	h.wantCalls()
	h.advance(15 * time.Second) // settle over: the sweep sees it
	h.wantCalls("start " + a.ID)
}

func TestGoneContainerClosesItsIncident(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.set(a.ID, unhealthy)
	h.advance(61*time.Second + 31*time.Second)
	h.fake.Remove(a.ID)
	h.advance(2 * time.Minute)
	if h.count(engine.IncidentClosed) != 1 {
		t.Fatalf("want IncidentClosed: %v", h.rec.Types())
	}
}

func TestActionsLabelWithoutRestart(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web", config.LabelActions, "recreate")
	h.tick()
	h.set(a.ID, unhealthy)
	h.advance(2 * time.Minute)
	h.wantCalls()
	if h.count(engine.RungUnavailable) != 1 || h.count(engine.Suspended) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
}

func TestCooldownLabelOverride(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web", config.LabelCooldown, "3m")
	h.tick()
	c := h.set(a.ID, unhealthy)
	h.event(c, "health_status", "unhealthy")
	h.advance(31 * time.Second)
	h.advance(2 * time.Minute)
	if h.count(engine.Suspended) != 0 {
		t.Fatalf("gave up before the 3m cooldown: %v", h.rec.Types())
	}
	h.advance(90 * time.Second)
	if h.count(engine.Suspended) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
}

func TestActionErrorIsReportedAndNotRetriedInTheSamePass(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.fake.FailMutations = true
	h.set(a.ID, unhealthy)
	h.advance(10 * time.Minute)
	h.wantCalls("restart " + a.ID)
	failed := false
	for _, e := range h.rec.Events() {
		failed = failed || (e.Type == engine.ActionResult && e.Attrs["ok"] == "false")
	}
	if !failed {
		t.Fatalf("a failed restart must be reported: %v", h.rec.Types())
	}
}

func TestDuplicateKeyPrefersNewest(t *testing.T) {
	// Compose renames the old container while it recreates it; both carry the labels.
	h := newHarness(t, nil, nil)
	old := h.running(hexID("01"), "p", "web")
	h.set(old.ID, func(c *docker.Container) {
		c.Name = "abc_p-web-1"
		c.Created = h.clk.Now().Add(-time.Hour)
		unhealthy(c)
	})
	nw := h.running(hexID("02"), "p", "web")
	h.set(nw.ID, func(c *docker.Container) { c.Created = h.clk.Now() })
	h.tick()
	h.advance(5 * time.Minute)
	h.wantCalls() // the new, healthy one decides the key
}

// ---- event stream ---------------------------------------------------------------------------------

// HandleEvent takes the actions the subscription asks for (docker.WatchedActions) and nothing else, so the two cannot
// drift apart. An open action window is the probe: every event that reaches the engine is attributed to it.
func TestHandleEventActionsMatchTheSubscription(t *testing.T) {
	h := newHarness(t, nil, nil)
	c := h.running(hexID("01"), "p", "web")
	h.tick()
	h.d.eng.Begin(engine.Target{Key: docker.StateKey(host, c.Name, c.Labels), ID: c.ID, Name: c.Name}, engine.Plan{}, rules.Decision{})
	reached := func(action string) bool {
		before := h.rec.Count(engine.EventAttributed)
		h.event(c, action, "unhealthy")
		return h.rec.Count(engine.EventAttributed) > before
	}
	for _, a := range docker.WatchedActions {
		if !reached(a) {
			t.Errorf("%s is subscribed to but HandleEvent drops it", a)
		}
	}
	for _, a := range []string{"exec_create", "exec_start", "exec_die", "stop", "restart", "rename", "attach", "pause"} {
		if docker.Watched(a) {
			t.Errorf("%s is subscribed to", a)
		}
		if reached(a) {
			t.Errorf("HandleEvent took %s, which is not subscribed to", a)
		}
	}
}

func TestEventStreamReconnectsWithSince(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.d.o.ReconnectBackoff = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan docker.Event, 8)
	sig := make(chan streamSignal, 16)
	go h.d.watchEvents(ctx, out, sig)

	waitFor(t, func() bool { return len(h.fake.Subscriptions()) == 1 })
	evTime := time.Date(2026, 10, 17, 12, 0, 5, 0, time.UTC)
	h.fake.Emit(docker.Event{Action: "die", ID: "x", Time: evTime})
	<-out
	h.fake.BreakStream(fmt.Errorf("connection reset"))
	waitFor(t, func() bool { return len(h.fake.Subscriptions()) == 2 })
	if got := h.fake.Subscriptions()[1]; !got.Equal(evTime) {
		t.Fatalf("resubscribed with since=%v, want %v", got, evTime)
	}
}

// A daemon that refuses every subscription is down, however often whalenurse
// retries: a failed retry must not count as the stream coming back.
func TestHealthyReportsAStreamThatFailsOnEveryRetry(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.d.o.ReconnectBackoff = 5 * time.Millisecond
	h.fake.EventsErr = fmt.Errorf("connection refused")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.d.watchEvents(ctx, make(chan docker.Event, 8), make(chan streamSignal, 16))

	waitFor(t, func() bool { return len(h.fake.Subscriptions()) >= 3 })
	h.clk.Advance(3 * time.Minute)
	h.d.lastTick.Store(h.clk.Now().UnixNano())
	n := len(h.fake.Subscriptions())
	waitFor(t, func() bool { return len(h.fake.Subscriptions()) > n+1 })
	if ok, why := h.d.Healthy(); ok || !strings.Contains(why, "events stream down") {
		t.Fatalf("Healthy() = %v, %q; want the stream reported down", ok, why)
	}
	if h.d.eventsUp.Load() {
		t.Fatal("events_up after failed subscriptions")
	}
}

func TestHealthyReflectsEventStream(t *testing.T) {
	h := newHarness(t, nil, nil)
	if ok, _ := h.d.Healthy(); !ok {
		t.Fatal("a fresh daemon should be given the threshold to connect")
	}
	h.clk.Advance(3 * time.Minute)
	h.d.lastTick.Store(h.clk.Now().UnixNano())
	if ok, why := h.d.Healthy(); ok {
		t.Fatalf("events stream never came up for 3m, want unhealthy (%s)", why)
	}
	h.d.setEvents(true)
	if ok, why := h.d.Healthy(); !ok {
		t.Fatal(why)
	}
	h.d.setEvents(false)
	h.clk.Advance(3 * time.Minute)
	h.d.lastTick.Store(h.clk.Now().UnixNano())
	if ok, _ := h.d.Healthy(); ok {
		t.Fatal("stream down for 3m should be unhealthy")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

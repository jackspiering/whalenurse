package daemon

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/store"
)

func modeAll(c *config.Global) { c.Mode = config.ModeAll }

func loadEvents(t *testing.T, name string) []docker.Event {
	t.Helper()
	f, err := os.Open("../../testdata/events/" + name + ".ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []docker.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		ev, err := docker.ParseEvent(sc.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		if len(ev.Action) >= 5 && ev.Action[:5] == "exec_" {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func evKey(ev docker.Event) string { return docker.StateKey(host, ev.Name, ev.Labels) }

// containersFrom puts one running container per container named in the events into the fake.
func (h *harness) containersFrom(evs []docker.Event) map[string]docker.Container {
	out := map[string]docker.Container{}
	for _, ev := range evs {
		k := evKey(ev)
		if _, ok := out[k]; ok {
			continue
		}
		c := docker.Container{
			ID: ev.ID, Name: ev.Name, Labels: ev.Labels, Status: "running", Running: true, RestartPolicy: "unless-stopped",
			StartedAt: h.clk.Now().Add(-time.Hour), Created: h.clk.Now().Add(-time.Hour), NetworkMode: "default",
		}
		h.fake.Put(c)
		out[k] = c
	}
	return out
}

// feed replays events with their fixture offsets, moving the clock to each one.
func (h *harness) feed(evs []docker.Event) {
	h.t.Helper()
	base := h.clk.Now()
	for _, ev := range evs {
		var off time.Duration // the scrubber rebases times onto 0, which the parser leaves unset
		if !ev.Time.IsZero() {
			off = time.Duration(ev.Time.UnixNano())
		}
		at := base.Add(off)
		if at.After(h.clk.Now()) {
			h.clk.Advance(at.Sub(h.clk.Now()))
		}
		ev.Time = h.clk.Now()
		h.d.HandleEvent(h.ctx, ev)
	}
}

func (h *harness) stopAll(ctrs map[string]docker.Container, exit int) {
	for _, c := range ctrs {
		h.set(c.ID, func(c *docker.Container) { c.Status, c.Running, c.ExitCode = "exited", false, exit })
	}
}

func (h *harness) startAll(ctrs map[string]docker.Container) {
	for _, c := range ctrs {
		h.set(c.ID, func(c *docker.Container) { c.Status, c.Running, c.ExitCode = "running", true, 0 })
	}
}

func (h *harness) markers(ctrs map[string]docker.Container) (n int) {
	for k := range ctrs {
		if h.d.Engine().Evidence(k).ManualStop {
			n++
		}
	}
	return n
}

func sortedKeys(m map[string]docker.Container) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// withStore wires a real SQLite store and a boot id, loading whatever the store holds.
func (h *harness) withStore(st *store.Store, bootID string) func(*Options) {
	return func(o *Options) {
		// As the binary does (openStore in cmd/whalenurse): re-key to the host ID, then load.
		if _, err := st.Rekey(h.cfg.HostID, store.RekeyMeta{Keyed: MetaKeyed, WithKey: MetaWithKey}); err != nil {
			h.t.Fatal(err)
		}
		snap, err := st.Load(h.clk.Now())
		if err != nil {
			h.t.Fatal(err)
		}
		o.Persist, o.Snapshot, o.Meta = st, snap, st
		o.BootID = func() string { return bootID }
	}
}

func settled(c *config.Global) { c.StartupSettle = 2 * time.Minute }

// ---- shutdown kills (plan §3.3, Manual-stop markers) -------------------------------------------

// A daemon shutdown or reboot sends kill to every running container. M1 read those as 12 manual stops.
// A stream loss quarantines them (mechanism a).
func TestShutdownKillsAreQuarantinedByAStreamLoss(t *testing.T) {
	for _, fixture := range []string{"m0b-daemon-restart-shutdown", "m0b-reboot-shutdown"} {
		t.Run(fixture, func(t *testing.T) {
			h := newHarness(t, modeAll, nil)
			evs := loadEvents(t, fixture)
			ctrs := h.containersFrom(evs)
			h.tick()
			h.feed(evs)
			if got := h.markers(ctrs); got != len(ctrs) {
				t.Fatalf("setup: %d of %d markers", got, len(ctrs))
			}
			h.stopAll(ctrs, 137)

			h.d.StreamLost(h.clk.Now())
			if got := h.markers(ctrs); got != 0 {
				t.Fatalf("%d markers survived the stream loss", got)
			}
			if h.count(engine.MarkersQuarantined) != 1 {
				t.Fatalf("%v", h.rec.Types())
			}
			// Judged without evidence: a signal exit without OOM is notify-only, never healed.
			h.advance(10 * time.Minute)
			h.wantCalls()
			if h.count(engine.NotifyOnly) != len(ctrs) {
				t.Fatalf("want %d notify-only, got %d", len(ctrs), h.count(engine.NotifyOnly))
			}
		})
	}
}

func TestAnOlderRealManualStopSurvivesTheQuarantine(t *testing.T) {
	h := newHarness(t, modeAll, nil)
	real := h.running(hexID("a1"), "p", "real")
	shut := h.running(hexID("b1"), "p", "shutdown")
	h.tick()
	h.event(real, "kill", "")
	h.clk.Advance(45 * time.Second)
	h.event(shut, "kill", "")
	h.clk.Advance(10 * time.Second)
	h.d.StreamLost(h.clk.Now())
	if !h.d.Engine().Evidence("h/p/real/1").ManualStop {
		t.Fatal("a stop 55s before the loss is a real manual stop and must survive")
	}
	if h.d.Engine().Evidence("h/p/shutdown/1").ManualStop {
		t.Fatal("a kill 10s before the loss is within the quarantine window")
	}
}

// The rejected burst rule (plan §3.3): `docker compose stop` sends the same burst of kill[15] with the stream
// still up. Those are deliberate stops: the markers stay, and the stopped containers are not healed. The
// container here exits 1 on SIGTERM, so it is the marker that keeps it down (137 would be notify-only anyway).
func TestComposeStopBurstKeepsItsMarkersAndTheContainersStayStopped(t *testing.T) {
	burst := func(t *testing.T) []docker.Event {
		var out []docker.Event
		for _, ev := range loadEvents(t, "m0b-daemon-restart-shutdown") {
			if ev.Action == "kill" && ev.Signal == "15" {
				out = append(out, ev)
			}
		}
		if len(out) < 2 {
			t.Fatalf("fixture has %d kill[15] events", len(out))
		}
		return out
	}
	setup := func(t *testing.T) (*harness, map[string]docker.Container, string) {
		h := newHarness(t, modeAll, nil)
		evs := burst(t)
		ctrs := h.containersFrom(evs)
		h.tick()
		h.feed(evs)
		h.stopAll(ctrs, 137)
		exits1 := sortedKeys(ctrs)[0]
		h.set(ctrs[exits1].ID, func(c *docker.Container) { c.ExitCode = 1 }) // exits 1 on SIGTERM
		return h, ctrs, exits1
	}

	t.Run("stream up: deliberate stop", func(t *testing.T) {
		h, ctrs, exits1 := setup(t)
		if got := h.markers(ctrs); got != len(ctrs) {
			t.Fatalf("%d of %d markers: a burst must not be mistaken for a shutdown", got, len(ctrs))
		}
		h.advance(10 * time.Minute)
		h.wantCalls() // nothing was started
		if h.count(engine.MarkersQuarantined) != 0 {
			t.Fatal("no stream loss, nothing to quarantine")
		}
		if !h.d.Engine().Evidence(exits1).ManualStop {
			t.Fatal("marker lost")
		}
	})

	t.Run("control: the same burst followed by a stream loss", func(t *testing.T) {
		// Shows the marker is what held the exit-1 container down above: with it gone (a real shutdown),
		// rule 4 sees a non-zero exit and no evidence.
		h, _, exits1 := setup(t)
		h.d.StreamLost(h.clk.Now())
		h.advance(2 * time.Minute)
		calls := h.fake.Calls()
		if len(calls) != 1 || calls[0].Verb != "start" || calls[0].Name == "" {
			t.Fatalf("calls = %v; the exit-1 container (%s) should be started", calls, exits1)
		}
	})
}

// Mechanism b: the boot id is kept with the markers. A reboot stops whalenurse in the same second as the
// containers, so a marker can exist with no matching event in the next run.
func TestANewBootIdDropsMarkersAndOpensARestoreWindow(t *testing.T) {
	st := openStore(t)
	h := newHarnessOpts(t, settled, nil, nil)
	h.start(h.withStore(st, "boot-A"))
	a := h.running(hexID("a1"), "p", "web")
	h.advance(2*time.Minute + time.Second)
	h.event(a, "kill", "")
	if !h.d.Engine().Evidence("h/p/web/1").ManualStop {
		t.Fatal("setup")
	}
	h.advance(time.Minute) // a real manual stop, a minute before whalenurse is restarted

	h.start(h.withStore(st, "boot-A")) // whalenurse restarted, same boot: a marker from before its last 30s is still good
	if !h.d.Engine().Evidence("h/p/web/1").ManualStop {
		t.Fatal("a restart within the same boot must keep the markers older than the quarantine window")
	}
	if !h.d.Engine().RestoreWindowUntil().Before(h.clk.Now()) {
		t.Fatal("no restore window without a reboot")
	}

	h.start(h.withStore(st, "boot-B")) // the host rebooted
	if h.d.Engine().Evidence("h/p/web/1").ManualStop {
		t.Fatal("a marker from before a reboot survived")
	}
	if want := h.clk.Now().Add(2 * time.Minute); !h.d.Engine().RestoreWindowUntil().Equal(want) {
		t.Fatalf("restore window until %v, want %v", h.d.Engine().RestoreWindowUntil(), want)
	}
	// And the drop was persisted: the next run of this boot does not see it again.
	h.start(h.withStore(st, "boot-B"))
	if h.d.Engine().Evidence("h/p/web/1").ManualStop {
		t.Fatal("dropped marker came back")
	}
}

// Mechanisms a and c together: a SUSPENDED container that is killed by a shutdown and restored stays SUSPENDED.
func suspendedStack(t *testing.T, fixture string) (*harness, map[string]docker.Container, string) {
	h := newHarness(t, func(c *config.Global) { modeAll(c); settled(c) }, nil)
	evs := loadEvents(t, fixture)
	ctrs := h.containersFrom(evs)
	key := "h/whalenurse-m0-b-boot/a-parent/1"
	if _, ok := ctrs[key]; !ok {
		t.Fatalf("no %s in %v", key, sortedKeys(ctrs))
	}
	h.set(ctrs[key].ID, func(c *docker.Container) { c.Health = "unhealthy" })
	h.d.StreamConnected() // the first connection
	h.advance(2*time.Minute + time.Second)
	h.advance(5 * time.Minute)
	if !h.d.Engine().Suspended(key) {
		t.Fatalf("setup: a-parent is not suspended: %v", h.rec.Types())
	}
	h.wantCalls("restart " + ctrs[key].ID)
	return h, ctrs, key
}

func TestASuspendedContainerKilledByAShutdownAndRestoredStaysSuspended(t *testing.T) {
	for _, tc := range []struct{ shutdown, boot string }{
		{"m0b-daemon-restart-shutdown", "m0b-daemon-restart-boot"},
		{"m0b-reboot-shutdown", "m0b-reboot-boot"},
	} {
		t.Run(tc.shutdown, func(t *testing.T) {
			h, ctrs, key := suspendedStack(t, tc.shutdown)
			h.feed(loadEvents(t, tc.shutdown))
			h.stopAll(ctrs, 137)

			h.d.StreamLost(h.clk.Now())
			h.clk.Advance(20 * time.Second) // the daemon is down, then back; its restore emits starts
			h.d.StreamConnected()
			h.startAll(ctrs)
			h.set(ctrs[key].ID, func(c *docker.Container) { c.Health = "unhealthy" })
			h.feed(loadEvents(t, tc.boot))

			if !h.d.Engine().Suspended(key) {
				t.Fatalf("the restore unsuspended it: %v", h.rec.Types())
			}
			if h.count(engine.Unsuspended) != 0 {
				t.Fatalf("%v", h.rec.Types())
			}
			h.advance(time.Minute)
			h.wantCalls("restart " + ctrs[key].ID) // and nothing more was tried
		})
	}
}

// The restore window on its own (mechanism c): no stream loss was seen, but the daemon came back.
func TestStartsInsideTheRestoreWindowDoNotClearSuspended(t *testing.T) {
	h, ctrs, key := suspendedStack(t, "m0b-daemon-restart-shutdown")
	h.event(ctrs[key], "kill", "") // a marker (for instance one that outlived the quarantine)
	h.d.Engine().OpenRestoreWindow(2*time.Minute, "test")
	h.event(ctrs[key], "start", "")
	if !h.d.Engine().Suspended(key) {
		t.Fatal("a start in the restore window is a restore")
	}
	// A kill inside the window sets no marker.
	other := ctrs["h/whalenurse-m0-b-boot/s-parent/1"]
	h.event(other, "kill", "")
	if h.d.Engine().Evidence("h/whalenurse-m0-b-boot/s-parent/1").ManualStop {
		t.Fatal("a kill inside the restore window set a marker")
	}
	// Outside it, the same sequence is a person: kill, then start, clears SUSPENDED.
	h.clk.Advance(3 * time.Minute)
	h.event(ctrs[key], "kill", "")
	h.event(ctrs[key], "start", "")
	if h.d.Engine().Suspended(key) {
		t.Fatal("kill then start outside the window should unsuspend")
	}
}

// In container mode a daemon restart kills whalenurse too. Its stream ends by its own SIGTERM, so no stream loss is
// ever seen, and the boot id is unchanged. The shutdown-kill markers it saw before dying were persisted. The
// heartbeat (plan §3.3 mechanism d, v3.5) treats the previous run's end as a stream loss: the markers set in the
// 30 s before last_seen are discarded, so the restore's `start` follows no marker and SUSPENDED survives.
// (This was the "known gap" of M2a, pinned by a test that asserted the opposite.)
func TestWhalenurseKilledByTheDaemonShutdownItselfStaysSuspended(t *testing.T) {
	for _, tc := range []struct{ shutdown, boot string }{
		{"m0b-daemon-restart-shutdown", "m0b-daemon-restart-boot"},
		{"m0b-reboot-shutdown", "m0b-reboot-boot"},
	} {
		t.Run(tc.shutdown, func(t *testing.T) {
			st := openStore(t)
			h := newHarnessOpts(t, func(c *config.Global) { modeAll(c) }, nil, nil)
			h.start(h.withStore(st, "boot-A"))
			evs := loadEvents(t, tc.shutdown)
			ctrs := h.containersFrom(evs)
			key := "h/whalenurse-m0-b-boot/a-parent/1"
			h.set(ctrs[key].ID, func(c *docker.Container) { c.Health = "unhealthy" })
			h.advance(7 * time.Minute)
			if !h.d.Engine().Suspended(key) {
				t.Fatalf("setup: %v", h.rec.Types())
			}
			h.feed(evs) // the shutdown kills reach whalenurse before it is killed itself; markers are persisted
			if h.markers(ctrs) == 0 {
				t.Fatal("setup: the kills should have set markers")
			}
			// whalenurse exits (SIGTERM from the daemon): the context just ends, no StreamLost. A clean stop writes last_seen.
			h.d.heartbeat(true)
			h.stopAll(ctrs, 137)
			h.clk.Advance(30 * time.Second) // the daemon restarts; the new daemon restores whalenurse among the others

			h.start(h.withStore(st, "boot-A")) // same boot id
			if got := h.markers(ctrs); got != 0 {
				t.Fatalf("%d shutdown-kill markers survived the restart of whalenurse", got)
			}
			h.startAll(ctrs)
			h.set(ctrs[key].ID, func(c *docker.Container) { c.Health = "unhealthy" })
			h.feed(loadEvents(t, tc.boot))
			if !h.d.Engine().Suspended(key) {
				t.Fatalf("the restore unsuspended it: %v", h.rec.Types())
			}
			if h.count(engine.Unsuspended) != 0 {
				t.Fatalf("%v", h.rec.Types())
			}
		})
	}
}

func TestALongGapSinceLastSeenOpensARestoreWindow(t *testing.T) {
	st := openStore(t)
	set := func(c *config.Global) { settled(c); c.PollInterval = time.Minute }
	h := newHarnessOpts(t, set, nil, nil)
	h.start(h.withStore(st, "boot-A"))
	a := h.running(hexID("a1"), "p", "web")
	h.advance(2*time.Minute + time.Second)
	h.d.heartbeat(true)

	h.clk.Advance(50 * time.Second) // under the poll interval: not a window
	h.start(h.withStore(st, "boot-A"))
	if !h.d.Engine().RestoreWindowUntil().Before(h.clk.Now()) {
		t.Fatal("a short gap opened a restore window")
	}

	h.d.heartbeat(true)
	h.clk.Advance(5 * time.Minute) // whalenurse was down for a while; the daemon may have been restoring
	h.start(h.withStore(st, "boot-A"))
	if want := h.clk.Now().Add(2 * time.Minute); !h.d.Engine().RestoreWindowUntil().Equal(want) {
		t.Fatalf("restore window until %v, want %v", h.d.Engine().RestoreWindowUntil(), want)
	}
	h.event(a, "kill", "") // inside it, a kill sets no marker
	if h.d.Engine().Evidence("h/p/web/1").ManualStop {
		t.Fatal("a kill inside the restore window set a marker")
	}
}

func TestHeartbeatIsWrittenEveryFewSecondsAndAtTheEnd(t *testing.T) {
	st := openStore(t)
	h := newHarnessOpts(t, nil, nil, nil)
	h.start(h.withStore(st, "boot-A"))
	last := func() int64 {
		v, _ := st.Meta(MetaLastSeen)
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	first := last()
	if first == 0 {
		t.Fatal("no last_seen written at start")
	}
	h.advance(3 * time.Second)
	if last() != first {
		t.Fatal("written more often than every few seconds")
	}
	h.advance(3 * time.Second)
	if last() == first {
		t.Fatal("not refreshed after the heartbeat interval")
	}
	h.clk.Advance(time.Second)
	h.d.heartbeat(true)
	if want := h.clk.Now().UnixNano(); last() != want {
		t.Fatalf("the final heartbeat should be 'now': %d vs %d", last(), want)
	}
	// A dry run has no store, hence no heartbeat and no startup bookkeeping.
	h2 := newHarness(t, nil, nil)
	h2.advance(time.Minute)
}

// ---- the sweep after a reconnect ----------------------------------------------------------------

func TestSweepRunsAfterEveryReconnectThatFollowsALostStream(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { settled(c); c.PollInterval = 24 * time.Hour }, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.advance(2*time.Minute + time.Second) // startup sweep
	h.d.StreamConnected()                  // the first connection is not a reconnect
	h.advance(5 * time.Minute)
	h.wantCalls()

	h.d.StreamLost(h.clk.Now())
	h.set(a.ID, unhealthy) // while the stream was down; health_status is edge-triggered, no event will repeat it
	h.d.StreamConnected()
	h.advance(time.Minute + 50*time.Second)
	h.wantCalls() // the daemon may still be restoring: wait out STARTUP_SETTLE
	h.advance(20 * time.Second)
	h.wantCalls("restart " + a.ID)
	if h.count(engine.RestoreWindowOpened) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
}

func TestAPIErrorsWhileTheDaemonRestoresOpenNoIncident(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.set(a.ID, unhealthy)
	h.fake.ListErr = fmt.Errorf("Cannot connect to the Docker daemon: is the docker daemon running?")
	h.d.StreamLost(h.clk.Now())
	h.advance(10 * time.Minute)
	h.wantCalls()
	if h.count(engine.IncidentOpened) != 0 {
		t.Fatalf("an API error is 'not ready', never an incident: %v", h.rec.Types())
	}
	h.fake.ListErr = nil
	h.d.StreamConnected()
	h.advance(2 * time.Minute)
	h.wantCalls("restart " + a.ID)
}

// ---- crash loops (plan §3.3 rule 3) -------------------------------------------------------------

// A container that restarts every 15 s must be healed no earlier than CRASHLOOP_AFTER and no later than a
// couple of inspect intervals after it, whatever POLL_INTERVAL and GRACE are, with or without die events.
func TestCrashLoopIsHealedAtTheSameTimeWhateverThePollIntervalAndGrace(t *testing.T) {
	const cycle = 15 * time.Second
	for _, poll := range []time.Duration{20 * time.Second, time.Minute, 3 * time.Minute} {
		for _, grace := range []time.Duration{0, 5 * time.Second, 30 * time.Second, 2 * time.Minute} {
			for _, events := range []bool{true, false} {
				name := fmt.Sprintf("poll=%s/grace=%s/events=%v", poll, grace, events)
				t.Run(name, func(t *testing.T) {
					h := newHarness(t, func(c *config.Global) { c.PollInterval, c.Grace = poll, grace }, nil)
					a := h.running(hexID("a1"), "p", "loop", config.LabelHealExited, "never")
					h.tick()
					h.advance(poll + time.Second) // a baseline poll before the loop starts
					t0 := h.clk.Now()
					count, healed := 0, time.Duration(-1)
					for healed < 0 && h.clk.Now().Sub(t0) < 30*time.Minute {
						count++
						c := h.set(a.ID, func(c *docker.Container) {
							c.RestartPolicy, c.RestartCount, c.FinishedAt = "always", count, h.clk.Now()
						})
						if events {
							h.event(c, "die", "")
						}
						h.advance(cycle)
						if len(h.fake.Calls()) > 0 {
							healed = h.clk.Now().Sub(t0)
						}
					}
					if healed < 0 {
						t.Fatalf("never healed: %v", h.rec.Types())
					}
					lag := poll + grace
					if healed < 5*time.Minute {
						t.Fatalf("healed after %s, before CRASHLOOP_AFTER", healed)
					}
					if limit := 5*time.Minute + 2*lag + 2*cycle; healed > limit {
						t.Fatalf("healed after %s, want within %s", healed, limit)
					}
				})
			}
		}
	}
}

// A poll has to read RestartCount of containers that look healthy, or a loop whose die events were missed
// (the events stream was down, or whalenurse was) is invisible: the container list carries no count.
func TestPollSeesALoopThatShowedNoEvents(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "loop")
	h.tick()
	for i := 1; i <= 12; i++ {
		h.set(a.ID, func(c *docker.Container) { c.RestartPolicy, c.RestartCount = "always", i })
		h.advance(time.Minute)
	}
	if len(h.fake.CallsOn(a.ID)) == 0 {
		t.Fatalf("%v", h.rec.Types())
	}
}

// ---- pause ----------------------------------------------------------------------------------------

func TestPausedProjectIsNotHealedUntilThePauseExpires(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	b := h.running(hexID("b1"), "q", "web")
	h.tick()
	if _, err := h.d.Engine().Pause("p", engine.DefaultPause); err != nil {
		t.Fatal(err)
	}
	h.set(a.ID, unhealthy)
	h.set(b.ID, unhealthy)
	h.advance(30 * time.Minute)
	h.wantCalls("restart " + b.ID) // only q was healed
	h.advance(time.Hour)
	h.wantCalls("restart " + b.ID)
	h.advance(31 * time.Minute) // the 2h pause is over: p is re-evaluated at once, not at the next poll
	h.wantCalls("restart "+b.ID, "restart "+a.ID)
	if h.count(engine.Resumed) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
}

func TestResumeReevaluatesWithoutWaitingForThePoll(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { c.PollInterval = 24 * time.Hour }, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.d.Engine().Pause("p/web", 0)
	c := h.set(a.ID, unhealthy)
	h.event(c, "health_status", "unhealthy")
	h.advance(10 * time.Minute)
	h.wantCalls()
	h.d.Engine().Resume("p/web")
	h.advance(5 * time.Second)
	h.wantCalls("restart " + a.ID)
}

func TestUnsuspendReevaluates(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.set(a.ID, unhealthy)
	h.advance(5 * time.Minute)
	if !h.d.Engine().Suspended("h/p/web/1") {
		t.Fatalf("setup: %v", h.rec.Types())
	}
	keys, _ := h.d.Engine().Unsuspend("p")
	if len(keys) != 1 {
		t.Fatal(keys)
	}
	h.advance(2 * time.Minute)
	h.wantCalls("restart "+a.ID, "restart "+a.ID) // a fresh incident, a fresh pass
}

// ---- persistence --------------------------------------------------------------------------------------

func TestStateSurvivesARestartOfWhalenurse(t *testing.T) {
	st := openStore(t)
	h := newHarnessOpts(t, nil, nil, nil)
	h.start(h.withStore(st, "boot-A"))
	a := h.running(hexID("a1"), "p", "web")
	b := h.running(hexID("b1"), "p", "other")
	h.tick()
	h.set(a.ID, unhealthy)
	h.advance(5 * time.Minute)
	if !h.d.Engine().Suspended("h/p/web/1") {
		t.Fatalf("setup: %v", h.rec.Types())
	}
	h.d.Engine().Pause("p/other", 3*time.Hour)

	h.start(h.withStore(st, "boot-A")) // whalenurse restarts
	if !h.d.Engine().Suspended("h/p/web/1") {
		t.Fatal("SUSPENDED was lost")
	}
	if paused, _ := h.d.Engine().Paused("h/p/other/1"); !paused {
		t.Fatal("the pause was lost")
	}
	h.set(b.ID, unhealthy)
	h.advance(20 * time.Minute)
	h.wantCalls("restart " + a.ID) // still only the one restart from before: the ladder did not start over
	var web engine.KeyStatus
	for _, k := range h.d.Engine().Status().Keys {
		if k.Key == "h/p/web/1" {
			web = k
		}
	}
	if web.State != "SUSPENDED" || web.Flaps != 1 || web.SuspendReason != engine.ReasonLadder {
		t.Fatalf("%+v", web)
	}
	hist, err := st.History("")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 0 {
		t.Fatalf("an incident that is still open is not history yet: %+v", hist)
	}
}

// In a container the default host ID is the hostname, which is the container's ID: an upgrade recreates whalenurse and
// changes it. The persisted state is re-keyed to the new host ID at startup (plan v3.13 item 12), so a SUSPENDED container
// stays SUSPENDED and its ladder does not start over.
func TestStateSurvivesARecreateOfWhalenurse(t *testing.T) {
	st := openStore(t)
	h := newHarnessOpts(t, nil, nil, nil)
	h.start(h.withStore(st, "boot-A"))
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.set(a.ID, unhealthy)
	h.advance(5 * time.Minute)
	if !h.d.Engine().Suspended("h/p/web/1") {
		t.Fatalf("setup: %v", h.rec.Types())
	}

	h.cfg.HostID = "0123456789ab" // whalenurse recreated: a new container ID, so a new hostname
	h.start(h.withStore(st, "boot-A"))
	if !h.d.Engine().Suspended("0123456789ab/p/web/1") {
		t.Fatalf("SUSPENDED was lost with the host ID: %v", h.d.Engine().Status().Keys)
	}
	h.advance(20 * time.Minute)
	if !h.d.Engine().Suspended("0123456789ab/p/web/1") {
		t.Fatalf("SUSPENDED was lost with the host ID: %v", h.d.Engine().Status().Keys)
	}
	h.wantCalls("restart " + a.ID) // the ladder did not start over
	for _, k := range h.d.Engine().Status().Keys {
		if strings.HasPrefix(k.Key, "h/") {
			t.Fatalf("a key under the old host ID is still there: %+v", k)
		}
	}
}

// The control socket calls the daemon from HTTP goroutines while the loop runs; -race is the judge.
func TestControlCommandsAreSafeWhileTheLoopRuns(t *testing.T) {
	h := newHarness(t, nil, nil)
	a := h.running(hexID("a1"), "p", "web")
	h.tick()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			h.d.Status()
			h.d.Pause("p", time.Hour)
			h.d.Resume("p")
			h.d.Unsuspend("p")
		}
	}()
	for i := 0; i < 200; i++ {
		h.set(a.ID, unhealthy)
		h.advance(5 * time.Second)
		h.set(a.ID, healthy)
		h.advance(5 * time.Second)
	}
	<-done
}

// Events() returns before the daemon has accepted the subscription, so a daemon that is down makes every retry
// "succeed" for a moment and then fail. Those are not reconnects: no restore window, no sweep, no log spam.
func TestFailedRetriesDuringAnOutageAreNotReconnects(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { c.StartupSettle = 2 * time.Minute }, nil)
	h.d.o.ReconnectBackoff = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan docker.Event, 8)
	sig := make(chan streamSignal, 64)
	go h.d.watchEvents(ctx, out, sig)

	waitFor(t, func() bool { return len(h.fake.Subscriptions()) == 1 })
	h.fake.Emit(docker.Event{Action: "die", ID: "x", Time: time.Now()}) // establishes the stream
	<-out
	if s := <-sig; s.lost {
		t.Fatalf("first signal must be 'connected': %+v", s)
	}
	for i := 0; i < 4; i++ { // the daemon is down: each retry fails at once
		h.fake.BreakStream(fmt.Errorf("connection refused"))
		waitFor(t, func() bool { return len(h.fake.Subscriptions()) == i+2 })
	}
	var lost, connected int
	for done := false; !done; {
		select {
		case s := <-sig:
			if s.lost {
				lost++
			} else {
				connected++
			}
		case <-time.After(200 * time.Millisecond):
			done = true
		}
	}
	if lost != 1 || connected != 0 {
		t.Fatalf("one loss and no reconnects expected, got lost=%d connected=%d", lost, connected)
	}
}

// ---- containers stopped by the host (plan v3.5 §3.3 rule 4) ---------------------------------------------

func TestAnAppThatExitsOneOnSigtermStaysDownAfterAStreamLossShutdown(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { settled(c); c.PollInterval = time.Minute }, nil)
	h.d.StreamConnected() // the first connection
	h.advance(2*time.Minute + time.Second)
	web := h.running(hexID("a1"), "p", "web")
	h.tick()

	// The daemon shuts down: the app gets SIGTERM and exits 1 at the end of the shutdown; the stream is lost.
	h.clk.Advance(5 * time.Second)
	finished := h.clk.Now()
	h.set(web.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 1, "no", finished
	})
	h.clk.Advance(10 * time.Second)
	h.d.StreamLost(h.clk.Now())
	h.clk.Advance(30 * time.Second) // the daemon is down, then back
	h.d.StreamConnected()
	h.advance(10 * time.Minute) // the sweep after the settle period, and several polls

	h.wantCalls() // stays down
	if h.count(engine.NotifyOnly) != 1 {
		t.Fatalf("want exactly one notification, got %d: %v", h.count(engine.NotifyOnly), h.rec.Types())
	}
	for _, e := range h.rec.Events() {
		if e.Type == engine.IncidentOpened {
			t.Fatalf("a host-stopped container is not an incident: %+v", e)
		}
	}
}

func TestTheSameAppThatCrashedOutsideAShutdownIsStarted(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { settled(c); c.PollInterval = time.Minute }, nil)
	h.d.StreamConnected()
	h.advance(2*time.Minute + time.Second)
	web := h.running(hexID("a1"), "p", "web")
	h.tick()
	h.clk.Advance(time.Hour)
	// It exited 1 an hour ago, long before any stream loss: a crash (a window is not open).
	h.set(web.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 1, "no", h.clk.Now().Add(-time.Hour)
	})
	h.d.StreamLost(h.clk.Now())
	h.clk.Advance(30 * time.Second)
	h.d.StreamConnected()
	h.advance(5 * time.Minute)
	h.wantCalls("start " + web.ID)
}

func TestPoliciesDockerRestoresAreLeftToItAndHealExitedAlwaysStartsThem(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { settled(c); c.PollInterval = time.Minute }, nil)
	h.d.StreamConnected()
	h.advance(2*time.Minute + time.Second)
	for _, id := range []string{"a1", "b1", "c1"} {
		h.running(hexID(id), "p", "svc-"+id)
	}
	h.running(hexID("d1"), "p", "svc-d1", config.LabelHealExited, "always")
	h.tick()
	h.clk.Advance(5 * time.Second)
	fin := h.clk.Now()
	for id, pol := range map[string]string{"a1": "always", "b1": "unless-stopped", "c1": "on-failure", "d1": "no"} {
		h.set(hexID(id), func(c *docker.Container) {
			c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 1, pol, fin
		})
	}
	h.d.StreamLost(h.clk.Now())
	h.clk.Advance(20 * time.Second)
	h.d.StreamConnected()
	h.advance(5 * time.Minute)
	h.wantCalls("start " + hexID("d1")) // only heal-exited=always; the rest is Docker's restore
}

// The other way in: whalenurse itself was stopped by the shutdown (no stream loss, same boot id). The window
// starts 30 s before last_seen and ends at the restore; the app exited 1 inside it.
func TestHostStoppedWindowFromLastSeenAfterWhalenurseWasKilledToo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		finished time.Duration // relative to last_seen
		want     bool          // started by whalenurse?
	}{
		{"exited 4 s before last_seen (the shutdown)", -4 * time.Second, false},
		{"exited 25 s before last_seen", -25 * time.Second, false},
		{"exited 40 s before last_seen: a crash", -40 * time.Second, true},
		{"exited after last_seen, before the restore", 15 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			h := newHarnessOpts(t, func(c *config.Global) { c.PollInterval = time.Minute }, nil, nil)
			h.start(h.withStore(st, "boot-A"))
			web := h.running(hexID("a1"), "p", "web")
			h.advance(30 * time.Second)
			h.d.heartbeat(true)
			lastSeen := h.clk.Now()
			h.set(web.ID, func(c *docker.Container) {
				c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 1, "no", lastSeen.Add(tc.finished)
			})
			h.clk.Advance(30 * time.Second) // the daemon restarts; whalenurse is restored with the rest
			h.start(h.withStore(st, "boot-A"))
			h.advance(5 * time.Minute)
			if tc.want {
				h.wantCalls("start " + web.ID)
			} else {
				h.wantCalls()
				if h.count(engine.NotifyOnly) != 1 {
					t.Fatalf("want one notification: %v", h.rec.Types())
				}
			}
		})
	}
}

func TestHostStoppedWindowIsCappedWithinOneBootAndOpenAfterAReboot(t *testing.T) {
	for _, tc := range []struct {
		name string
		boot string
		want bool
	}{
		{"same boot, whalenurse down for hours: the crash in the middle is not a shutdown", "boot-A", true},
		{"after a reboot the whole downtime is the shutdown", "boot-B", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			h := newHarnessOpts(t, func(c *config.Global) { c.PollInterval = time.Minute }, nil, nil)
			h.start(h.withStore(st, "boot-A"))
			web := h.running(hexID("a1"), "p", "web")
			h.advance(30 * time.Second)
			h.d.heartbeat(true)
			h.clk.Advance(2 * time.Hour)
			h.set(web.ID, func(c *docker.Container) {
				c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 1, "no", h.clk.Now().Add(-time.Hour)
			})
			h.start(h.withStore(st, tc.boot))
			h.advance(5 * time.Minute)
			if tc.want {
				h.wantCalls("start " + web.ID)
			} else {
				h.wantCalls()
			}
		})
	}
}

// A parent left down by policy leaves its dependents down: no ladder for them (the wait-for-parent path), and the
// parent is not started. (The single notification naming both is M2b.)
func TestDependentsOfAParentLeftDownByPolicyStayDown(t *testing.T) {
	h := newHarness(t, func(c *config.Global) { settled(c); c.PollInterval = time.Minute }, nil)
	h.d.StreamConnected()
	h.advance(2*time.Minute + time.Second)
	parent := h.running(hexID("27"), "p", "parent")
	dep := h.running(hexID("d1"), "p", "dep")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + parent.ID })
	h.tick()
	h.clk.Advance(5 * time.Second)
	fin := h.clk.Now()
	h.set(parent.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 137, "no", fin
	})
	h.set(dep.ID, func(c *docker.Container) {
		c.Status, c.Running, c.ExitCode, c.RestartPolicy, c.FinishedAt = "exited", false, 137, "unless-stopped", fin
		c.Error = "cannot join network namespace of a non running container: container p-parent-1 is exited"
	})
	h.d.StreamLost(h.clk.Now())
	h.clk.Advance(20 * time.Second)
	h.d.StreamConnected()
	h.advance(10 * time.Minute)
	h.wantCalls() // neither is started
}

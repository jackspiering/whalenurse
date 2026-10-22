package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
)

// A stop of whalenurse (SIGTERM: an upgrade, systemctl restart, a reboot) during a heal must neither kill the action
// halfway (a force-recreate killed between its create and its start leaves the service down) nor spend the rung, which
// would SUSPEND the container on the next run instead of finishing the repair.

// advanceUntil ticks like advance until cond holds; it fails the test after max.
func (h *harness) advanceUntil(max time.Duration, cond func() bool) {
	h.t.Helper()
	for elapsed := time.Duration(0); !cond(); elapsed += time.Second {
		if elapsed >= max {
			h.t.Fatalf("not reached in %s; events: %v", max, h.rec.Types())
		}
		h.clk.Advance(time.Second)
		h.d.Tick(h.ctx)
	}
}

func (h *harness) failedResults() int {
	n := 0
	for _, e := range h.rec.Events() {
		if e.Type == engine.ActionResult && e.Attrs["ok"] == "false" {
			n++
		}
	}
	return n
}

// The action itself runs on a context that the stop does not cancel.
func TestShutdownLetsAnInFlightRecreateFinish(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	var actErr error
	h.cf.OnCtx = func(_ string, actx context.Context) {
		cancel() // SIGTERM arrives while compose runs
		time.Sleep(10 * time.Millisecond)
		actErr = actx.Err()
	}
	h.cf.OnRecreate = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	h.tick()
	h.set(web.ID, unhealthy)
	h.advanceUntil(20*time.Minute, func() bool { return h.cf.Count("recreate") == 1 })
	if actErr != nil {
		t.Fatalf("the stop cancelled the recreate in flight: %v", actErr)
	}
	if h.failedResults() != 0 {
		t.Fatalf("a failed result: %v", h.rec.Types())
	}
}

func TestShutdownLetsAnInFlightRestartFinish(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	var actErr error
	h.fake.OnCtx = func(_ string, actx context.Context) {
		cancel()
		time.Sleep(10 * time.Millisecond)
		actErr = actx.Err()
	}
	h.tick()
	h.set(web.ID, unhealthy)
	h.advanceUntil(5*time.Minute, func() bool { return len(h.fake.Calls()) == 1 })
	if actErr != nil {
		t.Fatalf("the stop cancelled the restart in flight: %v", actErr)
	}
}

// When the action is cut short anyway (it outlived the shutdown grace and was stopped), the rung is not spent and nothing
// says it failed: the next run does it again.
func TestShutdownDuringRecreateDoesNotSpendTheRung(t *testing.T) {
	st := openStore(t)
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	h.restartWithStore(st)
	web := h.svc(hexID("a1"), "p", "web")
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	h.cf.OnRecreate = func(compose.Project) error {
		cancel()
		return errors.New("docker compose up -d --force-recreate ...: signal: terminated")
	}
	h.tick()
	h.set(web.ID, unhealthy)
	h.advanceUntil(20*time.Minute, func() bool { return h.cf.Count("recreate") == 1 })
	if n := h.failedResults(); n != 0 || h.count(engine.ActionInterrupted) != 1 {
		t.Fatalf("an interrupted recreate was reported as failed: %v", h.rec.Types())
	}

	// The next run, on the same store, with the container still unhealthy.
	h.ctx = context.Background()
	h.cf.OnRecreate = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	h.restartWithStore(st)
	h.advanceUntil(20*time.Minute, func() bool { return h.cf.Count("recreate") == 2 })
	if h.count(engine.Suspended) != 0 {
		t.Fatalf("SUSPENDED instead of recreated again: %v", h.rec.Types())
	}
}

// The pull itself stays cancellable (killing a pull changes no container), and an interrupted pull does not spend the
// rung either.
func TestShutdownDuringPullStopsItWithoutSpendingTheRung(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	var pullErr error
	h.cf.OnCtx = func(verb string, pctx context.Context) {
		if verb == "pull" {
			cancel()
			pullErr = pctx.Err()
		}
	}
	h.cf.OnPull = func(compose.Project) error { return errors.New("compose pull: context canceled") }
	h.tick()
	h.set(web.ID, unhealthy)
	h.advanceUntil(40*time.Minute, func() bool { return h.cf.Count("pull") == 1 })
	if pullErr == nil {
		t.Fatal("the pull was not cancelled by the stop")
	}
	if h.failedResults() != 0 || h.count(engine.Suspended) != 0 || h.count(engine.ActionInterrupted) != 1 {
		t.Fatalf("an interrupted pull was counted: %v", h.rec.Types())
	}
}

// A pull that moved the image is followed by its up even when the stop comes in between: without it, the next run's pull
// would find "no newer image" and SUSPEND a container that was never recreated onto it.
func TestShutdownAfterAPullThatMovedTheImageStillRunsTheUp(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	var upErr error
	h.cf.OnPull = func(compose.Project) error {
		h.fake.SetImage("web:1", imgNewer)
		cancel() // the stop arrives as the pull ends
		return nil
	}
	h.cf.OnCtx = func(verb string, actx context.Context) {
		if verb == "up" {
			upErr = actx.Err()
		}
	}
	h.cf.OnUp = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	h.tick()
	h.set(web.ID, unhealthy)
	h.advanceUntil(40*time.Minute, func() bool { return h.cf.Count("pull") == 1 })
	if h.cf.Count("up") != 1 || upErr != nil {
		t.Fatalf("the up after the pull: %d calls, ctx %v (%s)", h.cf.Count("up"), upErr, h.verbs())
	}
	if h.count(engine.TagMoved) != 1 || h.failedResults() != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
}

// The wait is bounded: an action still running stopGrace after the stop is cancelled, and counts as interrupted.
func TestShutdownGraceEndsAnActionThatDoesNotFinish(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	h.d.stopGrace = 50 * time.Millisecond
	web := h.svc(hexID("a1"), "p", "web")
	ctx, cancel := context.WithCancel(context.Background())
	h.ctx = ctx
	h.fake.OnCtx = func(_ string, actx context.Context) {
		cancel()
		select {
		case <-actx.Done():
		case <-time.After(5 * time.Second):
			t.Error("the action was not cancelled after the grace")
		}
	}
	h.fake.FailMutations = true // what a cancelled call returns
	h.tick()
	h.set(web.ID, unhealthy)
	h.advanceUntil(5*time.Minute, func() bool { return len(h.fake.Calls()) == 1 })
	if h.count(engine.ActionInterrupted) != 1 || h.failedResults() != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
}

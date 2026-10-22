package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
)

// M4: rung 3, the opt-in pull, against a fake Docker and a fake compose. Nothing here runs a process or touches a registry.

const imgNewer = "sha256:newnewnewnewnew0000000000000000000000000000000000000000000000000000"

// pullStack is a unhealthy web container (opted in to the pull) that neither a restart nor a recreate repairs: the
// recreate replaces it with another unhealthy one, so the ladder reaches rung 3.
func pullStack(t *testing.T, tweak func(*config.Global), server docker.ServerInfo, extra ...string) (*cfH, docker.Container) {
	t.Helper()
	h := newCompose(t, tweak, nil, server)
	web := h.svc(hexID("a1"), "p", "web", append([]string{config.LabelHealPull, "true"}, extra...)...)
	h.cf.OnRecreate = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "unhealthy" })
		return nil
	}
	return h, web
}

func breakAndRun(h *cfH, id string, d time.Duration) {
	h.tick()
	h.set(id, unhealthy)
	h.advance(d)
}

// ---- the label --------------------------------------------------------------------------------------------------

// Without whalenurse.heal.pull=true there is never a pull: the ladder is restart, recreate, SUSPENDED, whatever the
// image registry has.
func TestNoPullWithoutTheLabel(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	web := h.svc(hexID("a1"), "p", "web")
	h.cf.OnRecreate = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "unhealthy" })
		return nil
	}
	h.cf.OnPull = func(compose.Project) error { h.fake.SetImage("web:1", imgNewer); return nil }
	breakAndRun(h, web.ID, 30*time.Minute)

	if h.cf.Count("pull") != 0 {
		t.Fatalf("a pull without the label: %s", h.verbs())
	}
	if h.count(engine.ActionStarted) != 2 || h.count(engine.Suspended) != 1 || h.count(engine.TagMoved) != 0 {
		t.Fatalf("restart, recreate, SUSPENDED: %v", h.rec.Types())
	}
}

// Only a container that is not repaired by the earlier rungs gets to the pull.
func TestPullIsNotTriedWhenTheRestartHelps(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	h.tick()
	h.set(web.ID, unhealthy)
	h.advance(100 * time.Second) // the poll, GRACE, then the restart
	h.wantCalls("restart " + web.ID)
	h.set(web.ID, healthy) // what the restart did
	h.advance(15 * time.Minute)
	if h.cf.Count("pull") != 0 || h.cf.Count("recreate") != 0 || h.count(engine.Recovered) != 1 {
		t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
	}
}

// ---- no newer image ---------------------------------------------------------------------------------------------

// The digest is unchanged: nothing is recreated, the container is SUSPENDED at once, and the message says why.
func TestPullFindsNoNewerImageAndSuspends(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	breakAndRun(h, web.ID, 30*time.Minute)

	// Rung 2 and rung 3 each run the guard first (the oracle); the pull is last and no up follows it.
	if got := h.verbs(); got != "dry-run web, recreate web, policy web, dry-run web, pull web" {
		t.Fatalf("compose calls = %q", got)
	}
	if h.count(engine.TagMoved) != 0 {
		t.Fatal("nothing moved")
	}
	sus := h.events(engine.Suspended)
	if len(sus) != 1 || !strings.Contains(sus[0].Message, "the pull found no newer image for web:1 (still "+shortID(imgLabel("web"))+")") || sus[0].Attrs["reason"] != engine.ReasonLadder {
		t.Fatalf("%+v", sus)
	}
	var pulled engine.Event
	for _, e := range h.events(engine.ActionResult) {
		if e.Attrs["action"] == "pull" {
			pulled = e
		}
	}
	if pulled.Attrs["outcome"] != "unchanged" || pulled.Attrs["rung"] != "pull" || pulled.Attrs["ok"] != "true" {
		t.Fatalf("%+v", pulled)
	}
	// SUSPENDED the moment the pull said so, not after another cooldown.
	if !sus[0].Time.Equal(pulled.Time) {
		t.Fatalf("suspended at %v, pull ended at %v", sus[0].Time, pulled.Time)
	}
	// And one pass per incident: no second pull however long it stays unhealthy.
	h.advance(30 * time.Minute)
	if h.cf.Count("pull") != 1 {
		t.Fatalf("%s", h.verbs())
	}
}

// ---- a newer image ----------------------------------------------------------------------------------------------

// The image changed: up (non-forced, --pull never), the container is a new one, TagMoved says old -> new with the exact
// rollback, and the sidecar that shares the parent's network namespace is recreated against the new parent.
func TestPullUpgradesRecreatesAndRepairsTheSidecar(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	dep := h.svc(hexID("d1"), "p", "dep", compose.LabelDependsOn, "web:service_started:true")
	h.set(dep.ID, func(c *docker.Container) { c.NetworkMode = "container:" + web.ID })

	newWeb := web
	h.cf.OnRecreate = func(compose.Project) error { // rung 2: replaced, still unhealthy
		newWeb = h.replace("p", "web", func(c *docker.Container) { c.Health = "unhealthy" })
		return nil
	}
	h.cf.OnPull = func(compose.Project) error { h.fake.SetImage("web:1", imgNewer); return nil }
	h.cf.OnUp = func(p compose.Project) error {
		switch p.Service {
		case "web":
			newWeb = h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		case "dep":
			h.replace("p", "dep", func(c *docker.Container) { c.NetworkMode = "container:" + newWeb.ID })
		}
		return nil
	}
	breakAndRun(h, web.ID, 40*time.Minute)

	verbs := h.verbs()
	for _, want := range []string{"pull web, up web"} {
		if !strings.Contains(verbs, want) {
			t.Fatalf("compose calls = %q, want ... %q", verbs, want)
		}
	}
	if h.cf.Count("pull") != 1 {
		t.Fatalf("%s", verbs)
	}
	// Drift checks before the pull: the second oracle run comes before the pull, and nothing runs after the up but the
	// dependents step.
	if i, j := strings.LastIndex(verbs, "dry-run web"), strings.Index(verbs, "pull web"); i > j || i < 0 {
		t.Fatalf("the drift check must come before the pull: %q", verbs)
	}
	moved := h.events(engine.TagMoved)
	if len(moved) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
	m := moved[0].Attrs
	if m["ref"] != "web:1" || m["old"] != imgLabel("web") || m["new"] != imgNewer || m["ok"] != "true" || m["old_image"] != imgLabel("web") {
		t.Fatalf("%+v", m)
	}
	if want := "docker image tag " + imgLabel("web") + " web:1"; m["rollback_tag"] != want {
		t.Fatalf("rollback_tag = %q", m["rollback_tag"])
	}
	if want := "docker compose -p p --project-directory /opt/stacks/p -f /opt/stacks/p/compose.yaml up -d --no-deps --pull never --no-build web"; m["rollback_up"] != want {
		t.Fatalf("rollback_up = %q", m["rollback_up"])
	}
	// The container really was replaced, the sidecar follows the new parent, and the heal ended in Recovered.
	now := h.current("p", "web")
	if now.ID == newWeb.ID && now.ID == web.ID {
		t.Fatal("not replaced")
	}
	d2 := h.current("p", "dep")
	if d2.ID == dep.ID || d2.NetworkMode != "container:"+h.current("p", "web").ID || !d2.Running {
		t.Fatalf("the sidecar must be recreated against the new parent: %+v", d2)
	}
	for _, c := range h.fake.Calls() {
		if c.ID == dep.ID {
			t.Fatalf("API call on a dependent whose parent was gone: %+v", c)
		}
	}
	recovered := 0
	for _, e := range h.events(engine.Recovered) {
		if e.Key == key("p", "web") {
			recovered++
		}
	}
	if recovered != 1 || h.count(engine.Suspended) != 0 {
		t.Fatalf("%v", h.rec.Types())
	}
	var res engine.Event
	for _, e := range h.events(engine.ActionResult) {
		if e.Attrs["action"] == "pull" {
			res = e
		}
	}
	if res.Attrs["outcome"] != "upgraded" || res.Attrs["ok"] != "true" {
		t.Fatalf("%+v", res)
	}
	if h.count(engine.UpgradePostCheck) != 0 {
		t.Fatal("compose recreated it by itself; no post-check recreate expected")
	}
}

// If compose leaves the old container in place after the non-forced up, it is recreated with --force-recreate: "upgraded"
// is only said for a container that was really replaced.
func TestPullForceRecreatesWhenTheUpDidNothing(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	h.cf.OnPull = func(compose.Project) error { h.fake.SetImage("web:1", imgNewer); return nil }
	ups, recreates := 0, 0
	h.cf.OnUp = func(compose.Project) error { ups++; return nil } // nothing happens
	prev := h.cf.OnRecreate
	h.cf.OnRecreate = func(p compose.Project) error {
		recreates++
		if recreates == 1 {
			return prev(p)
		}
		h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	breakAndRun(h, web.ID, 40*time.Minute)

	if ups != 1 || recreates != 2 {
		t.Fatalf("one up, then one forced recreate: ups=%d recreates=%d (%s)", ups, recreates, h.verbs())
	}
	if h.count(engine.UpgradePostCheck) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
	if m := h.events(engine.TagMoved); len(m) != 1 || m[0].Attrs["ok"] != "true" {
		t.Fatalf("%+v", m)
	}
}

// The tag moved even though the up failed: the move is recorded, the event says the recreate failed, and the rollback is
// still given. The container is not announced as upgraded.
func TestPullThatMovedTheTagButFailedToRecreateStillRecordsTheMove(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	h.cf.OnPull = func(compose.Project) error { h.fake.SetImage("web:1", imgNewer); return nil }
	h.cf.OnUp = func(compose.Project) error { return errors.New("port is already allocated") }
	breakAndRun(h, web.ID, 40*time.Minute)

	moved := h.events(engine.TagMoved)
	if len(moved) != 1 || moved[0].Attrs["ok"] != "false" || !strings.Contains(moved[0].Attrs["error"], "port is already allocated") || moved[0].Attrs["rollback_tag"] == "" {
		t.Fatalf("%+v", moved)
	}
	if _, ok := h.d.moveOf("web:1"); !ok {
		t.Fatal("the move must be recorded although the up failed")
	}
	var res engine.Event
	for _, e := range h.events(engine.ActionResult) {
		if e.Attrs["action"] == "pull" {
			res = e
		}
	}
	if res.Attrs["outcome"] != "upgrade-failed" || res.Attrs["ok"] != "false" {
		t.Fatalf("%+v", res)
	}
	if h.count(engine.Suspended) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
}

// A pull that fails (no network, no such image) is an ordinary failed action: no up, no TagMoved, and the ladder ends in
// SUSPENDED after the cooldown.
func TestPullFailureIsAnOrdinaryFailedAction(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	h.cf.OnPull = func(compose.Project) error { return errors.New("pull access denied for web") }
	breakAndRun(h, web.ID, 30*time.Minute)
	if h.cf.Count("up") != 0 || h.count(engine.TagMoved) != 0 || h.count(engine.Suspended) != 1 {
		t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
	}
	var res engine.Event
	for _, e := range h.events(engine.ActionResult) {
		if e.Attrs["action"] == "pull" {
			res = e
		}
	}
	if res.Attrs["ok"] != "false" || !strings.Contains(res.Message, "pull access denied") || res.Attrs["outcome"] != "" {
		t.Fatalf("%+v", res)
	}
}

// ---- the drift checks before the pull ---------------------------------------------------------------------------

// Checks 1 and 3 run BEFORE the pull. A compose file that was edited since the recreate blocks the pull (it would deploy
// the edit), and nothing is pulled.
func TestDriftCheckBeforeThePullBlocksIt(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	calls := 0
	h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) {
		calls++
		if calls == 1 { // rung 2's guard: clean
			return compose.Oracle{Lines: []compose.Line{{ID: "Container " + name, Status: "Done", Text: "Running"}}}, nil
		}
		return compose.Recreated(name), nil // the file was edited after the recreate
	}
	h.cf.HashFn = func(compose.Project) (string, error) { return "h-edited", nil }
	breakAndRun(h, web.ID, 30*time.Minute)

	if h.cf.Count("pull") != 0 || h.cf.Count("up") != 0 {
		t.Fatalf("a blocked pull must not run: %s", h.verbs())
	}
	b := h.events(engine.DriftBlocked)
	if len(b) != 1 || !strings.Contains(b[0].Message, "config:") {
		t.Fatalf("%+v", b)
	}
	if h.count(engine.Suspended) != 1 {
		t.Fatalf("%v", h.rec.Types())
	}
	for _, e := range h.events(engine.Suspended) {
		if e.Attrs["reason"] != engine.ReasonDrift {
			t.Fatalf("%+v", e)
		}
	}
}

// A tag that moved under the container (another stack pulled it after our recreate) is not what check 1 is about, because
// the pull is meant to move it: the config hash still matches, so the pull goes ahead.
func TestPullAllowsATagThatMovedUnderTheContainer(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{})
	calls := 0
	h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) {
		calls++
		if calls == 1 {
			return compose.Oracle{Lines: []compose.Line{{ID: "Container " + name, Status: "Done", Text: "Running"}}}, nil
		}
		return compose.Recreated(name), nil // the tag moved: compose would recreate
	}
	h.cf.HashFn = func(compose.Project) (string, error) { return "h-web", nil } // the config is what it was
	prev := h.cf.OnRecreate
	h.cf.OnRecreate = func(p compose.Project) error {
		err := prev(p)
		h.fake.SetImage("web:1", "sha256:movedbyanother00000000000000000000000000000000000000000000000000")
		return err
	}
	h.cf.OnPull = func(compose.Project) error { h.fake.SetImage("web:1", imgNewer); return nil }
	h.cf.OnUp = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	breakAndRun(h, web.ID, 40*time.Minute)
	if h.cf.Count("pull") != 1 || h.cf.Count("up") != 1 || h.count(engine.DriftBlocked) != 0 || h.count(engine.TagMoved) != 1 {
		t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
	}
}

// ---- a shared tag -----------------------------------------------------------------------------------------------

// shared builds a second stack whose service runs the same image reference as the pulled one (an image of the same tag
// and the same ID as web had), unhealthy and past rung 1, so the next recreate runs its drift guard.
func (h *cfH) sharedAPI() docker.Container {
	api := h.svc(hexID("b1"), "other", "api")
	h.set(api.ID, func(c *docker.Container) { c.Image = "web:1" })
	h.set(api.ID, func(c *docker.Container) { c.Labels[compose.LabelImage] = imgLabel("web") })
	api, _ = h.fake.Inspect(h.ctx, api.ID)
	return api
}

// After the pull rung moved a tag, a drift block on another stack that shares the tag names the service that moved it and
// when.
func TestDriftBlockOnASharedTagNamesWhoMovedIt(t *testing.T) {
	meta := memMetaSafe{mu: &sync.Mutex{}, m: map[string]string{}}
	h := newCompose(t, nil, nil, docker.ServerInfo{}, func(o *Options) { o.Meta = meta })
	web := h.svc(hexID("a1"), "p", "web", config.LabelHealPull, "true")
	api := h.sharedAPI()
	h.cf.OnRecreate = func(p compose.Project) error {
		if p.Service == "web" {
			h.replace("p", "web", func(c *docker.Container) { c.Health = "unhealthy" })
		}
		return nil
	}
	h.cf.OnPull = func(compose.Project) error { h.fake.SetImage("web:1", imgNewer); return nil }
	h.cf.OnUp = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "healthy" })
		return nil
	}
	h.cf.OracleFn = func(_ compose.Project, name string) (compose.Oracle, error) {
		// compose says Recreate for the container whose image is not what the tag resolves to
		if strings.Contains(name, "other") {
			return compose.Recreated(name), nil
		}
		return compose.Oracle{Lines: []compose.Line{{ID: "Container " + name, Status: "Done", Text: "Running"}}}, nil
	}
	breakAndRun(h, web.ID, 20*time.Minute)
	moved := h.events(engine.TagMoved)
	if len(moved) != 1 {
		t.Fatalf("web should have pulled: %v", h.rec.Types())
	}

	// Now the other stack's container goes bad: restart, then a recreate that the guard blocks, because web:1 moved.
	h.set(api.ID, unhealthy)
	h.advance(10 * time.Minute)
	blocked := h.events(engine.DriftBlocked)
	if len(blocked) != 1 || blocked[0].Key != key("other", "api") {
		t.Fatalf("%+v", blocked)
	}
	b := blocked[0]
	if b.Attrs["moved_by"] != key("p", "web") || b.Attrs["moved_at"] == "" || b.Attrs["moved_ref"] != "web:1" {
		t.Fatalf("the block must say who moved the tag and when: %+v", b.Attrs)
	}
	if !strings.Contains(b.Message, "whalenurse moved this tag: "+key("p", "web")+" pulled it at ") || !strings.Contains(b.Message, "the tag moved") {
		t.Fatalf("%s", b.Message)
	}

	// It survives a restart of whalenurse (the store's meta): a second daemon on the same store still knows.
	h.start(func(o *Options) {
		o.Meta, o.Compose, o.ComposeActor, o.Stat = meta, h.cf, h.cf, okStat
	})
	if m, ok := h.d.moverOf(h.ctx, api); !ok || m.Key != key("p", "web") {
		t.Fatalf("%+v %v", m, ok)
	}
}

// The mover is only named while the tag still points where its pull put it: if someone moved it again, the earlier pull
// is not to blame, and nothing is claimed.
func TestMoverIsNotNamedOnceTheTagMovedAgain(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	api := h.sharedAPI()
	h.d.recordMove(tagMove{Ref: normalizeRef("web:1"), Key: key("p", "web"), At: h.clk.Now(), Old: imgLabel("web"), New: imgNewer, OldPlain: imgLabel("web"), NewPlain: imgNewer})
	h.fake.SetImage("web:1", imgNewer)
	if _, ok := h.d.moverOf(h.ctx, api); !ok {
		t.Fatal("the tag is where the pull put it: the mover must be named")
	}
	h.fake.SetImage("web:1", "sha256:someoneelse00000000000000000000000000000000000000000000000000000")
	if m, ok := h.d.moverOf(h.ctx, api); ok {
		t.Fatalf("the tag moved again: nothing may be claimed about the earlier pull: %+v", m)
	}
}

func TestNormalizeRef(t *testing.T) {
	same := [][]string{
		{"nginx", "nginx:latest", "docker.io/library/nginx:latest", "library/nginx", "index.docker.io/library/nginx", "docker.io/nginx"},
		{"ghcr.io/o/app", "ghcr.io/o/app:latest"},
		{"127.0.0.1:5000/whalenurse-test/app:1"},
	}
	for _, group := range same {
		for _, r := range group {
			if normalizeRef(r) != normalizeRef(group[0]) {
				t.Errorf("%q and %q should be the same reference: %q vs %q", r, group[0], normalizeRef(r), normalizeRef(group[0]))
			}
		}
	}
	for _, pair := range [][2]string{{"nginx:1", "nginx"}, {"ghcr.io/o/nginx", "nginx"}, {"127.0.0.1:5000/a:1", "127.0.0.1:5001/a:1"}, {"a/b", "b"}} {
		if normalizeRef(pair[0]) == normalizeRef(pair[1]) {
			t.Errorf("%q and %q are different images", pair[0], pair[1])
		}
	}
	if got := normalizeRef("app@sha256:abc"); got != "app@sha256:abc" {
		t.Errorf("a digest reference is kept: %q", got)
	}
	if got := normalizeRef("127.0.0.1:5000/a"); got != "127.0.0.1:5000/a:latest" {
		t.Errorf("a registry port is not a tag: %q", got)
	}
}

// ---- dry-run and the version gate ------------------------------------------------------------------------------

// Dry-run never pulls: the guard (which only reads) runs, the pull goes to the no-op actor, and no image is compared,
// recorded or announced.
func TestDryRunNeverPulls(t *testing.T) {
	h, web := pullStack(t, func(c *config.Global) { c.DryRun = true }, docker.ServerInfo{})
	h.cf.OnPull = func(compose.Project) error { t.Error("the compose Fake was asked to pull in dry-run"); return nil }
	var would []string
	h.start(func(o *Options) {
		o.Compose, o.Stat = h.cf, okStat
		o.Actor = docker.DryRun{}
		o.ComposeActor = compose.DryRun{Would: func(v string, p compose.Project) { would = append(would, v+" "+p.Service) }}
	})
	breakAndRun(h, web.ID, 30*time.Minute)
	if h.cf.Count("pull") != 0 || h.cf.Count("recreate") != 0 || h.cf.Count("up") != 0 {
		t.Fatalf("%s", h.verbs())
	}
	if fmt.Sprint(would) != "[recreate web pull web]" {
		t.Fatalf("would = %v", would)
	}
	h.wantCalls() // no restart either
	var pulls []engine.Event
	for _, e := range h.events(engine.ActionResult) {
		if e.Attrs["action"] == "pull" {
			pulls = append(pulls, e)
		}
	}
	if len(pulls) != 1 || pulls[0].Attrs["dry_run"] != "true" || pulls[0].Attrs["outcome"] != "" {
		t.Fatalf("%+v", pulls)
	}
	if h.count(engine.TagMoved) != 0 {
		t.Fatal("a dry run moves nothing")
	}
	if _, ok := h.d.moveOf("web:1"); ok {
		t.Fatal("a dry run records no move")
	}
}

// The version gate disables rungs 2 and 3 together: the ladder ends after the restart, and compose is never asked to pull.
func TestVersionGateDisablesThePullRung(t *testing.T) {
	h, web := pullStack(t, nil, docker.ServerInfo{Version: "28.0.4", Arch: "amd64"})
	h.cf.OnPull = func(compose.Project) error { t.Error("pull ran below the version gate"); return nil }
	breakAndRun(h, web.ID, 30*time.Minute)
	if len(h.cf.Calls()) != 0 {
		t.Fatalf("compose must not run at all: %s", h.verbs())
	}
	var rungs []string
	for _, e := range h.events(engine.RungUnavailable) {
		rungs = append(rungs, e.Attrs["rung"])
	}
	if strings.Join(rungs, ",") != "recreate,pull" {
		t.Fatalf("rungs unavailable = %v", rungs)
	}
	if h.count(engine.ActionStarted) != 1 || h.count(engine.Suspended) != 1 {
		t.Fatalf("restart, then SUSPENDED: %v", h.rec.Types())
	}
}

// A container that compose did not make cannot be pulled: its ladder has no rung 3.
func TestNonComposeContainerHasNoPullRung(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	c := docker.Container{ID: hexID("f1"), Name: "loner", Labels: map[string]string{config.LabelEnable: "true", config.LabelHealPull: "true"},
		Status: "running", Running: true, Health: "unhealthy", Image: "loner:1", StartedAt: h.clk.Now().Add(-time.Hour), Created: h.clk.Now().Add(-time.Hour), NetworkMode: "bridge"}
	h.fake.Put(c)
	h.tick()
	h.advance(30 * time.Minute)
	if len(h.cf.Calls()) != 0 || h.count(engine.Suspended) != 1 {
		t.Fatalf("%s / %v", h.verbs(), h.rec.Types())
	}
}

// ---- health while a long action runs ----------------------------------------------------------------------------

// The loop is synchronous: while a pull runs there are no ticks. /healthz (and with it the uptime ping) must not call the
// daemon dead for that, but must still do so once the action is far overdue.
func TestHealthyToleratesAnActionInFlight(t *testing.T) {
	h := newCompose(t, nil, nil, docker.ServerInfo{})
	h.tick()
	h.d.setEvents(true)
	h.clk.Advance(8 * time.Minute) // far past 3 polls + the action timeout
	if ok, _ := h.d.Healthy(); ok {
		t.Fatal("a silent loop with no action in flight is unhealthy")
	}
	h.d.acting.Store(h.clk.Now().Add(-7 * time.Minute).UnixNano())
	if ok, why := h.d.Healthy(); !ok {
		t.Fatalf("an action that started 7 minutes ago is not a dead loop: %s", why)
	}
	h.d.acting.Store(h.clk.Now().Add(-12 * time.Minute).UnixNano())
	if ok, _ := h.d.Healthy(); ok {
		t.Fatal("an action stuck for 12 minutes is a dead loop")
	}
}

// ---- last_seen during a long action -------------------------------------------------------------------------------

// The loop does not tick while a pull runs, so last_seen would go stale, and a daemon restart in the middle of a long pull
// would leave the containers it stopped outside the shutdown window (5 minutes after last_seen): a `restart: "no"` app
// that exits 1 on SIGTERM would then be started (plan §3.3 rule 4). A goroutine keeps last_seen current while an action
// runs.
func TestLastSeenAdvancesWhileAPullIsBlocked(t *testing.T) {
	meta := memMetaSafe{mu: &sync.Mutex{}, m: map[string]string{}}
	h := newCompose(t, nil, nil, docker.ServerInfo{}, func(o *Options) { o.Meta = meta })
	h.d.beatEvery = 5 * time.Millisecond
	web := h.svc(hexID("a1"), "p", "web", config.LabelHealPull, "true")
	h.cf.OnRecreate = func(compose.Project) error {
		h.replace("p", "web", func(c *docker.Container) { c.Health = "unhealthy" })
		return nil
	}
	started, release := make(chan struct{}), make(chan struct{})
	var seenDuring [2]string
	h.cf.OnPull = func(compose.Project) error {
		close(started)
		seenDuring[0], _ = meta.Meta(MetaLastSeen)
		h.clk.Advance(8 * time.Minute) // the pull takes a long time
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if v, _ := meta.Meta(MetaLastSeen); v != seenDuring[0] {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		seenDuring[1], _ = meta.Meta(MetaLastSeen)
		<-release
		return nil
	}
	close(release)
	breakAndRun(h, web.ID, 30*time.Minute)
	<-started
	if seenDuring[0] == "" || seenDuring[1] == seenDuring[0] {
		t.Fatalf("last_seen did not advance while the pull was blocked: %q -> %q", seenDuring[0], seenDuring[1])
	}
	want := h.clk.Now().Add(-time.Hour).UnixNano() // the advanced clock is what it wrote, far past the first value
	var a, b int64
	fmt.Sscan(seenDuring[0], &a)
	fmt.Sscan(seenDuring[1], &b)
	if b-a < int64(7*time.Minute) || b < want {
		t.Fatalf("last_seen = %d then %d: it should follow the clock", a, b)
	}
}

type memMetaSafe struct {
	mu *sync.Mutex
	m  map[string]string
}

func (m memMetaSafe) Meta(k string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[k]
	return v, ok
}
func (m memMetaSafe) SetMeta(k, v string) { m.mu.Lock(); defer m.mu.Unlock(); m.m[k] = v }

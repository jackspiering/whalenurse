package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/rules"
)

// composeTimeout bounds one compose recreate: it stops the old container (stop_grace_period) and starts the new one.
const composeTimeout = 5 * time.Minute

// guardTimeout bounds the drift guard's read-only commands.
const guardTimeout = 2 * time.Minute

// depSettle is how long one dependent is given to come back (running, and healthy if it has a healthcheck) before the
// next dependent of the same parent is restarted or recreated (plan §3.5: one at a time, with a health wait).
const depSettle = 90 * time.Second

// depRetry is how often a dependent that is waiting for its sibling looks again.
const depRetry = 2 * time.Second

// composeState is the result of the startup version check.
type composeState struct {
	server docker.ServerInfo
	bin    *compose.Binary
	gate   compose.Gate
	unread bool // the Engine version has never been read: the gate is no verdict yet, and each poll tries again
}

// ComposeStatus is the compose part of `whalenurse status`.
type ComposeStatus struct {
	Binary     string   `json:"binary,omitempty"`
	Source     string   `json:"source,omitempty"`
	Version    string   `json:"version,omitempty"`
	Engine     string   `json:"engine,omitempty"`
	EngineArch string   `json:"engine_arch,omitempty"`
	Rungs      bool     `json:"recreate_enabled"` // rungs 2 and 3, the drift guard and the dependents recreate
	Disabled   []string `json:"disabled_rungs,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	DriftGuard string   `json:"drift_guard"`
}

func (d *Daemon) cs() composeState {
	if p := d.compState.Load(); p != nil {
		return *p
	}
	return composeState{gate: compose.Gate{Reason: "the startup version check has not run"}}
}

// CheckVersions is the startup version gate (plan §3.6, *Minimum versions per rung*): rungs 2 and 3 and the drift guard
// need Engine 28.1 or later and Compose v5.2.0 or later. Below that, or when a version cannot be read, or when there is no
// compose binary at all, it logs one warning, disables every use of compose, and the ladder ends with notify and
// SUSPENDED after rung 1. It is called at start, before the startup sweep and before the sweep after the events stream
// came back, because the Engine may have been upgraded in between, and at each poll until a first read succeeds.
func (d *Daemon) CheckVersions(ctx context.Context) {
	prev := d.cs()
	vctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	info, err := d.rd.ServerVersion(vctx)
	var bin *compose.Binary
	if d.o.Compose != nil {
		b := d.o.Compose.Binary()
		bin = &b
	}
	st := composeState{server: info, bin: bin}
	if err != nil {
		if d.compState.Load() != nil {
			return // keep what was known (or that nothing is); the next check tries again
		}
		st.gate = compose.Gate{Reason: "cannot read the Docker Engine version: " + err.Error()}
		st.unread = true
	} else {
		st.gate = compose.CheckVersions(info.Version, bin)
	}
	if bin == nil && d.o.ComposeNote != "" {
		st.gate.Reason = st.gate.Reason + "; " + d.o.ComposeNote
	}
	if st.gate.Recreate && d.o.ComposeActor == nil {
		st.gate = compose.Gate{Reason: "no compose actor is configured"}
	}
	first := d.compState.Load() == nil
	d.compState.Store(&st)
	if first || prev.gate != st.gate {
		if !st.gate.Recreate {
			d.log.Warn("rungs 2 and 3 (recreate, pull) and the drift guard are disabled; the ladder ends with notify and SUSPENDED after rung 1",
				"reason", st.gate.Reason, "engine", info.Version, "compose", composeName(bin), "need", fmt.Sprintf("Engine >= %s and Compose >= v%s", compose.MinEngine, compose.MinCompose))
			d.eng.Note(engine.Target{}, engine.RungsDisabled, "rungs 2 and 3 are disabled: "+st.gate.Reason,
				map[string]string{"rungs": "recreate,pull", "reason": st.gate.Reason})
		} else {
			d.log.Info("compose rungs enabled", "compose", composeName(bin), "engine", info.Version, "arch", info.Arch, "drift_guard", string(d.cfg.DriftGuard))
		}
	}
}

func composeName(b *compose.Binary) string {
	if b == nil {
		return "none"
	}
	return fmt.Sprintf("%s %s (%s)", b.Path, b.VersionString(), b.Source)
}

func (d *Daemon) composeStatus() ComposeStatus {
	cs := d.cs()
	s := ComposeStatus{Engine: cs.server.Version, EngineArch: cs.server.Arch, Rungs: cs.gate.Recreate, Reason: cs.gate.Reason, DriftGuard: string(d.cfg.DriftGuard)}
	if cs.bin != nil {
		s.Binary, s.Source, s.Version = cs.bin.Path, string(cs.bin.Source), cs.bin.VersionString()
	}
	if !cs.gate.Recreate {
		s.Disabled = []string{config.RungRecreate, config.RungPull}
	}
	return s
}

// recreateBlocker says why rung 2 cannot run for this container, or "" when it can. It covers the version gate, a
// container that compose did not make, and compose inputs that are missing or unreadable (plan §3.4 *Fallbacks*).
func (d *Daemon) recreateBlocker(c docker.Container) string {
	cs := d.cs()
	if !cs.gate.Recreate {
		return "the compose rungs are disabled: " + cs.gate.Reason
	}
	if _, _, _, ok := docker.ComposeKeyParts(c.Labels); !ok {
		return "not a compose container: only rung 1 applies"
	}
	p, err := compose.ProjectFromLabels(c.Labels)
	if err != nil {
		return err.Error()
	}
	if err := p.Readable(d.o.Stat); err != nil {
		return err.Error()
	}
	return ""
}

// unavailable builds Obs.Unavailable for a container.
// Rungs 2 and 3 share the version gate and the compose inputs, so they are unavailable together.
func (d *Daemon) unavailable(c docker.Container) map[string]string {
	if why := d.recreateBlocker(c); why != "" {
		return map[string]string{config.RungRecreate: why, config.RungPull: why}
	}
	return nil
}

// holdsReference reports whether network_mode, ipc, pid or volumes_from names another container: compose hashes those
// after resolving them, so check 1 of the drift guard cannot be used for it (plan §3.6, E4).
func holdsReference(c docker.Container) bool {
	if docker.ContainerRef(c.NetworkMode) != "" || docker.ContainerRef(c.IpcMode) != "" || docker.ContainerRef(c.PidMode) != "" {
		return true
	}
	return len(c.VolumesFrom) > 0
}

// refsRefusal re-checks every container: reference just before an API restart or a forced recreate. The parent can be
// recreated between the decision and the action, and an API restart of a dependent whose parent ID is gone turns a
// running dependent into an exited one (E1). It returns why the action must not go ahead, or "".
func (d *Daemon) refsRefusal(ctx context.Context, c docker.Container) string {
	for _, mode := range []string{c.NetworkMode, c.IpcMode, c.PidMode} {
		ref := docker.ContainerRef(mode)
		if ref == "" {
			continue
		}
		p, err := d.rd.Inspect(ctx, ref)
		switch {
		case errors.Is(err, docker.ErrNotFound):
			return fmt.Sprintf("container:%s no longer exists; it is recreated, not restarted", docker.ShortID(ref))
		case err != nil:
			return fmt.Sprintf("cannot verify container:%s (%v); not acting", docker.ShortID(ref), err)
		case !p.Running:
			return fmt.Sprintf("parent %s is not running; act on the parent only", docker.ShortID(p.ID))
		}
	}
	return ""
}

// staleParentRefusal is the check before a stale dependent is recreated: its old parent is gone (that is why it is
// stale), so the services it depends on must have containers that run, or the new dependent would fail with "cannot join
// network namespace" (plan §3.5, ordering). It reads the project's containers from the last list and the dependency names
// from the compose depends_on label (network_mode: service:X counts as a dependency on X).
func (d *Daemon) staleParentRefusal(c docker.Container) string {
	project := c.Labels[docker.LabelProject]
	if project == "" {
		return ""
	}
	for _, dep := range d.parentServices(docker.StateKey(d.cfg.HostID, c.Name, c.Labels), c.Labels) {
		for _, s := range d.idx.all {
			if s.Labels[docker.LabelProject] != project || s.Labels[docker.LabelService] != dep || docker.IsOneOff(s.Labels) {
				continue
			}
			if s.State != "running" {
				return fmt.Sprintf("service %s that it depends on is %s; the parent is healed first", dep, s.State)
			}
			if d.eng.Busy(d.keyOf(s)) {
				return fmt.Sprintf("service %s that it depends on is being healed; waiting for it", dep)
			}
		}
	}
	return ""
}

// dependsOn reads the compose depends_on label: "svc:condition:restart,svc2:...".
func dependsOn(label string) []string {
	var out []string
	for _, e := range strings.Split(label, ",") {
		if name, _, _ := strings.Cut(strings.TrimSpace(e), ":"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// driftFindings runs the drift guard (plan §3.6) for the container about to be recreated and returns what it found as
// text, one entry per finding. It reads only: the dry-run oracle, `config --hash` and an image inspect.
// With pull set an image-only move is allowed: checks 1 and 3 only (compose.GuardInput.AllowImageMove).
func (d *Daemon) driftFindings(ctx context.Context, c docker.Container, p compose.Project, pull bool) []compose.Finding {
	cs := d.cs()
	gctx, cancel := context.WithTimeout(ctx, guardTimeout)
	defer cancel()
	in := compose.GuardInput{
		Project: p, ContainerName: c.Name, Labels: c.Labels, ImageRef: c.Image, Arch: cs.server.Arch, RefHolder: holdsReference(c), AllowImageMove: pull,
	}
	return (compose.Guard{Compose: d.o.Compose, Images: d.rd}).Check(gctx, in)
}

// parentOptedIn reports whether a container: parent of c is opted in. A parent that is gone (that is why a dependent is
// stale) is found through the services named in c's depends_on label.
func (d *Daemon) parentOptedIn(c docker.Container) bool {
	for _, mode := range []string{c.NetworkMode, c.IpcMode, c.PidMode} {
		ref := docker.ContainerRef(mode)
		if ref == "" {
			continue
		}
		found := false
		for _, s := range d.idx.all {
			if docker.MatchesRef(s.ID, s.Name, ref) {
				found = true
				if pc, _ := config.ParseLabels(s.Labels, d.cfg.Mode); pc.OptedIn {
					return true
				}
			}
		}
		if found {
			continue
		}
		project := c.Labels[docker.LabelProject]
		for _, dep := range d.parentServices(docker.StateKey(d.cfg.HostID, c.Name, c.Labels), c.Labels) {
			for _, s := range d.idx.all {
				if s.Labels[docker.LabelProject] != project || s.Labels[docker.LabelService] != dep || docker.IsOneOff(s.Labels) {
					continue
				}
				if pc, _ := config.ParseLabels(s.Labels, d.cfg.Mode); pc.OptedIn {
					return true
				}
			}
		}
	}
	return false
}

// requestDependentSweep asks for the dependents of whatever was just acted on to be looked at at the given time (plan
// §3.5: when the action is whalenurse's own, it runs the dependents step explicitly, because the parent's own events fell
// inside the action's in-flight window and were not interpreted).
func (d *Daemon) requestDependentSweep(at time.Time) {
	if d.sweepAt.IsZero() || at.Before(d.sweepAt) {
		d.sweepAt = at
	}
}

// ---- executing rung 2 and the dependents recreate ---------------------------------------------------

// executeRecreate runs a planned recreate: the pre-action checks, the drift guard, then compose. ActionRecreate is rung 2
// (`up -d --force-recreate --no-deps ...`); ActionRecreateStale is the dependents step after a parent recreate
// (`up -d --no-deps ...` without --force-recreate, then a post-check, and --force-recreate only if it is still stale).
func (d *Daemon) executeRecreate(ctx context.Context, t engine.Target, c docker.Container, dec rules.Decision, plan engine.Plan, cfg config.Container) {
	if why := d.recreateBlocker(c); why != "" {
		d.refuse(t, dec, "cannot recreate: "+why)
		return
	}
	p, err := compose.ProjectFromLabels(c.Labels)
	if err != nil {
		d.refuse(t, dec, "cannot recreate: "+err.Error())
		return
	}
	stale := plan.Act == rules.ActionRecreateStale
	if stale {
		if why := d.staleParentRefusal(c); why != "" {
			d.refuse(t, dec, why)
			return
		}
	} else if why := d.refsRefusal(ctx, c); why != "" {
		d.refuse(t, dec, why)
		return
	}
	if d.siblingBusy(ctx, c, t.Key) {
		d.schedule(t.Key, d.clk.Now().Add(depRetry))
		return
	}

	// The drift guard, before the recreate (plan §3.6). It runs in dry-run too: it only reads.
	// An image-only move is let through for a container that opted in to the pull rung (the heal may take the newer
	// image: config drift still blocks) and for a sidecar taking the tag its parent's own pull just moved (plan v3.8).
	allow := cfg.HasRung(config.RungPull)
	if stale {
		allow = d.dependentMayTakeMove(ctx, c)
	}
	if !d.guardVerdict(ctx, t, c, dec, p, allow) {
		return
	}

	d.eng.Begin(t, plan, dec)
	actx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	var runErr error
	d.markActing()
	if stale {
		runErr = d.o.ComposeActor.Up(actx, p)
		if runErr == nil && !d.cfg.DryRun {
			runErr = d.postCheckStale(actx, t, c, p)
		}
	} else {
		runErr = d.o.ComposeActor.Recreate(actx, p)
		if allow && !d.cfg.DryRun {
			d.noteUpgrade(actx, t, c, p, runErr)
		}
	}
	recheck := d.eng.End(t, plan, engine.Result{Err: runErr, DryRun: d.cfg.DryRun}, cfg)
	d.afterAction(t, c, recheck)
}

// postCheckStale looks at the dependent after the non-forced up: it must exist, run, and point at containers that exist
// and run. If it is still stale, the up did not recreate it (compose decided nothing changed), and it is recreated with
// --force-recreate (plan §3.5).
func (d *Daemon) postCheckStale(ctx context.Context, t engine.Target, old docker.Container, p compose.Project) error {
	if err := d.refresh(ctx); err != nil {
		return nil // the next look at the key decides
	}
	s, ok := d.idx.byKey[t.Key]
	if !ok {
		return nil
	}
	cur, err := d.rd.Inspect(ctx, s.ID)
	if err != nil {
		return nil
	}
	if why := d.staleAfterUp(ctx, cur); why != "" {
		d.eng.Note(t, engine.DependentPostCheck, "still stale after a non-forced up ("+why+"): recreating with --force-recreate",
			map[string]string{"old_id": docker.ShortID(old.ID), "id": docker.ShortID(cur.ID)})
		return d.o.ComposeActor.Recreate(ctx, p)
	}
	return nil
}

// staleAfterUp reports why a dependent that was just brought up is still not pointing at a running parent, or "".
func (d *Daemon) staleAfterUp(ctx context.Context, c docker.Container) string {
	for _, mode := range []string{c.NetworkMode, c.IpcMode, c.PidMode} {
		ref := docker.ContainerRef(mode)
		if ref == "" {
			continue
		}
		p, err := d.rd.Inspect(ctx, ref)
		switch {
		case errors.Is(err, docker.ErrNotFound):
			return "container:" + docker.ShortID(ref) + " is gone"
		case err != nil:
			return ""
		case !p.Running:
			return "parent " + docker.ShortID(ref) + " is not running"
		case !c.StartedAt.IsZero() && p.StartedAt.After(c.StartedAt):
			return "parent " + docker.ShortID(ref) + " started after the dependent"
		}
	}
	if !c.Running {
		return "the dependent is not running"
	}
	return ""
}

// afterAction schedules what follows our own action on a key: the dependents of whatever was just restarted or recreated
// are looked at when the key itself is (plan §3.5), and the one-at-a-time gate for its siblings is armed.
func (d *Daemon) afterAction(t engine.Target, c docker.Container, recheck time.Time) {
	if !recheck.IsZero() {
		d.schedule(t.Key, recheck)
		d.requestDependentSweep(recheck)
	}
	if key := firstRef(c); key != "" {
		d.depGate[key] = depGate{key: t.Key, until: d.clk.Now().Add(depSettle)}
	}
}

// firstRef is the raw container: reference of a dependent: the same string for every sibling that shares the parent.
func firstRef(c docker.Container) string {
	for _, m := range []string{c.NetworkMode, c.IpcMode, c.PidMode} {
		if r := docker.ContainerRef(m); r != "" {
			return r
		}
	}
	return ""
}

type depGate struct {
	key   string
	until time.Time
}

// siblingBusy implements "one at a time, with a health wait": while another dependent of the same parent was restarted or
// recreated a moment ago and has not come back (running, and healthy when it has a healthcheck), this one waits. It is
// a scheduled step, not a sleep, so the loop, the heartbeat and the events keep running.
func (d *Daemon) siblingBusy(ctx context.Context, c docker.Container, key string) bool {
	ref := firstRef(c)
	if ref == "" {
		return false
	}
	g, ok := d.depGate[ref]
	if !ok || g.key == key {
		return false
	}
	if !d.clk.Now().Before(g.until) {
		delete(d.depGate, ref)
		return false
	}
	if s, ok := d.idx.byKey[g.key]; ok {
		if sib, err := d.rd.Inspect(ctx, s.ID); err == nil && sib.Running && (sib.Health == "" || sib.Health == "healthy") {
			delete(d.depGate, ref)
			return false
		}
	} else {
		delete(d.depGate, ref) // it is gone
		return false
	}
	return true
}

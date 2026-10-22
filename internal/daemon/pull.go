package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/engine"
	"github.com/jackspiering/whalenurse/internal/rules"
)

// pullTimeout bounds `compose pull`: an image can be large. The loop is busy for that long (rung 2's recreate is too,
// for up to composeTimeout); Healthy tolerates an action in flight for this long plus a minute.
const pullTimeout = 10 * time.Minute

// metaTagMoved is the prefix of the store keys that remember which container moved which tag.
const metaTagMoved = "tagmoved:"

// tagMove is a record that whalenurse's own pull rung moved a tag: other stacks that share the tag see image drift
// afterwards, and the drift message names who did it and when (plan §3.6, §3.7).
type tagMove struct {
	Ref      string    `json:"ref"`       // the normalized reference
	Key      string    `json:"key"`       // the state key of the container whose pull rung moved it
	At       time.Time `json:"at"`        // when
	Old      string    `json:"old"`       // what the reference resolved to for this platform before ("" if there was no local image)
	New      string    `json:"new"`       // ... and after
	OldPlain string    `json:"old_plain"` // the same without a platform: the index digest on the containerd store
	NewPlain string    `json:"new_plain"`
}

// normalizeRef makes `nginx`, `nginx:latest` and `docker.io/library/nginx:latest` the same reference, so a move recorded
// under one spelling is found under another (plan §3.7). A reference with a digest is kept as it is.
func normalizeRef(ref string) string {
	r := strings.TrimSpace(ref)
	if r == "" || strings.Contains(r, "@") {
		return r
	}
	for _, p := range []string{"index.docker.io/", "registry-1.docker.io/", "docker.io/"} {
		r = strings.TrimPrefix(r, p)
	}
	if rest, ok := strings.CutPrefix(r, "library/"); ok && !strings.Contains(rest, "/") {
		r = rest
	}
	if last := r[strings.LastIndex(r, "/")+1:]; !strings.Contains(last, ":") {
		r += ":latest"
	}
	return r
}

// recordMove remembers the move: in memory, and in the store when there is one, so that it is still known after a restart
// of whalenurse (a drift block on another stack can come days later).
func (d *Daemon) recordMove(m tagMove) {
	if d.moves == nil {
		d.moves = map[string]tagMove{}
	}
	d.moves[m.Ref] = m
	if d.o.Meta != nil {
		if b, err := json.Marshal(m); err == nil {
			d.o.Meta.SetMeta(metaTagMoved+m.Ref, string(b))
		}
	}
}

func (d *Daemon) moveOf(ref string) (tagMove, bool) {
	ref = normalizeRef(ref)
	if m, ok := d.moves[ref]; ok {
		return m, true
	}
	if d.o.Meta != nil {
		if v, ok := d.o.Meta.Meta(metaTagMoved + ref); ok {
			var m tagMove
			if json.Unmarshal([]byte(v), &m) == nil && m.Ref == ref {
				return m, true
			}
		}
	}
	return tagMove{}, false
}

// moverOf says who moved the tag that the container's image reference points at, or returns false. It claims the move only
// while the reference still resolves to what that pull produced: if someone moved the tag again since, nothing is
// said about the earlier pull.
func (d *Daemon) moverOf(ctx context.Context, c docker.Container) (tagMove, bool) {
	m, ok := d.moveOf(c.Image)
	if !ok {
		return tagMove{}, false
	}
	arch := d.cs().server.Arch
	if now, err := d.rd.ImageID(ctx, c.Image, arch); err == nil && m.New != "" && now == m.New {
		return m, true
	}
	if now, err := d.rd.ImageID(ctx, c.Image, ""); err == nil && m.NewPlain != "" && now == m.NewPlain {
		return m, true
	}
	return tagMove{}, false
}

// guardVerdict runs the drift guard (plan §3.6) before a recreate or a pull and says whether the action may go ahead. When it may not, the container is SUSPENDED (Blocked); when the findings were
// verified drift and WHALENURSE_DRIFT_GUARD=warn, the action goes ahead and says so. It runs in dry-run too: it only reads.
func (d *Daemon) guardVerdict(ctx context.Context, t engine.Target, c docker.Container, dec rules.Decision, p compose.Project, allowImageMove bool) bool {
	fs := d.driftFindings(ctx, c, p, allowImageMove)
	if len(fs) == 0 {
		return true
	}
	if ctx.Err() != nil {
		return false // whalenurse is stopping: a check cut short is not a finding, and nothing is blocked or recorded
	}
	var findings []string
	var extra map[string]string
	verifiedOnly := true
	for _, f := range fs {
		text := f.String()
		if f.Check == compose.CheckImage {
			if m, ok := d.moverOf(ctx, c); ok {
				text += fmt.Sprintf(" (whalenurse moved this tag: %s pulled it at %s)", m.Key, m.At.UTC().Format(time.RFC3339))
				extra = map[string]string{"moved_by": m.Key, "moved_at": m.At.UTC().Format(time.RFC3339), "moved_ref": m.Ref}
			}
		}
		findings = append(findings, text)
		verifiedOnly = verifiedOnly && f.Verified()
	}
	if d.cfg.DriftGuard == config.DriftWarn && verifiedOnly {
		d.eng.Warned(t, findings) // warn overrides verified drift only
		return true
	}
	if d.cfg.DriftGuard == config.DriftWarn {
		findings = append(findings, "WHALENURSE_DRIFT_GUARD=warn does not override a check that could not be made")
	}
	d.eng.BlockedWith(t, dec, findings, extra)
	return false
}

// executePull is rung 3 (plan §3.7), carried out in this order and no other:
//
//  1. the pre-action checks, then drift checks 1 and 3 (the guard in pull mode) BEFORE the pull, because after a pull that
//     changed the image compose's dry-run says Recreate, which would block every pull rung;
//  2. `compose pull <svc>`; the image the reference resolves to is read before and after;
//  3. unchanged: the ladder is spent and the container is SUSPENDED (Result.Exhausted), with a message that says so;
//  4. changed: the move is recorded at once (the tag has moved for every stack that shares it, whatever happens next),
//     then `up -d --no-deps --pull never --no-build <svc>`, and a check that the container really is a new one;
//  5. TagMoved (the "upgraded during heal" notification, with the rollback commands), then the dependents step, which
//     afterAction schedules exactly as it does after rung 2.
//
// In dry-run nothing is pulled: the guard runs (it only reads), and the pull goes to compose.DryRun.
func (d *Daemon) executePull(ctx context.Context, t engine.Target, c docker.Container, dec rules.Decision, plan engine.Plan, cfg config.Container) {
	if why := d.recreateBlocker(c); why != "" {
		d.refuse(t, dec, "cannot pull: "+why)
		return
	}
	p, err := compose.ProjectFromLabels(c.Labels)
	if err != nil {
		d.refuse(t, dec, "cannot pull: "+err.Error())
		return
	}
	if c.Image == "" {
		d.refuse(t, dec, "cannot pull: the container has no image reference")
		return
	}
	if why := d.refsRefusal(ctx, c); why != "" {
		d.refuse(t, dec, why)
		return
	}
	if d.siblingBusy(ctx, c, t.Key) {
		d.schedule(t.Key, d.clk.Now().Add(depRetry))
		return
	}
	// The pull rung respects a service's pull_policy never and build (plan v3.8): it does not apply, the ladder is spent.
	if why := d.pullPolicyRefusal(ctx, p); why != "" {
		if ctx.Err() != nil {
			return // the read failed because whalenurse is stopping, not because the policy is unreadable
		}
		d.eng.RungNotApplicable(t, config.RungPull, why, cfg)
		d.schedule(t.Key, d.clk.Now().Add(d.cfg.Grace))
		return
	}
	if !d.guardVerdict(ctx, t, c, dec, p, true) {
		return
	}

	arch := d.cs().server.Arch
	ref := c.Image
	old, oldErr := d.rd.ImageID(ctx, ref, arch)
	if oldErr != nil && !errors.Is(oldErr, docker.ErrImageNotFound) {
		d.refuse(t, dec, fmt.Sprintf("cannot pull: the image %s cannot be read first (%v), so a changed image could not be told from an unchanged one", ref, oldErr))
		return
	}
	oldPlain, _ := d.rd.ImageID(ctx, ref, "")

	// Sidecars of this container that run the same image reference, created from the image that is about to be replaced,
	// are recreated with it (the dependents step may take this move, and only this one): their service names go into the
	// rollback, so the one message puts them all back.
	sidecars := d.sidecarsSharing(ctx, c, ref, old, oldPlain)

	if ctx.Err() != nil {
		return
	}
	d.eng.Begin(t, plan, dec)
	// The pull itself stops with whalenurse (killing a pull changes no container) and is then not counted.
	pctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	d.markActing()
	pullErr := d.o.ComposeActor.Pull(pctx, p)
	if interrupted(ctx, pullErr) {
		d.eng.Interrupt(t, plan, pullErr)
		return
	}
	// Whatever follows a pull that ran is finished even when whalenurse is stopping: a pull that moved the image must be
	// followed by its up, or the next run's pull would find "no newer image" and SUSPEND a container never moved onto it.
	actx, acancel := d.actionContext(ctx, composeTimeout)
	defer acancel()
	if d.cfg.DryRun {
		recheck := d.eng.End(t, plan, engine.Result{DryRun: true}, cfg)
		d.afterAction(t, c, recheck)
		return
	}
	if pullErr != nil {
		recheck := d.eng.End(t, plan, engine.Result{Err: fmt.Errorf("compose pull: %w", pullErr)}, cfg)
		d.afterAction(t, c, recheck)
		return
	}
	now, nowErr := d.rd.ImageID(actx, ref, arch)
	if nowErr != nil {
		recheck := d.eng.End(t, plan, engine.Result{Err: fmt.Errorf("the pull ran, but the image %s cannot be read afterwards: %w", ref, nowErr)}, cfg)
		d.afterAction(t, c, recheck)
		return
	}
	if old != "" && now == old {
		d.eng.End(t, plan, engine.Result{
			Exhausted: fmt.Sprintf("the pull found no newer image for %s (still %s)", ref, shortID(old)),
			Attrs:     map[string]string{"outcome": "unchanged", "ref": ref, "image": old},
		}, cfg)
		return
	}

	// The image changed. Record it first: the tag moved for every stack that shares it, whether or not the up works.
	nowPlain, _ := d.rd.ImageID(actx, ref, "")
	at := d.clk.Now()
	d.recordMove(tagMove{Ref: normalizeRef(ref), Key: t.Key, At: at, Old: old, New: now, OldPlain: oldPlain, NewPlain: nowPlain})

	d.markActing()
	upErr := d.o.ComposeActor.Up(actx, p)
	if upErr == nil {
		upErr = d.postCheckUpgraded(actx, t, c, p)
	}

	attrs := map[string]string{"ref": ref, "old": old, "new": now, "at": at.UTC().Format(time.RFC3339), "ok": fmt.Sprint(upErr == nil)}
	if back := firstNonEmpty(oldPlain, old); back != "" {
		attrs["old_image"] = back
		also := sidecars
		attrs["rollback_tag"], attrs["rollback_up"] = compose.RollbackCommands(p, ref, back, also...)
		if len(also) > 0 {
			attrs["sidecars"] = strings.Join(also, ",")
		}
	}
	outcome, msg := "upgraded", fmt.Sprintf("%s: %s moved to %s; recreated", ref, shortID(old), shortID(now))
	if old == "" {
		msg = fmt.Sprintf("%s: pulled %s (no local copy before); recreated", ref, shortID(now))
	}
	if upErr != nil {
		outcome, msg = "upgrade-failed", strings.Replace(msg, "recreated", "the recreate failed: "+upErr.Error(), 1)
		attrs["error"] = upErr.Error()
	}
	d.eng.Note(t, engine.TagMoved, msg, attrs)
	recheck := d.eng.End(t, plan, engine.Result{Err: upErr, Attrs: map[string]string{"outcome": outcome, "ref": ref, "old": old, "new": now}}, cfg)
	d.afterAction(t, c, recheck)
}

// postCheckUpgraded looks at the container after the non-forced up: it must be a new container (compose recreates on a
// changed image by itself, E5), and it must run. If compose left the old one in place, it is recreated with
// --force-recreate; "upgraded" is only announced for a container that was really replaced.
func (d *Daemon) postCheckUpgraded(ctx context.Context, t engine.Target, old docker.Container, p compose.Project) error {
	cur, err := d.currentOf(ctx, t.Key)
	if err != nil {
		return err
	}
	if cur.ID == old.ID {
		d.eng.Note(t, engine.UpgradePostCheck, "the container is still the old one after a non-forced up: recreating it with --force-recreate",
			map[string]string{"id": docker.ShortID(old.ID)})
		d.markActing()
		if err := d.o.ComposeActor.Recreate(ctx, p); err != nil {
			return err
		}
		if cur, err = d.currentOf(ctx, t.Key); err != nil {
			return err
		}
		if cur.ID == old.ID {
			return errors.New("the container was not replaced even by --force-recreate")
		}
	}
	if !cur.Running {
		return fmt.Errorf("the new container %s is %s, not running", docker.ShortID(cur.ID), cur.Status)
	}
	return nil
}

// currentOf inspects the container that now holds the key.
func (d *Daemon) currentOf(ctx context.Context, key string) (docker.Container, error) {
	if err := d.refresh(ctx); err != nil {
		return docker.Container{}, fmt.Errorf("cannot list containers after the up: %w", err)
	}
	s, ok := d.idx.byKey[key]
	if !ok {
		return docker.Container{}, errors.New("no container for the service after the up")
	}
	return d.rd.Inspect(ctx, s.ID)
}

func shortID(id string) string { return docker.ShortID(id) }

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// markActing says that an action phase is starting: /healthz tolerates a silent loop for pullTimeout plus a minute from
// here. Every phase that can take minutes (the pull, the up, a forced recreate) calls it, so a pull followed by an up
// followed by a forced recreate is judged phase by phase, not as one 20-minute silence.
func (d *Daemon) markActing() { d.acting.Store(d.clk.Now().UnixNano()) }

// keepAlive writes the last_seen heartbeat from a goroutine of its own while an action runs, and returns the function
// that stops it. The loop is synchronous, so during a pull or a recreate it does not tick and would write nothing; a
// last_seen that old would make the shutdown window of a daemon restart (plan §3.3 rule 4: 30 s before last_seen, up to
// 5 minutes after it within one boot) miss the containers that restart stopped, and a `restart: "no"` app that exits 1 on
// SIGTERM would be started by whalenurse. The goroutine does not touch d.lastBeat, which only the loop owns; the store's
// SetMeta is safe from another goroutine (database/sql).
func (d *Daemon) keepAlive() (stop func()) {
	if d.o.Meta == nil {
		return func() {}
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(d.beatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				d.o.Meta.SetMeta(MetaLastSeen, strconv.FormatInt(d.clk.Now().UnixNano(), 10))
			}
		}
	}()
	return func() { close(done); <-finished }
}

// pullPolicyRefusal reads the service's pull_policy and returns why the pull rung does not apply, or "". never and build
// forbid a pull (plan v3.8: the label opts in to a pull, not to overriding an explicit "do not pull"); missing,
// if_not_present, the refresh windows and no policy at all are replaced by --policy always. A policy that cannot be read
// is a reason too: the rung fails closed.
func (d *Daemon) pullPolicyRefusal(ctx context.Context, p compose.Project) string {
	gctx, cancel := context.WithTimeout(ctx, guardTimeout)
	defer cancel()
	policy, err := d.o.Compose.PullPolicy(gctx, p)
	switch {
	case err != nil:
		return "the pull rung does not apply: cannot read the service's pull_policy (" + err.Error() + ")"
	case compose.PullPolicyBlocksPull(policy):
		return "pull rung does not apply (pull_policy " + policy + ")"
	}
	return ""
}

// sidecarsSharing returns the compose services (same project) of the container's dependents that run the same image
// reference and were created from the image the pull is about to replace.
func (d *Daemon) sidecarsSharing(ctx context.Context, c docker.Container, ref string, old ...string) []string {
	var out []string
	for _, s := range d.idx.all {
		// A sidecar that is already stale (its parent's ID is gone, because rung 2 recreated the parent and the dependents
		// step has not reached it yet) no longer matches the parent by ID: the depends_on label finds it.
		if s.ID == c.ID || !hasRef(s) || s.Labels[docker.LabelProject] != c.Labels[docker.LabelProject] {
			continue
		}
		if !docker.MatchesRef(c.ID, c.Name, docker.ContainerRef(s.NetworkMode)) && !slices.Contains(d.parentServices(d.keyOf(s), s.Labels), c.Labels[docker.LabelService]) {
			continue
		}
		dep, err := d.rd.Inspect(ctx, s.ID)
		if err != nil || normalizeRef(dep.Image) != normalizeRef(ref) || !labelIsOneOf(dep.Labels[compose.LabelImage], old) {
			continue
		}
		if svc := dep.Labels[docker.LabelService]; svc != "" {
			out = append(out, svc)
		}
	}
	sort.Strings(out)
	return out
}

func labelIsOneOf(label string, ids []string) bool {
	if label == "" {
		return false
	}
	for _, id := range ids {
		if id != "" && id == label {
			return true
		}
	}
	return false
}

// dependentMayTakeMove says whether a stale dependent may take the tag that its parent's own pull moved, in the parent's
// open incident, and only that (plan v3.8): the reference must still resolve to what that pull produced, the pull must
// have been run by a service the dependent depends on, after that service's incident opened, and the dependent must have
// been created from the image that pull replaced. Anything else about the tag is still image drift.
func (d *Daemon) dependentMayTakeMove(ctx context.Context, c docker.Container) bool {
	m, ok := d.moverOf(ctx, c)
	if !ok || !labelIsOneOf(c.Labels[compose.LabelImage], []string{m.Old, m.OldPlain}) {
		return false
	}
	project := c.Labels[docker.LabelProject]
	for _, dep := range d.parentServices(docker.StateKey(d.cfg.HostID, c.Name, c.Labels), c.Labels) {
		for _, s := range d.idx.all {
			if s.Labels[docker.LabelProject] != project || s.Labels[docker.LabelService] != dep || d.keyOf(s) != m.Key {
				continue
			}
			if opened, open := d.eng.IncidentOpenedAt(m.Key); open && !m.At.Before(opened) {
				return true
			}
		}
	}
	return false
}

// noteUpgrade tells that rung 2 recreated a container of a pull-enabled service onto a different image than it was
// created from (the tag moved under it, and the guard let the image-only move through): "upgraded during heal", with the
// rollback. It says nothing when the image is the same.
func (d *Daemon) noteUpgrade(ctx context.Context, t engine.Target, old docker.Container, p compose.Project, recreateErr error) {
	if recreateErr != nil {
		return
	}
	cur, err := d.currentOf(ctx, t.Key)
	if err != nil || cur.ImageID == "" || old.ImageID == "" || cur.ImageID == old.ImageID {
		return
	}
	tag, up := compose.RollbackCommands(p, old.Image, old.ImageID)
	d.eng.Note(t, engine.Upgraded, fmt.Sprintf("%s: %s -> %s; recreated", old.Image, shortID(old.ImageID), shortID(cur.ImageID)), map[string]string{
		"ref": old.Image, "old": old.ImageID, "new": cur.ImageID, "old_image": old.ImageID,
		"rollback_tag": tag, "rollback_up": up, "ok": "true",
	})
}

// Package engine holds the per-container state machine: incidents, the ladder,
// SUSPENDED and PAUSED, the flap limit, the crash-loop tracker, the rate limit,
// the action-in-flight window and the manual-stop markers. It is pure. It makes
// no Docker calls, opens no file, and takes its time from an injected clock; the
// daemon feeds it decisions and performs the actions it returns, and a
// Persister (internal/store) keeps its state across restarts.
//
// Rung 2 (recreate) is carried out by the daemon through compose. The engine
// only plans it (Plan.Act is ActionRecreate or ActionRecreateStale), and the
// daemon runs the drift guard in between and reports the verdict back through
// Blocked or Warned. Rung 3 (pull, opt-in) is planned the same way (ActionPull);
// when the daemon finds the pulled image unchanged it says so in Result.Exhausted
// and the ladder ends at once. Where a rung cannot run (the version gate, no
// compose inputs) the ladder records RungUnavailable and moves on, so an
// unhealthy container then goes restart, then SUSPENDED.
//
// The Engine is safe for concurrent use: the daemon loop drives it, and the
// control socket's commands (pause, resume, unsuspend, status) call it from HTTP
// goroutines. Events are published while the lock is held, so subscribers must
// not call back into the Engine.
package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackspiering/whalenurse/internal/clock"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
	"github.com/jackspiering/whalenurse/internal/rules"
)

// Suspension reasons.
const (
	ReasonLadder   = "ladder"   // every rung ran and the container is still bad
	ReasonFlapping = "flapping" // more than FLAP_LIMIT incidents within FLAP_WINDOW
	ReasonDrift    = "drift"    // the drift guard skipped the recreate
)

// Params configures an Engine.
type Params struct {
	Host           string
	BaseCooldown   time.Duration
	RecoveryPeriod time.Duration
	CrashloopAfter time.Duration
	FlapLimit      int
	FlapWindow     time.Duration
	RateLimit      int           // actions per hour; 0 disables the limit
	InflightTail   time.Duration // how long after an action its events are still attributed to it
	DryRun         bool          // only labels events; the no-op actor is wired by the caller
	Persist        Persister     // nil: state lives in memory only
}

// Target identifies the container an evaluation is about.
type Target struct {
	Key  string
	ID   string
	Name string
}

// Plan is what the daemon should do after Evaluate.
type Plan struct {
	Act       rules.Action // ActionNone, ActionRestart, ActionStart, ActionRecreate, ActionRecreateStale or ActionPull
	Rung      string
	RecheckAt time.Time // inspect again then (cooldown, recovery period); zero for none
}

// Result is the outcome of an action.
type Result struct {
	Err    error
	DryRun bool
	// Attrs are added to the ActionResult event (the pull rung says its outcome and the image here).
	Attrs map[string]string
	// Exhausted, when set, says the action found nothing left to try: the pull rung sets it when the pulled image is the
	// one already there. The ladder is then spent at once, without a cooldown, and the container is SUSPENDED with this
	// text as the cause (plan §3.7: "if the digest is unchanged, notify and SUSPEND").
	Exhausted string
}

type window struct{ start, end time.Time } // end zero: still open

type incident struct {
	opened        time.Time
	class         rules.Class
	rung          int // index into the configured rungs of the next rung to run
	cooldownUntil time.Time
	spent         bool   // nothing left to try
	spentReason   string // why: ReasonLadder or ReasonFlapping
	suspended     bool
	suspendedAt   time.Time
	recovering    bool
	recoverySince time.Time
	recoveryCount int
	rateNoted     bool
}

type keyState struct {
	incident *incident
	windows  []window
	tracker  rules.Tracker
	advisory string // dedupe key of the last advisory event
	name     string
	flaps    []time.Time
	loop     LoopRecord
	dirty    bool
}

type pause struct {
	scope   string
	until   time.Time
	created time.Time
}

// Engine is the state machine for all keys on one host.
type Engine struct {
	mu     sync.Mutex
	p      Params
	clk    clock.Clock
	bus    *Bus
	keys   map[string]*keyState
	spent  []time.Time // timestamps of actions in the last hour
	pauses map[string]*pause
	reeval map[string]bool // keys the daemon should look at now (resume, unsuspend, pause expiry)

	restoreUntil time.Time // a start or kill before this is a restore, not a person

	shutdowns []shutdownWindow // when the host was shutting the containers down; see HostStopped
}

// shutdownWindow is a span in which a container that stopped was stopped by a daemon shutdown or reboot.
type shutdownWindow struct {
	from, to time.Time
	open     bool // the events stream is still lost: no restore yet, so the window has no end
}

// New creates an Engine that publishes to bus.
func New(p Params, clk clock.Clock, bus *Bus) *Engine {
	if p.InflightTail == 0 {
		p.InflightTail = 2 * time.Second
	}
	if p.RecoveryPeriod == 0 {
		p.RecoveryPeriod = 10 * time.Minute
	}
	if p.CrashloopAfter == 0 {
		p.CrashloopAfter = 5 * time.Minute
	}
	if p.FlapLimit == 0 {
		p.FlapLimit = 3
	}
	if p.FlapWindow == 0 {
		p.FlapWindow = 24 * time.Hour
	}
	return &Engine{p: p, clk: clk, bus: bus, keys: map[string]*keyState{}, pauses: map[string]*pause{}, reeval: map[string]bool{}}
}

// Restore loads persisted state. Call it once, before the engine is used.
func (e *Engine) Restore(s Snapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, k := range s.Keys {
		st := &keyState{name: k.Name, loop: k.Loop, flaps: append([]time.Time(nil), k.Flaps...)}
		if !k.MarkerAt.IsZero() {
			st.tracker.SetMarker(k.MarkerAt)
		}
		if r := k.Incident; r != nil {
			st.incident = &incident{
				opened: r.Opened, class: rules.Class(r.Class), rung: r.Rung, cooldownUntil: r.CooldownUntil,
				spent: r.Spent, spentReason: r.SpentReason, suspended: r.Suspended, suspendedAt: r.SuspendedAt,
				recovering: r.Recovering, recoverySince: r.RecoverySince, recoveryCount: r.RecoveryCount,
			}
		}
		e.keys[k.Key] = st
	}
	for _, p := range s.Pauses {
		e.pauses[p.Scope] = &pause{scope: p.Scope, until: p.Until, created: p.Created}
	}
	e.spent = append(e.spent, s.Actions...)
}

func (e *Engine) state(key string) *keyState {
	st, ok := e.keys[key]
	if !ok {
		st = &keyState{}
		e.keys[key] = st
	}
	return st
}

// AttrIncident is the attribute that says which incident of the key an event belongs to: the time it opened. A
// subscriber that must say something once per incident (notify) keys on it, because an incident survives a restart of
// whalenurse and its events then arrive without the IncidentOpened.
const AttrIncident = "incident"

func (e *Engine) emit(t Target, typ EventType, msg string, attrs map[string]string) {
	if st, ok := e.keys[t.Key]; ok && st.incident != nil {
		with := map[string]string{AttrIncident: st.incident.opened.UTC().Format(time.RFC3339Nano)}
		for k, v := range attrs {
			with[k] = v
		}
		attrs = with
	}
	e.bus.Publish(Event{Time: e.clk.Now(), Host: e.p.Host, Key: t.Key, Container: t.Name, Type: typ, Message: msg, Attrs: attrs})
}

// flush writes the key to the Persister if it changed.
func (e *Engine) flush(key string, st *keyState) {
	if !st.dirty || e.p.Persist == nil {
		st.dirty = false
		return
	}
	st.dirty = false
	rec := KeyRecord{Key: key, Name: st.name, MarkerAt: st.tracker.MarkedAt(), Loop: st.loop, Flaps: append([]time.Time(nil), st.flaps...)}
	if inc := st.incident; inc != nil {
		rec.Incident = &IncidentRecord{
			Opened: inc.opened, Class: string(inc.class), Rung: inc.rung, CooldownUntil: inc.cooldownUntil,
			Spent: inc.spent, SpentReason: inc.spentReason, Suspended: inc.suspended, SuspendedAt: inc.suspendedAt,
			Recovering: inc.recovering, RecoverySince: inc.recoverySince, RecoveryCount: inc.recoveryCount,
		}
	}
	e.p.Persist.SaveKey(rec)
}

// closeIncident ends the open incident with an outcome and records it in the history.
func (e *Engine) closeIncident(t Target, st *keyState, outcome string) {
	inc := st.incident
	if inc == nil {
		return
	}
	st.incident = nil
	st.dirty = true
	if e.p.Persist != nil {
		e.p.Persist.CloseIncident(ClosedIncident{Key: t.Key, Opened: inc.opened, Closed: e.clk.Now(), Class: string(inc.class), Outcome: outcome})
	}
}

// Busy reports whether the key has an open incident that is still being worked,
// or an action in flight. The rules use it to hold back the dependents of a busy
// parent. A suspended or recovered (healthy) incident is not busy: nothing is
// left to wait for.
func (e *Engine) Busy(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.keys[key]
	if !ok {
		return false
	}
	if inc := st.incident; inc != nil && !inc.suspended && !inc.recovering {
		return true
	}
	for _, w := range st.windows {
		if w.end.IsZero() {
			return true
		}
	}
	return false
}

// IncidentOpenedAt returns when the key's open incident was opened.
func (e *Engine) IncidentOpenedAt(key string) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.keys[key]; ok && st.incident != nil {
		return st.incident.opened, true
	}
	return time.Time{}, false
}

// RungNotApplicable skips the rung the plan named, without running it and without spending a rate-limit slot: the
// service's own configuration rules it out (the pull rung of a service with pull_policy never or build). When it was
// the last rung the ladder is spent and the container is SUSPENDED, with why as the cause.
func (e *Engine) RungNotApplicable(t Target, rung, why string, cfg config.Container) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.state(t.Key)
	defer e.flush(t.Key, st)
	inc := st.incident
	if inc == nil {
		return
	}
	e.emit(t, RungUnavailable, fmt.Sprintf("rung %q skipped: %s", rung, why), map[string]string{"rung": rung, "not_applicable": "true"})
	inc.rung++
	st.dirty = true
	if inc.rung >= len(cfg.Actions) {
		inc.spent, inc.spentReason = true, ReasonLadder
		e.suspend(t, st, "every rung ran or was unavailable: "+why, rules.Decision{Class: inc.class})
	}
}

// IncidentOpen reports whether the key has an incident, and whether it is SUSPENDED.
func (e *Engine) IncidentOpen(key string) (open, suspended bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.keys[key]; ok && st.incident != nil {
		return true, st.incident.suspended
	}
	return false, false
}

// Suspended reports whether the key is SUSPENDED.
func (e *Engine) Suspended(key string) bool {
	_, s := e.IncidentOpen(key)
	return s
}

// Evidence returns what the events so far say about how the key stopped.
func (e *Engine) Evidence(key string) rules.Evidence {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.keys[key]; ok {
		return st.tracker.Evidence()
	}
	return rules.Evidence{}
}

// Forget drops all state for a key whose container no longer exists.
func (e *Engine) Forget(t Target) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.keys[t.Key]
	if !ok {
		return
	}
	if st.incident != nil {
		e.emit(t, IncidentClosed, "container is gone", map[string]string{"reason": "gone"})
		e.closeIncident(t, st, "gone")
	}
	delete(e.keys, t.Key)
	if e.p.Persist != nil {
		e.p.Persist.DeleteKey(t.Key)
	}
}

// ---- Docker events, markers, the restore window ---------------------------------------------

// OpenRestoreWindow starts (or extends) a window in which a `start` is a restore
// and a `kill` sets no marker: the daemon was down and is bringing containers
// back (plan §3.3, mechanism c).
func (e *Engine) OpenRestoreWindow(d time.Duration, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	until := e.clk.Now().Add(d)
	if until.After(e.restoreUntil) {
		e.restoreUntil = until
	}
	e.emit(Target{}, RestoreWindowOpened, "restore window: a start is a restore, a kill sets no marker ("+why+")",
		map[string]string{"until": e.restoreUntil.UTC().Format(time.RFC3339), "why": why})
}

// RestoreWindowUntil returns when the restore window ends; zero or past if none is open.
func (e *Engine) RestoreWindowUntil() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.restoreUntil
}

// AddShutdownWindow records a span in which containers were stopped by the host: from 30 s before the previous
// run's last_seen to the daemon's restore (plan §3.3 rule 4).
func (e *Engine) AddShutdownWindow(from, to time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdowns = append(e.shutdowns, shutdownWindow{from: from, to: to})
}

// OpenShutdownWindow starts a window at an events-stream loss; it has no end until CloseShutdownWindow.
func (e *Engine) OpenShutdownWindow(from time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, w := range e.shutdowns {
		if w.open {
			return
		}
	}
	e.shutdowns = append(e.shutdowns, shutdownWindow{from: from, open: true})
}

// CloseShutdownWindow ends the open window when the stream is back: the daemon has restored its containers.
func (e *Engine) CloseShutdownWindow(to time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.shutdowns {
		if e.shutdowns[i].open {
			e.shutdowns[i].open, e.shutdowns[i].to = false, to
		}
	}
}

// HostStopped reports whether a container that finished at the given time was stopped by a daemon shutdown or
// reboot, that is, inside a shutdown window.
func (e *Engine) HostStopped(finishedAt time.Time) bool {
	if finishedAt.IsZero() || finishedAt.Year() < 2000 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, w := range e.shutdowns {
		if !finishedAt.Before(w.from) && (w.open || !finishedAt.After(w.to)) {
			return true
		}
	}
	return false
}

// QuarantineMarkers discards every manual-stop marker set in the 30 s before lostAt
// (mechanism a: the events stream ended, so the kills may be a daemon shutdown).
// Older markers, real manual stops, survive. It returns the keys it cleared.
func (e *Engine) QuarantineMarkers(lostAt time.Time) []string {
	return e.dropMarkers(func(st *keyState) bool { return st.tracker.Quarantine(lostAt.Add(-rules.QuarantineWindow)) },
		"events stream lost: kills in the 30s before it may be a daemon shutdown")
}

// DropAllMarkers discards every marker (mechanism b: a new boot id, so none of
// them can belong to this boot).
func (e *Engine) DropAllMarkers(why string) []string {
	return e.dropMarkers(func(st *keyState) bool { return st.tracker.Drop() }, why)
}

func (e *Engine) dropMarkers(drop func(*keyState) bool, why string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var cleared []string
	for k, st := range e.keys {
		if drop(st) {
			st.dirty = true
			e.flush(k, st)
			cleared = append(cleared, k)
		}
	}
	sort.Strings(cleared)
	if len(cleared) > 0 {
		e.emit(Target{}, MarkersQuarantined, fmt.Sprintf("%d manual-stop markers discarded: %s", len(cleared), why),
			map[string]string{"count": strconv.Itoa(len(cleared))})
	}
	return cleared
}

// ObserveDockerEvent records a Docker event for a key. Events that fall inside
// an action's in-flight window are attributed to the action and not
// interpreted: they set no manual-stop marker, clear nothing and open nothing.
// Events inside the restore window are restores: a `start` clears no SUSPENDED
// and a `kill` sets no marker. It reports whether the event was attributed to an action.
func (e *Engine) ObserveDockerEvent(t Target, ev docker.Event) (attributed bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.state(t.Key)
	at := ev.Time
	if at.IsZero() {
		at = e.clk.Now()
	}
	e.prune(st)
	for _, w := range st.windows {
		if !at.Before(w.start) && (w.end.IsZero() || !at.After(w.end)) {
			e.emit(t, EventAttributed, "docker event during a whalenurse action", map[string]string{"action": ev.Action})
			return true
		}
	}

	restoring := e.clk.Now().Before(e.restoreUntil)
	if restoring && ev.Action == "kill" {
		// The daemon is restoring or has just been: this is not a person.
		e.emit(t, RestoreEvent, "kill inside the restore window: no manual-stop marker", map[string]string{"action": ev.Action})
		return false
	}

	wasManual := st.tracker.Evidence().ManualStop
	st.tracker.ApplyAt(ev.Action, ev.Detail, at)
	isManual := st.tracker.Evidence().ManualStop
	if wasManual != isManual {
		st.dirty = true
	}
	switch {
	case !wasManual && isManual:
		e.emit(t, ManualStopMarked, "kill seen outside any action: container stopped by hand", map[string]string{"action": ev.Action})
	case wasManual && !isManual:
		e.emit(t, ManualStopCleared, "start seen: manual-stop marker cleared", nil)
	}

	// Leaving SUSPENDED: a recreate by someone else, or a start by hand after a
	// manual stop. A plain start (the restart policy at work) does not count, or
	// every policy restart would re-arm the ladder. Neither does a start inside
	// the restore window, nor one that follows a quarantined shutdown kill: no
	// marker is left for it to follow.
	switch ev.Action {
	case "create":
		e.reset(t, st, "container was recreated outside whalenurse")
	case "start":
		switch {
		case restoring:
			e.emit(t, RestoreEvent, "start inside the restore window: a restore, nothing is unsuspended", map[string]string{"action": ev.Action})
		case wasManual:
			e.reset(t, st, "started by hand after a manual stop")
		}
	}
	e.flush(t.Key, st)
	return false
}

func (e *Engine) reset(t Target, st *keyState, why string) {
	if st.incident != nil && st.incident.suspended {
		e.emit(t, Unsuspended, why, map[string]string{"reason": why})
		e.closeIncident(t, st, "unsuspended")
	} else if st.incident != nil && why == "container was recreated outside whalenurse" {
		e.emit(t, IncidentClosed, why, map[string]string{"reason": "recreated"})
		e.closeIncident(t, st, "recreated")
	}
	st.advisory = ""
}

func (e *Engine) prune(st *keyState) {
	cut := e.clk.Now().Add(-time.Minute)
	kept := st.windows[:0]
	for _, w := range st.windows {
		if w.end.IsZero() || w.end.After(cut) {
			kept = append(kept, w)
		}
	}
	st.windows = kept
}

// ---- evaluation -------------------------------------------------------------------------------

// Evaluate feeds the engine one classification and returns what to do. The
// caller must have inspected the container no earlier than GRACE after the
// trigger.
func (e *Engine) Evaluate(t Target, d rules.Decision, cfg config.Container, o Obs) Plan {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	st := e.state(t.Key)
	if st.name != t.Name && t.Name != "" {
		st.name = t.Name
		st.dirty = true
	}
	e.prune(st)
	defer e.flush(t.Key, st)

	// PAUSED: no actions and no state changes; events are still recorded.
	if e.pausedLocked(t.Key, now) != nil {
		return Plan{}
	}

	switch d.Action {
	case rules.ActionRestart, rules.ActionStart, rules.ActionRecreate, rules.ActionRecreateStale:
		return e.heal(t, st, d, cfg, o, now)
	case rules.ActionNeedsRecreate:
		e.advise(t, st, d, NeedsRecreate)
	case rules.ActionWaitForParent:
		e.advise(t, st, d, WaitingForParent)
	case rules.ActionNotify:
		e.advise(t, st, d, NotifyOnly)
	default: // ActionNone
		st.advisory = ""
		return e.settle(t, st, d, cfg, o, now)
	}
	return Plan{}
}

// settle handles a decision that needs no action while an incident may be open.
func (e *Engine) settle(t Target, st *keyState, d rules.Decision, cfg config.Container, o Obs, now time.Time) Plan {
	inc := st.incident
	if inc == nil || d.Class == rules.ClassSkip {
		return Plan{}
	}
	switch d.Class {
	case rules.ClassOK:
		return e.recovering(t, st, inc, o, now)
	case rules.ClassStarting, rules.ClassRestarting:
		// Not recovered yet and not failing: look again after the cooldown.
		if inc.recovering {
			inc.recovering = false
			st.dirty = true
		}
		return Plan{RecheckAt: now.Add(config.CooldownFor(e.p.BaseCooldown, cfg.Cooldown, 0))}
	default:
		// Stopped on purpose, or in a state that needs no action. A SUSPENDED
		// container stays SUSPENDED (only the §3.2 triggers clear it); any other
		// incident is over.
		if inc.suspended {
			return Plan{}
		}
		e.emit(t, IncidentClosed, d.Reason, map[string]string{"reason": string(d.Class)})
		e.closeIncident(t, st, "stopped")
	}
	return Plan{}
}

// recovering handles an observation of a healthy container while an incident is
// open. "Recovered" is plan §3.2's definition: with a healthcheck, health is
// healthy; without one, the container is running and RestartCount has not
// changed for RECOVERY_PERIOD. SUSPENDED clears at that point ("returns to
// healthy"); the incident closes only after the container has stayed recovered
// for RECOVERY_PERIOD.
func (e *Engine) recovering(t Target, st *keyState, inc *incident, o Obs, now time.Time) Plan {
	if !inc.recovering || o.RestartCount != inc.recoveryCount {
		inc.recovering, inc.recoverySince, inc.recoveryCount = true, now, o.RestartCount
		st.dirty = true
	}
	period := e.p.RecoveryPeriod
	healthy := o.HasHealthcheck || now.Sub(inc.recoverySince) >= period
	if healthy && inc.suspended {
		inc.suspended = false
		st.dirty = true
		e.emit(t, Unsuspended, "container returned to healthy", map[string]string{"reason": "healthy"})
	}
	if now.Sub(inc.recoverySince) >= period {
		e.emit(t, Recovered, "container has been healthy for "+period.String(), map[string]string{"opened": inc.opened.UTC().Format(time.RFC3339)})
		e.closeIncident(t, st, "recovered")
		return Plan{}
	}
	return Plan{RecheckAt: inc.recoverySince.Add(period)}
}

// advise emits an advisory event once per (class, action) until the state changes.
func (e *Engine) advise(t Target, st *keyState, d rules.Decision, typ EventType) {
	sig := string(d.Class) + "/" + string(d.Action)
	if st.advisory == sig {
		return
	}
	st.advisory = sig
	attrs := map[string]string{"class": string(d.Class)}
	if d.Cause != rules.CauseNone {
		attrs["cause"] = string(d.Cause)
	}
	if len(d.Dependents) > 0 {
		attrs["dependents"] = strings.Join(d.Dependents, ",")
	}
	e.emit(t, typ, d.Reason, attrs)
}

func (e *Engine) flapLimit(cfg config.Container) int {
	if cfg.FlapLimit > 0 {
		return cfg.FlapLimit
	}
	return e.p.FlapLimit
}

func (e *Engine) heal(t Target, st *keyState, d rules.Decision, cfg config.Container, o Obs, now time.Time) Plan {
	st.advisory = ""
	if st.incident == nil {
		// Flap count: closing an incident does not reset it, only the window expiring does. A dependent repaired by the
		// dependents step belongs to its parent's incident and is not counted (plan v3.6).
		cut := now.Add(-e.p.FlapWindow)
		kept := st.flaps[:0]
		for _, f := range st.flaps {
			if f.After(cut) {
				kept = append(kept, f)
			}
		}
		st.flaps = kept
		if !d.IsDependentStep() {
			st.flaps = append(st.flaps, now)
		}
		st.incident = &incident{opened: now, class: d.Class}
		st.dirty = true
		e.emit(t, IncidentOpened, d.Reason, map[string]string{"class": string(d.Class), "action": string(d.Action)})
		// More than FLAP_LIMIT incidents within FLAP_WINDOW: SUSPENDED, whatever became of the last ones.
		if limit := e.flapLimit(cfg); !d.IsDependentStep() && len(st.flaps) > limit {
			st.incident.spent, st.incident.spentReason = true, ReasonFlapping
			e.suspend(t, st, fmt.Sprintf("flapping: %d incidents within %s (limit %d): %s", len(st.flaps), e.p.FlapWindow, limit, d.Reason), d)
			return Plan{}
		}
	}
	inc := st.incident
	if inc.recovering {
		// It failed again before RECOVERY_PERIOD was over: still the same incident, so the ladder goes on where it was.
		inc.recovering = false
		st.dirty = true
	}
	if inc.suspended {
		return Plan{}
	}
	if inc.spent {
		// The ladder already ran its pass (or the key is flapping) and the suspension was lifted by a
		// recovery that did not last. One pass per incident: SUSPENDED again, nothing is tried.
		e.suspend(t, st, "failed again before the incident closed ("+inc.spentReason+"): "+d.Reason, d)
		return Plan{}
	}
	for _, w := range st.windows {
		if w.end.IsZero() {
			return Plan{} // an action is running; its result will schedule the next look
		}
	}
	if now.Before(inc.cooldownUntil) {
		return Plan{RecheckAt: inc.cooldownUntil}
	}

	for inc.rung < len(cfg.Actions) {
		r := cfg.Actions[inc.rung]
		if r == config.RungRestart && d.Action.IsRecreate() {
			// Restarting a dependent whose parent is gone fails and makes it worse (E1); a network that is gone
			// is not repaired by a restart either. Rung 2 is the first rung that can help.
			inc.rung++
			st.dirty = true
			continue
		}
		if why, bad := o.Unavailable[r]; bad {
			e.emit(t, RungUnavailable, fmt.Sprintf("rung %q: %s; skipped", r, why), map[string]string{"rung": r})
			inc.rung++
			st.dirty = true
			continue
		}
		switch r {
		case config.RungRestart, config.RungRecreate, config.RungPull:
			if !e.rateOK(now) {
				if !inc.rateNoted {
					inc.rateNoted = true
					e.emit(t, RateLimited, fmt.Sprintf("rate limit of %d actions/hour reached; will retry", e.p.RateLimit), nil)
				}
				return Plan{RecheckAt: e.nextSlot(now)}
			}
			inc.rateNoted = false
			act := d.Action
			switch {
			case r == config.RungRecreate && !act.IsRecreate():
				act = rules.ActionRecreate // rung 2 after a restart: compose --force-recreate
			case r == config.RungPull:
				act = rules.ActionPull // rung 3: pull, and an up only if the image changed (the daemon decides)
			}
			return Plan{Act: act, Rung: r}
		default:
			e.emit(t, RungUnavailable, fmt.Sprintf("rung %q is not a rung whalenurse knows; skipped", r), map[string]string{"rung": r})
			inc.rung++
			st.dirty = true
		}
	}
	inc.spent, inc.spentReason = true, ReasonLadder
	msg := "every rung ran or was unavailable"
	if inc.rung == 0 {
		msg = "no usable rung configured"
	}
	e.suspend(t, st, msg+": "+d.Reason, d)
	return Plan{}
}

// Blocked is the drift guard's verdict "do not recreate" (plan §3.6): the rung is skipped, the cause is reported, and
// the container is SUSPENDED, because the pull rung would be stopped by the same findings. The daemon calls it instead
// of Begin, so no rate-limit slot is spent. It reports false when the incident is gone or already suspended.
func (e *Engine) Blocked(t Target, d rules.Decision, findings []string) bool {
	return e.BlockedWith(t, d, findings, nil)
}

// BlockedWith is Blocked with extra attributes on the DriftBlocked event: when the tag that moved was moved by
// whalenurse's own pull rung, the daemon says which container did it and when (plan §3.6, §3.7).
func (e *Engine) BlockedWith(t Target, d rules.Decision, findings []string, extra map[string]string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.state(t.Key)
	defer e.flush(t.Key, st)
	inc := st.incident
	if inc == nil || inc.suspended {
		return false
	}
	cause := strings.Join(findings, "; ")
	attrs := map[string]string{"class": string(d.Class), "findings": cause}
	for k, v := range extra {
		attrs[k] = v
	}
	e.emit(t, DriftBlocked, "recreate skipped by the drift guard: "+cause, attrs)
	inc.spent, inc.spentReason = true, ReasonDrift
	e.suspend(t, st, "recreate skipped by the drift guard ("+cause+"): "+d.Reason, d)
	return true
}

// Warned records that WHALENURSE_DRIFT_GUARD=warn let a recreate through despite the findings.
func (e *Engine) Warned(t Target, findings []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cause := strings.Join(findings, "; ")
	e.emit(t, DriftWarned, "recreating despite the drift guard (WHALENURSE_DRIFT_GUARD=warn): "+cause, map[string]string{"findings": cause})
}

// Note publishes an event the daemon observed (the version gate, a post-check) so it reaches the same stream as the
// engine's own.
func (e *Engine) Note(t Target, typ EventType, msg string, attrs map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emit(t, typ, msg, attrs)
}

func (e *Engine) suspend(t Target, st *keyState, msg string, d rules.Decision) {
	inc := st.incident
	inc.suspended, inc.suspendedAt = true, e.clk.Now()
	st.dirty = true
	e.emit(t, Suspended, msg, map[string]string{"class": string(d.Class), "reason": inc.spentReason, "held": "until it recovers, is recreated or started by hand, or you run unsuspend"})
}

// Begin opens the in-flight window for an action and returns when it started.
func (e *Engine) Begin(t Target, plan Plan, d rules.Decision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	st := e.state(t.Key)
	st.windows = append(st.windows, window{start: now})
	e.spent = append(e.spent, now)
	if e.p.Persist != nil && !e.p.DryRun {
		e.p.Persist.SaveAction(now)
	}
	e.emit(t, ActionStarted, string(plan.Act)+": "+d.Reason, e.actionAttrs(plan, false))
}

// End closes the window, records the result and returns when to look again.
func (e *Engine) End(t Target, plan Plan, res Result, cfg config.Container) (recheck time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	st := e.state(t.Key)
	defer e.flush(t.Key, st)
	for i := range st.windows {
		if st.windows[i].end.IsZero() {
			st.windows[i].end = now.Add(e.p.InflightTail)
		}
	}
	msg := "ok"
	if res.Err != nil {
		msg = "failed: " + res.Err.Error()
	}
	attrs := e.actionAttrs(plan, res.Err == nil)
	for k, v := range res.Attrs {
		attrs[k] = v
	}
	if res.DryRun {
		attrs["dry_run"] = "true"
		msg = "dry-run: would " + string(plan.Act)
	}
	e.emit(t, ActionResult, string(plan.Act)+" "+msg, attrs)
	inc := st.incident
	if inc == nil {
		return time.Time{}
	}
	if res.Exhausted != "" && res.Err == nil && !res.DryRun {
		// Nothing left to try (the pull found no newer image): no cooldown to wait out, the ladder is spent.
		inc.rung++
		inc.spent, inc.spentReason = true, ReasonLadder
		e.suspend(t, st, "every rung ran or was unavailable: "+res.Exhausted, rules.Decision{Class: inc.class})
		return time.Time{}
	}
	cd := config.CooldownFor(e.p.BaseCooldown, cfg.Cooldown, inc.rung)
	inc.rung++
	inc.cooldownUntil = now.Add(cd)
	st.dirty = true
	return inc.cooldownUntil
}

// Interrupt closes the window of an action that whalenurse's own stop cut short (the action outlived the shutdown grace,
// or the pull, which a stop does not wait for). It is not a result: the rung is not consumed and no failure is reported,
// so the next run plans the same rung again. The rate-limit slot Begin took stays spent: the action may have changed
// something before it was stopped.
func (e *Engine) Interrupt(t Target, plan Plan, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	st := e.state(t.Key)
	for i := range st.windows {
		if st.windows[i].end.IsZero() {
			st.windows[i].end = now.Add(e.p.InflightTail)
		}
	}
	msg := string(plan.Act) + " interrupted: whalenurse is stopping; the rung runs again after the restart"
	if err != nil {
		msg += " (" + err.Error() + ")"
	}
	e.emit(t, ActionInterrupted, msg, map[string]string{"action": string(plan.Act), "rung": plan.Rung})
}

// Refuse cancels a planned action whose pre-action check failed. The rung is
// not consumed and no rate-limit slot is spent (Begin was never called).
func (e *Engine) Refuse(t Target, d rules.Decision, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emit(t, ActionRefused, why, map[string]string{"class": string(d.Class)})
}

func (e *Engine) actionAttrs(plan Plan, ok bool) map[string]string {
	a := map[string]string{"action": string(plan.Act), "rung": plan.Rung, "ok": strconv.FormatBool(ok)}
	if e.p.DryRun {
		a["dry_run"] = "true"
	}
	return a
}

func (e *Engine) rateOK(now time.Time) bool {
	if e.p.RateLimit <= 0 {
		return true
	}
	cut := now.Add(-time.Hour)
	kept := e.spent[:0]
	for _, t := range e.spent {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	e.spent = kept
	return len(e.spent) < e.p.RateLimit
}

func (e *Engine) nextSlot(now time.Time) time.Time {
	if len(e.spent) == 0 {
		return now.Add(time.Minute)
	}
	return e.spent[0].Add(time.Hour + time.Second)
}

// ---- PAUSED, unsuspend, status -----------------------------------------------------------------

// DefaultPause is the pause length when the command gives none.
const DefaultPause = 2 * time.Hour

// ParseScope validates a pause scope: <project> or <project>/<service>. A
// container that is not in a compose project is addressed by its name.
func ParseScope(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	parts := strings.Split(s, "/")
	if s == "" || len(parts) > 2 {
		return "", fmt.Errorf("scope %q: want <project> or <project>/<service>", s)
	}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("scope %q: empty part", s)
		}
	}
	return s, nil
}

// scopeMatches reports whether a state key (host/project/service/number, or
// host/name) is inside the scope.
func scopeMatches(scope, key string) bool {
	_, rest, ok := strings.Cut(key, "/")
	if !ok {
		return false
	}
	have, want := strings.Split(rest, "/"), strings.Split(scope, "/")
	if len(want) > len(have) {
		return false
	}
	for i := range want {
		if have[i] != want[i] {
			return false
		}
	}
	return true
}

// Pause stops all actions for the scope for d (zero: until resumed). Events are
// still recorded. It replaces an earlier pause of the same scope.
func (e *Engine) Pause(scope string, d time.Duration) (Pause, error) {
	scope, err := ParseScope(scope)
	if err != nil {
		return Pause{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	p := &pause{scope: scope, created: now}
	if d > 0 {
		p.until = now.Add(d)
	}
	e.pauses[scope] = p
	rec := Pause{Scope: scope, Until: p.until, Created: now}
	if e.p.Persist != nil {
		e.p.Persist.SavePause(rec)
	}
	attrs := map[string]string{"scope": scope}
	msg := "paused " + scope + " indefinitely"
	if d > 0 {
		attrs["until"] = p.until.UTC().Format(time.RFC3339)
		msg = fmt.Sprintf("paused %s for %s (until %s)", scope, d, p.until.UTC().Format(time.RFC3339))
	}
	e.emit(Target{Key: e.p.Host + "/" + scope}, Paused, msg, attrs)
	return rec, nil
}

// Resume lifts a pause and queues the keys under it for re-evaluation. It
// reports whether the scope was paused.
func (e *Engine) Resume(scope string) (bool, error) {
	scope, err := ParseScope(scope)
	if err != nil {
		return false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.pauses[scope]; !ok {
		return false, nil
	}
	e.endPause(scope, "resumed by command")
	return true, nil
}

func (e *Engine) endPause(scope, why string) {
	delete(e.pauses, scope)
	if e.p.Persist != nil {
		e.p.Persist.DeletePause(scope)
	}
	for k := range e.keys {
		if scopeMatches(scope, k) {
			e.reeval[k] = true
		}
	}
	e.emit(Target{Key: e.p.Host + "/" + scope}, Resumed, "resumed "+scope+": "+why, map[string]string{"scope": scope, "why": why})
}

// pausedLocked returns the pause covering the key, expiring the ones that ran out.
func (e *Engine) pausedLocked(key string, now time.Time) *pause {
	var hit *pause
	for _, p := range e.pauses {
		if !p.until.IsZero() && !now.Before(p.until) {
			continue // expired; ExpirePauses announces it
		}
		if scopeMatches(p.scope, key) && (hit == nil || len(p.scope) > len(hit.scope)) {
			hit = p
		}
	}
	return hit
}

// Paused reports whether actions on the key are paused, and until when (zero:
// indefinitely).
func (e *Engine) Paused(key string) (bool, time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.pausedLocked(key, e.clk.Now()); p != nil {
		return true, p.until
	}
	return false, time.Time{}
}

// ExpirePauses ends the pauses whose time is up and queues their keys for
// re-evaluation ("resume / expiry -> re-evaluate", plan §3.2).
func (e *Engine) ExpirePauses() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	for scope, p := range e.pauses {
		if !p.until.IsZero() && !now.Before(p.until) {
			e.endPause(scope, "pause expired")
		}
	}
}

// TakeReeval returns the keys to look at now (after resume, expiry or unsuspend) and forgets them.
func (e *Engine) TakeReeval() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.reeval) == 0 {
		return nil
	}
	out := make([]string, 0, len(e.reeval))
	for k := range e.reeval {
		out = append(out, k)
	}
	e.reeval = map[string]bool{}
	sort.Strings(out)
	return out
}

// Unsuspend lifts SUSPENDED for every key in the scope and returns the keys.
// For a key that was suspended for flapping it also forgets the flap history:
// without that, the next incident would suspend it again at once.
func (e *Engine) Unsuspend(scope string) ([]string, error) {
	scope, err := ParseScope(scope)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for k, st := range e.keys {
		if st.incident == nil || !st.incident.suspended || !scopeMatches(scope, k) {
			continue
		}
		t := Target{Key: k, Name: st.name}
		why := "unsuspend command"
		if st.incident.spentReason == ReasonFlapping {
			st.flaps = nil
			why += " (flap history cleared)"
		}
		e.emit(t, Unsuspended, why, map[string]string{"reason": "command"})
		e.closeIncident(t, st, "unsuspended")
		st.advisory = ""
		e.reeval[k] = true
		e.flush(k, st)
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// KeyStatus is one row of Status.
type KeyStatus struct {
	Key           string    `json:"key"`
	Name          string    `json:"name,omitempty"`
	State         string    `json:"state"` // ok, incident, recovering, SUSPENDED, PAUSED
	Class         string    `json:"class,omitempty"`
	Rung          int       `json:"rung,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	SuspendReason string    `json:"suspend_reason,omitempty"`
	PausedUntil   time.Time `json:"paused_until,omitempty"`
	Flaps         int       `json:"flaps"`
	ManualStop    bool      `json:"manual_stop,omitempty"`
	LoopIncreases int       `json:"loop_increases,omitempty"`
}

// Status is the engine's part of `whalenurse status`.
type Status struct {
	Keys         []KeyStatus `json:"keys"`
	Pauses       []Pause     `json:"pauses"`
	ActionsLastH int         `json:"actions_last_hour"`
	RateLimit    int         `json:"rate_limit"`
	RestoreUntil time.Time   `json:"restore_window_until,omitempty"`
}

// Status snapshots every key worth showing: one with an incident, a pause, flap
// history, a marker or a crash-loop count.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clk.Now()
	s := Status{RateLimit: e.p.RateLimit, Keys: []KeyStatus{}, Pauses: []Pause{}}
	if now.Before(e.restoreUntil) {
		s.RestoreUntil = e.restoreUntil.UTC()
	}
	e.rateOK(now) // prunes e.spent
	s.ActionsLastH = len(e.spent)
	for _, p := range e.pauses {
		s.Pauses = append(s.Pauses, Pause{Scope: p.scope, Until: p.until.UTC(), Created: p.created.UTC()})
	}
	sort.Slice(s.Pauses, func(i, j int) bool { return s.Pauses[i].Scope < s.Pauses[j].Scope })
	cut := now.Add(-e.p.FlapWindow)
	for k, st := range e.keys {
		flaps := 0
		for _, f := range st.flaps {
			if f.After(cut) {
				flaps++
			}
		}
		ks := KeyStatus{Key: k, Name: st.name, State: "ok", Flaps: flaps, ManualStop: st.tracker.Evidence().ManualStop, LoopIncreases: st.loop.N}
		if inc := st.incident; inc != nil {
			ks.State, ks.Class, ks.Rung, ks.CooldownUntil = "incident", string(inc.class), inc.rung, inc.cooldownUntil.UTC()
			switch {
			case inc.suspended:
				ks.State, ks.SuspendReason = "SUSPENDED", inc.spentReason
			case inc.recovering:
				ks.State = "recovering"
			}
		}
		if p := e.pausedLocked(k, now); p != nil {
			ks.PausedUntil = p.until.UTC()
			if ks.State == "ok" || ks.State == "incident" {
				ks.State = "PAUSED"
			}
		}
		if ks.State == "ok" && flaps == 0 && !ks.ManualStop && ks.LoopIncreases == 0 {
			continue
		}
		s.Keys = append(s.Keys, ks)
	}
	sort.Slice(s.Keys, func(i, j int) bool { return s.Keys[i].Key < s.Keys[j].Key })
	return s
}

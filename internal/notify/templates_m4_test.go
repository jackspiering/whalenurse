package notify

import (
	"strings"
	"testing"
	"unicode/utf8"

	wncompose "github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/engine"
)

// M4: the messages of the pull rung (plan §3.7, v3.8).

const (
	oldID = "sha256:aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	newID = "sha256:bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
)

func movedAttrs(p wncompose.Project, ref string, ok bool) map[string]string {
	tag, up := wncompose.RollbackCommands(p, ref, oldID)
	a := map[string]string{"ref": ref, "old": oldID, "new": newID, "old_image": oldID, "rollback_tag": tag, "rollback_up": up, "ok": "true"}
	if !ok {
		a["ok"], a["error"] = "false", "port is already allocated"
	}
	return a
}

func stackProject() wncompose.Project {
	return wncompose.Project{Name: "stack", WorkingDir: "/opt/stacks/stack", ConfigFiles: []string{"/opt/stacks/stack/compose.yaml"}, Service: "web"}
}

// The upgrade message gives old -> new and the exact rollback, both commands whole.
func TestUpgradedDuringHealGivesTheDigestsAndTheExactRollback(t *testing.T) {
	a := movedAttrs(stackProject(), "ghcr.io/o/web:latest", true)
	m := renderOne(t, ev(engine.TagMoved, webKey, "x", a), rctx{})
	if !strings.Contains(m.Title, "nas: stack-web-1 upgraded during heal") || !m.Critical {
		t.Fatalf("%v critical=%v", m, m.Critical)
	}
	for _, want := range []string{webKey, "ghcr.io/o/web:latest aaaaaaaaaaaa -> bbbbbbbbbbbb", "roll back:", a["rollback_tag"], a["rollback_up"], "every stack that shares this tag"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, m.Body)
		}
	}
	// Exactly the two commands, on lines of their own, in this order.
	lines := strings.Split(m.Body, "\n")
	if n := len(lines); lines[n-2] != a["rollback_tag"] || lines[n-1] != a["rollback_up"] {
		t.Errorf("the rollback commands must be the last two lines, whole:\n%s", m.Body)
	}
}

func TestUpgradeThatFailedToRecreateStillGivesTheRollback(t *testing.T) {
	m := renderOne(t, ev(engine.TagMoved, webKey, "x", movedAttrs(stackProject(), "web:1", false)), rctx{})
	for _, want := range []string{"pulled a newer image, but the recreate failed", "error: port is already allocated", "docker image tag " + oldID} {
		if !strings.Contains(m.Title+m.Body, want) {
			t.Errorf("lacks %q: %v", want, m)
		}
	}
}

// With no local copy before the pull there is nothing to roll back to, and the message says so instead of printing a
// command that cannot work.
func TestUpgradeWithoutAPreviousImageHasNoRollbackCommand(t *testing.T) {
	m := renderOne(t, ev(engine.TagMoved, webKey, "x", map[string]string{"ref": "web:1", "new": newID, "ok": "true"}), rctx{})
	if strings.Contains(m.Body, "docker image tag") || !strings.Contains(m.Body, "no rollback") || !strings.Contains(m.Body, "no local copy") {
		t.Fatalf("%s", m.Body)
	}
}

// The rollback is never cut, whatever the other text: with the longest names the commands can reasonably have, the body
// stays under MaxBodyPinned and every command is whole. The key line is never cut either.
func TestUpgradedMessageWorstCase(t *testing.T) {
	e := worstCaseUpgrade(t)
	a, key := e.Attrs, e.Key
	for _, c := range []struct {
		typ engine.EventType
		dry bool
	}{{engine.TagMoved, false}, {engine.TagMoved, true}, {engine.Upgraded, false}, {engine.Upgraded, true}} {
		dry := c.dry
		e.Type = c.typ
		m := renderOne(t, e, rctx{dryRun: dry})
		t.Logf("%s, dry-run %v: body %d runes, of them %d pinned (max %d)", e.Type, dry, utf8.RuneCountInString(m.Body), utf8.RuneCountInString(m.pinned), MaxBodyPinned)
		if utf8.RuneCountInString(m.Title) > MaxTitle || utf8.RuneCountInString(m.Body) > MaxBodyPinned {
			t.Errorf("%d / %d runes (max %d / %d):\n%s", utf8.RuneCountInString(m.Title), utf8.RuneCountInString(m.Body), MaxTitle, MaxBodyPinned, m.Body)
		}
		if !strings.Contains(m.Body, a["rollback_tag"]+"\n"+a["rollback_up"]) {
			t.Errorf("the rollback was cut or reordered:\n%s", m.Body)
		}
		if !strings.HasPrefix(m.Body, key) && !strings.HasPrefix(m.Body, "[dry-run] "+key) {
			t.Errorf("the key line was cut:\n%s", m.Body)
		}
	}
}

// The pull's own ActionResult is not sent when the pull rung has its own words for the outcome; a failed pull command is.
func TestPullActionResultsAreNotSaidTwice(t *testing.T) {
	for _, outcome := range []string{"unchanged", "upgraded", "upgrade-failed"} {
		e := ev(engine.ActionResult, webKey, "pull ok", map[string]string{"action": "pull", "rung": "pull", "ok": "true", "outcome": outcome})
		if _, _, ok := render(e, rctx{}); ok {
			t.Errorf("outcome %s is said by the Suspended or TagMoved message, not twice", outcome)
		}
	}
	failed := ev(engine.ActionResult, webKey, "pull failed: compose pull: pull access denied", map[string]string{"action": "pull", "rung": "pull", "ok": "false"})
	m := renderOne(t, failed, rctx{})
	if !strings.Contains(m.Title, "pull failed") || !strings.Contains(m.Body, "pull access denied") {
		t.Fatalf("%v", m)
	}
	dry := ev(engine.ActionResult, webKey, "dry-run: would pull", map[string]string{"action": "pull", "rung": "pull", "ok": "true", "dry_run": "true"})
	if m := renderOne(t, dry, rctx{dryRun: true}); !strings.Contains(m.Title, "would pull") || !strings.HasPrefix(m.Title, DryRunMarker) {
		t.Fatalf("%v", m)
	}
}

// "No newer image" is said by the giving-up message, with the cause.
func TestNoNewerImageIsInTheGivingUpMessage(t *testing.T) {
	e := ev(engine.Suspended, webKey, "every rung ran or was unavailable: the pull found no newer image for web:1 (still aaaaaaaaaaaa)", map[string]string{"reason": "ladder"})
	m := renderOne(t, e, rctx{})
	if !strings.Contains(m.Body, "cause: the pull found no newer image for web:1 (still aaaaaaaaaaaa)") || !strings.Contains(m.Title, "giving up") {
		t.Fatalf("%v", m)
	}
}

// A drift block on a shared tag says which container moved the tag and when, on a line of its own, and the reason line
// does not carry the same sentence twice.
func TestDriftBlockNamesWhoMovedTheTag(t *testing.T) {
	findings := "image: image web:1 now resolves to bbbbbbbbbbbb, but this container was created from aaaaaaaaaaaa: the tag moved (whalenurse moved this tag: nas/other/web/1 pulled it at 2026-10-19T10:30:00Z)"
	e := ev(engine.DriftBlocked, webKey, "x", map[string]string{"findings": findings, "moved_by": "nas/other/web/1", "moved_at": "2026-10-19T10:30:00Z", "moved_ref": "web:1"})
	m := renderOne(t, e, rctx{})
	if !strings.Contains(m.Body, "tag moved by whalenurse's pull rung: other/web at 2026-10-19 10:30 UTC") {
		t.Fatalf("%s", m.Body)
	}
	if strings.Count(m.Body, "whalenurse moved this tag") != 0 {
		t.Fatalf("the guard's sentence is replaced by the line: %s", m.Body)
	}
	if !strings.Contains(m.Body, "the tag moved") || !strings.Contains(m.Body, "whalenurse unsuspend stack/web") {
		t.Fatalf("%s", m.Body)
	}
	if utf8.RuneCountInString(m.Body) > MaxBody {
		t.Fatalf("%d runes", utf8.RuneCountInString(m.Body))
	}
}

func TestUpgradePostCheckStaysInTheLog(t *testing.T) {
	if _, _, ok := render(ev(engine.UpgradePostCheck, webKey, "x", nil), rctx{}); ok {
		t.Fatal("UpgradePostCheck is not notified")
	}
}

// One upgrade is said once per incident, the same dedupe as everything else.
func TestUpgradeIsDedupedPerIncident(t *testing.T) {
	e := ev(engine.TagMoved, webKey, "x", movedAttrs(stackProject(), "web:1", true))
	e.Attrs[engine.AttrIncident] = "2026-10-19T10:00:00Z"
	_, d1, _ := render(e, rctx{})
	_, d2, _ := render(e, rctx{})
	e.Attrs[engine.AttrIncident] = "2026-10-19T11:00:00Z"
	_, d3, _ := render(e, rctx{})
	if d1 == "" || d1 != d2 || d1 == d3 {
		t.Fatalf("%q %q %q", d1, d2, d3)
	}
}

// With the real, long image reference of a drift block that names the mover, the unsuspend command is still in the body
// (found by CI: the hint and the reason had pushed it out). The hint is what gives way.
func TestDriftBlockWithAMoverKeepsTheUnsuspendCommand(t *testing.T) {
	ref := "127.0.0.1:32772/whalenurse-test/shared:latest"
	findings := "image: image " + ref + " now resolves to c42447ac2b0e, but this container was created from dda213a6a6b0: the tag moved (whalenurse moved this tag: itest/whalenurse-test-pull-a-015936/app/1 pulled it at 2026-10-19T07:45:02Z)"
	e := ev(engine.DriftBlocked, "itest/whalenurse-test-pull-b-3a0f41/api/1", "x", map[string]string{"findings": findings, "moved_by": "itest/whalenurse-test-pull-a-015936/app/1", "moved_at": "2026-10-19T07:45:02Z"})
	m := renderOne(t, e, rctx{})
	if !strings.Contains(m.Body, "whalenurse unsuspend whalenurse-test-pull-b-3a0f41/api") || !strings.Contains(m.Body, "tag moved by whalenurse's pull rung: whalenurse-test-pull-a-015936/app at 2026-10-19 07:45 UTC") {
		t.Fatalf("%s", m.Body)
	}
	if utf8.RuneCountInString(m.Body) > MaxBody {
		t.Fatalf("%d runes", utf8.RuneCountInString(m.Body))
	}
}

// Rung 2 on a pull-enabled container that took a tag someone else moved: the same "upgraded during heal" message, with the
// rollback, and it says that someone else moved the tag.
func TestUpgradedDuringRung2HasTheRollbackAndSaysWhoMovedTheTag(t *testing.T) {
	a := movedAttrs(stackProject(), "web:1", true)
	m := renderOne(t, ev(engine.Upgraded, webKey, "x", a), rctx{})
	if !strings.Contains(m.Title, "upgraded during heal") || !m.Critical {
		t.Fatalf("%v", m)
	}
	for _, want := range []string{"web:1 aaaaaaaaaaaa -> bbbbbbbbbbbb", "moved by someone else", a["rollback_tag"], a["rollback_up"]} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("lacks %q:\n%s", want, m.Body)
		}
	}
	if strings.Contains(m.Body, "every stack that shares this tag") {
		t.Errorf("whalenurse did not move this tag:\n%s", m.Body)
	}
	// Said once per incident, like the other.
	e := ev(engine.Upgraded, webKey, "x", a)
	e.Attrs[engine.AttrIncident] = "2026-10-19T10:00:00Z"
	_, d1, _ := render(e, rctx{})
	_, d2, _ := render(e, rctx{})
	if d1 == "" || d1 != d2 {
		t.Fatalf("%q %q", d1, d2)
	}
}

// The sidecar's rollback is in the same message: one up line that names the parent and the sidecars, whole.
func TestUpgradeMessageCarriesTheSidecarsRollback(t *testing.T) {
	p := stackProject()
	tag, up := wncompose.RollbackCommands(p, "web:1", oldID, "side", "side2")
	a := movedAttrs(p, "web:1", true)
	a["rollback_tag"], a["rollback_up"], a["sidecars"] = tag, up, "side,side2"
	m := renderOne(t, ev(engine.TagMoved, webKey, "x", a), rctx{})
	for _, want := range []string{"sidecars on the same image, recreated with it: side, side2", tag + "\n" + up} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("lacks %q:\n%s", want, m.Body)
		}
	}
	if !strings.HasSuffix(up, "up -d --no-deps --pull never --no-build web side side2") {
		t.Fatalf("%s", up)
	}
	if utf8.RuneCountInString(m.Body) > MaxBodyPinned {
		t.Fatalf("%d runes", utf8.RuneCountInString(m.Body))
	}
}

// The giving-up message for a pull rung that does not apply says so as its cause.
func TestPullRungDoesNotApplyIsTheGivingUpCause(t *testing.T) {
	e := ev(engine.Suspended, webKey, "every rung ran or was unavailable: pull rung does not apply (pull_policy never)", map[string]string{"reason": "ladder"})
	m := renderOne(t, e, rctx{})
	if !strings.Contains(m.Body, "cause: pull rung does not apply (pull_policy never)") || !strings.Contains(m.Title, "giving up") {
		t.Fatalf("%v", m)
	}
}

// The rollback lines are sent byte for byte: whitespace inside a quoted path is part of the path.
func TestRollbackLinesAreSentByteForByte(t *testing.T) {
	for _, dir := range []string{"/opt/My  Stacks/p", "/opt/My\tStacks/p", "/opt/My Stacks/p"} {
		p := wncompose.Project{Name: "p", WorkingDir: dir, ConfigFiles: []string{dir + "/compose.yaml"}, Service: "web"}
		a := movedAttrs(p, "web:1", true)
		m := renderOne(t, ev(engine.TagMoved, "nas/p/web/1", "x", a), rctx{})
		if !strings.HasSuffix(m.Body, "\n"+a["rollback_tag"]+"\n"+a["rollback_up"]) {
			t.Errorf("%q: the rollback was changed:\n%q", dir, m.Body)
		}
	}
}

// worstCaseUpgrade is the "upgraded during heal" event with the longest names the commands can reasonably have.
func worstCaseUpgrade(t *testing.T) engine.Event {
	t.Helper()
	p := wncompose.Project{
		Name: "whalenurse-" + strings.Repeat("p", 29), WorkingDir: "/srv/docker/stacks/" + strings.Repeat("d", 41),
		ConfigFiles: []string{"/srv/docker/stacks/" + strings.Repeat("d", 41) + "/" + strings.Repeat("c", 30) + ".yaml"}, Service: strings.Repeat("s", 30),
	}
	ref := "registry.example.com:5000/" + strings.Repeat("r", 53)
	a := movedAttrs(p, ref, false)
	a["rollback_tag"], a["rollback_up"] = wncompose.RollbackCommands(p, ref, oldID, strings.Repeat("a", 30))
	a["sidecars"] = strings.Repeat("a", 30)
	a["error"] = strings.Repeat("compose says something very long and noisy about why this failed; ", 12)
	e := ev(engine.TagMoved, "nas/"+p.Name+"/"+p.Service+"/1", "x", a)
	e.Container = p.Name + "-" + p.Service + "-1"
	return e
}

// A rung-2 upgrade whose recreate failed after compose had replaced the container: its own headline (rung 2 pulled
// nothing), the error, and the rollback.
func TestUpgradedThatFailedSaysTheRecreateFailedNotThePull(t *testing.T) {
	m := renderOne(t, ev(engine.Upgraded, webKey, "x", movedAttrs(stackProject(), "web:1", false)), rctx{})
	for _, want := range []string{"took the moved tag, but the recreate failed", "error: port is already allocated", "docker image tag " + oldID} {
		if !strings.Contains(m.Title+m.Body, want) {
			t.Errorf("lacks %q: %v", want, m)
		}
	}
	if strings.Contains(m.Title, "pulled") || utf8.RuneCountInString(m.Title) > MaxTitle {
		t.Errorf("%v", m)
	}
}

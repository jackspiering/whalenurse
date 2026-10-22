package notify

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackspiering/whalenurse/internal/engine"
)

func renderOne(t *testing.T, e engine.Event, c rctx) Message {
	t.Helper()
	m, _, ok := render(e, c)
	if !ok {
		t.Fatalf("%s is not rendered", e.Type)
	}
	return m
}

func TestTemplates(t *testing.T) {
	long := strings.Repeat("compose says something very long and noisy about why this failed; ", 12)
	cases := []struct {
		name     string
		e        engine.Event
		c        rctx
		title    []string
		body     []string
		critical bool
	}{
		{"action ok", ev(engine.ActionResult, webKey, "restart ok", map[string]string{"action": "restart", "rung": "restart", "ok": "true"}),
			rctx{reason: "container is unhealthy"}, []string{"nas: stack-web-1 restarted"}, []string{webKey, "why: container is unhealthy", "rung: restart"}, false},
		{"action failed", ev(engine.ActionResult, webKey, "recreate failed: exit status 1: no such image", map[string]string{"action": "recreate", "rung": "recreate", "ok": "false"}),
			rctx{}, []string{"recreate failed"}, []string{webKey, "error: exit status 1: no such image"}, false},
		{"action dry run", ev(engine.ActionResult, webKey, "dry-run: would restart", map[string]string{"action": "restart", "rung": "restart", "ok": "true", "dry_run": "true"}),
			rctx{dryRun: true}, []string{"[dry-run] nas: stack-web-1 would restart"}, []string{"[dry-run] " + webKey}, false},
		{"recovered", ev(engine.Recovered, webKey, "container has been healthy for 10m0s", nil),
			rctx{}, []string{"nas: stack-web-1 recovered"}, []string{webKey, "healthy for 10m0s"}, false},
		{"giving up", ev(engine.Suspended, webKey, "every rung ran or was unavailable: container is unhealthy", map[string]string{"reason": "ladder"}),
			rctx{}, []string{"giving up, SUSPENDED"}, []string{webKey, "cause: container is unhealthy", "whalenurse unsuspend stack/web"}, true},
		{"flapping", ev(engine.Suspended, webKey, "flapping: 4 incidents within 24h0m0s (limit 3): container is unhealthy", map[string]string{"reason": "flapping"}),
			rctx{}, []string{"flapping, SUSPENDED"}, []string{webKey, "4 incidents within 24h0m0s", "whalenurse unsuspend stack/web"}, true},
		{"drift blocked with the compose-upgrade hint", ev(engine.DriftBlocked, webKey, "recreate skipped by the drift guard: x",
			map[string]string{"findings": "compose would recreate this container (its dry-run says: Recreate): the compose file, an env file or the image changed since it was created (compose upgraded from 5.2.0 to 5.6.0 since this container was created, so this may be a changed hash layout rather than an edit)"}),
			rctx{}, []string{"recreate blocked by the drift guard"}, []string{webKey, "compose upgraded 5.2.0 to 5.6.0", "hint:", "docker compose up -d web", "whalenurse unsuspend stack/web"}, true},
		{"drift blocked, bundled compose", ev(engine.DriftBlocked, webKey, "x", map[string]string{"findings": "can't verify drift (compose 5.2.0 on host vs 5.6.0 in whalenurse): the container was made by another compose release"}),
			rctx{}, []string{"blocked"}, []string{"can't verify drift", "/host/docker-compose"}, true},
		{"drift blocked, moved tag", ev(engine.DriftBlocked, webKey, "x", map[string]string{"findings": "image busybox:1.37 now resolves to aaa, but this container was created from bbb: the tag moved"}),
			rctx{}, []string{"blocked"}, []string{"the tag moved", "docker compose up -d web"}, true},
		{"drift warned", ev(engine.DriftWarned, webKey, "x", map[string]string{"findings": "the tag moved"}),
			rctx{}, []string{"recreating despite drift"}, []string{webKey, "found: the tag moved"}, false},
		{"notify only with dependents", ev(engine.NotifyOnly, webKey, "stopped by the host shutdown, restart policy \"no\": left down", map[string]string{"class": "exited-host-stopped", "dependents": "stack-sidecar-1,stack-other-1"}),
			rctx{}, []string{"needs attention"}, []string{webKey, "left down", "left down with it: stack-sidecar-1, stack-other-1"}, true},
		{"needs recreate", ev(engine.NeedsRecreate, webKey, "needs a recreate; compose is not available", map[string]string{"class": "stale"}),
			rctx{}, []string{"needs a recreate"}, []string{webKey, "compose is not available"}, true},
		{"version gate", engine.Event{Host: testHost, Type: engine.RungsDisabled, Message: "rungs 2 and 3 are disabled: Engine 27.0 is below 28.1"},
			rctx{}, []string{"nas: recreate and pull rungs are disabled"}, []string{"host nas", "Engine 27.0 is below 28.1"}, true},
		{"paused", engine.Event{Host: testHost, Key: "nas/stack/web", Type: engine.Paused, Message: "paused stack/web for 2h0m0s (until 14:00)"},
			rctx{}, []string{"nas: paused stack/web"}, []string{"nas/stack/web", "for 2h0m0s"}, false},
		{"resumed", engine.Event{Host: testHost, Key: "nas/stack/web", Type: engine.Resumed, Message: "resumed stack/web: the pause expired"},
			rctx{}, []string{"nas: resumed stack/web"}, []string{"nas/stack/web", "expired"}, false},
		{"unsuspended by recovery", ev(engine.Unsuspended, webKey, "container returned to healthy", map[string]string{"reason": "healthy"}),
			rctx{}, []string{"nas: stack-web-1 unsuspended"}, []string{webKey, "returned to healthy"}, false},
		{"unsuspended by command", ev(engine.Unsuspended, webKey, "unsuspended by whalenurse unsuspend", map[string]string{"reason": "command"}),
			rctx{}, []string{"unsuspended"}, []string{webKey, "by whalenurse unsuspend"}, false},
		{"action rate limit", ev(engine.RateLimited, webKey, "rate limit of 10 actions/hour reached; will retry", nil),
			rctx{}, []string{"nas: action rate limit reached"}, []string{webKey, "10 actions/hour", "WHALENURSE_RATE_LIMIT", "once an hour"}, true},
		{"non-compose container", ev(engine.Suspended, "nas/plainbox", "every rung ran or was unavailable: container is unhealthy", map[string]string{"reason": "ladder"}),
			rctx{}, []string{"giving up"}, []string{"nas/plainbox", "whalenurse unsuspend plainbox"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := renderOne(t, tc.e, tc.c)
			for _, w := range tc.title {
				if !strings.Contains(m.Title, w) {
					t.Errorf("title %q lacks %q", m.Title, w)
				}
			}
			for _, w := range tc.body {
				if !strings.Contains(m.Body, w) {
					t.Errorf("body %q lacks %q", m.Body, w)
				}
			}
			if m.Critical != tc.critical {
				t.Errorf("critical = %v, want %v", m.Critical, tc.critical)
			}
			// Every message names the host and the key (or the scope), and is a phone push, not a log.
			if !strings.Contains(m.Title, testHost) || !strings.Contains(m.Body, testHost) {
				t.Errorf("host missing: %v", m)
			}
			if utf8.RuneCountInString(m.Title) > MaxTitle || utf8.RuneCountInString(m.Body) > MaxBody {
				t.Errorf("too long: %d / %d runes: %v", utf8.RuneCountInString(m.Title), utf8.RuneCountInString(m.Body), m)
			}
			if tc.c.dryRun && (!strings.HasPrefix(m.Title, DryRunMarker) || !strings.HasPrefix(m.Body, DryRunMarker)) {
				t.Errorf("dry-run message is not marked in title and body: %v", m)
			}
		})
	}

	// Worst case: every free-text field at length. The limits hold and the key line survives.
	worst := []engine.Event{
		ev(engine.ActionResult, webKey, "recreate failed: "+long, map[string]string{"action": "recreate", "rung": "recreate", "ok": "false"}),
		ev(engine.Suspended, webKey, "every rung ran or was unavailable: "+long, map[string]string{"reason": "ladder"}),
		ev(engine.DriftBlocked, webKey, "x", map[string]string{"findings": long + "compose upgraded from 5.2.0 to 5.6.0"}),
		ev(engine.NotifyOnly, webKey, long, map[string]string{"class": "x", "dependents": strings.Repeat("stack-sidecar-1,", 30)}),
		ev(engine.Recovered, webKey, long, nil),
	}
	for _, e := range worst {
		m := renderOne(t, e, rctx{dryRun: true, reason: long})
		if utf8.RuneCountInString(m.Title) > MaxTitle || utf8.RuneCountInString(m.Body) > MaxBody {
			t.Errorf("%s: %d / %d runes", e.Type, utf8.RuneCountInString(m.Title), utf8.RuneCountInString(m.Body))
		}
		if !strings.Contains(m.Body, "[dry-run] "+webKey+"\n") {
			t.Errorf("%s: the key line was cut: %q", e.Type, m.Body)
		}
		if strings.Count(m.Body, "\n") > 6 {
			t.Errorf("%s: too many lines for a push: %q", e.Type, m.Body)
		}
	}
}

func TestEventsThatAreNotListedAreNotNotified(t *testing.T) {
	for _, typ := range []engine.EventType{
		engine.IncidentOpened, engine.IncidentClosed, engine.ActionStarted, engine.RungUnavailable,
		engine.WaitingForParent, engine.ActionRefused, engine.EventAttributed, engine.ManualStopMarked,
		engine.ManualStopCleared, engine.MarkersQuarantined, engine.RestoreWindowOpened, engine.RestoreEvent, engine.DependentPostCheck,
	} {
		if _, _, ok := render(ev(typ, webKey, "x", nil), rctx{}); ok {
			t.Errorf("%s should stay in the log", typ)
		}
	}
}

// The Suspended(drift) right after its DriftBlocked is folded by the dispatcher (TestADriftBlockedSuspensionIsExactlyOneMessage).
// The one render sees is a re-suspension later in the incident: a giving-up message that keeps the drift context.
func TestADriftSuspensionAgainIsAGivingUpMessage(t *testing.T) {
	m := renderOne(t, ev(engine.Suspended, webKey, "failed again before the incident closed (drift): unhealthy", map[string]string{"reason": "drift"}), rctx{dryRun: true})
	if !strings.HasSuffix(m.Title, "giving up, SUSPENDED") || !m.Critical || !strings.Contains(m.Body, "cause: unhealthy again; the drift guard blocked the recreate") ||
		!strings.Contains(m.Body, "whalenurse unsuspend stack/web") || utf8.RuneCountInString(m.Body) > MaxBody || utf8.RuneCountInString(m.Title) > MaxTitle {
		t.Fatalf("%v", m)
	}
}

func TestALongContainerNameIsCutNotTheHeadline(t *testing.T) {
	e := ev(engine.Suspended, "nas/a-very-long-project-name/an-equally-long-service-name-here/1", "every rung ran or was unavailable: unhealthy", map[string]string{"reason": "ladder"})
	e.Container = "a-very-long-project-name-an-equally-long-service-name-here-1"
	m := renderOne(t, e, rctx{dryRun: true})
	if !strings.HasSuffix(m.Title, "giving up, SUSPENDED") || utf8.RuneCountInString(m.Title) > MaxTitle || !strings.HasPrefix(m.Title, "[dry-run] nas: ") {
		t.Fatalf("%q", m.Title)
	}
}

// A long host never cuts the headline: the container name gives way first, then the host. Every title ends with its whole
// headline, for host IDs up to 32 runes (a container's 12-hex ID is the default), with and without the dry-run marker.
func TestTheHeadlineSurvivesALongHost(t *testing.T) {
	cases := []struct {
		e        engine.Event
		headline string
	}{
		{ev(engine.DriftBlocked, webKey, "x", map[string]string{"findings": "the tag moved"}), "recreate blocked by the drift guard, SUSPENDED"},
		{ev(engine.DriftWarned, webKey, "x", map[string]string{"findings": "the tag moved"}), "recreating despite drift (WHALENURSE_DRIFT_GUARD=warn)"},
		{ev(engine.Suspended, webKey, "every rung ran or was unavailable: unhealthy", map[string]string{"reason": "ladder"}), "giving up, SUSPENDED"},
		{ev(engine.NeedsRecreate, webKey, "x", nil), "needs a recreate that is not available"},
		{ev(engine.TagMoved, webKey, "x", map[string]string{"ref": "web:1", "new": newID, "ok": "false"}), "pulled a newer image, but the recreate failed"},
		{ev(engine.Upgraded, webKey, "x", map[string]string{"ref": "web:1", "new": newID, "ok": "false"}), "took the moved tag, but the recreate failed"},
		{ev(engine.RungsDisabled, "", "x", nil), "recreate and pull rungs are disabled"},
	}
	for _, host := range []string{"nas", "0123456789ab", "homelab-nas", strings.Repeat("h", 32)} {
		for _, dry := range []bool{false, true} {
			for _, c := range cases {
				e := c.e
				e.Host = host
				e.Container = "a-rather-long-project-name-web-1"
				m := renderOne(t, e, rctx{dryRun: dry})
				if utf8.RuneCountInString(m.Title) > MaxTitle || !strings.HasSuffix(m.Title, c.headline) {
					t.Errorf("host %q dry-run %v: %q (%d runes)", host, dry, m.Title, utf8.RuneCountInString(m.Title))
				}
				if dry && !strings.HasPrefix(m.Title, DryRunMarker) {
					t.Errorf("the marker comes first: %q", m.Title)
				}
			}
			// A pause names its scope after the verb; a long scope is what gives way, never the verb.
			p := ev(engine.Paused, host+"/a-rather-long-project-name/"+strings.Repeat("s", 40), "paused for 2h", nil)
			p.Host = host
			if m := renderOne(t, p, rctx{dryRun: dry}); utf8.RuneCountInString(m.Title) > MaxTitle || !strings.Contains(m.Title, ": paused a-rather") {
				t.Errorf("%q", m.Title)
			}
		}
	}
}

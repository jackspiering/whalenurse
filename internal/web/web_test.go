package web

import (
	"github.com/jackspiering/whalenurse/internal/compose"
	"go/build"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackspiering/whalenurse/internal/api"
)

// "The dashboard uses only this API": the package may render the documents of internal/api and nothing from the
// healer itself, so a page cannot show anything the API's output rules did not clean.
func TestWebImportsOnlyTheAPI(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if strings.HasPrefix(imp, "github.com/jackspiering/whalenurse/") && imp != "github.com/jackspiering/whalenurse/internal/api" {
			t.Errorf("internal/web imports %s: it may import internal/api only", imp)
		}
	}
	if len(pkg.TestImports) > 0 {
		for _, imp := range pkg.TestImports {
			if strings.Contains(imp, "internal/engine") || strings.Contains(imp, "internal/store") || strings.Contains(imp, "internal/daemon") {
				t.Errorf("its tests import %s", imp)
			}
		}
	}
}

func TestLadderTextSeparatesRunFromSkipped(t *testing.T) {
	opened := time.Now()
	c := api.ContainerDoc{
		Ladder: []string{"restart", "recreate", "pull"},
		Healer: api.HealerDoc{IncidentOpened: opened, RungsRun: 2, NextRung: "pull", SkippedRungs: []api.SkippedDoc{{Rung: "recreate", Reason: "x"}}},
	}
	if got, want := ladderPath(c), "restart ✓ → recreate skipped – not available → pull (next)"; got != want {
		t.Errorf("ladderPath = %q, want %q", got, want)
	}
	if got, want := ladder(c), "1 of 3 rungs run, 1 skipped (not available), next: pull"; got != want {
		t.Errorf("ladder = %q, want %q", got, want)
	}
	c.Healer.SkippedRungs = nil
	if got, want := ladderPath(c), "restart ✓ → recreate ✓ → pull (next)"; got != want {
		t.Errorf("ladderPath = %q, want %q", got, want)
	}
	if got := ladderPath(api.ContainerDoc{Ladder: []string{"restart", "recreate"}}); got != "restart → recreate" {
		t.Errorf("no incident: %q", got)
	}
}

func TestExpiryReadsAsTimeLeftAndTheClockTime(t *testing.T) {
	now := time.Now().UTC()
	re := regexp.MustCompile(`^<time datetime="[^"]+" title="[^"]+">(.*)</time>$`)
	text := func(t time.Time) string {
		m := re.FindStringSubmatch(string(expiry(t)))
		if m == nil {
			return "NO MATCH " + string(expiry(t))
		}
		return m[1]
	}
	if got := text(now.Add(2*time.Minute + 20*time.Second)); !regexp.MustCompile(`^2m left \(until \d\d:\d\d\)$`).MatchString(got) && !regexp.MustCompile(`^2m left \(until \d+ \w+ \d\d:\d\d\)$`).MatchString(got) {
		t.Errorf("2 minutes: %q", got)
	}
	in := now.Add(time.Hour + 57*time.Minute + 30*time.Second)
	want := "1h 57m left (until " + in.Format("15:04") + ")"
	if in.YearDay() != now.YearDay() {
		want = "1h 57m left (until " + in.Format("2 Jan 15:04") + ")"
	}
	if got := text(in); got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if got := text(now.Add(50*time.Hour + time.Minute)); !regexp.MustCompile(`^2d 2h left \(until \d+ \w{3} \d\d:\d\d\)$`).MatchString(got) {
		t.Errorf("another day carries its date: %q", got)
	}
	if got := text(now.Add(-3 * time.Minute)); got != "ended 3m ago" {
		t.Errorf("past: %q", got)
	}
	if strings.Contains(string(expiry(now.Add(time.Minute))), "until in") {
		t.Error("the old wording")
	}
	if expiry(time.Time{}) != "" {
		t.Error("zero")
	}
}

func TestMessageSaysDriftInPlainWordsAndKeepsTheRawTextUnderDetails(t *testing.T) {
	raw := "recreate skipped by the drift guard (config: compose would recreate this container (its dry-run says: Recreate, Recreated, Starting, Started): the compose file, an env file or the image changed since it was created): healthcheck reports unhealthy"
	got := string(message(raw))
	if !strings.Contains(got, "compose would recreate this container: the configuration or the image changed since it was created") {
		t.Errorf("plain sentence missing: %s", got)
	}
	plain := got[:strings.Index(got, "<details")]
	if strings.Contains(plain, "Recreated") || strings.Contains(plain, "dry-run says") {
		t.Errorf("the raw words are still in the sentence: %s", plain)
	}
	if !strings.Contains(got, `<details class="raw"><summary>details</summary><code>`) || !strings.Contains(got, "its dry-run says: Recreate, Recreated, Starting, Started") {
		t.Errorf("the raw output must stay under details: %s", got)
	}
	if got := string(message("restart ok <b>x</b>")); got != "restart ok &lt;b&gt;x&lt;/b&gt;" {
		t.Errorf("escaping: %s", got)
	}
}

func TestRelBadgeSaysWhatTheOtherEndIsDoing(t *testing.T) {
	for _, c := range []struct {
		r    api.RelationDoc
		want string
	}{
		{api.RelationDoc{State: "running", Health: "healthy", HealerState: "ok"}, `badge-ok">running, healthy<`},
		{api.RelationDoc{State: "running"}, `badge-ok">running<`},
		{api.RelationDoc{State: "exited"}, `badge-bad">exited<`},
		{api.RelationDoc{State: "running", Health: "unhealthy", HealerState: "SUSPENDED"}, `badge-bad">running, unhealthy · SUSPENDED<`},
		{api.RelationDoc{State: "running", Health: "unhealthy", HealerState: "incident"}, `badge-warn">running, unhealthy · incident<`},
		{api.RelationDoc{State: "running", HealerState: "PAUSED"}, `badge-info">running · PAUSED<`},
	} {
		if got := string(relBadge(c.r)); !strings.Contains(got, c.want) {
			t.Errorf("%+v: %s, want %s", c.r, got, c.want)
		}
	}
	if relBadge(api.RelationDoc{}) != "" {
		t.Error("an unknown state has no badge")
	}
}

func TestInScopeMirrorsTheEnginesRule(t *testing.T) {
	for scope, want := range map[string]bool{"p": true, "p/web": true, "p/web/1": true, "p/db": false, "q": false, "p/web/1/x": false} {
		if got := inScope(scope, "h/p/web/1"); got != want {
			t.Errorf("inScope(%q) = %v", scope, got)
		}
	}
	if inScope("p", "nokey") {
		t.Error("a key without a host")
	}
}

func TestSuspendedTextInPlainWords(t *testing.T) {
	cfg := "recreate skipped by the drift guard (config: compose would recreate this container (its dry-run says: Recreate, Recreated, Starting, Started): the compose file, an env file or the image changed since it was created): healthcheck reports unhealthy"
	img := "recreate skipped by the drift guard (image web:1 now resolves to aaaa, but this container was created from bbbb: the tag moved): healthcheck reports unhealthy"
	hash := "recreate skipped by the drift guard (config: config --hash is h2, the container was created with h1: the compose file or an env file changed): container exited with code 1"
	unv := "recreate skipped by the drift guard (unverified: image web:1 cannot be resolved for linux/amd64: no such image): healthcheck reports unhealthy"
	for _, c := range []struct {
		msg              string
		restarted, short bool
		want             string
	}{
		{cfg, true, false, "Unhealthy; a restart didn't help, and the recreate was blocked because the configuration or image changed since the container was created."},
		{cfg, false, false, "Unhealthy; the recreate was blocked because the configuration or image changed since the container was created."},
		{img, true, false, "Unhealthy; a restart didn't help, and the recreate was blocked because the image tag moved since the container was created."},
		{hash, false, false, "Container exited with code 1; the recreate was blocked because the configuration or image changed since the container was created."},
		{unv, true, false, "Unhealthy; a restart didn't help, and the recreate was blocked because unverified: image web:1 cannot be resolved for linux/amd64: no such image."},
		{cfg, true, true, "Unhealthy; the recreate was blocked by the drift guard, so the container is SUSPENDED."},
	} {
		got, ok := suspendedText(c.msg, c.restarted, c.short)
		if !ok || got != c.want {
			t.Errorf("%q\n got %q\nwant %q", c.msg[:60], got, c.want)
		}
	}
	// Anything else is not rewritten.
	if _, ok := suspendedText("every rung ran or was unavailable: healthcheck reports unhealthy", true, false); ok {
		t.Error("a ladder suspension is not a drift block")
	}
	if got := string(suspendedHTML("every rung ran or was unavailable: x", true)); got != "every rung ran or was unavailable: x" {
		t.Errorf("%s", got)
	}
	// The Suspended row of a timeline is short; any other event keeps its text.
	if got := string(eventText("Suspended", cfg)); strings.Contains(got[:strings.Index(got, "<details")], "changed since") {
		t.Errorf("the short row repeats the drift explanation: %s", got)
	}
	if got := string(eventText("DriftBlocked", "recreate skipped by the drift guard: config: compose would recreate this container (its dry-run says: Recreate): the compose file, an env file or the image changed since it was created")); !strings.Contains(got, "the configuration or the image changed since it was created") {
		t.Errorf("the DriftBlocked row keeps its explanation: %s", got)
	}
}

func TestPauseMessageIsFriendly(t *testing.T) {
	until := time.Now().UTC().Add(2 * time.Hour)
	msg := "paused whalenurse-test-demo/cache for 2h0m0s (until " + until.Format(time.RFC3339) + ")"
	got := string(message(msg))
	clock := until.Format("15:04")
	if until.YearDay() != time.Now().UTC().YearDay() {
		clock = until.Format("2 Jan 15:04")
	}
	if want := "paused whalenurse-test-demo/cache for 2h, until " + clock; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if strings.Contains(got, "details") {
		t.Error("a reworded pause has nothing to fold")
	}
	for in, want := range map[time.Duration]string{2 * time.Hour: "2h", 90 * time.Minute: "1h 30m", 45 * time.Minute: "45m", 90 * time.Second: "1m 30s", 30 * time.Second: "30s", 26 * time.Hour: "26h"} {
		if got := shortDur(in); got != want {
			t.Errorf("shortDur(%v) = %q, want %q", in, got, want)
		}
	}
	// Other text and an unparsable duration are left as they are.
	if got := string(message("resumed web: expired")); got != "resumed web: expired" {
		t.Error(got)
	}
}

func TestBlockedRungHasItsOwnMarker(t *testing.T) {
	c := api.ContainerDoc{
		Ladder: []string{"restart", "recreate", "pull"},
		Healer: api.HealerDoc{IncidentOpened: time.Now(), RungsRun: 1, Suspended: &api.SuspendDoc{Reason: "drift"}},
	}
	if got, want := ladderPath(c), "restart ✓ → recreate ✗ blocked (drift) → pull"; got != want {
		t.Errorf("ladderPath = %q, want %q", got, want)
	}
	if got, want := ladder(c), "1 of 3 rungs run, recreate blocked (drift)"; got != want {
		t.Errorf("ladder = %q, want %q", got, want)
	}
	// Suspended for another reason, or all rungs run: no blocked marker.
	c.Healer.Suspended.Reason = "ladder"
	if strings.Contains(ladderPath(c), "blocked") || blockedRung(c) != "" {
		t.Error("a ladder suspension is not a block")
	}
	c.Healer.Suspended.Reason, c.Healer.RungsRun = "drift", 3
	if blockedRung(c) != "" {
		t.Error("nothing is left to block")
	}
}

// The page's copy of the shell quoting gives what compose's does, for ordinary and hostile names.
func TestShellWordMatchesCompose(t *testing.T) {
	for _, s := range []string{"", "p/web", "stack-1/web_2.x", "p$(id)/web", "w;id", "it's", "a b", "`x`", "a\tb", "-f", "p|q", "ünï/cødé"} {
		if got, want := shellWord(s), compose.ShellWord(s); got != want {
			t.Errorf("%q: %q, compose says %q", s, got, want)
		}
	}
}

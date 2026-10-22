package compose

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeImages map[string]string

func (f fakeImages) ImageID(_ context.Context, ref, arch string) (string, error) {
	if id, ok := f[ref+"|"+arch]; ok {
		return id, nil
	}
	return "", errors.New("no such image: " + ref)
}

const (
	imgOld = "sha256:aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	imgNew = "sha256:bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
)

func guardIn(refHolder bool) GuardInput {
	return GuardInput{
		Project: proj(), ContainerName: "wn-web-1", ImageRef: "web:1", Arch: "amd64", RefHolder: refHolder,
		Labels: map[string]string{LabelConfigHash: "h1", LabelImage: imgOld, LabelVersion: "5.6.0"},
	}
}

func causes(fs []Finding) string {
	var s []string
	for _, f := range fs {
		s = append(s, f.Check)
	}
	return strings.Join(s, ",")
}

func TestGuardCleanContainerIsNotBlocked(t *testing.T) {
	f := NewFake()
	g := Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}
	if got := g.Check(context.Background(), guardIn(false)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// The oracle found nothing, so the explicit image comparison is not needed.
	if f.Count("hash") != 0 {
		t.Fatal("the hash fallback ran although the oracle answered")
	}
}

func TestGuardOracleDrift(t *testing.T) {
	// compose would recreate, and the tag has not moved: config drift.
	f := NewFake()
	f.OracleFn = func(Project, string) (Oracle, error) { return Recreated("wn-web-1"), nil }
	g := Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}
	got := g.Check(context.Background(), guardIn(false))
	if causes(got) != CheckConfig || !strings.Contains(got[0].Cause, "Recreate, Recreated") {
		t.Fatalf("%v", got)
	}
	// ... and the tag moved: the finding says image, once.
	g.Images = fakeImages{"web:1|amd64": imgNew}
	got = g.Check(context.Background(), guardIn(false))
	if causes(got) != CheckImage || !strings.Contains(got[0].Cause, "bbbbbbbbbbbb") || !strings.Contains(got[0].Cause, "aaaaaaaaaaaa") {
		t.Fatalf("%v", got)
	}
	// The image cannot be resolved when the oracle already blocks: the oracle's finding stands, no unverified noise.
	g.Images = fakeImages{}
	got = g.Check(context.Background(), guardIn(false))
	if causes(got) != CheckConfig {
		t.Fatalf("%v", got)
	}
}

func TestGuardHashFallback(t *testing.T) {
	f := NewFake() // the default oracle is unparseable? no: make it so
	f.OracleFn = func(Project, string) (Oracle, error) { return Oracle{}, ErrUnparseable }
	g := Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}
	f.HashFn = func(Project) (string, error) { return "h1", nil }
	if got := g.Check(context.Background(), guardIn(false)); len(got) != 0 {
		t.Fatalf("matching hash: %v", got)
	}
	f.HashFn = func(Project) (string, error) { return "h2", nil }
	got := g.Check(context.Background(), guardIn(false))
	if causes(got) != CheckConfig || !strings.Contains(got[0].Cause, "h2") {
		t.Fatalf("%v", got)
	}
	// The hash covers config only: after the fallback the image is compared explicitly.
	f.HashFn = func(Project) (string, error) { return "h1", nil }
	g.Images = fakeImages{"web:1|amd64": imgNew}
	if got = g.Check(context.Background(), guardIn(false)); causes(got) != CheckImage {
		t.Fatalf("%v", got)
	}
	// Neither works: unverified, which blocks like drift.
	f.HashFn = func(Project) (string, error) { return "", errors.New("boom") }
	g.Images = fakeImages{"web:1|amd64": imgOld}
	got = g.Check(context.Background(), guardIn(false))
	if causes(got) != CheckUnverified || !strings.Contains(got[0].Cause, "boom") {
		t.Fatalf("%v", got)
	}
	// An oracle that failed for another reason is reported and falls back too.
	f.OracleFn = func(Project, string) (Oracle, error) { return Oracle{}, errors.New("yaml: line 3") }
	f.HashFn = func(Project) (string, error) { return "", errors.New("yaml: line 3") }
	if got = g.Check(context.Background(), guardIn(false)); causes(got) != CheckUnverified || !strings.Contains(got[0].Cause, "dry-run failed") {
		t.Fatalf("%v", got)
	}
	// No hash label: cannot compare.
	in := guardIn(false)
	delete(in.Labels, LabelConfigHash)
	f.HashFn = func(Project) (string, error) { return "h1", nil }
	if got = g.Check(context.Background(), in); causes(got) != CheckUnverified || !strings.Contains(got[0].Cause, LabelConfigHash) {
		t.Fatalf("%v", got)
	}
}

func TestGuardReferenceHolderSkipsCheck1(t *testing.T) {
	f := NewFake()
	f.OracleFn = func(Project, string) (Oracle, error) { return Recreated("wn-web-1"), nil } // expected after a parent recreate
	f.HashFn = func(Project) (string, error) { return "never-matches", nil }
	g := Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}
	if got := g.Check(context.Background(), guardIn(true)); len(got) != 0 {
		t.Fatalf("a dependent must not be blocked by the oracle or the hash: %v", got)
	}
	if f.Count("dry-run")+f.Count("hash") != 0 {
		t.Fatal("neither the oracle nor the hash may run for a reference holder")
	}
	// It still gets the explicit image comparison.
	g.Images = fakeImages{"web:1|amd64": imgNew}
	if got := g.Check(context.Background(), guardIn(true)); causes(got) != CheckImage {
		t.Fatalf("%v", got)
	}
	// ... and an image that cannot be resolved is a failure.
	g.Images = fakeImages{}
	if got := g.Check(context.Background(), guardIn(true)); causes(got) != CheckUnverified || !strings.Contains(got[0].Cause, "web:1") {
		t.Fatalf("%v", got)
	}
	// A container without the image label cannot be verified.
	in := guardIn(true)
	delete(in.Labels, LabelImage)
	g.Images = fakeImages{"web:1|amd64": imgOld}
	if got := g.Check(context.Background(), in); causes(got) != CheckUnverified {
		t.Fatalf("%v", got)
	}
}

func TestGuardVersionSkewOnlyForTheBundledCompose(t *testing.T) {
	drift := func(Project, string) (Oracle, error) { return Recreated("wn-web-1"), nil }
	in := guardIn(false)
	in.Labels[LabelVersion] = "5.2.0"

	host := NewFake() // 5.6.0 from the host plugin: the labels say 5.2.0, but only the bundled copy is checked
	host.OracleFn = drift
	if got := (Guard{Compose: host, Images: fakeImages{"web:1|amd64": imgOld}}).Check(context.Background(), in); causes(got) != CheckConfig {
		t.Fatalf("host plugin: %v", got)
	}

	bundled := NewFake()
	bundled.Bin = Binary{Path: BundledPath, Source: SourceBundled, Version: Version{5, 6, 0, ""}}
	bundled.OracleFn = drift
	g := Guard{Compose: bundled, Images: fakeImages{"web:1|amd64": imgOld}}
	got := g.Check(context.Background(), in)
	if causes(got) != CheckSkew || !strings.Contains(got[0].Cause, "can't verify drift (compose 5.2.0 on host vs 5.6.0 in whalenurse)") {
		t.Fatalf("%v", got)
	}
	// The same release: it is drift.
	in.Labels[LabelVersion] = "5.6.0"
	if got = g.Check(context.Background(), in); causes(got) != CheckConfig {
		t.Fatalf("%v", got)
	}
	// Different releases but nothing changed: no finding at all.
	in.Labels[LabelVersion] = "5.2.0"
	bundled.OracleFn = nil
	if got = g.Check(context.Background(), in); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// A real image change is reported as such even under skew.
	bundled.OracleFn = drift
	g.Images = fakeImages{"web:1|amd64": imgNew}
	if got = g.Check(context.Background(), in); causes(got) != CheckImage {
		t.Fatalf("%v", got)
	}
}

// Compose before v5.5.0 records the image (index) ID in the label on the containerd store, where later releases record the
// platform manifest digest: the v5.2.0 leg of the CI matrix found it. Both are the image the container was created from.
func TestGuardAcceptsTheIndexIDInTheLabel(t *testing.T) {
	f := NewFake()
	in := guardIn(true)
	in.Labels[LabelImage] = "sha256:index00000011111111111111111111111111111111111111111111111111111111"
	g := Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld, "web:1|": "sha256:index00000011111111111111111111111111111111111111111111111111111111"}}
	if got := g.Check(context.Background(), in); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// A moved tag changes both IDs, so it is still caught.
	g.Images = fakeImages{"web:1|amd64": imgNew, "web:1|": "sha256:index2222222222222222222222222222222222222222222222222222222222222222"}
	if got := g.Check(context.Background(), in); causes(got) != CheckImage {
		t.Fatalf("%v", got)
	}
	// And a label that matches neither is drift.
	in.Labels[LabelImage] = "sha256:somethingelse"
	g.Images = fakeImages{"web:1|amd64": imgOld, "web:1|": "sha256:index00000011111111111111111111111111111111111111111111111111111111"}
	if got := g.Check(context.Background(), in); causes(got) != CheckImage {
		t.Fatalf("%v", got)
	}
}

func TestFindingsThatWarnMayOverride(t *testing.T) {
	for check, want := range map[string]bool{CheckConfig: true, CheckImage: true, CheckSkew: false, CheckUnverified: false} {
		if (Finding{Check: check}).Verified() != want {
			t.Errorf("%s verified = %v", check, !want)
		}
	}
}

// An upgraded host plugin is information in the drift message, not a block of its own (plan v3.6).
func TestGuardNotesAnUpgradedHostPlugin(t *testing.T) {
	f := NewFake() // host plugin 5.6.0
	f.OracleFn = func(_ Project, name string) (Oracle, error) { return Recreated(name), nil }
	in := guardIn(false)
	in.Labels[LabelVersion] = "5.2.0"
	g := Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}
	got := g.Check(context.Background(), in)
	if len(got) != 1 || got[0].Check != CheckConfig || !got[0].Verified() ||
		!strings.Contains(got[0].Cause, "compose upgraded from 5.2.0 to 5.6.0 since this container was created") {
		t.Fatalf("%v", got)
	}
	// Same release: no note. No version label: no note.
	in.Labels[LabelVersion] = "5.6.0"
	if got = g.Check(context.Background(), in); len(got) != 1 || strings.Contains(got[0].Cause, "upgraded") {
		t.Fatalf("%v", got)
	}
	delete(in.Labels, LabelVersion)
	if got = g.Check(context.Background(), in); len(got) != 1 || strings.Contains(got[0].Cause, "upgraded") {
		t.Fatalf("%v", got)
	}
	// An upgrade without drift is not a finding at all.
	in.Labels[LabelVersion] = "5.2.0"
	f.OracleFn = nil
	if got = g.Check(context.Background(), in); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// ---- the check before the pull (rung 3, plan §3.7) -------------------------------------------------------------------

func pullIn(refHolder bool) GuardInput {
	in := guardIn(refHolder)
	in.AllowImageMove = true
	return in
}

func TestGuardBeforePull(t *testing.T) {
	ctx := context.Background()
	hashOK := func(Project) (string, error) { return "h1", nil }

	// Nothing changed: no findings, and no image comparison was made.
	f := NewFake()
	if got := (Guard{Compose: f, Images: fakeImages{}}).Check(ctx, pullIn(false)); len(got) != 0 {
		t.Fatalf("%v", got)
	}

	// The compose file was edited: a config finding, as for rung 2. The pull would deploy the edit.
	f = NewFake()
	f.OracleFn = func(Project, string) (Oracle, error) { return Recreated("wn-web-1"), nil }
	f.HashFn = func(Project) (string, error) { return "h2", nil }
	got := (Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}).Check(ctx, pullIn(false))
	if causes(got) != CheckConfig {
		t.Fatalf("%v", got)
	}

	// Only the tag moved (another stack pulled it): the oracle says Recreate, the hash still matches the label, and the
	// image differs. That is what a pull is for, so it is not a finding.
	f = NewFake()
	f.OracleFn = func(Project, string) (Oracle, error) { return Recreated("wn-web-1"), nil }
	f.HashFn = hashOK
	if got := (Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgNew}}).Check(ctx, pullIn(false)); len(got) != 0 {
		t.Fatalf("an image-only move must not block the pull: %v", got)
	}
	// ... but the same move with the hash unreadable cannot be told from an edit: it blocks.
	f.HashFn = func(Project) (string, error) { return "", ErrUnparseable }
	if got := (Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgNew}}).Check(ctx, pullIn(false)); causes(got) != CheckConfig {
		t.Fatalf("%v", got)
	}
	// ... and so does a Recreate that is neither an edit nor a moved tag (a network or a volume changed).
	f.HashFn = hashOK
	if got := (Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}).Check(ctx, pullIn(false)); causes(got) != CheckConfig {
		t.Fatalf("%v", got)
	}

	// The hash fallback (no oracle output) does not run the explicit image comparison in pull mode: a moved tag is allowed.
	f = NewFake()
	f.OracleFn = func(Project, string) (Oracle, error) { return Oracle{}, ErrUnparseable }
	f.HashFn = hashOK
	if got := (Guard{Compose: f, Images: fakeImages{}}).Check(ctx, pullIn(false)); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	// A reference holder has no check 1 and no check 2: nothing to find, and the oracle is not asked.
	f = NewFake()
	if got := (Guard{Compose: f, Images: fakeImages{}}).Check(ctx, pullIn(true)); len(got) != 0 || f.Count("dry-run") != 0 {
		t.Fatalf("%v", got)
	}
}

// Check 3 still applies before a pull: with the bundled compose and a different release than the one that made the
// container, a change compose sees cannot be told from a hash layout that moved.
func TestGuardBeforePullKeepsTheSkewCheck(t *testing.T) {
	f := NewFake()
	f.Bin.Source, f.Bin.Version = SourceBundled, Version{5, 6, 0, ""}
	f.OracleFn = func(Project, string) (Oracle, error) { return Recreated("wn-web-1"), nil }
	f.HashFn = func(Project) (string, error) { return "h2", nil }
	in := pullIn(false)
	in.Labels[LabelVersion] = "5.2.0"
	got := (Guard{Compose: f, Images: fakeImages{"web:1|amd64": imgOld}}).Check(context.Background(), in)
	if causes(got) != CheckSkew {
		t.Fatalf("%v", got)
	}
}

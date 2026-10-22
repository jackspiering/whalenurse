package compose

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// recRunner records every command and answers from a table.
type recRunner struct {
	cmds []Command
	out  func(Command) (Result, error)
}

func (r *recRunner) Run(_ context.Context, c Command) (Result, error) {
	r.cmds = append(r.cmds, c)
	if r.out != nil {
		return r.out(c)
	}
	return Result{}, nil
}

func proj() Project {
	return Project{Name: "whalenurse-test-x", WorkingDir: "/opt/stacks/x", ConfigFiles: []string{"/opt/stacks/x/compose.yaml", "/opt/stacks/x/override.yaml"},
		EnvFiles: []string{"/opt/stacks/x/.env"}, Service: "web"}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]Version{
		"5.6.0": {5, 6, 0}, "v5.6.0": {5, 6, 0}, "Docker Compose version v5.2.0\n": {5, 2, 0}, "29.9.1": {29, 9, 1},
		"28.1.0-rc.1": {28, 1, 0}, "28.1": {28, 1, 0}, "2.38.2": {2, 38, 2}, "  v5.10.3+dev ": {5, 10, 3},
	} {
		if got, ok := ParseVersion(in); !ok || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "dev", "latest", "v5"} {
		if _, ok := ParseVersion(in); ok {
			t.Errorf("ParseVersion(%q) should fail", in)
		}
	}
}

func TestVersionOrder(t *testing.T) {
	if !(Version{5, 1, 9}).Less(MinCompose) || (Version{5, 2, 0}).Less(MinCompose) || (Version{5, 10, 0}).Less(MinCompose) {
		t.Fatal("ordering by component, not by string")
	}
	if !(Version{28, 0, 4}).Less(MinEngine) || (Version{28, 1, 0}).Less(MinEngine) || (Version{29, 9, 1}).Less(MinEngine) {
		t.Fatal("engine minimum is 28.1.0")
	}
}

func TestCheckVersions(t *testing.T) {
	ok := &Binary{Path: "/host/docker-compose", Source: SourceHost, Version: Version{5, 2, 0}}
	for _, tc := range []struct {
		name, engine string
		bin          *Binary
		want         bool
		reason       string
	}{
		{"both at the minimum", "28.1.0", ok, true, ""},
		{"newer", "29.9.1", &Binary{Version: Version{5, 6, 0}}, true, ""},
		{"engine 28.0.4", "28.0.4", ok, false, "Docker Engine 28.0.4 is older than 28.1.0"},
		{"engine 25", "25.0.5", ok, false, "older than 28.1.0"},
		{"compose 5.1.0", "29.9.1", &Binary{Path: "/p", Version: Version{5, 1, 0}}, false, "Docker Compose 5.1.0 (/p) is older than 5.2.0"},
		{"compose v2 on the runner", "29.9.1", &Binary{Path: "/p", Version: Version{2, 38, 2}}, false, "Compose 2.38.2"},
		{"no compose", "29.9.1", nil, false, "no docker compose binary"},
		{"unreadable compose version", "29.9.1", &Binary{Path: "/p", Raw: "garbage"}, false, "cannot be read"},
		{"unreadable engine version", "", ok, false, "cannot be read"},
		{"both old", "25.0.0", &Binary{Version: Version{2, 0, 0}}, false, "Engine 25.0.0"},
	} {
		g := CheckVersions(tc.engine, tc.bin)
		if g.Recreate != tc.want || !strings.Contains(g.Reason, tc.reason) || (tc.want && g.Reason != "") {
			t.Errorf("%s: %+v", tc.name, g)
		}
	}
}

func TestProjectFromLabels(t *testing.T) {
	good := map[string]string{
		LabelProject: "p", LabelService: "web", LabelWorkingDir: "/opt/stacks/p",
		LabelConfigFiles: "/opt/stacks/p/compose.yaml, /opt/stacks/p/o.yaml", LabelEnvFile: "/opt/stacks/p/.env",
	}
	p, err := ProjectFromLabels(good)
	if err != nil || p.Name != "p" || p.Service != "web" || len(p.ConfigFiles) != 2 || p.ConfigFiles[1] != "/opt/stacks/p/o.yaml" || len(p.EnvFiles) != 1 {
		t.Fatalf("%+v %v", p, err)
	}
	for _, drop := range []string{LabelProject, LabelService, LabelWorkingDir, LabelConfigFiles} {
		l := map[string]string{}
		for k, v := range good {
			if k != drop {
				l[k] = v
			}
		}
		if _, err := ProjectFromLabels(l); err == nil || !strings.Contains(err.Error(), drop) {
			t.Errorf("without %s: %v", drop, err)
		}
	}
	// No env file is fine.
	delete(good, LabelEnvFile)
	if p, err := ProjectFromLabels(good); err != nil || len(p.EnvFiles) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
}

// WN-001: the project and service labels become arguments of the compose command line, so a value shaped like a flag
// (an image can ship the label) must never get there. A service of --dry-run=false after `up` overrides the oracle's root
// --dry-run (cobra and pflag: last one wins) and turns it into a real `up` of the whole project.
func TestProjectFromLabelsRefusesFlagShapedIdentity(t *testing.T) {
	good := map[string]string{
		LabelProject: "p", LabelService: "web", LabelWorkingDir: "/opt/stacks/p",
		LabelConfigFiles: "/opt/stacks/p/compose.yaml, /opt/stacks/p/o.yaml", LabelEnvFile: "/opt/stacks/p/.env",
	}
	for _, tc := range []struct{ key, value string }{
		{LabelService, "--dry-run=false"}, {LabelService, "-o/tmp/x"}, {LabelService, "--pull=always"}, {LabelService, "-"},
		{LabelService, "web,db"}, {LabelService, "web:1"}, {LabelService, "we b"}, {LabelService, "web\n"},
		{LabelProject, "-x"}, {LabelProject, "_p"}, {LabelProject, "P"}, {LabelProject, "p.x"}, {LabelProject, "p q"},
	} {
		l := maps.Clone(good)
		l[tc.key] = tc.value
		p, err := ProjectFromLabels(l)
		if err == nil {
			t.Errorf("%s=%q: accepted as %+v", tc.key, tc.value, p)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%q: the error should name the label: %v", tc.key, tc.value, err)
		}
	}
	// What compose itself accepts still passes: a normalized project name, a service name of the compose spec.
	for _, tc := range []struct{ project, service string }{
		{"p", "web"}, {"whalenurse-test-x", "web_1"}, {"0p_q-r", "Web.app-2"}, {"p", "_x"}, {"p", ".x"},
	} {
		l := maps.Clone(good)
		l[LabelProject], l[LabelService] = tc.project, tc.service
		if _, err := ProjectFromLabels(l); err != nil {
			t.Errorf("%s/%s: %v", tc.project, tc.service, err)
		}
	}
}

// No compose command built here may carry a label value as a flag, whatever Project it is handed.
func TestNoLabelValueReachesComposeAsAFlag(t *testing.T) {
	const evil = "--dry-run=false"
	for _, bad := range []func(*Project){
		func(p *Project) { p.Service = evil },
		func(p *Project) { p.Name = evil },
	} {
		r := &recRunner{}
		c := New(Binary{Path: "/x"}, r)
		p := proj()
		bad(&p)
		ctx := context.Background()
		_, _ = c.DryRunUp(ctx, p, "c-web-1")
		_, _ = c.ConfigHash(ctx, p)
		_, _ = c.PullPolicy(ctx, p)
		_ = c.Recreate(ctx, p)
		_ = c.Up(ctx, p)
		_ = c.Pull(ctx, p)
		for _, cmd := range r.cmds {
			for i, a := range cmd.Args {
				if a == "--" {
					break
				}
				if a == evil {
					t.Errorf("argument %d of %q is the label value", i, cmd)
				}
			}
		}
	}
}

func TestReadableNamesTheIdenticalPathMount(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/compose.yaml"
	if err := os.WriteFile(f, []byte("services: {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := Project{Name: "p", WorkingDir: dir, ConfigFiles: []string{f}}
	if err := ok.Readable(nil); err != nil {
		t.Fatal(err)
	}
	missing := Project{Name: "p", WorkingDir: "/nonexistent/stack", ConfigFiles: []string{"/nonexistent/stack/compose.yaml"}, EnvFiles: []string{"/nonexistent/stack/.env"}}
	err := missing.Readable(nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"/nonexistent/stack does not exist", "compose file /nonexistent/stack/compose.yaml", "env file", "identical host path", ":ro"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from: %v", want, err)
		}
	}
	notDir := Project{Name: "p", WorkingDir: f, ConfigFiles: []string{f}}
	if err := notDir.Readable(nil); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("%v", err)
	}
}

// Every command line is built here, so this is where "never down, always --no-deps --pull never --no-build" is pinned.
func TestCommandLines(t *testing.T) {
	r := &recRunner{out: func(c Command) (Result, error) {
		if slices.Contains(c.Args, "config") {
			return Result{Stdout: []byte("web abc123\n")}, nil
		}
		return Result{Stderr: []byte(`{"id":"Container c-web-1","status":"Done","text":"Running"}` + "\n")}, nil
	}}
	c := New(Binary{Path: "/host/docker-compose", Source: SourceHost}, r)
	p := proj()
	ctx := context.Background()
	if err := c.Recreate(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Up(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DryRunUp(ctx, p, "c-web-1"); err != nil {
		t.Fatal(err)
	}
	if h, err := c.ConfigHash(ctx, p); err != nil || h != "abc123" {
		t.Fatalf("%q %v", h, err)
	}
	base := "-p whalenurse-test-x --project-directory /opt/stacks/x -f /opt/stacks/x/compose.yaml -f /opt/stacks/x/override.yaml --env-file /opt/stacks/x/.env "
	want := []string{
		base + "up -d --force-recreate --no-deps --pull never --no-build web",
		base + "up -d --no-deps --pull never --no-build web",
		base + "--dry-run --progress json up -d --no-deps --pull never --no-build web",
		base + "config --hash=web",
	}
	if len(r.cmds) != len(want) {
		t.Fatalf("%d commands", len(r.cmds))
	}
	for i, w := range want {
		if got := strings.Join(r.cmds[i].Args, " "); got != w {
			t.Errorf("command %d:\n got %s\nwant %s", i, got, w)
		}
		if r.cmds[i].Bin != "/host/docker-compose" || r.cmds[i].Dir != "/" {
			t.Errorf("command %d: %+v", i, r.cmds[i])
		}
	}
	for _, cmd := range r.cmds {
		for _, bad := range []string{"down", "rm", "pull", "build", "run", "stop", "kill"} {
			if slices.Contains(cmd.Args, bad) {
				t.Errorf("%s contains %q", cmd, bad)
			}
		}
		if slices.Contains(cmd.Args, "up") && (!slices.Contains(cmd.Args, "--no-deps") || !slices.Contains(cmd.Args, "--no-build") ||
			strings.Join(cmd.Args, " ") == "" || !strings.Contains(strings.Join(cmd.Args, " "), "--pull never")) {
			t.Errorf("an up without --no-deps --pull never --no-build: %s", cmd)
		}
	}
	// A name compose could not have written never reaches the command line (WN-001); it would only ever have been one
	// argument, never shell text, since nothing runs a shell.
	p.Service = "we b;rm -rf /"
	r.cmds = nil
	if err := c.Recreate(ctx, p); err == nil || len(r.cmds) != 0 {
		t.Fatalf("%v %v", err, r.cmds)
	}
}

// The pull is the one command that fetches an image. It is exactly `pull --policy always <service>`: one service, no
// --include-deps, nothing that builds or recreates, and --policy always so a pull_policy of never, build or missing in the
// compose file cannot turn the rung into a silent no-op (compose v5.2.0 and v5.6.0 both honour the flag).
func TestPullCommandLine(t *testing.T) {
	r := &recRunner{}
	c := New(Binary{Path: "/host/docker-compose", Source: SourceHost}, r)
	if err := c.Pull(context.Background(), proj()); err != nil {
		t.Fatal(err)
	}
	want := "-p whalenurse-test-x --project-directory /opt/stacks/x -f /opt/stacks/x/compose.yaml -f /opt/stacks/x/override.yaml --env-file /opt/stacks/x/.env pull --policy always web"
	if len(r.cmds) != 1 || strings.Join(r.cmds[0].Args, " ") != want || r.cmds[0].Dir != "/" {
		t.Fatalf("%v", r.cmds)
	}
	for _, bad := range []string{"down", "rm", "build", "up", "--include-deps", "--ignore-pull-failures"} {
		if slices.Contains(r.cmds[0].Args, bad) {
			t.Errorf("pull contains %q", bad)
		}
	}
}

func TestRollbackCommandsAreExactAndQuoted(t *testing.T) {
	tag, up := RollbackCommands(proj(), "localhost:5000/app:latest", "sha256:"+strings.Repeat("a", 64))
	if want := "docker image tag sha256:" + strings.Repeat("a", 64) + " localhost:5000/app:latest"; tag != want {
		t.Errorf("tag = %q", tag)
	}
	if want := "docker compose -p whalenurse-test-x --project-directory /opt/stacks/x -f /opt/stacks/x/compose.yaml -f /opt/stacks/x/override.yaml --env-file /opt/stacks/x/.env up -d --no-deps --pull never --no-build web"; up != want {
		t.Errorf("up = %q", up)
	}
	// A path with a space or a quote is one shell word.
	p := Project{Name: "p", WorkingDir: "/opt/my stacks/it's", ConfigFiles: []string{"/opt/my stacks/it's/compose.yaml"}, Service: "web"}
	_, up = RollbackCommands(p, "web:1", "sha256:x")
	if !strings.Contains(up, `--project-directory '/opt/my stacks/it'\''s'`) {
		t.Errorf("%s", up)
	}
	// A sidecar's service name comes from its label: one compose could not have written is left out, not pasted as a flag.
	_, up = RollbackCommands(proj(), "web:1", "sha256:x", "vpn", "--build", "a b")
	if !strings.HasSuffix(up, " web vpn") {
		t.Errorf("%s", up)
	}
}

func TestChildEnvStripsWhatCouldRedirectCompose(t *testing.T) {
	env := ChildEnv([]string{"PATH=/bin", "HOME=/h", "DOCKER_HOST=unix:///var/run/docker.sock", "COMPOSE_PROJECT_NAME=evil", "COMPOSE_FILE=/x", "COMPOSE_PROFILES=a",
		"DOCKER_CONTEXT=remote", "DOCKER_CONFIG=/elsewhere", "DOCKER_AUTH_CONFIG={}", "WHALENURSE_NOTIFY_URLS=secret://", "NO_COLOR=0"}, "")
	got := strings.Join(env, "\n")
	for _, bad := range []string{"COMPOSE_", "DOCKER_CONTEXT", "/elsewhere", "DOCKER_AUTH_CONFIG", "WHALENURSE_", "NO_COLOR=0"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s survived: %v", bad, env)
		}
	}
	for _, keep := range []string{"PATH=/bin", "HOME=/h", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONFIG=" + NoDockerConfig, "NO_COLOR=1"} {
		if !slices.Contains(env, keep) {
			t.Errorf("%s missing: %v", keep, env)
		}
	}
}

// HOME is passed through, so DOCKER_CONFIG must be set, not only removed: unset, the Docker CLI reads $HOME/.docker
// (under the example systemd unit that is /var/lib/whalenurse/.docker).
func TestChildEnvNeverLetsComposeReadAConfigDirectory(t *testing.T) {
	for _, tc := range []struct {
		env       []string
		dir, want string
	}{
		{[]string{"HOME=/h"}, "/run/whalenurse-docker-config-1", "/run/whalenurse-docker-config-1"},
		{[]string{"HOME=/h", "DOCKER_CONFIG=/h/.docker"}, "/run/whalenurse-docker-config-1", "/run/whalenurse-docker-config-1"},
		{[]string{"HOME=/h", "DOCKER_CONFIG=/h/.docker"}, "", NoDockerConfig},
	} {
		var got []string
		for _, kv := range ChildEnv(tc.env, tc.dir) {
			if v, ok := strings.CutPrefix(kv, "DOCKER_CONFIG="); ok {
				got = append(got, v)
			}
		}
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%v, %q: DOCKER_CONFIG %v, want %s", tc.env, tc.dir, got, tc.want)
		}
	}
	// The last resort reads as "no config file": it does not exist and no one can create it.
	if _, err := os.Stat(NoDockerConfig); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s: %v", NoDockerConfig, err)
	}
	if err := os.Mkdir(NoDockerConfig, 0o700); err == nil {
		_ = os.Remove(NoDockerConfig)
		t.Fatalf("%s could be created", NoDockerConfig)
	}
}

func TestPrivateConfigDirIsFreshEmptyAndPrivate(t *testing.T) {
	parent := t.TempDir()
	dir, remove, err := PrivateConfigDir("", filepath.Join(parent, "missing"), parent)
	if err != nil || filepath.Dir(dir) != parent {
		t.Fatal(dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatal(fi, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatal(ents)
	}
	other, removeOther, err := PrivateConfigDir(parent)
	if err != nil || other == dir {
		t.Fatal(other, err)
	}
	removeOther()
	if err := os.MkdirAll(filepath.Join(dir, "buildx", "instances"), 0o700); err != nil {
		t.Fatal(err)
	}
	remove()
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, _, err := PrivateConfigDir(filepath.Join(parent, "missing")); err == nil {
		t.Fatal("created in a missing parent")
	}
}

func TestDryRunActorRunsNothing(t *testing.T) {
	var verbs []string
	d := DryRun{Would: func(v string, _ Project) { verbs = append(verbs, v) }}
	if d.Recreate(context.Background(), proj()) != nil || d.Up(context.Background(), proj()) != nil || d.Pull(context.Background(), proj()) != nil || fmt.Sprint(verbs) != "[recreate up pull]" {
		t.Fatal(verbs)
	}
}

// ---- the oracle ------------------------------------------------------------------------------------------------

// The texts E5 recorded for compose v5.6.0 (docs/m0-findings.md): one JSON object per line, on stderr.
const (
	oracleUnchanged = `{"id":"Container wn-web-1","status":"Done","text":"Running"}` + "\n"
	oracleChanged   = `{"id":"Container wn-web-1","status":"Working","text":"Recreate"}
{"id":"Container wn-web-1","status":"Done","text":"Recreated"}
{"id":"Container wn-web-1","status":"Working","text":"Starting"}
{"id":"Container wn-web-1","status":"Done","text":"Started"}
`
	oracleStopped = `{"id":"Container wn-web-1","status":"Working","text":"Starting"}
{"id":"Container wn-web-1","status":"Done","text":"Started"}
`
)

func TestParseOracle(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		recreate  bool
		err       error
		texts     string
	}{
		{"unchanged", oracleUnchanged, false, nil, "Running"},
		{"changed config or image", oracleChanged, true, nil, "Recreate, Recreated, Starting, Started"},
		{"stopped, config unchanged: Starting is not drift", oracleStopped, false, nil, "Starting, Started"},
		{"the old container is renamed to <id>_<name> while it is recreated", `{"id":"Container 4f2a9b1c_wn-web-1","status":"Working","text":"Recreate"}` + "\n", true, nil, "Recreate"},
		{"other containers are ignored", `{"id":"Container other-1","status":"Done","text":"Recreate"}` + "\n" + oracleUnchanged, false, nil, "Running"},
		{"noise around the lines", "time=... level=warning msg=x\n" + oracleChanged + "\nnot json\n", true, nil, "Recreate, Recreated, Starting, Started"},
		{"an old compose printed null", "null\n", false, ErrUnparseable, ""},
		{"empty", "", false, ErrUnparseable, ""},
		{"plain text progress", " Container wn-web-1  Running \n", false, ErrUnparseable, ""},
		{"parsed, but nothing about this container", `{"id":"Network wn_default","status":"Done","text":"Resource is up-to-date"}` + "\n", false, ErrUnparseable, ""},
		{"a line without text", `{"id":"Container wn-web-1","status":"Done"}` + "\n", false, ErrUnparseable, ""},
	} {
		o, err := ParseOracle([]byte(tc.out), "wn-web-1")
		if !errors.Is(err, tc.err) {
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.err)
			continue
		}
		if err == nil && (o.WouldRecreate() != tc.recreate || strings.Join(o.Texts(), ", ") != tc.texts) {
			t.Errorf("%s: recreate=%v texts=%v", tc.name, o.WouldRecreate(), o.Texts())
		}
	}
}

func TestOracleIsReadFromStderrToo(t *testing.T) {
	r := &recRunner{out: func(Command) (Result, error) {
		return Result{Stdout: []byte("noise\n"), Stderr: []byte(oracleChanged)}, nil
	}}
	o, err := New(Binary{Path: "c"}, r).DryRunUp(context.Background(), proj(), "wn-web-1")
	if err != nil || !o.WouldRecreate() {
		t.Fatalf("%v %v", o, err)
	}
	r.out = func(Command) (Result, error) { return Result{Stdout: []byte(oracleChanged)}, nil }
	if o, err = New(Binary{Path: "c"}, r).DryRunUp(context.Background(), proj(), "wn-web-1"); err != nil || !o.WouldRecreate() {
		t.Fatalf("stdout too: %v %v", o, err)
	}
}

func TestParseConfigHash(t *testing.T) {
	out := []byte("db 1111aaaa2222bbbb\nweb 427fb0d10b7c52fc\n")
	if h, err := ParseConfigHash(out, "web"); err != nil || h != "427fb0d10b7c52fc" {
		t.Fatalf("%q %v", h, err)
	}
	if _, err := ParseConfigHash(out, "nope"); err == nil {
		t.Fatal("a missing service is an error")
	}
	if _, err := ParseConfigHash([]byte(""), "web"); err == nil {
		t.Fatal("empty output is an error")
	}
}

// ---- binary lookup ---------------------------------------------------------------------------------------------

type fakeInfo struct{ dir bool }

func (fakeInfo) Name() string       { return "f" }
func (fakeInfo) Size() int64        { return 1 }
func (fakeInfo) Mode() os.FileMode  { return 0o755 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool      { return f.dir }
func (fakeInfo) Sys() any           { return nil }

func TestResolveOrder(t *testing.T) {
	versions := map[string]string{"/env/c": "5.9.0\n", "/host/docker-compose": "v5.6.0\n", BundledPath: "5.4.0\n", HostPluginDirs[0]: "5.3.0\n"}
	broken := map[string]bool{}
	present := map[string]bool{}
	for p := range versions {
		present[p] = true
	}
	mk := func(env string) Lookup {
		return Lookup{
			Env: env,
			Stat: func(p string) (os.FileInfo, error) {
				if present[p] {
					return fakeInfo{}, nil
				}
				return nil, os.ErrNotExist
			},
			Runner: &recRunner{out: func(c Command) (Result, error) {
				if broken[c.Bin] {
					return Result{}, errors.New("exec format error")
				}
				return Result{Stdout: []byte(versions[c.Bin])}, nil
			}},
		}
	}
	ctx := context.Background()
	check := func(env, wantPath string, wantSrc Source, wantV Version) {
		t.Helper()
		b, _, err := Resolve(ctx, mk(env))
		if err != nil || b.Path != wantPath || b.Source != wantSrc || b.Version != wantV {
			t.Fatalf("env=%q: %+v %v", env, b, err)
		}
	}
	check("/env/c", "/env/c", SourceEnv, Version{5, 9, 0})
	check("", "/host/docker-compose", SourceHost, Version{5, 6, 0})
	delete(present, "/host/docker-compose")
	check("", BundledPath, SourceBundled, Version{5, 4, 0})
	broken[BundledPath] = true // exists but does not run: next
	check("", HostPluginDirs[0], SourceHost, Version{5, 3, 0})
	delete(present, HostPluginDirs[0])
	if b, notes, err := Resolve(ctx, mk("")); err == nil || b != nil || len(notes) < 3 {
		t.Fatalf("nothing found must be an error with notes: %v %v %v", b, notes, err)
	}
	// An explicit WHALENURSE_COMPOSE_BIN that does not work is an error, never a reason to use another binary.
	present["/host/docker-compose"] = true
	if b, _, err := Resolve(ctx, mk("/env/missing")); err == nil || b != nil || !strings.Contains(err.Error(), "WHALENURSE_COMPOSE_BIN=/env/missing") {
		t.Fatalf("%v %v", b, err)
	}
	// A directory is not a binary.
	l := mk("")
	l.Stat = func(string) (os.FileInfo, error) { return fakeInfo{dir: true}, nil }
	if b, _, err := Resolve(ctx, l); err == nil || b != nil {
		t.Fatalf("%v %v", b, err)
	}
	// The source comes from the path that matched, not from the version.
	present[BundledPath], broken[BundledPath] = true, false
	delete(present, "/host/docker-compose")
	versions[BundledPath] = "5.6.0"
	if b, _, _ := Resolve(ctx, mk("")); b.Source != SourceBundled {
		t.Fatalf("%+v", b)
	}
	// An unreadable version still resolves (the gate then disables the rungs: fail closed).
	versions[BundledPath] = "garbage"
	if b, _, err := Resolve(ctx, mk("")); err != nil || !b.Version.IsZero() || b.Raw != "garbage" {
		t.Fatalf("%+v %v", b, err)
	}
}

func TestPullPolicyCommandLineAndParsing(t *testing.T) {
	r := &recRunner{out: func(Command) (Result, error) {
		return Result{Stdout: []byte(`{"name":"x","services":{"web":{"image":"web:1","pull_policy":"never","environment":{"SECRET":"s3cret"}}}}`)}, nil
	}}
	c := New(Binary{Path: "/host/docker-compose", Source: SourceHost}, r)
	got, err := c.PullPolicy(context.Background(), proj())
	if err != nil || got != "never" {
		t.Fatalf("%q %v", got, err)
	}
	want := "-p whalenurse-test-x --project-directory /opt/stacks/x -f /opt/stacks/x/compose.yaml -f /opt/stacks/x/override.yaml --env-file /opt/stacks/x/.env config --format json --no-env-resolution web"
	if len(r.cmds) != 1 || strings.Join(r.cmds[0].Args, " ") != want {
		t.Fatalf("%v", r.cmds)
	}
	for in, want := range map[string]string{
		`{"services":{"web":{"image":"a"}}}`:                   "",
		`{"services":{"web":{"pull_policy":"missing"}}}`:       "missing",
		`{"services":{"web":{"pull_policy":" Build "}}}`:       "Build",
		`{"services":{"web":{"pull_policy":"daily"},"db":{}}}`: "daily",
		`{"services":{"web":{"pull_policy":"every_12h"}}}`:     "every_12h",
	} {
		if got, err := ParsePullPolicy([]byte(in), "web"); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "null", "not json", `{"services":{}}`, `{"services":{"db":{}}}`} {
		if _, err := ParsePullPolicy([]byte(in), "web"); !errors.Is(err, ErrUnparseable) {
			t.Errorf("%q: %v", in, err)
		}
	}
	// A parse error never quotes the output (it can hold interpolated secrets).
	if _, err := ParsePullPolicy([]byte(`{"services": s3cret`), "web"); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("%v", err)
	}
	for policy, blocks := range map[string]bool{"never": true, "build": true, "Never": true, " BUILD ": true, "": false, "always": false, "missing": false, "if_not_present": false, "daily": false, "refresh": false} {
		if PullPolicyBlocksPull(policy) != blocks {
			t.Errorf("PullPolicyBlocksPull(%q) = %v", policy, !blocks)
		}
	}
}

func TestRollbackCommandsNameTheSidecarsInOneUp(t *testing.T) {
	_, up := RollbackCommands(proj(), "app:1", "sha256:x", "side", "side2")
	if !strings.HasSuffix(up, "up -d --no-deps --pull never --no-build web side side2") {
		t.Fatalf("%s", up)
	}
}

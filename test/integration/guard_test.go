package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/docker"
)

// stub is a Runner that records argv and answers inspect lookups from a table.
type stub struct {
	calls    []string
	projects map[string]string // container -> compose project label
	context  string
	psOut    string
}

func (s *stub) Run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, line)
	switch {
	case strings.HasPrefix(line, "docker context show"):
		return s.context + "\n", nil
	case strings.HasPrefix(line, "docker inspect --format {{index .Config.Labels"):
		p, ok := s.projects[args[len(args)-1]]
		if !ok {
			return "", errors.New("no such container")
		}
		return p + "\n", nil
	case strings.HasPrefix(line, "docker inspect --format {{.Id}}"):
		return "id-" + args[len(args)-1] + "\n", nil
	case strings.HasPrefix(line, "docker ps"):
		return s.psOut, nil
	case strings.HasPrefix(line, "docker port"):
		return "127.0.0.1:49153\n[::1]:49153\n", nil
	}
	return "", nil
}

func (s *stub) mutations() []string {
	var out []string
	for _, c := range s.calls {
		if !strings.HasPrefix(c, "docker inspect") && !strings.HasPrefix(c, "docker context") && !strings.HasPrefix(c, "docker ps") {
			out = append(out, c)
		}
	}
	return out
}

func refused(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("want a REFUSED error, got %v", err)
	}
}

var badProjects = []string{
	"", "whalenurse-test-", "whalenurse-test", "prod", "default", "whalenurse", "whalenurse-m0-e1", "xwhalenurse-test-a",
	"whalenurse-test-A", "whalenurse-test-a b", "whalenurse-test-a/b", "whalenurse-test-a\nprod", "whalenurse-test--x", "WHALENURSE-TEST-a",
	"whalenurse-test-a;rm", "../whalenurse-test-a", "whalenurse-test-é",
}

func TestProjectRegexRejectsBadNames(t *testing.T) {
	for _, p := range badProjects {
		if err := RequireProject(p); err == nil {
			t.Errorf("project %q was accepted", p)
		}
	}
	for _, p := range []string{"whalenurse-test-a", "whalenurse-test-heal-3f2a", "whalenurse-test-0_x-y"} {
		if err := RequireProject(p); err != nil {
			t.Errorf("project %q was refused: %v", p, err)
		}
	}
}

func TestComposeRefusesBadProjectsAndSmuggling(t *testing.T) {
	for _, p := range badProjects {
		s := &stub{}
		g := &Guard{R: s}
		_, err := g.Compose(context.Background(), p, "ps")
		refused(t, err)
		refused(t, g.Down(context.Background(), p))
		if len(s.calls) != 0 {
			t.Errorf("project %q reached docker: %v", p, s.calls)
		}
	}
	good := "whalenurse-test-a"
	for _, args := range [][]string{
		{"-p", "prod", "ps"}, {"-pprod", "ps"}, {"--project-name", "prod"}, {"--project-name=prod", "ps"},
		{"down"}, {"-f", "x.yml", "down", "-v"}, {"rm", "-f"}, {"up", "-d", "rm"},
	} {
		s := &stub{}
		g := &Guard{R: s}
		_, err := g.Compose(context.Background(), good, args...)
		refused(t, err)
		if len(s.calls) != 0 {
			t.Errorf("%v reached docker: %v", args, s.calls)
		}
	}
	// --pull and --progress are legitimate and start with two dashes.
	s := &stub{}
	g := &Guard{R: s}
	if _, err := g.Compose(context.Background(), good, "up", "-d", "--pull", "never", "--progress", "plain"); err != nil {
		t.Fatal(err)
	}
	if got := s.calls[0]; got != "docker compose -p whalenurse-test-a up -d --pull never --progress plain" {
		t.Fatal(got)
	}
	if err := g.Down(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	if got := s.calls[1]; got != "docker compose -p whalenurse-test-a down -v --timeout 2 --remove-orphans" {
		t.Fatal(got)
	}
}

func TestCtrChecksTheContainersProjectFirst(t *testing.T) {
	s := &stub{projects: map[string]string{"ours": "whalenurse-test-a", "theirs": "prod", "m0": "whalenurse-m0-e1", "plain": ""}}
	g := &Guard{R: s}
	ctx := context.Background()
	for _, ctr := range []string{"theirs", "m0", "plain", "unknown", ""} {
		_, err := g.Ctr(ctx, "restart", ctr)
		refused(t, err)
	}
	if m := s.mutations(); len(m) != 0 {
		t.Fatalf("mutations reached docker: %v", m)
	}
	for _, verb := range []string{"rm", "prune", "pause", "update", ""} {
		_, err := g.Ctr(ctx, verb, "ours")
		refused(t, err)
	}
	if _, err := g.Ctr(ctx, "restart", "ours"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Ctr(ctx, "exec", "ours", "rm", "/ok/flag"); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker restart id-ours", "docker exec id-ours rm /ok/flag"}
	if got := s.mutations(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%v", got)
	}
}

func TestPullOnlyBaseImages(t *testing.T) {
	s := &stub{}
	g := &Guard{R: s}
	refused(t, g.PullBase(context.Background(), "alpine:latest"))
	refused(t, g.PullBase(context.Background(), "busybox:1.37; rm -rf /"))
	if len(s.calls) != 0 {
		t.Fatal(s.calls)
	}
	if err := g.PullBase(context.Background(), "busybox:1.37"); err != nil {
		t.Fatal(err)
	}
}

func TestSanitizeEnv(t *testing.T) {
	ctx := context.Background()
	t.Setenv("DOCKER_HOST", "")
	if err := (&Guard{R: &stub{context: "default"}}).SanitizeEnv(ctx); err != nil {
		t.Fatal(err)
	}
	refused(t, (&Guard{R: &stub{context: "remote-prod"}}).SanitizeEnv(ctx))
	for _, h := range []string{"tcp://prod:2375", "ssh://root@prod", "unix:///tmp/other.sock"} {
		t.Setenv("DOCKER_HOST", h)
		refused(t, (&Guard{R: &stub{context: "default"}}).SanitizeEnv(ctx))
	}
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	if err := (&Guard{R: &stub{context: "default"}}).SanitizeEnv(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCleanEnvDropsRedirectingVariables(t *testing.T) {
	got := cleanEnv([]string{"PATH=/bin", "COMPOSE_PROJECT_NAME=prod", "DOCKER_CONTEXT=prod", "COMPOSE_FILE=x", "DOCKER_CONFIG=/x", "HOME=/h"})
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h" {
		t.Fatal(got)
	}
}

func TestPreflightFindsOptedInStrangers(t *testing.T) {
	ctx := context.Background()
	t.Setenv("WHALENURSE_MODE", "")
	ok := &stub{psOut: "abc whalenurse-test-a\ndef whalenurse-test-b\n"}
	if err := (&Guard{R: ok}).PreflightNoStrangers(ctx); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{"abc prod\n", "abc \n", "abc whalenurse-m0-x\n", "abc whalenurse-test-a\ndef media\n"} {
		refused(t, (&Guard{R: &stub{psOut: out}}).PreflightNoStrangers(ctx))
	}
	t.Setenv("WHALENURSE_MODE", "all")
	refused(t, (&Guard{R: ok}).PreflightNoStrangers(ctx))
}

func TestGuardedActorRefusesOtherProjects(t *testing.T) {
	f := docker.NewFake()
	f.Put(docker.Container{ID: "ours", Name: "ours", Labels: map[string]string{docker.LabelProject: "whalenurse-test-a"}})
	f.Put(docker.Container{ID: "theirs", Name: "theirs", Labels: map[string]string{docker.LabelProject: "media"}})
	f.Put(docker.Container{ID: "plain", Name: "plain", Labels: map[string]string{}})
	a := Actor{Reader: f, Inner: f}
	ctx := context.Background()
	for _, id := range []string{"theirs", "plain", "missing"} {
		refused(t, a.Restart(ctx, id))
		refused(t, a.Start(ctx, id))
	}
	if len(f.Calls()) != 0 {
		t.Fatalf("calls reached the inner actor: %v", f.Calls())
	}
	if err := a.Restart(ctx, "ours"); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx, "ours"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(f.Calls()); !strings.Contains(got, "restart") || len(f.Calls()) != 2 {
		t.Fatal(got)
	}
}

// ---- the compose filter and the image helpers (M2b) ----------------------------------------------------------------

const goodUp = "-p whalenurse-test-a --project-directory /s -f /s/compose.yaml --env-file /s/.env up -d --force-recreate --no-deps --pull never --no-build web"

func TestComposeFilterAcceptsExactlyWhatTheComposePackageBuilds(t *testing.T) {
	for _, ok := range []string{
		goodUp,
		"-p whalenurse-test-a --project-directory /s -f /s/compose.yaml up -d --no-deps --pull never --no-build web",
		"-p whalenurse-test-a --project-directory /s -f /s/compose.yaml --dry-run --progress json up -d --no-deps --pull never --no-build web",
		"-p whalenurse-test-a --project-directory /s -f /s/compose.yaml config --hash=web",
		"-p whalenurse-test-a --project-directory /s -f /s/compose.yaml config --format json --no-env-resolution web", // the pull rung's policy read
		"-p whalenurse-test-a version --short",
		"-p whalenurse-test-a version",
		"-p whalenurse-test-a --project-directory /s -f /s/compose.yaml --env-file /s/.env pull --policy always web", // rung 3
	} {
		if err := CheckComposeCommand(strings.Fields(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func TestComposeFilterRefuses(t *testing.T) {
	for name, args := range map[string]string{
		"down":                       "-p whalenurse-test-a down -v",
		"down after the files":       "-p whalenurse-test-a --project-directory /s -f /s/c.yaml down",
		"rm":                         "-p whalenurse-test-a rm -f web",
		"pull":                       "-p whalenurse-test-a pull web",
		"pull with the policy first": "-p whalenurse-test-a pull web --policy always",
		"pull policy missing":        "-p whalenurse-test-a pull --policy missing web",
		"pull with dependencies":     "-p whalenurse-test-a pull --policy always --include-deps web",
		"pull all services":          "-p whalenurse-test-a pull --policy always",
		"pull two services":          "-p whalenurse-test-a pull --policy always web db",
		"pull a flag as a service":   "-p whalenurse-test-a pull --policy always --quiet",
		"pull of another project":    "-p prod pull --policy always web",
		"build":                      "-p whalenurse-test-a build web",
		"run":                        "-p whalenurse-test-a run web",
		"stop":                       "-p whalenurse-test-a stop web",
		"another project":            "-p prod up -d --no-deps --pull never --no-build web",
		"an m0 project":              "-p whalenurse-m0-e1 up -d --no-deps --pull never --no-build web",
		"no project":                 "up -d --no-deps --pull never --no-build web",
		"project via --project-name": "--project-name whalenurse-test-a up -d --no-deps --pull never --no-build web",
		"up may pull":                "-p whalenurse-test-a up -d --no-deps --pull always --no-build web",
		"up pull missing":            "-p whalenurse-test-a up -d --no-deps --pull missing --no-build web",
		"up without --pull":          "-p whalenurse-test-a up -d --no-deps --no-build web",
		"up may build":               "-p whalenurse-test-a up -d --no-deps --pull never --build web",
		"up without --no-build":      "-p whalenurse-test-a up -d --no-deps --pull never web",
		"up with dependencies":       "-p whalenurse-test-a up -d --pull never --no-build web",
		"up attached":                "-p whalenurse-test-a up --no-deps --pull never --no-build web",
		"up all services":            "-p whalenurse-test-a up -d --no-deps --pull never --no-build",
		"up two services":            "-p whalenurse-test-a up -d --no-deps --pull never --no-build web db",
		"up --remove-orphans":        "-p whalenurse-test-a up -d --no-deps --pull never --no-build --remove-orphans web",
		"up --renew-anon-volumes":    "-p whalenurse-test-a up -d --no-deps --pull never --no-build -V web",
		"config hash of all":         "-p whalenurse-test-a config --hash=*",
		"config hash of two":         "-p whalenurse-test-a config --hash=a,b",
		"config rendered":            "-p whalenurse-test-a config",
		"config json of all":         "-p whalenurse-test-a config --format json --no-env-resolution",
		"config json with env":       "-p whalenurse-test-a config --format json web",
		"config yaml":                "-p whalenurse-test-a config --format yaml --no-env-resolution web",
		"config json of two":         "-p whalenurse-test-a config --format json --no-env-resolution web db",
		"config json, flag service":  "-p whalenurse-test-a config --format json --no-env-resolution --hash=web",
		"version -f":                 "-p whalenurse-test-a version --format json",
		"flag without value":         "-p whalenurse-test-a -f",
		"no subcommand":              "-p whalenurse-test-a --dry-run",
	} {
		if err := CheckComposeCommand(strings.Fields(args)); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %s was not refused (%v)", name, args, err)
		}
	}
}

type recordingCompose struct{ ran []string }

func (r *recordingCompose) Run(_ context.Context, c compose.Command) (compose.Result, error) {
	r.ran = append(r.ran, c.String())
	return compose.Result{}, nil
}

func TestComposeRunnerOnlyRunsWhatTheFilterAccepts(t *testing.T) {
	rec := &recordingCompose{}
	r := &ComposeRunner{Inner: rec}
	if _, err := r.Run(context.Background(), compose.Command{Bin: "/usr/libexec/docker/cli-plugins/docker-compose", Args: strings.Fields(goodUp)}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), compose.Command{Bin: "/usr/bin/docker", Args: strings.Fields(goodUp)}); !errors.Is(err, ErrRefused) {
		t.Fatalf("another binary: %v", err)
	}
	if _, err := r.Run(context.Background(), compose.Command{Bin: "/x/docker-compose", Args: strings.Fields("-p whalenurse-test-a down")}); !errors.Is(err, ErrRefused) {
		t.Fatalf("down: %v", err)
	}
	if len(rec.ran) != 1 || len(r.Log()) != 1 || len(r.Mutating()) != 1 || len(r.Pulls()) != 0 {
		t.Fatalf("%v %v", rec.ran, r.Log())
	}
	if _, err := r.Run(context.Background(), compose.Command{Bin: "/x/docker-compose", Args: strings.Fields("-p whalenurse-test-a pull --policy always web")}); err != nil {
		t.Fatal(err)
	}
	if len(r.Pulls()) != 1 || len(r.Mutating()) != 1 {
		t.Fatalf("a pull is counted by Pulls: %v / %v", r.Pulls(), r.Mutating())
	}
	if _, err := r.Run(context.Background(), compose.Command{Bin: "/x/docker-compose", Args: strings.Fields("-p whalenurse-test-a --dry-run --progress json up -d --no-deps --pull never --no-build web")}); err != nil {
		t.Fatal(err)
	}
	if len(r.Mutating()) != 1 {
		t.Fatalf("a --dry-run up is not a mutation: %v", r.Mutating())
	}
}

func TestImageHelpersStayInTheirNamespace(t *testing.T) {
	s := &stub{}
	g := &Guard{R: s}
	ctx := context.Background()
	for _, dst := range []string{"busybox:1.37", "prod/app:1", "whalenurse:1", "", "whalenurse-test", "whalenurse-test/x y", "whalenurse-test/x;rm"} {
		refused(t, g.Tag(ctx, "busybox:1.37", dst))
		refused(t, g.Untag(ctx, dst))
		refused(t, g.Build(ctx, dst, "FROM busybox:1.37\n"))
	}
	refused(t, g.Tag(ctx, "alpine:3", "whalenurse-test/x:1"))
	refused(t, g.Tag(ctx, "prod/app:1", "whalenurse-test/x:1"))
	refused(t, g.Build(ctx, "whalenurse-test/x:1", "FROM alpine:3.22\n"))
	refused(t, g.Build(ctx, "whalenurse-test/x:1", "FROM busybox:1.37\nFROM prod/app:1\n"))
	refused(t, g.Build(ctx, "whalenurse-test/x:1", "RUN true\n"))
	if len(s.mutations()) != 0 {
		t.Fatalf("a refused call ran: %v", s.mutations())
	}
	if err := g.Tag(ctx, "busybox:1.37", "whalenurse-test/x:1"); err != nil {
		t.Fatal(err)
	}
	if err := g.Tag(ctx, "whalenurse-test/x:1", "whalenurse-test/y:1"); err != nil {
		t.Fatal(err)
	}
	if err := g.Build(ctx, "whalenurse-test/z:1", "FROM busybox:1.37\nRUN touch /x\n"); err != nil {
		t.Fatal(err)
	}
	if err := g.Untag(ctx, "whalenurse-test/x:1"); err != nil {
		t.Fatal(err)
	}
	if len(s.mutations()) != 4 {
		t.Fatalf("%v", s.mutations())
	}
}

// ---- the test registry (M4) ---------------------------------------------------------------------------------------

const regRef = "127.0.0.1:49153/whalenurse-test/pull-app:latest"

func TestRegistryHelpersStayInTheTestRegistry(t *testing.T) {
	s := &stub{projects: map[string]string{"reg": "whalenurse-test-registry-ab12", "other": "prod"}}
	g := &Guard{R: s}
	ctx := context.Background()
	for _, ref := range []string{
		"", "busybox:1.37", "localhost:5000/whalenurse-test/x:1", "127.0.0.1/whalenurse-test/x:1", "127.0.0.1:5000/x:1",
		"127.0.0.1:5000/whalenurse-test", "127.0.0.1:5000/other/x:1", "127.0.0.1:5000/whalenurse-test/x y", "127.0.0.1:5000/whalenurse-test/x;rm",
		"ghcr.io/jackspiering/whalenurse:latest", "10.0.0.5:5000/whalenurse-test/x:1", "127.0.0.1:5000/whalenurse-test/../x:1x/$(id)",
		"127.0.0.1:5000@evil.com/whalenurse-test/x:1", "docker.io/127.0.0.1:5000/whalenurse-test/x:1",
	} {
		refused(t, g.Push(ctx, ref))
		refused(t, g.PullRegistry(ctx, ref))
	}
	if len(s.mutations()) != 0 {
		t.Fatalf("a refused call ran: %v", s.mutations())
	}
	if err := g.Push(ctx, regRef); err != nil {
		t.Fatal(err)
	}
	if err := g.PullRegistry(ctx, regRef); err != nil {
		t.Fatal(err)
	}
	// Tag and Build accept a registry reference as a destination, an image ID as a source, and nothing looser.
	id := "sha256:" + strings.Repeat("a", 64)
	if err := g.Tag(ctx, id, regRef); err != nil {
		t.Fatal(err)
	}
	if err := g.Tag(ctx, "whalenurse-test/v2:1", regRef); err != nil {
		t.Fatal(err)
	}
	if err := g.Build(ctx, regRef, "FROM busybox:1.37\nRUN echo v1 > /version\n"); err != nil {
		t.Fatal(err)
	}
	refused(t, g.Tag(ctx, "sha256:abc", regRef))
	refused(t, g.Tag(ctx, id+"x", regRef))
	refused(t, g.Tag(ctx, id, "prod/app:1"))
	refused(t, g.Tag(ctx, "127.0.0.1:49153/other/x:1", regRef))
	// The registry itself is a base image, and only as itself.
	if err := g.PullBase(ctx, "registry:2"); err != nil {
		t.Fatal(err)
	}
	refused(t, g.PullBase(ctx, "registry:3"))
	// The published port is read only for a container of a test project.
	if p, err := g.PublishedPort(ctx, "reg", "5000/tcp"); err != nil || p != "49153" {
		t.Fatalf("%q %v", p, err)
	}
	if _, err := g.PublishedPort(ctx, "other", "5000/tcp"); !errors.Is(err, ErrRefused) {
		t.Fatalf("%v", err)
	}
}

func TestRunRollbackRunsOnlyWhatARollbackIs(t *testing.T) {
	id := "sha256:" + strings.Repeat("a", 64)
	tagLine := "docker image tag " + id + " " + regRef
	good := "docker compose -p whalenurse-test-a --project-directory /tmp/s -f /tmp/s/compose.yaml up -d --no-deps --pull never --no-build web"
	// The form written before plan v3.13 item 7, without --no-build, is still a rollback.
	if err := (&Guard{R: &stub{}}).RunRollback(context.Background(), tagLine, strings.Replace(good, " --no-build", "", 1)); err != nil {
		t.Fatal(err)
	}
	s := &stub{}
	g := &Guard{R: s}
	ctx := context.Background()
	if err := g.RunRollback(ctx, tagLine, good); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker tag " + id + " " + regRef, "docker " + strings.TrimPrefix(good, "docker ")}
	if got := s.mutations(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%v", got)
	}
	// The parent and the sidecars it was recreated with, in one up.
	s3 := &stub{}
	if err := (&Guard{R: s3}).RunRollback(ctx, tagLine, good+" side side2"); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string][2]string{
		"another project":       {tagLine, strings.Replace(good, "whalenurse-test-a", "prod", 1)},
		"an extra flag":         {tagLine, strings.Replace(good, "up -d", "up -d --force-recreate", 1)},
		"a different command":   {tagLine, "docker compose -p whalenurse-test-a down"},
		"down in the middle":    {tagLine, "docker compose -p whalenurse-test-a down && " + good},
		"quoted path":           {tagLine, strings.Replace(good, "/tmp/s ", "'/tmp/s;x' ", 1)},
		"a shell metacharacter": {tagLine, good + " ; id"},
		"pull may pull":         {tagLine, strings.Replace(good, "--pull never", "--pull always", 1)},
		"may build":             {tagLine, strings.Replace(good, "--no-build", "--build", 1)},
		"--no-build twice":      {tagLine, strings.Replace(good, "--no-build", "--no-build --no-build", 1)},
		"--no-build elsewhere":  {tagLine, strings.Replace(strings.Replace(good, " --no-build", "", 1), "up -d", "up --no-build -d", 1)},
		"not docker":            {tagLine, "sh -c id"},
		"tag onto a bad ref":    {"docker image tag " + id + " prod/app:1", good},
		"tag from a name":       {"docker image tag busybox:1.37 " + regRef, good},
		"tag with more":         {tagLine + " && docker rm x", good},
	} {
		s2 := &stub{}
		refused(t, (&Guard{R: s2}).RunRollback(ctx, tc[0], tc[1]))
		if m := s2.mutations(); len(m) != 0 {
			t.Errorf("%s: a refused rollback reached docker (not even its tag may run): %v", name, m)
		}
	}
}

// M7: only a pull, and only with the directory the test registered, may carry a DOCKER_CONFIG of its own.
func TestComposeRunnerOnlyLetsAPullUseTheCredentialsDirectory(t *testing.T) {
	rec := &recordingCompose{}
	r := &ComposeRunner{Inner: rec}
	r.AllowAuthDir("/tmp/whalenurse-test-auth-1")
	pull := strings.Fields("-p whalenurse-test-a pull --policy always web")
	ctx := context.Background()
	if _, err := r.Run(ctx, compose.Command{Bin: "/x/docker-compose", Args: pull, ConfigDir: "/tmp/whalenurse-test-auth-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, compose.Command{Bin: "/x/docker-compose", Args: pull, ConfigDir: "/home/me/.docker"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("another directory: %v", err)
	}
	if _, err := r.Run(ctx, compose.Command{Bin: "/x/docker-compose", Args: strings.Fields(goodUp), ConfigDir: "/tmp/whalenurse-test-auth-1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("an up with the credentials: %v", err)
	}
	none := &ComposeRunner{Inner: rec}
	if _, err := none.Run(ctx, compose.Command{Bin: "/x/docker-compose", Args: pull, ConfigDir: "/tmp/whalenurse-test-auth-1"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("no directory registered: %v", err)
	}
	if got := r.PullConfigDirs(); len(got) != 1 || got[0] != "/tmp/whalenurse-test-auth-1" {
		t.Fatalf("%v", got)
	}
}

func TestPushWithOnlyUsesTheTestsOwnDockerConfig(t *testing.T) {
	s := &stub{}
	g := &Guard{R: s}
	ref := "127.0.0.1:5000/whalenurse-test/x:v1"
	for _, dir := range []string{"/home/me/.docker", os.TempDir() + "/other-dir", os.TempDir() + "/" + DockerConfigPrefix + "1/sub", ""} {
		if err := g.PushWith(context.Background(), ref, dir); !errors.Is(err, ErrRefused) {
			t.Errorf("%q: %v", dir, err)
		}
	}
	if err := g.PushWith(context.Background(), "ghcr.io/x/y:v1", os.TempDir()+"/"+DockerConfigPrefix+"1"); !errors.Is(err, ErrRefused) {
		t.Errorf("a reference outside the test registry: %v", err)
	}
	if len(s.calls) != 0 {
		t.Fatalf("a refusal ran docker: %v", s.calls)
	}
}

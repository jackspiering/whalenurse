// Package integration holds the tests that run against a real Docker daemon.
// They run in CI on GitHub-hosted runners, never as part of `go test ./...`
// on a developer machine (the tests need -tags integration and
// WHALENURSE_INTEGRATION=1).
//
// SAFETY MODEL (fails closed), a port of test/m0/lib.sh. Everything in this
// package that changes anything goes through Guard, and each method refuses to
// act unless the compose project of its target matches ^whalenurse-test-...$:
//
//	Compose   docker compose, always with -p <project>; refuses -p, --project-name, down and rm
//	Down      the ONLY place that runs `down -v`, and only for matching projects
//	Ctr       stop/start/restart/kill/exec on a container whose project label matches
//	PullBase  pulls only the base images in BaseImages
//	Tag/Untag/Build  image tags under whalenurse-test/ (or in the test registry, 127.0.0.1:<port>/whalenurse-test/) only; a
//	          build may start FROM a base image only
//	Push/PullRegistry  push or pull references of the test registry only (M4: a registry:2 on loopback, started as a
//	          whalenurse-test-* compose project)
//	RunRollback  runs the rollback commands of an "upgraded during heal" message, after checking them line by line
//	Actor     wraps whalenurse's own docker.Actor so the healer under test cannot
//	          touch a container outside a matching project either
//	ComposeRunner  the compose Runner the healer under test runs compose through: only `up` (with -d --no-deps --pull never
//	          --no-build), `pull --policy always <one service>` (rung 3), `config --hash` and `version`, only with -p
//	          <matching project>; never down, rm, build or an up that could pull
//
// Reads (inspect, ps, logs) use plain docker. This file has no build tag so
// the guard itself is unit-tested without Docker (guard_test.go).
package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/jackspiering/whalenurse/internal/compose"
	"github.com/jackspiering/whalenurse/internal/config"
	"github.com/jackspiering/whalenurse/internal/docker"
)

// ProjectRE is the only compose project name the tests may act on.
var ProjectRE = regexp.MustCompile(`^whalenurse-test-[a-z0-9][a-z0-9_-]*$`)

// BaseImages are the only images the tests may pull.
var BaseImages = []string{"busybox:1.37", "registry:2"}

// Runner runs an external command and returns its stdout.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs real commands with an environment that cannot redirect
// docker or compose at something else.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = cleanEnv(os.Environ())
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func cleanEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "COMPOSE_PROFILES", "COMPOSE_ENV_FILES", "COMPOSE_PATH_SEPARATOR", "DOCKER_CONTEXT", "DOCKER_CONFIG":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Guard performs the mutating Docker operations of the tests.
type Guard struct {
	R        Runner
	Projects []string // projects touched, for cleanup
}

// NewGuard returns a Guard that runs real commands.
func NewGuard() *Guard { return &Guard{R: ExecRunner{}} }

// ErrRefused wraps every refusal.
var ErrRefused = errors.New("REFUSED")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// RequireProject checks a compose project name.
func RequireProject(p string) error {
	if p == "" || !ProjectRE.MatchString(p) {
		return refuse("project %q does not match %s", p, ProjectRE)
	}
	return nil
}

// SanitizeEnv refuses to run unless docker points at the local socket and the
// default context.
func (g *Guard) SanitizeEnv(ctx context.Context) error {
	switch h := os.Getenv("DOCKER_HOST"); h {
	case "", "unix:///var/run/docker.sock", "unix:///run/docker.sock":
	default:
		return refuse("DOCKER_HOST=%s: only the local socket is allowed", h)
	}
	out, err := g.R.Run(ctx, "docker", "context", "show")
	if err != nil {
		return refuse("cannot read the docker context: %v", err)
	}
	if c := strings.TrimSpace(out); c != "default" {
		return refuse("docker context is %q, expected \"default\"", c)
	}
	return nil
}

// Compose runs `docker compose -p <project> args...`.
func (g *Guard) Compose(ctx context.Context, project string, args ...string) (string, error) {
	if err := RequireProject(project); err != nil {
		return "", err
	}
	for _, a := range args {
		switch {
		case a == "down" || a == "rm":
			return "", refuse("compose %q is only allowed through Down", a)
		case strings.HasPrefix(a, "-p") || a == "--project-name" || strings.HasPrefix(a, "--project-name="):
			// -p* also catches the attached form (-pother); --pull and --progress start with two dashes.
			return "", refuse("caller-supplied project name (%s)", a)
		}
	}
	g.touch(project)
	return g.R.Run(ctx, "docker", append([]string{"compose", "-p", project}, args...)...)
}

// Down is the only `down -v` in the harness.
func (g *Guard) Down(ctx context.Context, project string) error {
	if err := RequireProject(project); err != nil {
		return err
	}
	_, err := g.R.Run(ctx, "docker", "compose", "-p", project, "down", "-v", "--timeout", "2", "--remove-orphans")
	return err
}

func (g *Guard) touch(p string) {
	for _, q := range g.Projects {
		if q == p {
			return
		}
	}
	g.Projects = append(g.Projects, p)
}

// ProjectOf reads the compose project label of a container.
func (g *Guard) ProjectOf(ctx context.Context, ctr string) (string, error) {
	out, err := g.R.Run(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, ctr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Ctr runs stop/start/restart/kill/exec on a container whose project label
// matches. For exec the arguments follow the container.
func (g *Guard) Ctr(ctx context.Context, verb, ctr string, args ...string) (string, error) {
	switch verb {
	case "stop", "start", "restart", "kill", "exec":
	default:
		return "", refuse("verb %q not allowed", verb)
	}
	if ctr == "" {
		return "", refuse("no container")
	}
	p, err := g.ProjectOf(ctx, ctr)
	if err != nil {
		return "", refuse("cannot determine the project of %q: %v", ctr, err)
	}
	if err := RequireProject(p); err != nil {
		return "", err
	}
	id, err := g.R.Run(ctx, "docker", "inspect", "--format", "{{.Id}}", ctr)
	if err != nil {
		return "", refuse("cannot resolve %q: %v", ctr, err)
	}
	id = strings.TrimSpace(id)
	if verb == "exec" {
		return g.R.Run(ctx, "docker", append([]string{"exec", id}, args...)...)
	}
	return g.R.Run(ctx, "docker", append([]string{verb}, append(args, id)...)...)
}

// PullBase pulls one of BaseImages.
func (g *Guard) PullBase(ctx context.Context, ref string) error {
	for _, b := range BaseImages {
		if b == ref {
			_, err := g.R.Run(ctx, "docker", "pull", "-q", ref)
			return err
		}
	}
	return refuse("pull of %q: not an allowed base image", ref)
}

// PreflightNoStrangers fails if whalenurse could act on anything outside a
// matching project: it refuses WHALENURSE_MODE=all and any container that
// carries whalenurse.enable=true outside a whalenurse-test-* project.
func (g *Guard) PreflightNoStrangers(ctx context.Context) error {
	if m := os.Getenv("WHALENURSE_MODE"); m != "" && m != string(config.ModeLabel) {
		return refuse("WHALENURSE_MODE=%s: integration tests run in label mode only", m)
	}
	out, err := g.R.Run(ctx, "docker", "ps", "-a", "--filter", "label=whalenurse.enable=true",
		"--format", `{{.ID}} {{.Label "com.docker.compose.project"}}`)
	if err != nil {
		return refuse("cannot list opted-in containers: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		id, project, _ := strings.Cut(line, " ")
		if !ProjectRE.MatchString(project) {
			return refuse("container %s is opted in (whalenurse.enable=true) but belongs to project %q", id, project)
		}
	}
	return nil
}

// Actor wraps whalenurse's own Actor: the healer under test can restart or
// start only containers of a matching project.
type Actor struct {
	Reader docker.Reader
	Inner  docker.Actor
}

func (a Actor) check(ctx context.Context, id string) error {
	c, err := a.Reader.Inspect(ctx, id)
	if err != nil {
		return refuse("cannot inspect %q: %v", id, err)
	}
	if p := c.Labels[docker.LabelProject]; !ProjectRE.MatchString(p) {
		return refuse("whalenurse tried to act on %q (project %q)", c.Name, p)
	}
	return nil
}

func (a Actor) Restart(ctx context.Context, id string) error {
	if err := a.check(ctx, id); err != nil {
		return err
	}
	return a.Inner.Restart(ctx, id)
}

func (a Actor) Start(ctx context.Context, id string) error {
	if err := a.check(ctx, id); err != nil {
		return err
	}
	return a.Inner.Start(ctx, id)
}

var _ docker.Actor = Actor{}

// TagPrefix is the only image namespace the tests may create.
const TagPrefix = "whalenurse-test/"

// RegistryRE is the only kind of registry reference the tests may push to or pull from: the loopback registry:2 that a
// test starts for itself, in the whalenurse-test/ namespace. 127.0.0.1 and not localhost: no resolver, no ::1, and the
// Engine treats 127.0.0.0/8 as an insecure (plain HTTP) registry by default.
var RegistryRE = regexp.MustCompile(`^127\.0\.0\.1:[0-9]{2,5}/whalenurse-test/[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9._-]+)?$`)

// ImageIDRE is an image ID as `docker image inspect --format {{.Id}}` prints it.
var ImageIDRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func requireTag(tag string) error {
	if strings.ContainsAny(tag, " \t\n;&|$`") || !(strings.HasPrefix(tag, TagPrefix) || RegistryRE.MatchString(tag)) {
		return refuse("image tag %q is not under %s or in the test registry", tag, TagPrefix)
	}
	return nil
}

func requireRegistryRef(ref string) error {
	if !RegistryRE.MatchString(ref) {
		return refuse("%q is not a reference of the test registry (127.0.0.1:<port>/whalenurse-test/...)", ref)
	}
	return nil
}

// Push pushes a reference of the test registry.
func (g *Guard) Push(ctx context.Context, ref string) error {
	if err := requireRegistryRef(ref); err != nil {
		return err
	}
	_, err := g.R.Run(ctx, "docker", "push", "-q", ref)
	return err
}

// DockerConfigPrefix is the only kind of directory the tests may hand to `docker --config`: one they made themselves.
const DockerConfigPrefix = "whalenurse-test-docker-config-"

// PushWith is Push with a docker config directory of the test's own, which holds the credentials of an authenticated
// test registry (M7). Anything else as a config directory is refused: it could hold another registry's login.
func (g *Guard) PushWith(ctx context.Context, ref, configDir string) error {
	if err := requireRegistryRef(ref); err != nil {
		return err
	}
	if filepath.Dir(configDir) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(configDir), DockerConfigPrefix) {
		return refuse("docker config %q is not a %s* directory of the test", configDir, DockerConfigPrefix)
	}
	_, err := g.R.Run(ctx, "docker", "--config", configDir, "push", "-q", ref)
	return err
}

// PullRegistry pulls a reference of the test registry.
func (g *Guard) PullRegistry(ctx context.Context, ref string) error {
	if err := requireRegistryRef(ref); err != nil {
		return err
	}
	_, err := g.R.Run(ctx, "docker", "pull", "-q", ref)
	return err
}

// PublishedPort reads the host port a container published for a container port ("5000/tcp"): where the registry is.
func (g *Guard) PublishedPort(ctx context.Context, ctr, port string) (string, error) {
	p, err := g.ProjectOf(ctx, ctr)
	if err != nil {
		return "", err
	}
	if err := RequireProject(p); err != nil {
		return "", err
	}
	out, err := g.R.Run(ctx, "docker", "port", ctr, port)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	_, hostPort, ok := strings.Cut(line, ":")
	if !ok {
		return "", fmt.Errorf("cannot read a published port from %q", out)
	}
	return hostPort[strings.LastIndex(hostPort, ":")+1:], nil
}

// ImageID reads the ID of a local image (a read).
func (g *Guard) ImageID(ctx context.Context, ref string) (string, error) {
	out, err := g.R.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref)
	return strings.TrimSpace(out), err
}

// Tag points dst, a whalenurse-test/ tag, at src: a base image or another whalenurse-test/ tag.
func (g *Guard) Tag(ctx context.Context, src, dst string) error {
	if err := requireTag(dst); err != nil {
		return err
	}
	ok := strings.HasPrefix(src, TagPrefix) || RegistryRE.MatchString(src) || ImageIDRE.MatchString(src)
	for _, b := range BaseImages {
		ok = ok || src == b
	}
	if !ok {
		return refuse("tag source %q is neither a base image, nor under %s, nor in the test registry, nor an image ID", src, TagPrefix)
	}
	_, err := g.R.Run(ctx, "docker", "tag", src, dst)
	return err
}

// Untag removes a whalenurse-test/ tag (the image stays if something else uses it).
func (g *Guard) Untag(ctx context.Context, tag string) error {
	if err := requireTag(tag); err != nil {
		return err
	}
	_, err := g.R.Run(ctx, "docker", "rmi", tag)
	return err
}

var fromRE = regexp.MustCompile(`(?im)^\s*FROM\s+(\S+)`)

// Build builds a whalenurse-test/ image from a Dockerfile held in memory, whose every FROM is a base image.
func (g *Guard) Build(ctx context.Context, tag, dockerfile string) error {
	if err := requireTag(tag); err != nil {
		return err
	}
	froms := fromRE.FindAllStringSubmatch(dockerfile, -1)
	if len(froms) == 0 {
		return refuse("the Dockerfile has no FROM")
	}
	for _, m := range froms {
		allowed := false
		for _, b := range BaseImages {
			allowed = allowed || m[1] == b
		}
		if !allowed {
			return refuse("FROM %q is not a base image", m[1])
		}
	}
	dir, err := os.MkdirTemp("", "whalenurse-test-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return err
	}
	_, err = g.R.Run(ctx, "docker", "build", "-q", "-t", tag, dir)
	return err
}

// ImagePulls counts the image `pull` events in [since, until]: a heal must produce none (plan §3.1).
func (g *Guard) ImagePulls(ctx context.Context, since, until string) (int, error) {
	out, err := g.R.Run(ctx, "docker", "events", "--since", since, "--until", until, "--filter", "type=image", "--filter", "event=pull", "--format", "{{.Action}} {{.Actor.ID}}")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "pull") {
			n++
		}
	}
	return n, nil
}

// ImagePullRefs lists the references of the image `pull` events in [since, until] (what was pulled, as docker names it).
func (g *Guard) ImagePullRefs(ctx context.Context, since, until string) ([]string, error) {
	out, err := g.R.Run(ctx, "docker", "events", "--since", since, "--until", until, "--filter", "type=image", "--filter", "event=pull", "--format", "{{.Action}} {{.Actor.ID}}")
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "pull"); ok {
			refs = append(refs, strings.TrimSpace(rest))
		}
	}
	return refs, nil
}

// ---- compose, as whalenurse runs it --------------------------------------------------------------------------------

// CheckComposeCommand is the filter in front of whalenurse's compose: it accepts exactly the five shapes the compose
// package builds and nothing else. A command whalenurse should never run (down, rm, build, run, a different project,
// a pull that is not `--policy always <one service>`, an up that could pull or build or touch other services) is REFUSED.
func CheckComposeCommand(args []string) error {
	if len(args) < 3 || args[0] != "-p" {
		return refuse("compose command does not start with -p <project>: %v", args)
	}
	if err := RequireProject(args[1]); err != nil {
		return err
	}
	i := 2
	for i < len(args) {
		switch a := args[i]; {
		case a == "--project-directory" || a == "-f" || a == "--env-file" || a == "--progress":
			if i+1 >= len(args) {
				return refuse("%s without a value", a)
			}
			i += 2
		case a == "--dry-run":
			i++
		default:
			goto sub
		}
	}
	return refuse("compose command has no subcommand: %v", args)
sub:
	rest := args[i+1:]
	switch args[i] {
	case "version":
		if len(rest) > 1 || (len(rest) == 1 && rest[0] != "--short") {
			return refuse("compose version with %v", rest)
		}
	case "config":
		switch {
		case len(rest) == 1 && strings.HasPrefix(rest[0], "--hash=") && !strings.ContainsAny(rest[0], ",*"):
		case len(rest) == 4 && rest[0] == "--format" && rest[1] == "json" && rest[2] == "--no-env-resolution" && !strings.HasPrefix(rest[3], "-"):
			// The pull rung reads one service's pull_policy; the rest of the output is dropped by the caller.
		default:
			return refuse("compose config with %v: only --hash=<one service> or --format json --no-env-resolution <one service>", rest)
		}
	case "pull":
		// Rung 3 only: one service, with the policy flag, nothing else (no --include-deps).
		if len(rest) != 3 || rest[0] != "--policy" || rest[1] != "always" || strings.HasPrefix(rest[2], "-") {
			return refuse("compose pull with %v: only --policy always <one service>", rest)
		}
	case "up":
		want := map[string]bool{"-d": false, "--no-deps": false, "--no-build": false}
		pullNever, force, services := false, false, 0
		for j := 0; j < len(rest); j++ {
			switch a := rest[j]; {
			case a == "--pull":
				if j+1 >= len(rest) || rest[j+1] != "never" {
					return refuse("compose up --pull must be never: %v", rest)
				}
				pullNever = true
				j++
			case a == "--force-recreate":
				force = true
			case strings.HasPrefix(a, "-"):
				if _, ok := want[a]; !ok {
					return refuse("compose up with flag %s", a)
				}
				want[a] = true
			default:
				services++
			}
		}
		for f, seen := range want {
			if !seen {
				return refuse("compose up without %s", f)
			}
		}
		if !pullNever || services != 1 {
			return refuse("compose up needs --pull never and exactly one service: %v (force=%v)", rest, force)
		}
	default:
		return refuse("compose %s is not something whalenurse runs", args[i])
	}
	return nil
}

// ComposeRunner is the compose.Runner the healer under test uses: it checks every command with CheckComposeCommand, keeps
// a log of what ran, and then runs it for real.
type ComposeRunner struct {
	Inner compose.Runner
	mu    sync.Mutex
	log   []string

	// authDir is the one directory a `pull` may be given as DOCKER_CONFIG (WHALENURSE_REGISTRY_AUTH_DIR, M7); pullDirs
	// records the ConfigDir of each pull, in order.
	authDir  string
	pullDirs []string
}

// AllowAuthDir lets `compose pull` (and nothing else) run with dir as its DOCKER_CONFIG.
func (r *ComposeRunner) AllowAuthDir(dir string) { r.authDir = dir }

// PullConfigDirs returns the ConfigDir each pull ran with ("" for the runner's own empty directory).
func (r *ComposeRunner) PullConfigDirs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pullDirs...)
}

// NewComposeRunner runs real compose behind the filter, with configDir (an empty directory, as whalenurse gives it) as its
// DOCKER_CONFIG.
func NewComposeRunner(configDir string) *ComposeRunner {
	return &ComposeRunner{Inner: compose.ExecRunner{ConfigDir: configDir}}
}

func (r *ComposeRunner) Run(ctx context.Context, c compose.Command) (compose.Result, error) {
	if filepath.Base(c.Bin) != "docker-compose" {
		return compose.Result{}, refuse("compose binary %q", c.Bin)
	}
	if err := CheckComposeCommand(c.Args); err != nil {
		return compose.Result{}, err
	}
	isPull := strings.Contains(strings.Join(c.Args, " "), " pull --policy always ")
	if c.ConfigDir != "" && !(isPull && r.authDir != "" && c.ConfigDir == r.authDir) {
		return compose.Result{}, refuse("compose %v with DOCKER_CONFIG %q: only a pull may use the credentials directory", c.Args, c.ConfigDir)
	}
	r.mu.Lock()
	r.log = append(r.log, strings.Join(c.Args, " "))
	if isPull {
		r.pullDirs = append(r.pullDirs, c.ConfigDir)
	}
	r.mu.Unlock()
	return r.Inner.Run(ctx, c)
}

// Log returns the commands that ran, in order.
func (r *ComposeRunner) Log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

// Mutating returns the logged commands that could have changed something: an up that is not a --dry-run (a pull is
// counted by Pulls).
func (r *ComposeRunner) Mutating() []string {
	var out []string
	for _, l := range r.Log() {
		if strings.Contains(l, " up ") && !strings.Contains(l, "--dry-run") {
			out = append(out, l)
		}
	}
	return out
}

// Pulls returns the logged `compose pull` commands: rung 3, the only place whalenurse fetches an image.
func (r *ComposeRunner) Pulls() []string {
	var out []string
	for _, l := range r.Log() {
		if strings.Contains(l, " pull --policy ") {
			out = append(out, l)
		}
	}
	return out
}

var _ compose.Runner = (*ComposeRunner)(nil)

// ---- the rollback of an upgrade -------------------------------------------------------------------------------------

var serviceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

var rollbackTagRE = regexp.MustCompile(`^docker image tag (sha256:[0-9a-f]{64}) (\S+)$`)

// RunRollback runs the two lines of the rollback an "upgraded during heal" message gave, exactly as a person would paste
// them, after checking each: the first must tag an image ID onto a reference of the test registry; the second must be
// `docker compose -p <test project> [--project-directory D] [-f F]... [--env-file E]... up -d --no-deps --pull never --no-build <svc>...`
// (the parent and the sidecars recreated with it; plan v3.13 item 7 added --no-build, and the form written before it, without
// the flag, is accepted too).
// Anything else is REFUSED, so a rollback that named another project or added a flag cannot do harm.
func (g *Guard) RunRollback(ctx context.Context, tagLine, upLine string) error {
	m := rollbackTagRE.FindStringSubmatch(tagLine)
	if m == nil {
		return refuse("rollback line %q is not `docker image tag <image id> <ref>`", tagLine)
	}
	if err := requireTag(m[2]); err != nil {
		return err
	}
	args := strings.Fields(upLine)
	if len(args) < 6 || args[0] != "docker" || args[1] != "compose" || args[2] != "-p" {
		return refuse("rollback line %q is not a docker compose -p command", upLine)
	}
	if err := RequireProject(args[3]); err != nil {
		return err
	}
	tail := []string{"up", "-d", "--no-deps", "--pull", "never"}
	rest := args[4:]
	i := 0
	for i < len(rest) && (rest[i] == "--project-directory" || rest[i] == "-f" || rest[i] == "--env-file") {
		if i+1 >= len(rest) || strings.ContainsAny(rest[i+1], "'\"$`;&|") {
			return refuse("rollback line %q: odd value for %s", upLine, rest[i])
		}
		i += 2
	}
	rest = rest[i:]
	if len(rest) > len(tail) && rest[len(tail)] == "--no-build" {
		tail = append(tail, "--no-build")
	}
	if len(rest) < len(tail)+1 {
		return refuse("rollback line %q does not end in `%s <service>...`", upLine, strings.Join(tail, " "))
	}
	for j, w := range tail {
		if rest[j] != w {
			return refuse("rollback line %q does not end in `%s <service>...`", upLine, strings.Join(tail, " "))
		}
	}
	for _, svc := range rest[len(tail):] {
		if !serviceRE.MatchString(svc) {
			return refuse("rollback line %q: %q is not a compose service name", upLine, svc)
		}
	}
	// Both lines are good: now run them, in order.
	if err := g.Tag(ctx, m[1], m[2]); err != nil {
		return err
	}
	g.touch(args[3])
	_, err := g.R.Run(ctx, "docker", args[1:]...)
	return err
}

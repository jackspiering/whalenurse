package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Command is one invocation of the compose binary.
type Command struct {
	Bin  string
	Args []string
	Dir  string
	// ConfigDir, when set, is the DOCKER_CONFIG of this one command instead of the runner's. Only the pull rung's
	// `compose pull` sets it, to WHALENURSE_REGISTRY_AUTH_DIR.
	ConfigDir string
}

func (c Command) String() string { return c.Bin + " " + strings.Join(c.Args, " ") }

// Result is what a command printed. Compose writes its progress, including the --progress json lines of the dry-run
// oracle, to stderr, so both streams are kept.
type Result struct {
	Stdout, Stderr []byte
}

// Combined is stdout followed by stderr.
func (r Result) Combined() []byte {
	return append(append([]byte(nil), r.Stdout...), r.Stderr...)
}

// Runner runs a command. Everything in this package goes through it, so a unit test never runs compose and the
// integration tests wrap it in a fail-closed filter (test/integration/guard.go).
type Runner interface {
	Run(ctx context.Context, c Command) (Result, error)
}

// ExecRunner runs real commands, with an environment that cannot redirect compose at another project or Docker config.
// ConfigDir is the DOCKER_CONFIG the commands get: an empty directory of whalenurse's own (PrivateConfigDir); the zero
// value is NoDockerConfig.
type ExecRunner struct {
	ConfigDir string
}

// StopDelay is how long a cancelled command has between SIGTERM and SIGKILL (and how long Run then waits for its output
// to close). A compose up that is stopped mid-recreate with SIGTERM can still finish or undo a step; SIGKILL cannot.
var StopDelay = 10 * time.Second

func (r ExecRunner) Run(ctx context.Context, c Command) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Bin, c.Args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = StopDelay
	// Its own process group: a Ctrl-C in the terminal reaches whalenurse, which lets the command finish, and not the
	// command itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = c.Dir
	dir := r.ConfigDir
	if c.ConfigDir != "" {
		dir = c.ConfigDir
	}
	cmd.Env = ChildEnv(os.Environ(), dir)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	if err != nil {
		return res, fmt.Errorf("%s: %w: %s", c, err, tail(errb.String()))
	}
	return res, nil
}

// Compose must always get a DOCKER_CONFIG: unset, the Docker CLI reads $HOME/.docker, and a config.json there brings
// credentials and a currentContext that points compose at another daemon. The directory must be empty and must be
// writable: compose v5.2.0 to v5.5.x build buildx's store in their dry-run client (buildx store.New creates
// <config>/buildx/{instances,defaults,activity}), and when that fails the dry-run fails and the oracle is lost. So it is a
// fresh directory of whalenurse's own (PrivateConfigDir), and NoDockerConfig is only the last resort.

// NoDockerConfig is the DOCKER_CONFIG when there is no private directory: a path that cannot exist (nothing, root
// included, can create an entry in /proc), which the Docker CLI reads as "no config file". A path under a regular file
// (/dev/null/x) would not do: that is ENOTDIR, which the CLI warns about on every command, onto the stderr the oracle
// parses. With it, the dry-run of compose v5.2.0 to v5.5.x fails and the drift guard falls back to config --hash.
const NoDockerConfig = "/proc/whalenurse-no-docker-config"

// PrivateConfigDir creates an empty directory (mode 0700, a fresh random name, so nothing can be planted in it
// beforehand) in the first of the parents where that works, for the DOCKER_CONFIG of the compose commands. remove deletes
// it with whatever compose put there.
func PrivateConfigDir(parents ...string) (dir string, remove func(), err error) {
	var errs []error
	for _, p := range parents {
		if p == "" {
			continue
		}
		d, err := os.MkdirTemp(p, "whalenurse-docker-config-")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		return d, func() { _ = os.RemoveAll(d) }, nil
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("no directory to create it in"))
	}
	return "", func() {}, errors.Join(errs...)
}

// ChildEnv is the environment compose runs with: the parent's, minus everything that could point compose at another
// project, file set, profile, Docker config or credentials (COMPOSE_*, DOCKER_CONTEXT, DOCKER_CONFIG,
// DOCKER_AUTH_CONFIG), and minus whalenurse's own settings, plus DOCKER_CONFIG=configDir (NoDockerConfig when empty) so
// $HOME/.docker is never read, and NO_COLOR so the output is plain.
func ChildEnv(env []string, configDir string) []string {
	if configDir == "" {
		configDir = NoDockerConfig
	}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "COMPOSE_"), strings.HasPrefix(k, "WHALENURSE_"), k == "DOCKER_CONTEXT", k == "DOCKER_CONFIG",
			k == "DOCKER_AUTH_CONFIG", k == "NO_COLOR":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "DOCKER_CONFIG="+configDir, "NO_COLOR=1")
}

// tail keeps the end of compose's complaint: the cause is last.
func tail(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 600 {
		return "..." + s[len(s)-600:]
	}
	return s
}

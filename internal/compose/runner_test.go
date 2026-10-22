package compose

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A compose command that is cancelled (its timeout, or the end of whalenurse's shutdown grace) gets SIGTERM first, so
// compose can stop what it is doing, and SIGKILL only StopDelay later.
func TestExecRunnerStopsComposeWithSIGTERMFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := ExecRunner{}.Run(ctx, Command{Bin: "/bin/sh", Args: []string{"-c", "trap 'echo got-term >&2; kill $!; exit 3' TERM; sleep 5 & wait"}, Dir: "/"})
	if err == nil {
		t.Fatal("no error from a cancelled command")
	}
	if !strings.Contains(err.Error(), "got-term") || strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("not stopped by SIGTERM: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("took %s", time.Since(start))
	}
}

// A command that ignores SIGTERM is still killed after StopDelay.
func TestExecRunnerKillsACommandThatIgnoresSIGTERM(t *testing.T) {
	old := StopDelay
	StopDelay = 300 * time.Millisecond
	defer func() { StopDelay = old }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := ExecRunner{}.Run(ctx, Command{Bin: "/bin/sh", Args: []string{"-c", "trap '' TERM; sleep 5"}, Dir: "/"})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err %v after %s", err, time.Since(start))
	}
}

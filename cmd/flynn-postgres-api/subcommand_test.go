package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// NEW-54: an unknown subcommand (an older image asked for a newer action,
// such as `console`) fell through to starting the API server, so the job
// hung. It must exit non-zero with a usage error at once.
func TestMainRejectsUnknownSubcommand(t *testing.T) {
	if os.Getenv("FLYNN_TEST_RUN_MAIN") == "1" {
		os.Args = append([]string{"flynn-postgres-api"}, strings.Fields(os.Getenv("FLYNN_TEST_MAIN_ARGS"))...)
		main()
		return
	}
	for _, args := range []string{"console", "bogus x", "--nope"} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainRejectsUnknownSubcommand$")
		cmd.Env = append(os.Environ(), "FLYNN_TEST_RUN_MAIN=1", "FLYNN_TEST_MAIN_ARGS="+args, "PORT=0", "CONTROLLER_KEY=")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		timedOut := ctx.Err() != nil
		cancel()
		if timedOut {
			t.Fatalf("%q: still running after 20s (started the server)", args)
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 2 {
			t.Fatalf("%q: exit %v, want status 2; stderr:\n%s", args, err, stderr.String())
		}
		first := strings.Fields(args)[0]
		if !strings.Contains(stderr.String(), "unknown subcommand \""+first+"\"") || !strings.Contains(stderr.String(), "usage: flynn-postgres-api") {
			t.Fatalf("%q: stderr %q", args, stderr.String())
		}
	}
}

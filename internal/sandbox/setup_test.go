package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A SETUP FAILURE SAYS WHICH OF THREE THINGS FAILED: a file the box would not
// take, a command that exited non-zero, or a command that reported no exit
// status.
//
// One sentence covered all three, so a direct box refusing a file at a system
// path read "command #0 failed (exit 0)" — no step has a command #0, and exit
// 0 is success — and a command with no exit status read "failed (exit 0)" as
// well. Each message here is the one an operator reads in
// sandbox_setup_failed and an agent reads back from its failed launch.
func TestASetupFailureSaysWhatFailed(t *testing.T) {
	t.Parallel()

	t.Run("a file the box refuses", func(t *testing.T) {
		t.Parallel()
		box := mustCreate(t, newDirect(t), Spec{Placement: Direct})
		err := ApplySetup(t.Context(), box, []SetupStep{{
			Name:     "git-auth",
			Files:    map[string]string{"/usr/local/bin/git-credential-crewlet": "#!/bin/sh\n"},
			Commands: []string{"true"},
		}}, nil)
		var setup *SetupError
		if !errors.As(err, &setup) {
			t.Fatalf("a refused file is not a SetupError: %v", err)
		}
		if setup.File != "/usr/local/bin/git-credential-crewlet" || setup.Command != 0 || setup.Exited {
			t.Errorf("a refused file reads as %+v, want the file and no command", setup)
		}
		msg := err.Error()
		if !strings.Contains(msg, `setup step "git-auth" could not write /usr/local/bin/git-credential-crewlet`) {
			t.Errorf("the message does not name the file it could not write: %s", msg)
		}
		if strings.Contains(msg, "command #") || strings.Contains(msg, "exit") {
			t.Errorf("a file the box refused is described as a command and an exit status: %s", msg)
		}
		if !strings.Contains(msg, "outside the box") {
			t.Errorf("the box's own reason was dropped: %s", msg)
		}
	})

	t.Run("a command that exits non-zero", func(t *testing.T) {
		t.Parallel()
		box := mustCreate(t, newDirect(t), Spec{Placement: Direct})
		err := ApplySetup(t.Context(), box, []SetupStep{{
			Name:     "deps",
			Commands: []string{"true", "echo no registry >&2; exit 3"},
		}}, nil)
		var setup *SetupError
		if !errors.As(err, &setup) {
			t.Fatalf("a failed command is not a SetupError: %v", err)
		}
		if !setup.Exited || setup.Exit != 3 || setup.Command != 2 {
			t.Errorf("a command that exited 3 reads as %+v", setup)
		}
		if msg := err.Error(); msg != `setup step "deps" command #2 failed (exit 3) — no registry` {
			t.Errorf("message = %q", msg)
		}
	})

	// A SIGNAL IS A STATUS TOO, read as a shell reads one. The process a
	// direct box starts is the command's own shell, and when a signal ends
	// that shell itself — an OOM kill, a command that execs into a program
	// that dies of one — the host reported -1, which read "failed (exit -1)":
	// a status no process exits with, and nothing naming the signal.
	t.Run("a command whose own shell a signal ends", func(t *testing.T) {
		t.Parallel()
		box := mustCreate(t, newDirect(t), Spec{Placement: Direct})
		err := ApplySetup(t.Context(), box, []SetupStep{{
			Name:     "deps",
			Commands: []string{"echo out of memory >&2; kill -TERM $$"},
		}}, nil)
		var setup *SetupError
		if !errors.As(err, &setup) {
			t.Fatalf("a signalled command is not a SetupError: %v", err)
		}
		if !setup.Exited || setup.Exit != 128+15 || setup.Command != 1 {
			t.Errorf("a command its shell's SIGTERM ended reads as %+v, want exit 143", setup)
		}
		want := `setup step "deps" command #1 failed (exit 143) — out of memory` + "\n" +
			`killed by signal 15 (terminated)`
		if msg := err.Error(); msg != want {
			t.Errorf("message = %q, want %q", msg, want)
		}
	})

	t.Run("a command that reports no exit status", func(t *testing.T) {
		t.Parallel()
		box := NewFakeSandbox("unreachable")
		box.ExecFunc = func(context.Context, string, ExecOptions) (ExecResult, error) {
			// What an E2B box answers for a command whose stream ended
			// without a verdict: it may well have started.
			return ExecResult{ExitCode: 1}, errors.New("box went away mid-command")
		}
		err := ApplySetup(t.Context(), box, []SetupStep{{
			Name:     "deps",
			Commands: []string{"make deps"},
		}}, nil)
		var setup *SetupError
		if !errors.As(err, &setup) {
			t.Fatalf("a command with no exit status is not a SetupError: %v", err)
		}
		if setup.Exited || setup.Command != 1 {
			t.Errorf("a command with no exit status reads as %+v", setup)
		}
		if msg := err.Error(); msg != `setup step "deps" command #1 reported no exit status — box went away mid-command` {
			t.Errorf("message = %q", msg)
		}
	})
}

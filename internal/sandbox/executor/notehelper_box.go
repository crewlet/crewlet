package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// noteHelperArch is the note helper build a box of machine runs, its `uname
// -m` as Go names the architecture, refused where none is built.
func noteHelperArch(machine string) (string, error) {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("executor: this box's machine is %s, and the note helper is built "+
		"for x86_64 and aarch64 alone: %w", strconv.Quote(strings.TrimSpace(machine)), errNoHelperBuild)
}

// errNoHelperBuild is a box whose machine no note helper is built for
// ([noteHelperArch]): a run that needs the helper is refused, and a Codex run
// that does not starts without what the helper hands it. An install missing a
// build that exists is never this ([noteHelperBinary]).
var errNoHelperBuild = errors.New("no note helper build for this machine")

// writeNoteHelper writes the note helper's build for box's machine
// ([noteHelperBinary]) to path, at every launch, never cached: a box reused
// across builds would otherwise run another build's helper against this
// one's bridge. The run's script makes it executable.
func writeNoteHelper(ctx context.Context, box sandbox.Sandbox, path string) error {
	helper, err := noteHelperFor(ctx, box)
	if err != nil {
		return err
	}
	return writeHelperBuild(ctx, box, path, helper)
}

// noteHelperFor is the note helper's build for box's machine, read before
// anything is written ([errNoHelperBuild] where none is built).
func noteHelperFor(ctx context.Context, box sandbox.Sandbox) ([]byte, error) {
	machine, err := box.Exec(ctx, "uname -m", sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: reading the box's machine for the note helper: %w", err)
	}
	arch, err := noteHelperArch(machine.Stdout)
	if err != nil {
		return nil, err
	}
	return noteHelperBinary(arch)
}

// writeHelperBuild writes helper, a build [noteHelperFor] read, to path as a
// program the box runs ([writeProgram]).
func writeHelperBuild(ctx context.Context, box sandbox.Sandbox, path string, helper []byte) error {
	if err := writeProgram(ctx, box, path, helper); err != nil {
		return fmt.Errorf("executor: writing the note helper: %w", err)
	}
	return nil
}

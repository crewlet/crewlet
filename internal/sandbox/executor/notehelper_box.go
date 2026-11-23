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
// -m` as Go names the architecture, refused where the engine embeds none.
func noteHelperArch(machine string) (string, error) {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("executor: this box's machine is %s, and the engine carries the note helper "+
		"for x86_64 and aarch64 alone: %w", strconv.Quote(strings.TrimSpace(machine)), errNoHelperBuild)
}

// errNoHelperBuild is a box whose machine the engine carries no note helper
// build for ([noteHelperArch]): a run that needs the helper is refused, and a
// Codex run that does not starts without what the helper hands it.
var errNoHelperBuild = errors.New("no note helper build for this machine")

// writeNoteHelper writes the note helper's build for box's machine
// ([noteHelperBinary]) to path, at every launch, never cached: a box reused
// across builds would otherwise run another build's helper against this
// one's bridge. The run's script makes it executable.
func writeNoteHelper(ctx context.Context, box sandbox.Sandbox, path string) error {
	machine, err := box.Exec(ctx, "uname -m", sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("executor: reading the box's machine for the note helper: %w", err)
	}
	arch, err := noteHelperArch(machine.Stdout)
	if err != nil {
		return err
	}
	helper, err := noteHelperBinary(arch)
	if err != nil {
		return err
	}
	if err = box.WriteFile(ctx, path, helper); err != nil {
		return fmt.Errorf("executor: writing the note helper: %w", err)
	}
	return nil
}

package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// errNoNoteHelper marks a box whose machine the engine carries no note helper
// build for ([noteHelperArch]).
var errNoNoteHelper = errors.New("the engine carries the note helper for x86_64 and aarch64 alone")

// noteHelperArch is the note helper build a box of machine runs, its `uname
// -m` as Go names the architecture, refused where the engine embeds none.
func noteHelperArch(machine string) (string, error) {
	switch strings.TrimSpace(machine) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("executor: this box's machine is %s, and %w", strconv.Quote(strings.TrimSpace(machine)),
		errNoNoteHelper)
}

// writeNoteHelper is the ONE writer of the note helper's build for box's
// machine ([noteHelperBinary]) at [Paths.NoteHelper], executable, for every
// harness that runs it. The box's copy is kept where `sha256sum` reads it as
// this engine's build, so a reused box takes no upload of several megabytes per
// launch, and replaced otherwise (removed first, so the harness opens a file
// new to the box): a copy another build wrote, or anything else, never runs
// against this build's bridge. A box without `sha256sum` takes it at every
// launch. No clear removes it ([Runner.ClearArtifacts]). A machine with no
// build is [errNoNoteHelper].
func writeNoteHelper(ctx context.Context, box sandbox.Sandbox, path string) error {
	q := shellword.Quote(path)
	probe, err := box.Exec(ctx, "uname -m; chmod 0755 "+q+" 2> /dev/null; sha256sum "+q+" 2> /dev/null || true",
		sandbox.ExecOptions{})
	if err != nil {
		return fmt.Errorf("executor: reading the box's machine for the note helper: %w", err)
	}
	machine, held, _ := strings.Cut(probe.Stdout, "\n")
	arch, err := noteHelperArch(machine)
	if err != nil {
		return err
	}
	sum, err := noteHelperSum(arch)
	if err != nil {
		return err
	}
	if f := strings.Fields(held); len(f) > 0 && f[0] == sum {
		return nil
	}
	helper, err := noteHelperBinary(arch)
	if err != nil {
		return err
	}
	if _, err = box.Exec(ctx, "rm -f "+q, sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: removing the box's note helper: %w", err)
	}
	if err = box.WriteFile(ctx, path, helper); err != nil {
		return fmt.Errorf("executor: writing the note helper: %w", err)
	}
	if _, err = box.Exec(ctx, "chmod 0755 "+q, sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("executor: making the note helper executable: %w", err)
	}
	return nil
}

// Package helperbuild builds the note helper (internal/notehelper) for every
// box arch: the one recipe the release, the image, `make notehelper` and the
// tests run, so a helper is always built from the source beside the engine it
// serves and nothing executable is committed.
//
// A box is Linux whatever host the engine runs on, and its arch is its own, so
// every engine install carries a build per arch in [Archs], named [File]. The
// engine finds them at run time (internal/sandbox/executor's
// noteHelperPlaces): beside its executable, under /usr/local/share/crewlet
// (the image), then in the directory [DirEnv] names.
//
// # The recipe
//
// Static (CGO_ENABLED=0), for the oldest CPU of each arch a box may run on
// (GOAMD64=v1, GOARM64=v8.0), with -trimpath, no VCS stamp and an empty build
// id, so the bytes depend on the source, the modules and the toolchain alone.
// GOFLAGS is cleared: a caller's flags (a test run's -race, a -tags) are for
// what the caller builds, never for the helper.
//
// # Tests
//
// A test binary whose engine launches a run in a box wraps its run in
// [WithHelpers], which builds the helpers once into a temporary directory and
// names it in [DirEnv]. The test binary sits in go's own temporary directory,
// so nothing is found beside it.
package helperbuild

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// DirEnv names the environment variable holding a directory of note helper
// builds, the last place the engine looks.
const DirEnv = "CREWLET_NOTE_HELPER_DIR"

// Archs are the box arches the helper is built for, by GOARCH's names.
var Archs = []string{"amd64", "arm64"}

// pkg is the helper's import path: built by path, so a build runs from any
// directory of the module, a test's package directory included.
const pkg = "github.com/crewlet/crewlet/internal/notehelper"

// File is the name of the helper's build for a linux box of arch.
func File(arch string) string { return "notehelper-linux-" + arch }

// Build writes the helper's build for every arch in [Archs] into dir, as
// [File] names each, by goCmd.
func Build(ctx context.Context, goCmd, dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("helperbuild: making %s: %w", dir, err)
	}
	for _, arch := range Archs {
		out := filepath.Join(dir, File(arch))
		cmd := exec.CommandContext(ctx, goCmd, "build", "-trimpath", "-buildvcs=false", //nolint:gosec // the caller's go, building this module's helper
			"-ldflags=-s -w -buildid=", "-o", out, pkg)
		// The last of a duplicated key wins (os/exec), so these hold whatever
		// the caller's environment says.
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOAMD64=v1",
			"GOARM64=v8.0", "GOFLAGS=")
		if msg, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("helperbuild: building %s with %s: %w\n%s", out, goCmd, err, msg)
		}
	}
	return nil
}

// GoCommand is the go a test builds the helper with: the Makefile's
// (CREWLET_GO), else the one on PATH, which `go test` puts its own GOROOT's
// bin before.
func GoCommand() string {
	if selected := os.Getenv("CREWLET_GO"); selected != "" {
		return selected
	}
	return "go"
}

// WithHelpers builds the helpers into a temporary directory, names it in
// [DirEnv], and returns run's status, removing the directory after: a test
// binary's TestMain runs `os.Exit(helperbuild.WithHelpers(m.Run))`. A build
// that fails ends the binary with 1, naming why.
func WithHelpers(run func() int) int {
	dir, err := os.MkdirTemp("", "notehelper-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "helperbuild: a directory for the note helper's builds:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err = Build(context.Background(), GoCommand(), dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err = os.Setenv(DirEnv, dir); err != nil {
		fmt.Fprintln(os.Stderr, "helperbuild:", err)
		return 1
	}
	return run()
}

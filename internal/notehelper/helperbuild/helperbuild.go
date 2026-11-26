// Package helperbuild builds the note helper (internal/notehelper) for every
// box arch: the one recipe the release, the image, `make notehelper` and the
// tests run, so a helper is always built from the source beside the engine it
// serves and nothing executable is committed.
//
// A box is Linux whatever host the engine runs on, and its arch is its own, so
// every engine install carries a build per arch in [Archs], named [File]. The
// engine finds them at run time (internal/sandbox/executor's
// noteHelperSearch): only in the directory [DirEnv] names while it is set,
// else beside its executable (a release archive, `make crewlet`), then under
// /usr/local/share/crewlet (the image).
//
// # The recipe
//
// Static (CGO_ENABLED=0), for the oldest CPU of each arch a box may run on
// (GOAMD64=v1, GOARM64=v8.0), with -trimpath, no VCS stamp and an empty build
// id, so the bytes depend on the source, the modules and the toolchain alone.
// GOFLAGS is cleared: a caller's flags (a test run's -race, a -tags) are for
// what the caller builds, never for the helper. It builds with the go that
// builds the engine beside it, with no toolchain pin of its own: the pin to
// go.mod's go line existed only so committed builds matched a rebuild.
//
// # Tests
//
// A test binary whose engine launches a run in a box wraps its run in
// [WithHelpers], which builds the helpers once into a temporary directory and
// names it in [DirEnv]. The test binary sits in go's own temporary directory,
// so nothing is found beside it.
package helperbuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/notehelper/helpersource"
)

// DirEnv names the environment variable holding a directory of note helper
// builds: while it is set, the only place the engine looks.
const DirEnv = "CREWLET_NOTE_HELPER_DIR"

// Archs are the box arches the helper is built for, by GOARCH's names.
var Archs = []string{"amd64", "arm64"}

// pkg is the helper's import path: built by path, so a build runs from any
// directory of the module, a test's package directory included.
const pkg = "github.com/crewlet/crewlet/internal/notehelper"

// sourcePkg holds the digest, so it is left out of what the digest covers.
const sourcePkg = "github.com/crewlet/crewlet/internal/notehelper/helpersource"

// DigestFile is the generated file holding helpersource.Digest, from the
// module's root.
const DigestFile = "internal/notehelper/helpersource/digest.go"

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

// SourceDigest is helpersource.Digest as the helper's source makes it now (the
// package doc of helpersource says what it covers), listed by goCmd.
func SourceDigest(ctx context.Context, goCmd string) (string, error) {
	dirs := map[string]string{}
	for _, arch := range Archs {
		cmd := exec.CommandContext(ctx, goCmd, "list", "-deps", "-json=ImportPath,Dir,Module", pkg) //nolint:gosec // the caller's go, listing this module's helper
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch, "GOFLAGS=")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("helperbuild: listing the note helper's packages with %s: %w\n%s", goCmd, err, stderr.String())
		}
		for dec := json.NewDecoder(bytes.NewReader(out)); ; {
			var p struct {
				ImportPath, Dir string
				Module          *struct{ Main bool }
			}
			if err = dec.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return "", fmt.Errorf("helperbuild: reading go list's answer: %w", err)
			}
			if p.Module != nil && p.Module.Main && p.ImportPath != sourcePkg {
				dirs[p.ImportPath] = p.Dir
			}
		}
	}
	h := sha256.New()
	paths := make([]string, 0, len(dirs))
	for path := range dirs {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		files, err := filepath.Glob(filepath.Join(dirs[path], "*.go"))
		if err != nil {
			return "", err
		}
		slices.Sort(files)
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file) //nolint:gosec // a file of this module's own source
			if err != nil {
				return "", fmt.Errorf("helperbuild: reading the note helper's source: %w", err)
			}
			_, _ = fmt.Fprintf(h, "%s/%s %d\n", path, filepath.Base(file), len(src))
			_, _ = h.Write(src)
		}
	}
	return helpersource.Prefix + hex.EncodeToString(h.Sum(nil)), nil
}

// WriteDigest rewrites root's [DigestFile] to hold digest.
func WriteDigest(root, digest string) error {
	hexDigest, ok := strings.CutPrefix(digest, helpersource.Prefix)
	if !ok {
		return fmt.Errorf("helperbuild: %q is not a note helper source digest", digest)
	}
	src := "// Code generated by `go run ./scripts/notehelper -digest`. DO NOT EDIT.\n\n" +
		"package helpersource\n\n" +
		"// Digest is the note helper's source, as the package doc says.\n" +
		"const Digest = Prefix + \"" + hexDigest + "\"\n"
	return os.WriteFile(filepath.Join(root, DigestFile), []byte(src), 0o644) //nolint:gosec // committed source, read by everyone
}

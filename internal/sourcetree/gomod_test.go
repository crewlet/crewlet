package sourcetree

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// NO PACKAGE PATTERN READS A DEPENDENCY INSTALL AS THIS MODULE'S: handed this
// repository's own go.mod, the go command lists the packages [Walk] hands
// over and none that a `node_modules` holds, wherever the install sits.
//
// The guards read this tree through the walk, but `go vet ./...`, `go build
// ./...`, golangci-lint and internal/solo/partition read it through the go
// command, whose `...` steps over only names starting with `.` or `_`,
// `vendor` and testdata, and a directory holding a go.mod of its own. An npm
// package can ship Go without a go.mod, and flatted does: once `npm ci` had
// run in dashboard/, every one of those took flatted in, so golangci-lint
// failed the second `make check` of a fresh checkout on the package comment
// flatted does not have, and `make test` compiled and ran it. CI's Go jobs run
// where no install has, so only a contributor's gate ever saw it. The rule
// therefore sits in go.mod, as `ignore node_modules`, where every command that
// expands a pattern reads it, and this holds that line to the walk's rule in
// both directions by taking the walk's answer as the want: an anchored
// `ignore ./dashboard/node_modules` lists the other two installs, and a walk
// that started reading installs would list what go.mod leaves out.
//
// The module is a temporary one: the repository's real go.mod, a package of
// this tree, and an install at the root (where a node tool run from there
// writes one), in dashboard/ at the incident's own path, and below a package
// of the tree. Every package imports nothing, so the listing reads no go.sum
// and no module, and with GOPROXY off a listing that wanted one fails here
// rather than fetching it.
func TestNoPackagePatternReadsADependencyInstall(t *testing.T) {
	t.Parallel()
	gomod, err := os.ReadFile(filepath.Join(Root(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	root := layout(t, map[string]string{
		"go.mod":                    string(gomod),
		"internal/ours/ours.go":     "package ours\n",
		"node_modules/tool/tool.go": "package tool\n",
		"dashboard/node_modules/flatted/golang/pkg/flatted/flatted.go": "package flatted\n",
		"internal/ours/node_modules/dep/dep.go":                        "package dep\n",
	})
	want := walkedPackageDirs(t, root)
	if !slices.Contains(want, "internal/ours") {
		t.Fatalf("the walk handed over packages in %q, without the tree's own: "+
			"there is nothing for the listing to agree with", want)
	}
	if got := listedPackageDirs(t, root); !slices.Equal(got, want) {
		t.Errorf("go list ./... over this repository's go.mod lists packages in %q, "+
			"but the walk hands over %q: go.mod's `ignore node_modules` and the walk "+
			"no longer agree on which directories are this module's", got, want)
	}
}

// walkedPackageDirs is every directory below root, relative to it and sorted,
// holding a Go file that [Walk] hands over.
func walkedPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := Walk(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir(), !strings.HasSuffix(d.Name(), ".go"):
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dirs = append(dirs, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(dirs)
	return slices.Compact(dirs)
}

// listedPackageDirs is the directory of every package `go list ./...` finds
// in the module at root, relative to it and sorted.
//
// Each directory is taken relative to the module's, both as the go command
// spells them, so a temporary directory reached through a symbolic link reads
// the same as one that is not.
func listedPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	// The caller's toolchain, for the reason internal/solo/partition gives at
	// goCommand: the go that `make` expands every ./... with.
	goCmd := os.Getenv("CREWLET_GO")
	if goCmd == "" {
		goCmd = "go"
	}
	cmd := exec.CommandContext(t.Context(), goCmd, "list", "-f", "{{.Module.Dir}}\t{{.Dir}}", "./...")
	cmd.Dir = root
	// GOWORK off, so a go.work above wherever TMPDIR points cannot turn this
	// into that workspace's listing; GOPROXY off, so a module the listing
	// wanted is a failure here and never a download.
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list ./...: %v\n%s", err, stderr.Bytes())
	}
	var dirs []string
	for line := range strings.Lines(string(out)) {
		module, dir, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t")
		if !ok {
			t.Fatalf("go list printed %q, not a module directory and a package directory", line)
		}
		rel, err := filepath.Rel(module, dir)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, filepath.ToSlash(rel))
	}
	slices.Sort(dirs)
	return dirs
}

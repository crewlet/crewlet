// Command notices writes the third-party notices for the crewlet binary: the
// license and notice files of every Go module linked into it, and of the Go
// toolchain whose standard library and runtime it carries.
//
// goreleaser runs it as a before hook (.goreleaser.yaml), and the release
// archives and the container image ship what it writes beside LICENSE. The
// binary is a redistribution of every module it links, and most of those
// licenses (Apache-2.0 for the NATS server and OpenTelemetry, BSD for the Go
// toolchain and golang.org/x) require their notices to travel with it.
//
// # Linked modules, not the module graph
//
// The set is what `go list -deps` reports for the main package on each release
// target, merged. `go list -m all` is the obvious source and the wrong one: it
// is the whole module graph, test-only dependencies of dependencies included,
// and most of those modules are never downloaded because nothing builds them,
// so their license files are not on disk to copy. Measured on this module: 158
// modules in the graph, 78 of them present in the module cache, 60 linked. The
// linked set is exactly what the binary contains, and every one of its modules
// is on disk because the build needs its source.
//
// The targets are passed in rather than listed here, so the hook in
// .goreleaser.yaml names them beside the build matrix they have to match: a
// package imported on one platform only still ships in that platform's archive.
//
// # Every directory between a linked package and its root, not the root alone
//
// A module's root license is not always the whole story. A module that carries
// code from elsewhere keeps that code's license beside it: the NATS server's
// internal/fastrand is the LevelDB-Go authors' BSD code under its own LICENSE,
// which the server's root Apache license does not reproduce, and a BSD license
// obliges a binary redistribution to carry that copyright notice. So the
// notices of a component are the files in its root AND in every directory on
// the path from each package the binary links up to that root. A directory the
// binary links nothing from contributes nothing, so a module's tests, examples
// and unused subpackages add no text for code that is not in the archive.
//
// The standard library is walked the same way, from the linked packages up to
// GOROOT, so the toolchain follows the one rule too. A nested file whose text
// is byte-identical to one already reproduced for the same component (the Go
// license again under GOROOT/src/vendor/golang.org/x) is written once.
//
// # A component with no license file stops the release
//
// Its notice cannot be reproduced, and shipping without it is the defect this
// command exists to remove. The error names every such module so the decision
// (a replace, a vendored notice, a different dependency) is made by a person.
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

func main() {
	out := flag.String("out", "", "file to write the notices to (required)")
	pkg := flag.String("main", "", "the main package the binary is built from, for example ./cmd/crewlet (required)")
	targets := flag.String("targets", "", "comma-separated GOOS/GOARCH release targets, for example linux/amd64,darwin/arm64 (required)")
	flag.Parse()

	if err := run(context.Background(), *out, *pkg, *targets); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, out, pkg, targetList string) error {
	if out == "" || pkg == "" || targetList == "" {
		return errors.New("-out, -main and -targets are all required; see .goreleaser.yaml for how the release calls this")
	}
	targets, err := parseTargets(targetList)
	if err != nil {
		return err
	}

	toolchain, err := goToolchain(ctx)
	if err != nil {
		return err
	}
	lists := make([][]component, 0, len(targets))
	for _, t := range targets {
		listed, std, listErr := goList(ctx, pkg, t)
		if listErr != nil {
			return listErr
		}
		lists = append(lists, listed)
		toolchain.Packages = append(toolchain.Packages, std...)
	}
	toolchain.Packages = sortedUnique(toolchain.Packages)

	var buf bytes.Buffer
	if err = render(&buf, toolchain, merge(lists...)); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("creating the directory for %s: %w", out, err)
	}
	// World-readable on purpose: a notices file is published text.
	if err = os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	return nil
}

// target is one GOOS/GOARCH pair.
type target struct{ goos, goarch string }

func parseTargets(list string) ([]target, error) {
	var out []target
	for _, raw := range strings.Split(list, ",") {
		goos, goarch, ok := strings.Cut(strings.TrimSpace(raw), "/")
		if !ok || goos == "" || goarch == "" {
			return nil, fmt.Errorf("-targets: %q is not GOOS/GOARCH (for example linux/amd64)", raw)
		}
		out = append(out, target{goos, goarch})
	}
	return out, nil
}

// component is one body of third-party code the binary links: a module, or the
// Go toolchain's standard library and runtime.
type component struct {
	Path    string
	Version string
	// Dir is the component's root on disk: the module directory, or GOROOT.
	Dir string
	// Packages is the directory of every package the binary links from the
	// component, each inside Dir, sorted and unique.
	Packages []string
}

// listedPackage is the part of `go list -json` output this reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *listedModule
}

type listedModule struct {
	Path    string
	Version string
	Dir     string
	Main    bool
	Replace *listedModule
}

// goList runs `go list -deps -json` for one target.
//
// CGO_ENABLED=0 because that is how every release target is built, and cgo
// changes which files, and so which imports, a package has.
func goList(ctx context.Context, pkg string, t target) ([]component, []string, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-json", pkg)
	cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("go list -deps %s for %s/%s: %w: %s", pkg, t.goos, t.goarch, err, stderr.String())
	}
	listed, std, err := parseList(bytes.NewReader(stdout))
	if err != nil {
		return nil, nil, fmt.Errorf("go list -deps %s for %s/%s: %w", pkg, t.goos, t.goarch, err)
	}
	return listed, std, nil
}

// parseList reads a `go list -json` stream and returns the modules its
// packages belong to (one entry per package, which merge folds together) and
// the directories of the standard library packages, excluding the main module.
//
// A replaced module is reported under its original path, since that is the
// name the code imports, with the version and directory of the replacement,
// since that is the source that was linked.
func parseList(r io.Reader) ([]component, []string, error) {
	dec := json.NewDecoder(r)
	var (
		out []component
		std []string
	)
	for {
		var p listedPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return out, std, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("decoding the package list: %w", err)
		}
		if p.Standard {
			if p.Dir == "" {
				return nil, nil, fmt.Errorf("standard package %s has no source directory; the toolchain's "+
					"GOROOT/src is incomplete, so reinstall the toolchain and retry", p.ImportPath)
			}
			std = append(std, p.Dir)
			continue
		}
		if p.Module == nil || p.Module.Main {
			continue
		}
		m := component{Path: p.Module.Path, Version: p.Module.Version, Dir: p.Module.Dir}
		if rep := p.Module.Replace; rep != nil {
			m.Version, m.Dir = rep.Version, rep.Dir
			if rep.Version == "" {
				m.Version = "(replaced by " + rep.Path + ")"
			}
		}
		if m.Dir == "" || p.Dir == "" {
			return nil, nil, fmt.Errorf("package %s: module %s %s has no source directory; run `go mod download` and retry",
				p.ImportPath, m.Path, m.Version)
		}
		m.Packages = []string{p.Dir}
		out = append(out, m)
	}
}

// merge unions the per-target lists, sorted by path, one entry per module
// carrying every package directory any target links from it.
func merge(lists ...[]component) []component {
	seen := map[string]component{}
	for _, list := range lists {
		for _, m := range list {
			key := m.Path + "@" + m.Version
			if prior, ok := seen[key]; ok {
				m.Packages = append(prior.Packages, m.Packages...)
			}
			seen[key] = m
		}
	}
	out := make([]component, 0, len(seen))
	for _, m := range seen {
		m.Packages = sortedUnique(m.Packages)
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b component) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Version, b.Version))
	})
	return out
}

func sortedUnique(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

// goToolchain locates the toolchain the build runs on, whose runtime and
// standard library are linked into every binary it produces.
func goToolchain(ctx context.Context) (component, error) {
	raw, err := exec.CommandContext(ctx, "go", "env", "-json", "GOROOT", "GOVERSION").Output()
	if err != nil {
		return component{}, fmt.Errorf("go env GOROOT GOVERSION: %w", err)
	}
	var env struct{ GOROOT, GOVERSION string }
	if err := json.Unmarshal(raw, &env); err != nil {
		return component{}, fmt.Errorf("decoding go env: %w", err)
	}
	return component{Path: "Go toolchain (standard library and runtime)", Version: env.GOVERSION, Dir: env.GOROOT}, nil
}

// noticeName matches the files a component's own notices live in: its
// license, a NOTICE (which Apache-2.0 requires to be passed on), and a patent
// grant, bare or with a suffix such as LICENSE.txt, LICENSE-MIT or
// COPYING.LESSER.
var noticeName = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([-._][^/]*)?$`)

// sourceExtensions are the files the go command compiles or links into a
// package. The pattern above accepts any suffix, and the walk reads package
// directories, so a package that happens to hold a license.go or a
// notice_linux.s would otherwise have its code pasted into the notices.
var sourceExtensions = map[string]bool{
	".go": true, ".s": true, ".S": true, ".sx": true, ".c": true, ".cc": true,
	".cpp": true, ".cxx": true, ".h": true, ".hh": true, ".hpp": true, ".hxx": true,
	".m": true, ".f": true, ".F": true, ".for": true, ".f90": true,
	".swig": true, ".swigcxx": true, ".syso": true,
}

func isNotice(name string) bool {
	return noticeName.MatchString(name) && !sourceExtensions[filepath.Ext(name)]
}

// notice is one notice file of a component, named relative to its root.
type notice struct {
	name string
	text string
}

// noticesOf collects a component's notice files: its root first, then every
// directory on the way from each linked package up to the root, in path order.
// A file whose text was already collected for this component is skipped.
func noticesOf(c component) ([]notice, error) {
	var nested []string
	for _, pkg := range c.Packages {
		rel, err := filepath.Rel(c.Dir, pkg)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("%s %s: the linked package directory %s is not inside its root %s; "+
				"the go list output is inconsistent, so rerun the release from a clean module cache",
				c.Path, c.Version, pkg, c.Dir)
		}
		for ; rel != "."; rel = filepath.Dir(rel) {
			nested = append(nested, rel)
		}
	}
	// The root first, because it is the license a reader looks for, then the
	// nested directories in path order so the output is byte-stable.
	ordered := append([]string{"."}, sortedUnique(nested)...)

	var out []notice
	seen := map[string]bool{}
	for _, dir := range ordered {
		entries, err := os.ReadDir(filepath.Join(c.Dir, dir))
		if err != nil {
			return nil, fmt.Errorf("reading %s %s at %s: %w", c.Path, c.Version, filepath.Join(c.Dir, dir), err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !isNotice(e.Name()) {
				continue
			}
			name := filepath.ToSlash(filepath.Join(dir, e.Name()))
			raw, err := os.ReadFile(filepath.Join(c.Dir, dir, e.Name()))
			if err != nil {
				return nil, fmt.Errorf("reading %s of %s %s: %w", name, c.Path, c.Version, err)
			}
			// Trailing whitespace only decides how the sections are spaced,
			// so it neither counts toward a duplicate nor reaches the file.
			text := strings.TrimRight(string(raw), "\n\r\t ")
			if seen[text] {
				continue
			}
			seen[text] = true
			out = append(out, notice{name: name, text: text})
		}
	}
	return out, nil
}

const rule = "================================================================================"

// render writes the notices document. The output depends only on its inputs,
// so two runs over one checkout produce identical bytes.
func render(w io.Writer, toolchain component, modules []component) error {
	var missing []string
	fmt.Fprintf(w, "Third-party notices for the crewlet binary\n\n"+
		"The crewlet binary is built with the Go toolchain and links the Go modules\n"+
		"listed below. Each section reproduces that component's own license and\n"+
		"notice files, unmodified, including those kept beside the packages the\n"+
		"binary links from it. Crewlet itself is licensed under the MIT License\n"+
		"in LICENSE. The dashboard the binary embeds lists its own bundled\n"+
		"dependencies and fonts in dashboard/THIRD_PARTY_NOTICES.txt beside this\n"+
		"file, and a running engine serves the same file at\n"+
		"/static/dashboard/THIRD_PARTY_NOTICES.txt.\n")
	for _, c := range append([]component{toolchain}, modules...) {
		notices, err := noticesOf(c)
		if err != nil {
			return err
		}
		if len(notices) == 0 {
			missing = append(missing, c.Path+" "+c.Version)
			continue
		}
		fmt.Fprintf(w, "\n%s\n%s %s\n%s\n", rule, c.Path, c.Version, rule)
		for _, n := range notices {
			fmt.Fprintf(w, "\n--- %s ---\n\n%s\n", n.name, n.text)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("no license or notice file in %s; the release cannot "+
			"reproduce their notices, so decide how each is attributed before releasing",
			strings.Join(missing, ", "))
	}
	return nil
}

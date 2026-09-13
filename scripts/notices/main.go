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
// # A module with no license file stops the release
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

	lists := make([][]module, 0, len(targets))
	for _, t := range targets {
		listed, listErr := goList(ctx, pkg, t)
		if listErr != nil {
			return listErr
		}
		lists = append(lists, listed)
	}
	toolchain, err := goToolchain(ctx)
	if err != nil {
		return err
	}

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

// module is one linked module and where its source is on disk.
type module struct {
	Path    string
	Version string
	Dir     string
}

// listedPackage is the part of `go list -json` output this reads.
type listedPackage struct {
	ImportPath string
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
func goList(ctx context.Context, pkg string, t target) ([]module, error) {
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-json", pkg)
	cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps %s for %s/%s: %w: %s", pkg, t.goos, t.goarch, err, stderr.String())
	}
	listed, err := parseList(bytes.NewReader(stdout))
	if err != nil {
		return nil, fmt.Errorf("go list -deps %s for %s/%s: %w", pkg, t.goos, t.goarch, err)
	}
	return listed, nil
}

// parseList reads a `go list -json` stream and returns the modules its
// packages belong to, excluding the main module and the standard library.
//
// A replaced module is reported under its original path, since that is the
// name the code imports, with the version and directory of the replacement,
// since that is the source that was linked.
func parseList(r io.Reader) ([]module, error) {
	dec := json.NewDecoder(r)
	var out []module
	for {
		var p listedPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decoding the package list: %w", err)
		}
		if p.Module == nil || p.Module.Main {
			continue
		}
		m := module{Path: p.Module.Path, Version: p.Module.Version, Dir: p.Module.Dir}
		if rep := p.Module.Replace; rep != nil {
			m.Version, m.Dir = rep.Version, rep.Dir
			if rep.Version == "" {
				m.Version = "(replaced by " + rep.Path + ")"
			}
		}
		if m.Dir == "" {
			return nil, fmt.Errorf("package %s: module %s %s has no source directory; run `go mod download` and retry",
				p.ImportPath, m.Path, m.Version)
		}
		out = append(out, m)
	}
}

// merge unions the per-target lists, sorted by path, one entry per module.
func merge(lists ...[]module) []module {
	seen := map[string]module{}
	for _, list := range lists {
		for _, m := range list {
			seen[m.Path+"@"+m.Version] = m
		}
	}
	out := make([]module, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b module) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Version, b.Version))
	})
	return out
}

// goToolchain locates the toolchain the build runs on, whose runtime and
// standard library are linked into every binary it produces.
func goToolchain(ctx context.Context) (module, error) {
	raw, err := exec.CommandContext(ctx, "go", "env", "-json", "GOROOT", "GOVERSION").Output()
	if err != nil {
		return module{}, fmt.Errorf("go env GOROOT GOVERSION: %w", err)
	}
	var env struct{ GOROOT, GOVERSION string }
	if err := json.Unmarshal(raw, &env); err != nil {
		return module{}, fmt.Errorf("decoding go env: %w", err)
	}
	return module{Path: "Go toolchain (standard library and runtime)", Version: env.GOVERSION, Dir: env.GOROOT}, nil
}

// noticeFile matches the files a module's own notices live in: its license,
// a NOTICE (which Apache-2.0 requires to be passed on), and a patent grant.
var noticeFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([-._][^/]*)?$`)

// noticesIn returns the notice files at the root of a module, sorted by name.
func noticesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && noticeFile.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

const rule = "================================================================================"

// render writes the notices document. The output depends only on its inputs,
// so two runs over one checkout produce identical bytes.
func render(w io.Writer, toolchain module, modules []module) error {
	var missing []string
	fmt.Fprintf(w, "Third-party notices for the crewlet binary\n\n"+
		"The crewlet binary is built with the Go toolchain and links the Go modules\n"+
		"listed below. Each section reproduces that component's own license and\n"+
		"notice files, unmodified. Crewlet itself is licensed under the MIT License\n"+
		"in LICENSE. The dashboard the binary embeds lists its own bundled\n"+
		"dependencies and fonts in dashboard/THIRD_PARTY_NOTICES.txt beside this\n"+
		"file, and a running engine serves the same file at\n"+
		"/static/dashboard/THIRD_PARTY_NOTICES.txt.\n")
	for _, m := range append([]module{toolchain}, modules...) {
		files, err := noticesIn(m.Dir)
		if err != nil {
			return fmt.Errorf("reading %s %s at %s: %w", m.Path, m.Version, m.Dir, err)
		}
		if len(files) == 0 {
			missing = append(missing, m.Path+" "+m.Version)
			continue
		}
		fmt.Fprintf(w, "\n%s\n%s %s\n%s\n", rule, m.Path, m.Version, rule)
		for _, name := range files {
			if err = writeFile(w, m, name); err != nil {
				return err
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("no license or notice file at the root of %s; the release cannot "+
			"reproduce their notices, so decide how each is attributed before releasing",
			strings.Join(missing, ", "))
	}
	return nil
}

// writeFile copies one notice file into the document, verbatim apart from
// trailing whitespace, which only decides how the sections are spaced.
func writeFile(w io.Writer, m module, name string) error {
	text, err := os.ReadFile(filepath.Join(m.Dir, name))
	if err != nil {
		return fmt.Errorf("reading %s of %s %s: %w", name, m.Path, m.Version, err)
	}
	fmt.Fprintf(w, "\n--- %s ---\n\n%s\n", name, strings.TrimRight(string(text), "\n\r\t "))
	return nil
}

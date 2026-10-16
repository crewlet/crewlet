package assistant_test

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

const module = "github.com/crewlet/crewlet"

// neverImported are the package trees no file under internal/assistant may
// import itself, each root and every package below it: the database, every
// domain's state and writer but the assistant's own, and the fleet's sealed
// credentials. DIRECT imports only, because the assistant's own domain
// (internal/assistantdomain) is the one sanctioned way to its conversations
// and imports the database itself.
var neverImported = []string{
	"internal/store",
	"internal/statelog",
	"internal/fleetsecrets",
}

// importedAnyway are the packages under a [neverImported] root the assistant
// may import, each with why it is no way into the database. Every one is held
// to reaching nothing in the module at all, so the reason stays true.
var importedAnyway = map[string]string{
	"internal/statelog/metrics": "the engine's instrument catalogue and its one " +
		"recorder: names, units and an atomic add, which open no database, read " +
		"no record and write nothing",
}

// within reports whether pkg is root or a package below it.
func within(pkg, root string) bool {
	return pkg == root || strings.HasPrefix(pkg, root+"/")
}

// underAny is the first of roots pkg is within, or "".
func underAny(pkg string, roots []string) string {
	for _, root := range roots {
		if within(pkg, root) {
			return root
		}
	}
	return ""
}

// allFiles reads a package's imports from every file, whatever GOOS or tag it
// is built for, so a file built for another platform is walked too.
var allFiles = func() build.Context {
	c := build.Default
	c.UseAllFiles = true
	return c
}()

// neverReached are the packages no package under internal/assistant may reach
// at any depth: each is a way into the company that skips the guard, the
// authority table or the person.
//
//   - the operator surface, the question registry, the tool registry and the
//     builtins are the other transports into the tools, which a direct call
//     would be a fourth of;
//   - the MCP bridge acts as a SEAT, which holds every app;
//   - the agent turn context falls back to a seat's principal;
//   - the credential owner holds the grant's token, which the assistant must
//     never be able to read;
//   - every other domain's writer and reader, and the engine that wires them.
var neverReached = []string{
	"internal/api/operator",
	"internal/api/queries",
	"internal/api/mcpbridge",
	"internal/api/auth",
	"internal/api/workapi",
	"internal/api/iamapi",
	"internal/api/authapi",
	"internal/api/secretsapi",
	"internal/api/setupapi",
	"internal/tools",
	"internal/agent/builtin",
	"internal/agent/turnctx",
	"internal/iamdomain",
	"internal/worklet",
	"internal/doclet",
	"internal/artifactsdomain",
	"internal/artifactdata",
	"internal/engine",
}

// reachedAnyway are the packages under a [neverReached] root the assistant
// may reach, each with why it is no way into the company. The walk goes on
// through each, so what one of them reaches is held to the same rule and the
// reason stays true.
//
// The tool session the assistant's model calls through decodes a call's
// arguments as every transport into a tool does, through the one decode that
// lives beside the tool registry rather than in it, so the assistant reaches
// the argument leaves below internal/tools and never the registry.
var reachedAnyway = map[string]string{
	"internal/tools/argval": "the one reader per declared argument type " +
		"(ADR-0059): a value in and a value out, with no registry, no tool and " +
		"no principal",
	"internal/tools/toolargs": "a call's arguments as values: the one decode " +
		"every transport into a tool shares and their canonical form " +
		"(ADR-0059), with no registry, no tool and no principal",
}

// THE ASSISTANT IMPORTS NO WRITER AND NO SEAT (ADR-0050).
//
// Every read and write the assistant makes is the person's own request,
// through the one handler chain, decided by their own grants. The guarantee
// is structural only while no package on the assistant's side can reach
// product data, a seat's authority or the grant's token any other way, so this
// walks every package under internal/assistant and everything it imports, to
// any depth, and fails on the first path into one, a package below a banned
// root included. Mutation: import internal/api/auth (or anything that does)
// from any assistant package, internal/store/storetest from one, or
// internal/worklet from a package [reachedAnyway] names, and this names the
// path.
func TestTheAssistantImportsNoWriterAndNoSeat(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	var packages []string
	err := sourcetree.Walk(filepath.Join(root, "internal", "assistant"),
		func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || d.Name() == "testdata" {
				return err
			}
			if pkg, err := allFiles.ImportDir(path, 0); err == nil && len(pkg.GoFiles) > 0 {
				rel, _ := filepath.Rel(root, path)
				packages = append(packages, filepath.ToSlash(rel))
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if dir := filepath.Join(root, "internal", "assistant"); hasGo(dir) {
		packages = append(packages, "internal/assistant")
	}
	if len(packages) < 7 {
		t.Fatalf("the walk found %d packages under internal/assistant, so it is not "+
			"reading the tree it guards", len(packages))
	}

	imports := importer{root: root, seen: map[string][]string{}}
	for allowed := range importedAnyway {
		if direct := imports.of(t, allowed); len(direct) > 0 {
			t.Errorf("%s is imported by the assistant because it reaches nothing in "+
				"the module, and it now imports %v", allowed, direct)
		}
	}
	for allowed := range reachedAnyway {
		if underAny(allowed, neverReached) == "" {
			t.Errorf("%s may be reached as no way into the company, but it is under "+
				"no root the walk bans, so the exemption exempts nothing", allowed)
		}
	}
	for _, pkg := range packages {
		for _, imported := range imports.of(t, pkg) {
			if _, allowed := importedAnyway[imported]; allowed {
				continue
			}
			if root := underAny(imported, neverImported); root != "" {
				t.Errorf("%s imports %s, under %s: the assistant reaches its own "+
					"conversations through internal/assistantdomain and nothing else",
					pkg, imported, root)
			}
		}
		if path := imports.reaches(t, pkg, neverReached, nil); path != nil {
			t.Errorf("%s reaches %s through %s: that is a way into the company "+
				"that skips the person", pkg, path[len(path)-1], strings.Join(path, " -> "))
		}
	}
}

// hasGo reports whether dir holds a non-test Go file of its own.
func hasGo(dir string) bool {
	pkg, err := allFiles.ImportDir(dir, 0)
	return err == nil && len(pkg.GoFiles) > 0
}

// importer reads the module's packages' imports, once each.
type importer struct {
	root string
	seen map[string][]string
}

// of is pkg's non-test imports inside this module, relative to its root.
func (im importer) of(t *testing.T, pkg string) []string {
	t.Helper()
	if got, ok := im.seen[pkg]; ok {
		return got
	}
	p, err := allFiles.ImportDir(filepath.Join(im.root, filepath.FromSlash(pkg)), 0)
	if err != nil {
		t.Fatalf("read %s: %v", pkg, err)
	}
	var out []string
	for _, path := range p.Imports {
		if rel, ok := strings.CutPrefix(path, module+"/"); ok {
			out = append(out, rel)
		}
	}
	im.seen[pkg] = out
	return out
}

// reaches is the import path from pkg to the first package within one of
// targets' trees it reaches, at any depth inside the module, or nil. A package
// [reachedAnyway] names is walked through rather than reported.
func (im importer) reaches(t *testing.T, pkg string, targets []string, path []string) []string {
	t.Helper()
	path = append(slices.Clone(path), pkg)
	if _, allowed := reachedAnyway[pkg]; !allowed && underAny(pkg, targets) != "" &&
		len(path) > 1 {
		return path
	}
	if slices.Contains(path[:len(path)-1], pkg) {
		return nil
	}
	for _, next := range im.of(t, pkg) {
		if found := im.reaches(t, next, targets, path); found != nil {
			return found
		}
	}
	return nil
}

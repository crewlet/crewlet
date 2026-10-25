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

// neverImported are the packages no file under internal/assistant may import
// itself: the database, every domain's state and writer but the assistant's
// own, and every surface that decides or reaches product data some other way
// than a request through the handler chain.
var neverImported = []string{
	"internal/store",
	"internal/statelog",
	"internal/fleetsecrets",
}

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
	"internal/tracker",
	"internal/pages",
	"internal/artifactsdomain",
	"internal/artifactdata",
	"internal/engine",
}

// THE ASSISTANT IMPORTS NO WRITER AND NO SEAT (ADR-0049).
//
// Every read and write the assistant makes is the person's own request,
// through the one handler chain, decided by their own grants. The guarantee
// is structural only while no package on the assistant's side can reach
// product data, a seat's authority or the grant's token any other way, so this
// walks every package under internal/assistant and everything it imports, to
// any depth, and fails on the first path into one. Mutation: import
// internal/api/auth (or anything that does) from any assistant package and
// this names the path.
func TestTheAssistantImportsNoWriterAndNoSeat(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	var packages []string
	err := sourcetree.Walk(filepath.Join(root, "internal", "assistant"),
		func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || d.Name() == "testdata" {
				return err
			}
			if pkg, err := build.ImportDir(path, 0); err == nil && len(pkg.GoFiles) > 0 {
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
	for _, pkg := range packages {
		direct := imports.of(t, pkg)
		for _, banned := range neverImported {
			if slices.Contains(direct, banned) {
				t.Errorf("%s imports %s: the assistant reaches its own conversations "+
					"through internal/assistantdomain and nothing else", pkg, banned)
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
	pkg, err := build.ImportDir(dir, 0)
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
	p, err := build.ImportDir(filepath.Join(im.root, filepath.FromSlash(pkg)), 0)
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

// reaches is the import path from pkg to the first of targets it reaches, or
// nil.
func (im importer) reaches(t *testing.T, pkg string, targets []string, path []string) []string {
	t.Helper()
	path = append(slices.Clone(path), pkg)
	if slices.Contains(targets, pkg) && len(path) > 1 {
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

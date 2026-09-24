package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// EVERY REGISTERED EVENT TYPE HAS A PUBLISHER.
//
// A type registered and never published is a decode path, a summary renderer,
// a category row and a documented wire name for a fact nothing produces — and
// it is INVISIBLE, because a consumer waiting for it is indistinguishable from
// one whose event has not happened yet. Sixteen of the sixty-one were in that
// state, and three of them had live consumers that therefore rendered nothing:
// the dashboard's token header, its seat `terminated` state, and the audit
// log's line for a node coming up.
//
// # Why it reads the SOURCE
//
// Because calling every publisher needs a broker, a store, a company and a
// fleet — which is exactly why this went unnoticed. What a publisher looks
// like is a composite literal of the payload type outside its own package, so
// that is what this walks for.
func TestEveryRegisteredEventTypeIsPublishedSomewhere(t *testing.T) {
	t.Parallel()
	producers := payloadLiterals(t)
	for _, typ := range events.RegisteredTypes() {
		payload, ok := events.PayloadFor(typ)
		if !ok {
			t.Errorf("%q is registered under no Go type", typ)
			continue
		}
		of := reflect.TypeOf(payload).Elem()
		// ONLY THE SHIPPED VOCABULARY. A test in this module may
		// register a payload of its own to exercise the registry, and
		// that one has no publisher by construction.
		if !strings.HasSuffix(of.PkgPath(), "/internal/events/types") {
			continue
		}
		if name := of.Name(); !producers[name] {
			t.Errorf("%s (%q) is registered and nothing constructs it: a "+
				"consumer waiting for it cannot tell that from an event that "+
				"has not happened yet. Publish it, or retire the type",
				name, typ)
		}
	}
}

// EVERY DECLARED GUARD KIND HAS A PRODUCER.
//
// The same gap as the one above, one level down: a registered event type can
// carry a value nothing ever writes into it. `unhandled_exception` was
// declared, documented and given an AFK sentence by the dashboard, and no code
// path set it, because nothing recovered a panic at all. The registry test
// cannot see that, since the event type itself had producers for its other
// kinds.
//
// A producer is a breach built with the kind, `Kind: types.GuardX`, in a
// non-test file: comparing against a kind is reading it, and only a writer
// makes the value reachable.
func TestEveryDeclaredGuardKindIsProducedSomewhere(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	declared := guardKinds(t, filepath.Join(root, "internal", "events", "types"))
	written := sourceMatches(t, root, regexp.MustCompile(`\bKind:\s*types\.(Guard[A-Za-z0-9_]+)\b`))
	for _, name := range declared {
		if !written[name] {
			t.Errorf("types.%s is a declared guard kind and no breach is built "+
				"with it: a dashboard sentence for it describes a state no seat "+
				"can reach. Produce it, or retire the kind", name)
		}
	}
}

// EVERY WAY A PAGE CAN REACH A SEAT HAS A PRODUCER.
//
// The guard-kind gap once more, for `knowledge_read`: the type has several
// producers, so the registry test above passes as long as ONE of them runs,
// and a `via` nothing writes is a column value every "read by" screen offers
// as a filter and no row can ever carry. A producer is a read built with the
// value, `Via: types.ReadViaX` or a call handing `types.ReadViaX` to the
// helper that builds one, in a non-test file.
func TestEveryKnowledgeReadViaIsProducedSomewhere(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	declared := constsOf(t, filepath.Join(root, "internal", "events", "types"), "KnowledgeReadVia")
	written := sourceMatches(t, root, regexp.MustCompile(
		`(?:\bVia:\s*|knowledgeRead\(turn,\s*)types\.(ReadVia[A-Za-z0-9_]+)\b`))
	for _, name := range declared {
		if !written[name] {
			t.Errorf("types.%s is a declared way a page reaches a seat and no read "+
				"is built with it: a filter on it can never match a row. Produce "+
				"it, or retire the value", name)
		}
	}
}

// guardKinds is every constant the payload package declares as a GuardKind,
// read from its source so a kind added there is covered without being listed
// here.
func guardKinds(t *testing.T, dir string) []string {
	t.Helper()
	return constsOf(t, dir, "GuardKind")
}

// constsOf is every constant the payload package declares with the named
// type, read from its source so a value added there is covered without being
// listed here.
func constsOf(t *testing.T, dir, typeName string) []string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var kinds []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if typ, ok := value.Type.(*ast.Ident); ok && typ.Name == typeName {
					for _, ident := range value.Names {
						kinds = append(kinds, ident.Name)
					}
				}
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatalf("no %s constant found, so this test could not fail", typeName)
	}
	return kinds
}

// sourceMatches is every first submatch of pattern in the module's non-test Go
// files outside the payloads' own package.
func sourceMatches(t *testing.T, root string, pattern *regexp.Regexp) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			// The payloads' OWN package is skipped: a literal there is
			// a test fixture or a summary's receiver, not a publisher.
			if d.Name() == "testdata" || d.Name() == "types" ||
				strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range pattern.FindAllStringSubmatch(string(body), -1) {
			found[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	return found
}

// payloadLiterals is every `types.X{` a non-test file in this module writes.
//
// A composite literal outside the payload's own package is what a PUBLISHER
// looks like: the payload types have no constructors, so an event is built by
// naming the struct. Scanning the source rather than calling anything is the
// point: the reason this gap survived is that reaching the publishers needs a
// broker, a store, a company and a fleet.
func payloadLiterals(t *testing.T) map[string]bool {
	t.Helper()
	found := sourceMatches(t, moduleRoot(t),
		regexp.MustCompile(`\btypes\.([A-Z][A-Za-z0-9_]*)\{`))
	if len(found) == 0 {
		t.Fatal("no payload literal found anywhere, so this test could not fail")
	}
	return found
}

// moduleRoot is the directory holding go.mod, walking up from this package.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

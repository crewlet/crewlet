package engine

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/regexlit"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY REGISTERED EVENT TYPE HAS A PUBLISHER: a type nothing publishes is
// INVISIBLE, since a consumer waiting for it cannot tell it from an event that
// has not happened yet.
//
// It reads the SOURCE for a composite literal of the payload type outside its
// own package (payloadLiterals), because calling every publisher needs a
// broker, a store, a company and a fleet.
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
		// Only the shipped vocabulary: a test's own registered payload has
		// no publisher by construction.
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

// EVERY DECLARED GUARD KIND HAS A PRODUCER: the registry gate above, one level
// down, since a published type can still carry a kind nothing writes.
//
// A producer is a breach built with the kind, `Kind: types.GuardX`, in a
// non-test file: a comparison only reads it.
func TestEveryDeclaredGuardKindIsProducedSomewhere(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
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

// EVERY WAY A PAGE CAN REACH A SEAT HAS A PRODUCER: a `knowledge_read` `via`
// nothing writes is a "read by" filter no row can match. A producer is
// `Via: types.ReadViaX`, or `types.ReadViaX` handed to knowledgeRead, in a
// non-test file.
func TestEveryKnowledgeReadViaIsProducedSomewhere(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
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

// guardKinds is every constant the payload package declares as a GuardKind.
func guardKinds(t *testing.T, dir string) []string {
	t.Helper()
	return constsOf(t, dir, "GuardKind")
}

// constsOf is every constant the payload package declares with the named
// type, read from its source so a new value is covered without a list here.
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

// THE WALK'S START IS NEVER ONE OF ITS OWN SKIPS: a checkout at `~/.crewlet`
// must not be skipped whole as a dot-directory (publishes), checked through
// the read, the prefilter and the line split the gates above use.
func TestAModuleUnderADotNamedDirectoryIsStillRead(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".crewlet")
	for path, body := range map[string]string{
		"internal/pub/pub.go":        "package pub\nvar _ = types.Published{}\n",
		"internal/.hidden/hidden.go": "package hidden\nvar _ = types.Hidden{}\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := sourceMatches(t, root, regexp.MustCompile(`\btypes\.([A-Z][A-Za-z0-9_]*)\{`))
	if !found["Published"] || found["Hidden"] {
		t.Errorf("found %v, want the module's own literal and not the dot-directory's", found)
	}
}

// sourceMatches is every first submatch of pattern in the module's non-test Go
// files outside the payloads' own package.
//
// The patterns have no literal prefix for the regexp package to jump to, so a
// whole-module scan under -race took seconds a gate; a file reaches one only
// if it holds what a match needs (regexlit.Required),
// and only its matching lines where no match crosses a line
// (regexlit.LineLocal); both are exact, so the result equals a full scan.
func sourceMatches(t *testing.T, root string, pattern *regexp.Regexp) map[string]bool {
	t.Helper()
	need, perLine := regexlit.Required(pattern), regexlit.LineLocal(pattern)
	found := map[string]bool{}
	for _, f := range moduleFiles(t, root) {
		if !publishes(f.rel) || !need.Admits(f.body) {
			continue
		}
		texts := [][]byte{f.body}
		if perLine {
			texts = bytes.Split(f.body, []byte("\n"))
		}
		for _, text := range texts {
			if !need.Admits(text) {
				continue
			}
			for _, m := range pattern.FindAllSubmatch(text, -1) {
				found[string(m[1])] = true
			}
		}
	}
	return found
}

// publishes reports whether a file below the module root is one a publisher
// can live in: not the payloads' OWN package (a literal there is a fixture or
// a summary's receiver), testdata, or a dot-directory the go command does not
// build. Only directories BELOW the root are judged, since the root's name is
// wherever somebody cloned it.
func publishes(rel string) bool {
	dirs := strings.Split(rel, "/")
	for _, dir := range dirs[:len(dirs)-1] {
		if dir == "testdata" || dir == "types" || strings.HasPrefix(dir, ".") {
			return false
		}
	}
	return true
}

// payloadLiterals is every `types.X{` a non-test file in this module writes.
//
// A composite literal outside the payload's own package is what a PUBLISHER
// looks like: the payload types have no constructors.
func payloadLiterals(t *testing.T) map[string]bool {
	t.Helper()
	found := sourceMatches(t, sourcetree.Root(t),
		regexp.MustCompile(`\btypes\.([A-Z][A-Za-z0-9_]*)\{`))
	if len(found) == 0 {
		t.Fatal("no payload literal found anywhere, so this test could not fail")
	}
	return found
}

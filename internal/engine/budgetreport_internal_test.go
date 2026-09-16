package engine

import (
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

// payloadLiterals is every `types.X{` a non-test file in this module writes.
//
// A composite literal outside the payload's own package is what a PUBLISHER
// looks like: the payload types have no constructors, so an event is built by
// naming the struct. Scanning the source rather than calling anything is the
// point — the reason this gap survived is that reaching the publishers needs a
// broker, a store, a company and a fleet.
func payloadLiterals(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	found := map[string]bool{}
	literal := regexp.MustCompile(`\btypes\.([A-Z][A-Za-z0-9_]*)\{`)
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
		for _, m := range literal.FindAllStringSubmatch(string(body), -1) {
			found[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
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

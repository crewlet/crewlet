package mediatype

import (
	"errors"
	"mime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/words"
)

// EVERY ROW IS WELL FORMED: one row per extension, a default type, a known
// class, a served type the mime parser reads, and a text type that names its
// charset where the format has one. A malformed row is a header the serving
// origin sends to every browser.
func TestEveryRowIsWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, row := range Table() {
		if !strings.HasPrefix(row.Extension, ".") || strings.ToLower(row.Extension) != row.Extension {
			t.Errorf("extension %q is not a lower-case dotted extension", row.Extension)
		}
		if seen[row.Extension] {
			t.Errorf("extension %q has two rows", row.Extension)
		}
		seen[row.Extension] = true
		if len(row.Types) == 0 {
			t.Errorf("%s has no type", row.Extension)
		}
		for _, typ := range row.Types {
			if !typ.Class.Valid() {
				t.Errorf("%s: class %q is not one this table knows", row.Extension, typ.Class)
			}
			if _, _, err := mime.ParseMediaType(typ.Served); err != nil {
				t.Errorf("%s: served type %q does not parse: %v", row.Extension, typ.Served, err)
			}
			if typ.Scriptable && !typ.Text {
				t.Errorf("%s: a scriptable type is text by construction", row.Extension)
			}
		}
	}
	if len(seen) < 30 {
		t.Fatalf("the table has %d rows; the walk is not reading it", len(seen))
	}
}

// THE SERVED TYPE IS THE TABLE'S, NEVER THE CLIENT'S: a declaration chooses
// between an extension's own types and nothing else.
func TestADeclaredTypeOnlyChoosesBetweenTheRowsOwnTypes(t *testing.T) {
	t.Parallel()
	got, err := Resolve("clip.mp4", "audio/mp4")
	if err != nil || got.Served != "audio/mp4" || got.Class != ClassAudio {
		t.Fatalf("Resolve(clip.mp4, audio/mp4) = %+v, %v", got, err)
	}
	got, err = Resolve("clip.mp4", "")
	if err != nil || got.Served != "video/mp4" {
		t.Fatalf("Resolve(clip.mp4, \"\") = %+v, %v; want the default video/mp4", got, err)
	}
	got, err = Resolve("index.HTML", "TEXT/HTML")
	if err != nil || got.Served != "text/html; charset=utf-8" {
		t.Fatalf("a declaration in another case did not match: %+v, %v", got, err)
	}
	for _, c := range []struct{ path, declared string }{
		{"index.html", "application/javascript"}, // a page passed off as a script
		{"logo.png", "image/svg+xml"},            // a PNG passed off as a scriptable SVG
		{"run.exe", ""},                          // an extension the table has no row for
		{"Makefile", ""},                         // no extension at all
		{"archive.tar.gz", "application/gzip"},
	} {
		if _, err := Resolve(c.path, c.declared); !errors.Is(err, ErrRefused) {
			t.Errorf("Resolve(%q, %q) = %v, want ErrRefused", c.path, c.declared, err)
		}
	}
}

// A REFUSED FILE IS TOLD WHAT IT CAN BE, as a sentence says a set: every
// extension an artifact may publish, and every type a declared file of its
// extension is served as, each quoted and the last joined by "or". The first
// was a run of bare extensions separated by spaces, and the second a hand
// join. Mutation: join either set by hand again and this fails.
func TestARefusedFileIsToldWhatItCanBe(t *testing.T) {
	t.Parallel()
	_, err := Resolve("run.exe", "")
	if err == nil || !strings.HasSuffix(err.Error(), ". Use "+words.Or(extensions())) {
		t.Errorf("an unknown extension is refused as %v, want every extension offered", err)
	}
	_, err = Resolve("clip.mp4", "text/plain")
	if err == nil || !strings.HasSuffix(err.Error(), "served as "+words.Or([]string{"video/mp4", "audio/mp4"})) {
		t.Errorf("a wrong declaration is refused as %v, want both types offered", err)
	}
}

// THE SCRIPTABLE SET IS EXACTLY WHAT A BROWSER EXECUTES: HTML, SVG, XML and
// JavaScript. A type wrongly left out would be served to the dashboard origin
// by a consumer that trusted the flag.
func TestTheScriptableSetIsWhatABrowserExecutes(t *testing.T) {
	t.Parallel()
	want := map[string]bool{".html": true, ".htm": true, ".svg": true, ".xml": true, ".js": true, ".mjs": true}
	for _, row := range Table() {
		scriptable := false
		for _, typ := range row.Types {
			scriptable = scriptable || typ.Scriptable
		}
		if scriptable != want[row.Extension] {
			t.Errorf("%s scriptable = %v, want %v", row.Extension, scriptable, want[row.Extension])
		}
	}
}

// AN ALREADY-COMPRESSED FORMAT IS NOT GZIPPED AGAIN, and every text type is.
func TestOnlyTextTypesAreCompressed(t *testing.T) {
	t.Parallel()
	for _, row := range Table() {
		for _, typ := range row.Types {
			if typ.Compressible != typ.Text {
				t.Errorf("%s (%s): compressible = %v and text = %v; the two agree "+
					"for every type this table serves", row.Extension, typ.Served,
					typ.Compressible, typ.Text)
			}
		}
	}
	if typ, ok := For("bundle.wasm"); !ok || typ.Compressible {
		t.Errorf("WebAssembly = %+v, %v; it is served uncompressed", typ, ok)
	}
}

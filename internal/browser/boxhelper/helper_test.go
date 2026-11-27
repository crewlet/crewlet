package boxhelper

import (
	"bytes"
	"io/fs"
	"regexp"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
)

// THE HELPER'S VERSION MOVES WITH EVERY BYTE AND NAME IT SHIPS: Version is the
// version of Files, sixteen hex digits, and a byte changed or added, a module
// renamed, two modules swapped, or a module's last byte moved into the next
// one's body or name each move it, so a box never takes one helper for
// another, and the order hashed is the order written.
//
// Mutations, each red: hash the bodies without the names (a rename keeps it);
// sort the modules by name before hashing (a swap keeps it); drop the length (a
// byte moved into the next module's name keeps it).
func TestTheHelperVersionMovesWithEveryByteAndNameItShips(t *testing.T) {
	t.Parallel()
	files := Files()
	v := Version()
	if v != version(files) || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(v) {
		t.Fatalf("Version() is %q, and the version of Files() is %q: want the one, sixteen hex digits", v, version(files))
	}
	if len(files) < 2 {
		t.Fatalf("the helper is %d modules, and this case moves bytes between two", len(files))
	}
	// changed is the version of a copy of files that change altered.
	changed := func(change func([]File)) string {
		c := make([]File, len(files))
		for i, f := range files {
			c[i] = File{Name: f.Name, Body: bytes.Clone(f.Body)}
		}
		change(c)
		return version(c)
	}
	// cut is c[i]'s last byte, which it no longer holds.
	cut := func(c []File, i int) byte {
		last := c[i].Body[len(c[i].Body)-1]
		c[i].Body = c[i].Body[:len(c[i].Body)-1]
		return last
	}
	for i := range files {
		j := (i + 1) % len(files)
		for what, change := range map[string]func([]File){
			"its first byte changed": func(c []File) { c[i].Body[0] ^= 1 },
			"its last byte changed":  func(c []File) { c[i].Body[len(c[i].Body)-1] ^= 1 },
			"a byte added":           func(c []File) { c[i].Body = append(c[i].Body, '\n') },
			"renamed":                func(c []File) { c[i].Name += "x" },
			"swapped with the next":  func(c []File) { c[i], c[j] = c[j], c[i] },
			"its last byte moved into the next body": func(c []File) {
				c[j].Body = append([]byte{cut(c, i)}, c[j].Body...)
			},
			"its last byte moved into the next name": func(c []File) {
				c[j].Name = string(cut(c, i)) + c[j].Name
			},
		} {
			if changed(change) == v {
				t.Errorf("%s with %s keeps the version %s", files[i].Name, what, v)
			}
		}
	}
}

// EVERY EMBEDDED MODULE IS WRITTEN AFTER WHAT IT IMPORTS: js/ embeds exactly
// the helper's own modules and its entry, so none is shipped unwritten or
// listed unshipped (live's are held so in its own case); Files writes the
// helper's, then live's, then the entry, no name twice, since they share one
// folder; and each relative import names a module written before it, so the
// order Files keeps is the one its doc states.
//
// Mutations, each red: list tree.mjs before common.mjs; tree.mjs importing a
// module listed after it (./chromium.mjs, say); embed a module the list
// leaves out; write live's modules after the entry; a live module named as
// one of the helper's.
func TestEveryEmbeddedModuleIsWrittenAfterWhatItImports(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(js, "js")
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, e := range entries {
		embedded = append(embedded, e.Name())
	}
	own := append(slices.Clone(modules), entry)
	if !slices.Equal(embedded, slices.Sorted(slices.Values(own))) {
		t.Fatalf("js/ embeds %q, and the helper's modules are %q: list every module, and embed only those", embedded,
			own)
	}
	var names []string
	for _, f := range Files() {
		names = append(names, f.Name)
	}
	if want := slices.Concat(modules, live.Modules(), []string{entry}); !slices.Equal(names, want) {
		t.Fatalf("Files writes %q, want the helper's modules, then live's, then the entry: %q", names, want)
	}
	if sorted := slices.Sorted(slices.Values(names)); len(slices.Compact(sorted)) != len(names) {
		t.Fatalf("Files writes %q, a name twice, and they share one folder", names)
	}
	imports := regexp.MustCompile(`(?:from|import)\s*\(?\s*"\./([^"]+)"`)
	var written []string
	for _, f := range Files() {
		for _, m := range imports.FindAllSubmatch(f.Body, -1) {
			if !slices.Contains(written, string(m[1])) {
				t.Errorf("%s imports ./%s, which is not written before it", f.Name, m[1])
			}
		}
		written = append(written, f.Name)
	}
}

package boxhelper

import (
	"bytes"
	"io/fs"
	"regexp"
	"slices"
	"testing"
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
// the modules Files lists, so none is shipped unwritten or listed unshipped,
// and each relative import names a module listed before it, so the order Files
// keeps is the one its doc states.
//
// Mutations, each red: list tree.mjs before common.mjs; tree.mjs importing a
// module listed after it (./chromium.mjs, say); embed a module the list
// leaves out.
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
	if !slices.Equal(embedded, slices.Sorted(slices.Values(modules))) {
		t.Fatalf("js/ embeds %q, and the helper's modules are %q: list every module, and embed only those", embedded,
			modules)
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

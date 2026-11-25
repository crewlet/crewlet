package live

import (
	"io/fs"
	"slices"
	"testing"
)

// EVERY MODULE IS LISTED AND EMBEDDED: js/ embeds exactly the modules
// Modules lists, so none ships unwritten or is listed unshipped; the order
// each is written in, after what it imports, is boxhelper's
// TestEveryEmbeddedModuleIsWrittenAfterWhatItImports's.
//
// Mutation: embed a module the list leaves out.
func TestEveryModuleIsListedAndEmbedded(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(js, "js")
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, e := range entries {
		embedded = append(embedded, e.Name())
	}
	if want := slices.Sorted(slices.Values(Modules())); !slices.Equal(embedded, want) {
		t.Fatalf("js/ embeds %q, and Modules lists %q: list every module, and embed only those", embedded, want)
	}
	for _, name := range Modules() {
		if len(Module(name)) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
}

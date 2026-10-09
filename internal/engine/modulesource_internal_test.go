package engine

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// moduleFile is one non-test Go file of the module, as the source gates in
// this package read it.
type moduleFile struct {
	// rel is the file's path below the root the read started at, with
	// forward slashes.
	rel string
	// path is the file's path as the walk found it.
	path string
	body []byte
}

// dir is the directory holding the file, as the walk found it.
func (f moduleFile) dir() string { return filepath.Dir(f.path) }

// under reports whether the file lies below one of the read's top-level
// directories.
func (f moduleFile) under(tops ...string) bool {
	for _, top := range tops {
		if strings.HasPrefix(f.rel, top+"/") {
			return true
		}
	}
	return false
}

// readModule is every non-test .go file below root, by sourcetree.Walk's rule
// and no other skip.
//
// ONE READ FOR EVERY GATE HERE, each narrowing it by the rule its own walk
// used to apply — the payload gates skip testdata, the payloads' own package
// and dot-directories anywhere below the root; the group gate reads internal/;
// the duty gate internal/ and cmd/. Five gates each walked and read the
// module for themselves, and then ran a pattern with no literal prefix, or a
// parser, over all of it; the read is shared, and each gate's prefilter
// (sourcetree.Required, sourcetree.Identifiers) decides what it examines.
func readModule(root string) ([]moduleFile, error) {
	var out []moduleFile
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir(), !strings.HasSuffix(path, ".go"), strings.HasSuffix(path, "_test.go"):
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, moduleFile{rel: filepath.ToSlash(rel), path: path, body: body})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the module's Go source under %s: %w", root, err)
	}
	if len(out) == 0 {
		// Every gate reading this asserts an absence over it, and a read
		// that found nothing asserts it just as confidently.
		return nil, fmt.Errorf("found no Go source under %s — a gate reading it "+
			"would certify nothing", root)
	}
	return out, nil
}

// moduleSources is readModule of this module's root, read once per test
// binary: the source is the build's, so no gate can see it change. The bytes
// are kept and not the syntax trees, which for eighteen megabytes of Go would
// hold several hundred megabytes for the rest of the binary — a gate parses
// the few files its prefilter admits.
var moduleSources = sync.OnceValues(func() ([]moduleFile, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return nil, err
	}
	return readModule(root)
})

// moduleFiles is the non-test Go source below root: the binary's one read of
// the module when root is the module's own, and a fresh read through the same
// function for any other — a tree a control plants — so the control certifies
// the read the gates rely on.
func moduleFiles(t *testing.T, root string) []moduleFile {
	t.Helper()
	module, err := sourcetree.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	read := func() ([]moduleFile, error) { return readModule(root) }
	if root == module {
		read = moduleSources
	}
	files, err := read()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

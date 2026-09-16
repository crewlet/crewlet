// Package clientsource finds a constant the dashboard declares, so a gate can
// hold it against the engine's own.
//
// # Why the engine reads the client at all
//
// Several lists exist twice by necessity. The dashboard is a separate build in
// a separate language that cannot import a Go identifier, so where it must
// know a set the engine owns — the event categories a filter chip offers, the
// addressable config collections a screen lists, the field names a history row
// is keyed on — it carries its own copy. Every one of those copies has drifted
// at least once, and each drift is SILENT in the direction that matters: a
// category the engine never assigns is a chip that filters a log down to
// nothing, a config kind spelled with the wrong separator is a refusal on
// every read, and a field name the history bag does not use is an attribution
// line that simply never appears.
//
// So the copy is checked. The engine side is where the check belongs, because
// the engine is the side that owns the value.
//
// # Keyed on the declaration, never on a path
//
// [Declaration] walks the tree and matches source, rather than reading a file
// somebody named. A gate that hard-codes where a constant lives breaks when a
// screen moves — and breaks LOUDLY but WRONGLY, reporting a drift between two
// lists neither of which changed. Keyed on the declaration, a move and a
// rename are both invisible, and the two failures it can report are the two
// that matter: nothing declares it, which is a gate certifying nothing, and
// TWO declarations exist, which is two copies that can drift from each other
// as well as from the engine — wherever the second one is, including beside
// the first in the same file.
//
// # One reader, for the same reason as everything else here
//
// This walk was written twice before it was written here, and a third copy was
// what made the case: three implementations of "find the one file that matches
// this pattern" is three chances for one to start skipping a directory the
// others read, and a gate that reads less than it thinks reports a pass it did
// not earn.
package clientsource

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Tree is the dashboard's SOURCE, relative to a package directory under
// `internal/`. Not the built bundle: that is minified and carries no
// declaration to find, and a gate pointed at it silently matched nothing for
// the whole of the React rewrite while reporting a pass.
//
// Callers one level deeper (`internal/api/...`) join another `..` themselves.
const Tree = "../../dashboard/src"

// Declaration returns the single capture of the one declaration under `tree`
// matching `pattern`.
//
// EVERY match in every file, not the first of each. Matching once per file
// counts FILES rather than declarations, and the copy a gate exists to catch
// is as easily a second one in the same module as one a directory over: a
// `const CATEGORIES = [...] as const` at block scope inside a component is
// legal TypeScript, and with the module-scope copy still above it the gate
// validated the first and the screen rendered the second.
//
// The error says which of the two failures happened, because they have
// different remedies: nothing matched means the constant was renamed or
// removed and the gate now certifies nothing; two matched means a second copy
// exists and must go.
func Declaration(tree, pattern string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("clientsource: %q is not a pattern: %w", pattern, err)
	}
	var found []string
	err = filepath.WalkDir(tree, func(path string, entry os.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			return nil
		case !strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx"):
			return nil
		// The suites are excluded, or a case that quotes the constant to
		// assert its shape would count as a second declaration of it.
		case strings.Contains(entry.Name(), ".test."):
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(source), -1) {
			found = append(found, m[1])
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("clientsource: the dashboard tree at %s could not be "+
			"walked, so this gate certifies nothing: %w", tree, err)
	}
	if len(found) != 1 {
		return "", fmt.Errorf("clientsource: %d declarations under %s match %s, want "+
			"exactly one — none is a gate certifying nothing, and two are two copies "+
			"that can drift from each other as well as from the engine",
			len(found), tree, pattern)
	}
	return found[0], nil
}

// Strings pulls every double-quoted string out of a declaration's body.
//
// WHATEVER IS BETWEEN THE QUOTES, never a shape that looks right. A pattern
// that matched only well-formed values silently skipped the malformed one —
// so a config kind spelled `mcp_servers` was not extracted at all, and the
// gate passed on a screen that could never read that collection.
func Strings(body string) []string {
	var out []string
	for _, m := range quoted.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

var quoted = regexp.MustCompile(`"([^"]*)"`)

// Field pulls the value of every `name: "…"` pair out of a declaration's body,
// for a list of objects rather than a list of strings.
func Field(body, name string) []string {
	re := regexp.MustCompile(regexp.QuoteMeta(name) + `:\s*"([^"]*)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

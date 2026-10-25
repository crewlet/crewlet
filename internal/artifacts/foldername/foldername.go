// Package foldername is the grammar of a folder name and the key two sibling
// folders may not share: pure functions over text, kept out of
// internal/artifacts because that package is a leaf (its leaf_test.go) and the
// Unicode tables this needs are not in its leaf set.
package foldername

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// A FOLDER NAME IS A PATH SEGMENT AND A LINE OF TEXT A PERSON READS, so its
// grammar is decided once, here, and every surface (the HTTP routes, the agent
// tools, the dashboard's form, the writer that forms the record) asks
// [Check] rather than restating a rule.
//
//   - It is NFC, trimmed of leading and trailing white space, and 1 to
//     [artifacts.MaxFolderNameBytes] UTF-8 bytes after both.
//   - It carries no `/`: the agents address a folder by a path of names, and a
//     name that held the separator would be two segments to one reader and one
//     to another.
//   - It carries no control character (C0, C1), no format character (the
//     bidirectional overrides and isolates, the zero-width joiners and spaces,
//     the byte-order mark, the soft hyphen, the tag characters) and no line or
//     paragraph separator. Each draws as nothing, or reorders what is drawn
//     around it, so two names that look identical could be two folders, and a
//     name could show one path while naming another.
//   - It is not `.` or `..`, which every reader of a path takes as "here" and
//     "the parent".
//
// THE KEY IS WHAT UNIQUENESS IS DECIDED ON. Two siblings may not share a
// [Key]: the name case-folded (Unicode full case folding, so
// "Straße" and "STRASSE" collide as a reader expects) and normalised to NFC
// again. The WRITER computes it and the record carries it, and the applier
// compares carried keys and never folds, so a Unicode table change in a later
// Go release cannot make two nodes disagree about a record they both applied.

// ErrInvalid reports a folder name outside the grammar, naming the rule and the
// fix. The artifacts writer wraps it in its own ErrInvalid, which is the
// sentinel that crosses the estate's wire.
var ErrInvalid = errors.New("foldername: invalid folder name")

// Check answers the canonical form of a folder name a caller typed
// and its uniqueness key, or [ErrInvalid] naming what to change.
func Check(raw string) (name, key string, err error) {
	if !utf8.ValidString(raw) {
		return "", "", fmt.Errorf("%w: the name is not valid UTF-8 text", ErrInvalid)
	}
	name = strings.TrimFunc(norm.NFC.String(raw), unicode.IsSpace)
	switch {
	case name == "":
		return "", "", fmt.Errorf("%w: a folder needs a name", ErrInvalid)
	case len(name) > artifacts.MaxFolderNameBytes:
		return "", "", fmt.Errorf("%w: the name is %d bytes and a folder name is at most %d; "+
			"shorten it", ErrInvalid, len(name), artifacts.MaxFolderNameBytes)
	case name == "." || name == "..":
		return "", "", fmt.Errorf("%w: %q reads as a place in a path rather than a name; "+
			"choose another name", ErrInvalid, name)
	case strings.ContainsRune(name, '/'):
		return "", "", fmt.Errorf("%w: a folder name may not contain \"/\", which separates "+
			"the folders in a path; use another character", ErrInvalid)
	}
	for i, r := range name {
		if invisibleInName(r) {
			return "", "", fmt.Errorf("%w: the name holds an invisible or control character "+
				"(U+%04X at byte %d), which would make two different names look the same; "+
				"remove it", ErrInvalid, r, i)
		}
	}
	return name, Key(name), nil
}

// Key is the key two sibling folders may not share: the name
// case-folded and normalised to NFC. It is asked only of a name
// [Check] admitted, and only by the writer, whose record carries it.
func Key(name string) string {
	return norm.NFC.String(cases.Fold().String(name))
}

// invisibleInName reports a rune a folder name may not hold: a control
// character, a format character, or a line or paragraph separator.
func invisibleInName(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

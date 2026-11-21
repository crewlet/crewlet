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
// grammar is decided once, here: every surface asks [Check]. A name is NFC,
// trimmed, 1 to [artifacts.MaxFolderNameBytes] UTF-8 bytes, and carries no
// `/` (agents address a folder by a path of names), no `.` or `..`, no control,
// separator or format character (bidi overrides, zero-width joiners, BOM, soft
// hyphen, tags): each draws as nothing or reorders its surroundings, so two
// look-alikes could be two folders and a name could show one path while naming
// another. THE KEY IS WHAT UNIQUENESS IS DECIDED ON: the name case-folded
// (full Unicode folding, so "Straße" and "STRASSE" collide) and NFC again. The
// WRITER computes it and the record carries it; the applier never folds, so a
// Unicode table change in a later Go release cannot split two nodes.

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

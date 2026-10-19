package edits

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Edit replaces one exact occurrence of Old with New in the file at Path of
// the base version.
type Edit struct {
	// Path is the file the edit applies to; empty names the base version's
	// entry ([Normalize]).
	Path string `json:"path,omitempty"`

	// Old is the text to find: exactly once in the file as it stands after
	// the edits before this one. Never empty, since an empty text occurs
	// everywhere.
	Old string `json:"old_str"`

	// New is the text it is replaced with; empty deletes Old.
	New string `json:"new_str"`
}

// ErrRefused is every refusal this package answers, wrapped by [*Refused].
var ErrRefused = errors.New("edits: refused")

// Refused names the edit that could not be applied and why. The whole set is
// refused with it: nothing an earlier edit did survives.
type Refused struct {
	// Index is the edit's position in the request, from zero.
	Index int

	// Path is the file it named.
	Path string

	// Matches is how many times Old occurs where it was looked for: zero or
	// more than one for a refusal of the match, and -1 for any other refusal
	// (the text is not UTF-8, the file is not in the base version, the
	// result is too large).
	Matches int

	// Reason says what to change.
	Reason string
}

func (e *Refused) Error() string {
	switch e.Matches {
	case -1:
		return fmt.Sprintf("edit %d on %q: %s", e.Index, e.Path, e.Reason)
	case 0:
		return fmt.Sprintf("edit %d on %q: old_str matches nothing in the file as it "+
			"stands after the edits before it; read the base version's text again "+
			"and quote it exactly", e.Index, e.Path)
	}
	places := fmt.Sprint(e.Matches)
	if e.Matches >= maxCounted {
		places = "at least " + places
	}
	return fmt.Sprintf("edit %d on %q: old_str matches %s places in the file as it "+
		"stands after the edits before it; quote more of the surrounding text so it "+
		"matches exactly one", e.Index, e.Path, places)
}

// Unwrap makes every refusal an [ErrRefused].
func (e *Refused) Unwrap() error { return ErrRefused }

// Normalize gives every edit its path, an empty one naming entry, and refuses
// an edit that could never apply: an empty Old, or an Old or New that is not
// UTF-8. It does not judge the path's grammar, which is the manifest's.
func Normalize(list []Edit, entry string) ([]Edit, error) {
	out := make([]Edit, len(list))
	for i, e := range list {
		if e.Path == "" {
			if entry == "" {
				return nil, &Refused{Index: i, Matches: -1, Reason: "the edit names no " +
					"path and the base version has no entry to default to"}
			}
			e.Path = entry
		}
		switch {
		case e.Old == "":
			return nil, &Refused{Index: i, Path: e.Path, Matches: -1,
				Reason: "old_str is empty, and empty text occurs everywhere"}
		case !utf8.ValidString(e.Old) || !utf8.ValidString(e.New):
			return nil, &Refused{Index: i, Path: e.Path, Matches: -1,
				Reason: "old_str and new_str must be UTF-8 text"}
		}
		out[i] = e
	}
	return out, nil
}

// Paths is every distinct path the edits name, in the order first named: the
// base version files a caller reads before [Apply].
func Paths(list []Edit) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range list {
		if !seen[e.Path] {
			seen[e.Path] = true
			out = append(out, e.Path)
		}
	}
	return out
}

// Apply applies every edit of list, in order, to the base version's texts, and
// answers each edited file's new text. ALL OR NOTHING: the first edit that
// cannot apply refuses the whole set ([*Refused]) and nothing is answered.
//
// Each edit's Old must occur EXACTLY ONCE in its file as it stands after the
// edits before it, overlapping occurrences counted: `aa` in `aaa` is two
// places, and replacing either would be a guess. A file must be UTF-8 text,
// and limit(path) bounds what an edit may grow it to, checked after every edit
// so the refusal names the edit that crossed it.
//
// texts is read, never written: the base version's bytes are the caller's.
func Apply(texts map[string][]byte, list []Edit, limit func(path string) int64) (map[string][]byte, error) {
	current := map[string]string{}
	for i, e := range list {
		text, ok := current[e.Path]
		if !ok {
			base, held := texts[e.Path]
			if !held {
				return nil, &Refused{Index: i, Path: e.Path, Matches: -1,
					Reason: "the base version holds no such file"}
			}
			if !utf8.Valid(base) {
				return nil, &Refused{Index: i, Path: e.Path, Matches: -1,
					Reason: "the file is not UTF-8 text, so it cannot be edited; " +
						"upload its new bytes instead"}
			}
			text = string(base)
		}
		at, matches := find(text, e.Old)
		if matches != 1 {
			return nil, &Refused{Index: i, Path: e.Path, Matches: matches}
		}
		text = text[:at] + e.New + text[at+len(e.Old):]
		if max := limit(e.Path); int64(len(text)) > max {
			return nil, &Refused{Index: i, Path: e.Path, Matches: -1,
				Reason: fmt.Sprintf("the file would be %d bytes and it may be at most %d",
					len(text), max)}
		}
		current[e.Path] = text
	}
	out := make(map[string][]byte, len(current))
	for p, text := range current {
		out[p] = []byte(text)
	}
	return out, nil
}

// find is where old first occurs in text, and how many places it occurs at,
// overlapping ones counted: one is the only answer an edit applies on, and the
// count a refusal names tells the author how much more context to quote.
// Counting stops at [maxCounted], which is already past any answer but "quote
// more of the surrounding text".
func find(text, old string) (int, int) {
	first := strings.Index(text, old)
	if first < 0 {
		return -1, 0
	}
	n := 1
	for at := first + 1; n < maxCounted; n++ {
		next := strings.Index(text[at:], old)
		if next < 0 {
			break
		}
		at += next + 1
	}
	return first, n
}

// maxCounted is the most matches a refusal counts.
const maxCounted = 1000

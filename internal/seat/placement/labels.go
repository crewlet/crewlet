package placement

import (
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// MaxLabelKeyBytes bounds a node label key.
//
// SIXTY-THREE: Kubernetes' limit on a label name — the length of a DNS label —
// which is the grammar an operator writing node.labels most likely already
// types, and a bound on what every presence lease and every object placement
// map repeats once per key.
const MaxLabelKeyBytes = 63

// ErrLabelKey is a string that is not a node label key.
var ErrLabelKey = errors.New("not a node label key")

// CheckLabelKey refuses a string that cannot be a node label key: empty, longer
// than [MaxLabelKeyBytes], not UTF-8, or holding whitespace or any character
// that does not print.
//
// ONE GRAMMAR, here, because a key is written in more than one place and
// matched EXACTLY between them: node.labels on every node, and the selector a
// role's placement names. Two validators that disagree never raise — a key one
// accepts and the other refuses is a label nothing can match.
// Whitespace is refused rather than trimmed because it is the one difference a
// reader cannot see: "zone" and "zone " look identical in both files and
// match nothing.
func CheckLabelKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: it is empty", ErrLabelKey)
	case len(key) > MaxLabelKeyBytes:
		return fmt.Errorf("%w: %q is %d bytes, at most %d", ErrLabelKey, key, len(key),
			MaxLabelKeyBytes)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: %q is not UTF-8", ErrLabelKey, key)
	}
	for _, r := range key {
		if unicode.IsSpace(r) || !unicode.IsGraphic(r) {
			return fmt.Errorf("%w: %q holds %U, and a key is matched exactly, so "+
				"it may hold no whitespace or unprintable character", ErrLabelKey, key, r)
		}
	}
	return nil
}

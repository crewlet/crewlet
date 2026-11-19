package advisordomain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The sentinels: each is a fact a caller acts on differently.
var (
	// ErrStaleVersion reports a write decided from a key, an episode's
	// revision or a verdict that has moved since it was read: read it again,
	// and decide again.
	ErrStaleVersion = errors.New("advisordomain: the finding changed since it was read")

	// ErrExists reports a record whose object already holds it: an episode
	// opened, a run requested or recorded, a week's report or a day's expiry
	// written.
	ErrExists = errors.New("advisordomain: already recorded")

	// ErrHeld reports an episode a key's hold keeps quiet: one no graver than
	// the hold's severity, before the hold lapses.
	ErrHeld = errors.New("advisordomain: the finding's key is held")

	// ErrIllegalMove reports a move the episode's state does not admit: a
	// closed episode moves no more (package doc, "Legal moves").
	ErrIllegalMove = errors.New("advisordomain: the finding cannot make that move")

	// ErrNotFound reports a key, an episode, a run or a report the log holds
	// nothing about.
	ErrNotFound = errors.New("advisordomain: not found")

	// ErrConflict reports a write that lost its compare-and-set every round.
	ErrConflict = errors.New("advisordomain: the record's subject kept changing under this write")

	// ErrInvalid reports a write or a query whose input the domain refuses,
	// naming the field and what to change.
	ErrInvalid = errors.New("advisordomain: invalid")
)

// invalid is an [ErrInvalid] naming what to change.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Sentence is a refusal of this domain worded for whoever reads it. The state
// log's own refusals take the framework's words for a person
// ([statelog.Sentence]); every other loses the package and class words it
// opens with, which say nothing about what to change.
func Sentence(err error) string {
	if said, ok := statelog.Sentence(err); ok {
		return said
	}
	text := err.Error()
	for _, cut := range sentenceCuts {
		text = strings.ReplaceAll(text, cut, "")
	}
	return text
}

// sentenceCuts are the words [Sentence] removes, the longest first, so a class
// word is removed whole before the package word inside it.
var sentenceCuts = []string{ErrInvalid.Error() + ": ", "advisordomain: "}

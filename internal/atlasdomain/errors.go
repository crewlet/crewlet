package atlasdomain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The sentinels: each is a fact a caller acts on differently.
var (
	// ErrStaleVersion reports a person's write decided from an entry that has
	// changed since they read it: read the entry again, and decide again.
	ErrStaleVersion = errors.New("atlasdomain: the entry changed since it was read")

	// ErrPurged reports a write about an entry or a value an administrator
	// purged. Nothing brings either back.
	ErrPurged = errors.New("atlasdomain: purged")

	// ErrNotDue reports an expiry of an hour that holds nothing past the
	// retention any more.
	ErrNotDue = errors.New("atlasdomain: nothing in that hour is past its retention")

	// ErrConflict reports a write that lost its compare-and-set every round.
	ErrConflict = errors.New("atlasdomain: the record's subject kept changing under this write")

	// ErrExists reports a create-once record whose subject already holds
	// one: the same claim landed under this operation, or a purge already ran.
	ErrExists = errors.New("atlasdomain: already recorded")

	// ErrInvalid reports a write or a query whose input the domain refuses,
	// naming the field and what to change.
	ErrInvalid = errors.New("atlasdomain: invalid")
)

// invalid is an [ErrInvalid] naming what to change.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Sentence is a refusal of this domain worded for whoever reads it: a person's
// write surface, a seat's tool, the assistant. The state log's own refusals are
// said in the framework's words for a person ([statelog.Sentence]), and every
// other loses the package and class words it opens with, which are for a log
// and say nothing about what to change, the vocabulary's included. No refusal
// here repeats a value, so nothing is cut from what follows them.
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
var sentenceCuts = []string{
	ErrInvalid.Error() + ": ", ErrPurged.Error() + ": ", atlas.ErrInvalid.Error() + ": ",
	"atlasdomain: ", "atlas: ",
}

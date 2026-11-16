package gitlet

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// A FIND ([GitFind]) is a finder's "go to file": the files of a commit's tree
// whose path holds the typed characters in order. An estate request, never a
// record, so it moves no RecordVersion.

// THE FINDER'S BOUNDS.
const (
	// MaxFindQueryBytes is the longest query a find takes: what a person
	// types into a finder, past any file's path a person would type, and the
	// bound of the matcher's work on each path, which is linear in it.
	MaxFindQueryBytes = 256
	// MaxFindLimit is the most files one find answers, and DefaultFindLimit how many
	// one naming none answers: a list a person scrolls, narrowed by typing on.
	// Fewer where their paths fill the answer's bytes.
	MaxFindLimit     = 100
	DefaultFindLimit = 50
)

// FindQuery is the files of a commit's tree whose path holds Query's
// characters in order ([GitFind]).
type FindQuery struct {
	// Rev is a commit's id (or a tag's naming one), or a tree's, whole or
	// abbreviated: the files of its root tree are searched.
	Rev string `json:"rev"`
	// Query is the characters a path holds in order, case-folded as unicode.SimpleFold
	// does (`K` finds the Kelvin sign): 1 to MaxFindQueryBytes bytes of UTF-8, no NUL,
	// a space a character like any other. Never handed to git.
	Query string `json:"query"`
	// Limit is the most files answered, 1 to MaxFindLimit; zero is
	// DefaultFindLimit.
	Limit int `json:"limit,omitempty"`
}

// Files is the most files q answers: its limit, or DefaultFindLimit where it
// names none.
func (q FindQuery) Files() int {
	if q.Limit == 0 {
		return DefaultFindLimit
	}
	return q.Limit
}

// validate refuses a find no Gitlet node answers ([ErrInvalid], naming the
// field): a revision that is no object id, a query that is empty, too long,
// not UTF-8 or holding a NUL, and a limit outside 1 to MaxFindLimit.
func (q FindQuery) validate() error {
	if err := validRev("find.rev", q.Rev); err != nil {
		return err
	}
	switch {
	case q.Query == "":
		return fmt.Errorf("%w: find.query: name the characters to find", ErrInvalid)
	case len(q.Query) > MaxFindQueryBytes:
		return fmt.Errorf("%w: find.query: %d bytes, past the %d a find takes", ErrInvalid, len(q.Query),
			MaxFindQueryBytes)
	case !utf8.ValidString(q.Query):
		return fmt.Errorf("%w: find.query: not UTF-8: a find matches the characters a person types", ErrInvalid)
	case strings.IndexByte(q.Query, 0) >= 0:
		return fmt.Errorf("%w: find.query: a query holds no NUL, which no path holds", ErrInvalid)
	}
	if q.Limit < 0 || q.Limit > MaxFindLimit {
		return fmt.Errorf("%w: find.limit: %d is not 1 to %d: a find answers 1 to %d files, %d when left out",
			ErrInvalid, q.Limit, MaxFindLimit, MaxFindLimit, DefaultFindLimit)
	}
	return nil
}

// FindAnswer is a find's one answer: no cursor, a person narrows by typing.
type FindAnswer struct {
	// Tree is the root tree searched, whole.
	Tree string `json:"tree"`
	// Matches is the best matches, best first: at most the query's limit,
	// fewer where their paths fill the answer's bytes.
	Matches []FindMatch `json:"matches"`
	// Total is how many files of the tree match, and Files how many files
	// the tree holds: every blob of its recursive listing whose path is at
	// most MaxGitPathBytes, a link among them and a submodule not.
	Total int `json:"total"`
	Files int `json:"files"`
}

// FindMatch is one file found.
type FindMatch struct {
	// Path is its path from the root, PathB64 its exact bytes where it is not
	// valid UTF-8 ([WirePath]).
	Path    string `json:"path"`
	PathB64 string `json:"path_b64,omitempty"`
	// OID is its blob, Mode its git mode (100644, 100755, 120000).
	OID  string `json:"oid"`
	Mode string `json:"mode"`
	// Spans are the runs of Path the query matched, each [start, end) in code points
	// of Path as carried (a run that is not UTF-8 is one position, matched by
	// nothing): what a finder draws in bold.
	Spans [][2]int `json:"spans"`
}

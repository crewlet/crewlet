package gitlet

import (
	"fmt"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/words"
)

// A FINDING'S EXCERPT is its blob's lines around its range, marked as its revision's diff
// changes them, as the change's files tab shows them.

// MaxExcerptAnchors is the most anchors one read excerpts: the findings one review raises.
const MaxExcerptAnchors = MaxFindingsPerReview

// DefaultExcerptContext is the lines a card shows each side of a finding's range, and
// MaxExcerptContext the most it may ask; the file view shows more.
const (
	DefaultExcerptContext = 3
	MaxExcerptContext     = 10
)

// MaxExcerptLines is the most lines one excerpt holds: the widest context either side and a
// range's first twenty lines, which is what a card shows of a long range.
const MaxExcerptLines = 2*MaxExcerptContext + 20

// MaxExcerptLineBytes is the most a line's text may encode to and be shown; past it the
// line is described by its size, as a blame's is (MaxBlameLineBytes). Forty such lines are
// under 44 KiB, so one excerpt always fits a page.
const MaxExcerptLineBytes = 1 << 10

// ExcerptsQuery asks for the lines around each anchor's range ([GitExcerpts]).
type ExcerptsQuery struct {
	// Anchors are the ranges, 1 to MaxExcerptAnchors, each id once.
	Anchors []ExcerptAnchor `json:"anchors"`
	// Context is the lines shown before and after each range: nil is DefaultExcerptContext.
	Context *int `json:"context,omitempty"`
}

// ExcerptAnchor is one range and the revision whose diff marks it: Base and Head whole ids,
// Base empty where the revision shares no history with its target (every line is added).
type ExcerptAnchor struct {
	LineAnchor
	Base string `json:"base,omitempty"`
	Head string `json:"head"`
}

// ExcerptsAnswer is a page of excerpts, in the anchors' order.
type ExcerptsAnswer struct {
	Excerpts []Excerpt `json:"excerpts"`
}

// Excerpt is one anchor's lines. Status is how the revision's diff changes its file (git's
// A, D, M or R), "" for a file it leaves as it was; Binary a pair git calls binary, shown by
// no line; More a window longer than MaxExcerptLines, whose first lines Lines holds.
type Excerpt struct {
	ID     string        `json:"id"`
	Status string        `json:"status,omitempty"`
	Binary bool          `json:"binary,omitempty"`
	Lines  []ExcerptLine `json:"lines"`
	More   bool          `json:"more,omitempty"`
}

// ExcerptLine is one line: Mark " " for a line both sides hold, "+" one the revision adds,
// "-" one it deletes; Old and New its numbers, absent on a side it is not on; Text without its
// line feed, or past MaxExcerptLineBytes absent with TextBytes its size.
type ExcerptLine struct {
	Mark      string `json:"mark"`
	Old       int    `json:"old,omitempty"`
	New       int    `json:"new,omitempty"`
	Text      string `json:"text,omitempty" untrusted:"fence"`
	TextBytes int    `json:"text_bytes,omitempty"`
}

// ContextOf is the context q applies: its own, or DefaultExcerptContext where it names none.
func (q ExcerptsQuery) ContextOf() int {
	if q.Context == nil {
		return DefaultExcerptContext
	}
	return *q.Context
}

func (q ExcerptsQuery) validate() error {
	if n := len(q.Anchors); n == 0 || n > MaxExcerptAnchors {
		return fmt.Errorf("%w: excerpts.anchors: %d anchors; a read excerpts 1 to %d", ErrInvalid, n,
			MaxExcerptAnchors)
	}
	if c := q.ContextOf(); c < 0 || c > MaxExcerptContext {
		return fmt.Errorf("%w: excerpts.context: %d lines; an excerpt shows 0 to %d each side of its range",
			ErrInvalid, c, MaxExcerptContext)
	}
	ids := make(map[string]bool, len(q.Anchors))
	for i, a := range q.Anchors {
		field := fmt.Sprintf("excerpts.anchors[%d]", i)
		switch {
		case a.ID == "" || len(a.ID) > maxAnchorIDBytes || !utf8.ValidString(a.ID):
			return fmt.Errorf("%w: %s.id: an anchor is named by 1 to %d bytes of UTF-8, which its answer carries "+
				"back", ErrInvalid, field, maxAnchorIDBytes)
		case ids[a.ID]:
			return fmt.Errorf("%w: %s.id: %q names two anchors, whose answers would read as one", ErrInvalid,
				field, a.ID)
		case !a.Side.Valid():
			return fmt.Errorf("%w: %s.side: %q is not one of %s", ErrInvalid, field, a.Side, words.Or(Sides()))
		case a.Side == SideOld && a.Base == "":
			return fmt.Errorf("%w: %s.side: an anchor on the old side is read in the revision's base, which "+
				"base names", ErrInvalid, field)
		case a.Start < 1 || a.End < a.Start || a.End > MaxAnchorLine:
			return fmt.Errorf("%w: %s: lines %d to %d are no range: lines count from 1, end is at or after "+
				"start, and no file holds a line past %d", ErrInvalid, field, a.Start, a.End, MaxAnchorLine)
		}
		ids[a.ID] = true
		if _, err := GitPath(field+".path", a.Path, a.PathB64, false); err != nil {
			return err
		}
		for _, id := range []struct {
			field, id string
			empty     bool
		}{{".blob_oid", a.BlobOID, false}, {".head", a.Head, false}, {".base", a.Base, true}} {
			if err := wholeID(field+id.field, id.id, id.empty); err != nil {
				return err
			}
		}
	}
	return nil
}

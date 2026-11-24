package seataccess

import (
	"encoding/json"
	"maps"
	"math"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A SEAT'S RECORD IS HELD TO ITS CLASS (internal/coord's MaxSeatAccessBytes),
// so every word in it that a vendor or a step writes has a bound of its own:
// a sentence, a note and a name to [MaxSeatAccessTextBytes] as encoded, cut
// at its head and marked, and a link to [MaxSeatAccessURLBytes], dropped
// past it, since a cut link leads nowhere. What is left to grow is the access
// itself (the document's scope and grants, and the vendor's reading of them),
// which a sync checks lane by lane before it takes a step ([syncer.fits]).

// MaxSeatAccessTextBytes bounds one sentence, note or name the ledger keeps,
// as encoded: a vendor's error or step is useful at its head, and a step that
// lists a scope lists the document's, which the Agents tab shows whole.
const MaxSeatAccessTextBytes = 1 << 10

// MaxSeatAccessURLBytes bounds one link the ledger keeps, as encoded: the
// length browsers and proxies keep whole.
const MaxSeatAccessURLBytes = 2 << 10

// encodedLen is how many bytes v is as the ledger encodes it.
func encodedLen(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return math.MaxInt
	}
	return len(raw)
}

// boundedText is s cut, marked, until it encodes within
// [MaxSeatAccessTextBytes] beside its quotes: a byte JSON writes as six
// halves what is kept again.
func boundedText(s string) string {
	for n := MaxSeatAccessTextBytes; ; n /= 2 {
		cut := textcut.Within(s, n)
		if encodedLen(cut)-2 <= MaxSeatAccessTextBytes || n == 0 {
			return cut
		}
	}
}

// boundedURL is u, or nothing where it encodes past [MaxSeatAccessURLBytes].
func boundedURL(u string) string {
	if encodedLen(u)-2 > MaxSeatAccessURLBytes {
		return ""
	}
	return u
}

// bounded is r with every word in it held to its bound, as the ledger keeps
// it. r's own lanes are left as they were.
func (r Record) bounded() Record {
	out := r
	out.Lanes = make(map[Lane]LaneRecord, len(r.Lanes))
	for lane, lr := range r.Lanes {
		if lr.Sync != nil {
			sync := lr.Sync.bounded()
			lr.Sync = &sync
		}
		if lr.Settling != nil {
			settling := *lr.Settling
			settling.Reason = boundedText(settling.Reason)
			lr.Settling = &settling
		}
		if lr.Words != nil {
			lr.Words = lr.Words.bounded()
		}
		out.Lanes[lane] = lr
	}
	return out
}

// bounded is r with its steps' words and its failure's held to their bounds.
func (r SyncResult) bounded() SyncResult {
	out := r
	if r.Steps != nil {
		out.Steps = make([]StepOutcome, len(r.Steps))
		for i, step := range r.Steps {
			step.Text, step.Note = boundedText(step.Text), boundedText(step.Note)
			step.ActionURL = boundedURL(step.ActionURL)
			out.Steps[i] = step
		}
	}
	if r.Error != nil {
		f := *r.Error
		f.Said, f.Remedy, f.ActionURL = boundedText(f.Said), boundedText(f.Remedy), boundedURL(f.ActionURL)
		out.Error = &f
	}
	return out
}

// bounded is w with each level's words and each scope entry's name held to
// [MaxSeatAccessTextBytes].
func (w *GrantWords) bounded() *GrantWords {
	out := &GrantWords{}
	for _, l := range w.Levels {
		out.Levels = append(out.Levels, Level{ID: l.ID, Name: boundedText(l.Name), Note: boundedText(l.Note)})
	}
	if w.ScopeNames != nil {
		out.ScopeNames = maps.Clone(w.ScopeNames)
		for entry, name := range out.ScopeNames {
			out.ScopeNames[entry] = boundedText(name)
		}
	}
	return out
}

// fitsItsClass reports whether r, as the ledger keeps it, is within its class.
func (r Record) fitsItsClass() bool {
	return encodedLen(r.bounded()) <= coord.MaxSeatAccessBytes
}

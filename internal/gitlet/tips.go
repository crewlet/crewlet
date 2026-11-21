package gitlet

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
)

// MaxTips is the most objects one tips read names: a screen of branches or tags.
// Each tip counted against a base is one commit walk, so it bounds a read's
// processes, which BenchmarkTips holds well inside the read's budget.
const MaxTips = 100

// TipsQuery is many objects read at once (GitTips): each tip's facts and, with
// a Base, how far it is from it.
type TipsQuery struct {
	// OIDs are the objects read, 1 to MaxTips WHOLE ids, each once: a
	// branch's head or a tag as its ref's row names it.
	OIDs []string `json:"oids"`
	// Base is the commit (or a tag naming one) each tip's commit is counted
	// against, whole or abbreviated. Empty counts nothing.
	Base string `json:"base,omitempty"`
}

func (q TipsQuery) validate() error {
	switch {
	case len(q.OIDs) == 0:
		return refusedf("%w: tips.oids: name the objects to read", ErrInvalid)
	case len(q.OIDs) > MaxTips:
		return refusedf("%w: tips.oids: %d objects, past the %d one read names (MaxTips)", ErrInvalid,
			len(q.OIDs), MaxTips)
	}
	seen := make(map[string]bool, len(q.OIDs))
	for i, id := range q.OIDs {
		switch {
		case !gitref.ValidOID(id) || id == ZeroOID:
			return fmt.Errorf("%w: tips.oids[%d]: %q is not a whole object id: a tip is read by the id its "+
				"ref's row names", ErrInvalid, i, id)
		case seen[id]:
			return fmt.Errorf("%w: tips.oids[%d]: %s is named twice", ErrInvalid, i, id)
		}
		seen[id] = true
	}
	if q.Base != "" {
		return validRev("tips.base", q.Base)
	}
	return nil
}

// TipsAnswer is one page of a tips read: the tips from its cursor on, in the
// query's order, as many as the answer's bytes hold and the read's budget of
// git counted; Next resumes at the first one left out.
type TipsAnswer struct {
	// Base is the base's commit, whole, where the query named one.
	Base string `json:"base,omitempty"`
	Tips []Tip  `json:"tips"`
}

// Tip is one object of a tips read.
type Tip struct {
	// OID is the object as the query named it; Type its type (commit, tag,
	// tree or blob). Peeled is what it names once every tag is peeled, itself
	// for any other type, and PeeledType that object's type.
	OID        string `json:"oid"`
	Type       string `json:"type"`
	Peeled     string `json:"peeled"`
	PeeledType string `json:"peeled_type"`
	// Tag is an annotated tag's own facts, the outermost tag's for a tag of a
	// tag; absent for every other type.
	Tag *TipTag `json:"tag,omitempty"`
	// Commit is the peeled commit as a history lists it; absent where the
	// object peels to a tree or a blob.
	Commit *LogCommit `json:"commit,omitempty"`
	// Behind is the commits the base has that the tip lacks, Ahead the reverse:
	// set for a commit tip beside a base; absent with CountsOutOfTime where the
	// read's budget of git ran out counting them.
	Behind          *int `json:"behind,omitempty"`
	Ahead           *int `json:"ahead,omitempty"`
	CountsOutOfTime bool `json:"counts_out_of_time,omitempty"`
}

// TipTag is an annotated tag's own facts, each unbounded field described past
// its bound as a LogCommit's are (MaxLogPersonBytes, MaxLogSubjectBytes).
type TipTag struct {
	Tagger           string `json:"tagger,omitempty"`
	TaggerBytes      int    `json:"tagger_bytes,omitempty"`
	TaggerEmail      string `json:"tagger_email,omitempty"`
	TaggerEmailBytes int    `json:"tagger_email_bytes,omitempty"`
	// TaggedAt is the tagger's date, to the second; absent for a tag that
	// names no tagger or no date git reads.
	TaggedAt *time.Time `json:"tagged_at,omitempty"`
	// Subject is the message's first paragraph as a commit's is read (git's
	// %s), a signature no part of it.
	Subject      string `json:"subject,omitempty"`
	SubjectBytes int    `json:"subject_bytes,omitempty"`
}

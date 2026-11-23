package executor

import (
	"github.com/crewlet/crewlet/internal/redact"
)

// WHAT THE SCRUB TOOK OUT of the members a take's meter counts, put back on
// its count ([meterReading.unscrubbed]): a box that resumes its own copy
// resumes it unscrubbed, so a count read off the kept archive alone is low by
// every value and redaction the scrub took out. Each text's share is the
// charge of what no marker keeps of its own words, never a value's length
// less its marker's: Gemini CLI charges a non-ASCII password above the ASCII
// marker that replaced it, however much shorter. ADR-0073's amendment on
// Gemini CLI and Qwen Code ("What the scrub took out is counted back") has
// the argument.

// scrubTally is what a take's scrub took out of the members its meter counts
// ([newScrubTally]), at their charge ([escCharge]), once the scrub ends.
type scrubTally struct {
	// metered reports whether the meter counts the member a kept archive
	// names so; an export's one document is named "".
	metered func(member string) bool
	taken   int64
}

// newScrubTally is the tally of a take of session's archive, as carries'
// meter counts it ([carrier.meters]).
func newScrubTally(carries carrier, session string) *scrubTally {
	return &scrubTally{metered: func(member string) bool { return carries.meters(session, member) }}
}

// tallying is b keeping tally of what its scrub takes out of the members
// tally's meter counts.
func (b scrubBounds) tallying(tally *scrubTally) scrubBounds {
	b.tally = tally
	return b
}

// member reports whether the member named kept, as the kept archive names it,
// is one t counts; nil counts none.
func (t *scrubTally) member(kept string) bool {
	return t != nil && t.metered(kept)
}

// took keeps what spool's first readings took out, once they have all ended;
// nil keeps nothing.
func (t *scrubTally) took(spool *unitSpool) {
	if t != nil {
		t.taken = spool.taken
	}
}

// tallying is s keeping tally of what it takes out of each text, where
// tally says so ([textScrub.taken]).
func (s scrubber) tallying(tally bool) scrubber {
	s.tally = tally
	return s
}

// escCharge is the bytes p comes to as JSON.stringify writes it in a string:
// `"`, `\` and the five controls it names two each, any other control six,
// every other byte one. AT LEAST every charge a harness the meter counts puts
// on a text: none above a token a byte of the file it holds (a quarter of its
// UTF-16 units for Gemini CLI's JSON, a token and a half a unit of its text,
// at most a token a byte of a valid UTF-8 rune of two bytes or more), and a
// file holds what its harness's JSON writer wrote.
func escCharge[T string | []byte](p T) int64 {
	n := int64(len(p))
	for i := range len(p) {
		switch c := p[i]; {
		case c == '"', c == '\\', c == '\b', c == '\f', c == '\n', c == '\r', c == '\t':
			n++
		case c < ' ':
			n += 5
		}
	}
	return n
}

// takenFrom is what a scrub took out of text, kept as kept, at its charge: the
// charge of text less that of kept's own words ([redact.OwnWords]). A marker
// text held already counts as taken, which only tells more.
func takenFrom(text, kept string) int64 {
	if text == kept {
		return 0
	}
	var own int64
	for _, words := range redact.OwnWords(kept) {
		own += escCharge(words)
	}
	return max(0, escCharge(text)-own)
}

// ownCharge is [takenFrom] of a text that arrives in pieces: the charge of its
// kept text's own words, read as it is written ([redact.OwnWordsWriter]),
// holding none of it.
type ownCharge struct {
	words     *redact.OwnWordsWriter
	stretches ownStretches
}

func newOwnCharge() *ownCharge {
	c := &ownCharge{}
	c.words = redact.NewOwnWordsWriter(&c.stretches)
	return c
}

// write reads p, the kept text's next bytes.
func (c *ownCharge) write(p []byte) { _, _ = c.words.Write(p) }

// taken ends the kept text and is what its scrub took out of a text that
// charged in; zero for no count (nil).
func (c *ownCharge) taken(in int64) int64 {
	if c == nil {
		return 0
	}
	_ = c.words.Close()
	return max(0, in-c.stretches.n)
}

// ownStretches charges the own words an [redact.OwnWordsWriter] hands it,
// taking back a run that turned out to be a marker.
type ownStretches struct{ n, marked int64 }

var _ redact.StretchReader = (*ownStretches)(nil)

func (s *ownStretches) Stretch(int) {}

func (s *ownStretches) Write(p []byte) (int, error) {
	s.n += escCharge(p)
	return len(p), nil
}

func (s *ownStretches) Mark()   { s.marked = s.n }
func (s *ownStretches) Commit() {}
func (s *ownStretches) Rewind() { s.n = s.marked }

// tallier is where a text's scrub writes, told beside its bytes what the scrub
// took out of it, so that goes where the bytes go: kept, cut back or let go
// with them ([unitSpool.tally], [candidate.tally]).
type tallier interface{ tally(taken int64) }

// tallyOnto tells w, where it keeps a tally, that taken was taken out of
// what was written to it.
func tallyOnto(w any, taken int64) {
	if t, ok := w.(tallier); ok && taken > 0 {
		t.tally(taken)
	}
}

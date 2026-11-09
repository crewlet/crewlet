package types

import "slices"

// CommittedChange is one kind of change a turn's write committed: what the
// turn's outcome is read from, never the name of the tool that made it.
type CommittedChange string

// The changes a committed write can be.
const (
	// CommittedItem is a work item created, or a field, status, assignment,
	// relation or checklist of one moved.
	CommittedItem CommittedChange = "item"
	// CommittedComment is a comment on a work item, a page or a change.
	CommittedComment CommittedChange = "comment"
	// CommittedAnswer is a comment that answers somebody's ask.
	CommittedAnswer CommittedChange = "answer"
	// CommittedPage is a page created or saved.
	CommittedPage CommittedChange = "page"
	// CommittedChat is a chat message posted.
	CommittedChat CommittedChange = "chat"
	// CommittedCode is a push, a change opened, merged or closed, or a review.
	CommittedCode CommittedChange = "code"
)

// CommittedChanges is the closed set.
var CommittedChanges = []CommittedChange{
	CommittedItem, CommittedComment, CommittedAnswer, CommittedPage, CommittedChat, CommittedCode,
}

// Valid reports whether c is one of [CommittedChanges].
func (c CommittedChange) Valid() bool { return slices.Contains(CommittedChanges, c) }

// TurnOutcome is what a turn did, read from the changes its writes committed.
type TurnOutcome string

// The three outcomes, highest first: a turn that moved anything moved, one that
// only wrote words said words, and one that committed nothing did nothing.
const (
	OutcomeMoved   TurnOutcome = "moved"
	OutcomeWords   TurnOutcome = "words"
	OutcomeNothing TurnOutcome = "nothing"
)

// TurnOutcomes is the closed set.
var TurnOutcomes = []TurnOutcome{OutcomeMoved, OutcomeWords, OutcomeNothing}

// Valid reports whether o is one of [TurnOutcomes].
func (o TurnOutcome) Valid() bool { return slices.Contains(TurnOutcomes, o) }

// Outcome is the class a committed change puts its turn in on its own.
func (c CommittedChange) Outcome() TurnOutcome {
	switch c {
	case CommittedComment, CommittedChat:
		return OutcomeWords
	case CommittedItem, CommittedAnswer, CommittedPage, CommittedCode:
		return OutcomeMoved
	}
	return OutcomeNothing
}

// TurnTokens is a turn's model spend: its own and delegated calls (Input,
// Output and the cached share), and its in-turn auxiliary calls apart.
type TurnTokens struct {
	Input      int `json:"input,omitempty"`
	Output     int `json:"output,omitempty"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	AuxInput   int `json:"aux_input,omitempty"`
	AuxOutput  int `json:"aux_output,omitempty"`
}

// Plus is t and o summed.
func (t TurnTokens) Plus(o TurnTokens) TurnTokens {
	return TurnTokens{Input: t.Input + o.Input, Output: t.Output + o.Output,
		CacheRead: t.CacheRead + o.CacheRead, CacheWrite: t.CacheWrite + o.CacheWrite,
		AuxInput: t.AuxInput + o.AuxInput, AuxOutput: t.AuxOutput + o.AuxOutput}
}

// WakeOf is what woke a turn, as the turn ledger keys it: the object's kind and
// the reason it reached the seat, a schedule by its name, and otherwise the
// waking event's type.
func WakeOf(t Trigger) string {
	switch o := t.Origin; {
	case o.Kind == OriginSchedule:
		return o.Kind + ":" + o.ID
	case o.Kind != "" && o.Reason != "":
		return o.Kind + ":" + o.Reason
	case o.Kind != "":
		return o.Kind
	}
	return t.Type
}

// OutcomeOf is the highest outcome a turn's committed changes reach.
func OutcomeOf(changes []CommittedChange) TurnOutcome {
	out := OutcomeNothing
	for _, c := range changes {
		switch c.Outcome() {
		case OutcomeMoved:
			return OutcomeMoved
		case OutcomeWords:
			out = OutcomeWords
		}
	}
	return out
}

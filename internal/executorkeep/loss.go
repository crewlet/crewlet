package executorkeep

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

// A RUN THAT DID NOT CONTINUE ITS SESSION SAYS SO, AND WHY: the package doc's
// "A session not continued". [Loss] is why, in one vocabulary every surface
// reads (the run's record, its launch and collection events, the executor
// log's end, the counter and the session_carry objective), and [NotContinued]
// is the whole verdict: the loss, what the run did instead, the turns either
// side, and the engine's detail.

// Loss is why a run did not continue the newest session its turn or its
// conversation left. A FAULT ([Loss.Fault]) is a bad event for the
// session_carry objective; the rest are deliberate, said and never burned.
type Loss string

const (
	// LossTooLarge is when the session was past [SessionMaxBytes], taken or sealed.
	LossTooLarge Loss = "too_large"

	// LossKeepFailed is when the session could not be taken out of its box and kept
	// (its take, pack, scrub or stream failed, a taker lease or the company's
	// policy could not be read, the run's bridged log could not be read, a
	// minted session was not bound).
	LossKeepFailed Loss = "keep_failed"

	// LossNoSpoolRoom is when the node found no spool room within its wait.
	LossNoSpoolRoom Loss = "no_spool_room"

	// LossStoreRefused is when the object store refused the upload, or its byte
	// reservation is spent.
	LossStoreRefused Loss = "store_refused"

	// LossLogRefused is when the executor log refused the capture's record, or never
	// answered it, or its row names no capture the run's record keeps.
	LossLogRefused Loss = "log_refused"

	// LossCaptureLate is when a newer capture was still being kept when the run
	// started, and the run did not wait it out.
	LossCaptureLate Loss = "capture_late"

	// LossReleaseFloor is when the session was written by a harness release or in an
	// archive format no box of this build continues.
	LossReleaseFloor Loss = "release_floor"

	// LossBoxRefused is when the box's harness said it continues no such session, or
	// the box could not be asked.
	LossBoxRefused Loss = "box_refused"

	// LossRestoreFailed is when the session's object could not be fetched or read,
	// or the box's restore failed and its script started a fresh session.
	LossRestoreFailed Loss = "restore_failed"

	// LossBoxLost is when the job was lost with its box before any capture of it.
	LossBoxLost Loss = "box_lost"

	// LossContextExhausted is when the session ran out of its model's context, and
	// is retired. Deliberate.
	LossContextExhausted Loss = "context_exhausted"

	// LossRequestRefused is when the model's provider refused the request the
	// session makes, and it is retired. Deliberate.
	LossRequestRefused Loss = "request_refused"

	// LossHarnessChanged is when the member the launch picked runs another harness,
	// and a session is continued only by the harness that wrote it. Deliberate.
	LossHarnessChanged Loss = "harness_changed"

	// LossBudgetFloor is when the seat's budget windows cannot carry what the
	// session sends again before the run reads a word. Deliberate.
	LossBudgetFloor Loss = "budget_floor"

	// LossEntryChanged is when the launch runs under another providers.llm entry
	// than the session ran under, and its harness continues a carried session
	// only under its own (sandbox.EntryContinues). Deliberate.
	LossEntryChanged Loss = "entry_changed"
)

// Losses are every loss this build names, faults first.
var Losses = []Loss{
	LossTooLarge, LossKeepFailed, LossNoSpoolRoom, LossStoreRefused, LossLogRefused, LossCaptureLate,
	LossReleaseFloor, LossBoxRefused, LossRestoreFailed, LossBoxLost,
	LossContextExhausted, LossRequestRefused, LossHarnessChanged, LossBudgetFloor, LossEntryChanged,
}

// deliberate are the losses the engine chose, which burn no objective.
var deliberate = []Loss{LossContextExhausted, LossRequestRefused, LossHarnessChanged, LossBudgetFloor,
	LossEntryChanged}

// Valid reports whether l is one this build names. A newer build's loss off
// the wire is a value all the same, worded [UnnamedLossWords].
func (l Loss) Valid() bool { return slices.Contains(Losses, l) }

// Within reports whether l is inside the bound a record holds a loss to: a
// UTF-8 word of 1 to [WordMaxBytes] bytes, a newer build's included. What every
// writer and reader of a record holds it to, so a newer build's loss is relayed,
// never refused. Where this build originates one it is one of [Losses] (the
// package doc's "A session not continued").
func (l Loss) Within() bool { return wordWithin(string(l)) }

// wordWithin reports whether w is an engine word a record holds: 1 to
// [WordMaxBytes] bytes of UTF-8.
func wordWithin(w string) bool { return w != "" && len(w) <= WordMaxBytes && utf8.ValidString(w) }

// Fault reports whether l is a bad event for the session_carry objective:
// every loss but the deliberate five, a newer build's unnamed one included,
// since a loss this build cannot name is not one it chose.
func (l Loss) Fault() bool { return !slices.Contains(deliberate, l) }

// UnnamedLossWords is how a loss this build does not name is worded.
const UnnamedLossWords = "for a reason this build does not name"

// Words is the loss as a clause of [NotContinued.Sentence]: what happened to
// the session. The dashboard holds a copy (contract/sessions.ts's
// SESSION_LOSS_WORDS), which internal/api's gate holds to this.
func (l Loss) Words() string {
	switch l {
	case LossTooLarge:
		return fmt.Sprintf("its session was larger than the %d MiB kept for one session", SessionMaxBytes>>20)
	case LossKeepFailed:
		return "its session could not be taken out of its box and kept"
	case LossNoSpoolRoom:
		return "the node had no spool room to keep its session"
	case LossStoreRefused:
		return "the object store refused its session"
	case LossLogRefused:
		return "the executor log did not record its session"
	case LossCaptureLate:
		return "its newest session was still being kept when the run started"
	case LossReleaseFloor:
		return "its session was written by a harness release or format no box continues"
	case LossBoxRefused:
		return "the box's harness would not continue its session"
	case LossRestoreFailed:
		return "its session could not be restored into the box"
	case LossBoxLost:
		return "its box was lost before its session was kept"
	case LossContextExhausted:
		return "its session ran out of its model's context"
	case LossRequestRefused:
		return "its model's provider refused the request its session makes"
	case LossHarnessChanged:
		return "the seat now runs another harness, which cannot continue a session this one wrote"
	case LossBudgetFloor:
		return "the seat's budget windows cannot carry what its session sends again"
	case LossEntryChanged:
		return "the launch runs under another providers.llm entry than its session ran under"
	}
	return UnnamedLossWords
}

// Instead is what a run did in place of the session it did not continue.
type Instead string

const (
	// InsteadFresh is a session of its own, started in the lost one's place.
	InsteadFresh Instead = "fresh"

	// InsteadOlder is an older session, continued in the lost one's place:
	// it ends before the work the lost one held.
	InsteadOlder Instead = "older"
)

// Insteads are both.
var Insteads = []Instead{InsteadFresh, InsteadOlder}

// Valid reports whether i is one of [Insteads].
func (i Instead) Valid() bool { return slices.Contains(Insteads, i) }

// Within reports whether i is inside its bound, as [Loss.Within].
func (i Instead) Within() bool { return wordWithin(string(i)) }

// NotContinued is a run's verdict on a session it did not continue: the
// loss, what it did instead, and where it can be placed, which turn's session
// was lost and which older one, or older capture of its own turn, it
// continued. Detail is the engine's account, which quotes what refused (a
// box, a store) and so is fenced.
type NotContinued struct {
	Loss    Loss    `json:"loss"`
	Instead Instead `json:"instead"`

	// LostTurn is the turn whose session was lost where it is not the run's
	// own: an earlier turn of the conversation. Empty for the run's own turn.
	LostTurn string `json:"lost_turn,omitempty"`

	// OlderTurn is the turn whose older session the run continued instead,
	// and Older the older capture of its own turn it continued; each only
	// beside [InsteadOlder], and at most one.
	OlderTurn string        `json:"older_turn,omitempty"`
	Older     *CapturePlace `json:"older,omitempty"`

	Detail string `json:"detail,omitempty" untrusted:"fence"`
}

// NotContinuedMaxEntries is how many verdicts an [Ending] keeps: the turn's
// first (what the conversation lost going into it) and its newest (what its
// final session lacks). Its launches' records hold every one.
const NotContinuedMaxEntries = 2

// Lost is the verdict loss, instead, detail of the run's own turn's session.
func Lost(loss Loss, instead Instead, detail string) *NotContinued {
	return &NotContinued{Loss: loss, Instead: instead, Detail: detail}
}

// Validate refuses a verdict the engine could not mean: a loss or an instead
// outside its bound, an older place before (1, 1) or beside a fresh session, or
// a turn id outside its bound. The detail is cut, not refused ([NotContinued.Fit]).
//
// A LOSS OR AN INSTEAD THIS BUILD DOES NOT NAME IS NOT REFUSED: a newer build
// sends it, and a node of this build relays one a newer node stamped, so a
// refusal would lose the record it rides in a rolling upgrade (the package doc's
// "A session not continued").
func (n NotContinued) Validate() error {
	switch {
	case !n.Loss.Within():
		return fmt.Errorf("the session's loss %q is not a word of 1 to %d bytes of UTF-8", n.Loss, WordMaxBytes)
	case !n.Instead.Within():
		return fmt.Errorf("what the run did instead, %q, is not a word of 1 to %d bytes of UTF-8", n.Instead,
			WordMaxBytes)
	case n.Instead == InsteadFresh && (n.OlderTurn != "" || n.Older != nil):
		return fmt.Errorf("a run that started a fresh session names an older one it continued")
	case n.OlderTurn != "" && n.Older != nil:
		return fmt.Errorf("the verdict names both an older turn and an older capture of the run's own")
	case n.Older != nil && (n.Older.LaunchSeq <= 0 || n.Older.CaptureSeq <= 0):
		return fmt.Errorf("the older capture is placed at (%d, %d), and every capture is placed from (1, 1)",
			n.Older.LaunchSeq, n.Older.CaptureSeq)
	case !utf8.ValidString(n.Detail):
		return fmt.Errorf("the verdict's detail is not valid UTF-8")
	}
	for _, turn := range []struct{ field, id string }{{"lost_turn", n.LostTurn}, {"older_turn", n.OlderTurn}} {
		if len(turn.id) > TurnIDMaxBytes || !utf8.ValidString(turn.id) {
			return fmt.Errorf("the verdict's %s is not a valid UTF-8 id of at most %d bytes", turn.field,
				TurnIDMaxBytes)
		}
	}
	return nil
}

// Fit is the verdict with its detail cut as a record keeps it ([Excerpt]).
func (n NotContinued) Fit() NotContinued {
	n.Detail = Excerpt(n.Detail, UnkeptReasonMaxBytes)
	return n
}

// Within reports whether every field is inside its bound, a loss or an
// instead a newer build names included: what a reader holds a record to.
func (n NotContinued) Within() bool {
	return n.Loss.Within() && n.Instead.Within() &&
		len(n.LostTurn) <= TurnIDMaxBytes && len(n.OlderTurn) <= TurnIDMaxBytes &&
		(n.Older == nil || (n.Older.LaunchSeq > 0 && n.Older.CaptureSeq > 0)) &&
		len(n.Detail) <= UnkeptReasonMaxBytes
}

// Sentence is the verdict in the engine's words, the one wording every
// surface shows (the dashboard's lib/sessionLoss.ts is its copy, held to it by
// internal/api's gate). It holds no detail: that is somebody else's words.
func (n NotContinued) Sentence() string {
	whose := "its session"
	if n.LostTurn != "" {
		whose = "turn " + n.LostTurn + "'s session"
	}
	said := "The run did not continue " + whose + ": " + n.Loss.Words() + "."
	switch {
	case n.Instead == InsteadFresh:
		return said + " It started a fresh session."
	case n.Instead != InsteadOlder:
		return said
	case n.OlderTurn != "":
		return said + " It continued turn " + n.OlderTurn + "'s older session instead, which ends before that work."
	case n.Older != nil:
		return said + fmt.Sprintf(" It continued an older capture of its turn (launch %d, capture %d) instead, "+
			"which ends before that work.", n.Older.LaunchSeq, n.Older.CaptureSeq)
	}
	return said + " It continued an older session instead, which ends before that work."
}

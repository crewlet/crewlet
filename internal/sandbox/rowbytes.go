package sandbox

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A run's row is held to a byte budget, and every field it carries has a share
// of it.
//
// # Why there is a budget at all
//
// The row is ONE VALUE in the coordination store, read and written whole by
// every mutation (coordstore.go), and one value is one broker message, held to
// [queue.MaxPayloadBytes] with its headers. Past it every write of the row is
// refused at the broker: a bridged call's append, a lease stamped on the run, a
// status flip, the hold a turn takes on its next scratch repository. The
// bridged call log grew one call at a time with nothing measuring it — two
// hundred builtin answers of 64 KiB are 12.8 MiB — so a long run's row stopped
// taking writes, and the log was the first thing to go: the resumed turn's
// ledger, its delivery check and its reviewer's evidence missed what the run
// did.
//
// # The shares, in bytes as the row holds them
//
// Every figure here is bytes AS THE ROW'S ENCODER WRITES THEM (json.Marshal,
// HTML escaping on), escaping counted: a cut is measured by internal/kept, never
// estimated from what was sent, so no six-fold multiplier is spent on text that
// does not escape. [RowBytes] is split three ways, and the split closes:
//
//	the bridged call log ............ BridgeLogBytes      3,407,872  (3.25 MiB)
//	the parked turn's state ......... ExecuteStateBytes   2,097,152  (2 MiB)
//	everything else, and the room
//	  a later build adds fields in .. RestBytes           2,879,488  (2.75 MiB)
//	                                                      ---------
//	                                  RowBytes            8,384,512
//
// THE BRIDGED CALL LOG is held by construction: [MaxBridgeCalls] calls, one of
// them (the newest successful submission, which the collection replays) at most
// [MaxSubmissionCallBytes] and every other at most [MaxBridgeCallBytes], plus
// the identities of the calls its count bound dropped
// ([MaxElidedIdentityBytes]). 199 × 16,384 + 73,728 + 65,536 for the
// identities + 259 for the keys and commas is 3,399,939, inside the share.
//
// THE PARKED STATE is held by the park, which is the one writer of it: past
// [ExecuteStateBytes] the closed rounds' payloads are condensed, and a state
// still past it is refused ([ErrStateTooLarge]) — see
// internal/agent/execstate's EncodeWithin.
//
// THE REST is every other field, each at the bound its own writer holds it to
// (TestTheWholeRowFitsItsBudget builds every one of them at that bound, escaped
// at its worst): the identity, status and time fields; the title a launch
// keeps ([MaxRunTitleBytes], twice) and a colleague's question; the ask and
// its decision; the branch, the handed secrets and the notes; the owed end and
// the captures; the earlier rounds' tool names ([MaxPriorCalls]); and
// [ExtraRowBytes] of room for the keys a LATER BUILD ADDS AT THE TOP OF THE
// ROW, which is where the next field goes. What that leaves is what three
// fields bounded by their SOURCE rather than by this engine can take: a
// recorded answer, the copies of one owed back to the seat, and a held call's
// sealed arguments — a person's reply and a tool's arguments, which a message
// surface and an MCP server bound. The test measures it at 1.7 MiB with every
// other field at its worst, which holds an answer of a whole inbox batch of the
// engine's own messages (chatlet.MaxBody, worklet.MaxCommentBody: 32 KiB a
// message, twenty to a batch); a vendor's message and an MCP tool's arguments
// are the vendor's and the server's to bound.
const (
	// RowBytes is what one run's row may weigh as the coordination store
	// holds it: the broker's payload bound less the room its headers take
	// inside it — a KV update carries its expected-revision header in the
	// same max_payload, about seventy bytes, and four KiB is that with room.
	RowBytes = queue.MaxPayloadBytes - 4<<10

	// BridgeLogBytes is the bridged call log's share: `bridge_calls`,
	// `bridge_calls_elided` and `bridge_elided_calls` together.
	BridgeLogBytes = 3<<20 + 1<<18

	// ExecuteStateBytes is the parked turn's share: the `execute_state` value
	// alone, which the park condenses to fit and refuses past it.
	ExecuteStateBytes = 2 << 20

	// RestBytes is every other field's share, and the room for what a later
	// build adds.
	RestBytes = RowBytes - BridgeLogBytes - ExecuteStateBytes

	// ExtraRowBytes is the room, inside RestBytes, for keys a later build
	// adds at the top of the row ([PendingRun.Extra]) and that this build
	// carries without reading: STATED HERE, so the next field a build adds is
	// added inside it, and the build that adds it moves its own figure out of
	// this one rather than out of a share somebody else's arithmetic needs.
	ExtraRowBytes = 256 << 10
)

// The per-call bounds of the bridged call log. See [MaxBridgeCallBytes].
const (
	// MaxBridgeCallNameBytes bounds a recorded call's tool name, raw. No tool
	// a seat holds is longer — a harness lists tools of at most 64 characters
	// (harnessprofile.ToolNameMax), and an MCP tool's wire name is held to 128
	// (coord.MaxMCPToolNameBytes) — so a longer name is a call to no tool,
	// cut with the cut said, which moves no operation id: no tool derives one
	// from a name it does not have.
	MaxBridgeCallNameBytes = 256

	// MaxBridgeArgsBytes bounds what the row keeps of a call's arguments,
	// as it holds them: a JSON string of the arguments' JSON text, escaped
	// twice over. EIGHT KIB is what a reviewer reads verbatim and the board
	// shows: every identifier a call names (ids, keys, paths, channels, a
	// branch) and a message body of a couple of pages. A file's contents, a
	// document, a commit's megabyte are what is left out, value by value
	// (internal/kept's Args), the identifiers beside them kept.
	MaxBridgeArgsBytes = 8 << 10

	// MaxBridgeOutputBytes bounds what the row keeps of a call's output.
	// FOUR KIB, because the engine reads a FAILED call's output alone (its
	// error, which the ledger and the reviewer show), and a successful one is
	// read by a person on the board, for whom its opening says what it was.
	// Past it the opening is kept and the cut said (internal/kept's Text).
	MaxBridgeOutputBytes = 4 << 10

	// MaxBridgeCallBytes bounds one call as the row holds it, every field at
	// its bound: the name at its worst (256 bytes escaped six-fold, 1,538),
	// the arguments (8,192) and the output (4,096), the digest and every
	// other field (at most 600, measured by TestABridgedCallIsHeldToItsShare),
	// and what is left — 1,958 bytes — the room for keys a LATER BUILD ADDS
	// TO A CALL ([BridgeCall.Extra]), which this build carries unread.
	MaxBridgeCallBytes = 16 << 10

	// MaxSubmissionArgsBytes is what the row keeps of the arguments of the
	// run's NEWEST SUCCESSFUL SUBMISSION — the `submit_work` the collection
	// replays as the round's outcome (internal/agent/runner's
	// replaySubmission), which therefore has to be on the row whole. Every
	// other call's arguments, an earlier submission's included, are held to
	// [MaxBridgeArgsBytes].
	//
	// TWICE [MaxSubmissionBytes], what the live tool accepts: the tool weighs
	// the submission as it reads it, its arguments in their canonical form,
	// and the row keeps them as the harness sent them — a null the model
	// passed, the whitespace round a choice it spelt — so a submission the
	// tool took is never one the row cannot keep whole.
	MaxSubmissionArgsBytes = 64 << 10

	// MaxSubmissionBytes is the most a live `submit_work` may weigh, its
	// canonical arguments as the row would hold them: past it the tool
	// refuses the submission, naming the size, while the run can still
	// shorten it (internal/agent/runner). Half [MaxSubmissionArgsBytes].
	MaxSubmissionBytes = MaxSubmissionArgsBytes / 2

	// MaxSubmissionCallBytes bounds the newest successful submission's call
	// as the row holds it: [MaxBridgeCallBytes] with its arguments' share
	// raised to [MaxSubmissionArgsBytes].
	MaxSubmissionCallBytes = MaxBridgeCallBytes - MaxBridgeArgsBytes + MaxSubmissionArgsBytes

	// MaxElidedIdentityBytes bounds the identities of the calls the log's
	// count bound dropped ([PendingRun.ElidedCalls]), as the row holds them,
	// key included. SIXTY-FOUR KIB is about 450 distinct identities at a
	// typical tool name's length, exact; past it identities fold by tool and
	// then into one count ([ElidedCall]), which costs a resumed call log an
	// over-count and never an under-count.
	MaxElidedIdentityBytes = 64 << 10

	// MaxRunTitleBytes bounds each copy of what a person reads a run by that
	// its row keeps — its task description, its label — and the question of
	// the colleague its turn answers, as the row holds them. A trigger's own
	// words, which no surface bounds below the broker's payload; past it the
	// row keeps their opening with the cut said. A page: what a board shows
	// of a run's ask, and where its turn's own record keeps the whole.
	MaxRunTitleBytes = 16 << 10

	// MaxPriorCalls bounds [PendingRun.PriorCalls]: the tool names a turn's
	// earlier rounds called with success. Five hundred and twelve is past
	// any seat's catalogue a run can call through its harness; a set that
	// would grow past it says its record is incomplete instead
	// ([PendingRun.PriorCallsElided]), the reading every delivery check
	// already takes of a record it cannot see whole.
	MaxPriorCalls = 512
)

// rowRecord is the record a payload this package keeps in part says it is not
// kept on.
const rowRecord kept.Record = "the run's record"

// ErrStateTooLarge is a parked turn's state past its share of the row
// ([ExecuteStateBytes]), refused by [PendingStore.MarkSuspended] rather than by
// the broker: a row that big would refuse every write after it.
var ErrStateTooLarge = errors.New("sandbox: the parked turn's state is past its share of the run's row")

// keptCall is call as the row keeps it, its arguments held to argsBudget and
// every other field to its bound ([MaxBridgeCallBytes]): a name past
// [MaxBridgeCallNameBytes] cut, arguments past their budget kept with their
// largest values left out, an output past [MaxBridgeOutputBytes] kept as its
// opening — each said in the text, and the whole's size recorded beside it
// ([BridgeCall.ArgsBytes], [BridgeCall.OutputBytes]).
//
// IDEMPOTENT, so a call kept once is kept again unchanged, and a call an older
// build recorded whole is kept within its bounds the next time this build
// writes the row. A size already recorded is the whole's and is never replaced
// by the size of what was already kept.
func keptCall(call BridgeCall, argsBudget int) BridgeCall {
	call.Name = textcut.Within(call.Name, MaxBridgeCallNameBytes)
	if args, whole := rowRecord.Args(call.Args, argsBudget); whole > 0 {
		call.Args = args
		if call.ArgsBytes == 0 {
			call.ArgsBytes = whole
		}
	}
	if output, whole := rowRecord.Text(call.Output, MaxBridgeOutputBytes); whole > 0 {
		call.Output = output
		if call.OutputBytes == 0 {
			call.OutputBytes = whole
		}
	}
	return call
}

// submitted reports whether call is a submission the collection could replay:
// a `submit_work` the surface did not refuse.
func submitted(call BridgeCall) bool { return call.Name == SubmitWorkTool && !call.Failed }

// appendCall adds one finished call to run's log and holds the log to its
// bounds: the count ([MaxBridgeCalls], the middle dropped and its identities
// kept), and the bytes, every call at its own share — which is the whole of
// the log's arithmetic ([BridgeLogBytes]).
//
// EVERY CALL IS KEPT AGAIN ON EVERY APPEND, not only the one appended: the
// newest successful submission's larger share passes to a newer one, and the
// one before it is held to the ordinary share; and a call an older build
// recorded whole, in a fleet mid-upgrade, is held to its share here rather
// than carried at whatever size it came. Keeping a call already kept changes
// nothing ([keptCall]).
func (r *PendingRun) appendCall(next BridgeCall) {
	var dropped []BridgeCall
	r.BridgeCalls, r.BridgeCallsElided, dropped = appendBounded(r.BridgeCalls, r.BridgeCallsElided, next)
	for _, call := range dropped {
		r.ElidedCalls = elide(r.ElidedCalls, call)
	}
	newest := -1
	for i, call := range r.BridgeCalls {
		if submitted(call) && (newest < 0 || call.Seq > r.BridgeCalls[newest].Seq) {
			newest = i
		}
	}
	for i := range r.BridgeCalls {
		budget := MaxBridgeArgsBytes
		if i == newest {
			budget = MaxSubmissionArgsBytes
		}
		r.BridgeCalls[i] = keptCall(r.BridgeCalls[i], budget)
	}
}

// ElidedCall is one identity among the calls a run's log dropped from its
// middle ([PendingRun.ElidedCalls]): how many of them called Name with the
// arguments whose digest is Digest ([BridgeCall.Digest]), and how many of those
// failed.
//
// THE COUNT BOUND COSTS EVIDENCE, NEVER AN OPERATION ID. A resumed call log
// seeds a call for every one counted here, so a later call counts its repeats
// (internal/agent/turnctx's CallLog) exactly as the run's own process would
// have, and a delivery reader counts a dropped reply as the reply it was.
//
// HELD TO [MaxElidedIdentityBytes], and past it an identity is FOLDED, in two
// steps that each keep the count and give up a detail: into its tool's entry
// with no digest — calls to Name whose arguments are not known, which a seed
// counts against every later call to Name — and past that into the one entry
// with no name — calls whose tool is not known either, which a seed counts
// against every later call and a delivery reader cannot attribute. Both are
// over-counts; neither is ever an under-count.
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go), so a key a later build adds
// to an identity survives this build's writes. An identity this build folds
// away takes its keys with it: they described an identity the row no longer
// keeps.
type ElidedCall struct {
	Name   string `json:"name,omitempty"`
	Digest string `json:"digest,omitempty"`
	Calls  int    `json:"calls"`
	Failed int    `json:"failed,omitempty"`

	// Extra is every key of the identity this build does not know — see
	// wirekeys.go.
	Extra map[string]json.RawMessage `json:"-"`
}

// elidedKey is what `bridge_elided_calls` costs on the row beyond its entries:
// its key, its brackets and the comma before it.
const elidedKey = len(`,"bridge_elided_calls":[]`)

// elide folds one call the log dropped into its identities, held to
// [MaxElidedIdentityBytes] — see [ElidedCall].
func elide(ids []ElidedCall, call BridgeCall) []ElidedCall {
	failed := 0
	if call.Failed {
		failed = 1
	}
	ids = tally(ids, ElidedCall{Name: call.Name, Digest: call.Digest, Calls: 1, Failed: failed})
	// WHAT THE TALLY WEIGHS: its entries, a comma between each two, and its
	// key — kept as the folds below change it rather than measured again.
	sum := 0
	for _, e := range ids {
		sum += entryBytes(e)
	}
	weight := func() int { return elidedKey + sum + max(len(ids)-1, 0) }
	// PAST THE BOUND: identities fold into their tool's entry, the largest
	// first, until the tally fits; then tools into the one unnamed entry.
	// Each fold only shrinks the tally: an entry goes, and what it counted
	// joins one that weighs no more than it did.
	for _, step := range []func(ElidedCall) (ElidedCall, bool){
		func(e ElidedCall) (ElidedCall, bool) {
			return ElidedCall{Name: e.Name, Calls: e.Calls, Failed: e.Failed}, e.Digest != ""
		},
		func(e ElidedCall) (ElidedCall, bool) {
			return ElidedCall{Calls: e.Calls, Failed: e.Failed}, e.Name != ""
		},
	} {
		if weight() <= MaxElidedIdentityBytes {
			return ids
		}
		type weighed struct {
			e    ElidedCall
			cost int
		}
		order := make([]weighed, len(ids))
		for i, e := range ids {
			order[i] = weighed{e, entryBytes(e)}
		}
		slices.SortStableFunc(order, func(a, b weighed) int { return cmp.Compare(b.cost, a.cost) })
		for _, w := range order {
			if weight() <= MaxElidedIdentityBytes {
				return ids
			}
			folded, foldable := step(w.e)
			if !foldable {
				continue
			}
			ids = slices.DeleteFunc(ids, func(x ElidedCall) bool { return x.Name == w.e.Name && x.Digest == w.e.Digest })
			sum -= w.cost
			if i := slices.IndexFunc(ids, func(x ElidedCall) bool {
				return x.Name == folded.Name && x.Digest == folded.Digest
			}); i >= 0 {
				sum -= entryBytes(ids[i])
			}
			ids = tally(ids, folded)
			i := slices.IndexFunc(ids, func(x ElidedCall) bool { return x.Name == folded.Name && x.Digest == folded.Digest })
			sum += entryBytes(ids[i])
		}
	}
	return ids
}

// tally adds e's counts to the identity it names, or adds it, keeping the
// identities in one order — by name, then digest — so the same calls dropped
// make the same row.
func tally(ids []ElidedCall, e ElidedCall) []ElidedCall {
	for i := range ids {
		if ids[i].Name == e.Name && ids[i].Digest == e.Digest {
			ids[i].Calls += e.Calls
			ids[i].Failed += e.Failed
			return ids
		}
	}
	at, _ := slices.BinarySearchFunc(ids, e, func(a, b ElidedCall) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Digest, b.Digest))
	})
	return slices.Insert(ids, at, e)
}

// elidedBytes is what ids weigh on the row, key included.
func elidedBytes(ids []ElidedCall) int {
	n := elidedKey
	for i, e := range ids {
		if i > 0 {
			n++
		}
		n += entryBytes(e)
	}
	return n
}

// entryBytes is what one identity weighs on the row.
func entryBytes(e ElidedCall) int {
	raw, err := json.Marshal(e)
	if err != nil {
		return 0
	}
	return len(raw)
}

// elidedCount is how many calls the identities account for.
func elidedCount(ids []ElidedCall) int {
	n := 0
	for _, e := range ids {
		n += e.Calls
	}
	return n
}

// ElisionKnown reports whether every call the log dropped from its middle is
// accounted for by name and outcome ([PendingRun.ElidedCalls]): each counted
// in an identity that names its tool. False on a row an older build elided
// (it kept a count and no identities), and where the tally folded calls into
// its unnamed entry and any of those succeeded — a call whose tool is not
// known may have been the reply, so a reader of whether the turn replied
// cannot say it was not.
func (r PendingRun) ElisionKnown() bool {
	if elidedCount(r.ElidedCalls) != r.BridgeCallsElided {
		return false
	}
	for _, e := range r.ElidedCalls {
		if e.Name == "" && e.Calls > e.Failed {
			return false
		}
	}
	return true
}

// keptTitle is a run's title as its row keeps it: within [MaxRunTitleBytes]
// as the row holds it, the cut said.
func keptTitle(s string) string {
	title, _ := rowRecord.Text(s, MaxRunTitleBytes)
	return title
}

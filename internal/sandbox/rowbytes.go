package sandbox

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A run's values in the coordination store are held to a byte budget, and every
// field they carry has a share of it.
//
// # Why there is a budget at all
//
// A run is kept as values in the coordination store ([coord.SandboxRuns]): its
// row ([PendingRun]), read and written whole by every mutation (coordstore.go),
// and beside it the parts a job can make large — the bridged call log
// ([BridgeLog]), read and written whole by every append, and the secrets the
// job's box was handed. One value is one broker message, held to
// [queue.MaxPayloadBytes] with its headers, and past it every write of the
// value is refused at the broker: for the row, a lease stamped on the run, a
// status flip, the hold a turn takes on its next scratch repository; for the
// log, every call's append. The bridged call log grew one call at a time with
// nothing measuring it — two hundred builtin answers of 64 KiB are 12.8 MiB —
// so a long run's log stopped taking writes, and its calls were the first
// thing to go: the resumed turn's ledger, its delivery check and its
// reviewer's evidence missed what the run did.
//
// # The shares, in bytes as each value holds them
//
// Every figure here is bytes AS THE STORE'S ENCODER WRITES THEM (json.Marshal,
// HTML escaping on), escaping counted: a cut is measured by internal/kept, never
// estimated from what was sent, so no six-fold multiplier is spent on text that
// does not escape. Each value of a run may weigh [RowBytes]. The row's is split
// two ways, and the split closes:
//
//	the parked turn's state ......... ExecuteStateBytes   2,097,152  (2 MiB)
//	everything else, and the room
//	  a later build adds fields in .. RestBytes           6,287,360  (6 MiB less 4 KiB)
//	                                                      ---------
//	                                  RowBytes            8,384,512
//
// and the log's two ways, beside it:
//
//	its calls and their tallies ..... BridgeLogBytes      3,473,408  (3.3125 MiB)
//	the turn's earlier rounds' tool
//	  names, the log's own fields,
//	  and the room a later build
//	  adds keys to the log in ....... the rest            4,911,104  (4.68 MiB)
//	                                                      ---------
//	                                  RowBytes            8,384,512
//
// The handed set beside them ([PendingStore.Handed]) is the third value, at
// most [MaxHandedBytes] before it is sealed: a fraction of [RowBytes], which
// TestTheWholeRowFitsItsBudget holds it to as well.
//
// THE LOG'S CALLS are held by construction: [MaxBridgeCalls] calls, one of them
// (the newest successful submission, which the collection replays) at most
// [MaxSubmissionCallBytes] and every other at most [MaxBridgeCallBytes]; the
// identities of the calls the log does not list ([MaxElidedIdentityBytes]) and
// of the calls the run's call log took in from a bridged call's own work
// ([MaxAbsorbedIdentityBytes]). 199 × 16,384 + 73,728 + 2 × 65,536 + 300 for
// the keys, the commas and the count is 3,465,516, inside the share. The rest
// of the log is the turn's earlier rounds' tool names ([MaxPriorCalls]) and the
// log's own fields — its identifiers (its job, its round, the answer it was cut
// for), every kind of change its round's calls committed, each once
// ([BridgeLog.Committed]), and how many of the calls it dropped no save point
// holds ([BridgeLog.LateElided]) — which weigh tens of KiB together at their
// worst (TestTheWholeLogFitsItsBudget): the room left above them is a later
// build's, for keys it adds to the log or to an identity ([BridgeLog.Extra],
// [ElidedCall.Extra]); a key added to a call comes out of the call's own share.
//
// THE PARKED STATE is held by the park, which is the one writer of it: past
// [ExecuteStateBytes] the closed rounds' payloads are condensed, and a state
// still past it is refused ([ErrStateTooLarge]) — see
// internal/agent/execstate's EncodeWithin. A field the turn's frozen context
// gains is added inside this share, which the closed rounds give way to, as
// how the turn's conversation's earlier turns ended was (execstate's
// Frozen.Endings, up to 24,000 bytes as the state holds them, never
// condensed).
//
// THE REST is every other field of the row, each at the bound its own writer
// holds it to (TestTheWholeRowFitsItsBudget builds every one of them at that
// bound, through the code that writes it, escaped at its worst), and the
// shares stated here:
//
//	a recorded answer ............... MaxAnswerBytes            524,288  (512 KiB)
//	the copies one owes back ........ MaxAnswerBytes            524,288  (512 KiB)
//	a held call's sealed arguments .. MaxHeldArgsBytes          262,144  (256 KiB)
//	the turn's read of its
//	  conversation .................. ConversationBaseMaxBytes   24,576  (24 KiB)
//	what a later build adds ......... ExtraRowBytes             262,144  (256 KiB)
//	every other field, measured ..... 1,700,923  (with 2,988,955 to spare)
//
// — the identity, status and time fields; the title a launch keeps
// ([MaxRunTitleBytes], twice) and a colleague's question; a refused reply's
// refusal beside its answer ([PendingRun.AnswerRefusal], a hundred delivery
// ids at most, two figures and where it was posted, [MaxRefusalAddressBytes])
// and the round's refusals kept for its review ([maxRoundRefusals]); the ask and its
// decision, and the seats it is put to ([MaxAudienceHandles]); a held call's
// sealed result and the call a person approved, on the held call and owed to
// the log ([OwedCall]); the branch and the notes ([MaxNoteSealedBytes]); the
// job's own record; the owed end and the captures; and what the job may do on
// Gitlet — the holds of its turn ([MaxCodeHolds]), its
// repositories ([MaxRunRepos], each held to its grammar, its cone and its
// change's title, [MaxChangeTitleBytes], among them) and its floors. Every one
// of them is bounded by THIS ENGINE: a person's reply, a tool's arguments and a
// box's account of its session are each held to their share by the write that
// puts them on the row, which refuses one past it by name ([ErrAnswerTooLarge],
// [ErrHeldCallTooLarge], [ErrCaptureUnbounded]) rather than leave it to the
// broker; and the turn's read of its conversation is held to its line by
// construction, made of a read the executor log bounds field by field
// ([NewConversationBase]), the largest of which a test holds under it.
//
// [ExtraRowBytes] is the room for the keys a LATER BUILD ADDS to the row — at
// its top and in its sections (the job's own record, [LaunchRecord], first
// among them) — which is where the next such field goes. A field that lands is
// moved out of it into a line of its own above, as the turn's read of its
// conversation was ([PendingRun.Base]), so the room stays whole for the field
// after it.
const (
	// RowBytes is what one value of a run may weigh as the coordination store
	// holds it — its row, and each part beside it, the bridged call log
	// first: the broker's payload bound less the room its headers take inside
	// it — a KV update carries its expected-revision header in the same
	// max_payload, about seventy bytes, and four KiB is that with room.
	RowBytes = queue.MaxPayloadBytes - 4<<10

	// BridgeLogBytes is the bridged call log's share of its value: `calls`,
	// `elided`, `elided_calls` and `absorbed_calls` together. The rest of the
	// value — the turn's earlier rounds' tool names, the log's own fields (its
	// identifiers, the kinds of change its calls committed and its count of
	// the dropped calls no save point holds) and the room for a later build's
	// keys — is what [RowBytes] leaves.
	BridgeLogBytes = 3<<20 + 1<<18 + 1<<16

	// ExecuteStateBytes is the parked turn's share of the row: the
	// `execute_state` value alone, which the park condenses to fit and
	// refuses past it.
	ExecuteStateBytes = 2 << 20

	// RestBytes is every other field's share of the row, and the room for
	// what a later build adds.
	RestBytes = RowBytes - ExecuteStateBytes

	// ExtraRowBytes is the room, inside RestBytes, for keys a later build
	// adds to the row, at its top ([PendingRun.Extra]) or in one of its
	// sections ([LaunchRecord.Extra], [HeldCall.Extra], [OwedCall.Extra],
	// [SessionCapture.Extra], [ConversationBase.Extra] and its session's and
	// siblings', [RunSteer.Extra] and its notes'), and that this build
	// carries without reading: STATED HERE, so the next field a
	// build adds is added inside it, and the build that adds it moves its own
	// figure out of this one rather than out of a share somebody else's
	// arithmetic needs. The turn's read of its conversation ([PendingRun.Base])
	// has its own place, at [ConversationBaseMaxBytes], and takes none of this
	// room: all of it is still a later build's.
	ExtraRowBytes = 256 << 10

	// MaxAnswerBytes bounds a recorded answer as the row holds it — its seal
	// and the deliveries it was made of ([RecordedAnswer]) — and, apart, the
	// copies letting go of it owes the seat ([PendingRun.HandBack]): the two
	// can be on the row at once, a new answer recorded while an earlier
	// decline's copies are still being published.
	//
	// HALF A MEBIBYTE is a person's reply many times over: a batch of chat
	// messages at a few KiB each, sealed and carried as the deliveries they
	// are, weighs tens of KiB. What it does not hold is a batch of pages,
	// which a reply that will be the run's next prompt — read again by its
	// harness at every step after — has no business being. A reply past it
	// is not recorded as the answer ([ErrAnswerTooLarge]): its REFUSAL is,
	// in the answer's place — the figures and the deliveries' ids, none of
	// its words, a few KiB at most ([PendingStore.RecordRefusal]) — and the
	// run is resumed told that the reply did not reach it, so it asks again
	// for less or carries on (ADR-0114); nothing of the reply is lost, since
	// its delivery stays on the seat's inbox until the refusal leaves the
	// run. NOT LOWERED for that: a log or a configuration pasted whole, the
	// answer a coding run's question typically gets, is taken whole. An
	// answer by turn is held far under it, at its source and where it is
	// recorded ([MaxAnswerTextBytes]).
	MaxAnswerBytes = 512 << 10

	// MaxRefusalAddressBytes bounds where a refused reply was posted, as the
	// row holds it ([AnswerRefusal.Address]): ids — the transport, the
	// conversation and its thread, the message and its sender — which a
	// Slack thread reply, the widest a shipped surface stamps, keeps in
	// under a KiB. Four KiB keeps every surface's whole with room for one
	// that stamps more, and a reply whose address is past it is refused
	// with none (its resume raises no indicator) rather than a page of
	// metadata carried twice on the row, in the clear and in the answer's
	// seal.
	MaxRefusalAddressBytes = 4 << 10

	// MaxHeldArgsBytes bounds a held call's arguments as the row holds them
	// once sealed ([HeldCall.Arguments]): AEAD-sealed and written as one
	// string, so a byte of the arguments costs a third more, and a byte the
	// JSON encoder escapes six times that.
	//
	// A QUARTER OF A MEBIBYTE, about 190 KiB of arguments in plain text: a
	// call a person reads before they approve it — a message, an issue, a
	// file of ordinary size — and nothing like a commit of a megabyte, which
	// a person is not shown to approve line by line. A call past it is
	// refused before it is held, its size named, so the run splits or
	// shortens it ([ErrHeldCallTooLarge]); held, it never reached its server,
	// so nothing but this bounds it.
	MaxHeldArgsBytes = 256 << 10

	// MaxNoteSealedBytes bounds one person's note as its seal holds it
	// ([RunNote.Sealed], raw: the row writes it as base64, a third more),
	// held by [PendingStore.FileNote]. SIXTEEN KIB: a note is at most two
	// thousand characters (internal/agent/steer's MaxNoteRunes), six bytes
	// each at their most escaped, sealed with who sent it and the frame
	// around it — about 12.5 KiB, which the engine's own test holds a note
	// at its worst to — and a turn takes at most ten of them
	// (steer.MaxTurnNotes), each kept on the row once read.
	MaxNoteSealedBytes = 16 << 10

	// MaxAudienceHandles bounds the seats a parked question is put to
	// ([PendingRun.AudienceHandles]). TWO HUNDRED AND FIFTY-SIX: a `team`
	// audience is every person in the asking seat's unit, which nothing else
	// bounds, and a question put to more people than that is put to nobody in
	// particular. The requester, the manager and a colleague named by the run
	// are one seat each; the FALLBACK, for a label that names nobody the chart
	// has, is the seat's lead chain — its managers up to the top of the chart,
	// or for a seat nobody manages the leads of the units that hold it — as
	// long as the chart is deep, which is far under it. A `team` audience is
	// resolved as the unit's lead first and then its people in the chart's
	// order, and the chain nearest first, so past the bound a unit keeps its
	// lead and its first members, and a chain its nearest leads.
	//
	// WHAT A SEAT PAST IT LOSES is being the question's audience: the list is
	// what the board shows a question's body to, and what puts it in a
	// person's own "waiting on me". A member past the bound with no reach of
	// their own into the asking seat — its lead's, or an admin's, who are
	// shown every question of it — sees the run with its question, its
	// context and its decision withheld, and is not asked. Who may ANSWER is
	// unchanged: an answer is authorized on the seat and its requester, never
	// on this list.
	MaxAudienceHandles = 256
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

	// MaxBridgeArgsBytes bounds what the log keeps of a call's arguments,
	// as it holds them: a JSON string of the arguments' JSON text, escaped
	// twice over. EIGHT KIB is what a reviewer reads verbatim and the board
	// shows: every identifier a call names (ids, keys, paths, channels, a
	// branch) and a message body of a couple of pages. A file's contents, a
	// document, a commit's megabyte are what is left out, value by value
	// (internal/kept's Args), the identifiers beside them kept.
	MaxBridgeArgsBytes = 8 << 10

	// MaxBridgeOutputBytes bounds what the log keeps of a call's output.
	// FOUR KIB, because the engine reads a FAILED call's output alone (its
	// error, which the ledger and the reviewer show), and a successful one is
	// read by a person on the board, for whom its opening says what it was.
	// Past it the opening is kept and the cut said (internal/kept's Text).
	MaxBridgeOutputBytes = 4 << 10

	// MaxBridgeCallBytes bounds one call as the log holds it, every field at
	// its bound: the name at its worst (256 bytes escaped six-fold, 1,538),
	// the arguments (8,192) and the output (4,096), the digest and every
	// other field (at most 700, measured by TestABridgedCallIsHeldToItsShare
	// with both its places at their widest — the place it was admitted at and
	// the one its round's log took it at, [BridgeCall.Appended], whose 31
	// bytes at their widest the 600 this figure was held room for — its late
	// mark set, [BridgeCall.Late], whose 12 bytes it held room for too, the
	// mark that nobody waited for it, [BridgeCall.Abandoned], whose 17 it held
	// room for as well, and every kind of change a call can commit to its
	// turn, [BridgeCall.Committed], each once, whose 61 it held no room for:
	// 654 measured with every one of them, 593 before those kinds; 700 is
	// that worst rounded up to the next hundred, as 600 was for the marks
	// since, which leaves the next small mark room), and what is left — 1,858
	// bytes — the room for keys a LATER BUILD ADDS TO A CALL
	// ([BridgeCall.Extra]), which this build carries unread.
	MaxBridgeCallBytes = 16 << 10

	// MaxSubmissionArgsBytes is what the log keeps of the arguments of the
	// run's NEWEST SUCCESSFUL SUBMISSION — the `submit_work` the collection
	// replays as the round's outcome (internal/agent/runner's
	// replaySubmission), which therefore has to be in the log whole — and
	// NEVER A LATE ONE ([BridgeCall.Late]), which the collection never
	// replays: a job a relaunch replaced made it, and the round's submission is
	// the one the job carrying the round on makes. Every other call's
	// arguments, an earlier submission's and a late one's included, are held
	// to [MaxBridgeArgsBytes].
	//
	// TWICE [MaxSubmissionBytes], what the live tool accepts: the tool weighs
	// the submission as it reads it, its arguments in their canonical form,
	// and the log keeps them as the harness sent them — a null the model
	// passed, the whitespace round a choice it spelt — so a submission the
	// tool took is never one the log cannot keep whole.
	MaxSubmissionArgsBytes = 64 << 10

	// MaxSubmissionBytes is the most a live `submit_work` may weigh, its
	// canonical arguments as the log would hold them: past it the tool
	// refuses the submission, naming the size, while the run can still
	// shorten it (internal/agent/runner). Half [MaxSubmissionArgsBytes].
	MaxSubmissionBytes = MaxSubmissionArgsBytes / 2

	// MaxSubmissionCallBytes bounds the newest successful submission's call
	// as the log holds it: [MaxBridgeCallBytes] with its arguments' share
	// raised to [MaxSubmissionArgsBytes].
	MaxSubmissionCallBytes = MaxBridgeCallBytes - MaxBridgeArgsBytes + MaxSubmissionArgsBytes

	// MaxElidedIdentityBytes bounds the identities of the calls the log does
	// not list ([BridgeLog.ElidedCalls]) — its dropped middle — as the log
	// holds them, key included. SIXTY-FOUR KIB is about 450 distinct
	// identities at a typical tool name's length, exact; past it identities
	// fold by tool and then into one count ([ElidedCall]), which costs a
	// resumed call log an over-count and never an under-count.
	MaxElidedIdentityBytes = 64 << 10

	// MaxAbsorbedIdentityBytes bounds the identities of the calls the run's
	// call log took in from its bridged calls' own work
	// ([BridgeLog.AbsorbedCalls]) — a delegated worker's — as the log holds
	// them, key included: the figure [MaxElidedIdentityBytes] is, for the
	// same tally and the same folds.
	MaxAbsorbedIdentityBytes = 64 << 10

	// MaxRunTitleBytes bounds each copy of what a person reads a run by that
	// its row keeps — its task description, its label — and the question of
	// the colleague its turn answers, as the row holds them. A trigger's own
	// words, which no surface bounds below the broker's payload; past it the
	// row keeps their opening with the cut said. A page: what a board shows
	// of a run's ask, and where its turn's own record keeps the whole.
	MaxRunTitleBytes = 16 << 10

	// MaxPriorCalls bounds [BridgeLog.PriorCalls]: the tool names a turn's
	// earlier rounds called with success. Five hundred and twelve is past
	// any seat's catalogue a run can call through its harness; a set that
	// would grow past it says its record is incomplete instead
	// ([BridgeLog.PriorCallsElided]), the reading every delivery check
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

// ErrAnswerTooLarge is a reply past its share of the row ([MaxAnswerBytes]) as
// the answer it would be recorded as — its seal and its deliveries — or as the
// copies letting it go would owe the seat: refused by
// [PendingStore.RecordAnswer] before anything is written, and only to a run
// the write finds still waiting on the question, as an
// [*AnswerTooLargeError] naming what it weighs. Nothing of the reply is lost
// by it: a chat reply's REFUSAL is recorded as the question's outcome
// ([PendingStore.RecordRefusal]) and resumes the run told so, its delivery
// kept until the refusal leaves the run ([AnswerRefused]), and the refusal is
// announced as `executor_answered` with the outcome too_large
// ([Coordinator.TryResumeFromAnswer], ADR-0114).
var ErrAnswerTooLarge = errors.New("sandbox: the reply is past a recorded answer's share of the run's row")

// AnswerTooLargeError is [ErrAnswerTooLarge] for one reply: what it weighs as
// the row would hold it (the answer, or the copies letting it go would owe,
// whichever is more) and what an answer may weigh there — the two figures the
// refusal records ([AnswerRefusal]), its `executor_answered` record carries,
// and the run is told.
type AnswerTooLargeError struct {
	TurnID string
	Weighs int
	May    int
}

// Error states what the answer weighs ROUNDED UP ([kept.SizeUp]) beside the
// bound in [kept.Size]'s units, which round down: rounded down too, a reply a
// byte past the bound read "weighs 512 KiB ..., and an answer may weigh
// 512 KiB" in the log line that refuses it.
func (e *AnswerTooLargeError) Error() string {
	return fmt.Sprintf("sandbox: the answer to run %s weighs %s on its row, and an answer may weigh %s",
		e.TurnID, kept.SizeUp(e.Weighs), kept.Size(e.May))
}

// Unwrap is [ErrAnswerTooLarge], which every caller tests for.
func (e *AnswerTooLargeError) Unwrap() error { return ErrAnswerTooLarge }

// ErrNoteTooLarge is a note whose seal is past its bound on the row
// ([MaxNoteSealedBytes]), refused by [PendingStore.FileNote] before anything is
// written: the engine bounds a note's words before it seals them, so one past
// it was sealed by nothing of this engine's.
var ErrNoteTooLarge = errors.New("sandbox: a note's seal is past its bound on the run's row")

// ErrHeldCallTooLarge is a held call whose sealed arguments are past their
// share of the row ([MaxHeldArgsBytes]), refused by [PendingStore.RecordAsk]
// before anything is written, as the call is refused before it is held
// (internal/agent/builtin's CallHold): the run is told the size, and splits or
// shortens the call.
var ErrHeldCallTooLarge = errors.New("sandbox: a held call's sealed arguments are past their share of the run's row")

// keptCall is call as the log keeps it, its arguments held to argsBudget and
// every other field to its bound ([MaxBridgeCallBytes]): a name past
// [MaxBridgeCallNameBytes] cut, arguments past their budget kept with their
// largest values left out, an output past [MaxBridgeOutputBytes] kept as its
// opening — each said in the text, and the whole's size recorded beside it
// ([BridgeCall.ArgsBytes], [BridgeCall.OutputBytes]).
//
// AND A CALL HELD FOR A PERSON'S APPROVAL, OR THE ONE THEY APPROVED, KEEPS
// NEITHER ITS ARGUMENTS NOR THEIR DIGEST: the arguments ride the run's row
// sealed on the held call (ADR-0077), and a digest of them in the clear beside
// the tool's name would let anybody who reads the log — or a backup of the
// store — confirm a guess at what the seal hides ([BridgeCall.Withheld]).
// Whoever hands such a call to the log leaves both out; the log holds every
// writer to it, and so does the row, where the call a person approved rides
// its held call and is owed to the log ([approvedCall]).
//
// IDEMPOTENT, so a call kept once is kept again unchanged, and a call an older
// build recorded whole is kept within its bounds the next time this build
// writes the log. A size already recorded is the whole's and is never replaced
// by the size of what was already kept.
func keptCall(call BridgeCall, argsBudget int) BridgeCall {
	call.Name = textcut.Within(call.Name, MaxBridgeCallNameBytes)
	if call.Withheld() {
		call.Args, call.ArgsBytes, call.Digest = "", 0, ""
	}
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

// unheld reports whether no save point holds call, whatever its number: a call
// the log took late ([BridgeCall.Late]), or one its harness stopped waiting for
// before it returned ([BridgeCall.Abandoned]). Each is told to every
// continuation of its round by its mark, so the log's dropped middle keeps it
// while an ordinary call is left there ([appendBounded]) and counts one it gives
// up ([BridgeLog.LateElided]), since a dropped call keeps no mark.
func (c BridgeCall) unheld() bool { return c.Late || c.Abandoned }

// submitted reports whether call is a submission the collection could replay:
// a `submit_work` the surface did not refuse, and the log did not take late
// ([BridgeCall.Late]) — which the collection never takes for the round's
// (internal/agent/runner's sinceAnswer), so a late one given the round's
// whole share would cut the round's own submission to the ordinary one, and
// the collection, refusing a cut submission, would read the round as one that
// submitted nothing.
func submitted(call BridgeCall) bool {
	return call.Name == SubmitWorkTool && !call.Failed && !call.Late
}

// appendCall adds one finished call to the log, with absorbed — the calls the
// run's call log took in from the call's own work — and holds the log to its
// bounds: the count ([MaxBridgeCalls], the middle dropped and its identities
// kept), and the bytes, every call at its own share and each tally at its own —
// which is the whole of the calls' arithmetic ([BridgeLogBytes]).
//
// THE TALLIES ARE TAKEN AS THEY READ before anything is added to them
// ([BridgeLog.Unlisted], [BridgeLog.Absorbed]): calls a build without them
// dropped with no identity are counted as calls whose tool is not known. So
// the log this write leaves always holds tallies that account for every call
// its count says it does not list.
//
// EVERY CALL IS KEPT AGAIN ON EVERY APPEND, not only the one appended: the
// newest successful submission's larger share passes to a newer one, and the
// one before it is held to the ordinary share; and a call an older build
// recorded whole, in a fleet mid-upgrade, is held to its share here rather
// than carried at whatever size it came. Keeping a call already kept changes
// nothing ([keptCall]).
//
// THE NEWEST AS THE COLLECTION READS IT, which replays it: of the submissions
// it could replay ([submitted], never a late one), the one placed last in the
// order the bridge admitted them ([BridgeCall.Seq]), and of two at one place —
// a session opened again on the job while a call of the closed one was still
// in flight reuses its place — the one the log took last, as the collection's
// stable sort by place leaves it. A share kept on any other leaves the one the
// collection replays cut, which it refuses, and the round reads as one that
// submitted nothing.
//
// AND THE CALL IS NUMBERED AS THE ROUND'S NEXT APPEND ([BridgeCall.Appended]),
// here and nowhere else, before the bound drops anything: this is the one
// writer of what the log lists and of what it counts without listing, so the
// count the number is taken from ([BridgeLog.Appends]) rises by exactly one an
// append, whichever of them the call or the one the bound drops for it lands
// in. Whatever number a caller handed in is the caller's guess, and is
// replaced.
//
// IN PLACE ON l's OWN CALLS: a caller holding a log it shares with another —
// a copy whose list is another's too — clones the list first.
func (l *BridgeLog) appendCall(next BridgeCall, absorbed []ElidedCall) {
	l.Committed = committedOnce(l.Committed, next.Committed)
	dropped, taken := l.Unlisted(), l.Absorbed()
	next.Appended = l.Appends() + 1
	var elided []BridgeCall
	l.Calls, l.Elided, elided = appendBounded(l.Calls, l.Elided, next)
	for _, call := range elided {
		dropped = fold(dropped, identity(call), elidedTally)
		// AND ONE NO SAVE POINT HOLDS COUNTED AS SUCH, its mark gone with
		// the call ([BridgeLog.LateElided]).
		if call.unheld() {
			l.LateElided++
		}
	}
	for _, e := range absorbed {
		if e.Calls <= 0 {
			continue
		}
		taken = fold(taken, ElidedCall{Name: e.Name, Digest: e.Digest, Calls: e.Calls}, absorbedTally)
	}
	l.ElidedCalls, l.AbsorbedCalls = dropped, taken
	newest := -1
	for i, call := range l.Calls {
		if submitted(call) && (newest < 0 || call.Seq >= l.Calls[newest].Seq) {
			newest = i
		}
	}
	for i := range l.Calls {
		budget := MaxBridgeArgsBytes
		if i == newest {
			budget = MaxSubmissionArgsBytes
		}
		l.Calls[i] = keptCall(l.Calls[i], budget)
	}
}

// lists reports whether the log holds call already: the same place
// ([BridgeCall.Seq]), the same instant the bridge handed it to the surface,
// and the same name and identity, all as the log keeps them ([keptCall]).
//
// ALL FOUR, because each alone is some other call's. A place alone could be a
// call numbered by a session opened again on the job while one of the closed
// session's calls was still in flight, which reuses it; an instant alone is
// not set on a call nothing ran; and the same tool called with other arguments
// at the same place and instant is another call, as is another tool. A call
// with no place is never one the log holds: the bridge numbers every call it
// records.
func (l BridgeLog) lists(call BridgeCall) bool {
	if call.Seq <= 0 {
		return false
	}
	kept := keptCall(call, MaxBridgeArgsBytes)
	return slices.ContainsFunc(l.Calls, func(c BridgeCall) bool {
		return c.Seq == kept.Seq && c.StartedAt.Equal(kept.StartedAt) && c.Name == kept.Name &&
			c.Digest == kept.Digest
	})
}

// identity is one call as a tally counts it: its tool, the digest it was
// admitted under — none for a call whose arguments are withheld, so its tally
// keeps no digest of them either — and whether it failed.
func identity(call BridgeCall) ElidedCall {
	e := ElidedCall{Name: call.Name, Digest: call.Digest, Calls: 1}
	if call.Withheld() {
		e.Digest = ""
	}
	if call.Failed {
		e.Failed = 1
	}
	return e
}

// ElidedCall is one identity in a tally of calls a run's log does not list as
// calls: how many of them called Name with the arguments whose digest is
// Digest ([BridgeCall.Digest]), and how many of those failed. Two tallies are
// kept of them, each the round's, as the log they are kept in is:
//
//   - [BridgeLog.ElidedCalls], calls of the round the log does not list —
//     its dropped middle — which every reader of the round reads as calls it
//     made;
//   - [BridgeLog.AbsorbedCalls], calls the run's call log took in from a
//     bridged call's own work — a delegated worker's — which a resumed call
//     log counts and no reader of the round's deliveries reads, since the
//     round did not make them. Failed is never set on one.
//
// THE COUNT BOUND COSTS EVIDENCE, NEVER AN OPERATION ID. A resumed call log
// seeds a call for every one counted here, so a later call counts its repeats
// (internal/agent/turnctx's CallLog) exactly as the run's own process would
// have, and a delivery reader counts a dropped reply as the reply it was.
//
// HELD TO ITS TALLY'S BOUND ([MaxElidedIdentityBytes],
// [MaxAbsorbedIdentityBytes]), and past it an identity is FOLDED, in two steps
// that each keep the count and give up a detail: into its tool's entry with no
// digest — calls to Name whose arguments are not known, which a seed counts
// against every later call to Name — and past that into the one entry with no
// name — calls whose tool is not known either, which a seed counts against
// every later call and a delivery reader cannot attribute. Both are
// over-counts; neither is ever an under-count. An entry with a name and no
// digest is also how a call whose arguments are withheld is counted
// ([BridgeCall.Withheld]).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go), so a key a later build adds
// to an identity survives this build's writes. An identity this build folds
// away takes its keys with it: they described an identity the log no longer
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

// tallyKind is one of the log's two tallies of identities: what it costs in
// the log beyond its entries (key: its key, its brackets and the comma before
// it) and the bound it is held to, as the log holds it.
type tallyKind struct{ key, bound int }

// The log's two tallies — see [ElidedCall].
var (
	elidedTally   = tallyKind{key: len(`,"elided_calls":[]`), bound: MaxElidedIdentityBytes}
	absorbedTally = tallyKind{key: len(`,"absorbed_calls":[]`), bound: MaxAbsorbedIdentityBytes}
)

// fold counts e into ids, held to its kind's bound as the log holds the tally —
// see [ElidedCall].
func fold(ids []ElidedCall, e ElidedCall, kind tallyKind) []ElidedCall {
	key, bound := kind.key, kind.bound
	ids = tally(ids, e)
	// WHAT THE TALLY WEIGHS: its entries, a comma between each two, and its
	// key — kept as the folds below change it rather than measured again.
	sum := 0
	for _, e := range ids {
		sum += entryBytes(e)
	}
	weight := func() int { return key + sum + max(len(ids)-1, 0) }
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
		if weight() <= bound {
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
			if weight() <= bound {
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
// make the same log.
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

// tallyBytes is what ids weigh in the log as a tally of kind.
func tallyBytes(ids []ElidedCall, kind tallyKind) int {
	n := kind.key
	for i, e := range ids {
		if i > 0 {
			n++
		}
		n += entryBytes(e)
	}
	return n
}

// entryBytes is what one identity weighs in the log.
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

// Unlisted is every call of this round its log does not list, by identity: the
// tally kept of them ([BridgeLog.ElidedCalls]), and one count with no tool for
// every call the log says it does not list and no identity accounts for —
// dropped by a build that kept a count and no identities — whose outcomes are
// not known either. What every reader of the round's calls reads them by, the
// seed and the delivery checks alike: the count is an over-count of every
// later call where it is seeded, and a record a delivery reader cannot read
// whole ([BridgeLog.ElisionKnown]).
func (l BridgeLog) Unlisted() []ElidedCall {
	ids := slices.Clone(l.ElidedCalls)
	if short := l.Elided - elidedCount(ids); short > 0 {
		ids = tally(ids, ElidedCall{Calls: short})
	}
	return ids
}

// Absorbed is the calls the run's call log took in from this round's bridged
// calls' own work, by identity ([BridgeLog.AbsorbedCalls]).
func (l BridgeLog) Absorbed() []ElidedCall { return slices.Clone(l.AbsorbedCalls) }

// ElisionKnown reports whether every call of this round its log does not list
// is accounted for by name and outcome ([BridgeLog.Unlisted]): each counted in
// an identity that names its tool. False on a log a build without identities
// elided (it kept a count and no identities), and where the tally folded calls
// into its unnamed entry and any of those succeeded — a call whose tool is not
// known may have been the reply, so a reader of whether the turn replied
// cannot say it was not.
func (l BridgeLog) ElisionKnown() bool {
	for _, e := range l.Unlisted() {
		if e.Name == "" && e.Calls > e.Failed {
			return false
		}
	}
	return true
}

// widestInstant is an instant at the widest the row's encoder writes one: nine
// digits of fraction and a zone of its own, never `Z`.
var widestInstant = time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.FixedZone("", 14*3600))

// keptTitle is a run's title as its row keeps it: within [MaxRunTitleBytes]
// as the row holds it, the cut said.
func keptTitle(s string) string {
	title, _ := rowRecord.Text(s, MaxRunTitleBytes)
	return title
}

// answerFits reports why answer is past its share of the row
// ([MaxAnswerBytes]), nil where it is within it: as the row holds it, and as
// the copies letting it go of would owe the seat ([handBackOf]), which the
// share bounds as well, since a decline cannot refuse to hand a reply back.
//
// AS THE ROW WILL HOLD IT, not as it is handed over: the record and the steps
// after it add the status it took the run out of ([RecordedAnswer.From]), and
// the claims that take it or lose it ([RecordedAnswer.TakenAt],
// [RecordedAnswer.LostClaims], [RecordedAnswer.FirstLostAt]), each measured
// here at its widest, so an answer that fits its share now still fits it
// once every one of them is written.
func answerFits(turnID string, answer RecordedAnswer) error {
	answer.From = slices.MaxFunc(Awaiting, func(a, b string) int { return cmp.Compare(len(a), len(b)) })
	answer.TakenAt, answer.FirstLostAt, answer.LostClaims = widestInstant, widestInstant, math.MaxInt
	raw, err := json.Marshal(answer)
	if err != nil {
		return fmt.Errorf("sandbox: encoding the answer to run %s: %w", turnID, err)
	}
	copies, err := json.Marshal(handBackOf(answer))
	if err != nil {
		return fmt.Errorf("sandbox: encoding what letting go of the answer to run %s would owe: %w",
			turnID, err)
	}
	if n := max(len(raw), len(copies)); n > MaxAnswerBytes {
		return &AnswerTooLargeError{TurnID: turnID, Weighs: n, May: MaxAnswerBytes}
	}
	return nil
}

// addressBytes is what an address weighs as the row's encoder writes it.
func addressBytes(address map[string]string) int {
	if len(address) == 0 {
		return 0
	}
	n := 2 // the braces
	for k, v := range address {
		n += kept.Quoted(k) + 1 + kept.Quoted(v) + 1 // the colon, and the comma
	}
	return n
}

// HeldArgsFit reports why held call arguments sealed as sealed are past their
// share of the row ([MaxHeldArgsBytes]), nil where they are within it: what the
// holder asks before it records the question, and the store again as it writes
// it. The figure past the bound is stated rounded up ([kept.SizeUp]), so
// arguments a byte past it never read as the bound itself.
func HeldArgsFit(sealed string) error {
	if n := kept.Quoted(sealed); n > MaxHeldArgsBytes {
		return fmt.Errorf("sandbox: the call's arguments weigh %s once sealed, and a held call's may "+
			"weigh %s: %w", kept.SizeUp(n), kept.Size(MaxHeldArgsBytes), ErrHeldCallTooLarge)
	}
	return nil
}

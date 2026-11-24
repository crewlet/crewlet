package sandbox

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A run's values in the coordination store ([coord.SandboxRuns]) are held to a
// byte budget, [RowBytes] each, under [coord.MaxValueBytes]; past it every
// write is refused ([coord.ErrValueTooLarge]). Every field has a share, in
// bytes as the store's encoder writes them: the row splits into
// [ExecuteStateBytes] and [RestBytes], the log into [BridgeLogBytes],
// [InFlightBytes] and [LogRestBytes], and every other field is held by its
// writer, room for a later build's keys in [ExtraRowBytes].
// TestTheWholeRowFitsItsBudget and TestTheWholeLogFitsItsBudget hold the model.
// See the package doc, "A run's byte budget".
const (
	// RowBytes is what one value of a run may weigh as the coordination store
	// holds it — its row, and each part beside it, the bridged call log
	// first: what one key holds, the store's own bound on a value, which
	// every backend refuses a write past ([coord.MaxValueBytes]). The
	// transport's ceiling is not it: a value that large has a leader's read
	// a leaf's link closes on, so the store holds a value to the largest whose
	// read crosses, and a share budgeted to the transport would be one the
	// store refuses every write of.
	RowBytes = coord.MaxValueBytes

	// BridgeLogBytes is the bridged call log's share of its value: `calls`,
	// `elided`, `elided_calls` and `absorbed_calls` together. The rest of the
	// value — the turn's earlier rounds' tool names, the log's own fields (its
	// identifiers, the kinds of change its calls committed, the records they
	// wrote and its count of the dropped calls no save point holds) and the
	// room for a later build's keys — is what [RowBytes] leaves.
	BridgeLogBytes = 3<<20 + 1<<18 + 1<<16

	// ExecuteStateBytes is the parked turn's share of the row: the
	// `execute_state` value alone, which the park condenses to fit and
	// refuses past it.
	ExecuteStateBytes = 2 << 20

	// RestBytes is every other field's share of the row, and the room for
	// what a later build adds.
	RestBytes = RowBytes - ExecuteStateBytes

	// ExtraRowBytes is the room, inside RestBytes, for keys a later build adds to
	// the row, at its top ([PendingRun.Extra]) or in one of its sections
	// ([LaunchRecord.Extra], [HeldCall.Extra], [OwedCall.Extra],
	// [SessionCapture.Extra] in each capture section, [ConversationBase.Extra] and
	// its session's and siblings', [RunSteer.Extra] and its notes', their hooks'
	// serves', the calls those answered, an item's late call or wakes and the
	// notes' tellings), and that this build carries without reading: STATED HERE,
	// so the next field a build adds is added inside it, and the build that adds
	// it moves its own figure out of this one, as the turn's wake items, the
	// job's fold-ins and what the turn wrote took theirs ([wakeRowBytes],
	// [foldInRowBytes], [wroteRowBytes]). The turn's read of its conversation
	// ([PendingRun.Base]) has its own place, at [ConversationBaseMaxBytes].
	ExtraRowBytes = 256<<10 - wakeRowBytes - foldInRowBytes - wroteRowBytes

	// wakeRowBytes is the turn's wake items' figure, moved out of the room a
	// later build adds keys in: [MaxTurnWakes] items at their bound, a hundred
	// KiB, which the 102,064 bytes TestTheWholeRowFitsItsBudget weighs them at
	// leave a little over.
	wakeRowBytes = 100 << 10

	// foldInRowBytes is the job's fold-ins' figure, moved out of the room a
	// later build adds keys in: [MaxJobFoldIns] at [MaxFoldInBytes] each.
	foldInRowBytes = MaxJobFoldIns * MaxFoldInBytes

	// wroteRowBytes is the figure of the keys of what the turn's calls wrote
	// ([PendingRun.WroteKeys]), moved out of the room a later build adds keys
	// in: [types.MaxWrittenRecords] keys of [writtenKeyBytes] in hex, written
	// as themselves, 4,495 bytes with the field's name, which 4.5 KiB leaves a
	// little over (TestTheWholeRowFitsItsBudget weighs them).
	wroteRowBytes = 4<<10 + 512

	// MaxAnswerBytes bounds a recorded answer as the row holds it (its seal
	// and deliveries, [RecordedAnswer]) and, apart, the copies letting go of
	// it owes the seat ([PendingRun.HandBack]): both can be on the row at
	// once. HALF A MEBIBYTE is a person's reply many times over, not a batch
	// of pages, which a reply that becomes the run's next prompt has no
	// business being. A reply past it is not recorded ([ErrAnswerTooLarge]):
	// its REFUSAL is, ids and figures only ([PendingStore.RecordRefusal]),
	// and the run is told the reply did not reach it (ADR-0114); the delivery
	// stays on the inbox until then. NOT LOWERED: a pasted log or
	// configuration is taken whole. An answer by turn is held far under it
	// ([MaxAnswerTextBytes]).
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

	// MaxNoteServes bounds the serves of one note, or a late call's item, to one
	// job's hooks ([RunNote.Serves]), held by [PendingStore.ServeHook]: a first
	// serve and two after losses. Three is a judgment value no run of losses
	// has measured. A serve is lost when its harness gives the hook's call up,
	// past the hook's ten seconds (internal/sandbox/executor's
	// ClaudeHookTimeout); one more serve rides out a lost answer, a second a
	// stall across two boundaries, and each more costs another ten-second
	// stall. A serve answered nothing is removed ([HookServe.Unanswered]),
	// spending none. Past it the note waits for the review or the next brief.
	// See the package doc, "A turn's notes".
	MaxNoteServes = 3

	// MaxNoteCallBytes bounds each half of the call a hook's serve answered
	// ([NoteCall]): its MCP session's id and its JSON-RPC id, raw, a string
	// id's quotes counted. SIXTY-FOUR BYTES: the session is the bridge's own
	// (go-sdk mints it as rand.Text, 26 characters) and the id a harness's
	// counter, so an ordinary call is a fraction of it. A call past it is kept
	// as none, never cut, so a cut call can never match another cut alike: its
	// serve is then settled only by the job's own session. And so is a call
	// outside the id charset ([keptCallOf]), so a kept half is written as
	// itself, between the two quotes every string takes, and a string id's own
	// two quotes at two bytes each.
	MaxNoteCallBytes = 64

	// MaxJobItems bounds the items for one job a run's row holds
	// ([RunNote.For]), apart from a person's notes: the bound the store's own
	// write that files one holds ([CoordStore.fileLateItems]), since no filer
	// files one ([PendingStore.FileNote] refuses it, [ErrNoteKind]). TEN: an
	// item is a late call for the job alone ([RunNote.LateCall]), such as a
	// call its predecessor still ran when it was replaced, ordinarily one or a
	// few, and Claude Code runs at most ten tool calls at once by default
	// (CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY, which nothing in this tree sets);
	// past it a call is told by every continuation instead. Ten at their
	// widest copy ([itemCall], within [MaxBridgeCallBytes]), each served
	// [MaxNoteServes] times and read, are under a quarter of a mebibyte of the
	// row, which TestTheWholeRowFitsItsBudget holds.
	MaxJobItems = 10

	// MaxCallTellings bounds the late calls a run's row records its jobs'
	// sessions took ([RunSteer.Tellings]). TWENTY, A JUDGMENT anchored to the
	// tellings of the two newest jobs that took items, [MaxJobItems] each: a
	// continuation reads the newest capture's lineage, whose jobs are those.
	// An older telling dropped past it is a call told again, under words that
	// say an earlier message may have told it, the safe direction. Twenty at
	// their widest are about 3 KiB of the row, which
	// TestTheWholeRowFitsItsBudget holds.
	MaxCallTellings = 2 * MaxJobItems

	// MaxJobWakes bounds the wake items one job holds ([RunNote.Wake]), apart
	// from a person's notes and late calls, held by [PendingStore.FileWake].
	// FOUR, the most beside the notes' and items' serves that one read of a
	// job's session can ask about: its hook record's masks are sixty-four takes
	// wide (internal/sandbox/executor's maxAskedTakes, a compile-time check),
	// and twenty notes and items at [MaxNoteServes] ask sixty. Wakes between
	// two steps join one item ([MaxItemWakes]), so four items carry a burst.
	MaxJobWakes = 4

	// MaxTurnWakes bounds the wake items the row holds for the whole turn: a
	// wake item read is the turn's, kept by every relaunch since its delivery
	// is worked ([openNotes]). EIGHT, two jobs' worth: past it a wake runs as a
	// turn of its own, the safe default.
	MaxTurnWakes = 2 * MaxJobWakes

	// MaxWakeServes bounds the serves of one wake item to one job's hooks: ONE.
	// A serve its harness lost leaves the wake unread, and an unread wake runs
	// as a turn of its own once the run is over, so a second serve would buy a
	// ten-second stall for what is never lost.
	MaxWakeServes = 1

	// MaxItemWakes bounds the wakes one item carries ([WakeItem.Wakes]): FOUR,
	// a burst between two steps, which one hook answer can show beside a
	// person's widest note only as the newest few anyway (internal/engine's
	// wakeItemMessage names how many it leaves out).
	MaxItemWakes = 4

	// MaxWakeItemBytes bounds one wake item's seals together, and so each
	// wake's ([SealedWake.Sealed], raw: the row writes base64, a third more).
	// EIGHT KIB: a wake's words are at most a thousand characters
	// (internal/agent/steer's MaxWakeRunes), six bytes each at their most
	// escaped, sealed with who wrote them, where and the ledger keys of their
	// delivery. [MaxTurnWakes] items at it fit [wakeRowBytes] of the row.
	MaxWakeItemBytes = 8 << 10

	// MaxJobFoldIns bounds the fold-ins one job asks ([PendingRun.FoldIns]),
	// answered or not: FOUR, [MaxJobWakes]. Each puts a request in front of a
	// person who decides it by hand while the run works, so four other
	// requests behind one job is already a queue that belongs in turns of its
	// own, which is where a fifth waits; and four at [MaxFoldInBytes] is
	// sixteen KiB of a row read and written whole on every flip.
	MaxJobFoldIns = MaxJobWakes

	// MaxFoldInBytes bounds one fold-in as the row writes it ([FoldIn]): FOUR
	// KIB, an excerpt of the asker's words an approver reads beside who asked,
	// where and whom it is put to. The words are display alone (the delivery
	// on the inbox is what joins), so the engine cuts them to fit and says how
	// much more is where they were written ([FoldIn.Cut]).
	MaxFoldInBytes = 4 << 10

	// MaxFoldInPutTo bounds the seats a fold-in is put to ([FoldIn.PutTo]):
	// EIGHT, the run's requester and the seven nearest of the seat's leads. A
	// lead further up still answers it; a question put to the whole top of a
	// deep chart is put to nobody in particular.
	MaxFoldInPutTo = 8

	// MaxWakeIDBytes bounds a wake's id ([SealedWake.ID]), of the id charset
	// so it is written as itself: SIXTY-FOUR BYTES, past the delivery's UUID
	// its filer names it by.
	MaxWakeIDBytes = 64

	// MaxAudienceHandles bounds the seats a parked question is put to
	// ([PendingRun.AudienceHandles]). TWO HUNDRED AND FIFTY-SIX: a `team`
	// audience is every person in the asking seat's unit, which nothing else
	// bounds, and a question put to more people than that is put to nobody in
	// particular. A `team` is resolved lead first, then its people in the
	// chart's order, so past the bound a unit keeps its lead and first
	// members. WHAT A SEAT PAST IT LOSES is being the audience: the list is
	// who the board shows the question's body to and whose "waiting on me"
	// it enters; a member past it with no reach of their own (the seat's
	// lead, an admin) sees the question withheld. Who may ANSWER is unchanged:
	// authorized on the seat and its requester, never on this list.
	MaxAudienceHandles = 256
)

// The two figures the table above measures rather than shares out: what every
// other field of the row weighs at its worst, and what the rest's share leaves
// to spare beside it and the room a later build adds fields in, each as
// TestTheWholeRowFitsItsBudget measures its model of the worst row.
//
// PINNED, so they cannot go stale: that test compares both with what the model
// weighs, exactly, and fails until a change that moves either (a field the row
// gains, above all) restates both here and in the table. The figure the table
// stated before them was read by nothing, and stayed what it was while the row
// grew.
const (
	measuredOtherFields = 2_364_388
	measuredSpare       = 354_716
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

	// MaxBridgeCallIDBytes bounds the id a call's harness gave it, as the log
	// keeps it ([BridgeCall.CallID], [ValidCallID]): SIXTY-FOUR BYTES of
	// letters, digits, `-` and `_`, each written as itself, so the cost is
	// its length, measured at the bound by the shares' own tests. A JUDGMENT
	// CALL, cheap to get wrong: Claude Code's ids are `toolu_` and a random
	// tail, thirty bytes (internal/e2e's claudeID), and an id past the bound
	// costs its call no more than having none: it is kept as none, never cut,
	// and read by its place, never matched wrongly. Over twice the stand-in's,
	// and within the share every call and entry already had.
	MaxBridgeCallIDBytes = 64

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
	// the arguments (8,192), the output (4,096), the digest and every other
	// field (at most 800, as TestABridgedCallIsHeldToItsShare measures with
	// every field at its widest: both places, [BridgeCall.Appended], the late
	// and abandoned marks, every kind of change in [BridgeCall.Committed],
	// and the harness's id, [BridgeCall.CallID]; 731 measured, rounded up to
	// the next hundred so the next small mark has room), and what is left,
	// 1,758 bytes, for keys a LATER BUILD ADDS TO A CALL ([BridgeCall.Extra]),
	// which this build carries unread.
	MaxBridgeCallBytes = 16 << 10

	// MaxSubmissionArgsBytes is what the log keeps of the arguments of the
	// run's NEWEST SUCCESSFUL SUBMISSION, the `submit_work` the collection
	// replays as the round's outcome (internal/agent/runner's
	// replaySubmission), so it must be in the log whole, and NEVER A LATE ONE
	// ([BridgeCall.Late]), which is never replayed. Every other call's
	// arguments are held to [MaxBridgeArgsBytes].
	//
	// TWICE [MaxSubmissionBytes], what the live tool accepts: the tool weighs
	// canonical arguments while the log keeps them as the harness sent them
	// (a passed null, whitespace), so a submission the tool took is never one
	// the log cannot keep whole.
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

	// MaxCallsInFlight bounds the calls of a round a run's log holds IN FLIGHT
	// at once ([BridgeLog.InFlight]): begun, and not yet finished. TWO HUNDRED
	// AND FIFTY-SIX, TIED TO THE LOG'S BYTE BUDGET: that many entries, each at
	// its widest ([MaxCallInFlightBytes]), are a line of their own in the
	// log's arithmetic ([InFlightBytes], one MiB of the room the log leaves
	// beside its calls), which TestTheWholeLogFitsItsBudget measures with
	// every one of them in the worst log. Past it a call is not begun, so not
	// run: its harness is told it could not be recorded, failed, and may make
	// it again ([ErrCallsInFlight]). No figure in this tree bounds how many
	// calls a run makes at once; this bounds what its log holds of them.
	MaxCallsInFlight = 256

	// MaxCallInFlightBytes bounds one entry in flight as the log holds it
	// ([keptEntry]): its identity — its name at its worst (256 bytes escaped
	// six-fold, 1,538 as the encoder writes it), its digest, its place, the
	// instants the log and the bridge took it at, its bound
	// ([BridgeCall.Until]) and the incarnation that runs it
	// ([BridgeCall.Runner], held to [maxIncarnationBytes], 770 at its most
	// escaped) — and the id its harness gave it ([BridgeCall.CallID], held to
	// [MaxBridgeCallIDBytes]): 2,616 bytes at their widest, 2,539 without that
	// id, measured by TestACallInFlightIsHeldToItsShare. FOUR KIB is that
	// rounded up to the next power of two: what is left is the room for the
	// keys a later build adds to an entry, which this build carries unread
	// ([BridgeCall.Extra]).
	MaxCallInFlightBytes = 4 << 10

	// InFlightBytes is the log's entries in flight at their most:
	// [MaxCallsInFlight] of [MaxCallInFlightBytes] each, taken out of the room
	// the log leaves beside its calls.
	InFlightBytes = MaxCallsInFlight * MaxCallInFlightBytes

	// LogRestBytes is the rest of the log's value beside its calls and its
	// calls in flight: the turn's earlier rounds' tool names, the log's own
	// fields (the records the turn's calls wrote among them, [BridgeLog.Wrote]:
	// at most 128 ids of at most 256 bytes, about 200 KiB at their most
	// escaped), and the room a later build adds keys to the log in. STATED, NOT
	// DERIVED, as the table above states it, so the log's split closes only
	// while every line of it is the table's: a change of [BridgeLogBytes] or of
	// [InFlightBytes] — [MaxCallsInFlight] raised, say — fails
	// TestACallInFlightIsHeldToItsShare until this figure, and the table and
	// docs/concepts/code-sandbox.md that state it, move with it.
	LogRestBytes = 1_768_362

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

// RowBudget is the bound of a change of a run's row whose caller bounds it for
// a reason of its own (a slow store, an election, a teardown's grace): that
// reason plus the time a value at its bound ([RowBytes]) takes to cross this
// node's connection to the store ([PendingStore.RowCrossing]).
//
// Every change reads and writes the row whole, up to six megabytes, over a
// link allowed as little as a mebibyte in thirty seconds, so held to the
// reason alone a row was cut mid-crossing and read as a store that did not
// answer. Counted at the row's bound, not the row being changed: the store
// decides after the deadline is set, and a row grows. A budget read by
// ANOTHER node ([SessionCaptureBudget], [BoxReadBudget]) counts
// coord.MaxValueCrossing; a stopping node gets no crossing.
func RowBudget(store PendingStore, reason time.Duration) time.Duration {
	return reason + store.RowCrossing()
}

// RowReadBudget is [RowBudget] for a READ of a run's row: the caller's reason
// plus the time the store's reply with the row at its bound takes to cross this
// node's connection ([PendingStore.RowReadCrossing]), what the store itself
// gives one read of a run's key beyond the thirty seconds of silence it keeps
// (docs/concepts/coordination.md). Never a change's crossing: a read sends no
// write, and three sends of one are nine minutes at the slowest pace. A budget
// read by another node counts coord.MaxValueReadCrossing ([captureReadBudget]).
func RowReadBudget(store PendingStore, reason time.Duration) time.Duration {
	return reason + store.RowReadCrossing()
}

// ErrStateTooLarge is a parked turn's state past its share of the row
// ([ExecuteStateBytes]), refused by [PendingStore.MarkSuspended] rather than by
// the store: a row that big would refuse every write after it.
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

// keptCall is call as the log keeps it, arguments held to argsBudget and every
// other field to its bound ([MaxBridgeCallBytes]): a name, arguments and an
// output past theirs are cut with the cut said and the whole's size recorded
// ([BridgeCall.ArgsBytes], [BridgeCall.OutputBytes]). A CALL HELD FOR
// APPROVAL, OR THE ONE APPROVED, KEEPS NEITHER ITS ARGUMENTS NOR THEIR
// DIGEST: the arguments ride the row sealed (ADR-0077), and a digest
// in the clear would let a log reader confirm a guess at what the seal hides
// ([BridgeCall.Withheld], [approvedCall]). The harness's id is kept WHOLE OR
// NOT AT ALL ([keptCallID]); a finished call keeps NOTHING THAT WAITS FOR IT
// ([BridgeCall.Runner], [BridgeCall.Until]), held here whatever handed it,
// since only an entry's runner is bounded ([keptEntry]). IDEMPOTENT: a size
// already recorded is the whole's, never replaced.
func keptCall(call BridgeCall, argsBudget int) BridgeCall {
	call.Name = textcut.Within(call.Name, MaxBridgeCallNameBytes)
	call.CallID = keptCallID(call.CallID)
	call.Runner, call.Until = "", time.Time{}
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

// OutputOpening is the opening the log kept of a call's output it cut, read
// back off output, the text it keeps ([BridgeCall.Output]), and outputBytes,
// the whole's size beside it ([BridgeCall.OutputBytes]); false where output is
// no such cut. RAW, as the call's harness was handed it, which a holdings
// reading compares with its session's copy ([TextKeyOf]): a reader that
// redacts it reads only as far as its redaction settles.
func OutputOpening(output string, outputBytes int) (opening string, ok bool) {
	return rowRecord.Opening(output, outputBytes)
}

// RedactedOpening is what a reader may show of a call's output the log cut
// (output and outputBytes, as [OutputOpening] reads them): the redaction of
// the opening's SETTLED lines alone ([redact.Settled]), which is the opening
// of the whole's redaction, since a credential the log's cut ran across left
// a head no rule knows. whole is the size it is the opening of: its own bytes
// and the bytes the log measured past those lines, unread and so unredacted,
// at least one. A text the log's marker does not read back off is settled as
// it stands.
func RedactedOpening(output string, outputBytes int) (shown string, whole int) {
	opening, ok := OutputOpening(output, outputBytes)
	if !ok {
		opening = output
	}
	settled := opening[:redact.Settled(opening)]
	shown = redact.Secrets(settled)
	return shown, len(shown) + max(outputBytes-len(settled), 1)
}

// ShownOutput is a call's output as the log keeps it (output, outputBytes
// as [BridgeCall] holds them) as a person reading the log is shown it, and the
// size beside it: whole, redacted; cut, its [RedactedOpening] marked as the
// log marks a cut, the size that opening is of beside it, so [OutputOpening]
// reads it back. The log itself stays as the harness was handed it.
func ShownOutput(output string, outputBytes int) (shown string, shownBytes int) {
	if outputBytes == 0 {
		return redact.Secrets(output), 0
	}
	opening, whole := RedactedOpening(output, outputBytes)
	return rowRecord.Cut(opening, whole), whole
}

// ValidCallID reports whether id is one the log keeps as the id a call's
// harness gave it ([BridgeCall.CallID]): letters, digits, `-` and `_`, at
// most [MaxBridgeCallIDBytes] of them — the shape a session's id is held to
// ([ValidSessionID]), at a bound of its own.
//
// ONE RULE, HELD WHEREVER A CALL'S ID IS WRITTEN. The id is the box's word, a
// value of its request's `_meta` that its own agent can set as it likes: so
// the bridge takes no other as the call arrives (internal/api/mcpbridge), and
// the log holds every writer to it again as it keeps a call or an entry
// ([keptCallID]), whatever bridge, or build, handed it.
func ValidCallID(id string) bool {
	return id != "" && len(id) <= MaxBridgeCallIDBytes && !strings.ContainsFunc(id, outsideIDCharset)
}

// keptCallID is id as the log keeps it: whole where [ValidCallID] holds it,
// and NONE otherwise — NEVER CUT, since two ids cut alike would name two calls
// as one, while a call with none is read as every call an older build recorded
// is, by its place.
func keptCallID(id string) string {
	if !ValidCallID(id) {
		return ""
	}
	return id
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

// appendCall adds one finished call, with absorbed (the calls its own work
// took in), and holds the log to its bounds: the count ([MaxBridgeCalls]) and
// the bytes ([BridgeLogBytes]). THE TALLIES ARE TAKEN AS THEY READ before
// anything is added ([BridgeLog.Unlisted], [BridgeLog.Absorbed]). EVERY CALL
// IS KEPT AGAIN ON EVERY APPEND ([keptCall]): the newest submission's larger
// share passes to a newer one, an older build's whole call is cut. THE NEWEST
// AS THE COLLECTION READS IT: of the replayable ([submitted]) the one placed
// last ([BridgeCall.Seq]), of two at one place the one the log took last; a
// share on any other cuts the one replayed, which the collection refuses.
// THE CALL IS NUMBERED HERE ([BridgeCall.Appended]), before the bound drops
// anything, so [BridgeLog.Appends] rises by one; a caller's number is
// replaced. IN PLACE on l's calls: a caller sharing the list clones it.
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

// listedAs is call as the log lists it, found by the identity [BridgeLog.lists]
// reads, and false where the log does not list it: the call a late-call item
// copies ([itemCall]), which carries the number and the mark the log gave it.
func (l BridgeLog) listedAs(call BridgeCall) (BridgeCall, bool) {
	if call.Seq <= 0 {
		return BridgeCall{}, false
	}
	kept := keptCall(call, MaxBridgeArgsBytes)
	i := slices.IndexFunc(l.Calls, func(c BridgeCall) bool {
		return c.Seq == kept.Seq && c.StartedAt.Equal(kept.StartedAt) && c.Name == kept.Name &&
			c.Digest == kept.Digest
	})
	if i < 0 {
		return BridgeCall{}, false
	}
	return l.Calls[i], true
}

// itemCall is call as a late-call item keeps it ([LateCall.Call]): what the
// engine tells of it and finds it by — its name, its arguments and their size,
// whether it failed and, where it did, its error and that error's size, its
// places, its instants and its marks — held to an ordinary call's share
// ([keptCall], [MaxBridgeArgsBytes]), whatever share the log kept it at. Never
// a success's output, which no reader of an item shows, nor its digest, its
// harness's id, who answered it, what it committed, a runner or a bound.
func itemCall(call BridgeCall) BridgeCall {
	kept := BridgeCall{Name: call.Name, Args: call.Args, ArgsBytes: call.ArgsBytes,
		ArgsUnparsed: call.ArgsUnparsed, Failed: call.Failed, Seq: call.Seq, StartedAt: call.StartedAt,
		At: call.At, Appended: call.Appended, Late: call.Late, Abandoned: call.Abandoned, Held: call.Held,
		Approved: call.Approved}
	if call.Failed {
		kept.Output, kept.OutputBytes = call.Output, call.OutputBytes
	}
	return keptCall(kept, MaxBridgeArgsBytes)
}

// lists reports whether the log holds call already: the same place
// ([BridgeCall.Seq]), the same instant the bridge handed it to the surface,
// and the same name and identity, all as the log keeps them ([keptCall]).
//
// ALL FOUR, because each alone is some other call's: a session opened again
// on the job reuses a place, an instant is not set on a call nothing ran, and
// other arguments or another tool at the same place and instant are another
// call. A call with no place never matches ([BridgeLog.listsEarlier] finds a
// late call of an earlier round). NEVER BY THE ID ITS HARNESS GAVE IT
// ([BridgeCall.CallID]), the box's word: a repeated call may resend an id.
func (l BridgeLog) lists(call BridgeCall) bool {
	_, listed := l.listedAs(call)
	return listed
}

// listedEarlierAs is call as the log lists it where the log took it as a late
// call of an earlier round ([takeEarlier]), and false where it does not: a
// listed late call with no place, the same nonzero instant, and the same name
// and identity as the log keeps them. Three of [BridgeLog.lists]' four: the
// place guards against a reused one, which a call with none cannot be, and no
// two calls share an instant ([BridgeLog.inFlightAt]). A call with no instant,
// answered without running, is never matched.
func (l BridgeLog) listedEarlierAs(call BridgeCall) (BridgeCall, bool) {
	if call.StartedAt.IsZero() {
		return BridgeCall{}, false
	}
	kept := keptCall(call, MaxBridgeArgsBytes)
	i := slices.IndexFunc(l.Calls, func(c BridgeCall) bool {
		return c.Seq == 0 && c.Late && c.StartedAt.Equal(kept.StartedAt) && c.Name == kept.Name &&
			c.Digest == kept.Digest
	})
	if i < 0 {
		return BridgeCall{}, false
	}
	return l.Calls[i], true
}

// listsEarlier reports whether the log lists call already as a late call of an
// earlier round ([BridgeLog.listedEarlierAs]).
func (l BridgeLog) listsEarlier(call BridgeCall) bool {
	_, listed := l.listedEarlierAs(call)
	return listed
}

// keptEntry is call's entry in flight as the log keeps it
// ([BridgeLog.InFlight]): its IDENTITY, within [MaxCallInFlightBytes] — its
// name held to [MaxBridgeCallNameBytes] as a finished call's is, the digest
// the bridge handed (none, for a tool that may ask a person first: ADR-0077),
// its place, the instants it was taken at, its bound and its runner, held to
// the most an incarnation may be ([maxIncarnationBytes]) — and the id its
// harness gave it, kept whole or not at all as a finished call's is
// ([keptCallID]), so the entry and the call's one update name it alike; never
// its arguments, its output or a mark, which its one update brings.
func keptEntry(call BridgeCall) BridgeCall {
	return BridgeCall{
		Name: textcut.Within(call.Name, MaxBridgeCallNameBytes), Digest: call.Digest,
		CallID: keptCallID(call.CallID), At: call.At, Seq: call.Seq, StartedAt: call.StartedAt,
		Runner: textcut.Within(call.Runner, maxIncarnationBytes), Until: call.Until,
	}
}

// inFlightAt is the index of call's entry in flight in l, found by its
// IDENTITY — its place ([BridgeCall.Seq]), the instant the bridge took it and
// its tool, as the log keeps them — or -1 where l holds none. NEVER BY ITS
// DIGEST: an entry leaves it out for a tool that may ask a person first, and
// the finished call may then carry one. NOR BY THE ID ITS HARNESS GAVE IT
// ([BridgeCall.CallID]), the box's word, which a harness that makes a call
// again may send twice: two entries under one id are two calls. Two openings
// of a job can reuse a place, never at one instant. A call with no place,
// which the bridge never numbered, has no entry.
func (l BridgeLog) inFlightAt(call BridgeCall) int {
	if call.Seq <= 0 {
		return -1
	}
	name := textcut.Within(call.Name, MaxBridgeCallNameBytes)
	return slices.IndexFunc(l.InFlight, func(entry BridgeCall) bool {
		return entry.Seq == call.Seq && entry.StartedAt.Equal(call.StartedAt) && entry.Name == name
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

// ElidedCall is one identity in a tally of calls the log does not list: how
// many called Name with the arguments whose digest is Digest
// ([BridgeCall.Digest]), how many failed. Two tallies, each the round's:
// [BridgeLog.ElidedCalls] (the dropped middle, read as the round's calls) and
// [BridgeLog.AbsorbedCalls] (a delegated worker's, never read as the round's;
// Failed never set). THE COUNT BOUND COSTS EVIDENCE, NEVER AN OPERATION ID: a
// resumed call log seeds a call per one counted (turnctx's CallLog). Past the
// bound ([MaxElidedIdentityBytes], [MaxAbsorbedIdentityBytes]) an identity is
// FOLDED, keeping the count: into its tool's entry with no digest (also a
// withheld call, [BridgeCall.Withheld]), then the one entry with no name;
// over-counts, never under. A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go).
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
// as the row holds it, the cut said, and NEVER INSIDE A MARKER the redaction
// wrote, which the launching turn's title holds ([LaunchRequest.Task]): one
// cut in part reads as the title's own words, so the cut moves back to where
// the markers begin ([redact.MarkersAround]).
func keptTitle(s string) string {
	title, cut := rowRecord.Text(s, MaxRunTitleBytes)
	if cut == 0 {
		return title
	}
	if opening, ok := rowRecord.Opening(title, cut); ok {
		if begin, _, inside := redact.MarkersAround(s, len(opening)); inside {
			title, _ = rowRecord.TextOf(s[:begin], len(s), MaxRunTitleBytes)
		}
	}
	return title
}

// keptQuestion is a colleague's question as the row keeps it, raw: within
// [keptTitle]'s bound, and CUT WHERE ITS REDACTION ([ColleagueAsk.Question]) IS
// AN OPENING OF THE WHOLE QUESTION'S. A cut inside a credential leaves a part
// the redaction no longer knows as one, so such a cut moves back to where the
// redaction settles ([redact.Settled]): the last line break before it. That
// gives up as much as a line of the opening (all of it, for a question on one
// line), which a question past 16 KiB can spare: the turn's own record keeps
// the whole, and no shorter cut is known to settle. The second check holds what
// is served, the redaction of the opening WITH the cut's note, to the opening's
// own, so the guarantee never rests on how a rule reads the note's words.
func keptQuestion(q string) string {
	held := keptTitle(q)
	opening, cut := rowRecord.Opening(held, len(q))
	if !cut || held == q {
		return held
	}
	shown := redact.Secrets(opening)
	if strings.HasPrefix(redact.Secrets(q), shown) && strings.HasPrefix(redact.Secrets(held), shown) {
		return held
	}
	held, _ = rowRecord.TextOf(opening[:redact.Settled(opening)], len(q), MaxRunTitleBytes)
	return held
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

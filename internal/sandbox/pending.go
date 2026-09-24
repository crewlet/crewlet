package sandbox

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/workkey"
)

// The durable state of a detached coding job.
//
// A run OUTLIVES its kick-off turn, its process, and sometimes its node. What
// survives is this row: the completion turn rebuilds everything it needs from
// it, and a startup pass re-attaches to still-live boxes after a restart —
// without which a restart orphans a box that runs to its TTL, bills for every
// second, and hands its result to nobody.

// The run's state machine. Small on purpose: every state below is one a
// RECOVERY pass has to be able to act on, and a state nobody recovers from is
// a state that leaks a box.
//
// A RUN THAT IS OVER HAS NO STATE, BECAUSE IT HAS NO RECORD. Settling a run,
// done or failed, deletes its record ([PendingStore.Finish]) once its box is
// reclaimed. The record lives in an ageless bucket, since a parked run can
// wait days and a bucket age would reap the only thing that knows a billed box
// exists, and every completion poll and seat recovery reads the whole bucket.
// A terminal status kept there would be read on every one of those passes for
// the life of the deployment by readers that all skip it, while what a run
// ended as is already on the record that outlives it: the phase events of the
// resumed turn, or its sandbox_run_failed announcement.
const (
	// StatusLaunching — the job has been started, but the turn has not yet
	// written the conversation a resume re-enters. The seat is BUSY and a
	// box exists; what does not exist yet is anything to resume INTO.
	//
	// This state is the launch's two halves made visible, and it exists
	// because they are not one write. [Launch] starts a detached process
	// and returns; the suspended Execute conversation is serialized onto
	// the row only once the turn's frame unwinds, which is milliseconds
	// later on an idle host and hundreds of milliseconds on a loaded one.
	// A completion fired inside that window was claimed against a row with
	// no ExecuteState, and the coordinator — finding nothing to resume —
	// FAILED the run: a coding job that finished too fast destroyed the
	// agent's whole turn, permanently, with no retry. The window is not
	// theoretical, it is measured: a job that completes in ~100 ms against
	// an unwind that takes longer is the ordinary case for a trivial run.
	//
	// So the row says "launching" until the conversation is on it, and it
	// is [PendingStore.MarkSuspended] — one write, both facts — that makes
	// the run pollable and claimable at the same instant.
	StatusLaunching = "launching"

	// StatusRunning — the job is executing, the suspended conversation is
	// on the row, and the seat is BUSY.
	StatusRunning = "running"

	// StatusAwaiting — the agent asked a person and stopped. The seat is
	// FREE: a clarification can wait days, and holding a seat closed for
	// one would stop every other thing that seat does.
	StatusAwaiting = "awaiting_clarification"

	// StatusResumed — the tail has been claimed. THE AT-MOST-ONCE GATE.
	StatusResumed = "resumed"

	// StatusReseed — a paused box was reaped past its pause TTL. The run is
	// NOT over: the answer can still arrive, and the work re-seeds from the
	// pushed branch rather than from a snapshot that no longer exists.
	StatusReseed = "reseed"
)

// Claimable are the statuses whose tail has not run yet.
//
// Reseed belongs here, and that is the whole reason it is a state rather than
// a flag: reaping the box does not end the run.
//
// LAUNCHING IS DELIBERATELY ABSENT. A claim is the promise that a resume can
// follow it, and a launching row has no conversation to resume — claiming one
// is exactly the mistake [StatusLaunching] exists to make impossible.
var Claimable = []string{StatusRunning, StatusAwaiting, StatusReseed}

// Tail is what a claim expects to find on a run: the job the signal is about,
// and the statuses that signal may take the tail out of.
//
// A CLAIM NAMES WHAT IT CLAIMS, because the two signals that take a tail are
// about different moments of one row. A completion says a job finished, which
// is only ever true of a RUNNING row still holding that job; an answer says a
// parked question was answered, which is only ever true of an [Awaiting] row
// holding the job that asked it. A claim that took any claimable status took
// whichever the row happened to be in when the signal arrived. A duplicate
// completion therefore re-collected a run that had parked on its question and
// asked it again, and one that outlived its own job claimed the next job
// while that one was still running: the turn resumed on a half-written result
// and the settle tore the box down under the job.
type Tail struct {
	// Launch is the [PendingRun.LaunchID] the signal was raised for,
	// matched exactly, the empty value included.
	Launch string

	// From is the statuses the signal may claim out of. A status outside
	// [Claimable] is never claimed, whatever this says.
	From []string
}

// CompletionTail is what a completion of one job claims: that job, while it
// is running. The poll only fires on a running row, so a completion that
// finds its job in any other status is a duplicate of one that already took
// it.
func CompletionTail(launch string) Tail {
	return Tail{Launch: launch, From: []string{StatusRunning}}
}

// AnswerTail is what an answer claims: the job that asked, while it waits.
func AnswerTail(launch string) Tail {
	return Tail{Launch: launch, From: Awaiting}
}

// Release is how a claimed tail is handed back for its signal's retry: the
// claim it hands back, where to, and what the claim already did.
//
// A RELEASE NAMES WHAT IT RELEASES, for the reason a claim names what it
// claims. The claim is held across the resume, and the resumed turn is free to
// move the row on: an executor that calls run_sandbox again opens a new launch
// on it, and a seat's next owner reaps a claim its previous owner abandoned. A
// failed resume that put back whatever the row held by then reverted the new
// launch to running, with no box or no conversation, or revived a run already
// reaped and announced lost.
type Release struct {
	// Launch is the [PendingRun.LaunchID] the claim took, matched exactly.
	Launch string

	// To is the status the claim took the run out of, which is one of
	// [Claimable].
	To string

	// Charged is whether the run's spend is on the token counter, recorded
	// on the row by the release itself. See [PendingRun.Charged].
	Charged bool

	// Fence is the lease the claim was taken under.
	Fence Fence
}

// Holding are the statuses in which a run holds its seat, so the seat takes no
// new turn while it is in one.
//
// Launching is here and awaiting is not, and both for the same reason: the
// seat is held while the engine is driving the run, and freed while a person
// is. A launching run is a fraction of a second of engine work; a parked one
// can wait days for an answer that arrives on the seat's own inbox.
var Holding = []string{StatusLaunching, StatusRunning, StatusResumed}

// Awaiting are the statuses still waiting on a person's answer, matched back
// by conversation.
var Awaiting = []string{StatusAwaiting, StatusReseed}

// Active are the statuses that still own engine-side state — a seat, a box, or
// a pending tail — and so must survive a restart.
//
// It is also every status a record can hold, since an ended run has no record,
// so it is the closed set [PendingStore.SetStatus] accepts as well as the one
// every listing matches. Asserted at the write rather than assumed: a status
// nothing recovers from is a status that leaks a box, so a typo has to be
// refused instead of becoming a record no recovery pass matches. And matched
// at the read, so a record carrying a status this build does not know is never
// mistaken for live work.
//
// RESUMED IS HERE, which looks wrong and is not: boot recovery has to be able
// to SEE a tail that died mid-flight with the previous engine. Nothing else
// would ever look at that row again, and its paused box would leak for ever.
// LAUNCHING is here for the same reason and only that reason — it is never
// polled, but a node that died mid-launch left a box behind, and a row nobody
// lists is a box nobody reclaims.
var Active = []string{
	StatusLaunching, StatusRunning, StatusAwaiting, StatusReseed, StatusResumed,
}

// BridgeCall is one tool call a bridged run made.
//
// Its own shape rather than [tools.Call], because this crosses the wire into
// the fleet's coordination store where a node running a different build reads
// it — so the json tags are a wire format and the type must not carry a
// dependency the store layer has no business on. Same rule the rest of
// [PendingRun] follows: add fields, never rename one.
//
// It is the value of the call's own record ([coord.BridgeCalls]) and, for
// builds that read the run's row, an entry of [PendingRun.BridgeCalls].
type BridgeCall struct {
	// Seq is the call's place in its launch's log, from 1, in the order the
	// appends were numbered. Set by the store on the way out of a record: the
	// record's KEY carries it, so the record's value never does. On the
	// run's row a call carries it when this build appended it, and is zero
	// when an older build did.
	Seq uint64 `json:"seq,omitempty"`

	Name string `json:"name"`
	// Args is what the caller passed, as JSON text rather than a decoded
	// map: the map's values would round-trip through the store's own
	// encoder a second time, and a large id survives one pass and not two.
	//
	// Arguments the call's record does not hold are held there as a marker in
	// their place — still JSON, so every reader that decodes Args shows the
	// marker where the arguments would be. In the record's FITTED form, for
	// arguments too large for it: [ArgsInParts] when the call's whole was
	// filed in parts, [ArgsNotKept] when it was not. In its LEAST form, for
	// arguments longer than that marker, set aside because a server refused
	// the record: [RefusedArgsInParts] or [RefusedArgsNotKept]. A call read
	// back whole ([PendingStore.BridgeCalls]) whose parts do not reassemble
	// carries [ArgsUnreadable] or [RefusedArgsUnreadable] in place of its
	// record's in-parts marker. See [MaxBridgeCallBytes].
	Args string `json:"args,omitempty"`

	// Output is what the tool returned. Cut to fit the call's record when
	// the record could not hold it whole, and then it ends in "…" — and, when
	// the call's whole could not be kept in parts either, in a note after
	// the mark saying so. In the record's least form an output longer than
	// its mark is replaced by it: "…" alone when the whole is kept in parts,
	// and "…" followed by [RefusedWholeNotKept] when it is not. A call read
	// back whole whose parts do not reassemble ends in a note saying why,
	// whether or not its output was cut. See [MaxBridgeCallBytes].
	Output string `json:"output,omitempty"`

	Failed bool      `json:"failed,omitempty"`
	At     time.Time `json:"at"`

	// WholeBytes and WholeParts are the REFERENCE a record holding a call cut
	// — its fitted form or its least form — carries to the call's whole: how
	// long the call's record is encoded whole, and how many part records filed
	// under this one hold those bytes (see [MaxBridgeCallBytes]).
	//
	// Both zero on a call its record holds whole, and on a cut call whose
	// parts could not all be written: a record never names parts that were
	// not all filed. A call read back whole ([PendingStore.BridgeCalls])
	// carries neither. It is the whole itself — or, when its parts do not
	// reassemble, its record's cut form with the reference CLEARED: the
	// parts it would name are exactly what could not be read, the run's end
	// purges them, and the note at the end of its output carries the length
	// and the count.
	WholeBytes int `json:"whole_bytes,omitempty"`
	WholeParts int `json:"whole_parts,omitempty"`
}

// MaxBridgeCallBytes bounds one bridged call's RECORD: the record, encoded, in
// bytes.
//
// It is [coord.MaxRecordBytes], the most one coordination record may hold,
// because each call is one record. A call that record cannot hold whole is
// neither refused nor cut away: refused, it would be missing from the only log
// a resume reads, which is the failure the per-call records exist to end.
//
// ITS WHOLE IS KEPT IN PARTS. The call's record exactly as it would have been
// encoded whole is split into part records of at most [partBytes],
// filed in the same bucket under the call's own record ([coord.BridgeCalls]),
// FIRST; the record is filed after them, holding the call FIT to this ceiling
// and a reference to the parts ([BridgeCall.WholeBytes]). What reads a call
// whole — [PendingStore.BridgeCalls], which a resume rebuilds its phase from —
// reassembles it from the parts, so the resumed phase's own record carries it
// whole. What reads the record alone — a page of the run board, and a build
// that knows only the run's row, which holds a copy of the record — shows the
// cut form. The parts go with the launch's calls, by every purge of them (see
// [PendingStore.Finish]).
//
// PARTS THAT DO NOT REASSEMBLE into the call the record names — one missing,
// one short, one holding another call — are never handed over as it. The
// reader gets the record's cut form instead, its reference cleared, with
// [ArgsUnreadable] or [RefusedArgsUnreadable] in place of arguments the record
// set aside and a note at the end of its output saying why, and the node logs
// `sandbox_bridge_call_whole_unreadable` at ERROR.
//
// THE FIT decides the arguments first, whole or not at all, because they are
// JSON and a cut through JSON is text no reader can parse. They are kept unless
// the record would be over the ceiling with them and an output of nothing but
// its mark; only then does a marker take their place — [ArgsInParts], or
// [ArgsNotKept] when the parts could not be written. THE OUTPUT IS FIT TO WHAT
// IS LEFT, from the whole of it: cut on a character boundary and ending in "…"
// when it does not fit, and left exactly as it was when it does. Each mark is
// in the field that was cut, and only there.
//
// PARTS THAT CANNOT BE WRITTEN — the bucket refuses them, the store cannot be
// reached — leave the record in its fitted form, or its least form when the
// server refuses the fitted record too, with no reference; and the whole is
// then kept nowhere the engine can read back: only the coding agent in the
// box, which was handed the whole output, and the tool, which took the whole
// arguments, ever had it. The record says so where each reader looks — the
// not-kept marker in place of arguments it set aside, a note after a cut
// output's mark ([WholeNotKept], [RefusedWholeNotKept]) — and once it has
// landed the node logs `sandbox_bridge_call_whole_not_kept` at ERROR.
//
// A SERVER THAT REFUSES A RECORD WITHIN THIS CEILING accepts less than the
// contract: a node does not boot against one, but can meet one later, after a
// reconnect. The node logs `sandbox_bridge_call_refused_within_ceiling` at
// ERROR, naming max_payload, and the call's whole goes to parts split to what
// that server takes: a part it refuses is retried at half its size, or at
// [partFloorBytes] when half would be smaller, and only a refused
// part of the floor or smaller ends the split. The call's record then holds its
// LEAST FORM: its name and its outcome, each text no longer than its mark
// exactly as it was, and each longer one set aside — the arguments for
// [RefusedArgsInParts], the output for "…" — beside the reference; or, when
// the split ended without the parts, beside none, the arguments
// [RefusedArgsNotKept] and the output's mark followed by [RefusedWholeNotKept].
// Once that record has landed the node logs
// `sandbox_bridge_call_least_form_filed`, naming the whole's length and its
// parts. That is so whichever record the server refused: the whole of a call
// within this ceiling, or the fitted record of one past it.
const MaxBridgeCallBytes = coord.MaxRecordBytes

// partBytes is the most one part holds — of a bridged call's whole, or of a
// suspension's (see [PendingRun.ExecuteState]) — measured on the part record's
// value, which is the part's own bytes stored as they are.
//
// ONE RECORD'S CEILING, because a part is one record: it is the most either
// backend stores in one, and so the fewest parts a whole splits into — and
// every part is one write made before the record that names it can be: for a
// call, before the bridge answers the box (internal/api/mcpbridge appends a
// call before returning its result); for a suspension, before the run opens to
// the completion poll.
const partBytes = coord.MaxRecordBytes

// partFloorBytes is the smallest a refused part is split to.
//
// A PART REFUSED AS TOO LARGE IS RETRIED AT HALF ITS SIZE, OR AT THIS FLOOR
// WHEN HALF WOULD BE SMALLER, and only a refused part of this size or smaller
// ends the split. Halving alone would not reach the floor: from
// [partBytes] it steps from 130,048 bytes straight to 65,024, below
// it, so a server that takes a part of this size and refuses one of 130,048
// would be asked for neither, and would lose a whole it could have held.
//
// Only a server below the contract's ceiling refuses a part (see
// [MaxBridgeCallBytes]). Halving from [partBytes] passes below
// nats-server's own default max_payload of 1 MiB — MAX_PAYLOAD_SIZE in its
// server/const.go, what a server with none configured announces — on the third
// split, and this floor, sixteen times below that default, still takes parts
// through a server set lower. A server refusing parts of this size is not one a
// size can chase: the refusal is a setting to fix — on the KV backend its error
// names max_payload, and `sandbox_bridge_call_whole_not_kept` carries that
// error — and a call's record says its whole was not kept, while a suspension
// that cannot be kept fails its run with that error.
const partFloorBytes = 64 << 10

// ArgsNotKept is the [BridgeCall.Args] a call's FITTED form is recorded with
// when its arguments were too large for its record and its whole could not be
// kept in parts either: a JSON object whose one member, keyed "…", says how
// many bytes of arguments were not kept.
//
// JSON rather than an empty string, because empty arguments are what a call
// made with none looks like, and every reader — the resumed phase, the
// reviewer's tool log, the run board — renders Args as it finds it. A marker
// in the field itself is the one mark all of them show without being taught
// to look for it.
func ArgsNotKept(bytes int) string {
	return argsMarker(bytes, argsTooLarge, argsNotKept)
}

// ArgsInParts is the [BridgeCall.Args] a call's FITTED form holds when its
// arguments were too large for its record and the call's whole WAS filed in
// parts: the same one-member object as [ArgsNotKept], saying where the
// arguments went rather than that they are gone.
//
// WORDED AS WHAT HAPPENED, never as what a reader can reach now, because it
// is shown verbatim by readers that cannot reach the parts: a page of the run
// board, and a build that knows only the run's row, whose resume copies the
// row's copy of the record into the resumed phase's record — which outlives
// the parts, since the run's end purges them. A reader of the call whole never
// shows it: it has the arguments themselves, or [ArgsUnreadable] when the
// parts do not reassemble.
func ArgsInParts(bytes int) string {
	return argsMarker(bytes, argsTooLarge, argsInParts)
}

// ArgsUnreadable is the [BridgeCall.Args] a call read back whole carries in
// place of its fitted record's [ArgsInParts] when the parts it was filed in do
// not reassemble into it: the arguments are neither here nor anywhere the
// reader could reach, and the record's marker saying where they went is no
// longer the whole truth. The note at the end of the call's output says why.
func ArgsUnreadable(bytes int) string {
	return argsMarker(bytes, argsTooLarge, argsUnreadable)
}

// RefusedArgsNotKept is [ArgsNotKept] for a call's LEAST form, the record
// filed once a server refused one the contract's ceiling admits (see
// [MaxBridgeCallBytes]). Its arguments were set aside because that server
// refused the record, whether or not they were also too large for one, and
// this marker, like [RefusedArgsInParts] and [RefusedArgsUnreadable], says so.
func RefusedArgsNotKept(bytes int) string {
	return argsMarker(bytes, argsRefused, argsNotKept)
}

// RefusedArgsInParts is [ArgsInParts] for a call's least form; see
// [RefusedArgsNotKept].
func RefusedArgsInParts(bytes int) string {
	return argsMarker(bytes, argsRefused, argsInParts)
}

// RefusedArgsUnreadable is [ArgsUnreadable] for a call's least form; see
// [RefusedArgsNotKept].
func RefusedArgsUnreadable(bytes int) string {
	return argsMarker(bytes, argsRefused, argsUnreadable)
}

// Why a record sets a call's arguments aside, and what became of them: the
// two halves of every marker [argsMarker] writes.
const (
	argsTooLarge = "were too large for the record this call is kept in"
	argsRefused  = "were set aside when a server refused this call's record"

	argsInParts    = "were filed whole in parts under it"
	argsNotKept    = "were not kept"
	argsUnreadable = "the parts they were filed in could not be read back"
)

// argsMarker is the one-member object, keyed "…", that stands in
// [BridgeCall.Args] for arguments a call's record does not hold: how many
// bytes of them there were, why they are not here, and what became of them.
func argsMarker(bytes int, why, fate string) string {
	return `{"…":"` + strconv.Itoa(bytes) + ` bytes of arguments ` + why + `, and ` + fate + `"}`
}

// argsInPartsBytes reports whether args is exactly an [ArgsInParts] or a
// [RefusedArgsInParts] marker, the byte count it names, and whether it is the
// least form's.
//
// EXACT, by rebuilding the marker from the count it parsed: arguments a call
// was really made with are left alone unless they are that marker byte for
// byte.
func argsInPartsBytes(args string) (n int, refused, ok bool) {
	rest, ok := strings.CutPrefix(args, `{"…":"`)
	if !ok {
		return 0, false, false
	}
	digits, _, ok := strings.Cut(rest, " ")
	if !ok {
		return 0, false, false
	}
	n, err := strconv.Atoi(digits)
	switch {
	case err != nil:
		return 0, false, false
	case args == ArgsInParts(n):
		return n, false, true
	case args == RefusedArgsInParts(n):
		return n, true, true
	}
	return 0, false, false
}

// WholeNotKept is the note a FITTED record's cut [BridgeCall.Output] ends
// with, after its "…", when the call's whole — bytes long, encoded — could not
// be kept in parts either.
//
// IN THE OUTPUT, for the reason [ArgsNotKept] is in the arguments: every
// reader of a record renders the output as it finds it, and a cut that ended
// in "…" alone would read the same whether its whole was kept or lost.
func WholeNotKept(bytes int) string {
	return "\n[the whole of this call, " + strconv.Itoa(bytes) + " bytes, was too large for its " +
		"record and could not be kept anywhere else]"
}

// RefusedWholeNotKept is [WholeNotKept] for a call's least form, whose output
// was set aside because a server refused its record, whether or not it was
// also too large for one; see [RefusedArgsNotKept].
func RefusedWholeNotKept(bytes int) string {
	return "\n[the whole of this call, " + strconv.Itoa(bytes) + " bytes, was set aside when a " +
		"server refused its record, and could not be kept anywhere else]"
}

// BridgeCallPageBytes bounds the calls one page of a run's log carries, by
// what their records weigh.
//
// ONE RECORD'S CEILING, so the heaviest page is no heavier than the heaviest
// single record, which is all a page reads of a call: a call whose whole is
// kept in parts is paged as its record, and its parts are not read. A count
// alone cannot bound a page: a call's output can be megabytes, and a page
// bounded only by a count weighs whatever its calls sum to. No record is
// heavier than this, so a page always carries at least the call it starts at.
// A first page that also carries the log's end ([BridgeCallPage.End]) is two
// such pages, each bounded by this.
const BridgeCallPageBytes = coord.MaxRecordBytes

// BridgeCallPage is one page of a run's bridged-call log.
type BridgeCallPage struct {
	// Calls are in the order the run made them.
	Calls []BridgeCall

	// Next is the cursor for the calls this page does not carry: the Seq to
	// pass as `after`. Zero when the page — with End, on a first page —
	// carries every call the log holds from its cursor on.
	Next uint64

	// End is the log's NEWEST calls, carried by a FIRST page (after zero)
	// that does not reach them: how the run ended — a bridged run finishes by
	// submitting — without reading its middle. In the order the run made
	// them, none of them in Calls, and bounded as a page is. Empty on every
	// later page, and on a first page that reaches the end itself.
	End []BridgeCall

	// Between is how many calls fall after Calls and before End: the middle
	// a first page does not carry, which paging on from Next reaches.
	Between int

	// Total is how many calls the log holds, whatever this page carries.
	Total int

	// Dropped is how many calls the run made that the log does not hold at
	// all, and DroppedAfter how many of Calls come before the place they
	// were. Only a log an older build kept on the run's row can have them
	// (see [BridgeLog.Dropped]), and no page reaches them.
	Dropped      int
	DroppedAfter int
}

// BridgeLog is a run's whole bridged-call log, as a resume reads it.
type BridgeLog struct {
	// Calls are every call the log holds, in the order the run made them,
	// each WHOLE: a call its record holds cut is reassembled from its parts.
	// The one exception is a call whose whole was not kept, or whose parts
	// cannot be read back whole. That call is its record's cut form and says
	// so — a whole that was not kept by its record's own marks (see
	// [MaxBridgeCallBytes]); parts that cannot be read back by an unreadable
	// marker ([ArgsUnreadable], [RefusedArgsUnreadable]) in place of
	// arguments its record set aside, and by a note at the end of its output
	// saying why — never a short text passed off as the whole.
	Calls []BridgeCall

	// Dropped is how many calls the run made that Calls does not hold, and
	// DroppedAfter how many of Calls come before the place they were.
	//
	// ZERO ON EVERY LAUNCH THIS BUILD RECORDED: its per-call records drop
	// nothing. A launch an older build recorded kept its calls only on the
	// run's row, in a list that holds its first MaxBridgeCalls/2 calls and
	// its newest and drops the ones between them ([MaxBridgeCalls] states
	// the rule); those calls are kept nowhere, so this is a count of what
	// is missing rather than a cursor to it.
	Dropped      int
	DroppedAfter int
}

// UnitOfWork is the identity this run's once-per-unit-of-work writes collapse
// on, or "" when the run has none.
//
// NOT the raw field, because nothing rewrites a parked row: a run suspended by
// a build from before ADR-0017 carries no work key at all, and its TurnID IS
// one. A resume days later reads that row and has no trigger left to
// re-derive from, so without this its conversation entry and its tracker
// writes would dedupe against an empty key.
//
// ON SHAPE rather than on absence, for the reason [workkey.IsDerived] gives:
// a post-split run with no ledgerable trigger is also missing the field, and
// answering it with a run id would arm a dedupe guard with a value that means
// nothing.
func (r PendingRun) UnitOfWork() string {
	if r.WorkKey != "" {
		return r.WorkKey
	}
	if workkey.IsDerived(r.TurnID) {
		return r.TurnID
	}
	return ""
}

// MaxBridgeCalls bounds [PendingRun.BridgeCalls], the list of a bridged run's
// calls on the run's own row, and bounds nothing else.
//
// That list is OLDER BUILDS' VIEW of the log. This build records every call as
// its own record ([coord.BridgeCalls]) and reads them back from there, whole;
// the list stays on the row because a rolling upgrade puts builds that know
// only the row on the same coordination store, and one of them can be the
// node that collects the run.
//
// For those readers the row is ONE VALUE, read and written whole on every
// mutation, so its list keeps the first and last MaxBridgeCalls/2 calls and
// counts the middle it drops in [PendingRun.BridgeCallsElided]. It is kept to
// a byte budget as well, dropping from the middle and counting the same way,
// so the view never fills the row the run's own lifecycle has to go on
// writing (see rowBytes in coordstore.go). A write of the view that fails
// all the same — a store that cannot be reached — loses the call from that
// view with nothing counting it, and the append logs
// `sandbox_bridge_row_view_append_failed`. None of this reaches this build,
// which never reads the list while the records hold the launch's calls.
//
// THE ONE PLACE THIS BUILD READS THE LIST is a launch an older build
// recorded, which has no records at all — and there the middle the older
// build dropped is gone for good. [PendingStore.BridgeCalls] hands its count
// and its place to the resume as [BridgeLog.Dropped], rather than handing
// over the kept calls as though they were every call.
//
// TWO HUNDRED because it is the count the older builds themselves keep that
// list to: they append to it with the same rule, and a peer reading a run's
// row sees the shape its own appends would have made. Nothing in this build
// is sized by it.
const MaxBridgeCalls = 200

// PendingRun is one detached job's durable state, keyed by its kick-off turn.
//
// The json tags are a WIRE FORMAT, not decoration: the record lives in the
// fleet's coordination store, where a node running a different build reads
// what this one wrote. Renaming a field renames a key, and a key the reader
// does not know decodes to a zero value — an emptied box reference is a
// leaked sandbox. Add fields; never rename or repurpose one.
type PendingRun struct {
	// TurnID is the RUN this record belongs to — one execution of a turn,
	// and this record's own key. See ADR-0017.
	TurnID string `json:"turn_id"`

	// WorkKey is the unit of work that run was dispatched for. It rides
	// the row so a resumed turn keeps the identity its writes have to be
	// idempotent against — the resume re-enters the loop mid-round, with
	// no trigger left to re-derive it from. Empty on a row written before
	// this field existed, and on a turn with no ledgerable trigger, which
	// is the documented "nothing to collapse" case.
	WorkKey string `json:"work_key,omitempty"`

	AgentHandle string `json:"agent_handle"`
	AgentID     string `json:"agent_id"`
	Role        string `json:"role"`

	SandboxID   string `json:"sandbox_id"`
	CodingAgent string `json:"coding_agent"`

	// Placement is which configured backend holds this run's box.
	//
	// PERSISTED, not re-derived from the config, and that is the whole
	// reason the field exists: the completion turn may run in a different
	// process on a different node, minutes or days later, and the company
	// configuration may have been applied again in between. Reconnecting to
	// a remote box through the local backend does not error usefully — it
	// reports a box that has vanished, and a run that is still going is
	// abandoned as gone. A row written before this field existed decodes
	// empty, which the manager reads as the provider default.
	Placement string `json:"placement,omitempty"`
	CommandID string `json:"command_id"`
	Status    string `json:"status"`

	// LaunchID names the job this row currently holds.
	//
	// The row is the TURN's, and a turn can run more than one job: a
	// resumed executor that calls run_sandbox again reuses it, and
	// [PendingStore.BeginLaunch] names each launch anew. A completion
	// carries the name of the job it saw finish, which is what lets a
	// claim tell the job it was raised for from one that has replaced it.
	// See [Tail].
	//
	// Minted by the store and never by the caller, for the reason the
	// status is: a caller that could choose it could reuse one. Empty on a
	// row a build that predates it wrote, and a completion from such a
	// build carries none, so the two still match each other and nothing
	// else.
	LaunchID string `json:"launch_id,omitempty"`

	// Owner is the process INCARNATION that owns this run's seat, and
	// OwnerEpoch the seat lease's epoch at the moment of the claim.
	//
	// The epoch is the FENCING TOKEN: every mutation on a live run carries
	// it, so a node whose lease moved cannot write even if it has not
	// noticed yet. The ownership check is an optimisation; the fence is the
	// guarantee. Empty owner means unclaimed — what an in-flight run looks
	// like the instant before its seat's new owner recovers it.
	Owner      string `json:"owner"`
	OwnerEpoch int64  `json:"owner_epoch"`

	// TaskDescription is the ask the suspended turn was working on, so a
	// resume days later has the task even when the trigger that produced
	// it is long gone.
	TaskDescription string `json:"task_description"`

	// Reply is who is waiting for the suspended turn — [turn.Reply]'s wire
	// value, written as a plain string because this row crosses builds.
	//
	// It has to be persisted rather than re-derived: the resumed turn does
	// not see the trigger, so without this a turn somebody was waiting on
	// would come back from its coding run free to end in silence. An empty
	// value decodes as "nobody is waiting", which is the safe half — see
	// [turn.Reply].
	Reply string `json:"reply,omitempty"`

	// ConversationKey is where to report back AND what matches a person's
	// answer to this run.
	ConversationKey string `json:"conversation_key"`

	// Branch is the pushed WIP branch: the durable half of the work, and
	// what a re-seeded run starts from when its snapshot is gone.
	Branch    string `json:"branch"`
	SessionID string `json:"session_id"`

	Question string `json:"question"`
	Audience string `json:"audience"`

	// TraceID and SpanID are the trace the run started under, so the
	// follow-up turn nests beneath it rather than appearing as unrelated
	// work minutes later.
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`

	DelegationDepth int      `json:"delegation_depth"`
	DelegationChain []string `json:"delegation_chain"`

	// ExecuteState is the SUSPENDED Execute conversation: the serialized
	// messages, including the assistant turn with the dangling tool call,
	// plus the surface bookkeeping needed to re-enter the loop where it
	// stopped.
	//
	// Empty for exactly as long as the run is [StatusLaunching], which is
	// the state that says so: the launch starts the job, and the turn
	// writes this when its frame unwinds. Nothing polls or claims a run in
	// that window, so a claimed run always has one — and a claimed run
	// WITHOUT one can now only mean the row was written by a build that
	// predates the launching state, which the coordinator fails rather
	// than resuming into nothing.
	//
	// A CONVERSATION THE ROW CANNOT HOLD IS NOT HERE. The row is one
	// coordination record, and a conversation grows with its turn: when the
	// row holding it would be past half the transport's ceiling — or a
	// server set below that ceiling refuses it — its whole goes to part
	// records filed under the run's launch, first, and this holds a
	// REFERENCE to them in its place: one key, naming the whole's length and
	// how many parts hold it. [PendingStore.Suspension] is what reads a
	// run's conversation whole, from either. The reference is in this field
	// rather than one of its own because a build that predates it carries
	// this map whole through every write it makes to the row, and hands a
	// resume it cannot decode back for a node that can; a field it did not
	// know it would drop on its first write, and an empty map here it would
	// fail as a run with no conversation.
	ExecuteState map[string]any `json:"execute_state"`

	// BridgeCalls is OLDER BUILDS' BOUNDED VIEW of what a run made through
	// the MCP bridge, in order.
	//
	// This build does not read it while the launch has records. Every call
	// is its own record ([coord.BridgeCalls]), written FIRST and read back
	// whole by [PendingStore.BridgeCalls]; this list is written after it,
	// for the builds a rolling upgrade leaves on the same store that know
	// only the row, and it is kept to [MaxBridgeCalls] for their sake. It is
	// read here only for a launch that has no records at all, which is a
	// launch an older build recorded, and then with its dropped middle
	// reported beside it — see [PendingStore.BridgeCalls].
	//
	// Empty on every run that is not bridged, which is every ordinary
	// coding run.
	BridgeCalls []BridgeCall `json:"bridge_calls,omitempty"`

	// BridgeCallsElided counts the calls dropped from the middle of that
	// list, so its readers can tell a short run from a long one whose
	// middle was cut. The per-call records drop nothing and carry no count.
	BridgeCallsElided int `json:"bridge_calls_elided,omitempty"`

	// Charged is whether this launch's collected tokens are on the fleet's
	// token counter.
	//
	// ON THE ROW, not in the coordinator's memory, because the charge sits
	// inside the part of the tail that is RETRIED. A resume that fails
	// hands the claim back and the completion comes back, to this node or
	// to the seat's next owner, and the retry collects the same finished
	// job again. With nothing recording the first charge, every retry
	// charged the run again, against the seat's budget and the company's,
	// for as long as the resume kept failing.
	//
	// WRITTEN BY THE RELEASE that hands the claim back (see [Release]),
	// because that is the only write through which a retry reaches the
	// charge again: recorded in a write of its own, a store that refused it
	// and then accepted the release reopened the run with no record, and the
	// retry charged it twice. A run parked on its question is reached only
	// by an answer, which charges nothing, so its park needs no record.
	//
	// Launch-scoped, like the suspension: a second run_sandbox call in one
	// turn is a second job with spend of its own, so [PendingStore.BeginLaunch]
	// clears it, and nothing else does.
	Charged bool `json:"charged,omitempty"`

	PauseTTLSeconds float64 `json:"pause_ttl_seconds"`

	// PausedAt is when this run's box was paused, zero when it is not.
	// Together with SandboxID it is the engine's record of the box, and
	// what lets the reaper reclaim a snapshot nothing else would ever free.
	PausedAt time.Time `json:"paused_at"`

	// ClaimedFrom is TRANSIENT and never persisted — hence `json:"-"` —
	// the status the row held immediately BEFORE a claim flipped it to
	// resumed. A failed resume
	// dispatch reverts to exactly this, so a NAK'd trigger can re-claim on
	// redelivery. Inferring it from the other fields is unsound — a reused
	// run keeps its old question.
	ClaimedFrom string `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Paused reports whether this run's box is currently snapshotted.
func (r PendingRun) Paused() bool { return !r.PausedAt.IsZero() }

// HasBox reports whether a box exists for this run at all.
func (r PendingRun) HasBox() bool { return r.SandboxID != "" }

// PendingStore is the persistence surface for detached runs.
//
// ONE IMPLEMENTATION, [CoordStore], on the fleet's coordination store, whose
// record operations are certified on both coordination backends. It stays an
// interface because the coordinator's hardest properties (the at-most-once
// claim, the scoped release, epoch fencing) are properties of the store's
// conditional writes, which the sandboxtest suite certifies apart from any
// caller, and because a coordinator case can stage a store that refuses one
// write only by wrapping it.
type PendingStore interface {
	// BeginLaunch opens a launch on this turn's row: it creates the row
	// when there is none, and RESETS an existing one to launching —
	// clearing the previous job's suspended conversation, the question
	// it was parked on and the record of its charge, while keeping the
	// row's identity and its box. Either way the launch gets a new
	// [PendingRun.LaunchID].
	//
	// CREATE-OR-RESET rather than create-if-absent, because the SECOND
	// run_sandbox call in one turn presents the same turn id as the first
	// and is a different job. Left alone, the row kept the first
	// suspension's conversation and whatever status the tail had reached —
	// so a resume that relaunched read back as `resumed`, the settle path
	// could not tell it from a finished turn, and it tore down the box the
	// new job was running in.
	BeginLaunch(ctx context.Context, run PendingRun, fence Fence) error

	Get(ctx context.Context, turnID string) (PendingRun, bool, error)

	// ClaimForResume atomically flips a run to resumed, when it holds the
	// tail's launch in one of the tail's statuses.
	//
	// Reports the row IFF THIS CALL WON: the at-most-once tail guard, and
	// the reason a claim names its launch (see [Tail]). The returned row
	// carries ClaimedFrom, so a failed dispatch can put it back exactly
	// where it was.
	ClaimForResume(ctx context.Context, turnID string, tail Tail) (PendingRun, bool, error)

	// ReleaseClaim hands a claimed run back to the status it was claimed
	// from, reporting whether THIS call did.
	//
	// Only while the claim still stands: the run is resumed, holds the
	// release's launch, and no newer lease outranks the release's fence.
	// Anything else means the run has moved on from the claim (see
	// [Release]), and a retry of the signal has nothing left to take.
	//
	// The claim's charge is recorded IN THE SAME WRITE, and never cleared
	// by one: a run is reopened to a retry with its record or not at all
	// (see [PendingRun.Charged]).
	//
	// FALSE IS NOT AN ERROR: it is a run that moved on, or a row that is
	// gone. A release to a status outside [Claimable] is an error, because
	// no claim ever takes a run out of one.
	ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error)

	// MarkAwaiting parks a run on a question, freeing the seat.
	MarkAwaiting(ctx context.Context, turnID string, q Clarification) error

	// ClaimOwnership takes the run for a node, reporting whether it won.
	// A run whose epoch is already higher is not stolen.
	ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error)

	// SetStatus moves a run between the live states, fenced on the epoch.
	// Ending a run is not a status; see Finish.
	SetStatus(ctx context.Context, turnID, status string, fence Fence) error

	// Finish ends a run by deleting its record, unless a newer lease
	// outranks the fence, reporting whether THIS call deleted it.
	//
	// The caller reclaims the box FIRST. A record naming a box that is
	// already gone is harmless (recovery reaps it and a kill of a gone box
	// is a no-op), while a live box whose record was deleted is named by
	// nothing and billed until its provider's TTL.
	//
	// Conditional on the version it read and re-decided on a lost race, so
	// a delete racing a write sees that write before it deletes. FALSE IS
	// NOT AN ERROR: the run is already gone, which is the ordinary shape of
	// two parties reaching the end of one run, or a newer lease owns it.
	//
	// THE RUN'S BRIDGED CALLS END WITH IT: once the record is deleted, its
	// launch's calls and every part filed under them are purged. So what
	// has to outlive the run is read from them BEFORE it is finished — a
	// resume carries every call, whole, into the resumed phase's record —
	// and every caller that finishes a run no resume collected says where
	// that run's calls went.
	Finish(ctx context.Context, turnID string, fence Fence) (bool, error)

	// ExpirePause flips a run parked on a clarification to reseed AND
	// clears its box record, reporting whether THIS call won.
	//
	// The pause reaper's authority, and the reason it is a conditional flip
	// rather than a plain SetStatus. The reaper decides from a snapshot
	// taken seconds ago, and the answer that un-parks the run may have
	// arrived since — ClaimForResume has already moved the row and an
	// Execute loop is reconnecting to that very box. Killing the box before
	// this returns true destroys it underneath that resume. Conditional on
	// StatusAwaiting alone: a run already reseeded has no snapshot left to
	// expire, and any other status means somebody else owns the tail.
	//
	// It clears the box IN THE SAME WRITE rather than leaving that to a
	// following ReleaseBox, because the gap between two writes is a state a
	// reader can see: a run reading as `reseed` while still naming its box
	// tells an arriving answer that the checkout is live, moments before it
	// is destroyed. One statement, no window.
	ExpirePause(ctx context.Context, turnID string) (bool, error)

	// AttachSandbox records the box and the command a run is using.
	AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error

	// MarkBoxPaused and ReleaseBox are the two halves of the box record:
	// paused_at set means a snapshot exists and is being billed for,
	// cleared means the box is gone.
	MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error
	ReleaseBox(ctx context.Context, turnID string) error

	// MarkSuspended writes the suspended conversation AND flips launching to
	// running, in one compare-and-swap.
	//
	// ONE WRITE, because it is one fact: the run becomes resumable and
	// becomes pollable at the same instant. Two writes leave a state a
	// reader can see — a `running` row with no conversation — and the
	// reader is the completion poll, which fires on it. See
	// [StatusLaunching] for what that cost.
	//
	// Reports whether the flip happened. FALSE IS NOT AN ERROR and is not a
	// lost race either — it is a run that is no longer launching, which is
	// left alone rather than overwritten: re-arming a row whose tail has
	// already been claimed would hand a redelivered completion a second
	// resume of a turn that is over. The caller has a suspended
	// conversation with nowhere to put it, so the run cannot be resumed and
	// must be failed rather than left holding a box.
	//
	// A CONVERSATION THE ROW CANNOT HOLD is kept whole in parts filed under
	// the run's launch BEFORE the write that names them (see
	// [PendingRun.ExecuteState]), so the one write still opens the run to
	// the poll with its conversation reachable. Parts a server refuses are
	// split smaller, down to the same floor a bridged call's are. A whole
	// that cannot be kept is an ERROR naming the limit that refused it, and
	// the run is left launching for the caller to fail.
	MarkSuspended(ctx context.Context, turnID string, state map[string]any) (bool, error)

	// Suspension returns a run's suspended conversation WHOLE: the one its
	// row holds, or, when the row holds a reference in its place, the
	// whole reassembled from the parts filed under the run's launch. A row
	// with no conversation answers an empty one.
	//
	// Parts that do not make the whole the reference names — one missing,
	// one short — are [ErrSuspensionUnreadable], never a shorter
	// conversation passed off as the one that was suspended, which a resume
	// would re-enter mid-round with its dangling call gone. A store that
	// cannot be read is any other error, for the caller to retry.
	Suspension(ctx context.Context, run PendingRun) (map[string]any, error)

	// AppendBridgeCall records one tool call a bridged run made through the
	// MCP bridge, so the resume of a run that outlived its process still
	// has every call it made. See [BridgeCall].
	//
	// The call becomes its own record under the run's CURRENT launch, and
	// that record is the authoritative copy. A call too large for one
	// record, or one a server below the ceiling refused, keeps its whole in
	// parts filed under the record before it, and the record holds it cut —
	// fitted to the ceiling, or in its least form — with a reference to them;
	// or, when the parts could not be written, cut and saying the whole was
	// not kept (see [MaxBridgeCallBytes]). The run row's bounded list
	// ([PendingRun.BridgeCalls]) is written after the record, for older
	// builds; a failure there is logged and does not fail the append,
	// because the call is already recorded everywhere this build reads.
	//
	// NO FENCE, unlike every other mutation here: this is a log append
	// rather than an ownership decision, and refusing to record a call that
	// already ran because the seat's lease moved would lose evidence of
	// something that is true either way.
	//
	// Reports whether the append landed. FALSE IS NOT AN ERROR: it is a run
	// whose row is gone, which is the ordinary shape of a late call from a
	// box that is shutting down, and the caller must not fail the box's
	// call over it.
	AppendBridgeCall(ctx context.Context, turnID string, call BridgeCall) (bool, error)

	// BridgeCalls returns the run's current launch's log, in the order the
	// calls were made: the whole record an agent-mode resume rebuilds its
	// phase from.
	//
	// EVERY CALL WHOLE: a call whose record holds it cut, with a reference
	// to its parts, is reassembled from them, and a part that is missing or
	// a whole that does not reassemble yields the record's cut form with the
	// reference cleared, its set-aside arguments marked unreadable and a
	// note in its output saying why — never a short "whole" (see
	// [BridgeLog.Calls]).
	//
	// From the per-call records, which hold EVERY call the launch made.
	// From the row's list only when the launch has NO records — which is a
	// launch an older build recorded, since this build writes the record
	// before the row and never the row without the record — and that list
	// is the one log that can be missing calls: past [MaxBridgeCalls] the
	// older build kept only its two ends. Those are counted and placed in
	// [BridgeLog.Dropped] and [BridgeLog.DroppedAfter], never passed off as
	// a complete log.
	//
	// A read that fails is an error, never an empty log: "this run called
	// nothing" is exactly what the delivery check reads as a turn that
	// reached nobody.
	BridgeCalls(ctx context.Context, run PendingRun) (BridgeLog, error)

	// BridgeCallPage returns one page of the same log: the calls after the
	// cursor `after` (zero for the first page), at most limit of them, and
	// stopping before a call whose record would take the page past
	// [BridgeCallPageBytes] — the first call always carried. Each call is AS
	// ITS RECORD HOLDS IT: a call whose whole is kept in parts is its cut
	// form and its reference ([BridgeCall.WholeBytes]), which a page never
	// reassembles, and no part is ever paged or counted. A FIRST page
	// that does not reach the end of the log also carries the log's newest
	// calls, at most limit of them and bounded the same way, in
	// [BridgeCallPage.End], and counts the calls between the two. A limit
	// below one is an error.
	//
	// A LOG AN OLDER BUILD KEPT ON THE ROW IS ONE PAGE, whatever the limit:
	// it is at most [MaxBridgeCalls] calls, already read whole with the row,
	// so a limit would bound no read. The page places the calls that build
	// dropped within Calls, at [BridgeCallPage.DroppedAfter].
	BridgeCallPage(ctx context.Context, run PendingRun, after uint64, limit int) (BridgeCallPage, error)

	// ListActive returns every run that still owns engine-side state.
	ListActive(ctx context.Context) ([]PendingRun, error)

	// ListActiveForSeat is the "is this seat busy?" read.
	ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error)

	// FindAwaitingByConversation matches a person's answer back to the run
	// that asked.
	FindAwaitingByConversation(ctx context.Context, handle, conversation string) (PendingRun, bool, error)
}

// ErrSuspensionUnreadable reports a run whose suspended conversation was kept
// in parts that cannot be read back whole. PERMANENT: the parts are what the
// run's reference names, and a retry reads the same ones.
var ErrSuspensionUnreadable = errors.New("sandbox: the suspended conversation cannot be read back whole")

// Clarification is what a parked run is waiting for.
type Clarification struct {
	Question string
	// Audience is who should answer: "requester", "team", "manager", or a
	// handle. Carried rather than derived, because the coding agent chose
	// it and the engine has no better information about who knows.
	Audience string
	// Branch is the WIP pushed before parking — the durable half of the work
	// while the question waits.
	Branch    string
	SessionID string
}

// BoxRef is the box and command a run is attached to.
type BoxRef struct {
	SandboxID   string
	CommandID   string
	CodingAgent string
	SessionID   string
	PauseTTLSec float64
}

// Fence is the ownership token a mutation carries.
//
// A ZERO FENCE MEANS UNFENCED and is deliberate rather than a default: recovery
// writes and the boot pass legitimately have no lease yet. What must never
// happen is a node writing under a lease it has LOST, and that is the case a
// non-zero fence closes.
type Fence struct {
	Owner string
	Epoch int64
}

// Fenced reports whether this token constrains anything.
func (f Fence) Fenced() bool { return f.Epoch > 0 }

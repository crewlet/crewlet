package sandbox

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/secrets"
)

// worstAt is an instant at the widest the row's encoder writes one: nine
// digits of fraction and a zone that is not UTC.
var worstAt = time.Date(2026, 11, 3, 23, 59, 59, 999_999_999, time.FixedZone("x", 5*3600+1800))

// worstCall is one bridged call with every field at its widest, the payloads
// far past what the row keeps and in the bytes that escape the most: a name
// past its bound; arguments holding a body of markup beside the identifiers
// that say where it went, and forty notes of markup a few hundred bytes each,
// so what is kept once the body is left out is as close to its share as
// whole notes come; an output of markup; every kind of change a call can
// commit to its turn, each once ([everyKind]); and the id its harness gave it
// at its bound ([worstCallID]). NOT HELD AND NOT APPROVED: a call whose
// arguments are withheld keeps none of them ([BridgeCall.Withheld]), so the
// flags cost a call twenty-seven bytes and the arguments it then does not keep
// eight thousand.
func worstCall(t *testing.T, name string) BridgeCall {
	t.Helper()
	notes := map[string]any{}
	for i := range 40 {
		notes[fmt.Sprintf("n%02d", i)] = strings.Repeat("<", 300)
	}
	args, err := json.Marshal(map[string]any{
		"repo": "acme/api", "branch": "crewlet/eng/fix", "notes": notes,
		"files": []any{map[string]any{"path": "internal/a.go", "content": strings.Repeat("<", 64<<10)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := strings.Repeat("s", 32)
	return BridgeCall{
		Name: name, Args: string(args), Output: strings.Repeat("<", 64<<10),
		Failed: true, At: worstAt, Seq: math.MaxInt, Appended: math.MaxInt, Late: true, Abandoned: true,
		StartedAt: worstAt, DurationMS: math.MaxInt64, Origin: "mcp:" + server, Server: server,
		Source: "mcp:" + server, Refusal: string(mcp.RefusalReassignmentBudget), Digest: strings.Repeat("f", 64),
		HeldID: uuid.NewString(), Committed: everyKind(), CallID: worstCallID,
	}
}

// worstCallID is the id a call's harness gave it at the widest the log keeps
// one ([MaxBridgeCallIDBytes]): every character it may hold is written as
// itself, so any id at the bound is the widest.
var worstCallID = strings.Repeat("Az09-_", MaxBridgeCallIDBytes/6) + strings.Repeat("x", MaxBridgeCallIDBytes%6)

// everyKind is every kind of change a call can commit to its turn, each once,
// as the log spells them ([BridgeCall.Committed]): the closed set
// (types.CommittedChanges), read off it, so a kind added there is measured
// here. The most one call carries, since the bridge reads a call's kinds off
// its turn's set of changes, which holds each once (internal/agent/turnctx's
// Written.Since), and the most the log's union of them holds
// ([BridgeLog.Committed]).
func everyKind() []string {
	kinds := make([]string, 0, len(types.CommittedChanges))
	for _, c := range types.CommittedChanges {
		kinds = append(kinds, string(c))
	}
	return kinds
}

// A BRIDGED CALL IS HELD TO ITS SHARE OF THE LOG, AT ITS WORST. Every field of a
// call kept by the log, each at the widest the log's encoder writes it — a name
// past its bound in markup, arguments and an output of a megabyte of markup,
// both its marks, every kind of change it can commit and the id its harness
// gave it at its bound — weighs no more than [MaxBridgeCallBytes] less the room
// left for what a later build adds to a call; the newest successful submission
// no more than [MaxSubmissionCallBytes] less the same. And the parts are each
// within the figure rowbytes.go states for them, and those figures with the
// room close at the share, so the arithmetic there is this test's. A call
// handed an id past its bound, in markup, is kept with none, so the share holds
// whatever a writer hands the log.
//
// Mutation: keep the arguments or the output whole, or measure a cut by the
// payload's own length rather than escaped, and the call overruns its share;
// leave the name uncut, and the name does; keep a call without the kinds it
// committed, or without its id, and the case is no longer at its worst; keep
// an id whatever it is (log-keeps-any-id), and the call handed one past its
// bound keeps it.
func TestABridgedCallIsHeldToItsShare(t *testing.T) {
	t.Parallel()
	const (
		others  = 800  // what rowbytes.go counts for a call's every other field
		reserve = 1758 // what rowbytes.go leaves a later build in one call
	)
	if MaxBridgeCallNameBytes*6+2+MaxBridgeArgsBytes+MaxBridgeOutputBytes+others+reserve != MaxBridgeCallBytes {
		t.Errorf("a call's name, arguments, output and other fields, with the room left for a later build, "+
			"do not close at its share %d", MaxBridgeCallBytes)
	}
	for name, tc := range map[string]struct {
		budget, share, args int
	}{
		"an ordinary call":      {MaxBridgeArgsBytes, MaxBridgeCallBytes, MaxBridgeArgsBytes},
		"the newest submission": {MaxSubmissionArgsBytes, MaxSubmissionCallBytes, MaxSubmissionArgsBytes},
	} {
		call := keptCall(worstCall(t, strings.Repeat("<", 4<<10)), tc.budget)
		raw, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		nameBytes, argsBytes, outputBytes := kept.Quoted(call.Name), kept.Quoted(call.Args), kept.Quoted(call.Output)
		fixed := len(raw) - nameBytes - argsBytes - outputBytes
		switch {
		case nameBytes > 256*6+2:
			t.Errorf("%s: its name weighs %d, past the 1,538 its bound allows", name, nameBytes)
		case argsBytes > tc.args:
			t.Errorf("%s: its arguments weigh %d, past their %d", name, argsBytes, tc.args)
		case outputBytes > MaxBridgeOutputBytes:
			t.Errorf("%s: its output weighs %d, past its %d", name, outputBytes, MaxBridgeOutputBytes)
		case fixed > others:
			t.Errorf("%s: its other fields weigh %d, past the %d the per-call bounds count", name, fixed, others)
		case len(raw) > tc.share-reserve:
			t.Errorf("%s: weighs %d, past its share %d less the %d left for a later build",
				name, len(raw), tc.share, reserve)
		}
		t.Logf("%s weighs %d: name %d, arguments %d, output %d, the rest %d", name, len(raw),
			nameBytes, argsBytes, outputBytes, fixed)
		// AT ITS WORST, or the bound above held nothing: the arguments are
		// kept to within a note's weight of their share.
		if argsBytes < tc.args-2000 {
			t.Errorf("%s: its arguments are kept at %d, far under their %d: the case is not at its worst",
				name, argsBytes, tc.args)
		}
		if call.ArgsBytes == 0 || call.OutputBytes != 64<<10 {
			t.Errorf("%s: states a whole of %d arguments and %d output, want both", name,
				call.ArgsBytes, call.OutputBytes)
		}
		// AND EVERY KIND IT COMMITTED KEPT, or the other fields were measured
		// short of them, and so its id at its bound.
		if !slices.Equal(call.Committed, everyKind()) {
			t.Errorf("%s: is kept with %d of the %d kinds of change it committed: the case is not at its worst",
				name, len(call.Committed), len(types.CommittedChanges))
		}
		if call.CallID != worstCallID {
			t.Errorf("%s: is kept with the id %q, want its harness's whole: the case is not at its worst",
				name, call.CallID)
		}
	}
	handed := worstCall(t, strings.Repeat("<", 4<<10))
	handed.CallID = strings.Repeat("<", 64<<10)
	if kept := keptCall(handed, MaxBridgeArgsBytes); kept.CallID != "" {
		t.Errorf("a call handed an id of %d bytes is kept with one of %d, want none: an id past its bound is "+
			"kept as none, never cut", len(handed.CallID), len(kept.CallID))
	}
}

// everyField is [worstCall] with every other field a call can carry set
// besides: both its marks, both its sizes, a runner past the most an
// incarnation may be in markup and a bound at the widest instant. What the
// models hand a writer that keeps a call (an entry in flight, [keptEntry]; the
// call a person approved, [approvedCall]), so a field the call it keeps leaves
// zero is one that writer leaves out, never one the model did not hand it
// ([zeroInTheWorstRow] and [zeroInTheWorstLog] name each).
func everyField(t *testing.T, name string) BridgeCall {
	t.Helper()
	call := worstCall(t, name)
	call.Held, call.Approved, call.ArgsBytes, call.OutputBytes = true, true, math.MaxInt, math.MaxInt
	call.Runner, call.Until = strings.Repeat("<", 4<<10), worstAt
	return call
}

// worstEntry is one call's entry in flight at its widest as the log keeps it
// ([keptEntry]): a name past its bound in the bytes that escape the most, a
// digest, both places and every instant at their widest, a runner past the
// most an incarnation may be in markup, and the id its harness gave it at its
// bound ([worstCallID]). KEPT FROM A CALL WITH EVERY FIELD SET ([everyField]),
// so a field the entry leaves zero is one keptEntry leaves out.
func worstEntry(t *testing.T, seq int) BridgeCall {
	t.Helper()
	call := everyField(t, strings.Repeat("<", 4<<10))
	call.Seq = math.MaxInt - seq
	return keptEntry(call)
}

// A CALL IN FLIGHT IS HELD TO ITS SHARE. An entry written ahead of a call keeps
// its identity and the id its harness gave it ([keptEntry]): at its widest —
// its name and its runner past their bounds in markup the encoder escapes
// six-fold, every instant at its longest, its id at its bound — it weighs no
// more than [MaxCallInFlightBytes], whatever the caller handed it, arguments,
// output and an id past its bound included; and that id it keeps as none,
// never cut, as a finished call keeps one. And the log's split closes as its
// table states it: its calls, its calls in flight and the rest of it
// ([BridgeLogBytes], [InFlightBytes], [LogRestBytes]) are [RowBytes] together,
// so a share moved without the table fails here.
//
// Mutation: keep a caller's runner, or its arguments, as handed
// (entry-keeps-its-runner-whole), or its id whatever it is (log-keeps-any-id),
// and an entry overruns its share; cut an entry's id to its bound rather than
// keep it as none (entry-cuts-its-id), and the entry handed one past it keeps a
// cut id; hold more calls in flight than the table states
// (more-calls-in-flight-than-the-table-says), and the log's split no longer
// closes.
func TestACallInFlightIsHeldToItsShare(t *testing.T) {
	t.Parallel()
	entry := worstEntry(t, 0)
	if entry.CallID != worstCallID {
		t.Errorf("the widest entry keeps the id %q, want its harness's whole: the case is not at its worst",
			entry.CallID)
	}
	handed := entry
	handed.Args, handed.Output = strings.Repeat("<", 64<<10), strings.Repeat("<", 64<<10)
	handed.Runner, handed.CallID = strings.Repeat("<", 64<<10), strings.Repeat("<", 64<<10)
	if kept := keptEntry(handed); kept.CallID != "" {
		t.Errorf("an entry handed an id of %d bytes is kept with one of %d, want none: an id past its bound is "+
			"kept as none, never cut", len(handed.CallID), len(kept.CallID))
	}
	for name, call := range map[string]BridgeCall{"at its widest": entry, "as a caller handed it": keptEntry(handed)} {
		raw, err := json.Marshal(call)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("an entry in flight %s weighs %d of its %d", name, len(raw), MaxCallInFlightBytes)
		if len(raw) > MaxCallInFlightBytes {
			t.Errorf("an entry in flight %s weighs %d, past its share %d", name, len(raw), MaxCallInFlightBytes)
		}
	}
	if sum := BridgeLogBytes + InFlightBytes + LogRestBytes; sum != RowBytes {
		t.Errorf("the log's calls (%d), its %d calls in flight (%d) and the rest of it (%d) are %d together, "+
			"want the %d a value may: a share moved without the table that states it", BridgeLogBytes,
			MaxCallsInFlight, InFlightBytes, LogRestBytes, sum, RowBytes)
	}
}

// KEEPING A CALL AGAIN CHANGES NOTHING. Every append keeps every call in the
// log again ([BridgeLog.appendCall]), so a call kept once must come back as it
// was — its sizes still the whole's, never the size of what was kept.
//
// Mutation: record the kept text's size as the whole on a second keep, and the
// board says a megabyte was a few KiB.
func TestKeepingACallAgainChangesNothing(t *testing.T) {
	t.Parallel()
	once := keptCall(worstCall(t, "code_commit_files"), MaxBridgeArgsBytes)
	twice := keptCall(once, MaxBridgeArgsBytes)
	a, _ := json.Marshal(once)
	b, _ := json.Marshal(twice)
	if string(a) != string(b) {
		t.Errorf("a call kept twice changed:\n%s\n%s", a[:200], b[:200])
	}
}

// A WITHHELD CALL KEEPS NEITHER ITS ARGUMENTS NOR THEIR DIGEST, whoever handed
// it to the log: a call held for a person's approval, and the one they
// approved, carry their arguments sealed on the held call (ADR-0077), and a
// digest of them in the clear would confirm a guess at what the seal hides. A
// call dropped from the middle is tallied the same way, by its tool alone.
//
// Mutation: keep a withheld call's digest, or tally it under one, and the log
// holds it in the clear.
func TestAWithheldCallKeepsNeitherItsArgumentsNorTheirDigest(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]BridgeCall{
		"held":     {Name: "close_issue", Held: true, Failed: true, Args: `{"id":7}`, Digest: strings.Repeat("d", 64)},
		"approved": {Name: "close_issue", Approved: true, Args: `{"id":7}`, Digest: strings.Repeat("d", 64)},
	} {
		got := keptCall(call, MaxBridgeArgsBytes)
		if got.Args != "" || got.ArgsBytes != 0 || got.Digest != "" {
			t.Errorf("%s: the log keeps %q (%d bytes) under identity %q, want neither", name, got.Args,
				got.ArgsBytes, got.Digest)
		}
		if e := identity(call); e.Digest != "" || e.Name != "close_issue" || e.Calls != 1 {
			t.Errorf("%s: a dropped call is tallied as %+v, want its tool alone", name, e)
		}
	}
}

// A FINISHED CALL IS NOBODY'S TO WAIT FOR, whoever hands it to the log. Only a
// call's entry in flight carries the process running it and the bound a
// collection waits for it until ([keptEntry]); a call kept finished keeps
// neither ([keptCall]), though it is handed both: as the log keeps any call, as
// the store records the call a person approved on its held call
// ([approvedCall]), and as the log appends that call, which no append over the
// bridge takes ([BridgeLog.withApproved]). A runner on a finished call is
// bounded by nothing, so a call kept with the one it was handed would hold the
// log's share and the row's to nothing.
//
// Mutation: keep a finished call's runner and bound as handed
// (kept-call-keeps-its-runner), and each of the three keeps both.
func TestAFinishedCallIsNobodysToWaitFor(t *testing.T) {
	t.Parallel()
	waited := func(call BridgeCall) BridgeCall {
		call.Runner, call.Until = strings.Repeat("<", 4<<10), worstAt
		return call
	}
	approved := approvedCall(waited(BridgeCall{Name: "close_issue", Origin: "builtin"}),
		HeldCall{ID: "held-1", Tool: "close_issue", MadeAt: worstAt})
	appended, ok := BridgeLog{Round: "r1"}.withApproved("r1", waited(approved))
	if !ok || len(appended.Calls) != 1 {
		t.Fatalf("the approved call's append left %d calls (appended %v), want it appended", len(appended.Calls), ok)
	}
	for name, call := range map[string]BridgeCall{
		"kept by the log":               keptCall(waited(BridgeCall{Name: "post_message", Seq: 1}), MaxBridgeArgsBytes),
		"approved, on its held call":    approved,
		"approved, appended to its log": appended.Calls[0],
	} {
		if call.Runner != "" || !call.Until.IsZero() {
			t.Errorf("a finished call %s keeps a runner of %d bytes and the bound %s, want neither", name,
				len(call.Runner), call.Until)
		}
	}
}

// THE CALL A PERSON APPROVED STATES NO SIZE OF THE OUTPUT IT DROPS. Its result
// rides the row sealed on the held call, and the call keeps none of it
// ([approvedCall]), so a size beside no output would read as a whole output cut
// to nothing: whoever hands the call one, it keeps no size of its output, as it
// keeps none of its arguments' ([keptCall]).
//
// Mutation: keep the size of the approved call's output as handed
// (approved-call-keeps-an-output-size), and it states one.
func TestTheApprovedCallStatesNoSizeOfTheOutputItDrops(t *testing.T) {
	t.Parallel()
	got := approvedCall(BridgeCall{Name: "close_issue", Output: "closed", OutputBytes: 64 << 10,
		Args: `{"id":7}`, ArgsBytes: 64 << 10}, HeldCall{ID: "held-1", Tool: "close_issue", MadeAt: worstAt})
	if got.Output != "" || got.OutputBytes != 0 || got.Args != "" || got.ArgsBytes != 0 {
		t.Errorf("the approved call keeps the output %q stated at %d bytes and the arguments %q stated at %d, "+
			"want neither, nor a size of either", got.Output, got.OutputBytes, got.Args, got.ArgsBytes)
	}
}

// IDENTITIES FOLD PAST THEIR SHARE AND NEVER LOSE A COUNT. A long run's dropped
// middle is a tally of identities held to [MaxElidedIdentityBytes]: while they
// fit, every identity is exact; past it identities fold into their tool's
// count and then into one count with no tool — so the tally always fits, and
// always accounts for every call dropped, with its failures.
//
// Mutation: drop an identity rather than fold it, and the count falls short of
// the calls; skip the second step, and a tally of many tools overruns its
// share.
func TestIdentitiesFoldPastTheirShareAndNeverLoseACount(t *testing.T) {
	t.Parallel()
	var ids []ElidedCall
	calls, failed := 0, 0
	for i := range 4000 {
		call := BridgeCall{
			// SIXTY TOOLS, then ever more of them, each called with
			// arguments of its own: exact first, by tool past the share,
			// unnamed past that.
			Name:   fmt.Sprintf("tool_%04d_%s", i%(60+i/10), strings.Repeat("x", 200)),
			Digest: fmt.Sprintf("%064x", i),
			Failed: i%7 == 0,
		}
		ids = fold(ids, identity(call), elidedTally)
		calls++
		if call.Failed {
			failed++
		}
		if n := tallyBytes(ids, elidedTally); n > MaxElidedIdentityBytes {
			t.Fatalf("after %d drops the tally weighs %d, past its %d", calls, n, MaxElidedIdentityBytes)
		}
	}
	gotCalls, gotFailed, unnamed := 0, 0, false
	for _, e := range ids {
		gotCalls += e.Calls
		gotFailed += e.Failed
		unnamed = unnamed || e.Name == ""
	}
	if gotCalls != calls || gotFailed != failed {
		t.Errorf("the tally counts %d calls (%d failed), want %d (%d)", gotCalls, gotFailed, calls, failed)
	}
	if !unnamed {
		t.Error("four thousand drops across hundreds of long names never reached the unnamed count")
	}

	// AN EXACT TALLY WHILE IT FITS: a few identities, each counted apart.
	var few []ElidedCall
	for i := range 30 {
		few = fold(few, identity(BridgeCall{Name: "read_file", Digest: fmt.Sprintf("%064x", i%3)}),
			elidedTally)
	}
	if len(few) != 3 || few[0].Calls != 10 || few[0].Digest == "" {
		t.Errorf("thirty drops of three identities tally %+v, want three exact entries of ten", few)
	}
}

// heldResultAtItsWorst is the result of a held call as the row holds it at its
// widest: an output at [MaxHeldResultBytes] of markup, sealed the way the
// engine seals one ([CallSeal.SealResult]) — encoded with every byte escaped
// six-fold, then sealed, then written as one string.
func heldResultAtItsWorst(t *testing.T, held HeldCall) string {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := NewCallSeal(cipher).SealResult(strings.Repeat("a", 64), held,
		HeldResult{Output: strings.Repeat("<", MaxHeldResultBytes), Failed: true})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// paddedTo is v grown, by the string pad sets, until it encodes to exactly n
// bytes — the widest a field held to n can be.
func paddedTo[T any](t *testing.T, n int, v T, pad func(*T, string)) T {
	t.Helper()
	pad(&v, "")
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > n {
		t.Fatalf("%T encodes to %d bytes before it is padded, past the %d it is grown to", v, len(raw), n)
	}
	pad(&v, strings.Repeat("a", n-len(raw)))
	if raw, _ = json.Marshal(v); len(raw) != n {
		t.Fatalf("%T padded to %d bytes, want %d", v, len(raw), n)
	}
	return v
}

// admitsPanel reports whether a held call's own contract ([HeldCall.validate])
// admits a call put to a panel of n delegates: whether it refuses nothing of
// the call's delegation, whatever it refuses of the rest of a call built to
// hold that alone.
func admitsPanel(n int) bool {
	delegates := make([]string, n)
	for i := range delegates {
		delegates[i] = fmt.Sprintf("delegate-%d", i)
	}
	held := HeldCall{Act: string(delegated.KindSSHOperate), Delegation: &HeldDelegation{Rule: 1,
		Delegates: delegates, Digest: strings.Repeat("d", maxHeldBindingBytes)}}
	return !slices.ContainsFunc(held.validate(nil), func(err error) bool {
		return strings.HasPrefix(err.Error(), "held_call.delegation ")
	})
}

// worstRow is a run's row with every field at its bound, each built in the
// bytes that escape the most and, where a field is sealed or cut by code of
// this package's, by that code: the parked state at its share, a recorded
// answer and the copies of a let-go one each at theirs, a held call carrying
// its sealed arguments at their share, its result sealed at its widest, the
// call a person approved, which a reset owes the log too, the decision the
// call carries out and the delegates it was put to first; the turn's read of
// its conversation at its bound; who the turn works for and the answers that
// woke it; and what its job may do on Gitlet: the most holds a turn may take,
// the most repositories a job is given, each at the widest its grammar admits,
// and its floors.
//
// A FIELD ADDED TO THE ROW IS SET HERE AT ITS BOUND, in the change that adds it,
// so the arithmetic stays this test's: a field of the row comes out of the
// rest's share (rowbytes.go's RestBytes, through ExtraRowBytes), and one of the
// parked state out of the state's, which the park holds it to whatever it
// carries. TestTheWorstRowSetsEveryField holds this to that, failing on any
// field of the row left zero here that [zeroInTheWorstRow] does not name, and
// TestTheWholeRowFitsItsBudget on figures rowbytes.go's table no longer states.
// A field of the log beside it is set in [worstLog].
func worstRow(t *testing.T) PendingRun {
	t.Helper()
	markup := func(n int) string { return strings.Repeat("<", n) }
	plain := func(n int) string { return strings.Repeat("a", n) }
	handles := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%064d", i)
		}
		return out
	}
	// EVERY OPTION'S ID AT THE THIRTY-TWO BYTES ITS PATTERN ALLOWS.
	options := make([]decision.Option, decision.MaxOptions)
	for i := range options {
		options[i] = decision.Option{ID: fmt.Sprintf("option-%025d", i), Label: markup(decision.MaxOptionLabel),
			Detail: markup(decision.MaxOptionDetail)}
	}
	evidence := make([]decision.Evidence, decision.MaxEvidence)
	for i := range evidence {
		evidence[i] = decision.Evidence{Kind: "link", Ref: markup(decision.MaxEvidenceRef),
			Label: markup(decision.MaxEvidenceLabel)}
	}
	// TEN NOTES (internal/agent/steer's MaxTurnNotes), each sealed at the
	// bound the store holds a note's seal to, each served to a job before it
	// was read, and read by a take that named itself — the engine's takes name
	// themselves by a fresh UUID — and was settled unanswered, and then by a
	// launch's brief that told it again, keeping that take's read beside its
	// own: every field of both reads at its widest.
	read := func() *NoteRead {
		return &NoteRead{At: worstAt, Order: math.MaxInt, Phase: "execute", Iteration: 99, Round: 99,
			Boundary: "tool_batch", Launch: uuid.NewString(), Take: uuid.NewString(), Unanswered: true}
	}
	notes := make([]RunNote, 10)
	for i := range notes {
		told := read()
		told.Retold = read()
		notes[i] = RunNote{ID: uuid.NewString(), Seq: i + 1, Sealed: []byte(plain(MaxNoteSealedBytes)), At: worstAt,
			Served: uuid.NewString(), Read: told}
	}
	capture := &SessionCapture{LaunchSeq: math.MaxInt64, CaptureSeq: math.MaxInt64, Launch: uuid.NewString(),
		State: CaptureUnresolved, Reason: markup(1 << 10), Through: new(math.MaxInt), BridgeSeq: math.MaxInt,
		BridgeOpen: make([]int, maxOpenPlaces), NoteCursor: math.MaxInt, ContextTokens: math.MaxInt64,
		At: worstAt, Harness: plain(32), HarnessVersion: plain(64), SessionID: plain(128), ArchiveFormat: 99,
		Object: plain(256), ObjectHash: plain(64), ObjectSize: math.MaxInt64, ContentKey: plain(256),
		Home: markup(1024), Workspace: markup(1024), Final: true, Interrupted: true,
		SeatBudget: math.MaxInt64, Minted: worstAt, Taker: plain(maxIncarnationBytes), TakerEpoch: math.MaxInt64,
		StandbyTaker: plain(maxIncarnationBytes), StandbyTakerEpoch: math.MaxInt64, StandbyAt: worstAt}
	for i := range capture.BridgeOpen {
		capture.BridgeOpen[i] = math.MaxInt
	}
	if err := capture.Bounded(); err != nil {
		t.Fatalf("the worst capture is past what the row carries: %v", err)
	}
	// THE DECISION THE HELD CALL CARRIES OUT ([HeldAct]), each field at the
	// widest its source admits: the question, which for one asked in words is
	// the question comment's whole body, at most internal/worklet's
	// MaxCommentBody (32 KiB, named here as a figure because that package
	// imports this one), in the bytes that escape the most; the option the
	// answer chose, its label and its id at theirs, and the act the engine
	// composed for the question ([delegated.Act]); who answered it, a login at
	// [iam.MaxLogin]; and the task, its key, the question's id and the act's
	// server each at the sixty-four bytes this model gives an identifier (an
	// SSH server's id is at most forty).
	acting := &HeldAct{Item: plain(64), Key: plain(64), Ask: plain(64), Question: markup(32 << 10),
		Choice: markup(decision.MaxOptionLabel), ChoiceID: options[0].ID,
		Act: &delegated.Act{Kind: delegated.Kind(plain(32)), Approve: options[0].ID, Digest: plain(64),
			Host: plain(64)},
		AnsweredBy: plain(iam.MaxLogin), AnsweredAt: worstAt}
	// THE DELEGATES THE CALL WAS PUT TO FIRST ([HeldDelegation], ADR-0125): a
	// panel of two, the widest a held call's own contract admits, which
	// refuses a third as internal/config refuses a rule naming one (checked
	// here against [HeldCall.validate], so a contract that widens the panel
	// fails this model until it is widened too); each delegate with its answer
	// at the bounds the verdict tool holds one to ([HeldVerdict]: its reason
	// and the most findings, in the bytes that escape the most), and the
	// escalation that put the call to people at the most reasons it carries
	// ([HeldEscalation]): each an engine word, none of which is longer than the
	// thirty-two bytes this model gives a closed word, and a sentence at its
	// bound.
	delegates := handles(2)
	if !admitsPanel(len(delegates)) || admitsPanel(len(delegates)+1) {
		t.Fatalf("a held call's own contract does not admit a panel of %d delegates and refuse one of %d: put "+
			"the worst held call to the widest panel HeldCall.validate admits", len(delegates), len(delegates)+1)
	}
	verdict := func(by string) HeldVerdict {
		findings := make([]string, MaxReviewFindings)
		for i := range findings {
			findings[i] = markup(MaxReviewFindingRunes)
		}
		return HeldVerdict{By: by, Verdict: plain(32), Confidence: plain(32), Reason: markup(MaxReviewReasonRunes),
			Findings: findings, At: worstAt}
	}
	given := make([]HeldVerdict, len(delegates))
	for i, by := range delegates {
		given[i] = verdict(by)
	}
	overruled, said := make([]string, maxEscalationReasons), make([]string, maxEscalationReasons)
	for i := range maxEscalationReasons {
		overruled[i], said[i] = plain(32), markup(maxEscalationRunes)
	}
	delegation := &HeldDelegation{Rule: math.MaxInt, Delegates: delegates, Digest: plain(maxHeldBindingBytes),
		Given: given, Escalated: &HeldEscalation{Overruled: overruled, Engine: said, At: worstAt}}
	// AND A FIXED CALL'S MARKS BESIDE THEM, which a call to an engine tool
	// carries: who may answer it, a closed word, and what its tool binds it to
	// and the identity of what it decides, each a digest at its bound
	// ([HeldCall.Binding], [HeldCall.Subject]).
	held := &HeldCall{ID: uuid.NewString(), LaunchID: uuid.NewString(), Server: plain(32), Tool: plain(128),
		Raw: plain(128), Digest: plain(64), Arguments: plain(MaxHeldArgsBytes - 2), Fixed: true,
		Approvers: plain(32), Binding: plain(maxHeldBindingBytes), Acting: acting, Act: plain(32),
		Subject: plain(maxHeldBindingBytes), Delegation: delegation, HeldAt: worstAt, CallingAt: worstAt,
		MadeAt: worstAt}
	held.Result = heldResultAtItsWorst(t, *held)
	// THE CALL A PERSON APPROVED, as the store records it on the held call
	// ([approvedCall]) and a reset carries it on to the log it is owed to
	// ([OwedCall]): KEPT FROM A CALL WITH EVERY FIELD SET ([everyField]), so
	// each field it carries is at its widest and each it leaves zero (its
	// arguments, their digest and size, its output and a size of it, an id a
	// harness gave it, its places and marks, a runner and a bound) is one
	// approvedCall or keptCall leaves out, never one the model did not hand it.
	approved := approvedCall(everyField(t, markup(4<<10)), *held)
	held.Call = &approved
	if err := HeldArgsFit(held.Arguments); err != nil {
		t.Fatalf("the worst held call's arguments are past their share: %v", err)
	}
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = uuid.NewString()
		}
		return out
	}
	answer := paddedTo(t, MaxAnswerBytes, RecordedAnswer{Sealed: []byte(plain(64 << 10)), Via: types.AnswerViaChat,
		EventIDs: ids(20), PostedAt: worstAt, RecordedAt: worstAt, From: StatusAwaiting, TakenAt: worstAt,
		LostClaims: math.MaxInt, FirstLostAt: worstAt}, func(a *RecordedAnswer, pad string) {
		a.Events = []json.RawMessage{json.RawMessage(`"` + pad + `"`)}
	})
	// A REFUSAL BESIDE IT, AT ITS BOUND, which the row never holds beside an
	// answer at its share (its own answer weighs a few KiB): one delivery's
	// ids at the most a drain hands one partition (internal/config's
	// `notification_coalesce_max_batch`, at most 100), its two figures at
	// their widest, and where it was posted at [MaxRefusalAddressBytes] in the
	// bytes that escape the most. And the round's refusals kept for its
	// review, as many as it keeps, each at its widest.
	address := map[string]string{"transport": ""}
	pad := MaxRefusalAddressBytes - addressBytes(address)
	address["transport"] = markup(pad/6) + plain(pad%6)
	if got := addressBytes(address); got != MaxRefusalAddressBytes {
		t.Fatalf("the worst address weighs %d, want its bound %d", got, MaxRefusalAddressBytes)
	}
	refusal := &AnswerRefusal{EventIDs: ids(100), Bytes: math.MaxInt, MaxBytes: math.MaxInt, Address: address}
	roundRefusals := make([]RoundRefusal, maxRoundRefusals)
	for i := range roundRefusals {
		roundRefusals[i] = RoundRefusal{Job: uuid.NewString(), Messages: math.MaxInt, Bytes: math.MaxInt,
			MaxBytes: math.MaxInt}
	}
	handBack := paddedTo(t, MaxAnswerBytes, []HandedBack{{ID: uuid.NewString(), Original: uuid.NewString()}},
		func(h *[]HandedBack, pad string) {
			(*h)[0].Event = json.RawMessage(`"` + pad + `"`)
		})
	launch := LaunchRecord{StartedAt: worstAt, Started: true, Model: markup(256), ProviderKey: plain(64),
		SignInID: uuid.NewString(), FollowUp: FollowUpReason(plain(32)), Brief: BriefKind(plain(32)),
		FreshSessionID: plain(128), SessionStart: SessionStart(plain(32)), ResumeCarry: ResumeCarry(plain(32)),
		Iteration: math.MaxInt, Published: true, CollectFailures: math.MaxInt, CollectFailingSince: worstAt,
		ResumeFailures: math.MaxInt, ResumeFailingSince: worstAt, Collected: true, CollectedAt: worstAt,
		// THE WIDEST WINDOW A RELEASE COUNTS ONTO THE RECORD, a day's: a
		// release writes none that is no window (BudgetWindow.Valid), and of
		// the three periods a day spells the widest pair, written as itself.
		RelaunchRefusedIn: BudgetWindow{Period: period.Day, Label: period.At(period.Day, worstAt, nil).Label},
		EndedAtDeadline:   true, HarnessEnd: HarnessEnd(plain(32)),
		Bridged: EngineSpend{Aux: AuxTokens{Input: math.MaxInt, Output: math.MaxInt, CacheRead: math.MaxInt,
			CacheWrite: math.MaxInt}, Workers: math.MaxInt, WorkerInput: math.MaxInt, WorkerOutput: math.MaxInt},
		Condensed: AuxTokens{Input: math.MaxInt, Output: math.MaxInt, CacheRead: math.MaxInt,
			CacheWrite: math.MaxInt}}
	// THE PARKED STATE AT ITS SHARE, whatever it holds: the park condenses a
	// state's closed rounds to fit and refuses one still past it
	// (internal/agent/execstate's EncodeWithin), and what no park condenses —
	// the turn's own context, how its conversation's earlier turns ended among
	// it (Frozen.Endings, up to 24,000 bytes as the state holds them) — is
	// inside this share, never beside it.
	state := `"` + plain(ExecuteStateBytes-2) + `"`
	// THE TURN'S READ OF ITS CONVERSATION at its bound
	// ([ConversationBaseMaxBytes]), every field of it set and the whole grown
	// to the bound through the section's own carried keys, so the row's
	// arithmetic holds any base within its bound, as it does an answer within
	// its share: internal/engine's TestAFrozenReadFitsItsBound holds the
	// largest read the executor log admits, frozen as a turn's first launch
	// freezes it, under the bound.
	base := paddedTo(t, ConversationBaseMaxBytes, ConversationBase{ReadThrough: math.MaxInt64,
		Absorbs: []string{plain(64)},
		Siblings: []BaseSibling{{Turn: plain(64), Object: plain(256), ObjectHash: plain(64),
			ObjectSize: math.MaxInt64}},
		Session: &BaseSession{Turn: plain(64), Object: plain(256), ObjectHash: plain(64), ObjectSize: math.MaxInt64,
			ContentKey: plain(256), Harness: plain(32), HarnessVersion: plain(64), SessionID: plain(128),
			ArchiveFormat: 99, Home: plain(1024), Workspace: plain(1024), ContextTokens: math.MaxInt64,
			Interrupted: true, CapturedAt: worstAt, WentOn: true, Lost: true, EndedAt: worstAt}},
		func(b *ConversationBase, pad string) {
			b.Extra = map[string]json.RawMessage{"pad": json.RawMessage(`"` + pad + `"`)}
		})
	// THE OWED END AS ITS WRITER PUTS IT ON THE ROW ([owedEnd]): every prose
	// field past its bound in markup, fitted to the executor log's, and every
	// other at the log's own bound.
	end, err := owedEnd(plain(64), executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		Decision: plain(executorkeep.WordMaxBytes), Delivered: true,
		DeliveredTo: plain(executorkeep.WordMaxBytes), Answer: plain(executorkeep.WordMaxBytes),
		DeliveryUnknown: true, CompletedWork: markup(1 << 20), BlockedOn: markup(1 << 20),
		StoppedBy: markup(executorkeep.ActorMaxBytes), StopReason: markup(1 << 20), Failure: markup(1 << 20),
		Unkept: &executorkeep.Unkept{LaunchSeq: math.MaxInt64, CaptureSeq: math.MaxInt64,
			Reason: markup(1 << 20)}})
	if err != nil {
		t.Fatalf("the worst owed end: %v", err)
	}
	// WHAT THE JOB MAY DO ON GITLET: the most repositories a job is
	// given ([MaxRunRepos]), each at the widest its grammar admits
	// ([RunRepo.ValidateFor]) — a name of [gitref.MaxRepoNameBytes], a whole
	// commit id, a branch of the longest a ref allows in the bytes that
	// escape the most, the most cone paths at their longest, and an ask to
	// open a change into another such branch under a title at its bound
	// ([MaxChangeTitleBytes]); a change it works on is never beside that ask,
	// and weighs less.
	repos := make([]RunRepo, MaxRunRepos)
	for i := range repos {
		paths := make([]string, MaxConePaths)
		for j := range paths {
			paths[j] = fmt.Sprintf("%03d", j) + markup(MaxConePathBytes-3)
		}
		repos[i] = RunRepo{Repo: fmt.Sprintf("%063d", i), RepoID: uuid.NewString(),
			Base: strings.Repeat("f", gitref.OIDHexLen), Branch: fmt.Sprintf("%d", i) + markup(gitref.MaxBranchBytes-1),
			Access: RepoWrite, Paths: paths,
			Open: &OpenChange{Target: "refs/heads/" + markup(gitref.MaxBranchBytes), Title: markup(MaxChangeTitleBytes),
				Draft: true}}
		if err := repos[i].ValidateFor(CloneCoding); err != nil {
			t.Fatalf("the worst repository is not one a job may be given: %v", err)
		}
	}
	// THE TURN'S HOLDS, the most a turn may take ([MaxCodeHolds]), each a
	// seat branch's key at Gitlet's fixed width (internal/gitlet's
	// RefHoldKey, 69 bytes, the wider of the two kinds); and its floors, a
	// statelog position's token each — a stream's name, under 64 bytes, and
	// two 64-bit numbers at their widest — which the engine mints and this
	// package carries unread.
	holds := make([]string, MaxCodeHolds)
	for i := range holds {
		holds[i] = uuid.NewString() + ":" + fmt.Sprintf("%032x", i)
	}
	floor := fmt.Sprintf("%s@%d:%d", plain(64), uint64(math.MaxUint64), uint64(math.MaxUint64))
	// WHO THE TURN WORKS FOR ([types.WorkingFor], ADR-0109), on the row and on
	// the colleague's ask it answers: every field set, a seat's handle and a
	// credential's login each at [iam.MaxLogin] beside every app, though one
	// names a seat or a credential and never both. Beside it on the row, the
	// credential that woke the turn ([PendingRun.RequesterLogin]), kept beside
	// a requester seat here though the row keeps it only where it names none,
	// and the answers whose deliveries woke the turn ([PendingRun.Answered]):
	// one comment's id for each delivery one drain hands a partition
	// (internal/config's `notification_coalesce_max_batch`, at most 100, which
	// bounds a digest's constituents too), each at the sixty-four bytes this
	// model gives an identifier.
	workingFor := func() *types.WorkingFor {
		return &types.WorkingFor{Kind: types.WorkingForKind(plain(32)), Seat: plain(iam.MaxLogin),
			Login: plain(iam.MaxLogin), Apps: slices.Clone(iam.Apps)}
	}
	run := PendingRun{
		TurnID: plain(64), WorkKey: plain(64), WorkSince: worstAt,
		AgentHandle: plain(64), AgentID: uuid.NewString(), Role: markup(128),
		SandboxID: plain(128), Harness: plain(32), Placement: plain(64), CommandID: plain(128),
		Status: StatusAnswered, LaunchID: uuid.NewString(), LaunchSeq: math.MaxInt64,
		BoxSince: worstAt, Capture: capture, Kept: capture, Base: &base,
		ExhaustedSessions: slices.Repeat([]string{plain(executorkeep.SessionIDMaxBytes)},
			executorkeep.ExhaustedMaxSessions),
		OwedEnd: end, EndRecorded: true,
		Launch: launch, Owner: plain(maxIncarnationBytes), OwnerEpoch: math.MaxInt64,
		TaskDescription: keptTitle(markup(1 << 20)), Label: keptTitle(markup(1 << 20)),
		Reply: plain(64), PartitionKey: markup(512), ConversationKey: markup(512),
		Branch: markup(gitref.MaxBranchBytes), SessionID: plain(128),
		Question: markup(decision.MaxQuestion), QuestionContext: markup(MaxAskContextBytes),
		Decision: &decision.Decision{Question: markup(decision.MaxQuestion), Options: options,
			Recommended: options[0].ID, Rationale: markup(decision.MaxRationale), Evidence: evidence,
			Role: decision.RoleApprover, Inform: &decision.Inform{Surface: decision.InformMattermost,
				Channel: markup(decision.MaxInformChannel)}},
		Audience:          markup(MaxAskToBytes),
		HeldCall:          held,
		OwedCall:          &OwedCall{Round: uuid.NewString(), Call: approved},
		ParkedInputTokens: math.MaxInt, ParkedOutputTokens: math.MaxInt,
		ParkedCacheReadTokens: math.MaxInt, ParkedCacheWriteTokens: math.MaxInt,
		AskedAt: worstAt, Answer: &answer, AnswerRefusal: refusal, DeclinedAnswers: handles(maxDeclinedAnswers),
		HandBack: handBack, EndingReturnsRefusal: true, RoundRefusals: roundRefusals,
		Ending: &RecordedEnding{ID: uuid.NewString(), At: worstAt, Reason: plain(64), Detail: markup(1 << 10),
			ErrorKind: plain(32), Returned: true, Unused: true, Reclaim: true},
		Stop: &RunStop{By: markup(128), ByKind: plain(32), OperatorID: plain(64), Reason: markup(4 * 500),
			Collected: true, InputTokens: math.MaxInt, OutputTokens: math.MaxInt, CacheReadTokens: math.MaxInt,
			CacheWriteTokens: math.MaxInt, Condensed: AuxTokens{Input: math.MaxInt, Output: math.MaxInt,
				CacheRead: math.MaxInt, CacheWrite: math.MaxInt}},
		// THE LONGEST TRACKER A WORK ITEM LIVES IN, `github` and `gitlab`.
		WorkItem: &types.WorkItem{Backend: types.WorkGitHub, ID: plain(64), Key: plain(64),
			Project: plain(64)},
		Answered:        handles(100),
		AudienceHandles: handles(MaxAudienceHandles), AudienceFallback: true, Requester: plain(64),
		RequesterLogin: plain(iam.MaxLogin), RequesterApps: slices.Clone(iam.Apps), WorkingFor: workingFor(),
		ColleagueAsk: &ColleagueAsk{ChannelID: plain(64), Question: keptTitle(markup(1 << 20)),
			Chain: handles(32), TraceID: plain(32), SpanID: plain(16), ParentSpanID: plain(16),
			WorkingFor: workingFor()},
		TraceID: plain(32), SpanID: plain(16), DelegationDepth: math.MaxInt, DelegationChain: handles(32),
		ExecuteState: json.RawMessage(state), Charged: true, CompanyCharged: true,
		CodeHolds: holds, Repos: repos, CodeFloor: floor, CodeLaunch: floor, CodeCeiling: plain(64),
		PauseTTLSeconds: math.MaxFloat64, PausedAt: worstAt, Deadline: worstAt, LaunchNode: plain(64),
		Lost: &JobLoss{Cause: LossCause(plain(32)), Node: plain(64), At: worstAt}, RoundLosses: math.MaxInt,
		Round: uuid.NewString(), AnsweredJob: uuid.NewString(),
		HeldFor: Condition(plain(32)), HeldUntil: worstAt,
		Steer:     RunSteer{Seat: []byte(plain(512)), Seq: math.MaxInt, Notes: notes, Pulled: uuid.NewString(), Reads: math.MaxInt},
		CreatedAt: worstAt, UpdatedAt: worstAt,
	}
	return run
}

// worstLog is a run's bridged call log with every field at its bound, each
// built in the bytes that escape the most and through the code that writes it
// ([BridgeLog.appendCall]): two hundred of the worst calls kept and a hundred
// dropped, by names that escape the most, so the tally of their identities
// folds to its last step; each call's own work tallied the same way, so the
// identities it made fold too; the most tool names the turn's earlier rounds
// fold to ([MaxPriorCalls]); the log's identifiers — its job, its round, the
// answer it was cut for — at their widest; its count of the late calls it
// dropped ([BridgeLog.LateElided]) at the most it can be, every call it drops a
// late one, as every call but the newest submission is; every kind of change
// a call can commit, on every call and so, through the append, in the log's
// union of what its calls committed ([BridgeLog.Committed]); and the most
// calls in flight it holds ([MaxCallsInFlight]), each entry at its widest
// ([worstEntry]). EACH CALL HANDED WHAT NO FINISHED CALL KEEPS besides, a
// runner and a bound, and its own work tallied with its failures, so a field
// the log leaves zero is one the append leaves out, never one the model did
// not hand it ([zeroInTheWorstLog] names each).
//
// A FIELD ADDED TO THE LOG IS SET HERE AT ITS BOUND, in the change that adds it:
// one of the calls' comes out of [BridgeLogBytes], and any other out of the
// room [RowBytes] leaves above it. TestTheWorstLogSetsEveryField holds this to
// that, failing on any field of the log left zero here that
// [zeroInTheWorstLog] does not name.
func worstLog(t *testing.T) BridgeLog {
	t.Helper()
	bridged := BridgeLog{Launch: uuid.NewString(), Round: uuid.NewString(), CutFor: uuid.NewString(),
		AnsweredThrough: math.MaxInt, PriorCalls: make([]string, MaxPriorCalls), PriorCallsElided: true}
	for i := range bridged.PriorCalls {
		// A SUCCESSFUL call's name, which the surface resolved: a tool's
		// name, held to the MCP name grammar (internal/mcp's toolnames.go),
		// so it escapes to itself.
		bridged.PriorCalls[i] = fmt.Sprintf("%0128d", i)
	}
	for i := range MaxBridgeCalls + elidedWorst {
		call := worstCall(t, fmt.Sprintf("tool_%04d_%s", i, strings.Repeat("<", 300)))
		call.Seq = i + 1
		call.Failed = i%2 == 0
		call.Runner, call.Until = strings.Repeat("<", 4<<10), worstAt
		if i == MaxBridgeCalls+elidedWorst-1 {
			// AND NOT LATE: the whole share is never a late submission's
			// ([submitted]), so the log's worst is its own job's.
			call.Name, call.Failed, call.Late = SubmitWorkTool, false, false
		}
		worker := []ElidedCall{{Name: fmt.Sprintf("worker_%04d_%s", i, strings.Repeat("<", 300)),
			Digest: fmt.Sprintf("%064x", i), Calls: 1, Failed: 1}}
		bridged.appendCall(call, worker)
	}
	for i := range MaxCallsInFlight {
		bridged.InFlight = append(bridged.InFlight, worstEntry(t, i))
	}
	return bridged
}

// elidedWorst is how many calls the worst log drops.
const elidedWorst = 100

// THE WHOLE ROW FITS ITS BUDGET: the arithmetic in rowbytes.go, held to the
// row's own encoder. Every field is at its bound in the bytes that escape the
// most — the three a person or a tool supplies included, a recorded answer and
// the copies of a let-go one each at its share and a held call's sealed
// arguments at theirs, the turn's read of its conversation at its bound, and
// what the job may do on Gitlet at the most its writers take — and the
// parked state is at its share. The state weighs no more than
// [ExecuteStateBytes] and the rest no more than [RestBytes] less the room left
// for what a later build adds; the whole, with that room, no more than
// [RowBytes]. And the handed set kept beside the row, at the most a launch
// keeps, sealed, is a value well within [RowBytes] too.
//
// AND THE FIGURES THE TABLE STATES ARE THE ONES THE ROW WEIGHS: every other
// field ([measuredOtherFields]) and what the rest's share leaves to spare
// ([measuredSpare]), compared exactly, since the worst row weighs the same on
// every run: its ids and its held result's seal are new on every run, each of
// a fixed width, its instants are one, and its keys are written in order. A
// figure only read, never compared, went stale the first time the row grew
// and nothing said so; pinned, a change that moves either, a field the row
// gains above all, restates both and the table in the change that moves them.
//
// Mutation: raise MaxAnswerBytes, MaxHeldArgsBytes or ConversationBaseMaxBytes
// past what the rest has room for, raise MaxRunRepos, MaxConePaths or
// MaxCodeHolds far enough, or let a note's seal or a held call's result grow,
// and the rest overruns its share; and raise maxIncarnationBytes to 700,000,
// and the row's incarnations at that bound — its owner's, and the taker and the
// standby's taker of each capture it holds — overrun it too.
// Grow or shrink a note's seal by a byte, and every other field no longer
// weighs the figure the table states; grow or shrink the turn's read of its
// conversation by a byte, and the rest no longer leaves the spare it states.
func TestTheWholeRowFitsItsBudget(t *testing.T) {
	t.Parallel()
	run := worstRow(t)
	whole := encoded(t, run)

	withoutState := run
	withoutState.ExecuteState = nil
	stateBytes := len(run.ExecuteState)
	rest := encoded(t, withoutState)

	withoutSource := withoutState
	withoutSource.Answer, withoutSource.HandBack, withoutSource.Base = nil, nil, nil
	held := *withoutSource.HeldCall
	held.Arguments = ""
	withoutSource.HeldCall = &held
	others := encoded(t, withoutSource)
	spare := RestBytes - ExtraRowBytes - rest
	t.Logf("the row weighs %d of its %d: the state %d of %d, the rest %d of %d with %d left for a later "+
		"build — of which a recorded answer, its hand-back, a held call's arguments and the turn's read of "+
		"its conversation %d, and every other field %d — and %d to spare", whole, RowBytes, stateBytes,
		ExecuteStateBytes, rest, RestBytes, ExtraRowBytes, rest-others, others, spare)

	if others != measuredOtherFields || spare != measuredSpare {
		t.Errorf("every other field weighs %d with %d to spare, and rowbytes.go states %d with %d: restate "+
			"measuredOtherFields, measuredSpare and the lines of the table that state them", others, spare,
			measuredOtherFields, measuredSpare)
	}
	if stateBytes > ExecuteStateBytes {
		t.Errorf("the parked state weighs %d, past its share %d", stateBytes, ExecuteStateBytes)
	}
	if rest > RestBytes-ExtraRowBytes {
		t.Errorf("the rest weighs %d, past its share %d less the %d left for a later build", rest, RestBytes,
			ExtraRowBytes)
	}
	if whole+ExtraRowBytes > RowBytes {
		t.Errorf("the row weighs %d, and with a later build's %d past its %d", whole, ExtraRowBytes, RowBytes)
	}
	if ExecuteStateBytes+RestBytes != RowBytes {
		t.Error("the row's two shares do not add up to its budget")
	}

	// THE HANDED SET, the third value: the most secrets a launch keeps
	// ([MaxHandedBytes]), sealed and written as one string — a third more,
	// and the seal's own bytes.
	handed, err := json.Marshal(handedSet{Launch: uuid.NewString(), LaunchSeq: math.MaxInt64,
		Sealed: strings.Repeat("a", MaxHandedBytes*4/3+1<<10)})
	if err != nil {
		t.Fatal(err)
	}
	if len(handed) > RowBytes {
		t.Errorf("the handed set weighs %d, past the %d a value may", len(handed), RowBytes)
	}
}

// zeroInTheWorstRow is every field of the row [worstRow] leaves zero, by its
// path ([zeroOnTheWire]), and why the row never carries it beside the rest at
// their worst: the code that writes the row leaves it out of a value the model
// hands that code with the field set, or the row holds it apart from a field
// that weighs more, which the worst row sets. Each of the first kind names the
// code that leaves it out, so the code ceasing to is a field the walk finds
// set, and its name a reason that no longer holds.
var zeroInTheWorstRow = func() map[string]string {
	zero := map[string]string{
		"Repos[0].Change": "a change the job works on is never beside an ask to open one (RunRepo.ValidateFor " +
			"refuses the two together), and the ask, which the worst row sets, weighs more",
		"Steer.Notes[0].Read.Retold.Retold": "a brief that tells a note again keeps the marked take's read " +
			"beside its own (openNotes), and that read is never one a brief told: only a read marked unanswered " +
			"is told again, a settle marks only a read naming its take, and a read a brief told names none",
	}
	// THE CALL A PERSON APPROVED, on the held call and owed to the log
	// ([OwedCall]), as approvedCall keeps it from a call with every field set
	// ([everyField]).
	for field, why := range map[string]string{
		"Args": "its arguments ride the row sealed on the held call, and neither it nor the log keeps any of " +
			"a withheld call's (approvedCall, keptCall, ADR-0077)",
		"ArgsBytes": "nor their size (keptCall)",
		"Digest": "nor a digest of them, which would confirm a guess at what the seal hides (approvedCall, " +
			"keptCall)",
		"Output": "its result rides the row sealed on the held call (approvedCall)",
		"OutputBytes": "nor a size of it, which beside no output would read as a whole output cut to nothing " +
			"(approvedCall)",
		"CallID":    "no harness named it: the resume made it on the engine host (approvedCall)",
		"Held":      "it was made, so it is marked approved and never held (approvedCall)",
		"Seq":       "it has no place until the log takes it and places it (approvedCall, BridgeLog.withApproved)",
		"Appended":  "nor a number, which the log gives it as it takes it (approvedCall, BridgeLog.appendCall)",
		"Late":      "the store appends it for its own round, so it is never late (approvedCall)",
		"Abandoned": "no harness gave it up: the resume made it on the engine host (approvedCall)",
		"Runner":    "nobody waits for a finished call, so it keeps no runner (keptCall)",
		"Until":     "nor a bound to wait for it until (keptCall)",
	} {
		zero["HeldCall.Call."+field], zero["OwedCall.Call."+field] = why, why
	}
	return zero
}()

// THE WORST ROW SETS EVERY FIELD THE ROW CARRIES. TestTheWholeRowFitsItsBudget
// fails only on a row past its shares, and a field [worstRow] leaves out makes
// the row it measures smaller, so the rule the model states, that a field the
// row gains is set there at its bound in the change that adds it, was held by
// nothing: the decision a held call carries out and the delegates it was put
// to, who the turn works for, the credential and the answers that woke it, and
// the ending's error kind each joined the row and none of them the model. So
// every field the row's encoder writes is set in the worst row
// ([zeroOnTheWire]), but those [zeroInTheWorstRow] names, each with why the
// row never carries it beside the rest at their worst; and each it names is
// zero there, so a reason that stops holding is struck rather than left to
// hide a field.
//
// Mutation: leave the job's record without how its harness ended, a note
// without the job it was served to, the held call without the delegates it was
// put to, or the refusal's address with a second key and no value, and the
// row has a field nothing names; name a field the worst row sets, and the
// list gives a reason that does not hold; let approvedCall keep the held mark
// or the output's size of the call it is handed, or keptCall a finished
// call's runner, and a reason the list names no longer holds; widen the panel
// a held call's contract admits (panel-widened), or put the worst held call to
// a narrower one (panel-of-one), and the model is not at its worst.
func TestTheWorstRowSetsEveryField(t *testing.T) {
	t.Parallel()
	everyFieldSet(t, "row", worstRow(t), zeroInTheWorstRow)
}

// THE WHOLE LOG FITS ITS BUDGET: the log is a value of its own beside the row,
// held to [RowBytes] as the row is, and its calls to their share of it. Two
// hundred of the worst calls beside both tallies at their shares weigh no more
// than [BridgeLogBytes], and the whole log, with the most tool names the turn's
// earlier rounds fold to and its identifiers at their widest, no more than
// [RowBytes] — what is left above it being the room a later build adds keys to
// the log in. Its calls carry every kind of change a call can commit, and its
// union of them holds every kind. AND [MaxCallsInFlight] ENTRIES IN FLIGHT, each
// at its widest, weigh no more than their line [InFlightBytes], inside the
// whole: the bound a call's write ahead of it is held to measured where it
// counts, beside every other field of the log at its worst.
//
// Mutation: keep a call's output whole, or raise MaxBridgeCalls, and the calls
// overrun their share; raise MaxPriorCalls far enough, and the log overruns
// its value; stop the append folding a call's kinds into the log's union, and
// the log is not measured at its worst; keep an entry's runner whole
// (entry-keeps-its-runner-whole), and the entries overrun their line.
func TestTheWholeLogFitsItsBudget(t *testing.T) {
	t.Parallel()
	bridged := worstLog(t)
	whole := logEncoded(t, bridged)
	withoutCalls := bridged
	withoutCalls.Calls, withoutCalls.Elided, withoutCalls.ElidedCalls, withoutCalls.AbsorbedCalls = nil, 0, nil, nil
	withoutInFlight := withoutCalls
	withoutInFlight.InFlight = nil
	others := logEncoded(t, withoutInFlight)
	inFlight := logEncoded(t, withoutCalls) - others
	calls := whole - others - inFlight
	t.Logf("the log weighs %d of the %d a value may: its calls and their tallies %d of %d, its %d calls in "+
		"flight %d of %d, and every other field %d — the turn's earlier rounds' tool names among them — with "+
		"%d left for a later build", whole, RowBytes, calls, BridgeLogBytes, MaxCallsInFlight, inFlight,
		InFlightBytes, others, RowBytes-whole)

	if calls > BridgeLogBytes {
		t.Errorf("the log's calls weigh %d, past their share %d", calls, BridgeLogBytes)
	}
	if inFlight > InFlightBytes {
		t.Errorf("the log's %d calls in flight weigh %d, past their line %d", MaxCallsInFlight, inFlight,
			InFlightBytes)
	}
	if whole > RowBytes {
		t.Errorf("the log weighs %d, past the %d a value may", whole, RowBytes)
	}
	// EVERY DROPPED CALL COUNTED, the tally folded as far as its share made it
	// fold: names that escape the most leave none of them a tool, so the
	// record is incomplete for a reader of whether the turn replied. And the
	// calls the log's calls made, each counted.
	if len(bridged.Calls) != MaxBridgeCalls || bridged.Elided != elidedWorst ||
		elidedCount(bridged.Unlisted()) != elidedWorst || bridged.ElisionKnown() {
		t.Errorf("the log keeps %d calls with %d dropped and %d counted (known %v), want %d with %d "+
			"counted and the elision unknown", len(bridged.Calls), bridged.Elided,
			elidedCount(bridged.Unlisted()), bridged.ElisionKnown(), MaxBridgeCalls, elidedWorst)
	}
	if bridged.LateElided != elidedWorst {
		t.Errorf("the log counts %d of its %d dropped calls late, want every one: the count is measured at "+
			"the most it can be", bridged.LateElided, elidedWorst)
	}
	if !slices.Equal(bridged.Committed, everyKind()) {
		t.Errorf("the log's union of what its calls committed holds %d kinds, want every one of the %d: the "+
			"union is measured at its most", len(bridged.Committed), len(types.CommittedChanges))
	}
	if n := elidedCount(bridged.Absorbed()); n != MaxBridgeCalls+elidedWorst {
		t.Errorf("the calls' own work is counted %d times, want %d", n, MaxBridgeCalls+elidedWorst)
	}
	if n := tallyBytes(bridged.AbsorbedCalls, absorbedTally); n > MaxAbsorbedIdentityBytes {
		t.Errorf("the absorbed tally weighs %d, past its %d", n, MaxAbsorbedIdentityBytes)
	}
}

// zeroInTheWorstLog is [zeroInTheWorstRow] for the log beside the row
// ([worstLog]).
var zeroInTheWorstLog = func() map[string]string {
	withheld := "the worst call is neither held nor approved: a withheld call keeps none of its arguments " +
		"(BridgeCall.Withheld), so it weighs less (worstCall)"
	finished := "nobody waits for a finished call, so the log keeps it with neither the runner nor the bound " +
		"it was handed (keptCall, on every append)"
	folded := "the worst log's dropped calls are named in the bytes that escape the most, so the tally of " +
		"them folds to its entry with no name, which sorts first (fold): the elision TestTheWholeLogFitsItsBudget " +
		"reads as unknown, with the tally's bound held by the fold and counted at its share in rowbytes.go"
	absorbed := "the calls' own work is tallied the same way, and " + folded
	zero := map[string]string{
		"Calls[0].Held":           withheld,
		"Calls[0].Approved":       withheld,
		"Calls[0].Runner":         finished,
		"Calls[0].Until":          finished,
		"ElidedCalls[0].Name":     folded,
		"ElidedCalls[0].Digest":   folded,
		"AbsorbedCalls[0].Name":   absorbed,
		"AbsorbedCalls[0].Digest": absorbed,
		"AbsorbedCalls[0].Failed": "the log takes a call's own work in by its tool and identity alone, never " +
			"its failures (appendCall)",
	}
	// AN ENTRY IN FLIGHT, as keptEntry keeps it from a call with every field
	// set ([worstEntry]).
	for _, field := range []string{"Args", "ArgsBytes", "Output", "OutputBytes", "Failed", "Appended", "Late",
		"Abandoned", "DurationMS", "Origin", "Server", "Source", "Refusal", "Held", "Approved", "HeldID",
		"Committed"} {
		zero["InFlight[0]."+field] = "an entry keeps its call's identity alone (keptEntry), and its one update " +
			"brings the rest"
	}
	return zero
}()

// THE WORST LOG SETS EVERY FIELD THE LOG CARRIES, for the reason the worst row
// does (TestTheWorstRowSetsEveryField): the log beside the row is measured by a
// model of its own ([worstLog]) under the same rule, which nothing held it to
// either. Its calls and its entries in flight are walked as the log keeps
// them: each entry kept from a call with every field set, and each call
// appended with a runner and a bound and its own work with its failures, so a
// field either leaves zero is one keptEntry or the append leaves out.
//
// Mutation: leave the log without its mark that the earlier rounds' tool names
// are incomplete, and the log has a field nothing names; let keptEntry keep a
// call's origin, keptCall a finished call's runner, or the append the failures
// of a call's own work, and the list names a field the worst log sets.
func TestTheWorstLogSetsEveryField(t *testing.T) {
	t.Parallel()
	everyFieldSet(t, "log", worstLog(t), zeroInTheWorstLog)
}

// logEncoded is what log weighs as the store's encoder writes it.
func logEncoded(t *testing.T, bridged BridgeLog) int {
	t.Helper()
	raw, err := json.Marshal(bridged)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

// encoded is what run weighs as the row's encoder writes it.
func encoded(t *testing.T, run PendingRun) int {
	t.Helper()
	raw, err := encodeRun(run)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

// everyFieldSet fails t on every field the worst value of a model leaves zero
// ([zeroOnTheWire]) that named does not name, and on every field named names
// that the value does not leave zero: a reason that stopped holding, struck
// rather than left to hide the field it names.
func everyFieldSet(t *testing.T, model string, v any, named map[string]string) {
	t.Helper()
	zero := zeroOnTheWire(reflect.ValueOf(v), "")
	for _, path := range zero {
		if _, ok := named[path]; !ok {
			t.Errorf("the worst %s leaves %s zero: set it at its bound in the model, in the change that adds "+
				"it, or name it with why the %s never carries it beside the rest at their worst", model, path,
				model)
		}
	}
	for _, path := range slices.Sorted(maps.Keys(named)) {
		if !slices.Contains(zero, path) {
			t.Errorf("the worst %s does not leave %s zero, which is named as a field it does: strike the name, "+
				"whose reason no longer holds or which names no field", model, path)
		}
	}
}

// zeroOnTheWire is the path of every field of v the store's encoder writes that
// v leaves at its zero value: every exported field whose JSON key is not "-",
// followed through pointers, structs, the first element of each slice and every
// value of each map, and named as Go names it (`HeldCall.Call.Args`,
// `Steer.Notes[0].Read`, `AnswerRefusal.Address[transport]`). A nil pointer, an
// empty slice or map and a zero value are one path each, all of what is under
// them being left out; a struct with no field of its own on the wire (an
// instant) and a run of bytes are one value each.
//
// A FIELD A MODEL LEAVES ZERO IS A FIELD IT LEAVES OUT: the value it measures is
// smaller than the one the store can hold by that field's weight, and no share
// is past its bound for it, so only a walk of the type finds it.
func zeroOnTheWire(v reflect.Value, path string) []string {
	named := func(field string) string {
		if path == "" {
			return field
		}
		return path + "." + field
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return []string{path}
		}
		return zeroOnTheWire(v.Elem(), path)
	case reflect.Struct:
		var zero []string
		carried := false
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if key, _, _ := strings.Cut(field.Tag.Get("json"), ","); !field.IsExported() || key == "-" {
				continue
			}
			carried = true
			zero = append(zero, zeroOnTheWire(v.Field(i), named(field.Name))...)
		}
		if !carried && v.IsZero() {
			return []string{path}
		}
		return zero
	case reflect.Slice:
		switch {
		case v.Len() == 0:
			return []string{path}
		case v.Type().Elem().Kind() == reflect.Uint8:
			return nil
		}
		return zeroOnTheWire(v.Index(0), path+"[0]")
	case reflect.Map:
		if v.Len() == 0 {
			return []string{path}
		}
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return cmp.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
		var zero []string
		for _, key := range keys {
			zero = append(zero, zeroOnTheWire(v.MapIndex(key), fmt.Sprintf("%s[%v]", path, key))...)
		}
		return zero
	}
	if v.IsZero() {
		return []string{path}
	}
	return nil
}

// AN ANSWER BY TURN AT ITS SOURCE'S BOUND FITS A RECORDED ANSWER'S SHARE. An
// answer by turn has no ordinary message to fall back on, so one the row could
// not carry would be one a person was told was taken and the run never was:
// `answer_executor` and the record both hold an answer to a page
// ([MaxAnswerTextBytes], which is MaxAskContextBytes), and that page at its
// worst — every byte one the encoder escapes six-fold, sealed with the longest
// names, and carried again as the delivery it came in — fits [MaxAnswerBytes],
// as the row holds it and as the copy a let-go of it would owe.
//
// Mutation: raise MaxAnswerTextBytes past what a recorded answer's share
// holds, or shrink the share under it, and this goes red.
func TestAnAnswerByTurnAtItsSourcesBoundFitsARecordedAnswersShare(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: strings.Repeat("t", 64), LaunchID: uuid.NewString()}
	given := types.ExecutorAnswerGiven{TurnID: run.TurnID, LaunchID: run.LaunchID,
		Answer: strings.Repeat("<", MaxAnswerTextBytes), AnsweredBy: strings.Repeat("<", 128),
		AnsweredByKind: strings.Repeat("k", 32), OperatorID: strings.Repeat("o", 64)}
	trigger := events.New(given, events.TraceContext{})
	words, answer, err := answerGivenOf(run, given, trigger, worstAt)
	if err != nil {
		t.Fatal(err)
	}
	answer = sealedOnto(t, rigAnswers(), run, words, answer)
	if err := answerFits(run.TurnID, answer); err != nil {
		t.Errorf("an answer by turn at its bound does not fit a recorded answer's share: %v", err)
	}
	raw, _ := json.Marshal(answer)
	t.Logf("an answer by turn at its bound weighs %d of the %d a recorded answer may", len(raw), MaxAnswerBytes)
}

// AN ANSWER IS MEASURED AS THE ROW WILL HOLD IT. The record and the steps after
// it write onto a recorded answer the status it took the run out of and the
// claims that take it or lose it — when it was taken, when it was first lost
// and how many times — none of which the reply handed over carries: so the
// share is judged with each of them at its widest. An answer at its share with
// all four written fits it; one at its share without any ONE of them is past
// it once the row writes that one, and is refused before it is recorded.
//
// Mutation: measure the answer without any one of the four, and an answer at
// its share without that one is recorded, to outgrow the share when the step
// that writes it lands.
func TestAnAnswerIsMeasuredAsTheRowWillHoldIt(t *testing.T) {
	t.Parallel()
	pad := func(a *RecordedAnswer, pad string) { a.Events = []json.RawMessage{json.RawMessage(`"` + pad + `"`)} }
	handed := RecordedAnswer{Sealed: []byte("words"), Via: types.AnswerViaChat, EventIDs: []string{"e1"},
		PostedAt: worstAt, RecordedAt: worstAt}
	// EACH AT ITS WIDEST: the longest status an answer takes a run out of, an
	// instant at its widest and the largest count.
	written := func() RecordedAnswer {
		a := handed
		a.From = slices.MaxFunc(Awaiting, func(x, y string) int { return cmp.Compare(len(x), len(y)) })
		a.TakenAt, a.FirstLostAt, a.LostClaims = widestInstant, widestInstant, math.MaxInt
		return a
	}
	if err := answerFits("t1", paddedTo(t, MaxAnswerBytes, written(), pad)); err != nil {
		t.Errorf("an answer at its share with every one of them written = %v, want it to fit", err)
	}
	for name, unset := range map[string]func(*RecordedAnswer){
		"the status it took the run out of": func(a *RecordedAnswer) { a.From = "" },
		"when it was taken":                 func(a *RecordedAnswer) { a.TakenAt = time.Time{} },
		"when it was first lost":            func(a *RecordedAnswer) { a.FirstLostAt = time.Time{} },
		"how many times it was lost":        func(a *RecordedAnswer) { a.LostClaims = 0 },
	} {
		short := written()
		unset(&short)
		if err := answerFits("t1", paddedTo(t, MaxAnswerBytes, short, pad)); !errors.Is(err, ErrAnswerTooLarge) {
			t.Errorf("an answer at its share without %s = %v, want ErrAnswerTooLarge: the row writes it later",
				name, err)
		}
	}
}

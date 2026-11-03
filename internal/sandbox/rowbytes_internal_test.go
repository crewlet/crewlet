package sandbox

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/mcp"
)

// worstAt is an instant at the widest the row's encoder writes one: nine
// digits of fraction and a zone that is not UTC.
var worstAt = time.Date(2026, 11, 3, 23, 59, 59, 999_999_999, time.FixedZone("x", 5*3600+1800))

// worstCall is one bridged call with every field at its widest, the payloads
// far past what the row keeps and in the bytes that escape the most: a name
// past its bound; arguments holding a body of markup beside the identifiers
// that say where it went, and forty notes of markup a few hundred bytes each,
// so what is kept once the body is left out is as close to its share as
// whole notes come; and an output of markup.
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
		Failed: true, At: worstAt, Seq: math.MaxInt, StartedAt: worstAt, DurationMS: math.MaxInt64,
		Origin: "mcp:" + server, Server: server, Source: "mcp:" + server,
		Refusal: string(mcp.RefusalReassignmentBudget), Held: true, Approved: true,
		Digest: strings.Repeat("f", 64),
	}
}

// A BRIDGED CALL IS HELD TO ITS SHARE OF THE ROW, AT ITS WORST. Every field of a
// call kept by the row, each at the widest the row's encoder writes it — a name
// past its bound in markup, arguments and an output of a megabyte of markup —
// weighs no more than [MaxBridgeCallBytes] less the room left for what a later
// build adds to a call; the newest successful submission no more than
// [MaxSubmissionCallBytes] less the same. And the parts are each within the
// figure rowbytes.go states for them, so the arithmetic there is this test's.
//
// Mutation: keep the arguments or the output whole, or measure a cut by the
// payload's own length rather than escaped, and the call overruns its share;
// leave the name uncut, and the name does.
func TestABridgedCallIsHeldToItsShare(t *testing.T) {
	t.Parallel()
	const reserve = 1958 // what rowbytes.go leaves a later build in one call
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
		case fixed > 600:
			t.Errorf("%s: its other fields weigh %d, past the 600 rowbytes.go counts", name, fixed)
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
	}
}

// KEEPING A CALL AGAIN CHANGES NOTHING. Every append keeps every call on the
// row again ([PendingRun.appendCall]), so a call kept once must come back as it
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
		ids = elide(ids, call)
		calls++
		if call.Failed {
			failed++
		}
		if n := elidedBytes(ids); n > MaxElidedIdentityBytes {
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
		few = elide(few, BridgeCall{Name: "read_file", Digest: fmt.Sprintf("%064x", i%3)})
	}
	if len(few) != 3 || few[0].Calls != 10 || few[0].Digest == "" {
		t.Errorf("thirty drops of three identities tally %+v, want three exact entries of ten", few)
	}
}

// worstRow is a run's row with every field this engine bounds at its bound,
// each in the bytes that escape the most, the bridged log full of the worst
// calls and its tally at its share, and the parked state at its share. The
// three fields bounded by their SOURCE — a recorded answer, its hand-back and a
// held call's sealed arguments — are left empty: they get what this leaves.
//
// A FIELD ADDED TO THE ROW IS SET HERE AT ITS BOUND, in the change that adds it,
// so the arithmetic stays this test's: a field of the row comes out of the
// rest's share (rowbytes.go's RestBytes), and one of the parked state out of
// the state's, which the park holds it to whatever it carries.
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
	options := make([]decision.Option, decision.MaxOptions)
	for i := range options {
		options[i] = decision.Option{ID: fmt.Sprintf("option-%02d", i), Label: markup(decision.MaxOptionLabel),
			Detail: markup(decision.MaxOptionDetail)}
	}
	evidence := make([]decision.Evidence, decision.MaxEvidence)
	for i := range evidence {
		evidence[i] = decision.Evidence{Kind: "link", Ref: markup(decision.MaxEvidenceRef),
			Label: markup(decision.MaxEvidenceLabel)}
	}
	notes := make([]RunNote, 10)
	for i := range notes {
		notes[i] = RunNote{ID: uuid.NewString(), Seq: i + 1, Sealed: []byte(plain(4*2000 + 256)), At: worstAt,
			Read: &NoteRead{At: worstAt, Order: math.MaxInt, Phase: "execute", Iteration: 99, Round: 99,
				Boundary: "brief", Launch: uuid.NewString()}}
	}
	capture := &SessionCapture{LaunchSeq: math.MaxInt64, CaptureSeq: math.MaxInt64, Launch: uuid.NewString(),
		State: CaptureUnresolved, Reason: markup(1 << 10), BridgeSeq: math.MaxInt,
		BridgeOpen: make([]int, maxOpenPlaces), NoteCursor: math.MaxInt, ContextTokens: math.MaxInt64,
		At: worstAt, Harness: plain(32), HarnessVersion: plain(64), SessionID: plain(128), ArchiveFormat: 99,
		Object: plain(256), ObjectHash: plain(64), ObjectSize: math.MaxInt64, ContentKey: plain(256),
		Home: markup(1024), Workspace: markup(1024)}
	for i := range capture.BridgeOpen {
		capture.BridgeOpen[i] = math.MaxInt
	}
	state := `"` + plain(ExecuteStateBytes-2) + `"`
	run := PendingRun{
		TurnID: plain(64), WorkKey: plain(64), WorkSince: worstAt,
		AgentHandle: plain(64), AgentID: uuid.NewString(), Role: markup(128),
		SandboxID: plain(128), Harness: plain(32), Placement: plain(64), CommandID: plain(128),
		Status: StatusAwaiting, LaunchID: uuid.NewString(), LaunchSeq: math.MaxInt64,
		Capture: capture, Kept: capture, Handed: plain(MaxHandedBytes*4/3 + 1<<10),
		OwedEnd: json.RawMessage(`"` + plain(128<<10) + `"`), EndRecorded: true,
		PriorCalls: make([]string, MaxPriorCalls), PriorCallsElided: true,
		Owner: plain(128), OwnerEpoch: math.MaxInt64,
		TaskDescription: keptTitle(markup(1 << 20)), Label: keptTitle(markup(1 << 20)),
		Reply: plain(64), PartitionKey: markup(512), ConversationKey: markup(512),
		Branch: markup(MaxBranchBytes), SessionID: plain(128),
		Question: markup(decision.MaxQuestion), QuestionContext: markup(MaxAskContextBytes),
		Decision: &decision.Decision{Question: markup(decision.MaxQuestion), Options: options,
			Recommended: options[0].ID, Rationale: markup(decision.MaxRationale), Evidence: evidence,
			Role: decision.RoleApprover},
		Audience:          markup(MaxAskToBytes),
		HeldCall:          &HeldCall{ID: uuid.NewString(), LaunchID: uuid.NewString(), Server: plain(32), Tool: plain(128), Raw: plain(128), Digest: plain(64), HeldAt: worstAt, CallingAt: worstAt, Result: plain(MaxHeldResultBytes*4/3 + 1<<10), MadeAt: worstAt},
		ParkedInputTokens: math.MaxInt, ParkedOutputTokens: math.MaxInt,
		ParkedCacheReadTokens: math.MaxInt, ParkedCacheWriteTokens: math.MaxInt,
		AskedAt: worstAt, DeclinedAnswers: handles(maxDeclinedAnswers),
		Ending: &RecordedEnding{ID: uuid.NewString(), At: worstAt, Reason: plain(64), Detail: markup(1 << 10),
			Returned: true, Unused: true, Reclaim: true},
		Stop: &RunStop{By: markup(128), ByKind: plain(32), OperatorID: plain(64), Reason: markup(4 * 500),
			Collected: true, InputTokens: math.MaxInt, OutputTokens: math.MaxInt},
		WorkItem:        &types.WorkItem{ID: plain(64), Key: plain(64), Project: plain(64)},
		AudienceHandles: handles(256), AudienceFallback: true, Requester: plain(64),
		ColleagueAsk: &ColleagueAsk{ChannelID: plain(64), Question: keptTitle(markup(1 << 20)),
			Chain: handles(32), TraceID: plain(32), SpanID: plain(16), ParentSpanID: plain(16)},
		TraceID: plain(32), SpanID: plain(16), DelegationDepth: math.MaxInt, DelegationChain: handles(32),
		ExecuteState: json.RawMessage(state), AnsweredThrough: math.MaxInt, Charged: true, CompanyCharged: true,
		PauseTTLSeconds: math.MaxFloat64, PausedAt: worstAt, Deadline: worstAt, LaunchNode: plain(64),
		Lost: &JobLoss{Cause: LossCause(plain(32)), Node: plain(64), At: worstAt}, RoundLosses: math.MaxInt,
		HeldFor: Condition(plain(32)), HeldUntil: worstAt,
		Steer:     RunSteer{Seat: []byte(plain(512)), Seq: math.MaxInt, Notes: notes, Pulled: uuid.NewString(), Reads: math.MaxInt},
		CreatedAt: worstAt, UpdatedAt: worstAt,
	}
	for i := range run.PriorCalls {
		// A SUCCESSFUL call's name, which the surface resolved: a tool's
		// name, held to the MCP name grammar (internal/mcp's toolnames.go),
		// so it escapes to itself.
		run.PriorCalls[i] = fmt.Sprintf("%0128d", i)
	}
	// TWO HUNDRED CALLS KEPT AND A HUNDRED DROPPED, by names that escape the
	// most: about forty such identities fill their share, so the tally folds
	// to its last step.
	for i := range MaxBridgeCalls + elidedWorst {
		call := worstCall(t, fmt.Sprintf("tool_%04d_%s", i, strings.Repeat("<", 300)))
		call.Seq = i + 1
		call.Failed = i%2 == 0
		if i == MaxBridgeCalls+elidedWorst-1 {
			call.Name, call.Failed, call.Held, call.Approved = SubmitWorkTool, false, false, false
		}
		run.appendCall(call)
	}
	return run
}

// elidedWorst is how many calls the worst row's log drops.
const elidedWorst = 100

// THE WHOLE ROW FITS ITS BUDGET: the arithmetic in rowbytes.go, held to the
// row's own encoder. Every field this engine bounds is at its bound in the
// bytes that escape the most, the bridged log is two hundred of the worst calls
// beside its tally of a hundred dropped ones, and the parked state is at its
// share; the log weighs no more than [BridgeLogBytes], the state no more than
// [ExecuteStateBytes], and the rest no more than [RestBytes] less the room left
// for what a later build adds at the top of the row — and what that leaves for
// the three fields bounded by their source is at least a MiB, which holds a
// batch of twenty of the engine's own messages at their bound twice over.
//
// Mutation: keep a call's output whole, or raise MaxBridgeCalls, and the log
// overruns its share; stop holding a run's title at the launch, and the rest
// does.
func TestTheWholeRowFitsItsBudget(t *testing.T) {
	t.Parallel()
	run := worstRow(t)
	whole := encoded(t, run)

	withoutLog := run
	withoutLog.BridgeCalls, withoutLog.BridgeCallsElided, withoutLog.ElidedCalls = nil, 0, nil
	logBytes := whole - encoded(t, withoutLog)

	withoutState := withoutLog
	withoutState.ExecuteState = nil
	stateBytes := len(run.ExecuteState)
	rest := encoded(t, withoutState)
	source := RestBytes - ExtraRowBytes - rest
	t.Logf("the row weighs %d of its %d: the log %d of %d, the state %d of %d, the rest %d of %d "+
		"(%d of it left for a later build); the answer, its hand-back and a held call's arguments get %d",
		whole, RowBytes, logBytes, BridgeLogBytes, stateBytes, ExecuteStateBytes, rest, RestBytes,
		ExtraRowBytes, source)

	if logBytes > BridgeLogBytes {
		t.Errorf("the bridged log weighs %d, past its share %d", logBytes, BridgeLogBytes)
	}
	if stateBytes > ExecuteStateBytes {
		t.Errorf("the parked state weighs %d, past its share %d", stateBytes, ExecuteStateBytes)
	}
	if source < 1<<20 {
		t.Errorf("the fields bounded by their source are left %d, under the MiB rowbytes.go promises them", source)
	}
	if whole+ExtraRowBytes > RowBytes {
		t.Errorf("the row weighs %d, and with a later build's %d past its %d", whole, ExtraRowBytes, RowBytes)
	}
	if BridgeLogBytes+ExecuteStateBytes+RestBytes != RowBytes {
		t.Error("the three shares do not add up to the row's budget")
	}
	// EVERY DROPPED CALL COUNTED, the tally folded as far as its share made it
	// fold: names that escape the most leave none of them a tool, so the
	// record is incomplete for a reader of whether the turn replied.
	if len(run.BridgeCalls) != MaxBridgeCalls || run.BridgeCallsElided != elidedWorst ||
		elidedCount(run.ElidedCalls) != elidedWorst || run.ElisionKnown() {
		t.Errorf("the log keeps %d calls with %d dropped and %d counted (known %v), want %d with %d "+
			"counted and the elision unknown", len(run.BridgeCalls), run.BridgeCallsElided,
			elidedCount(run.ElidedCalls), run.ElisionKnown(), MaxBridgeCalls, elidedWorst)
	}
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

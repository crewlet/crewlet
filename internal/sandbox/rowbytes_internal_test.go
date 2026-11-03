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
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/mcp"
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
// whole notes come; and an output of markup. NOT HELD AND NOT APPROVED: a call
// whose arguments are withheld keeps none of them ([BridgeCall.Withheld]), so
// the flags cost a call twenty-seven bytes and the arguments it then does not
// keep eight thousand.
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
		Refusal: string(mcp.RefusalReassignmentBudget), Digest: strings.Repeat("f", 64),
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

// A WITHHELD CALL KEEPS NEITHER ITS ARGUMENTS NOR THEIR DIGEST, whoever handed
// it to the row: a call held for a person's approval, and the one they
// approved, carry their arguments sealed on the held call (ADR-0077), and a
// digest of them in the clear would confirm a guess at what the seal hides. A
// call dropped from the middle is tallied the same way, by its tool alone.
//
// Mutation: keep a withheld call's digest, or tally it under one, and the row
// holds it in the clear.
func TestAWithheldCallKeepsNeitherItsArgumentsNorTheirDigest(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]BridgeCall{
		"held":     {Name: "close_issue", Held: true, Failed: true, Args: `{"id":7}`, Digest: strings.Repeat("d", 64)},
		"approved": {Name: "close_issue", Approved: true, Args: `{"id":7}`, Digest: strings.Repeat("d", 64)},
	} {
		got := keptCall(call, MaxBridgeArgsBytes)
		if got.Args != "" || got.ArgsBytes != 0 || got.Digest != "" {
			t.Errorf("%s: the row keeps %q (%d bytes) under identity %q, want neither", name, got.Args,
				got.ArgsBytes, got.Digest)
		}
		if e := identity(call); e.Digest != "" || e.Name != "close_issue" || e.Calls != 1 {
			t.Errorf("%s: a dropped call is tallied as %+v, want its tool alone", name, e)
		}
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

// worstRow is a run's row with every field at its bound, each built in the
// bytes that escape the most and, where a field is sealed or cut by code of
// this package's, by that code: the bridged log full of the worst calls, both
// of its tallies at their shares, the parked state at its share, a recorded
// answer and the copies of a let-go one each at theirs, and a held call
// carrying its sealed arguments at their share and its result sealed at its
// widest.
//
// A FIELD ADDED TO THE ROW IS SET HERE AT ITS BOUND, in the change that adds it,
// so the arithmetic stays this test's: a field of the row comes out of the
// rest's share (rowbytes.go's RestBytes, through ExtraRowBytes), and one of the
// parked state out of the state's, which the park holds it to whatever it
// carries.
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
	// TEN NOTES (internal/agent/steer's MaxTurnNotes), each sealed at the
	// bound the store holds a note's seal to.
	notes := make([]RunNote, 10)
	for i := range notes {
		notes[i] = RunNote{ID: uuid.NewString(), Seq: i + 1, Sealed: []byte(plain(MaxNoteSealedBytes)), At: worstAt,
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
	if err := capture.Bounded(); err != nil {
		t.Fatalf("the worst capture is past what the row carries: %v", err)
	}
	held := &HeldCall{ID: uuid.NewString(), LaunchID: uuid.NewString(), Server: plain(32), Tool: plain(128),
		Raw: plain(128), Digest: plain(64), Arguments: plain(MaxHeldArgsBytes - 2), HeldAt: worstAt,
		CallingAt: worstAt, MadeAt: worstAt}
	held.Result = heldResultAtItsWorst(t, *held)
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
	handBack := paddedTo(t, MaxAnswerBytes, []HandedBack{{ID: uuid.NewString(), Original: uuid.NewString()}},
		func(h *[]HandedBack, pad string) {
			(*h)[0].Event = json.RawMessage(`"` + pad + `"`)
		})
	launch := LaunchRecord{StartedAt: worstAt, Started: true, Model: markup(256), ProviderKey: plain(64),
		SignInID: uuid.NewString(), FollowUp: FollowUpReason(plain(32)), Brief: BriefKind(plain(32)),
		FreshSessionID: plain(128), SessionStart: SessionStart(plain(32)), ResumeCarry: ResumeCarry(plain(32)),
		Iteration: math.MaxInt, Published: true, CollectFailures: math.MaxInt, CollectFailingSince: worstAt,
		ResumeFailures: math.MaxInt, ResumeFailingSince: worstAt, Collected: true, CollectedAt: worstAt,
		Bridged: EngineSpend{Aux: AuxTokens{Input: math.MaxInt, Output: math.MaxInt, CacheRead: math.MaxInt,
			CacheWrite: math.MaxInt}, Workers: math.MaxInt, WorkerInput: math.MaxInt, WorkerOutput: math.MaxInt},
		Condensed: AuxTokens{Input: math.MaxInt, Output: math.MaxInt, CacheRead: math.MaxInt,
			CacheWrite: math.MaxInt}}
	state := `"` + plain(ExecuteStateBytes-2) + `"`
	run := PendingRun{
		TurnID: plain(64), WorkKey: plain(64), WorkSince: worstAt,
		AgentHandle: plain(64), AgentID: uuid.NewString(), Role: markup(128),
		SandboxID: plain(128), Harness: plain(32), Placement: plain(64), CommandID: plain(128),
		Status: StatusAnswered, LaunchID: uuid.NewString(), LaunchSeq: math.MaxInt64,
		Capture: capture, Kept: capture, Handed: plain(MaxHandedBytes*4/3 + 1<<10),
		OwedEnd: json.RawMessage(`"` + plain(128<<10) + `"`), EndRecorded: true,
		PriorCalls: make([]string, MaxPriorCalls), PriorCallsElided: true,
		Launch: launch, Owner: plain(128), OwnerEpoch: math.MaxInt64,
		TaskDescription: keptTitle(markup(1 << 20)), Label: keptTitle(markup(1 << 20)),
		Reply: plain(64), PartitionKey: markup(512), ConversationKey: markup(512),
		Branch: markup(MaxBranchBytes), SessionID: plain(128),
		Question: markup(decision.MaxQuestion), QuestionContext: markup(MaxAskContextBytes),
		Decision: &decision.Decision{Question: markup(decision.MaxQuestion), Options: options,
			Recommended: options[0].ID, Rationale: markup(decision.MaxRationale), Evidence: evidence,
			Role: decision.RoleApprover},
		Audience:          markup(MaxAskToBytes),
		HeldCall:          held,
		ParkedInputTokens: math.MaxInt, ParkedOutputTokens: math.MaxInt,
		ParkedCacheReadTokens: math.MaxInt, ParkedCacheWriteTokens: math.MaxInt,
		AskedAt: worstAt, Answer: &answer, DeclinedAnswers: handles(maxDeclinedAnswers), HandBack: handBack,
		Ending: &RecordedEnding{ID: uuid.NewString(), At: worstAt, Reason: plain(64), Detail: markup(1 << 10),
			Returned: true, Unused: true, Reclaim: true},
		Stop: &RunStop{By: markup(128), ByKind: plain(32), OperatorID: plain(64), Reason: markup(4 * 500),
			Collected: true, InputTokens: math.MaxInt, OutputTokens: math.MaxInt},
		WorkItem:        &types.WorkItem{ID: plain(64), Key: plain(64), Project: plain(64)},
		AudienceHandles: handles(MaxAudienceHandles), AudienceFallback: true, Requester: plain(64),
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
	// to its last step. And each call's own work tallied the same way, so the
	// identities it made fold too.
	for i := range MaxBridgeCalls + elidedWorst {
		call := worstCall(t, fmt.Sprintf("tool_%04d_%s", i, strings.Repeat("<", 300)))
		call.Seq = i + 1
		call.Failed = i%2 == 0
		if i == MaxBridgeCalls+elidedWorst-1 {
			call.Name, call.Failed = SubmitWorkTool, false
		}
		worker := []ElidedCall{{Name: fmt.Sprintf("worker_%04d_%s", i, strings.Repeat("<", 300)),
			Digest: fmt.Sprintf("%064x", i), Calls: 1}}
		run.appendCall(call, nil, worker)
	}
	return run
}

// elidedWorst is how many calls the worst row's log drops.
const elidedWorst = 100

// THE WHOLE ROW FITS ITS BUDGET: the arithmetic in rowbytes.go, held to the
// row's own encoder. Every field is at its bound in the bytes that escape the
// most — the three a person or a tool supplies included, a recorded answer and
// the copies of a let-go one each at its share and a held call's sealed
// arguments at theirs — the bridged log is two hundred of the worst calls
// beside both of its tallies at their shares, and the parked state is at its
// share. The log weighs no more than [BridgeLogBytes], the state no more than
// [ExecuteStateBytes], and the rest no more than [RestBytes] less the room
// left for what a later build adds; and the whole, with that room, no more
// than [RowBytes].
//
// Mutation: keep a call's output whole, or raise MaxBridgeCalls, and the log
// overruns its share; raise MaxAnswerBytes or MaxHeldArgsBytes past what the
// rest has room for, or let a note's seal or a held call's result grow, and
// the rest does.
func TestTheWholeRowFitsItsBudget(t *testing.T) {
	t.Parallel()
	run := worstRow(t)
	whole := encoded(t, run)

	withoutLog := run
	withoutLog.BridgeCalls, withoutLog.BridgeCallsElided = nil, 0
	withoutLog.ElidedCalls, withoutLog.AbsorbedCalls, withoutLog.TallyRound = nil, nil, time.Time{}
	logBytes := whole - encoded(t, withoutLog)

	withoutState := withoutLog
	withoutState.ExecuteState = nil
	stateBytes := len(run.ExecuteState)
	rest := encoded(t, withoutState)

	withoutSource := withoutState
	withoutSource.Answer, withoutSource.HandBack = nil, nil
	held := *withoutSource.HeldCall
	held.Arguments = ""
	withoutSource.HeldCall = &held
	others := encoded(t, withoutSource)
	t.Logf("the row weighs %d of its %d: the log %d of %d, the state %d of %d, the rest %d of %d with %d "+
		"left for a later build — of which a recorded answer, its hand-back and a held call's arguments "+
		"%d, and every other field %d — and %d to spare", whole, RowBytes, logBytes, BridgeLogBytes,
		stateBytes, ExecuteStateBytes, rest, RestBytes, ExtraRowBytes, rest-others, others,
		RestBytes-ExtraRowBytes-rest)

	if logBytes > BridgeLogBytes {
		t.Errorf("the bridged log weighs %d, past its share %d", logBytes, BridgeLogBytes)
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
	if BridgeLogBytes+ExecuteStateBytes+RestBytes != RowBytes {
		t.Error("the three shares do not add up to the row's budget")
	}
	// EVERY DROPPED CALL COUNTED, the tally folded as far as its share made it
	// fold: names that escape the most leave none of them a tool, so the
	// record is incomplete for a reader of whether the turn replied. And the
	// calls the log's calls made, each counted.
	if len(run.BridgeCalls) != MaxBridgeCalls || run.BridgeCallsElided != elidedWorst ||
		elidedCount(run.Elided()) != elidedWorst || run.ElisionKnown() {
		t.Errorf("the log keeps %d calls with %d dropped and %d counted (known %v), want %d with %d "+
			"counted and the elision unknown", len(run.BridgeCalls), run.BridgeCallsElided,
			elidedCount(run.Elided()), run.ElisionKnown(), MaxBridgeCalls, elidedWorst)
	}
	if n := elidedCount(run.Absorbed()); n != MaxBridgeCalls+elidedWorst {
		t.Errorf("the calls' own work is counted %d times, want %d", n, MaxBridgeCalls+elidedWorst)
	}
	if n := tallyBytes(run.AbsorbedCalls, absorbedTally); n > MaxAbsorbedIdentityBytes {
		t.Errorf("the absorbed tally weighs %d, past its %d", n, MaxAbsorbedIdentityBytes)
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

// AN ANSWER BY TURN AT ITS SOURCE'S BOUND FITS A RECORDED ANSWER'S SHARE. An
// answer by turn has no ordinary message to fall back on, so one the row could
// not carry would be one a person was told was taken and the run never was:
// `answer_executor` holds an answer to a page (internal/agent/builtin's
// MaxRunAnswerBytes, which is MaxAskContextBytes), and that page at its worst —
// every byte one the encoder escapes six-fold, sealed with the longest names,
// and carried again as the delivery it came in — fits [MaxAnswerBytes], as the
// row holds it and as the copy a let-go of it would owe.
//
// Mutation: raise MaxRunAnswerBytes past what a recorded answer's share
// holds, or shrink the share under it, and this goes red.
func TestAnAnswerByTurnAtItsSourcesBoundFitsARecordedAnswersShare(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: strings.Repeat("t", 64), LaunchID: uuid.NewString()}
	given := types.ExecutorAnswerGiven{TurnID: run.TurnID, LaunchID: run.LaunchID,
		Answer: strings.Repeat("<", MaxAskContextBytes), AnsweredBy: strings.Repeat("<", 128),
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

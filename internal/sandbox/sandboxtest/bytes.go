package sandboxtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's row held to its byte budget: the cases [Run] drives for it.
//
// EACH IS A PROPERTY OF THE WRITE THAT GROWS THE ROW. The row is one value in
// the coordination store, held to the broker's payload bound (sandbox's
// RowBytes), and the writes that grow it — a bridged call appended, a parked
// state, a launch's title — each hold what they write to its share. A suite
// that asserted the codec alone would certify the author's arithmetic and
// nothing about what lands on the record, so the raw cases read the record's
// own bytes.

// bytesCases are the cases that read the row through the store.
var bytesCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ACallTheLogDropsKeepsItsIdentity", testACallTheLogDropsKeepsItsIdentity},
	{"TheNewestSubmissionIsKeptWholeForItsReplay", testTheNewestSubmissionIsKeptWholeForItsReplay},
	{"ANewRoundFoldsTheDroppedCallsIntoTheTurnsEarlierCalls", testANewRoundFoldsTheDroppedCallsIntoTheTurnsEarlierCalls},
	{"AParkedStatePastItsShareIsRefused", testAParkedStatePastItsShareIsRefused},
	{"ARunsTitleIsHeldToItsShare", testARunsTitleIsHeldToItsShare},
	{"AnAppendCarriesWhatItsSessionOwedAndLandsOnce", testAnAppendCarriesWhatItsSessionOwedAndLandsOnce},
	{"AWithheldCallIsKeptWithoutItsIdentity", testAWithheldCallIsKeptWithoutItsIdentity},
	{"AnAnswerPastItsShareIsRefused", testAnAnswerPastItsShareIsRefused},
	{"AHeldCallPastItsShareIsRefused", testAHeldCallPastItsShareIsRefused},
	{"ANotePastItsBoundIsRefused", testANotePastItsBoundIsRefused},
	{"AQuestionIsPutToTheNearestOfItsAudience", testAQuestionIsPutToTheNearestOfItsAudience},
}

// bytesRawCases are the cases that read the raw record.
var bytesRawCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore, coord.SandboxRuns)
}{
	{"ABridgedCallCarryingAMegabyteIsKeptWithinItsShare", testABridgedCallCarryingAMegabyteIsKeptWithinItsShare},
	{"TwoHundredFullAnswersLeaveTheRowWithinItsBudget", testTwoHundredFullAnswersLeaveTheRowWithinItsBudget},
	{"ATallyAnotherRoundLeftIsNoneOfThisRounds", testATallyAnotherRoundLeftIsNoneOfThisRounds},
	{"ACaptureStampPastItsBoundsIsRefused", testACaptureStampPastItsBoundsIsRefused},
}

// commitArgs is a code_commit_files call's arguments as the engine records
// them: identifiers beside a file far past any call's share.
func commitArgs(t *testing.T, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"repo": "acme/api", "branch": "crewlet/eng/fix", "message": "fix the flake",
		"files": []any{map[string]any{"path": "internal/a.go", "content": content}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// digestOf is a call's identity as the bridge would carry it, distinct per n.
func digestOf(n int) string { return fmt.Sprintf("%064x", n) }

// raw is the run's record as the coordination store holds it.
func raw(t *testing.T, runs coord.SandboxRuns, turnID string) []byte {
	t.Helper()
	record, found, err := runs.SandboxRun(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("read the raw record of %s: %v %v", turnID, found, err)
	}
	return record.Value
}

// A BRIDGED CALL CARRYING A MEGABYTE IS KEPT WITHIN ITS SHARE. A commit's file
// of a megabyte of markup is the arguments' largest value, so it is left out —
// a marker naming its size and digest in its place — while the repository,
// the branch and the file's path stay whole beside it; the call states the
// size of what it was sent with, keeps its identity, and adds no more to the
// record than its share.
//
// Mutation: record the call as the bridge handed it, and the record grows by
// six megabytes of escapes; drop the identity, and a resumed run cannot count
// the call it was.
func testABridgedCallCarryingAMegabyteIsKeptWithinItsShare(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	r := run("t-bytes-commit")
	launch := mustLaunched(t, s, r)
	before := len(raw(t, runs, r.TurnID))
	args := commitArgs(t, strings.Repeat("<", 1<<20))
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "code_commit_files", Args: args, Output: "committed abc123",
			Digest: digestOf(1), Seq: 1}}); err != nil || !recorded {
		t.Fatalf("AppendBridgeCall = %v, %v", recorded, err)
	}
	if grew := len(raw(t, runs, r.TurnID)) - before; grew > sandbox.MaxBridgeCallBytes {
		t.Errorf("one call grew the record by %d bytes, past its share %d", grew, sandbox.MaxBridgeCallBytes)
	}
	got := mustGet(t, s, r.TurnID).BridgeCalls
	if len(got) != 1 {
		t.Fatalf("%d calls recorded, want the one", len(got))
	}
	call := got[0]
	if call.ArgsBytes != len(args) || call.OutputBytes != 0 || call.Digest != digestOf(1) {
		t.Errorf("the call states %d bytes of arguments, %d of output and identity %q; want %d, 0 and its own",
			call.ArgsBytes, call.OutputBytes, call.Digest, len(args))
	}
	var kept map[string]any
	if err := json.Unmarshal([]byte(call.Args), &kept); err != nil {
		t.Fatalf("what the row kept is not the call's arguments: %v", err)
	}
	files, _ := kept["files"].([]any)
	file, _ := firstFile(files)
	content, _ := file["content"].(string)
	if kept["repo"] != "acme/api" || kept["branch"] != "crewlet/eng/fix" || file["path"] != "internal/a.go" {
		t.Errorf("an identifier was not kept whole: %s", call.Args)
	}
	if !strings.HasPrefix(content, "(1.0 MiB not kept on the run's record, digest ") {
		t.Errorf("the file is not said to be left out: %.120q", content)
	}
}

func firstFile(files []any) (map[string]any, bool) {
	if len(files) == 0 {
		return nil, false
	}
	file, ok := files[0].(map[string]any)
	return file, ok
}

// logBytes is what the bridged log weighs in a raw record: its three keys and
// their values, as written.
func logBytes(t *testing.T, record []byte) int {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record, &fields); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, key := range []string{"bridge_calls", "bridge_calls_elided", "bridge_elided_calls",
		"bridge_absorbed_calls", "bridge_tally_round"} {
		if value, ok := fields[key]; ok {
			n += len(`,"":`) + len(key) + len(value)
		}
	}
	return n
}

// TWO HUNDRED FULL ANSWERS LEAVE THE ROW WITHIN ITS BUDGET. A long run whose
// every call answered a builtin's whole 64 KiB page — and sixty more past the
// log's count, dropped from its middle — leaves a log that weighs no more than
// its share of the row (sandbox's BridgeLogBytes), every call stating the size
// it answered and the dropped ones counted by identity.
//
// Mutation: keep each call's output whole, and the log weighs 12.8 MiB of
// pages before a byte of their escapes, past the whole row's budget.
func testTwoHundredFullAnswersLeaveTheRowWithinItsBudget(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	r := run("t-bytes-pages")
	launch := mustLaunched(t, s, r)
	page := strings.Repeat("{\"line\":\"<li>a & b</li>\"}\n", 64<<10/27)
	total := sandbox.MaxBridgeCalls + 60
	for i := range total {
		if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
			Call: sandbox.BridgeCall{Name: "read_page", Args: fmt.Sprintf(`{"page":%d}`, i), Output: page,
				Digest: digestOf(i), Seq: i + 1}}); err != nil || !recorded {
			t.Fatalf("append %d = %v, %v", i, recorded, err)
		}
	}
	record := raw(t, runs, r.TurnID)
	if n := logBytes(t, record); n > sandbox.BridgeLogBytes {
		t.Errorf("the log weighs %d bytes, past its share %d", n, sandbox.BridgeLogBytes)
	}
	if len(record) > sandbox.RowBytes {
		t.Errorf("the record weighs %d bytes, past the row's budget %d", len(record), sandbox.RowBytes)
	}
	got := mustGet(t, s, r.TurnID)
	for _, call := range got.BridgeCalls {
		if call.OutputBytes != len(page) || kept.Quoted(call.Output) > sandbox.MaxBridgeOutputBytes {
			t.Fatalf("call %d keeps %d bytes of output stating %d, want at most %d stating %d", call.Seq,
				kept.Quoted(call.Output), call.OutputBytes, sandbox.MaxBridgeOutputBytes, len(page))
		}
	}
	if got.BridgeCallsElided != 60 || !got.ElisionKnown() {
		t.Errorf("%d calls dropped, accounted %v; want 60, every one by identity",
			got.BridgeCallsElided, got.ElisionKnown())
	}
}

// A CALL THE LOG DROPS KEEPS ITS IDENTITY. Past the log's count its middle is
// dropped, and each call dropped is counted by its tool and the digest it was
// admitted under, with its failures — so the count bound costs the record the
// call and never what a resumed call log counts, or what a reader of whether
// the turn replied reads.
//
// Mutation: drop a call without tallying it, and its identity's count falls
// short; tally it under its arguments' marker rather than its digest, and two
// calls asking for the same thing are counted as two.
func testACallTheLogDropsKeepsItsIdentity(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-identity")
	launch := mustLaunched(t, s, r)
	total := sandbox.MaxBridgeCalls + 30
	for i := range total {
		name := "read_file"
		if i%2 == 1 {
			name = "post_message"
		}
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
			Call: sandbox.BridgeCall{Name: name, Digest: digestOf(i % 4), Failed: i%3 == 0, Seq: i + 1}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got := mustGet(t, s, r.TurnID)
	if got.BridgeCallsElided != 30 || len(got.BridgeCalls) != sandbox.MaxBridgeCalls {
		t.Fatalf("%d kept and %d dropped, want %d and 30", len(got.BridgeCalls), got.BridgeCallsElided,
			sandbox.MaxBridgeCalls)
	}
	// What was dropped is calls head..head+29 (the oldest after the head):
	// their identities are the ones the tally must hold.
	want := map[string]sandbox.ElidedCall{}
	head := sandbox.MaxBridgeCalls / 2
	for i := head; i < head+30; i++ {
		name := "read_file"
		if i%2 == 1 {
			name = "post_message"
		}
		key := name + "/" + digestOf(i%4)
		e := want[key]
		e.Name, e.Digest = name, digestOf(i%4)
		e.Calls++
		if i%3 == 0 {
			e.Failed++
		}
		want[key] = e
	}
	if len(got.ElidedCalls) != len(want) {
		t.Fatalf("the tally holds %d identities, want %d: %+v", len(got.ElidedCalls), len(want), got.ElidedCalls)
	}
	for _, e := range got.ElidedCalls {
		w := want[e.Name+"/"+e.Digest]
		if e.Calls != w.Calls || e.Failed != w.Failed {
			t.Errorf("identity %s/%s counts %d (%d failed), want %d (%d)", e.Name, e.Digest[:8], e.Calls,
				e.Failed, w.Calls, w.Failed)
		}
	}
	if !got.ElisionKnown() {
		t.Error("an elision every dropped call of which is tallied by tool reads as unknown")
	}
}

// THE NEWEST SUBMISSION IS KEPT WHOLE FOR ITS REPLAY. The collection replays a
// run's newest successful `submit_work` off its row as the round's outcome, so
// that one call keeps its arguments up to the larger share
// (MaxSubmissionArgsBytes); the moment a newer one lands, the one before it is
// held to every call's ordinary share, and a submission the surface refused,
// which is never replayed, is held to it from the start.
//
// Mutation: hold every call to the ordinary share, and the newest submission
// is replayed as a marker; keep every submission at the larger share, and a
// run that resubmits grows the log past its share.
func testTheNewestSubmissionIsKeptWholeForItsReplay(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-submission")
	launch := mustLaunched(t, s, r)
	submission := func(seq int) string {
		raw, err := json.Marshal(map[string]any{"outcome": "delivered",
			"summary": fmt.Sprintf("round %d: ", seq) + strings.Repeat("the build passed. ", 20<<10/18)})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	add := func(seq int, failed bool) {
		t.Helper()
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
			Call: sandbox.BridgeCall{Name: sandbox.SubmitWorkTool, Args: submission(seq), Failed: failed,
				Digest: digestOf(seq), Seq: seq}}); err != nil {
			t.Fatalf("append submission %d: %v", seq, err)
		}
	}
	whole := func(seq int) bool {
		t.Helper()
		for _, call := range mustGet(t, s, r.TurnID).BridgeCalls {
			if call.Seq == seq {
				if call.ArgsBytes == 0 && call.Args != submission(seq) {
					t.Errorf("submission %d states no cut and holds other arguments", seq)
				}
				if call.ArgsBytes > 0 && kept.Quoted(call.Args) > sandbox.MaxBridgeArgsBytes {
					t.Errorf("submission %d is cut to %d bytes, past the ordinary share %d", seq,
						kept.Quoted(call.Args), sandbox.MaxBridgeArgsBytes)
				}
				return call.ArgsBytes == 0
			}
		}
		t.Fatalf("submission %d is not on the record", seq)
		return false
	}
	add(1, false)
	if !whole(1) {
		t.Error("the run's only submission was not kept whole")
	}
	add(2, false)
	if whole(1) || !whole(2) {
		t.Errorf("after a newer submission, the first is whole %v and the newer %v; want only the newer",
			whole(1), whole(2))
	}
	add(3, true)
	if whole(3) || !whole(2) {
		t.Errorf("after a refused submission, it is whole %v and the newest successful one %v; want only the "+
			"newest successful one", whole(3), whole(2))
	}
}

// A NEW ROUND FOLDS THE DROPPED CALLS INTO THE TURN'S EARLIER CALLS. A round's
// log is cleared when the next round opens, and what it held is folded into
// the turn's earlier calls by name, which is what the run's own ending reads
// to say whether the turn replied — a reply its log dropped from the middle
// included, read off its identity, so the turn's record is not taken for
// incomplete.
//
// Mutation: fold the kept calls alone, and the reply the log dropped is
// missing from the turn's earlier calls.
func testANewRoundFoldsTheDroppedCallsIntoTheTurnsEarlierCalls(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-fold")
	launch := mustLaunched(t, s, r)
	head := sandbox.MaxBridgeCalls / 2
	for i := range sandbox.MaxBridgeCalls + 10 {
		name := "read_file"
		if i == head+3 {
			name = "post_message"
		}
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
			Call: sandbox.BridgeCall{Name: name, Digest: digestOf(i), Seq: i + 1}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	before := mustGet(t, s, r.TurnID)
	for _, call := range before.BridgeCalls {
		if call.Name == "post_message" {
			t.Fatal("the reply was kept; the case needs it dropped from the middle")
		}
	}
	next := run(r.TurnID)
	if _, err := s.BeginLaunch(ctx, next, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	got := mustGet(t, s, r.TurnID)
	if len(got.BridgeCalls) != 0 || got.BridgeCallsElided != 0 || len(got.ElidedCalls) != 0 {
		t.Errorf("the new round opened with %d calls, %d dropped and %d identities; want none",
			len(got.BridgeCalls), got.BridgeCallsElided, len(got.ElidedCalls))
	}
	if !strings.Contains(strings.Join(got.PriorCalls, ","), "post_message") || got.PriorCallsElided {
		t.Errorf("the turn's earlier calls are %v (incomplete %v); want the dropped reply among them, complete",
			got.PriorCalls, got.PriorCallsElided)
	}
}

// A PARKED STATE PAST ITS SHARE IS REFUSED, BEFORE ANYTHING IS WRITTEN. The park
// condenses a state to fit (internal/agent/execstate), so one still past its
// share of the row is a turn whose own context will not fit, and written it
// would be refused by the broker on that write and every write after it. A
// state at its share is written.
//
// Mutation: write the state whatever its size, and the run is parked on a row
// past its budget.
func testAParkedStatePastItsShareIsRefused(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	state := func(n int) sandbox.Suspension {
		return sandbox.Suspension{State: json.RawMessage(`"` + strings.Repeat("a", n-2) + `"`), Iteration: 1}
	}
	r := run("t-bytes-state")
	mustBeginLaunch(t, s, r)
	ok, err := s.MarkSuspended(ctx, r.TurnID, state(sandbox.ExecuteStateBytes+1))
	if !errors.Is(err, sandbox.ErrStateTooLarge) || ok {
		t.Fatalf("a state past its share = %v, %v; want ErrStateTooLarge", ok, err)
	}
	if got := mustGet(t, s, r.TurnID); got.Status != sandbox.StatusLaunching || len(got.ExecuteState) != 0 {
		t.Errorf("the refused park left the run %s holding %d bytes of state; want it launching, holding none",
			got.Status, len(got.ExecuteState))
	}
	if ok, err := s.MarkSuspended(ctx, r.TurnID, state(sandbox.ExecuteStateBytes)); err != nil || !ok {
		t.Errorf("a state at its share = %v, %v; want it written", ok, err)
	}
}

// A RUN'S TITLE IS HELD TO ITS SHARE. What a person reads a run by — its task
// description and its label — and the question of the colleague its turn
// answers are a trigger's own words, which nothing else bounds below the
// broker's payload; the write that creates the row keeps each within its share,
// as its opening with the cut said, and a relaunch writes none of them again.
//
// Mutation: write the launch's words as handed, and a pasted megabyte is six on
// the row.
func testARunsTitleIsHeldToItsShare(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	words := "please look at this log: " + strings.Repeat("<line>", 1<<20/6)
	r := run("t-bytes-title")
	r.TaskDescription, r.Label = words, words
	r.ColleagueAsk = &sandbox.ColleagueAsk{ChannelID: "a2a-1", Question: words}
	mustLaunched(t, s, r)
	check := func(step string) {
		t.Helper()
		got := mustGet(t, s, r.TurnID)
		for field, text := range map[string]string{"task description": got.TaskDescription,
			"label": got.Label, "colleague's question": got.ColleagueAsk.Question} {
			if kept.Quoted(text) > sandbox.MaxRunTitleBytes || !strings.HasPrefix(text, "please look at this log: ") ||
				!strings.Contains(text, "not kept on the run's record") {
				t.Errorf("%s: the %s is %d bytes as the row holds it, its share %d, opening %.40q",
					step, field, kept.Quoted(text), sandbox.MaxRunTitleBytes, text)
			}
		}
	}
	check("the launch")
	again := run(r.TurnID)
	again.TaskDescription = "something else"
	if _, err := s.BeginLaunch(ctx, again, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	check("a relaunch")
}

// AN APPEND CARRIES WHAT ITS SESSION OWED, AND LANDS ONCE. A bridged call whose
// own append never landed is owed by its session, by identity, and the next
// append carries it: the row counts it as a call of the round it does not list,
// beside the dropped middle, so a resumed call log seeds it and a reader of
// whether the turn replied reads it. The calls a call's own work made — a
// delegated worker's — ride the same append into a tally of their own, which
// no reader of the round's deliveries counts. And an append whose answer was
// lost, asked again, is answered recorded and changes nothing: the call and
// what it carried are on the row once.
//
// Mutation: drop the owed identities, or the absorbed ones, and a resumed run
// counts fewer calls than its process did; append a repeat again, and both
// tallies double.
func testAnAppendCarriesWhatItsSessionOwedAndLandsOnce(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-owed")
	launch := mustLaunched(t, s, r)
	a := sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "delegate", Digest: digestOf(3), Seq: 3, StartedAt: base},
		Unrecorded: []sandbox.ElidedCall{{Name: "comment_on_work_item", Digest: digestOf(1), Calls: 1},
			{Name: "read_file", Digest: digestOf(2), Calls: 1, Failed: 1}},
		Absorbed: []sandbox.ElidedCall{{Name: "leave_channel", Digest: digestOf(9), Calls: 1}},
	}
	for attempt := range 2 {
		if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, a); err != nil || !recorded {
			t.Fatalf("append, attempt %d = %v, %v; want it recorded", attempt+1, recorded, err)
		}
	}
	got := mustGet(t, s, r.TurnID)
	if len(got.BridgeCalls) != 1 || got.BridgeCallsElided != 2 {
		t.Fatalf("the row lists %d calls and counts %d it does not list, want the one and the two owed",
			len(got.BridgeCalls), got.BridgeCallsElided)
	}
	elided := got.Elided()
	if len(elided) != 2 || elided[0].Name != "comment_on_work_item" || elided[1].Failed != 1 || !got.ElisionKnown() {
		t.Errorf("the owed calls read %+v (known %v), want both by identity and outcome", elided,
			got.ElisionKnown())
	}
	absorbed := got.Absorbed()
	if len(absorbed) != 1 || absorbed[0].Name != "leave_channel" || absorbed[0].Calls != 1 {
		t.Errorf("the delegate's own work reads %+v, want the worker's one call", absorbed)
	}
	// ANOTHER CALL AT THE SAME PLACE IS NOT THE SAME CALL: a session opened
	// again on the job numbers its calls after the row's, and a call the
	// closed session still had in flight can land at a place it reuses.
	other := sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_file", Digest: digestOf(4), Seq: 3, StartedAt: base.Add(time.Second)}}
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, other); err != nil || !recorded {
		t.Fatalf("another call at the same place = %v, %v; want it recorded", recorded, err)
	}
	if got := mustGet(t, s, r.TurnID); len(got.BridgeCalls) != 2 {
		t.Errorf("the row lists %d calls, want both calls made at the one place", len(got.BridgeCalls))
	}
}

// A WITHHELD CALL IS KEPT WITHOUT ITS IDENTITY. A call held for a person's
// approval, and the call they approved, carry their arguments sealed on the
// held call (ADR-0077): a digest of them in the clear on the call's record
// would confirm a guess at what the seal hides. So the row keeps neither,
// whichever writer hands them — the bridge's append of the held call, the
// resume's record of the approved one — and a resumed call log counts each as a
// call of its tool whose arguments are not known.
//
// Mutation: keep a withheld call's digest, and the row holds it in the clear.
func testAWithheldCallIsKeptWithoutItsIdentity(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-withheld")
	launch := mustLaunched(t, s, r)
	if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "tr_close_issue", Held: true, Failed: true, Args: `{"id":"7"}`,
			Digest: digestOf(7), Seq: 1}}); err != nil {
		t.Fatalf("append the held call: %v", err)
	}
	held := mustGet(t, s, r.TurnID).BridgeCalls[0]
	if held.Digest != "" || held.Args != "" || !held.Held {
		t.Errorf("the held call is kept as %+v, want it held, with neither its arguments nor their digest", held)
	}
}

// replyBatch is a chat reply of n messages of size bytes of markup each, as the
// answer it would be recorded as: each delivery the event it is, and the words
// sealed (standing in for the coordinator's seal, as large as the words are).
func replyBatch(n, size int) sandbox.RecordedAnswer {
	a := sandbox.RecordedAnswer{Via: types.AnswerViaChat, RecordedAt: base}
	var words strings.Builder
	for range n {
		body := strings.Repeat("<", size)
		ev := events.New(types.ExternalNotification{Body: body}, events.TraceContext{})
		raw, err := json.Marshal(ev)
		if err != nil {
			panic(err)
		}
		a.EventIDs = append(a.EventIDs, ev.ID.String())
		a.Events = append(a.Events, raw)
		words.WriteString(body)
	}
	a.Sealed = sealOf(words.String())
	return a
}

// AN ANSWER PAST ITS SHARE IS REFUSED, BEFORE ANYTHING IS WRITTEN. A reply is
// recorded on the run's row as its answer — its words sealed, and every
// delivery it was made of as the event it is — and a batch of long messages in
// markup, whose every byte the row's encoder escapes six-fold, weighs
// megabytes there: twice over with the copies a decline would owe. So the
// store holds a recorded answer to its share (sandbox's MaxAnswerBytes) and
// refuses one past it by name, leaving the run waiting on its question, and
// the reply goes its way as the ordinary message it is. A reply within its
// share is recorded.
//
// Mutation: keep the events whole whatever they weigh, and twenty messages
// of 32 KiB of markup are recorded as the run's answer, megabytes of it.
func testAnAnswerPastItsShareIsRefused(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-answer")
	launch := mustLaunched(t, s, r)
	park(t, s, r.TurnID)
	_, ok, err := s.RecordAnswer(ctx, r.TurnID, launch, replyBatch(20, 32<<10), sandbox.Fence{})
	if !errors.Is(err, sandbox.ErrAnswerTooLarge) || ok {
		t.Fatalf("a reply of twenty 32 KiB messages of markup = %v, %v; want ErrAnswerTooLarge", ok, err)
	}
	if got := mustGet(t, s, r.TurnID); got.Answer != nil || got.Status != sandbox.StatusAwaiting {
		t.Fatalf("the refused answer left the run %s holding %v; want it awaiting, holding none",
			got.Status, got.Answer != nil)
	}
	if _, ok, err := s.RecordAnswer(ctx, r.TurnID, launch, replyBatch(3, 2<<10), sandbox.Fence{}); err != nil || !ok {
		t.Errorf("a reply of three short messages = %v, %v; want it recorded", ok, err)
	}
}

// A HELD CALL PAST ITS SHARE IS REFUSED, BEFORE ANYTHING IS WRITTEN. A call held
// for a person's approval never reaches its server, so nothing but the run's
// row bounds what it carries, and its arguments ride the row sealed. The store
// holds them to their share (sandbox's MaxHeldArgsBytes) and refuses a call
// past it by name — the holder refuses such a call to the run first, naming its
// size — and records a call within it.
//
// Mutation: record the held call whatever it carries, and a megabyte file
// rides the row three and a half times over.
func testAHeldCallPastItsShareIsRefused(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-held")
	launch := mustLaunched(t, s, r)
	big := heldAsk(t, r.TurnID, launch)
	sealed, err := heldSeal(t).SealArguments(r.TurnID, *big.Held,
		map[string]any{"path": "index.html", "content": strings.Repeat("<p>", 1<<20/3)})
	if err != nil {
		t.Fatal(err)
	}
	big.Held.Arguments = sealed
	if outcome, err := s.RecordAsk(ctx, r.TurnID, launch, big); !errors.Is(err, sandbox.ErrHeldCallTooLarge) {
		t.Fatalf("a held call carrying a megabyte of markup = %v, %v; want ErrHeldCallTooLarge", outcome, err)
	}
	if got := mustGet(t, s, r.TurnID); got.HeldCall != nil || got.Question != "" {
		t.Fatalf("the refused call left a question on the run: %+v", got.HeldCall)
	}
	if outcome, err := s.RecordAsk(ctx, r.TurnID, launch, heldAsk(t, r.TurnID, launch)); err != nil ||
		outcome != sandbox.AskRecorded {
		t.Errorf("a held call within its share = %v, %v; want it recorded", outcome, err)
	}
}

// A NOTE PAST ITS BOUND IS REFUSED, BEFORE ANYTHING IS WRITTEN. A person's note
// is sealed by the engine and kept on the run's row once read, ten to a turn;
// the store holds each seal to its bound (sandbox's MaxNoteSealedBytes), which
// the engine's own longest note fits, so the row's arithmetic is the store's.
//
// Mutation: file a note whatever its seal weighs, and ten of them grow the row
// without a bound.
func testANotePastItsBoundIsRefused(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-note")
	mustLaunched(t, s, r)
	_, _, err := s.FileNote(ctx, r.TurnID, sandbox.NoteFiling{MaxWaiting: 10, MaxNotes: 10,
		Note: sandbox.RunNote{ID: "n1", Sealed: bytes.Repeat([]byte("a"), sandbox.MaxNoteSealedBytes+1)}})
	if !errors.Is(err, sandbox.ErrNoteTooLarge) {
		t.Fatalf("a note sealed past its bound = %v; want ErrNoteTooLarge", err)
	}
	if got := mustGet(t, s, r.TurnID); len(got.Steer.Notes) != 0 {
		t.Fatalf("the refused note is on the record: %+v", got.Steer.Notes)
	}
	if filed, _, err := s.FileNote(ctx, r.TurnID, sandbox.NoteFiling{MaxWaiting: 10, MaxNotes: 10,
		Note: sandbox.RunNote{ID: "n2", Sealed: bytes.Repeat([]byte("a"), sandbox.MaxNoteSealedBytes)}}); err != nil ||
		filed != sandbox.NoteAccepted {
		t.Errorf("a note at its bound = %v, %v; want it filed", filed, err)
	}
}

// A QUESTION IS PUT TO THE NEAREST OF ITS AUDIENCE. A `team` audience is every
// person in the asking seat's unit, which nothing else bounds, and the row
// carries the seats a question is put to; the park holds them to their bound
// (sandbox's MaxAudienceHandles) and keeps the first, which the resolution
// names nearest first.
//
// Mutation: park the question on every seat it resolved to, and a unit of
// thousands rides the row.
func testAQuestionIsPutToTheNearestOfItsAudience(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-audience")
	mustLaunched(t, s, r)
	Ask(t, s, r.TurnID, sandbox.Ask{Question: "which branch?", To: sandbox.AskToTeam})
	handles := make([]string, sandbox.MaxAudienceHandles+44)
	for i := range handles {
		handles[i] = fmt.Sprintf("person-%04d", i)
	}
	if err := s.MarkAwaiting(ctx, r.TurnID, sandbox.Clarification{Answerers: sandbox.Audience{Handles: handles},
		AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	got := mustGet(t, s, r.TurnID).AudienceHandles
	if len(got) != sandbox.MaxAudienceHandles || got[0] != handles[0] ||
		got[len(got)-1] != handles[sandbox.MaxAudienceHandles-1] {
		t.Errorf("the question is put to %d seats, %v first; want the nearest %d", len(got), got[:1],
			sandbox.MaxAudienceHandles)
	}
}

// A TALLY ANOTHER ROUND LEFT IS NONE OF THIS ROUND'S. An older build, which
// knows neither tally, opens a new round by clearing the round's calls and
// their count, and carries the tallies on as keys it does not know — where
// they named the earlier round's calls as this one's: a reply this round never
// made, and identities its log never held. And an older build that drops calls
// from the middle counts them and keeps no identity. Read by this build, a
// tally is the round's only where it was kept under the round's first call;
// and calls counted and not accounted for read as calls whose tool is not
// known — counted against every later call, and a record no reader of whether
// the turn replied can read whole. The next append of this build starts the
// round's tally over, from what it reads.
//
// Mutation: read the tallies off the row as they stand, and the earlier
// round's reply answers this one, and its identities are seeded into it.
func testATallyAnotherRoundLeftIsNoneOfThisRounds(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	r := run("t-bytes-stale")
	launch := mustLaunched(t, s, r)
	for i := range sandbox.MaxBridgeCalls + 2 {
		name := "read_file"
		if i == sandbox.MaxBridgeCalls/2 {
			name = "post_message"
		}
		if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
			Call:     sandbox.BridgeCall{Name: name, Digest: digestOf(i), Seq: i + 1},
			Absorbed: []sandbox.ElidedCall{{Name: "leave_channel", Digest: digestOf(i), Calls: 1}}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	// AN OLDER BUILD OPENS THE NEXT ROUND: its calls and their count go,
	// and the keys it does not know stay as they were.
	rewrite(t, runs, r.TurnID, func(row map[string]json.RawMessage) {
		row["bridge_calls"] = json.RawMessage(`[{"name":"read_file","seq":1,"at":"2031-01-01T00:00:00Z"}]`)
		delete(row, "bridge_calls_elided")
	})
	got := mustGet(t, s, r.TurnID)
	if len(got.Elided()) != 0 || len(got.Absorbed()) != 0 || !got.ElisionKnown() {
		t.Errorf("the next round reads %+v dropped and %+v absorbed (known %v), want none of the last round's",
			got.Elided(), got.Absorbed(), got.ElisionKnown())
	}
	// AND DROPS THREE CALLS OF ITS OWN, keeping no identity of them.
	rewrite(t, runs, r.TurnID, func(row map[string]json.RawMessage) {
		row["bridge_calls_elided"] = json.RawMessage(`3`)
	})
	got = mustGet(t, s, r.TurnID)
	if e := got.Elided(); len(e) != 1 || e[0].Name != "" || e[0].Calls != 3 || got.ElisionKnown() {
		t.Errorf("three calls dropped with no identity read %+v (known %v), want three calls of no known tool",
			e, got.ElisionKnown())
	}
	if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_file", Digest: digestOf(500), Seq: 500}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got = mustGet(t, s, r.TurnID)
	if e := got.ElidedCalls; len(e) != 1 || e[0].Name != "" || e[0].Calls != 3 || len(got.AbsorbedCalls) != 0 {
		t.Errorf("this build's next append keeps %+v dropped and %+v absorbed, want the round's own: three "+
			"calls of no known tool, nothing absorbed", e, got.AbsorbedCalls)
	}
}

// rewrite changes turnID's raw record as a build that does not know some of its
// keys would write it, through the store the record is held in.
func rewrite(t *testing.T, runs coord.SandboxRuns, turnID string, change func(map[string]json.RawMessage)) {
	t.Helper()
	_, _, err := runs.MutateSandboxRun(t.Context(), turnID, func(record coord.Record) (coord.SandboxRunChange, error) {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(record.Value, &row); err != nil {
			return coord.SandboxRunChange{}, err
		}
		change(row)
		raw, err := json.Marshal(row)
		if err != nil {
			return coord.SandboxRunChange{}, err
		}
		return coord.SandboxRunChange{Put: raw}, nil
	})
	if err != nil {
		t.Fatalf("rewrite the raw record of %s: %v", turnID, err)
	}
}

// A CAPTURE STAMP PAST ITS BOUNDS IS REFUSED, BEFORE ANYTHING IS WRITTEN. What a
// box says of its session — the release of the harness that wrote it, read off
// a file the run's own agent can write — reaches the row on the capture's
// stamp, twice once it lands (the place, and the turn's kept capture). The
// store holds every such field to the bound the executor log holds it to and
// refuses a stamp past one by name, leaving the record as it was; a stamp
// within them is taken.
//
// Mutation: stamp the capture whatever it carries, and a release of a megabyte
// of digits is on the row.
func testACaptureStampPastItsBoundsIsRefused(t *testing.T, s sandbox.PendingStore, runs coord.SandboxRuns) {
	ctx := t.Context()
	r := run("t-bytes-capture")
	launch := mustLaunched(t, s, r)
	before := raw(t, runs, r.TurnID)
	stamp := sandbox.SessionCapture{LaunchSeq: 1, CaptureSeq: 1, Launch: launch, State: sandbox.CaptureBegun,
		At: base, Harness: "claude-code", HarnessVersion: strings.Repeat("9", 1<<20) + ".0.0", SessionID: "s-1"}
	if took, err := s.StampCapture(ctx, r.TurnID, stamp); !errors.Is(err, sandbox.ErrCaptureUnbounded) || took {
		t.Fatalf("a stamp naming a megabyte release = %v, %v; want ErrCaptureUnbounded", took, err)
	}
	if after := raw(t, runs, r.TurnID); !bytes.Equal(after, before) {
		t.Fatalf("the refused stamp changed the record: %d bytes to %d", len(before), len(after))
	}
	stamp.HarnessVersion = "2.1.310"
	if took, err := s.StampCapture(ctx, r.TurnID, stamp); err != nil || !took {
		t.Errorf("a stamp within its bounds = %v, %v; want it taken", took, err)
	}
}

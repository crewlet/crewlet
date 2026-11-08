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

// A run's row and its log held to their byte budget: the cases [Run] drives for
// it.
//
// EACH IS A PROPERTY OF THE WRITE THAT GROWS A VALUE. The row and the log beside
// it are each one value in the coordination store, held to the broker's payload
// bound (sandbox's RowBytes), and the writes that grow them — a bridged call
// appended to the log, a parked state, a launch's title — each hold what they
// write to its share. A suite that asserted the codec alone would certify the
// author's arithmetic and nothing about what lands in the store, so the raw
// cases read the values' own bytes.

// bytesCases are the cases that read the row and its log through the store.
var bytesCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"ACallTheLogDropsKeepsItsIdentity", testACallTheLogDropsKeepsItsIdentity},
	{"TheNewestSubmissionIsKeptWholeForItsReplay", testTheNewestSubmissionIsKeptWholeForItsReplay},
	{"ANewRoundFoldsTheDroppedCallsIntoTheTurnsEarlierCalls", testANewRoundFoldsTheDroppedCallsIntoTheTurnsEarlierCalls},
	{"AParkedStatePastItsShareIsRefused", testAParkedStatePastItsShareIsRefused},
	{"ARunsTitleIsHeldToItsShare", testARunsTitleIsHeldToItsShare},
	{"AnAppendCarriesWhatItsSessionOwedWhole", testAnAppendCarriesWhatItsSessionOwedWhole},
	{"AnOwedAppendThatLandedIsListedOnce", testAnOwedAppendThatLandedIsListedOnce},
	{"AnAppendIsTheSameCallOnlyAtItsPlaceInstantNameAndIdentity",
		testAnAppendIsTheSameCallOnlyAtItsPlaceInstantNameAndIdentity},
	{"AWithheldCallIsKeptWithoutItsIdentity", testAWithheldCallIsKeptWithoutItsIdentity},
	{"AnAnswerPastItsShareIsRefused", testAnAnswerPastItsShareIsRefused},
	{"AnAnswerPastItsShareToAQuestionNoLongerWaitingIsNoRefusal",
		testAnAnswerPastItsShareToAQuestionNoLongerWaitingIsNoRefusal},
	{"AHeldCallPastItsShareIsRefused", testAHeldCallPastItsShareIsRefused},
	{"ANotePastItsBoundIsRefused", testANotePastItsBoundIsRefused},
	{"AQuestionIsPutToTheFirstOfItsAudience", testAQuestionIsPutToTheFirstOfItsAudience},
}

// bytesRawCases are the cases that read the raw record or the raw log.
var bytesRawCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore, coord.SandboxRuns)
}{
	{"ABridgedCallCarryingAMegabyteIsKeptWithinItsShare", testABridgedCallCarryingAMegabyteIsKeptWithinItsShare},
	{"TwoHundredFullAnswersLeaveTheLogWithinItsBudget", testTwoHundredFullAnswersLeaveTheLogWithinItsBudget},
	{"CallsDroppedWithNoIdentityReadAsCallsOfNoKnownTool", testCallsDroppedWithNoIdentityReadAsCallsOfNoKnownTool},
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

// rawLog is the run's bridged call log as the coordination store holds it
// beside the record.
func rawLog(t *testing.T, runs coord.SandboxRuns, turnID string) []byte {
	t.Helper()
	record, found, err := runs.SandboxRunPart(t.Context(), turnID, coord.RunLog)
	if err != nil || !found {
		t.Fatalf("read the raw log of %s: %v %v", turnID, found, err)
	}
	return record.Value
}

// A BRIDGED CALL CARRYING A MEGABYTE IS KEPT WITHIN ITS SHARE. A commit's file
// of a megabyte of markup is the arguments' largest value, so it is left out —
// a marker naming its size and digest in its place — while the repository,
// the branch and the file's path stay whole beside it; the call states the
// size of what it was sent with, keeps its identity, and adds no more to the
// log than its share.
//
// Mutation: record the call as the bridge handed it, and the log grows by six
// megabytes of escapes; drop the identity, and a resumed run cannot count the
// call it was.
func testABridgedCallCarryingAMegabyteIsKeptWithinItsShare(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	r := run("t-bytes-commit")
	launch := mustLaunched(t, s, r)
	before := len(rawLog(t, runs, r.TurnID))
	args := commitArgs(t, strings.Repeat("<", 1<<20))
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "code_commit_files", Args: args, Output: "committed abc123",
			Digest: digestOf(1), Seq: 1}}); err != nil || !recorded {
		t.Fatalf("AppendBridgeCall = %v, %v", recorded, err)
	}
	if grew := len(rawLog(t, runs, r.TurnID)) - before; grew > sandbox.MaxBridgeCallBytes {
		t.Errorf("one call grew the log by %d bytes, past its share %d", grew, sandbox.MaxBridgeCallBytes)
	}
	got := mustLogged(t, s, r.TurnID).Log.Calls
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
		t.Fatalf("what the log kept is not the call's arguments: %v", err)
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

// callsBytes is what a raw log's calls weigh in it: the keys sandbox's
// BridgeLogBytes is the share of, and their values, as written.
func callsBytes(t *testing.T, log []byte) int {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(log, &fields); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, key := range []string{"calls", "elided", "elided_calls", "absorbed_calls"} {
		if value, ok := fields[key]; ok {
			n += len(`,"":`) + len(key) + len(value)
		}
	}
	return n
}

// TWO HUNDRED FULL ANSWERS LEAVE THE LOG WITHIN ITS BUDGET. A long run whose
// every call answered a builtin's whole 64 KiB page — and sixty more past the
// log's count, dropped from its middle — leaves a log whose calls weigh no
// more than their share (sandbox's BridgeLogBytes) and which is a value the
// broker takes (sandbox's RowBytes), every call stating the size it answered
// and the dropped ones counted by identity; and the row beside it is a value
// the broker takes too, every append having written nothing but its spend
// there.
//
// Mutation: keep each call's output whole, and the log weighs 12.8 MiB of
// pages before a byte of their escapes, past a value's budget.
func testTwoHundredFullAnswersLeaveTheLogWithinItsBudget(t *testing.T, s sandbox.PendingStore,
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
	logged := rawLog(t, runs, r.TurnID)
	if n := callsBytes(t, logged); n > sandbox.BridgeLogBytes {
		t.Errorf("the log's calls weigh %d bytes, past their share %d", n, sandbox.BridgeLogBytes)
	}
	if len(logged) > sandbox.RowBytes {
		t.Errorf("the log weighs %d bytes, past a value's budget %d", len(logged), sandbox.RowBytes)
	}
	if record := raw(t, runs, r.TurnID); len(record) > sandbox.RowBytes {
		t.Errorf("the record weighs %d bytes, past the row's budget %d", len(record), sandbox.RowBytes)
	}
	got := mustLogged(t, s, r.TurnID).Log
	for _, call := range got.Calls {
		if call.OutputBytes != len(page) || kept.Quoted(call.Output) > sandbox.MaxBridgeOutputBytes {
			t.Fatalf("call %d keeps %d bytes of output stating %d, want at most %d stating %d", call.Seq,
				kept.Quoted(call.Output), call.OutputBytes, sandbox.MaxBridgeOutputBytes, len(page))
		}
	}
	if got.Elided != 60 || !got.ElisionKnown() {
		t.Errorf("%d calls dropped, accounted %v; want 60, every one by identity",
			got.Elided, got.ElisionKnown())
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
	got := mustLogged(t, s, r.TurnID).Log
	if got.Elided != 30 || len(got.Calls) != sandbox.MaxBridgeCalls {
		t.Fatalf("%d kept and %d dropped, want %d and 30", len(got.Calls), got.Elided,
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
// run's newest successful `submit_work` off its log as the round's outcome, so
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
		for _, call := range mustLogged(t, s, r.TurnID).Log.Calls {
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
		t.Fatalf("submission %d is not in the log", seq)
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
// incomplete. The new round's log starts with neither tally: the identities of
// the calls the last round dropped, and of the calls its calls' own work made,
// are the last round's.
//
// Mutation: fold the kept calls alone, and the reply the log dropped is
// missing from the turn's earlier calls; carry the tallies into the new
// round's log, and the last round's work is seeded into the next.
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
			Call:     sandbox.BridgeCall{Name: name, Digest: digestOf(i), Seq: i + 1},
			Absorbed: []sandbox.ElidedCall{{Name: "leave_channel", Digest: digestOf(i), Calls: 1}}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	before := mustLogged(t, s, r.TurnID).Log
	if len(before.ElidedCalls) == 0 || len(before.AbsorbedCalls) == 0 {
		t.Fatal("the round kept no tally; the case needs both")
	}
	for _, call := range before.Calls {
		if call.Name == "post_message" {
			t.Fatal("the reply was kept; the case needs it dropped from the middle")
		}
	}
	next := run(r.TurnID)
	if _, err := s.BeginLaunch(ctx, next, sandbox.Fence{}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	got := mustLogged(t, s, r.TurnID).Log
	if len(got.Calls) != 0 || got.Elided != 0 || len(got.ElidedCalls) != 0 || len(got.AbsorbedCalls) != 0 {
		t.Errorf("the new round opened with %d calls, %d dropped and %d and %d identities; want none",
			len(got.Calls), got.Elided, len(got.ElidedCalls), len(got.AbsorbedCalls))
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

// callAt is a call the bridge ran at place seq, handed to the surface at
// started, as an append hands it to the log.
func callAt(name string, seq int, started time.Time, digest string) sandbox.BridgeCall {
	return sandbox.BridgeCall{Name: name, Seq: seq, StartedAt: started, Digest: digest}
}

// listed is how many times the log lists a call at place seq under name.
func listed(log sandbox.BridgeLog, name string, seq int) int {
	n := 0
	for _, c := range log.Calls {
		if c.Name == name && c.Seq == seq {
			n++
		}
	}
	return n
}

// AN APPEND CARRIES WHAT ITS SESSION OWED, WHOLE, AND LANDS ONCE. A bridged
// call whose own append did not land is owed by its session as the append it
// was — the call and what its own work absorbed — and a later append carries
// it: the log LISTS it, as the call it is, beside the call that carried it,
// so a resumed call log seeds it by its identity, the delivery checks read
// its outcome, and the board shows it where it was made. An append that names
// no call of its own — what a session sends once its job is over — carries
// what it owes all the same. The calls a call's own work made — a delegated
// worker's — ride the same write into a tally of their own, which no reader of
// the round's deliveries counts. And an append whose answer was lost, asked
// again, is answered recorded and changes nothing.
//
// Mutation: drop the owed appends, or what they absorbed, and a resumed run
// counts fewer calls than its process did; count an owed call as a call the
// log does not list, and the board says a call from the middle went missing
// that the log could have shown; append a repeat again, and the calls and the
// tally double.
func testAnAppendCarriesWhatItsSessionOwedWhole(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-owed")
	launch := mustLaunched(t, s, r)
	a := sandbox.BridgeAppend{Launch: launch,
		Call:     callAt("delegate", 3, base.Add(3*time.Second), digestOf(3)),
		Absorbed: []sandbox.ElidedCall{{Name: "leave_channel", Digest: digestOf(9), Calls: 1}},
		Owed: []sandbox.OwedAppend{
			{Call: callAt("comment_on_work_item", 1, base.Add(time.Second), digestOf(1))},
			{Call: sandbox.BridgeCall{Name: "read_file", Seq: 2, StartedAt: base.Add(2 * time.Second),
				Digest: digestOf(2), Failed: true},
				Absorbed: []sandbox.ElidedCall{{Name: "join_channel", Digest: digestOf(8), Calls: 1}}},
		},
	}
	for attempt := range 2 {
		if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, a); err != nil || !recorded {
			t.Fatalf("append, attempt %d = %v, %v; want it recorded", attempt+1, recorded, err)
		}
	}
	got := mustLogged(t, s, r.TurnID).Log
	if len(got.Calls) != 3 || got.Elided != 0 || len(got.Unlisted()) != 0 {
		t.Fatalf("the log lists %d calls and counts %d it does not list (%+v), want the call and the two "+
			"it carried listed, and none unlisted", len(got.Calls), got.Elided, got.Unlisted())
	}
	for _, want := range []struct {
		name string
		seq  int
	}{{"comment_on_work_item", 1}, {"read_file", 2}, {"delegate", 3}} {
		if n := listed(got, want.name, want.seq); n != 1 {
			t.Errorf("the log lists %s at %d %d times, want once", want.name, want.seq, n)
		}
	}
	if !got.Calls[1].Failed {
		t.Error("the owed call that failed is listed as a success")
	}
	absorbed := got.Absorbed()
	if len(absorbed) != 2 || absorbed[0].Name != "join_channel" || absorbed[1].Name != "leave_channel" ||
		absorbed[0].Calls != 1 || absorbed[1].Calls != 1 {
		t.Errorf("the calls' own work reads %+v, want each worker's one call", absorbed)
	}

	// A CARRY THAT NAMES NO CALL OF ITS OWN, as a session sends one at its
	// job's end: what it owed is listed all the same.
	carry := sandbox.BridgeAppend{Launch: launch, Owed: []sandbox.OwedAppend{
		{Call: callAt(sandbox.SubmitWorkTool, 4, base.Add(4*time.Second), digestOf(4))}}}
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, carry); err != nil || !recorded {
		t.Fatalf("a carry naming no call = %v, %v; want it recorded", recorded, err)
	}
	if got := mustLogged(t, s, r.TurnID).Log; len(got.Calls) != 4 || listed(got, sandbox.SubmitWorkTool, 4) != 1 {
		t.Errorf("after the carry the log lists %+v, want the owed submission beside the three", got.Calls)
	}
	// AND A CARRY FOR A JOB THE LOG IS NOT OPEN FOR is not recorded, and is
	// said so: a relaunch replaced the job its calls were made in.
	carry.Launch = "another-job"
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, carry); err != nil || recorded {
		t.Errorf("a carry for another job = %v, %v; want it unrecorded, without an error", recorded, err)
	}
}

// AN OWED APPEND THAT LANDED IS LISTED ONCE. A session cannot tell an append
// that did not land from one that landed and lost every answer — the broker
// took the write and the connection dropped before its acknowledgement — so it
// owes both alike, whole, with what they carried, and a later append carries
// them. The log lists each call it does not hold, and skips the one it does,
// with what that one absorbed and what it carried: all of it landed in the
// write that listed it. So a call is listed once, and its work counted once,
// however many appends carried it.
//
// Mutation: list or count an owed call without asking whether the log holds
// it already, and the landed call is listed twice and its worker's call
// counted twice.
func testAnOwedAppendThatLandedIsListedOnce(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-owed-landed")
	launch := mustLaunched(t, s, r)
	c0 := sandbox.OwedAppend{Call: callAt("read_page", 5, base.Add(5*time.Second), digestOf(5))}
	x := sandbox.OwedAppend{Call: callAt("delegate", 7, base.Add(7*time.Second), digestOf(7)),
		Absorbed: []sandbox.ElidedCall{{Name: "leave_channel", Digest: digestOf(70), Calls: 1}}}
	// X LANDS, carrying C0 — and its session, its answer lost, owes both.
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: x.Call, Absorbed: x.Absorbed, Owed: []sandbox.OwedAppend{c0}}); err != nil || !recorded {
		t.Fatalf("X's append = %v, %v; want it recorded", recorded, err)
	}
	// Y CARRIES THEM, as its session owed them.
	if recorded, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: callAt("post_message", 8, base.Add(8*time.Second), digestOf(8)),
		Owed: []sandbox.OwedAppend{c0, x}}); err != nil || !recorded {
		t.Fatalf("Y's append = %v, %v; want it recorded", recorded, err)
	}
	got := mustLogged(t, s, r.TurnID).Log
	for _, want := range []struct {
		name string
		seq  int
	}{{"read_page", 5}, {"delegate", 7}, {"post_message", 8}} {
		if n := listed(got, want.name, want.seq); n != 1 {
			t.Errorf("the log lists %s at %d %d times, want once", want.name, want.seq, n)
		}
	}
	if len(got.Calls) != 3 || got.Elided != 0 {
		t.Errorf("the log lists %d calls and counts %d it does not list, want the three and none",
			len(got.Calls), got.Elided)
	}
	if absorbed := got.Absorbed(); len(absorbed) != 1 || absorbed[0].Calls != 1 {
		t.Errorf("X's own work is counted %+v, want its worker's one call once", absorbed)
	}
}

// AN APPEND IS THE SAME CALL ONLY AT ITS PLACE, INSTANT, NAME AND IDENTITY. The
// log knows a call it already holds by all four as it keeps them, so an append
// asked again lands once; and a call that differs from one the log holds in
// any ONE of them is another call, and is listed beside it. Each has a call
// behind it that the other three cannot tell apart: a session opened again on
// the job numbers its calls after the log's last, so a call the closed session
// had in flight can land at a place the new one reuses (the place alone); a
// model that makes the same call again in a reopened session makes it at
// another instant (the instant); and calls made side by side, or one tool
// after another, are told apart by what they asked for and which tool they
// named (the identity, the name).
//
// Mutation: drop any one of the four from the comparison, and the call that
// differs from the landed one in it alone is answered as the landed one's
// retry and never listed.
func testAnAppendIsTheSameCallOnlyAtItsPlaceInstantNameAndIdentity(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-same-call")
	launch := mustLaunched(t, s, r)
	landed := callAt("post_message", 5, base, digestOf(1))
	appended := func(call sandbox.BridgeCall) {
		t.Helper()
		if recorded, err := s.AppendBridgeCall(ctx, r.TurnID,
			sandbox.BridgeAppend{Launch: launch, Call: call}); err != nil || !recorded {
			t.Fatalf("append of %+v = %v, %v; want it recorded", call, recorded, err)
		}
	}
	appended(landed)
	for _, tc := range []struct {
		what string
		call sandbox.BridgeCall
	}{
		{"another place", callAt("post_message", 6, base, digestOf(1))},
		{"another instant", callAt("post_message", 5, base.Add(time.Second), digestOf(1))},
		{"another tool", callAt("read_file", 5, base, digestOf(1))},
		{"another identity", callAt("post_message", 5, base, digestOf(2))},
	} {
		before := len(mustLogged(t, s, r.TurnID).Log.Calls)
		appended(tc.call)
		if after := len(mustLogged(t, s, r.TurnID).Log.Calls); after != before+1 {
			t.Errorf("%s: the log lists %d calls after it, want %d: a call that differs from the landed "+
				"one in %s alone was taken for its retry", tc.what, after, before+1, tc.what)
		}
	}
	// AND THE LANDED CALL ASKED AGAIN is its retry, listed no more.
	before := len(mustLogged(t, s, r.TurnID).Log.Calls)
	appended(landed)
	if after := len(mustLogged(t, s, r.TurnID).Log.Calls); after != before {
		t.Errorf("the landed call asked again left %d calls listed, want %d", after, before)
	}
}

// A WITHHELD CALL IS KEPT WITHOUT ITS IDENTITY. A call held for a person's
// approval, and the call they approved, carry their arguments sealed on the
// held call (ADR-0077): a digest of them in the clear on the call's record
// would confirm a guess at what the seal hides. So the log keeps neither,
// whichever writer hands them — the bridge's append of the held call, the
// resume's record of the approved one — and a resumed call log counts each as a
// call of its tool whose arguments are not known.
//
// Mutation: keep a withheld call's digest, and the log holds it in the clear.
func testAWithheldCallIsKeptWithoutItsIdentity(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-withheld")
	launch := mustLaunched(t, s, r)
	if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "tr_close_issue", Held: true, Failed: true, Args: `{"id":"7"}`,
			Digest: digestOf(7), Seq: 1}}); err != nil {
		t.Fatalf("append the held call: %v", err)
	}
	held := mustLogged(t, s, r.TurnID).Log.Calls[0]
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

// AN ANSWER PAST ITS SHARE TO A QUESTION NO LONGER WAITING IS NO REFUSAL. What a
// reply weighs is decided against the run as the write finds it: to a run
// answered another way since the caller matched it, or asked of a question the
// run no longer waits on, the reply is false with no error, whatever it weighs,
// as every reply after an answer is — the ordinary message it looks like, never
// a refusal saying the run waits for a shorter answer to a question it is no
// longer asking. The run is left as the earlier answer left it.
//
// Mutation: refuse the share before the row is read, as the store did, and
// both replies come back ErrAnswerTooLarge.
func testAnAnswerPastItsShareToAQuestionNoLongerWaitingIsNoRefusal(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-answered")
	launch := mustLaunched(t, s, r)
	park(t, s, r.TurnID)
	if _, ok, err := s.RecordAnswer(ctx, r.TurnID, launch, replyBatch(1, 64), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("the premise: a short reply = %v, %v; want it recorded", ok, err)
	}
	for name, asked := range map[string]string{
		"answered another way":          launch,
		"a question it no longer waits": launch + "-earlier",
	} {
		got, ok, err := s.RecordAnswer(ctx, r.TurnID, asked, replyBatch(20, 32<<10), sandbox.Fence{})
		if err != nil || ok || got.TurnID != "" {
			t.Errorf("%s: a reply past its share = %+v, %v, %v; want false with no error, the "+
				"reply after an answer that it is", name, got.TurnID, ok, err)
		}
	}
	if got := mustGet(t, s, r.TurnID); got.Status != sandbox.StatusAnswered || got.Answer == nil ||
		len(got.Answer.EventIDs) != 1 {
		t.Errorf("the run is %s holding %+v; want it answered by the short reply alone", got.Status, got.Answer)
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

// A QUESTION IS PUT TO THE FIRST OF ITS AUDIENCE. A `team` audience is every
// person in the asking seat's unit, which nothing else bounds, and the row
// carries the seats a question is put to; the park holds them to their bound
// (sandbox's MaxAudienceHandles) and keeps the first, in the order the
// resolution names them — the unit's lead, then its members in the chart's
// order.
//
// Mutation: park the question on every seat it resolved to, and a unit of
// thousands rides the row.
func testAQuestionIsPutToTheFirstOfItsAudience(t *testing.T, s sandbox.PendingStore) {
	ctx := t.Context()
	r := run("t-bytes-audience")
	mustLaunched(t, s, r)
	Ask(t, s, r.TurnID, Question("which branch?", sandbox.AskToTeam))
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
		t.Errorf("the question is put to %d seats, %v first; want the first %d", len(got), got[:1],
			sandbox.MaxAudienceHandles)
	}
}

// CALLS DROPPED WITH NO IDENTITY READ AS CALLS OF NO KNOWN TOOL. A build that
// knows no tally drops calls from the log's middle and counts them, keeping no
// identity of them — in a fleet mid-upgrade, an append of an older build on the
// round's log. Read by this build, calls the log counts and its tally does not
// account for are calls whose tool is not known: counted against every later
// call, and a record no reader of whether the turn replied can read whole. The
// next append of this build keeps the tally accounting for them, from what it
// reads.
//
// Mutation: read the tally off the log as it stands, and three calls the round
// made are seeded as none and its record read as whole.
func testCallsDroppedWithNoIdentityReadAsCallsOfNoKnownTool(t *testing.T, s sandbox.PendingStore,
	runs coord.SandboxRuns,
) {
	ctx := t.Context()
	r := run("t-bytes-unknown")
	launch := mustLaunched(t, s, r)
	if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_file", Digest: digestOf(1), Seq: 1}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// AN OLDER BUILD DROPS THREE CALLS of the round, keeping no identity.
	rewriteLog(t, runs, r.TurnID, func(log map[string]json.RawMessage) {
		log["elided"] = json.RawMessage(`3`)
	})
	got := mustLogged(t, s, r.TurnID).Log
	if e := got.Unlisted(); len(e) != 1 || e[0].Name != "" || e[0].Calls != 3 || got.ElisionKnown() {
		t.Errorf("three calls dropped with no identity read %+v (known %v), want three calls of no known tool",
			e, got.ElisionKnown())
	}
	if _, err := s.AppendBridgeCall(ctx, r.TurnID, sandbox.BridgeAppend{Launch: launch,
		Call: sandbox.BridgeCall{Name: "read_file", Digest: digestOf(500), Seq: 500}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	got = mustLogged(t, s, r.TurnID).Log
	if e := got.ElidedCalls; len(e) != 1 || e[0].Name != "" || e[0].Calls != 3 || len(got.AbsorbedCalls) != 0 {
		t.Errorf("this build's next append keeps %+v dropped and %+v absorbed, want the round's own: three "+
			"calls of no known tool, nothing absorbed", e, got.AbsorbedCalls)
	}
}

// rewriteLog changes turnID's raw log as a build that does not know some of its
// keys would write it, through the store the log is held in.
func rewriteLog(t *testing.T, runs coord.SandboxRuns, turnID string, change func(map[string]json.RawMessage)) {
	t.Helper()
	_, _, err := runs.MutateSandboxRunPart(t.Context(), turnID, coord.RunLog,
		func(record coord.Record, found bool) ([]byte, error) {
			if !found {
				return nil, errors.New("the run has no log to rewrite")
			}
			var log map[string]json.RawMessage
			if err := json.Unmarshal(record.Value, &log); err != nil {
				return nil, err
			}
			change(log)
			return json.Marshal(log)
		})
	if err != nil {
		t.Fatalf("rewrite the raw log of %s: %v", turnID, err)
	}
}

// A CAPTURE STAMP PAST ITS BOUNDS IS REFUSED, BEFORE ANYTHING IS WRITTEN. What a
// box says of its session — the release of the harness that wrote it, read off
// a file the run's own agent can write — reaches the row on the capture's
// stamp, twice once it lands (the place, and the turn's kept capture). The
// store holds every such field to the bound the executor log holds it to and
// refuses a stamp past one by name, leaving the record as it was; a stamp
// within them is taken. So is the process the stamp names as its taker held to
// what a run's row carries of a process incarnation.
//
// Mutation: stamp the capture whatever it carries, and a release of a megabyte
// of digits is on the row; drop the taker from the stamp's bounds, and a
// kilobyte taker is.
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
	stamp.Taker = strings.Repeat("n", 1<<10) + ":1"
	if took, err := s.StampCapture(ctx, r.TurnID, stamp); !errors.Is(err, sandbox.ErrCaptureUnbounded) || took {
		t.Fatalf("a stamp naming a kilobyte taker = %v, %v; want ErrCaptureUnbounded", took, err)
	}
	if after := raw(t, runs, r.TurnID); !bytes.Equal(after, before) {
		t.Fatalf("the stamp refused for its taker changed the record: %d bytes to %d", len(before), len(after))
	}
	stamp.Taker = "node-a:" + incarnationSuffix
	if took, err := s.StampCapture(ctx, r.TurnID, stamp); err != nil || !took {
		t.Errorf("a stamp within its bounds = %v, %v; want it taken", took, err)
	}
}

// incarnationSuffix is the uuid half of a process incarnation, `<node>:<uuid>`,
// as internal/config's NewIncarnation mints it whole.
const incarnationSuffix = "4f2a9c1e-7b3d-4e8a-9f61-2c5d8e0b7a34"

package sandboxtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
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
}

// bytesRawCases are the cases that read the raw record.
var bytesRawCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore, coord.SandboxRuns)
}{
	{"ABridgedCallCarryingAMegabyteIsKeptWithinItsShare", testABridgedCallCarryingAMegabyteIsKeptWithinItsShare},
	{"TwoHundredFullAnswersLeaveTheRowWithinItsBudget", testTwoHundredFullAnswersLeaveTheRowWithinItsBudget},
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
	for _, key := range []string{"bridge_calls", "bridge_calls_elided", "bridge_elided_calls"} {
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

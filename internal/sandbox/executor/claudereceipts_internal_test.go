package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The takes and firings of the hand-built threads: synthetic ids alone.
var (
	rhBatchTake  = rcUUID(201)
	rhStopTake   = rcUUID(202)
	rhUntaken    = rcUUID(203)
	rhLaterTake  = rcUUID(204)
	rhBatchFire  = "hook-" + rcUUID(101)
	rhStopFire   = rcUUID(102)
	rhLaterFire  = "hook-" + rcUUID(103)
	rhOtherFire  = "hook-" + rcUUID(104)
	rhBatch      = claudeEventToolBatch
	rhStop       = claudeEventFinish
	rhAskBatch   = sandbox.AskedTake{Take: rhBatchTake, Boundary: types.SteerBoundaryToolBatch}
	rhAskStop    = sandbox.AskedTake{Take: rhStopTake, Boundary: types.SteerBoundaryFinish}
	rhAskUntaken = sandbox.AskedTake{Take: rhUntaken, Boundary: types.SteerBoundaryFinish}
)

// rhSuccess is a hook's success at event, of firing, whose stdout, the hook's
// whole answer, names each of takes' tokens.
func rhSuccess(event, firing string, takes ...string) string {
	said := ""
	for _, take := range takes {
		said += " " + claudeTakeToken(take)
	}
	return `{"type":"hook_success","hookName":"` + event + `","toolUseID":"` + firing + `","hookEvent":"` + event +
		`","content":"","stdout":"{\"systemMessage\":\"crewlet: a note reached the run` + said +
		`\"}","stderr":"","exitCode":0}`
}

// rhSystem is a hook's system message at event, of firing, naming takes.
func rhSystem(event, firing string, takes ...string) string {
	said := ""
	for _, take := range takes {
		said += " " + claudeTakeToken(take)
	}
	return `{"type":"hook_system_message","content":"crewlet: a note reached the run` + said + `","hookName":"` +
		event + `","toolUseID":"` + firing + `","hookEvent":"` + event + `"}`
}

// rhCancelled is a firing at event past its hook's timeout.
func rhCancelled(event, firing string) string {
	return `{"type":"hook_cancelled","hookName":"` + event + `","toolUseID":"` + firing + `","hookEvent":"` + event +
		`","timedOut":true,"timeoutMs":10000}`
}

// rhRecord is one record of a hand-built thread, made of its parent's number,
// its own and its time, as the rc helpers make one.
type rhRecord func(parent, n, ms int) string

func rhAttach(attachment string) rhRecord {
	return func(parent, n, ms int) string { return rcAttach(parent, n, ms, attachment) }
}

func rhReply(content string) rhRecord {
	return func(parent, n, ms int) string { return rcReply(parent, n, fmt.Sprintf("msg_%d", n), ms, content) }
}

func rhResults(ids ...string) rhRecord {
	return func(parent, n, ms int) string { return rcResults(parent, n, ms, ids...) }
}

// rhPair is a firing's three records at event, its success and its system
// message naming takes, then its additional context.
func rhPair(event, firing string, takes ...string) []rhRecord {
	return []rhRecord{rhAttach(rhSuccess(event, firing, takes...)), rhAttach(rhSystem(event, firing, takes...)),
		rhAttach(rcContextOf(event, firing))}
}

// rhChained is records chained in order, each the next's parent, the first
// rooting the thread under a prompt.
func rhChained(records ...[]rhRecord) []string { return rhOn([]string{rcPrompt(0, 1, 0)}, records...) }

// rhOn is lines, then records chained in order after its last.
func rhOn(lines []string, records ...[]rhRecord) []string {
	lines = slices.Clone(lines)
	for _, group := range records {
		for _, record := range group {
			n := len(lines) + 1
			lines = append(lines, record(n-1, n, n*100))
		}
	}
	return lines
}

// rhCall is a reply calling id and its result.
func rhCall(id string) []rhRecord { return []rhRecord{rhReply(rcCalls(id)), rhResults(id)} }

// rhBase is a thread whose batch hook took rhBatchTake and whose Stop hook
// took rhStopTake, each its whole pair.
func rhBase() []string {
	return rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), []rhRecord{rhReply(rcSaid("done"))},
		rhPair(rhStop, rhStopFire, rhStopTake))
}

// rhProbe is the lines of a probe's kept main thread before its /compact: a
// batch's and a Stop's pairs as the release wrote them, on a thread every size
// path reads alike, which the whole thread is not ([rhProbeWhole]).
func rhProbe(t *testing.T, thread string) []string {
	t.Helper()
	lines := rhProbeWhole(t, thread)
	cut := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, `"subtype":"compact_boundary"`) })
	if cut < 0 {
		t.Fatalf("%s holds no compaction to cut before", thread)
	}
	return lines[:cut]
}

// rhProbeWhole is the lines of a probe's whole kept main thread, its /compact
// included.
func rhProbeWhole(t *testing.T, thread string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude", thread))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// rhRead is what readMainThread answers of lines, the box's main thread, asked
// takes alone as a stop's or a loss's read asks them, at release.
func rhRead(t *testing.T, release string, ended bool, lines []string, takes ...sandbox.AskedTake) sandbox.TakeReceipts {
	t.Helper()
	box, paths := cntBox(cntThread(lines...), nil)
	return ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
		Release: release, Takes: takes, Ended: ended}).Receipts
}

// rhWant holds got to the takes held, restarted and absent.
func rhWant(t *testing.T, at string, got sandbox.TakeReceipts, held, restarted, absent []string) {
	t.Helper()
	norm := func(s []string) []string { return slices.Sorted(slices.Values(s)) }
	if !slices.Equal(norm(got.Held), norm(held)) || !slices.Equal(norm(got.Restarted), norm(restarted)) ||
		!slices.Equal(norm(got.Absent), norm(absent)) {
		t.Errorf("%s: held %v, restarted %v, absent %v (%s); want held %v, restarted %v, absent %v", at, got.Held,
			got.Restarted, got.Absent, got.Why, held, restarted, absent)
	}
}

// A TAKE IS HELD ONLY WHERE ITS TOKEN AND A CONTEXT SHARE A FIRING: a system
// message naming it, then that firing's additional context at its event. Its
// token in a success's stdout alone, a context of another firing or before
// the message, or a firing of the other event holds nothing.
//
// Mutation: open a pair on any hook record whose words name the token (a
// byte search), and the stdout case goes red.
func TestATakeIsHeldOnlyWhereItsTokenAndAContextShareAFiring(t *testing.T) {
	t.Parallel()
	for _, thread := range ctProbeThreads {
		release := strings.Split(thread, "/")[0]
		got := rhRead(t, release, true, rhProbe(t, thread),
			sandbox.AskedTake{Take: ctProbeBatchTake, Boundary: types.SteerBoundaryToolBatch}, sandbox.AskedTake{Take: ctProbeStopTake,
				Boundary: types.SteerBoundaryFinish})
		rhWant(t, thread, got, []string{ctProbeBatchTake, ctProbeStopTake}, nil, nil)
	}
	rhWant(t, "the whole pairs", rhRead(t, "2.1.322", true, rhBase(), rhAskBatch, rhAskStop),
		[]string{rhBatchTake, rhStopTake}, nil, nil)
	for name, batch := range map[string][]rhRecord{
		"a context of another firing": {rhAttach(rhSuccess(rhBatch, rhBatchFire, rhBatchTake)),
			rhAttach(rhSystem(rhBatch, rhBatchFire, rhBatchTake)), rhAttach(rcContextOf(rhBatch, rhOtherFire))},
		"a context before its system message": {rhAttach(rhSuccess(rhBatch, rhBatchFire, rhBatchTake)),
			rhAttach(rcContextOf(rhBatch, rhBatchFire)), rhAttach(rhSystem(rhBatch, rhBatchFire, rhBatchTake))},
		"the token in its success's stdout alone": {rhAttach(rhSuccess(rhBatch, rhBatchFire, rhBatchTake)),
			rhAttach(rhSystem(rhBatch, rhBatchFire)), rhAttach(rcContextOf(rhBatch, rhBatchFire))},
		"a firing of the other event": rhPair(rhStop, rhBatchFire, rhBatchTake),
	} {
		lines := rhChained(rhCall("x1"), batch, []rhRecord{rhReply(rcSaid("done"))},
			rhPair(rhStop, rhStopFire, rhStopTake))
		rhWant(t, name, rhRead(t, "2.1.322", true, lines, rhAskBatch, rhAskStop), []string{rhStopTake}, nil, nil)
	}
}

// A PAIR BEFORE THE LAST RESTART IS RESTARTED, one after it held: a restart
// that lies after a pair's system message may have dropped it from what a
// resume hands.
//
// Mutation: drop the restart check, and the pairs before it read as held.
func TestAPairBeforeTheLastRestartIsRestarted(t *testing.T) {
	t.Parallel()
	restart := []rhRecord{func(parent, n, ms int) string { return rcSystem(parent, n, ms, "microcompact_boundary", "") }}
	after := rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), []rhRecord{rhReply(rcSaid("done"))},
		rhPair(rhStop, rhStopFire, rhStopTake), restart)
	rhWant(t, "a restart after both", rhRead(t, "2.1.322", true, after, rhAskBatch, rhAskStop), nil,
		[]string{rhBatchTake, rhStopTake}, nil)
	between := rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), restart,
		[]rhRecord{rhReply(rcSaid("done"))}, rhPair(rhStop, rhStopFire, rhStopTake))
	rhWant(t, "a restart between them", rhRead(t, "2.1.322", true, between, rhAskBatch, rhAskStop),
		[]string{rhStopTake}, []string{rhBatchTake}, nil)
}

// rhCleared is a reply whose request's context edit is report, as written.
func rhCleared(report string) rhRecord {
	return func(parent, n, ms int) string {
		reply := rcReply(parent, n, fmt.Sprintf("msg_%d", n), ms, rcSaid("answered"))
		return strings.Replace(reply, `"stop_reason":"end_turn"}`, `"stop_reason":"end_turn","context_management":`+
			report+`}`, 1)
	}
}

// A BATCH PAIR BEFORE A CLEARED REPLY IS RESTARTED, AND A STOP PAIR IS NOT: a
// reply whose request cleared tool results, or whose report does not
// read, may have cleared a batch's note with its result, never a Stop's.
//
// Mutation: read no reply's clearing, or read it for a Stop's pair too, and a
// case goes red.
func TestABatchPairBeforeAClearedReplyIsRestartedAndAStopPairIsNot(t *testing.T) {
	t.Parallel()
	for name, report := range map[string]string{
		"a reply that cleared results":    `{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3}]}`,
		"a reply whose report is no edit": `7`,
	} {
		lines := rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake),
			[]rhRecord{rhReply(rcSaid("done"))}, rhPair(rhStop, rhStopFire, rhStopTake), []rhRecord{rhCleared(report)})
		rhWant(t, name, rhRead(t, "2.1.322", true, lines, rhAskBatch, rhAskStop), []string{rhStopTake},
			[]string{rhBatchTake}, nil)
	}
	kept := rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), []rhRecord{rhReply(rcSaid("done"))},
		rhPair(rhStop, rhStopFire, rhStopTake), []rhRecord{rhCleared(`{"applied_edits":[]}`)})
	rhWant(t, "a reply that cleared nothing", rhRead(t, "2.1.322", true, kept, rhAskBatch, rhAskStop),
		[]string{rhBatchTake, rhStopTake}, nil, nil)
}

// A PAIR THE RESUME DOES NOT HAND IS NOT HELD, and is evidence its take
// arrived, so never absent: a pair a retraction withdrew either record of, and
// one off the chain the leaf walks, under a parent naming no record.
//
// Mutation: hold a pair without asking the chain whether it is handed, read a
// take whose pair is not handed as absent, or feed the reading a line before
// the chain (each record read by the line before's handle), and a case goes
// red.
func TestAPairTheResumeDoesNotHandIsNotHeld(t *testing.T) {
	t.Parallel()
	later := sandbox.AskedTake{Take: rhLaterTake, Boundary: types.SteerBoundaryToolBatch}
	// The batch's system message is record 5; the fallback after the Stop's
	// pair takes it back.
	withdrawn := rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), rhCall("x2"),
		rhPair(rhBatch, rhLaterFire, rhLaterTake), []rhRecord{rhReply(rcSaid("done"))},
		rhPair(rhStop, rhStopFire, rhStopTake), []rhRecord{func(parent, n, ms int) string {
			return rcFallback(parent, n, ms, rcQuoted(5))
		}})
	rhWant(t, "a withdrawn system message", rhRead(t, "2.1.322", true, withdrawn, rhAskBatch, later, rhAskStop),
		[]string{rhLaterTake, rhStopTake}, nil, nil)
	// Its context, record 6, taken back instead: its system message, handed,
	// holds nothing alone.
	withdrawn[len(withdrawn)-1] = rcFallback(15, 16, 1600, rcQuoted(6))
	rhWant(t, "a withdrawn additional context", rhRead(t, "2.1.322", true, withdrawn, rhAskBatch, later,
		rhAskStop), []string{rhLaterTake, rhStopTake}, nil, nil)
	// Records 4 to 6 hang under a parent naming no record, and the reply after
	// them names the batch's result, record 3, as its parent: a thread no size
	// path reads alike either, so nothing in it is held.
	off := rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), []rhRecord{rhReply(rcSaid("done"))},
		rhPair(rhStop, rhStopFire, rhStopTake))
	off[3] = rcAttach(rcNowhere, 4, 400, rhSuccess(rhBatch, rhBatchFire, rhBatchTake))
	off[6] = rcReply(3, 7, "msg_7", 700, rcSaid("done"))
	rhWant(t, "a pair off the chain", rhRead(t, "2.1.322", true, off, rhAskBatch, rhAskStop), nil, nil, nil)
}

// A PAIR IS HELD ONLY WHERE EVERY SIZE PATH READS IT ALIKE: a thread a
// large-file reader of the resume may read otherwise holds no pair, since the
// copy resumed may differ in size from the one read; a record keyed otherwise
// or a later root makes every thread such a one. A compaction's boundary binds
// only what lies before it (#1532): a pair after the last boundary is held, a
// pair before it restarted where a reply of the model followed it there, else
// undecided, its note only in the summary.
//
// Mutation: hold a pair without asking Alike, restart one before the boundary
// that no reply followed, or bind a thread to the small path at a boundary
// again, and a case goes red.
func TestAPairIsHeldOnlyWhereEverySizePathReadsItAlike(t *testing.T) {
	t.Parallel()
	keyed := rhBase()
	keyed[0] = ctReplace(t, keyed[0], `{"parentUuid":null,"isSidechain":false,`, `{"isSidechain":false,"parentUuid":null,`)
	compaction := []rhRecord{func(_, n, ms int) string { return rcBoundary(n, ms, `{"trigger":"manual"}`) },
		func(parent, n, ms int) string { return rcSummary(parent, n, ms) }, rhReply(rcSaid("after"))}
	root := []rhRecord{func(_, n, ms int) string { return rcPrompt(0, n, ms) }, rhReply(rcSaid("after"))}
	compacted := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_kept", 100, rcSaid("kept")),
		rcBoundary(3, 300, `{"trigger":"manual"}`), rcSummary(3, 4, 400)}
	for name, tc := range map[string]struct {
		lines           []string
		held, restarted []string
		undecidedBySize bool
	}{
		"a record keyed otherwise":      {lines: keyed, undecidedBySize: true},
		"a later root after both pairs": {lines: rhOn(rhBase(), root), undecidedBySize: true},
		"a compaction after both pairs, a reply after the batch's alone": {lines: rhOn(rhBase(), compaction),
			restarted: []string{rhBatchTake}},
		"both pairs after a compaction": {lines: rhOn(compacted, rhCall("x1"), rhPair(rhBatch, rhBatchFire,
			rhBatchTake), []rhRecord{rhReply(rcSaid("done"))}, rhPair(rhStop, rhStopFire, rhStopTake)),
			held: []string{rhBatchTake, rhStopTake}},
	} {
		got := rhRead(t, "2.1.330", true, tc.lines, rhAskBatch, rhAskStop)
		rhWant(t, name, got, tc.held, tc.restarted, nil)
		if bySize := strings.Contains(got.Why, "every size path"); bySize != tc.undecidedBySize {
			t.Errorf("%s: why %q; want the size paths named %t", name, got.Why, tc.undecidedBySize)
		}
	}
	for _, thread := range ctProbeThreads {
		got := rhRead(t, strings.Split(thread, "/")[0], true, rhProbeWhole(t, thread),
			sandbox.AskedTake{Take: ctProbeBatchTake, Boundary: types.SteerBoundaryToolBatch}, sandbox.AskedTake{Take: ctProbeStopTake,
				Boundary: types.SteerBoundaryFinish})
		rhWant(t, thread+" whole", got, nil, []string{ctProbeBatchTake, ctProbeStopTake}, nil)
	}
}

// rhC1 is what readMainThread answers of row c1's main thread
// (testdata/claude/2.1.330/holdings/c1, measured): note A at a batch, answered
// by the next reply, an auto-compaction, then note B at a batch, its reply
// after; padded past 5 MiB, as c1's dispatch and prune rows padded its brief,
// where padded.
func rhC1(t *testing.T, padded bool) sandbox.TakeReceipts {
	t.Helper()
	return rhRead(t, "2.1.330", true, rhC1Lines(t, padded), sandbox.AskedTake{Take: rhC1A,
		Boundary: types.SteerBoundaryToolBatch}, sandbox.AskedTake{Take: rhC1B, Boundary: types.SteerBoundaryToolBatch})
}

// rhC1Lines is c1's main thread, its brief padded past 5 MiB where padded.
func rhC1Lines(t *testing.T, padded bool) []string {
	t.Helper()
	lines := rhProbeWhole(t, "2.1.330/holdings/c1/projects/crewlet/5e55100a-0000-4000-8000-000000000162.jsonl")
	if padded {
		lines[2] = ctReplace(t, lines[2], `"content":"Do the task.`,
			`"content":"Do the task. PAD`+strings.Repeat("p", 6_000_000))
	}
	return lines
}

// The takes of c1's notes A and B, and the line of its boundary.
const (
	rhC1A, rhC1B = "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000b2"
	rhC1Boundary = 38
)

// A NOTE TAKEN AFTER A COMPACTION IS CONFIRMED ON THE SMALL PATH (#1532): c1's
// thread, resumed small by 2.1.310, 2.1.322 and 2.1.330, handed the summary,
// the kept segment and note B, never A (measured). B's pair, after the last
// boundary, is held; A's, before it and answered, restarted.
//
// Mutation: bind the thread to the small path at a boundary again, and both
// go undecided.
func TestANoteTakenAfterACompactionIsConfirmedOnTheSmallPath(t *testing.T) {
	t.Parallel()
	rhWant(t, "c1, small", rhC1(t, false), []string{rhC1B}, []string{rhC1A}, nil)
}

// A NOTE TAKEN AFTER A COMPACTION IS CONFIRMED PAST FIVE MEBIBYTES BY THE
// DISPATCH (#1532): c1's thread padded past 5 MiB, resumed by each release's
// default large-file reader, handed what the small path does past the last
// boundary (measured), so B is held, A restarted.
//
// Mutation: answer a thread past the bound only where it is path-free whole,
// and both go undecided.
func TestANoteTakenAfterACompactionIsConfirmedPastFiveMebibytesByTheDispatch(t *testing.T) {
	t.Parallel()
	rhWant(t, "c1, past 5 MiB", rhC1(t, true), []string{rhC1B}, []string{rhC1A}, nil)
}

// A NOTE TAKEN AFTER A COMPACTION IS CONFIRMED PAST FIVE MEBIBYTES BY THE
// PRUNE (#1532): c1's padded thread, resumed under
// CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP=1, handed what the dispatch did
// (measured on each release): the chain answers both readers alike, so B is
// held, A restarted, and the records before the boundary are no answer past
// the bound.
//
// Mutation: hand a record before the last boundary of a thread past the
// bound, and a line before it reads as handed.
func TestANoteTakenAfterACompactionIsConfirmedPastFiveMebibytesByThePrune(t *testing.T) {
	t.Parallel()
	rhWant(t, "c1, pruned", rhC1(t, true), []string{rhC1B}, []string{rhC1A}, nil)
	read := rcChain(t, "", rhC1Lines(t, true)...)
	for n, h := range read.handles {
		if n < rhC1Boundary && read.c.Handed(h) != handedUnknown {
			t.Errorf("line %d, before the boundary, answers %s; want %s", n, read.c.Handed(h), handedUnknown)
		}
	}
}

// NO TAKE IS HELD FOR A RELEASE THE RESUME WAS NOT READ IN: the chain
// models 2.1.310's and 2.1.322's resume alone, so a job another release ran
// confirms nothing, and so denies nothing, and a read of its takes alone
// opens no thread for them.
//
// Mutation: drop the release gate, or walk for takes it refuses, and a case
// goes red.
func TestNoTakeIsHeldForAReleaseTheResumeWasNotReadIn(t *testing.T) {
	t.Parallel()
	for _, release := range []string{"2.1.300", "2.1.323", ""} {
		fake, paths := cntBox(cntThread(rhBase()...), nil)
		box := &rhOpens{FakeSandbox: fake}
		got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
			Release: release, Takes: []sandbox.AskedTake{rhAskBatch, rhAskStop, rhAskUntaken}, Ended: true})
		rhWant(t, "release "+release, got.Receipts, nil, nil, nil)
		if box.opens != 0 || got.Receipts.Why == "" {
			t.Errorf("release %q: %d opens, why %q; want none, and why", release, box.opens, got.Receipts.Why)
		}
	}
}

// A RUN ON THE PINNED RELEASE CONFIRMS ITS HOOK TAKES: the session Claude
// Code 2.1.330, the release the runner pins, wrote for a run that handed one note at a batch and one at Stop, each
// answered, and which 2.1.330 resumed with both notes in its first request
// (testdata/claude/2.1.330/holdings/n1, measured), holds both takes once its
// job's harness ended, so its notes are confirmed read.
//
// Mutation: leave 2.1.330 out of the releases whose resume was read, and both
// takes go undecided.
func TestAPinnedReleaseRunConfirmsItsHookTakes(t *testing.T) {
	t.Parallel()
	const batch, stop = "00000000-0000-4000-8000-0000000000a1", "00000000-0000-4000-8000-0000000000b2"
	got := rhRead(t, "2.1.330", true,
		rhProbeWhole(t, "2.1.330/holdings/n1/projects/crewlet/5e55100a-0000-4000-8000-000000000160.jsonl"),
		sandbox.AskedTake{Take: batch, Boundary: types.SteerBoundaryToolBatch},
		sandbox.AskedTake{Take: stop, Boundary: types.SteerBoundaryFinish})
	rhWant(t, "2.1.330's n1", got, []string{batch, stop}, nil, nil)
}

// A READ ASKED MORE TAKES THAN A HOOK RECORD NAMES ANSWERS, AND COUNTS: past
// the 64 a record's masks hold, every take is undecided with why, and the
// count is read as the count alone reads it, never a panic that answers
// nothing on every retry.
//
// Mutation: hand the walk every take asked, and this goes red.
func TestAReadAskedMoreTakesThanAHookRecordNamesAnswers(t *testing.T) {
	t.Parallel()
	raw := cntThread(rhBase()...)
	var alone resumedReading
	walkThread(t.Context(), strings.NewReader(raw), threadPlainMax, testPass(t), &alone)
	takes := []sandbox.AskedTake{rhAskBatch, rhAskStop}
	for i := len(takes); i <= maxAskedTakes; i++ {
		takes = append(takes, sandbox.AskedTake{Take: rcUUID(500 + i), Boundary: types.SteerBoundaryToolBatch})
	}
	box, paths := cntBox(raw, nil)
	got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
		Release: "2.1.322", Count: true, Takes: takes, Ended: true})
	rhWant(t, fmt.Sprintf("%d takes", len(takes)), got.Receipts, nil, nil, nil)
	if got.Receipts.Why == "" || !reflect.DeepEqual(got.Count, alone.count(0)) {
		t.Errorf("%d takes: why %q, count %+v; want why, and the count %+v", len(takes), got.Receipts.Why,
			got.Count, alone.count(0))
	}
}

// ABSENCE IS PROVEN ONLY BY A THREAD THAT MEETS EVERY CONDITION: its harness
// ended it, read whole with no torn piece, unrestarted, another take at the
// same event held, no stray name, every hook record read, and a thread the
// chain places. A held take's own stdout is no stray name.
//
// Mutation: drop any condition, read a held take at the other event, read a
// held take's own stdout as stray, or tell the chain no refused line, and a
// case goes red. Dropping the chain's placing is equivalent: a thread it
// cannot place holds no take, so the held take's condition refuses first.
func TestAbsenceIsProvenOnlyByAThreadThatMeetsEveryCondition(t *testing.T) {
	t.Parallel()
	takes := []sandbox.AskedTake{rhAskBatch, rhAskStop, rhAskUntaken}
	held := []string{rhBatchTake, rhStopTake}
	rhWant(t, "every condition met", rhRead(t, "2.1.322", true, rhBase(), takes...), held, nil,
		[]string{rhUntaken})
	rhWant(t, "a harness that did not end its run", rhRead(t, "2.1.322", false, rhBase(), takes...), held, nil, nil)
	rhWant(t, "another take held at the other event alone", rhRead(t, "2.1.322", true, rhBase(), rhAskBatch,
		rhAskUntaken), []string{rhBatchTake}, nil, nil)
	for name, tc := range map[string]struct {
		lines func(t *testing.T) []string
		held  []string
	}{
		"a line that is no record, between the pairs": {lines: func(*testing.T) []string {
			lines := rhBase()
			return slices.Insert(lines, 7, "[1,2]")
		}, held: held},
		"a restart before both pairs": {lines: func(*testing.T) []string {
			return rhChained([]rhRecord{func(parent, n, ms int) string {
				return rcSystem(parent, n, ms, "microcompact_boundary", "")
			}}, rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), []rhRecord{rhReply(rcSaid("done"))},
				rhPair(rhStop, rhStopFire, rhStopTake))
		}, held: held},
		"the untaken take in another firing's stdout": {lines: func(*testing.T) []string {
			lines := rhBase()
			lines[3] = rcAttach(3, 4, 400, rhSuccess(rhBatch, rhBatchFire, rhBatchTake, rhUntaken))
			return lines
		}, held: held},
		"a system message whose firing has no context": {lines: func(*testing.T) []string {
			return append(rhBase(), rcAttach(10, 11, 1100, rhSystem(rhBatch, rhLaterFire, rhLaterTake)))
		}, held: held},
		"a hook record whose event is no string": {lines: func(t *testing.T) []string {
			return append(rhBase(), rcAttach(10, 11, 1100, ctReplace(t, rhSuccess(rhBatch, rhLaterFire),
				`"hookEvent":"PostToolBatch"`, `"hookEvent":7`)))
		}, held: held},
		"a thread the chain cannot place": {lines: func(*testing.T) []string {
			return append(rhBase(), `{"type":"progress","data":{},"sessionId":"`+ctSession+`"}`)
		}},
	} {
		got := rhRead(t, "2.1.322", true, tc.lines(t), append(slices.Clone(takes),
			sandbox.AskedTake{Take: rhLaterTake, Boundary: types.SteerBoundaryToolBatch})...)
		rhWant(t, name, got, tc.held, nil, nil)
	}
	box, paths := cntBox(cntThread(rhBase()...)+strings.TrimSuffix(rcPrompt(10, 11, 1100), "}"), nil)
	got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
		Release: "2.1.322", Takes: takes, Ended: true}).Receipts
	rhWant(t, "a torn last piece", got, held, nil, nil)
}

// AN ASK PAST THE MASKS DECIDES NO TAKE: a row a peer with wider bounds wrote
// may ask more takes than a hook record's masks hold, and the read answers
// none of them, saying why, rather than any it could still tell apart.
//
// Mutation: ask the first 64 of an over-wide ask, and the two pairs read as
// held; ignore the refusal, and the why names no refused ask.
func TestAnAskPastTheMasksDecidesNoTake(t *testing.T) {
	t.Parallel()
	takes := []sandbox.AskedTake{rhAskBatch, rhAskStop}
	for i := len(takes); i <= maxAskedTakes; i++ {
		takes = append(takes, sandbox.AskedTake{Take: rcUUID(300 + i), Boundary: types.SteerBoundaryFinish})
	}
	got := rhRead(t, "2.1.322", true, rhBase(), takes...)
	rhWant(t, fmt.Sprintf("%d asked", len(takes)), got, nil, nil, nil)
	if !strings.Contains(got.Why, errAskedPastMasks.Error()) {
		t.Errorf("%d asked: why %q; want it to name the refused ask", len(takes), got.Why)
	}
	var absent []string
	for _, take := range takes[2:maxAskedTakes] {
		absent = append(absent, take.Take)
	}
	rhWant(t, fmt.Sprintf("%d asked", maxAskedTakes), rhRead(t, "2.1.322", true, rhBase(), takes[:maxAskedTakes]...),
		[]string{rhBatchTake, rhStopTake}, nil, absent)
}

// A HOOK-CANCELLED LINE DENIES NOTHING: a firing past its hook's timeout names
// no take, so it neither proves a take at its event absent nor stands in the
// way of an absence the thread proves otherwise.
//
// Mutation: read a cancelled firing as its event's evidence, and the first
// case goes red; read it as a stray name, and the second does.
func TestAHookCancelledLineDeniesNothing(t *testing.T) {
	t.Parallel()
	later := sandbox.AskedTake{Take: rhLaterTake, Boundary: types.SteerBoundaryToolBatch}
	alone := rhChained(rhCall("x1"), []rhRecord{rhAttach(rhCancelled(rhBatch, rhLaterFire)),
		rhReply(rcSaid("done"))}, rhPair(rhStop, rhStopFire, rhStopTake))
	rhWant(t, "a cancelled firing alone at its event", rhRead(t, "2.1.322", true, alone, later, rhAskStop),
		[]string{rhStopTake}, nil, nil)
	beside := rhChained(rhCall("x1"), rhPair(rhBatch, rhBatchFire, rhBatchTake), rhCall("x2"),
		[]rhRecord{rhAttach(rhCancelled(rhBatch, rhLaterFire)), rhReply(rcSaid("done"))},
		rhPair(rhStop, rhStopFire, rhStopTake))
	rhWant(t, "a cancelled firing beside a held take", rhRead(t, "2.1.322", true, beside, rhAskBatch, later,
		rhAskStop), []string{rhBatchTake, rhStopTake}, nil, []string{rhLaterTake})
}

// rhOpens is a box that counts the files opened through it.
type rhOpens struct {
	*sandbox.FakeSandbox
	opens int
}

func (b *rhOpens) OpenFile(ctx context.Context, p string) (io.ReadCloser, error) {
	b.opens++
	return b.FakeSandbox.OpenFile(ctx, p)
}

// ONE WALK READS THE THREAD FOR THE COUNT AND THE RECEIPTS: one open, its
// bytes streamed once, the count as the count alone reads it, and the takes
// held; a count marked before the walk, or not asked, still reads the takes.
//
// Mutation: open the thread again for the receipts, or end the read at the
// count's mark, and a case goes red.
func TestOneWalkReadsTheThreadForTheCountAndTheReceipts(t *testing.T) {
	t.Parallel()
	raw := cntThread(rhProbe(t, ctProbeThreads[1])...)
	var alone resumedReading
	walkThread(t.Context(), strings.NewReader(raw), threadPlainMax, testPass(t), &alone)
	takes := []sandbox.AskedTake{{Take: ctProbeBatchTake, Boundary: types.SteerBoundaryToolBatch}, {Take: ctProbeStopTake, Boundary: types.SteerBoundaryFinish}}
	saved, _ := claudePrecompact(ctSession)
	for name, tc := range map[string]struct {
		count bool
		files map[string]string
		want  *sandbox.ResumedCount
	}{
		"the count beside the takes":   {count: true, want: alone.count(0)},
		"a count marked before a walk": {count: true, files: map[string]string{saved[0]: "{}"}, want: &sandbox.ResumedCount{Mark: sandbox.CountPrecompact}},
		"no count asked":               {},
	} {
		fake, paths := cntBox(raw, tc.files)
		box := &rhOpens{FakeSandbox: fake}
		got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
			Release: "2.1.322", Count: tc.count, Takes: takes, Ended: true})
		if box.opens != 1 || fake.Delivered() != int64(len(raw)) {
			t.Errorf("%s: %d opens streamed %d bytes of a %d-byte thread; want one, once", name, box.opens,
				fake.Delivered(), len(raw))
		}
		if !reflect.DeepEqual(got.Count, tc.want) {
			t.Errorf("%s: the count is %+v; want %+v", name, got.Count, tc.want)
		}
		rhWant(t, name, got.Receipts, []string{ctProbeBatchTake, ctProbeStopTake}, nil, nil)
	}
	if got := (ClaudeCode{}).readMainThread(t.Context(), sandbox.NewFakeSandbox("box-none"), Paths{},
		sandbox.MainThreadAsk{Session: ctSession, Release: "2.1.322"}); !reflect.DeepEqual(got,
		sandbox.MainThreadReading{}) {
		t.Errorf("a read asking nothing answers %+v; want no reading", got)
	}
}

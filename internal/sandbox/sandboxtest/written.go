package sandboxtest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// writtenCases hold the records a run's calls wrote ([sandbox.BridgeLog.Wrote])
// to the contract: what a busy seat's router reads to hand a wake about one of
// them to the running run (internal/engine, ADR-0167).
var writtenCases = []struct {
	name string
	fn   func(*testing.T, sandbox.PendingStore)
}{
	{"WhatARunWroteIsKeptOnceThroughEveryOpeningOfItsLog", testWhatARunWroteIsKeptOnceThroughEveryOpeningOfItsLog},
	{"ARunsWrittenSetTakesNoMorePastItsBoundAndDropsNothing", testARunsWrittenSetTakesNoMorePastItsBoundAndDropsNothing},
}

// writtenFleetCases are [writtenCases] over two nodes' stores on one backend.
var writtenFleetCases = []struct {
	name string
	fn   func(t *testing.T, s, other sandbox.PendingStore)
}{
	{"WhatARunWroteIsReadByTheNodeThatTakesItOver", testWhatARunWroteIsReadByTheNodeThatTakesItOver},
}

// wroteOf is the records turnID's run wrote, as its router reads them: the
// turn's log as the row's job stands to hold it ([sandbox.PendingStore.TurnLog]).
// And it fails t unless the row keeps one key for each of them
// ([sandbox.PendingRun.WroteKeys]), what a listing of the fleet's runs reads.
func wroteOf(t *testing.T, s sandbox.PendingStore, turnID string) []types.WrittenRecord {
	t.Helper()
	row := mustGet(t, s, turnID)
	log, err := s.TurnLog(t.Context(), row)
	if err != nil {
		t.Fatalf("read the turn log of %s: %v", turnID, err)
	}
	if len(row.WroteKeys) != len(log.Wrote) {
		t.Fatalf("the row of %s keeps %d keys of what its calls wrote, and its log %d records", turnID,
			len(row.WroteKeys), len(log.Wrote))
	}
	return log.Wrote
}

// WHAT A RUN WROTE IS KEPT, EACH RECORD ONCE, THROUGH EVERY OPENING OF ITS LOG.
// A call's record folds the records it wrote into the turn's set; a begin adds
// none (the set is a record's alone), and a record asked again or a record
// written twice adds nothing. A relaunch carrying the round on (a parked run's
// answer, a loss) keeps the set, a late call of the job it replaced adds to
// it, and so do a send-back's new round and a record of the round before it.
//
// Mutation: drop the set from a new round's opening, fold a late or an
// earlier round's records nowhere, or fold a begin's, and a read is off.
func testWhatARunWroteIsKeptOnceThroughEveryOpeningOfItsLog(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wrote"
	ctx := t.Context()
	thread := types.ConversationWritten("chatlet:room-1:msg-0")
	item := types.ItemWritten(types.WorkItem{Backend: "worklet", ID: "task-1"})
	change := types.ConversationWritten("gitlet:change:c-1")
	page := types.ConversationWritten("page:p-1")
	first := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	post := madeAt("post_message", 1, 1)
	if _, err := s.AppendBridgeCall(ctx, turn, sandbox.BridgeAppend{Step: sandbox.AppendBegin, Launch: first,
		Round: round, Call: post, Wrote: []types.WrittenRecord{page}}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if got := wroteOf(t, s, turn); len(got) != 0 {
		t.Fatalf("a call's begin wrote %v, want nothing before its record", got)
	}
	for range 2 {
		recordCall(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round, Call: post,
			Wrote: []types.WrittenRecord{thread, item, thread}})
	}
	want := []types.WrittenRecord{thread, item}
	if got := wroteOf(t, s, turn); !slices.Equal(got, want) {
		t.Fatalf("the run wrote %v, want %v, each once", got, want)
	}
	carried := carryRoundOn(t, s, turn)
	if got := wroteOf(t, s, turn); !slices.Equal(got, want) {
		t.Errorf("after a relaunch carried the round on, the run wrote %v, want %v", got, want)
	}
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: first, Round: round,
		Call: madeAt("code_comment_on_change", 2, 2), Wrote: []types.WrittenRecord{change}}) {
		t.Fatal("the replaced job's late call was not recorded")
	}
	want = append(want, change)
	if got := wroteOf(t, s, turn); !slices.Equal(got, want) {
		t.Errorf("after the replaced job's late call, the run wrote %v, want %v", got, want)
	}
	sendBack(t, s, turn)
	if got := wroteOf(t, s, turn); !slices.Equal(got, want) {
		t.Errorf("in the send-back's new round, the run wrote %v, want %v", got, want)
	}
	if !recordLate(t, s, turn, sandbox.BridgeAppend{Launch: carried, Round: round,
		Call: madeAt("save_page", 3, 3), Wrote: []types.WrittenRecord{page}}) {
		t.Fatal("the earlier round's record was not recorded")
	}
	want = append(want, page)
	if got := wroteOf(t, s, turn); !slices.Equal(got, want) {
		t.Errorf("after the earlier round's record, the run wrote %v, want %v", got, want)
	}
}

// A RUN'S WRITTEN SET TAKES NO MORE PAST ITS BOUND AND DROPS NOTHING IT HOLDS.
// Held to types.MaxWrittenRecords, it is marked once a record is refused for
// room ([sandbox.BridgeLog.WroteMore]), and every record it held is still held,
// a key for each on the row.
//
// Mutation: drop the oldest record for a newer one, leave the set unmarked, or
// let the row's keys grow past the set's, and this goes red.
func testARunsWrittenSetTakesNoMorePastItsBoundAndDropsNothing(t *testing.T, s sandbox.PendingStore) {
	const turn = "t-wrote-full"
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	all := make([]types.WrittenRecord, 0, types.MaxWrittenRecords+1)
	for i := range types.MaxWrittenRecords + 1 {
		all = append(all, types.ConversationWritten(fmt.Sprintf("chatlet:room-1:msg-%d", i)))
	}
	recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("post_message", 1, 1),
		Wrote: all[:types.MaxWrittenRecords]})
	log, err := s.TurnLog(t.Context(), mustGet(t, s, turn))
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Wrote) != types.MaxWrittenRecords || log.WroteMore {
		t.Fatalf("a set at its bound holds %d records (full %v), want %d and unmarked", len(log.Wrote),
			log.WroteMore, types.MaxWrittenRecords)
	}
	recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("post_message", 2, 2),
		Wrote: all[types.MaxWrittenRecords:]})
	if log, err = s.TurnLog(t.Context(), mustGet(t, s, turn)); err != nil {
		t.Fatal(err)
	}
	if !log.WroteMore || !slices.Equal(log.Wrote, all[:types.MaxWrittenRecords]) {
		t.Errorf("a set past its bound holds %d records, the first %v (full %v): want the first %d kept and "+
			"the set marked", len(log.Wrote), log.Wrote[0], log.WroteMore, types.MaxWrittenRecords)
	}
	if keys := mustGet(t, s, turn).WroteKeys; len(keys) != types.MaxWrittenRecords {
		t.Errorf("the row keeps %d keys of a set past its bound, want %d", len(keys), types.MaxWrittenRecords)
	}
}

// WHAT A RUN WROTE IS READ BY THE NODE THAT TAKES IT OVER, and what that node's
// calls write joins it: the set lives on the log in the fleet's store, never
// in the memory of the node whose bridge took the calls.
//
// Mutation: keep the set anywhere but the log, and the other node reads none.
func testWhatARunWroteIsReadByTheNodeThatTakesItOver(t *testing.T, s, other sandbox.PendingStore) {
	const turn = "t-wrote-fleet"
	thread := types.ConversationWritten("chatlet:room-1:msg-0")
	item := types.ItemWritten(types.WorkItem{Backend: "worklet", ID: "task-1"})
	job := mustLaunched(t, s, run(turn))
	round := mustGet(t, s, turn).Round
	recordCall(t, s, turn, sandbox.BridgeAppend{Launch: job, Round: round, Call: madeAt("post_message", 1, 1),
		Wrote: []types.WrittenRecord{thread}})
	if got := wroteOf(t, other, turn); !slices.Equal(got, []types.WrittenRecord{thread}) {
		t.Fatalf("the node taking the run over reads it wrote %v, want the thread", got)
	}
	recordCall(t, other, turn, sandbox.BridgeAppend{Launch: job, Round: round,
		Call: madeAt("create_work_item", 2, 2), Wrote: []types.WrittenRecord{item}})
	if got := wroteOf(t, s, turn); !slices.Equal(got, []types.WrittenRecord{thread, item}) {
		t.Errorf("after the other node's call, the run wrote %v, want the thread and the item", got)
	}
}

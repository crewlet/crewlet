package turnctx_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
)

// Since is what was committed between two reads of the set: a kind the set
// held before is not the later calls', and a nil set committed nothing.
func TestSinceIsWhatTheCallsBetweenTwoReadsCommitted(t *testing.T) {
	t.Parallel()
	w := &turnctx.Written{}
	w.Commit(types.CommittedComment)
	before := w.Changes()
	w.Commit(types.CommittedComment)
	w.Commit(types.CommittedItem)
	if got := w.Since(before); !slices.Equal(got, []types.CommittedChange{types.CommittedItem}) {
		t.Errorf("since = %v, want only the item committed after the first read", got)
	}
	if got := w.Since(w.Changes()); len(got) != 0 {
		t.Errorf("since now = %v, want nothing", got)
	}
	var none *turnctx.Written
	if got := none.Since(nil); len(got) != 0 {
		t.Errorf("a nil set committed %v", got)
	}
}

// A TURN'S RECORDS ARE WHAT ITS WRITES CREATED OR WROTE, EACH ONCE, AND WHAT A
// CALL ADDED IS READ BETWEEN TWO READS. An item written is a record by its ref;
// a record written twice is one; an invalid one is none; the set holds
// types.MaxWrittenRecords; a nil set records nothing.
//
// Mutation: drop Add's record, or RecordsSince's difference, and this goes red.
func TestATurnsRecordsAreWhatItsWritesWroteEachOnce(t *testing.T) {
	t.Parallel()
	item := types.WorkItem{Backend: "worklet", ID: "task-1", Key: "ENG-1"}
	thread := types.ConversationWritten("chatlet:room:root")
	w := &turnctx.Written{}
	w.Add(item)
	before := w.Records()
	w.Wrote(thread, thread, types.WrittenRecord{Kind: "page", ID: "p"}, types.ConversationWritten(""))
	w.Add(item)
	if got, want := w.Records(), []types.WrittenRecord{types.ItemWritten(item), thread}; !slices.Equal(got, want) {
		t.Errorf("records = %v, want %v", got, want)
	}
	if got := w.RecordsSince(before); !slices.Equal(got, []types.WrittenRecord{thread}) {
		t.Errorf("records since = %v, want the thread written after the first read", got)
	}
	full := &turnctx.Written{}
	for i := range turnctx.MaxWritten + 1 {
		full.Add(types.WorkItem{Backend: "worklet", ID: "task-" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	if _, many := full.Items(); !many || len(full.Records()) != types.MaxWrittenRecords {
		t.Errorf("a turn past MaxWritten items holds %d records, want %d", len(full.Records()),
			types.MaxWrittenRecords)
	}
	var none *turnctx.Written
	none.Wrote(thread)
	if got := none.RecordsSince(nil); len(got) != 0 {
		t.Errorf("a nil set wrote %v", got)
	}
}

package types_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A WRITTEN SET HOLDS EACH VALID RECORD ONCE, IN FIRST-WRITE ORDER, AND AT ITS
// BOUND TAKES NO MORE AND DROPS NOTHING. An id past its bound, of bytes that are
// not UTF-8, or of a kind outside the set is no record; the set it was folded
// into is never written under its holder.
//
// Mutation: cut a long id instead of refusing it, drop the oldest at the bound,
// or append into have's array, and a case goes red.
func TestAWrittenSetHoldsEachRecordOnceAndDropsNothingAtItsBound(t *testing.T) {
	t.Parallel()
	item := types.ItemWritten(types.WorkItem{Backend: "jira", ID: "10042", Key: "ENG-4"})
	if item != (types.WrittenRecord{Kind: types.WrittenItem, ID: "jira:10042"}) {
		t.Errorf("an item is recorded as %+v, want its ref", item)
	}
	if (types.ItemWritten(types.WorkItem{Backend: "jira"}) != types.WrittenRecord{}) ||
		(types.ConversationWritten("") != types.WrittenRecord{}) {
		t.Error("an item with no id or an empty identity names a record")
	}
	thread := types.ConversationWritten("chatlet:room:root")
	for name, r := range map[string]types.WrittenRecord{
		"an id past its bound":   types.ConversationWritten(strings.Repeat("x", types.MaxWrittenIDBytes+1)),
		"an id not UTF-8":        types.ConversationWritten("chatlet:\xff"),
		"a kind outside the set": {Kind: "page", ID: "p-1"},
	} {
		if r.Valid() {
			t.Errorf("%s is a valid record", name)
		}
		if got, _ := types.FoldWritten(nil, r); len(got) != 0 {
			t.Errorf("%s was recorded: %v", name, got)
		}
	}
	have := make([]types.WrittenRecord, 1, 4)
	have[0] = item
	got, full := types.FoldWritten(have, thread, item, thread)
	if !slices.Equal(got, []types.WrittenRecord{item, thread}) || full {
		t.Errorf("folded %v (full %v), want the item and the thread once", got, full)
	}
	if spare := have[:2]; spare[1] == thread {
		t.Error("the fold wrote into the array its caller holds")
	}
	var set []types.WrittenRecord
	for i := range types.MaxWrittenRecords {
		set, _ = types.FoldWritten(set, types.ConversationWritten(fmt.Sprintf("chatlet:room:%d", i)))
	}
	set, full = types.FoldWritten(set, thread)
	if !full || len(set) != types.MaxWrittenRecords || set[0].ID != "chatlet:room:0" || slices.Contains(set, thread) {
		t.Errorf("a set at its bound took a record or dropped one (full %v, %d held)", full, len(set))
	}
	if again, refused := types.FoldWritten(set, set[5]); refused || len(again) != len(set) {
		t.Error("a record the full set holds is refused as one it has no room for")
	}
	if !types.WrittenItem.Valid() || !types.WrittenConversation.Valid() || types.WrittenKind("page").Valid() {
		t.Error("the kinds are not the closed set")
	}
}

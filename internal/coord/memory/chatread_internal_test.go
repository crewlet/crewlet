package memory

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// THE TWIN REFUSES A CHAT READ RECORD PAST ITS BOUND, AS THE STORE DOES. No
// flush can make one today, so the record is held here as a field the
// bound's arithmetic leaves out would make it: a flush onto it and an
// absence stamp are each refused as coord.ErrValueTooLarge, and the record
// stays as it was. A twin that certified it would pass a write the KV
// backend refuses.
//
// Mutation: drop either EncodeChatRead hold from AdvanceChatRead or
// SetChatReadAbsence, and its write lands.
func TestTheTwinRefusesAChatReadRecordPastItsBound(t *testing.T) {
	t.Parallel()
	f := NewFleet()
	past := coord.ChatReadState{Muted: []string{strings.Repeat("r", coord.MaxChatReadBytes)}, Revision: 7}
	f.chatReads["founder"] = past
	_, err := f.AdvanceChatRead(t.Context(), "founder", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("eng"): 1}, At: time.Now().UTC()})
	if !errors.Is(err, coord.ErrValueTooLarge) {
		t.Errorf("a flush onto a record past its bound = %v, want coord.ErrValueTooLarge", err)
	}
	exists, err := f.SetChatReadAbsence(t.Context(), "founder", time.Now().UTC())
	if !errors.Is(err, coord.ErrValueTooLarge) || exists {
		t.Errorf("an absence stamp on a record past its bound = %v, %v; want coord.ErrValueTooLarge", exists, err)
	}
	if got := f.chatReads["founder"]; got.Revision != past.Revision || len(got.Cursors) != 0 ||
		!got.AbsentSince.IsZero() {
		t.Errorf("the refused writes changed the record: revision %d, %d cursors, absent since %v",
			got.Revision, len(got.Cursors), got.AbsentSince)
	}
}

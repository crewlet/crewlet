package executordomain

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CAPTURE NAMES THE OBJECT ITS ROW NAMES. A seat's sessions are Required, so
// a backup reads the executor log past its store copy and carries every object
// a record names, because a restore replays those records into the rows: what
// the declaration's reader says one record writes has to be the row the
// applier wrote, spelled as the table's walk spells it, with the object the row
// names. Read here off the bytes the harness applies, through the real applier.
//
// AND EVERY OTHER RECORD IS ANSWERED: none for one that names no object (an
// end, the barrier, a capture the apply refuses on the record alone), and a
// refusal the backup recognises for a record this build cannot read whose
// scope reaches a seat's sessions.
//
// Mutation: answer a newer build's capture as none, spell the row turn-first,
// or drop the subject check, and this fails.
func TestACaptureNamesTheObjectItsRowNames(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	records := SessionObjectReferences.Records
	turn := newTurn()
	c := capturing("ana", "chat:room", turn, 1, 1, 100, budget)
	captured := e.encode(ConversationSubject(c.Handle, c.ConversationKey, c.Turn), OpCapture,
		seatScope(c.Handle), c)
	if _, n := e.applyEncoded(OpCapture, captured); n != 1 {
		t.Fatalf("the capture wrote %d rows; want its one", n)
	}

	got, err := records(captured)
	if err != nil || len(got) != 1 {
		t.Fatalf("the capture: Records = %+v, %v; want the one row it writes", got, err)
	}
	if want := objstore.JoinOwner("ana", turn); got[0].NamedBy != want || !got[0].Names {
		t.Errorf("the capture names row %q (names %v), want %q", got[0].NamedBy, got[0].Names, want)
	}
	row, found := e.session("ana", turn)
	if !found {
		t.Fatal("the capture left no row")
	}
	if got[0].Object != row.Object {
		t.Errorf("the capture names object %+v, and its applied row names %+v", got[0].Object, row.Object)
	}

	ended := e.encode(ConversationSubject(c.Handle, c.ConversationKey, c.Turn), OpEnd,
		ScopeSet{Subject: true}, ending("ana", "chat:room", turn, "", Ending{Outcome: OutcomeCompleted}))
	if got, err := records(ended); err != nil || got != nil {
		t.Errorf("an end: Records = %+v, %v; want none", got, err)
	}
	barrier, err := EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := records(barrier); err != nil || got != nil {
		t.Errorf("a barrier: Records = %+v, %v; want none", got, err)
	}
	elsewhere := capturing("ana", "chat:room", newTurn(), 1, 1, 100, budget)
	misplaced := e.encode(ConversationSubject("ana", "chat:other", elsewhere.Turn), OpCapture,
		seatScope("ana"), elsewhere)
	if got, err := records(misplaced); err != nil || got != nil {
		t.Errorf("a capture on another conversation's subject: Records = %+v, %v; want none, "+
			"since the apply writes no row for it", got, err)
	}

	rewritten := func(payload []byte, fn func(map[string]any)) []byte {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal(payload, &doc); err != nil {
			t.Fatal(err)
		}
		fn(doc)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	newer := func(doc map[string]any) { doc["v"] = RecordVersion + 1 }
	for name, payload := range map[string][]byte{
		"a capture a newer build wrote": rewritten(captured, newer),
		"an end a newer build wrote":    rewritten(ended, newer),
		"a kind this build has never heard of": rewritten(captured, func(doc map[string]any) {
			doc["subject"].(map[string]any)["k"] = "workspace"
		}),
	} {
		if _, err := records(payload); !errors.Is(err, objstore.ErrRecordUnreadable) {
			t.Errorf("%s: %v; want ErrRecordUnreadable, since its scope reaches a seat's sessions",
				name, err)
		}
	}
	if got, err := records(rewritten(barrier, newer)); err != nil || got != nil {
		t.Errorf("a barrier a newer build wrote: Records = %+v, %v; want none, since its scope "+
			"is the framework's own", got, err)
	}
	if _, err := records([]byte("not a record")); err == nil {
		t.Error("bytes no build reads were answered as naming nothing")
	}
}

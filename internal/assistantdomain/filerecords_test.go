package assistantdomain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A FILE RECORD NAMES THE OBJECT ITS ROW NAMES (ADR-0075). A conversation's
// files are Required (ADR-0074), so a backup reads the assistant's log past its
// store copy and carries every object a record names, because a restore
// replays those records into the rows: what the declaration's reader says one
// record writes has to be the row the applier wrote, spelled as the table's
// walk spells it, with the object the row names. Read here off the bytes the
// real writer published, applied by the real applier.
//
// AND EVERY OTHER RECORD IS ANSWERED: none for one that writes no file (a
// conversation's create, the barrier), and a refusal the backup recognises
// for a record this build cannot read whose scope reaches a conversation.
// Mutation: answer a newer build's file record as none, or spell the row
// file-first, and this fails.
func TestAFileRecordNamesTheObjectItsRowNames(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	f := newFiles(t, n)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "Files")
	kept := f.attach(ada, conversation, "chart.png", png("chart"))
	records := assistantdomain.FileObjectReferences.Records

	at := func(seq uint64) []byte {
		t.Helper()
		_, payload, _, ok, err := statelog.VerifiedLog{Log: n.log, Verifier: n.verifier}.At(t.Context(), seq)
		if err != nil || !ok {
			t.Fatalf("read the log at %d: ok %v, %v", seq, ok, err)
		}
		return payload
	}
	last, err := n.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stored := at(last)
	got, err := records(stored)
	if err != nil || len(got) != 1 {
		t.Fatalf("the file's record: Records = %+v, %v; want the one row it writes", got, err)
	}
	if want := objstore.JoinOwner(conversation.String(), kept.ID.String()); got[0].NamedBy != want ||
		!got[0].Names {
		t.Errorf("the file's record names row %q (names %v), want %q", got[0].NamedBy, got[0].Names, want)
	}
	if key := f.objectOf(t, kept); got[0].Object.Key.String() != key {
		t.Errorf("the file's record names object %s, and its applied row names %s", got[0].Object.Key, key)
	}

	created := at(1)
	if got, err := records(created); err != nil || got != nil {
		t.Errorf("a conversation's create: Records = %+v, %v; want none", got, err)
	}
	barrier, err := assistantdomain.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := records(barrier); err != nil || got != nil {
		t.Errorf("a barrier: Records = %+v, %v; want none", got, err)
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
	newer := func(doc map[string]any) { doc["v"] = assistantdomain.RecordVersion + 1 }
	for name, payload := range map[string][]byte{
		"a file record a newer build wrote":           rewritten(stored, newer),
		"a conversation's create a newer build wrote": rewritten(created, newer),
		"a kind this build has never heard of": rewritten(stored, func(doc map[string]any) {
			doc["subject"].(map[string]any)["k"] = "folder"
		}),
	} {
		if _, err := records(payload); !errors.Is(err, objstore.ErrRecordUnreadable) {
			t.Errorf("%s: %v; want ErrRecordUnreadable, since its scope reaches a conversation", name, err)
		}
	}
	if _, err := records([]byte("not a record")); err == nil {
		t.Error("bytes no build reads were answered as naming nothing")
	}
}

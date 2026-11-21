package inbox_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
)

// A MOVEMENT CARRIES IDENTIFIERS ONLY: the domain, whose inbox, how many
// notices arrived, and the subject and reason of the newest — and the broker's
// instant its push is timed from, which is no content either.
//
// It tells a screen to re-read the inbox through the SAME question and
// authority as the poll it replaces; an excerpt, title or author would be a
// read path around that authority, so the field set is held here. The source
// is the fifth because a node relays each domain's movements from its own
// sound copy; the instant is the sixth because the push is timed from it
// (internal/objectives' change visibility).
func TestAMovementCarriesIdentifiersOnly(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"Source": "source", "Handle": "handle", "UnreadDelta": "unread_delta",
		"Subject": "subject", "Reason": "reason", "At": "at,omitzero",
	}
	typ := reflect.TypeFor[inbox.Movement]()
	if typ.NumField() != len(want) {
		t.Fatalf("a movement has %d fields, want exactly %d — five "+
			"identifiers and the broker's instant", typ.NumField(), len(want))
	}
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag, ok := want[field.Name]
		if !ok {
			t.Errorf("a movement carries %s, which is not one of the five "+
				"identifiers or the instant it may name", field.Name)
			continue
		}
		if got := field.Tag.Get("json"); got != tag {
			t.Errorf("%s is %q on the wire, want %q", field.Name, got, tag)
		}
	}
}

// A BATCH ANNOUNCES EACH PERSON ONCE, NAMING ITS DOMAIN AND ITS NEWEST NOTICE,
// TIMED FROM ITS OLDEST.
//
// Notes are keyed on the notice row's own key, so a re-run transaction body
// does not announce twice. The newest is named by position, then record id,
// never map order. An ARRIVAL outranks a purge's removal (a deleted notice is
// nothing to point at, and counts nothing). The movement is timed from the
// OLDEST note, which waited longest. A drained batch is empty.
//
// Mutation: key notes per call, let a removal count, drop the arrival rule,
// time from the newest note, or keep notes after Drain, and a step fails.
func TestABatchAnnouncesEachPersonOnceWithItsNewestNotice(t *testing.T) {
	t.Parallel()
	at := func(sec int64) time.Time { return time.Unix(1_700_000_000+sec, 0).UTC() }
	var b inbox.Batch
	for _, n := range []inbox.Note{
		{Record: "r-1", Handle: "bo", Subject: "t-1", Reason: "assignee", Position: 1,
			At: at(10)},
		// THE SAME NOTICE AGAIN — a re-run of the body that wrote it.
		{Record: "r-1", Handle: "bo", Subject: "t-1", Reason: "assignee", Position: 1,
			At: at(10)},
		{Record: "r-3", Handle: "bo", Subject: "t-3", Reason: "mention", Position: 3,
			At: at(30)},
		{Record: "r-2", Handle: "bo", Subject: "t-2", Reason: "watcher", Position: 3,
			At: at(30)},
		// A REMOVAL at a later position than any arrival.
		{Record: "r-0", Handle: "bo", Subject: "t-0", Reason: "watcher", Position: 9,
			At: at(90), Removed: true},
		{Record: "r-0", Handle: "ana", Subject: "t-0", Reason: "assignee", Position: 9,
			At: at(90), Removed: true},
	} {
		b.Note(n)
	}
	got := b.Drain("tracker")
	want := []inbox.Movement{
		{Source: "tracker", Handle: "ana", UnreadDelta: 0, Subject: "t-0", Reason: "assignee",
			At: at(90)},
		{Source: "tracker", Handle: "bo", UnreadDelta: 3, Subject: "t-3", Reason: "mention",
			At: at(10)},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the batch announced %+v, want %+v", got, want)
	}
	if again := b.Drain("tracker"); again != nil {
		t.Errorf("a drained batch announced %+v again", again)
	}
}

// A notice whose words the batch took away moves its recipient's inbox and
// counts nothing, never displacing that notice's arrival in the same batch.
func TestAChangedNoticeMovesAnInboxAndCountsNothing(t *testing.T) {
	t.Parallel()
	at := time.Unix(1_700_000_000, 0).UTC()
	var b inbox.Batch
	for _, n := range []inbox.Note{
		{Record: "r-1", Handle: "bo", Subject: "a-1", Reason: "owner", Position: 1, At: at},
		// THE SAME NOTICE'S WORDS TAKEN AWAY later in the batch.
		{Record: "r-1", Handle: "bo", Subject: "a-1", Reason: "owner", Position: 1, At: at, Changed: true},
		// AN OLDER NOTICE CHANGED, at a later position than the arrival.
		{Record: "r-0", Handle: "bo", Subject: "a-0", Reason: "mention", Position: 5, At: at, Changed: true},
		// A NOTICE ONLY CHANGED moves its recipient's inbox.
		{Record: "r-0", Handle: "ana", Subject: "a-0", Reason: "participant", Position: 5, At: at,
			Changed: true},
	} {
		b.Note(n)
	}
	got := b.Drain("artifacts")
	want := []inbox.Movement{
		{Source: "artifacts", Handle: "ana", UnreadDelta: 0, Subject: "a-0", Reason: "participant", At: at},
		{Source: "artifacts", Handle: "bo", UnreadDelta: 1, Subject: "a-1", Reason: "owner", At: at},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the batch announced %+v, want %+v", got, want)
	}
}

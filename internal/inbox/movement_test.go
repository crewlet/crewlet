package inbox_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/inbox"
)

// A MOVEMENT CARRIES IDENTIFIERS ONLY: the domain, whose inbox, how many
// notices arrived, and the subject and reason of the newest.
//
// It rides a socket frame to whoever watches that seat, and what it is for is
// telling a screen to re-read the inbox through the SAME question, and the
// same authority, as the poll it replaces. A field carrying an excerpt, a title
// or an author would be a read path around that authority — so the set of
// fields is held here, and adding one is a decision somebody has to make in
// this file. The source is the fifth because a node relays each domain's
// movements from its own sound copy, and a screen composes its inbox from
// every domain's.
func TestAMovementCarriesIdentifiersOnly(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"Source": "source", "Handle": "handle", "UnreadDelta": "unread_delta",
		"Subject": "subject", "Reason": "reason",
	}
	typ := reflect.TypeFor[inbox.Movement]()
	if typ.NumField() != len(want) {
		t.Fatalf("a movement has %d fields, want exactly %d identifiers",
			typ.NumField(), len(want))
	}
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag, ok := want[field.Name]
		if !ok {
			t.Errorf("a movement carries %s, which is not one of the five "+
				"identifiers it may name", field.Name)
			continue
		}
		if got := field.Tag.Get("json"); got != tag {
			t.Errorf("%s is %q on the wire, want %q", field.Name, got, tag)
		}
	}
}

// A BATCH ANNOUNCES EACH PERSON ONCE, NAMING ITS DOMAIN AND ITS NEWEST NOTICE.
//
// The store re-runs a transaction body that failed transiently, so a note
// kept per call would announce the re-run's notice twice: notes are keyed on
// the notice row's own key. An import that files a hundred notices for one
// person is one movement of a hundred, naming the newest by position and,
// among equals, by record id — never a map's iteration order, or one batch
// would highlight a different subject on every node. An ARRIVAL outranks a
// purge's removal whatever their positions, because a deleted notice is
// nothing to point at, and a removal counts nothing. And a drained batch is
// empty, or the next commit announces this one's again.
//
// Mutation: key notes per call, let a removal count, drop the arrival rule,
// or keep the notes after Drain, and a step fails.
func TestABatchAnnouncesEachPersonOnceWithItsNewestNotice(t *testing.T) {
	t.Parallel()
	var b inbox.Batch
	for _, n := range []inbox.Note{
		{Record: "r-1", Handle: "bo", Subject: "t-1", Reason: "assignee", Position: 1},
		// THE SAME NOTICE AGAIN — a re-run of the body that wrote it.
		{Record: "r-1", Handle: "bo", Subject: "t-1", Reason: "assignee", Position: 1},
		{Record: "r-3", Handle: "bo", Subject: "t-3", Reason: "mention", Position: 3},
		{Record: "r-2", Handle: "bo", Subject: "t-2", Reason: "watcher", Position: 3},
		// A REMOVAL at a later position than any arrival.
		{Record: "r-0", Handle: "bo", Subject: "t-0", Reason: "watcher", Position: 9,
			Removed: true},
		{Record: "r-0", Handle: "ana", Subject: "t-0", Reason: "assignee", Position: 9,
			Removed: true},
	} {
		b.Note(n)
	}
	got := b.Drain("tracker")
	want := []inbox.Movement{
		{Source: "tracker", Handle: "ana", UnreadDelta: 0, Subject: "t-0", Reason: "assignee"},
		{Source: "tracker", Handle: "bo", UnreadDelta: 3, Subject: "t-3", Reason: "mention"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the batch announced %+v, want %+v", got, want)
	}
	if again := b.Drain("tracker"); again != nil {
		t.Errorf("a drained batch announced %+v again", again)
	}
}

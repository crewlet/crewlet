package stream_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/worklet"
)

// visibility is an objective meter that keeps the change-visibility stamps it
// was handed.
type visibility struct {
	mu     sync.Mutex
	stamps []time.Time
	other  []objectives.Name
}

func (v *visibility) RecordSince(n objectives.Name, stamp time.Time, answered bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if n != objectives.ChangeVisibility || !answered {
		v.other = append(v.other, n)
		return
	}
	v.stamps = append(v.stamps, stamp)
}

func (v *visibility) seen() []time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.stamps)
}

// A PUSHED CHANGE IS TIMED FROM THE BROKER'S INSTANT FOR ITS RECORD, AND ONLY
// WHEN A SOCKET WAS HANDED IT.
//
// Change visibility is the record's storing to its push to the sockets
// watching it: an artifact's movement, a datastore's changed documents, a
// store purged. A change in a room nobody is in was shown to nobody, and a
// datastore batch a reader may see none of is not pushed to them, so neither
// is an event — counted, a quiet room's every change would read as visible.
//
// Mutation: time a push nobody was handed, or time it from the push's own
// clock, and the stamps are not these.
func TestAPushedChangeIsTimedFromItsRecordWhenSomebodyIsShownIt(t *testing.T) {
	t.Parallel()
	judge := &visibility{}
	svc := buildService(t, stream.Options{Rooms: stream.Rooms{Access: newAccess(),
		Stores: stores{}, Presence: nopPresence{}}, Objectives: judge})
	stored := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return stored.Add(time.Duration(i) * time.Second) }

	svc.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: sheet,
		What: artifactsdomain.MoveMeta, Head: 2, At: at(1)}})

	fred := stream.NewClient(roomReader("fred"))
	svc.Hub().Register(fred)
	if _, _, ok := svc.Hub().JoinArtifact(fred, sheet, artifacts.RoleViewer, "s-fred"); !ok {
		t.Fatal("the client did not join")
	}
	svc.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: sheet,
		What: artifactsdomain.MoveMeta, Head: 3, At: at(2)}})
	shown := dataBatch(sheet, delta("notes", "open", artifacts.ReadAll, "dana", `{"t":"open"}`))
	shown.At = at(3)
	hidden := dataBatch(sheet, delta("drafts", "dana-draft", artifacts.ReadPrivate, "dana",
		`{"d":"hers"}`))
	hidden.At = at(4)
	svc.ArtifactDataMoved([]artifactdata.Moved{shown, hidden,
		{Artifact: sheet, Purged: true, At: at(5)}})

	if got, want := judge.seen(), []time.Time{at(2), at(3), at(5)}; !slices.Equal(got, want) {
		t.Errorf("pushes were timed from %v, want %v: the movement and the documents fred "+
			"was shown and the purge, and neither the empty room's nor the private row's", got,
			want)
	}
	if len(judge.other) != 0 {
		t.Errorf("the pushes recorded %v beside their visibility", judge.other)
	}
	if got := len(drain(fred)); got != 3 {
		t.Errorf("fred was pushed %d frames, want the three he was timed for", got)
	}
}

// AN INBOX MOVEMENT IS TIMED FROM ITS RECORDS' INSTANT, AND ONLY WHEN A
// SOCKET WATCHING THE SEAT WAS HANDED IT.
//
// An `inbox_changed` frame is how a person at the dashboard learns they have
// work, and it is a committed change pushed to the sockets watching it like
// any room's — so it is a change visibility event, from the broker's instant
// for the oldest record that moved the inbox to the push. A seat nobody
// watches was shown to nobody, and the re-read a switch of source asks every
// watched inbox for is no committed change at all.
//
// Mutation: drop the timing from InboxChanged, and the watched movement is
// not timed; time the push from its own clock, or time one nobody watches,
// and the stamps are not these.
func TestAnInboxMovementIsTimedFromItsRecordsWhenSomebodyWatches(t *testing.T) {
	t.Parallel()
	judge := &visibility{}
	svc := buildService(t, stream.Options{Objectives: judge})
	stored := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	lead := stream.NewClient(reader)
	svc.Hub().Register(lead)
	svc.Hub().Watch(lead, "lead")

	svc.InboxChanged(stream.InboxChange{Handle: "coder", UnreadDelta: 1, Subject: "t-1",
		Reason: "assignee", Stored: stored.Add(time.Second)})
	svc.InboxChanged(stream.InboxChange{Handle: "lead", UnreadDelta: 2, Subject: "t-2",
		Reason: "assignee", Stored: stored.Add(2 * time.Second)})
	svc.InboxReread(worklet.Domain{}.Name())

	if got, want := judge.seen(), []time.Time{stored.Add(2 * time.Second)}; !slices.Equal(got, want) {
		t.Errorf("inbox pushes were timed from %v, want %v: the movement the lead's "+
			"socket was shown, and neither the unwatched seat's nor the re-read", got, want)
	}
	if got := len(drain(lead)); got != 2 {
		t.Errorf("the lead's socket was pushed %d frames, want its movement and the re-read", got)
	}
}

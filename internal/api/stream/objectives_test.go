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

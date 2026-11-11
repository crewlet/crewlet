package stream_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
)

// A SEED THAT LANDS WHILE A SOCKET IS OPEN IS SENT TO IT, as the snapshot the
// socket opened with, now carrying the history.
//
// A seed read again after the boot's budget lands behind snapshots that
// described a projection without it, and the feed has no push that inserts
// rows behind the live ones: without a fresh snapshot an open tab showed a feed
// starting at the boot until it reloaded. A seed that moves nothing sends
// nothing.
//
// Mutation: drop the replies from Service.Seed and the open socket receives no
// frame.
func TestASeedThatLandsWhileASocketIsOpenIsSentToIt(t *testing.T) {
	t.Parallel()
	s, c := newService(t, stream.Options{})
	drain(c)

	history := livestate.History{Events: []livestate.FeedRow{{
		ID: "ev-old", Type: "agent_turn_completed", Category: "agent",
		Timestamp: clock.Add(-time.Hour).Format(time.RFC3339Nano),
	}}}
	s.Seed(history)
	frames := drain(c)
	if len(frames) != 1 || frames[0].Kind() != stream.KindSnapshot {
		t.Fatalf("frames = %v, want one snapshot", kindsOfFrames(frames))
	}
	var snapshot struct {
		Events []livestate.FeedRow `json:"events"`
	}
	decodeData(t, frames[0], &snapshot)
	if len(snapshot.Events) != 1 || snapshot.Events[0].ID != "ev-old" {
		t.Errorf("the snapshot's feed = %+v, want the seeded row", snapshot.Events)
	}

	s.Seed(history)
	if frames := drain(c); len(frames) != 0 {
		t.Errorf("a seed that moved nothing sent %v", kindsOfFrames(frames))
	}
}

// A PAUSE SEED THAT LANDS WHILE A SOCKET IS OPEN PUSHES THE SEATS IT PAUSED.
//
// Mutation: land the pauses on the projection without the push, and the open
// socket draws the seat as working until its next event.
func TestAPauseSeedPushesTheSeatsItPaused(t *testing.T) {
	t.Parallel()
	s, c := newService(t, stream.Options{})
	drain(c)

	s.SeedPauses([]livestate.SeedPause{{AgentID: "a-coder", Paused: livestate.Paused{
		By: "erin.ng", At: clock.Format(time.RFC3339Nano),
	}}})
	rows := agentRows(t, drain(c))
	if len(rows) != 1 || rows[0]["agent_id"] != "a-coder" || rows[0]["paused"] == nil {
		t.Errorf("pushed = %v, want the paused seat", rows)
	}
}

// kindsOfFrames is the kind of each frame, in order.
func kindsOfFrames(frames []*stream.Frame) []stream.Kind {
	out := make([]stream.Kind, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Kind())
	}
	return out
}

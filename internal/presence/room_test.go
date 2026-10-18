package presence

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// A LATE FULL FRAME FILLS IN AND NEVER UNDOES.
//
// The join-time answer is sealed when the peer answers and applied once the
// ask has waited out its budget, after the peer's broadcasts have already said
// what changed since. Here the room hears node-a's answer (sealed first,
// naming one member) only after node-a's newer frames: a second member that
// joined, and a third that joined and left. The answer must not delete the
// member a newer frame added, must not bring back the one a newer frame said
// left, and must still fill in a member the room had never heard of. A
// rebroadcast newer than everything then replaces node-a's members as before.
//
// Mutation: apply every full frame as a replacement, and the second member
// vanishes until node-a's next rebroadcast (fifteen seconds; S6 measured it).
func TestALateFullFrameFillsInAndNeverUndoes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d).Add(artifacts.PresenceFrameValidity) }
	member := func(session string) artifacts.PresenceMember {
		return artifacts.PresenceMember{Session: session, Seat: session, Kind: iam.ActorHuman,
			Activity: artifacts.PresenceViewing, At: start}
	}
	id := artifacts.IDFor("a late answer")
	r := newRoom(id)
	frame := func(sealed time.Duration, full bool, upsert []artifacts.PresenceMember, leave ...string) Frame {
		return Frame{V: frameVersion, Kind: FrameState, Node: "node-a", Artifact: id,
			Upsert: upsert, Leave: leave, Full: full, Until: at(sealed)}
	}
	held := func() []string {
		var out []string
		for key := range r.remote {
			out = append(out, key.session)
		}
		slices.Sort(out)
		return out
	}

	// THE NEWER FRAMES ARRIVE FIRST: dana joins, sam joins and leaves.
	r.hear(frame(2*time.Second, false, []artifacts.PresenceMember{member("dana")}), start)
	r.hear(frame(3*time.Second, false, []artifacts.PresenceMember{member("sam")}), start)
	r.hear(frame(4*time.Second, false, nil, "sam"), start)

	// THE ANSWER, sealed at one second, names jane (whom nothing newer
	// mentioned) and sam (who left after it was sealed), and not dana.
	r.hear(frame(time.Second, true, []artifacts.PresenceMember{member("jane"), member("sam")}), start)
	if got, want := held(), []string{"dana", "jane"}; !slices.Equal(got, want) {
		t.Fatalf("after the late answer the room holds %v from node-a, want %v: a frame older "+
			"than what the room heard since fills in and never undoes", got, want)
	}

	// A REBROADCAST NEWER THAN EVERYTHING still replaces node-a's members.
	r.hear(frame(5*time.Second, true, []artifacts.PresenceMember{member("dana")}), start)
	if got, want := held(), []string{"dana"}; !slices.Equal(got, want) {
		t.Fatalf("after a newer rebroadcast the room holds %v from node-a, want %v", got, want)
	}

	// AND THE MEMORY OF IT ENDS WITH THE FRAMES IT GUARDS AGAINST: once the
	// newest frame's validity has passed, nothing sealed before it can be
	// opened, and the room keeps no record of the node.
	r.expire(at(6 * time.Second))
	if len(r.told) != 0 || len(r.departed) != 0 {
		t.Fatalf("the room still remembers %d node(s) and %d departure(s) past every frame's "+
			"validity", len(r.told), len(r.departed))
	}
}

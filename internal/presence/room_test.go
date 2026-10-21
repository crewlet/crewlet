package presence

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A LATE FULL FRAME FILLS IN AND NEVER UNDOES.
//
// The join-time answer is sealed when the peer answers and applied once the
// ask has waited out its budget, after the peer's broadcasts have already said
// what changed since. Here the room hears node-a's answer only after node-a's
// newer frames: members that joined, and one that joined and left. The answer
// must not delete a member a newer frame added, must not set a member back to
// what it was before a newer frame, must not bring back the one a newer frame
// said left, and must still fill in a member the room had never heard of and
// update one whose last frame was older. A rebroadcast newer than everything
// then replaces node-a's members as before.
//
// Mutation: apply every frame whatever instant it carries, and ida vanishes
// until node-a's next rebroadcast (fifteen seconds; S6 measured it).
func TestALateFullFrameFillsInAndNeverUndoes(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return start.Add(d).Add(FrameValidity) }
	member := func(session string) Member {
		return Member{Session: session, Seat: session, Kind: iam.ActorHuman,
			Activity: Viewing, At: start}
	}
	id := ArtifactRoom(artifacts.IDFor("a late answer"))
	r := newRoom(id)
	frame := func(sealed time.Duration, full bool, upsert []Member, leave ...string) Frame {
		return Frame{V: frameVersion, Kind: FrameState, Node: "node-a", Room: id,
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

	// AN OLDER FRAME FIRST: bob was viewing, half a second in.
	r.hear(frame(500*time.Millisecond, false, []Member{member("bob")}), start)

	// THE NEWER FRAMES: dana joins, ida joins, sam joins and leaves.
	r.hear(frame(2*time.Second, false, []Member{member("dana")}), start)
	r.hear(frame(2500*time.Millisecond, false, []Member{member("ida")}), start)
	r.hear(frame(3*time.Second, false, []Member{member("sam")}), start)
	r.hear(frame(4*time.Second, false, nil, "sam"), start)

	// THE ANSWER, sealed at one second, names bob (editing since, which is
	// newer than what the room holds of him), dana (idle, which is older
	// than the frame that said she joined viewing), jane (whom nothing
	// newer mentioned) and sam (who left after it was sealed), and not ida.
	editing, idle := member("bob"), member("dana")
	editing.Activity, idle.Activity = Editing, Idle
	r.hear(frame(time.Second, true, []Member{editing, idle, member("jane"),
		member("sam")}), start)
	if got, want := held(), []string{"bob", "dana", "ida", "jane"}; !slices.Equal(got, want) {
		t.Fatalf("after the late answer the room holds %v from node-a, want %v: a frame older "+
			"than what the room heard since fills in and never undoes", got, want)
	}
	if got := r.remote[remoteKey{node: "node-a", session: "bob"}].member.Activity; got != Editing {
		t.Fatalf("bob shows %s after a frame newer than the one that last said him, want editing", got)
	}
	if got := r.remote[remoteKey{node: "node-a", session: "dana"}].member.Activity; got != Viewing {
		t.Fatalf("dana shows %s after a frame older than the one that last said her, want viewing", got)
	}

	// A REBROADCAST NEWER THAN EVERYTHING still replaces node-a's members.
	r.hear(frame(5*time.Second, true, []Member{member("dana")}), start)
	if got, want := held(), []string{"dana"}; !slices.Equal(got, want) {
		t.Fatalf("after a newer rebroadcast the room holds %v from node-a, want %v", got, want)
	}

	// AND THE MEMORY OF A DEPARTURE ENDS WITH THE FRAMES IT GUARDS AGAINST:
	// once the leave's validity has passed, nothing sealed before it can be
	// opened.
	r.expire(at(6 * time.Second))
	if len(r.departed) != 0 {
		t.Fatalf("the room still remembers %d departure(s) past every frame's validity",
			len(r.departed))
	}
}

// A TURN IS AN AGENT'S: a frame whose person member names one, or whose turn
// is past its bound, shows nobody.
func TestOnlyAnAgentsMemberNamesATurn(t *testing.T) {
	t.Parallel()
	agent := Member{Session: "s-1", Seat: "agent-cto", Kind: iam.ActorAgent,
		Activity: Editing, Turn: "run-1"}
	if err := checkMember(RoomArtifact, agent); err != nil {
		t.Fatalf("an agent's member naming its turn was refused: %v", err)
	}
	person := agent
	person.Kind = iam.ActorHuman
	if checkMember(RoomArtifact, person) == nil {
		t.Fatal("a person's member naming a turn was accepted")
	}
	long := agent
	long.Turn = strings.Repeat("r", maxTurnBytes+1)
	if checkMember(RoomArtifact, long) == nil {
		t.Fatal("a turn past its bound was accepted")
	}
}

// A ROOM IS ITS KIND AND ITS ID, told on a subject naming both: a kind this
// build keeps none of, or a room naming nothing, is refused where it enters —
// a join, an update, a touch — rather than kept as a room nobody's frames could
// reach.
//
// Mutation: drop the room's check from Join or Touch, and the room of an
// unknown kind is joined or touched.
func TestARoomIsItsKindAndItsID(t *testing.T) {
	t.Parallel()
	good := ArtifactRoom(artifacts.IDFor("a room of its own"))
	if err := good.check(); err != nil {
		t.Fatalf("an artifact's room was refused: %v", err)
	}
	if got, want := good.subject(), "crewlet.rooms.artifact."+good.ID.String(); got != want {
		t.Errorf("an artifact's room is told on %q, want %q", got, want)
	}
	n := startNode(t, memory.NewBroker(), "node-a", newClock())
	for name, r := range map[string]Room{
		"a kind this build keeps none of": {Kind: "page", ID: good.ID},
		"a room naming nothing":           {Kind: RoomArtifact},
	} {
		if r.check() == nil {
			t.Errorf("%s was accepted as a room", name)
		}
		if _, _, err := joined(n.hub, r, dana); err == nil {
			t.Errorf("a socket joined %s", name)
		}
		n.hub.Touch(r, Agent{Seat: "agent-cto", Turn: "run-1", Activity: Editing})
	}
	n.hub.mu.Lock()
	held := len(n.hub.rooms)
	n.hub.mu.Unlock()
	if held != 0 {
		t.Errorf("the hub holds %d room(s) after joins and touches it refused", held)
	}
}

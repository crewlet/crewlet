package stream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// joinProbe is a running presence hub that opens the window a join's roster
// used to fall into: once the hub's join returns, and before the surface that
// asked it goes on, it lets another member move and waits until that move's
// fan-out has been handed to the surface's own sink — exactly what a fan-out
// taken a moment after a join does on a busy node.
type joinProbe struct {
	*presence.Hub
	handed    chan presence.Change
	afterJoin func()
}

func newJoinProbe(hub *presence.Hub) *joinProbe {
	return &joinProbe{Hub: hub, handed: make(chan presence.Change, 64)}
}

// Join is the hub's join, then the window.
func (p *joinProbe) Join(room presence.Room, who presence.Local, hand presence.Hand) (string, error) {
	session, err := p.Hub.Join(room, who, hand)
	if err == nil && p.afterJoin != nil {
		p.afterJoin()
	}
	return session, err
}

// OnChange registers the surface's sink, and says each change once the sink
// has returned from it.
func (p *joinProbe) OnChange(fn func(presence.Change)) func() {
	return p.Hub.OnChange(func(c presence.Change) {
		fn(c)
		select {
		case p.handed <- c:
		default:
		}
	})
}

// waitHanded waits until the surface has been handed a change showing seat
// at activity.
func (p *joinProbe) waitHanded(t *testing.T, seat string, activity presence.Activity) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-p.handed:
			for _, m := range c.Upsert {
				if m.Seat == seat && m.Activity == activity {
					return
				}
			}
		case <-deadline:
			t.Fatalf("the surface was never handed %s %s", seat, activity)
		}
	}
}

// moveAfterJoin arms the window: after the next join, seat's member (session
// in room) starts editing, and the join goes on only once the fan-out saying
// so has been handed to the surface.
func (p *joinProbe) moveAfterJoin(t *testing.T, room presence.Room, session, seat string) {
	t.Helper()
	p.afterJoin = func() {
		p.afterJoin = nil
		if err := p.Update(room, session, presence.Editing, presence.Focus{}); err != nil {
			t.Error(err)
			return
		}
		deadline := time.After(5 * time.Second)
		for {
			select {
			case c := <-p.handed:
				for _, m := range c.Upsert {
					if m.Seat == seat && m.Activity == presence.Editing {
						return
					}
				}
			case <-deadline:
				t.Error("the move's fan-out never reached the surface")
				return
			}
		}
	}
}

// drainHanded empties what the probe has said so far.
func (p *joinProbe) drainHanded() {
	for {
		select {
		case <-p.handed:
		default:
			return
		}
	}
}

// AN ARTIFACT ROOM'S JOINER IS NEVER SHOWN AN OLDER ROSTER THAN THE ROOM.
//
// The join answered its roster after the presence hub's join returned, while
// the room's fan-outs went to the sockets in it whenever the hub took them: a
// fan-out taken after the roster could reach the socket ahead of the join's
// answer, and the tab — which replaces the room with a roster — put back what
// the fan-out had moved on; one taken before the socket was in the room reached
// nobody. Here dana is in the room, and the moment erin's join returns from the
// hub dana starts editing and that fan-out is handed over: erin's screen must
// end showing dana editing.
//
// Mutation: join the socket to the room and answer after the presence join
// returns (the hand only keeping the roster), and erin's screen ends showing
// dana viewing; answer after it with the socket joined in the hand, and the
// fan-out reaches erin first and the roster behind it puts dana back to
// viewing.
func TestAnArtifactJoinersRosterIsNeverOlderThanTheRoom(t *testing.T) {
	t.Parallel()
	id := artifacts.IDFor("stream-join-order-artifact")
	room := presence.ArtifactRoom(id)
	probe := newJoinProbe(presenceHub(t, memory.NewBroker(), "node-a"))
	opts := quietOptions()
	opts.Rooms = Rooms{Access: &switchedAccess{role: artifacts.RoleViewer}, Stores: quietStores{},
		Presence: probe}
	s, err := NewService(livestate.New(), opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(s.Stop)

	dana, err := probe.Hub.Join(room, presence.Local{Seat: "dana", Kind: iam.ActorHuman},
		func(string, presence.Roster) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	// DANA IS SHOWN FIRST, so the roster erin is handed holds her viewing.
	probe.waitHanded(t, "dana", presence.Viewing)
	probe.moveAfterJoin(t, room, dana, "dana")

	erin := iam.Principal{ID: uuid.New(), Kind: iam.KindPerson, Login: "erin.login",
		Seat: "erin", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantArtifacts}}
	c := NewClient(NewAudience(erin, nil))
	s.hub.Register(c)
	s.joinArtifact(iam.WithPrincipal(t.Context(), erin), c,
		request{Kind: "artifact_join", ID: 7, Artifact: id.String()})

	shown := map[string]presence.Member{}
	apply := func(f *Frame) {
		var outer struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(f.Raw(), &outer); err != nil {
			t.Fatalf("decode %s: %v", f.Raw(), err)
		}
		switch f.Kind() {
		case KindResult:
			var joined ArtifactJoined
			if err := json.Unmarshal(outer.Data, &joined); err != nil {
				t.Fatal(err)
			}
			clear(shown)
			for _, m := range joined.Roster.Members {
				shown[m.Session] = m
			}
		case KindArtifactPresence:
			var push ArtifactPresence
			if err := json.Unmarshal(outer.Data, &push); err != nil {
				t.Fatal(err)
			}
			for _, m := range push.Upsert {
				shown[m.Session] = m
			}
			for _, session := range push.Leave {
				delete(shown, session)
			}
		}
	}
	// EVERYTHING THE ROOM WILL SAY, said: the move's fan-out was handed over
	// inside the join, and erin's own arrival follows within a coalescing
	// interval or two.
	time.Sleep(100 * time.Millisecond)
	probe.drainHanded()
	answered := false
	for _, f := range drainClient(c) {
		answered = answered || f.Kind() == KindResult
		apply(f)
	}
	if !answered {
		t.Fatal("erin's join was never answered")
	}
	if got := shown[dana]; got.Activity != presence.Editing {
		t.Fatalf("erin's screen shows dana %q, want editing — a roster older than "+
			"a fan-out she was already handed put dana back", got.Activity)
	}
}

// A CHAT ROOM'S JOINER IS NEVER SHOWN AN OLDER ROSTER THAN THE ROOM: the chat
// hub's join goes through the same hand as an artifact's, so the same window
// holds nothing for it.
//
// Mutation: place the socket and send its roster after the presence join
// returns, and bob's screen ends showing alice viewing.
func TestAChatJoinersRosterIsNeverOlderThanTheRoom(t *testing.T) {
	t.Parallel()
	probe := newJoinProbe(presenceHub(t, memory.NewBroker(), "node-a"))
	h := NewChatHub(ChatHubOptions{
		Rooms:    func() ChatRooms { return twoRooms() },
		Presence: probe,
		Now:      func() time.Time { return chatClock },
	})
	h.Start(t.Context())
	t.Cleanup(h.Stop)
	room := presence.ChatRoom(uuid.MustParse(publicRoom))

	alice, err := probe.Hub.Join(room, presence.Local{Seat: "alice", Kind: iam.ActorHuman},
		func(string, presence.Roster) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	probe.waitHanded(t, "alice", presence.Viewing)
	probe.moveAfterJoin(t, room, alice, "alice")

	bob := watcherFor(t, h, chatPerson("bob"))
	h.presenceFrame(t.Context(), bob, publicRoom, presence.Viewing, presence.Focus{})
	roomShown(t, bob, publicRoom, func(r shownRoom) bool {
		return r.members["alice"] == presence.Editing && r.members["bob"] == presence.Viewing
	})
	settleChat()
	probe.drainHanded()
	chatKinds(bob)
	if shown, err := screenOf(bob).shown(publicRoom); err != nil ||
		shown.members["alice"] != presence.Editing {
		t.Fatalf("once the room settled bob's screen shows alice %q (%v), want editing",
			shown.members["alice"], err)
	}
}

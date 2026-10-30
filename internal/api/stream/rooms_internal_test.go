package stream

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A ROOM WHOSE DECISION COULD NOT BE REACHED IS ASKED AGAIN ON A BACKOFF: the
// floor first, doubling on each further miss, never past the ceiling, and no
// sooner than a refusal's own longer wait.
func TestARoomRedecisionBacksOff(t *testing.T) {
	t.Parallel()
	plain := errors.New("the estate router could not reach a data node")
	behind := &statelog.Refused{Code: statelog.RefuseBehind, RetryAfter: 4 * time.Second}
	for _, tc := range []struct {
		name     string
		attempts int
		err      error
		want     time.Duration
	}{
		{"first miss", 1, plain, roomRedecideFloor},
		{"second miss", 2, plain, 2 * roomRedecideFloor},
		{"fifth miss", 5, plain, 16 * roomRedecideFloor},
		{"past the ceiling", 9, plain, roomRedecideCeiling},
		{"many misses", 1000, plain, roomRedecideCeiling},
		{"a refusal's longer wait", 1, behind, 4 * time.Second},
		{"a refusal's wait past the ceiling", 1,
			&statelog.Refused{Code: statelog.RefuseBehind, RetryAfter: time.Hour}, roomRedecideCeiling},
	} {
		if got := roomRedecideDelay(tc.attempts, tc.err); got != tc.want {
			t.Errorf("%s: asked again after %s, want %s", tc.name, got, tc.want)
		}
	}
}

// switchedAccess decides every join at one role, or fails every decision with
// err while it is set.
type switchedAccess struct {
	mu    sync.Mutex
	role  artifacts.Role
	err   error
	asked int
}

func (a *switchedAccess) Standing(context.Context, iam.Principal, artifacts.ID) (artifacts.Role, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return a.role, a.err
}

func (a *switchedAccess) set(role artifacts.Role, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.role, a.err = role, err
}

func (a *switchedAccess) times() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

type quietStores struct{}

func (quietStores) Through(context.Context, artifacts.ID) (int64, int64, error) { return 1, 0, nil }

type quietPresence struct{}

func (quietPresence) Join(_ presence.Room, _ presence.Local, hand presence.Hand) (string, error) {
	if !hand("s", presence.Roster{}) {
		return "", presence.ErrDeclined
	}
	return "s", nil
}
func (quietPresence) Update(presence.Room, string, presence.Activity, presence.Focus) error {
	return nil
}
func (quietPresence) Leave(presence.Room, string)                  {}
func (quietPresence) OnChange(func(presence.Change)) (stop func()) { return func() {} }

// A CREDENTIAL RECHECK THAT CANNOT DECIDE A ROOM HOLDS IT: the membership was
// not suspended by any narrowing, so keeping it as it was would go on pushing
// documents at a role nobody vouched for. It is held, its datastore pushes are
// withheld, and the decider asks again until a decision lands.
func TestACredentialRecheckThatCannotDecideHoldsTheRoom(t *testing.T) {
	t.Parallel()
	id := artifacts.IDFor("stream-rooms-internal-recheck")
	acc := &switchedAccess{role: artifacts.RoleEditor}
	opts := quietOptions()
	opts.Rooms = Rooms{Access: acc, Stores: quietStores{}, Presence: quietPresence{}}
	s, err := NewService(livestate.New(), opts)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(s.Stop)
	p := iam.Principal{Kind: iam.KindPerson, Login: "erin@example.com", Seat: "erin",
		Stage: iam.StageActive}
	c := NewClient(NewAudience(p, nil))
	s.hub.Register(c)
	if _, _, ok := s.hub.JoinArtifact(c, id, artifacts.RoleEditor, "s-erin"); !ok {
		t.Fatal("the client did not join")
	}

	acc.set("", errors.New("the identity copy is past its stall grace"))
	s.recheckRooms(iam.WithPrincipal(t.Context(), p), c)
	s.hub.Broadcast(PushArtifact(KindArtifactData, id, ArtifactData{Artifact: id, Through: 2},
		s.now()))
	if got := drainClient(c); len(got) != 0 {
		t.Fatalf("a member whose credential recheck could not decide its room received %d frames", len(got))
	}

	// The decider asks again on its own, and the decision that lands
	// resumes the member and tells it to read what it missed.
	acc.set(artifacts.RoleViewer, nil)
	deadline := time.Now().Add(5 * time.Second)
	var kinds []Kind
	for !slices.Contains(kinds, KindArtifactData) {
		if time.Now().After(deadline) {
			t.Fatalf("the held room was never decided again (asked %d times)", acc.times())
		}
		time.Sleep(20 * time.Millisecond)
		for _, f := range drainClient(c) {
			kinds = append(kinds, f.Kind())
		}
	}
	if members := s.hub.Members(id); len(members) != 1 || members[0].Role != artifacts.RoleViewer {
		t.Fatalf("the member was resumed as %+v, want the viewer the decision found", members)
	}
}

// A READER WITH NO SEAT IS NAMED IN THE ROOM AS THE HISTORY NAMES THEM. The
// room carried the seat alone, which is nothing for an unbound person or an
// operator's token, so every such reader was drawn as an unnamed "somebody"
// beside a change record that named them by their login.
func TestAPresenceMemberIsNamedAsItsRecordIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    iam.Principal
		want string
	}{
		{"a bound person, by their seat",
			iam.Principal{Kind: iam.KindPerson, Login: "jane.doe", Seat: "jane"}, "jane"},
		{"an unbound person, by their login",
			iam.Principal{Kind: iam.KindPerson, Login: "jane.doe"}, "jane.doe"},
		{"an operator's token, by its login",
			iam.Principal{Kind: iam.KindMachine, Login: "token:review"}, "token:review"},
		{"an agent, by its seat",
			iam.Principal{Kind: iam.KindSeat, Login: "seat:pm", Seat: "pm"}, "pm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := presenceSeat(tc.p); got != tc.want {
				t.Fatalf("presenceSeat(%+v) = %q, want %q", tc.p, got, tc.want)
			}
			if got, want := presenceSeat(tc.p), iam.ActorFor(tc.p).Name; got != want {
				t.Fatalf("the room names %q, the history %q", got, want)
			}
		})
	}
}

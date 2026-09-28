package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// EVERY ANSWER TO A REMOVAL MEANS WHAT IT SAYS ON THE FIRST ASK, and two of them
// mean something else from the second on — because a lost answer is what the
// second ask exists for, so the second answer may be about the first ask.
//
//   - "in flight" on the first ask is somebody else's change and is refused; on
//     a later one it is most likely this call's own proposal still being
//     committed, and is waited out.
//   - "not a member" on the first ask is refused; on a later one, for a server
//     this member counted before asking, it is this call's own earlier commit
//     once this member no longer counts it — and a typo, a server nobody
//     counted, is never reported removed however many asks it took.
func TestEachAnswerToARemovalIsReadByWhichAskItCouldBeAbout(t *testing.T) {
	t.Parallel()
	answer := func(apiErr *server.ApiError, success bool) []byte {
		b, err := json.Marshal(server.JSApiMetaServerRemoveResponse{
			ApiResponse: server.ApiResponse{Error: apiErr}, Success: success})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var (
		committed = answer(nil, true)
		notMember = answer(server.NewJSClusterServerNotMemberError(), false)
		inFlight  = answer(server.NewJSClusterServerMemberChangeInflightError(), false)
		refused   = answer(server.NewJSClusterRequiredError(), false)
		silent    = answer(nil, false)
		viewFails = errors.New("no view")
	)
	for _, tc := range []struct {
		name    string
		data    []byte
		asks    int
		listed  bool
		counted func() (bool, error)
		settled bool
		want    error // nil is a committed removal
		wantAny bool  // an error that is none of the sentinels
	}{
		{name: "committed", data: committed, asks: 1, listed: true, settled: true},
		{name: "committed after an ask went unanswered", data: committed, asks: 3,
			listed: true, settled: true},
		{name: "not a member, first ask", data: notMember, asks: 1, listed: true,
			settled: true, want: ErrNotAMetaPeer},
		{name: "not a member, later ask, never counted", data: notMember, asks: 4,
			listed: false, settled: true, want: ErrNotAMetaPeer},
		{name: "not a member, later ask, no longer counted", data: notMember, asks: 2,
			listed: true, counted: func() (bool, error) { return false, nil }, settled: true},
		{name: "not a member, later ask, still counted", data: notMember, asks: 2,
			listed: true, counted: func() (bool, error) { return true, nil },
			settled: false, want: ErrNotAMetaPeer},
		{name: "not a member, later ask, view unreadable", data: notMember, asks: 2,
			listed: true, counted: func() (bool, error) { return false, viewFails },
			settled: true, want: viewFails},
		{name: "in flight, first ask", data: inFlight, asks: 1, listed: true,
			settled: true, want: ErrMembershipChanging},
		{name: "in flight, later ask", data: inFlight, asks: 2, listed: true,
			settled: false, want: ErrMembershipChanging},
		{name: "another refusal", data: refused, asks: 2, listed: true, settled: true,
			wantAny: true},
		{name: "an answer that claims nothing", data: silent, asks: 1, listed: true,
			settled: true, wantAny: true},
		{name: "an unreadable answer", data: []byte("{"), asks: 1, listed: true,
			settled: true, wantAny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counted := tc.counted
			if counted == nil {
				counted = func() (bool, error) {
					t.Error("read this member's view for an answer that does not turn on it")
					return true, nil
				}
			}
			settled, why := judgeRemoval("gone", tc.data, removalAsked{
				asks: tc.asks, listed: tc.listed, counted: counted})
			if settled != tc.settled {
				t.Errorf("settled %v, want %v (%v)", settled, tc.settled, why)
			}
			switch {
			case tc.wantAny:
				if why == nil || errors.Is(why, ErrNotAMetaPeer) ||
					errors.Is(why, ErrMembershipChanging) {
					t.Errorf("answered %v, want a refusal of its own", why)
				}
			case tc.want == nil:
				if why != nil {
					t.Errorf("answered %v, want a committed removal", why)
				}
			case !errors.Is(why, tc.want):
				t.Errorf("answered %v, want %v", why, tc.want)
			}
		})
	}
}

// A COMMITTED REMOVAL IS REPORTED ONLY ONCE THIS MEMBER'S OWN VIEW HAS DROPPED
// THE SERVER, because the group a removal reports is read from that view next.
// A member drops a removed peer when it stores the change and the leader
// answers once a quorum has — which need not include the member that carried
// it — so no cluster can stage the lag on demand, and the wait is held here
// against a view that lags by construction. A view that never catches up is
// reported as a member behind its own group, never as a removal it has not
// seen.
func TestARemovalWaitsForThisMembersOwnViewToDropTheServer(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	lagging := func() (MetaGroup, error) {
		g := MetaGroup{Cluster: "c", Peers: []MetaPeer{{Name: "a"}, {Name: "b"}}}
		if reads.Add(1) <= 3 {
			g.Peers = append(g.Peers, MetaPeer{Name: "gone"})
		}
		return g, nil
	}
	if err := awaitApplied(t.Context(), lagging, "gone"); err != nil {
		t.Fatalf("a view that caught up answered %v", err)
	}
	if n := reads.Load(); n != 4 {
		t.Fatalf("returned after %d reads of the view, want the first read that no "+
			"longer counts the server (4)", n)
	}

	behind := func() (MetaGroup, error) {
		return MetaGroup{Peers: []MetaPeer{{Name: "gone"}}}, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*appliedPoll)
	defer cancel()
	err := awaitApplied(ctx, behind, "gone")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "behind its own group") {
		t.Fatalf("a view that never caught up answered %v", err)
	}

	unreadable := errors.New("no view")
	if err := awaitApplied(t.Context(), func() (MetaGroup, error) {
		return MetaGroup{}, unreadable
	}, "gone"); !errors.Is(err, unreadable) {
		t.Fatalf("an unreadable view answered %v", err)
	}
}

// THE RESEND IS THE HEARTBEAT THAT ANNOUNCES A NEW LEADER, and the view is read
// well inside one: a removal made across an election is answered within a
// heartbeat of it, and a committed one costs at most a tenth of one more.
func TestTheRemovalCadenceFollowsTheRaftHeartbeat(t *testing.T) {
	t.Parallel()
	if removeResend != time.Second {
		t.Errorf("removeResend is %v: nats-server's raft heartbeat is one second", removeResend)
	}
	if appliedPoll <= 0 || appliedPoll > removeResend/10 {
		t.Errorf("appliedPoll is %v, want at most a tenth of the resend (%v)", appliedPoll, removeResend)
	}
}

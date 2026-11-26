package codesearch

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/signedask"
)

// servedUntil is a node's reads that answer at once, keeping each read's
// context's deadline: the instant the node serves the read until.
type servedUntil struct {
	mu        sync.Mutex
	deadlines []time.Time
}

func (r *servedUntil) Read(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	d, _ := ctx.Deadline()
	r.mu.Lock()
	r.deadlines = append(r.deadlines, d)
	r.mu.Unlock()
	return gitlet.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Search: &gitlet.SearchAnswer{}}, nil
}

// A NODE AHEAD OF ITS ASKER READS FOR ITS ASKER'S WHOLE WAIT: its reads are
// served until a reply's margin before the wait the request carries ends,
// counted from the request's arrival, whether its clock stands three seconds
// ahead of the asker's or ahead by more than the round less the margin. A
// request carrying no wait reads nothing and is answered with nothing: an
// older unreleased build's asker gets no compatibility.
//
// Mutations: serve every request until its deadline on the node's clock, and
// the reads of a node ahead are cut short by its lead, red; serve one
// carrying no wait to its deadline, red.
//
// In a synctest bubble, so the request's trip takes no time and the instant
// is exact arithmetic.
func TestANodeAheadOfItsAskerReadsForItsAskersWholeWait(t *testing.T) {
	t.Parallel()
	ring := recordseal.OneKey("k1", "the code search suite's fleet key material")
	for name, tc := range map[string]struct {
		ahead   time.Duration
		carries bool
		served  time.Duration // from arrival
	}{
		"three seconds ahead": {ahead: 3 * time.Second, carries: true,
			served: RoundBudget - gitlet.ReplyMargin},
		"ahead by more than the round less the margin": {ahead: RoundBudget - gitlet.ReplyMargin + 500*time.Millisecond,
			carries: true, served: RoundBudget - gitlet.ReplyMargin},
		"carrying no wait": {ahead: 2 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				wire, err := NewWire(ring)
				if err != nil {
					t.Fatal(err)
				}
				reads := &servedUntil{}
				srv, err := NewServer(wire, "git-a", reads, 4, queue.NewAnswerBudget(nil))
				if err != nil {
					t.Fatal(err)
				}
				srv.now = func() time.Time { return time.Now().Add(tc.ahead) }
				arrived := time.Now()
				req := Request{Version: Protocol, ID: signedask.NewID(), Deadline: arrived.Add(RoundBudget),
					Asker:  gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"},
					Search: gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral},
					Assign: map[string][]Assigned{"git-a": {{RepoID: "id-a", Name: "a", Home: true}}}}
				if tc.carries {
					req.BudgetMS = signedask.WaitMS(RoundBudget)
				}
				sealed, err := wire.SealRequest(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := srv.Answer(t.Context(), sealed)
				reads.mu.Lock()
				got := reads.deadlines
				reads.mu.Unlock()
				if !tc.carries {
					if raw != nil || err == nil || len(got) > 0 {
						t.Fatalf("a request carrying no wait was answered %d bytes (%v) and read %d repositories, "+
							"want nothing answered or read", len(raw), err, len(got))
					}
					return
				}
				if err != nil {
					t.Fatalf("a node %v ahead of its asker answered nothing: %v", tc.ahead, err)
				}
				if want := arrived.Add(tc.served); len(got) != 1 || !got[0].Equal(want) {
					t.Errorf("the node's read was served until %v, want %v: %v after the request arrived", got,
						arrived.Add(tc.served), tc.served)
				}
			})
		})
	}
}

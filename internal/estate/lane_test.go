package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A QUEUED PUSH HOLDS NO SLOT A READ IS ANSWERED THROUGH. A proposal waits —
// queued behind its repository's earlier pushes, then for its batch, for as
// long as its asker's attempt — so a data node answers proposals on a lane of
// their own, as wide as one repository's committer holds. With every slot of
// that lane held by a push behind a held batch, a tracker read and a git
// fetch's code.resolve asked of the SAME node are answered while every one of
// those pushes still holds its slot; on one registration, sixty-four queued
// pushes held every slot the node answered anything through, and a reader
// gave up and marked the node suspect for every domain.
//
// JUDGED BY WHAT WAS STILL HELD, never by a clock: the reader has the
// fleet's attempt, production's read attempt ([fleet.router]), so a node slow
// under load still answers it, and what proves the read never waited for a
// proposal's slot is that no push had let go of one when it was answered. Served on one registration, every slot stays held until the
// release below, so the read waits out its whole attempt and is refused.
func TestAQueuedPushHoldsNoSlotAReadIsAnsweredThrough(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	hold := make(chan struct{})
	node.set(func(n *fakeNode) { n.holdProposals = hold })
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	var pushes sync.WaitGroup
	// RELEASED, THEN WAITED FOR, on every way out: cleanups run last
	// registered first, so the wait is registered before the release.
	t.Cleanup(pushes.Wait)
	t.Cleanup(release)

	pusher := f.router(t, "agent-2", nil)
	// EVERY PUSH'S ATTEMPT OUTLASTS THE READS: a push whose asker gave up
	// would hand its slot back, and a read answered through it would prove
	// nothing.
	pushing, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	failed := make(chan error, proposalAnswers)
	var returned atomic.Int32
	for i := range proposalAnswers {
		pushes.Add(1)
		go func() {
			defer pushes.Done()
			repo := fmt.Sprintf("019a0c2e-0000-7000-8000-%012x", i)
			_, err := pusher.Code().Propose(pushing, proposalTo(repo, "push-"+repo))
			returned.Add(1)
			if err != nil {
				failed <- err
			}
		}()
	}

	// AT THE SHARED REGISTRATION'S BOUND, which on one registration is
	// already every slot the node has; and AGAIN with the proposal lane
	// full.
	for _, held := range []int{queue.ConcurrentAnswers, proposalAnswers} {
		deadline := time.Now().Add(2 * time.Minute)
		for int(node.proposing.Load()) < held {
			if time.Now().After(deadline) {
				t.Fatalf("%d of %d pushes reached the committer in two minutes — the node "+
					"answers fewer proposals at once than one repository's committer holds",
					node.proposing.Load(), held)
			}
			time.Sleep(time.Millisecond)
		}
		reads, cancelReads := context.WithTimeout(t.Context(), 2*Production().ReadAttempt)
		if _, err := f.client.Work().Tasks(reads, tracker.Query{}, time.Now()); err != nil {
			cancelReads()
			t.Fatalf("with %d pushes held on the node, a tracker read of it = %v, want it "+
				"answered: a queued push held a slot the node's reads are answered through",
				held, err)
		}
		if _, err := f.client.Code().Resolve(reads, code.ResolveQuery{Name: "app"}); err != nil {
			cancelReads()
			t.Fatalf("with %d pushes held on the node, a git fetch's code.resolve of it = "+
				"%v, want it answered", held, err)
		}
		cancelReads()
		if n := returned.Load(); n != 0 {
			t.Fatalf("%d pushes had returned when the reads were answered, with %d held — "+
				"a read answered through a slot a push let go of proves nothing", n, held)
		}
	}

	release()
	pushes.Wait()
	close(failed)
	for err := range failed {
		t.Errorf("a held push, once its batch was released: %v", err)
	}
}

// EACH LANE IS SERVED ON ITS OWN SUBJECT WITH ITS OWN BOUND, and proposals'
// bound is everything the committer and its two sibling writers hold at once:
// a burst must meet its component's own bounds — busy at once past a key's
// count, the node's count or the node's bytes — never the registration's,
// past which a request waits unseen in the broker's buffer for a slot. A node
// serving git registers the git reads' lane and the git writes' lane too: the
// reads' as wide as the shared one (the node's seat git-work share, not a
// lane, bounds the git work running), and the writes' as wide as the writes it
// holds and the shared lane's width beside them, for the answers it gives at
// once to a write past the held ones — so no such write waits unseen in the
// broker's buffer either. A node serving none registers neither.
func TestEachLaneIsServedOnItsOwnSubjectWithItsOwnBound(t *testing.T) {
	t.Parallel()
	shared := map[string]int{
		Subject("data.a"):         queue.ConcurrentAnswers,
		ProposalSubject("data.a"): proposalAnswers,
	}
	withGit := maps.Clone(shared)
	withGit[laneGitReads.subject("data.a")] = queue.ConcurrentAnswers
	withGit[laneGitWrites.subject("data.a")] = gitWritesHeld + queue.ConcurrentAnswers
	for _, tc := range []struct {
		name string
		git  LocalGit
		want map[string]int
	}{
		{"a node serving no git", nil, shared},
		{"a git node", gitHalf{node: &fakeGit{name: "data.a"}}, withGit},
	} {
		srv := newLaneServer(t, "", nil)
		stop, err := Serve(t.Context(), srv, ServerOptions{Self: "data.a",
			Local: &fakeNode{name: "data.a"}, Git: tc.git, Envelopes: fleetEnvelopes})
		if err != nil {
			t.Fatalf("%s: serve: %v", tc.name, err)
		}
		if got := srv.registered(); !maps.Equal(got, tc.want) {
			t.Fatalf("%s registered %v, want %v", tc.name, got, tc.want)
		}
		if err := stop(t.Context()); err != nil {
			t.Fatalf("%s: withdraw: %v", tc.name, err)
		}
		if got := srv.withdrawals(); len(got) != len(tc.want) {
			t.Errorf("%s: the withdrawal ended %v, want every lane", tc.name, got)
		}
	}
	// ONE TOKEN MORE, never a subject another node's id spells: an
	// escaped id is one token, so a dotted id stays inside its own.
	for _, l := range []lane{laneProposals, laneGitReads, laneGitWrites} {
		if s := l.subject("data.a"); s == Subject("data.a."+string(l)) ||
			!strings.HasPrefix(s, Subject("data.a")+".") {
			t.Errorf("the %s subject %q is not data.a's own", l, s)
		}
	}
	// EVERY HOLDER'S SHARE AT ONCE: the committer's node-wide count holds at
	// least one repository's full queue and the batch ahead of it, and the
	// lane holds it beside both writers' counts.
	if code.CommitterNodeProposals < code.CommitterInbox+code.MaxPushesPerRecord {
		t.Errorf("the committer holds %d proposals a node, under one repository's full queue and the batch "+
			"ahead of it (%d): a burst to one repository would be refused before its queue filled",
			code.CommitterNodeProposals, code.CommitterInbox+code.MaxPushesPerRecord)
	}
	if held := code.CommitterNodeProposals + code.ChangeWriterNodeWrites + code.ChecksWriterNodeWrites; proposalAnswers < held {
		t.Errorf("the proposal lane answers %d at once, under the %d its committer and its two writers hold "+
			"together: a burst would wait unseen for a slot before its component could answer it busy, "+
			"and one component's burst would hold the slots another's requests need", proposalAnswers, held)
	}
}

// EVERY HOLDER ON THE PROPOSAL LANE HAS ITS SHARE AT ONCE. The committer, the
// change writer and the checks writer each hold a request's slot for as long
// as they hold the request — a push queued behind a held batch, a write of a
// change's conversation or of a repository's checks waiting its key's turn
// behind a publish an election holds — and each refuses busy at once past its
// own node-wide count. With all three full at once, every one of their
// requests reaches its component: none waits unseen in the broker's buffer
// for a slot another component's burst holds, so a burst of comments on a
// node's changes never keeps a push routed to it from its committer.
//
// JUDGED BY WHAT REACHED EACH COMPONENT, never by a clock, as the read case
// above is: every request's attempt outlasts the wait.
//
// Mutation: size the lane to the committer's count alone, as it was before
// the writers' shares, and the writes past it never reach their writers.
func TestEveryHolderOnTheProposalLaneHasItsShareAtOnce(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	hold := make(chan struct{})
	node.set(func(n *fakeNode) {
		n.holdProposals, n.holdConversations, n.holdChecks = hold, hold, hold
	})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	var asks sync.WaitGroup
	t.Cleanup(asks.Wait)
	t.Cleanup(release)

	r := f.router(t, "agent-2", nil)
	asking, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	total := code.CommitterNodeProposals + code.ChangeWriterNodeWrites + code.ChecksWriterNodeWrites
	failed := make(chan error, total)
	ask := func(each func() error) {
		asks.Add(1)
		go func() {
			defer asks.Done()
			if err := each(); err != nil {
				failed <- err
			}
		}()
	}
	for i := range code.CommitterNodeProposals {
		repo := fmt.Sprintf("019a0c2e-0000-7000-8000-%012x", i)
		ask(func() error {
			_, err := r.Code().Propose(asking, proposalTo(repo, "push-"+repo))
			return err
		})
	}
	for i := range code.ChangeWriterNodeWrites {
		ask(func() error {
			_, err := r.Code().Converse(asking, code.ConversationWrite{
				OpID: fmt.Sprintf("019a0c2e-0000-7000-8000-%012d.code_comment_on_change", i),
				Comment: &code.Comment{RepoID: fmt.Sprintf("repo-%04d", i),
					ChangeID: fmt.Sprintf("019a0c2e-0000-7000-8000-%012d", 100_000+i), Body: "why?"}})
			return err
		})
	}
	for i := range code.ChecksWriterNodeWrites {
		ask(func() error {
			_, err := r.Code().Checks(asking, code.ChecksRequest{
				OpID: fmt.Sprintf("019a0c2e-0000-7000-8000-%012d.code_check_claim", i),
				Started: &code.CheckStarted{CheckKey: code.CheckKey{
					RepoID: fmt.Sprintf("019a0c2e-0000-7000-8000-%012d", 200_000+i),
					OID:    "1111111111111111111111111111111111111111", Name: "unit", Attempt: 1},
					Runner: "git-1", Placement: code.PlacementContainer,
					StartedAt: time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)}})
			return err
		})
	}

	deadline := time.Now().Add(2 * time.Minute)
	for _, c := range []struct {
		what    string
		reached *atomic.Int32
		share   int
	}{
		{"pushes reached the committer", &node.proposing, code.CommitterNodeProposals},
		{"conversation writes reached the change writer", &node.conversing, code.ChangeWriterNodeWrites},
		{"check writes reached the checks writer", &node.checking, code.ChecksWriterNodeWrites},
	} {
		for int(c.reached.Load()) < c.share {
			if time.Now().After(deadline) {
				t.Fatalf("%d of %d %s in two minutes, with every holder full at once — the lane answers "+
					"fewer requests than its holders hold together", c.reached.Load(), c.share, c.what)
			}
			time.Sleep(time.Millisecond)
		}
	}

	release()
	asks.Wait()
	close(failed)
	for err := range failed {
		t.Errorf("a held request, once released: %v", err)
	}
}

// A LANE ANSWERS ONLY ITS OWN OPERATIONS, whatever an asker sends: a proposal
// asked on the shared subject would hold a slot every read waits for, and a
// read asked on the proposals' would hold one a push does — so each is
// answered unserved, nothing run, and the asker moves on to a node whose
// build agrees with its own about where the operation is asked.
func TestALaneAnswersOnlyItsOwnOperations(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node := f.nodes["data-a"]
	for _, tc := range []struct {
		op      string
		subject string
		want    string
	}{
		{opCodePropose.spec.name, Subject("data-a"), "the proposals lane"},
		{opTasks.spec.name, ProposalSubject("data-a"), "the shared lane"},
	} {
		rep := askSignedOn(t, f.start(t), tc.subject,
			request{Op: tc.op, Args: json.RawMessage(`{}`), From: "agent-9"})
		if rep.Unserved != unservedNoBackend || !strings.Contains(rep.Detail, tc.want) {
			t.Errorf("%s asked on %s answered %+v, want it unserved naming %s", tc.op, tc.subject,
				rep, tc.want)
		}
	}
	if node.askedFor("code.propose") || node.askedFor("tasks") {
		t.Errorf("an operation asked on another lane ran: %v", node.asked)
	}
}

// EVERY LANE OR NONE: a node that answered its reads and no proposal would be
// asked first for every push to the repositories it commits for and cost each
// a whole write attempt — and a git node that answered no git read, or no git
// write, would be a silent home to every repository it is first for — so a
// lane that cannot be registered withdraws the ones registered before it.
func TestAServeThatCannotRegisterALaneWithdrawsTheOthers(t *testing.T) {
	t.Parallel()
	refused := errors.New("this broker registers no answerer there")
	srv := newLaneServer(t, ProposalSubject("data-a"), refused)
	if _, err := Serve(t.Context(), srv, ServerOptions{Self: "data-a",
		Local: &fakeNode{name: "data-a"}, Envelopes: fleetEnvelopes}); !errors.Is(err, refused) {
		t.Fatalf("serve with a lane the broker refuses = %v, want the refusal", err)
	}
	if got := srv.withdrawals(); len(got) != 1 || got[0] != Subject("data-a") {
		t.Errorf("after a lane was refused the node withdrew %v, want the shared lane it had "+
			"registered", got)
	}
	gitNode := newLaneServer(t, laneGitReads.subject("data-a"), refused)
	if _, err := Serve(t.Context(), gitNode, ServerOptions{Self: "data-a",
		Local: &fakeNode{name: "data-a"}, Git: gitHalf{node: &fakeGit{name: "data-a"}},
		Envelopes: fleetEnvelopes}); !errors.Is(err, refused) {
		t.Fatalf("serve with a git lane the broker refuses = %v, want the refusal", err)
	}
	if got := gitNode.withdrawals(); !slices.Equal(got, []string{ProposalSubject("data-a"), Subject("data-a")}) {
		t.Errorf("after the git lane was refused the node withdrew %v, want the proposals' and the "+
			"shared lanes it had registered", got)
	}
	writesRefused := newLaneServer(t, laneGitWrites.subject("data-a"), refused)
	if _, err := Serve(t.Context(), writesRefused, ServerOptions{Self: "data-a",
		Local: &fakeNode{name: "data-a"}, Git: gitHalf{node: &fakeGit{name: "data-a"}},
		Envelopes: fleetEnvelopes}); !errors.Is(err, refused) {
		t.Fatalf("serve with a git writes' lane the broker refuses = %v, want the refusal", err)
	}
	if got := writesRefused.withdrawals(); !slices.Equal(got, []string{laneGitReads.subject("data-a"),
		ProposalSubject("data-a"), Subject("data-a")}) {
		t.Errorf("after the git writes' lane was refused the node withdrew %v, want the git reads', the "+
			"proposals' and the shared lanes it had registered", got)
	}
}

// newLaneServer is a started [laneServer] refusing the subject refuse with
// refusal, and no subject where refuse is empty.
func newLaneServer(t *testing.T, refuse string, refusal error) *laneServer {
	t.Helper()
	s := &laneServer{q: memory.NewBroker().Client(), refuse: refuse, refusal: refusal}
	if err := s.q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.q.Stop(context.Background()) })
	return s
}

// laneServer is a broker that records every registration's subject and bound
// and every withdrawal, and refuses one subject where it is told to.
type laneServer struct {
	q       *memory.Queue
	refuse  string
	refusal error

	mu    sync.Mutex
	regs  map[string]int
	ended []string
}

func (s *laneServer) Serve(ctx context.Context, subject string, answers int,
	h queue.AnswerFunc) (queue.Unsubscribe, error) {

	if subject == s.refuse {
		return nil, s.refusal
	}
	stop, err := s.q.Serve(ctx, subject, answers, h)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.regs == nil {
		s.regs = map[string]int{}
	}
	s.regs[subject] = answers
	s.mu.Unlock()
	return func(ctx context.Context) error {
		s.mu.Lock()
		s.ended = append(s.ended, subject)
		s.mu.Unlock()
		return stop(ctx)
	}, nil
}

func (s *laneServer) registered() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.regs)
}

func (s *laneServer) withdrawals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ended...)
}

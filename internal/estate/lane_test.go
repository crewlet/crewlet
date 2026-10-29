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
// bound is the committer's queue: a burst to one repository must meet the
// committer's own bounds — busy at once past its count, its bytes or the
// node's — never the registration's, past which a push waits unseen in the
// broker's buffer for a slot. A node serving git registers the git reads' lane
// and the git writes' lane too, each as wide as the shared one (the node's
// seat git-work share, not a lane, bounds the git work running, and a write
// holds its slot mostly through waits that run no git), and a node serving
// none registers neither.
func TestEachLaneIsServedOnItsOwnSubjectWithItsOwnBound(t *testing.T) {
	t.Parallel()
	shared := map[string]int{
		Subject("data.a"):         queue.ConcurrentAnswers,
		ProposalSubject("data.a"): proposalAnswers,
	}
	withGit := maps.Clone(shared)
	withGit[laneGitReads.subject("data.a")] = queue.ConcurrentAnswers
	withGit[laneGitWrites.subject("data.a")] = queue.ConcurrentAnswers
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
	if proposalAnswers < code.CommitterInbox+code.MaxPushesPerRecord {
		t.Errorf("the proposal lane answers %d at once, under one repository's full queue and the "+
			"batch ahead of it (%d): a burst would wait for a slot before the committer could "+
			"answer it busy", proposalAnswers, code.CommitterInbox+code.MaxPushesPerRecord)
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

package query_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/events/types"
)

// heldListing runs every git process on the pool beneath but a tree's listing,
// which it holds until its context ends — a read whose git the stop finds
// running — telling entered when it begins. ran counts what it was asked.
type heldListing struct {
	query.Pool
	entered chan struct{}
	ran     atomic.Int64
}

func (p *heldListing) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	p.ran.Add(1)
	if len(c.Args) > 0 && c.Args[0] == "ls-tree" {
		p.entered <- struct{}{}
		<-ctx.Done()
		return gitproc.Result{Exit: -1}, ctx.Err()
	}
	return p.Pool.Run(ctx, c)
}

// A STOP CUTS THE READS IN FLIGHT AND REFUSES EVERY LATER ONE, each answered
// unready about the node, node_failed — so its walk asks the next Gitlet node and
// lets it fill a copy — and nothing recorded: a read cut, or refused before it
// started, sent the repository's contents to nobody. A later read starts
// nothing at all.
//
// Mutation: drop the cut, and the stop waits for a git the read holds until
// the hang detector goes red; answer a cut read with what its wait said, and it
// reads as the caller's own cancellation.
func TestAStopCutsTheReadsInFlightAndRefusesTheRest(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	x := newFixture(t, codetest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	rec := &fetches{}
	pool := &heldListing{Pool: x.f.Nodes()[0].Pool(), entered: make(chan struct{}, 1)}
	s := x.server(0, query.Options{Git: pool, Fetched: rec.record})

	answered := make(chan error, 1)
	go func() {
		_, err := s.Read(context.Background(), treeQ(repo, code.TreeQuery{Rev: head}))
		answered <- err
	}()
	<-pool.entered
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.Stop(context.Background())
	}()
	select {
	case err := <-answered:
		if !unready(err, code.UnreadyNodeFailed) {
			t.Errorf("the read the stop cut answered %v, want ErrGitUnready{node_failed}", err)
		}
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the stop never cut the read whose git it found running")
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the stop never returned once the read it cut had answered")
	}

	ran := pool.ran.Load()
	if _, err := s.Read(t.Context(), treeQ(repo, code.TreeQuery{Rev: head})); !unready(err, code.UnreadyNodeFailed) {
		t.Errorf("a read after the stop answered %v, want ErrGitUnready{node_failed}", err)
	}
	if more := pool.ran.Load() - ran; more != 0 {
		t.Errorf("a read after the stop ran %d git processes, want none", more)
	}
	if got := rec.all(); len(got) != 0 {
		t.Errorf("reads the stop cut or refused were recorded: %+v", got)
	}
}

// A STOP WAITS FOR A READ STILL BEING RECORDED: a read that answered before
// the stop is recorded before the stop returns, so whatever the node does with
// its records once Gitlet has stopped on it — the usage day's last flush —
// is short of no read this server answered.
//
// A read is recorded inside the span a stop waits for
// (TestAStopWaitsForEveryReadInFlight): its recording runs on the read's own
// context (Options.Fetched), which the stop cuts while the read is in flight
// — so the recording sees the stop's cut, where one made once its read had
// ended would see that end first.
//
// Mutation: record a read on a context that has ended, as one recorded once
// its read had ended — red.
func TestAStopWaitsForAReadStillBeingRecorded(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	x := newFixture(t, codetest.SoloOptions{}, settings)
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	repo := x.repo(codetest.RepoName)
	recording, release := make(chan context.Context, 1), make(chan struct{})
	var recorded atomic.Int64
	s := x.server(0, query.Options{Fetched: func(ctx context.Context, _ types.CodeFetched) {
		recording <- ctx
		<-release
		recorded.Add(1)
	}})

	answered := make(chan error, 1)
	go func() {
		_, err := s.Read(context.Background(), treeQ(repo, code.TreeQuery{Rev: head}))
		answered <- err
	}()
	in := <-recording
	if err := in.Err(); err != nil {
		t.Fatalf("the read was recorded on a context that had ended (%v): outside the span a stop waits for",
			context.Cause(in))
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.Stop(context.Background())
	}()
	select {
	case <-in.Done():
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the stop never cut the read being recorded")
	}
	if cause := context.Cause(in); !errors.Is(cause, query.ErrStopped) {
		t.Fatalf("the read being recorded was ended by %v, want the stop's cut: the read the stop waits for", cause)
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the stop never returned once the read was recorded")
	}
	if recorded.Load() != 1 {
		t.Errorf("the read was recorded %d times by the time the stop returned, want once", recorded.Load())
	}
	if err := <-answered; err != nil {
		t.Errorf("the read the stop waited for answered %v, want its answer", err)
	}
}

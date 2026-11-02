package assistant_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
)

// heldRig is a rig whose one round waits for hold to close.
func heldRig(t *testing.T, o evals.RigOptions, hold chan struct{}) *assistanttest.Rig {
	t.Helper()
	o.Rounds = []evals.Round{{Text: "Done.", Input: 100, Output: 20, Hold: hold}}
	return assistanttest.NewRig(t, o)
}

// asked counts the rounds a model was asked across rigs.
func asked(rigs ...*assistanttest.Rig) int {
	n := 0
	for _, r := range rigs {
		n += len(r.Model.Requests())
	}
	return n
}

// settledAsked is how many rounds the rigs' models were asked once at least
// one was and a moment has passed for any second runner to ask too.
func settledAsked(rigs ...*assistanttest.Rig) int {
	deadline := time.Now().Add(5 * time.Second)
	for asked(rigs...) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	return asked(rigs...)
}

// gate is a fleet turn counter that admits every start and holds the first
// one there until release closes: a start held between its checks and the
// record that opens its turn, the window a racing retry lands in.
type gate struct {
	mu      sync.Mutex
	turns   int
	entered chan struct{}
	release chan struct{}
}

func newGate() *gate { return &gate{entered: make(chan struct{}), release: make(chan struct{})} }

func (g *gate) Allow(ctx context.Context, bucket string, _ int, _ time.Duration, _ time.Time) (bool, error) {
	if !strings.HasPrefix(bucket, "assistant.turn:") {
		return true, nil
	}
	g.mu.Lock()
	g.turns++
	first := g.turns == 1
	g.mu.Unlock()
	if first {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	}
	return true, nil
}

func (g *gate) counted() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.turns
}

// THE SAME KEY IS ONE START ON A NODE: a retry landing while the first
// attempt is still between its checks and its turn's record waits for that
// attempt and answers its turn. It takes no second running slot, counts no
// second start on the person's rate, and runs no second loop.
//
// Mutation: drop the attempt map from Start and the retry is counted as a
// second start (and, past the record's own refusal, would run a second loop).
func TestARetryRacingItsFirstStartIsThatStart(t *testing.T) {
	t.Parallel()
	hold, g := make(chan struct{}), newGate()
	defer close(hold)
	f := heldRig(t, evals.RigOptions{Rate: g}, hold)
	type answer struct {
		turn uuid.UUID
		err  error
	}
	first, retry := make(chan answer, 1), make(chan answer, 1)
	go func() { a, err := f.Ask("retried", "One question", nil); first <- answer{a.TurnID, err} }()
	<-g.entered
	go func() { a, err := f.Ask("retried", "One question", nil); retry <- answer{a.TurnID, err} }()
	select {
	case got := <-retry:
		t.Fatalf("the retry answered %+v before its first attempt did", got)
	case <-time.After(100 * time.Millisecond):
	}
	close(g.release)
	want := assistant.TurnID(evals.Key("retried"))
	for name, ch := range map[string]chan answer{"first": first, "retry": retry} {
		if got := <-ch; got.err != nil || got.turn != want {
			t.Errorf("the %s start answered %+v, want turn %s", name, got, want)
		}
	}
	if n := g.counted(); n != 1 {
		t.Errorf("one question was counted %d times on the person's rate, want 1", n)
	}
	if n := settledAsked(f); n != 1 {
		t.Errorf("the model was asked %d rounds for one question, want 1", n)
	}
}

// THE SAME KEY ON TWO NODES IS ONE TURN TOO: the record decides, and the start
// that finds its turn already open runs nothing.
//
// Mutation: answer an already-open turn as opened (OpenTurn's ErrTurnOpen)
// and both nodes run a loop for one question.
func TestStartsUnderOneKeyOnTwoNodesRunOneLoop(t *testing.T) {
	t.Parallel()
	hold, g := make(chan struct{}), newGate()
	defer close(hold)
	a := heldRig(t, evals.RigOptions{Node: "node-a", Rate: g}, hold)
	b := heldRig(t, evals.RigOptions{Node: "node-b", Store: a.Store}, hold)
	held := make(chan error, 1)
	go func() { _, err := a.Ask("retried", "One question", nil); held <- err }()
	<-g.entered
	if _, err := b.Ask("retried", "One question", nil); err != nil {
		t.Fatal(err)
	}
	settledAsked(b)
	close(g.release)
	if err := <-held; err != nil {
		t.Fatalf("the start that found its turn open answered %v, want the turn", err)
	}
	if n := settledAsked(a, b); n != 1 {
		t.Errorf("one question asked the models %d rounds across the nodes, want 1", n)
	}
}

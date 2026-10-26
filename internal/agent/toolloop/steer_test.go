package toolloop_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A person's notes to a running loop. A note enters as a USER message before a
// run, never inside a tool's result; one that arrives while a run is going ends
// that run once its calls are answered.

// stepBox is a person's notes, recording every drain into a shared log so a
// case can say what happened FIRST.
type stepBox struct {
	mu      sync.Mutex
	pending []toolloop.SteerNote
	log     *stepLog
}

// stepLog is the order the loop asked for things in.
type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) add(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, step)
}

func (l *stepLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.steps)
}

func (b *stepBox) offer(id, msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, toolloop.SteerNote{ID: id, Message: msg})
}

func (b *stepBox) Drain(int) []toolloop.SteerNote {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log.add("drain")
	out := b.pending
	b.pending = nil
	return out
}

func (b *stepBox) Waiting() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending) > 0
}

func (b *stepBox) left() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// indexOf finds a user message in a conversation, or -1.
func indexOf(msgs []llm.Message, content string) int {
	for i, m := range msgs {
		if m.Role == llm.RoleUser && m.Content == content {
			return i
		}
	}
	return -1
}

// offerDuring offers one note the first time the named tool runs — the moment
// a person steers a turn is exactly while it is busy.
func offerDuring(box *stepBox, tool, note string) func(context.Context, llm.ToolCall) {
	var once sync.Once
	return func(_ context.Context, call llm.ToolCall) {
		if call.Name == tool {
			once.Do(func() { box.offer("n-1", note) })
		}
	}
}

// A NOTE IS READ AT THE NEXT RUN'S OPENING, AFTER THE FENCE. Offered while
// round 1's call runs, it ends that run once the call is answered and is read
// by the next run — round 2 — and not by the run already going. The fence is
// asked first, so a turn this node no longer holds is never handed a note it
// will not act on.
func TestASteerIsDeliveredAtTheNextRunAfterTheFence(t *testing.T) {
	t.Parallel()
	log := &stepLog{}
	box := &stepBox{log: log}
	surf := newSurface("search")
	surf.hook = offerDuring(box, "search", "use the staging channel")
	prov := script(
		runOf(calling("", "search"), calling("", "search"), says("done")),
		runOf(calling("", "search"), says("done")),
	)
	fence := func() error {
		log.add("fence")
		return nil
	}
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 5, Fence: fence, Steer: box})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want the one the note ended and the one that read it", len(seen))
	}
	if indexOf(seen[0].Messages, "use the staging channel") != -1 {
		t.Fatal("the first run read a note that was offered after it began")
	}
	if indexOf(seen[1].Messages, "use the staging channel") == -1 {
		t.Fatalf("the second run did not read the note: %+v", seen[1].Messages)
	}
	if len(res.Steers) != 1 || res.Steers[0] != (toolloop.SteerMark{Round: 2, ID: "n-1"}) {
		t.Errorf("steers = %+v, want the note marked on round 2", res.Steers)
	}
	steps := log.all()
	drains := 0
	for i, step := range steps {
		if step == "drain" {
			drains++
			if i == 0 || steps[i-1] != "fence" {
				t.Fatalf("a drain ran before its run's fence: %v", steps)
			}
		}
	}
	if drains != 2 {
		t.Errorf("drained %d times for two runs: %v", drains, steps)
	}
}

// A CLOSED FENCE READS NOTHING. The run that would have read the note never
// starts, so the note stays in the box — where the turn's end finds it and
// reports it expired rather than delivered.
func TestAClosedFenceLeavesTheNoteUnread(t *testing.T) {
	t.Parallel()
	log := &stepLog{}
	box := &stepBox{log: log}
	box.offer("n-1", "stop posting")
	stopped := errors.New("stopped")
	prov := script()
	_, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface(), MaxRounds: 3,
		Fence: func() error { return stopped }, Steer: box})
	if !errors.Is(err, stopped) {
		t.Fatalf("err = %v, want the fence's own", err)
	}
	if len(log.all()) != 0 || box.left() != 1 {
		t.Errorf("a fenced-off run drained its notes: log %v, pending %d", log.all(), box.left())
	}
	if n := len(prov.Seen()); n != 0 {
		t.Errorf("%d runs past a closed fence", n)
	}
}

// answered checks that every assistant turn with calls is followed, at once, by
// one answer per call and nothing else.
func answered(t *testing.T, msgs []llm.Message, note string) {
	t.Helper()
	at := indexOf(msgs, note)
	if at == -1 {
		t.Fatalf("the note is not in the conversation: %+v", msgs)
	}
	for i, m := range msgs {
		if m.Role != llm.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		ids := map[string]bool{}
		for _, c := range m.ToolCalls {
			ids[c.ID] = true
		}
		for k := i + 1; k <= i+len(m.ToolCalls); k++ {
			if k >= len(msgs) || msgs[k].Role != llm.RoleTool || !ids[msgs[k].ToolCallID] {
				t.Fatalf("the calls of the assistant turn at %d are not answered at %d: %+v", i, k, msgs)
			}
			delete(ids, msgs[k].ToolCallID)
		}
		if at > i && at <= i+len(m.ToolCalls) {
			t.Fatalf("the note is at %d, inside the calls and results of the turn at %d", at, i)
		}
	}
}

// A NOTE NEVER SPLITS A RESPONSE FROM ITS CALLS. A response that asked for two
// calls gets both run and both answered before anything else is said: a
// provider rejects a user message between a call and its result, a model shown
// one would read the note as the tool's output — and a note that arrived while
// the first call ran is not a reason to drop the second, which the model asked
// for in the same breath. Offered during the FIRST call, the moment a naive
// delivery would end the run there, it lands after both.
func TestASteerNeverSplitsAResponseFromItsCalls(t *testing.T) {
	t.Parallel()
	box := &stepBox{log: &stepLog{}}
	surf := newSurface("first", "second")
	surf.hook = offerDuring(box, "first", "the note")
	prov := script(
		runOf(calling("", "first", "second"), says("never reached in this run")),
		runOf(says("done")),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 4, Steer: box})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := surf.calls(); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("the surface ran %v, want both calls of the response", got)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the CLI asked its model %d time(s) after the note ended its run", late)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want the one the note ended and the one that read it", len(seen))
	}
	answered(t, seen[1].Messages, "the note")
	for _, m := range seen[1].Messages {
		if m.Role == llm.RoleTool && m.Failed {
			t.Errorf("the next run reads %q as a failed call — both calls ran", m.Content)
		}
	}
	answered(t, res.Messages, "the note")
}

// A NOTE WAITS FOR EVERY CALL IN FLIGHT. A CLI that runs a response's calls at
// once has them all out when a note arrives during one: the run ends once the
// last of them is answered — not at the first — and the next run reads every
// result, then the note.
func TestANoteDuringParallelCallsWaitsForEveryCallInFlight(t *testing.T) {
	t.Parallel()
	box := &stepBox{log: &stepLog{}}
	surf := newSurface("slow", "quick")
	surf.holdTool, surf.hold, surf.entered = "slow", make(chan struct{}), make(chan struct{})
	// The note arrives during quick, while slow is held; slow is let go
	// only once quick has returned and the note is waiting.
	quickDone := make(chan struct{})
	surf.hook = func(_ context.Context, call llm.ToolCall) {
		if call.Name == "quick" {
			<-surf.entered
			box.offer("n-1", "the note")
			close(quickDone)
		}
	}
	go func() {
		<-quickDone
		close(surf.hold)
	}()
	prov := script(
		runOf(llmtest.Response{Parallel: true, Calls: []llmtest.Call{{Name: "slow"}, {Name: "quick"}}},
			says("never reached in this run")),
		runOf(says("done")),
	)
	_, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 4, Steer: box})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if late := prov.Late(); late != 0 {
		t.Errorf("the CLI asked its model %d time(s) after the note ended its run", late)
	}
	seen := prov.Seen()
	if len(seen) != 2 {
		t.Fatalf("%d runs, want the one the note ended and the one that read it", len(seen))
	}
	answered(t, seen[1].Messages, "the note")
	results := map[string]string{}
	for _, m := range seen[1].Messages {
		if m.Role == llm.RoleTool {
			results[m.Name] = m.Content
		}
	}
	if results["slow"] != "slow ok" || results["quick"] != "quick ok" {
		t.Errorf("the next run read %v, want both calls' real results", results)
	}
	for _, r := range prov.Results() {
		if r.Output == "the note" {
			t.Errorf("the note was put inside a tool's result: %+v", r)
		}
	}
}

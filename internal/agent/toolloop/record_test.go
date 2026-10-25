package toolloop_test

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// What a loop records about each round and each call, and what it publishes
// while they run. Every figure is measured where the call is made, because that
// is the only frame with both ends of it.

// frames collects the live view, safely, for a case that reads it while the
// loop is still publishing.
type frames struct {
	mu  sync.Mutex
	all []toolloop.Result
}

func (f *frames) on(r toolloop.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.all = append(f.all, r)
}

func (f *frames) last() toolloop.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.all) == 0 {
		return toolloop.Result{}
	}
	return f.all[len(f.all)-1]
}

func (f *frames) list() []toolloop.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.all)
}

// pause is a response's Writing that takes d: the model's half of a round.
func pause(d time.Duration) func() { return func() { time.Sleep(d) } }

// EVERY ROUND RECORDS ITS OWN SPAN AND TOKENS: one round per model request,
// numbered one-based, started in UTC within the run, timed for the MODEL's half
// only — at least the model's own time and never its calls on top — and costed
// as its own request, cache included as a breakdown of input and never on top
// of it. A chain can move a phase between models, so each round names the one
// that served IT.
func TestEveryRoundRecordsItsOwnSpanAndTokens(t *testing.T) {
	t.Parallel()
	const delay = 15 * time.Millisecond
	surf := newSurface("read")
	surf.delay = 30 * time.Millisecond
	prov := script(runOf(
		llmtest.Response{Text: "reading", Model: "first-member", Writing: pause(delay),
			Calls: []llmtest.Call{{Name: "read"}, {Name: "read", Args: `{"n":2}`}},
			Usage: llm.Usage{Input: 1000, Output: 40, CacheRead: 800, CacheWrite: 150}},
		llmtest.Response{Text: "done", Model: "second-member", Writing: pause(delay),
			Usage: llm.Usage{Input: 1300, Output: 20, CacheRead: 1100}},
	))
	before := time.Now().UTC()
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 6})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Rounds) != 2 {
		t.Fatalf("recorded %d rounds, want one per model request (2): %+v", len(res.Rounds), res.Rounds)
	}
	first, second := res.Rounds[0], res.Rounds[1]
	for i, r := range res.Rounds {
		if r.Round != i+1 {
			t.Errorf("round %d is numbered %d — the lists join on a one-based round", i, r.Round)
		}
		if r.StartedAt.Location() != time.UTC {
			t.Errorf("round %d started at %v, want UTC", r.Round, r.StartedAt)
		}
		if r.StartedAt.Before(before) || r.StartedAt.After(after) {
			t.Errorf("round %d started at %v, outside the run [%v, %v]", r.Round, r.StartedAt, before, after)
		}
		if r.Duration < delay {
			t.Errorf("round %d took %v, want at least the model's %v", r.Round, r.Duration, delay)
		}
	}
	// THE MODEL'S HALF ONLY: round 2's model began once round 1's calls had
	// returned, so its start is after them and its duration excludes them.
	lastCall := res.Executions[len(res.Executions)-1]
	if callsEnded := lastCall.StartedAt.Add(lastCall.Duration); second.StartedAt.Before(callsEnded) {
		t.Errorf("round 2 began at %v, before round 1's calls ended at %v", second.StartedAt, callsEnded)
	}
	if second.Duration >= delay+surf.delay {
		t.Errorf("round 2 took %v — its duration carries the calls' time", second.Duration)
	}
	if first.Model != "first-member" || second.Model != "second-member" {
		t.Errorf("rounds served by %q, %q — want the model each response named", first.Model, second.Model)
	}
	if first.InputTokens != 1000 || first.OutputTokens != 40 || first.CacheRead != 800 || first.CacheWrite != 150 {
		t.Errorf("round 1 cost %+v, want its own request's counts", first)
	}
	if first.ToolCalls != 2 || second.ToolCalls != 0 {
		t.Errorf("tool calls per round = %d, %d, want 2, 0", first.ToolCalls, second.ToolCalls)
	}
	if res.CacheRead != 1900 || res.CacheWrite != 150 {
		t.Errorf("cache = %d read / %d write, want 1900 / 150", res.CacheRead, res.CacheWrite)
	}
	if res.InputTokens != 2300 {
		t.Errorf("input = %d, want 2300 — the cache must not be added to it", res.InputTokens)
	}
	if res.MaxRounds != 6 {
		t.Errorf("MaxRounds = %d, want the cap the loop ran under echoed (6)", res.MaxRounds)
	}
}

// A RUN'S FIRST ROUND STARTS WHEN THE RUN DID. A phase of several runs — a
// corrective, a note, a long call — has a gap between them, the engine's own,
// and a round timed from the previous run's last response would carry that gap
// as the model's time.
func TestARunsFirstRoundStartsWithTheRun(t *testing.T) {
	t.Parallel()
	const gap = 60 * time.Millisecond
	surf := newSurface("delegate", "submit_work")
	surf.delay = gap
	prov := script(
		runOf(calling("", "delegate")),
		runOf(llmtest.Response{Writing: pause(10 * time.Millisecond), Calls: []llmtest.Call{{Name: "submit_work"}}}),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, ParkLocally: []string{"delegate"},
		TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Rounds) != 2 || len(res.Invocations) != 2 {
		t.Fatalf("rounds %+v over runs %+v, want one round in each of two runs", res.Rounds, res.Invocations)
	}
	second, run2 := res.Rounds[1], res.Invocations[1]
	if second.StartedAt.Before(run2.StartedAt) {
		t.Errorf("run 2's first round started at %v, before run 2 itself at %v", second.StartedAt, run2.StartedAt)
	}
	if second.Duration > run2.Duration {
		t.Errorf("run 2's first round took %v, longer than the whole run's %v", second.Duration, run2.Duration)
	}
}

// A TOOL CALL IS TIMED AND NAMES ITS ORIGIN: started in UTC after its round's
// model half ended, at least the tool's own time, and who answered it — the
// surface's own report — carried onto its record.
func TestAToolCallIsTimedAndNamesItsOrigin(t *testing.T) {
	t.Parallel()
	const delay = 15 * time.Millisecond
	surf := newSurface("read", "write")
	surf.delay, surf.origin, surf.server = delay, "mcp:github", "github"
	prov := script(runOf(calling("reading", "read", "write"), says("done")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 4})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Executions) != 2 {
		t.Fatalf("executions = %+v, want two", res.Executions)
	}
	round := res.Rounds[0]
	modelEnded := round.StartedAt.Add(round.Duration)
	for _, ex := range res.Executions {
		if ex.Duration < delay {
			t.Errorf("%s took %v, want at least the surface's %v", ex.Name, ex.Duration, delay)
		}
		if ex.StartedAt.Location() != time.UTC {
			t.Errorf("%s started at %v, want UTC", ex.Name, ex.StartedAt)
		}
		if ex.StartedAt.Before(modelEnded) {
			t.Errorf("%s began at %v, before its round's model half ended at %v", ex.Name, ex.StartedAt, modelEnded)
		}
		if ex.Origin != "mcp:github" || ex.Server != "github" {
			t.Errorf("%s names origin %q server %q, want mcp:github / github", ex.Name, ex.Origin, ex.Server)
		}
		if ex.CallID == "" {
			t.Errorf("%s carries no call id", ex.Name)
		}
	}
	a, b := res.Executions[0], res.Executions[1]
	if b.StartedAt.Before(a.StartedAt.Add(a.Duration)) {
		t.Errorf("the second call began at %v, before the first ended — the CLI made them one after another",
			b.StartedAt)
	}
}

// A CALL IS TOLD ITS ROUND ON THE PHASE'S SCALE: an extension continuing a phase
// twenty rounds in has its calls nest under rounds 21 and 22, not 1 and 2.
func TestACallIsToldItsRoundOnThePhasesScale(t *testing.T) {
	t.Parallel()
	surf := newSurface("read")
	var mu sync.Mutex
	var rounds []int
	surf.hook = func(ctx context.Context, _ llm.ToolCall) {
		round, ok := toolloop.CallRound(ctx)
		if !ok {
			round = -1
		}
		mu.Lock()
		rounds = append(rounds, round)
		mu.Unlock()
	}
	prov := script(runOf(calling("one", "read"), calling("two", "read"), says("done")))
	cfg := toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 4, Host: host(t), Messages: opening()}
	if _, err := toolloop.Run(toolloop.WithRoundOffset(t.Context(), 20), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(rounds, []int{21, 22}) {
		t.Errorf("calls were told rounds %v, want [21 22] — the phase's numbers, not the invocation's", rounds)
	}
	if _, ok := toolloop.CallRound(t.Context()); ok {
		t.Error("a context no loop stamped answers a round")
	}
}

// THE PHASE NAMES THE ENTRY THAT SERVED IT, by the model's own precedence: the
// configured head stands in until a run's answer names an entry, the first that
// does wins, and a bare backend that names none leaves the head as the answer.
func TestThePhaseNamesTheEntryThatServedIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		keys []string
		want string
	}{
		{"the answers name entries", []string{"backup", "third"}, "backup"},
		{"a bare backend names none", []string{"", ""}, "head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := &keyed{Script: script(runOf(says(writtenSubmission)), runOf(calling("", "submit_work"))),
				keys: tc.keys}
			prog := &toolloop.Progress{}
			res, err := run(t, toolloop.Config{Provider: prov, ProviderKey: "head", Surface: newSurface("submit_work"),
				MaxRounds: 6, Progress: prog, TerminateAfter: []string{"submit_work"}})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(prov.Seen()) != 2 {
				t.Fatalf("%d runs, want two", len(prov.Seen()))
			}
			if res.ProviderKey != tc.want {
				t.Errorf("ProviderKey = %q, want %q", res.ProviderKey, tc.want)
			}
			if got := prog.Snapshot().ProviderKey; got != tc.want {
				t.Errorf("the snapshot names %q, want %q", got, tc.want)
			}
		})
	}
}

// keyed names a providers.llm entry on each run's answer, as a chain does.
type keyed struct {
	*llmtest.Script
	mu   sync.Mutex
	keys []string
}

func (k *keyed) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	comp, err := k.Script.Complete(ctx, req)
	k.mu.Lock()
	defer k.mu.Unlock()
	if comp != nil && len(k.keys) > 0 {
		comp.ProviderKey, k.keys = k.keys[0], k.keys[1:]
	}
	return comp, err
}

// EVERY ROUND CARRIES ITS STOP REASON, so a reader can tell a request that ended
// to run its calls from one that answered without inferring it from what the
// round holds.
func TestEveryRoundRecordsItsStopReason(t *testing.T) {
	t.Parallel()
	prov := script(runOf(calling("", "read"), says("done")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 5})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Rounds) != 2 || res.Rounds[0].StopReason != llm.StopToolUse ||
		res.Rounds[1].StopReason != llm.StopEnd {
		t.Fatalf("rounds = %+v", res.Rounds)
	}
}

// THE CONVERSATION ONLY GROWS. A model answers the conversation it was sent, so
// every run a phase makes is handed the previous one's conversation with
// messages ADDED at the end, and the conversation it hands back extends the
// last one — whatever added them: a run's rounds and results, a person's note,
// the empty-answer corrective, the finishing one. And every assistant turn is
// the response as the model gave it: its prose and its calls, untouched.
func TestTheConversationOnlyGrows(t *testing.T) {
	t.Parallel()
	box := &stepBox{log: &stepLog{}}
	surf := newSurface("search", "submit")
	surf.hook = offerDuring(box, "search", "use the staging channel")
	responses := []llmtest.Response{
		{Text: "searching", Calls: []llmtest.Call{{ID: "c1", Name: "search", Args: `{"q":"x"}`}}},
		{},                  // said nothing and called nothing
		{Text: "All done."}, // prose where a submission was owed
		{Calls: []llmtest.Call{{ID: "c2", Name: "submit"}}},
	}
	prov := script(runOf(responses[0], says("never reached")), runOf(responses[1]),
		runOf(responses[2]), runOf(responses[3]))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 8, Steer: box,
		TerminateAfter: []string{"submit"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := prov.Seen()
	if len(seen) != 4 {
		t.Fatalf("made %d runs, want 4 — the scenario did not run as written", len(seen))
	}
	if indexOf(seen[1].Messages, "use the staging channel") < 0 {
		t.Fatal("the note never reached the conversation — the scenario did not run as written")
	}
	grows := func(what string, before, after []llm.Message) {
		t.Helper()
		if len(after) <= len(before) {
			t.Errorf("%s: %d messages after %d — nothing was added", what, len(after), len(before))
			return
		}
		for i := range before {
			if !reflect.DeepEqual(before[i], after[i]) {
				t.Errorf("%s: message %d changed\n  was %+v\n  now %+v", what, i, before[i], after[i])
			}
		}
	}
	for k := 1; k < len(seen); k++ {
		grows("run "+string(rune('0'+k+1)), seen[k-1].Messages, seen[k].Messages)
	}
	grows("the conversation handed back", seen[len(seen)-1].Messages, res.Messages)

	var turns int
	for _, m := range res.Messages {
		if m.Role != llm.RoleAssistant {
			continue
		}
		want := responses[turns]
		if m.Content != want.Text || len(m.ToolCalls) != len(want.Calls) {
			t.Errorf("assistant turn %d = %+v, want the response as given: %+v", turns+1, m, want)
		}
		for j, c := range m.ToolCalls {
			if c.ID != want.Calls[j].ID || c.Name != want.Calls[j].Name {
				t.Errorf("assistant turn %d call %d = %+v, want %+v", turns+1, j, c, want.Calls[j])
			}
		}
		turns++
	}
	if turns != len(responses) {
		t.Errorf("%d assistant turns in the conversation, want %d", turns, len(responses))
	}
}

package toolloop_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
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
	surf := newSurface("slow", "submit_work")
	// The slow call is answered "still running" at 300ms and runs to 600, so
	// the engine holds the next run for the rest of it: the gap.
	surf.hook = func(_ context.Context, call llm.ToolCall) {
		if call.Name == "slow" {
			time.Sleep(600 * time.Millisecond)
		}
	}
	prov := script(
		runOf(calling("", "slow")),
		runOf(llmtest.Response{Writing: pause(10 * time.Millisecond), Calls: []llmtest.Call{{Name: "submit_work"}}}),
	)
	prov.ToolTimeout = 5300 * time.Millisecond
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf,
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

// THE RUNNING CALL IS PUBLISHED BEFORE THE CALL AND CLEARED AFTER: the frame a
// tool sees when it starts names it running — its round, its arguments, a UTC
// start — beside the calls that already answered and the round's own start, and
// once the round's calls return no frame names it, nor does the finished result.
func TestTheRunningCallIsPublishedBeforeTheCallAndClearedAfter(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	surf := newSurface("read", "write")
	var atCall []toolloop.Result
	surf.hook = func(context.Context, llm.ToolCall) { atCall = append(atCall, fr.last()) }
	prov := script(runOf(
		llmtest.Response{Text: "reading", Calls: []llmtest.Call{
			{Name: "read", Args: `{"path":"a.go"}`}, {Name: "write", Args: `{"path":"b.go"}`},
		}},
		says("done"),
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: surf, MaxRounds: 4, OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(atCall) != 2 {
		t.Fatalf("saw %d frames at call time, want one per call", len(atCall))
	}
	for i, want := range []struct {
		name, path string
		done       int
	}{{"read", "a.go", 0}, {"write", "b.go", 1}} {
		frame := atCall[i]
		if len(frame.Running) != 1 {
			t.Fatalf("call %q reached the tool with %d calls named running", want.name, len(frame.Running))
		}
		running := frame.Running[0]
		if running.Name != want.name || running.Args["path"] != want.path || running.Round != 1 || running.ID == "" {
			t.Errorf("the frame before %q named %+v", want.name, running)
		}
		if running.StartedAt.IsZero() || running.StartedAt.Location() != time.UTC {
			t.Errorf("the running call's start is %v, want a UTC instant", running.StartedAt)
		}
		if len(frame.Executions) != want.done {
			t.Errorf("the frame before %q carried %d executions, want %d", want.name, len(frame.Executions), want.done)
		}
		if frame.RoundsUsed != 1 || !frame.RoundStartedAt.Equal(res.Rounds[0].StartedAt) {
			t.Errorf("the frame before %q says round %d started %v, want round 1's %v",
				want.name, frame.RoundsUsed, frame.RoundStartedAt, res.Rounds[0].StartedAt)
		}
	}
	all := fr.list()
	var after *toolloop.Result
	for i := range all {
		if len(all[i].Executions) == 2 {
			after = &all[i]
			break
		}
	}
	if after == nil || len(after.Running) != 0 {
		t.Errorf("the first frame after the round's calls = %+v; want none running", after)
	}
	if res.Running != nil || !res.RoundStartedAt.IsZero() || !res.RunStartedAt.IsZero() || res.Partial != nil {
		t.Errorf("a finished result carries live-only state: running %+v, round start %v, run start %v",
			res.Running, res.RoundStartedAt, res.RunStartedAt)
	}
}

// A LIVE FRAME KEEPS THE RUN'S CLOCK APART FROM THE ROUND'S. A run is many
// requests: while one is out, the frame names when the run began — the start
// its invocation is recorded with — and when the request began, which is later
// once the run has made one; between runs, and on the finished result, no run
// is out and the run's start is zero.
func TestALiveFrameNamesTheRunInFlightApartFromItsRound(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	var during toolloop.Result
	prov := script(
		runOf(calling("", "read"), llmtest.Response{Text: writtenSubmission, Writing: func() {
			time.Sleep(5 * time.Millisecond)
			during = fr.last()
		}}),
		runOf(calling("", "submit_work")),
	)
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read", "submit_work"),
		OnProgress: fr.on, TerminateAfter: []string{"submit_work"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Invocations) != 2 {
		t.Fatalf("invocations %+v, want two runs", res.Invocations)
	}
	if !during.RunStartedAt.Equal(res.Invocations[0].StartedAt) {
		t.Errorf("while run 1's second request is out the frame says the run began %v, want run 1's %v",
			during.RunStartedAt, res.Invocations[0].StartedAt)
	}
	if !during.RoundStartedAt.After(during.RunStartedAt) {
		t.Errorf("the request in flight began %v, not after its run began %v — the run's clock, not the round's",
			during.RoundStartedAt, during.RunStartedAt)
	}
	between := false
	for _, f := range fr.list() {
		if len(f.Invocations) == 1 && f.RunStartedAt.IsZero() {
			between = true
		}
		if !f.RunStartedAt.IsZero() && len(f.Invocations) == 1 &&
			!f.RunStartedAt.Equal(res.Invocations[1].StartedAt) {
			t.Errorf("a frame of run 2 says it began %v, want %v", f.RunStartedAt, res.Invocations[1].StartedAt)
		}
	}
	if !between {
		t.Error("no frame between the two runs said no run was out")
	}
	if !res.RunStartedAt.IsZero() {
		t.Errorf("a finished result names a run in flight from %v", res.RunStartedAt)
	}
}

// A REQUEST IN FLIGHT HAS ITS OWN ROUND AND ITS OWN INSTANT on the live view.
// Once a round's calls have all answered, the CLI is asking its model again:
// the frame says so — the next round, started after every recorded one, no call
// running — rather than naming the previous round for as long as the model
// takes, which is what a reader counts "thinking for 40s" from.
func TestARequestInFlightIsAnnouncedBeforeTheModelAnswers(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	var during toolloop.Result
	prov := script(runOf(
		calling("reading", "read"),
		llmtest.Response{Text: "done", Writing: func() {
			time.Sleep(5 * time.Millisecond)
			during = fr.last()
		}},
	))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 4,
		OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if during.RoundsUsed != 2 || len(during.Rounds) != 1 {
		t.Fatalf("while round 2's request is out the live view says round %d with %d recorded; "+
			"want round 2, 1 recorded", during.RoundsUsed, len(during.Rounds))
	}
	if !during.RoundStartedAt.After(during.Rounds[0].StartedAt) {
		t.Errorf("round 2's frame carries start %v, not later than round 1's %v",
			during.RoundStartedAt, during.Rounds[0].StartedAt)
	}
	if len(during.Running) != 0 {
		t.Errorf("a request's frame names a tool call in flight: %+v", during.Running)
	}
	if res.RoundsUsed != 2 {
		t.Errorf("rounds = %d, want 2", res.RoundsUsed)
	}
}

// PROGRESS IS PUBLISHED AS A RUN OPENS, AS EACH RESPONSE IS READ, BEFORE EACH
// CALL AND AFTER IT: a reader sees the request in flight before the model
// answers, the model's prose before its tools run, each call named as it starts,
// and its result once it returns.
func TestProgressIsPublishedAsARunOpensAnswersAndAroundEachCall(t *testing.T) {
	t.Parallel()
	fr := &frames{}
	prov := script(runOf(calling("working", "read", "read"), says("done")))
	res, err := run(t, toolloop.Config{Provider: prov, Surface: newSurface("read"), MaxRounds: 5,
		OnProgress: fr.on})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	all := fr.list()
	if len(all) == 0 || len(all[0].Narration) != 0 || all[0].RoundsUsed != 1 {
		t.Fatalf("the first frame %+v, want the opening of round 1 before the model answered", all)
	}
	// The order a reader saw things in.
	var seen []string
	for _, f := range all {
		var step string
		switch {
		case len(f.Running) == 1:
			step = "running " + strings.Repeat("+", len(f.Executions))
		case len(f.Narration) > 0 && f.Narration[0].Content == "working" && len(f.Executions) == 0:
			step = "spoke"
		case len(f.Executions) == 2:
			step = "returned"
		default:
			continue
		}
		if len(seen) == 0 || seen[len(seen)-1] != step {
			seen = append(seen, step)
		}
	}
	want := []string{"spoke", "running ", "running +", "returned"}
	if !slices.Equal(seen[:min(len(seen), len(want))], want) {
		t.Errorf("a reader saw %v, want %v", seen, want)
	}
	// A CALL STILL TO COME IS NOT A CALL THAT FAILED: no frame records one
	// as not run while the CLI is about to make it.
	for i, f := range all {
		for _, m := range f.Messages {
			if m.Role == llm.RoleTool && m.Failed {
				t.Errorf("frame %d records %s as failed: %q", i, m.Name, m.Content)
			}
		}
	}
	if res.Running != nil {
		t.Errorf("a finished result names a call in flight: %+v", res.Running)
	}
}

// A CALL IS TOLD ITS ROUND, on the scale its records use, which is what the
// assistant's brief turn holds its reads to; and a context no loop stamped
// answers none, so a tool called outside a loop is never read as in round 0.
//
// Mutation: drop the stamp in the dispatcher's execute and every call answers
// no round.
func TestACallIsToldItsRound(t *testing.T) {
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
	if _, err := toolloop.Run(t.Context(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(rounds, []int{1, 2}) {
		t.Errorf("calls were told rounds %v, want [1 2], the rounds their records name", rounds)
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

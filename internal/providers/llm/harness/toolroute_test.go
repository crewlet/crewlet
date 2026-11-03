package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// fakeClaude is a claude-code entry whose harness is this test binary playing
// script — the shipped profile, its tool route and its request record, with
// only the binary replaced. Signed in with a fake key: the fake reaches no
// vendor.
func fakeClaude(t *testing.T, script fakeScript, tune func(*Config)) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	raw, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Key: "seat-llm", Harness: "claude-code", StateDir: dir, Timeout: 20 * time.Second,
		MaxConcurrent: 2, Overrides: map[string]any{"binary": os.Args[0]},
		Env:  fakeChildEnv(map[string]string{fakeMCPEnv: string(raw)}),
		Auth: Auth{Mode: AuthAPIKey, APIKey: "sk-ant-test-not-a-key"},
	}
	if tune != nil {
		tune(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// phase is a seat's request with tools, as the tool loop sends one.
func phase(host llm.ToolHost) llm.Request {
	return llm.Request{Agent: true, Tools: host, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent Dev at Nimbus."},
		{Role: llm.RoleUser, Content: "do the task"},
	}}
}

// A RUN'S CALLS REACH THE HOST, AND EACH IS JOINED TO THE REQUEST THAT ASKED
// FOR IT. The harness calls the seat's tools itself, over MCP, and the host sees
// nothing but the calls — so the requests between them, their prose and their
// calls as the model asked for them reach it only as the backend reads them
// from the harness's record. Each request is reported in order, its calls named as
// the host offered them (`search`, not `mcp__crewlet__search`), the harness's own
// tools marked as the harness's, and the id the harness hands the server with a call
// is the id the record gives it.
//
// Mutation: report calls under the name the harness shows its model, and the host
// would match none of them to a tool it offered; drop the host flag, and the
// harness's own WebFetch reads as a call the host never saw.
func TestARunsCallsReachTheHostAndAreJoinedToTheirRequests(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{Model: "claude-test-7", Responses: []fakeResponse{
		{Text: "Looking it up.", Calls: []fakeCall{
			{Name: "search", Args: map[string]any{"q": "release notes"}},
			{Name: "WebFetch", Args: map[string]any{"url": "https://example.com"}, Own: true},
		}, Usage: fakeUsage{Input: 100, Output: 20}},
		{Text: "Done: the notes are in.", Usage: fakeUsage{Input: 140, Output: 9}},
	}}, nil)
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	var deltas []string
	req := phase(host)
	req.OnDelta = func(d llm.Delta) { deltas = append(deltas, d.Content) }

	comp, err := p.Complete(t.Context(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "Done: the notes are in." || comp.StopReason != llm.StopEnd {
		t.Errorf("completion = %q (%q), want the last response's prose, ended", comp.Content, comp.StopReason)
	}
	if comp.Model != "claude-test-7" {
		t.Errorf("Model = %q, want the model the record names", comp.Model)
	}
	_, calls, _, _ := host.snapshot()
	if len(calls) != 1 || calls[0].Name != "search" || calls[0].Args["q"] != "release notes" {
		t.Fatalf("the host was called %+v, want the one search", calls)
	}
	turns := host.latestTurns()
	if len(turns) != 2 {
		t.Fatalf("the host was told of %d requests, want 2: %+v", len(turns), turns)
	}
	first := turns[0]
	if first.Text != "Looking it up." || first.Stop != llm.StopToolUse || first.Model != "claude-test-7" {
		t.Errorf("first request = %+v", first)
	}
	if len(first.Calls) != 2 {
		t.Fatalf("first request's calls = %+v, want search and the harness's own fetch", first.Calls)
	}
	search, fetch := first.Calls[0], first.Calls[1]
	if search.Name != "search" || !search.Host || search.Arguments["q"] != "release notes" {
		t.Errorf("the host's call was reported as %+v", search)
	}
	if id, _ := calls[0].Meta["claudecode/toolUseId"].(string); id == "" || id != search.ID {
		t.Errorf("the call the host ran carried id %q and the record names %q — they must join",
			calls[0].Meta["claudecode/toolUseId"], search.ID)
	}
	if fetch.Name != "WebFetch" || fetch.Host {
		t.Errorf("the harness's own fetch was reported as %+v", fetch)
	}
	if second := turns[1]; second.Text != "Done: the notes are in." || second.Stop != llm.StopEnd || len(second.Calls) != 0 {
		t.Errorf("second request = %+v", second)
	}
	if got := strings.Join(deltas, "|"); got != "Looking it up.|Done: the notes are in." {
		t.Errorf("streamed %q, want each response's prose once, in order", got)
	}
	host.mu.Lock()
	opts := host.opts[0]
	host.mu.Unlock()
	if !opts.Relists || opts.ToolTimeout != 10*time.Minute || !opts.ObservesRequests ||
		opts.Concurrency != llm.CallsReadOnlyAtOnce || opts.CallIDMeta != "claudecode/toolUseId" {
		t.Errorf("the host was told %+v, want the profile's relisting, its 600s tool timeout, "+
			"a record of requests, calls made beside each other only when read-only, and "+
			"call ids under claudecode/toolUseId", opts)
	}
}

// EACH REQUEST IS CHARGED AS IT IS READ, AND THE RUN'S OWN TOTAL SETTLES THE
// REST. A phase's whole loop is one run now, so a budget charged at its end
// gates nothing: the input of a request is charged the moment the record shows
// the request start — so before any call it asks for runs, since the host joins
// a call to its request first — its output when the record closes it, and the
// harness's own total for the run settles whatever that missed, never charging
// anything twice.
//
// THE OUTPUT IS NOT ASKED FOR AT THE CALL: the record closes a request after
// its calls are listed, and the call reaches the engine over the socket while
// that line may still be on its way through the harness's output — a race no host
// waits out, and one this test lost now and then when it asked.
//
// Mutation: charge only at the end (drop the per-request spends), and the
// first spend comes after every call; charge the result's total on top of
// the requests', and the run is charged twice.
func TestEachRequestIsChargedAsItIsReadAndTheTotalSettlesTheRest(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{
		Responses: []fakeResponse{
			{Text: "a", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 100, Output: 20, CacheRead: 50, CacheWrite: 7}},
			{Text: "b", Usage: fakeUsage{Input: 200, Output: 30}},
		},
		// The run's own count holds 13 output tokens no request reported —
		// a side request the harness made on its own account.
		ResultUsage: &fakeUsage{Input: 300, Output: 63, CacheRead: 50, CacheWrite: 7},
	}, nil)
	host := newTestHost(t, "search")
	var spentBeforeCall llm.Usage
	recorded := false
	host.handle("search", func(ctx context.Context, _ map[string]any) (string, bool) {
		recorded = host.awaitRecorded(ctx, "search")
		_, _, spentBeforeCall, _ = host.snapshot()
		return "ok", false
	})
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	spentBeforeCall.Output = 0
	if want := (llm.Usage{Input: 157, CacheRead: 50, CacheWrite: 7}); !recorded || spentBeforeCall != want {
		t.Errorf("before the first call ran (recorded: %v) the host was charged %+v in, want the "+
			"first request's %+v", recorded, spentBeforeCall, want)
	}
	_, _, spent, spends := host.snapshot()
	if want := (llm.Usage{Input: 357, Output: 63, CacheRead: 50, CacheWrite: 7}); spent != want {
		t.Errorf("the run was charged %+v in all (%+v), want the harness's own total %+v", spent, spends, want)
	}
	if comp.InputTokens != 357 || comp.OutputTokens != 63 {
		t.Errorf("completion carries %d in / %d out, want the run's totals", comp.InputTokens, comp.OutputTokens)
	}
	turns := host.latestTurns()
	if len(turns) != 2 || turns[0].Usage == nil || *turns[0].Usage != (llm.Usage{Input: 157, Output: 20, CacheRead: 50, CacheWrite: 7}) {
		t.Errorf("the first request's record carries %+v, want its own counts", turns)
	}
}

// A HARNESS WHOSE RECORD SHOWS NO REQUESTS IS NOT REFUSED. Per-request structure
// is optional per profile: the host is told the run observes none — so it
// counts the whole run as one round — is told of no request, and is charged
// the run's own total once it ends.
//
// Mutation: tell the host requests are observed when the profile reads none,
// and it waits on a record that never comes; skip the settle, and the run is
// charged nothing.
func TestARunWithNoRequestRecordIsOneRoundChargedAtTheEnd(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
		{Text: "a", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 100, Output: 20}},
		{Text: "the answer", Usage: fakeUsage{Input: 130, Output: 10}},
	}}, func(c *Config) { c.Overrides["requests"] = nil })
	host := newTestHost(t, "search")
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	turns, calls, spent, spends := host.snapshot()
	host.mu.Lock()
	observes := host.opts[0].ObservesRequests
	host.mu.Unlock()
	if observes || len(turns) != 0 {
		t.Errorf("the host was told the run observes requests (%v) and %d of them", observes, len(turns))
	}
	if len(calls) != 1 {
		t.Errorf("calls = %+v, want the one search", calls)
	}
	if len(spends) != 1 || spent != (llm.Usage{Input: 230, Output: 30}) {
		t.Errorf("charged %+v, want the run's own total once", spends)
	}
	if comp.Content != "the answer" {
		t.Errorf("Content = %q, want the answer path's", comp.Content)
	}
}

// A RECORD THAT SHOWS REQUESTS BUT NOT THEIR COUNTS IS CHARGED AN ESTIMATE AS
// EACH ONE STARTS — the prompt the engine sent and what the requests before it
// wrote — so the budget gate sees a run's spend while it goes, and the harness's
// own total settles the difference at the end, never refunding below what was
// charged.
//
// Mutation: estimate only at the end, and nothing is charged before the call
// runs.
func TestARecordWithoutCountsIsChargedAnEstimateAsItGoes(t *testing.T) {
	t.Parallel()
	noUsage := func(events []any) []any {
		out := make([]any, 0, len(events))
		for _, e := range events {
			rule := map[string]any{}
			for k, v := range e.(map[string]any) {
				if k != "usage" {
					rule[k] = v
				}
			}
			out = append(out, rule)
		}
		return out
	}
	builtin, _ := harnessprofile.Builtin("claude-code")
	raw, err := yaml.Marshal(builtin.Requests)
	if err != nil {
		t.Fatal(err)
	}
	var requests map[string]any
	if err := yaml.Unmarshal(raw, &requests); err != nil {
		t.Fatal(err)
	}
	events := requests["events"].([]any)
	p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
		{Text: "a", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 100, Output: 20}},
		{Text: "done", Usage: fakeUsage{Input: 130, Output: 10}},
	}, ResultUsage: &fakeUsage{Input: 50_000, Output: 900}}, func(c *Config) {
		c.Overrides["requests"] = map[string]any{"events": noUsage(events)}
	})
	host := newTestHost(t, "search")
	var before llm.Usage
	var estimated bool
	host.handle("search", func(ctx context.Context, _ map[string]any) (string, bool) {
		// As the tool loop's host does before it runs a call: the call and
		// the record race, and the request is charged as the record shows it.
		if !host.awaitRecorded(ctx, "search") {
			t.Error("the record never showed the request that asked for the call")
		}
		_, _, before, _ = host.snapshot()
		_, _, _, spends := host.snapshot()
		estimated = len(spends) > 0 && spends[0].Estimated
		return "ok", false
	})
	if _, err := p.Complete(t.Context(), phase(host)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if before.Input == 0 || !estimated {
		t.Errorf("before the call ran the host was charged %+v (estimated %v), want an estimate "+
			"of the first request's input", before, estimated)
	}
	if _, _, spent, _ := host.snapshot(); spent.Input != 50_000 || spent.Output != 900 {
		t.Errorf("the run was charged %+v in all, want the harness's own total", spent)
	}
}

// A RUN THAT FAILS IS CHARGED WHAT IT SPENT. A run is a phase's whole loop,
// and a failure — a crash, a refused request, a run cut off mid-answer —
// leaves most harnesses printing no total at all: the requests the record showed
// are charged as they were read, and a request the record never closed is
// charged an estimate of what it wrote, flagged as one.
//
// Mutation: report spend only on a run that answered, and the failed run is
// charged nothing.
func TestARunThatFailsIsChargedWhatItSpent(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{
		Responses: []fakeResponse{
			{Text: "first", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 100, Output: 20}},
			{Text: strings.Repeat("words ", 40), Usage: fakeUsage{Input: 180}, StopEarly: true},
		},
		NoResult: true, Exit: 3, Stderr: "the harness fell over\n",
	}, nil)
	host := newTestHost(t, "search")
	_, err := p.Complete(t.Context(), phase(host))
	if llm.KindOf(err) != llm.KindServer {
		t.Fatalf("err = %v, want a server failure", err)
	}
	_, _, spent, spends := host.snapshot()
	if spent.Input != 280 {
		t.Errorf("input charged %d, want both requests' 280 (%+v)", spent.Input, spends)
	}
	estimated := slices.ContainsFunc(spends, func(u llm.Usage) bool { return u.Estimated && u.Output > 0 })
	if spent.Output <= 20 || !estimated {
		t.Errorf("output charged %d (%+v), want the first's 20 and an estimate of the cut-off second's",
			spent.Output, spends)
	}
}

// THE HOST ENDING A RUN STOPS THE HARNESS, WITHIN ITS GRACE, AND IS NO FAILURE. A
// phase that finished, parked or was cut ends its run mid-way: the harness is
// signalled, given its grace, and killed if it ignores the signal — and the
// run comes back as the answer of what it did, its record told to the host,
// for the tool loop to read.
//
// Mutation: ignore Done, and the stubborn harness holds the call until the
// timeout; report the stopped run as an error, and the chain walks to the
// next member over a phase that ended.
func TestTheHostEndingARunStopsTheHarnessWithinItsGrace(t *testing.T) {
	t.Parallel()
	for name, stubborn := range map[string]bool{"a harness that stops when asked": false, "one that ignores it": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := fakeClaude(t, fakeScript{
				Responses: []fakeResponse{{Text: "submitting", Calls: []fakeCall{{Name: "submit"}},
					Usage: fakeUsage{Input: 10, Output: 5}}},
				Hang: !stubborn, HangUnsignalled: stubborn,
			}, nil)
			p.termGrace = 300 * time.Millisecond
			host := newTestHost(t, "submit")
			host.handle("submit", func(context.Context, map[string]any) (string, bool) {
				host.end()
				return "submitted", false
			})
			started := time.Now()
			comp, err := p.Complete(t.Context(), phase(host))
			if err != nil {
				t.Fatalf("a run the host ended came back as a failure: %v", err)
			}
			if took := time.Since(started); took > 5*time.Second {
				t.Errorf("the run took %v to end, want about its grace", took)
			}
			if comp.Content != "submitting" || comp.StopReason != "" {
				t.Errorf("completion = %q (%q), want the run's last prose and no stop reason "+
					"of the model's", comp.Content, comp.StopReason)
			}
			if turns := host.latestTurns(); len(turns) != 1 || len(turns[0].Calls) != 1 {
				t.Errorf("the host was told %+v, want the one request and its call", turns)
			}
		})
	}
}

// A RUN THAT NEVER REACHED ITS TOOLS IS A SERVER FAILURE, NOT AN ANSWER. A harness
// whose server configuration did not take answers without them, and an
// answer from a seat that never had its tools reads exactly like a model
// that chose to call none — so it is a failure naming the route, which the
// chain may carry to another member.
//
// Mutation: hand the answer back whatever Release said, and this comes back
// as a completion.
func TestARunThatNeverReachedItsToolsIsAServerFailure(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{NoConnect: true, Responses: []fakeResponse{
		{Text: "I could not find any tools, so here is a guess.", Usage: fakeUsage{Input: 10, Output: 5}},
	}}, nil)
	host := newTestHost(t, "search")
	_, err := p.Complete(t.Context(), phase(host))
	if llm.KindOf(err) != llm.KindServer || !strings.Contains(err.Error(), "never reached the seat's tools") {
		t.Fatalf("err = %v, want a server failure saying the harness never reached its tools", err)
	}
	if _, _, spent, _ := host.snapshot(); spent.Total() != 15 {
		t.Errorf("the failed run was charged %+v, want what it spent", spent)
	}
}

// timeout_seconds BOUNDS A STRETCH OF THE MODEL'S OWN WORK, NOT THE RUN. A
// tool the engine runs stops the clock — the model is not working, and a long
// call is bounded by its own deadline — and a fresh stretch starts when it
// returns; a model silent past the limit with nothing in flight is still a
// timeout.
//
// Mutation: bound the whole run (never pause the clock), and the first case
// times out inside its tool call.
func TestTheClockStopsWhileAToolRuns(t *testing.T) {
	t.Parallel()
	// The first stretch is the harness's start too — a race-built binary, its
	// relay and the listing — so the limit leaves that room on a loaded
	// runner, where 700ms was within reach of a start alone.
	limit := 1500 * time.Millisecond
	t.Run("a call longer than the limit", func(t *testing.T) {
		t.Parallel()
		p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
			{Text: "working", Calls: []fakeCall{{Name: "slow"}}, Usage: fakeUsage{Input: 1, Output: 1}},
			{Text: "done", Usage: fakeUsage{Input: 1, Output: 1}},
		}}, func(c *Config) { c.Timeout = limit })
		host := newTestHost(t, "slow")
		host.handle("slow", func(context.Context, map[string]any) (string, bool) {
			time.Sleep(3 * limit)
			return "finished", false
		})
		comp, err := p.Complete(t.Context(), phase(host))
		if err != nil {
			t.Fatalf("a run whose tool outlasted the limit was cut: %v", err)
		}
		if comp.Content != "done" {
			t.Errorf("Content = %q", comp.Content)
		}
		host.mu.Lock()
		busy := slices.Clone(host.busy)
		host.mu.Unlock()
		if !slices.Equal(busy, []bool{true, false}) {
			t.Errorf("the backend was told %v, want the call starting and ending", busy)
		}
	})
	t.Run("a silent stretch past it", func(t *testing.T) {
		t.Parallel()
		p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
			{Text: "thinking", SilentMS: int(4 * limit / time.Millisecond), Usage: fakeUsage{Input: 1, Output: 1}},
		}}, func(c *Config) { c.Timeout = limit })
		_, err := p.Complete(t.Context(), phase(newTestHost(t, "slow")))
		if llm.KindOf(err) != llm.KindTimeout {
			t.Fatalf("err = %v, want a timeout", err)
		}
	})
}

// A MODEL CALL MADE INSIDE A TOOL CALL RUNS ON THE RUN'S SLOT. A tool may ask
// a model itself — a condensing pass, a judge — on the seat's own entry, and
// at max_concurrent 1 the run holds the entry's only slot for as long as the
// tool takes: waiting for a slot of its own, the nested call would wait for
// the run, which waits for it. And the slot is lent to calls nested in the
// run's tools ALONE: a model call from anywhere else still waits for one.
//
// Mutation: drop the lease from the context the run is served under, and the
// nested call waits until the test's deadline; lend to any caller, and the
// unrelated call below starts a second harness beside the first.
func TestAModelCallInsideAToolCallRunsOnTheRunsSlot(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{
		Responses: []fakeResponse{
			{Text: "condensing", Calls: []fakeCall{{Name: "condense"}}, Usage: fakeUsage{Input: 1, Output: 1}},
			{Text: "done", Usage: fakeUsage{Input: 1, Output: 1}},
		},
		TextAnswer: "the gist",
	}, func(c *Config) { c.MaxConcurrent = 1 })
	host := newTestHost(t, "condense")
	var unrelated, nestedErr error
	var nestedAnswer string
	host.handle("condense", func(ctx context.Context, _ map[string]any) (string, bool) {
		// Another seat's call, made while the run holds the slot: it waits,
		// and gives up at its own short deadline.
		outside, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, unrelated = p.Complete(outside, //nolint:contextcheck // another seat's call carries none of this call's values
			llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}})

		nested, cancel2 := context.WithTimeout(ctx, 5*time.Second)
		defer cancel2()
		comp, err := p.Complete(nested, llm.Request{Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "condense this"},
		}})
		if err != nil {
			nestedErr = err
			return "nested call failed", true
		}
		nestedAnswer = comp.Content
		return comp.Content, false
	})
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if nestedErr != nil || nestedAnswer != "the gist" {
		t.Errorf("the call made inside the tool got %q (%v), want the gist on the run's slot",
			nestedAnswer, nestedErr)
	}
	if comp.Content != "done" {
		t.Errorf("Content = %q", comp.Content)
	}
	if llm.KindOf(unrelated) != llm.KindTimeout {
		t.Errorf("a call from outside the run got %v, want a wait for a slot that timed out", unrelated)
	}
	if len(p.slots) != 0 {
		t.Errorf("%d slots still held after every call returned", len(p.slots))
	}
}

// A RUN THE HARNESS'S OWN TURN CAP ENDED IS AN ANSWER STOPPED AT THE CAP. Claude
// Code at --max-turns exits 1 with a result event it flags as an error; read
// as a failure, the chain would hand the phase to another member to start
// again, while the run did what it did and stopped — the tool loop decides
// what follows.
//
// Mutation: drop the cap signature, and this is a server failure.
func TestARunTheHarnessOwnCapEndedIsAnAnswerStoppedAtTheCap(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{
		Responses: []fakeResponse{{Text: "still going", Calls: []fakeCall{{Name: "search"}},
			Usage: fakeUsage{Input: 5, Output: 5}}},
		Exit: 1, IsError: true, Subtype: "error_max_turns",
	}, nil)
	host := newTestHost(t, "search")
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("a capped run came back as a failure: %v", err)
	}
	if comp.StopReason != llm.StopCap || comp.Content != "still going" {
		t.Errorf("completion = %q (%q), want the last prose stopped at the cap", comp.Content, comp.StopReason)
	}
}

// A FAILED RUN'S ERROR QUOTES NO LINE OF ITS RECORD. The record carries every
// call's arguments and every tool's result, and an error is shown and logged
// wherever a failure goes; what it says is stderr and the harness's own failure
// event, by field.
//
// Mutation: put the tail of stdout in the error, as a text function's does,
// and both markers below are in it.
func TestAFailedRunsErrorQuotesNoLineOfItsRecord(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{
		Responses: []fakeResponse{{Text: "posting", Calls: []fakeCall{
			{Name: "post", Args: map[string]any{"body": "ARGUMENT-MARKER"}},
		}, Usage: fakeUsage{Input: 5, Output: 5}}},
		Exit: 1, IsError: true, Subtype: "error_during_execution",
		Result: "API Error: 500 the vendor is overloaded", Stderr: "retrying failed\n",
	}, nil)
	host := newTestHost(t, "post")
	host.handle("post", func(context.Context, map[string]any) (string, bool) { return "RESULT-MARKER", false })
	_, err := p.Complete(t.Context(), phase(host))
	if llm.KindOf(err) != llm.KindServer {
		t.Fatalf("err = %v, want a server failure", err)
	}
	msg := err.Error()
	for _, want := range []string{"exited 1", "subtype=error_during_execution", "API Error: 500", "retrying failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not say %q: %s", want, msg)
		}
	}
	for _, leaked := range []string{"ARGUMENT-MARKER", "RESULT-MARKER", "tool_use", "stream_event"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("the error quotes the run's record (%q): %s", leaked, msg)
		}
	}
}

// A LOGIN THE HARNESS REFRESHES MID-RUN IS CARRIED BACK WHILE THE RUN GOES ON. A
// run is a seat's whole phase, alive through every tool it calls, and every
// other seat seeds its home from the shared login: a refreshed token carried
// back only when the run ends leaves them all on the one the vendor rotated
// away. It is carried at the next tool call — when the harness waits on the engine
// — under the shared login's compare-and-swap, and the generation's baseline
// moves with it.
//
// Mutation: drop the carry from the call boundary, and the tool below finds
// the shared login unchanged while the run is still alive.
func TestALoginRefreshedMidRunIsCarriedBackWhileItGoesOn(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
		{Text: "checking", Calls: []fakeCall{{Name: "check"}}, Usage: fakeUsage{Input: 1, Output: 1},
			RefreshLogin: `{"token":"refreshed"}`},
		// A second refresh before the run ends, which the release carries —
		// over the first, which the run carried itself.
		{Text: "done", Usage: fakeUsage{Input: 1, Output: 1}, RefreshLogin: `{"token":"refreshed again"}`},
	}}, func(c *Config) { c.Auth = Auth{Mode: AuthSubscription} })
	shared := filepath.Join(p.Workspace().CredentialsDir(), ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte(`{"token":"original"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host := newTestHost(t, "check")
	// Seen from INSIDE the run: the call waits on the engine while the harness
	// that refreshed the login is still alive, its generation still open.
	carriedDuringRun := false
	host.handle("check", func(context.Context, map[string]any) (string, bool) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if raw, _ := os.ReadFile(shared); string(raw) == `{"token":"refreshed"}` { //nolint:gosec // the test's own dir
				carriedDuringRun = true
				return "carried", false
			}
			time.Sleep(20 * time.Millisecond)
		}
		return "not carried", true
	})
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "done" {
		t.Errorf("Content = %q", comp.Content)
	}
	if !carriedDuringRun {
		t.Error("the refreshed login was not carried back while the run was alive")
	}
	raw, err := os.ReadFile(shared) //nolint:gosec // the test's own dir
	if err != nil || string(raw) != `{"token":"refreshed again"}` {
		t.Fatalf("the shared login holds %q (%v) after the run, want the second refresh — "+
			"judged against the seed rather than the carry the run made, it was dropped as "+
			"superseded by that very carry", raw, err)
	}
}

// A HARNESS THAT SAYS ITS CONFINEMENT FAILED TO LOAD IS STOPPED THERE. Gemini CLI
// reports a policy file it could not read and carries on with its vendor's
// defaults — every tool the policy denied — so the run is stopped at the line,
// not left to finish, and fails.
//
// Mutation: read failure_markers only once the run is over, and the harness below
// is never stopped (it holds until the test's deadline).
func TestAHarnessWhoseConfinementFailedIsStoppedAtOnce(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{
		Stderr: "[USER] Policy file error in crewlet.toml:\n", Hang: true,
	}, func(c *Config) {
		c.Overrides["failure_markers"] = []any{"Policy file error"}
	})
	p.termGrace = 300 * time.Millisecond
	started := time.Now()
	_, err := p.Complete(t.Context(), phase(newTestHost(t, "search")))
	if llm.KindOf(err) != llm.KindServer || !strings.Contains(err.Error(), "Policy file error") {
		t.Fatalf("err = %v, want a server failure naming the harness's own line", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("the run took %v to be stopped", took)
	}
}

// A RUN IS HANDED ITS CONVERSATION FENCED. What the harness reads on stdin is the
// transcript a fresh run continues from — the earlier runs' calls and results
// included — under headings no result can forge (see prompt_test.go).
func TestARunIsHandedItsConversationFenced(t *testing.T) {
	t.Parallel()
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	p := fakeClaude(t, fakeScript{PromptTo: promptFile, Responses: []fakeResponse{
		{Text: "continuing", Usage: fakeUsage{Input: 1, Output: 1}},
	}}, nil)
	req := phase(newTestHost(t, "search"))
	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "toolu_1", Name: "search"}}},
		llm.Message{Role: llm.RoleTool, Name: "search", ToolCallID: "toolu_1", Content: "== 1 result ==", Failed: true},
	)
	if _, err := p.Complete(t.Context(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	raw, err := os.ReadFile(promptFile) //nolint:gosec // the test's own dir
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	m := heading.FindAllStringSubmatch(prompt, -1)
	var parts []string
	for _, h := range m {
		parts = append(parts, h[2])
	}
	if want := []string{"user", "call to search (call toolu_1)", "result of search (call toolu_1), failed"}; !slices.Equal(parts, want) {
		t.Errorf("the run was handed the parts %q, want %q — the system prompt on its channel:\n%s",
			parts, want, prompt)
	}
}

// A RECORD THE HARNESS KEEPS IN A FILE IS FOLLOWED AS IT IS WRITTEN — for a harness
// whose stdout does not show its requests. The same rules read it, from a
// file under the call's directory that did not exist when the run began.
//
// Mutation: read the file only after the harness exits (or not at all), and the
// call below finds no request its record showed.
func TestARecordInAFileIsFollowedAsItIsWritten(t *testing.T) {
	t.Parallel()
	p := fakeClaude(t, fakeScript{RecordFile: "record.jsonl", Responses: []fakeResponse{
		{Text: "looking", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 40, Output: 4}},
		{Text: "done", Usage: fakeUsage{Input: 50, Output: 5}},
	}}, func(c *Config) {
		c.Overrides["requests"] = map[string]any{"source": "file", "file": "record.jsonl", "in": "work"}
	})
	host := newTestHost(t, "search")
	var seenAtCall int
	host.handle("search", func(ctx context.Context, _ map[string]any) (string, bool) {
		// The file is read every recordPoll, so the call and the record race.
		host.awaitRecorded(ctx, "search")
		seenAtCall = len(host.latestTurns())
		return "ok", false
	})
	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if seenAtCall != 1 {
		t.Errorf("when the call ran the host had been told of %d requests, want the one that asked for it", seenAtCall)
	}
	if turns := host.latestTurns(); len(turns) != 2 || turns[1].Text != "done" || comp.Content != "done" {
		t.Errorf("turns = %+v, content %q", turns, comp.Content)
	}
	if _, _, spent, _ := host.snapshot(); spent.Input != 90 || spent.Output != 9 {
		t.Errorf("spent %+v, want the two requests' counts from the file", spent)
	}
}

// TWO RUNS AT ONCE ARE TWO SESSIONS. Each run is served on its own socket, its
// record read apart, so concurrent phases of one entry never cross.
func TestConcurrentRunsKeepTheirRecordsApart(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, word := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := fakeClaude(t, fakeScript{Responses: []fakeResponse{
				{Text: word, Calls: []fakeCall{{Name: "search", Args: map[string]any{"q": word}}},
					Usage: fakeUsage{Input: 1, Output: 1}},
				{Text: word + " done", Usage: fakeUsage{Input: 1, Output: 1}},
			}}, nil)
			host := newTestHost(t, "search")
			comp, err := p.Complete(context.Background(), phase(host))
			if err != nil {
				errs <- err
				return
			}
			_, calls, _, _ := host.snapshot()
			if comp.Content != word+" done" || len(calls) != 1 || calls[0].Args["q"] != word {
				errs <- errors.New("a run read another's record: " + comp.Content)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

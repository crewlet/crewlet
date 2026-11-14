package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
)

// worst is n bytes of the byte that costs the most once encoded: a `<`, six
// bytes as encoding/json writes it. The words a record carries that somebody
// else wrote — a tool's answer, a model's prose, a prompt, a person's name —
// are any bytes, and are filled with it.
func worst(n int) string { return strings.Repeat("<", n) }

// named is n bytes of what an identifier is made of, which encodes as itself:
// the names a company's configuration gives (a model, a provider entry, an MCP
// server, a tool) and the ids and words the engine mints (a note's id, an
// origin's, a refusal's class, a harness's outcome) are held to such
// grammars, and filled with it at the most each may be.
func named(n int) string { return strings.Repeat("n", n) }

// worstPrompt is a prompt of n bytes in parts outlined parts, each of the byte
// that escapes the most.
func worstPrompt(n, parts int) prompts.Prompt {
	var p prompts.Prompt
	for i := range parts {
		part := worst(n / parts)
		p.Sections = append(p.Sections, prompts.Section{Key: fmt.Sprintf("part_%d", i),
			Title: fmt.Sprintf("Part %d", i), Bytes: len(part)})
		p.Text += part
	}
	return p
}

// worstRounds is how many rounds the worst record ran: twice what a record
// lists, so its window is past its bound.
const worstRounds = 2 * maxRecordRounds

// worstArgs is arguments of n values, each of the byte that escapes the most:
// past a share, they keep as many of their values as fit it, which is the most
// arguments a record can keep, where one large value would be left out whole.
func worstArgs(n int) map[string]any {
	args := map[string]any{}
	for i := range n {
		args[fmt.Sprintf("v%03d", i)] = worst(128)
	}
	return args
}

// worstCalls is a phase's calls at their worst, one a round: each failed, its
// name past the longest a record keeps and unique in its opening, its id one
// no log keeps, its arguments and its answer past their shares in the byte
// that escapes the most — and, last, a successful submission whose arguments
// are past what any record keeps of them.
func worstCalls(rounds int) []toolloop.Execution {
	answer := worst(1 << 10)
	args := worstArgs(20)
	out := make([]toolloop.Execution, 0, rounds+1)
	for round := 1; round <= rounds; round++ {
		out = append(out, toolloop.Execution{
			Round: round, Name: fmt.Sprintf("%d%s", round, worst(2*sandbox.MaxBridgeCallNameBytes)),
			CallID: worst(sandbox.MaxBridgeCallIDBytes + 1), Args: args,
			Output: answer, Failed: true, Origin: "mcp:" + named(128), Server: named(128), Source: "mcp:" + named(128),
			Refusal: "unavailable", StartedAt: time.Unix(1<<32, 0), Duration: time.Hour,
		})
	}
	out = append(out, toolloop.Execution{Round: rounds, Name: SubmitWorkTool, Args: worstArgs(200), Output: answer})
	return out
}

// worstRecord is a phase's record with every input at its worst, a collected
// run's where run is set. EACH PAST ITS SHARE, AND NO FURTHER — the prompts
// past promptBytes, the response past responseBytes, the notes past
// notesBytes and the error past its clip, once escaped — since what a record
// keeps of a payload past its share does not grow with it, and a case that
// weighed its own test binary down by megabytes more would measure nothing
// more.
func worstRecord(run bool) phaseRecord {
	rec := phaseRecord{
		Phase: phase.Execute, Iteration: 1,
		System: worstPrompt(promptBytes/4, 8), User: worstPrompt(promptBytes/4, 8),
		Result: toolloop.Result{
			Text: worst(responseBytes / 5), Executions: worstCalls(worstRounds), RoundsUsed: worstRounds,
			Model: named(256), ProviderKey: named(256),
		},
		Decision: named(64), Notes: worst(notesBytes / 4), Failed: true,
		Err: errors.New(worst(events.MaxDiagnosticBytes + 1)),
	}
	for round := 1; round <= worstRounds; round++ {
		rec.Result.Rounds = append(rec.Result.Rounds, toolloop.Round{Round: round, StartedAt: time.Unix(1<<32, 0),
			Duration: time.Hour, Model: named(256), InputTokens: 1 << 30, OutputTokens: 1 << 30, ToolCalls: 1 << 20,
			StopReason: llm.StopReason(named(64))})
		rec.Result.Narration = append(rec.Result.Narration, toolloop.Narration{Round: round, Content: worst(1 << 10)})
		rec.Result.Invocations = append(rec.Result.Invocations, toolloop.Invocation{
			StartedAt: time.Unix(1<<32, 0), Duration: time.Hour, ProviderKey: named(256), Model: named(256),
			Outcome: toolloop.InvocationOutcome(named(64)), FirstRound: round, LastRound: round, InputTokens: 1 << 30, OutputTokens: 1 << 30})
		rec.Result.Steers = append(rec.Result.Steers, toolloop.SteerMark{Round: round, ID: named(64)})
	}
	for i := range 4096 {
		// THE LONGEST NAME AN MCP SERVER'S TOOL MAY HAVE, in the characters a
		// tool's name is held to, which no escape grows.
		rec.Available = append(rec.Available, fmt.Sprintf("%0128d", i))
		rec.Catalogue = append(rec.Catalogue, fmt.Sprintf("%0128d", i))
	}
	if run {
		// THE PUSH RECORD'S REASON, whose worst is past the pushed refs' own
		// (internal/sandbox's runtext.go): a record carries the refs only
		// where it has no reason, and the reason where it read none.
		rec.Run = RunRecord{Harness: named(64), SandboxID: named(128), ProviderKey: named(256), Model: named(256),
			LaunchID: named(64), PushedUnread: events.ClipDiagnostic(worst(1 << 20))}
		for used := 0; used < sandbox.MaxDeliveredRefBytes; used += 256 {
			rec.Run.DeliveredRefs = append(rec.Run.DeliveredRefs, worst(256))
		}
		for i := range 4096 {
			rec.RunElided = append(rec.RunElided, ledger.Elided{Name: fmt.Sprintf("%d%s", i, worst(512)), Calls: 9,
				Failed: 3})
		}
	}
	return rec
}

// worstTrigger is what woke the turn, at its worst: a summary at its bound
// and a coalesced batch's every origin, each named at length.
func worstTrigger() types.Trigger {
	trigger := types.Trigger{ID: named(64), Type: named(64),
		Summary: textcut.Within(worst(1<<20), types.MaxTriggerSummaryBytes), Actor: worst(256),
		Integration: named(64), Sender: worst(256), SourceEventType: named(64)}
	for range 100 {
		trigger.Origins = append(trigger.Origins, types.TriggerOrigin{Kind: named(32), ID: named(256),
			Reason: named(64)})
	}
	trigger.Origin = trigger.Origins[0]
	return trigger
}

// published is the one event an emitter published, and what it weighs on the
// wire as the queue's twin weighs it: the whole event, encoded.
func published(t *testing.T, pub *collector) (*events.Event, int) {
	t.Helper()
	if len(pub.events) != 1 {
		t.Fatalf("the emitter published %d events, want one", len(pub.events))
	}
	wire, err := json.Marshal(pub.events[0])
	if err != nil {
		t.Fatalf("the record does not encode: %v", err)
	}
	return pub.events[0], len(wire)
}

// worstEmitter is an emitter publishing into pub for a turn its worst
// trigger woke.
func worstEmitter(pub *collector, mu *sync.Mutex) emitter {
	return emitter{pub: pub, turn: Turn{RunID: named(64), AgentID: named(64), WorkKey: named(256),
		Trigger: worstTrigger(), ConversationKey: named(256)}, role: worst(256), tally: &Spend{}, mu: mu}
}

// A PHASE'S RECORD AT EVERY BOUND FITS ONE MESSAGE. Each of its variable
// payloads past its share — its prompts, its response, its prose, its notes,
// its tool names, its rounds and its calls, every one of them of the byte that
// escapes the most, and a collected run's refs, push record and dropped calls
// besides — is kept to its share, and the record, its trigger at its worst
// beside it, is published within the queue's ceiling rather than refused by
// it and lost; and it still says what it left out, by count and by tool.
//
// Mutation: carry any one payload whole (the calls, a prompt, the response,
// the prose, the notes, the names), or widen a share past what the sum leaves,
// and the record is past the ceiling; drop the round window, and a record of a
// thousand rounds is.
func TestAPhaseRecordAtEveryBoundFitsOneMessage(t *testing.T) {
	t.Parallel()
	// ONE AT A TIME: each builds a record of megabytes, and the package's other
	// cases run beside them.
	for name, run := range map[string]bool{"a host phase": false, "a collected run": true} {
		t.Run(name, func(t *testing.T) {
			pub := &collector{}
			var mu sync.Mutex
			worstEmitter(pub, &mu).completed(context.Background(), worstRecord(run))
			ev, size := published(t, pub)
			if size > queue.MaxPayloadBytes {
				t.Fatalf("the record weighs %d bytes, past the queue's %d", size, queue.MaxPayloadBytes)
			}
			t.Logf("the record weighs %d bytes, %d short of the queue's ceiling", size, queue.MaxPayloadBytes-size)
			rec := *ev.Data.(*types.AgentPhaseCompleted)
			if rec.CallsElided == 0 || len(rec.ElidedCalls) != maxTallyNames+1 || rec.RoundsElided != maxRecordRounds ||
				rec.ToolsAvailableElided == 0 || rec.ToolCatalogueElided == 0 {
				t.Errorf("the record says it left out %d calls (%d tallies), %d rounds and %d+%d names; want each "+
					"count, the tally at its census", rec.CallsElided, len(rec.ElidedCalls), rec.RoundsElided,
					rec.ToolsAvailableElided, rec.ToolCatalogueElided)
			}
		})
	}
}

// A WORKER'S RECORD AT EVERY BOUND FITS ONE MESSAGE, as the turn's own phases'
// do: a delegated worker's loop answers its MCP tools whole and runs as many
// rounds as its cap lets it, and its record is held to the same shares.
//
// Mutation: carry a worker's calls, prompts or prose whole, and its record is
// past the ceiling.
func TestAWorkersRecordAtEveryBoundFitsOneMessage(t *testing.T) {
	t.Parallel()
	rec := worstRecord(false)
	res := subagent.Result{ID: named(256), Worker: named(256), Status: subagent.StatusOK, Text: rec.Result.Text,
		Rounds: worstRounds, RoundRecords: rec.Result.Rounds, MaxRounds: worstRounds, Model: named(256),
		ProviderKey: named(256), ToolsAvailable: rec.Available, Executions: rec.Result.Executions,
		Narration: rec.Result.Narration, SystemPrompt: rec.System, UserPrompt: rec.User,
		Error: worst(events.MaxDiagnosticBytes + 1)}
	for i := range 4096 {
		res.Rejected = append(res.Rejected, fmt.Sprintf("%d%s", i, worst(256)))
	}
	pub := &collector{}
	var mu sync.Mutex
	worstEmitter(pub, &mu).subagentCompleted(context.Background(), res)
	if _, size := published(t, pub); size > queue.MaxPayloadBytes {
		t.Fatalf("the worker's record weighs %d bytes, past the queue's %d", size, queue.MaxPayloadBytes)
	}
}

// A TOOL LIST WITHIN ITS SHARE IS KEPT WHOLE, byte for byte the rows it was
// handed, and the tally its record kept already is only folded: nothing about
// an ordinary record changes.
//
// Mutation: cut every row to the bridged call's shares whatever the list
// weighs, and a host phase's long answer is cut on a record that had room.
func TestAToolListWithinItsShareIsKeptWhole(t *testing.T) {
	t.Parallel()
	rows := toolExecutions([]toolloop.Execution{
		{Round: 1, Name: "read_file", Args: map[string]any{"path": "a"}, Output: strings.Repeat("x", 64<<10)},
		{Round: 1, Name: "post_message", Failed: true, Output: "no"},
	})
	got := keptExecutions(rows, []types.ElidedCalls{{Name: "b", Calls: 2}, {Name: "a", Calls: 1}})
	if len(got.Rows) != 2 || got.Rows[0]["result"] != rows[0]["result"] || got.Elided != 3 ||
		!slices.Equal(got.ByTool, []types.ElidedCalls{{Name: "a", Calls: 1}, {Name: "b", Calls: 2}}) {
		t.Errorf("a list within its share kept %+v, want its rows whole and its tally folded", got)
	}
}

// A PHASE'S TOOL LIST PAST ITS SHARE KEEPS ITS FIRST AND NEWEST CALLS AND
// COUNTS THE REST BY TOOL. Every row is held to the bridged call's shares —
// its answer and a failure's error to their openings, its arguments to their
// share with the newest submission's to its own, a name past its bound cut —
// and past the call cap the middle is dropped: the first half and the newest
// half kept, and every dropped call counted under its tool, with its failure.
//
// Mutation: drop from either end, and how the phase began or ended is gone;
// tally a dropped call under no tool, or not at all, and a reader counts fewer
// calls than the phase made; cut the submission like any row, and the record
// shows a submission other than the one the phase made.
func TestAPhaseRecordsToolListPastItsShareKeepsItsEndsAndCountsTheRest(t *testing.T) {
	t.Parallel()
	// EACH ANSWER PAST ITS SHARE ONCE ESCAPED, AND NO FURTHER: seven hundred
	// `<` are 4,200 bytes encoded, past sandbox.MaxBridgeOutputBytes, so every
	// row is cut and the cut rows are still past the list's share together.
	body := worst(700)
	var execs []toolloop.Execution
	for i := range 1000 {
		execs = append(execs, toolloop.Execution{Round: 1, Name: fmt.Sprintf("tool_%d", i%3),
			Args: map[string]any{"text": body}, Output: body, Failed: i%2 == 1})
	}
	execs = append(execs, toolloop.Execution{Round: 1, Name: SubmitWorkTool,
		Args: map[string]any{"summary": worst(8 << 10)}})
	rows := toolExecutions(execs)
	got := keptExecutions(rows, nil)
	if n := len(got.Rows); n != sandbox.MaxBridgeCalls || got.Elided != len(rows)-n {
		t.Fatalf("the list keeps %d rows and counts %d dropped, want %d kept and the rest counted", n, got.Elided,
			sandbox.MaxBridgeCalls)
	}
	if listBytes(got.Rows) > toolListBytes {
		t.Errorf("the kept rows weigh %d bytes, past their share of %d", listBytes(got.Rows), toolListBytes)
	}
	half := sandbox.MaxBridgeCalls / 2
	if got.Rows[0]["name"] != "tool_0" || got.Rows[half-1]["name"] != fmt.Sprintf("tool_%d", (half-1)%3) ||
		got.Rows[len(got.Rows)-1]["name"] != SubmitWorkTool {
		t.Errorf("the kept rows run %v … %v, want the first half and the newest half", got.Rows[0]["name"],
			got.Rows[len(got.Rows)-1]["name"])
	}
	calls, failed := 0, 0
	for _, e := range got.ByTool {
		if !strings.HasPrefix(e.Name, "tool_") {
			t.Errorf("a dropped call is tallied under %q", e.Name)
		}
		calls, failed = calls+e.Calls, failed+e.Failed
	}
	if calls != got.Elided || failed != got.Elided/2 {
		t.Errorf("the tally counts %d calls (%d failed), want the %d dropped (%d failed)", calls, failed,
			got.Elided, got.Elided/2)
	}
	for _, row := range got.Rows[:len(got.Rows)-1] {
		for _, key := range []string{"result", "error"} {
			answer, ok := row[key].(string)
			if !ok {
				continue
			}
			if kept.Quoted(answer) > sandbox.MaxBridgeOutputBytes || !strings.Contains(answer, "not kept on this record") {
				t.Fatalf("a row's %s keeps %d bytes, want its opening within %d, the cut said", key,
					kept.Quoted(answer), sandbox.MaxBridgeOutputBytes)
			}
		}
	}
	if args := got.Rows[len(got.Rows)-1]["arguments"]; args != rows[len(rows)-1]["arguments"] {
		t.Errorf("the submission's arguments were cut to %d bytes, want them whole within the submission's share",
			len(args.(string)))
	}
	if rows[0]["result"] != body {
		t.Error("keeping the list changed the rows it was handed")
	}
}

// A RECORD'S TALLY NAMES THE TOOLS CALLED MOST, AND COUNTS THE REST UNDER NO
// NAME. A model can call names no tool has, each once, and a tally that named
// them all would grow with them: past its census the tools with the fewest
// calls are counted unnamed, every call and failure still counted, and the
// names it keeps in name order with the unnamed last.
//
// Mutation: name every tool, and the tally grows with what the model invents;
// drop the calls past the census rather than count them, and a reader counts
// fewer calls than the phase made.
func TestARecordsTallyNamesTheToolsCalledMost(t *testing.T) {
	t.Parallel()
	var tally []types.ElidedCalls
	for i := range maxTallyNames + 10 {
		tally = append(tally, types.ElidedCalls{Name: fmt.Sprintf("invented_%03d", i), Calls: 1, Failed: 1})
	}
	tally = append(tally, types.ElidedCalls{Name: "read_file", Calls: 50, Failed: 2}, types.ElidedCalls{Calls: 3})
	got := keptTally(tally)
	if len(got) != maxTallyNames+1 || got[len(got)-1].Name != "" || !slices.ContainsFunc(got,
		func(e types.ElidedCalls) bool { return e.Name == "read_file" && e.Calls == 50 }) {
		t.Fatalf("the tally keeps %d entries, want %d named — read_file among them — and the unnamed last",
			len(got), maxTallyNames)
	}
	if calls := tallied(got); calls != tallied(tally) {
		t.Errorf("the tally counts %d calls, want every one of the %d", calls, tallied(tally))
	}
	if !slices.IsSortedFunc(got[:maxTallyNames], func(a, b types.ElidedCalls) int {
		return strings.Compare(a.Name, b.Name)
	}) {
		t.Error("the named entries are not in name order")
	}
}

// A PROMPT PAST ITS SHARE KEEPS EVERY PART'S OPENING, AND AN OUTLINE THAT
// STILL TILES IT. The dashboard reads a prompt by its outline only where the
// outline tiles the text — byte counts adding up, every boundary between two
// characters — so the record cuts the prompt part by part, the longest first,
// and rewrites each part's size to what it kept; a prompt within its share is
// kept as it was.
//
// Mutation: cut the whole text to its opening, and every part after the first
// is gone; keep the outline as it was, and it no longer tiles the text.
func TestAPromptPastItsShareKeepsEveryPartsOpeningAndAnOutlineThatTilesIt(t *testing.T) {
	t.Parallel()
	p := worstPrompt(1<<20, 8)
	p.Sections[0].Bytes, p.Sections[1].Bytes = 3, p.Sections[0].Bytes+p.Sections[1].Bytes-3
	got := keptPrompt(p)
	if kept.Quoted(got.Text) > promptBytes || !got.Valid() || len(got.Sections) != len(p.Sections) {
		t.Fatalf("the prompt keeps %d bytes in %d parts (outline valid %t), want it within %d, every part kept",
			kept.Quoted(got.Text), len(got.Sections), got.Valid(), promptBytes)
	}
	at := 0
	for i, s := range got.Sections {
		part := got.Text[at : at+s.Bytes]
		at += s.Bytes
		if s.Key != p.Sections[i].Key {
			t.Fatalf("part %d is keyed %q, want %q", i, s.Key, p.Sections[i].Key)
		}
		if i == 0 && part != "<<<" {
			t.Errorf("the shortest part keeps %q, want it whole", part)
		}
		if i > 0 && !strings.HasPrefix(part, "<") || i > 0 && !strings.Contains(part, "not kept on this record") {
			t.Errorf("part %d keeps %.40q…, want its opening, the cut said", i, part)
		}
	}
	small := worstPrompt(1<<10, 2)
	if again := keptPrompt(small); again.Text != small.Text || !slices.Equal(again.Sections,
		small.Sections) {
		t.Error("a prompt within its share was changed")
	}
}

// A RECORD'S ROUNDS PAST THEIR WINDOW KEEP THE FIRST HALF AND THE NEWEST HALF,
// each kept round with everything of it — its provider call, its prose, the
// harness runs over it, the notes read in it and the calls made in it — and
// the calls of the rounds left out tallied by tool for the tool list, so no
// round is half drawn.
//
// Mutation: keep a dropped round's calls, prose or notes, and the record draws
// a round it says it left out; tally none of its calls, and a reader counts
// fewer calls than the phase made.
func TestARecordsRoundsPastTheirWindowKeepTheirEnds(t *testing.T) {
	t.Parallel()
	rec := worstRecord(false)
	r := recordRounds{Rounds: phaseRounds(rec.Result.Rounds), Invocations: phaseInvocations(rec.Result.Invocations),
		Steers: phaseSteers(rec.Result.Steers), Narration: roundNarration(rec.Result.Narration),
		Calls: toolExecutions(rec.Result.Executions)}
	got := keptRounds(r)
	if got.Elided != worstRounds-maxRecordRounds || len(got.Rounds) != maxRecordRounds ||
		len(got.Invocations) != maxRecordRounds || len(got.Steers) != maxRecordRounds ||
		len(got.Narration) != maxRecordRounds {
		t.Fatalf("the window keeps %d rounds, %d runs, %d notes and %d prose rows, and leaves out %d; want %d "+
			"of each, and %d left out", len(got.Rounds), len(got.Invocations), len(got.Steers), len(got.Narration),
			got.Elided, maxRecordRounds, worstRounds-maxRecordRounds)
	}
	half := maxRecordRounds / 2
	if got.Rounds[half-1].Round != half || got.Rounds[half].Round != worstRounds-half+1 {
		t.Errorf("the window keeps rounds through %d and from %d, want through %d and from %d",
			got.Rounds[half-1].Round, got.Rounds[half].Round, half, worstRounds-half+1)
	}
	// One call a round, and the submission in the last: the kept rounds' and
	// the rest tallied.
	if len(got.Calls) != maxRecordRounds+1 || tallied(got.Dropped) != worstRounds-maxRecordRounds {
		t.Errorf("the window keeps %d calls and tallies %d, want %d and %d", len(got.Calls),
			tallied(got.Dropped), maxRecordRounds+1, worstRounds-maxRecordRounds)
	}
	if few := (recordRounds{Rounds: r.Rounds[:3], Calls: r.Calls[:3]}); keptRounds(few).Elided != 0 {
		t.Error("a phase within the window lost rounds")
	}
}

// A COLLECTED RUN'S COPY OF ITS ANSWER SAYS THE RESPONSE HOLDS IT. The record
// carries a run's report whole as its response, and its round's prose carries
// a copy, which past the prose's share keeps its opening, its cut saying where
// the rest is, rather than doubling the report on the record or reading as the
// whole lost; and the record keeps the copy as it was made.
//
// Mutation: copy the answer whole, and a long report rides its record twice;
// cut it with the record's own marker, and it reads as lost.
func TestACollectedRunsCopyOfItsAnswerSaysTheResponseHoldsIt(t *testing.T) {
	t.Parallel()
	answer := worst(sandbox.MaxRunTextBytes)
	res := agentRunResult(answer, nil, nil, RunRecord{}, time.Time{}, time.Time{})
	if len(res.Result.Narration) != 1 {
		t.Fatalf("the run's round carries %d prose rows, want its copy", len(res.Result.Narration))
	}
	copied := res.Result.Narration[0].Content
	if kept.Quoted(copied) > answerCopyBytes || !strings.HasSuffix(copied,
		"not kept on this copy; the phase's response holds it whole)") || res.Result.Text != answer {
		t.Errorf("the copy keeps %d bytes ending %q, want its opening within %d, saying the response holds it",
			kept.Quoted(copied), copied[max(len(copied)-80, 0):], answerCopyBytes)
	}
	if rows := keptNarration(roundNarration(res.Result.Narration), narrationBytes); rows[0]["content"] != copied {
		t.Error("the record cut the copy again")
	}
}

// A RECORD'S LIST OF TOOL NAMES KEEPS ITS NAMES IN ORDER WHILE THEY FIT, and
// says how many more the list held.
//
// Mutation: keep every name, and a seat of many servers' tools puts them all
// on every record; count none, and the list reads as all the seat held.
func TestARecordsToolNamesKeepTheirOrderAndCountTheRest(t *testing.T) {
	t.Parallel()
	var names []string
	for i := range 4096 {
		names = append(names, fmt.Sprintf("%0128d", i))
	}
	got, more := keptNames(names)
	raw, _ := json.Marshal(got)
	if len(raw) > namesBytes || more == 0 || len(got)+more != len(names) || got[len(got)-1] != names[len(got)-1] {
		t.Errorf("the list keeps %d names (%d bytes) and counts %d more, want the first within %d and the rest "+
			"counted", len(got), len(raw), more, namesBytes)
	}
	if few, more := keptNames(names[:3]); len(few) != 3 || more != 0 {
		t.Error("a short list lost names")
	}
}

// liveFrameBytes is the most a running phase's frame may weigh at its worst:
// a MiB, the sum of its live shares (recordbytes.go, "The live frame") and its
// round window's records beside its trigger, with room to spare.
const liveFrameBytes = 1 << 20

// worstRunning is a phase in flight at its worst: the worst record's rounds,
// calls and prose, a hundred calls in flight with arguments past their share,
// and a round being written with three abandoned attempts, each of the byte
// that escapes the most.
func worstRunning() toolloop.Result {
	res := worstRecord(false).Result
	for i := range 100 {
		res.Running = append(res.Running, toolloop.RunningCall{ID: named(64), Round: worstRounds,
			Name: fmt.Sprintf("%d%s", i, worst(2*sandbox.MaxBridgeCallNameBytes)), Args: worstArgs(200)})
	}
	res.Partial = &toolloop.Partial{Round: worstRounds + 1, Content: worst(1 << 20)}
	for range 3 {
		res.Partial.Abandoned = append(res.Partial.Abandoned, toolloop.Narration{Round: worstRounds + 1,
			Content: worst(1 << 20)})
	}
	return res
}

// A RUNNING PHASE'S FRAME AT EVERY BOUND STAYS UNDER A MIB. It is republished
// whole five times a second to every dashboard open on its seat, and the
// socket hub queues hundreds for a client that falls behind: past its live
// shares it keeps the newest of what grows — its calls, its prose, its calls
// in flight, the attempts at the round in flight — and its round window, and
// says what it left out.
//
// Mutation: carry the frame's calls, its prose, its calls in flight or its
// abandoned attempts whole, or drop its round window, and the frame is past
// its budget; count nothing it leaves out, and a reader takes the frame for
// the whole phase.
func TestARunningPhasesFrameAtEveryBoundStaysUnderAMiB(t *testing.T) {
	t.Parallel()
	pub := &collector{}
	var mu sync.Mutex
	worstEmitter(pub, &mu).progress(context.Background(), phase.Review, 1, worstRunning(),
		roundCaps{max: worstRounds, ceiling: worstRounds})
	ev, size := published(t, pub)
	if size > liveFrameBytes {
		t.Fatalf("the frame weighs %d bytes, past its %d", size, liveFrameBytes)
	}
	t.Logf("the frame weighs %d bytes, %d short of its budget", size, liveFrameBytes-size)
	frame := *ev.Data.(*types.AgentTurnProgress)
	if frame.CallsElided == 0 || frame.RoundsElided == 0 || frame.RunningElided == 0 || len(frame.ElidedCalls) == 0 {
		t.Errorf("the frame says it left out %d calls (%d tallies), %d rounds and %d calls in flight; want each",
			frame.CallsElided, len(frame.ElidedCalls), frame.RoundsElided, frame.RunningElided)
	}
}

// A RUNNING PHASE'S OPENING FRAME KEEPS ITS PROMPTS TO A RECORD'S SHARE, part by
// part with the outline rewritten to the kept text, as the phase's record does:
// the frame is sent once, and the prompt the model was sent is not changed. The
// user's prompt rides it twice, as its `prompt` and as its second message, so
// the frame is held to three shares and its trigger.
//
// Mutation: send the prompts whole, and a company's document at length puts
// megabytes on a frame every open dashboard is sent.
func TestARunningPhasesOpeningFrameKeepsItsPromptsToTheirShare(t *testing.T) {
	t.Parallel()
	pub := &collector{}
	var mu sync.Mutex
	worstEmitter(pub, &mu).prompted(context.Background(), phase.Review, 1, worstPrompt(1<<20, 8),
		worstPrompt(1<<20, 8), roundCaps{})
	ev, size := published(t, pub)
	if size > 3*promptBytes+64<<10 {
		t.Fatalf("the opening frame weighs %d bytes, past its prompts' shares", size)
	}
	frame := *ev.Data.(*types.AgentTurnProgress)
	for _, m := range frame.PromptMessages {
		if kept.Quoted(m.Content) > promptBytes || len(m.Sections) != 8 {
			t.Errorf("the %s message keeps %d bytes in %d parts, want it within %d, every part kept", m.Role,
				kept.Quoted(m.Content), len(m.Sections), promptBytes)
		}
	}
}

// A RUNNING PHASE'S LIST PAST ITS SHARE KEEPS ITS NEWEST CALLS, none of its
// first: a reader of a running phase watches what arrives, and its record keeps
// how it began once it completes. Every dropped call is counted by tool.
//
// Mutation: keep the record's first half, and a reader watching a long phase
// sees its first calls and not the one it just made.
func TestARunningPhasesListKeepsItsNewestCalls(t *testing.T) {
	t.Parallel()
	var execs []toolloop.Execution
	for i := range 400 {
		execs = append(execs, toolloop.Execution{Round: 1, Name: fmt.Sprintf("tool_%03d", i),
			Args: worstArgs(20), Output: worst(4 << 10)})
	}
	got := keptList(liveExecutions(execs), liveList, nil)
	if got.Elided == 0 || got.Elided+len(got.Rows) != len(execs) || listBytes(got.Rows) > liveListBytes {
		t.Fatalf("the list keeps %d rows (%d bytes) and counts %d, want the newest within %d and the rest counted",
			len(got.Rows), listBytes(got.Rows), got.Elided, liveListBytes)
	}
	if first, last := got.Rows[0]["name"], got.Rows[len(got.Rows)-1]["name"]; last != "tool_399" ||
		first != fmt.Sprintf("tool_%03d", len(execs)-len(got.Rows)) {
		t.Errorf("the list runs %v to %v, want the newest calls through tool_399", first, last)
	}
}

// A RUNNING PHASE'S CALLS IN FLIGHT KEEP THEIR SHARE, each one's arguments held
// to the identifiers a reader scans it for, the first that fit listed and how
// many more were running counted.
//
// Mutation: keep a running call's arguments whole, or every call, and a
// harness's fan-out puts its bodies on every frame.
func TestARunningPhasesCallsInFlightKeepTheirShare(t *testing.T) {
	t.Parallel()
	got, more := keptRunning(runningCalls(worstRunning().Running))
	raw, _ := json.Marshal(got)
	if len(got) == 0 || len(raw) > liveRunningBytes || more == 0 || len(got)+more != 100 {
		t.Errorf("the frame lists %d calls in flight (%d bytes) and counts %d more, want the first within %d and "+
			"the rest counted", len(got), len(raw), more, liveRunningBytes)
	}
	for _, call := range got {
		if kept.Quoted(call.Arguments) > liveRunningArgsBytes {
			t.Errorf("a call in flight keeps %d bytes of arguments, past %d", kept.Quoted(call.Arguments),
				liveRunningArgsBytes)
		}
	}
}

// A ROUND IN FLIGHT KEEPS ITS NEWEST ABANDONED ATTEMPTS' TAILS. A provider that
// gave up partway left text a reader already watched arrive, and the frame keeps
// what it saw last of each of the newest attempts, as it keeps the round's own
// text, the attempts together held to their share.
//
// Mutation: keep each attempt's opening, and the frame shows a reader text it
// scrolled past rather than where the attempt died; keep every attempt, and an
// outage's retries outgrow the share.
func TestARoundInFlightKeepsItsNewestAbandonedAttemptsTails(t *testing.T) {
	t.Parallel()
	attempt := worst(1<<20) + "where it died"
	var abandoned []toolloop.Narration
	for range liveAbandonedAttempts + 2 {
		abandoned = append(abandoned, toolloop.Narration{Round: 2, Content: attempt})
	}
	out := partialRound(&toolloop.Partial{Round: 2, Content: "now", Abandoned: abandoned})
	rows, _ := out["abandoned"].([]types.RoundNarration)
	if len(rows) != liveAbandonedAttempts || listBytes(rows) > liveAbandonedBytes {
		t.Fatalf("the round keeps %d attempts (%d bytes), want the newest %d within %d", len(rows),
			listBytes(rows), liveAbandonedAttempts, liveAbandonedBytes)
	}
	for _, row := range rows {
		if content, _ := row["content"].(string); !strings.HasSuffix(content, "where it died") {
			t.Errorf("an attempt keeps %.40q…, want its tail", content)
		}
	}
}

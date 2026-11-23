package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/tools"
)

// The late call the store hands the job working on its round
// ([sandbox.RunNote.LateCall]), as this node's inbox hands it on. The package
// doc's "A late call is handed to the job working as it lands" says why.

// lateRig is a run whose round a relaunch carried on, on one node's store: the
// first job's session ledger still bound to it, so every call it records lands
// late, for the job working on the round now.
type lateRig struct {
	fleet       *coordmem.Fleet
	store       *sandbox.CoordStore
	runs        *runSteers
	turn        string
	first, next sandbox.PendingRun
	ledger      *bridgeLedger
}

// newLateRig is turn's run, its first job replaced by a relaunch carrying its
// round on.
func newLateRig(t *testing.T, turn string) *lateRig {
	t.Helper()
	return newLateRigOn(t, turn, executor.ClaudeCodeName)
}

// newLateRigOn is [newLateRig] for jobs of the harness named.
func newLateRigOn(t *testing.T, turn, harness string) *lateRig {
	t.Helper()
	fleet := coordmem.NewFleet()
	store := sandbox.NewCoordStore(fleet)
	runs, _ := steersOver(t, steerRing, store)
	r := &lateRig{fleet: fleet, store: store, runs: runs, turn: turn}
	launch := sandbox.PendingRun{TurnID: turn, AgentID: sweAgent, AgentHandle: "swe", Role: "SWE",
		Harness: harness, TraceID: "tr-1", CreatedAt: time.Now().UTC(),
		Steer: sandbox.RunSteer{Seat: runs.anchor(turn, sweAgent, "swe")}}
	var err error
	if r.first, err = store.BeginLaunch(t.Context(), launch, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	r.ledger = &bridgeLedger{store: store, spend: &steppingMeter{}}
	r.ledger.bind(r.first.NamedJob(r.first.OpenedAfter, "", 0))
	launch.Continues = true
	if r.next, err = store.BeginLaunch(t.Context(), launch, sandbox.Fence{}); err != nil {
		t.Fatalf("the relaunch that carries the round on: %v", err)
	}
	if r.next.Round != r.first.Round {
		t.Fatalf("the relaunch works in round %q, want %q", r.next.Round, r.first.Round)
	}
	return r
}

// late records call, the seq'th of the first job's session, through that
// session's ledger: listed late, and handed to the job working on the round.
func (r *lateRig) late(t *testing.T, seq int, call tools.Call) {
	t.Helper()
	started := time.Date(2026, 11, 9, 9, 0, seq, 0, time.UTC)
	if err := r.ledger.Record(t.Context(), r.turn, mcpbridge.Call{Call: call, Seq: seq,
		StartedAt: started}); err != nil {
		t.Fatalf("Record %s: %v", call.Name, err)
	}
}

// items is every late-call item on the rig's record, in its order.
func (r *lateRig) items(t *testing.T) []sandbox.RunNote {
	t.Helper()
	var out []sandbox.RunNote
	for _, n := range mustRow(t, r.store, r.turn).Steer.Notes {
		if n.LateCall != nil {
			out = append(out, n)
		}
	}
	return out
}

// hookAnswer is a Claude Code hook's answer as the harness reads it.
type hookAnswer struct {
	Output struct {
		Context string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
	Line string `json:"systemMessage"`
}

// readHook decodes a hook's answer, failing the case on one that does not.
func readHook(t *testing.T, answer string) hookAnswer {
	t.Helper()
	var hook hookAnswer
	if err := json.Unmarshal([]byte(answer), &hook); err != nil {
		t.Fatalf("the hook's answer does not decode: %v: %q", err, answer)
	}
	return hook
}

// A LATE CALL REACHES THE WORKING JOB AT ITS NEXT BATCH. A call the replaced
// job's session records through its own ledger lands late, and the store hands
// it to the job working on the round; that job's next tool batch is answered
// it, under the engine's heading for a late call, never as anybody's note, its
// outcome before the call, and the hook's line names a late call. The item is
// served to the job's hook, read only once its session shows it took it.
//
// Mutation: render an item as a person's note (item-as-a-note), and the answer
// opens with a note's heading.
func TestALateCallReachesTheWorkingJobAtItsNextBatch(t *testing.T) {
	t.Parallel()
	r := newLateRig(t, "run-1")
	r.late(t, 1, tools.Call{Name: "post_message", Args: map[string]any{"channel": "#eng"}})
	items := r.items(t)
	if len(items) != 1 || items[0].For != r.next.LaunchID {
		t.Fatalf("the record holds items %+v, want the late call's, for the working job", items)
	}
	inbox := boundInbox(t, r.runs, r.store, r.turn, r.next.LaunchID)
	viewHolds(t, inbox, 1)
	answer := inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall())
	hook := readHook(t, answer)
	if !strings.HasPrefix(hook.Output.Context, prompts.LateCallHead+"\n") ||
		strings.Contains(hook.Output.Context, "## A note from") {
		t.Fatalf("the batch was handed %q, want the late call under its own heading, as nobody's note",
			hook.Output.Context)
	}
	outcome := strings.Index(hook.Output.Context, "Outcome: success")
	if call := strings.Index(hook.Output.Context, `Call: post_message({"channel":"#eng"})`); outcome < 0 ||
		call < outcome {
		t.Errorf("the account tells its outcome at %d and its call at %d, want both, the outcome first:\n%s",
			outcome, call, hook.Output.Context)
	}
	if !strings.HasPrefix(hook.Line, "crewlet: a call that finished after its run stopped waiting for it") {
		t.Errorf("the hook's line is %q, want it to name a late call", hook.Line)
	}
	servedNotRead(t, r.store, r.turn, items[0].ID, r.next.LaunchID, takeOf(t, answer))
}

// AN ITEM AT ITS SERVE BOUND IS SAID LEFT TO THE CONTINUATION. A late call's
// item served to the job's hook as often as a note may be, and never shown
// taken, is passed over like a note, and said so once at WARN
// (steer_serve_bound); but no review reads an item, so the line says the next
// launch removes it and its continuation tells the call. NOT PARALLEL: it
// reads the process's log.
//
// Mutation: say an item waits for the review as a note does
// (item-bound-as-a-note), and the line says what never happens.
func TestAnItemAtItsServeBoundIsSaidLeftToTheContinuation(t *testing.T) {
	logs := captureSteerLogs(t)
	ctx := t.Context()
	r := newLateRig(t, "run-1")
	r.late(t, 1, tools.Call{Name: "post_message", Args: map[string]any{"channel": "#eng"}})
	inbox := boundInbox(t, r.runs, r.store, r.turn, r.next.LaunchID)
	viewHolds(t, inbox, 1)
	item := r.items(t)[0].ID
	for k := range sandbox.MaxNoteServes {
		answer := inbox.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
		if answer == "" {
			t.Fatalf("serve %d answered nothing, want the late call", k+1)
		}
		if _, err := r.store.SettleHook(ctx, r.turn, sandbox.HookSettle{Launch: r.next.LaunchID,
			Absent: []string{takeOf(t, answer)}}); err != nil {
			t.Fatal(err)
		}
		viewSettled(t, inbox, item, k+1)
	}
	if got := inbox.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall()); got != "" {
		t.Fatalf("an item at its bound was served again: %q", got)
	}
	said := logs.String()
	if !strings.Contains(said, `"steer_serve_bound"`) ||
		!strings.Contains(said, "the next launch removes it, and its continuation tells the call") ||
		strings.Contains(said, "it waits for the run's review") {
		t.Errorf("an item at its bound is said as:\n%s\nwant it left to the next launch's continuation", said)
	}
}

// viewSettled waits until the inbox's view of the record, which a tool batch is
// served from, shows note id served n times and none of them live. On the
// view's own change signal alone: a view that never settles is the suite's
// timeout, never a deadline a loaded runner can miss (internal/fixedwait).
func viewSettled(t *testing.T, inbox *runInbox, id string, n int) {
	t.Helper()
	for {
		_, watch := inbox.bound()
		view, _, changed := watch.snapshot()
		at := slices.IndexFunc(view.Steer.Notes, func(held sandbox.RunNote) bool { return held.ID == id })
		if at >= 0 {
			if _, live := view.Steer.Notes[at].LiveServe(); !live && len(view.Steer.Notes[at].Serves) == n {
				return
			}
		}
		<-changed
	}
}

// NO LATE CALL IS HANDED AT A CLAUDE CODE FINISH. A job about to finish makes
// no more calls, so the duplicate an item prevents cannot happen in it, and
// its review judges it on the report it gives next, which an item at its
// finish would replace: the finish's hook answers a person's note and leaves
// the item waiting, unserved, for the next launch's continuation to tell.
//
// Mutation: hand items at the finish (items-at-the-finish), and the finish is
// answered the late call.
func TestNoLateCallIsHandedAtAClaudeCodeFinish(t *testing.T) {
	t.Parallel()
	r := newLateRig(t, "run-1")
	r.late(t, 1, tools.Call{Name: "delegate", Args: map[string]any{"to": "qa"}})
	fileSealed(t, r.runs, r.turn, steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, r.runs, r.store, r.turn, r.next.LaunchID)
	hook := readHook(t, inbox.Take(t.Context(), mcpbridge.SteerEventFinish, hookCall()))
	if strings.Contains(hook.Output.Context, prompts.LateCallHead) || !strings.Contains(hook.Output.Context,
		"use staging") {
		t.Errorf("the finish was handed %q, want the person's note and no late call", hook.Output.Context)
	}
	if item := r.items(t)[0]; !item.Waiting() || item.Served != "" {
		t.Errorf("the item holds %+v after the finish, want it waiting, served to nobody", item)
	}
}

// A PERSON'S NOTE IS NEVER LEFT WAITING FOR ITEMS. One hook's answer carries
// what fits ([executor.HookNotesThatFit]): a person's widest note, filed after
// two wide late calls, is fitted first, so it reaches the job at the next
// batch beside the first call, and the second waits; fitted in the order filed,
// the two calls would fill the answer and the person's correction wait behind
// the store's account.
//
// Mutation: fit the candidates in the order filed (fit-in-filing-order), and
// the note is left waiting.
func TestAPersonsNoteIsNeverLeftWaitingForItems(t *testing.T) {
	t.Parallel()
	r := newLateRig(t, "run-1")
	wide := func(name string) tools.Call {
		args := map[string]any{}
		for k := 0; ; k++ {
			args[fmt.Sprintf("k%02d", k)] = strings.Repeat("x", 150)
			msg := lateItemMessage(t.Context(), sandbox.LateCall{Call: sandbox.BridgeCall{Name: name,
				Args: mustJSON(t, args)}}, provenance.NewBoundary())
			if executor.UTF16Len(msg) > 4000 {
				return tools.Call{Name: name, Args: args}
			}
		}
	}
	r.late(t, 1, wide("post_message"))
	r.late(t, 2, wide("create_work_item"))
	note := steer.Note{ID: "n-1", Text: strings.Repeat("\U0001F680", steer.MaxNoteRunes),
		By: strings.Repeat("a", iam.MaxLogin)}
	fileSealed(t, r.runs, r.turn, note)
	// THE PREMISE: the two calls fit one answer together, and either fits
	// beside the note, which never fits beside both.
	widest := executor.UTF16Len(prompts.SteerMessage(note.By, note.Text))
	for _, item := range r.items(t) {
		units := executor.UTF16Len(lateItemMessage(t.Context(), *item.LateCall, provenance.NewBoundary()))
		if 2*units+2 > executor.ClaudeContextBudget || widest+units+2 > executor.ClaudeContextBudget ||
			widest+2*units+4 <= executor.ClaudeContextBudget {
			t.Fatalf("the premise: an item of %d units beside a note of %d does not split the answer as the "+
				"case needs", units, widest)
		}
	}
	inbox := boundInbox(t, r.runs, r.store, r.turn, r.next.LaunchID)
	viewHolds(t, inbox, 3)
	hook := readHook(t, inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall()))
	if !strings.Contains(hook.Output.Context, note.Text) || strings.Count(hook.Output.Context,
		prompts.LateCallHead) != 1 {
		t.Errorf("the batch was handed %d units holding the note %v and %d late calls; want the note and one",
			executor.UTF16Len(hook.Output.Context), strings.Contains(hook.Output.Context, note.Text),
			strings.Count(hook.Output.Context, prompts.LateCallHead))
	}
	if !strings.HasPrefix(hook.Line, "crewlet: a note from "+note.By+" and a call that finished") {
		t.Errorf("the hook's line is %q, want the note named first, then the call", hook.Line)
	}
}

// mustJSON is v as JSON text.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A LATE CALL REACHES AN OPENCODE JOB AT ITS NEXT POLL, AND IS READ ON ITS
// CONFIRMATION. The plugin's poll is answered the item as one message of its
// own, served first, in the engine's words; the plugin admits it and confirms
// it once OpenCode reports it delivered, which records it read by the job and
// the job's session as holding the call ([sandbox.RunSteer.Tellings]).
//
// Mutation: answer a poll a person's notes alone (poll-skips-items), and the
// job is never handed the call; confirm as a step's take
// (confirm-not-a-confirmation), and no telling is recorded.
func TestALateCallReachesAnOpenCodeJobAtItsNextPollAndIsReadOnItsConfirmation(t *testing.T) {
	t.Parallel()
	r := newLateRigOn(t, "run-1", executor.OpenCodeName)
	r.late(t, 1, tools.Call{Name: "delegate", Args: map[string]any{"to": "qa"}})
	item := r.items(t)[0]
	inbox := boundInbox(t, r.runs, r.store, r.turn, r.next.LaunchID)
	viewHolds(t, inbox, 1)
	notes, _ := inbox.Wait(t.Context(), 0)
	if len(notes) != 1 || notes[0].ID != item.ID || !strings.HasPrefix(notes[0].Message, prompts.LateCallHead) ||
		notes[0].Inbox != executor.InboxID(executor.OpenCodeName, r.next.LaunchID, item.ID) {
		t.Fatalf("the poll was answered %+v, want the late call alone, in the engine's words", notes)
	}
	if served := noteOn(t, r.store, r.turn, item.ID); served.Served != r.next.LaunchID || !served.Waiting() {
		t.Fatalf("the item holds %+v once polled, want it served to the job, waiting", served)
	}
	// AND, FOLDED BY A COMPACTION, IT IS ADMITTED AGAIN AS A NOTE IS.
	if granted, _, err := inbox.Again(t.Context(), []string{item.ID}); err != nil ||
		granted[item.ID] != executor.AgainID(executor.OpenCodeName, r.next.LaunchID, item.ID) {
		t.Fatalf("the ask for the item's second admission answered %v, %v, want its job's second id", granted, err)
	}
	if err := inbox.Read(t.Context(), []string{item.ID}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if read := readOn(t.Context(), r.store, r.turn, item.ID); read == nil || read.Launch != r.next.LaunchID {
		t.Errorf("the confirmed item is read %+v, want it read by the job", read)
	}
	if told := mustRow(t, r.store, r.turn).Steer.Tellings; len(told) != 1 || told[0].Launch != r.next.LaunchID ||
		told[0].Appended != item.LateCall.Call.Appended || told[0].Round != item.LateCall.Round {
		t.Errorf("the confirmation recorded tellings %+v, want the job's session holding the call", told)
	}
}

// AN OPENCODE JOB CONFIRMS TWELVE NOTES AND ITEMS IN ONE READ. Its plugin names
// every note it owes in one confirmation, over the bridge's notes route, and
// one job can be owed a person's notes and its own items at once: the route
// takes all twelve (the bridge's bound is what one job can be owed), and every
// one is read.
//
// Mutation: hold the route to a person's ten (read-bound-at-ten), and the
// confirmation is refused, every note left waiting.
func TestAnOpenCodeJobConfirmsTwelveNotesAndItemsInOneRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r := newLateRig(t, "run-1")
	for seq := 1; seq <= 7; seq++ {
		r.late(t, seq, tools.Call{Name: fmt.Sprintf("delegate_%d", seq)})
	}
	var ids []string
	for _, item := range r.items(t) {
		ids = append(ids, item.ID)
	}
	for k := range steer.MaxPendingNotes {
		id := fmt.Sprintf("n-%d", k)
		fileSealed(t, r.runs, r.turn, steer.Note{ID: id, Text: "note " + id, By: "founder"})
		ids = append(ids, id)
	}
	if len(ids) != 12 {
		t.Fatalf("the premise: the job is owed %d notes and items, want twelve", len(ids))
	}
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.SteerPattern, bridge.SteerHandler())
	server.Config.Handler = mux
	company := specCompany(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - {name: SWE, handle: swe, llm: zulu}
`)
	swe := company.Org.AgentSeatByHandle("swe")
	surface := tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).
		ForTurn(&turnctx.Turn{RunID: r.turn, Seat: swe, Org: company.Org})
	inbox := boundInbox(t, r.runs, r.store, r.turn, r.next.LaunchID)
	endpoint := bridge.Open(ctx, &mcpbridge.Session{RunID: r.turn, Launch: r.next.LaunchID, Handle: "swe",
		Role: swe.Name, Surface: surface, Steer: inbox})
	if endpoint == "" {
		t.Fatal("the bridge opened no session for the job")
	}
	viewHolds(t, inbox, 12)
	if notes, _ := inbox.Wait(ctx, 0); len(notes) != 12 {
		t.Fatalf("the poll was answered %d notes and items, want all twelve", len(notes))
	}
	if status := confirmOn(t, server.Client(), mcpbridge.SteerURL(endpoint), ids...); status != http.StatusNoContent {
		t.Fatalf("a confirmation of twelve was answered %d, want it recorded: 204", status)
	}
	for _, id := range ids {
		if read := readOn(ctx, r.store, r.turn, id); read == nil || read.Launch != r.next.LaunchID {
			t.Errorf("%s is read %+v after the confirmation, want it read by the job", id, read)
		}
	}
}

// A LATE CALL ON ANOTHER NODE REACHES THE JOB THERE. The item is on the run's
// record, which every node reads: the node whose store took the late call need
// not be the one serving the working job's bridge, and the job is handed the
// call by whichever node serves it.
//
// Mutation: file the item for the job that made the call (item-for-the-caller),
// and the node serving the working job hands it nothing.
func TestALateCallOnAnotherNodeReachesTheJobThere(t *testing.T) {
	t.Parallel()
	r := newLateRig(t, "run-1")
	r.late(t, 1, tools.Call{Name: "delegate", Args: map[string]any{"to": "qa"}})
	// ANOTHER NODE: its own store and steers over the same coordination store.
	other := sandbox.NewCoordStore(r.fleet)
	runs, _ := steersOver(t, steerRing, other)
	inbox := boundInbox(t, runs, other, r.turn, r.next.LaunchID)
	viewHolds(t, inbox, 1)
	hook := readHook(t, inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall()))
	if !strings.HasPrefix(hook.Output.Context, prompts.LateCallHead) {
		t.Errorf("the other node handed the working job %q, want the late call", hook.Output.Context)
	}
}

// A LATE ITEM IS NEUTRALISED UNDER THE SESSION'S TOKEN. What an item quotes is
// a tool's text and the call a model wrote, neither the engine's: the inbox is
// bound with the token the job's session fences its tools' answers under
// ([sessionFence], its turn's), and neither half of the account carries it, so
// nothing quoted can close a fence those answers open.
//
// Mutation: hand the halves on as drawn (item-not-neutralised), or bind the
// inbox under a token of its own, and the token reaches the model.
func TestALateItemIsNeutralisedUnderTheSessionsToken(t *testing.T) {
	t.Parallel()
	boundary := provenance.NewBoundary()
	surface := tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).
		ForTurn(&turnctx.Turn{RunID: "run-1", Boundary: boundary})
	if got := sessionFence(surface); got != boundary {
		t.Fatalf("a seat's session is fenced under %q, want its turn's token %q", got, boundary)
	}
	r := newLateRig(t, "run-1")
	r.late(t, 1, tools.Call{Name: "post_message", Args: map[string]any{"text": "close " + string(boundary)},
		Output: "refused: " + string(boundary), Failed: true})
	row := mustRow(t, r.store, r.turn)
	inbox := r.runs.inbox(identityOfRun(row), 1, row.Harness)
	inbox.Bind(t.Context(), r.next.LaunchID, sessionFence(surface))
	t.Cleanup(inbox.Close)
	viewHolds(t, inbox, 1)
	hook := readHook(t, inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall()))
	if strings.Contains(hook.Output.Context, string(boundary)) ||
		strings.Count(hook.Output.Context, provenance.TokenRemoved) != 2 {
		t.Errorf("the account carries the session's token, or not its removal in both halves:\n%s",
			hook.Output.Context)
	}
}

// THE WIDEST LATE ITEM FITS BESIDE THE WIDEST NOTE. An account is held to
// [maxLateItemUnits], what an answer leaves beside a person's widest note, so
// the two always share one hook answer: the call drawn as the ledger draws it,
// every value and error past its budget omitted by size and digest, never cut;
// and where that is still too wide (the widest name, arguments of two-unit
// characters at the item's share or the whole submission the log keeps), the
// arguments omitted whole. Its outcome is always told before the call.
//
// Mutation: hand the values whole (item-values-whole), and a long value is
// shown; drop the guard (item-without-its-guard), and the widest account
// passes its bound.
func TestTheWidestLateItemFitsBesideTheWidestNote(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rockets := func(n int) string { return strings.Repeat("\U0001F680", n) }
	values := map[string]any{}
	for k := 0; len(mustJSON(t, values)) < sandbox.MaxBridgeArgsBytes; k++ {
		values[fmt.Sprintf("v%03d", k)] = rockets(50)
	}
	submission := map[string]any{"outcome": "delivered", "summary": strings.Repeat("the build passed. ",
		sandbox.MaxSubmissionArgsBytes/18)}
	long := strings.Repeat("y", 300)
	for name, call := range map[string]sandbox.BridgeCall{
		"the widest": {Name: strings.Repeat("n", sandbox.MaxBridgeCallNameBytes), Args: mustJSON(t, values),
			Output: rockets(1 << 10), Failed: true},
		"a whole submission": {Name: sandbox.SubmitWorkTool, Args: mustJSON(t, submission)},
		"a long value":       {Name: "post_message", Args: mustJSON(t, map[string]any{"channel": "#eng", "text": long})},
	} {
		msg := lateItemMessage(ctx, sandbox.LateCall{Call: call}, provenance.NewBoundary())
		if units := executor.UTF16Len(msg); units > maxLateItemUnits ||
			executor.HookNotesThatFit(executor.ClaudeCodeName, []string{widestNote(), msg}) != 2 {
			t.Errorf("%s: the account is %d units, past the %d beside the widest note", name, units, maxLateItemUnits)
		}
		if outcome, told := strings.Index(msg, "\nOutcome: "), strings.Index(msg, "\nCall: "); outcome < 0 ||
			told < outcome {
			t.Errorf("%s: the outcome is told at %d and the call at %d, want the outcome first", name, outcome, told)
		}
		t.Logf("%s: %d units of %d", name, executor.UTF16Len(msg), maxLateItemUnits)
	}
	msg := lateItemMessage(ctx, sandbox.LateCall{Call: sandbox.BridgeCall{Name: "post_message",
		Args: mustJSON(t, map[string]any{"channel": "#eng", "text": long})}}, provenance.NewBoundary())
	if !strings.Contains(msg, compact.Omitted(long)) || strings.Contains(msg, long[:250]) ||
		!strings.Contains(msg, `"channel":"#eng"`) {
		t.Errorf("a value past the ledger's budget is not omitted by size and digest beside the short one:\n%s", msg)
	}
	if executor.UTF16Len(widestNote()) <= 2*steer.MaxNoteRunes {
		t.Errorf("the widest note is %d units, under its text's own %d", executor.UTF16Len(widestNote()),
			2*steer.MaxNoteRunes)
	}
}

// AN ITEM OF A KIND THIS BUILD DOES NOT KNOW REACHES NO MODEL. A late call is
// handed only as the store files it: for one job, unsealed. A note claiming a
// call with a seal, or for no job, and an unsealed note of a kind a later
// build files (a key this build does not know), are handed to nobody — never
// opened as a person's note, never rendered as a late call — and each is said
// once, at ERROR.
//
// Mutation: hand a sealed note claiming a call (sealed-call-handed), and the
// model is told a call nobody's store filed.
func TestAnItemOfAKindThisBuildDoesNotKnowReachesNoModel(t *testing.T) {
	logs := captureSteerLogs(t)
	r := newLateRig(t, "run-1")
	job := r.next.LaunchID
	call := &sandbox.LateCall{Round: r.next.Round, Call: sandbox.BridgeCall{Name: "delegate", Seq: 1, Appended: 1}}
	sealed, err := r.runs.seal(r.turn, steer.Note{ID: "sealed-call", Text: "x", By: "founder"})
	if err != nil {
		t.Fatal(err)
	}
	sealed.For, sealed.LateCall = job, call
	unknown := sandbox.RunNote{ID: "later-kind", For: job,
		Extra: map[string]json.RawMessage{"later_kind": json.RawMessage(`{"x":1}`)}}
	for _, n := range []sandbox.RunNote{sealed, {ID: "no-job", LateCall: call}, unknown} {
		putNote(t, r.fleet, r.turn, n)
	}
	inbox := boundInbox(t, r.runs, r.store, r.turn, job)
	// THE VIEW HAS READ THE RECORD, every note on it routed, and none opened.
	viewHolds(t, inbox, 0)
	if got := inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall()); got != "" {
		t.Errorf("the batch was answered %q, want nothing", got)
	}
	if notes, _ := inbox.Wait(t.Context(), 0); len(notes) != 0 {
		t.Errorf("the poll was answered %+v, want nothing", notes)
	}
	inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall())
	said := map[string]int{}
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if !strings.Contains(line, `"steer_note_unverified"`) || !strings.Contains(line, `"level":"ERROR"`) {
			continue
		}
		for _, id := range []string{"sealed-call", "no-job", "later-kind"} {
			if strings.Contains(line, `"note_id":"`+id+`"`) {
				said[id]++
			}
		}
	}
	for _, id := range []string{"sealed-call", "no-job", "later-kind"} {
		if said[id] != 1 {
			t.Errorf("%s was said %d times at ERROR, want once", id, said[id])
		}
	}
}

// putNote puts n on turnID's record as given, waiting and numbered after every
// note the record holds: onto the records the store is built on, the write any
// party that reaches the coordination store can make.
func putNote(t *testing.T, records coord.SandboxRuns, turnID string, n sandbox.RunNote) {
	t.Helper()
	if _, ok, err := records.MutateSandboxRun(t.Context(), turnID, func(current coord.Record) (coord.SandboxRunChange, error) {
		var row sandbox.PendingRun
		if err := json.Unmarshal(current.Value, &row); err != nil {
			return coord.SandboxRunChange{}, err
		}
		row.Steer.Seq++
		n.Seq = row.Steer.Seq
		row.Steer.Notes = append(row.Steer.Notes, n)
		raw, err := json.Marshal(row)
		return coord.SandboxRunChange{Put: raw}, err
	}); err != nil || !ok {
		t.Fatalf("put note %s on %s: %v %v", n.ID, turnID, ok, err)
	}
}

// THE FILER'S STATUSES ARE THE INBOX'S. The store files a late call for a job
// only in the statuses its harness takes notes in ([sandbox.ItemStatuses]),
// which are the inbox's ([steerTakeStatuses]): an item filed in any other is
// one no reader of the job takes, and one refused in a status the inbox serves
// is a call the job is never handed as it lands.
//
// Mutation: add a status to either list (filer-files-while-awaiting), and they
// differ.
func TestTheFilersStatusesAreTheInboxs(t *testing.T) {
	t.Parallel()
	if !slices.Equal(sandbox.ItemStatuses, steerTakeStatuses) {
		t.Errorf("the store files late calls in %v, and the inbox takes notes in %v", sandbox.ItemStatuses,
			steerTakeStatuses)
	}
}

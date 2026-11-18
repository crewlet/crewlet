package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// A CALL STILL IN FLIGHT IS COUNTED BY ITS IDENTITY, ON EVERY PATH A RUN'S
// CALLS ARE READ BACK ON. The bridge writes a call's entry before it runs, so
// a call whose update never landed is on the record by that entry alone
// (runner.Resume.Unfinished). Collection, continuation after an answer and
// reopening elsewhere count it as the unparked run did, so a later, different
// call never reuses its operation id. A closed round's record keeps it, so a
// later round counts it too; one with no digest (a tool that may ask a person
// first) counts as its tool with unknown arguments, never with none.
//
// Mutation: leave the calls in flight out of the seed, and the call in
// flight's operation id is derived again; seed a closed round's digestless
// call by its arguments, and it counts as the tool called with none.
func TestACallStillInFlightIsCountedByItsIdentityOnEveryPath(t *testing.T) {
	t.Parallel()
	register := func(t *testing.T, reg *tools.Registry) {
		if err := reg.RegisterWith(&statusWriter{}, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
			t.Fatal(err)
		}
	}
	inFlight := []runner.BridgedCall{{Call: ledger.Call{Name: "set_status", Result: runner.UnfinishedNote,
		Failed: true, Digest: turnctx.ArgsDigest(published), Unfinished: true}, Seq: 1}}
	state := execstate.State{Version: execstate.Version, Round: 1, ActiveTools: []string{"set_status"}}
	// THE RUN THAT NEVER PARKED made `published`, still in flight, then makes
	// `noted`: one different call before it.
	check := func(t *testing.T, calls *turnctx.CallLog) {
		t.Helper()
		if got := calls.Ordinal("set_status", noted); got != 1 {
			t.Errorf("the segment counts %d different calls before its next one, want the call in flight, 1", got)
		}
	}

	t.Run("collected", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register,
			resume: &runner.Resume{State: state, Answer: "done", Unfinished: inFlight},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		check(t, calls)
	})
	t.Run("continued", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
			calls: calls, register: register, launcher: &playLauncher{},
			resume: &runner.Resume{State: state, Unfinished: inFlight, Asked: &runner.Asked{
				Question: "which item?", Answer: "GP-12", SessionID: "s-1", BoxKept: true}},
		})
		if _, _, err := r.Resume(context.Background(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		check(t, calls)
	})
	t.Run("reopened", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{calls: calls, register: register})
		if _, err := r.ReopenAgentRun(context.Background(), state, nil, inFlight, nil, nil, runner.Bridged{},
			false); err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		check(t, calls)
	})
	t.Run("a closed round, its entry with no digest", func(t *testing.T) {
		t.Parallel()
		calls := turnctx.NewCallLog()
		closed := state
		closed.Round = 2
		closed.Iterations = []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{{Name: "set_status",
			Result: runner.UnfinishedNote, Failed: true, Unfinished: true}}}}
		r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
			buildOpts{calls: calls, register: register})
		if _, err := r.ReopenAgentRun(context.Background(), closed, nil, nil, nil, nil, runner.Bridged{},
			false); err != nil {
			t.Fatalf("ReopenAgentRun: %v", err)
		}
		if got := calls.Ordinal("set_status", map[string]any{}); got != 1 {
			t.Errorf("the segment counts %d different calls before one with no arguments, want the call whose "+
				"arguments are not known, 1", got)
		}
	})
}

// A COLLECTED ROUND'S RECORD LISTS A CALL WHOSE OUTCOME IS UNKNOWN AS ONE:
// after the round's calls, where the reviewer and the turn's tool log read
// it, FAILED so no delivery check, citation or submission takes it as done,
// and answered in words that say its effects may stand, never as a plain
// failure, which a run makes again.
//
// Mutation: leave the unfinished calls out of the round's record, and the
// reviewer and the turn's tool log never see the call.
func TestACollectedRoundListsACallWhoseOutcomeIsUnknown(t *testing.T) {
	t.Parallel()
	register := func(t *testing.T, reg *tools.Registry) {
		if err := reg.RegisterWith(&statusWriter{}, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}, buildOpts{
		calls: turnctx.NewCallLog(), register: register,
		resume: &runner.Resume{
			State:  execstate.State{Version: execstate.Version, Round: 1, ActiveTools: []string{"set_status"}},
			Answer: "done",
			Unfinished: []runner.BridgedCall{{Call: ledger.Call{Name: "set_status", Result: runner.UnfinishedNote,
				Failed: true, Digest: turnctx.ArgsDigest(published), Unfinished: true}, Seq: 1}},
		},
	})
	work, _, err := r.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(work.Calls) != 1 {
		t.Fatalf("the round's record lists %+v, want the call whose outcome is unknown", work.Calls)
	}
	if c := work.Calls[0]; c.Name != "set_status" || !c.Failed || !c.Unfinished || c.Result != runner.UnfinishedNote {
		t.Errorf("the round's record lists %+v, want the call failed and marked unfinished, in the words "+
			"that say its outcome is unknown", c)
	}
}

// AN UNFINISHED CALL IS TOLD IT MAY HAVE TAKEN EFFECT. Carried on failed (no
// delivery or submission) and answering runner.UnfinishedNote, it is told to a
// run continuing its kept session apart from the calls after the save, in the
// late calls' section, whatever the session's end. That section never says
// the call finished, and excepts it, in its line's words, from "a call marked
// `→ error` did not take effect": it may have, so it is checked before being
// made again rather than duplicated.
//
// Mutation: word the section as saying its calls finished or that an error
// did not take effect, tell the call among the calls after the save, leave it
// out of the brief, or word the note without saying its outcome is unknown,
// and a case goes red.
func TestAnUnfinishedCallIsToldItMayHaveTakenEffect(t *testing.T) {
	t.Parallel()
	const (
		lateHeading = "## Calls the run that made them stopped waiting for, or whose outcome is unknown"
		excepted    = "one marked `→ error` did not take effect, unless it says its outcome is unknown: that one" +
			" may have, so check whether it did before making it again."
	)
	unfinished := runner.BridgedCall{Call: ledger.Call{Name: "set_status", Result: runner.UnfinishedNote,
		Failed: true, Digest: turnctx.ArgsDigest(published), Unfinished: true}, Seq: 3}
	bridged := []runner.BridgedCall{
		numbered("slack_post", map[string]any{"text": "the held one"}, 1, 1),
		numbered("slack_post", map[string]any{"text": "the ordinary one"}, 2, 2),
	}
	for _, said := range []string{"outcome is unknown", "may have taken effect",
		"Check whether it did before making it again."} {
		if !strings.Contains(runner.UnfinishedNote, said) {
			t.Errorf("the line of a call whose outcome is unknown does not say %q, which the late calls' words "+
				"go by: %q", said, runner.UnfinishedNote)
		}
	}
	for _, interrupted := range []bool{false, true} {
		basis := keptBasis
		basis.Through, basis.Interrupted = 1, interrupted
		brief := lostRelaunch(t, basis, runner.Resume{Bridged: bridged,
			Unfinished: []runner.BridgedCall{unfinished}}).Session.Brief
		since, apart, found := strings.Cut(brief, lateHeading)
		if !found {
			t.Fatalf("a session (cut off: %v) is told no call under the late calls' words:\n%s", interrupted, brief)
		}
		if !strings.Contains(since, "the ordinary one") || strings.Contains(since, "set_status") {
			t.Errorf("a session (cut off: %v) is not told the call after its save apart from the call whose "+
				"outcome is unknown:\n%s", interrupted, brief)
		}
		words, listed, _ := strings.Cut(apart, "\n- ")
		if call, _, _ := strings.Cut(listed, "\n"); !strings.HasPrefix(call, "set_status(") ||
			!strings.HasSuffix(call, ") → error: "+runner.UnfinishedNote) {
			t.Errorf("a session (cut off: %v) is not told the call whose outcome is unknown as its line says "+
				"it:\n%s", interrupted, brief)
		}
		notDone := strings.NewReplacer("before it finished", "", "before they finished", "").Replace(words)
		if strings.Contains(notDone, "finished") || !strings.Contains(words, excepted) ||
			strings.Count(words, "did not take effect") != 1 {
			t.Errorf("a session (cut off: %v) is told the call whose outcome is unknown under words that say "+
				"it finished, or did not take effect:\n%s", interrupted, words)
		}
	}
}

// EVERY READER OF A ROUND IS TOLD A CALL WHOSE OUTCOME IS UNKNOWN MAY HAVE
// TAKEN EFFECT. Such a call is on the round's record, failed, answering
// runner.UnfinishedNote, and each reader reads it under a rule for `→ error`:
// the reviewer and, once it sends the round back, the next round's fresh
// brief (the prior-work ledger) and the send-back continuing the session (the
// late calls). Each rule excepts it from "did not take effect", as its line
// does; otherwise a reviewer sends it back to retry, and a run posts or
// delegates twice.
//
// Mutation: drop the exception from the reviewer's rule, the prior-work
// ledger's or a send-back's, and its reader goes red; leave the call out of
// the round's record, and its reviewer and the next round's fresh brief do.
func TestEveryReaderOfARoundIsToldACallWhoseOutcomeIsUnknownMayHaveTakenEffect(t *testing.T) {
	t.Parallel()
	const excepted = "did not take effect, unless it says its outcome is unknown: that one may have"
	// ITS ARGUMENTS SAID TO BE KEPT ON NO RECORD, never drawn `set_status()`,
	// the tool called with none (ledger.Call.Unfinished).
	line := "- set_status(arguments kept on no record) → error: " + runner.UnfinishedNote
	// rules is the section of text under heading, up to the next heading.
	rules := func(t *testing.T, reader, text, heading string) string {
		t.Helper()
		_, under, found := strings.Cut(text, heading)
		if !found {
			t.Fatalf("%s is told no %q:\n%s", reader, heading, text)
		}
		if end := strings.Index(under, "\n## "); end >= 0 {
			under = under[:end]
		}
		return under
	}
	// excepts checks that every "did not take effect" a section says is excepted.
	excepts := func(t *testing.T, reader, section string) {
		t.Helper()
		if said := strings.Count(section, "did not take effect"); said == 0 || said != strings.Count(section,
			excepted) {
			t.Errorf("%s is told the call whose outcome is unknown under a rule that says a failed call did not "+
				"take effect without excepting it:\n%s", reader, section)
		}
	}

	prov := &scriptedProvider{review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool,
		`{"decision":"done"}`)}}
	entries := []phase.Entry{{Key: "default", Provider: prov}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	relauncher := &probingLauncher{basis: keptBasis}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Answer: "done", Unfinished: []runner.BridgedCall{{
			Call: ledger.Call{Name: "set_status", Result: runner.UnfinishedNote, Failed: true,
				Digest: turnctx.ArgsDigest(published), Unfinished: true}, Seq: 1}}}})
	work, _, err := collector.Resume(t.Context(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if _, err := collector.Review(t.Context(), 1, work, nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	review := prov.requestsFor("review")[0].Messages[0].Content
	if !strings.Contains(review, line) {
		t.Errorf("the round's reviewer is not told the call whose outcome is unknown as its line says it:\n%s",
			review)
	}
	excepts(t, "the round's reviewer", rules(t, "the round's reviewer", review, "## REVIEW phase"))

	history := []ledger.Iteration{{Iteration: 1, Calls: work.Calls}}
	if _, _, err := collector.Execute(t.Context(), 2, "set the status only once it is known", history); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	again := relauncher.last(t)
	ledgered := rules(t, "the next round's fresh brief", again.Brief, "## Already done earlier in this turn")
	if !strings.Contains(ledgered, line) {
		t.Errorf("the next round's fresh brief does not list the call whose outcome is unknown as its line says "+
			"it:\n%s", again.Brief)
	}
	excepts(t, "the next round's fresh brief", ledgered)
	if again.Session == nil {
		t.Fatal("the send-back continues no kept session")
	}
	if !strings.Contains(again.Session.Brief, line) {
		t.Errorf("the send-back continuing its session is not told the call whose outcome is unknown as its "+
			"line says it:\n%s", again.Session.Brief)
	}
	excepts(t, "the send-back continuing its session", rules(t, "the send-back continuing its session",
		again.Session.Brief, "## Already done earlier in this turn"))
}

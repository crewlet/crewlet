package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// withSandbox registers the run_sandbox stand-in as a first-party tool, on the
// executor's surface from its first round.
func withSandbox(t *testing.T, reg *tools.Registry) {
	t.Helper()
	if err := reg.Register(suspendingTool{}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// A ROUND THAT CALLS SOMETHING BESIDE ITS SANDBOX RUN PARKS, and the resume
// picks the phase up from exactly there.
//
// A detached run is called alone, AFTER every other call its response made, so
// the parked conversation ends with that response, its siblings answered and
// the run the one call left open — the shape a resume re-enters. It could not
// park at all when the loop drove each round itself: the run stopped the round
// before the calls after it ran, two calls were left open, and the suspension
// was refused as one nothing could resume.
//
// THREE CLIS: one that makes the response's calls one after another in the
// order the model wrote them, one that makes them at once, and one that made
// the run first and so never makes the call after it — whose call the record
// answers as never made, so that one too is no second open call.
func TestARoundThatCallsSomethingBesideItsSandboxRunParks(t *testing.T) {
	t.Parallel()
	sibling := llmtest.Call{Name: "lookup_colleague", Args: `{"name":"ana"}`}
	run := llmtest.Call{Name: toolname.RunSandbox, Args: `{"task":"fix the flake"}`}
	for _, tc := range []struct {
		name     string
		response llmtest.Response
		// ran says the sibling reached the surface, before the run.
		ran bool
	}{
		{name: "the sibling first, one at a time",
			response: llmtest.Response{Calls: []llmtest.Call{sibling, run}}, ran: true},
		{name: "both at once",
			response: llmtest.Response{Calls: []llmtest.Call{run, sibling}, Parallel: true}, ran: true},
		{name: "the run first, one at a time",
			response: llmtest.Response{Calls: []llmtest.Call{run, sibling}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate := &org.RoleSandbox{Enabled: true}
			prov := &scriptedProvider{execute: []llmtest.Response{tc.response}}
			r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
				buildOpts{sandbox: gate, register: withSandbox})
			w, _, err := r.Execute(context.Background(), 1, "", nil)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !w.Suspended {
				t.Fatalf("the phase did not park: %+v", w)
			}
			parked, ok := r.Suspended()
			if !ok {
				t.Fatal("no suspension was recorded: the state failed its own check, so " +
					"nothing would collect the run")
			}
			state := parked.State
			if state.PendingCallName != toolname.RunSandbox {
				t.Fatalf("parked on %q, want the sandbox run", state.PendingCallName)
			}
			names := callNames(w.Calls)
			if got := slices.Contains(names, "lookup_colleague"); got != tc.ran {
				t.Fatalf("the surface ran %v; the sibling ran = %v, want %v", names, got, tc.ran)
			}
			if tc.ran && slices.Index(names, "lookup_colleague") > slices.Index(names, toolname.RunSandbox) {
				t.Errorf("the surface ran %v: the run went before the call beside it", names)
			}
			// THE SIBLING IS ANSWERED, the run is not.
			answers := map[string]string{}
			for _, m := range state.Messages {
				if m.Role == llm.RoleTool {
					answers[m.Name] = m.Content
				}
			}
			if _, open := answers[toolname.RunSandbox]; open {
				t.Error("the parked run was answered, so there is nothing to resume into")
			}
			if _, done := answers["lookup_colleague"]; !done {
				t.Errorf("the call beside the run is unanswered: %+v", state.Messages)
			}

			// THROUGH THE ROW, as the engine persists it and another process
			// reads it back.
			blob, err := execstate.Encode(state)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			back, found, err := execstate.Decode(blob)
			if err != nil || !found {
				t.Fatalf("Decode = %v, %v", found, err)
			}

			// THE RESUME ANSWERS THE RUN AND THE NEXT CLI RUN IS HANDED THE
			// WHOLE TRANSCRIPT: the response that made both calls, both
			// answers, the run's last.
			next := &scriptedProvider{execute: []llmtest.Response{submitWork(t)}}
			resumed, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: next}}, buildOpts{
				sandbox: gate, register: withSandbox,
				resume: &runner.Resume{State: back, Answer: "the run fixed the flake"},
			})
			done, _, err := resumed.Resume(context.Background(), nil)
			if err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if done.Outcome != turn.OutcomeBlocked || done.Rescued {
				t.Errorf("the resumed phase ended %s (rescued %v), want its submission", done.Outcome, done.Rescued)
			}
			sent := next.requestsFor("execute")
			if len(sent) == 0 {
				t.Fatal("the resumed phase started no CLI run")
			}
			transcript := sent[0].Messages
			last := transcript[len(transcript)-1]
			if last.Role != llm.RoleTool || last.Name != toolname.RunSandbox ||
				!strings.Contains(last.Content, "fixed the flake") {
				t.Errorf("the resumed run's transcript ends on %+v, want the run's answer", last)
			}
			calls := 0
			for _, m := range transcript {
				for _, c := range m.ToolCalls {
					if c.Name == toolname.RunSandbox || c.Name == "lookup_colleague" {
						calls++
					}
				}
			}
			if calls != 2 {
				t.Errorf("the resumed run's transcript carries %d of the parked response's two calls", calls)
			}
		})
	}
}

// A PARKED PHASE'S CLI RUNS AND CALL IDS SURVIVE THE RESUME.
//
// The resumed phase's record is the only account the phase will ever have —
// a suspending phase publishes no record of its own — so what the half before
// the park did has to come back off the row: the run it was made in, and each
// call's id, which pairs the call's row with the call the conversation made.
//
// Mutation: drop the invocations from the parked state, or the call id from
// the rows read back, and the resumed record loses them.
func TestAParkedPhasesRunsAndCallIDsSurviveTheResume(t *testing.T) {
	t.Parallel()
	gate := &org.RoleSandbox{Enabled: true}
	prov := &scriptedProvider{execute: []llmtest.Response{
		calling(llmtest.Call{Name: "lookup_colleague", Args: `{"name":"ana"}`}),
		calling(llmtest.Call{Name: toolname.RunSandbox, Args: `{"task":"fix the flake"}`}),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{sandbox: gate, register: withSandbox})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parked, ok := r.Suspended()
	if !ok {
		t.Fatal("no suspension was recorded")
	}
	blob, err := execstate.Encode(parked.State)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	state, _, err := execstate.Decode(blob)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(state.Invocations) != 1 {
		t.Fatalf("the parked row carries %d CLI runs, want the one that parked", len(state.Invocations))
	}

	pub := newCapture()
	next := &scriptedProvider{execute: []llmtest.Response{submitWork(t)}}
	resumed, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: next}}, buildOpts{
		pub: pub, sandbox: gate, register: withSandbox,
		resume: &runner.Resume{State: state, Answer: "the run fixed the flake"},
	})
	if _, _, err := resumed.Resume(context.Background(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	done := completedPhase(t, pub, "execute")
	if len(done.Invocations) != 2 {
		t.Errorf("the record carries %d CLI runs, want the parked one and the resumed one: %+v",
			len(done.Invocations), done.Invocations)
	}
	ids := map[string]string{}
	for _, m := range state.Messages {
		for _, c := range m.ToolCalls {
			ids[c.Name] = c.ID
		}
	}
	if ids["lookup_colleague"] == "" || ids[toolname.RunSandbox] == "" {
		t.Fatalf("the parked conversation names its calls %v, so this case holds nothing", ids)
	}
	checked := 0
	for _, ex := range done.ToolExecutions {
		name, _ := ex["name"].(string)
		if want, made := ids[name]; made {
			checked++
			if ex["call_id"] != want {
				t.Errorf("%s's row carries call_id %v, want the conversation's %q", name, ex["call_id"], want)
			}
		}
	}
	if checked == 0 {
		t.Error("the record carries none of the parked half's calls, so no id was checked")
	}
}

// THE CALL THAT PARKED THE PHASE IS ON ITS RECORD. It ran — it launched the
// box the resume collected — so it is an execution like any other, with what
// it returned and its id; only its ANSWER is left open in the conversation.
// Without the row, the one durable account of the phase says nothing started a
// coding run, and a resumed run's call log, seeded from the parked rows,
// counts no earlier launch.
func TestTheCallThatParkedThePhaseIsOnTheResumedRecord(t *testing.T) {
	t.Parallel()
	gate := &org.RoleSandbox{Enabled: true}
	prov := &scriptedProvider{execute: []llmtest.Response{
		calling(llmtest.Call{Name: toolname.RunSandbox, Args: `{"task":"fix the flake"}`}),
	}}
	r, _ := buildWith(t, []phase.Entry{{Key: "default", Provider: prov}},
		buildOpts{sandbox: gate, register: withSandbox})
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	parked, ok := r.Suspended()
	if !ok {
		t.Fatal("no suspension was recorded")
	}
	var row map[string]any
	for _, ex := range parked.State.ToolExecutions {
		if ex["name"] == toolname.RunSandbox {
			row = ex
		}
	}
	if row == nil {
		t.Fatalf("the parked row's executions %v do not carry the call that parked the phase",
			parked.State.ToolExecutions)
	}
	if row["call_id"] != parked.State.PendingCallID {
		t.Errorf("the parked call's row carries call_id %v, want the pending call's %q",
			row["call_id"], parked.State.PendingCallID)
	}
}

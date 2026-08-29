package learning_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT THIS WORKER IS FOR.
//
// Every reader of a synthesized skill shipped before anything wrote one:
// use_skill, the Plan-phase catalogue, refine_skill, the curator's ageing
// pass, the health rollup. All of them ran correctly over a permanently empty
// table, which is why nothing went red. These cases are about the producer.

// synthesizer builds one whose model answers with a fixed draft, reusing this
// package's own auxProvider and stubModels rather than a second pair of twins.
func synthesizer(t *testing.T, db *store.DB, answer string, opts learning.SynthesizerOptions) *learning.Synthesizer {
	t.Helper()
	s, _ := synthesizerWith(t, db, &auxProvider{replies: []llm.Completion{{Content: answer}}}, opts)
	return s
}

func synthesizerWith(t *testing.T, db *store.DB, p *auxProvider,
	opts learning.SynthesizerOptions,
) (*learning.Synthesizer, *auxProvider) {
	t.Helper()
	s, err := learning.NewSynthesizer(&stubModels{p: p}, learning.NewSkills(db), opts)
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
	return s, p
}

// newStore is this package's external-test store opener, spelled through the
// one onboarding_test.go already provides.
func newStore(t *testing.T) *store.DB {
	t.Helper()
	return openStore(t, filepath.Join(t.TempDir(), "synth.db"))
}

// toolTurn is a settled turn that called n distinct tools.
func toolTurn(tools ...string) learning.Turn {
	return learning.Turn{
		Role: &org.Role{Name: "Dev"},
		Event: types.TurnCompleted{
			Agent: "agent-uuid", AgentHandle: "dev", RoleName: "Dev", TurnID: "t1",
			ToolSequence: tools, ReviewOutcome: "done",
			TaskSummary: "ship the release", PlanSummary: "cut, tag, announce",
		},
	}
}

const goodDraft = `{"name":"cut-a-release","description":"Ship a tagged release",` +
	`"content":"1. run the pipeline\n2. tag\n3. announce"}`

// A TURN THAT DID REAL WORK PRODUCES A SKILL — the case whose absence made
// the whole downstream subsystem dead.
func TestATurnWithAProcedureIsDistilledIntoASkill(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	s := synthesizer(t, db, goodDraft, learning.SynthesizerOptions{MinToolCalls: 3})

	out, err := s.Reflect(t.Context(), toolTurn("a", "b", "c", "d"))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("payloads = %d, want the SkillSynthesized event", len(out))
	}
	ev, ok := out[0].(types.SkillSynthesized)
	if !ok || ev.SkillName != "cut-a-release" || ev.Trigger != types.SynthesisSingleTurn {
		t.Fatalf("event = %+v", out[0])
	}

	// AND THE ROW IS REALLY THERE. The event without the row would be the
	// same bug one layer up.
	got, found, err := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v — the skill was announced and not written", found, err)
	}
	if got.Content == "" || len(got.ToolSequence) != 4 {
		t.Errorf("stored skill = %+v, want the body and the tool run", got)
	}
}

// A SHORT TURN IS NOT A PROCEDURE. One or two calls is a step, and drafting a
// skill for it produces a catalogue entry that says less than the tool's own
// description.
func TestATurnBelowTheToolThresholdIsSkipped(t *testing.T) {
	t.Parallel()
	s := synthesizer(t, newStore(t), goodDraft, learning.SynthesizerOptions{MinToolCalls: 5})
	if got := s.Skip(toolTurn("a", "b")); got != learning.SkipTooFewTools {
		t.Fatalf("skip = %q, want %q", got, learning.SkipTooFewTools)
	}
}

// AN UNSETTLED TURN IS NOT LEARNED FROM. A skill drafted from work the agent
// itself judged incomplete encodes a procedure the next round may contradict.
func TestAnUnsettledTurnDraftsNothing(t *testing.T) {
	t.Parallel()
	s := synthesizer(t, newStore(t), goodDraft, learning.SynthesizerOptions{MinToolCalls: 1})
	turn := toolTurn("a", "b", "c")
	turn.Event.ReviewOutcome = "self_iterate"
	if got := s.Skip(turn); got != learning.SkipNotSettled {
		t.Fatalf("skip = %q, want %q", got, learning.SkipNotSettled)
	}
}

// THE MODEL DECLINING IS THE ORDINARY ANSWER, not an error: most turns have
// no reusable shape, and asking is cheap.
func TestADeclinedDraftWritesNothing(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	s := synthesizer(t, db, "{}", learning.SynthesizerOptions{MinToolCalls: 1})

	out, err := s.Reflect(t.Context(), toolTurn("a", "b", "c"))
	if err != nil {
		t.Fatalf("a declined draft was reported as a failure: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("payloads = %v, want none", out)
	}
	if n, _ := learning.NewSkills(db).Count(t.Context(), "dev", learning.ListOptions{}); n != 0 {
		t.Errorf("%d skills written for a declined draft", n)
	}
}

// A HALF-DRAFT IS DROPPED. A skill with no body costs prompt budget on every
// future turn and teaches nothing; one with no name cannot be addressed.
func TestAnIncompleteDraftIsDropped(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]string{
		"no content": `{"name":"x","description":"d","content":""}`,
		"no name":    `{"name":"","description":"d","content":"steps"}`,
		"no summary": `{"name":"x","description":"","content":"steps"}`,
		"not json":   `I think the skill here is to run the pipeline.`,
	} {
		db := newStore(t)
		s := synthesizer(t, db, answer, learning.SynthesizerOptions{MinToolCalls: 1})
		if _, err := s.Reflect(t.Context(), toolTurn("a", "b")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n, _ := learning.NewSkills(db).Count(t.Context(), "dev", learning.ListOptions{}); n != 0 {
			t.Errorf("%s: wrote a skill anyway", name)
		}
	}
}

// THE SAME PROCEDURE IS NOT DRAFTED TWICE. A seat doing the same job weekly
// would otherwise accumulate paraphrases of one skill, and the prefetch would
// spend its budget showing the model the same thing repeatedly.
func TestASecondDraftOfTheSameProcedureIsRefused(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	s := synthesizer(t, db, goodDraft, learning.SynthesizerOptions{MinToolCalls: 1})

	if _, err := s.Reflect(t.Context(), toolTurn("a", "b", "c", "d")); err != nil {
		t.Fatal(err)
	}
	// The same tools in a DIFFERENT order: the same procedure, and treating
	// order as identity is how a seat ends up with a skill per permutation.
	if _, err := s.Reflect(t.Context(), toolTurn("d", "c", "b", "a")); err != nil {
		t.Fatal(err)
	}
	if n, _ := learning.NewSkills(db).Count(t.Context(), "dev", learning.ListOptions{}); n != 1 {
		t.Errorf("skills = %d, want the one procedure", n)
	}
}

// A DIFFERENT PROCEDURE STILL EARNS ITS OWN SKILL, or the duplicate check
// above would be a synthesizer that writes exactly one skill per seat.
func TestADifferentProcedureIsStillDrafted(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	skills := learning.NewSkills(db)
	first := synthesizer(t, db, goodDraft, learning.SynthesizerOptions{MinToolCalls: 1})
	if _, err := first.Reflect(t.Context(), toolTurn("a", "b", "c", "d")); err != nil {
		t.Fatal(err)
	}
	second := synthesizer(t, db,
		`{"name":"triage-an-alert","description":"Handle a page","content":"steps"}`,
		learning.SynthesizerOptions{MinToolCalls: 1})
	if _, err := second.Reflect(t.Context(), toolTurn("w", "x", "y", "z")); err != nil {
		t.Fatal(err)
	}
	if n, _ := skills.Count(t.Context(), "dev", learning.ListOptions{}); n != 2 {
		t.Errorf("skills = %d, want both procedures", n)
	}
}

// THE PER-SEAT CAP IS HONOURED, and it is checked BEFORE the model call — a
// draft made only to be thrown away is money spent for nothing.
func TestTheSeatCapStopsTheDraftBeforeTheModelCall(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	s, provider := synthesizerWith(t, db,
		&auxProvider{replies: []llm.Completion{{Content: goodDraft}}},
		learning.SynthesizerOptions{MinToolCalls: 1, MaxSkillsPerAgent: 1})
	if _, err := s.Reflect(t.Context(), toolTurn("a", "b", "c")); err != nil {
		t.Fatal(err)
	}
	calls := provider.callCount()
	if _, err := s.Reflect(t.Context(), toolTurn("q", "r", "s")); err != nil {
		t.Fatal(err)
	}
	if provider.callCount() != calls {
		t.Errorf("the model was called %d more times past the cap; the count "+
			"is cheap and the draft is not", provider.calls-calls)
	}
}

// A MODEL FAILURE IS REPORTED, not swallowed. Reflection is best effort at
// the dispatcher, but a worker that returns nil on an unreachable model makes
// "nothing to draft" and "could not reach the model" the same answer.
func TestAnUnreachableModelIsReported(t *testing.T) {
	t.Parallel()
	s, _ := synthesizerWith(t, newStore(t),
		&auxProvider{err: errors.New("upstream 503")},
		learning.SynthesizerOptions{MinToolCalls: 1})
	if _, err := s.Reflect(t.Context(), toolTurn("a", "b")); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the provider failure named", err)
	}
}

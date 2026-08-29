package learning_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT THIS WORKER IS FOR.
//
// `auto_refine_on_success` and `auto_refine_on_failure` validated, schema'd
// and shipped in the example company with no reader outside internal/config:
// the documented auto path did not exist, so a skill only ever improved when
// a model happened to call refine_skill mid-turn. These cases are about the
// worker that closes that loop, and about the three ways it must decline.

// refiner builds one whose auxiliary model answers with a fixed body.
func refiner(t *testing.T, db *store.DB, answer string, opts learning.RefinerOptions) *learning.Refiner {
	t.Helper()
	r, _ := refinerWith(t, db, &auxProvider{replies: []llm.Completion{{Content: answer}}}, opts)
	return r
}

func refinerWith(t *testing.T, db *store.DB, p *auxProvider,
	opts learning.RefinerOptions,
) (*learning.Refiner, *auxProvider) {
	t.Helper()
	r, err := learning.NewRefiner(&stubModels{p: p}, learning.NewSkills(db), opts)
	if err != nil {
		t.Fatalf("NewRefiner: %v", err)
	}
	return r, p
}

// seedSkill writes one skill for the seat and returns it.
func seedSkill(t *testing.T, db *store.DB, name, body string) learning.Skill {
	t.Helper()
	now := time.Now().UTC()
	sk := learning.Skill{
		ID: "skill-" + name, AgentHandle: "dev", Name: name,
		Description: "how to " + name, Content: body,
		ToolSequence: []string{"a", "b"},
		CreatedAt:    now, UpdatedAt: now,
	}
	if err := learning.NewSkills(db).Insert(t.Context(), sk); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return sk
}

// usedTurn is a settled turn that was offered the given skill ids.
func usedTurn(outcome string, skillIDs ...string) learning.Turn {
	return learning.Turn{
		Role: &org.Role{Name: "Dev"},
		Event: types.TurnCompleted{
			Agent: "agent-uuid", AgentHandle: "dev", RoleName: "Dev", TurnID: "t1",
			SkillsUsed: skillIDs, ReviewOutcome: outcome,
			ToolSequence: []string{"a", "b"},
			TaskSummary:  "ship the release", PlanSummary: "cut, tag, announce",
		},
	}
}

const goodChoice = `{"skill_name":"cut-a-release","bullet":"The tag must exist before the pipeline runs."}`

// A SUCCESSFUL TURN APPENDS AN OBSERVATION — the auto path that did not exist.
func TestASuccessfulTurnAppendsWhatPracticeTaught(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline\n2. tag")
	r := refiner(t, db, goodChoice, learning.RefinerOptions{})

	out, err := r.Reflect(t.Context(), usedTurn("done", sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("payloads = %d, want the SkillRefined event", len(out))
	}
	ev, ok := out[0].(types.SkillRefined)
	if !ok {
		t.Fatalf("payload = %T, want types.SkillRefined", out[0])
	}
	if ev.SkillName != "cut-a-release" || ev.SkillVersion != 2 ||
		ev.RefinementKind != string(learning.RefineObserved) {
		t.Fatalf("event = %+v", ev)
	}

	stored, found, err := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
	if err != nil || !found {
		t.Fatalf("Get: %v found=%v", err, found)
	}
	if !strings.Contains(stored.Content, "1. run the pipeline") {
		t.Fatalf("the procedure was replaced rather than appended to:\n%s", stored.Content)
	}
	if !strings.Contains(stored.Content, "**Observed in practice:** The tag must exist") {
		t.Fatalf("no observation bullet:\n%s", stored.Content)
	}
	if stored.Version != 2 {
		t.Fatalf("version = %d, want 2 — the prior body must be archived", stored.Version)
	}
	versions, err := learning.NewSkills(db).Versions(t.Context(), sk.ID, 10)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 1 || strings.Contains(versions[0].Content, "Observed in practice") {
		t.Fatalf("the archived version is not the pre-refinement body: %+v", versions)
	}
}

// A FAILED TURN WRITES A COUNTER-EXAMPLE, which is the half that matters most:
// a skill that led an agent wrong is exactly the one worth annotating.
func TestAFailedTurnWritesACounterExample(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	r := refiner(t, db, goodChoice, learning.RefinerOptions{})

	out, err := r.Reflect(t.Context(), usedTurn("failed", sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	ev, ok := out[0].(types.SkillRefined)
	if !ok || ev.RefinementKind != string(learning.RefineCounterExample) {
		t.Fatalf("event = %+v, want a counter_example refinement", out[0])
	}
	stored, _, _ := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
	if !strings.Contains(stored.Content, "**Counter-example:**") {
		t.Fatalf("no counter-example bullet:\n%s", stored.Content)
	}
}

// THE TWO TOGGLES GATE THE TWO OUTCOMES INDEPENDENTLY. Both were config-only
// before this worker existed, so a company setting either got a validated
// revision and no behaviour change.
func TestTheOutcomeTogglesGateTheirOwnHalf(t *testing.T) {
	t.Parallel()
	off, on := false, true
	cases := []struct {
		name       string
		opts       learning.RefinerOptions
		outcome    string
		wantReason string
	}{
		{"success off", learning.RefinerOptions{OnSuccess: &off}, "done", learning.SkipOutcomeNotRefined},
		{"success on", learning.RefinerOptions{OnSuccess: &on}, "done", ""},
		{"failure off", learning.RefinerOptions{OnFailure: &off}, "failed", learning.SkipOutcomeNotRefined},
		{"failure on", learning.RefinerOptions{OnFailure: &on}, "failed", ""},
		{"failure off leaves success alone", learning.RefinerOptions{OnFailure: &off}, "done", ""},
		{"success off leaves failure alone", learning.RefinerOptions{OnSuccess: &off}, "failed", ""},
	}
	db := newStore(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := refiner(t, db, goodChoice, c.opts)
			if got := r.Skip(usedTurn(c.outcome, "skill-x")); got != c.wantReason {
				t.Fatalf("Skip = %q, want %q", got, c.wantReason)
			}
		})
	}
}

// AN UNSETTLED TURN IS NOT REFINED. self_iterate is a round the agent itself
// judged incomplete and the engine will reattempt — annotating a skill from it
// writes a lesson the next round may contradict, and then a second bullet when
// the turn actually settles.
func TestASelfIteratingTurnIsNotRefined(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	r := refiner(t, db, goodChoice, learning.RefinerOptions{})
	if got := r.Skip(usedTurn("self_iterate", "skill-x")); got != learning.SkipNotSettled {
		t.Fatalf("Skip = %q, want %q", got, learning.SkipNotSettled)
	}
}

// A TURN THAT WAS OFFERED NO SKILL IS SKIPPED BEFORE ANY MODEL CALL. Without
// this gate every turn in the company would cost an auxiliary completion to be
// told there is nothing to refine.
func TestATurnWithNoSkillsCostsNoCall(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	r, p := refinerWith(t, db, &auxProvider{replies: []llm.Completion{{Content: goodChoice}}},
		learning.RefinerOptions{})
	turn := usedTurn("done")
	if got := r.Skip(turn); got != learning.SkipNoSkillsOffered {
		t.Fatalf("Skip = %q, want %q", got, learning.SkipNoSkillsOffered)
	}
	if _, err := r.Reflect(t.Context(), turn); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if p.calls != 0 {
		t.Fatalf("calls = %d, want 0 — a turn with no live skill must not reach the model", p.calls)
	}
}

// A SKILL THE CURATOR ARCHIVED SINCE THE TURN IS NOT RESURRECTED. The turn's
// list is ids captured when its prompt was built; refining one that has since
// been archived would grow a body nobody will ever be shown.
func TestAnArchivedSkillIsNotRefined(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	skills := learning.NewSkills(db)
	ok, err := skills.Transition(t.Context(), learning.Transition{
		SkillID: sk.ID, To: learning.SkillArchived, Reason: "test", At: time.Now().UTC(),
	})
	if err != nil || !ok {
		t.Fatalf("Transition: %v ok=%v", err, ok)
	}
	r, p := refinerWith(t, db, &auxProvider{replies: []llm.Completion{{Content: goodChoice}}},
		learning.RefinerOptions{})
	out, err := r.Reflect(t.Context(), usedTurn("done", sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(out) != 0 || p.calls != 0 {
		t.Fatalf("payloads = %d, calls = %d, want the archived skill left alone", len(out), p.calls)
	}
}

// A SKILL THIS TURN NEVER USED IS NOT A CANDIDATE. The seat's catalogue is
// its whole history; the question is what THIS turn taught, so offering the
// model every skill the seat owns invites a bullet on a procedure the turn
// never followed.
func TestOnlyTheSkillsTheTurnUsedAreOffered(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	used := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	seedSkill(t, db, "triage-a-bug", "1. reproduce it")
	r, p := refinerWith(t, db, &auxProvider{replies: []llm.Completion{{Content: "{}"}}},
		learning.RefinerOptions{})
	if _, err := r.Reflect(t.Context(), usedTurn("done", used.ID)); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	prompt := p.seen[0].Messages[len(p.seen[0].Messages)-1].Content
	if !strings.Contains(prompt, "cut-a-release") {
		t.Fatalf("the used skill is missing from the prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "triage-a-bug") {
		t.Fatalf("a skill this turn never used was offered to the model:\n%s", prompt)
	}
}

// THE CHOSEN NAME RESOLVES EXACTLY, even when two of the seat's skills share
// a prefix. A near-match that landed on the wrong one would annotate a
// procedure the turn never followed, with the bullet reading as if it had.
func TestTheNamedSkillIsTheOneEdited(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	base := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	hotfix := seedSkill(t, db, "cut-a-release-hotfix", "1. branch from the tag")
	r := refiner(t, db, `{"skill_name":"cut-a-release-hotfix","bullet":"Branch before you tag."}`,
		learning.RefinerOptions{})
	if _, err := r.Reflect(t.Context(), usedTurn("done", base.ID, hotfix.ID)); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	skills := learning.NewSkills(db)
	edited, _, _ := skills.Get(t.Context(), "dev", "cut-a-release-hotfix")
	untouched, _, _ := skills.Get(t.Context(), "dev", "cut-a-release")
	if edited.Version != 2 || !strings.Contains(edited.Content, "Branch before you tag.") {
		t.Fatalf("the named skill was not the one edited: v%d\n%s", edited.Version, edited.Content)
	}
	if untouched.Version != 1 {
		t.Fatalf("the prefix-sharing sibling was edited too: v%d\n%s",
			untouched.Version, untouched.Content)
	}
}

// NOOP IS THE EXPECTED ANSWER, and it must not be an error: most turns teach
// their skills nothing, and a pass that failed on every one of them would
// report a company whose learning is permanently broken.
func TestADeclineWritesNothingAndIsNotAnError(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"{}", "", "  ", "I have nothing to add.",
		`{"skill_name":"cut-a-release","bullet":""}`, `{"skill_name":"","bullet":"x"}`} {
		db := newStore(t)
		sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
		r := refiner(t, db, answer, learning.RefinerOptions{})
		out, err := r.Reflect(t.Context(), usedTurn("done", sk.ID))
		if err != nil {
			t.Fatalf("answer %q: Reflect: %v", answer, err)
		}
		if len(out) != 0 {
			t.Fatalf("answer %q: payloads = %d, want none", answer, len(out))
		}
		stored, _, _ := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
		if stored.Version != 1 || strings.Contains(stored.Content, "practice") {
			t.Fatalf("answer %q: the skill was changed: v%d\n%s", answer, stored.Version, stored.Content)
		}
	}
}

// A FENCED ANSWER IS STILL AN ANSWER. A model told "JSON and nothing else"
// fences it often enough that a parser without this reads every reply as a
// decline — indistinguishable from a model with nothing to say.
func TestAFencedAnswerIsRead(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	r := refiner(t, db, "```json\n"+goodChoice+"\n```", learning.RefinerOptions{})
	out, err := r.Reflect(t.Context(), usedTurn("done", sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("payloads = %d, want the fenced answer read", len(out))
	}
}

// A NAME THE MODEL INVENTED IS DROPPED, not fuzzy-matched onto the nearest
// candidate: a bullet appended to the wrong procedure is worse than none.
func TestAnUnknownSkillNameIsDropped(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	r := refiner(t, db, `{"skill_name":"cut-a-release-v2","bullet":"tag first"}`,
		learning.RefinerOptions{})
	out, err := r.Reflect(t.Context(), usedTurn("done", sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("payloads = %d, want the invented name dropped", len(out))
	}
	stored, _, _ := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
	if stored.Version != 1 {
		t.Fatalf("version = %d — a near-miss name must not edit a skill", stored.Version)
	}
}

// THE BODY CAP SKIPS, IT DOES NOT TRUNCATE. A clipped procedure lands
// mid-step and the model reads the remainder as the whole thing.
func TestARefinementPastTheBodyCapIsSkippedNotClipped(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	body := strings.Repeat("x", 200)
	sk := seedSkill(t, db, "cut-a-release", body)
	r := refiner(t, db, goodChoice, learning.RefinerOptions{MaxBodyChars: 210})

	out, err := r.Reflect(t.Context(), usedTurn("done", sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("payloads = %d, want the over-cap refinement skipped", len(out))
	}
	stored, _, _ := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
	if stored.Content != body || stored.Version != 1 {
		t.Fatalf("the skill was edited past its cap: v%d len=%d", stored.Version, len(stored.Content))
	}
}

// SUCCESSIVE BULLETS COLLECT UNDER ONE HEADING rather than scattering a new
// section through the body on every turn.
func TestSuccessiveRefinementsShareOneHeading(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	r := refiner(t, db, goodChoice, learning.RefinerOptions{})
	for range 2 {
		if _, err := r.Reflect(t.Context(), usedTurn("done", sk.ID)); err != nil {
			t.Fatalf("Reflect: %v", err)
		}
	}
	stored, _, _ := learning.NewSkills(db).Get(t.Context(), "dev", "cut-a-release")
	if got := strings.Count(stored.Content, "## What practice added"); got != 1 {
		t.Fatalf("headings = %d, want 1:\n%s", got, stored.Content)
	}
	if got := strings.Count(stored.Content, "**Observed in practice:**"); got != 2 {
		t.Fatalf("bullets = %d, want 2:\n%s", got, stored.Content)
	}
}

// THE PROMPT CARRIES THE CANDIDATES BY EXACT NAME AND THE TURN'S OUTCOME.
// Without the bodies the model cannot tell what a skill already says, and
// asks it to write down what is already there.
func TestThePromptShowsTheSkillsAndTheTurn(t *testing.T) {
	t.Parallel()
	db := newStore(t)
	sk := seedSkill(t, db, "cut-a-release", "1. run the pipeline")
	r, p := refinerWith(t, db, &auxProvider{replies: []llm.Completion{{Content: "{}"}}},
		learning.RefinerOptions{MaxTokens: 1234})
	if _, err := r.Reflect(t.Context(), usedTurn("failed", sk.ID)); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(p.seen) != 1 {
		t.Fatalf("calls = %d, want 1", len(p.seen))
	}
	req := p.seen[0]
	if req.MaxTokens != 1234 {
		t.Fatalf("MaxTokens = %d, want the company's budget_tokens", req.MaxTokens)
	}
	user := req.Messages[len(req.Messages)-1].Content
	for _, want := range []string{"cut-a-release", "1. run the pipeline", "failed", "ship the release"} {
		if !strings.Contains(user, want) {
			t.Fatalf("the prompt omits %q:\n%s", want, user)
		}
	}
}

// A REFINER WITH NO MODEL REGISTRY OR NO STORE REFUSES TO BE BUILT rather
// than silently declining every turn.
func TestARefinerNeedsBothHalves(t *testing.T) {
	t.Parallel()
	if _, err := learning.NewRefiner(nil, learning.NewSkills(newStore(t)),
		learning.RefinerOptions{}); err == nil {
		t.Fatal("NewRefiner accepted a nil model registry")
	}
	if _, err := learning.NewRefiner(&stubModels{p: &auxProvider{}}, nil,
		learning.RefinerOptions{}); err == nil {
		t.Fatal("NewRefiner accepted a nil skill store")
	}
}

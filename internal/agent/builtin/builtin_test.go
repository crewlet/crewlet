package builtin_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

const companyDoc = `
name: Nimbus
providers:
  llm:
    p:
      type: anthropic
      model: m
      api_keys: ["${K}"]
roles:
  - name: Agent CEO
    handle: agent-ceo
    llm: p
  - name: Agent CTO
    handle: agent-cto
    llm: p
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
`

func organization(t *testing.T) *org.Organization {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(companyDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return o
}

// turnFor builds the context a tool runs under, as the runner does.
func turnFor(t *testing.T, handle string) *turnctx.Turn {
	t.Helper()
	o := organization(t)
	seat := o.AgentSeatByHandle(handle)
	if seat == nil {
		t.Fatalf("no seat %q", handle)
	}
	return &turnctx.Turn{ID: "wk-1", Seat: seat, Org: o}
}

// registered builds a registry over the given deps and returns one tool.
func registered(t *testing.T, deps builtin.Deps, name string) tools.Callable {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, deps); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, ok := reg.Snapshot().Lookup(name)
	if !ok {
		t.Fatalf("%s was not registered", name)
	}
	return entry.Tool
}

func callFor(t *testing.T, tool tools.Callable, turn *turnctx.Turn, args map[string]any) tools.Result {
	t.Helper()
	seated, ok := tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s cannot know who called it", tool.Name())
	}
	res, err := seated.CallForTurn(context.Background(), turn, args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return res
}

// --- registration --------------------------------------------------------- //

func TestATooWithNoBackingIsOmittedNotBroken(t *testing.T) {
	t.Parallel()
	// A model shown a tool that always fails learns to distrust the whole
	// catalogue, and burns a round finding out each time. A node without a
	// store gets the tools it can serve and no others.
	reg := tools.NewRegistry()
	names, err := builtin.Register(reg, builtin.Deps{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(names) != 1 || names[0] != builtin.LookupColleagueTool {
		t.Errorf("a storeless node registered %v; only lookup_colleague needs "+
			"nothing but the turn's own org", names)
	}
}

func TestEveryBuiltinDeclaresWhetherItIsARead(t *testing.T) {
	t.Parallel()
	// THE DELIVERY GATE reads this. A turn that planned to act and then
	// only read delivered nothing, and the gate can see that only if it
	// knows which calls were reads — unannotated counts as NOT a known
	// read, so a read-only builtin left unannotated makes every recall
	// look like a delivery.
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, fullDeps(t)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range reg.Snapshot().Entries() {
		if e.Annotations.ReadOnly.String() == "unknown" {
			t.Errorf("%s does not say whether it is a read", e.Name())
		}
		if e.Origin != tools.OriginBuiltin {
			t.Errorf("%s registered as %q, not builtin", e.Name(), e.Origin)
		}
	}
}

// --- lookup_colleague ----------------------------------------------------- //

func TestLookupNamesTheSeatAndItsIdentities(t *testing.T) {
	t.Parallel()
	tool := registered(t, builtin.Deps{}, builtin.LookupColleagueTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"query": "Founder"})
	if res.Failed {
		t.Fatalf("failed: %s", res.Output)
	}
	for _, want := range []string{"founder", "human", "U0FOUNDER"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output does not mention %q:\n%s", want, res.Output)
		}
	}
}

func TestAnAmbiguousLookupRefusesToGuess(t *testing.T) {
	t.Parallel()
	// An agent that silently addressed the wrong colleague is worse than
	// one that asked which: the wrong person is pulled into work that is
	// not theirs, and the right one never hears about it.
	tool := registered(t, builtin.Deps{}, builtin.LookupColleagueTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"query": "agent"})
	if !res.Failed {
		t.Fatalf("an ambiguous query answered with one seat:\n%s", res.Output)
	}
	for _, want := range []string{"agent-ceo", "agent-cto", "ambiguous"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output does not mention %q:\n%s", want, res.Output)
		}
	}
}

// --- a2a_ask -------------------------------------------------------------- //

type asker struct {
	asks []a2a.Ask
	err  error
}

func (a *asker) Open(_ context.Context, ask a2a.Ask) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	a.asks = append(a.asks, ask)
	return "ch-1", nil
}

func TestAnAskCarriesTheCallingSeatNotAnArgument(t *testing.T) {
	t.Parallel()
	// The requester comes from the surface. A model that wrote a different
	// handle in its arguments cannot become somebody else.
	svc := &asker{}
	tool := registered(t, builtin.Deps{A2A: svc}, builtin.A2AAskTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"target": "agent-cto", "brief": "What broke last night?",
		"requester": "agent-cto", // ignored, and must be
	})
	if res.Failed {
		t.Fatalf("failed: %s", res.Output)
	}
	if len(svc.asks) != 1 {
		t.Fatalf("asks = %d", len(svc.asks))
	}
	got := svc.asks[0]
	if got.Requester != "agent-ceo" || got.Target != "agent-cto" {
		t.Errorf("ask = %+v", got)
	}
	if got.SenderRole != "Agent CEO" {
		t.Errorf("sender role = %q, so the answering prompt cannot say who asked",
			got.SenderRole)
	}
	// The chain travels so the answering seat can refuse past the cap
	// rather than discovering the loop at runtime.
	if got.DelegationDepth != 1 || len(got.DelegationChain) != 1 {
		t.Errorf("delegation = depth %d chain %v", got.DelegationDepth, got.DelegationChain)
	}
}

func TestAnAskToAHumanExplainsWhyItCannotWork(t *testing.T) {
	t.Parallel()
	// A human seat is addressable and never spawned, so no turn answers
	// the channel. Without this the agent waits for an answer that is
	// never coming and nothing fails.
	svc := &asker{}
	tool := registered(t, builtin.Deps{A2A: svc}, builtin.A2AAskTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"target": "founder", "brief": "Approve the budget?",
	})
	if !res.Failed {
		t.Fatalf("an ask to a human was accepted:\n%s", res.Output)
	}
	if len(svc.asks) != 0 {
		t.Errorf("a channel was opened to a human: %+v", svc.asks)
	}
	lower := strings.ToLower(res.Output)
	if !strings.Contains(lower, "human") || !strings.Contains(lower, "mention") {
		t.Errorf("the refusal does not say what to do instead:\n%s", res.Output)
	}
}

func TestAnAskToAnUnknownHandleIsRefusedBeforeItIsSent(t *testing.T) {
	t.Parallel()
	// An ask to a handle that does not exist is a turn spent waiting for an
	// answer that can never come, with nothing failing.
	svc := &asker{}
	tool := registered(t, builtin.Deps{A2A: svc}, builtin.A2AAskTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"target": "nobody-at-all", "brief": "hello",
	})
	if !res.Failed || len(svc.asks) != 0 {
		t.Errorf("res = %+v, asks = %+v", res, svc.asks)
	}
	if !strings.Contains(res.Output, builtin.LookupColleagueTool) {
		t.Errorf("the refusal does not point at lookup_colleague:\n%s", res.Output)
	}
}

func TestASelfAskIsRefused(t *testing.T) {
	t.Parallel()
	// A channel to yourself has no responder: the answering side decides
	// who replies by comparing the woken seat against the requester, so a
	// self-ask wakes this seat, reads as an incoming ANSWER, and is never
	// replied to — a turn spent on nothing.
	svc := &asker{}
	tool := registered(t, builtin.Deps{A2A: svc}, builtin.A2AAskTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"target": "agent-ceo", "brief": "hello",
	})
	if !res.Failed || len(svc.asks) != 0 {
		t.Errorf("res = %+v, asks = %+v", res, svc.asks)
	}
}

func TestAnEmptyBriefIsRefusedWithTheReason(t *testing.T) {
	t.Parallel()
	// The colleague sees ONLY the brief — not the plan, not the
	// conversation — so an empty one wakes a seat with nothing to act on.
	svc := &asker{}
	tool := registered(t, builtin.Deps{A2A: svc}, builtin.A2AAskTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"target": "agent-cto"})
	if !res.Failed || len(svc.asks) != 0 {
		t.Errorf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "brief") {
		t.Errorf("the refusal does not name the missing field:\n%s", res.Output)
	}
}

// --- the memory tools ----------------------------------------------------- //

type skillStore struct {
	skills []learning.Skill
	used   []string
}

func (s *skillStore) Get(_ context.Context, handle, name string) (learning.Skill, bool, error) {
	for _, sk := range s.skills {
		if sk.AgentHandle == handle && sk.Name == name {
			return sk, true, nil
		}
	}
	return learning.Skill{}, false, nil
}

func (s *skillStore) List(_ context.Context, handle string, _ learning.ListOptions) ([]learning.Skill, error) {
	var out []learning.Skill
	for _, sk := range s.skills {
		if sk.AgentHandle == handle {
			out = append(out, sk)
		}
	}
	return out, nil
}

func (s *skillStore) MarkUsed(_ context.Context, id string, _ time.Time) learning.Use {
	s.used = append(s.used, id)
	return learning.Use{}
}

func (s *skillStore) Update(_ context.Context, id string, rev learning.Revision, _ learning.Refinement) (learning.Skill, error) {
	for i := range s.skills {
		if s.skills[i].ID == id {
			s.skills[i].Content = rev.Content
			s.skills[i].Version++
			return s.skills[i], nil
		}
	}
	return learning.Skill{}, context.Canceled
}

func TestASkillLoadsOnlyForItsOwnSeat(t *testing.T) {
	t.Parallel()
	// A skill is one seat's distilled experience. Loading another's would
	// make the per-seat store a shared one, and the whole design — a diary
	// keyed on a DERIVED agent id so a renamed handle orphans its rows
	// rather than inheriting somebody else's — rests on that holding.
	store := &skillStore{skills: []learning.Skill{
		{ID: "s1", AgentHandle: "agent-cto", Name: "deploy", Content: "1. push"},
	}}
	tool := registered(t, builtin.Deps{Skills: store}, builtin.UseSkillTool)

	if res := callFor(t, tool, turnFor(t, "agent-cto"),
		map[string]any{"skill_name": "deploy"}); res.Failed {
		t.Fatalf("the owning seat could not load it: %s", res.Output)
	}
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"skill_name": "deploy"})
	if !res.Failed {
		t.Errorf("another seat loaded it:\n%s", res.Output)
	}
}

func TestLoadingASkillRecordsThatItWasUsed(t *testing.T) {
	t.Parallel()
	// The telemetry is what answers "do agents actually load the skills the
	// synthesizer drafts, or is the apparent benefit just extra sampling?"
	store := &skillStore{skills: []learning.Skill{
		{ID: "s1", AgentHandle: "agent-ceo", Name: "post", Content: "body"},
	}}
	tool := registered(t, builtin.Deps{Skills: store}, builtin.UseSkillTool)
	callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"skill_name": "post"})
	if len(store.used) != 1 || store.used[0] != "s1" {
		t.Errorf("used = %v", store.used)
	}
}

func TestAnUnknownSkillNamesTheOnesTheSeatHas(t *testing.T) {
	t.Parallel()
	store := &skillStore{skills: []learning.Skill{
		{ID: "s1", AgentHandle: "agent-ceo", Name: "post-weekly"},
	}}
	tool := registered(t, builtin.Deps{Skills: store}, builtin.UseSkillTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"skill_name": "post weekly"})
	if !res.Failed || !strings.Contains(res.Output, "post-weekly") {
		t.Errorf("a near-miss does not point at the real name:\n%s", res.Output)
	}
}

func TestRefiningReplacesTheWholeBody(t *testing.T) {
	t.Parallel()
	// A whole-body replacement, not a patch: a model asked for a diff
	// produces something diff-shaped that does not apply, and a partial
	// edit that half-applied leaves a procedure that is neither version.
	store := &skillStore{skills: []learning.Skill{
		{ID: "s1", AgentHandle: "agent-ceo", Name: "post", Content: "old", Version: 1},
	}}
	tool := registered(t, builtin.Deps{Refinable: store}, builtin.RefineSkillTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{
		"skill_name": "post", "content": "new procedure", "reason": "the old one 404'd",
	})
	if res.Failed {
		t.Fatalf("failed: %s", res.Output)
	}
	if store.skills[0].Content != "new procedure" || store.skills[0].Version != 2 {
		t.Errorf("skill = %+v", store.skills[0])
	}
}

func TestRefiningWithNoBodyIsRefused(t *testing.T) {
	t.Parallel()
	// It replaces the body, so an empty one would delete the skill.
	store := &skillStore{skills: []learning.Skill{
		{ID: "s1", AgentHandle: "agent-ceo", Name: "post", Content: "old"},
	}}
	tool := registered(t, builtin.Deps{Refinable: store}, builtin.RefineSkillTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"skill_name": "post"})
	if !res.Failed || store.skills[0].Content != "old" {
		t.Errorf("res = %+v, skill = %+v", res, store.skills[0])
	}
}

type diaryStore struct{ wrote []learning.DiaryEntry }

func (d *diaryStore) Write(_ context.Context, e learning.DiaryEntry) error {
	d.wrote = append(d.wrote, e)
	return nil
}

func (d *diaryStore) Recent(_ context.Context, agentID string, _ time.Time, limit int) ([]learning.DiaryEntry, error) {
	var out []learning.DiaryEntry
	for _, e := range d.wrote {
		if e.AgentID == agentID && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestANoteIsKeptAgainstTheDerivedAgentID(t *testing.T) {
	t.Parallel()
	// The DERIVED id, never the handle: renaming a handle then cleanly
	// orphans the old rows rather than handing one seat's memory to
	// whoever takes the name next.
	d := &diaryStore{}
	tool := registered(t, builtin.Deps{Diary: d}, builtin.ReflectAndPersistTool)
	turn := turnFor(t, "agent-ceo")
	if res := callFor(t, tool, turn, map[string]any{
		"content": "Deploys go out on Thursdays.",
	}); res.Failed {
		t.Fatalf("failed: %s", res.Output)
	}
	if len(d.wrote) != 1 {
		t.Fatalf("wrote %d", len(d.wrote))
	}
	want, _ := turn.Org.AgentIDFor(turn.Seat)
	if d.wrote[0].AgentID != want.String() {
		t.Errorf("agent id = %q, want the derived one", d.wrote[0].AgentID)
	}
	if d.wrote[0].AgentID == turn.Handle() {
		t.Error("the note was keyed on the handle")
	}
	// Attributed to the tool, so an operator reading the diary can tell
	// what the agent CHOSE to keep from what a worker decided for it.
	if !strings.Contains(d.wrote[0].Source, builtin.ReflectAndPersistTool) {
		t.Errorf("source = %q", d.wrote[0].Source)
	}
}

func TestAnOverlongNoteIsRefusedWithTheReason(t *testing.T) {
	t.Parallel()
	// A note is re-read in later prompts, so its cost is paid on every turn
	// that recalls it — not once.
	d := &diaryStore{}
	tool := registered(t, builtin.Deps{Diary: d}, builtin.ReflectAndPersistTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"),
		map[string]any{"content": strings.Repeat("x", 5000)})
	if !res.Failed || len(d.wrote) != 0 {
		t.Errorf("res = %+v, wrote %d", res, len(d.wrote))
	}
	if !strings.Contains(res.Output, "knowledge base") {
		t.Errorf("the refusal does not say where the long version belongs:\n%s", res.Output)
	}
}

func TestRefreshReadsOnlyThisSeatsNotes(t *testing.T) {
	t.Parallel()
	d := &diaryStore{}
	write := registered(t, builtin.Deps{Diary: d}, builtin.ReflectAndPersistTool)
	read := registered(t, builtin.Deps{Diary: d}, builtin.RefreshMemoryTool)

	callFor(t, write, turnFor(t, "agent-ceo"), map[string]any{"content": "ceo fact"})
	callFor(t, write, turnFor(t, "agent-cto"), map[string]any{"content": "cto fact"})

	res := callFor(t, read, turnFor(t, "agent-ceo"), nil)
	if strings.Contains(res.Output, "cto fact") {
		t.Errorf("one seat read another's notes:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "ceo fact") {
		t.Errorf("a seat could not read its own notes:\n%s", res.Output)
	}
}

type episodeStore struct{ episodes []learning.Episode }

func (e *episodeStore) Recent(_ context.Context, handle string, limit int) ([]learning.Episode, error) {
	var out []learning.Episode
	for _, ep := range e.episodes {
		if ep.Handle == handle && len(out) < limit {
			out = append(out, ep)
		}
	}
	return out, nil
}

func (e *episodeStore) ForConversation(_ context.Context, handle, conv string, limit int) ([]learning.Episode, error) {
	var out []learning.Episode
	for _, ep := range e.episodes {
		if ep.Handle == handle && ep.ConversationKey == conv && len(out) < limit {
			out = append(out, ep)
		}
	}
	return out, nil
}

func TestRecallIsBoundedByWhatAModelWillRead(t *testing.T) {
	t.Parallel()
	// Recall goes into a prompt, and a tool that honoured "limit: 500"
	// would let one call spend a phase's whole context on history.
	store := &episodeStore{}
	for i := range 100 {
		store.episodes = append(store.episodes, learning.Episode{
			ID: string(rune('a' + i%26)), Handle: "agent-ceo",
			TaskSummary: "did a thing", StartedAt: time.Now().UTC(),
		})
	}
	tool := registered(t, builtin.Deps{Episodes: store}, builtin.QueryEpisodesTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{"limit": 500})
	if res.Failed {
		t.Fatalf("failed: %s", res.Output)
	}
	if n := strings.Count(res.Output, "did a thing"); n > 25 {
		t.Errorf("recalled %d turns; the cap is 25", n)
	}
}

func TestNoEarlierTurnsIsAnAnswerNotAFailure(t *testing.T) {
	t.Parallel()
	// "This is new work" is useful. A failed result would make the model
	// treat its own fresh start as a broken tool.
	tool := registered(t, builtin.Deps{Episodes: &episodeStore{}}, builtin.QueryEpisodesTool)
	res := callFor(t, tool, turnFor(t, "agent-ceo"), nil)
	if res.Failed {
		t.Errorf("an empty history was reported as a failure: %s", res.Output)
	}
}

type onboardingStore struct{ marks []learning.Marker }

func (o *onboardingStore) Mark(_ context.Context, m learning.Marker, _ time.Time) error {
	o.marks = append(o.marks, m)
	return nil
}

func TestOnboardingIsMarkedAgainstTheChainItWasDoneFor(t *testing.T) {
	t.Parallel()
	// The chain hash makes the marker specific to THIS org shape: a company
	// whose management chain changed is a different orientation, and a
	// marker that ignored it would leave a seat permanently un-onboarded to
	// its new context.
	store := &onboardingStore{}
	tool := registered(t, builtin.Deps{Onboarding: store}, builtin.MarkOnboardedTool)
	turn := turnFor(t, "agent-ceo")
	if res := callFor(t, tool, turn, map[string]any{"notes": "read the charter"}); res.Failed {
		t.Fatalf("failed: %s", res.Output)
	}
	if len(store.marks) != 1 {
		t.Fatalf("marks = %d", len(store.marks))
	}
	got := store.marks[0]
	if got.Handle != "agent-ceo" || got.Role != "Agent CEO" {
		t.Errorf("marker = %+v", got)
	}
	if got.ChainHash == "" || got.ChainHash != learning.ChainHash(turn.Org, turn.Seat) {
		t.Errorf("chain hash = %q", got.ChainHash)
	}
	if got.Summary != "read the charter" {
		t.Errorf("summary = %q", got.Summary)
	}
}

// --- the seat boundary ---------------------------------------------------- //

func TestEverySeatScopedToolRefusesWithoutASeat(t *testing.T) {
	t.Parallel()
	// A surface built outside a turn — a validate command, a test driving a
	// runner directly — has no seat, and a tool that speaks for one must
	// refuse rather than act as nobody or panic.
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, fullDeps(t)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, e := range reg.Snapshot().Entries() {
		res, err := e.Tool.Call(context.Background(), map[string]any{
			"query": "x", "target": "agent-cto", "brief": "x",
			"skill_name": "x", "content": "x",
		})
		if err != nil {
			t.Errorf("%s returned an error rather than a failed result: %v", e.Name(), err)
			continue
		}
		if !res.Failed {
			t.Errorf("%s acted with no seat: %q", e.Name(), res.Output)
		}
	}
}

func fullDeps(t *testing.T) builtin.Deps {
	t.Helper()
	store := &skillStore{}
	return builtin.Deps{
		A2A: &asker{}, Skills: store, Refinable: store,
		Episodes: &episodeStore{}, Diary: &diaryStore{},
		Onboarding: &onboardingStore{},
	}
}

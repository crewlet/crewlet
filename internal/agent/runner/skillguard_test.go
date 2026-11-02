package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// capture records what a phase published.
type capture struct {
	mu     chan struct{}
	events []*events.Event
}

func newCapture() *capture { return &capture{mu: make(chan struct{}, 1)} }

func (c *capture) Publish(_ context.Context, _ string, ev *events.Event) error {
	c.mu <- struct{}{}
	c.events = append(c.events, ev)
	<-c.mu
	return nil
}

func (c *capture) blocked() []*types.ToolSkillGuardBlocked {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.ToolSkillGuardBlocked
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.ToolSkillGuardBlocked](ev); ok {
			out = append(out, got)
		}
	}
	return out
}

// startedPhases is the phase each agent_phase_started event names, in order.
func (c *capture) startedPhases() []string {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []string
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.AgentPhaseStarted](ev); ok {
			out = append(out, string(got.Phase))
		}
	}
	return out
}

// promptsFor is the opening prompt each phase published, as the live view
// renders it: the round with RoundNum -1 carries the whole conversation.
func (c *capture) promptsFor(ph string) []string {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []string
	for _, ev := range c.events {
		got, ok := events.DataAs[*types.AgentTurnProgress](ev)
		if !ok || got.RoundNum >= 0 || string(got.Phase) != ph {
			continue
		}
		joined := got.Prompt
		for _, msg := range got.PromptMessages {
			joined += "\n" + msg.Content
		}
		out = append(out, joined)
	}
	return out
}

func (c *capture) kinds() []string {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []string
	for _, ev := range c.events {
		out = append(out, ev.Type)
	}
	return out
}

var _ queue.Publisher = (*capture)(nil)

// guardedRunner is a seat whose surface carries one required skill covering
// the tool its run will reach for, with the covered tool registered under
// origin — so the same skill can cover a first-party tool or an MCP server's —
// and the launcher its executor's runs are handed to.
func guardedRunner(t *testing.T, pub queue.Publisher, origin string) (*runner.Runner, *playLauncher) {
	t.Helper()
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{
		Key: "chat-conventions", Title: "Chat conventions",
		Summary: "how this company writes on chat",
		Body:    "Always thread your reply.",
		Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
		SourcePageID: "chat-conventions-page",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}

	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: &scriptedProvider{}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "slack_post", out: "posted"},
		origin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// THE REAL LOADER, not a stub: the unlock is observed from a
	// SUCCESSFUL call, so a stub that answered anything would unlock the
	// tool without ever having produced the body.
	if _, err := builtin.Register(reg, builtin.Deps{
		ToolSkills: registry,
		// THE REAL AUTHORITY DECISION, because every builtin is gated at
		// registration: a nil Authorizer REFUSES, so a fixture without
		// one would never load the skill and the unlock below could not
		// happen for a reason that has nothing to do with skills.
		Authorize: builtin.Decide(authz.NoChart{}),
	}); err != nil {
		t.Fatalf("builtin.Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	launcher := &playLauncher{}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Launcher:  launcher,
		Task:      "post the summary",
		Publisher: pub,
		Turn: runner.Turn{
			RunID: "t-guard", WorkKey: "wk-t-guard", AgentID: "agent-1",
			// THE ACTING SEAT, which is what every tool call is
			// authorized as: [tools.Surface] derives the principal from
			// it, and a turn with none reaches the authority gate as
			// nobody — which refuses every builtin, load_tool_skill
			// included.
			Context: &turnctx.Turn{
				RunID: "t-guard", WorkKey: "wk-t-guard",
				Seat: role,
				Org:  &org.Organization{Name: "Acme", Roles: []*org.Role{role}},
			},
		},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r, launcher
}

// guardedLaunch launches the executor's run in round on [guardedRunner]'s
// seat. What the run then calls is the test's to [play] against the surface
// it was launched with: the one every bridged call is dispatched through.
func guardedLaunch(t *testing.T, pub queue.Publisher, origin string, round int) runner.AgentRunRequest {
	t.Helper()
	r, launcher := guardedRunner(t, pub, origin)
	if _, _, err := r.Execute(context.Background(), round, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return launcher.last(t)
}

// THE GUARD REFUSES A COVERED TOOL, at the dispatch point every call goes
// through — the bridge's, for an executor's run. Asking harder in the prompt
// does not stop a model going straight for the tool.
func TestTheExecutorIsRefusedAToolBeforeItsSkillIsLoaded(t *testing.T) {
	t.Parallel()
	req := guardedLaunch(t, newCapture(), tools.OriginBuiltin, 1)
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})

	// The refusal names the exact call to make, because the error IS the
	// recovery path.
	refusal := bridged[0]
	if !refusal.Failed {
		t.Fatalf("the covered tool ran before its skill was loaded: %q", refusal.Result)
	}
	if !strings.Contains(refusal.Result, `load_tool_skill(key="chat-conventions")`) {
		t.Fatalf("the refusal does not name the call:\n%s", refusal.Result)
	}
}

// EVERY REFUSAL IS PUBLISHED. Occasional blocks are the guard working; the
// chronic case — one skill blocked over and over — says the catalogue
// summary is not landing, and that is invisible in the turn's own record
// where a block looks like any other failed call.
func TestEveryRefusalIsPublishedForOperators(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	// A LATER ITERATION, so a refusal stamped with any other number — the
	// zero a guard built without its iteration reported — is told apart.
	req := guardedLaunch(t, pub, tools.OriginBuiltin, 2)
	play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})

	blocked := pub.blocked()
	if len(blocked) != 1 {
		t.Fatalf("%d refusals were published, want one; saw %v",
			len(blocked), pub.kinds())
	}
	got := blocked[0]
	if got.ToolName != "slack_post" || got.TurnID != "t-guard" ||
		got.Phase != types.Phase(phase.Execute) {
		t.Fatalf("the event = %+v", got)
	}
	// The iteration the phase ran as, which every other event of the
	// phase carries: a reader joining refusals to their phase needs it.
	if got.Iteration != 2 {
		t.Errorf("the refusal names iteration %d, want the phase's 2", got.Iteration)
	}
	// EVERY pending skill, not just the one that blocked: a model about to
	// be blocked twice more is a model an operator needs to see whole.
	if !slices.Equal(got.SkillKeys, []string{"chat-conventions"}) {
		t.Fatalf("skill keys = %v", got.SkillKeys)
	}
}

// A REQUIRED SKILL ON AN MCP TOOL IS ENFORCED as the catalogue promised it.
// An executor starts with no MCP tool on its active list and activates one by
// name, while its prompt lists the skills for every tool in its universe — so
// a guard armed over the active list alone let the activated tool through
// unread, under a catalogue that had called its skill required.
func TestARequiredSkillOnAnActivatedMCPToolIsEnforced(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	req := guardedLaunch(t, pub, tools.Origin("slack"), 1)
	play(t, req, run{calls: []llm.ToolCall{
		call(t, toolname.Activate, `{"name":"slack_post"}`),
		call(t, "slack_post", `{"text":"hi"}`),
	}})
	if !strings.Contains(req.System, "`chat-conventions` (required") {
		t.Fatalf("the catalogue does not list the skill as required:\n%s", req.System)
	}
	blocked := pub.blocked()
	if len(blocked) != 1 || blocked[0].ToolName != "slack_post" {
		t.Fatalf("the activated MCP tool ran without its required skill: refusals %+v", blocked)
	}
}

// LOADING UNLOCKS IT, in the same run and without another refusal.
func TestLoadingTheSkillUnlocksTheTool(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	req := guardedLaunch(t, pub, tools.OriginBuiltin, 1)
	bridged := play(t, req, run{calls: []llm.ToolCall{
		call(t, builtin.LoadToolSkillTool, `{"key":"chat-conventions"}`),
		call(t, "slack_post", `{"text":"hi"}`),
	}})
	if got := pub.blocked(); len(got) != 0 {
		t.Fatalf("a load-then-call sequence was blocked: %+v", got)
	}
	if posted := bridged[1]; posted.Failed || posted.Result != "posted" {
		t.Fatalf("the unlocked tool did not run: %+v", posted)
	}
}

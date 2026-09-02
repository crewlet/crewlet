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
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
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

// guardedRunner builds a runner whose executor surface carries one required
// skill covering the tool the model will reach for.
func guardedRunner(t *testing.T, prov *scriptedProvider, pub queue.Publisher) *runner.Runner {
	t.Helper()
	registry := skills.NewRegistry()
	if err := registry.Upsert(skills.Skill{
		Key: "chat-conventions", Title: "Chat conventions",
		Summary: "how this company writes on chat",
		Body:    "Always thread your reply.",
		Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "slack_post", out: "posted"},
		tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// THE REAL LOADER, not a stub: the unlock is observed from a
	// SUCCESSFUL call, so a stub that answered anything would unlock the
	// tool without ever having produced the body.
	if _, err := builtin.Register(reg, builtin.Deps{ToolSkills: registry}); err != nil {
		t.Fatalf("builtin.Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Caps:      runner.Caps{ExecutorRounds: 3},
		Task:      "post the summary",
		Publisher: pub,
		Turn:      runner.Turn{ID: "t-guard", AgentID: "agent-1"},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// THE GUARD REFUSES A COVERED TOOL, at the dispatch point every call goes
// through. Asking harder in the prompt does not stop a model going straight
// for the tool.
func TestTheExecutorIsRefusedAToolBeforeItsSkillIsLoaded(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, "slack_post", `{"text":"hi"}`),
		text("posted"),
	}}
	pub := newCapture()
	r := guardedRunner(t, prov, pub)

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The refusal names the exact call to make, because the error IS the
	// recovery path.
	var refusal string
	for _, m := range prov.requestsFor("execute")[1].Messages {
		if strings.Contains(m.Content, "load_tool_skill") {
			refusal = m.Content
		}
	}
	if refusal == "" {
		t.Fatal("the model was never told what to load")
	}
	if !strings.Contains(refusal, `load_tool_skill(key="chat-conventions")`) {
		t.Fatalf("the refusal does not name the call:\n%s", refusal)
	}
}

// EVERY REFUSAL IS PUBLISHED. Occasional blocks are the guard working; the
// chronic case — one skill blocked over and over — says the catalogue
// summary is not landing, and that is invisible in the turn's own record
// where a block looks like any other failed call.
func TestEveryRefusalIsPublishedForOperators(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, "slack_post", `{"text":"hi"}`),
		text("gave up"),
	}}
	pub := newCapture()
	r := guardedRunner(t, prov, pub)

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

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
	// EVERY pending skill, not just the one that blocked: a model about to
	// be blocked twice more is a model an operator needs to see whole.
	if !slices.Equal(got.SkillKeys, []string{"chat-conventions"}) {
		t.Fatalf("skill keys = %v", got.SkillKeys)
	}
}

// LOADING UNLOCKS IT, in the same session and without another round of
// refusals.
func TestLoadingTheSkillUnlocksTheTool(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, builtin.LoadToolSkillTool, `{"key":"chat-conventions"}`),
		submitCall(t, "slack_post", `{"text":"hi"}`),
		text("posted"),
	}}
	pub := newCapture()
	r := guardedRunner(t, prov, pub)

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := pub.blocked(); len(got) != 0 {
		t.Fatalf("a load-then-call sequence was blocked: %+v", got)
	}
}

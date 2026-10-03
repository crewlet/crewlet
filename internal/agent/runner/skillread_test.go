package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// What a phase's prompt offered from the knowledge base, and what its tools
// read: the pages behind a tool-skill catalogue are recorded as a
// `knowledge_read` with `via: skill_injected`, per rendered catalogue, and a
// tool call records the phase it ran in.

// reads is every knowledge_read a capture holds.
func (c *capture) reads() []*types.KnowledgeRead {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.KnowledgeRead
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.KnowledgeRead](ev); ok {
			out = append(out, got)
		}
	}
	return out
}

// skilledRunner builds a runner over two published skills: one about a tool the
// executor has, and one an operator scoped to Review on an MCP server the seat
// holds a credential for.
func skilledRunner(t *testing.T, prov *scriptedProvider, pub *capture) *runner.Runner {
	t.Helper()
	registry := skills.NewRegistry()
	registry.Replace([]skills.Skill{{
		Key: "chat-conventions", Title: "Chat conventions",
		Summary: "how this company writes on chat", Body: "Always thread your reply.",
		Trigger:      skills.Trigger{Tool: "slack_post"},
		SourcePageID: "pg-chat", SourceBackend: "native", SourceContainer: "SKILLS",
		SourceTitle: "Chat conventions page",
	}, {
		Key: "review-bar", Title: "Review bar",
		Summary: "REVIEW-BAR-SUMMARY", Body: "What a delivered change needs.",
		Trigger:      skills.Trigger{MCPServer: "github"},
		Phases:       []prompts.Phase{prompts.PhaseReview},
		SourcePageID: "pg-review", SourceBackend: "native", SourceContainer: "SKILLS",
		SourceTitle: "Review bar page",
	}})

	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "slack_post", out: "posted"},
		tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// THE REAL LOADER, publishing through the same capture, so a load made
	// inside a phase is seen with the phase the surface bound it to.
	if _, err := builtin.Register(reg, builtin.Deps{ToolSkills: registry, Events: pub}); err != nil {
		t.Fatalf("builtin.Register: %v", err)
	}
	role := &org.Role{
		Name: "CTO", DeclaredHandle: "cto",
		MCPEnv: org.MCPEnv{"github": {"TOKEN": "${GH}"}},
	}
	company := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: company, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Caps:      runner.Caps{ExecutorRounds: 3},
		Task:      "post the summary",
		Publisher: pub,
		Turn: runner.Turn{
			RunID: "t-read", WorkKey: "wk-read", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-read", WorkKey: "wk-read", Seat: role, Org: company},
		},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// THE EXECUTOR'S CATALOGUE IS RECORDED AGAINST ITS PAGES, once per prompt,
// and a skill the model then loads is recorded as a read in the phase that
// loaded it.
func TestTheExecutorsSkillCatalogueIsRecordedAsARead(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{
		submitCall(t, builtin.LoadToolSkillTool, `{"key":"chat-conventions"}`),
		submitWork(t),
	}}
	pub := newCapture()
	r := skilledRunner(t, prov, pub)

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var injected, loaded []*types.KnowledgeRead
	for _, read := range pub.reads() {
		switch read.Via {
		case types.ReadViaSkillInjected:
			injected = append(injected, read)
		case types.ReadViaSkillLoaded:
			loaded = append(loaded, read)
		}
	}
	if len(injected) != 1 {
		t.Fatalf("%d catalogue reads for one executor prompt, want one; saw %v",
			len(injected), pub.kinds())
	}
	got := injected[0]
	want := types.KnowledgeReadPage{ID: "pg-chat", Container: "SKILLS", Title: "Chat conventions page"}
	if got.Phase != types.PhaseExecute || got.Backend != "native" || got.AgentHandle != "cto" ||
		got.TurnID != "t-read" || len(got.Pages) != 1 || got.Pages[0] != want {
		t.Errorf("catalogue read = %+v; want the executor offered %+v", got, want)
	}
	// THE PHASE A TOOL SEES IS THE PHASE THAT CALLED IT: the surface binds
	// the turn as that phase, so the loader can say it ran in the executor.
	if len(loaded) != 1 || loaded[0].Phase != types.PhaseExecute ||
		len(loaded[0].Pages) != 1 || loaded[0].Pages[0].ID != "pg-chat" {
		t.Errorf("load reads = %+v; want one skill_loaded of pg-chat in execute", loaded)
	}
}

// A SKILL AN OPERATOR SCOPED TO REVIEW REACHES THE REVIEWER, and is recorded
// as offered there. The prompt, the parser and the docs all supported it and
// the runner never passed the registry to the reviewer's prompt, so a skill
// authored with `phases: [review]` reached nobody.
func TestAReviewScopedSkillReachesTheReviewer(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{review: []llm.Completion{
		submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`),
	}}
	pub := newCapture()
	r := skilledRunner(t, prov, pub)

	if _, err := r.Review(context.Background(), 1, workFor("posted"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	requests := prov.requestsFor("review")
	if len(requests) == 0 {
		t.Fatal("the reviewer was never asked")
	}
	system := requests[0].Messages[0].Content
	if !strings.Contains(system, "REVIEW-BAR-SUMMARY") {
		t.Fatalf("the review-scoped skill is not in the reviewer's prompt:\n%s", system)
	}
	var injected []*types.KnowledgeRead
	for _, read := range pub.reads() {
		if read.Via == types.ReadViaSkillInjected {
			injected = append(injected, read)
		}
	}
	if len(injected) != 1 || injected[0].Phase != types.PhaseReview ||
		len(injected[0].Pages) != 1 || injected[0].Pages[0].ID != "pg-review" {
		t.Errorf("review catalogue reads = %+v; want one naming pg-review", injected)
	}
}

// A WORKER'S CATALOGUE IS RECORDED IN THE SUBAGENT PHASE, once per worker
// prompt. A worker's prompt is built inside the subagent package, so its offer
// is drained on the one hook that package calls on every path a child takes —
// and a worker offered a skill whose page nobody counted would leave the pages
// a company's fan-outs are shown reading as unread.
func TestAWorkersSkillCatalogueIsRecordedInTheSubagentPhase(t *testing.T) {
	t.Parallel()
	registry := skills.NewRegistry()
	registry.Replace([]skills.Skill{{
		Key: "research-method", Title: "Research method",
		Summary: "how this company researches", Body: "Cite sources.",
		Trigger:      skills.Trigger{MCPServer: "github"},
		Phases:       []prompts.Phase{prompts.PhaseSubagent},
		SourcePageID: "pg-research", SourceBackend: "native", SourceContainer: "SKILLS",
		SourceTitle: "Research method page",
	}})
	prov := &delegatingProvider{}
	pub := newCapture()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "read_file", out: "contents"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	role := &org.Role{
		Name: "CTO", DeclaredHandle: "cto",
		MCPEnv: org.MCPEnv{"github": {"TOKEN": "${GH}"}},
	}
	company := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: company, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Caps:      runner.Caps{ExecutorRounds: 6},
		Task:      "fan this out",
		Publisher: pub,
		Subagent:  &runner.SubagentConfig{Limits: shipped()},
		Turn: runner.Turn{RunID: "t-fan", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-fan", Seat: role, Org: company}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var worker []*types.KnowledgeRead
	for _, read := range pub.reads() {
		if read.Via == types.ReadViaSkillInjected && read.Phase == types.PhaseSubagent {
			worker = append(worker, read)
		}
	}
	if len(worker) != 1 || len(worker[0].Pages) != 1 || worker[0].Pages[0].ID != "pg-research" {
		t.Fatalf("worker catalogue reads = %+v; want one naming pg-research (saw %v)",
			worker, pub.kinds())
	}
}

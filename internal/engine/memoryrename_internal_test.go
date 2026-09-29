package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
)

// A RENAMED SEAT KEEPS WHAT IT LEARNED.
//
// A seat's identity is the handle it was CREATED under (ADR-0019), and its
// mailbox, lease, diary and onboarding marker already followed it through a
// rename. Five tables did not: episodes, counterparty profiles, the two skill
// tables and the conversation ledger named the seat by the handle it answered
// to when it learned something, and every reader asked by the handle it answers
// to now — so a renamed seat recalled none of its earlier work, loaded none of
// its skills, knew nobody, and answered a thread it had already answered as if
// for the first time.
//
// So this case writes every kind of that memory through the REAL writers — the
// reflect dispatcher's workers and the dispatcher's conversation record —
// renames both the seat and the colleague it learned about, and reads it all
// back through the REAL readers a renamed seat's next turn uses: the turn-start
// prefetch, the memory tools, and the thread history the dispatcher hands a
// turn.
func TestARenamedSeatKeepsItsEpisodesSkillsProfilesAndLedger(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "memory.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	embed := embeddings.NewFake(64)

	// BEFORE: the seat and the colleague it deals with, as created.
	before := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Sarah Chen"},
		{Name: "Pat Lee"},
	}}
	// AFTER: both renamed. What a rename changes is the address; the
	// origin is frozen by the first rename and the retired handle kept as
	// an alias, exactly as the chart writes it.
	after := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Sarah Okonkwo", DeclaredHandle: "sarah-okonkwo",
			OriginHandle: "sarah-chen", FormerHandles: []string{"sarah-chen"}},
		{Name: "Pat Lee-Moss", DeclaredHandle: "pat-moss",
			OriginHandle: "pat-lee", FormerHandles: []string{"pat-lee"}},
	}}
	sarahThen, sarahNow := before.Roles[0], after.Roles[0]
	agentID, ok := after.AgentIDFor(sarahNow)
	if !ok {
		t.Fatal("the renamed seat has no agent id")
	}

	// ---- write, before the rename, through the real writers ---------- //

	const (
		task         = "ship the release to production"
		conversation = "slack:C1:1700000000.000100"
		skillName    = "cut-a-release"
		trait        = "prefers thursday releases"
	)
	models := scriptedModels{}
	episodes, skills := learning.NewEpisodes(db), learning.NewSkills(db)
	counterparties := learning.NewCounterparties(db)
	episodist, err := learning.NewEpisodist(episodes, learning.EpisodistOptions{Embed: embed.Embed})
	if err != nil {
		t.Fatalf("NewEpisodist: %v", err)
	}
	synthesizer, err := learning.NewSynthesizer(models, skills,
		learning.SynthesizerOptions{MinToolCalls: 1})
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
	profiler, err := learning.NewProfiler(models, counterparties, learning.ProfilerOptions{})
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}
	reflector, err := learning.NewReflector(before, discardPublisher{},
		[]learning.Worker{episodist, synthesizer, profiler}, nil)
	if err != nil {
		t.Fatalf("NewReflector: %v", err)
	}
	at := time.Now().UTC().Add(-time.Hour)
	pass := reflector.Reflect(ctx, types.TurnCompleted{
		Agent: agentID.String(), AgentHandle: sarahThen.Handle(), RoleName: sarahThen.Name,
		TurnID: "run-1", WorkKey: "work-1",
		StartedAt: at, EndedAt: at.Add(time.Minute), DurationMS: 60_000,
		TaskSummary: task, PlanSummary: "cut, tag, announce",
		ToolSequence:    []string{"fetch", "build", "tag"},
		ReviewOutcome:   "done",
		ConversationKey: conversation,
		Interactions: []types.InboundInteraction{{
			Sender: types.CanonicalIdentity{Handle: "pat-lee", DisplayName: "Pat Lee"},
			Body:   "please ship it, and on thursdays from now on",
		}},
	}, events.TraceContext{})
	if pass.Skip != "" || len(pass.Failed) != 0 || len(pass.Ran) != 3 {
		t.Fatalf("the reflection pass that writes the memory did not run whole: %+v", pass)
	}

	company := &Company{Org: before}
	dispatcher := &Dispatcher{
		Conversations: ledgerstore.NewConversations(db),
		// The engine's own resolver, over whichever company is live.
		Origin: func(handle string) string { return seatOrigin(company, handle) },
	}
	dispatcher.RecordSession(ctx, sarahThen.Handle(), conversation, "run-1", "work-1",
		"@pat: please ship it", turn.Result{
			Decision: phase.Done, Delivered: true, Artifact: "shipped v1.4",
		}, at.Add(time.Minute))

	// ---- the rename ---------------------------------------------------- //

	company = &Company{Org: after}
	if err := reflector.Reconfigure(after,
		[]learning.Worker{episodist, synthesizer, profiler}, nil); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}

	// ---- write again, after it, under the seat's NEW handle ---------- //
	//
	// A turn the renamed seat completes is published under the handle it
	// answers to now, and so is its colleague's message. What it learns
	// has to land beside what it learned before, or the seat's memory
	// splits in two at the rename.
	at = at.Add(10 * time.Minute)
	pass = reflector.Reflect(ctx, types.TurnCompleted{
		Agent: agentID.String(), AgentHandle: sarahNow.Handle(), RoleName: sarahNow.Name,
		TurnID: "run-2", WorkKey: "work-2",
		StartedAt: at, EndedAt: at.Add(time.Minute), DurationMS: 60_000,
		TaskSummary: "announce the release to the team", PlanSummary: "post, link, thank",
		ToolSequence:    []string{"fetch", "build", "tag"},
		ReviewOutcome:   "done",
		ConversationKey: conversation,
		Interactions: []types.InboundInteraction{{
			Sender: types.CanonicalIdentity{Handle: "pat-moss", DisplayName: "Pat Lee-Moss"},
			Body:   "thanks — announce it too, please",
		}},
	}, events.TraceContext{})
	if pass.Skip != "" || len(pass.Failed) != 0 {
		t.Fatalf("the renamed seat's reflection pass did not run: %+v", pass)
	}
	dispatcher.RecordSession(ctx, sarahNow.Handle(), conversation, "run-2", "work-2",
		"@pat: announce it too", turn.Result{
			Decision: phase.Done, Delivered: true, Artifact: "announced v1.4",
		}, at.Add(time.Minute))

	// EVERY ROW NAMES THE SEAT BY ONE HANDLE — the one it was created under
	// — whichever handle it answered to when it wrote the row, and so does
	// every profile of the colleague. Two spellings of one seat in a table
	// is the split this case exists to catch.
	for _, column := range []struct{ table, column, want string }{
		{"episodes", "agent_handle", "sarah-chen"},
		{"synthesized_skills", "agent_handle", "sarah-chen"},
		{"counterparty_profiles", "observer_handle", "sarah-chen"},
		{"counterparty_profiles", "subject_handle", "pat-lee"},
		{"conversation_sessions", "agent_handle", "sarah-chen"},
	} {
		var spellings []string
		rows, err := db.SQL().QueryContext(ctx,
			"SELECT DISTINCT "+column.column+" FROM "+column.table)
		if err != nil {
			t.Fatalf("read %s: %v", column.table, err)
		}
		for rows.Next() {
			var spelling string
			if err := rows.Scan(&spelling); err != nil {
				t.Fatalf("scan %s: %v", column.table, err)
			}
			spellings = append(spellings, spelling)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close %s: %v", column.table, err)
		}
		if len(spellings) != 1 || spellings[0] != column.want {
			t.Errorf("%s.%s holds %v, want only %q — the handle the seat was "+
				"created under", column.table, column.column, spellings, column.want)
		}
	}

	// ---- read, after it, through the real readers -------------------- //

	// THE TURN-START PREFETCH: similar prior work, the skill catalogue, and
	// what the seat knows about the colleague who woke it — who is named by
	// the handle they answer to NOW.
	blocks := prefetch.New(prefetch.Sources{
		Episodes: episodes, Counterparties: counterparties, Skills: skills,
		Embed: embed.Embed,
	}).Fetch(ctx, prefetch.Request{
		Seat: sarahNow, AgentID: agentID.String(), Org: after,
		Task: task, TurnID: "run-2",
		Senders: []learning.Subject{{Handle: "pat-moss", Name: "Pat Lee-Moss"}},
	})
	if !strings.Contains(blocks.EpisodeRecall, task) {
		t.Errorf("the renamed seat's prefetch recalled none of the work it did "+
			"before the rename:\n%s", blocks.EpisodeRecall)
	}
	if !strings.Contains(blocks.SynthesizedSkills, skillName) {
		t.Errorf("the renamed seat's prefetch offered none of the skills it drafted "+
			"before the rename:\n%s", blocks.SynthesizedSkills)
	}
	if !strings.Contains(blocks.CounterpartyProfile, trait) {
		t.Errorf("the renamed seat knows nothing of the renamed colleague it "+
			"profiled before both renames:\n%s", blocks.CounterpartyProfile)
	}
	if strings.Contains(blocks.CounterpartyProfile, "pat-lee") {
		t.Errorf("the profile names the colleague by a handle they retired:\n%s",
			blocks.CounterpartyProfile)
	}
	if !strings.Contains(blocks.CounterpartyProfile, "interactions: 2") {
		t.Errorf("the two interactions either side of the renames were counted "+
			"into two profiles of one person:\n%s", blocks.CounterpartyProfile)
	}

	// THE MEMORY TOOLS, called in the renamed seat's own turn.
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Skills: skills, Refinable: skills, Episodes: episodes,
		Authorize: builtin.Decide(authz.NoChart{}),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	now := &turnctx.Turn{RunID: "run-2", WorkKey: "work-2", Seat: sarahNow, Org: after}
	out := callSeatTool(t, reg, builtin.QueryEpisodesTool, now,
		map[string]any{"conversation": conversation})
	for _, want := range []string{task, "announce the release to the team"} {
		if !strings.Contains(out, want) {
			t.Errorf("query_episodes is missing the turn %q from this thread, "+
				"one side of the rename:\n%s", want, out)
		}
	}
	if out := callSeatTool(t, reg, builtin.UseSkillTool, now,
		map[string]any{"skill_name": skillName}); !strings.Contains(out, "# "+skillName) {
		t.Errorf("use_skill could not load a skill drafted before the rename:\n%s", out)
	}
	if out := callSeatTool(t, reg, builtin.RefineSkillTool, now, map[string]any{
		"skill_name": skillName, "content": "1. fetch\n2. build\n3. tag on thursday",
	}); !strings.Contains(out, "now version 2") {
		t.Errorf("refine_skill could not refine a skill drafted before the rename:\n%s", out)
	}
	var archivedUnder string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT agent_handle FROM synthesized_skill_versions`).Scan(&archivedUnder); err != nil {
		t.Fatalf("the refinement archived no prior version: %v", err)
	}
	if archivedUnder != sarahThen.Handle() {
		t.Errorf("the archived version is filed under %q, want the handle the seat "+
			"was created under (%q)", archivedUnder, sarahThen.Handle())
	}

	// THE THREAD HISTORY the dispatcher hands the renamed seat's next turn in
	// the same thread — which is what stops it answering twice.
	history, err := dispatcher.history(ctx, sarahNow.Handle(), conversation)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 || history[0].TurnID != "run-1" || history[1].TurnID != "run-2" {
		t.Errorf("the renamed seat's history in a thread it already answered is "+
			"%+v, want the entry it filed before the rename and the one after", history)
	}
}

// callSeatTool calls one registered builtin in a seat's turn and returns what
// it said, failing on a refusal: every call here is the seat reading its own
// memory, which nothing refuses.
func callSeatTool(t *testing.T, reg *tools.Registry, name string, tn *turnctx.Turn,
	args map[string]any) string {
	t.Helper()
	entry, ok := reg.Snapshot().Lookup(name)
	if !ok {
		t.Fatalf("%s was not registered", name)
	}
	seated, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s cannot know which seat called it", name)
	}
	res, err := seated.CallForTurn(t.Context(), tn, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.Failed {
		t.Fatalf("%s refused: %s", name, res.Output)
	}
	return res.Output
}

// scriptedModels answers the two auxiliary calls the writers above make: the
// synthesizer drafts one skill, and the profiler records one trait.
type scriptedModels struct{}

func (scriptedModels) Head(*org.Role, phase.Phase) (chain.Member, error) {
	return chain.Member{Key: "scripted", Provider: scriptedProvider{}}, nil
}

type scriptedProvider struct{}

func (scriptedProvider) Model() string { return "scripted" }

func (scriptedProvider) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	system := ""
	if len(req.Messages) > 0 {
		system = req.Messages[0].Content
	}
	switch system {
	case learning.SynthesisSystemPrompt:
		return &llm.Completion{Content: `{"name":"cut-a-release",` +
			`"description":"Ship a tagged release","content":"1. fetch\n2. build\n3. tag"}`}, nil
	case learning.ProfilerSystemPrompt:
		return &llm.Completion{Content: `{"release_day":"prefers thursday releases"}`}, nil
	}
	return &llm.Completion{}, nil
}

// discardPublisher takes the lifecycle events a reflection pass announces and
// keeps none: this case is about the rows, not the feed.
type discardPublisher struct{}

func (discardPublisher) Publish(context.Context, string, *events.Event) error { return nil }

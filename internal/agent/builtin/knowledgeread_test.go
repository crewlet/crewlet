package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
)

// The three tools a seat reads the knowledge base through, and the record each
// leaves: one `knowledge_read` per call, naming the pages it reached — and
// nothing at all for a call that reached none.

// rankedSearcher answers every search with the same hits.
type rankedSearcher struct{ hits []knowledge.Hit }

func (rankedSearcher) CanSearch(*org.Role, *org.Organization) bool { return true }

func (s rankedSearcher) Search(context.Context, knowledge.Query) []knowledge.Hit {
	return s.hits
}

// pageSkills serves one tool skill read from a named page.
type pageSkills struct{ loaded skills.Loaded }

func (p pageSkills) Load(key string) (skills.Loaded, bool) {
	if key != "deploys" {
		return skills.Loaded{}, false
	}
	return p.loaded, true
}

// reads is every knowledge_read a spy was handed.
func reads(t *testing.T, out *recordingTelemetry) []*types.KnowledgeRead {
	t.Helper()
	var got []*types.KnowledgeRead
	for _, ev := range out.sent {
		if read, ok := ev.Data.(*types.KnowledgeRead); ok {
			got = append(got, read)
		}
	}
	return got
}

// executing is a turn bound to the executor, the way a phase surface binds it.
func executing(t *testing.T) *turnctx.Turn {
	t.Helper()
	return turnFor(t, "agent-ceo").InPhase(types.PhaseExecute)
}

// A PAGE READ IN FULL IS RECORDED AGAINST THE PAGE, in the phase that read it,
// with the backend its id is an address in.
func TestReadingAPageRecordsWhichPage(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{}
	kb := newFakeKB()
	tool := registered(t, builtin.Deps{Pages: builtin.PageDeps{Reader: kb}, Events: out},
		builtin.GetPageTool)

	turn := executing(t)
	if res := callFor(t, tool, turn, map[string]any{"page": "p1"}); res.Failed {
		t.Fatalf("get_page failed: %q", res.Output)
	}
	got := reads(t, out)
	if len(got) != 1 {
		t.Fatalf("published %d reads, want one per call (topics %v)", len(got), out.topics)
	}
	read := got[0]
	if read.Via != types.ReadViaGetPage || read.Backend != "native" ||
		read.Phase != types.PhaseExecute {
		t.Errorf("read = via %q backend %q phase %q; want get_page/native/execute",
			read.Via, read.Backend, read.Phase)
	}
	want := types.KnowledgeReadPage{ID: "p1", Container: "ENG", Title: "Deploy Runbook"}
	if len(read.Pages) != 1 || read.Pages[0] != want {
		t.Errorf("pages = %+v; want exactly %+v", read.Pages, want)
	}
	if read.AgentHandle != "agent-ceo" || read.TurnID != turn.RunID || read.WorkKey != turn.WorkKey {
		t.Errorf("the read does not place itself: handle %q turn %q key %q",
			read.AgentHandle, read.TurnID, read.WorkKey)
	}
	if out.sent[0].Source != "agent-ceo" {
		t.Errorf("source = %q, want the seat", out.sent[0].Source)
	}

	// A PAGE THAT WAS NOT THERE WAS NOT READ.
	before := len(out.sent)
	if res := callFor(t, tool, turn, map[string]any{"page": "nope"}); !res.Failed {
		t.Fatal("a missing page was served")
	}
	if len(out.sent) != before {
		t.Error("a page that was not found was recorded as read")
	}
}

// A SEARCH IS ONE READ LISTING WHAT IT SHOWED, in the order it showed it —
// and a search that showed nothing is no read at all.
func TestASearchRecordsItsRankedPagesAndItsQuery(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{}
	search := rankedSearcher{hits: []knowledge.Hit{
		{PageID: "a", Container: "ENG", Title: "Deploys", Backend: "confluence"},
		{Title: "unaddressable"},
		{PageID: "c", Container: "OPS", Title: "Rollbacks", Backend: "confluence"},
	}}
	tool := registered(t, builtin.Deps{Knowledge: search, Events: out}, builtin.SearchKnowledgeTool)

	callFor(t, tool, executing(t), map[string]any{"query": "deploy rollback"})
	got := reads(t, out)
	if len(got) != 1 {
		t.Fatalf("published %d reads, want one per search", len(got))
	}
	read := got[0]
	if read.Via != types.ReadViaSearch || read.Query != "deploy rollback" ||
		read.Backend != "confluence" {
		t.Errorf("read = via %q query %q backend %q", read.Via, read.Query, read.Backend)
	}
	// THE RANK IS THE POSITION SHOWN: the hit with no id keeps its place in
	// the count although nothing can address it.
	want := []types.KnowledgeReadPage{
		{ID: "a", Container: "ENG", Title: "Deploys", Rank: 1},
		{ID: "c", Container: "OPS", Title: "Rollbacks", Rank: 3},
	}
	if len(read.Pages) != len(want) || read.Pages[0] != want[0] || read.Pages[1] != want[1] {
		t.Errorf("pages = %+v; want %+v", read.Pages, want)
	}

	empty := &recordingTelemetry{}
	none := registered(t, builtin.Deps{Knowledge: rankedSearcher{}, Events: empty},
		builtin.SearchKnowledgeTool)
	callFor(t, none, executing(t), map[string]any{"query": "nothing here"})
	if len(empty.sent) != 0 {
		t.Errorf("a search with no hits published %d events", len(empty.sent))
	}
}

// A LONG QUERY IS CLIPPED ON A RUNE, never through one. The record is a label,
// and a clip that split a multi-byte rune would be invalid UTF-8 on the wire.
func TestASearchRecordsALongQueryClippedOnARune(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{}
	tool := registered(t, builtin.Deps{
		Knowledge: rankedSearcher{hits: []knowledge.Hit{{PageID: "a", Title: "A"}}},
		Events:    out,
	}, builtin.SearchKnowledgeTool)
	query := strings.Repeat("é", 150) // 300 bytes, under the tool's own cap
	callFor(t, tool, executing(t), map[string]any{"query": query})
	got := reads(t, out)
	if len(got) != 1 {
		t.Fatalf("published %d reads", len(got))
	}
	if q := got[0].Query; q != types.KnowledgeReadQuery(query) || len(q) > types.KnowledgeReadQueryMax {
		t.Errorf("query = %d bytes %q; want it clipped to %d by the one rule",
			len(q), q, types.KnowledgeReadQueryMax)
	}
}

// LOADING A TOOL SKILL IS READING ITS PAGE: the load names the page it came
// from, and a read is recorded against that page beside it.
func TestLoadingAToolSkillRecordsItsPage(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{}
	tool := registered(t, builtin.Deps{
		ToolSkills: pageSkills{loaded: skills.Loaded{
			Body: "# Deploys\n", PageID: "pg-9", Backend: "native",
			Container: "SKILLS", Title: "Deploy skill",
		}},
		Events: out,
	}, builtin.LoadToolSkillTool)

	if res := callFor(t, tool, executing(t), map[string]any{"key": "deploys"}); res.Failed {
		t.Fatalf("load_tool_skill failed: %q", res.Output)
	}
	var used *types.SkillUsed
	for _, ev := range out.sent {
		if u, ok := ev.Data.(*types.SkillUsed); ok {
			used = u
		}
	}
	if used == nil || used.SourcePageID != "pg-9" || used.SourceContainer != "SKILLS" {
		t.Errorf("skill_used = %+v; want it to name page pg-9 in SKILLS", used)
	}
	got := reads(t, out)
	if len(got) != 1 {
		t.Fatalf("published %d reads beside the load, want one", len(got))
	}
	want := types.KnowledgeReadPage{ID: "pg-9", Container: "SKILLS", Title: "Deploy skill"}
	if got[0].Via != types.ReadViaSkillLoaded || got[0].Backend != "native" ||
		len(got[0].Pages) != 1 || got[0].Pages[0] != want {
		t.Errorf("read = %+v; want skill_loaded of %+v", got[0], want)
	}

	// An unknown key loaded nothing, so it read nothing.
	before := len(out.sent)
	callFor(t, tool, executing(t), map[string]any{"key": "missing"})
	if len(out.sent) != before {
		t.Error("an unknown skill key was recorded")
	}
}

// A READ WITH NO SEAT IS NOT A SEAT'S READ. The operator surface serves the
// same tools to a person, whose actions its own audit records; a knowledge_read
// with no agent would count toward every "read by agents" total.
func TestAReadOutsideATurnRecordsNothing(t *testing.T) {
	t.Parallel()
	out := &recordingTelemetry{}
	tool := registered(t, builtin.Deps{Pages: builtin.PageDeps{
		Reader: newFakeKB(),
		Actor: func(context.Context, *turnctx.Turn) (pages.Actor, error) {
			return pages.Actor{Handle: "operator"}, nil
		},
	}, Events: out}, builtin.GetPageTool)
	if res := callFor(t, tool, nil, map[string]any{"page": "p1"}); res.Failed {
		t.Fatalf("get_page failed for an operator: %q", res.Output)
	}
	if len(out.sent) != 0 {
		t.Errorf("a read with no seat published %d events", len(out.sent))
	}
}

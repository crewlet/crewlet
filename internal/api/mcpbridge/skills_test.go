package mcpbridge_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// handed is the one skill a fixture's harness was handed as its own: listed as
// crewlet:mcp-github, standing for the Tool Skill mcp:github.
var handed = map[string]string{"crewlet:mcp-github": "mcp:github"}

// skillsFixture is a seat's session over a surface holding the engine's own
// read_page and its skill loader, opened on a bridge serving every route a box
// dials: the job's endpoint and its skill route.
type skillsFixture struct {
	loader   *stubTool
	ledger   *ledger
	server   *httptest.Server
	endpoint string
}

// newSkillsFixture opens the session, its harness handed skills, by the name it
// lists each under to the key it stands for — nil for one handed none.
func newSkillsFixture(t *testing.T, skills map[string]string) *skillsFixture {
	t.Helper()
	reg := tools.NewRegistry()
	f := &skillsFixture{ledger: &ledger{},
		loader: &stubTool{name: toolname.LoadToolSkill, out: "how this company uses GitHub"}}
	for _, tool := range []*stubTool{{name: "read_page"}, f.loader} {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register(%s): %v", tool.name, err)
		}
	}
	surface := tools.NewSurface("execute", reg.Snapshot(),
		[]string{"read_page", toolname.LoadToolSkill}).ForTurn(seatTurn())
	f.server = httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(f.server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL,
	})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.SkillPattern, bridge.SkillHandler())
	f.server.Config.Handler = mux
	f.endpoint = bridge.Open(t.Context(), &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: surface, Ledger: f.ledger, Skills: skills,
	})
	if f.endpoint == "" {
		t.Fatal("the bridge opened no session")
	}
	return f
}

// loads is the keys of the loads the run's record holds, in its order.
func (f *skillsFixture) loads(t *testing.T) []string {
	t.Helper()
	var keys []string
	for _, row := range f.ledger.rows() {
		if row.Name != toolname.LoadToolSkill {
			t.Errorf("the run's record holds a call of %s; only loads were reported", row.Name)
			continue
		}
		key, _ := row.Args["key"].(string)
		keys = append(keys, key)
	}
	return keys
}

// A LOAD THE HARNESS REPORTS IS THE RUN'S OWN LOAD of the Tool Skill its name
// stands for: a call of load_tool_skill with the skill's key, made on the run's
// surface and recorded on its record as every call of the run's is — what the
// required-skill guard unlocks by, and what every rebuild of the run replays.
// The hook that reported it is answered with empty text, never an error.
//
// Mutation: record the load under the name the harness listed it by, and the
// loader is handed a key no registry holds; answer the hook with anything but
// empty text, and Claude Code reads it as the hook's output.
func TestALoadTheHarnessReportsIsTheRunsOwnLoad(t *testing.T) {
	t.Parallel()
	f := newSkillsFixture(t, handed)
	box := dial(t, f.endpoint)
	res := callTool(t, box, mcpbridge.SkillTool, map[string]any{"skill": "crewlet:mcp-github"})
	if res.IsError || text(res) != "" {
		t.Fatalf("the hook's call was answered %q (error %v), want empty text", text(res), res.IsError)
	}
	if got := f.loads(t); !slices.Equal(got, []string{"mcp:github"}) {
		t.Fatalf("the run's record holds the loads %v, want mcp:github's", got)
	}
	if seen := f.loader.seen(); len(seen) != 1 || seen[0]["key"] != "mcp:github" {
		t.Errorf("the loader was handed %v, want the skill's key", seen)
	}
}

// A NAME THE RUN WAS NOT HANDED IS NONE OF ITS TOOL SKILLS: a load of the
// checkout's own skill, a name without the namespace it was listed under, or
// a variable the harness never filled in is answered as quietly as a load is,
// and recorded nowhere.
func TestASkillTheRunWasNotHandedIsRecordedNowhere(t *testing.T) {
	t.Parallel()
	f := newSkillsFixture(t, handed)
	box := dial(t, f.endpoint)
	for _, name := range []string{"pdf", "mcp-github", "${tool_input.skill}", ""} {
		if res := callTool(t, box, mcpbridge.SkillTool, map[string]any{"skill": name}); res.IsError || text(res) != "" {
			t.Errorf("a report of %q was answered %q (error %v), want empty text", name, text(res), res.IsError)
		}
	}
	if got := f.loads(t); len(got) != 0 {
		t.Errorf("the run's record holds the loads %v, want none", got)
	}
}

// THE HARNESS'S OWN TOOL IS ITS LOADER: a harness handed skills of its own is
// listed neither the bridge's loader — which would put a skill's text in front
// of its model a second way — nor the hook's tool, which no model may call. A
// run handed none is listed the loader as any other tool.
//
// Mutation: list load_tool_skill to every harness, and a run handed its skills
// natively is offered two loaders; list skill_loaded, and its model can unlock a
// skill it never read.
func TestTheBridgesLoaderIsNotListedToAHarnessThatLoadsItsOwn(t *testing.T) {
	t.Parallel()
	if names, _ := listed(t, newSkillsFixture(t, handed).endpoint); !slices.Equal(names, []string{"read_page"}) {
		t.Errorf("a harness handed its own skills is listed %v, want read_page alone", names)
	}
	if names, _ := listed(t, newSkillsFixture(t, nil).endpoint); !slices.Equal(names,
		[]string{toolname.LoadToolSkill, "read_page"}) {
		t.Errorf("a harness handed no skills is listed %v, want the loader beside read_page", names)
	}
}

// THE SKILL ROUTE IS A PLUGIN'S WAY IN, read the same way: a report posted
// there is the run's load as a hook's is. A session handed no skills has no
// route, and only a POST of a name is a report.
func TestTheSkillRouteRecordsAPluginsReport(t *testing.T) {
	t.Parallel()
	send := func(f *skillsFixture, method, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, mcpbridge.SkillURL(f.endpoint),
			strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, req.URL, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	f := newSkillsFixture(t, handed)
	if code := send(f, http.MethodPost, `{"skill":"crewlet:mcp-github"}`); code != http.StatusNoContent {
		t.Fatalf("a plugin's report was answered %d, want 204", code)
	}
	if code := send(f, http.MethodPost, `{"skill":"pdf"}`); code != http.StatusNoContent {
		t.Errorf("a report of a skill the run was not handed was answered %d, want 204", code)
	}
	if got := f.loads(t); !slices.Equal(got, []string{"mcp:github"}) {
		t.Errorf("the run's record holds the loads %v, want mcp:github's alone", got)
	}
	if code := send(f, http.MethodGet, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("a GET of the route was answered %d, want 405", code)
	}
	if code := send(f, http.MethodPost, "not a report"); code != http.StatusBadRequest {
		t.Errorf("a body that is no report was answered %d, want 400", code)
	}
	if code := send(newSkillsFixture(t, nil), http.MethodPost, `{"skill":"crewlet:mcp-github"}`); code != http.StatusNotFound {
		t.Errorf("a session handed no skills answered its route %d, want 404", code)
	}
}

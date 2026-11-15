package assistant_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// page answers GET /research/page with one page of a vendor's documentation.
var page = assistanttest.Routes(map[string]exec.Response{
	"GET /research/page": evals.JSON(map[string]any{"url": "https://docs.example.com/rules",
		"host": "docs.example.com", "title": "Origin rules", "from": 0, "length": 40,
		"text":    "Host header overrides need the Enterprise plan.",
		"read_at": "2026-10-24T09:41:00Z"}),
	"GET /research/search": evals.JSON(map[string]any{"query": "host header override",
		"results": []map[string]any{{"title": "Origin rules", "url": "https://docs.example.com/rules",
			"snippet": "Host header overrides need the Enterprise plan."}}}),
})

func researchCompany(t *testing.T) assistant.Company { return webCompany(t, false) }

func webCompany(t *testing.T, search bool) assistant.Company {
	t.Helper()
	c, err := evals.ResearchCompany([]string{"docs.example.com"}, search, "grace")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The web read is offered only while the company lists research hosts, and
// the search only while it names a search server too.
func TestTheWebIsOfferedOnlyWhereTheCompanyListsHosts(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		company      func(t *testing.T) assistant.Company
		read, search bool
	}{
		"no research":  {func(t *testing.T) assistant.Company { return assistanttest.Company(t, true, "grace") }, false, false},
		"hosts listed": {researchCompany, true, false},
		"search named": {func(t *testing.T) assistant.Company { return webCompany(t, true) }, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := assistanttest.NewRig(t, evals.RigOptions{Company: c.company(t),
				Tools: []catalog.Tool{getTask(t)}, Rounds: []evals.Round{evals.Say("Hello.")}})
			accepted, err := f.Ask("offer", "What can you do?", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			tools := offered(t, f)
			if read, search := slices.Contains(tools, catalog.FetchWebPageToolName),
				slices.Contains(tools, catalog.SearchWebToolName); read != c.read || search != c.search {
				t.Fatalf("the web read offered %v and the search %v, want %v and %v",
					read, search, c.read, c.search)
			}
		})
	}
}

// A page reaches the model fenced as the site's words, and a turn reads at most
// eight: the ninth is refused before anything is sent.
func TestATurnReadsAFewPagesFencedAsTheSites(t *testing.T) {
	t.Parallel()
	read, refused := readTheWeb(t, catalog.FetchWebPageToolName,
		map[string]any{"url": "https://docs.example.com/rules"}, 9)
	if read != 8 || refused != 1 {
		t.Errorf("the turn read %d pages fenced as the site's and refused %d, want 8 and 1",
			read, refused)
	}
}

// Search results reach the model fenced as the sites' words, and a turn
// searches at most four times.
func TestATurnSearchesAFewTimesFencedAsTheSites(t *testing.T) {
	t.Parallel()
	read, refused := readTheWeb(t, catalog.SearchWebToolName,
		map[string]any{"query": "host header override"}, 5)
	if read != 4 || refused != 1 {
		t.Errorf("the turn searched %d times fenced as the sites' and was refused %d, want 4 and 1",
			read, refused)
	}
}

// readTheWeb calls tool calls times in one turn, and counts the answers fenced
// as a site's and the calls refused for the turn's bound.
func readTheWeb(t *testing.T, tool string, args map[string]any, calls int) (read, refused int) {
	t.Helper()
	rounds := make([]evals.Round, 0, calls+1)
	for i := range calls {
		rounds = append(rounds, evals.Call("w"+string(rune('a'+i)), tool, args))
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Company: webCompany(t, true), Handler: page,
		Rounds: append(rounds, evals.Say("The Pro plan is not enough."))})
	accepted, err := f.Ask("web", "Does the Pro plan allow Host overrides?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	for _, m := range f.Model.Seen() {
		if m.Role != llm.RoleTool {
			continue
		}
		switch {
		case m.Failed && strings.Contains(m.Content, "at most"):
			refused++
		case strings.Contains(m.Content, "source=web_page trust=external") &&
			strings.Contains(m.Content, "Enterprise plan"):
			read++
		}
	}
	return read, refused
}

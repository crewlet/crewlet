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
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// page answers GET /research/page with one page of a vendor's documentation.
var page = assistanttest.Routes(map[string]exec.Response{
	"GET /research/page": evals.JSON(map[string]any{"url": "https://docs.example.com/rules",
		"host": "docs.example.com", "title": "Origin rules", "from": 0, "length": 40,
		"text":    "Host header overrides need the Enterprise plan.",
		"read_at": "2026-10-24T09:41:00Z"}),
})

func researchCompany(t *testing.T) assistant.Company {
	t.Helper()
	c, err := evals.ResearchCompany([]string{"docs.example.com"}, "grace")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The web read is offered only while the company lists research hosts.
func TestTheWebIsOfferedOnlyWhereTheCompanyListsHosts(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		company func(t *testing.T) assistant.Company
		want    bool
	}{
		"no research":  {func(t *testing.T) assistant.Company { return assistanttest.Company(t, true, "grace") }, false},
		"hosts listed": {researchCompany, true},
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
			if got := slices.Contains(offered(t, f), catalog.FetchWebPageToolName); got != c.want {
				t.Fatalf("the web read offered %v, want %v", got, c.want)
			}
		})
	}
}

// A page reaches the model fenced as the site's words, and a turn reads at most
// eight: the ninth is refused before anything is sent.
func TestATurnReadsAFewPagesFencedAsTheSites(t *testing.T) {
	t.Parallel()
	fetch := map[string]any{"url": "https://docs.example.com/rules"}
	rounds := make([]evals.Round, 0, 10)
	for i := range 9 {
		rounds = append(rounds, evals.Call("f"+string(rune('a'+i)), catalog.FetchWebPageToolName, fetch))
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Company: researchCompany(t), Handler: page,
		Rounds: append(rounds, evals.Say("The Pro plan is not enough."))})
	accepted, err := f.Ask("web", "Does the Pro plan allow Host overrides?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var read, refused int
	for _, m := range f.Model.Seen() {
		if m.Role != llm.RoleTool {
			continue
		}
		switch {
		case m.Failed && strings.Contains(m.Content, "at most 8"):
			refused++
		case strings.Contains(m.Content, "source=web_page trust=external") &&
			strings.Contains(m.Content, "Enterprise plan"):
			read++
		}
	}
	if read != 8 || refused != 1 {
		t.Errorf("the turn read %d pages fenced as the site's and refused %d, want 8 and 1",
			read, refused)
	}
	card := evals.Last(frames, wire.EventCard)
	if card == nil || card.Card.Card.Entity == nil || card.Card.Card.Entity.Kind != wire.ResourceWebPage ||
		card.Card.Card.Entity.ID != "https://docs.example.com/rules" {
		t.Errorf("a page read drew %+v, want a web_page card by its address", card)
	}
}

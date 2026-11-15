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

// pageAnswer is one page of a vendor's documentation as GET /research/page
// answers it, and page a handler answering it.
var (
	pageAnswer = evals.JSON(map[string]any{"url": "https://docs.example.com/rules",
		"host": "docs.example.com", "title": "Origin rules", "from": 0, "length": 40,
		"text":    "Host header overrides need the Enterprise plan.",
		"read_at": "2026-10-24T09:41:00Z"})
	page = assistanttest.Routes(map[string]exec.Response{"GET /research/page": pageAnswer})
)

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
	f.Follow(accepted.TurnID, 0)
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
}

// After a web page, a comment on the task the question names is proposed and
// one on another task refused; with no page read, both are proposed.
func TestAWebTaintedTurnProposesOnlyOnItsSubject(t *testing.T) {
	t.Parallel()
	comment := assistanttest.Tool(t, "comment_on_task")
	routes := map[string]exec.Response{
		"GET /worklet/ENG-41": assistanttest.Task("ENG-41", "Route the docs", "Which way?"),
		"GET /worklet/ENG-99": assistanttest.Task("ENG-99", "Payroll", "Pay day."),
	}
	routes["GET /research/page"] = pageAnswer
	for name, tc := range map[string]struct {
		web     bool
		target  string
		propose bool
	}{
		"the task asked about, after a page": {true, "ENG-41", true},
		"another task, after a page":         {true, "ENG-99", false},
		"another task, with no page read":    {false, "ENG-99", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var rounds []evals.Round
			if tc.web {
				rounds = append(rounds, evals.Call("f", catalog.FetchWebPageToolName,
					map[string]any{"url": "https://docs.example.com/rules"}))
			}
			rounds = append(rounds, evals.Call("c", "comment_on_task",
				map[string]any{"item": tc.target, "body": "Noted."}), evals.Say("Done."))
			f := assistanttest.NewRig(t, evals.RigOptions{Company: researchCompany(t),
				Tools: []catalog.Tool{comment}, Acts: []string{comment.Act},
				Handler: assistanttest.Routes(routes), Rounds: rounds})
			accepted, err := f.Ask("subject", "What should ENG-41 do about the plan?", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			if got := len(f.Store.Proposals()) == 1; got != tc.propose {
				t.Errorf("proposed %v, want %v", got, tc.propose)
			}
		})
	}
}

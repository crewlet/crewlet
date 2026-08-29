package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// waitForSeat blocks until this node owns the seat: publishing before the
// claim is safe (the queue is durable) but makes a later failure read as a
// lost message rather than a slow claim.
func waitForSeat(t *testing.T, n *node, handle string) {
	t.Helper()
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), handle)
	})
}

// waitForTurn blocks until the seat has planned, which is the only phase
// these tests assert about.
func waitForTurn(t *testing.T, n *node) {
	t.Helper()
	waitFor(t, "the plan phase to run", func() bool {
		return slices.Contains(n.model.seen(), "plan")
	})
}

// The Plan-phase prefetch, end to end: a memory this seat wrote reaches the
// prompt of the turn it bears on.
//
// The claim being tested is NOT "the block rendered" — the prefetch suite
// covers that against fakes. It is that a real node, with a real store,
// resolves the seat, reads its diary, runs the filter on the seat's own
// auxiliary model and puts the result in front of the planner. Every one of
// those is a wire that was not connected before.

// remember writes a memory as this seat's own, the way the persist decider
// does after a turn.
func remember(t *testing.T, n *node, content string) {
	t.Helper()
	seat, ok := n.engine.Registry().ByHandle("ceo")
	if !ok {
		t.Fatal("no CEO seat")
	}
	diary := learning.NewDiary(n.engine.Backends().Store)
	err := diary.Write(t.Context(), learning.DiaryEntry{
		ID: "mem-" + content[:4], AgentID: seat.AgentID.String(),
		Kind: learning.DiaryLong, Content: content,
		Source: "test", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("write memory: %v", err)
	}
}

// planPrompt is the system prompt of the call that planned.
func planPrompt(t *testing.T, n *node) string {
	t.Helper()
	phases, systems := n.model.seen(), n.model.systemPrompts()
	for i, phase := range phases {
		if phase == "plan" && i < len(systems) {
			return systems[i]
		}
	}
	t.Fatalf("no plan call was made; phases = %v", phases)
	return ""
}

func TestASeatsOwnMemoryReachesThePlanPrompt(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	remember(t, n, "always use semantic commit messages on this repository")

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	system := planPrompt(t, n)
	if !strings.Contains(system, "## Personal memory") {
		t.Fatalf("the plan prompt has no memory section:\n%s", tail(system))
	}
	// The scripted model answers every auxiliary call with the same
	// canned response, so what reaches the prompt is whichever memory the
	// filter's answer selected — the point is that the seat's OWN store
	// was read and the result was rendered, not which one it picked.
	if !strings.Contains(system, "semantic commit") &&
		!strings.Contains(system, "no stored memories surfaced") {
		t.Fatalf("neither the memory nor the empty hint rendered:\n%s", tail(system))
	}
}

// A FRESH SEAT gets no memory section at all — not an empty one. A heading
// with nothing under it tells the planner it has a memory it cannot read.
func TestAFreshSeatGetsNoMemorySection(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	if system := planPrompt(t, n); strings.Contains(system, "## Personal memory") {
		t.Fatalf("a seat with nothing stored got a memory section:\n%s", tail(system))
	}
}

// A SEAT THAT ONBOARDS ON THIS VERY TURN IS NOT THEN TOLD TO ONBOARD.
//
// The prefetch is frozen at turn start and the onboarding pass runs after
// it, so the hint is rendered against a seat that has not onboarded YET —
// which is true at that instant and false by the time Plan reads it. Without
// the suppression the first turn of every seat's life ends with the planner
// being told to go and read the pages it has just finished reading.
func TestASeatThatJustOnboardedIsNotToldToOnboard(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	// The pass really did run — otherwise this asserts the absence of a
	// hint that was never going to be there.
	if phases := n.model.seen(); !slices.Contains(phases, "onboarding") {
		t.Fatalf("the onboarding pass did not run; phases = %v", phases)
	}
	if system := planPrompt(t, n); strings.Contains(system, "## First-turn onboarding") {
		t.Fatalf("a seat that just onboarded was nagged:\n%s", tail(system))
	}
}

// tail is the last of a long prompt, for a failure message that is readable.
func tail(s string) string {
	if len(s) <= 1200 {
		return s
	}
	return "…" + s[len(s)-1200:]
}

// ── the recon path, end to end ──

// wikiInstance is a fake Confluence serving the one read a knowledge search
// makes: the CQL content search.
type wikiInstance struct {
	url string

	mu      sync.Mutex
	queries []string
}

func fakeWiki(t *testing.T) *wikiInstance {
	t.Helper()
	w := &wikiInstance{}
	server := httptest.NewServer(http.HandlerFunc(
		func(rw http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/content/search") {
				// THE CQL, not the plain query: what this asserts is
				// that the aux model's text reached the instance, and it
				// arrives wrapped in the scope clause BuildCQL adds.
				w.mu.Lock()
				w.queries = append(w.queries, r.URL.Query().Get("cql"))
				w.mu.Unlock()
				fmt.Fprint(rw, `{"results":[{"id":"page-1","title":"Staging runbook",`+
					`"space":{"key":"ENG"},`+
					`"body":{"storage":{"value":"how the proxy is wired"}}}]}`)
				return
			}
			fmt.Fprint(rw, `{"results":[]}`)
		}))
	t.Cleanup(server.Close)
	w.url = server.URL
	return w
}

func (w *wikiInstance) searched() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.queries...)
}

// wikiCompany enables Confluence WITH a read credential and a read SCOPE.
//
// Both are needed: a seat searching on the shared org credential may not
// search unscoped, because an unscoped search on a shared credential shows
// one seat pages its own account could never read. The scope is what makes
// the search permitted at all.
func wikiCompany(url string) func(string) string {
	return func(doc string) string {
		return strings.Replace(doc, "roles:\n", `integrations:
  confluence:
    url: `+url+`
    token: confluence-org-token
    webhook_secret: ${CREWLET_TEST_CONFLUENCE_SECRET}
knowledge:
  confluence_spaces: [ENG]
roles:
`, 1)
	}
}

// wakeWithPointer publishes a trigger that only NAMES what changed, which is
// what the thin-trigger gate exists for.
func wakeWithPointer(t *testing.T, n *node, handle string) {
	t.Helper()
	body := "PR !42 got a comment"
	ev := events.New(types.ExternalNotification{
		NotificationSource: "gitlab", SourceEventType: "note.comment",
		Sender: "human-dev", Subject: "Add the rate limiter",
		Body: body, SalientBody: &body,
		// THE FLAG the parser sets, and the only thing that tells the
		// engine this body is a reference rather than the context.
		ContextRequiresRecon: true,
	}, events.TraceContext{})
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		topics.AgentInbox(handle), ev); err != nil {
		t.Fatalf("wake %s: %v", handle, err)
	}
}

// THE WHOLE RECON PATH: a pointer trigger gates the Plan-time search off,
// and the plan summary then recovers it for Execute.
//
// Neither half is observable without a knowledge backend, which is why this
// test stands one up: with no searcher the gate and the recovery both render
// nothing and a broken wire looks exactly like a working one.
func TestAPointerTriggerDefersTheKnowledgeSearchToTheExecutor(t *testing.T) {
	wiki := fakeWiki(t)
	n := startWith(t, wikiCompany(wiki.url))
	waitForSeat(t, n, "ceo")

	wakeWithPointer(t, n, "ceo")
	waitForTurn(t, n)
	waitFor(t, "the execute phase to run", func() bool {
		return slices.Contains(n.model.seen(), "execute")
	})

	// THE PLAN PROMPT says to look again rather than carrying pages found
	// by searching "PR !42 got a comment".
	plan := planPrompt(t, n)
	if !strings.Contains(plan, "no team documents surfaced at turn start") {
		t.Fatalf("the gated plan prompt does not say to search later:\n%s", tail(plan))
	}

	// THE EXECUTE PROMPT carries what the recovery found.
	phases, systems := n.model.seen(), n.model.systemPrompts()
	var execute string
	for i, phase := range phases {
		if phase == "execute" && i < len(systems) {
			execute = systems[i]
			break
		}
	}
	if !strings.Contains(execute, "Staging runbook") {
		t.Fatalf("the executor did not receive the recovered knowledge:\n%s", tail(execute))
	}

	// And the search really did run on the PLAN SUMMARY rather than on the
	// pointer: the original task is boilerplate on a thin trigger and
	// would only dilute the one good query this turn has.
	searched := wiki.searched()
	if len(searched) == 0 {
		t.Fatal("no knowledge search ran at all")
	}
	for _, query := range searched {
		if strings.Contains(query, "PR !42") {
			t.Fatalf("the search ran on the pointer: %q", query)
		}
	}
}

// A SUBSTANTIVE TRIGGER searches at Plan time and does NOT search again for
// Execute: the Plan-time prefetch already ran against a real trigger, and
// running anyway would spend a model call and a search to produce the block
// the planner already read.
func TestASubstantiveTriggerSearchesOnceAtPlanTime(t *testing.T) {
	wiki := fakeWiki(t)
	n := startWith(t, wikiCompany(wiki.url))
	waitForSeat(t, n, "ceo")

	n.wake(t, "ceo", "the staging login redirect keeps looping, can you look")
	waitForTurn(t, n)
	waitFor(t, "the execute phase to run", func() bool {
		return slices.Contains(n.model.seen(), "execute")
	})

	if plan := planPrompt(t, n); !strings.Contains(plan, "Staging runbook") {
		t.Fatalf("the planner did not receive the knowledge block:\n%s", tail(plan))
	}
	if got := len(wiki.searched()); got != 1 {
		t.Fatalf("a substantive trigger ran %d searches, want exactly one", got)
	}
}

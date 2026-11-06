// The four readers that existed and nothing asked.

package queries_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/worklet"
)

// answeredMap runs one question and insists it succeeded, so a case about a
// VALUE never quietly becomes a case about a refusal. It takes no credential,
// for [askAsOperator]'s own reason: no case here can hand itself one by
// accident.
func answeredMap(t *testing.T, s queries.Sources, what string,
	params map[string]any) map[string]any {

	t.Helper()
	got, err := askNative(t, s, what, params)
	return answerMap(t, got, err)
}

// answeredTranscripts is [answeredMap] for the questions that serve what a
// turn SAID, which carry their own grant.
func answeredTranscripts(t *testing.T, s queries.Sources, what string,
	params map[string]any) map[string]any {

	t.Helper()
	got, err := askTranscripts(t, s, what, params)
	return answerMap(t, got, err)
}

// ---- work_search --------------------------------------------------------- //

// SEARCH IS GATED ON ITS OWN INDEX, not on the tracker. A node holding the
// whole board and no lexical index is a real state — it joined recently — and
// registering the two together would leave the board unanswerable on a node
// that can answer every question on it.
func TestSearchIsUnregisteredWithoutAnIndexAndTheBoardIsNot(t *testing.T) {
	t.Parallel()
	s := queries.Sources{Work: &stubWork{}}
	if _, err := askNative(t, s, "work_search", map[string]any{"q": "billing"}); !errors.Is(
		err, queries.ErrUnknown) {

		t.Errorf("work_search on a node with no index answered %v, want unknown", err)
	}
	if _, err := askNative(t, s, "work_items", nil); err != nil {
		t.Errorf("the board on that same node answered %v, and it has every row", err)
	}
}

// AN INDEX STILL BUILDING IS NOT A FAILURE AND NOT AN EMPTY RESULT. Both would
// be acted on: a failure sends a reader to an operator, and "nothing matched"
// has them file the duplicate. It is a third answer, with a reason.
func TestAnIndexStillBuildingIsReportedRatherThanReturnedAsAFailure(t *testing.T) {
	t.Parallel()
	w := &stubWork{err: worklet.ErrIndexBuilding}
	got := answeredMap(t, queries.Sources{Work: &stubWork{}, WorkSearch: w},
		"work_search", map[string]any{"q": "billing"})
	switch {
	case got["available"] != false:
		t.Errorf("available = %v, want false", got["available"])
	case got["reason"] != "building":
		t.Errorf("reason = %v, want building", got["reason"])
	case got["note"] == "" || got["note"] == nil:
		t.Error("the answer carries no note, so a screen has nothing to say")
	}
	hits, ok := got["hits"].([]worklet.Ranked)
	if !ok || hits == nil {
		// AN EMPTY SLICE, never null: a client rendering `hits.length`
		// should not have to guard the field as well.
		t.Fatalf("hits = %#v, want an empty slice", got["hits"])
	}
}

// AND A REAL FAILURE IS STILL A FAILURE. The building case is one sentinel,
// not a catch-all: a store that could not be reached must not read as an index
// that will be ready in a minute.
func TestASearchThatFailedIsNotReportedAsBuilding(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("the store could not be reached")
	w := &stubWork{err: sentinel}
	_, err := askNative(t, queries.Sources{Work: &stubWork{}, WorkSearch: w},
		"work_search", map[string]any{"q": "billing"})
	if !errors.Is(err, sentinel) {
		t.Errorf("a failed search answered %v, want the failure", err)
	}
}

// THE DEFAULT LIMIT IS THE SCREEN'S, not the tool's. A tool's answer is read
// into a prompt where every row costs context; a screen's is scanned.
func TestSearchDefaultsToTheScreensPageRatherThanTheTools(t *testing.T) {
	t.Parallel()
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: &stubWork{}, WorkSearch: w},
		"work_search", map[string]any{"q": "  billing  "}); err != nil {

		t.Fatalf("search: %v", err)
	}
	if w.searchLimit != queries.DefaultSearchLimit {
		t.Errorf("limit = %d, want %d", w.searchLimit, queries.DefaultSearchLimit)
	}
	// TRIMMED, because a phrase pasted out of chat carries whitespace and
	// the index would rank it against terms nobody typed.
	if w.searchText != "billing" {
		t.Errorf("text = %q, want it trimmed", w.searchText)
	}
}

// NO PHRASE IS THE PROBE, not a malformed search: it reaches the search with the
// mode it asks about, whose outcome says which modes a screen's control may offer
// before anybody has typed. Whitespace is no phrase either.
func TestASearchWithNoPhraseIsTheProbe(t *testing.T) {
	t.Parallel()
	w := &stubWork{searchText: "untouched"}
	if _, err := askNative(t, queries.Sources{Work: &stubWork{}, WorkSearch: w},
		"work_search", map[string]any{"q": "   ", "mode": "semantic"}); err != nil {
		t.Fatalf("work_search with no phrase: %v, want the probe's answer", err)
	}
	if w.searchText != "" || w.searchMode != knowledge.ModeSemantic {
		t.Errorf("the probe asked %q for %q, want semantic for nothing", w.searchMode, w.searchText)
	}
}

// WORK_SEARCH HONOURS THE MODE IT IS ASKED FOR and says what it served.
//
// The three modes are the knowledge search's own vocabulary, and the four
// outcome fields are what one screen control reads from both answers — a
// served mode that differs from the asked one is the only way a reader learns
// they are looking at the words alone.
func TestWorkSearchHonoursTheModeAndSaysWhatItServed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		asked string
		want  knowledge.Mode
	}{
		{"", knowledge.ModeHybrid},
		{"hybrid", knowledge.ModeHybrid},
		{"keyword", knowledge.ModeKeyword},
		{"semantic", knowledge.ModeSemantic},
	} {
		w := &stubWork{searchOutcome: knowledge.Outcome{
			ServedMode: knowledge.ModeKeyword,
			Modes:      []knowledge.Mode{knowledge.ModeKeyword},
			Degraded:   knowledge.DegradedNoEmbeddings,
			Coverage: knowledge.Coverage{
				Nodes: []knowledge.NodeCoverage{
					{ID: "n1", Answered: true},
					{ID: "n2", Error: "no answer arrived inside the search budget"},
				},
				BucketsMissing: 32,
			},
		}}
		params := map[string]any{"q": "billing"}
		if tc.asked != "" {
			params["mode"] = tc.asked
		}
		got := answeredMap(t, queries.Sources{Work: &stubWork{}, WorkSearch: w},
			"work_search", params)
		if w.searchMode != tc.want {
			t.Errorf("mode %q reached the searcher as %q, want %q",
				tc.asked, w.searchMode, tc.want)
		}
		if got["served_mode"] != "keyword" || got["degraded"] != "no_embeddings" {
			t.Errorf("served_mode=%v degraded=%v, want the searcher's own",
				got["served_mode"], got["degraded"])
		}
		cov, ok := got["coverage"].(knowledge.Coverage)
		if !ok || cov.Complete || cov.BucketsMissing != 32 || len(cov.Nodes) != 2 {
			t.Errorf("coverage = %#v, want the partial answer the searcher reported", got["coverage"])
		}
		if modes, _ := got["modes"].([]string); len(modes) != 1 || modes[0] != "keyword" {
			t.Errorf("modes = %#v", got["modes"])
		}
	}
	_, err := askNative(t, queries.Sources{Work: &stubWork{}, WorkSearch: &stubWork{}},
		"work_search", map[string]any{"q": "billing", "mode": "meaning"})
	if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(err.Error(), "semantic") {
		t.Errorf("an unknown mode answered %v, want a refusal naming the modes", err)
	}
}

// ---- work_routing -------------------------------------------------------- //

// THE HORIZON REACHES THE READER, and it is the company's own rather than a
// default. It is the whole reason the reader takes one: an absent recipient
// set older than the horizon is a set retention may have taken, and one newer
// than it is a routing that reached nobody. Dating it against a retention
// nobody is running would report the wrong one of those.
func TestTheRoutingReadCarriesTheCompanysOwnInboxHorizon(t *testing.T) {
	t.Parallel()
	w := &stubWork{}
	s := viewerSources(t, w)
	if _, err := askNative(t, s, "work_routing", map[string]any{"record_id": "r-1"}); err != nil {
		t.Fatalf("routing: %v", err)
	}
	if w.routingQuery.RecordID != "r-1" {
		t.Errorf("record = %q, want r-1", w.routingQuery.RecordID)
	}
	if w.routingQuery.Retention <= 0 {
		t.Fatal("the read states no horizon, so every absent recipient set " +
			"comes back `unknown` on a company that keeps an inbox for a year")
	}
}

// AND A REGISTRY WITH NO EPOCH STATES NONE, rather than the shipped default. A
// registry wired without a company source genuinely cannot say how long this
// company keeps a notice, and answering with 365 days would date a set against
// a number nobody here is running.
func TestWithNoCompanyTheRoutingReadStatesNoHorizon(t *testing.T) {
	t.Parallel()
	w := &stubWork{}
	if _, err := askNative(t, queries.Sources{Work: w}, "work_routing",
		map[string]any{"record_id": "r-1"}); err != nil {

		t.Fatalf("routing: %v", err)
	}
	if w.routingQuery.Retention != 0 {
		t.Errorf("a process with no epoch stated a horizon of %s",
			w.routingQuery.Retention)
	}
}

func TestARoutingReadWithNoRecordIsRefusedNamingTheParameter(t *testing.T) {
	t.Parallel()
	_, err := askNative(t, queries.Sources{Work: &stubWork{}}, "work_routing",
		map[string]any{"record_id": "  "})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a routing read with no record answered %v, want bad params", err)
	}
}

// ---- a seat's memory ---------------------------------------------------- //

// stubMemory records what it was asked and answers fixtures: the holder's
// routing and the pages are memread's, and what is asserted here is only what
// this surface hands it and hands back.
type stubMemory struct {
	memory memread.Memory
	err    error

	seat    memread.Seat
	episode string
	limit   int
	skills  learning.SkillOrder
	seats   []memread.Seat
}

func (s *stubMemory) Memory(_ context.Context, seat memread.Seat, q memread.MemoryQuery) (memread.Memory, error) {
	s.seat, s.limit, s.skills = seat, q.Limit, q.Skills
	return s.memory, s.err
}

func (s *stubMemory) Episode(_ context.Context, seat memread.Seat, id string) (memread.EpisodeDetail, error) {
	s.seat, s.episode = seat, id
	return memread.EpisodeDetail{Handle: seat.Handle}, s.err
}

func (s *stubMemory) Overview(_ context.Context, seats []memread.Seat) (memread.Overview, error) {
	s.seats = seats
	return memread.Overview{}, s.err
}

// trailCompany holds three AGENT seats, since a seat's trail is an agent's —
// a person keeps no memory and says nothing on a surface through the engine —
// and three so a lead of one has a seat beside it they do not lead.
const trailCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    p: {model: m}
roles:
  - name: Ana Diaz
    handle: ana
    llm: p
  - name: Bo Lang
    handle: bo
    llm: p
  - name: Cy Park
    handle: cy
    llm: p
`

// trailSources is [viewerSources] over [trailCompany].
func trailSources(t *testing.T, memory *stubMemory) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(trailCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return queries.Sources{Company: companySource(t, cfg), Memory: memory, Chart: flatChart{}}
}

// A LEAD READS THEIR OWN SEATS' TRAIL, AND NO ONE ELSE'S (ADR-0033).
//
// What a seat remembered and said is its trail, read under one verb
// ([authz.ActionSeatTrailRead]) by the seat itself, whoever leads it, and an
// admin — and by nobody else, whatever apps they hold: a lead steers, pauses
// and answers for their agents and could do none of it well blind, while
// somebody who merely shares the company does not read every prompt an agent
// was given. Its rows, over a seat's memory (its sessions, the other half,
// are held to the same rows by TestASeatsSessionsAreItsTrail):
//
//   - an admin with no relation to the seat reads it;
//   - a lead holding NO permission reads the seat they lead, since a seat's
//     trail belongs to no app;
//   - and that lead is refused the seat beside it they do not lead, as is a
//     caller holding every app — each told admin would have admitted them.
//
// Mutation: make `audit.seat.read` admin-only (the company-wide trail's rule)
// and the lead's row goes red; drop the lead arm's chart check and the
// refused rows do.
func TestALeadReadsTheirSeatsTrailAndNoOneElses(t *testing.T) {
	t.Parallel()
	for _, question := range []struct{ what, param string }{
		{"agent_memory", "id"},
	} {
		admin := &stubMemory{}
		if _, err := askHolding(t, trailSources(t, admin), "pat", question.what,
			map[string]any{question.param: "bo"}, iam.GrantAdmin); err != nil {
			t.Fatalf("%s: an admin with no relation to bo was refused: %v", question.what, err)
		}
		if admin.seat.Handle != "bo" {
			t.Errorf("%s: the holder was asked about %q, want the seat the admin named",
				question.what, admin.seat.Handle)
		}

		led := &stubMemory{}
		s := trailSources(t, led)
		s.Chart = leadsChart{lead: "ana", report: "bo"}
		if _, err := askHolding(t, s, "ana", question.what,
			map[string]any{question.param: "bo"}); err != nil {
			t.Fatalf("%s: bo's lead, holding no permission, was refused bo's trail: %v",
				question.what, err)
		}
		if led.seat.Handle != "bo" {
			t.Errorf("%s: the holder was asked about %q, want bo", question.what,
				led.seat.Handle)
		}

		for _, c := range []struct {
			name   string
			chart  authz.Chart
			asked  string
			grants []iam.Grant
		}{
			{"a lead asking for a seat they do not lead",
				leadsChart{lead: "ana", report: "bo"}, "cy", nil},
			{"a caller holding every app and leading nobody", flatChart{}, "bo",
				slices.Clone(iam.Apps)},
		} {
			refusedStub := &stubMemory{}
			s := trailSources(t, refusedStub)
			s.Chart = c.chart
			_, err := askHolding(t, s, "ana", question.what,
				map[string]any{question.param: c.asked}, c.grants...)
			var refusal *queries.Refusal
			if !errors.As(err, &refusal) || !errors.Is(err, queries.ErrUnauthorized) {
				t.Errorf("%s: %s read %s's trail: %v", question.what, c.name, c.asked, err)
				continue
			}
			if !slices.Equal(refusal.Grants, []iam.Grant{iam.GrantAdmin}) {
				t.Errorf("%s: %s was refused naming %v, want admin, which would "+
					"have admitted them", question.what, c.name, refusal.Grants)
			}
			if refusedStub.seat.Handle != "" {
				t.Errorf("%s: %s: the holder was asked about %q anyway", question.what,
					c.name, refusedStub.seat.Handle)
			}
		}
	}
}

// A HOLDER THAT COULD NOT ANSWER IS UNAVAILABLE, which a client retries —
// never an empty memory and never a failure it reports as a bug. Its lease
// unreadable, its node silent, or the seat still arriving on it: each clears
// by waiting.
//
// Mutation: map memread.ErrUnavailable to a plain error in agent_memory and
// this goes red.
func TestAMemoryReadTheHolderCouldNotAnswerIsUnavailable(t *testing.T) {
	t.Parallel()
	s := trailSources(t, &stubMemory{err: fmt.Errorf("%w: node-b holds ana and did not answer",
		memread.ErrUnavailable)})
	_, err := askAsSeat(t, s, "ana", "agent_memory", map[string]any{"id": "ana"})
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("agent_memory answered %v, want ErrUnavailable", err)
	}
}

// The four readers that existed and nothing asked.

package queries_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/tracker"
)

// answeredMap and answeredAsOperator run one question and insist it succeeded,
// so a case about a VALUE never quietly becomes a case about a refusal. Two
// functions rather than a flag, for [askAsOperator]'s own reason: no case here
// can hand itself a credential by accident.
func answeredMap(t *testing.T, s queries.Sources, what string,
	params map[string]any) map[string]any {

	t.Helper()
	got, err := askNative(t, s, what, params)
	return answerMap(t, got, err)
}

func answeredAsOperator(t *testing.T, s queries.Sources, what string,
	params map[string]any) map[string]any {

	t.Helper()
	got, err := askAsOperator(t, s, what, params)
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
	w := &stubWork{err: tracker.ErrIndexBuilding}
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
	hits, ok := got["hits"].([]tracker.Ranked)
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

func TestASearchWithNoPhraseIsRefusedNamingTheParameter(t *testing.T) {
	t.Parallel()
	_, err := askNative(t, queries.Sources{Work: &stubWork{}, WorkSearch: &stubWork{}},
		"work_search", map[string]any{"q": "   "})
	if !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a search with no phrase answered %v, want bad params", err)
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

// ---- conversations ------------------------------------------------------- //

// stubConversations records what it was asked and answers fixtures.
type stubConversations struct {
	threads      []ledgerstore.Thread
	entries      []ledger.Session
	handle       string
	key          string
	limit        int
	historyLimit int
	err          error
}

func (s *stubConversations) Threads(_ context.Context, handle string, limit int) (
	[]ledgerstore.Thread, error) {

	s.handle, s.limit = handle, limit
	if limit > 0 && len(s.threads) > limit {
		return s.threads[:limit], s.err
	}
	return s.threads, s.err
}

func (s *stubConversations) History(_ context.Context, handle, key string, limit int) (
	[]ledger.Session, error) {

	s.handle, s.key, s.historyLimit = handle, key, limit
	return s.entries, s.err
}

// SCOPED LIKE EVERY OTHER PER-SEAT QUESTION. A caller reads the seat their own
// token is bound to; naming somebody else's needs an operator credential.
func TestTheThreadLedgerIsScopedToTheCallersOwnSeat(t *testing.T) {
	t.Parallel()
	ledgerStub := &stubConversations{
		threads: []ledgerstore.Thread{{Key: "slack:C1", Entries: 3, LastAt: time.Now().UTC()}},
	}
	s := viewerSources(t, &stubWork{})
	s.Conversations = ledgerStub

	// The operator's own seat, with no handle named.
	got := answeredAsOperator(t, s, "conversations", nil)
	if got["handle"] != "ana" {
		t.Errorf("handle = %v, want the seat bound to the token", got["handle"])
	}
	if ledgerStub.handle != "ana" {
		t.Errorf("the ledger was asked about %q", ledgerStub.handle)
	}

	// Somebody else's, with no credential at all.
	if _, err := askNative(t, s, "conversations", map[string]any{"handle": "bo"}); err == nil {
		t.Error("an anonymous caller read another seat's threads")
	}
}

// TWO SHAPES IN ONE ANSWER, because the screen asks two questions with one
// navigation: which threads this seat is in, and — when one is named — what it
// said in that one. Split into two questions the second would need the first's
// answer to know what to ask for.
func TestNamingAThreadAddsItsTurnsToTheSameAnswer(t *testing.T) {
	t.Parallel()
	ledgerStub := &stubConversations{
		threads: []ledgerstore.Thread{{Key: "slack:C1", Entries: 2}},
		entries: []ledger.Session{{TurnID: "t-1", Reply: "done"}},
	}
	s := viewerSources(t, &stubWork{})
	s.Conversations = ledgerStub

	// WITHOUT a thread named: the list, and an EMPTY entries list rather
	// than an absent key.
	got := answeredAsOperator(t, s, "conversations", nil)
	entries, ok := got["entries"].([]ledger.Session)
	if !ok || len(entries) != 0 {
		t.Fatalf("entries = %#v, want an empty slice", got["entries"])
	}
	if ledgerStub.key != "" {
		t.Errorf("the ledger was asked for the history of %q", ledgerStub.key)
	}

	// WITH one: the same list, plus its turns.
	got = answeredAsOperator(t, s, "conversations",
		map[string]any{"conversation": "slack:C1"})
	entries, _ = got["entries"].([]ledger.Session)
	if len(entries) != 1 || entries[0].TurnID != "t-1" {
		t.Fatalf("entries = %#v, want the named thread's turns", got["entries"])
	}
	if ledgerStub.key != "slack:C1" {
		t.Errorf("the ledger was asked about %q", ledgerStub.key)
	}
	rows, _ := got["conversations"].([]map[string]any)
	if len(rows) != 1 || rows[0]["key"] != "slack:C1" {
		t.Errorf("the thread list went missing when one was opened: %#v", got["conversations"])
	}
}

// ---- counterparties ------------------------------------------------------ //

type stubCounterparties struct {
	profiles  []learning.Profile
	observer  string
	err       error
	truncated bool
}

func (s *stubCounterparties) List(_ context.Context, observer string) ([]learning.Profile, bool, error) {
	s.observer = observer
	return s.profiles, s.truncated, s.err
}

// THE THIRD MEMORY, and the key that was always an empty list. The store has
// been written since the learning loop landed and the answer carried the key
// from the day it existed, so a seat's profiles were written, replicated and
// invisible.
func TestASeatsCounterpartyProfilesReachTheMemoryAnswer(t *testing.T) {
	t.Parallel()
	seen := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	store := &stubCounterparties{profiles: []learning.Profile{{
		Observer:           "ana",
		Subject:            learning.Subject{Handle: "bo", Name: "Bo Lang"},
		Traits:             map[string]any{"prefers": "async"},
		InteractionCount:   7,
		FirstSeenAt:        seen,
		LastUpdatedAt:      seen,
		LastCorroboratedAt: seen,
	}}}
	s := viewerSources(t, &stubWork{})
	s.Counterparties = store

	got := answeredMap(t, s, "agent_memory", map[string]any{"id": "ana"})
	if store.observer != "ana" {
		t.Errorf("the store was asked about %q", store.observer)
	}
	rows, ok := got["counterparties"].([]map[string]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("counterparties = %#v, want one row", got["counterparties"])
	}
	row := rows[0]
	if row["interactions"] != 7 {
		t.Errorf("interactions = %v, want 7", row["interactions"])
	}
	if row["resolved"] != true {
		t.Error("a profile whose subject is a seat in this company reads as unresolved")
	}
	// BOTH INSTANTS, because they measure different cadences and the gap
	// between them is what says this seat has stopped learning about
	// somebody it still works with.
	for _, key := range []string{"last_updated_at", "last_corroborated_at", "first_seen_at"} {
		if row[key] == nil {
			t.Errorf("the row carries no %s", key)
		}
	}
	subject, _ := row["subject"].(map[string]any)
	if subject["handle"] != "bo" || subject["name"] != "Bo Lang" {
		t.Errorf("subject = %#v", row["subject"])
	}
}

// AN ERROR IS NOT AN EMPTY LIST. Everything in `learning` is best effort by
// design and a failed read answers empty — but that rule is about the TURN,
// which must not die because a diary was slow. A screen reporting "this seat
// has worked with nobody" when the truth is "the store could not be reached"
// is the collapse the three-valued answers exist to prevent.
func TestAFailedCounterpartyReadIsNotAnEmptyList(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("the store could not be reached")
	s := viewerSources(t, &stubWork{})
	s.Counterparties = &stubCounterparties{err: sentinel}
	if _, err := askNative(t, s, "agent_memory", map[string]any{"id": "ana"}); !errors.Is(
		err, sentinel) {

		t.Errorf("a failed profile read answered %v, want the failure", err)
	}
}

// THE THREAD ROSTER IS BOUNDED IN BOTH DIRECTIONS, and the ledger is why: its
// `Threads` applies a `LIMIT` only when one is positive, so an absent or
// negative one reads every key a seat has ever spoken under through this
// process. A busy chat workspace gives a seat a thread per channel.
//
// The store is asked for ONE MORE than the page, which is the evidence row the
// truncation flag is read from.
func TestTheConversationPageIsBoundedInBothDirections(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		asked any
		want  int
	}{
		{"absent", nil, queries.DefaultConversationPage},
		{"zero", 0, queries.DefaultConversationPage},
		{"negative", -1, queries.DefaultConversationPage},
		{"past the ceiling", queries.MaxConversationPage + 1, queries.MaxConversationPage},
		{"inside", 7, 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ledgerStub := &stubConversations{}
			s := viewerSources(t, &stubWork{})
			s.Conversations = ledgerStub
			params := map[string]any{}
			if c.asked != nil {
				params["limit"] = c.asked
			}
			if _, err := askAsOperator(t, s, "conversations", params); err != nil {
				t.Fatalf("conversations: %v", err)
			}
			if ledgerStub.limit != c.want+1 {
				t.Errorf("the ledger was asked for %d, want %d — the page plus its evidence row",
					ledgerStub.limit, c.want+1)
			}
		})
	}
}

// A ROSTER THAT FILLED SAYS SO. A page holding exactly the limit is otherwise
// indistinguishable from a seat that speaks on exactly that many surfaces, and
// the reader is looking for one thread — the one that is missing is the one
// they came for.
func TestAFullThreadRosterSaysItIsAPage(t *testing.T) {
	t.Parallel()
	// One more thread than the page asked for.
	threads := make([]ledgerstore.Thread, 8)
	for i := range threads {
		threads[i] = ledgerstore.Thread{Key: fmt.Sprintf("slack:C%d", i), Entries: 1}
	}
	ledgerStub := &stubConversations{threads: threads}
	s := viewerSources(t, &stubWork{})
	s.Conversations = ledgerStub

	got := answeredAsOperator(t, s, "conversations", map[string]any{"limit": 7})
	if got["truncated"] != true {
		t.Errorf("truncated = %v, want the roster to say it is a page", got["truncated"])
	}
	rows, _ := got["conversations"].([]map[string]any)
	if len(rows) != 7 {
		t.Fatalf("rendered %d threads, want the page — the evidence row is not a result", len(rows))
	}

	// And a roster that fits reports itself whole, with the field PRESENT
	// rather than absent: a client cannot tell a missing key from a false
	// one without reading the engine's source.
	ledgerStub.threads = threads[:3]
	got = answeredAsOperator(t, s, "conversations", map[string]any{"limit": 7})
	if truncated, ok := got["truncated"].(bool); !ok || truncated {
		t.Errorf("truncated = %#v, want a present false", got["truncated"])
	}
}

// A THREAD IS READ WHOLE. Its entries are bounded by the write-time trim, so
// the roster's page size has no business cutting them — and cutting them low
// loses a conversation's OPENING, because the store orders newest-first to
// make a `LIMIT` keep the recent turns.
func TestAThreadsHistoryIsNotCutByTheRostersPage(t *testing.T) {
	t.Parallel()
	ledgerStub := &stubConversations{
		threads: []ledgerstore.Thread{{Key: "slack:C1", Entries: 2}},
		entries: []ledger.Session{{TurnID: "t-1"}},
	}
	s := viewerSources(t, &stubWork{})
	s.Conversations = ledgerStub

	if _, err := askAsOperator(t, s, "conversations", map[string]any{
		"conversation": "slack:C1", "limit": 7,
	}); err != nil {
		t.Fatalf("conversations: %v", err)
	}
	if ledgerStub.historyLimit != 0 {
		t.Errorf("the thread was read with a limit of %d, want the whole thread: "+
			"the trim is the bound, and this layer cannot see what it is set to",
			ledgerStub.historyLimit)
	}
}

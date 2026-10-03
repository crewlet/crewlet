// A name a person typed, resolved to a seat — or to an honest list.

package queries_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
)

// colleagueCompany has a handle that is also the start of another seat's
// name ("swe" beside "SWE Lead"), two seats whose names share a word
// ("Platform Engineer", "Data Engineer"), and one chat id.
const colleagueCompany = `
name: Acme
roles:
  - name: SWE
    handle: swe
  - name: SWE Lead
    handle: swe-lead
  - name: Platform Engineer
    handle: platform-engineer
  - name: Data Engineer
    handle: data-engineer
  - name: Ana Diaz
    handle: ana
    kind: human
    contact:
      slack_user_id: U07XK2PQ
`

func colleagueSources(t *testing.T) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(colleagueCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return queries.Sources{Company: func() *config.Company { return cfg }}
}

// resolveAs asks `colleague` as a caller holding operatorID ("" is anonymous).
func resolveAs(t *testing.T, operatorID, q string) map[string]any {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, colleagueSources(t))
	got, err := r.Answer(t.Context(), "colleague", map[string]any{"q": q}, operatorID)
	if err != nil {
		t.Fatalf("colleague %q: %v", q, err)
	}
	return asMap(t, got)
}

func handlesOf(t *testing.T, answer map[string]any) []string {
	t.Helper()
	rows, _ := answer["candidates"].([]any)
	var out []string
	for _, row := range rows {
		out = append(out, row.(map[string]any)["handle"].(string))
	}
	return out
}

// AN EXACT HANDLE SHORT-CIRCUITS every tier below it. "swe" is also the first
// word of "SWE Lead", so a resolver that ran every tier and merged would
// answer two seats and a screen would have to ask which — for a name that
// was exactly somebody's handle.
func TestColleagueExactHandleShortCircuits(t *testing.T) {
	t.Parallel()
	got := resolveAs(t, "ops-1", "swe")
	match, ok := got["match"].(map[string]any)
	if !ok {
		t.Fatalf("match = %v, want the one seat whose handle this is", got["match"])
	}
	if match["handle"] != "swe" {
		t.Errorf("match = %v, want swe", match["handle"])
	}
	if match["why"] != "handle matches exactly" {
		t.Errorf("why = %q, want the exact tier's own words", match["why"])
	}
	if h := handlesOf(t, got); len(h) != 1 {
		t.Errorf("candidates = %v; an exact handle is diluted by %d near misses", h, len(h)-1)
	}
}

// AN AMBIGUOUS NAME IS A LIST, NEVER A PICK. "engineer" names two seats
// equally well, and the answer that took the first row would hand a task to
// whichever of them sorts first.
func TestColleagueAmbiguousReturnsCandidatesNotAGuess(t *testing.T) {
	t.Parallel()
	got := resolveAs(t, "ops-1", "engineer")
	if got["match"] != nil {
		t.Fatalf("match = %v for a name two seats share — that is a guess", got["match"])
	}
	h := handlesOf(t, got)
	if strings.Join(h, ",") != "data-engineer,platform-engineer" {
		t.Errorf("candidates = %v, want both engineers, in a stable order", h)
	}
	for _, row := range got["candidates"].([]any) {
		if row.(map[string]any)["why"] == "" {
			t.Errorf("%v carries no reason, so a reader cannot tell a near miss from the answer", row)
		}
	}
}

// NOTHING MATCHED is an empty list and no match — the "try another spelling"
// answer, which a screen draws differently from "say which of these".
func TestColleagueNothingMatchedIsAnEmptyListNotNull(t *testing.T) {
	t.Parallel()
	got := resolveAs(t, "ops-1", "zzzz")
	if got["match"] != nil {
		t.Errorf("match = %v for a name nobody has", got["match"])
	}
	rows, ok := got["candidates"].([]any)
	if !ok || len(rows) != 0 {
		t.Errorf("candidates = %#v, want an empty array rather than null", got["candidates"])
	}
}

// A CHAT ID IS NOT PUBLIC. A seat's contact identities are read only through
// the operator-gated configuration, so an anonymous caller pasting one must
// not learn whose it is — while a caller holding a token resolves it exactly
// as the agent's own lookup does.
func TestColleagueNeverMatchesAChatIdForAnAnonymousCaller(t *testing.T) {
	t.Parallel()
	if got := resolveAs(t, "", "U07XK2PQ"); got["match"] != nil || len(handlesOf(t, got)) != 0 {
		t.Errorf("an anonymous caller resolved a chat id: %v", got)
	}
	got := resolveAs(t, "ops-1", "U07XK2PQ")
	match, _ := got["match"].(map[string]any)
	if match["handle"] != "ana" || match["why"] != "chat id matches exactly" {
		t.Errorf("a credentialed caller got %v, want ana by her chat id", got)
	}
}

// A NAME IS REQUIRED, AND A NAME IS SHORT. An empty box asks nothing, and a
// paragraph is refused naming what to send instead rather than fuzzy-matched
// against every seat.
func TestColleagueRefusesAnEmptyOrParagraphQuery(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, colleagueSources(t))
	for _, q := range []string{"", "   ", strings.Repeat("a", queries.ColleagueQueryMax+1)} {
		_, err := r.Answer(t.Context(), "colleague", map[string]any{"q": q}, "ops-1")
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("q of %d bytes answered %v, want bad params", len(q), err)
		}
	}
}

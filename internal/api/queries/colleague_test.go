// A name a person typed, resolved to a seat — or to an honest list.

package queries_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
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
	return queries.Sources{Company: companySource(t, cfg)}
}

// resolveAs asks `colleague` as a caller holding exactly these grants.
func resolveAs(t *testing.T, s queries.Sources, q string, grants ...iam.Grant) map[string]any {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	got, err := r.Answer(asking(t, grants...), "colleague", map[string]any{"q": q})
	if err != nil {
		t.Fatalf("colleague %q: %v", q, err)
	}
	return asMap(t, got)
}

// reader is the board's read alone, which is what `colleague` asks.
var reader = []iam.Grant{iam.GrantStateRead}

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
	got := resolveAs(t, colleagueSources(t), "swe", reader...)
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
	got := resolveAs(t, colleagueSources(t), "engineer", reader...)
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
	got := resolveAs(t, colleagueSources(t), "zzzz", reader...)
	if got["match"] != nil {
		t.Errorf("match = %v for a name nobody has", got["match"])
	}
	rows, ok := got["candidates"].([]any)
	if !ok || len(rows) != 0 {
		t.Errorf("candidates = %#v, want an empty array rather than null", got["candidates"])
	}
}

// A CHAT ID IS READ ON THE CONFIGURATION GRANT. A seat's contact identities
// are read only through the grant the company document is read under, so a
// caller holding the board's read alone pasting one must not learn whose it is
// — while a caller holding `config:read` resolves it exactly as the agent's own
// lookup does.
func TestColleagueMatchesAChatIdOnlyOnTheConfigurationRead(t *testing.T) {
	t.Parallel()
	if got := resolveAs(t, colleagueSources(t), "U07XK2PQ", reader...); got["match"] != nil ||
		len(handlesOf(t, got)) != 0 {
		t.Errorf("a caller without config:read resolved a chat id: %v", got)
	}
	got := resolveAs(t, colleagueSources(t), "U07XK2PQ", iam.GrantStateRead, iam.GrantConfigRead)
	match, _ := got["match"].(map[string]any)
	if match["handle"] != "ana" || match["why"] != "chat id matches exactly" {
		t.Errorf("a caller holding config:read got %v, want ana by her chat id", got)
	}
}

// A WITHHELD PERSON'S CHAT ID NAMES NOBODY, exactly as an inbound message from
// them resolves to nobody: the identity directory has suspended whoever holds
// the seat, so the screen must not offer them as somebody to hand work to by
// an id copied from one of their accounts.
//
// Mutation: build the corpus with no withholding, and ana is matched.
func TestColleagueWithholdsWhatTheDirectoryWithholds(t *testing.T) {
	t.Parallel()
	s := colleagueSources(t)
	s.WithheldContacts = func() func(string) bool {
		return func(handle string) bool { return handle == "ana" }
	}
	got := resolveAs(t, s, "U07XK2PQ", iam.GrantStateRead, iam.GrantConfigRead)
	if got["match"] != nil || len(handlesOf(t, got)) != 0 {
		t.Errorf("a withheld person was matched by her chat id: %v", got)
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
		_, err := r.Answer(asking(t, reader...), "colleague", map[string]any{"q": q})
		if !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("q of %d bytes answered %v, want bad params", len(q), err)
		}
	}
}

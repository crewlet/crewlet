// Who can reach the company through this engine, and as whom.

package queries_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

// accessCompany has one human seat for every binding state, an agent seat
// that must never be listed as a person, and contacts in both spellings.
//
// The variable names are this file's own, so no other test's environment can
// resolve them by accident: CREWLET_ACCESS_TEST_UNSET is never set anywhere.
const accessCompany = `
name: Acme
roles:
  - name: Ana Diaz
    handle: ana
    kind: human
    email: ana@example.com
    availability: weekdays
    contact:
      slack_user_id: U0ANA
      crewlet_operator_id: founder
  - name: Bo Lang
    handle: bo
    kind: human
    contact:
      github_login: bo-lang
      gitlab_username: ${CREWLET_ACCESS_TEST_UNSET}
  - name: Cy Moss
    handle: cy
    kind: human
    contact:
      slack_user_id: U0CY
      crewlet_operator_id: ${CREWLET_ACCESS_TEST_UNSET}
  - name: Dee Park
    handle: dee
    kind: human
    contact:
      slack_user_id: U0DEE
      crewlet_operator_id: founderr
  - name: Lead
    handle: lead
`

func accessSources(t *testing.T, posture queries.AccessPosture) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(accessCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return queries.Sources{
		Company: func() *config.Company { return cfg },
		Access:  &posture,
	}
}

// askAccess asks as the `founder` credential.
func askAccess(t *testing.T, s queries.Sources) queries.AccessAnswer {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	got, err := r.Answer(t.Context(), "access", nil, "founder")
	if err != nil {
		t.Fatalf("access: %v", err)
	}
	answer, ok := got.(queries.AccessAnswer)
	if !ok {
		t.Fatalf("access answered %T", got)
	}
	return answer
}

func personOf(t *testing.T, a queries.AccessAnswer, handle string) queries.AccessPerson {
	t.Helper()
	i := slices.IndexFunc(a.People, func(p queries.AccessPerson) bool { return p.Handle == handle })
	if i < 0 {
		t.Fatalf("no person %q in %+v", handle, a.People)
	}
	return a.People[i]
}

// A TOKEN IS A PERSON EXACTLY WHEN A HUMAN SEAT BINDS IT, and the answer walks
// the join from both ends: `founder` acts as Ana, `ci` is nobody's and reaches
// what an operator credential reaches, and the caller's own is marked.
func TestATokenIsAPersonExactlyWhenAHumanSeatBindsIt(t *testing.T) {
	t.Parallel()
	got := askAccess(t, accessSources(t, queries.AccessPosture{TokenIDs: []string{"founder", "ci"}}))

	want := []queries.AccessToken{
		{ID: "ci", Scope: queries.ScopeOperator},
		{ID: "founder", Scope: queries.ScopePerson,
			Seat: &queries.AccessSeat{Handle: "ana", Name: "Ana Diaz"}, Yours: true},
	}
	if len(got.Tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", got.Tokens, want)
	}
	for i := range want {
		g, w := got.Tokens[i], want[i]
		if g.ID != w.ID || g.Scope != w.Scope || g.Yours != w.Yours ||
			(g.Seat == nil) != (w.Seat == nil) || (g.Seat != nil && *g.Seat != *w.Seat) {
			t.Errorf("token %d = %+v (seat %+v), want %+v (seat %+v)", i, g, g.Seat, w, w.Seat)
		}
	}
}

// EACH BINDING FAILURE IS ITS OWN STATE, because each has its own remedy: an
// unset variable, a label no token carries, and no binding at all.
func TestEveryBindingStateIsToldApart(t *testing.T) {
	t.Parallel()
	got := askAccess(t, accessSources(t, queries.AccessPosture{TokenIDs: []string{"founder", "ci"}}))

	for handle, want := range map[string]queries.AccessBinding{
		"ana": queries.BindingBound,
		"bo":  queries.BindingUnbound,
		"cy":  queries.BindingUnresolved,
		"dee": queries.BindingNoToken,
	} {
		if p := personOf(t, got, handle); p.Binding != want {
			t.Errorf("%s binding = %q, want %q", handle, p.Binding, want)
		}
	}
	// THE BINDING IS VERBATIM: the reference an operator wrote, never what
	// a variable held.
	if p := personOf(t, got, "cy"); p.OperatorID != "${CREWLET_ACCESS_TEST_UNSET}" {
		t.Errorf("cy operator_id = %q, want the reference as written", p.OperatorID)
	}
	if slices.ContainsFunc(got.People, func(p queries.AccessPerson) bool { return p.Handle == "lead" }) {
		t.Error("an agent seat is listed among the people")
	}
	if !slices.IsSortedFunc(got.People, func(a, b queries.AccessPerson) int {
		return strings.Compare(a.Handle, b.Handle)
	}) {
		t.Errorf("people are not in handle order: %+v", got.People)
	}
}

// A CONTACT IS SHOWN AS WRITTEN, beside whether the engine can use it — and the
// binding is not among them, because it is an attribution and never an address.
func TestContactsAreTheConfiguredFieldsWithoutTheBinding(t *testing.T) {
	t.Parallel()
	got := askAccess(t, accessSources(t, queries.AccessPosture{TokenIDs: []string{"founder"}}))

	ana := personOf(t, got, "ana")
	if ana.Email != "ana@example.com" || ana.Availability != "weekdays" {
		t.Errorf("ana = %+v, want her email and availability", ana)
	}
	if want := []queries.AccessContact{{Key: "slack_user_id", Value: "U0ANA", Resolves: true}}; !slices.Equal(ana.Contacts, want) {
		t.Errorf("ana contacts = %+v, want %+v (no crewlet_operator_id)", ana.Contacts, want)
	}
	bo := personOf(t, got, "bo")
	want := []queries.AccessContact{
		{Key: "github_login", Value: "bo-lang", Resolves: true},
		{Key: "gitlab_username", Value: "${CREWLET_ACCESS_TEST_UNSET}", Reference: true},
	}
	if !slices.Equal(bo.Contacts, want) {
		t.Errorf("bo contacts = %+v, want %+v", bo.Contacts, want)
	}
}

// A DISABLED GUARD ACCEPTS NO TOKEN, so nobody is bound to one — every person
// who names a label is told it reaches nothing, rather than read as bound off
// a document the guard is not enforcing.
func TestADisabledGuardBindsNobody(t *testing.T) {
	t.Parallel()
	got := askAccess(t, accessSources(t, queries.AccessPosture{Disabled: true, AnonymousRead: true}))
	if !got.Auth.Disabled || !got.Auth.AnonymousRead {
		t.Errorf("auth = %+v, want the posture as given", got.Auth)
	}
	if got.Tokens == nil || len(got.Tokens) != 0 {
		t.Errorf("tokens = %#v, want an empty list", got.Tokens)
	}
	if p := personOf(t, got, "ana"); p.Binding != queries.BindingNoToken {
		t.Errorf("ana under a disabled guard = %q, want %q", p.Binding, queries.BindingNoToken)
	}
}

// ANONYMOUS IS REFUSED: the labels and who each one is are a map of which
// credential to take.
func TestAccessIsOperatorOnly(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, accessSources(t, queries.AccessPosture{TokenIDs: []string{"founder"}}))
	if !r.RequiresOperator("access") {
		t.Fatal("access is served to any caller")
	}
	if _, err := r.Answer(t.Context(), "access", nil, ""); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("anonymous access answered %v, want an authorization refusal", err)
	}
	// And a node given no posture does not register it at all.
	bare := queries.NewRegistry()
	queries.Register(bare, queries.Sources{})
	if slices.Contains(bare.Names(), "access") {
		t.Error("access is registered with no posture to answer from")
	}
}

// THE PEOPLE SCREEN READS WHAT THIS ANSWER SENDS, every shape held both ways.
func TestTheAccessScreenReadsWhatThisAnswerSends(t *testing.T) {
	t.Parallel()
	posture := queries.AccessPosture{TokenIDs: []string{"founder", "ci"},
		AnonymousRead: true, AllowedOrigins: []string{"https://example.com"}}
	r := queries.NewRegistry()
	queries.Register(r, accessSources(t, posture))
	raw, err := r.Answer(t.Context(), "access", nil, "founder")
	if err != nil {
		t.Fatal(err)
	}
	body := asMap(t, raw)
	holdShape(t, "AccessAnswer", []map[string]any{body}, false)
	holdShape(t, "AccessAuth", []map[string]any{asMap(t, body["auth"])}, false)
	tokens := rowsOf(t, body["tokens"])
	holdShape(t, "AccessToken", tokens, false)
	var seats []map[string]any
	for _, row := range tokens {
		if seat, ok := row["seat"].(map[string]any); ok {
			seats = append(seats, seat)
		}
	}
	holdShape(t, "AccessSeat", seats, false)
	people := rowsOf(t, body["people"])
	holdShape(t, "AccessPerson", people, false)
	var contacts []map[string]any
	for _, p := range people {
		contacts = append(contacts, rowsOf(t, p["contacts"])...)
	}
	holdShape(t, "AccessContact", contacts, false)
}

// THE DASHBOARD KNOWS EXACTLY THE SCOPES AND BINDING STATES THE ENGINE SENDS: a
// state it has no words for draws as nothing, and one the engine never sends is
// a branch nothing reaches.
func TestTheDashboardKnowsExactlyTheAccessStates(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string][]string{
		"TokenScope":    stringsOf(queries.TokenScopes),
		"AccessBinding": stringsOf(queries.AccessBindings),
	} {
		got, err := clientsource.Union(clientsource.Tree(t), name)
		if err != nil {
			t.Fatalf("%v — this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		want := slices.Sorted(slices.Values(engine))
		if !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", name, got, want)
		}
	}
	for _, s := range queries.TokenScopes {
		if !s.Valid() {
			t.Errorf("scope %q is listed and not valid", s)
		}
	}
	for _, b := range queries.AccessBindings {
		if !b.Valid() {
			t.Errorf("binding %q is listed and not valid", b)
		}
	}
}

func stringsOf[S ~string](values []S) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, string(v))
	}
	return out
}

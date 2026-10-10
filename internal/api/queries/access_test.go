// Who can reach the company through this engine, and as whom.

package queries_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
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
	got, err := r.Answer(t.Context(), "access", nil, asAdmin("founder"))
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

// keys is the posture's keys: `founder` an admin, `ci` a member, and any
// others named after them admins.
func keys(ids ...string) []queries.AccessKey {
	out := make([]queries.AccessKey, 0, len(ids))
	for _, id := range ids {
		role := config.RoleAdmin
		if id == "ci" {
			role = config.RoleMember
		}
		out = append(out, queries.AccessKey{ID: id, Role: role})
	}
	return out
}

// EVERY KEY NAMES ITS ROLE, AND A PERSON EXACTLY WHEN A HUMAN SEAT LINKS IT —
// two facts, never one read as the other: `founder` is an admin key linked to
// Ana, `ci` a member key nobody's seat links, and the caller's own is marked.
// The answer walks the join from both ends.
func TestEveryKeyNamesItsRoleAndThePersonItIsLinkedTo(t *testing.T) {
	t.Parallel()
	got := askAccess(t, accessSources(t, queries.AccessPosture{Keys: keys("founder", "ci")}))

	want := []queries.AccessToken{
		{ID: "ci", Role: config.RoleMember},
		{ID: "founder", Role: config.RoleAdmin,
			Seat: &queries.AccessSeat{Handle: "ana", Name: "Ana Diaz"}, Yours: true},
	}
	if len(got.Tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", got.Tokens, want)
	}
	for i := range want {
		g, w := got.Tokens[i], want[i]
		if g.ID != w.ID || g.Role != w.Role || g.Yours != w.Yours ||
			(g.Seat == nil) != (w.Seat == nil) || (g.Seat != nil && *g.Seat != *w.Seat) {
			t.Errorf("token %d = %+v (seat %+v), want %+v (seat %+v)", i, g, g.Seat, w, w.Seat)
		}
	}
}

// EACH BINDING FAILURE IS ITS OWN STATE, because each has its own remedy: an
// unset variable, a label no token carries, and no binding at all.
func TestEveryBindingStateIsToldApart(t *testing.T) {
	t.Parallel()
	got := askAccess(t, accessSources(t, queries.AccessPosture{Keys: keys("founder", "ci")}))

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
	got := askAccess(t, accessSources(t, queries.AccessPosture{Keys: keys("founder")}))

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
	got := askAccess(t, accessSources(t, queries.AccessPosture{Disabled: true,
		Anonymous: config.AnonymousPublic}))
	if !got.Auth.Disabled || got.Auth.Anonymous != config.AnonymousPublic {
		t.Errorf("auth = %+v, want the posture as given", got.Auth)
	}
	if got.Tokens == nil || len(got.Tokens) != 0 {
		t.Errorf("tokens = %#v, want an empty list", got.Tokens)
	}
	if p := personOf(t, got, "ana"); p.Binding != queries.BindingNoToken {
		t.Errorf("ana under a disabled guard = %q, want %q", p.Binding, queries.BindingNoToken)
	}
}

// THE MANAGED POSTURE IS PART OF THE AUTH ANSWER (ADR-0030): the writers in
// Tier A's order, and an empty list — never null — when every admin key may
// change the company document, which is a posture too.
func TestTheAuthAnswerNamesTheCompanyWriters(t *testing.T) {
	t.Parallel()
	managed := askAccess(t, accessSources(t, queries.AccessPosture{
		Keys: keys("founder", "gitops", "ci"), CompanyWriters: []string{"gitops", "ci"},
	}))
	if !slices.Equal(managed.Auth.CompanyWriters, []string{"gitops", "ci"}) {
		t.Errorf("company_writers = %v, want [gitops ci] as Tier A orders them", managed.Auth.CompanyWriters)
	}
	open := askAccess(t, accessSources(t, queries.AccessPosture{Keys: keys("founder")}))
	if open.Auth.CompanyWriters == nil || len(open.Auth.CompanyWriters) != 0 {
		t.Errorf("company_writers unset = %#v, want an empty list", open.Auth.CompanyWriters)
	}
}

// ACCESS IS AN ADMIN'S: the labels, what each reaches and who each one is are a
// map of which credential to take — refused to a caller with no key, and
// FORBIDDEN to a member, whose key a teammate holds.
func TestAccessIsAnAdminsAlone(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, accessSources(t, queries.AccessPosture{Keys: keys("founder", "ci")}))
	if got := r.ReachOf("access"); got != auth.ReachAdmin {
		t.Fatalf("access is registered at %q, want admin", got)
	}
	if _, err := r.Answer(t.Context(), "access", nil, nobody); !errors.Is(err, queries.ErrUnauthorized) {
		t.Errorf("anonymous access answered %v, want an authorization refusal", err)
	}
	if _, err := r.Answer(t.Context(), "access", nil, asMember("ci")); !errors.Is(err, queries.ErrForbidden) {
		t.Errorf("a member's access answered %v, want forbidden", err)
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
	posture := queries.AccessPosture{Keys: keys("founder", "gitops", "ci"),
		Anonymous: config.AnonymousPublic, AllowedOrigins: []string{"https://example.com"},
		CompanyWriters: []string{"gitops"}}
	r := queries.NewRegistry()
	queries.Register(r, accessSources(t, posture))
	raw, err := r.Answer(t.Context(), "access", nil, asAdmin("founder"))
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

// THE DASHBOARD KNOWS EXACTLY THE ROLES AND BINDING STATES THE ENGINE SENDS: a
// state it has no words for draws as nothing, and one the engine never sends is
// a branch nothing reaches.
func TestTheDashboardKnowsExactlyTheAccessStates(t *testing.T) {
	t.Parallel()
	for name, engine := range map[string][]string{
		"TokenRole":       stringsOf(config.TokenRoles),
		"AnonymousAccess": stringsOf(config.AnonymousAccesses),
		"Reach":           stringsOf(auth.Reaches),
		"AccessBinding":   stringsOf(queries.AccessBindings),
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

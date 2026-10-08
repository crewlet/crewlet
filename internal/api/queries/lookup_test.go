// A `${VAR}` in the chart, resolved through the serving node's own chain.

package queries_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
)

// handedOnly is a lookup that answers one variable, set in no process, and
// nothing else — what [queries.Sources.Env] is on a node whose secret store,
// or whose handed environment, holds the value. Each case asserts the
// premise, so it can never pass on the process environment instead.
func handedOnly(t *testing.T, variable, value string) func(string) (string, bool) {
	t.Helper()
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	return func(name string) (string, bool) {
		if name == variable {
			return value, true
		}
		return "", false
	}
}

// A BINDING RESOLVES THROUGH THE LOOKUP THE NODE HANDS IN, never the process
// environment behind it. CREWLET_ACCESS_TEST_UNSET is set in no process;
// handed to the answer, it binds Cy to the token it names and makes Bo's
// contact usable — on the token's row, the person's binding and the contact's
// own flag alike, because all three are one resolution and the guard that
// admits Cy reads the same.
func TestAccessResolvesABindingThroughTheLookupTheNodeHandsIn(t *testing.T) {
	t.Parallel()
	s := accessSources(t, queries.AccessPosture{TokenIDs: []string{"founder", "ci"}})
	s.Env = handedOnly(t, "CREWLET_ACCESS_TEST_UNSET", "ci")
	got := askAccess(t, s)

	if p := personOf(t, got, "cy"); p.Binding != queries.BindingBound {
		t.Errorf("cy binding = %q, want %q through the handed lookup", p.Binding, queries.BindingBound)
	}
	i := slices.IndexFunc(got.Tokens, func(tok queries.AccessToken) bool { return tok.ID == "ci" })
	if i < 0 || got.Tokens[i].Scope != queries.ScopePerson || got.Tokens[i].Seat == nil ||
		got.Tokens[i].Seat.Handle != "cy" {
		t.Errorf("tokens = %+v, want ci to act as cy", got.Tokens)
	}
	if bo := personOf(t, got, "bo"); !slices.Contains(bo.Contacts, queries.AccessContact{
		Key: "gitlab_username", Value: "${CREWLET_ACCESS_TEST_UNSET}", Reference: true, Resolves: true,
	}) {
		t.Errorf("bo contacts = %+v, want the reference resolving through the handed lookup", bo.Contacts)
	}
}

// AND THE VIEWER IS THE SEAT THE HANDED LOOKUP BINDS, both ways round: the
// credential resolves to Ana, and Ana's party carries the credential, which is
// what her own rows filed under it are read by.
func TestTheViewerResolvesItsBindingThroughTheLookupTheNodeHandsIn(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_QUERIES_TEST_HANDED_OPERATOR"
	cfg, err := config.ParseCompany([]byte(strings.Replace(viewerCompany,
		"crewlet_operator_id: ops-1", "crewlet_operator_id: ${"+variable+"}", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	work := &stubWork{}
	sources := queries.Sources{
		Company: func() *config.Company { return cfg },
		Env:     handedOnly(t, variable, "ops-1"),
		Work:    work,
	}
	answered, err := askAsOperator(t, sources, "viewer", nil)
	got := answerMap(t, answered, err)
	if got["handle"] != "ana" {
		t.Errorf("handle = %v, want ana, whom the handed lookup binds to ops-1", got["handle"])
	}

	// The other way round: her inbox is read by BOTH of her names, and the
	// credential's is the half only the handed lookup can resolve.
	if _, err := askAsOperator(t, sources, "work_inbox", nil); err != nil {
		t.Fatalf("work_inbox: %v", err)
	}
	wantParty(t, work.inboxQuery.Who, "ana", "ops-1")
}

// A COLLEAGUE IS FOUND BY AN ID THE HANDED LOOKUP HOLDS, as the agent's own
// lookup_colleague finds them: the same people, by the same ids, whichever
// surface asks.
func TestAColleagueIsFoundThroughTheLookupTheNodeHandsIn(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_QUERIES_TEST_HANDED_SLACK"
	cfg, err := config.ParseCompany([]byte(strings.Replace(colleagueCompany,
		"slack_user_id: U07XK2PQ", "slack_user_id: ${"+variable+"}", 1)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Company: func() *config.Company { return cfg },
		Env:     handedOnly(t, variable, "U0HANDED"),
	})
	answered, err := r.Answer(t.Context(), "colleague", map[string]any{"q": "U0HANDED"}, "ops-1")
	if err != nil {
		t.Fatalf("colleague: %v", err)
	}
	match, ok := asMap(t, answered)["match"].(map[string]any)
	if !ok || match["handle"] != "ana" {
		t.Errorf("answer = %v, want ana by the id the handed lookup holds", answered)
	}
}

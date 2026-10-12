// A `${VAR}` in a seat's contact ids, resolved through the serving node's own
// chain.

package queries_test

import (
	"os"
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

// A COLLEAGUE IS FOUND BY AN ID THE HANDED LOOKUP HOLDS, as the agent's own
// lookup_colleague finds them: the same people, by the same ids, whichever
// surface asks. Asked by a reader holding `config:read`, the one the exact-id
// tier answers at all.
//
// Mutation: build the corpus with a nil lookup (the process environment), and
// the reference resolves to nothing and nobody is found.
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
		Company: companySource(t, cfg),
		Env:     handedOnly(t, variable, "U0HANDED"),
	})
	answered, err := r.Answer(everyGrant(t), "colleague", map[string]any{"q": "U0HANDED"})
	if err != nil {
		t.Fatalf("colleague: %v", err)
	}
	match, ok := asMap(t, answered)["match"].(map[string]any)
	if !ok || match["handle"] != "ana" {
		t.Errorf("answer = %v, want ana by the id the handed lookup holds", answered)
	}
}

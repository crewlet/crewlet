package queries_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
)

// THE DEEP RUN READS ONLY ITS ALLOWLIST: every read it may ask is a registered
// question everybody, an administrator or an app holder may read (none decided
// record by record), and none is about identity, sessions, credentials or
// conversations. Mutation: add a question
// that answers sessions, or one nobody registers, and a check fails.
func TestTheDeepRunReadsOnlyItsAllowlist(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, everySeam(t))
	for _, name := range queries.AdvisorReadNames() {
		posture := r.PostureOf(name)
		if !slices.Contains(r.Names(), name) || (!posture.IsEverybody() && posture.Grant() == "") {
			t.Errorf("%s is on the allowlist with posture %s, want a registered read everybody, an "+
				"administrator or an app holder may make", name, posture)
		}
		for _, word := range []string{"iam", "session", "credential", "secret", "chat"} {
			if strings.Contains(name, word) {
				t.Errorf("%s is on the allowlist and reads %s", name, word)
			}
		}
	}
}

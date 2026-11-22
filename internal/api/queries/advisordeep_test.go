package queries_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/worklet"
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

// THE DEEP RUN'S READ LIST NAMES THE VALUES A CLOSED SET TAKES, so its model
// asks for open work as the grammar spells it rather than guessing "open",
// which the tracker refuses. Mutation: list work_items' status_group without
// its values, and this fails.
func TestTheDeepRunsReadListNamesTheStatusGroups(t *testing.T) {
	t.Parallel()
	params := queries.AdvisorReadParams("work_items")
	for _, g := range worklet.StatusGroups {
		if !strings.Contains(params, string(g)) {
			t.Errorf("work_items takes %q, which names no %s", params, g)
		}
	}
}

package learning

import (
	"regexp"
	"strings"
	"testing"
)

// The three readers of the terminal set agree, because they are one list.
//
// They were three separate literals — Settled, the lifecycle sweep's SQL
// tuple, and the clusterer's own pair — and the tuple's own doc claimed to be
// "ONE definition used twice" while cluster.go held a third copy it could not
// see. Both ways of getting it wrong are silent: an outcome the sweep treats
// as terminal and the clusterer does not is learned from but never drafts a
// skill, and the reverse drafts skills from episodes the sweep deletes as
// mid-state.
func TestEveryReaderOfTheTerminalSetAgrees(t *testing.T) {
	t.Parallel()
	settled := SettledOutcomes()
	if len(settled) == 0 {
		t.Fatal("no outcome ends a turn, so nothing can ever be learned from one")
	}
	for _, o := range settled {
		if !Settled(o) {
			t.Errorf("%q is in the settled set but Settled says otherwise", o)
		}
		// The SQL tuple is the sweep's reader, and it is a string rather
		// than a list — so this is the only thing holding it to the same
		// membership.
		if !strings.Contains(terminalOutcomes, "'"+o+"'") {
			t.Errorf("%q is settled but the sweep's tuple %s omits it", o, terminalOutcomes)
		}
	}
	// self_iterate is the one the whole distinction exists for: a mid-turn
	// state the engine will reattempt, so anything learned from it was
	// learned from work the agent itself judged incomplete.
	if Settled("self_iterate") {
		t.Error("self_iterate is settled, so the engine learns from rounds it will redo")
	}
	if strings.Contains(terminalOutcomes, "self_iterate") {
		t.Errorf("the sweep treats self_iterate as terminal: %s", terminalOutcomes)
	}
	if Settled("") {
		t.Error("an absent outcome is settled, so a turn that recorded nothing is learned from")
	}
}

// Every settled outcome is a bare identifier.
//
// terminalOutcomes is spelled INTO the query rather than bound, which is
// sound only while no value needs quoting rules the tuple does not have. The
// values are compile-time constants of this package and never caller input,
// so this is the constraint that keeps that true — not an escaping pass,
// which would imply the opposite.
func TestEverySettledOutcomeIsSafeToSpellIntoAQuery(t *testing.T) {
	t.Parallel()
	bare := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, o := range SettledOutcomes() {
		if !bare.MatchString(o) {
			t.Errorf("%q is not a bare identifier, so the sweep's tuple cannot hold it unquoted", o)
		}
	}
}

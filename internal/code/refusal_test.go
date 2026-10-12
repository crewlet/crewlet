package code_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// EVERY REFUSAL HAS ITS SENTENCE, and the five answers ABOUT a push are never
// recorded on one of its commands: a client sees "<code>: <sentence>", and a
// code with no sentence is a client reading its own name back.
func TestEveryRefusalHasItsSentence(t *testing.T) {
	t.Parallel()
	about := map[code.Refusal]bool{
		code.RefusalBusy: true, code.RefusalUnavailable: true, code.RefusalNotApplied: true,
		code.RefusalUnknown: true, code.RefusalPending: true,
	}
	seen := map[code.Refusal]bool{}
	for _, r := range code.Refusals() {
		if seen[r] {
			t.Errorf("%s is listed twice", r)
		}
		seen[r] = true
		if !r.Valid() {
			t.Errorf("%s is listed and Valid says it is not", r)
		}
		if r.Sentence() == string(r) || r.Sentence() == "" {
			t.Errorf("%s has no sentence", r)
		}
		// The client prints the sentence after "<code>: " on one line, and
		// a Detail ends the same way, so neither carries a full stop.
		if strings.HasSuffix(r.Sentence(), ".") {
			t.Errorf("%s's sentence ends with a full stop: %q", r, r.Sentence())
		}
		if r.Recorded() == about[r] {
			t.Errorf("%s: Recorded = %v — a verdict on a command is recorded, an answer "+
				"about a push never is", r, r.Recorded())
		}
	}
	if len(seen) != 20 {
		t.Errorf("%d refusal codes, and the closed set is twenty", len(seen))
	}
	if unknown := code.Refusal("something_new"); unknown.Valid() || unknown.Recorded() ||
		unknown.Sentence() != "something_new" {
		t.Error("a code this build does not know is not a value it can carry back")
	}
}

// THE STATELESS RULES REFUSE IN THEIR ORDER: what is not a ref before what is
// reserved, before a tag that moved.
func TestTheStatelessRulesRefuseInOrder(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, tc := range []struct {
		cmd  code.Command
		want code.Refusal
	}{
		{code.Command{Ref: "refs/heads/main", Old: a, New: b}, ""},
		{code.Command{Ref: "refs/heads/bad..ref", Old: a, New: b}, code.RefusalBadRef},
		// NOT A REF FIRST, wherever it would sit: a malformed name under
		// the reserved namespace is bad_ref, because no rule after the
		// first can reason about something that is not a ref.
		{code.Command{Ref: "refs/crewlet/bad..ref", Old: code.ZeroOID, New: b}, code.RefusalBadRef},
		{code.Command{Ref: "refs/remotes/origin/main", Old: code.ZeroOID, New: b}, code.RefusalReservedRef},
		{code.Command{Ref: "refs/crewlet/x/y", Old: a, New: b}, code.RefusalReservedRef},
		{code.Command{Ref: "refs/notes/commits", Old: a, New: b}, code.RefusalReservedRef},
		{code.Command{Ref: "refs/crewlet/merge/1", Old: code.ZeroOID, New: b}, code.RefusalReservedRef},
		{code.Command{Ref: "refs/tags/v1", Old: a, New: b}, code.RefusalTagMoved},
		{code.Command{Ref: "refs/tags/v1", Old: code.ZeroOID, New: b}, ""},
		{code.Command{Ref: "refs/tags/v1", Old: a, New: code.ZeroOID}, ""},
	} {
		if got := code.StatelessRefusal(tc.cmd); got != tc.want {
			t.Errorf("StatelessRefusal(%s %s→%s) = %q, want %q", tc.cmd.Ref, tc.cmd.Old[:1],
				tc.cmd.New[:1], got, tc.want)
		}
	}
}

// EVERY MATCHING PROTECTION RULE HOLDS, and the strongest refusal wins: two
// rules written for two reasons both apply to a ref they both match.
func TestEveryMatchingProtectionHolds(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	rules := []code.Protection{
		{Pattern: "refs/heads/main", AllowForce: true, AllowDelete: true, DirectPush: code.DirectPersons},
		{Pattern: "refs/heads/**", DirectPush: code.DirectPersons},
		{Pattern: "refs/heads/release/*", AllowForce: true, AllowDelete: true, DirectPush: code.DirectNone},
	}
	for _, tc := range []struct {
		cmd  code.Command
		want code.Refusal
	}{
		{code.Command{Ref: "refs/heads/main", Old: a, New: b}, ""},
		// TWO RULES MATCH and only the SECOND refuses a force-push: the
		// first's allow_force does not overrule it.
		{code.Command{Ref: "refs/heads/main", Old: a, New: b, Forced: true}, code.RefusalProtectedForce},
		{code.Command{Ref: "refs/heads/main", Old: a, New: code.ZeroOID}, code.RefusalProtectedDelete},
		{code.Command{Ref: "refs/heads/release/1", Old: a, New: b}, code.RefusalProtectedDirect},
		{code.Command{Ref: "refs/tags/v1", Old: code.ZeroOID, New: b}, ""},
		// direct_push: none WINS OVER THE REST: a ref nobody pushes to is
		// refused as that before its deletion or its force is weighed.
		{code.Command{Ref: "refs/heads/release/1", Old: a, New: code.ZeroOID, Forced: true},
			code.RefusalProtectedDirect},
	} {
		if got := code.ProtectionRefusal(tc.cmd, rules); got != tc.want {
			t.Errorf("ProtectionRefusal(%s) = %q, want %q", tc.cmd.Ref, got, tc.want)
		}
	}
}

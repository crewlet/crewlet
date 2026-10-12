package code_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
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
		if got := code.ProtectionRefusal(tc.cmd, parsed(t, rules...)); got != tc.want {
			t.Errorf("ProtectionRefusal(%s) = %q, want %q", tc.cmd.Ref, got, tc.want)
		}
	}
}

// parsed is rules as ProtectionRefusal asks them.
func parsed(t *testing.T, rules ...code.Protection) code.Protections {
	t.Helper()
	ps, err := code.ParseProtections(rules)
	if err != nil {
		t.Fatalf("ParseProtections: %v", err)
	}
	return ps
}

// A PARSED RULE MATCHES EXACTLY WHAT THE ONE-OFF FORM MATCHES: the decide and
// the receiver ask rules parsed once, and gitref.MatchPattern — the form that
// checks the ref and parses the pattern on every call, which gitref holds to
// real git — is the oracle, so the two can never drift. One rule a case, with
// direct_push none, so the refusal says exactly whether it matched.
func TestAParsedProtectionMatchesWhatMatchPatternMatches(t *testing.T) {
	t.Parallel()
	deep := "refs/heads/" + strings.Repeat("d/", 100) + "hotfix"
	patterns := []string{
		"refs/heads/main", "refs/heads/*", "refs/heads/**", "refs/heads/release/**",
		"refs/heads/release/*", "refs/heads/**/hotfix", "refs/heads/team*/release/**",
		"refs/heads/a/**/b/**/c", "refs/tags/v*", "refs/tags/**", "refs/heads/*-wip",
	}
	refs := []string{
		"refs/heads/main", "refs/heads/main/x", "refs/heads/release", "refs/heads/release/1.0",
		"refs/heads/release/1/2", "refs/heads/hotfix", "refs/heads/x/hotfix", deep,
		"refs/heads/team7/release/2/rc", "refs/heads/team7/feature", "refs/heads/a/b/c",
		"refs/heads/a/x/b/y/c", "refs/heads/a/c", "refs/tags/v1.0", "refs/tags/x",
		"refs/heads/my-wip", "refs/heads/wip",
	}
	matched := 0
	for _, pattern := range patterns {
		rules := parsed(t, code.Protection{Pattern: pattern, DirectPush: code.DirectNone})
		for _, ref := range refs {
			want := gitref.MatchPattern(pattern, ref)
			got := code.ProtectionRefusal(code.Command{Ref: ref, Old: code.ZeroOID,
				New: strings.Repeat("b", 40)}, rules) == code.RefusalProtectedDirect
			if got != want {
				t.Errorf("%s against %s: the parsed rule matched = %v, MatchPattern = %v",
					pattern, ref, got, want)
			}
			if want {
				matched++
			}
		}
	}
	// A TABLE THAT NEVER MATCHED would agree with an oracle that never
	// matches either.
	if matched < 20 {
		t.Fatalf("only %d (pattern, ref) pairs matched — the table no longer tells a "+
			"matching rule from an absent one", matched)
	}
}

// A RULE THAT IS NOT A PATTERN IS REFUSED, NEVER READ AS PROTECTING NOTHING: a
// stored rule this build cannot parse would otherwise leave the ref it was
// written for open to every push.
func TestAProtectionThatIsNotAPatternIsRefused(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{"refs/heads/*.lock", "refs/notes/x", "refs/heads/a**b", ""} {
		_, err := code.ParseProtections([]code.Protection{
			{Pattern: "refs/heads/main", DirectPush: code.DirectNone},
			{Pattern: pattern, DirectPush: code.DirectNone},
		})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", pattern)) {
			t.Errorf("ParseProtections with %q: %v; want an error naming it", pattern, err)
		}
	}
	if got := code.ProtectionRefusal(code.Command{Ref: "refs/heads/main", Old: code.ZeroOID,
		New: strings.Repeat("b", 40)}, code.Protections{}); got != "" {
		t.Errorf("no rules refused %q", got)
	}
}

// AN ATOMIC PUSH IS REFUSED WHOLE: once any command is ng, every command that
// is ok or undecided is ng atomic, a noop stays a noop and the refusal that
// caused it keeps its own code — and a push with no refusal is untouched.
func TestAtomicRefusesEveryOtherCommand(t *testing.T) {
	t.Parallel()
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	cmds := []code.Command{
		{Ref: "refs/heads/one", Old: a, New: b, Result: code.ResultOK},
		{Ref: "refs/heads/two", Old: a, New: b},
		{Ref: "refs/heads/three", Old: a, New: b, Result: code.ResultNoop},
		{Ref: "refs/heads/four", Old: a, New: b, Result: code.ResultNG, Reason: code.RefusalStale,
			Detail: "refs/heads/four is at bbbbbbb; fetch first"},
	}
	got := code.Atomic(cmds)
	want := []struct {
		result code.CommandResult
		reason code.Refusal
	}{
		{code.ResultNG, code.RefusalAtomic}, {code.ResultNG, code.RefusalAtomic},
		{code.ResultNoop, ""}, {code.ResultNG, code.RefusalStale},
	}
	for i, w := range want {
		if got[i].Result != w.result || got[i].Reason != w.reason {
			t.Errorf("command %d is %s %s, want %s %s", i, got[i].Result, got[i].Reason, w.result, w.reason)
		}
		if got[i].Reason == code.RefusalAtomic && got[i].Detail == "" {
			t.Errorf("command %d is refused atomic with no detail", i)
		}
	}
	if got[3].Detail != cmds[3].Detail {
		t.Errorf("the refusal that caused it lost its detail: %q", got[3].Detail)
	}
	if cmds[0].Result != code.ResultOK || cmds[1].Result != "" {
		t.Error("Atomic changed the commands it was handed")
	}
	clean := cmds[:3]
	if out := code.Atomic(clean); &out[0] != &clean[0] {
		t.Error("a push with no refusal was copied rather than returned as it is")
	}
}

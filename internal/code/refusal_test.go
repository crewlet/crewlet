package code_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
)

// EVERY REFUSAL HAS ITS SENTENCE, and the eleven answers ABOUT a push are never
// recorded on one of its commands: a client sees "<code>: <sentence>", and a
// code with no sentence is a client reading its own name back.
func TestEveryRefusalHasItsSentence(t *testing.T) {
	t.Parallel()
	about := map[code.Refusal]bool{
		code.RefusalBusy: true, code.RefusalCompactionBehind: true, code.RefusalPushQuota: true,
		code.RefusalExpunging: true, code.RefusalTooManyChanges: true, code.RefusalUnavailable: true,
		code.RefusalStoreFull: true, code.RefusalForgedCommitter: true, code.RefusalNotApplied: true,
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
	if len(seen) != 30 {
		t.Errorf("%d refusal codes, and the closed set is thirty", len(seen))
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
	rules := []code.PushRule{
		{Pattern: "refs/heads/main", AllowForce: true, AllowDelete: true, DirectPush: code.DirectPersons},
		{Pattern: "refs/heads/**", DirectPush: code.DirectPersons},
		{Pattern: "refs/heads/release/*", AllowForce: true, AllowDelete: true, DirectPush: code.DirectNone},
		{Pattern: "refs/heads/release/**", DirectPush: code.DirectNone},
		// A SECOND RULE refusing main's force and deletion, LATER in the
		// order: the refusal names the first.
		{Pattern: "refs/heads/m*", DirectPush: code.DirectPersons},
	}
	for _, tc := range []struct {
		cmd  code.Command
		want code.Refusal
		// by is the rule the refusal names: the first, in the rules'
		// order, to refuse the command for that reason.
		by string
	}{
		{code.Command{Ref: "refs/heads/main", Old: a, New: b}, "", ""},
		// TWO RULES MATCH and only the SECOND refuses a force-push: the
		// first's allow_force does not overrule it.
		{code.Command{Ref: "refs/heads/main", Old: a, New: b, Forced: true}, code.RefusalProtectedForce,
			"refs/heads/**"},
		{code.Command{Ref: "refs/heads/main", Old: a, New: code.ZeroOID}, code.RefusalProtectedDelete,
			"refs/heads/**"},
		{code.Command{Ref: "refs/heads/release/1", Old: a, New: b}, code.RefusalProtectedDirect,
			"refs/heads/release/*"},
		{code.Command{Ref: "refs/tags/v1", Old: code.ZeroOID, New: b}, "", ""},
		// direct_push: none WINS OVER THE REST: a ref nobody pushes to is
		// refused as that before its deletion or its force is weighed.
		{code.Command{Ref: "refs/heads/release/1", Old: a, New: code.ZeroOID, Forced: true},
			code.RefusalProtectedDirect, "refs/heads/release/*"},
		{code.Command{Ref: "refs/heads/release/1/x", Old: a, New: b}, code.RefusalProtectedDirect,
			"refs/heads/release/**"},
	} {
		got, rule := code.ProtectionRefusal(tc.cmd, parsed(t, rules...), code.PrincipalOperator)
		if got != tc.want || rule.Pattern != tc.by {
			t.Errorf("ProtectionRefusal(%s) = %q by %q, want %q by %q", tc.cmd.Ref, got,
				rule.Pattern, tc.want, tc.by)
		}
	}
}

// A COMMAND'S PROTECTION VERDICT ALLOCATES NOTHING, against the most rules a
// repository may carry, each of gitref's costliest shape: the decide asks it of
// every command of a batch, and the receiver of every command of a push, so an
// allocation a rule would be MaxProtectionsPerRepo of them a command — garbage
// at the rate the costliest round already spends its core on.
//
// THE WALK [code.CommitBudget] IS SIZED BY, never rules a ref leaves at their
// first part: the parts before a rule's first ** are compared once, at the
// ref's start, so rules named for sixty-four teams cost a ref of one team
// sixty-three comparisons and a single search. Every rule here starts its **
// at the ref's first branch component and holds a run of four globs before
// its second, so each searches the whole 255-byte ref. Both commands are
// DELETIONS, which every one of these rules refuses, so an answer of none
// says no rule matched.
//
// NOT PARALLEL, because an allocation count is the whole process's, and over
// A HUNDRED RUNS, which testing.AllocsPerRun divides and rounds down: a stray
// allocation of the process's own (search's fixture test names nats-server's
// sampler) divides to zero, and one a rule allocates a run never does.
func TestAProtectionVerdictAllocatesNothing(t *testing.T) {
	costliest := code.CostliestRules(code.MaxProtectionsPerRepo)
	for _, rule := range costliest {
		if !strings.HasPrefix(rule.Pattern, "refs/heads/**/") || strings.Count(rule.Pattern, "**") != 2 ||
			rule.AllowDelete {
			t.Fatalf("%s is not a run between two **s from the ref's first branch component, "+
				"refusing a deletion", rule.Pattern)
		}
	}
	// THE LAST RULE MATCHES every ref of the shape, so the deletion's walk
	// asks every costly rule before it and carries the one that refuses to
	// its end.
	refusing := append(costliest[:len(costliest)-1:len(costliest)-1], code.PushRule{
		Pattern: "refs/heads/" + code.WidestComponent + "/**", DirectPush: code.DirectPersons,
	})
	ref := code.WidestRef(0, 0)
	if len(ref) != gitref.MaxRefNameBytes {
		t.Fatalf("the ref is %d bytes, and the costliest is %d", len(ref), gitref.MaxRefNameBytes)
	}
	deletion := code.Command{Ref: ref, Old: strings.Repeat("b", 40), New: code.ZeroOID}
	for _, tc := range []struct {
		what  string
		rules []code.PushRule
		want  code.Refusal
	}{
		{"no rule matching", costliest, ""},
		{"the last rule refusing", refusing, code.RefusalProtectedDelete},
	} {
		ps := parsed(t, tc.rules...)
		var got code.Refusal
		if allocs := testing.AllocsPerRun(100, func() {
			got, _ = code.ProtectionRefusal(deletion, ps, code.PrincipalOperator)
		}); allocs != 0 {
			t.Errorf("%s: ProtectionRefusal allocated %v times a command, want none", tc.what, allocs)
		}
		if got != tc.want {
			t.Errorf("%s: ProtectionRefusal = %q, want %q — the case is not the one it measures",
				tc.what, got, tc.want)
		}
	}
}

// parsed is rules as ProtectionRefusal asks them.
func parsed(t *testing.T, rules ...code.PushRule) code.Protections {
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
		rules := parsed(t, code.PushRule{Pattern: pattern, DirectPush: code.DirectNone})
		for _, ref := range refs {
			want := gitref.MatchPattern(pattern, ref)
			r, _ := code.ProtectionRefusal(code.Command{Ref: ref, Old: code.ZeroOID,
				New: strings.Repeat("b", 40)}, rules, code.PrincipalOperator)
			got := r == code.RefusalProtectedDirect
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
		_, err := code.ParseProtections([]code.PushRule{
			{Pattern: "refs/heads/main", DirectPush: code.DirectNone},
			{Pattern: pattern, DirectPush: code.DirectNone},
		})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", pattern)) {
			t.Errorf("ParseProtections with %q: %v; want an error naming it", pattern, err)
		}
	}
	if got, rule := code.ProtectionRefusal(code.Command{Ref: "refs/heads/main", Old: code.ZeroOID,
		New: strings.Repeat("b", 40)}, code.Protections{}, code.PrincipalAgent); got != "" || rule != (code.PushRule{}) {
		t.Errorf("no rules refused %q by %+v", got, rule)
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

// A GONE REPOSITORY'S SENTENCE NAMES ITS STATE, and promises a restore only
// where declaring it again restores it: a deleted repository, which is told
// the instant its purge may begin where both the deletion's instant and the
// epoch's retention are known, and no instant where either is not — never a
// date computed from a zero. The code's own sentence, which a client sees
// where no detail was formed, holds for all four states, so it promises
// nothing.
func TestAGoneRepositorysSentenceNamesItsState(t *testing.T) {
	t.Parallel()
	const id = "0190f3c4-0000-7000-8000-00000000901e"
	deletedAt := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	for name, tc := range map[string]struct {
		gone  code.RepoGone
		want  []string
		never []string
	}{
		"deleted": {
			gone: code.RepoGone{ID: id, Name: "web", Status: code.RepoStatusDeleted,
				DeletedAt: deletedAt, Retention: week},
			want:  []string{"repository web is deleted", "restores it", "may begin from 2031-04-09T03:14:00Z"},
			never: []string{"new, empty"},
		},
		"deleted, its deletion's instant not known": {
			gone:  code.RepoGone{ID: id, Name: "web", Status: code.RepoStatusDeleted, Retention: week},
			want:  []string{"repository web is deleted", "restores it", "until its purge"},
			never: []string{"may begin from"},
		},
		"deleted, the epoch's retention not known": {
			gone:  code.RepoGone{ID: id, Name: "web", Status: code.RepoStatusDeleted, DeletedAt: deletedAt},
			want:  []string{"repository web is deleted", "restores it"},
			never: []string{"may begin from", "2031"},
		},
		"being purged": {
			gone: code.RepoGone{ID: id, Name: "web", Status: code.RepoStatusPurging,
				DeletedAt: deletedAt, Retention: week},
			want:  []string{"repository web is being purged", "cannot be undone", "new, empty repository"},
			never: []string{"restores", "2031"},
		},
		"purged": {
			gone:  code.RepoGone{ID: id, Name: "web", Purged: true},
			want:  []string{"repository web was purged", "new, empty repository"},
			never: []string{"restores"},
		},
		"purged, no name held": {
			gone:  code.RepoGone{ID: id, Purged: true},
			want:  []string{"repository " + id + " was purged", "declaring its name"},
			never: []string{"restores"},
		},
		"never created": {
			gone:  code.RepoGone{ID: id},
			want:  []string{"repository " + id + " does not exist"},
			never: []string{"restores", "purged"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tc.gone.Detail()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("%q does not say %q", got, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(got, never) {
					t.Errorf("%q says %q, which is false of this state", got, never)
				}
			}
			if strings.HasSuffix(got, ".") {
				t.Errorf("%q ends with a full stop, and a client prints it mid-line", got)
			}
		})
	}
	if s := code.RefusalRepoDeleted.Sentence(); strings.Contains(s, "restore") {
		t.Errorf("repo_deleted's own sentence %q promises a restore, which is false of a "+
			"repository being purged, purged, or never created", s)
	}
}

// EVERY SENTENCE A RECEIVER WRITES INTO A PROPOSAL FITS IT, at its longest:
// a ref of MaxRefNameBytes refused under each protection flag of a rule whose
// pattern is as long as a pattern may be — a seat's push under a rule that
// takes a person's alone among them, the longest of them, which a receiver of
// a seat's push forms — and stale at an id the repository does not hold. Each
// names no ref — git prints the ref beside it — and a proposal carrying it
// beside an update still standing validates, so the code host never has a
// push refused whole for a sentence of its own.
//
// Mutation: spell the persons rule's sentence with the remedy the code host's
// table gives ("…; a seat's change reaches it when a person pushes it") and it
// no longer fits.
func TestEveryReceiverSentenceFitsAProposal(t *testing.T) {
	t.Parallel()
	ref := "refs/heads/" + strings.Repeat("r", code.MaxRefNameBytes-len("refs/heads/"))
	pattern := "refs/tags/" + strings.Repeat("p", code.MaxRefNameBytes-len("refs/tags/"))
	if !gitref.ValidPattern(pattern) || !gitref.ValidRef(ref) {
		t.Fatalf("the longest ref (%v) and pattern (%v) must be ones the grammar takes",
			gitref.ValidRef(ref), gitref.ValidPattern(pattern))
	}
	rule := code.PushRule{Pattern: pattern, DirectPush: code.DirectNone}
	persons := code.PushRule{Pattern: pattern, DirectPush: code.DirectPersons}
	for _, tc := range []struct {
		what   string
		reason code.Refusal
		detail string
	}{
		{"direct_push none", code.RefusalProtectedDirect, code.ProtectionDetail(rule, code.RefusalProtectedDirect)},
		{"direct_push persons, to a seat", code.RefusalProtectedDirect,
			code.ProtectionDetail(persons, code.RefusalProtectedDirect)},
		{"allow_delete", code.RefusalProtectedDelete, code.ProtectionDetail(rule, code.RefusalProtectedDelete)},
		{"allow_force", code.RefusalProtectedForce, code.ProtectionDetail(rule, code.RefusalProtectedForce)},
		{"an id never held", code.RefusalStale, code.UnheldDetail(oidOf("an id this repository never held"))},
	} {
		if strings.Contains(tc.detail, "refs/heads/") {
			t.Errorf("%s's sentence names a ref: %q", tc.what, tc.detail)
		}
		p := proposal("0190f3c4-0000-7000-8000-00000000c0de",
			code.Command{Ref: ref, Old: oidOf("A"), New: oidOf("B"), Result: code.ResultNG, Reason: tc.reason,
				Detail: tc.detail},
			update("refs/heads/main", oidOf("A"), oidOf("C")))
		if err := p.Validate(); err != nil {
			t.Errorf("a proposal carrying %s's sentence at its longest (%d bytes): %v", tc.what, len(tc.detail), err)
		}
	}
}

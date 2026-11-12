package gitlet_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// THE MERGE GATE, JUDGED — gitlet.JudgeGate over facts a case builds by hand,
// one snapshot's rows and one epoch's chart, so each case holds the judgment
// alone; the reads that gather the facts from the rows are held by the
// readiness cases, which read a change through the shipped reader.

// gateChart is an organization two teams deep under one lead:
//
//	Engineering (id eng, lead cto)
//	  Web (id web, lead web-lead)
//	  Data (id data, lead data-lead)
//
// with dev in Web, a seat that only reads (reader, in Web) and the person
// founder — so a repository Engineering owns is reviewed by both teams'
// leads (they write it, their tier reviews) and maintained by the cto.
func gateChart() chart {
	seat := func(handle string, tier gitlet.Tier, unit string) gitlet.RosterSeat {
		return gitlet.RosterSeat{Handle: handle, Tier: tier, Unit: unit}
	}
	return chart{
		units: []chartUnit{
			{id: "eng", name: "Engineering", lead: "cto"},
			{id: "web", name: "Web", parent: "eng", lead: "web-lead"},
			{id: "data", name: "Data", parent: "eng", lead: "data-lead"},
		},
		seats: map[string]gitlet.RosterSeat{
			"cto":       seat("cto", gitlet.TierFullAccess, "eng"),
			"web-lead":  seat("web-lead", gitlet.TierFullAccess, "web"),
			"data-lead": seat("data-lead", gitlet.TierFullAccess, "data"),
			"dev":       seat("dev", gitlet.TierFullAccess, "web"),
			"reader":    seat("reader", gitlet.TierReadOnly, "web"),
			"founder":   {Handle: "founder", Human: true, Tier: gitlet.TierFullAccess, Unit: "eng"},
		},
	}
}

var (
	gateDev     = gitlet.Party{Handle: "dev", Kind: gitlet.PartyAgent}
	gateFounder = gitlet.Party{Handle: "founder", Kind: gitlet.PartyPerson}
	gateCTO     = gitlet.Party{Handle: "cto", Kind: gitlet.PartyAgent}
	gateWebLead = gitlet.Party{Handle: "web-lead", Kind: gitlet.PartyAgent}
	gateDataLd  = gitlet.Party{Handle: "data-lead", Kind: gitlet.PartyAgent}
)

// gateCase is dev's change web!42, from its own branch into main on platform,
// a repository Engineering owns, at revision 2: its facts against main
// recorded, touching touched — matched against the owners and the human
// paths the snapshot's repository row digests — merging cleanly, and
// nobody's approval yet, under the chart gateChart draws. Which human paths
// hold it is its home's match, which a case makes with matchedBy.
func gateCase(touched ...string) gitlet.GateFacts {
	return gitlet.GateFacts{
		Rows: gitlet.ReadinessRows{
			Repo: gitlet.RepoRef{ID: "id-platform", Name: "platform", Visibility: gitlet.VisibilityCompany,
				Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}},
			Change: gitlet.ChangeRow{ID: "change-42", RepoID: "id-platform", Number: 42, State: gitlet.ChangeStateOpen,
				SourceRef: "refs/heads/seats/dev/retry", TargetRef: mainRef, Head: oidOf("B2"), Revision: 2,
				Owner: "dev", OwnerKind: gitlet.PartyAgent},
			Authors: []gitlet.Party{gateDev},
			Facts: []gitlet.RevisionPatch{
				{N: 1, TargetRef: mainRef, BaseTree: oidOf("base"), HeadTree: oidOf("tree 1"), PatchID: oidOf("diff 1")},
				{N: 2, TargetRef: mainRef, BaseTree: oidOf("base"), HeadTree: oidOf("tree 2"), PatchID: oidOf("diff 2")},
			},
			Merge: &gitlet.RecordedMerge{Revision: 2, TargetRef: mainRef, TargetOID: oidOf("A"), State: gitlet.MergeClean},
		},
		Head: gitlet.HeadFacts{Recorded: true, TouchedCount: len(touched), OwnersDigest: oidOf("owners"),
			HumanPathsDigest: oidOf("human paths")},
		HumanPathsDigest: oidOf("human paths"),
		Roster:           gateChart(),
	}
}

// matchedBy is f with its head revision's human paths matched as its home
// matches them (gitlet.MatchHumanPaths): touched, the paths it touches, against
// the human paths rules hold its target to, classed by f's classes.
func matchedBy(f gitlet.GateFacts, rules gitlet.GateRules, touched ...string) gitlet.GateFacts {
	f.Head.HumanPaths = gitlet.MatchHumanPaths(rules.HumanPaths, touched, f.Head.TouchesAll, f.Head.Classes)
	return f
}

// approving is f with reviewer's standing verdict approving revision.
func approving(f gitlet.GateFacts, reviewer gitlet.Party, revision int64) gitlet.GateFacts {
	f.Rows.Approvals = append(slices.Clone(f.Rows.Approvals), gitlet.Approval{Reviewer: reviewer, Revision: revision})
	return f
}

// gated is main's one rule carrying gate, under no floor.
func gated(t *testing.T, gate gitlet.MergeGate) gitlet.GateRules {
	t.Helper()
	r, err := gitlet.GateRulesOf([]gitlet.Protection{{PushRule: gitlet.PushRule{Pattern: mainRef,
		DirectPush: gitlet.DirectPersons}, Gate: &gate}}, mainRef, "")
	if err != nil {
		t.Fatalf("GateRulesOf: %v", err)
	}
	return r
}

// bareGate is a gate holding nothing but its defaults — writers, no
// approval, a queue at 4/4, seats — which a case sets a term of.
func bareGate() gitlet.MergeGate {
	return gitlet.MergeGate{MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone,
		Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteSeats}
}

// judgedAs fails t unless r's precondition of kind is in state, its sentence
// holding says.
func judgedAs(t *testing.T, r gitlet.Readiness, kind gitlet.PreconditionKind, state gitlet.PreconditionState,
	says string) gitlet.Precondition {

	t.Helper()
	i := slices.IndexFunc(r.Preconditions, func(p gitlet.Precondition) bool { return p.Kind == kind })
	if i < 0 {
		t.Fatalf("the readiness %+v judges no %s", r, kind)
	}
	p := r.Preconditions[i]
	if p.State != state || !strings.Contains(p.Detail, says) {
		t.Fatalf("%s is %s (%q), want %s saying %q", kind, p.State, p.Detail, state, says)
	}
	return p
}

// OWNERS_APPROVE NEEDS ONE COMMON APPROVER: a change touching the paths of
// two owners, Web and Data, waits for ONE counted approval from somebody
// eligible for both — a lead on both teams' lead chains (the cto) or a
// person. Web's lead and Data's lead each approving, each eligible for one of
// them alone, is no such approval, and the term waits on the cto; the cto's
// approval meets it, and so does a person's.
//
// Mutation: count an approval eligible for any one touched owner (any
// approver per glob) — the two leads meet the term, red.
func TestOwnersApproveNeedsOneCommonApprover(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.OwnersApprove = true
	rules := gated(t, gate)
	f := gateCase("web/app.go", "data/schema.sql")
	f.OwnerRowsDigest = f.Head.OwnersDigest
	f.Head.RequestedOwners = []gitlet.Owner{{Kind: gitlet.OwnerUnit, Ref: "Data"}, {Kind: gitlet.OwnerUnit, Ref: "Web"}}

	both := approving(approving(f, gateWebLead, 2), gateDataLd, 2)
	p := judgedAs(t, gitlet.JudgeGate(both, rules), gitlet.PreconditionOwnersApproval, gitlet.PreconditionUnmet,
		"no one approval counts for all of them")
	if !slices.Equal(p.WaitsOn, []gitlet.Party{gateCTO}) {
		t.Errorf("the unmet owners' approval waits on %+v, want the one common lead, the cto", p.WaitsOn)
	}
	judgedAs(t, gitlet.JudgeGate(approving(both, gateCTO, 2), rules), gitlet.PreconditionOwnersApproval,
		gitlet.PreconditionMet, `seat "cto"'s approval counts for every path owner`)
	judgedAs(t, gitlet.JudgeGate(approving(f, gateFounder, 2), rules), gitlet.PreconditionOwnersApproval,
		gitlet.PreconditionMet, `person "founder"'s approval counts`)
	// A SEAT OWNER's chain is the seat and its unit's leads: reader's own
	// path and Web's share Web's lead and the cto.
	f.Head.RequestedOwners = []gitlet.Owner{{Kind: gitlet.OwnerSeat, Ref: "reader"}, {Kind: gitlet.OwnerUnit, Ref: "Web"}}
	judgedAs(t, gitlet.JudgeGate(approving(f, gateWebLead, 2), rules), gitlet.PreconditionOwnersApproval,
		gitlet.PreconditionMet, `seat "web-lead"'s approval counts`)
}

// AN OWNER ROW CHANGE LEAVES OWNERS_APPROVE COMPUTING: the head revision's
// facts recorded the owners its home matched under one digest of the
// repository's owner rows, and the snapshot holds rows of another — owners
// added or removed since, which the home has not matched yet. Judged on the
// stale match, the removed owner would still hold the change and an added one
// never; so the term waits, computing, for the home's next revise — and the
// same facts under their own digest are judged.
//
// Mutation: judge the recorded match whatever the snapshot's digest — the
// first readiness reads met, red.
func TestAnOwnerRowChangeLeavesOwnersApproveComputing(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.OwnersApprove = true
	rules := gated(t, gate)
	f := approving(gateCase("web/app.go"), gateWebLead, 2)
	f.Head.RequestedOwners = []gitlet.Owner{{Kind: gitlet.OwnerUnit, Ref: "Web"}}
	f.OwnerRowsDigest = oidOf("owners since")
	r := gitlet.JudgeGate(f, rules)
	judgedAs(t, r, gitlet.PreconditionOwnersApproval, gitlet.PreconditionComputing,
		"path owners changed since revision 2's facts matched")
	if r.Ready() {
		t.Error("a change whose owners' approval is computing reads ready")
	}
	f.OwnerRowsDigest = f.Head.OwnersDigest
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionOwnersApproval, gitlet.PreconditionMet,
		`seat "web-lead"'s approval counts`)
}

// A STALE APPROVAL DOES NOT COUNT: the founder approved revision 1, and
// revision 2 holds other code and another diff — so neither the gate's
// approval, nor its person's, nor its owners' counts that approval, each
// counted by M4's one rule. Revision 2 carrying revision 1's diff unchanged
// (equal patch ids) carries it to all three.
//
// Mutation: count an approval of any revision of the change — revision 2
// reads approved by the founder, red.
func TestAStaleApprovalDoesNotCount(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.Approvals, gate.OwnersApprove = gitlet.ApprovalsOne, true
	gate.HumanPaths = []gitlet.HumanPath{{Glob: "deploy/**"}}
	rules := gated(t, gate)
	f := matchedBy(approving(gateCase("deploy/app.yaml"), gateFounder, 1), rules, "deploy/app.yaml")
	f.Head.RequestedOwners = []gitlet.Owner{{Kind: gitlet.OwnerUnit, Ref: "Web"}}
	f.OwnerRowsDigest = f.Head.OwnersDigest
	stale := gitlet.JudgeGate(f, rules)
	judgedAs(t, stale, gitlet.PreconditionApproval, gitlet.PreconditionUnmet, "no approval counts on revision 2")
	judgedAs(t, stale, gitlet.PreconditionHumanApproval, gitlet.PreconditionUnmet, "no person who may approve it")
	judgedAs(t, stale, gitlet.PreconditionOwnersApproval, gitlet.PreconditionUnmet, "no one approval counts")

	f.Rows.Facts[1].PatchID = f.Rows.Facts[0].PatchID
	carried := gitlet.JudgeGate(f, rules)
	judgedAs(t, carried, gitlet.PreconditionApproval, gitlet.PreconditionMet, "carries the diff")
	judgedAs(t, carried, gitlet.PreconditionHumanApproval, gitlet.PreconditionMet, `person "founder"'s approval counts`)
	judgedAs(t, carried, gitlet.PreconditionOwnersApproval, gitlet.PreconditionMet, `person "founder"'s approval`)
}

// AN UNLESS CLASS WAIVES ONLY ITS ENTRY: docs/** is a person's unless every
// path it matched is comment_only, deploy/** a person's always. A change
// whose docs paths are all comment-only is held by deploy/** alone; one
// touching docs alone, comment-only, is held by none — a whitespace_only path
// counting as comment_only, the narrower class — and one docs path of real
// code holds it by docs/** again, its comment-only neighbour waiving nothing
// for it.
//
// Mutation: waive an entry where ANY path it matched is of a class its unless
// names — the last change reads met, red.
func TestAnUnlessClassWaivesOnlyItsEntry(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.HumanPaths = []gitlet.HumanPath{
		{Glob: "deploy/**"},
		{Glob: "docs/**", Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}},
	}
	rules := gated(t, gate)

	f := gateCase("deploy/app.yaml", "docs/guide.go", "docs/notes.go")
	f.Head.Classes = gitlet.RevisionClasses{WhitespaceOnly: []int{2}, CommentOnly: []int{1}, Complete: true}
	f = matchedBy(f, rules, "deploy/app.yaml", "docs/guide.go", "docs/notes.go")
	p := judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionHumanApproval, gitlet.PreconditionUnmet,
		`touches "deploy/**"`)
	if strings.Contains(p.Detail, "docs/**") {
		t.Errorf("a docs entry its unless waived still holds the change: %q", p.Detail)
	}

	f = gateCase("docs/guide.go", "docs/notes.go")
	f.Head.Classes = gitlet.RevisionClasses{WhitespaceOnly: []int{1}, CommentOnly: []int{0}, Complete: true}
	f = matchedBy(f, rules, "docs/guide.go", "docs/notes.go")
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionHumanApproval, gitlet.PreconditionMet,
		`touches "docs/**" only with changes of a class its unless waives`)

	f = gateCase("docs/guide.go", "docs/server.go")
	f.Head.Classes = gitlet.RevisionClasses{CommentOnly: []int{0}, Complete: true}
	f = matchedBy(f, rules, "docs/guide.go", "docs/server.go")
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionHumanApproval, gitlet.PreconditionUnmet,
		`touches "docs/**"`)

	// A REVISION THAT TOUCHES ALL is held by every entry, which no class
	// waives.
	f = gateCase()
	f.Head.TouchesAll = true
	f = matchedBy(f, rules)
	p = judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionHumanApproval, gitlet.PreconditionUnmet,
		`touches "deploy/**"`)
	if !strings.Contains(p.Detail, "docs/**") {
		t.Errorf("a revision touching all is not held by docs/**: %q", p.Detail)
	}
}

// A HUMAN PATH CHANGE LEAVES HUMAN_APPROVAL COMPUTING: the head revision's
// facts recorded which human paths hold it as its home matched them, under
// one digest of the repository's rules' human paths, and the snapshot's
// repository row digests them to another — a human path added or removed
// since, which the home has not matched yet. Judged on the stale match, a
// removed path would still hold the change and an added one never, and its
// places name entries of a list that is not the gate's; so the term waits,
// computing, for the home's next revise — and the same facts under their own
// digest are judged. A match naming a place past the gate's human paths is
// read as no match of this gate's either.
//
// Mutation: judge the recorded match whatever the snapshot's digest — the
// first readiness reads unmet, red.
func TestAHumanPathChangeLeavesHumanApprovalComputing(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.HumanPaths = []gitlet.HumanPath{{Glob: "deploy/**"}}
	rules := gated(t, gate)
	f := matchedBy(gateCase("deploy/app.yaml"), rules, "deploy/app.yaml")
	f.HumanPathsDigest = oidOf("human paths since")
	r := gitlet.JudgeGate(f, rules)
	judgedAs(t, r, gitlet.PreconditionHumanApproval, gitlet.PreconditionComputing,
		"human paths changed since revision 2's facts matched")
	if r.Ready() {
		t.Error("a change whose person's approval is computing reads ready")
	}
	f.HumanPathsDigest = f.Head.HumanPathsDigest
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionHumanApproval, gitlet.PreconditionUnmet,
		`touches "deploy/**"`)
	f.Head.HumanPaths = gitlet.HumanPathMatch{Held: []int{len(rules.HumanPaths)}}
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionHumanApproval, gitlet.PreconditionComputing,
		"human paths changed since")
}

// AN APPROVALS_UNLESS CLASS EXEMPTS ONLY A WHOLLY CLASSED REVISION: the gate
// asks an approval unless every path is comment_only, and a revision is
// exempt only where its classification is complete and every path it touches
// is listed under a class it names — a whitespace_only change counting as a
// comment_only one — which the gate counts, reading how many paths it
// touches, never the paths. One path of no class, an incomplete
// classification, or a comment_only path under a gate exempting
// whitespace_only alone each leave the approval asked.
//
// Mutation: exempt a revision whose classed paths are fewer than its touched
// paths — the second revision reads met, red.
func TestAnApprovalsUnlessClassExemptsOnlyAWhollyClassedRevision(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.Approvals, gate.ApprovalsUnless = gitlet.ApprovalsOne, []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}
	rules := gated(t, gate)

	f := gateCase("a.go", "b.go")
	f.Head.Classes = gitlet.RevisionClasses{CommentOnly: []int{0}, WhitespaceOnly: []int{1}, Complete: true}
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionApproval, gitlet.PreconditionMet,
		`needs no approval: every path it touches is "comment_only" and "whitespace_only"`)

	f.Head.Classes = gitlet.RevisionClasses{CommentOnly: []int{0}, Complete: true}
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionApproval, gitlet.PreconditionUnmet, "no approval")

	f.Head.Classes = gitlet.RevisionClasses{CommentOnly: []int{0}, WhitespaceOnly: []int{1}}
	judgedAs(t, gitlet.JudgeGate(f, rules), gitlet.PreconditionApproval, gitlet.PreconditionUnmet, "no approval")

	gate.ApprovalsUnless = []gitlet.ChangeClass{gitlet.ChangeClassWhitespaceOnly}
	f.Head.Classes = gitlet.RevisionClasses{CommentOnly: []int{0}, WhitespaceOnly: []int{1}, Complete: true}
	judgedAs(t, gitlet.JudgeGate(f, gated(t, gate)), gitlet.PreconditionApproval, gitlet.PreconditionUnmet,
		"no approval")
}

// AN EXTERNAL CHECK WAITS ON ITS REPORT: a required check with no attempt on
// the head commit — one the target's checks file defines, which the home
// queues, or one it does not, which a person's CI reports — is computing,
// waiting on nobody; a report of it running is computing still; a report that
// it passed meets the term, and one that it failed leaves it unmet, waiting on
// the change's owner.
//
// Mutation: read a required check with no attempt as met — the first
// readiness is ready, red.
func TestAnExternalCheckWaitsOnItsReport(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.Checks = []string{"ci"}
	rules := gated(t, gate)
	f := gateCase("app.go")
	r := gitlet.JudgeGate(f, rules)
	if p := judgedAs(t, r, gitlet.PreconditionCheck, gitlet.PreconditionComputing, "ci (no attempt yet)"); len(p.WaitsOn) > 0 {
		t.Errorf("a check nobody reported waits on %+v, want nobody", p.WaitsOn)
	}
	if r.Ready() {
		t.Fatal("a change whose required check has no attempt reads ready")
	}
	reported := func(state gitlet.CheckState) gitlet.GateFacts {
		f.Checks = []gitlet.GateCheck{{Name: "ci", Attempt: 1, State: state, Reason: gitlet.CheckReasonReported,
			Placement: gitlet.PlacementExternal}}
		return f
	}
	judgedAs(t, gitlet.JudgeGate(reported(gitlet.CheckStateRunning), rules), gitlet.PreconditionCheck,
		gitlet.PreconditionComputing, "ci (running, attempt 1)")
	judgedAs(t, gitlet.JudgeGate(reported(gitlet.CheckStatePassed), rules), gitlet.PreconditionCheck,
		gitlet.PreconditionMet, "ci (passed)")
	p := judgedAs(t, gitlet.JudgeGate(reported(gitlet.CheckStateFailed), rules), gitlet.PreconditionCheck,
		gitlet.PreconditionUnmet, "ci (failed, attempt 1) did not pass")
	if !slices.Equal(p.WaitsOn, []gitlet.Party{gateDev}) {
		t.Errorf("a failed check waits on %+v, want the change's owner", p.WaitsOn)
	}
}

// THE GATE NAMES EVERY UNMET TERM: a change that meets none of its target's
// gate — no approval, a human path touched with no person asked, two owners
// touched, a check failed, and a merger that only writes where maintainers
// merge — lists every term unmet, each naming whom it waits on (a merge only
// a maintainer or a person makes, the maintaining seat), in the order a
// change moves through; its first sentence is its earliest; and a gate that
// asks a term of nothing lists it not at all: an ungated branch asks no
// approval.
//
// Mutation: leave a term out of JudgeGate — the case's list of unmet terms
// is short of it, red.
func TestTheGateNamesEveryUnmetTerm(t *testing.T) {
	t.Parallel()
	gate := bareGate()
	gate.MergeBy, gate.Approvals, gate.OwnersApprove = gitlet.MergeByMaintainers, gitlet.ApprovalsOne, true
	gate.HumanPaths = []gitlet.HumanPath{{Glob: "deploy/**"}}
	gate.Checks = []string{"lint", "unit"}
	rules := gated(t, gate)
	f := matchedBy(gateCase("deploy/app.yaml", "data/schema.sql"), rules, "deploy/app.yaml", "data/schema.sql")
	f.OwnerRowsDigest = f.Head.OwnersDigest
	f.Head.RequestedOwners = []gitlet.Owner{{Kind: gitlet.OwnerUnit, Ref: "Data"}, {Kind: gitlet.OwnerUnit, Ref: "Web"}}
	f.Checks = []gitlet.GateCheck{
		{Name: "lint", Attempt: 1, State: gitlet.CheckStateFailed, Reason: gitlet.CheckReasonExit},
		{Name: "unit", Attempt: 2, State: gitlet.CheckStatePassed, Reason: gitlet.CheckReasonExit},
	}
	f.Merger = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	r := gitlet.JudgeGate(f, rules)
	var unmet []gitlet.PreconditionKind
	for _, p := range r.Preconditions {
		if p.State != gitlet.PreconditionUnmet {
			continue
		}
		unmet = append(unmet, p.Kind)
		if len(p.WaitsOn) == 0 || p.Detail == "" {
			t.Errorf("unmet %s names nobody it waits on (%+v) or no sentence (%q)", p.Kind, p.WaitsOn, p.Detail)
		}
	}
	want := []gitlet.PreconditionKind{gitlet.PreconditionApproval, gitlet.PreconditionHumanApproval,
		gitlet.PreconditionOwnersApproval, gitlet.PreconditionCheck, gitlet.PreconditionMergeAuthority}
	if !slices.Equal(unmet, want) {
		t.Fatalf("the unmet terms are %v, want %v", unmet, want)
	}
	if first := r.Preconditions[slices.IndexFunc(r.Preconditions, func(p gitlet.Precondition) bool {
		return p.Kind == gitlet.PreconditionApproval
	})].Detail; r.First != first {
		t.Errorf("the first sentence is %q, want the approval's, %q", r.First, first)
	}
	p := judgedAs(t, r, gitlet.PreconditionMergeAuthority, gitlet.PreconditionUnmet, "takes maintain")
	if !slices.Equal(p.WaitsOn, []gitlet.Party{gateCTO}) {
		t.Errorf("a merge only a maintainer makes waits on %+v, want the seat that maintains the repository",
			p.WaitsOn)
	}

	// AN UNGATED BRANCH asks no approval, no person, no owner and no check.
	open, err := gitlet.GateRulesOf(nil, mainRef, gitlet.MergeByPersons)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range gitlet.JudgeGate(f, open).Preconditions {
		switch p.Kind {
		case gitlet.PreconditionApproval, gitlet.PreconditionHumanApproval, gitlet.PreconditionOwnersApproval,
			gitlet.PreconditionCheck:
			t.Errorf("a branch no rule protects judges %s: %q", p.Kind, p.Detail)
		}
	}
}

// THE MERGE AUTHORITY IS THE STRICTEST RULE AND THE FLOOR: a person merges
// anything; a seat that writes merges where every rule matching the branch
// and the company's floor let writers; a second rule on the branch, or the
// floor, asking maintainers holds it to the maintainer; a floor reaches no
// branch no rule protects, where a merge is whoever may push the branch.
//
// Mutation: hold a merge to the first matching rule alone — the second rule's
// maintainers is never met, red.
func TestTheMergeAuthorityIsTheStrictestRuleAndTheFloor(t *testing.T) {
	t.Parallel()
	dev := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	founder := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:founder", Seat: "founder"}
	writers, maintainers := bareGate(), bareGate()
	maintainers.MergeBy = gitlet.MergeByMaintainers
	rule := func(pattern string, g gitlet.MergeGate) gitlet.Protection {
		return gitlet.Protection{PushRule: gitlet.PushRule{Pattern: pattern, DirectPush: gitlet.DirectPersons}, Gate: &g}
	}
	for _, tc := range []struct {
		name   string
		rules  []gitlet.Protection
		floor  gitlet.MergeBy
		merger gitlet.Principal
		want   gitlet.PreconditionState
	}{
		{"a writer under writers", []gitlet.Protection{rule(mainRef, writers)}, "", dev, gitlet.PreconditionMet},
		{"a writer under a second rule's maintainers", []gitlet.Protection{rule(mainRef, writers),
			rule("refs/heads/*", maintainers)}, "", dev, gitlet.PreconditionUnmet},
		{"a writer under the floor's maintainers", []gitlet.Protection{rule(mainRef, writers)},
			gitlet.MergeByMaintainers, dev, gitlet.PreconditionUnmet},
		{"the maintainer under the floor's maintainers", []gitlet.Protection{rule(mainRef, writers)},
			gitlet.MergeByMaintainers, cto, gitlet.PreconditionMet},
		{"a seat under the floor's persons", []gitlet.Protection{rule(mainRef, writers)}, gitlet.MergeByPersons, cto,
			gitlet.PreconditionUnmet},
		{"a person under the floor's persons", []gitlet.Protection{rule(mainRef, writers)}, gitlet.MergeByPersons,
			founder, gitlet.PreconditionMet},
		{"a writer on a branch no rule protects", nil, gitlet.MergeByPersons, dev, gitlet.PreconditionMet},
	} {
		r, err := gitlet.GateRulesOf(tc.rules, mainRef, tc.floor)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		f := gateCase("app.go")
		f.Merger = tc.merger
		p := judgedAs(t, gitlet.JudgeGate(f, r), gitlet.PreconditionMergeAuthority, tc.want, "")
		if tc.want == gitlet.PreconditionUnmet && strings.TrimSpace(p.Detail) == "" {
			t.Errorf("%s: an unmet merge authority says nothing", tc.name)
		}
	}
}

// A GATE THAT KEEPS A SEAT'S BRANCH WINS: a merge deletes a source in a
// seat's namespace unless a gate matching its target keeps it — the one of
// the two answers no later write undoes — so one rule saying `none` keeps it
// whatever another says, a value this build does not know keeps it, and a
// target no gate matches keeps nothing (its merge's caller deletes the seat's
// branch).
//
// Mutation: KeepsSource ORed with `false` → the none and unknown cases, red.
func TestAGateThatKeepsASeatsBranchWins(t *testing.T) {
	keeping := bareGate()
	keeping.DeleteSource = gitlet.DeleteNone
	unknown := bareGate()
	unknown.DeleteSource = "archive"
	rule := func(pattern string, g gitlet.MergeGate) gitlet.Protection {
		return gitlet.Protection{PushRule: gitlet.PushRule{Pattern: pattern, DirectPush: gitlet.DirectPersons}, Gate: &g}
	}
	for _, tc := range []struct {
		name  string
		rules []gitlet.Protection
		want  bool
	}{
		{"seats deletes", []gitlet.Protection{rule(mainRef, bareGate())}, false},
		{"none keeps", []gitlet.Protection{rule(mainRef, keeping)}, true},
		{"none beside seats keeps", []gitlet.Protection{rule(mainRef, bareGate()), rule("refs/heads/*", keeping)},
			true},
		{"an unknown value keeps", []gitlet.Protection{rule(mainRef, unknown)}, true},
		{"an ungated target keeps nothing", []gitlet.Protection{rule("refs/heads/release", keeping)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := gitlet.GateRulesOf(tc.rules, mainRef, "")
			if err != nil {
				t.Fatalf("GateRulesOf: %v", err)
			}
			if r.KeepsSource != tc.want {
				t.Errorf("KeepsSource = %v, want %v", r.KeepsSource, tc.want)
			}
		})
	}
}

// A CHECKS FILE CHANGE NEEDS A PERSON (U44): a rule requiring checks holds a
// change touching gitlet.ChecksDirGlob — the directory its checks file lives in
// — to a PERSON'S approval, as a human path its record never names: a seat's
// counted approval leaves the term unmet, naming the glob and waiting on the
// change's owner to ask a person, and a person's meets it. The combined gate
// names it among its human paths — what decides that a snapshot reads the
// change's touched paths at all — though no rule's record lists it.
// agents_edit_checks opts the rule out; a rule requiring no check names it
// not at all; a change touching nothing under it is met; and the opt-out is
// the RULE'S: a second rule requiring checks without it still holds the
// change.
//
// Mutation: drop the default from GateRulesOf — a seat's approval of a
// checks file change reads met (no human_approval term at all), red.
func TestAChecksFileChangeNeedsAPerson(t *testing.T) {
	t.Parallel()
	if _, ok := strings.CutPrefix(gitlet.ChecksFilePath, strings.TrimSuffix(gitlet.ChecksDirGlob, "**")); !ok {
		t.Fatalf("the checks file %s lies outside %s, the human path that holds it", gitlet.ChecksFilePath,
			gitlet.ChecksDirGlob)
	}
	checked := bareGate()
	checked.Checks = []string{"unit"}
	rules := gated(t, checked)
	if !slices.ContainsFunc(rules.HumanPaths, func(h gitlet.HumanPath) bool {
		return h.Glob == gitlet.ChecksDirGlob && len(h.Unless) == 0
	}) {
		t.Fatalf("a gate requiring checks names the human paths %+v, want %s among them", rules.HumanPaths,
			gitlet.ChecksDirGlob)
	}

	f := matchedBy(gateCase(gitlet.ChecksFilePath, "app.go"), rules, gitlet.ChecksFilePath, "app.go")
	f.Checks = []gitlet.GateCheck{{Name: "unit", Attempt: 1, State: gitlet.CheckStatePassed, Reason: gitlet.CheckReasonExit}}
	bySeat := gitlet.JudgeGate(approving(f, gateWebLead, 2), rules)
	p := judgedAs(t, bySeat, gitlet.PreconditionHumanApproval, gitlet.PreconditionUnmet, gitlet.ChecksDirGlob)
	if !slices.Equal(p.WaitsOn, []gitlet.Party{gateDev}) {
		t.Errorf("a checks file change no person is asked of waits on %+v, want its owner to ask one", p.WaitsOn)
	}
	if bySeat.Ready() {
		t.Fatal("a checks file change a seat alone approved reads ready to merge")
	}
	judgedAs(t, gitlet.JudgeGate(approving(f, gateFounder, 2), rules), gitlet.PreconditionHumanApproval,
		gitlet.PreconditionMet, `person "founder"'s approval counts`)
	judgedAs(t, gitlet.JudgeGate(matchedBy(gateCase("app.go"), rules, "app.go"), rules), gitlet.PreconditionHumanApproval,
		gitlet.PreconditionMet, "touches none of")

	// AGENTS_EDIT_CHECKS OPTS THE RULE OUT, and a rule requiring no check
	// holds nothing: a seat's approval merges the same change.
	opted := checked
	opted.AgentsEditChecks = true
	unchecked := bareGate()
	for name, gate := range map[string]gitlet.MergeGate{"agents_edit_checks": opted, "no checks": unchecked} {
		r := gitlet.JudgeGate(approving(f, gateWebLead, 2), gated(t, gate))
		if slices.ContainsFunc(r.Preconditions, func(p gitlet.Precondition) bool {
			return p.Kind == gitlet.PreconditionHumanApproval
		}) {
			t.Errorf("%s: a checks file change waits for a person: %+v", name, r.Preconditions)
		}
		if !r.Ready() {
			t.Errorf("%s: a seat-approved checks file change is not ready: %q", name, r.First)
		}
	}

	// THE OPT-OUT IS THE RULE'S: a second rule matching main that requires
	// checks without it holds the change, the strictest of the two.
	rule := func(pattern string, g gitlet.MergeGate) gitlet.Protection {
		return gitlet.Protection{PushRule: gitlet.PushRule{Pattern: pattern, DirectPush: gitlet.DirectPersons}, Gate: &g}
	}
	both, err := gitlet.GateRulesOf([]gitlet.Protection{rule(mainRef, opted), rule("refs/heads/*", checked)}, mainRef, "")
	if err != nil {
		t.Fatalf("GateRulesOf: %v", err)
	}
	judgedAs(t, gitlet.JudgeGate(matchedBy(approving(f, gateWebLead, 2), both, gitlet.ChecksFilePath, "app.go"), both),
		gitlet.PreconditionHumanApproval, gitlet.PreconditionUnmet, gitlet.ChecksDirGlob)
}

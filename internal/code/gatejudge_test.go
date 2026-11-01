package code_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// THE MERGE GATE, JUDGED — code.JudgeGate over facts a case builds by hand,
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
	seat := func(handle string, tier code.Tier, unit string) code.RosterSeat {
		return code.RosterSeat{Handle: handle, Tier: tier, Unit: unit}
	}
	return chart{
		units: []chartUnit{
			{id: "eng", name: "Engineering", lead: "cto"},
			{id: "web", name: "Web", parent: "eng", lead: "web-lead"},
			{id: "data", name: "Data", parent: "eng", lead: "data-lead"},
		},
		seats: map[string]code.RosterSeat{
			"cto":       seat("cto", code.TierFullAccess, "eng"),
			"web-lead":  seat("web-lead", code.TierFullAccess, "web"),
			"data-lead": seat("data-lead", code.TierFullAccess, "data"),
			"dev":       seat("dev", code.TierFullAccess, "web"),
			"reader":    seat("reader", code.TierReadOnly, "web"),
			"founder":   {Handle: "founder", Human: true, Tier: code.TierFullAccess, Unit: "eng"},
		},
	}
}

var (
	gateDev     = code.Party{Handle: "dev", Kind: code.PartyAgent}
	gateFounder = code.Party{Handle: "founder", Kind: code.PartyPerson}
	gateCTO     = code.Party{Handle: "cto", Kind: code.PartyAgent}
	gateWebLead = code.Party{Handle: "web-lead", Kind: code.PartyAgent}
	gateDataLd  = code.Party{Handle: "data-lead", Kind: code.PartyAgent}
)

// gateCase is dev's change web!42, from its own branch into main on platform,
// a repository Engineering owns, at revision 2: its facts against main
// recorded, touching touched, merging cleanly — and nobody's approval yet,
// under the chart gateChart draws.
func gateCase(touched ...string) code.GateFacts {
	return code.GateFacts{
		Rows: code.ReadinessRows{
			Repo: code.RepoRef{ID: "id-platform", Name: "platform", Visibility: code.VisibilityCompany,
				Owner: code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}},
			Change: code.ChangeRow{ID: "change-42", RepoID: "id-platform", Number: 42, State: code.ChangeStateOpen,
				SourceRef: "refs/heads/seats/dev/retry", TargetRef: mainRef, Head: oidOf("B2"), Revision: 2,
				Owner: "dev", OwnerKind: code.PartyAgent},
			Authors: []code.Party{gateDev},
			Facts: []code.RevisionPatch{
				{N: 1, TargetRef: mainRef, BaseTree: oidOf("base"), HeadTree: oidOf("tree 1"), PatchID: oidOf("diff 1")},
				{N: 2, TargetRef: mainRef, BaseTree: oidOf("base"), HeadTree: oidOf("tree 2"), PatchID: oidOf("diff 2")},
			},
			Merge: &code.RecordedMerge{Revision: 2, TargetRef: mainRef, TargetOID: oidOf("A"), State: code.MergeClean},
		},
		Head:   code.HeadFacts{Recorded: true, Touched: touched, OwnersDigest: oidOf("owners")},
		Roster: gateChart(),
	}
}

// approving is f with reviewer's standing verdict approving revision.
func approving(f code.GateFacts, reviewer code.Party, revision int64) code.GateFacts {
	f.Rows.Approvals = append(slices.Clone(f.Rows.Approvals), code.Approval{Reviewer: reviewer, Revision: revision})
	return f
}

// gated is main's one rule carrying gate, under no floor.
func gated(t *testing.T, gate code.MergeGate) code.GateRules {
	t.Helper()
	r, err := code.GateRulesOf([]code.Protection{{PushRule: code.PushRule{Pattern: mainRef,
		DirectPush: code.DirectPersons}, Gate: &gate}}, mainRef, "")
	if err != nil {
		t.Fatalf("GateRulesOf: %v", err)
	}
	return r
}

// bareGate is a gate holding nothing but its defaults — writers, no
// approval, a queue at 4/4, seats — which a case sets a term of.
func bareGate() code.MergeGate {
	return code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsNone,
		Landing: code.Landing{Depth: 4, Batch: 4}, DeleteSource: code.DeleteSeats}
}

// judgedAs fails t unless r's precondition of kind is in state, its sentence
// holding says.
func judgedAs(t *testing.T, r code.Readiness, kind code.PreconditionKind, state code.PreconditionState,
	says string) code.Precondition {

	t.Helper()
	i := slices.IndexFunc(r.Preconditions, func(p code.Precondition) bool { return p.Kind == kind })
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
	f.Head.RequestedOwners = []code.Owner{{Kind: code.OwnerUnit, Ref: "Data"}, {Kind: code.OwnerUnit, Ref: "Web"}}

	both := approving(approving(f, gateWebLead, 2), gateDataLd, 2)
	p := judgedAs(t, code.JudgeGate(both, rules), code.PreconditionOwnersApproval, code.PreconditionUnmet,
		"no one approval counts for all of them")
	if !slices.Equal(p.WaitsOn, []code.Party{gateCTO}) {
		t.Errorf("the unmet owners' approval waits on %+v, want the one common lead, the cto", p.WaitsOn)
	}
	judgedAs(t, code.JudgeGate(approving(both, gateCTO, 2), rules), code.PreconditionOwnersApproval,
		code.PreconditionMet, `seat "cto"'s approval counts for every path owner`)
	judgedAs(t, code.JudgeGate(approving(f, gateFounder, 2), rules), code.PreconditionOwnersApproval,
		code.PreconditionMet, `person "founder"'s approval counts`)
	// A SEAT OWNER's chain is the seat and its unit's leads: reader's own
	// path and Web's share Web's lead and the cto.
	f.Head.RequestedOwners = []code.Owner{{Kind: code.OwnerSeat, Ref: "reader"}, {Kind: code.OwnerUnit, Ref: "Web"}}
	judgedAs(t, code.JudgeGate(approving(f, gateWebLead, 2), rules), code.PreconditionOwnersApproval,
		code.PreconditionMet, `seat "web-lead"'s approval counts`)
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
	f.Head.RequestedOwners = []code.Owner{{Kind: code.OwnerUnit, Ref: "Web"}}
	f.OwnerRowsDigest = oidOf("owners since")
	r := code.JudgeGate(f, rules)
	judgedAs(t, r, code.PreconditionOwnersApproval, code.PreconditionComputing,
		"path owners changed since revision 2's facts matched")
	if r.Ready() {
		t.Error("a change whose owners' approval is computing reads ready")
	}
	f.OwnerRowsDigest = f.Head.OwnersDigest
	judgedAs(t, code.JudgeGate(f, rules), code.PreconditionOwnersApproval, code.PreconditionMet,
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
	gate.Approvals, gate.OwnersApprove = code.ApprovalsOne, true
	gate.HumanPaths = []code.HumanPath{{Glob: "deploy/**"}}
	rules := gated(t, gate)
	f := approving(gateCase("deploy/app.yaml"), gateFounder, 1)
	f.Head.RequestedOwners = []code.Owner{{Kind: code.OwnerUnit, Ref: "Web"}}
	f.OwnerRowsDigest = f.Head.OwnersDigest
	stale := code.JudgeGate(f, rules)
	judgedAs(t, stale, code.PreconditionApproval, code.PreconditionUnmet, "no approval counts on revision 2")
	judgedAs(t, stale, code.PreconditionHumanApproval, code.PreconditionUnmet, "no person who may approve it")
	judgedAs(t, stale, code.PreconditionOwnersApproval, code.PreconditionUnmet, "no one approval counts")

	f.Rows.Facts[1].PatchID = f.Rows.Facts[0].PatchID
	carried := code.JudgeGate(f, rules)
	judgedAs(t, carried, code.PreconditionApproval, code.PreconditionMet, "carries the diff")
	judgedAs(t, carried, code.PreconditionHumanApproval, code.PreconditionMet, `person "founder"'s approval counts`)
	judgedAs(t, carried, code.PreconditionOwnersApproval, code.PreconditionMet, `person "founder"'s approval`)
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
	gate.HumanPaths = []code.HumanPath{
		{Glob: "deploy/**"},
		{Glob: "docs/**", Unless: []code.ChangeClass{code.ChangeClassCommentOnly}},
	}
	rules := gated(t, gate)

	f := gateCase("deploy/app.yaml", "docs/guide.go", "docs/notes.go")
	f.Head.Classes = code.RevisionClasses{WhitespaceOnly: []int{2}, CommentOnly: []int{1}, Complete: true}
	p := judgedAs(t, code.JudgeGate(f, rules), code.PreconditionHumanApproval, code.PreconditionUnmet,
		"touches deploy/**")
	if strings.Contains(p.Detail, "docs/**") {
		t.Errorf("a docs entry its unless waived still holds the change: %q", p.Detail)
	}

	f = gateCase("docs/guide.go", "docs/notes.go")
	f.Head.Classes = code.RevisionClasses{WhitespaceOnly: []int{1}, CommentOnly: []int{0}, Complete: true}
	judgedAs(t, code.JudgeGate(f, rules), code.PreconditionHumanApproval, code.PreconditionMet,
		"touches docs/** only with changes of a class its unless waives")

	f = gateCase("docs/guide.go", "docs/server.go")
	f.Head.Classes = code.RevisionClasses{CommentOnly: []int{0}, Complete: true}
	judgedAs(t, code.JudgeGate(f, rules), code.PreconditionHumanApproval, code.PreconditionUnmet,
		"touches docs/**")
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
	r := code.JudgeGate(f, rules)
	if p := judgedAs(t, r, code.PreconditionCheck, code.PreconditionComputing, "ci (no attempt yet)"); len(p.WaitsOn) > 0 {
		t.Errorf("a check nobody reported waits on %+v, want nobody", p.WaitsOn)
	}
	if r.Ready() {
		t.Fatal("a change whose required check has no attempt reads ready")
	}
	reported := func(state code.CheckState) code.GateFacts {
		f.Checks = []code.GateCheck{{Name: "ci", Attempt: 1, State: state, Reason: code.CheckReasonReported,
			Placement: code.PlacementExternal}}
		return f
	}
	judgedAs(t, code.JudgeGate(reported(code.CheckStateRunning), rules), code.PreconditionCheck,
		code.PreconditionComputing, "ci (running, attempt 1)")
	judgedAs(t, code.JudgeGate(reported(code.CheckStatePassed), rules), code.PreconditionCheck,
		code.PreconditionMet, "ci (passed)")
	p := judgedAs(t, code.JudgeGate(reported(code.CheckStateFailed), rules), code.PreconditionCheck,
		code.PreconditionUnmet, "ci (failed, attempt 1) did not pass")
	if !slices.Equal(p.WaitsOn, []code.Party{gateDev}) {
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
	gate.MergeBy, gate.Approvals, gate.OwnersApprove = code.MergeByMaintainers, code.ApprovalsOne, true
	gate.HumanPaths = []code.HumanPath{{Glob: "deploy/**"}}
	gate.Checks = []string{"lint", "unit"}
	rules := gated(t, gate)
	f := gateCase("deploy/app.yaml", "data/schema.sql")
	f.OwnerRowsDigest = f.Head.OwnersDigest
	f.Head.RequestedOwners = []code.Owner{{Kind: code.OwnerUnit, Ref: "Data"}, {Kind: code.OwnerUnit, Ref: "Web"}}
	f.Checks = []code.GateCheck{
		{Name: "lint", Attempt: 1, State: code.CheckStateFailed, Reason: code.CheckReasonExit},
		{Name: "unit", Attempt: 2, State: code.CheckStatePassed, Reason: code.CheckReasonExit},
	}
	f.Merger = code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	r := code.JudgeGate(f, rules)
	var unmet []code.PreconditionKind
	for _, p := range r.Preconditions {
		if p.State != code.PreconditionUnmet {
			continue
		}
		unmet = append(unmet, p.Kind)
		if len(p.WaitsOn) == 0 || p.Detail == "" {
			t.Errorf("unmet %s names nobody it waits on (%+v) or no sentence (%q)", p.Kind, p.WaitsOn, p.Detail)
		}
	}
	want := []code.PreconditionKind{code.PreconditionApproval, code.PreconditionHumanApproval,
		code.PreconditionOwnersApproval, code.PreconditionCheck, code.PreconditionMergeAuthority}
	if !slices.Equal(unmet, want) {
		t.Fatalf("the unmet terms are %v, want %v", unmet, want)
	}
	if first := r.Preconditions[slices.IndexFunc(r.Preconditions, func(p code.Precondition) bool {
		return p.Kind == code.PreconditionApproval
	})].Detail; r.First != first {
		t.Errorf("the first sentence is %q, want the approval's, %q", r.First, first)
	}
	p := judgedAs(t, r, code.PreconditionMergeAuthority, code.PreconditionUnmet, "takes maintain")
	if !slices.Equal(p.WaitsOn, []code.Party{gateCTO}) {
		t.Errorf("a merge only a maintainer makes waits on %+v, want the seat that maintains the repository",
			p.WaitsOn)
	}

	// AN UNGATED BRANCH asks no approval, no person, no owner and no check.
	open, err := code.GateRulesOf(nil, mainRef, code.MergeByPersons)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range code.JudgeGate(f, open).Preconditions {
		switch p.Kind {
		case code.PreconditionApproval, code.PreconditionHumanApproval, code.PreconditionOwnersApproval,
			code.PreconditionCheck:
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
	dev := code.Principal{Kind: code.PrincipalAgent, Seat: "dev"}
	cto := code.Principal{Kind: code.PrincipalAgent, Seat: "cto"}
	founder := code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:founder", Seat: "founder"}
	writers, maintainers := bareGate(), bareGate()
	maintainers.MergeBy = code.MergeByMaintainers
	rule := func(pattern string, g code.MergeGate) code.Protection {
		return code.Protection{PushRule: code.PushRule{Pattern: pattern, DirectPush: code.DirectPersons}, Gate: &g}
	}
	for _, tc := range []struct {
		name   string
		rules  []code.Protection
		floor  code.MergeBy
		merger code.Principal
		want   code.PreconditionState
	}{
		{"a writer under writers", []code.Protection{rule(mainRef, writers)}, "", dev, code.PreconditionMet},
		{"a writer under a second rule's maintainers", []code.Protection{rule(mainRef, writers),
			rule("refs/heads/*", maintainers)}, "", dev, code.PreconditionUnmet},
		{"a writer under the floor's maintainers", []code.Protection{rule(mainRef, writers)},
			code.MergeByMaintainers, dev, code.PreconditionUnmet},
		{"the maintainer under the floor's maintainers", []code.Protection{rule(mainRef, writers)},
			code.MergeByMaintainers, cto, code.PreconditionMet},
		{"a seat under the floor's persons", []code.Protection{rule(mainRef, writers)}, code.MergeByPersons, cto,
			code.PreconditionUnmet},
		{"a person under the floor's persons", []code.Protection{rule(mainRef, writers)}, code.MergeByPersons,
			founder, code.PreconditionMet},
		{"a writer on a branch no rule protects", nil, code.MergeByPersons, dev, code.PreconditionMet},
	} {
		r, err := code.GateRulesOf(tc.rules, mainRef, tc.floor)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		f := gateCase("app.go")
		f.Merger = tc.merger
		p := judgedAs(t, code.JudgeGate(f, r), code.PreconditionMergeAuthority, tc.want, "")
		if tc.want == code.PreconditionUnmet && strings.TrimSpace(p.Detail) == "" {
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
	keeping.DeleteSource = code.DeleteNone
	unknown := bareGate()
	unknown.DeleteSource = "archive"
	rule := func(pattern string, g code.MergeGate) code.Protection {
		return code.Protection{PushRule: code.PushRule{Pattern: pattern, DirectPush: code.DirectPersons}, Gate: &g}
	}
	for _, tc := range []struct {
		name  string
		rules []code.Protection
		want  bool
	}{
		{"seats deletes", []code.Protection{rule(mainRef, bareGate())}, false},
		{"none keeps", []code.Protection{rule(mainRef, keeping)}, true},
		{"none beside seats keeps", []code.Protection{rule(mainRef, bareGate()), rule("refs/heads/*", keeping)},
			true},
		{"an unknown value keeps", []code.Protection{rule(mainRef, unknown)}, true},
		{"an ungated target keeps nothing", []code.Protection{rule("refs/heads/release", keeping)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := code.GateRulesOf(tc.rules, mainRef, "")
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
// change touching code.ChecksDirGlob — the directory its checks file lives in
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
	if _, ok := strings.CutPrefix(code.ChecksFilePath, strings.TrimSuffix(code.ChecksDirGlob, "**")); !ok {
		t.Fatalf("the checks file %s lies outside %s, the human path that holds it", code.ChecksFilePath,
			code.ChecksDirGlob)
	}
	checked := bareGate()
	checked.Checks = []string{"unit"}
	rules := gated(t, checked)
	if !slices.ContainsFunc(rules.HumanPaths, func(h code.HumanPath) bool {
		return h.Glob == code.ChecksDirGlob && len(h.Unless) == 0
	}) {
		t.Fatalf("a gate requiring checks names the human paths %+v, want %s among them", rules.HumanPaths,
			code.ChecksDirGlob)
	}

	f := gateCase(code.ChecksFilePath, "app.go")
	f.Checks = []code.GateCheck{{Name: "unit", Attempt: 1, State: code.CheckStatePassed, Reason: code.CheckReasonExit}}
	bySeat := code.JudgeGate(approving(f, gateWebLead, 2), rules)
	p := judgedAs(t, bySeat, code.PreconditionHumanApproval, code.PreconditionUnmet, code.ChecksDirGlob)
	if !slices.Equal(p.WaitsOn, []code.Party{gateDev}) {
		t.Errorf("a checks file change no person is asked of waits on %+v, want its owner to ask one", p.WaitsOn)
	}
	if bySeat.Ready() {
		t.Fatal("a checks file change a seat alone approved reads ready to merge")
	}
	judgedAs(t, code.JudgeGate(approving(f, gateFounder, 2), rules), code.PreconditionHumanApproval,
		code.PreconditionMet, `person "founder"'s approval counts`)
	judgedAs(t, code.JudgeGate(gateCase("app.go"), rules), code.PreconditionHumanApproval,
		code.PreconditionMet, "touches none of")

	// AGENTS_EDIT_CHECKS OPTS THE RULE OUT, and a rule requiring no check
	// holds nothing: a seat's approval merges the same change.
	opted := checked
	opted.AgentsEditChecks = true
	unchecked := bareGate()
	for name, gate := range map[string]code.MergeGate{"agents_edit_checks": opted, "no checks": unchecked} {
		r := code.JudgeGate(approving(f, gateWebLead, 2), gated(t, gate))
		if slices.ContainsFunc(r.Preconditions, func(p code.Precondition) bool {
			return p.Kind == code.PreconditionHumanApproval
		}) {
			t.Errorf("%s: a checks file change waits for a person: %+v", name, r.Preconditions)
		}
		if !r.Ready() {
			t.Errorf("%s: a seat-approved checks file change is not ready: %q", name, r.First)
		}
	}

	// THE OPT-OUT IS THE RULE'S: a second rule matching main that requires
	// checks without it holds the change, the strictest of the two.
	rule := func(pattern string, g code.MergeGate) code.Protection {
		return code.Protection{PushRule: code.PushRule{Pattern: pattern, DirectPush: code.DirectPersons}, Gate: &g}
	}
	both, err := code.GateRulesOf([]code.Protection{rule(mainRef, opted), rule("refs/heads/*", checked)}, mainRef, "")
	if err != nil {
		t.Fatalf("GateRulesOf: %v", err)
	}
	judgedAs(t, code.JudgeGate(approving(f, gateWebLead, 2), both), code.PreconditionHumanApproval,
		code.PreconditionUnmet, code.ChecksDirGlob)
}

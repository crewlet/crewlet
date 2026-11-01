package code_test

import (
	"database/sql"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S MERGE READINESS, judged by the reads from the rows the shipped
// committer's and change writer's records wrote on a real embedded broker —
// and stored on the change's mergeability by the records its home asks for.

// patched is the home's write of revision rev's facts on change — its head
// head against main — naming patch as its diff's patch id.
func (n *node) patched(repo, change string, rev int64, head, patch string) code.ConversationWrite {
	n.t.Helper()
	w := n.revising(repo, change, rev, oidOf(head), mainRef, nil)
	w.Revise.PatchID = patch
	return w
}

// readinessOf is web's change number's readiness as ada reads it, failing t
// where the read carries none.
func readinessOf(t *testing.T, n *node, number int64) code.Readiness {
	t.Helper()
	answer, err := readChange(t, n, ada, "", "web", number, 0, "")
	if err != nil || answer.Change == nil || answer.Change.Readiness == nil {
		t.Fatalf("the read of web!%d (%+v, %v) carries no readiness", number, answer.Change, err)
	}
	return *answer.Change.Readiness
}

// preconditionOf is r's precondition of kind, failing t where it has none.
func preconditionOf(t *testing.T, r code.Readiness, kind code.PreconditionKind) code.Precondition {
	t.Helper()
	for _, p := range r.Preconditions {
		if p.Kind == kind {
			return p
		}
	}
	t.Fatalf("the readiness %+v judges no %s", r, kind)
	return code.Precondition{}
}

// requireState fails t unless r's precondition of kind is in state, its
// sentence holding says.
func requireState(t *testing.T, r code.Readiness, kind code.PreconditionKind, state code.PreconditionState,
	says string) code.Precondition {

	t.Helper()
	p := preconditionOf(t, r, kind)
	if p.State != state || !strings.Contains(p.Detail, says) {
		t.Fatalf("%s is %s (%q), want %s saying %q", kind, p.State, p.Detail, state, says)
	}
	return p
}

// converse fails t unless w's write applied, and answers its result.
func converse(t *testing.T, w *code.ChangeWriter, cw code.ConversationWrite) code.ConversationResult {
	t.Helper()
	res, err := w.Converse(t.Context(), cw)
	requireConversed(t, res, err)
	return res
}

// AN APPROVAL CARRIES ONLY TO AN EQUAL PATCH ID (X02): an eligible reviewer's
// approval of revision 1 counts on revision 1; a rebase that pushes revision 2
// — another head, the same diff, its recorded patch id equal to revision 1's —
// carries it, the detail saying so; and revision 3, whose diff is another
// (its patch id differs), carries nothing, so the approval precondition is
// unmet again, waiting on the change's owner to ask a reviewer, since nobody
// who may approve it is asked.
//
// Mutation: carry an approval by comparing the revisions' heads rather than
// their patch ids → revision 2, a rebase onto another head, carries nothing,
// and the case goes red at its second readiness.
func TestAnApprovalCarriesOnlyToAnEqualPatchID(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionMet,
		`revision 1 is approved by seat "plat-lead"`)

	n.revise(repo, "B", "B2")
	converse(t, w, n.patched(repo, change, 2, "B2", oidOf("the retry diff")))
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionMet,
		`revision 2 carries the diff seat "plat-lead" approved unchanged`)

	n.revise(repo, "B2", "B3")
	converse(t, w, n.patched(repo, change, 3, "B3", oidOf("another diff")))
	p := requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionUnmet,
		"no approval counts on revision 3")
	if want := []code.Party{{Handle: "dev", Kind: code.PartyAgent}}; !slices.Equal(p.WaitsOn, want) {
		t.Fatalf("the unmet approval waits on %+v, want its owner %+v, since nobody who may approve is asked",
			p.WaitsOn, want)
	}
}

// omitting is the home's write of revision rev's facts on change — its head
// head against main — holding the code named held, its diff past the bound
// on one a patch id is taken of, so its id is omitted.
func (n *node) omitting(repo, change string, rev int64, head, held string) code.ConversationWrite {
	n.t.Helper()
	w := codeOf(n.revising(repo, change, rev, oidOf(head), mainRef, nil), held)
	w.Revise.PatchID, w.Revise.PatchIDOmitted = "", true
	return w
}

// A NO-CODE-CHANGE CARRIES AN APPROVAL PAST AN OMITTED PATCH ID. A change
// whose diff passes the bound on one a patch id is taken of — a vendored
// tree — has its id omitted on every revision, and an omitted id equals
// nothing, so no approval carries by its diff. But a revision whose base and
// head trees are the approved revision's — a message amended, commits
// squashed — holds the very code that was approved, byte for byte, and the
// approval carries to it by its code, the detail naming the kind that
// carried (no_code_change). A revision whose code moved carries nothing.
//
// Mutation: drop the code's arm of the carry — revision 2's approval is
// unmet, red.
func TestANoCodeChangeCarriesAnApprovalPastAnOmittedPatchID(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	converse(t, w, n.omitting(repo, change, 1, "B", "the vendored tree"))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	n.revise(repo, "B", "B2")
	converse(t, w, n.omitting(repo, change, 2, "B2", "the vendored tree"))
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionMet,
		`revision 2 holds the code seat "plat-lead" approved unchanged (no_code_change`)

	n.revise(repo, "B2", "B3")
	converse(t, w, n.omitting(repo, change, 3, "B3", "the vendored tree, one file of it changed"))
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionUnmet,
		"no approval counts on revision 3")
}

// NO CODE CHANGE IS REVISION TO REVISION: what carries an approval by its code
// is the head revision holding the APPROVED revision's base and head trees —
// never a diff that is empty against its own base, which changes nothing and
// holds other code than the approver read — and it holds across a revision
// of other code between the two: a head whose code returns to the approved
// code byte for byte holds what the approver read, as an equal patch id
// carries across one.
//
// Mutation: carry by the head's own diff being empty (its base tree its head
// tree) rather than by the approved revision's trees — revision 2 carries,
// red.
func TestNoCodeChangeIsRevisionToRevision(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	converse(t, w, codeOf(n.patched(repo, change, 1, "B", oidOf("the retry diff")), "the retry"))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))

	n.revise(repo, "B", "B2")
	empty := n.revising(repo, change, 2, oidOf("B2"), mainRef, nil)
	empty.Revise.BaseTree, empty.Revise.HeadTree, empty.Revise.PatchID = oidOf("main"), oidOf("main"), ""
	converse(t, w, empty)
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionUnmet,
		"no approval counts on revision 2")

	n.revise(repo, "B2", "B3")
	back := codeOf(n.revising(repo, change, 3, oidOf("B3"), mainRef, nil), "the retry")
	back.Revise.PatchID, back.Revise.PatchIDOmitted = "", true
	converse(t, w, back)
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionMet,
		`revision 3 holds the code seat "plat-lead" approved unchanged (no_code_change: the base and head `+
			`trees of revision 1`)
}

// AN APPROVAL WAITS FOR FACTS: an approval of an earlier revision is judged by
// both revisions' recorded patch ids, so while the head revision's facts
// against its target are unrecorded the approval precondition is COMPUTING —
// the engine owes them, and it waits on nobody — and it is met the moment
// they are recorded equal. But an approval of a revision whose own facts were
// never recorded NEVER carries: its home measured only the head, and a head
// that has moved on is measured no more, so that approval is unmet rather
// than computing for ever.
//
// Mutations: judge a head with no facts unmet → the first readiness reads
// unmet; judge an approved revision with no facts computing → the second
// change's approval reads computing.
func TestAnApprovalWaitsForFacts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	n.revise(repo, "B", "B2")
	r := readinessOf(t, n, 1)
	if p := requireState(t, r, code.PreconditionApproval, code.PreconditionComputing,
		"waits for revision 2's facts"); len(p.WaitsOn) != 0 {
		t.Fatalf("a computing approval waits on %+v, want nobody but the engine", p.WaitsOn)
	}
	requireState(t, r, code.PreconditionFacts, code.PreconditionComputing, "revision 2's facts")
	if r.Ready() || r.WaitsPerson() {
		t.Fatalf("a readiness computing its approval answers ready %t, waiting on a person %t", r.Ready(),
			r.WaitsPerson())
	}
	converse(t, w, n.patched(repo, change, 2, "B2", oidOf("the retry diff")))
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionMet,
		"carries the diff")

	// AN APPROVAL OF A REVISION NEVER MEASURED: web!2's first revision is
	// approved before its home read its facts, and a push moves on.
	c, _ := committerOn(t, n, n.pub)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/second", code.ZeroOID, oidOf("S"))))
	second, err := c.ProposeChange(t.Context(), opening(repo, "refs/heads/second", mainRef, dev))
	requireChanged(t, second, err)
	converse(t, w, reviewing(repo, second.ChangeID, platLead, 1, code.VerdictApprove, nil))
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/second", oidOf("S"), oidOf("S2"))))
	revised := n.revising(repo, second.ChangeID, 2, oidOf("S2"), mainRef, nil)
	converse(t, w, revised)
	requireState(t, readinessOf(t, n, 2), code.PreconditionApproval, code.PreconditionUnmet,
		"no approval counts on revision 2")
}

// AN AUTHOR'S APPROVAL NEVER COUNTS: a reviewer who approved a revision and
// then pushed the change's branch is an author of it, whose review of their
// own change counts for nothing — so their approval stops counting, however
// equal the next revision's patch id, and the unmet approval waits on nobody
// the chart names but the owner, since the only reviewer asked is now that
// author.
//
// Mutation: drop the author term from the approval's count → cto's approval
// carries to revision 2, and the case goes red.
func TestAnAuthorsApprovalNeverCounts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	c, _ := committerOn(t, n, n.pub)

	converse(t, w, asking(repo, change, dev, "cto"))
	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, reviewing(repo, change, cto, 1, code.VerdictApprove, nil))
	requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionMet, `seat "cto"`)

	pushedBy(t, c, repo, cto, update(featureRef, oidOf("B"), oidOf("B2")))
	converse(t, w, n.patched(repo, change, 2, "B2", oidOf("the retry diff")))
	p := requireState(t, readinessOf(t, n, 1), code.PreconditionApproval, code.PreconditionUnmet,
		"nobody who may approve it is asked")
	if want := []code.Party{{Handle: "dev", Kind: code.PartyAgent}}; !slices.Equal(p.WaitsOn, want) {
		t.Fatalf("the approval of a change its only asked reviewer now wrote waits on %+v, want its owner %+v",
			p.WaitsOn, want)
	}
}

// A LATER REQUEST FOR CHANGES WITHDRAWS ONLY THAT APPROVAL: a reviewer's
// standing verdict is their latest review that judges, so plat-lead asking for
// changes after approving withdraws plat-lead's approval and nothing else —
// cto's still counts, and no precondition holds the change back for the
// request itself (P8 decisions[13]: a merge counts approvals and standing
// blockers). With cto's withdrawn too, the approval is unmet, and still
// nothing else is.
//
// Mutation: count every approving review of the revision rather than each
// reviewer's standing verdict → plat-lead's withdrawn approval still counts,
// and the last readiness reads met.
func TestALaterRequestForChangesWithdrawsOnlyThatApproval(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	converse(t, w, reviewing(repo, change, cto, 1, code.VerdictApprove, nil))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil))
	r := readinessOf(t, n, 1)
	if p := requireState(t, r, code.PreconditionApproval, code.PreconditionMet, `seat "cto"`); strings.Contains(
		p.Detail, "plat-lead") {
		t.Fatalf("the approval still counts plat-lead's withdrawn one: %q", p.Detail)
	}
	for _, kind := range []code.PreconditionKind{code.PreconditionDraft, code.PreconditionFacts,
		code.PreconditionBlocker} {
		if p := preconditionOf(t, r, kind); p.State != code.PreconditionMet {
			t.Fatalf("a request for changes left %s %s (%q), want met: it holds nothing but its own approval",
				kind, p.State, p.Detail)
		}
	}
	converse(t, w, reviewing(repo, change, cto, 1, code.VerdictRequestChanges, nil))
	r = readinessOf(t, n, 1)
	requireState(t, r, code.PreconditionApproval, code.PreconditionUnmet, "no approval counts on revision 1")
	if p := preconditionOf(t, r, code.PreconditionBlocker); p.State != code.PreconditionMet {
		t.Fatalf("two requests for changes left the blocker precondition %s, want met", p.State)
	}
}

// THE READINESS NAMES WHO CAN CLEAR EACH BLOCKER (S40): every standing blocker
// is named, the oldest raised first, with its raiser, the seats that maintain
// the repository on the chart — Engineering's effective lead, cto — and any
// person; an open one waits on the change's owner, and one an author answered
// (disputed) on its raiser, who judges the answer. A blocker cleared by its
// dismissal leaves the list; and a repository whose owning seat's tier lets it
// review and no more names no maintainer, only persons beside the raiser.
//
// Mutations: name the owner unit's effective lead whatever its tier → the
// Platform-owned repository names plat-lead; let a disputed blocker wait on
// the owner → the WaitsOn misses ada.
func TestReadinessNamesWhoCanClearEachBlocker(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	backoff := converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 10, "No backoff"))).FindingIDs
	byAda := converse(t, w, reviewing(repo, change, ada, 1, code.VerdictComment, nil,
		raising(code.SeverityBlocker, 20, "Leaks the token"), raising(code.SeverityNit, 21, "Spelling"))).FindingIDs
	if len(backoff) != 1 || len(byAda) != 2 {
		t.Fatalf("the reviews raised %q and %q, want one finding and two", backoff, byAda)
	}
	converse(t, w, answering(repo, change, dev, code.FindingDisputed, byAda[0]))

	p := requireState(t, readinessOf(t, n, 1), code.PreconditionBlocker, code.PreconditionUnmet,
		"2 blockers stand")
	want := []code.Blocker{
		{FindingID: backoff[0], State: code.FindingOpen, ClearedBy: code.ClearedBy{
			Raiser: code.Party{Handle: "plat-lead", Kind: code.PartyAgent}, Maintainers: []string{"cto"},
			Persons: true}},
		{FindingID: byAda[0], State: code.FindingDisputed, ClearedBy: code.ClearedBy{
			Raiser: code.Party{Handle: "founder", Kind: code.PartyPerson}, Maintainers: []string{"cto"},
			Persons: true}},
	}
	if len(p.Blockers) != len(want) || p.BlockersOmitted != 0 {
		t.Fatalf("the blocker precondition names %+v (%d more), want %+v", p.Blockers, p.BlockersOmitted, want)
	}
	for i := range want {
		got := p.Blockers[i]
		if got.FindingID != want[i].FindingID || got.State != want[i].State ||
			got.ClearedBy.Raiser != want[i].ClearedBy.Raiser ||
			!slices.Equal(got.ClearedBy.Maintainers, want[i].ClearedBy.Maintainers) || !got.ClearedBy.Persons {
			t.Fatalf("blocker %d is %+v, want %+v", i, got, want[i])
		}
	}
	if wantWaits := []code.Party{{Handle: "dev", Kind: code.PartyAgent},
		{Handle: "founder", Kind: code.PartyPerson}}; !slices.Equal(p.WaitsOn, wantWaits) {
		t.Fatalf("the blockers wait on %+v, want the owner for the open one and the raiser of the disputed "+
			"one %+v", p.WaitsOn, wantWaits)
	}

	converse(t, w, answering(repo, change, cto, code.FindingDismissed, backoff[0]))
	if p := requireState(t, readinessOf(t, n, 1), code.PreconditionBlocker, code.PreconditionUnmet,
		"1 blocker stands"); len(p.Blockers) != 1 || p.Blockers[0].FindingID != byAda[0] {
		t.Fatalf("after a maintainer's dismissal the blockers are %+v, want ada's alone", p.Blockers)
	}

	// WHO MAINTAINS IS THE CHART'S: a repository Platform owns is led by
	// plat-lead, whose tier reviews and no more.
	rows := code.ReadinessRows{Repo: code.RepoRef{ID: "id-platform", Name: "platform",
		Owner: code.Owner{Kind: code.OwnerUnit, Ref: "Platform"}, Visibility: code.VisibilityCompany},
		Change: code.ChangeRow{Number: 1, State: code.ChangeStateOpen, Revision: 1, Head: oidOf("B"),
			TargetRef: mainRef, Owner: "dev", OwnerKind: code.PartyAgent},
		Blockers: []code.StandingBlocker{{FindingID: byAda[0], Raiser: code.Party{Handle: "plat-lead",
			Kind: code.PartyAgent}, State: code.FindingOpen}}, Standing: 1}
	led := preconditionOf(t, code.ReadinessOf(rows, testChart()), code.PreconditionBlocker)
	if len(led.Blockers) != 1 || len(led.Blockers[0].ClearedBy.Maintainers) != 0 ||
		!strings.Contains(led.Detail, "no seat maintains the repository") {
		t.Fatalf("a repository led by a seat that only reviews names %+v (%q), want no maintainer", led.Blockers,
			led.Detail)
	}
}

// A READINESS IS READ FROM ONE SNAPSHOT: a listing judges every change it
// serves — the readiness each row carries is the one a read of that change
// answers at the same position — against ONE chart for the whole read,
// asked of the reader once however many changes the page judges; and the
// readiness a read answers agrees with the rows its own answer carries, the
// conversation's standing blockers and the change's mergeability among them,
// since both are read in the read's one transaction.
//
// Mutation: ask the reader's chart again within one read → the listing asks
// it twice.
func TestReadinessIsReadFromOneSnapshot(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	c, _ := committerOn(t, n, n.pub)
	for _, branch := range []string{"second", "third"} {
		ref := "refs/heads/" + branch
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(ref, code.ZeroOID, oidOf(branch))))
		opened, err := c.ProposeChange(t.Context(), opening(repo, ref, mainRef, dev))
		requireChanged(t, opened, err)
	}
	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil,
		raising(code.SeverityBlocker, 10, "No backoff")))
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeConflict, "upload/retry.go"))

	charts := 0
	reader, err := code.NewReader(code.ReaderOptions{Log: n.authority, Roster: func() code.Roster {
		charts++
		return testChart()
	}})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	listing, err := reader.Read(t.Context(), code.ReadQuery{Asker: ada, Kind: code.ReadChanges, Repo: "web",
		Fresh: stale})
	if err != nil || len(listing.Changes) != 3 {
		t.Fatalf("the listing answered %+v, %v; want web's three changes", listing.Changes, err)
	}
	if charts != 1 {
		t.Fatalf("one listing of three changes asked the reader's chart %d times, want once", charts)
	}
	detail, err := reader.Read(t.Context(), code.ReadQuery{Asker: ada, Kind: code.ReadChange, Repo: "web",
		Change: 1, Fresh: statelog.FloorAt(statelog.ReadSession, listing.Position)})
	if err != nil || detail.Change == nil || detail.Change.Readiness == nil {
		t.Fatalf("the change read answered (%+v, %v) with no readiness", detail.Change, err)
	}
	listed := listing.Changes[slices.IndexFunc(listing.Changes, func(s code.ChangeSummary) bool {
		return s.Number == 1
	})]
	if listed.Readiness == nil || listed.Readiness.First != detail.Change.Readiness.First ||
		listed.Readiness.Met != detail.Change.Readiness.Met {
		t.Fatalf("the listing judged web!1 %+v and its read %+v at the same position", listed.Readiness,
			detail.Change.Readiness)
	}
	r := *detail.Change.Readiness
	blockers := preconditionOf(t, r, code.PreconditionBlocker)
	if detail.Change.Conversation == nil || int64(len(blockers.Blockers)) != detail.Change.Conversation.OpenBlockers {
		t.Fatalf("the read names %d blockers and counts %+v standing", len(blockers.Blockers),
			detail.Change.Conversation)
	}
	mergeable := requireState(t, r, code.PreconditionMergeable, code.PreconditionUnmet, "conflicts with")
	if m := detail.Change.Mergeability; m == nil || m.State != code.MergeConflict ||
		!strings.Contains(mergeable.Detail, m.TargetOID[:7]) {
		t.Fatalf("the read's mergeability %+v and its readiness %q disagree", m, mergeable.Detail)
	}
}

// A REVIEW IS FOLLOWED BY A REFRESH: a change's home records its merge with
// the readiness judged beside it — not ready, nobody having approved — and
// the change owes it nothing more; an approval then moves what the readiness
// reads, so the due listing names the change owing a REFRESH, which the home
// records over the merge the row holds, merging nothing: the stored answer is
// ready, judged at the inputs' new versions, while the merge's own instant —
// what a moved target's recheck is paced by — stays the merge's. Asked again
// with nothing moved, a refresh is superseded and appends nothing; and the
// change is due nothing more.
//
// Mutations: let a review leave the conversation's readiness version → the
// approval lists nothing due; drop the refresh term from the due listing →
// the change is never listed; move the row's instant on a refresh → the
// merge's instant moves.
func TestAReviewIsFollowedByARefresh(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	due := func() []code.ChangeDue {
		t.Helper()
		a, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChangesDue, Fresh: stale,
			ChangesDue: &code.ChangesDueQuery{RepoID: repo, RecheckBefore: harnessAt}})
		if err != nil {
			t.Fatalf("Read(changes_due): %v", err)
		}
		return a.DueChanges
	}

	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
	merged := n.mergeability(change)
	if stored := n.storedReadiness(change); stored.ready || stored.waitsPerson {
		t.Fatalf("the merge stored %+v, want not ready: nobody approved", stored)
	}
	if got := due(); len(got) != 0 {
		t.Fatalf("a change whose facts, merge and readiness are recorded is due %+v", got)
	}

	converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil))
	got := due()
	if len(got) != 1 || got[0].ID != change || !got[0].RefreshDue || got[0].FactsDue || got[0].MergeDue {
		t.Fatalf("after an approval the due listing names %+v, want web!1 owing a refresh alone", got)
	}
	repos, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadChangesDue, Fresh: stale,
		ChangesDue: &code.ChangesDueQuery{RecheckBefore: harnessAt}})
	if err != nil || !slices.Equal(repos.DueRepos, []string{repo}) {
		t.Fatalf("the listing of repositories answered %v, %v; want web, owing a refresh", repos.DueRepos, err)
	}

	refresh := code.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""), Mergeability: &code.Mergeability{
		RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"), TargetRef: mainRef, Refresh: true,
		Node: n.id}}
	res := converse(t, w, refresh)
	stored := n.storedReadiness(change)
	if !stored.ready || stored.waitsPerson || stored.changeVersion != 1 || stored.readinessVersion != 2 {
		t.Fatalf("the refresh stored %+v, want ready, judged at the change's version 1 and its conversation's "+
			"readiness version 2", stored)
	}
	if after := n.mergeability(change); after.at != merged.at || after.position != merged.position ||
		after.state != code.MergeClean || after.tip != merged.tip {
		t.Fatalf("the refresh left the merge %+v, want the merge's own %+v", after, merged)
	}
	if record := string(n.recordAt(res.Position).Mutation); !strings.Contains(record, `"refresh":true`) ||
		!strings.Contains(record, `"ready":true`) {
		t.Fatalf("the refresh's record carries %s, want a refresh stamped ready", record)
	}
	if got := due(); len(got) != 0 {
		t.Fatalf("a refreshed change is still due %+v", got)
	}
	before := n.end()
	res, err = w.Converse(t.Context(), code.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
		Mergeability: refresh.Mergeability})
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "already")
}

// heldReadiness is a change's readiness as its mergeability row stores it.
type heldReadiness struct {
	ready, waitsPerson              bool
	changeVersion, readinessVersion int64
}

// storedReadiness is change's stored readiness as n's rows hold it.
func (n *node) storedReadiness(change string) heldReadiness {
	n.t.Helper()
	var s heldReadiness
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT ready, waits_person, change_version, readiness_version
			FROM code_mergeability WHERE change_id = ?`, change).Scan(&s.ready, &s.waitsPerson, &s.changeVersion,
			&s.readinessVersion)
	}); err != nil {
		n.t.Fatalf("read %s's stored readiness: %v", change, err)
	}
	return s
}

// WHAT WAITS ON A PERSON IS LISTED FOR EVERYONE: a change whose first unmet
// precondition is an approval asked of a person alone stores that it waits on
// one when its home records its merge, and needs=person lists it — to a
// person, and to a seat, whose own asks it is not; once the person approves,
// the readiness the listing judges no longer waits on anybody, so the change
// is left out even before its home refreshes what it stored. A change whose
// approval waits on a seat as well as a person waits on no person alone.
//
// Mutation: list the stored answer without judging it again → the approved
// change is still listed.
func TestNeedsPersonListsWhatWaitsOnAPerson(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	converse(t, w, asking(repo, change, dev, "founder"))
	converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
	if stored := n.storedReadiness(change); !stored.waitsPerson || stored.ready {
		t.Fatalf("the merge stored %+v, want waiting on a person: founder alone is asked", stored)
	}
	for _, who := range []code.Principal{ada, dev} {
		if got, _ := listChanges(t, n, code.ReadQuery{Asker: who, Needs: code.NeedPerson}, 0); !slices.Equal(got,
			[]string{"web!1"}) {
			t.Fatalf("needs=person read by %+v lists %q, want web!1", who, got)
		}
	}
	if r := readinessOf(t, n, 1); !r.WaitsPerson() || !strings.Contains(r.First, `person "founder"`) {
		t.Fatalf("the readiness %+v does not wait on founder", r)
	}

	converse(t, w, reviewing(repo, change, ada, 1, code.VerdictApprove, nil))
	if got, _ := listChanges(t, n, code.ReadQuery{Asker: ada, Needs: code.NeedPerson}, 0); len(got) != 0 {
		t.Fatalf("needs=person lists %q after the person approved, want nothing", got)
	}

	// A SEAT ASKED BESIDE THE PERSON: either may approve, so it waits on no
	// person alone.
	rows := code.ReadinessRows{Repo: code.RepoRef{ID: repo, Name: "web",
		Owner: code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}, Visibility: code.VisibilityCompany},
		Change: code.ChangeRow{Number: 1, State: code.ChangeStateOpen, Revision: 1, Head: oidOf("B"),
			TargetRef: mainRef, Owner: "dev", OwnerKind: code.PartyAgent},
		Asked: []code.Party{{Handle: "founder", Kind: code.PartyPerson}, {Handle: "plat-lead",
			Kind: code.PartyAgent}}}
	if code.ReadinessOf(rows, testChart()).WaitsPerson() {
		t.Fatal("an approval a seat may give too waits on a person")
	}
}

// A REFRESH IS NO MERGE'S RECORD: it names the revision, the head and the
// target whose merge it refreshes and nothing of the merge — the writer stamps
// the tip, its position and the state from the change's row — and no write
// names the readiness or the versions the writer stamps; and a refresh of a
// change whose row holds no merge of its head revision is superseded, its
// home merging first.
func TestARefreshNamesTheMergeItRefreshesAndNothingMore(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	refresh := func(set func(*code.Mergeability)) code.ConversationWrite {
		m := &code.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf("B"),
			TargetRef: mainRef, Refresh: true, Node: n.id}
		set(m)
		return code.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""), Mergeability: m}
	}
	for name, tc := range map[string]struct {
		w     code.ConversationWrite
		field string
	}{
		"a refresh naming a tip": {refresh(func(m *code.Mergeability) { m.TargetOID = oidOf("A") }),
			"mergeability.refresh"},
		"a refresh naming a state": {refresh(func(m *code.Mergeability) { m.State = code.MergeClean }),
			"mergeability.refresh"},
		"a refresh naming a conflicted path": {refresh(func(m *code.Mergeability) { m.Conflicts = []string{"a.go"} }),
			"mergeability.conflicts"},
		"a write naming its readiness": {refresh(func(m *code.Mergeability) { m.Ready = true }),
			"mergeability.ready"},
		"a merge naming the versions": {n.mergingWith(repo, change, func(m *code.Mergeability) {
			m.ReadinessVersion = 3
		}), "mergeability.ready"},
	} {
		if _, err := w.Converse(t.Context(), tc.w); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s answered %v, want refused naming %s", name, err, tc.field)
		}
	}
	before := n.end()
	res, err := w.Converse(t.Context(), refresh(func(*code.Mergeability) {}))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded,
		"holds no merge of revision 1")
}

// mergingWith is a clean merge of change's revision 1 into main, tuned by set.
func (n *node) mergingWith(repo, change string, set func(*code.Mergeability)) code.ConversationWrite {
	n.t.Helper()
	w := n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean)
	set(w.Mergeability)
	return w
}

// THE GATE READS NO DECISION: a merge counts the change's own review objects
// — its reviews' verdicts, its findings and its checks — and never a decision
// the tracker records (ADR-0023's asks), so no file of the code host imports
// the tracker or spells one of its tables: neither M4's readiness nor the
// merge gate that widens it (gatejudge.go), nor any decide that will judge a
// merge by it. A decision a seat asked for on a task is an answer about the
// task, and a change gated by one would merge on a word nobody gave about its
// code.
//
// Mutation: import internal/tracker in gatejudge.go, or read
// `tracker_comments` there → red.
func TestTheGateReadsNoDecision(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	tracker := regexp.MustCompile(`\btracker_[a-z_]+\b`)
	walked := 0
	err := sourcetree.Walk(filepath.Join(root, "internal", "code"), func(path string, d fs.DirEntry,
		err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if ok, matchErr := build.Default.MatchFile(filepath.Dir(path), filepath.Base(path)); matchErr != nil ||
			!ok {
			return matchErr
		}
		walked++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, spec := range f.Imports {
			if p, _ := strconv.Unquote(spec.Path.Value); p == "github.com/crewlet/crewlet/internal/tracker" ||
				strings.HasPrefix(p, "github.com/crewlet/crewlet/internal/tracker/") {
				t.Errorf("%s imports %s: the code host counts its own review objects, never a tracker "+
					"decision", rel, p)
			}
		}
		for _, lit := range stringLiterals(f) {
			if table := tracker.FindString(lit); table != "" {
				t.Errorf("%s spells the tracker's table %s: a merge counts no tracker decision", rel, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/code: %v", err)
	}
	if walked == 0 {
		t.Fatal("the walk found no file of internal/code, so this guard measures its own walk")
	}
}

// stringLiterals is every string literal f spells, unquoted: the statements a
// file reads, among them.
func stringLiterals(f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(node ast.Node) bool {
		if lit, ok := node.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

// AN ENDED CHANGE WAITS ON NOBODY: a change whose stored readiness waits on a
// person — founder alone is asked — leaves code_mergeability_waits_idx in the
// same apply as the record that ends it, whichever that is: a close, its home's
// landing, a push that sets its target to its head, a push that deletes its
// source; and a mergeability decided while it was live that lands after its
// close stores no answer either. So the index holds the changes waiting now,
// and needs=person reads one page and finds nothing, rather than walking every
// change that ever ended while waiting, each read and dropped.
//
// Mutations: leave the close's clear out of applyChangeWrite → the closed and
// landed cases keep their row; leave it out of the push book → the push cases
// keep theirs; store the late record's answer as stamped → the late case keeps
// its row.
func TestAnEndedChangeWaitsOnNobody(t *testing.T) {
	t.Parallel()
	closeIt := func(t *testing.T, n *node, repo, change string) {
		t.Helper()
		c, _ := committerOn(t, n, n.pub)
		res, err := c.ProposeChange(t.Context(), closing(repo, change, dev,
			int64(n.count(`SELECT version FROM code_changes WHERE id = ?`, change))))
		requireChanged(t, res, err)
	}
	for name, end := range map[string]func(t *testing.T, n *node, repo, change string){
		"closed": closeIt,
		"landed by its home": func(t *testing.T, n *node, repo, change string) {
			t.Helper()
			n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("M"))))
			chartless, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
				o.SeatRules = func() (code.SeatRules, bool) { return code.SeatRules{}, false }
			})
			res, err := chartless.ProposeChange(t.Context(), landing(repo, change, 1, oidOf("B"), mainRef,
				oidOf("M")))
			if err != nil || res.Outcome != statelog.OutcomeApplied {
				t.Fatalf("the landing answered %+v, %v; want applied", res, err)
			}
		},
		"merged by a push setting its target to its head": func(t *testing.T, n *node, repo, _ string) {
			t.Helper()
			n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("B"))))
		},
		"closed by a push deleting its source": func(t *testing.T, n *node, repo, _ string) {
			t.Helper()
			n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), code.ZeroOID)))
		},
		"closed before a mergeability judged while it was live lands": func(t *testing.T, n *node, repo,
			change string) {

			t.Helper()
			closeIt(t, n, repo, change)
			// THE HOME'S RECORD, decided over the live change and appended
			// after its close: the two are on two subjects, which no
			// arbitration orders against each other.
			late := *n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean).Mergeability
			late.ChangeVersion, late.ReadinessVersion, late.WaitsPerson = 1, 3, true
			if _, err := n.publish(code.ChangeSubject(change), code.OpMergeability,
				code.Scope{code.RepoPath(repo), code.ChangePath(change)}, late); err != nil {
				t.Fatalf("publish the late mergeability: %v", err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			n.chart = testChart()
			repo, change := discussed(t, n, "web")
			w, _ := writerOn(t, n, n.pub)
			converse(t, w, asking(repo, change, dev, "founder"))
			converse(t, w, n.patched(repo, change, 1, "B", oidOf("the retry diff")))
			converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, code.MergeClean))
			if stored := n.storedReadiness(change); !stored.waitsPerson {
				t.Fatalf("the merge stored %+v, want waiting on a person: founder alone is asked", stored)
			}

			end(t, n, repo, change)
			if state := n.count(`SELECT COUNT(*) FROM code_changes WHERE id = ? AND state IN ('closed', 'merged')`,
				change); state != 1 {
				t.Fatal("the change is still live: the case ended nothing")
			}
			if stored := n.storedReadiness(change); stored.ready || stored.waitsPerson {
				t.Fatalf("the ended change stores %+v, want ready for nobody and waiting on nobody", stored)
			}
			if waiting := n.count(`SELECT COUNT(*) FROM code_mergeability INDEXED BY code_mergeability_waits_idx
				WHERE waits_person = 1`); waiting != 0 {
				t.Fatalf("code_mergeability_waits_idx holds %d changes, want none: the one there has ended",
					waiting)
			}
			if got, pages := listChanges(t, n, code.ReadQuery{Asker: ada, Needs: code.NeedPerson}, 0); len(got) != 0 ||
				pages != 1 {
				t.Fatalf("needs=person lists %q over %d pages, want nothing on one page", got, pages)
			}
		})
	}
}

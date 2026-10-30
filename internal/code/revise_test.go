package code_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REVISION'S FACTS THROUGH THE CHANGE WRITER: what the repository's home
// git node read of a revision against its target, handed to the change writer
// as the home's own write (no asker), decided in one snapshot on a real
// embedded broker as a record on the change's own subject — its guard, the
// path owners it asks and skips, whom it wakes — and applied by the shipped
// applier from the record alone.

// tip is ref's target on repo and the position of the record that moved it
// there, as n's rows hold them: what a home reads its facts against.
func (n *node) tip(repo, ref string) (string, statelog.Position) {
	n.t.Helper()
	var target string
	var at int64
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT target, position FROM code_refs
			WHERE repo_id = ? AND ref = ?`, repo, ref).Scan(&target, &at)
	}); err != nil {
		n.t.Fatalf("read %s's %s: %v", repo, ref, err)
	}
	return target, statelog.Unpack(topics.CodeLogStream, at)
}

// revising is the home's write of revision rev's facts on change — its head
// head, against target at its tip — touching touched, the path owners matched
// against owners: what a home that read them would send, its owners matched
// by code.MatchOwners. Its base is A, and its head's tree its head's own, so
// no two heads hold the same code unless a case says they do (codeOf).
func (n *node) revising(repo, change string, rev int64, head, target string, owners []code.PathOwner,
	touched ...string) code.ConversationWrite {

	n.t.Helper()
	tip, at := n.tip(repo, target)
	matched, digest := code.MatchOwners(owners, touched, false)
	return code.ConversationWrite{OpID: statelog.NewOpID(time.Now(), ""), Revise: &code.Revise{
		RepoID: repo, ChangeID: change, N: rev, HeadOID: head, TargetRef: target, TargetOID: tip,
		TargetPosition: at, BaseOID: oidOf("A"), BaseTree: oidOf("the tree of A"),
		HeadTree: oidOf("the tree of " + head), PatchID: oidOf("patch-" + head), Touched: touched, Node: n.id,
		RequestedOwners: matched, OwnersDigest: digest}}
}

// codeOf is w's revise as one holding the trees of the code named held — the
// base's and the head's — whatever its head commit: what a revision whose
// commits moved and whose code did not reads.
func codeOf(w code.ConversationWrite, held string) code.ConversationWrite {
	w.Revise.BaseTree, w.Revise.HeadTree = oidOf("the base tree of "+held), oidOf("the head tree of "+held)
	return w
}

// storedFacts is a revision's facts as n's rows hold them.
type storedFacts struct {
	target, tip      string
	at               int64
	touched          []string
	owners, skipped  string
	digest, patchID  string
	touchesAll, held bool
}

// facts is revision rev of change's facts as n's rows hold them; held is false
// where there are none.
func (n *node) facts(change string, rev int64) storedFacts {
	n.t.Helper()
	var f storedFacts
	var touched string
	err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `SELECT target_ref, target_oid, target_position, touched, owners,
			skipped, owners_digest, patch_id, touches_all FROM code_revision_facts WHERE change_id = ? AND n = ?`,
			change, rev).Scan(&f.target, &f.tip, &f.at, &touched, &f.owners, &f.skipped, &f.digest, &f.patchID,
			&f.touchesAll)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storedFacts{}
	case err != nil:
		n.t.Fatalf("read revision %d of %s's facts: %v", rev, change, err)
	}
	f.held = true
	if touched != "" {
		f.touched = strings.Split(touched, "\x00")
	}
	return f
}

// ownedChange is a change opened by dev from feature into main on a fresh
// repository named name — Engineering's, as changeRepo makes them — whose
// path owners are owners.
func ownedChange(t *testing.T, n *node, name string, owners ...code.PathOwner) (repo, change string) {
	t.Helper()
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.PathOwners = code.CanonicalPathOwners(owners)
	repo = n.ensure(name, s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(featureRef, code.ZeroOID, oidOf("B")), set(releaseRef, code.ZeroOID, oidOf("R"))))
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(repo, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	return repo, opened.ChangeID
}

// A REVISE IS GUARDED BY ITS REVISION AND ITS TARGET: facts are recorded only
// of the change's HEAD revision and head, against the branch it merges into
// now, and only past the tip the facts it holds of that revision were read at
// — so the same tip read twice (a home's pass run again, two homes a split
// presence view each called home), facts of a revision a push has moved past,
// of a head the change no longer has and against a branch it does not merge
// into are each refused superseded, appending nothing, while facts of a later
// tip of the same branch replace the row. A closed or merged change takes no
// facts; a revise naming any asker, or measuring a head from itself — which
// its target holds, and its home lands — is no write at all.
//
// Mutation: drop the position term of the guard, and the tip read twice
// appends a second record.
func TestAReviseIsGuardedByItsRevisionAndTarget(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()

	first := n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go")
	res, err := w.Converse(ctx, first)
	requireConversed(t, res, err)
	if f := n.facts(change, 1); !f.held || f.target != mainRef || f.tip != oidOf("A") ||
		!slices.Equal(f.touched, []string{"upload/retry.go"}) || f.digest != code.OwnersDigest(nil) {
		t.Fatalf("revision 1's facts are %+v, want main at A touching upload/retry.go", f)
	}

	again := n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go")
	before := n.end()
	res, err = w.Converse(ctx, again)
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "holds facts against")

	n.revise(repo, "B", "B2")
	for name, tc := range map[string]struct {
		w    code.ConversationWrite
		says string
	}{
		"facts of a revision the change has moved past": {
			n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go"), "is at revision 2"},
		"facts of a head the change does not have": {
			n.revising(repo, change, 2, oidOf("B3"), mainRef, nil, "upload/retry.go"), "is at revision 2"},
		"facts against a branch the change does not merge into": {
			n.revising(repo, change, 2, oidOf("B2"), releaseRef, nil, "upload/retry.go"), "merges into"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, tc.w)
		if err != nil || res.Refusal != code.ConversationRefusalSuperseded || !strings.Contains(res.Detail, tc.says) ||
			n.end() != before {
			t.Errorf("%s answered %+v, %v; want superseded saying %q, nothing appended", name, res, err, tc.says)
		}
	}

	second := n.revising(repo, change, 2, oidOf("B2"), mainRef, nil, "upload/retry.go", "upload/backoff.go")
	res, err = w.Converse(ctx, second)
	requireConversed(t, res, err)
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	later := n.revising(repo, change, 2, oidOf("B2"), mainRef, nil, "upload/backoff.go")
	res, err = w.Converse(ctx, later)
	requireConversed(t, res, err)
	if f := n.facts(change, 2); f.tip != oidOf("A2") || f.at != later.Revise.TargetPosition.Packed() ||
		!slices.Equal(f.touched, []string{"upload/backoff.go"}) {
		t.Fatalf("revision 2's facts are %+v, want the later tip's, A2, replacing the earlier one's", f)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_revision_facts WHERE change_id = ?`, change); got != 2 {
		t.Fatalf("the change holds %d revisions' facts, want one row each of its two revisions", got)
	}
	// THE EARLIER TIP'S FACTS, arriving after the later tip's — a pass that read
	// before the target moved, slower than the pass that read after — are a
	// fresh op of facts the change has moved past. (Sending `second` itself
	// again would be its own op retried, answered with its first outcome.)
	late := second
	late.OpID = statelog.NewOpID(time.Now(), "")
	before = n.end()
	res, err = w.Converse(ctx, late)
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "holds facts against")

	invalid := n.revising(repo, change, 2, oidOf("B2"), mainRef, nil, "upload/retry.go")
	invalid.Asker = dev
	if _, err := w.Converse(ctx, invalid); !errors.Is(err, code.ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "asker") {
		t.Fatalf("a revise naming an asker answered %v, want ErrInvalidProposal naming the asker", err)
	}
	merged := n.revising(repo, change, 2, oidOf("B2"), mainRef, nil)
	merged.Revise.BaseOID = merged.Revise.HeadOID
	if _, err := w.Converse(ctx, merged); !errors.Is(err, code.ErrInvalidProposal) ||
		!strings.Contains(err.Error(), "revise.base_oid") {
		t.Fatalf("facts measuring a head from itself answered %v, want ErrInvalidProposal naming its base", err)
	}

	c, _ := committerOn(t, n, n.pub)
	closed, err := c.ProposeChange(ctx, closing(repo, change, dev, n.change(change).version))
	requireChanged(t, closed, err)
	before = n.end()
	res, err = w.Converse(ctx, n.revising(repo, change, 2, oidOf("B2"), mainRef, nil, "upload/retry.go"))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalChangeClosed, "closed")
}

// A RETARGET RECOMPUTES THE FACTS: a revision's facts are against the branch its
// change merges into, so once the change is retargeted the facts its home holds
// against the old branch are no longer the change's — facts read against the
// old branch are refused superseded — and the facts read against the new one
// replace them, one row a revision.
//
// Mutation: hold a revise to the facts already held of its revision whatever
// branch they are against, and the new branch's facts are refused.
func TestARetargetRecomputesTheFacts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	c, _ := committerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go"))
	requireConversed(t, res, err)
	moved, err := c.ProposeChange(ctx, writing(dev, n.change(change).version, func(p *code.ChangeProposal) {
		p.Retarget = &code.ChangeRetarget{RepoID: repo, ChangeID: change, TargetRef: releaseRef}
	}))
	requireChanged(t, moved, err)

	before := n.end()
	res, err = w.Converse(ctx, n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go"))
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "merges into")

	res, err = w.Converse(ctx, n.revising(repo, change, 1, oidOf("B"), releaseRef, nil, "upload/retry.go",
		"release/notes.md"))
	requireConversed(t, res, err)
	f := n.facts(change, 1)
	if f.target != releaseRef || f.tip != oidOf("R") ||
		!slices.Equal(f.touched, []string{"upload/retry.go", "release/notes.md"}) {
		t.Fatalf("revision 1's facts after the retarget are %+v, want release's at R", f)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_revision_facts WHERE change_id = ?`, change); got != 1 {
		t.Fatalf("the change holds %d facts' rows after the retarget, want its one revision's", got)
	}
}

// MATCHING A REVISION'S TOUCHED PATHS IS THE HOME'S (code.MatchOwners): every
// owner one of whose globs matches a touched path — and every owner where the
// revision touches all — in the order the repository's settings hold them,
// beside the digest of the rows matched against.
func TestMatchOwnersReadsEveryGlobOfEveryOwner(t *testing.T) {
	t.Parallel()
	owners := code.CanonicalPathOwners([]code.PathOwner{
		pathOwner(code.OwnerUnit, "Platform", "upload/**"),
		pathOwner(code.OwnerSeat, "cto", "docs/*.md", "Makefile"),
		pathOwner(code.OwnerUnit, "Sales", "sales/**"),
	})
	for name, tc := range map[string]struct {
		touched []string
		all     bool
		want    []code.Owner
	}{
		"one glob of one owner": {[]string{"upload/retry.go", "README.md"}, false,
			[]code.Owner{{Kind: code.OwnerUnit, Ref: "Platform"}}},
		"the second glob of an owner": {[]string{"Makefile"}, false,
			[]code.Owner{{Kind: code.OwnerSeat, Ref: "cto"}}},
		"a glob that matches no deeper": {[]string{"docs/guides/code.md"}, false, nil},
		"two owners, in their order": {[]string{"sales/q3.csv", "docs/index.md"}, false,
			[]code.Owner{{Kind: code.OwnerSeat, Ref: "cto"}, {Kind: code.OwnerUnit, Ref: "Sales"}}},
		"touching all": {nil, true, []code.Owner{{Kind: code.OwnerSeat, Ref: "cto"},
			{Kind: code.OwnerUnit, Ref: "Platform"}, {Kind: code.OwnerUnit, Ref: "Sales"}}},
	} {
		got, digest := code.MatchOwners(owners, tc.touched, tc.all)
		if !reflect.DeepEqual(got, tc.want) || digest != code.OwnersDigest(owners) {
			t.Errorf("%s: MatchOwners = %v, %s; want %v, %s", name, got, digest, tc.want, code.OwnersDigest(owners))
		}
	}
	shuffled := []code.PathOwner{owners[2], owners[0], owners[1]}
	if code.OwnersDigest(shuffled) != code.OwnersDigest(owners) {
		t.Errorf("one repository's owners digest apart in two orders: a digest is of their one form")
	}
	if code.OwnersDigest(nil) == code.OwnersDigest(owners[:1]) {
		t.Errorf("no owners and one owner digest alike")
	}
}

// PATH OWNERS ARE REQUESTED FROM THE TOUCHED PATHS: the owners whose globs a
// revision's touched paths match are asked to review it — a unit owner of its
// EFFECTIVE LEAD, a seat owner of itself — each asked once, its ask a row
// with the reason path_owner naming no asker, the reviewer a participant; an
// owner whose globs the revision does not touch is asked nothing, and a
// revision touching all asks every owner the chart lets review.
//
// Mutation: resolve a unit owner to the unit's own key rather than its lead,
// and the ask names no seat; ask every owner whatever it matched, and the
// seat owning the docs is asked of a change to the code.
func TestPathOwnersAreRequestedFromTouchedPaths(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	owners := []code.PathOwner{
		pathOwner(code.OwnerUnit, "Platform", "upload/**"),
		pathOwner(code.OwnerSeat, "cto", "docs/*.md"),
	}
	repo, change := ownedChange(t, n, "web", owners...)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, n.revising(repo, change, 1, oidOf("B"), mainRef, owners, "upload/retry.go",
		"README.md"))
	requireConversed(t, res, err)
	asked, ok := n.reviewer(change, "plat-lead")
	if !ok || asked.reason != string(code.ReviewerPathOwner) || asked.state != string(code.ReviewerRequested) ||
		asked.kind != string(code.PartyAgent) || asked.by != "" || asked.byKind != "" {
		t.Fatalf("Platform's lead's ask is %+v (%v), want a path owner's ask of an agent naming no asker", asked, ok)
	}
	if _, ok := n.reviewer(change, "cto"); ok {
		t.Fatalf("the seat owning docs/*.md was asked of a revision touching no doc")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_participants WHERE change_id = ? AND handle = ?`, change,
		"plat-lead"); got != 1 {
		t.Fatalf("Platform's lead is %d participants of the change, want one", got)
	}
	f := n.facts(change, 1)
	if want, _ := json.Marshal([]code.Owner{{Kind: code.OwnerUnit, Ref: "Platform"}}); f.owners != string(want) ||
		f.skipped != "" {
		t.Fatalf("the facts' match is %q, skipping %q; want Platform's, skipping nobody", f.owners, f.skipped)
	}

	n.revise(repo, "B", "B2")
	every := n.revising(repo, change, 2, oidOf("B2"), mainRef, owners)
	every.Revise.Touched, every.Revise.TouchesAll = nil, true
	every.Revise.RequestedOwners, every.Revise.OwnersDigest = code.MatchOwners(owners, nil, true)
	res, err = w.Converse(ctx, every)
	requireConversed(t, res, err)
	if got, ok := n.reviewer(change, "cto"); !ok || got.reason != string(code.ReviewerPathOwner) {
		t.Fatalf("a revision touching all asked the cto %+v (%v), want a path owner's ask", got, ok)
	}
	revise := decodedRevise(t, n, res.Position)
	if want := []code.Reviewer{{Handle: "cto", Kind: code.PartyAgent}}; !reflect.DeepEqual(revise.Asked, want) {
		t.Fatalf("the record asks %+v, want the cto alone: Platform's lead is asked already", revise.Asked)
	}
}

// A PATH OWNER WHO CANNOT REVIEW IS SKIPPED, the record naming each and why: a
// seat whose grant is below review, an author of the change — whose review of
// their own change counts for nothing — a unit with no lead, and an owner the
// chart holds no seat of; and an owner whose ask the change holds already,
// withdrawn by somebody's word, is left as it is — neither asked again nor
// skipped.
//
// Mutation: drop the author term, and dev is asked to review dev's change;
// drop the grant term, and a read-only seat is asked.
func TestAPathOwnerWhoCannotReviewIsSkipped(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	owners := []code.PathOwner{
		pathOwner(code.OwnerSeat, "reader", "upload/**"),
		pathOwner(code.OwnerSeat, "dev", "upload/**"),
		pathOwner(code.OwnerUnit, "a unit since dissolved", "upload/**"),
		pathOwner(code.OwnerSeat, "gone", "upload/**"),
		pathOwner(code.OwnerUnit, "Platform", "upload/**"),
	}
	repo, change := ownedChange(t, n, "web", owners...)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, asking(repo, change, cto, "plat-lead"))
	requireConversed(t, res, err)
	res, err = w.Converse(ctx, withdrawing(repo, change, cto, "plat-lead"))
	requireConversed(t, res, err)

	res, err = w.Converse(ctx, n.revising(repo, change, 1, oidOf("B"), mainRef, owners, "upload/retry.go"))
	requireConversed(t, res, err)
	revise := decodedRevise(t, n, res.Position)
	want := []code.SkippedOwner{
		{Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, Handle: "dev", Why: code.SkipAuthor},
		{Owner: code.Owner{Kind: code.OwnerSeat, Ref: "gone"}, Handle: "gone", Why: code.SkipNoSeat},
		{Owner: code.Owner{Kind: code.OwnerSeat, Ref: "reader"}, Handle: "reader", Why: code.SkipBelowReview},
		{Owner: code.Owner{Kind: code.OwnerUnit, Ref: "a unit since dissolved"}, Why: code.SkipNoLead},
	}
	if len(revise.Asked) != 0 || !reflect.DeepEqual(revise.SkippedOwners, want) {
		t.Fatalf("the record asks %+v and skips %+v; want nobody asked, and skipped\n\t%+v", revise.Asked,
			revise.SkippedOwners, want)
	}
	for _, h := range []string{"reader", "dev", "gone"} {
		if _, ok := n.reviewer(change, h); ok {
			t.Errorf("%s was asked to review", h)
		}
	}
	if got, _ := n.reviewer(change, "plat-lead"); got.state != string(code.ReviewerWithdrawn) ||
		got.reason != string(code.ReviewerAsked) {
		t.Fatalf("Platform's lead's withdrawn ask is %+v after the facts, want it left withdrawn as the cto asked",
			got)
	}
	encoded, _ := json.Marshal(want)
	if f := n.facts(change, 1); f.skipped != string(encoded) {
		t.Fatalf("the facts' row skips %q, want %q", f.skipped, encoded)
	}
}

// AN OWNER RULE CHANGE SUPERSEDES THE REVISE: the decide recomputes the digest
// of the repository's path owners from its own snapshot, and facts matched
// against owners the repository has changed since are refused superseded —
// recorded nowhere, for the home to match again — while facts matched against
// the owners it holds now land.
//
// Mutation: skip the digest's check, and the facts land asking the owner the
// repository no longer names.
func TestAnOwnerRuleChangeSupersedesTheRevise(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	was := []code.PathOwner{pathOwner(code.OwnerUnit, "Platform", "upload/**")}
	repo, change := ownedChange(t, n, "web", was...)
	w, _ := writerOn(t, n, n.pub)
	matched := n.revising(repo, change, 1, oidOf("B"), mainRef, was, "upload/retry.go")

	now := []code.PathOwner{pathOwner(code.OwnerSeat, "cto", "upload/**")}
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.PathOwners = code.CanonicalPathOwners(now)
	n.ensure("web", s, t1)

	before := n.end()
	res, err := w.Converse(t.Context(), matched)
	requireConversationRefused(t, n, before, res, err, code.ConversationRefusalSuperseded, "path owners")
	if _, ok := n.reviewer(change, "plat-lead"); ok {
		t.Fatalf("facts matched against the repository's old owners asked the owner it no longer names")
	}
	res, err = w.Converse(t.Context(), n.revising(repo, change, 1, oidOf("B"), mainRef, now, "upload/retry.go"))
	requireConversed(t, res, err)
	if got, ok := n.reviewer(change, "cto"); !ok || got.reason != string(code.ReviewerPathOwner) {
		t.Fatalf("facts matched against the owners the repository holds asked the cto %+v (%v)", got, ok)
	}
}

// A REVISION'S FIRST FACTS WAKE WHO MUST LOOK AGAIN, AND A CHANGE'S FIRST
// FACTS ITS LEAD: the writer stamps on the record, from its snapshot and its
// chart, the reviewers whose standing verdict asks for changes on an earlier
// revision (re_review) — on a revision's first facts alone, so a retarget's or
// a later tip's facts wake nobody again — and, on the change's first facts
// where nobody who may review it is asked, the lead of the repository's owner
// (lead_fallback); a change somebody may review is asked of falls back to
// nobody.
//
// Mutation: wake again on every facts of a revision, and the later tip's
// facts name plat-lead a second time; fall back whatever was asked, and the
// owned repository's first facts name the cto beside its path owner.
func TestARevisionsFirstFactsWakeWhoMustLookAgain(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	res, err := w.Converse(ctx, n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go"))
	requireConversed(t, res, err)
	first := decodedRevise(t, n, res.Position)
	if first.LeadFallback == nil || *first.LeadFallback != (code.Reviewer{Handle: "cto", Kind: code.PartyAgent}) ||
		len(first.ReReview) != 0 {
		t.Fatalf("the change's first facts wake %+v and fall back to %+v; want nobody again and Engineering's "+
			"lead, the cto, since nobody may review it", first.ReReview, first.LeadFallback)
	}

	review, err := w.Converse(ctx, reviewing(repo, change, platLead, 1, code.VerdictRequestChanges, nil))
	requireReviewed(t, review, err)
	n.revise(repo, "B", "B2")
	res, err = w.Converse(ctx, n.revising(repo, change, 2, oidOf("B2"), mainRef, nil, "upload/retry.go"))
	requireConversed(t, res, err)
	second := decodedRevise(t, n, res.Position)
	if want := []code.Reviewer{{Handle: "plat-lead", Kind: code.PartyAgent}}; !reflect.DeepEqual(second.ReReview,
		want) || second.LeadFallback != nil {
		t.Fatalf("revision 2's first facts wake %+v and fall back to %+v; want plat-lead again, who asked for "+
			"changes on revision 1, and no lead: the change has had facts", second.ReReview, second.LeadFallback)
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("A2"))))
	res, err = w.Converse(ctx, n.revising(repo, change, 2, oidOf("B2"), mainRef, nil, "upload/retry.go"))
	requireConversed(t, res, err)
	if later := decodedRevise(t, n, res.Position); len(later.ReReview) != 0 || later.LeadFallback != nil {
		t.Fatalf("a later tip's facts of revision 2 wake %+v and fall back to %+v; want nobody", later.ReReview,
			later.LeadFallback)
	}

	owners := []code.PathOwner{pathOwner(code.OwnerUnit, "Platform", "upload/**")}
	owned, ownedID := ownedChange(t, n, "api", owners...)
	res, err = w.Converse(ctx, n.revising(owned, ownedID, 1, oidOf("B"), mainRef, owners, "upload/retry.go"))
	requireConversed(t, res, err)
	if r := decodedRevise(t, n, res.Position); r.LeadFallback != nil || len(r.Asked) != 1 {
		t.Fatalf("a change its path owner is asked of falls back to %+v, asking %+v; want nobody to fall back to",
			r.LeadFallback, r.Asked)
	}
}

// A REVISE APPLIES FROM ITS RECORD ALONE: whom its facts ask, skip and wake is
// stamped on the record by the writer from its snapshot and its chart, so a
// node applying the record judges nothing again — it holds no chart at all —
// and holds the rows of the node that wrote it, byte for byte.
func TestAReviseAppliesFromItsRecordAlone(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	owners := []code.PathOwner{
		pathOwner(code.OwnerUnit, "Platform", "upload/**"),
		pathOwner(code.OwnerSeat, "reader", "docs/**"),
	}
	repo, change := ownedChange(t, a, "web", owners...)
	w, _ := writerOn(t, a, a.pub)
	ctx := t.Context()
	first := a.revising(repo, change, 1, oidOf("B"), mainRef, owners, "upload/retry.go", "docs/upload.md")
	first.Revise.Classes = code.RevisionClasses{CommentOnly: []int{0}, Complete: true}
	res, err := w.Converse(ctx, first)
	requireConversed(t, res, err)
	a.revise(repo, "B", "B2")
	res, err = w.Converse(ctx, a.revising(repo, change, 2, oidOf("B2"), mainRef, owners, "upload/retry.go"))
	requireConversed(t, res, err)
	b.drain()
	for _, table := range []string{"code_revision_facts", "code_reviewers", "code_participants",
		"code_conversations"} {
		if got, want := b.dump(table), a.dump(table); !slices.Equal(got, want) {
			t.Errorf("%s replayed as\n\t%v\nwant\n\t%v", table, got, want)
		}
	}
}

// factsRead is the facts of revision rev of web's change number as a change
// read serves them, failing t where the read serves none.
func factsRead(t *testing.T, n *node, number, rev int64) code.RevisionFactsRow {
	t.Helper()
	answer, err := readChange(t, n, ada, "", "web", number, 0, "")
	if err != nil {
		t.Fatalf("read web!%d: %v", number, err)
	}
	for _, r := range answer.Revisions {
		if r.N == rev && r.Facts != nil {
			return *r.Facts
		}
	}
	t.Fatalf("the read of web!%d serves no facts of revision %d: %+v", number, rev, answer.Revisions)
	return code.RevisionFactsRow{}
}

// A CLASS IS RECORDED ON THE REVISION'S FACTS: what its home read of a
// revision's code — the path classes of the files it touches, each by its
// place in the touched list, whether its classification was complete, and
// the trees of its base and its head — rides the revise as the home sent it,
// is kept on the revision's facts row, and is served with its facts by a
// change read. Facts that touch all list no path, so they class none and are
// never complete: their row keeps no classes, and the read serves none.
//
// Mutation: write no classes to the row (bind "" for the column) — the read
// serves none, red.
func TestAClassIsRecordedOnTheRevisionsFacts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	classed := code.RevisionClasses{WhitespaceOnly: []int{2}, CommentOnly: []int{0}, Complete: true}
	write := n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go", "docs/upload.md",
		"upload/backoff.go")
	write.Revise.Classes = classed
	res, err := w.Converse(ctx, write)
	requireConversed(t, res, err)
	if r := decodedRevise(t, n, res.Position); !reflect.DeepEqual(r.Classes, classed) ||
		r.BaseTree != write.Revise.BaseTree || r.HeadTree != write.Revise.HeadTree {
		t.Fatalf("the revise recorded classes %+v and trees %s and %s, want the home's %+v, %s and %s", r.Classes,
			r.BaseTree, r.HeadTree, classed, write.Revise.BaseTree, write.Revise.HeadTree)
	}
	if f := factsRead(t, n, 1, 1); !reflect.DeepEqual(f.Classes, classed) || f.BaseTree != write.Revise.BaseTree ||
		f.HeadTree != write.Revise.HeadTree {
		t.Errorf("revision 1's facts are read with classes %+v and trees %s and %s, want %+v, %s and %s",
			f.Classes, f.BaseTree, f.HeadTree, classed, write.Revise.BaseTree, write.Revise.HeadTree)
	}

	n.revise(repo, "B", "B2")
	all := n.revising(repo, change, 2, oidOf("B2"), mainRef, nil)
	all.Revise.TouchesAll = true
	res, err = w.Converse(ctx, all)
	requireConversed(t, res, err)
	if f := factsRead(t, n, 1, 2); !f.TouchesAll || !f.Classes.IsZero() {
		t.Errorf("revision 2's facts, which touch all, are read touching all = %t with classes %+v, want no "+
			"class and incomplete", f.TouchesAll, f.Classes)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_revision_facts WHERE change_id = ? AND n = 2 AND classes = ''`,
		change); got != 1 {
		t.Errorf("revision 2's facts row keeps classes, want none for facts that touch all")
	}
}

// A REVISE'S CLASSES NAME ITS TOUCHED PATHS, and its trees are whole ids: a
// class names a path by its place in the touched list, so a revise naming a
// place past the list, places out of order or one twice, a path in both
// lists, or classes beside facts that touch all — which list no path, so
// class none and are never complete — is no write at all, refused before
// anything is decided, as is one naming no tree or a tree no id spells.
//
// Mutation: drop classesProblem from the revise's rules — the place past the
// list is accepted, red.
func TestARevisesClassesNameItsTouchedPaths(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	for name, tc := range map[string]struct {
		set   func(r *code.Revise)
		field string
	}{
		"a place past the list": {func(r *code.Revise) { r.Classes.WhitespaceOnly = []int{2} },
			"revise.classes.whitespace_only[0]"},
		"places out of order": {func(r *code.Revise) { r.Classes.CommentOnly = []int{1, 0} },
			"revise.classes.comment_only[1]"},
		"a place twice": {func(r *code.Revise) { r.Classes.CommentOnly = []int{0, 0} },
			"revise.classes.comment_only[1]"},
		"a path in both lists": {func(r *code.Revise) {
			r.Classes.WhitespaceOnly, r.Classes.CommentOnly = []int{1}, []int{1}
		}, "revise.classes.comment_only[0]"},
		"classes of facts that touch all": {func(r *code.Revise) {
			r.Touched, r.TouchesAll, r.Classes.Complete = nil, true, true
		}, "revise.classes"},
		"no head tree":           {func(r *code.Revise) { r.HeadTree = "" }, "revise.head_tree"},
		"a base tree of no id":   {func(r *code.Revise) { r.BaseTree = "the tree" }, "revise.base_tree"},
		"the zero id as a tree":  {func(r *code.Revise) { r.HeadTree = code.ZeroOID }, "revise.head_tree"},
		"a negative place named": {func(r *code.Revise) { r.Classes.CommentOnly = []int{-1} }, "comment_only[0]"},
	} {
		cw := n.revising(repo, change, 1, oidOf("B"), mainRef, nil, "upload/retry.go", "upload/backoff.go")
		tc.set(cw.Revise)
		before := n.end()
		if _, err := w.Converse(t.Context(), cw); !errors.Is(err, code.ErrInvalidProposal) ||
			!strings.Contains(err.Error(), tc.field) || n.end() != before {
			t.Errorf("%s answered %v, want ErrInvalidProposal naming %s, nothing appended", name, err, tc.field)
		}
	}
}

// decodedRevise is the revise the record at p carries, as n's log holds it.
func decodedRevise(t *testing.T, n *node, p statelog.Position) code.Revise {
	t.Helper()
	payload, err := code.DecodeMutation(n.recordAt(p))
	if err != nil {
		t.Fatalf("decode the record at %s: %v", p, err)
	}
	r, ok := payload.(code.Revise)
	if !ok {
		t.Fatalf("the record at %s carries a %T, want a revise", p, payload)
	}
	return r
}

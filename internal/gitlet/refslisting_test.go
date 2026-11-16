package gitlet_test

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// listed follows a refs listing from q to its end, answering every page.
func listed(t *testing.T, n *node, q gitlet.ReadQuery) []gitlet.ReadAnswer {
	t.Helper()
	var pages []gitlet.ReadAnswer
	for range 200 {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("Read(refs) page %d: %v", len(pages)+1, err)
		}
		pages = append(pages, answer)
		if answer.Next == "" {
			return pages
		}
		q.Cursor = answer.Next
	}
	t.Fatalf("the listing did not end in 200 pages")
	return nil
}

// refsIn is every ref the pages answer, in order, in detail or not.
func refsIn(pages []gitlet.ReadAnswer) []string {
	var out []string
	for _, p := range pages {
		for _, r := range p.Refs {
			out = append(out, r.Ref)
		}
		for _, r := range p.RefsDetail {
			out = append(out, r.Ref)
		}
	}
	return out
}

// detailOf is ref's row of a detailed first page of app's branches, as asked.
func detailOf(t *testing.T, n *node, who gitlet.Principal, repo, ref string) gitlet.RefListing {
	t.Helper()
	return detailUnder(t, n, who, "", repo, ref)
}

// detailUnder is detailOf for an asker whose turn acts for the ceiling seat.
func detailUnder(t *testing.T, n *node, who gitlet.Principal, ceiling, repo, ref string) gitlet.RefListing {
	t.Helper()
	q := gitlet.ReadQuery{Asker: who, CeilingSeat: ceiling, Kind: gitlet.ReadRefs, Repo: repo,
		Limit: gitlet.MaxRefsPerPage, RefListing: &gitlet.RefListingQuery{Detail: true}}
	if strings.HasPrefix(ref, "refs/tags/") {
		q.Prefix = "refs/tags/"
	}
	for _, page := range listed(t, n, q) {
		for _, l := range page.RefsDetail {
			if l.Ref == ref {
				return l
			}
		}
	}
	t.Fatalf("%s is not listed in %s", ref, repo)
	return gitlet.RefListing{}
}

// A BRANCH LISTING BY MOVE PAGES WITHOUT A GAP across one record's branches,
// and a window keeps exactly [moved_since, moved_before), to the nanosecond.
// Mutation: drop the cursor's ref tie-break, or round a bound the wrong way.
func TestABranchListingByMovePagesWithoutAGap(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	base := harnessAt.Add(-24 * time.Hour)
	at := map[string]time.Time{}
	for i := range 12 {
		ref := fmt.Sprintf("refs/heads/solo-%02d", i)
		at[ref] = base.Add(time.Duration(i) * time.Minute)
		n.pushAt(at[ref], id, entry(fmt.Sprintf("solo-%02d", i), set(ref, gitlet.ZeroOID, oidOf(ref))))
	}
	var group []gitlet.PushEntry
	together := base.Add(30 * time.Minute)
	for i := range 5 {
		ref := fmt.Sprintf("refs/heads/group-%d", i)
		at[ref] = together
		group = append(group, entry(fmt.Sprintf("group-%d", i), set(ref, gitlet.ZeroOID, oidOf(ref))))
	}
	n.pushAt(together, id, group...)

	ordered := func(since, before time.Time) []string {
		var want []string
		for ref, moved := range at {
			if !moved.Before(since) && (before.IsZero() || moved.Before(before)) { //fixedwait:stated the case states every instant
				want = append(want, ref)
			}
		}
		slices.SortFunc(want, func(a, b string) int {
			if c := at[b].Compare(at[a]); c != 0 { //fixedwait:stated the case states every instant
				return c
			}
			return strings.Compare(b, a)
		})
		return want
	}
	pages := listed(t, n, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app", Limit: 2,
		RefListing: &gitlet.RefListingQuery{Order: gitlet.RefOrderMoved}})
	if got, want := refsIn(pages), ordered(time.Time{}, time.Time{}); !slices.Equal(got, want) {
		t.Fatalf("the branches by move read\n\t%v\nwant\n\t%v", got, want)
	}
	for i, p := range pages[:len(pages)-1] {
		micros, ref, ok := strings.Cut(p.Next, " ")
		if _, err := strconv.ParseInt(micros, 10, 64); !ok || err != nil || !strings.HasPrefix(ref, "refs/heads/") {
			t.Errorf("page %d's Next is %q, want <microseconds> <branch>", i+1, p.Next)
		}
	}

	since, before := at["refs/heads/solo-03"], at["refs/heads/solo-09"]
	for _, tc := range []struct {
		name          string
		since, before time.Time
	}{
		{"since one branch's instant, before another's", since, before},
		{"a nanosecond past each", since.Add(time.Nanosecond), before.Add(time.Nanosecond)},
		{"since alone", since, time.Time{}},
	} {
		got := refsIn(listed(t, n, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app", Limit: 3,
			RefListing: &gitlet.RefListingQuery{Order: gitlet.RefOrderMoved, MovedSince: tc.since,
				MovedBefore: tc.before}}))
		if want := ordered(tc.since, tc.before); !slices.Equal(got, want) {
			t.Errorf("%s: the window read %v, want %v", tc.name, got, want)
		}
	}
}

// A BRANCH LISTING BY MOVE READS NO TAG: ten thousand tags beside thirty
// branches, and a moved listing under any other prefix is refused.
// Mutation: drop the namespace bounds from movedRefs (tags are listed).
func TestABranchListingByMoveReadsNoTag(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	for p := range 10 {
		cmds := make([]gitlet.Command, gitlet.MaxCommandsPerPush)
		for i := range cmds {
			tag := fmt.Sprintf("refs/tags/v%d.%04d", p, i)
			cmds[i] = set(tag, gitlet.ZeroOID, oidOf(tag))
		}
		n.pushAt(harnessAt.Add(time.Duration(p)*time.Minute), id, entry(fmt.Sprintf("tags-%d", p), cmds...))
	}
	var branches []string
	for i := range 30 {
		ref := fmt.Sprintf("refs/heads/b%02d", i)
		branches = append(branches, ref)
		n.pushAt(harnessAt.Add(-time.Duration(i+1)*time.Minute), id, entry(ref, set(ref, gitlet.ZeroOID, oidOf(ref))))
	}
	got := refsIn(listed(t, n, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
		RefListing: &gitlet.RefListingQuery{Order: gitlet.RefOrderMoved}}))
	if !slices.Equal(got, branches) {
		t.Fatalf("the branches by move read %d refs %v, want the 30 branches newest first", len(got), got)
	}
	tags := refsIn(listed(t, n, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
		Prefix: "refs/tags/", Limit: gitlet.MaxRefsPerPage, RefListing: &gitlet.RefListingQuery{}}))
	if len(tags) != 10*gitlet.MaxCommandsPerPush {
		t.Fatalf("the tags by name read %d, want every one", len(tags))
	}
	for _, prefix := range []string{"refs/tags/", "refs/heads/seats/dev/"} {
		_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
			Prefix: prefix, RefListing: &gitlet.RefListingQuery{Order: gitlet.RefOrderMoved}})
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "ref_listing.order") {
			t.Errorf("a moved listing under %s = %v, want ErrInvalid naming ref_listing.order", prefix, err)
		}
	}
}

// A LISTING NARROWED BY CONTAINS ANSWERS A CURSOR PAST ITS SCAN, and folds
// case as strings.EqualFold does (the Kelvin sign is k, the long s is s).
// Mutation: drop the short page's Next, unbound the scan, or fold by ToLower.
func TestAListingNarrowedByContainsAnswersACursorPastItsScan(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	gitlet.SetRefScanBounds(n.reader, 10, gitlet.MaxActiveBranchesCounted)
	var cmds []gitlet.Command
	for i := range 37 {
		ref := fmt.Sprintf("refs/heads/a%02d", i)
		cmds = append(cmds, set(ref, gitlet.ZeroOID, oidOf(ref)))
	}
	for _, name := range []string{"fix-a", "hotFIX", "prefix-z", "Kelvin", "ſeed"} {
		ref := "refs/heads/" + name
		cmds = append(cmds, set(ref, gitlet.ZeroOID, oidOf(ref)))
	}
	n.push(id, entry("p1", cmds...))

	contains := func(order gitlet.RefOrder, find string) []gitlet.ReadAnswer {
		return listed(t, n, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
			RefListing: &gitlet.RefListingQuery{Order: order, Contains: find}})
	}
	pages := contains(gitlet.RefOrderName, "FiX")
	if first := pages[0]; len(first.Refs) != 0 || first.Next != "refs/heads/a09" {
		t.Fatalf("page one holds %v and its Next is %q, want none found in its ten and a Next past a09",
			first.Refs, first.Next)
	}
	want := []string{"refs/heads/fix-a", "refs/heads/hotFIX", "refs/heads/prefix-z"}
	if got := refsIn(pages); !slices.Equal(got, want) {
		t.Fatalf("the branches holding FiX read %v, want %v", got, want)
	}
	// ONE RECORD MOVED THEM ALL, so by move they read by name, highest first.
	slices.Reverse(want)
	if got := refsIn(contains(gitlet.RefOrderMoved, "fix")); !slices.Equal(got, want) {
		t.Fatalf("the branches holding fix by move read %v, want %v", got, want)
	}
	for find, ref := range map[string]string{"k": "refs/heads/Kelvin", "SEED": "refs/heads/ſeed"} {
		if got := refsIn(contains(gitlet.RefOrderName, find)); !slices.Equal(got, []string{ref}) {
			t.Errorf("the branches holding %q read %q, want %q", find, got, ref)
		}
	}
}

// A BRANCH'S MOVER IS THE PUSH THAT SET IT, whatever the ids' order in one
// record; none where the record moved it back, or the history dropped the push.
// Mutation: take the first or the last setter, or fail on a pruned push.
func TestABranchsMoverIsThePushThatSetIt(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1", set("refs/heads/topic", gitlet.ZeroOID, oidOf("X")),
		set("refs/heads/back", gitlet.ZeroOID, oidOf("X"))))
	history, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadPushes,
		Repo: "app"})
	if err != nil || len(history.Pushes) != 1 || history.Pushes[0].PushID != "p1" {
		t.Fatalf("read the history: %v, %+v", err, history.Pushes)
	}
	p1 := history.Pushes[0]
	want := gitlet.RefMover{PushID: "p1", Actor: p1.Actor, ActorKind: p1.ActorKind, ActorSeat: p1.ActorSeat,
		Via: gitlet.ViaGit}
	if got := detailOf(t, n, asker, "app", "refs/heads/topic"); got.MovedBy == nil || *got.MovedBy != want ||
		!got.MovedAt.Equal(p1.At) {
		t.Fatalf("topic was moved by %+v at %s, want %+v at %s", got.MovedBy, got.MovedAt, want, p1.At)
	}

	// THE ENTRIES' IDS SORT AGAINST THE RECORD'S ORDER, as ids minted on two
	// nodes may.
	n.push(id, entry("z-first", set("refs/heads/topic", oidOf("X"), oidOf("Y"))),
		entry("a-second", set("refs/heads/topic", oidOf("Y"), oidOf("Z"))))
	if got := detailOf(t, n, asker, "app", "refs/heads/topic"); got.MovedBy == nil ||
		got.MovedBy.PushID != "a-second" {
		t.Fatalf("topic moved X to Y to Z in one record was moved by %+v, want a-second", got.MovedBy)
	}
	// AND WITH THEM, so neither the first row nor the last is the answer by luck.
	n.push(id, entry("b-first", set("refs/heads/topic", oidOf("Z"), oidOf("X"))),
		entry("c-second", set("refs/heads/topic", oidOf("X"), oidOf("Y"))))
	if got := detailOf(t, n, asker, "app", "refs/heads/topic"); got.MovedBy == nil ||
		got.MovedBy.PushID != "c-second" {
		t.Fatalf("topic moved Z to X to Y in one record was moved by %+v, want c-second", got.MovedBy)
	}
	n.push(id, entry("there", set("refs/heads/back", oidOf("X"), oidOf("Y"))),
		entry("back", set("refs/heads/back", oidOf("Y"), oidOf("X"))),
		entry("again", set("refs/heads/back", oidOf("X"), oidOf("Y"))))
	if got := detailOf(t, n, asker, "app", "refs/heads/back"); got.MovedBy != nil || got.OID != oidOf("Y") {
		t.Fatalf("back, moved away from Y and back to it in one record, at %s, was moved by %+v; want none named",
			got.OID, got.MovedBy)
	}

	old := harnessAt.Add(-gitlet.PushHistory - time.Hour)
	n.pushAt(old, id, entry("old", set("refs/heads/old", gitlet.ZeroOID, oidOf("old"))))
	n.pushAt(harnessAt, id, entry("later", set("refs/heads/later", gitlet.ZeroOID, oidOf("later"))))
	if pushes := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = 'old'`); pushes != 0 {
		t.Fatalf("the push history keeps %d rows of the push past its horizon", pushes)
	}
	if got := detailOf(t, n, asker, "app", "refs/heads/old"); got.MovedBy != nil || !got.MovedAt.Equal(old) {
		t.Fatalf("old, whose push the history dropped, was moved by %+v at %s; want none named, at %s",
			got.MovedBy, got.MovedAt, old)
	}
}

// A MERGED BRANCH'S MOVER IS THE MERGE: via merge, by the seat that merged it.
// Mutation: read the mover from another position.
func TestAMergedBranchsMoverIsTheMerge(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", gitlet.DirectPersons, mergeGate())
	approved(t, n, w, repo, change)
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	p := merging(repo, change, dev, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	res, err := c.ProposeMerge(t.Context(), p)
	requireMerged(t, res, err, oidOf("M"))
	got := detailOf(t, n, asker, "web", mainRef)
	want := gitlet.RefMover{PushID: p.Merge.MergeID, Actor: "dev", ActorKind: "agent", ActorSeat: "dev",
		Via: gitlet.ViaMerge}
	if got.MovedBy == nil || *got.MovedBy != want || got.Position != res.Position {
		t.Fatalf("main at %v was moved by %+v, want %+v at the merge's %v", got.Position, got.MovedBy, want,
			res.Position)
	}
}

// A GLOB RULE PROTECTS THE BRANCHES IT MATCHES, every matching rule together.
// Mutation: match by equality, or keep the first or the last rule alone.
func TestAGlobRuleProtectsTheBranchesItMatches(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	s := suiteSettings()
	s.Protections = []gitlet.Protection{
		{PushRule: gitlet.PushRule{Pattern: "refs/heads/main", DirectPush: gitlet.DirectNone}},
		{PushRule: gitlet.PushRule{Pattern: "refs/heads/rel/*", AllowForce: true, DirectPush: gitlet.DirectPersons}},
		{PushRule: gitlet.PushRule{Pattern: "refs/heads/rel/x", AllowDelete: true, DirectPush: gitlet.DirectPersons}},
		{PushRule: gitlet.PushRule{Pattern: "refs/heads/release/*", AllowForce: true,
			DirectPush: gitlet.DirectPersons}},
	}
	id := n.ensure("app", s, t1).RepoID
	var cmds []gitlet.Command
	for _, b := range []string{"main", "release/1.2", "rel/x", "feature/x"} {
		cmds = append(cmds, set("refs/heads/"+b, gitlet.ZeroOID, oidOf(b)))
	}
	n.push(id, entry("p1", cmds...))
	for ref, want := range map[string]*gitlet.RefProtection{
		"refs/heads/main": {Patterns: []string{"refs/heads/main"}, DirectPush: gitlet.DirectNone},
		"refs/heads/release/1.2": {Patterns: []string{"refs/heads/release/*"}, AllowForce: true,
			DirectPush: gitlet.DirectPersons},
		"refs/heads/rel/x": {Patterns: []string{"refs/heads/rel/*", "refs/heads/rel/x"},
			DirectPush: gitlet.DirectPersons},
		"refs/heads/feature/x": nil,
	} {
		got := detailOf(t, n, asker, "app", ref).Protection
		if (got == nil) != (want == nil) || got != nil && fmt.Sprint(*got) != fmt.Sprint(*want) {
			t.Errorf("%s is protected %+v, want %+v", ref, got, want)
		}
	}
}

// MAY_DELETE IS JUDGED AS THE DELETION WOULD BE, by an engine write's rules in
// their order and under the seat a turn acts for, each sentence the rule's own.
// Mutation: drop an arm or the ceiling, or judge the protections before the grant.
func TestMayDeleteIsJudgedAsTheDeletionWouldBe(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections = []gitlet.Protection{
		{PushRule: gitlet.PushRule{Pattern: "refs/heads/main", AllowDelete: true, DirectPush: gitlet.DirectPersons}},
		{PushRule: gitlet.PushRule{Pattern: "refs/heads/release/*", DirectPush: gitlet.DirectPersons}},
	}
	id := n.ensure("app", s, t1).RepoID
	refs := []string{"refs/heads/main", "refs/heads/release/1", "refs/heads/feature",
		"refs/heads/seats/dev/x", "refs/heads/seats/cto/y"}
	var cmds []gitlet.Command
	for _, ref := range refs {
		cmds = append(cmds, set(ref, gitlet.ZeroOID, oidOf(ref)))
	}
	n.push(id, entry("p1", cmds...))
	rules, err := gitlet.ParseProtections([]gitlet.PushRule{s.Protections[0].PushRule, s.Protections[1].PushRule})
	if err != nil {
		t.Fatal(err)
	}
	deletion := func(ref string) gitlet.Command {
		return gitlet.Command{Ref: ref, Old: oidOf(ref), New: gitlet.ZeroOID}
	}
	repo := gitlet.RepoRef{ID: id, Name: "app", Owner: s.Owner, Visibility: s.Visibility, DefaultBranch: "main"}
	devGrant := gitlet.GrantOf(dev, repo, n.chart)
	notPermitted, notPermittedWhy := devGrant.RefRule(deletion("refs/heads/feature"))
	othersSeat, othersSeatWhy := devGrant.RefRule(deletion("refs/heads/seats/cto/y"))
	cto := agent("cto")
	ceilinged, ceilingWhy := gitlet.GrantOf(cto, repo, n.chart).Under(devGrant).RefRule(deletion("refs/heads/feature"))
	if othersSeat != gitlet.RefusalNotPermitted || ceilinged != gitlet.RefusalNotPermitted ||
		!strings.Contains(ceilingWhy, "the seat whose ask started its turn") {
		t.Fatalf("the rules themselves say %s and %s %q", othersSeat, ceilinged, ceilingWhy)
	}
	protected, rule := gitlet.ProtectionRefusal(deletion("refs/heads/release/1"), rules, asker.Kind)
	protectedWhy := gitlet.ProtectionDetail(rule, protected)
	keptDefault, keptWhy := gitlet.DefaultBranchRefusal(deletion("refs/heads/main"), "app", "main")
	if notPermitted != gitlet.RefusalNotPermitted || protected != gitlet.RefusalProtectedDelete ||
		keptDefault != gitlet.RefusalDefaultBranch {
		t.Fatalf("the rules themselves say %s, %s and %s", notPermitted, protected, keptDefault)
	}
	type verdict struct {
		may     bool
		refusal gitlet.Refusal
		detail  string
	}
	for _, tc := range []struct {
		who     gitlet.Principal
		ceiling string
		ref     string
		want    verdict
	}{
		{asker, "", "refs/heads/main", verdict{refusal: keptDefault, detail: keptWhy}},
		{asker, "", "refs/heads/release/1", verdict{refusal: protected, detail: protectedWhy}},
		{asker, "", "refs/heads/feature", verdict{may: true}},
		{dev, "", "refs/heads/feature", verdict{refusal: notPermitted, detail: notPermittedWhy}},
		{dev, "", "refs/heads/release/1", verdict{refusal: notPermitted, detail: notPermittedWhy}},
		{dev, "", "refs/heads/seats/dev/x", verdict{may: true}},
		{dev, "", "refs/heads/seats/cto/y", verdict{refusal: othersSeat, detail: othersSeatWhy}},
		{cto, "", "refs/heads/feature", verdict{may: true}},
		{cto, "dev", "refs/heads/feature", verdict{refusal: ceilinged, detail: ceilingWhy}},
	} {
		got := detailUnder(t, n, tc.who, tc.ceiling, "app", tc.ref)
		if g := (verdict{got.MayDelete, got.DeleteRefusal, got.DeleteDetail}); g != tc.want {
			t.Errorf("%s (for %q) deleting %s reads %+v, want %+v", tc.who.Seat, tc.ceiling, tc.ref, g, tc.want)
		}
	}

	s.Archived = true
	n.ensure("app", s, t2)
	archived, archivedWhy := gitlet.RepoStateRefusal(gitlet.RepoRef{Name: "app", Status: gitlet.RepoStatusArchived,
		ManagedBy: s.ManagedBy})
	for _, ref := range refs {
		got := detailOf(t, n, asker, "app", ref)
		if got.MayDelete || got.DeleteRefusal != gitlet.RefusalRepoArchived || got.DeleteDetail != archivedWhy ||
			archived != gitlet.RefusalRepoArchived {
			t.Errorf("deleting %s of an archived repository reads %t %s %q, want %s %q", ref, got.MayDelete,
				got.DeleteRefusal, got.DeleteDetail, archived, archivedWhy)
		}
	}
}

// A DETAILED PAGE IS CHARGED THE MOVERS IT READ, and paging still reaches
// every branch one mirror-wide push moved.
// Mutation: charge nothing for the push rows read.
func TestADetailedPageIsChargedTheMoversItRead(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	var cmds []gitlet.Command
	var want []string
	for i := range 50 {
		ref := fmt.Sprintf("refs/heads/mirrored-%02d", i)
		cmds = append(cmds, set(ref, gitlet.ZeroOID, oidOf(ref)))
		want = append(want, ref)
	}
	n.push(id, entry("mirror", cmds...))
	gitlet.SetPageLimits(n.reader, gitlet.MaxSnapshotItems, 4<<10)
	q := gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app", RefListing: &gitlet.RefListingQuery{}}
	if plain := listed(t, n, q); len(plain[0].Refs) < 20 {
		t.Fatalf("a page in brief holds %d branches of a 4 KiB budget, want 20 or more", len(plain[0].Refs))
	}
	q.RefListing.Detail = true
	pages := listed(t, n, q)
	if got := len(pages[0].RefsDetail); got != 1 {
		t.Fatalf("a detailed page holds %d branches, want one: the push row it read fills its 4 KiB", got)
	}
	if got := refsIn(pages); !slices.Equal(got, want) {
		t.Fatalf("the detailed pages read %v, want every branch once", got)
	}
}

// A BRANCH NAMES ITS LIVE CHANGE, ELSE ITS NEWEST; a tag names none.
// Mutation: answer the newest first, or the oldest.
func TestABranchNamesItsLiveChangeElseItsNewest(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	var cmds []gitlet.Command
	for _, ref := range []string{"refs/heads/live", "refs/heads/ended", "refs/heads/reopened", "refs/heads/none",
		"refs/tags/v1"} {
		cmds = append(cmds, set(ref, gitlet.ZeroOID, oidOf(ref)))
	}
	n.push(id, entry("p1", cmds...))
	for i, c := range []struct {
		source, state, title string
	}{
		{"ended", "closed", "first"}, {"ended", "merged", "second"}, {"live", "open", "the live one"},
		{"reopened", "open", "reopened"}, {"reopened", "closed", "closed since"},
	} {
		n.exec(`INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
			head_oid, revision, owner, owner_kind, opened_at, opened_position, version, position)
			VALUES (?, ?, ?, ?, 0, ?, ?, 'refs/heads/main', ?, 1, 'dev', 'agent', 0, 1, 1, 1)`,
			fmt.Sprintf("change-%d", i+1), id, i+1, c.title, c.state, "refs/heads/"+c.source, oidOf(c.title))
	}
	for ref, want := range map[string]*gitlet.RefChange{
		"refs/heads/live":     {Number: 3, Title: "the live one", State: gitlet.ChangeStateOpen},
		"refs/heads/ended":    {Number: 2, Title: "second", State: gitlet.ChangeStateMerged},
		"refs/heads/reopened": {Number: 4, Title: "reopened", State: gitlet.ChangeStateOpen},
		"refs/heads/none":     nil,
		"refs/tags/v1":        nil,
	} {
		got := detailOf(t, n, asker, "app", ref).Change
		if (got == nil) != (want == nil) || got != nil && *got != *want {
			t.Errorf("%s names change %+v, want %+v", ref, got, want)
		}
	}
}

// THE ACTIVE COUNT STOPS AT ITS BOUND, leaves the default branch out, and is a
// first page's alone.
// Mutation: count the default branch, or drop the bound.
func TestTheActiveCountStopsAtItsBound(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	threshold := harnessAt.Add(-7 * 24 * time.Hour)
	for i, b := range []string{"main", "a", "b", "c", "d"} {
		ref := "refs/heads/" + b
		n.pushAt(threshold.Add(time.Duration(i)*time.Hour), id, entry("new-"+b, set(ref, gitlet.ZeroOID, oidOf(ref))))
	}
	for i := range 3 {
		ref := fmt.Sprintf("refs/heads/old-%d", i)
		n.pushAt(threshold.Add(-time.Duration(i+1)*time.Hour), id, entry(ref, set(ref, gitlet.ZeroOID, oidOf(ref))))
	}
	count := func() (*gitlet.BranchCount, string) {
		answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
			Limit: 2, RefListing: &gitlet.RefListingQuery{Order: gitlet.RefOrderMoved, ActiveSince: threshold}})
		if err != nil {
			t.Fatalf("Read(refs, active_since): %v", err)
		}
		return answer.ActiveBranches, answer.Next
	}
	got, next := count()
	if got == nil || *got != (gitlet.BranchCount{Count: 4}) {
		t.Fatalf("the active branches count %+v, want 4: a, b, c and d, never main", got)
	}
	gitlet.SetRefScanBounds(n.reader, gitlet.MaxRefsScanned, 3)
	if got, _ := count(); got == nil || *got != (gitlet.BranchCount{Count: 3, More: true}) {
		t.Fatalf("the active branches past a bound of 3 count %+v, want 3 and more", got)
	}
	_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
		Cursor: next, RefListing: &gitlet.RefListingQuery{Order: gitlet.RefOrderMoved, ActiveSince: threshold}})
	if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "ref_listing.active_since") {
		t.Fatalf("a later page's count = %v, want ErrInvalid naming ref_listing.active_since", err)
	}
}

// A REF LISTING IS REFUSED WHERE IT MEANS NOTHING, naming its field.
// Mutation: relax any refusal into today's listing.
func TestARefListingIsRefusedWhereItMeansNothing(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.push(id, entry("p1", set("refs/heads/main", gitlet.ZeroOID, oidOf("main"))))
	day := harnessAt.Add(-24 * time.Hour)
	for name, tc := range map[string]struct {
		q     gitlet.ReadQuery
		field string
	}{
		"on a repository read": {gitlet.ReadQuery{Kind: gitlet.ReadRepo,
			RefListing: &gitlet.RefListingQuery{}}, "ref_listing"},
		"an unknown order": {gitlet.ReadQuery{Kind: gitlet.ReadRefs,
			RefListing: &gitlet.RefListingQuery{Order: "size"}}, "ref_listing.order"},
		"a window by name": {gitlet.ReadQuery{Kind: gitlet.ReadRefs,
			RefListing: &gitlet.RefListingQuery{MovedSince: day}}, "ref_listing.moved_since"},
		"a bound by name": {gitlet.ReadQuery{Kind: gitlet.ReadRefs,
			RefListing: &gitlet.RefListingQuery{MovedBefore: day}}, "ref_listing.moved_before"},
		"an empty window": {gitlet.ReadQuery{Kind: gitlet.ReadRefs, RefListing: &gitlet.RefListingQuery{
			Order: gitlet.RefOrderMoved, MovedSince: day, MovedBefore: day}}, "ref_listing.moved_before"},
		"an instant at 1970": {gitlet.ReadQuery{Kind: gitlet.ReadRefs, RefListing: &gitlet.RefListingQuery{
			Order: gitlet.RefOrderMoved, MovedSince: time.Unix(0, 0)}}, "ref_listing.moved_since"},
		"a find past a branch's name": {gitlet.ReadQuery{Kind: gitlet.ReadRefs, RefListing: &gitlet.RefListingQuery{
			Contains: strings.Repeat("x", 245)}}, "ref_listing.contains"},
		"a find that is not UTF-8": {gitlet.ReadQuery{Kind: gitlet.ReadRefs,
			RefListing: &gitlet.RefListingQuery{Contains: "fix\xff"}}, "ref_listing.contains"},
		"a count past a first page": {gitlet.ReadQuery{Kind: gitlet.ReadRefs, Cursor: "refs/heads/a",
			RefListing: &gitlet.RefListingQuery{ActiveSince: day}}, "ref_listing.active_since"},
		"a count of the tags": {gitlet.ReadQuery{Kind: gitlet.ReadRefs, Prefix: "refs/tags/",
			RefListing: &gitlet.RefListingQuery{ActiveSince: day}}, "ref_listing.active_since"},
	} {
		tc.q.Asker, tc.q.Repo = asker, "app"
		_, err := n.reader.Read(t.Context(), tc.q)
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s: Read = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	for _, cursor := range []string{"refs/heads/main", "12 refs/tags/v1", "soon refs/heads/main"} {
		_, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadRefs, Repo: "app",
			Cursor: cursor, RefListing: &gitlet.RefListingQuery{Order: gitlet.RefOrderMoved}})
		if !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("a moved listing past %q = %v, want ErrBadCursor", cursor, err)
		}
	}
}

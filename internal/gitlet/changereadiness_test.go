package gitlet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// standing is one change a readiness case opens from a branch of its own: who opens it, whether
// plat-lead approves it, its one required check's state ("" for none), whether a blocker stands on
// it, and whether it is a draft or closed once its merge is recorded.
type standing struct {
	name                   string
	owner                  gitlet.Principal
	approved               bool
	check                  gitlet.CheckState
	blocker, draft, closed bool
}

// readinessWorld is a repository named web whose main asks one approval and requires the check
// unit, merged by mergeBy, with one change of each standing opened in order (web!1 first), its
// facts and its merge recorded after its reviews and its check, every record a minute after the
// one before: its id by name.
func readinessWorld(t *testing.T, mergeBy gitlet.MergeBy, changes ...standing) (*node, string,
	map[string]string) {

	t.Helper()
	n := newNode(t)
	n.chart = testChart()
	n.storedAt = func(seq uint64, _ time.Time) time.Time { return harnessAt.Add(time.Duration(seq) * time.Minute) }
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].Gate = &gitlet.MergeGate{MergeBy: mergeBy, Approvals: gitlet.ApprovalsOne,
		Checks: []string{"unit"}, Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteSeats}
	repo := n.ensure("web", s, t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A"))))
	ids := map[string]string{}
	for _, ch := range changes {
		source := "refs/heads/" + ch.name
		pushedBy(t, c, repo, ada, update(source, gitlet.ZeroOID, oidOf(ch.name)))
		p := opening(repo, source, mainRef, ch.owner)
		p.Open.Draft = ch.draft
		opened, err := c.ProposeChange(ctx, p)
		requireChanged(t, opened, err)
		id := opened.ChangeID
		ids[ch.name] = id
		if ch.draft {
			continue
		}
		converse(t, w, n.patched(repo, id, 1, ch.name, oidOf("the diff of "+ch.name)))
		if ch.approved {
			converse(t, w, reviewing(repo, id, platLead, 1, gitlet.VerdictApprove, nil))
		}
		if ch.blocker {
			raiser := cto
			if ch.owner == cto {
				raiser = dev
			}
			converse(t, w, reviewing(repo, id, raiser, 1, gitlet.VerdictComment, nil,
				raising(gitlet.SeverityBlocker, 1, "Retries forever")))
		}
		if ch.check != "" {
			n.checked(repo, oidOf(ch.name), mainRef, "unit", 1, ch.check)
		}
		converse(t, w, n.merging(repo, id, 1, oidOf(ch.name), mainRef, gitlet.MergeClean))
		if ch.closed {
			res, err := c.ProposeChange(ctx, closing(repo, id, dev,
				int64(n.count(`SELECT version FROM code_changes WHERE id = ?`, id))))
			requireChanged(t, res, err)
		}
	}
	return n, repo, ids
}

// everyStanding is one or more open changes in each bucket, a draft and a closed change: ready a1,
// a2, a3; approval b; checks c1 (failed), c2 (running), c3 (failed); blockers d; f a draft, g closed.
func everyStanding() []standing {
	passed, failed := gitlet.CheckStatePassed, gitlet.CheckStateFailed
	return []standing{
		{name: "a1", owner: dev, approved: true, check: passed},
		{name: "b", owner: cto, check: passed},
		{name: "c1", owner: dev, approved: true, check: failed},
		{name: "d", owner: cto, approved: true, check: passed, blocker: true},
		{name: "a2", owner: cto, approved: true, check: passed},
		{name: "c2", owner: cto, approved: true, check: gitlet.CheckStateRunning},
		{name: "f", owner: dev, draft: true},
		{name: "a3", owner: dev, approved: true, check: passed},
		{name: "c3", owner: dev, approved: true, check: failed},
		{name: "g", owner: dev, approved: true, check: passed, closed: true},
	}
}

// readyIndexed counts the changes code_mergeability_ready_idx holds, of ids where any is named.
func readyIndexed(n *node, ids ...string) int {
	n.t.Helper()
	q := `SELECT COUNT(*) FROM code_mergeability INDEXED BY code_mergeability_ready_idx WHERE ready = 1`
	if len(ids) == 0 {
		return n.count(q)
	}
	return n.count(q+` AND change_id IN (SELECT value FROM json_each(?))`, `["`+strings.Join(ids, `","`)+`"]`)
}

// A READINESS STANDS IN ONE BUCKET: the one its first unmet precondition names, else its first
// computing one's, the merger's authority aside. Mutation: drop the merge-authority skip, or read
// computing before unmet.
func TestAReadinessStandsInOneBucket(t *testing.T) {
	t.Parallel()
	type pre = gitlet.Precondition
	met, unmet, computing := gitlet.PreconditionMet, gitlet.PreconditionUnmet, gitlet.PreconditionComputing
	at := func(kind gitlet.PreconditionKind, state gitlet.PreconditionState) pre {
		return pre{Kind: kind, State: state}
	}
	base := func(more ...pre) []pre {
		return append([]pre{at(gitlet.PreconditionDraft, met), at(gitlet.PreconditionFacts, met)}, more...)
	}
	for _, tc := range []struct {
		name string
		pre  []pre
		want gitlet.ReadinessBucket
	}{
		{"every one met", base(at(gitlet.PreconditionApproval, met), at(gitlet.PreconditionCheck, met)),
			gitlet.BucketReady},
		{"the merger's authority alone unmet", base(at(gitlet.PreconditionApproval, met),
			at(gitlet.PreconditionMergeAuthority, unmet)), gitlet.BucketReady},
		{"an approval unmet before a failing check", base(at(gitlet.PreconditionApproval, unmet),
			at(gitlet.PreconditionCheck, unmet)), gitlet.BucketApproval},
		{"facts computing and an approval unmet", []pre{at(gitlet.PreconditionDraft, met),
			at(gitlet.PreconditionFacts, computing), at(gitlet.PreconditionApproval, unmet)}, gitlet.BucketApproval},
		{"a person's approval unmet", base(at(gitlet.PreconditionHumanApproval, unmet)), gitlet.BucketApproval},
		{"the owners' approval unmet", base(at(gitlet.PreconditionOwnersApproval, unmet)), gitlet.BucketApproval},
		{"a blocker unmet before a failing check", base(at(gitlet.PreconditionBlocker, unmet),
			at(gitlet.PreconditionCheck, unmet)), gitlet.BucketBlockers},
		{"a check computing alone", base(at(gitlet.PreconditionCheck, computing)), gitlet.BucketChecks},
		{"a conflict alone", base(at(gitlet.PreconditionMergeable, unmet)), ""},
		{"a queue computing alone", base(at(gitlet.PreconditionQueue, computing)), ""},
		{"a draft", []pre{at(gitlet.PreconditionDraft, unmet), at(gitlet.PreconditionFacts, met)}, ""},
		{"facts computing alone", []pre{at(gitlet.PreconditionDraft, met), at(gitlet.PreconditionFacts, computing)},
			""},
		{"no precondition", nil, ""},
	} {
		if got := (gitlet.Readiness{Preconditions: tc.pre}).Bucket(); got != tc.want {
			t.Errorf("%s: stands in %q, want %q", tc.name, got, tc.want)
		}
	}
	if !slices.Equal(gitlet.ReadinessBuckets(), []gitlet.ReadinessBucket{gitlet.BucketReady, gitlet.BucketApproval,
		gitlet.BucketChecks, gitlet.BucketBlockers}) || gitlet.ReadinessBucket("person").Valid() {
		t.Errorf("the buckets are %v; want ready, approval, checks and blockers, a person's being needs=person",
			gitlet.ReadinessBuckets())
	}
}

// EVERY BUCKET LISTS THE OPEN CHANGES STANDING IN IT, beside every other filter's walk: what that walk
// lists unnarrowed, kept to the bucket, in its order, each row judged in the bucket asked.
// Mutation: drop the bucket's test from listChanges, or make Bucket read any unmet precondition.
func TestEveryBucketListsTheOpenChangesStandingInIt(t *testing.T) {
	t.Parallel()
	n, _, _ := readinessWorld(t, gitlet.MergeByWriters, everyStanding()...)
	buckets := map[gitlet.ReadinessBucket][]string{
		gitlet.BucketReady:    {"web!8", "web!5", "web!1"},
		gitlet.BucketApproval: {"web!2"},
		gitlet.BucketChecks:   {"web!9", "web!6", "web!3"},
		gitlet.BucketBlockers: {"web!4"},
	}
	for _, walk := range []struct {
		name string
		q    gitlet.ReadQuery
	}{
		{"the open changes", gitlet.ReadQuery{}},
		{"a repository's", gitlet.ReadQuery{Repo: "web"}},
		{"an owner's", gitlet.ReadQuery{Owner: "dev", State: gitlet.ChangeStateOpen}},
		{"a week's", gitlet.ReadQuery{Window: gitlet.WindowWeek}},
		{"by last move", gitlet.ReadQuery{ChangeSort: gitlet.ChangeSortUpdated}},
		{"awaiting an owner's fix", gitlet.ReadQuery{Needs: gitlet.NeedFix, Owner: "cto"}},
	} {
		unnarrowed := walk.q
		unnarrowed.Asker = ada
		if unnarrowed.State == "" && unnarrowed.Needs == "" {
			unnarrowed.State = gitlet.ChangeStateOpen
		}
		all, _ := listChanges(t, n, unnarrowed, 50)
		var listedAny bool
		for bucket, in := range buckets {
			var want []string
			for _, ref := range all {
				if slices.Contains(in, ref) {
					want = append(want, ref)
				}
			}
			for _, limit := range []int{1, 50} {
				q := walk.q
				q.Asker, q.Readiness = ada, bucket
				got, _ := listChanges(t, n, q, limit)
				if !slices.Equal(got, want) {
					t.Errorf("%s narrowed to %s, %d a page: listed %v, want %v", walk.name, bucket, limit, got, want)
				}
				listedAny = listedAny || len(got) > 0
			}
			q := walk.q
			q.Kind, q.Asker, q.Readiness, q.Limit, q.Fresh = gitlet.ReadChanges, ada, bucket, 50, stale
			page, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("%s narrowed to %s: %v", walk.name, bucket, err)
			}
			for _, s := range page.Changes {
				if s.Readiness == nil {
					t.Errorf("%s narrowed to %s listed %s, which has no readiness", walk.name, bucket, s.Ref)
				} else if got := s.Readiness.Bucket(); got != bucket {
					t.Errorf("%s narrowed to %s listed %s, whose readiness stands in %q", walk.name, bucket, s.Ref, got)
				}
			}
		}
		if !listedAny {
			t.Errorf("%s listed nothing in any bucket: the case narrows nothing", walk.name)
		}
	}
}

// A READY CHANGE IS LISTED FROM ITS STORED ANSWER, JUDGED AGAIN: a blocker raised since leaves it out
// of ready at once and lists it under blockers; one approved since is listed once a refresh stores it.
// Mutation: list the ready index's rows without judging them.
func TestAReadyChangeIsListedFromItsStoredAnswerJudgedAgain(t *testing.T) {
	t.Parallel()
	passed := gitlet.CheckStatePassed
	n, repo, ids := readinessWorld(t, gitlet.MergeByWriters,
		standing{name: "a", owner: dev, approved: true, check: passed},
		standing{name: "b", owner: dev, check: passed})
	w, _ := writerOn(t, n, n.pub)
	list := func(bucket gitlet.ReadinessBucket) []string {
		t.Helper()
		got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: ada, Readiness: bucket}, 0)
		return got
	}
	refresh := func(change, head string) {
		t.Helper()
		converse(t, w, gitlet.ConversationWrite{OpID: statelog.NewOpID(harnessAt, ""),
			Mergeability: &gitlet.Mergeability{RepoID: repo, ChangeID: change, Revision: 1, HeadOID: oidOf(head),
				TargetRef: mainRef, Refresh: true, Node: n.id}})
	}
	if got := list(gitlet.BucketReady); !slices.Equal(got, []string{"web!1"}) {
		t.Fatalf("ready lists %v, want web!1, stored ready", got)
	}

	converse(t, w, reviewing(repo, ids["a"], cto, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityBlocker, 1, "Retries forever")))
	if !n.storedReadiness(ids["a"]).ready {
		t.Fatal("the review alone moved the stored answer: the case reads nothing stale")
	}
	if got := list(gitlet.BucketReady); len(got) != 0 {
		t.Errorf("ready lists %v after a blocker was raised on web!1, want nothing", got)
	}
	if got := list(gitlet.BucketBlockers); !slices.Equal(got, []string{"web!1"}) {
		t.Errorf("blockers lists %v, want web!1", got)
	}
	refresh(ids["a"], "a")
	if held := readyIndexed(n, ids["a"]); held != 0 {
		t.Errorf("code_mergeability_ready_idx holds web!1 %d times after its refresh, want none", held)
	}

	converse(t, w, reviewing(repo, ids["b"], platLead, 1, gitlet.VerdictApprove, nil))
	if got := list(gitlet.BucketReady); len(got) != 0 {
		t.Errorf("ready lists %v before web!2's home refreshed it, want nothing: its stored answer is not ready",
			got)
	}
	refresh(ids["b"], "b")
	if got := list(gitlet.BucketReady); !slices.Equal(got, []string{"web!2"}) {
		t.Errorf("ready lists %v once web!2 is refreshed, want web!2", got)
	}
}

// AN ENDED CHANGE LEAVES THE READY INDEX in the record that ends it, and one whose merge is recorded
// after its close stores no answer. Mutation: clear waits_person alone in clearReadinessSQL.
func TestAnEndedChangeLeavesTheReadyIndex(t *testing.T) {
	t.Parallel()
	passed := gitlet.CheckStatePassed
	n, repo, ids := readinessWorld(t, gitlet.MergeByWriters,
		standing{name: "a", owner: dev, approved: true, check: passed},
		standing{name: "b", owner: dev, approved: true, check: passed})
	if held := readyIndexed(n); held != 2 {
		t.Fatalf("code_mergeability_ready_idx holds %d changes, want both", held)
	}
	c, _ := committerOn(t, n, n.pub)
	res, err := c.ProposeChange(t.Context(), closing(repo, ids["a"], dev,
		int64(n.count(`SELECT version FROM code_changes WHERE id = ?`, ids["a"]))))
	requireChanged(t, res, err)
	if held := readyIndexed(n, ids["a"]); held != 0 {
		t.Errorf("the closed change is still in code_mergeability_ready_idx")
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("A"), oidOf("b"))))
	if merged := n.count(`SELECT COUNT(*) FROM code_changes WHERE id = ? AND state = 'merged'`, ids["b"]); merged != 1 {
		t.Fatal("the push merged nothing: the case ended nothing")
	}
	if held := readyIndexed(n); held != 0 {
		t.Errorf("code_mergeability_ready_idx holds %d changes, want none: both have ended", held)
	}
	if got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: ada, Readiness: gitlet.BucketReady}, 0); len(got) != 0 {
		t.Errorf("ready lists %v, want nothing", got)
	}

	late := *n.merging(repo, ids["a"], 1, oidOf("a"), mainRef, gitlet.MergeClean).Mergeability
	late.ChangeVersion, late.ReadinessVersion, late.Ready = 2, 3, true
	if _, err := n.publish(gitlet.ChangeSubject(ids["a"]), gitlet.OpMergeability,
		gitlet.Scope{gitlet.RepoPath(repo), gitlet.ChangePath(ids["a"])}, late); err != nil {
		t.Fatalf("publish the late mergeability: %v", err)
	}
	if stored := n.storedReadiness(ids["a"]); stored.ready || readyIndexed(n) != 0 {
		t.Errorf("a mergeability recorded after the close stored %+v, want ready for nobody", stored)
	}
}

// THE READY BUCKET IS THE SAME WHOEVER ASKS: a seat that may not merge into main lists the ready change
// a person does, its row saying the merge waits on somebody else. Mutation: drop the skip in Bucket.
func TestTheReadyBucketIsTheSameWhoeverAsks(t *testing.T) {
	t.Parallel()
	n, _, _ := readinessWorld(t, gitlet.MergeByMaintainers,
		standing{name: "a", owner: dev, approved: true, check: gitlet.CheckStatePassed})
	for _, who := range []gitlet.Principal{ada, dev} {
		if got, _ := listChanges(t, n, gitlet.ReadQuery{Asker: who, Readiness: gitlet.BucketReady}, 0); !slices.Equal(got,
			[]string{"web!1"}) {
			t.Errorf("ready read by %+v lists %v, want web!1", who, got)
		}
	}
	page, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: dev,
		Readiness: gitlet.BucketReady, Fresh: stale})
	if err != nil || len(page.Changes) != 1 || page.Changes[0].Readiness == nil {
		t.Fatalf("ready read by dev: %+v, %v", page.Changes, err)
	}
	requireState(t, *page.Changes[0].Readiness, gitlet.PreconditionMergeAuthority, gitlet.PreconditionUnmet,
		"maintainers")
}

// A READINESS IS REFUSED WHERE IT CANNOT NARROW, naming its field: a bucket this build does not know,
// or a state no bucket holds; any bucket alone, by last move or by opening is read.
// Mutation: drop readying from validateChanges' walk calls.
func TestAReadinessIsRefusedWhereItCannotNarrow(t *testing.T) {
	t.Parallel()
	n, _, _ := readinessWorld(t, gitlet.MergeByWriters)
	read := func(q gitlet.ReadQuery) error {
		q.Kind, q.Asker, q.Fresh = gitlet.ReadChanges, ada, stale
		_, err := n.reader.Read(t.Context(), q)
		return err
	}
	if err := read(gitlet.ReadQuery{Readiness: "sideways"}); !errors.Is(err, gitlet.ErrInvalid) ||
		!strings.Contains(err.Error(), "readiness:") {
		t.Errorf("an unknown bucket = %v, want ErrInvalid naming readiness", err)
	}
	for _, state := range []gitlet.ChangeState{gitlet.ChangeStateDraft, gitlet.ChangeStateClosed,
		gitlet.ChangeStateMerged} {
		if err := read(gitlet.ReadQuery{Readiness: gitlet.BucketChecks, State: state}); !errors.Is(err,
			gitlet.ErrInvalid) || !strings.Contains(err.Error(), "state:") {
			t.Errorf("checks beside state %s = %v, want ErrInvalid naming state", state, err)
		}
	}
	for _, bucket := range gitlet.ReadinessBuckets() {
		for _, sort := range []gitlet.ChangeSort{"", gitlet.ChangeSortUpdated, gitlet.ChangeSortOpened} {
			for _, state := range []gitlet.ChangeState{"", gitlet.ChangeStateOpen} {
				if err := read(gitlet.ReadQuery{Readiness: bucket, ChangeSort: sort, State: state}); err != nil {
					t.Errorf("%s by %q in state %q = %v, want it read", bucket, sort, state, err)
				}
			}
		}
	}
}

// A KEPT PREFIX OF A BUCKET'S PAGE RESUMES WHERE IT STOPPED, for the ready walk, the open walk and the
// walk by last move. Mutation: drop readying from ChangesCursor.
func TestAKeptPrefixOfABucketsPageResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	n, _, _ := readinessWorld(t, gitlet.MergeByWriters, everyStanding()...)
	refs := func(cs []gitlet.ChangeSummary) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Ref)
		}
		return out
	}
	for _, q := range []gitlet.ReadQuery{
		{Readiness: gitlet.BucketReady},
		{Readiness: gitlet.BucketChecks},
		{Readiness: gitlet.BucketChecks, ChangeSort: gitlet.ChangeSortUpdated},
	} {
		q.Kind, q.Asker, q.Limit, q.Fresh = gitlet.ReadChanges, ada, 50, stale
		page, err := n.reader.Read(t.Context(), q)
		if err != nil || len(page.Changes) != 3 {
			t.Fatalf("%s by %q: %v, %v; want three changes", q.Readiness, q.ChangeSort, refs(page.Changes), err)
		}
		q.Cursor = q.ChangesCursor(page.Changes[1])
		rest, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("%s by %q, resumed after %s: %v", q.Readiness, q.ChangeSort, page.Changes[1].Ref, err)
		}
		if got := refs(rest.Changes); len(got) == 0 || got[0] != page.Changes[2].Ref {
			t.Errorf("%s by %q, resumed after %s: listed %v, want it to begin %s", q.Readiness, q.ChangeSort,
				page.Changes[1].Ref, got, page.Changes[2].Ref)
		}
	}
}

// A NARROWED PAGE SAYS WHAT IT WAS NARROWED TO, every one, an empty page with a cursor and both halves
// of what waits on the asker included; an unnarrowed one says nothing. Mutation: set it only on a page
// that lists a row.
func TestANarrowedPageSaysWhatItWasNarrowedTo(t *testing.T) {
	t.Parallel()
	n, _, _ := readinessWorld(t, gitlet.MergeByWriters, everyStanding()...)
	pages := func(q gitlet.ReadQuery) []gitlet.ReadAnswer {
		t.Helper()
		q.Kind, q.Limit, q.Fresh = gitlet.ReadChanges, 1, stale
		var out []gitlet.ReadAnswer
		for range 100 {
			page, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(changes) as %+v: %v", q, err)
			}
			out = append(out, page)
			if page.Next == "" {
				return out
			}
			q.Cursor = page.Next
		}
		t.Fatalf("the listing %+v did not end in a hundred pages", q)
		return nil
	}
	for _, tc := range []struct {
		q          gitlet.ReadQuery
		emptyPages bool
	}{
		{gitlet.ReadQuery{Asker: ada, Readiness: gitlet.BucketBlockers}, true},
		{gitlet.ReadQuery{Asker: ada, Readiness: gitlet.BucketReady, Repo: "web"}, false},
		{gitlet.ReadQuery{Asker: cto, Readiness: gitlet.BucketChecks, Needs: gitlet.NeedMe}, true},
	} {
		var empty bool
		for i, page := range pages(tc.q) {
			if page.NarrowedTo != tc.q.Readiness {
				t.Errorf("%s, needs %q: page %d says it was narrowed to %q", tc.q.Readiness, tc.q.Needs, i+1,
					page.NarrowedTo)
			}
			empty = empty || (len(page.Changes) == 0 && page.Next != "")
		}
		if tc.emptyPages && !empty {
			t.Errorf("%s, needs %q: no page was empty with a cursor, so the case holds no such page",
				tc.q.Readiness, tc.q.Needs)
		}
	}
	for i, page := range pages(gitlet.ReadQuery{Asker: ada, State: gitlet.ChangeStateOpen}) {
		if page.NarrowedTo != "" {
			t.Errorf("unnarrowed page %d says it was narrowed to %q", i+1, page.NarrowedTo)
		}
	}
}

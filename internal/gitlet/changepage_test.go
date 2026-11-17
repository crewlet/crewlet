package gitlet_test

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// changeQuery is a stale `change` read of web!1 as cto, set filling in the rest.
func changeQuery(set func(q *gitlet.ReadQuery)) gitlet.ReadQuery {
	q := gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "web", Change: 1, Fresh: stale}
	set(&q)
	return q
}

// pagesOf is every page q reads, each after the cursor the one before named.
func pagesOf(t *testing.T, n *node, q gitlet.ReadQuery) []gitlet.ReadAnswer {
	t.Helper()
	var out []gitlet.ReadAnswer
	for pages := 1; ; pages++ {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("read %+v: %v", q, err)
		}
		out = append(out, answer)
		if answer.Next == "" {
			return out
		}
		if pages > 50 {
			t.Fatalf("a few rows took more than 50 pages of %+v", q)
		}
		q.Cursor = answer.Next
	}
}

// A DETAIL NAMES ITS PARTICIPANTS, BOUNDED WITH A COUNT OF THE REST: by handle with their
// kinds, the first MaxDetailParticipants and how many more, on the first page alone.
//
// Mutation: drop the LIMIT, count without the bound, or read them on a brief read.
func TestADetailNamesItsParticipantsBoundedWithACountOfTheRest(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)

	detail := func(number int64) *gitlet.ParticipantList {
		t.Helper()
		answer, err := readChange(t, n, cto, "", "web", number, 0, "")
		if err != nil || answer.Change == nil {
			t.Fatalf("read web!%d = %+v, %v; want its detail", number, answer.Change, err)
		}
		return answer.Change.Participants
	}
	if got := detail(1); got == nil || got.Rows == nil || len(got.Rows) != 0 || got.Omitted != 0 {
		t.Fatalf("a change nobody discussed names its participants %+v, want an empty list", got)
	}
	if encoded, err := json.Marshal(detail(1)); err != nil || string(encoded) != `{"rows":[]}` {
		t.Errorf("an empty list of participants encodes as %s (%v), want rows []", encoded, err)
	}

	converse(t, w, asking(repo, change, dev, "plat-lead", "cto"))
	converse(t, w, conversing(vpSales, func(cw *gitlet.ConversationWrite) {
		cw.Comment = &gitlet.Comment{RepoID: repo, ChangeID: change, Body: "Over to @founder.",
			Mentions: []string{"founder"}}
	}))
	converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil))
	want := []gitlet.ParticipantRow{
		{Handle: "cto", Kind: gitlet.PartyAgent},
		{Handle: "founder", Kind: gitlet.PartyPerson},
		{Handle: "plat-lead", Kind: gitlet.PartyAgent, Verdict: gitlet.VerdictApprove, VerdictRevision: 1},
		{Handle: "vp-sales", Kind: gitlet.PartyAgent},
	}
	if got := detail(1); got == nil || !slices.Equal(got.Rows, want) || got.Omitted != 0 {
		t.Fatalf("web!1's participants are %+v, want %+v and none omitted", got, want)
	}

	// PAST THE BOUND: the first by handle, and how many more.
	other := changeRepo(t, n, "api")
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(other, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	n.seedParticipants(opened.ChangeID, gitlet.MaxDetailParticipants+3)
	answer, err := readChange(t, n, cto, "", "api", 1, 0, "")
	if err != nil || answer.Change == nil || answer.Change.Participants == nil {
		t.Fatalf("read api!1 = %+v, %v; want its participants", answer.Change, err)
	}
	got := answer.Change.Participants
	if len(got.Rows) != gitlet.MaxDetailParticipants || got.Omitted != 3 ||
		got.Rows[0].Handle != "participant-00000" ||
		got.Rows[len(got.Rows)-1].Handle != "participant-00031" {
		t.Fatalf("api!1 names %d participants from %+v, %d omitted; want the first %d by handle and 3 more",
			len(got.Rows), got.Rows[0], got.Omitted, gitlet.MaxDetailParticipants)
	}

	// NOT ON A BRIEF READ, NOR ON A LATER PAGE.
	brief, err := n.reader.Read(t.Context(), changeQuery(func(q *gitlet.ReadQuery) { q.Brief = true }))
	if err != nil || brief.Change != nil {
		t.Errorf("a brief read of web!1 answers the detail %+v (%v), want none", brief.Change, err)
	}
	n.revise(repo, "B", "B2")
	first, err := readChange(t, n, cto, "", "web", 1, 1, "")
	if err != nil || first.Next == "" || first.Change == nil || first.Change.Participants == nil {
		t.Fatalf("a first page of one revision of web!1 = %+v, %v; want the detail and a next page", first, err)
	}
	later, err := readChange(t, n, cto, "", "web", 1, 1, first.Next)
	if err != nil || (later.Change != nil && later.Change.Participants != nil) {
		t.Errorf("a later page of web!1's revisions answers participants %+v (%v), want none", later.Change, err)
	}
}

// seedParticipants writes count participants of change straight into n's
// estate, more than the chart holds seats for.
func (n *node) seedParticipants(change string, count int) {
	n.t.Helper()
	for i := range count {
		n.exec(`INSERT INTO code_participants (change_id, handle, kind, position) VALUES (?, ?, 'agent', 1)`,
			change, fmt.Sprintf("participant-%05d", i))
	}
}

// A FINDING'S TIME IS ITS REVIEW'S on every read that answers findings.
//
// Mutation: join the change's latest review, or leave the after-cursor
// statement unjoined.
func TestAFindingsTimeIsItsReviews(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	storedBefore(n, 2*time.Hour)
	first := reviewedBy(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		raising(gitlet.SeverityBlocker, 3, "No backoff"), raising(gitlet.SeverityMajor, 7, "No jitter")))
	storedBefore(n, time.Hour)
	second := reviewedBy(t, w, reviewing(repo, change, cto, 1, gitlet.VerdictComment, nil,
		raising(gitlet.SeverityNit, 9, "A name")))

	at := map[string]time.Time{}
	for _, page := range pagesOf(t, n, changeQuery(func(q *gitlet.ReadQuery) { q.Part = gitlet.PartReviews })) {
		for _, rv := range page.Reviews {
			at[rv.ID] = rv.At
		}
	}
	if len(at) != 2 || at[first.id].Equal(at[second.id]) {
		t.Fatalf("the two reviews landed at %v, want two instants", at)
	}
	check := func(how string, findings []gitlet.FindingRow, want int) {
		t.Helper()
		if len(findings) != want {
			t.Errorf("%s: %d findings, want %d", how, len(findings), want)
		}
		for _, f := range findings {
			if f.RaisedAt.IsZero() || !f.RaisedAt.Equal(at[f.ReviewID]) {
				t.Errorf("%s: finding %s of review %s was raised at %v, want its review's %v", how, f.ID,
					f.ReviewID, f.RaisedAt, at[f.ReviewID])
			}
		}
	}
	var paged []gitlet.FindingRow
	pages := pagesOf(t, n, changeQuery(func(q *gitlet.ReadQuery) { q.Part, q.Limit = gitlet.PartFindings, 1 }))
	for _, page := range pages {
		paged = append(paged, page.Findings...)
	}
	check("the first page", pages[0].Findings, 1)
	check("every page after a cursor", paged, 3)
	named, err := n.reader.Read(t.Context(), changeQuery(func(q *gitlet.ReadQuery) {
		q.Part, q.FindingIDs = gitlet.PartFindings, []string{first.findings[1], second.findings[0]}
	}))
	if err != nil {
		t.Fatalf("read the findings named: %v", err)
	}
	check("the findings named", named.Findings, 2)
	standing, err := n.reader.Read(t.Context(), changeQuery(func(q *gitlet.ReadQuery) {
		q.Asker, q.Part = platLead, gitlet.PartStanding
	}))
	if err != nil {
		t.Fatalf("read plat-lead's standing findings: %v", err)
	}
	check("the standing part", standing.Findings, 2)
}

// reviewed is a review's id and the findings it raised.
type reviewed struct {
	id       string
	findings []string
}

// reviewedBy fails t unless w applies the review cw, and answers it.
func reviewedBy(t *testing.T, w *gitlet.ChangeWriter, cw gitlet.ConversationWrite) reviewed {
	t.Helper()
	res, err := w.Converse(t.Context(), cw)
	findings := requireReviewed(t, res, err)
	return reviewed{id: res.ID, findings: findings}
}

// AN ASK'S TIME IS ITS NOTICE'S WHILE THE NOTICE IS KEPT: a reviewer row's at is the
// instant of the record that last moved it, and none once that record left no notice.
//
// Mutation: read the first notice of the change, or leave at unset after a re-ask.
func TestAnAsksTimeIsItsNoticesWhileTheNoticeIsKept(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, 3*time.Hour)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	reviewers := func() map[string]gitlet.ReviewerRow {
		t.Helper()
		out := map[string]gitlet.ReviewerRow{}
		for _, page := range pagesOf(t, n, changeQuery(func(q *gitlet.ReadQuery) { q.Part = gitlet.PartReviewers })) {
			for _, rv := range page.Reviewers {
				out[rv.Reviewer] = rv
			}
		}
		return out
	}
	routedAt := func(p statelog.Position) time.Time {
		t.Helper()
		for _, page := range pagesOf(t, n, gitlet.ReadQuery{Asker: asker, Kind: gitlet.ReadChangeRouting,
			Repo: "web", Change: 1}) {
			for _, r := range page.Routed.Records {
				if r.Position == p {
					return r.At
				}
			}
		}
		t.Fatalf("web!1's routing holds no record at %v", p)
		return time.Time{}
	}

	// A SEAT ASKED PAST THE SEATS' HORIZON: no notice, no instant.
	storedBefore(n, 40*24*time.Hour)
	old := converse(t, w, asking(repo, change, dev, "plat-lead"))
	if rv := reviewers()["plat-lead"]; rv.Position != old.Position || !rv.At.IsZero() {
		t.Fatalf("plat-lead asked 40 days ago reads at %v (position %v), want none at %v", rv.At, rv.Position,
			old.Position)
	}

	// ASKED AGAIN: the second ask's record and its instant.
	storedBefore(n, 2*time.Hour)
	converse(t, w, withdrawing(repo, change, dev, "plat-lead"))
	storedBefore(n, time.Hour)
	again := converse(t, w, asking(repo, change, dev, "plat-lead"))
	rv := reviewers()["plat-lead"]
	if rv.Position != again.Position || rv.State != gitlet.ReviewerRequested || rv.At.IsZero() ||
		!rv.At.Equal(routedAt(again.Position)) || !rv.At.Equal(harnessAt.Add(-time.Hour)) {
		t.Fatalf("plat-lead asked again reads %+v, want the second ask at %v", rv, again.Position)
	}

	// ANSWERED BY A REVIEW: the review's instant.
	storedBefore(n, 30*time.Minute)
	review := converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil))
	rv = reviewers()["plat-lead"]
	if rv.State != gitlet.ReviewerReviewed || rv.Position != review.Position ||
		!rv.At.Equal(routedAt(review.Position)) || !rv.At.Equal(harnessAt.Add(-30*time.Minute)) {
		t.Fatalf("plat-lead's review reads %+v, want the review's record at %v", rv, review.Position)
	}

	// ONE ASK OF THREE, one instant for all.
	other := changeRepo(t, n, "api")
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(other, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	storedBefore(n, 10*time.Minute)
	three := converse(t, w, asking(other, opened.ChangeID, dev, "cto", "founder", "plat-lead"))
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "api",
		Change: 1, Part: gitlet.PartReviewers, Fresh: stale})
	if err != nil || len(answer.Reviewers) != 3 {
		t.Fatalf("read api!1's reviewers = %+v, %v; want three", answer.Reviewers, err)
	}
	for _, rv := range answer.Reviewers {
		if rv.Position != three.Position || !rv.At.Equal(harnessAt.Add(-10*time.Minute)) {
			t.Errorf("%s asked in one ask of three reads %+v, want the ask's record at %v", rv.Reviewer, rv,
				harnessAt.Add(-10*time.Minute))
		}
	}
}

// A PAGE OF REVIEWERS IS CUT WITH ITS INSTANTS AT THEIR WIDEST, since they are read
// after the cut: a budget two such rows fill ends the page at two.
//
// Mutation: cost the row without the widest instant, and the page takes a third row.
func TestAPageOfReviewersIsCutWithItsInstantsAtTheirWidest(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	converse(t, w, asking(repo, change, dev, "cto", "founder", "plat-lead"))
	q := changeQuery(func(q *gitlet.ReadQuery) { q.Part = gitlet.PartReviewers })
	whole, err := n.reader.Read(t.Context(), q)
	if err != nil || len(whole.Reviewers) != 3 {
		t.Fatalf("read web!1's reviewers = %+v, %v; want three", whole.Reviewers, err)
	}
	widest := func(rv gitlet.ReviewerRow) int {
		rv.At = time.Date(9999, 12, 31, 23, 59, 59, 999_999_999, time.UTC)
		encoded, err := json.Marshal(rv)
		if err != nil {
			t.Fatal(err)
		}
		return len(encoded)
	}
	budget := widest(whole.Reviewers[0]) + widest(whole.Reviewers[1])
	gitlet.SetPageLimits(n.reader, 1000, budget)
	first, err := n.reader.Read(t.Context(), q)
	if err != nil || len(first.Reviewers) != 2 || first.Next != whole.Reviewers[1].Reviewer {
		t.Fatalf("a page of %d bytes holds %+v next %q (%v), want the first two and a cursor after them", budget,
			first.Reviewers, first.Next, err)
	}
	if encoded, err := json.Marshal(first.Reviewers); err != nil || len(encoded) > budget {
		t.Errorf("the page's rows encode in %d bytes, past its budget of %d", len(encoded), budget)
	}
	q.Cursor = first.Next
	rest, err := n.reader.Read(t.Context(), q)
	if err != nil || len(rest.Reviewers) != 1 || rest.Reviewers[0].Reviewer != whole.Reviewers[2].Reviewer ||
		rest.Reviewers[0].At.IsZero() {
		t.Fatalf("the next page holds %+v (%v), want the third reviewer with its instant", rest.Reviewers, err)
	}
}

// pathGroup is one group of findings by path, state and revision.
type pathGroup struct {
	path     string
	state    gitlet.FindingState
	revision int64
}

// on is f raised on path.
func on(path string, f gitlet.Finding) gitlet.Finding {
	f.Path = path
	return f
}

// FINDINGS BY PATH ARE THE FINDINGS GROUPED by path, state and revision, paged through
// cursors that hold a slash, and read as a part: a reader with no chart answers them.
//
// Mutation: group by path alone, resume at the cursor's group, or judge readiness on them.
func TestFindingPathsAreTheFindingsGrouped(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	first := reviewedBy(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictRequestChanges, nil,
		on("a.go", raising(gitlet.SeverityBlocker, 1, "No backoff")),
		on("dir/b.go", raising(gitlet.SeverityMajor, 2, "No jitter")),
		on("dir/b.go", raising(gitlet.SeverityMinor, 3, "A long line")),
		on("c/d/e.go", raising(gitlet.SeverityNit, 4, "A name"))))
	others := reviewedBy(t, w, reviewing(repo, change, cto, 1, gitlet.VerdictComment, nil,
		on("a.go", raising(gitlet.SeverityNit, 5, "A typo")),
		on("z.go", raising(gitlet.SeverityMinor, 6, "A loop"))))
	converse(t, w, answering(repo, change, dev, gitlet.FindingResolved, others.findings[1]))
	converse(t, w, answering(repo, change, platLead, gitlet.FindingDismissed, first.findings[2]))
	n.revise(repo, "B", "B2")
	reviewedBy(t, w, reviewing(repo, change, platLead, 2, gitlet.VerdictRequestChanges,
		[]string{first.findings[0]}, on("dir/b.go", raising(gitlet.SeverityMajor, 8, "Still no jitter"))))

	counted := map[pathGroup]int64{}
	for _, page := range pagesOf(t, n, changeQuery(func(q *gitlet.ReadQuery) { q.Part = gitlet.PartFindings })) {
		for _, f := range page.Findings {
			counted[pathGroup{f.Path, f.State, f.Revision}]++
		}
	}
	var want []gitlet.FindingPathRow
	for g, count := range counted {
		want = append(want, gitlet.FindingPathRow{Path: g.path, State: g.state, Revision: g.revision, Findings: count})
	}
	slices.SortFunc(want, func(a, b gitlet.FindingPathRow) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.State, b.State), cmp.Compare(a.Revision, b.Revision))
	})
	states := map[gitlet.FindingState]bool{}
	for _, g := range want {
		states[g.State] = true
	}
	if len(want) < 6 || len(states) != 4 {
		t.Fatalf("the findings group as %+v, want open, resolved, dismissed and superseded groups", want)
	}

	byPath := func(limit int) ([]gitlet.FindingPathRow, []string) {
		t.Helper()
		var rows []gitlet.FindingPathRow
		var cursors []string
		for _, page := range pagesOf(t, n, changeQuery(func(q *gitlet.ReadQuery) {
			q.FindingPaths, q.Limit = true, limit
		})) {
			if page.Change != nil || len(page.Findings) > 0 || len(page.Revisions) > 0 {
				t.Fatalf("a page of findings by path carries more than its groups: %+v", page)
			}
			rows = append(rows, page.FindingPaths...)
			if page.Next != "" {
				cursors = append(cursors, page.Next)
			}
		}
		return rows, cursors
	}
	if got, cursors := byPath(0); !slices.Equal(got, want) || len(cursors) != 0 {
		t.Fatalf("web!1's findings by path are %+v (cursors %v), want %+v on one page", got, cursors, want)
	}
	chartless, err := gitlet.NewReader(gitlet.ReaderOptions{Log: n.authority})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// A first page of groups is a part: no readiness is judged, so no chart is needed.
	part, err := chartless.Read(t.Context(), gitlet.ReadQuery{Asker: ada, Kind: gitlet.ReadChange, Repo: "web",
		Change: 1, FindingPaths: true, Fresh: stale})
	if err != nil || !slices.Equal(part.FindingPaths, want) {
		t.Fatalf("a chartless reader's findings by path = %+v, %v; want %+v", part.FindingPaths, err, want)
	}
	got, cursors := byPath(2)
	if !slices.Equal(got, want) {
		t.Fatalf("web!1's findings by path two at a time are %+v, want %+v", got, want)
	}
	slashed := false
	for k, cursor := range cursors {
		g := want[2*k+1]
		if cursor != strconv.FormatInt(g.Revision, 10)+"/"+string(g.State)+"/"+g.Path {
			t.Errorf("page %d's cursor is %q, want the revision, state and path of %+v", k+1, cursor, g)
		}
		slashed = slashed || strings.Contains(g.Path, "/")
	}
	if !slashed {
		t.Errorf("no cursor of %v names a path holding a slash", cursors)
	}

	other := changeRepo(t, n, "api")
	c, _ := committerOn(t, n, n.pub)
	opened, err := c.ProposeChange(t.Context(), opening(other, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	empty, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "api",
		Change: 1, FindingPaths: true, Fresh: stale})
	if err != nil || empty.FindingPaths == nil || len(empty.FindingPaths) != 0 || empty.Next != "" ||
		empty.Change != nil {
		t.Errorf("a change with no finding answers %+v (%v), want an empty page", empty, err)
	}
}

// A FINDING PATHS CURSOR IS ONE THE READ ISSUES: revision, a Valid state and a path.
//
// Mutation: accept a state that is not Valid.
func TestAFindingPathsCursorIsOneTheReadIssues(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	discussed(t, n, "web")
	for _, cursor := range []string{"0/open/a", "1/opened/a", "1/open/", "x/open/a", "1", "1/open"} {
		if _, err := n.reader.Read(t.Context(), changeQuery(func(q *gitlet.ReadQuery) {
			q.FindingPaths, q.Cursor = true, cursor
		})); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("finding_paths after %q = %v, want ErrBadCursor", cursor, err)
		}
	}
	if _, err := n.reader.Read(t.Context(), changeQuery(func(q *gitlet.ReadQuery) {
		q.FindingPaths, q.Cursor = true, "2/superseded/dir/b.go"
	})); err != nil {
		t.Errorf("finding_paths after a group it could issue = %v, want a page", err)
	}
}

// FINDING PATHS ARE PAGED APART FROM THE CHANGE'S PARTS: the key beside another key
// of the read, or past MaxFindingPathsPerPage, is refused.
//
// Mutation: let a part ride beside it.
func TestFindingPathsArePagedApartFromTheChangesParts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	discussed(t, n, "web")
	for name, set := range map[string]func(q *gitlet.ReadQuery){
		"a part":       func(q *gitlet.ReadQuery) { q.Part = gitlet.PartReviewers },
		"detail_only":  func(q *gitlet.ReadQuery) { q.DetailOnly = true },
		"brief":        func(q *gitlet.ReadQuery) { q.Brief = true },
		"finding_ids":  func(q *gitlet.ReadQuery) { q.FindingIDs = []string{"019a0c2e-0000-7000-8000-00000000dead"} },
		"a limit past": func(q *gitlet.ReadQuery) { q.Limit = gitlet.MaxFindingPathsPerPage + 1 },
	} {
		if _, err := n.reader.Read(t.Context(), changeQuery(func(q *gitlet.ReadQuery) {
			q.FindingPaths = true
			set(q)
		})); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("finding_paths beside %s = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := n.reader.Read(t.Context(), changeQuery(func(q *gitlet.ReadQuery) {
		q.FindingPaths, q.Limit = true, gitlet.MaxFindingPathsPerPage
	})); err != nil {
		t.Errorf("finding_paths at MaxFindingPathsPerPage = %v, want a page", err)
	}
}

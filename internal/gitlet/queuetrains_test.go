package gitlet_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/store"
)

// A QUEUE READ OF EVERY REPOSITORY, its trains and ejections asked for: the shipped committer
// decides the queue's records, the shipped applier applies them and the shipped reader answers.

// queuedRepo is a fresh repository named name under owner at visibility, its main a queue
// target ([queueGate]) at A, holding one change into main a title, each from a branch of its
// own, opened by opener, approved by approver and its unit check passed on its head: the
// repository, and each change with its head, in the order of titles.
func queuedRepo(t *testing.T, n *node, c *gitlet.Committer, w *gitlet.ChangeWriter, name string,
	owner gitlet.Owner, v gitlet.Visibility, opener, approver gitlet.Principal,
	titles ...string) (repo string, changes, heads []string) {

	t.Helper()
	s := suiteSettings()
	s.Owner, s.Visibility = owner, v
	s.Protections[0].Gate = queueGate()
	repo = n.ensure(name, s, t1).RepoID
	cmds := []gitlet.Command{set(mainRef, gitlet.ZeroOID, oidOf("A"))}
	for i := range titles {
		cmds = append(cmds, set(fmt.Sprintf("refs/heads/q%d", i), gitlet.ZeroOID, oidOf(fmt.Sprintf("%s-q%d", name, i))))
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), cmds...))
	for i, title := range titles {
		head := oidOf(fmt.Sprintf("%s-q%d", name, i))
		open := opening(repo, fmt.Sprintf("refs/heads/q%d", i), mainRef, opener)
		open.Open.Title = title
		opened, err := c.ProposeChange(t.Context(), open)
		requireChanged(t, opened, err)
		converse(t, w, n.revising(repo, opened.ChangeID, 1, head, mainRef, nil))
		converse(t, w, reviewing(repo, opened.ChangeID, approver, 1, gitlet.VerdictApprove, nil))
		n.passed(repo, head, mainRef, "unit")
		changes, heads = append(changes, opened.ChangeID), append(heads, head)
	}
	return repo, changes, heads
}

// enqueued is ada's enqueue of change at head into main, landing by method, at place: its entry.
func enqueued(t *testing.T, c *gitlet.Committer, repo, change, head string, method gitlet.MergeMethod,
	deleteSource, closeTask bool, place int) string {

	t.Helper()
	p := enqueueing(repo, change, ada, head)
	p.Enqueue.Method, p.Enqueue.DeleteSource, p.Enqueue.CloseTask = method, deleteSource, closeTask
	res, err := c.ProposeQueue(t.Context(), p)
	requireQueued(t, res, err, gitlet.QueueWaiting, place)
	return p.Enqueue.EntryID
}

// ejected is the queue's ejection of repo's entry for a failed e2e, and the instant its row says
// it ended at.
func ejected(t *testing.T, n *node, c *gitlet.Committer, repo, entryID string) time.Time {
	t.Helper()
	res, err := c.ProposeQueue(t.Context(), queueRecord(func(p *gitlet.QueueProposal) {
		p.Eject = &gitlet.Eject{RepoID: repo, Entries: []gitlet.Ejection{{EntryID: entryID,
			Reason: gitlet.EjectCheckFailed, Checks: []string{"e2e"}}}}
	}))
	requireRecorded(t, res, err)
	var ended int64
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT ended_at FROM code_queue WHERE repo_id = ? AND entry_id = ?`,
			repo, entryID).Scan(&ended)
	}); err != nil {
		t.Fatalf("read the ejection's instant: %v", err)
	}
	return store.DecodeTime(ended)
}

// readQueue is a stale queue read of q as asker, failing t on a refusal.
func readQueue(t *testing.T, n *node, asker gitlet.Principal, q gitlet.ReadQuery) gitlet.ReadAnswer {
	t.Helper()
	q.Kind, q.Asker, q.Fresh = gitlet.ReadQueue, asker, stale
	answer, err := n.reader.Read(t.Context(), q)
	if err != nil {
		t.Fatalf("read the queue %+v: %v", q, err)
	}
	return answer
}

// EVERY REPOSITORY'S TRAINS AND EJECTIONS ARE READ AT ONCE: web holds a testing train of two
// candidates and a waiting entry, api a train of one and an entry its queue put out twenty
// minutes ago. A read naming no repository that asks for trains answers every live entry with
// its change's title and source branch, the candidates of both repositories' trains, api's
// ejection with how it was to land, and their total; the same read without trains answers the
// live entries alone, as before.
//
// Mutations: read the trains of the first row's repository for every target (as a named
// repository's read did): api's candidate is missing, red; keep the early return of the read of
// every repository: no trains and no ejections, red; answer them unasked: the plain read
// carries them, red.
func TestEveryRepositorysTrainsAndEjectionsAreReadAtOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	eng := gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	web, webChanges, webHeads := queuedRepo(t, n, c, w, "web", eng, gitlet.VisibilityCompany, dev, platLead,
		"Retry the upload", "Name the queue", "Count the trains")
	api, apiChanges, apiHeads := queuedRepo(t, n, c, w, "api", eng, gitlet.VisibilityCompany, dev, platLead,
		"Drop the cache", "Bound the walk")
	var webEntries []string
	for i, change := range webChanges {
		webEntries = append(webEntries, enqueued(t, c, web, change, webHeads[i], gitlet.MergeMethodMerge, false,
			false, i+1))
	}
	res, err := c.ProposeQueue(t.Context(), trainOf(web, oidOf("A"),
		gitlet.CandidateEntry{EntryID: webEntries[0], Commit: oidOf("web-M1")},
		gitlet.CandidateEntry{EntryID: webEntries[1], Commit: oidOf("web-M2")}))
	requireRecorded(t, res, err)
	out := enqueued(t, c, api, apiChanges[0], apiHeads[0], gitlet.MergeMethodSquash, true, false, 1)
	ejectedAt := ejected(t, n, c, api, out)
	apiEntry := enqueued(t, c, api, apiChanges[1], apiHeads[1], gitlet.MergeMethodMerge, false, false, 1)
	res, err = c.ProposeQueue(t.Context(), training(api, oidOf("A"), apiEntry, oidOf("api-M1")))
	requireRecorded(t, res, err)
	soon := ejectedAt.Add(20 * time.Minute)
	n.readAt.Store(&soon)

	plain := readQueue(t, n, ada, gitlet.ReadQuery{})
	if len(plain.Queue) != 4 || plain.Trains != nil || plain.Ejected != nil || plain.EjectedTotal != nil {
		t.Fatalf("a read of every queue asking for no trains answered %d entries, trains %+v, ejections %+v "+
			"and a total %+v; want the four live entries alone", len(plain.Queue), plain.Trains, plain.Ejected,
			plain.EjectedTotal)
	}
	got := readQueue(t, n, ada, gitlet.ReadQuery{Trains: true})
	type labelled struct {
		repo, title, source string
		place               int
		state               gitlet.QueueState
	}
	var rows []labelled
	for _, r := range got.Queue {
		rows = append(rows, labelled{r.Repo, r.Title, r.SourceRef, r.Place, r.State})
	}
	want := []labelled{
		{"web", "Retry the upload", "refs/heads/q0", 1, gitlet.QueueTesting},
		{"web", "Name the queue", "refs/heads/q1", 2, gitlet.QueueTesting},
		{"web", "Count the trains", "refs/heads/q2", 3, gitlet.QueueWaiting},
		{"api", "Bound the walk", "refs/heads/q1", 1, gitlet.QueueTesting},
	}
	slices.SortFunc(want, func(a, b labelled) int {
		return strings.Compare(map[string]string{"web": web, "api": api}[a.repo],
			map[string]string{"web": web, "api": api}[b.repo])
	})
	if !slices.Equal(rows, want) {
		t.Errorf("the live entries read %+v, want %+v", rows, want)
	}
	if !slices.Equal(plain.Queue, got.Queue) {
		t.Errorf("asking for trains moved the live entries: %+v, want %+v", got.Queue, plain.Queue)
	}
	var trains []string
	for _, cand := range got.Trains {
		trains = append(trains, cand.RepoID+" "+cand.TipOID)
	}
	slices.Sort(trains)
	wantTrains := []string{web + " " + oidOf("web-M1"), web + " " + oidOf("web-M2"), api + " " + oidOf("api-M1")}
	slices.Sort(wantTrains)
	if !slices.Equal(trains, wantTrains) {
		t.Errorf("the trains read %v, want web's two candidates and api's one: %v", trains, wantTrains)
	}
	if len(got.Ejected) != 1 {
		t.Fatalf("the ejections read %+v, want api's one", got.Ejected)
	}
	e := got.Ejected[0]
	if e.Repo != "api" || e.EntryID != out || e.Title != "Drop the cache" || e.SourceRef != "refs/heads/q0" ||
		e.Number != 1 || e.Method != gitlet.MergeMethodSquash || !e.DeleteSource || e.CloseTask ||
		e.Reason != gitlet.EjectCheckFailed || !slices.Equal(e.Checks, []string{"e2e"}) ||
		!e.EjectedAt.Equal(ejectedAt) {
		t.Errorf("api's ejection reads %+v, want api!1 'Drop the cache' from q0, squash deleting its source, "+
			"out for e2e", e)
	}
	if got.EjectedTotal == nil || *got.EjectedTotal != (gitlet.EjectedTotal{Total: 1}) {
		t.Errorf("the ejections' total reads %+v, want 1", got.EjectedTotal)
	}

	page := readQueue(t, n, ada, gitlet.ReadQuery{Trains: true, Limit: 2})
	next := readQueue(t, n, ada, gitlet.ReadQuery{Trains: true, Limit: 2, Cursor: page.Next})
	if page.Next == "" || page.EjectedTotal == nil || len(page.Ejected) != 1 || next.EjectedTotal != nil ||
		next.Ejected != nil || len(next.Trains) == 0 {
		t.Errorf("a first page answered ejections %+v and a total %+v, and the next %+v and %+v with %d "+
			"candidates; want the ejections on the first page alone and trains on both", page.Ejected,
			page.EjectedTotal, next.Ejected, next.EjectedTotal, len(next.Trains))
	}
}

// insertEntry writes one code_queue row of repoID straight into n's estate: the reads under
// test take no record to judge, and a day's ejections by records would take a day.
func (n *node) insertEntry(repoID, target string, seq int64, state gitlet.QueueState, endedAt time.Time) string {
	n.t.Helper()
	entryID := uuid.Must(uuid.NewV7()).String()
	var (
		ended  int64
		reason gitlet.QueueEndReason
	)
	switch state {
	case gitlet.QueueEjected:
		ended, reason = store.EncodeTime(endedAt), gitlet.EjectConflict
	case gitlet.QueueLanded:
		ended, reason = store.EncodeTime(endedAt), gitlet.EndMerged
	}
	n.exec(`INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method, delete_source,
			close_task, enqueued_by, enqueued_kind, enqueued_operator, state, enqueued_at, ended_at, end_reason)
		VALUES (?, ?, ?, ?, ?, ?, 'merge', 0, 0, 'founder', 'person', '', ?, ?, ?, ?)`,
		repoID, entryID, target, "change-of-"+entryID, seq, oidOf(entryID), string(state),
		store.EncodeTime(harnessAt.Add(-48*time.Hour)), ended, string(reason))
	return entryID
}

// A QUEUE'S EJECTIONS ARE BOUNDED AND COUNTED: of MaxEjectionsShown+3 ejections in the last day
// across two repositories, and one older, a read of every repository answers the newest
// MaxEjectionsShown, newest first, and counts every one of the day; a count bound below them
// answers the bound and says there are more, and rows of the ejections it counted alone. A named
// repository asking for trains counts its own, and a target's read its target's.
//
// Mutations: count the rows shown: the total is MaxEjectionsShown, red; drop the horizon from
// the count: the old one is counted, red; never set More: the bounded count says none past it,
// red; count a named repository's every target: the target's total holds release's, red; read
// the rows past the bound back to the day's horizon: web's older ejection is answered, red.
func TestAQueuesEjectionsAreBoundedAndCounted(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	web := n.ensure("web", suiteSettings(), t1).RepoID
	api := n.ensure("api", suiteSettings(), t1).RepoID
	shown := gitlet.MaxEjectionsShown + 3
	for i := range shown {
		repo, target := web, mainRef
		switch {
		case i%2 == 1:
			repo = api
		case i%4 == 0:
			target = releaseRef
		}
		n.insertEntry(repo, target, int64(i+1), gitlet.QueueEjected, harnessAt.Add(-time.Duration(i+1)*time.Minute))
	}
	n.insertEntry(web, mainRef, 1_000, gitlet.QueueEjected, harnessAt.Add(-gitlet.QueueEjectionsShown-time.Minute))

	got := readQueue(t, n, ada, gitlet.ReadQuery{Trains: true})
	if len(got.Ejected) != gitlet.MaxEjectionsShown {
		t.Fatalf("a read of every repository answered %d ejections, want the newest %d", len(got.Ejected),
			gitlet.MaxEjectionsShown)
	}
	newestFirst := func(a, b gitlet.EjectedRow) int {
		return b.EjectedAt.Compare(a.EjectedAt) //fixedwait:stated the rows' recorded instants, no clock's
	}
	if !slices.IsSortedFunc(got.Ejected, newestFirst) ||
		!got.Ejected[0].EjectedAt.Equal(harnessAt.Add(-time.Minute)) {
		t.Errorf("the ejections are not the newest first, from a minute ago: first %s", got.Ejected[0].EjectedAt)
	}
	if got.EjectedTotal == nil || *got.EjectedTotal != (gitlet.EjectedTotal{Total: int64(shown)}) {
		t.Errorf("the total reads %+v, want %d and no more", got.EjectedTotal, shown)
	}
	gitlet.SetQueueCountBound(n.reader, 10)
	if got = readQueue(t, n, ada, gitlet.ReadQuery{Trains: true}); got.EjectedTotal == nil ||
		*got.EjectedTotal != (gitlet.EjectedTotal{Total: 10, More: true}) {
		t.Errorf("the total within a bound of 10 reads %+v, want 10 and more", got.EjectedTotal)
	}
	gitlet.SetQueueCountBound(n.reader, gitlet.MaxQueueCounted)

	var webDay, mainDay int64
	for i := range shown {
		if i%2 == 0 {
			webDay++
			if i%4 != 0 {
				mainDay++
			}
		}
	}
	for _, tc := range []struct {
		q    gitlet.ReadQuery
		want int64
	}{
		{gitlet.ReadQuery{Repo: "web", Trains: true}, webDay},
		{gitlet.ReadQuery{Repo: "web", Target: mainRef, Trains: true}, mainDay},
	} {
		got := readQueue(t, n, ada, tc.q)
		if got.EjectedTotal == nil || *got.EjectedTotal != (gitlet.EjectedTotal{Total: tc.want}) {
			t.Errorf("a read of %+v totals %+v, want %d", tc.q, got.EjectedTotal, tc.want)
		}
	}
	if got := readQueue(t, n, ada, gitlet.ReadQuery{Repo: "web"}); got.EjectedTotal != nil {
		t.Errorf("a read of web asking for no trains totals %+v, want none", got.EjectedTotal)
	}

	// Past the bound, the rows keep to the ejections counted: a bound of 3 counts web's two newest
	// and api's, which dev does not read, so dev is answered web's two, not docs' nor web's older one.
	m := newNode(t)
	m.chart = testChart()
	repoOf := func(name, unit string) string {
		s := suiteSettings()
		s.Owner, s.Visibility = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: unit}, gitlet.VisibilityUnit
		return m.ensure(name, s, t1).RepoID
	}
	webB, docsB, apiB := repoOf("web", "Engineering"), repoOf("docs", "Engineering"), repoOf("api", "Sales")
	newest := m.insertEntry(webB, mainRef, 1, gitlet.QueueEjected, harnessAt.Add(-time.Minute))
	next := m.insertEntry(webB, mainRef, 2, gitlet.QueueEjected, harnessAt.Add(-2*time.Minute))
	m.insertEntry(apiB, mainRef, 1, gitlet.QueueEjected, harnessAt.Add(-3*time.Minute))
	m.insertEntry(docsB, mainRef, 1, gitlet.QueueEjected, harnessAt.Add(-4*time.Minute))
	m.insertEntry(webB, mainRef, 3, gitlet.QueueEjected, harnessAt.Add(-9*time.Minute))
	gitlet.SetQueueCountBound(m.reader, 3)
	bounded := readQueue(t, m, dev, gitlet.ReadQuery{Trains: true})
	var ids []string
	for _, row := range bounded.Ejected {
		ids = append(ids, row.EntryID)
	}
	if !slices.Equal(ids, []string{newest, next}) || bounded.EjectedTotal == nil ||
		*bounded.EjectedTotal != (gitlet.EjectedTotal{Total: 2, More: true}) {
		t.Errorf("dev's read within a bound of 3 answered %v totalling %+v, want web's two newest %v "+
			"totalling 2 and more", ids, bounded.EjectedTotal, []string{newest, next})
	}
}

// A QUEUE READ NAMES CHANGES ONLY WITHIN THE ASKER'S GRANT: dev reads web, Engineering's, and not
// api, which Sales keeps to itself. Dev's read of every repository asking for trains answers web's
// entries, train, ejection and total with their titles and nothing of api's: not even the title
// of api's change, a probe no encoded answer may hold; for a ceiling that reads neither it
// answers nothing; and a read naming api is refused.
//
// Mutations: build a row of every scanned entry, the unlisted repositories' too: api's title is
// answered, red; count every repository's ejections: the total counts api's, red; judge the
// asker alone: the ceiling's read answers web, red.
func TestAQueueReadNamesChangesOnlyWithinTheAskersGrant(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	web, webChanges, webHeads := queuedRepo(t, n, c, w, "web", gitlet.Owner{Kind: gitlet.OwnerUnit,
		Ref: "Engineering"}, gitlet.VisibilityUnit, dev, platLead, "Retry the upload", "Name the queue")
	const probe = "api-secret"
	api, apiChanges, apiHeads := queuedRepo(t, n, c, w, "api", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Sales"},
		gitlet.VisibilityUnit, ada, vpSales, probe, probe+"-too")
	webOut := enqueued(t, c, web, webChanges[0], webHeads[0], gitlet.MergeMethodMerge, false, false, 1)
	ejectedAt := ejected(t, n, c, web, webOut)
	webEntry := enqueued(t, c, web, webChanges[1], webHeads[1], gitlet.MergeMethodMerge, false, false, 1)
	res, err := c.ProposeQueue(t.Context(), training(web, oidOf("A"), webEntry, oidOf("web-M1")))
	requireRecorded(t, res, err)
	apiOut := enqueued(t, c, api, apiChanges[0], apiHeads[0], gitlet.MergeMethodMerge, false, false, 1)
	ejected(t, n, c, api, apiOut)
	apiEntry := enqueued(t, c, api, apiChanges[1], apiHeads[1], gitlet.MergeMethodMerge, false, false, 1)
	res, err = c.ProposeQueue(t.Context(), training(api, oidOf("A"), apiEntry, oidOf("api-M1")))
	requireRecorded(t, res, err)
	soon := ejectedAt.Add(time.Hour)
	n.readAt.Store(&soon)

	got := readQueue(t, n, dev, gitlet.ReadQuery{Trains: true})
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), probe) || strings.Contains(string(encoded), api) {
		t.Errorf("dev's read of every queue names api or its change: %s", encoded)
	}
	if len(got.Queue) != 1 || got.Queue[0].Repo != "web" || got.Queue[0].Title != "Name the queue" ||
		len(got.Trains) != 1 || got.Trains[0].RepoID != web || len(got.Ejected) != 1 ||
		got.Ejected[0].Title != "Retry the upload" || got.EjectedTotal == nil || got.EjectedTotal.Total != 1 {
		t.Errorf("dev's read of every queue answered %s, want web's entry, train and ejection, titled, "+
			"and a total of 1", encoded)
	}
	ceiled := readQueue(t, n, dev, gitlet.ReadQuery{Trains: true, CeilingSeat: "vp-sales"})
	if len(ceiled.Queue) != 0 || len(ceiled.Trains) != 0 || len(ceiled.Ejected) != 0 ||
		ceiled.EjectedTotal == nil || *ceiled.EjectedTotal != (gitlet.EjectedTotal{}) {
		t.Errorf("dev's read for vp-sales, who reads neither, answered %+v", ceiled)
	}
	if _, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadQueue, Asker: dev, Repo: "api",
		Trains: true, Fresh: stale}); !errors.Is(err, gitlet.ErrNotPermitted) {
		t.Errorf("dev's read of api's queue = %v, want ErrNotPermitted", err)
	}
}

// AN EJECTION CARRIES WHAT A RE-QUEUE SENDS: an entry enqueued squash, deleting its source and
// closing its work item, then put out, answers the three as enqueued; one enqueued merge asking
// neither answers merge and neither.
//
// Mutation: answer the ejection's method from its repository's gate, or drop either flag from
// ejectedRow: red.
func TestAnEjectionCarriesWhatAReQueueSends(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	w, _ := writerOn(t, n, n.pub)
	web, changes, heads := queuedRepo(t, n, c, w, "web", gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"},
		gitlet.VisibilityCompany, dev, platLead, "Retry the upload", "Name the queue")
	squashed := enqueued(t, c, web, changes[0], heads[0], gitlet.MergeMethodSquash, true, true, 1)
	at := ejected(t, n, c, web, squashed)
	merged := enqueued(t, c, web, changes[1], heads[1], gitlet.MergeMethodMerge, false, false, 1)
	ejected(t, n, c, web, merged)
	soon := at.Add(time.Hour)
	n.readAt.Store(&soon)

	type sends struct {
		method                  gitlet.MergeMethod
		deleteSource, closeTask bool
	}
	want := map[string]sends{squashed: {gitlet.MergeMethodSquash, true, true},
		merged: {gitlet.MergeMethodMerge, false, false}}
	got := readQueue(t, n, ada, gitlet.ReadQuery{Repo: "web"})
	if len(got.Ejected) != len(want) {
		t.Fatalf("web's ejections read %+v, want two", got.Ejected)
	}
	for _, e := range got.Ejected {
		if s := (sends{e.Method, e.DeleteSource, e.CloseTask}); s != want[e.EntryID] {
			t.Errorf("entry %s's ejection sends %+v, want %+v", e.EntryID, s, want[e.EntryID])
		}
	}
}

// BenchmarkTheQueueReadOfEveryRepository is the first page of a queue read of every repository
// asking for trains, over 50 repositories holding 200 live entries and 100 ejections of the day.
func BenchmarkTheQueueReadOfEveryRepository(b *testing.B) {
	gitlet.Quiet(b)
	n := newNode(b)
	for r := range 50 {
		repo := n.ensure(fmt.Sprintf("repo-%02d", r), suiteSettings(), t1).RepoID
		for i := range 4 {
			n.insertEntry(repo, mainRef, int64(i+1), gitlet.QueueWaiting, time.Time{})
		}
		for i := range 2 {
			n.insertEntry(repo, mainRef, int64(100+i), gitlet.QueueEjected,
				harnessAt.Add(-time.Duration(r*2+i+1)*time.Minute))
		}
	}
	for _, trains := range []bool{false, true} {
		b.Run(fmt.Sprintf("trains=%t", trains), func(b *testing.B) {
			q := gitlet.ReadQuery{Kind: gitlet.ReadQueue, Asker: ada, Trains: trains, Limit: 200, Fresh: stale}
			for b.Loop() {
				got, err := n.reader.Read(b.Context(), q)
				if err != nil || len(got.Queue) != 200 || trains != (len(got.Ejected) == gitlet.MaxEjectionsShown &&
					got.EjectedTotal != nil && got.EjectedTotal.Total == 100) {
					b.Fatalf("the first page = %d entries, %d ejections, total %+v, %v; want 200, and %d and 100 "+
						"where it asks for trains", len(got.Queue), len(got.Ejected), got.EjectedTotal, err,
						gitlet.MaxEjectionsShown)
				}
			}
		})
	}
}

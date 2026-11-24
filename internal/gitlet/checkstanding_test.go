package gitlet_test

import (
	"context"
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
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// standingEstate is web, every seat's to read, main at A and its tip protected, with web!1 from
// feature (head B) and web!2 from two (head C) into main; and api, Sales' alone, main at A, with
// api!1 from feature (head B).
type standingEstate struct {
	n                *node
	c                *gitlet.Committer
	web, api         string
	web1, web2, api1 string
}

func newStandingEstate(t testing.TB) standingEstate {
	t.Helper()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	e := standingEstate{n: n, c: c, web: changeRepo(t, n, "web")}
	e.web1 = e.open(t, e.web, featureRef, "")
	e.web2 = e.open(t, e.web, "refs/heads/two", oidOf("C"))
	s := suiteSettings()
	s.Owner, s.Visibility = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Sales"}, gitlet.VisibilityUnit
	e.api = n.ensure("api", s, t1).RepoID
	n.push(e.api, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(featureRef, gitlet.ZeroOID, oidOf("B"))))
	e.api1 = e.open(t, e.api, featureRef, "")
	return e
}

// open opens source into main on repo, pushing source at head first where head is set, and is
// the change's id.
func (e standingEstate) open(t testing.TB, repo, source, head string) string {
	t.Helper()
	if head != "" {
		e.n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(source, gitlet.ZeroOID, head)))
	}
	res, err := e.c.ProposeChange(t.Context(), opening(repo, source, mainRef, ada))
	requireChanged(t, res, err)
	return res.ChangeID
}

// attempt is one row of code_checks as a case seeds it: ended at its minute past harnessAt where
// its state is an ending, and not yet ended otherwise.
type attempt struct {
	repo, oid, target, name  string
	n                        int
	state                    gitlet.CheckState
	purpose                  gitlet.CheckPurpose
	change, candidate, entry string
	base                     string
	minute                   int
}

// headOf is attempt 1 of check name of web!1's head B for main, in state at minute.
func (e standingEstate) headOf(name string, state gitlet.CheckState, minute int) attempt {
	return attempt{repo: e.web, oid: oidOf("B"), target: mainRef, name: name, n: 1, state: state,
		purpose: gitlet.PurposeHead, change: e.web1, minute: minute}
}

const insertAttemptSQL = `INSERT INTO code_checks (repo_id, commit_oid, target_ref, name, attempt, state, purpose,
		change_id, revision, candidate_id, entry_id, base_oid, defs_oid, required, timeout_seconds, reason,
		runner_node, placement, box_handle, reporter, queued_at, started_at, finished_at, duration_ms, exit_code,
		failures_omitted, queued_position, finished_position, row_bytes, overdue_at, url, summary, def, failures,
		tail)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 1, 600, '', '', '', '', '', ?, 0, ?, 0, NULL, 0, 1, 0, 0, 0,
		'', '', '', '', '')`

// seed writes rows straight into code_checks, in one transaction: a history of purposes and
// subjects no case reaches through the writer but by a dispatch, a queue and a push apiece.
func (e standingEstate) seed(t testing.TB, rows ...attempt) {
	t.Helper()
	if err := e.n.db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, a := range rows {
			finished, revision := int64(0), 0
			if a.state.Ended() {
				finished = store.EncodeTime(harnessAt.Add(time.Duration(a.minute) * time.Minute))
			}
			if a.purpose == gitlet.PurposeHead {
				revision = 1
			}
			if _, err := tx.ExecContext(t.Context(), insertAttemptSQL, a.repo, a.oid, a.target, a.name, max(a.n, 1),
				string(a.state), string(a.purpose), a.change, revision, a.candidate, a.entry, a.base,
				store.EncodeTime(harnessAt), finished); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed %d attempts: %v", len(rows), err)
	}
}

// enqueue writes entry of change, enqueued at head, as its change's seq'th entry in state, tested
// by candidate.
func (e standingEstate) enqueue(repo, id, change string, seq int, head, state, candidate string) {
	e.n.exec(`INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid, method,
		delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state, train, candidate_id,
		commit_oid, enqueued_at) VALUES (?, ?, 'refs/heads/main', ?, ?, ?, 'merge', 1, 0, 'dev', 'agent', '', ?,
		'train-1', ?, '', 1)`, repo, id, change, seq, head, state, candidate)
}

// standingRead is every row a standing listing q lists, a page at a time from its own cursor,
// the first page's counts, and the pages it took; a later page answering counts fails t.
func standingRead(t *testing.T, n *node, q gitlet.ReadQuery) ([]gitlet.CheckRow, *gitlet.StandingCounts, int) {
	t.Helper()
	q.Kind, q.Fresh = gitlet.ReadChecks, statelog.Freshness{Level: statelog.ReadStale}
	var (
		rows   []gitlet.CheckRow
		counts *gitlet.StandingCounts
	)
	for pages := 1; pages <= 100; pages++ {
		got, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("read %+v: %v", q.Checks, err)
		}
		if pages == 1 {
			counts = got.StandingCounts
		} else if got.StandingCounts != nil {
			t.Fatalf("page %d of %+v answered counts %+v, want them on the first page alone", pages, q.Checks,
				got.StandingCounts)
		}
		rows = append(rows, got.Checks...)
		if got.Next == "" {
			return rows, counts, pages
		}
		q.Cursor = got.Next
	}
	t.Fatalf("the listing %+v did not end in a hundred pages", q.Checks)
	return nil, nil, 0
}

// labels is each row as "<repo>/<name>", in order.
func labels(rows []gitlet.CheckRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Repo+"/"+r.Name)
	}
	return out
}

// A RERUN STANDS AT ITS LATEST ATTEMPT: web!1's unit failed at attempt 1 and passed at attempt 2,
// so where it stands is attempt 2 alone, passed, read a row a page in the failing-first order,
// which reaches attempt 1 first; queued again as attempt 3, it stands queued.
//
// Mutation: drop the facts' later-attempt test and attempt 1 is listed failed and counted.
func TestARerunStandsAtItsLatestAttempt(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	first := e.headOf("unit", gitlet.CheckStateFailed, 1)
	second := e.headOf("unit", gitlet.CheckStatePassed, 2)
	second.n = 2
	e.seed(t, first, second)
	q := gitlet.ReadQuery{Asker: ada, Repo: "web", Limit: 1, Checks: &gitlet.CheckQuery{
		Standing: gitlet.StandingFailing}}
	rows, counts, _ := standingRead(t, e.n, q)
	if len(rows) != 1 || rows[0].Attempt != 2 || rows[0].State != gitlet.CheckStatePassed {
		t.Fatalf("unit stands at %+v, want attempt 2 alone, passed", rows)
	}
	if counts == nil || counts.Counts != (gitlet.CheckCounts{Passed: 1}) {
		t.Errorf("the counts are %+v, want one passed and no failure", counts)
	}
	third := e.headOf("unit", gitlet.CheckStateQueued, 0)
	third.n = 3
	e.seed(t, third)
	rows, counts, _ = standingRead(t, e.n, q)
	if len(rows) != 1 || rows[0].Attempt != 3 || rows[0].State != gitlet.CheckStateQueued {
		t.Fatalf("unit queued again stands at %+v, want attempt 3 alone, queued", rows)
	}
	if counts == nil || counts.Counts != (gitlet.CheckCounts{Queued: 1}) {
		t.Errorf("the counts are %+v, want one queued", counts)
	}
}

// RUNNING ATTEMPTS STAND BESIDE ENDED ONES: across web and api, what runs now is listed first,
// then what waits, then the endings newest first, in the window and in one repository; and the
// window without Standing still refuses a state that never ends in it.
//
// Mutation: drop the window's unended phases (sc.phasePlaces answering nothing for queued and
// running) and the window lists the endings alone.
func TestRunningAttemptsStandBesideEndedOnes(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	e.seed(t, e.headOf("lint", gitlet.CheckStateRunning, 0), e.headOf("unit", gitlet.CheckStatePassed, 3),
		attempt{repo: e.web, oid: oidOf("A"), target: mainRef, name: "build", state: gitlet.CheckStateFailed,
			purpose: gitlet.PurposeTip, minute: 5},
		attempt{repo: e.api, oid: oidOf("B"), target: mainRef, name: "unit", state: gitlet.CheckStateQueued,
			purpose: gitlet.PurposeHead, change: e.api1},
		attempt{repo: e.api, oid: oidOf("B"), target: mainRef, name: "lint", state: gitlet.CheckStatePassed,
			purpose: gitlet.PurposeHead, change: e.api1, minute: 4})
	for _, tc := range []struct {
		name string
		repo string
		want []string
	}{
		{"the window", "", []string{"web/lint", "api/unit", "web/build", "api/lint", "web/unit"}},
		{"web", "web", []string{"web/lint", "web/build", "web/unit"}},
		{"api", "api", []string{"api/unit", "api/lint"}},
	} {
		for _, limit := range []int{0, 2} {
			rows, _, _ := standingRead(t, e.n, gitlet.ReadQuery{Asker: ada, Repo: tc.repo, Limit: limit,
				Checks: &gitlet.CheckQuery{Standing: gitlet.StandingRecent, Since: harnessAt}})
			if got := labels(rows); !slices.Equal(got, tc.want) {
				t.Errorf("%s, %d a page, stands at %v, want %v", tc.name, limit, got, tc.want)
			}
		}
	}
	_, err := e.n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChecks, Asker: ada,
		Fresh:  statelog.Freshness{Level: statelog.ReadStale},
		Checks: &gitlet.CheckQuery{State: gitlet.CheckStateRunning}})
	if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "the window lists the attempts that ENDED") {
		t.Errorf("the window without Standing narrowed to running read %v, want today's refusal", err)
	}
}

// FAILING STANDS FIRST, ACROSS PAGES: with an attempt standing in every state, the failing-first
// order lists the failures, the errors, what runs, what waits, the cancellations, the passes and
// the skips, each newest ended first, and the recent order what runs and waits before every
// ending newest first; read two rows a page, each lists every attempt once, in that order, across
// every phase's end, in one repository and in the window.
//
// Mutation: start a phase after the first from the cursor's place rather than its top and a page
// crossing a phase's end skips the next phase's newest.
func TestFailingStandsFirstAcrossPages(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	e.seed(t, e.headOf("f-old", gitlet.CheckStateFailed, 1), e.headOf("errored", gitlet.CheckStateErrored, 2),
		e.headOf("cancelled", gitlet.CheckStateCancelled, 3), e.headOf("p-old", gitlet.CheckStatePassed, 4),
		e.headOf("skipped", gitlet.CheckStateSkipped, 5), e.headOf("f-new", gitlet.CheckStateFailed, 6),
		e.headOf("p-new", gitlet.CheckStatePassed, 7), e.headOf("run", gitlet.CheckStateRunning, 0),
		e.headOf("queue", gitlet.CheckStateQueued, 0))
	orders := map[gitlet.StandingOrder][]string{
		gitlet.StandingFailing: {"f-new", "f-old", "errored", "run", "queue", "cancelled", "p-new", "p-old",
			"skipped"},
		gitlet.StandingRecent: {"run", "queue", "p-new", "f-new", "skipped", "p-old", "cancelled", "errored",
			"f-old"},
	}
	for order, names := range orders {
		var want []string
		for _, name := range names {
			want = append(want, "web/"+name)
		}
		for _, repo := range []string{"web", ""} {
			for _, limit := range []int{0, 1, 2} {
				rows, _, pages := standingRead(t, e.n, gitlet.ReadQuery{Asker: ada, Repo: repo, Limit: limit,
					Checks: &gitlet.CheckQuery{Standing: order}})
				if got := labels(rows); !slices.Equal(got, want) {
					t.Errorf("%s in %q, %d a page, lists %v, want %v", order, repo, limit, got, want)
				}
				if limit > 0 && pages != len(want)/limit+1 {
					t.Errorf("%s in %q, %d a page, took %d pages, want %d: every page but the last full",
						order, repo, limit, pages, len(want)/limit+1)
				}
			}
		}
	}
}

// THE STANDING COUNTS ARE BOUNDED, EACH STATE APART, AND SAY SO: a first page counts the standing
// attempts by state in its scope and window, narrowed by a check's name and never by a state, and
// a later page none; held to two places a state, the state holding three is counted to two and
// named a floor, and the one holding two is exact.
//
// Mutations: share one bound across the states (one walk of two places) and the failures are not
// counted; name a state a floor where it holds exactly the bound and failed is named.
func TestTheStandingCountsAreBoundedAndSaySo(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	e.seed(t, e.headOf("p1", gitlet.CheckStatePassed, 1), e.headOf("p2", gitlet.CheckStatePassed, 2),
		e.headOf("p3", gitlet.CheckStatePassed, 3), e.headOf("f1", gitlet.CheckStateFailed, 4),
		e.headOf("f2", gitlet.CheckStateFailed, 5), e.headOf("r1", gitlet.CheckStateRunning, 0))
	late := harnessAt.Add(150 * time.Second)
	for _, tc := range []struct {
		name string
		repo string
		c    gitlet.CheckQuery
		want gitlet.CheckCounts
	}{
		{"web", "web", gitlet.CheckQuery{}, gitlet.CheckCounts{Passed: 3, Failed: 2, Running: 1}},
		{"the window", "", gitlet.CheckQuery{Since: harnessAt}, gitlet.CheckCounts{Passed: 3, Failed: 2, Running: 1}},
		{"web's p1", "web", gitlet.CheckQuery{Name: "p1"}, gitlet.CheckCounts{Passed: 1}},
		{"web's failures", "web", gitlet.CheckQuery{State: gitlet.CheckStateFailed},
			gitlet.CheckCounts{Passed: 3, Failed: 2, Running: 1}},
		{"web's late window", "web", gitlet.CheckQuery{Since: late}, gitlet.CheckCounts{Passed: 1, Failed: 2,
			Running: 1}},
		{"the late window", "", gitlet.CheckQuery{Since: late}, gitlet.CheckCounts{Passed: 1, Failed: 2,
			Running: 1}},
	} {
		c := tc.c
		c.Standing = gitlet.StandingRecent
		_, counts, _ := standingRead(t, e.n, gitlet.ReadQuery{Asker: ada, Repo: tc.repo, Limit: 1, Checks: &c})
		if counts == nil || counts.Counts != tc.want || len(counts.AtLeast) != 0 {
			t.Errorf("%s counted %+v, want %+v and no floor", tc.name, counts, tc.want)
		}
	}
	gitlet.SetStandingCountBound(e.n.reader, 2)
	_, counts, _ := standingRead(t, e.n, gitlet.ReadQuery{Asker: ada, Repo: "web", Checks: &gitlet.CheckQuery{
		Standing: gitlet.StandingFailing}})
	if counts == nil || counts.Counts != (gitlet.CheckCounts{Passed: 2, Failed: 2, Running: 1}) ||
		!slices.Equal(counts.AtLeast, []gitlet.CheckState{gitlet.CheckStatePassed}) {
		t.Errorf("held to two a state, the counts are %+v, want passed 2 named a floor, failed 2 and running 1",
			counts)
	}
}

// ONLY A LIVE SUBJECT STANDS. None of these stands: a closed change's head's attempt; one at a
// commit its change no longer heads; one checked for a branch its change does not merge into; a
// tip's at a commit its branch moved from; a bisection's; a candidate's of an entry its change
// queued again after; one of a candidate the entry is no longer tested by; and a candidate's for
// main of an entry whose change was then retargeted to release. Each of these does: the head's,
// of the live change at that head whichever change queued it (web!4 closed and web!5 opened at its
// head); the tip's; and the candidate's of an entry ejected while its change is open.
//
// Mutations: drop the head's branch test and retargeted stands; drop the entry's newest test and
// old-entry stands; drop the candidate's test and old-train stands; drop the candidate's branch
// test and moved-entry stands; judge a head by the change that queued it and reopened is lost.
func TestOnlyALiveSubjectStands(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	web3 := e.open(t, e.web, "refs/heads/three", oidOf("D"))
	web4 := e.open(t, e.web, "refs/heads/four", oidOf("F"))
	e.n.exec(`UPDATE code_changes SET state = 'closed' WHERE id IN (?, ?)`, web3, web4)
	web5 := e.open(t, e.web, "refs/heads/five", oidOf("F"))
	web6 := e.open(t, e.web, "refs/heads/six", oidOf("G"))
	e.enqueue(e.web, "entry-1", e.web2, 1, oidOf("C"), "ejected", "cand-1")
	e.enqueue(e.web, "entry-2", e.web2, 2, oidOf("C"), "ejected", "cand-2")
	e.enqueue(e.web, "entry-3", web6, 1, oidOf("G"), "dequeued", "cand-3")
	e.n.exec(`UPDATE code_changes SET target_ref = ? WHERE id = ?`, releaseRef, web6)
	candidate := func(name, oid, cand, entryID string, minute int) attempt {
		return attempt{repo: e.web, oid: oidOf(oid), target: mainRef, name: name, state: gitlet.CheckStateFailed,
			purpose: gitlet.PurposeCandidate, candidate: cand, entry: entryID, base: oidOf("A"), minute: minute}
	}
	oldHead := e.headOf("old-head", gitlet.CheckStatePassed, 2)
	oldHead.oid = oidOf("E")
	retargeted := e.headOf("retargeted", gitlet.CheckStatePassed, 3)
	retargeted.target = releaseRef
	closed := e.headOf("closed", gitlet.CheckStatePassed, 4)
	closed.oid, closed.change = oidOf("D"), web3
	reopened := e.headOf("reopened", gitlet.CheckStatePassed, 11)
	reopened.oid, reopened.change = oidOf("F"), web4
	tip := attempt{repo: e.web, oid: oidOf("A"), target: mainRef, name: "tip", state: gitlet.CheckStatePassed,
		purpose: gitlet.PurposeTip, minute: 5}
	oldTip, bisect := tip, tip
	oldTip.name, oldTip.oid, oldTip.minute = "old-tip", oidOf("Z"), 6
	bisect.name, bisect.purpose, bisect.minute = "bisect", gitlet.PurposeBisect, 7
	e.seed(t, e.headOf("head", gitlet.CheckStatePassed, 1), oldHead, retargeted, closed, reopened, tip, oldTip,
		bisect, candidate("old-entry", "K1", "cand-1", "entry-1", 8), candidate("ejected", "K2", "cand-2", "entry-2", 9),
		candidate("old-train", "K3", "cand-0", "entry-2", 10), candidate("moved-entry", "K4", "cand-3", "entry-3", 12))
	rows, counts, _ := standingRead(t, e.n, gitlet.ReadQuery{Asker: ada, Checks: &gitlet.CheckQuery{
		Standing: gitlet.StandingRecent}})
	if got, want := labels(rows), []string{"web/reopened", "web/ejected", "web/tip", "web/head"}; !slices.Equal(got,
		want) {
		t.Fatalf("the window stands at %v, want %v", got, want)
	}
	if c := rows[0].Change; c == nil || c.Ref != "web!5" || rows[0].SourceRef != "refs/heads/five" {
		t.Errorf("reopened names %+v from %q, want web!5 (%s), the live change at its head", c, rows[0].SourceRef,
			web5)
	}
	if counts == nil || counts.Counts != (gitlet.CheckCounts{Passed: 3, Failed: 1}) {
		t.Errorf("the counts are %+v, want three passed and one failed", counts)
	}
}

// A CANDIDATE'S ROW NAMES ITS ENTRY'S CHANGE: the change, its branch and the head its entry was
// enqueued at, beside the commit the candidate merged onto main and its base; a head's names its
// own change at its own commit; a tip's names none; and a listing not asked where checks stand
// names none on any row.
//
// Mutations: answer the candidate's commit as the change's head and e2e's change_head is K; join
// the change on every listing and the plain listing's rows name one.
func TestACandidatesRowNamesItsEntrysChange(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	e.enqueue(e.web, "entry-1", e.web2, 1, oidOf("C"), "testing", "cand-1")
	e.seed(t, e.headOf("unit", gitlet.CheckStatePassed, 1),
		attempt{repo: e.web, oid: oidOf("K"), target: mainRef, name: "e2e", state: gitlet.CheckStateFailed,
			purpose: gitlet.PurposeCandidate, candidate: "cand-1", entry: "entry-1", base: oidOf("A"), minute: 2},
		attempt{repo: e.web, oid: oidOf("A"), target: mainRef, name: "build", state: gitlet.CheckStatePassed,
			purpose: gitlet.PurposeTip, minute: 3})
	rows, _, _ := standingRead(t, e.n, gitlet.ReadQuery{Asker: ada, Repo: "web", Checks: &gitlet.CheckQuery{
		Standing: gitlet.StandingRecent}})
	if got := labels(rows); !slices.Equal(got, []string{"web/build", "web/e2e", "web/unit"}) {
		t.Fatalf("web stands at %v, want build, e2e and unit", got)
	}
	build, e2e, unit := rows[0], rows[1], rows[2]
	if build.Change != nil || build.SourceRef != "" || build.ChangeHead != "" {
		t.Errorf("the tip's row names %+v, %q and %q, want no change", build.Change, build.SourceRef,
			build.ChangeHead)
	}
	want := gitlet.ChangeName{Repo: "web", Ref: "web!2", Title: "Retry the upload", State: gitlet.ChangeStateOpen}
	if e2e.Change == nil || *e2e.Change != want || e2e.SourceRef != "refs/heads/two" ||
		e2e.ChangeHead != oidOf("C") || e2e.OID != oidOf("K") || e2e.Base != oidOf("A") {
		t.Errorf("the candidate's row names %+v from %q at %s, checked at %s on %s; want %+v from two at C, "+
			"checked at K merged onto A", e2e.Change, e2e.SourceRef, e2e.ChangeHead, e2e.OID, e2e.Base, want)
	}
	if unit.Change == nil || unit.Change.Ref != "web!1" || unit.SourceRef != featureRef ||
		unit.ChangeHead != unit.OID {
		t.Errorf("the head's row names %+v from %q at %s, want web!1 from feature at its own commit",
			unit.Change, unit.SourceRef, unit.ChangeHead)
	}
	plain := servedChecks(t, e.n, gitlet.ReadQuery{Kind: gitlet.ReadChecks, Asker: ada, Repo: "web",
		Fresh: statelog.Freshness{Level: statelog.ReadStale}})
	if len(plain) != 3 {
		t.Fatalf("web's plain listing holds %d rows, want 3", len(plain))
	}
	for _, row := range plain {
		if row.Change != nil || row.SourceRef != "" || row.ChangeHead != "" {
			t.Errorf("the plain listing's %s names %+v, %q and %q, want none", row.Name, row.Change, row.SourceRef,
				row.ChangeHead)
		}
	}
}

// STANDING IS REFUSED WHERE IT MEANS NOTHING: beside a change, a commit, a branch, latest, an end,
// an attempt, on a check read, and an order this build does not know, each naming its field; a
// window's start beside a repository is refused without Standing and taken with it.
//
// Mutation: relax the plain listing's start rule for every listing and the repository's start is
// taken without Standing.
func TestStandingIsRefusedWhereItMeansNothing(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	stand := func(c gitlet.CheckQuery) *gitlet.CheckQuery { c.Standing = gitlet.StandingFailing; return &c }
	for name, tc := range map[string]struct {
		q     gitlet.ReadQuery
		field string
	}{
		"beside a change": {gitlet.ReadQuery{Repo: "web", Checks: stand(gitlet.CheckQuery{Change: 1})},
			"checks.standing"},
		"beside a commit": {gitlet.ReadQuery{Repo: "web", Checks: stand(gitlet.CheckQuery{OID: oidOf("B")})},
			"checks.standing"},
		"beside a branch": {gitlet.ReadQuery{Repo: "web", Checks: stand(gitlet.CheckQuery{OID: oidOf("B"),
			Target: mainRef})}, "checks.standing"},
		"beside latest": {gitlet.ReadQuery{Checks: stand(gitlet.CheckQuery{Latest: true})}, "checks.latest"},
		"beside an end": {gitlet.ReadQuery{Checks: stand(gitlet.CheckQuery{Since: harnessAt,
			Until: harnessAt.Add(time.Hour)})}, "checks.until"},
		"beside an attempt": {gitlet.ReadQuery{Checks: stand(gitlet.CheckQuery{Attempt: 1})}, "checks.attempt"},
		"on a check read": {gitlet.ReadQuery{Kind: gitlet.ReadCheck, Repo: "web", Checks: stand(gitlet.CheckQuery{
			OID: oidOf("B"), Target: mainRef, Name: "unit", Attempt: 1})}, "checks.standing"},
		"an unknown order": {gitlet.ReadQuery{Checks: &gitlet.CheckQuery{Standing: "sideways"}},
			"checks.standing"},
		"a bad name":  {gitlet.ReadQuery{Checks: stand(gitlet.CheckQuery{Name: "Unit!"})}, "checks.name"},
		"a bad state": {gitlet.ReadQuery{Checks: stand(gitlet.CheckQuery{State: "green"})}, "checks.state"},
		"a start beside a repository, not standing": {gitlet.ReadQuery{Repo: "web",
			Checks: &gitlet.CheckQuery{Since: harnessAt}}, "checks.since"},
	} {
		q := tc.q
		if q.Kind == "" {
			q.Kind = gitlet.ReadChecks
		}
		q.Asker, q.Fresh = ada, statelog.Freshness{Level: statelog.ReadStale}
		_, err := e.n.reader.Read(t.Context(), q)
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%s answered %v, want gitlet.ErrInvalid naming %s", name, err, tc.field)
		}
	}
	if _, err := e.n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadChecks, Asker: ada, Repo: "web",
		Fresh:  statelog.Freshness{Level: statelog.ReadStale},
		Checks: stand(gitlet.CheckQuery{Since: harnessAt, State: gitlet.CheckStateQueued})}); err != nil {
		t.Errorf("a standing listing of web since a moment, narrowed to queued, was refused: %v", err)
	}
}

// A STANDING PAGE IS HELD TO THE ASKER'S GRANT: dev reads web and not api, so the window's pages
// never carry api's rows and still go on past its places, a page that read only api's empty with
// a cursor, and its counts leave api out; dev reading for a seat that reads neither is told
// nothing; and api's change's title, in a person's answer, is in none of dev's.
//
// Mutations: keep a place by its facts alone (no grant) and dev lists api's; count before the
// grant and dev's counts hold api's failure; take the cursor from the kept rows and the one-row
// listing stops at its first empty page.
func TestAStandingPageIsHeldToTheAskersGrant(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	const sentinel = "Sentinel title 7f3a"
	e.n.exec(`UPDATE code_changes SET title = ? WHERE id = ?`, sentinel, e.api1)
	e.seed(t, e.headOf("unit", gitlet.CheckStatePassed, 1),
		attempt{repo: e.web, oid: oidOf("A"), target: mainRef, name: "build", state: gitlet.CheckStatePassed,
			purpose: gitlet.PurposeTip, minute: 3},
		attempt{repo: e.api, oid: oidOf("B"), target: mainRef, name: "unit", state: gitlet.CheckStateFailed,
			purpose: gitlet.PurposeHead, change: e.api1, minute: 2},
		attempt{repo: e.api, oid: oidOf("B"), target: mainRef, name: "lint", state: gitlet.CheckStateRunning,
			purpose: gitlet.PurposeHead, change: e.api1})
	stale := statelog.Freshness{Level: statelog.ReadStale}
	encoded := func(q gitlet.ReadQuery) (string, []string, *gitlet.StandingCounts, int) {
		t.Helper()
		q.Kind, q.Fresh = gitlet.ReadChecks, stale
		q.Checks = &gitlet.CheckQuery{Standing: gitlet.StandingRecent}
		var (
			all    strings.Builder
			listed []string
			counts *gitlet.StandingCounts
			empty  int
		)
		for {
			got, err := e.n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("read as %+v: %v", q.Asker, err)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			all.Write(b)
			if q.Cursor == "" {
				counts = got.StandingCounts
			}
			if len(got.Checks) == 0 && got.Next != "" {
				empty++
			}
			listed = append(listed, labels(got.Checks)...)
			if got.Next == "" {
				return all.String(), listed, counts, empty
			}
			q.Cursor = got.Next
		}
	}
	body, listed, _, _ := encoded(gitlet.ReadQuery{Asker: ada, Limit: 1})
	if !strings.Contains(body, sentinel) || !slices.Equal(listed, []string{"api/lint", "web/build", "api/unit",
		"web/unit"}) {
		t.Fatalf("a person's window lists %v, want api's beside web's with api!1's title", listed)
	}
	body, listed, counts, empty := encoded(gitlet.ReadQuery{Asker: agent("dev"), Limit: 1})
	if !slices.Equal(listed, []string{"web/build", "web/unit"}) || empty < 2 {
		t.Errorf("dev's window lists %v with %d empty pages, want web's alone, going on past api's two places",
			listed, empty)
	}
	if counts == nil || counts.Counts != (gitlet.CheckCounts{Passed: 2}) {
		t.Errorf("dev's counts are %+v, want web's two passes alone", counts)
	}
	if strings.Contains(body, sentinel) {
		t.Errorf("dev's window carries api!1's title")
	}
	body, listed, counts, _ = encoded(gitlet.ReadQuery{Asker: agent("dev"), CeilingSeat: "outsider"})
	if len(listed) != 0 || counts == nil || counts.Counts != (gitlet.CheckCounts{}) || strings.Contains(body,
		sentinel) {
		t.Errorf("dev reading for a seat that reads neither listed %v and counted %+v, want nothing", listed,
			counts)
	}
}

// BenchmarkTheStandingCountsAtTheirBound is a standing listing's first page where every state
// holds one place past MaxStandingCounted, each standing: the most its counts read, in one
// repository and in the window.
func BenchmarkTheStandingCountsAtTheirBound(b *testing.B) {
	gitlet.Quiet(b)
	e := newStandingEstate(b)
	var rows []attempt
	for _, s := range gitlet.CheckStates() {
		for i := range gitlet.MaxStandingCounted + 1 {
			rows = append(rows, e.headOf(fmt.Sprintf("%s-%04d", s, i), s, 1+i%600))
		}
	}
	e.seed(b, rows...)
	for _, repo := range []string{"web", ""} {
		b.Run(fmt.Sprintf("repo=%q", repo), func(b *testing.B) {
			q := gitlet.ReadQuery{Kind: gitlet.ReadChecks, Asker: ada, Repo: repo,
				Fresh:  statelog.Freshness{Level: statelog.ReadStale},
				Checks: &gitlet.CheckQuery{Standing: gitlet.StandingFailing}}
			for b.Loop() {
				got, err := e.n.reader.Read(context.WithoutCancel(b.Context()), q)
				if err != nil || got.StandingCounts == nil || len(got.StandingCounts.AtLeast) != 7 {
					b.Fatalf("the first page = %+v, %v; want every state counted to its bound", got.StandingCounts,
						err)
				}
			}
		})
	}
}

// TestTheStandingCountBoundKeepsTheFirstPageQuick pins MaxStandingCounted at the bound that keeps a
// first page near the 50 ms line (BenchmarkTheStandingCountsAtTheirBound); a raise re-measures that.
func TestTheStandingCountBoundKeepsTheFirstPageQuick(t *testing.T) {
	t.Parallel()
	if gitlet.MaxStandingCounted > 128 {
		t.Fatalf("MaxStandingCounted is %d: past 128 places a state a first page leaves the 50 ms line; "+
			"re-measure BenchmarkTheStandingCountsAtTheirBound before raising it", gitlet.MaxStandingCounted)
	}
}

// A CI'S REPORT STANDS AS THE SUBJECT IT MEASURES: it names no purpose, so a failed one at web!1's
// head is that change's head check, a running one at main's tip the tip's, one at neither nowhere;
// the first page counts them so. Mutation: judge it by purpose alone; web stands at nothing.
func TestACIsReportStandsAsTheSubjectItMeasures(t *testing.T) {
	t.Parallel()
	e := newStandingEstate(t)
	w, _, _, _ := checksWriterOn(t, e.n)
	report := func(oid, name string, state gitlet.CheckState) {
		t.Helper()
		key := gitlet.CheckKey{RepoID: e.web, OID: oidOf(oid), Target: mainRef, Name: name, Attempt: 1}
		res, err := w.Publish(t.Context(), reporting(key, state, ""))
		requireChecked(t, "the CI's report of "+name, res, err)
	}
	report("B", "lint", gitlet.CheckStateFailed)
	report("A", "deploy", gitlet.CheckStateRunning)
	report("E", "stale", gitlet.CheckStateFailed)
	rows, counts, _ := standingRead(t, e.n, gitlet.ReadQuery{Asker: ada, Repo: "web", Checks: &gitlet.CheckQuery{
		Standing: gitlet.StandingRecent}})
	if got, want := labels(rows), []string{"web/deploy", "web/lint"}; !slices.Equal(got, want) {
		t.Fatalf("web stands at %v, want %v", got, want)
	}
	if deploy := rows[0]; deploy.Change != nil || deploy.ChangeHead != "" {
		t.Errorf("the tip's reported row names %+v at %q, want no change", deploy.Change, deploy.ChangeHead)
	}
	if lint := rows[1]; lint.Change == nil || lint.Change.Ref != "web!1" || lint.SourceRef != featureRef ||
		lint.ChangeHead != oidOf("B") {
		t.Errorf("the head's reported row names %+v from %q at %s, want web!1 from feature at B", lint.Change,
			lint.SourceRef, lint.ChangeHead)
	}
	if counts == nil || counts.Counts != (gitlet.CheckCounts{Running: 1, Failed: 1}) {
		t.Errorf("the counts are %+v, want one running and one failed", counts)
	}
}

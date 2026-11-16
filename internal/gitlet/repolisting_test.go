package gitlet_test

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// listedRow is a repository as the listing tests judge it, read from its row.
type listedRow struct {
	ref        gitlet.RepoRef
	lastPushAt sql.NullInt64
	liveBytes  int64
}

// listingNode is a node holding repositories of both kinds under three owners: pushed at
// distinct instants, two at one instant, two never pushed, one archived, one importing and one
// deleted.
func listingNode(t *testing.T) *node {
	t.Helper()
	n := newNode(t)
	n.chart = testChart()
	owned := func(unit string, v gitlet.Visibility) gitlet.RepoSettings {
		s := suiteSettings()
		s.Owner, s.Visibility = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: unit}, v
		return s
	}
	pushed := func(id string, at time.Time) {
		n.pushAt(at, id, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf(id))))
	}
	ids := map[string]string{}
	for _, r := range []struct {
		name string
		s    gitlet.RepoSettings
	}{
		{"api", owned("Engineering", gitlet.VisibilityCompany)},
		{"web", owned("Engineering", gitlet.VisibilityCompany)},
		{"docs", owned("Engineering", gitlet.VisibilityCompany)},
		{"old", owned("Engineering", gitlet.VisibilityCompany)},
		{"gone", owned("Engineering", gitlet.VisibilityCompany)},
		{"deals", owned("Sales", gitlet.VisibilityUnit)},
		{"infra", owned("Infra", gitlet.VisibilityUnit)},
	} {
		ids[r.name] = n.ensure(r.name, r.s, t1).RepoID
	}
	ids["dev.x"] = n.made(dev, "dev.x", seatOwned("dev"))
	ids["dev.y"] = n.made(dev, "dev.y", seatOwned("dev"))
	ids["vp-sales.z"] = n.made(vpSales, "vp-sales.z", seatOwned("vp-sales"))
	if got, err := n.store.EnsureRepo(t.Context(), n.activate(t1), gitlet.Declared{Name: "imp",
		Settings: owned("Engineering", gitlet.VisibilityCompany), Import: importSource}); err != nil ||
		got.Status != gitlet.RepoStatusImporting {
		t.Fatalf("ensure imp: %+v, %v; want it created importing", got, err)
	}
	n.drain()
	pushed(ids["api"], harnessAt.Add(-5*time.Hour))
	pushed(ids["old"], harnessAt.Add(-5*time.Hour))
	pushed(ids["deals"], harnessAt.Add(-4*time.Hour))
	pushed(ids["web"], harnessAt.Add(-3*time.Hour))
	pushed(ids["gone"], harnessAt.Add(-2*time.Hour))
	pushed(ids["dev.x"], harnessAt.Add(-90*time.Minute))
	pushed(ids["vp-sales.z"], harnessAt.Add(-5*time.Hour))
	archived := owned("Engineering", gitlet.VisibilityCompany)
	archived.Archived = true
	n.ensure("old", archived, t1)
	n.deleteAt(ids["gone"], harnessAt)
	n.changeCountsHold()
	return n
}

// listedRows is every repository the node holds, read from its rows.
func (n *node) listedRows() []listedRow {
	n.t.Helper()
	var out []listedRow
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT name, kind, status, owner_kind, owner, visibility,
			last_push_at, live_bytes FROM code_repos`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var r listedRow
			var kind, status, ownerKind, visibility string
			if err = rows.Scan(&r.ref.Name, &kind, &status, &ownerKind, &r.ref.Owner.Ref, &visibility,
				&r.lastPushAt, &r.liveBytes); err != nil {
				return err
			}
			r.ref.Kind, r.ref.Status = gitlet.RepoKind(kind), gitlet.RepoStatus(status)
			r.ref.Owner.Kind, r.ref.Visibility = gitlet.OwnerKind(ownerKind), gitlet.Visibility(visibility)
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read the repositories: %v", err)
	}
	return out
}

// listingAskers are who the listing tests list as: a person, and a seat of three units.
var listingAskers = []struct {
	name  string
	asker gitlet.Principal
}{{"a person", ada}, {"a seat of Engineering", cto}, {"a seat of Sales", vpSales}, {"dev of Infra", dev}}

// pagedRepos is every repository q lists, paged to its end, and the first page's answer.
func pagedRepos(t *testing.T, n *node, q gitlet.ReadQuery) ([]gitlet.RepoSummary, gitlet.ReadAnswer) {
	t.Helper()
	q.Kind, q.Fresh = gitlet.ReadRepos, stale
	var (
		all   []gitlet.RepoSummary
		first gitlet.ReadAnswer
	)
	for page := range 100 {
		answer, err := n.reader.Read(t.Context(), q)
		if err != nil {
			t.Fatalf("list %+v: %v", q, err)
		}
		if page == 0 {
			first = answer
		}
		all = append(all, answer.Repos...)
		if answer.Next == "" {
			return all, first
		}
		q.Cursor = answer.Next
	}
	t.Fatalf("the listing %+v never ended", q)
	return nil, first
}

// A REPOSITORY LISTING IS IN ORDER AND NARROWED BY KIND: every sort, kind, page size and asker
// pages to what the rows say, in name order or by last push (newest first, one instant's names
// highest first, then the never-pushed by name), the repositories in use of the kind the asker
// reads, each once; and a cursor of one order is refused by the other.
// Mutation: tie by name ascending in the dated phase, forget the never-pushed phase, merge the
// kinds by name, or filter the kind in Go after the page.
func TestARepositoryListingIsInOrderAndNarrowedByKind(t *testing.T) {
	t.Parallel()
	n := listingNode(t)
	rows := n.listedRows()
	byPush := func(a, b listedRow) int {
		pa, pb := a.lastPushAt, b.lastPushAt
		switch {
		case pa.Valid && pb.Valid:
			return cmp.Or(cmp.Compare(pb.Int64, pa.Int64), cmp.Compare(b.ref.Name, a.ref.Name))
		case pa.Valid != pb.Valid && pa.Valid:
			return -1
		case pa.Valid != pb.Valid:
			return 1
		}
		return cmp.Compare(a.ref.Name, b.ref.Name)
	}
	byName := func(a, b listedRow) int { return cmp.Compare(a.ref.Name, b.ref.Name) }
	for _, sort := range []gitlet.RepoSort{"", gitlet.RepoSortName, gitlet.RepoSortPushed} {
		for _, kind := range []gitlet.RepoKind{"", gitlet.RepoProject, gitlet.RepoScratch} {
			for _, who := range listingAskers {
				var want []string
				ordered := slices.Clone(rows)
				slices.SortFunc(ordered, byName)
				if sort == gitlet.RepoSortPushed {
					slices.SortFunc(ordered, byPush)
				}
				for _, r := range ordered {
					_, refused := gitlet.JudgeRead(who.asker, "", r.ref, n.chart)
					if (kind == "" || r.ref.Kind == kind) && r.ref.Status != gitlet.RepoStatusDeleted && refused == nil {
						want = append(want, r.ref.Name)
					}
				}
				for _, limit := range []int{1, 2, 100} {
					listed, _ := pagedRepos(t, n, gitlet.ReadQuery{Asker: who.asker, RepoKind: kind, RepoSort: sort,
						Limit: limit})
					got := make([]string, len(listed))
					for i, s := range listed {
						got[i] = s.Name
					}
					if !slices.Equal(got, want) {
						t.Errorf("%s listing sort %q kind %q %d a page: %q, want %q", who.name, sort, kind, limit,
							got, want)
					}
				}
			}
		}
	}
	// THE WHOLE ESTATE IS IN IT: a person lists by last push what the order above says, past
	// two kinds, a tie, the never-pushed and every status but the deleted one.
	if listed, _ := pagedRepos(t, n, gitlet.ReadQuery{Asker: ada, RepoSort: gitlet.RepoSortPushed}); len(listed) != 10 {
		t.Errorf("a person lists %d repositories by last push, want the ten in use", len(listed))
	}

	pushedPage, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadRepos, Asker: ada,
		RepoSort: gitlet.RepoSortPushed, Limit: 1, Fresh: stale})
	if err != nil || pushedPage.Next == "" {
		t.Fatalf("a first page by last push answered %+v, %v; want a cursor", pushedPage, err)
	}
	for _, tc := range []struct {
		name string
		q    gitlet.ReadQuery
	}{
		{"a cursor by last push given to a listing by name", gitlet.ReadQuery{Cursor: pushedPage.Next}},
		{"a name given to a listing by last push", gitlet.ReadQuery{RepoSort: gitlet.RepoSortPushed, Cursor: "web"}},
		{"an instant with no name", gitlet.ReadQuery{RepoSort: gitlet.RepoSortPushed, Cursor: "12 "}},
		{"never with no name", gitlet.ReadQuery{RepoSort: gitlet.RepoSortPushed, Cursor: "never"}},
	} {
		tc.q.Kind, tc.q.Asker, tc.q.Fresh = gitlet.ReadRepos, ada, stale
		if answer, err := n.reader.Read(t.Context(), tc.q); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("%s answered %d repositories, %v; want ErrBadCursor", tc.name, len(answer.Repos), err)
		}
	}
}

// A REPOSITORY LISTING REFUSES WHAT IT CANNOT SERVE: an unknown kind or order, either asked of
// another kind of read, either beside a prefix or the protected ones, a listing by last push of
// the deleted, and the repositories counted for one repository, each ErrInvalid naming its field.
// Mutation: a refusal relaxed into an empty or reordered answer.
func TestARepositoryListingRefusesWhatItCannotServe(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repos := &gitlet.CountsQuery{Repos: true}
	cases := []struct {
		field string
		q     gitlet.ReadQuery
	}{
		{"repo_kind", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoKind: "nope"}},
		{"repo_sort", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoSort: "nope"}},
		{"repo_kind", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoKind: gitlet.RepoProject, Prefix: "w"}},
		{"repo_kind", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoKind: gitlet.RepoProject, Protected: true}},
		{"repo_sort", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoSort: gitlet.RepoSortPushed, Prefix: "w"}},
		{"repo_sort", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoSort: gitlet.RepoSortPushed, Protected: true}},
		{"repo_sort", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoSort: gitlet.RepoSortPushed,
			Status: []gitlet.RepoStatus{gitlet.RepoStatusActive, gitlet.RepoStatusDeleted}}},
		{"repo", gitlet.ReadQuery{Kind: gitlet.ReadCounts, Counts: repos, Repo: "web"}},
	}
	for _, kind := range []gitlet.ReadKind{gitlet.ReadChanges, gitlet.ReadRefs, gitlet.ReadRepo, gitlet.ReadCounts} {
		cases = append(cases,
			struct {
				field string
				q     gitlet.ReadQuery
			}{"repo_kind", gitlet.ReadQuery{Kind: kind, Repo: "web", RepoKind: gitlet.RepoScratch, Counts: repos}},
			struct {
				field string
				q     gitlet.ReadQuery
			}{"repo_sort", gitlet.ReadQuery{Kind: kind, Repo: "web", RepoSort: gitlet.RepoSortName, Counts: repos}})
	}
	for _, tc := range cases {
		if tc.q.Kind != gitlet.ReadCounts {
			tc.q.Counts = nil
		} else if tc.field != "repo" {
			tc.q.Repo = ""
		}
		tc.q.Asker, tc.q.Fresh = ada, stale
		answer, err := n.reader.Read(t.Context(), tc.q)
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field+":") {
			t.Errorf("%+v answered %d repositories, %v; want ErrInvalid naming %s", tc.q, len(answer.Repos), err,
				tc.field)
		}
	}
}

// repoCardOf is repository name's card as its detail and a listing asked for facts answer it, failing
// the case where the two differ or the summary's last push and the card's disagree.
func repoCardOf(t *testing.T, n *node, name string) (*gitlet.RepoCard, time.Time) {
	t.Helper()
	detail, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: name, Asker: ada,
		RepoFacts: true, Fresh: stale})
	if err != nil || detail.Repo == nil || detail.Repo.Card == nil {
		t.Fatalf("read %s with its facts: %+v, %v", name, detail.Repo, err)
	}
	listed, _ := pagedRepos(t, n, gitlet.ReadQuery{Asker: ada, RepoFacts: true})
	i := slices.IndexFunc(listed, func(s gitlet.RepoSummary) bool { return s.Name == name })
	if i < 0 || !reflect.DeepEqual(listed[i].Card, detail.Repo.Card) {
		t.Fatalf("%s's card is %+v listed and %+v in detail, want one card", name, listed, detail.Repo.Card)
	}
	card := detail.Repo.Card
	if card.LastPush != nil && !card.LastPush.At.Equal(detail.Repo.LastPushAt) {
		t.Errorf("%s's last push is at %s and its row's last push at %s, want one instant", name,
			card.LastPush.At, detail.Repo.LastPushAt)
	}
	return card, detail.Repo.LastPushAt
}

// historyHead is the first push of repository name's history that was no expiry, as the history
// reads it: what a card's last push must name.
func historyHead(t *testing.T, n *node, name string) *gitlet.LastPush {
	t.Helper()
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: name, Asker: ada,
		Fresh: stale})
	if err != nil {
		t.Fatalf("read %s's history: %v", name, err)
	}
	for _, row := range answer.Pushes {
		if row.Via == gitlet.ViaExpiry {
			continue
		}
		p := &gitlet.LastPush{PushID: row.PushID, At: row.At, Actor: row.Actor, ActorKind: row.ActorKind,
			ActorSeat: row.ActorSeat, Via: row.Via}
		for _, c := range row.Commands {
			p.Outcome = gitlet.CountedCommands(p.Outcome, c)
		}
		return p
	}
	return nil
}

// A REPOSITORY CARD NAMES WHO PUSHED LAST: the newest push of its history that was no expiry,
// with what became of its commands, past a push refused whole (which records nothing), an expiry
// after it, a merge, two pushes of one record (the higher push id), the horizon (none) and the
// walk's bound (none past it); its instant always the repository's last push.
// Mutation: skip no expiry, order by at, read the newest row whatever its way, or count the
// outcome over the wrong row.
func TestARepositoryCardNamesWhoPushedLast(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	repo := repoOn(t, n, "web")
	seatX, seatY, seatZ := "refs/heads/seats/dev/x", "refs/heads/seats/dev/y", "refs/heads/seats/dev/z"
	expire := func(at time.Time, ref, old string) {
		e := entry(uuid.Must(uuid.NewV7()).String(), gitlet.Command{Ref: ref, Old: old, New: gitlet.ZeroOID,
			Result: gitlet.ResultOK})
		e.Principal, e.Via, e.Pack = gitlet.Principal{Kind: gitlet.PrincipalSystem}, gitlet.ViaExpiry, nil
		n.pushAt(at, repo, e)
	}
	same := func(step string, want *gitlet.LastPush) *gitlet.LastPush {
		t.Helper()
		got, _ := repoCardOf(t, n, "web")
		if head := historyHead(t, n, "web"); !reflect.DeepEqual(got.LastPush, head) {
			t.Errorf("%s: the card names %+v and its history's newest push that was no expiry is %+v", step,
				got.LastPush, head)
		}
		if want != nil && !reflect.DeepEqual(got.LastPush, want) {
			t.Errorf("%s: the card names %+v, want %+v", step, got.LastPush, want)
		}
		return got.LastPush
	}

	// (a) A PERSON'S PUSH, its credential named.
	n.pushAt(harnessAt.Add(-10*time.Hour), repo, entry(uuid.Must(uuid.NewV7()).String(),
		set(mainRef, oidOf("A"), oidOf("B"))))
	if a := same("a person's push", nil); a == nil || a.Via != gitlet.ViaGit || a.ActorSeat != "harness" ||
		a.Outcome != (gitlet.CommandCounts{Moved: 1}) {
		t.Errorf("a person's push is named %+v, want a git push by its credential that moved one ref", a)
	}
	// (b) A SEAT'S PUSH WITH A REFUSED COMMAND beside those that landed.
	refused := gitlet.Command{Ref: "refs/heads/release", Old: gitlet.ZeroOID, New: oidOf("R"),
		Result: gitlet.ResultNG, Reason: gitlet.RefusalNotPermitted}
	n.pushAt(harnessAt.Add(-9*time.Hour), repo, seatPush(uuid.Must(uuid.NewV7()).String(), "dev",
		set(mainRef, oidOf("B"), oidOf("C")), set(seatX, gitlet.ZeroOID, oidOf("X")),
		set(seatY, gitlet.ZeroOID, oidOf("Y")), set(seatZ, gitlet.ZeroOID, oidOf("Z")), refused))
	b := same("a seat's push with one refused", nil)
	if b == nil || b.ActorSeat != "dev" || b.Outcome != (gitlet.CommandCounts{Created: 3, Moved: 1, Refused: 1}) {
		t.Fatalf("a seat's push with one refused command is named %+v", b)
	}
	// (c) A PUSH REFUSED WHOLE records nothing.
	c, _ := committerOn(t, n, n.pub)
	if res, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf("stale"), oidOf("D")))); err == nil &&
		slices.ContainsFunc(res.Answer.Commands, func(c gitlet.Command) bool { return c.Result == gitlet.ResultOK }) {
		t.Fatalf("a push off a stale main landed: %+v", res)
	}
	same("a push refused whole", b)
	// (d) AN EXPIRY AFTER IT.
	expire(harnessAt.Add(-8*time.Hour), seatX, oidOf("X"))
	if _, lastPushAt := repoCardOf(t, n, "web"); !lastPushAt.Equal(b.At) {
		t.Errorf("an expiry moved the last push to %s, want %s", lastPushAt, b.At)
	}
	same("an expiry after the seat's push", b)
	// (e) A MERGE, by whoever merged.
	merged := entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, oidOf("C"), oidOf("M")))
	merged.Principal, merged.Via = agent("cto"), gitlet.ViaMerge
	n.pushAt(harnessAt.Add(-7*time.Hour), repo, merged)
	if e := same("a merge", nil); e == nil || e.Via != gitlet.ViaMerge || e.ActorSeat != "cto" {
		t.Errorf("a merge is named %+v, want the merge and its merger", e)
	}
	// (f) ONE RECORD OF TWO PUSHES BY TWO SEATS: the higher push id is the newer in its history.
	first, second := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	n.pushAt(harnessAt.Add(-6*time.Hour), repo,
		seatPush(second, "cto", set("refs/heads/b", gitlet.ZeroOID, oidOf("b"))),
		seatPush(first, "dev", set("refs/heads/a", gitlet.ZeroOID, oidOf("a"))))
	if f := same("one record of two pushes", nil); f == nil || f.PushID != max(first, second) {
		t.Errorf("one record of two pushes names %+v, want the push of the higher id %s", f, max(first, second))
	}
	// (h) THE WALK'S BOUND: two expiries after the last push are walked past with a walk of three,
	// and a walk of two reads none of what came before.
	expire(harnessAt.Add(-5*time.Hour), seatY, oidOf("Y"))
	expire(harnessAt.Add(-4*time.Hour), seatZ, oidOf("Z"))
	f := same("two expiries after the last push", nil)
	gitlet.SetRepoListingBounds(n.reader, gitlet.MaxReposSized, 2)
	if got, lastPushAt := repoCardOf(t, n, "web"); got.LastPush != nil || !lastPushAt.Equal(f.At) {
		t.Errorf("a walk of two past two expiries names %+v at %s, want none and the last push kept", got.LastPush,
			lastPushAt)
	}
	gitlet.SetRepoListingBounds(n.reader, gitlet.MaxReposSized, 3)
	same("a walk of three past two expiries", f)
	gitlet.SetRepoListingBounds(n.reader, gitlet.MaxReposSized, gitlet.MaxLastPushWalk)
	// (g) PAST THE HORIZON: a later expiry-only record's horizon takes every push it walks past.
	n.pushAt(harnessAt.Add(-3*time.Hour), repo, seatPush(uuid.Must(uuid.NewV7()).String(), "dev",
		set(seatX, gitlet.ZeroOID, oidOf("X2"))))
	last := same("a push before the horizon", nil)
	expire(harnessAt.Add(gitlet.PushHistory), seatX, oidOf("X2"))
	if got, lastPushAt := repoCardOf(t, n, "web"); got.LastPush != nil || !lastPushAt.Equal(last.At) {
		t.Errorf("past the horizon the card names %+v at %s, want none and the last push kept at %s",
			got.LastPush, lastPushAt, last.At)
	}
}

// A REPOSITORY CARD SAYS ITS DEFAULT BRANCH'S CHECKS: each check's latest attempt at the head for
// the default branch, as the commit's mark rolls it up for that branch; none for an empty
// repository or a head no attempt is kept of; and whether it is a push mirror.
// Mutation: read the rollup for the branch's short name, or at the previous head.
func TestARepositoryCardSaysItsDefaultBranchsChecks(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	empty := n.ensure("empty", suiteSettings(), t1).RepoID
	mirrored := suiteSettings()
	mirrored.MirrorTo = mirrorTarget
	n.ensure("mirrored", mirrored, t1)
	head := oidOf("A")
	n.attempted(repo, head, mainRef, attemptOf{name: "unit", attempt: 1, state: gitlet.CheckStatePassed,
		reason: gitlet.CheckReasonExit, finished: harnessAt})
	n.attempted(repo, head, mainRef, attemptOf{name: "e2e", attempt: 1, state: gitlet.CheckStateFailed,
		reason: gitlet.CheckReasonExit, required: true, started: harnessAt, finished: harnessAt.Add(time.Minute)})
	n.attempted(repo, head, mainRef, attemptOf{name: "lint", attempt: 1, state: gitlet.CheckStateRunning,
		started: harnessAt})
	n.attempted(repo, head, "refs/heads/release", attemptOf{name: "unit", attempt: 1,
		state: gitlet.CheckStatePassed, reason: gitlet.CheckReasonExit, finished: harnessAt})
	tip := func(name, commit string) *gitlet.CheckRollup {
		for _, m := range marksOf(t, n, name, commit)[0].Checks {
			if m.Target == mainRef {
				return &m.CheckRollup
			}
		}
		return nil
	}
	card, _ := repoCardOf(t, n, "web")
	if want := tip("web", head); want == nil || want.State != gitlet.RollupFailing ||
		!reflect.DeepEqual(card.TipChecks, want) {
		t.Errorf("web's tip checks are %+v, want main's at its head %+v", card.TipChecks, want)
	}
	if card.PushMirrored {
		t.Error("web is named a push mirror")
	}
	if card, _ := repoCardOf(t, n, "mirrored"); !card.PushMirrored || card.TipChecks != nil {
		t.Errorf("a push mirror with no head reads %+v, want a push mirror with no checks", card)
	}
	if card, _ := repoCardOf(t, n, "empty"); card.TipChecks != nil || card.LastPush != nil || card.PushMirrored {
		t.Errorf("an empty repository (%s) reads %+v, want an empty card", empty, card)
	}
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, head, oidOf("B"))))
	if card, _ := repoCardOf(t, n, "web"); card.TipChecks != nil {
		t.Errorf("a head no attempt is kept of reads %+v, want none", card.TipChecks)
	}
}

// A REPOSITORY LISTING'S FIRST PAGE TOTALS THE WHOLE LISTING: for each kind, a status and each
// asker, the first page's count is every repository paged to the end, and its size theirs summed;
// a later page, a prefix's and the protected ones' carry none, nor a listing that asks no facts;
// and past the size bound the count stays exact and the size is left out.
// Mutation: count the tallies the asker does not read, sum the page alone, or answer a partial size.
func TestARepositoryListingsFirstPageTotalsTheWholeListing(t *testing.T) {
	t.Parallel()
	n := listingNode(t)
	for _, kind := range []gitlet.RepoKind{"", gitlet.RepoProject, gitlet.RepoScratch} {
		for _, status := range [][]gitlet.RepoStatus{nil, {gitlet.RepoStatusArchived},
			{gitlet.RepoStatusDeleted, gitlet.RepoStatusActive}} {
			for _, who := range listingAskers {
				q := gitlet.ReadQuery{Asker: who.asker, RepoKind: kind, Status: status, RepoFacts: true, Limit: 1}
				listed, first := pagedRepos(t, n, q)
				var bytes int64
				for _, s := range listed {
					bytes += s.LiveBytes
				}
				if first.ReposTotal == nil || first.ReposTotal.Repos != int64(len(listed)) ||
					first.ReposTotal.LiveBytes == nil || *first.ReposTotal.LiveBytes != bytes {
					t.Errorf("%s's first page of kind %q in %v totals %+v, want %d repositories of %d bytes", who.name,
						kind, status, first.ReposTotal, len(listed), bytes)
				}
				if len(listed) > 1 {
					q.Cursor = first.Next
					q.Kind, q.Fresh = gitlet.ReadRepos, stale
					if later, err := n.reader.Read(t.Context(), q); err != nil || later.ReposTotal != nil {
						t.Errorf("%s's later page totals %+v, %v; want none", who.name, later.ReposTotal, err)
					}
				}
			}
		}
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"a prefix's":                 {Prefix: "w", RepoFacts: true},
		"the protected ones'":        {Protected: true, RepoFacts: true},
		"a listing asking no facts'": {},
	} {
		q.Kind, q.Asker, q.Fresh = gitlet.ReadRepos, ada, stale
		if answer, err := n.reader.Read(t.Context(), q); err != nil || answer.ReposTotal != nil {
			t.Errorf("%s first page totals %+v, %v; want none", name, answer.ReposTotal, err)
		}
	}
	// PAST THE SIZE BOUND: ten repositories in use are past a bound of nine, and within one of ten.
	for _, bound := range []int{9, 10} {
		gitlet.SetRepoListingBounds(n.reader, bound, gitlet.MaxLastPushWalk)
		listed, first := pagedRepos(t, n, gitlet.ReadQuery{Asker: ada, RepoFacts: true, Limit: 1})
		if got := first.ReposTotal; got == nil || got.Repos != int64(len(listed)) ||
			(got.LiveBytes == nil) != (bound < len(listed)) {
			t.Errorf("with a bound of %d, %d repositories total %+v: want their count, and a size only "+
				"within the bound", bound, len(listed), got)
		}
	}
}

// listingCounts is the repositories asker counts by kind and status in a counts read.
func listingCounts(t *testing.T, n *node, asker gitlet.Principal) *gitlet.RepoKindCounts {
	t.Helper()
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadCounts, Asker: asker,
		Counts: &gitlet.CountsQuery{Repos: true}, Fresh: stale})
	if err != nil || answer.Counts == nil || answer.Counts.Repos == nil {
		t.Fatalf("count the repositories as %+v: %+v, %v", asker, answer.Counts, err)
	}
	return answer.Counts.Repos
}

// THE REPOSITORIES ARE COUNTED AS THEY ARE LISTED: for each asker, each kind and status counts
// the repositories a listing of that kind and status lists, and Live those in use.
// Mutation: drop visibility from the repository tally key, or count a status in another's field.
func TestTheRepositoriesAreCountedAsTheyAreListed(t *testing.T) {
	t.Parallel()
	n := listingNode(t)
	for _, who := range listingAskers {
		got := listingCounts(t, n, who.asker)
		for _, kind := range gitlet.RepoKinds() {
			counts := map[gitlet.RepoKind]gitlet.RepoCounts{gitlet.RepoProject: got.Project,
				gitlet.RepoScratch: got.Scratch}[kind]
			field := map[gitlet.RepoStatus]int64{gitlet.RepoStatusImporting: counts.Importing,
				gitlet.RepoStatusActive: counts.Active, gitlet.RepoStatusArchived: counts.Archived,
				gitlet.RepoStatusDeleted: counts.Deleted, gitlet.RepoStatusPurging: counts.Purging}
			for _, status := range gitlet.RepoStatuses() {
				listed, _ := pagedRepos(t, n, gitlet.ReadQuery{Asker: who.asker, RepoKind: kind,
					Status: []gitlet.RepoStatus{status}, Limit: 2})
				if field[status] != int64(len(listed)) {
					t.Errorf("%s counts %d %s %s repositories, and the listing lists %d", who.name, field[status],
						status, kind, len(listed))
				}
			}
			if counts.Live != counts.Importing+counts.Active+counts.Archived {
				t.Errorf("%s counts %+v of kind %s: live is not those in use", who.name, counts, kind)
			}
		}
	}
	if got := listingCounts(t, n, ada); fmt.Sprint(*got) != fmt.Sprint(gitlet.RepoKindCounts{
		Project: gitlet.RepoCounts{Live: 7, Importing: 1, Active: 5, Archived: 1, Deleted: 1},
		Scratch: gitlet.RepoCounts{Live: 3, Active: 3}}) {
		t.Errorf("a person counts %+v", *got)
	}
}

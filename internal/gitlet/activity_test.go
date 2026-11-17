package gitlet_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the cases' zones, on a host without a zone database

	"github.com/crewlet/crewlet/internal/gitlet"
)

// activityNode is a node whose reader reads at at, with the test chart, and read answers q as ada.
func activityNode(t *testing.T, at time.Time) (*node, func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error)) {
	t.Helper()
	n := newNode(t)
	n.chart = testChart()
	n.readAt.Store(&at)
	return n, func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		q.Asker, q.Fresh = ada, stale
		return n.reader.Read(t.Context(), q)
	}
}

// activityHolds fails the test where the node's push activity is not what its push rows imply.
func (n *node) activityHolds() {
	n.t.Helper()
	if drift := gitlet.ActivityDrift(n.t, n.db); drift != "" {
		n.t.Fatal(drift)
	}
}

// pushOneAt is one push by who, setting a branch of its own, at the broker instant at.
func (n *node) pushOneAt(repoID string, at time.Time, who gitlet.Principal, via gitlet.PushVia) {
	n.t.Helper()
	id := newPushID()
	e := entry(id, set("refs/heads/b-"+id, gitlet.ZeroOID, oidOf(id)))
	e.Principal, e.Via = who, via
	n.pushAt(at, repoID, e)
}

// midnights is the local midnights of loc's days from day, count+1 of them: count days' bounds.
func midnights(loc *time.Location, year int, month time.Month, day, count int) []time.Time {
	out := make([]time.Time, count+1)
	for i := range out {
		out[i] = time.Date(year, month, day+i, 0, 0, 0, 0, loc)
	}
	return out
}

// PUSH DAYS FOLD AT THE ASKER'S MIDNIGHTS: a +5:45 zone's days and a -2:30 zone's across its change to
// -3:30, a 25-hour day, count each push of its own day, a microsecond either side of every bound, on a
// detail's spark, a listing's and a push history's days alike.
// Mutation: fold by UTC days, or place a quarter-hour by `<=` its bound.
func TestPushDaysFoldAtTheAskersMidnights(t *testing.T) {
	t.Parallel()
	n, read := activityNode(t, time.Date(2031, 11, 5, 12, 0, 0, 0, time.UTC))
	for _, zone := range []string{"Asia/Kathmandu", "America/St_Johns"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.ToLower(strings.NewReplacer("/", "-", "_", "").Replace(zone))
		id := n.ensure(name, described(zone), t1).RepoID
		bounds := midnights(loc, 2031, 11, 1, 3)
		long := bounds[2].Sub(bounds[1]) //fixedwait:stated two zone midnights the case names, no clock's
		if zone == "America/St_Johns" && long != 25*time.Hour {
			t.Fatalf("the premise: %s's November 2 is %s long, want its clock change's 25 hours", zone, long)
		}
		// Each day's first microsecond and last, the ones either side outside, and a count a day of its own;
		// two just before the first bound, so a fold shifted by a quarter-hour cannot balance out.
		instants := []time.Time{bounds[0].Add(-time.Microsecond), bounds[0].Add(-time.Microsecond),
			bounds[3], bounds[3], bounds[3],
			bounds[0], bounds[1].Add(-time.Microsecond),
			bounds[1], bounds[1], bounds[1].Add(90 * time.Minute), bounds[2].Add(-time.Microsecond),
			bounds[2], bounds[3].Add(-time.Microsecond)}
		for _, at := range instants {
			n.pushOneAt(id, at, ada, gitlet.ViaGit)
		}
		want := []int64{2, 4, 2}
		detail, err := read(gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: name, RepoFacts: true, DayBounds: bounds})
		if err != nil || detail.Repo == nil || !slices.Equal(detail.Repo.Spark, want) {
			t.Errorf("%s's detail answered %v, %v; want the spark %v", zone, detail.Repo, err, want)
		}
		listing, err := read(gitlet.ReadQuery{Kind: gitlet.ReadRepos, Prefix: name, RepoFacts: true,
			DayBounds: bounds})
		if err != nil || len(listing.Repos) != 1 || !slices.Equal(listing.Repos[0].Spark, want) {
			t.Errorf("%s's listing answered %+v, %v; want its row's spark %v", zone, listing.Repos, err, want)
		}
		history, err := read(gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: name, DayBounds: bounds})
		if err != nil || !slices.Equal(history.PushDays, want) {
			t.Errorf("%s's push history answered the days %v, %v; want %v", zone, history.PushDays, err, want)
		}
	}
}

// A PUSH HISTORY ANSWERS EVERY ONE OF THE ASKER'S DAYS, whatever its page holds, zeros included, and
// refuses them beside one push. Mutation: count the page's rows alone, or leave an empty day out.
func TestAPushHistoryAnswersEveryOneOfTheAskersDays(t *testing.T) {
	t.Parallel()
	n, read := activityNode(t, time.Date(2031, 4, 10, 0, 0, 0, 0, time.UTC))
	id := n.ensure("app", described("one"), t1).RepoID
	bounds := midnights(time.UTC, 2031, 4, 3, 4)
	for i := range 13 {
		n.pushOneAt(id, bounds[1].Add(time.Duration(i)*time.Hour), ada, gitlet.ViaGit)
	}
	for i := range 8 {
		n.pushOneAt(id, bounds[2].Add(time.Duration(i)*time.Hour), ada, gitlet.ViaGit)
	}
	want := []int64{0, 13, 8, 0}
	q := gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "app", DayBounds: bounds, Limit: 10}
	pages := 0
	for {
		page, err := read(q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if !slices.Equal(page.PushDays, want) {
			t.Errorf("page %d answered the days %v, want %v", pages, page.PushDays, want)
		}
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	if pages != 3 {
		t.Errorf("the history read %d pages of 10, want 3", pages)
	}
	none, err := read(gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "app",
		DayBounds: midnights(time.UTC, 2031, 4, 8, 2)})
	if err != nil || !slices.Equal(none.PushDays, []int64{0, 0}) {
		t.Errorf("days holding no push answered %v, %v; want two zeros", none.PushDays, err)
	}
	if _, err = read(gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "app", Push: newPushID(),
		DayBounds: bounds}); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "day_bounds") {
		t.Errorf("one push read with days = %v, want ErrInvalid naming day_bounds", err)
	}
}

// WHO PUSHES HERE IS COUNTED ON WHOLE UTC DAYS: a seat, a person through two credentials as one, and
// Gitlet for a merge, a landing and an import, never the expiry; a push on a day outside them is not.
// Mutation: count the expiry, the merger, or each credential apart.
func TestWhoPushesHereIsCountedOnWholeUTCDays(t *testing.T) {
	t.Parallel()
	n, read := activityNode(t, time.Date(2031, 4, 10, 0, 0, 0, 0, time.UTC))
	id := n.ensure("app", described("one"), t1).RepoID
	// The asker's days begin at 18:15Z: the UTC days that begin within them are April 4 and 5.
	bounds := []time.Time{time.Date(2031, 4, 3, 18, 15, 0, 0, time.UTC), time.Date(2031, 4, 4, 18, 15, 0, 0,
		time.UTC), time.Date(2031, 4, 5, 18, 15, 0, 0, time.UTC)}
	on := time.Date(2031, 4, 4, 9, 0, 0, 0, time.UTC)
	dev := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	ann := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "pat:ann", Seat: "ann"}
	annCI := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:ann-ci", Seat: "ann"}
	system := gitlet.Principal{Kind: gitlet.PrincipalSystem}
	n.pushOneAt(id, on, dev, gitlet.ViaGit)
	n.pushOneAt(id, on, ann, gitlet.ViaGit)
	n.pushOneAt(id, on.Add(24*time.Hour), annCI, gitlet.ViaGit)
	n.pushOneAt(id, on, ann, gitlet.ViaMerge)
	n.pushOneAt(id, on, dev, gitlet.ViaQueue)
	n.pushOneAt(id, on, system, gitlet.ViaImport)
	n.pushOneAt(id, on, system, gitlet.ViaExpiry)
	n.pushOneAt(id, on.Add(-24*time.Hour), dev, gitlet.ViaGit)
	answer, err := read(gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "app", RepoFacts: true, DayBounds: bounds})
	if err != nil || answer.Repo == nil || answer.Repo.Pushers == nil {
		t.Fatalf("the detail answered %+v, %v; want its pushers", answer.Repo, err)
	}
	got := answer.Repo.Pushers
	want := []gitlet.PusherPushes{{Kind: gitlet.PusherGitlet, Pushes: 3}, {Kind: gitlet.PusherPerson,
		Handle: "ann", Pushes: 2}, {Kind: gitlet.PusherAgent, Handle: "dev", Pushes: 1}}
	if !got.From.Equal(time.Date(2031, 4, 4, 0, 0, 0, 0, time.UTC)) ||
		!got.Until.Equal(time.Date(2031, 4, 6, 0, 0, 0, 0, time.UTC)) || !slices.Equal(got.Top, want) ||
		got.Omitted != 0 {
		t.Errorf("the pushers are %+v, want April 4 and 5 counting %+v", got, want)
	}
}

// DAY BOUNDS ARE REFUSED WHERE THEY MEAN NOTHING: on a kind that counts no pushes, without repo_facts on
// repositories, beside one push, too few or too many, off a quarter-hour, out of order, too wide, a
// listing's page too long, at 1970, or older than the history keeps whole, each ErrInvalid naming its
// field or rule; 31 local days across a clock change are answered. Mutation: relax one.
func TestDayBoundsAreRefusedWhereTheyMeanNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 11, 5, 12, 0, 0, 0, time.UTC)
	n, read := activityNode(t, now)
	n.ensure("app", described("one"), t1)
	day := time.Date(2031, 11, 1, 0, 0, 0, 0, time.UTC)
	days := func(count int) []time.Time { return midnights(time.UTC, 2031, 11, 1, count) }
	repo := func(bounds []time.Time) gitlet.ReadQuery {
		return gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "app", RepoFacts: true, DayBounds: bounds}
	}
	// The reader's clock less PushDaysKept is a quarter-hour mark: the oldest bound it answers.
	oldest := now.Add(-gitlet.PushDaysKept)
	for _, tc := range []struct {
		name  string
		q     gitlet.ReadQuery
		field string
	}{
		{"on refs", gitlet.ReadQuery{Kind: gitlet.ReadRefs, Repo: "app", DayBounds: days(1)}, "day_bounds"},
		{"on changes", gitlet.ReadQuery{Kind: gitlet.ReadChanges, Repo: "app", DayBounds: days(1)}, "day_bounds"},
		{"on counts", gitlet.ReadQuery{Kind: gitlet.ReadCounts, Counts: &gitlet.CountsQuery{Changes: true},
			DayBounds: days(1)}, "day_bounds"},
		{"on introduced", gitlet.ReadQuery{Kind: gitlet.ReadIntroduced, DayBounds: days(1)}, "day_bounds"},
		{"on repos without repo_facts", gitlet.ReadQuery{Kind: gitlet.ReadRepos, DayBounds: days(1)}, "day_bounds"},
		{"on a repo without repo_facts", gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "app", DayBounds: days(1)},
			"day_bounds"},
		{"beside one push", gitlet.ReadQuery{Kind: gitlet.ReadPushes, Repo: "app", Push: newPushID(),
			DayBounds: days(1)}, "day_bounds"},
		{"one instant", repo([]time.Time{day}), "day_bounds"},
		{"33 instants", repo(func() []time.Time {
			out := make([]time.Time, 33)
			for i := range out {
				out[i] = day.Add(time.Duration(i) * time.Hour)
			}
			return out
		}()), "day_bounds"},
		{"off a quarter-hour", repo([]time.Time{day.Add(7 * time.Minute), day.Add(24 * time.Hour)}), "day_bounds"},
		{"a nanosecond past a mark", repo([]time.Time{day.Add(time.Nanosecond), day.Add(24 * time.Hour)}),
			"day_bounds"},
		{"two equal instants", repo([]time.Time{day, day}), "day_bounds"},
		{"past 32 days", repo([]time.Time{day.Add(-29 * 24 * time.Hour), day.Add(3*24*time.Hour + 15*time.Minute)}),
			"day_bounds"},
		{"a listing past 15 days", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoFacts: true,
			DayBounds: []time.Time{day.Add(-11 * 24 * time.Hour), day.Add(4*24*time.Hour + 15*time.Minute)}},
			"day_bounds"},
		{"a listing of a page past MaxSparkReposPerPage", gitlet.ReadQuery{Kind: gitlet.ReadRepos, RepoFacts: true,
			DayBounds: days(1), Limit: gitlet.MaxSparkReposPerPage + 1}, "limit"},
		{"at 1970", repo([]time.Time{time.Unix(0, 0), day}), "is not after 1970"},
		{"older than the history keeps whole", repo([]time.Time{oldest.Add(-gitlet.PushSlot),
			oldest.Add(24 * time.Hour)}), "day_bounds"},
	} {
		if _, err := read(tc.q); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: = %v, want ErrInvalid naming %s", tc.name, err, tc.field)
		}
	}
	stJohns, zoneErr := time.LoadLocation("America/St_Johns")
	if zoneErr != nil {
		t.Fatal(zoneErr)
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"a listing of 15 days":               {Kind: gitlet.ReadRepos, RepoFacts: true, DayBounds: midnights(time.UTC, 2031, 10, 20, 15)},
		"a listing of a full page":           {Kind: gitlet.ReadRepos, RepoFacts: true, DayBounds: days(1), Limit: gitlet.MaxSparkReposPerPage},
		"32 days":                            repo(midnights(time.UTC, 2031, 10, 4, 31)),
		"32 midnights across a clock change": repo(midnights(stJohns, 2031, 10, 15, 31)),
		"the oldest the history has":         repo([]time.Time{oldest, oldest.Add(24 * time.Hour)}),
	} {
		if _, err := read(q); err != nil {
			t.Errorf("%s: = %v, want it answered", name, err)
		}
	}
}

// A REPOSITORY LISTING ANSWERS ITS SPARK WHEN ASKED: with repo_facts and day bounds every listed
// repository answers a count a day, zeros for one never pushed, and the detail its pushers; without
// bounds neither; and the page is measured with them. Mutation: read the sparks after the cut.
func TestARepositoryListingAnswersItsSparkWhenAsked(t *testing.T) {
	t.Parallel()
	n, read := activityNode(t, time.Date(2031, 4, 10, 0, 0, 0, 0, time.UTC))
	api := n.ensure("api", described("one"), t1).RepoID
	n.ensure("web", described("two"), t1)
	bounds := midnights(time.UTC, 2031, 4, 3, 3)
	for i := range 4 {
		n.pushOneAt(api, bounds[1].Add(time.Duration(i)*time.Hour), ada, gitlet.ViaGit)
	}
	listing := func(q gitlet.ReadQuery) gitlet.ReadAnswer {
		t.Helper()
		q.Kind = gitlet.ReadRepos
		answer, err := read(q)
		if err != nil {
			t.Fatal(err)
		}
		return answer
	}
	bare := listing(gitlet.ReadQuery{RepoFacts: true})
	for _, r := range bare.Repos {
		if r.ChangeCounts == nil || r.Spark != nil {
			t.Errorf("%s asked its facts without days answered %v and the spark %v", r.Name, r.ChangeCounts, r.Spark)
		}
	}
	asked := listing(gitlet.ReadQuery{RepoFacts: true, DayBounds: bounds})
	want := map[string][]int64{"api": {0, 4, 0}, "web": {0, 0, 0}}
	for _, r := range asked.Repos {
		if !slices.Equal(r.Spark, want[r.Name]) {
			t.Errorf("%s's spark is %v, want %v", r.Name, r.Spark, want[r.Name])
		}
	}
	if len(asked.Repos) != 2 {
		t.Fatalf("the listing answered %d repositories, want both", len(asked.Repos))
	}
	detail := func(q gitlet.ReadQuery) *gitlet.RepoDetail {
		t.Helper()
		q.Kind, q.Repo = gitlet.ReadRepo, "api"
		answer, err := read(q)
		if err != nil {
			t.Fatal(err)
		}
		return answer.Repo
	}
	if d := detail(gitlet.ReadQuery{RepoFacts: true}); d.Pushers != nil || d.Spark != nil {
		t.Errorf("the detail answered its pushers %+v and spark %v unasked", d.Pushers, d.Spark)
	}
	if d := detail(gitlet.ReadQuery{RepoFacts: true, DayBounds: bounds}); d.Pushers == nil ||
		fmt.Sprint(d.Pushers.Top) != "[{person founder 4}]" {
		t.Errorf("the detail asked with days answered the pushers %+v", d.Pushers)
	}

	// A PAGE IS MEASURED WITH ITS SPARKS: a byte bound one summary wide, with them, cuts after one.
	gitlet.SetPageLimits(n.reader, 10, gitlet.SummaryBytes(asked.Repos[0]))
	if got := listing(gitlet.ReadQuery{RepoFacts: true, DayBounds: bounds}); len(got.Repos) != 1 || got.Next == "" {
		t.Errorf("a page bounded to one summary with its spark answered %d of them (next %q), want one",
			len(got.Repos), got.Next)
	}
}

// THE ACTIVITY HOLDS THROUGH THE STORE'S PURGE: the purge the store decides takes a deleted repository's
// pushes by day and by pusher with its pushes, each step leaving the tables what the rows imply, and
// leaves another repository's. Mutation: leave the activity out of the final purge's deletes.
func TestTheActivityHoldsThroughTheStoresPurge(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := n.ensure("app", described("one"), t1).RepoID
	other := n.ensure("other", described("two"), t1).RepoID
	for i, who := range []gitlet.Principal{dev, ada, dev} {
		n.pushOneAt(id, t1.Add(time.Duration(i+1)*10*time.Minute), who, gitlet.ViaGit)
	}
	n.pushOneAt(other, t1.Add(time.Minute), ada, gitlet.ViaGit)
	n.activityHolds()
	n.deleteAt(id, harnessAt.Add(-2*keep))
	n.purgeNow(id)
	activity := func(repo string) int {
		return n.count(`SELECT (SELECT COUNT(*) FROM code_push_days WHERE repo_id = ?)
			+ (SELECT COUNT(*) FROM code_pusher_days WHERE repo_id = ?)`, repo, repo)
	}
	if left := activity(id); left != 0 {
		t.Errorf("the purged repository leaves %d activity rows", left)
	}
	if kept := activity(other); kept != 2 {
		t.Errorf("the purge left another repository %d activity rows, want its day and its pusher", kept)
	}
}

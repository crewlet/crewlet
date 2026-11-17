package gitlet_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// pushedAt is one push by who of via setting a branch of its own, in a record of its own at the broker
// instant at; it answers the push's id.
func (n *node) pushedAt(repoID string, at time.Time, who gitlet.Principal, via gitlet.PushVia) string {
	n.t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	e := entry(id, set("refs/heads/b-"+id, gitlet.ZeroOID, oidOf(id)))
	e.Principal, e.Via = who, via
	n.pushAt(at, repoID, e)
	return id
}

// narrowedPage is one page of a narrowed history: its rows' ids, its cursor and how far it read.
type narrowedPage struct {
	ids  []string
	next string
	scan *gitlet.PushScan
}

// narrowedPages reads repository name's history narrowed by f to its end, limit a page, and answers each
// page; it fails t past max pages.
func narrowedPages(t *testing.T, n *node, name string, f gitlet.PushFilter, limit, most int) []narrowedPage {
	t.Helper()
	var out []narrowedPage
	q := gitlet.ReadQuery{PushFilter: &f, Limit: limit}
	for {
		if len(out) == most {
			t.Fatalf("the history narrowed by %+v read past %d pages", f, most)
		}
		a := historyOf(t, n, name, q)
		p := narrowedPage{next: a.Next, scan: a.PushScan}
		for _, row := range a.Pushes {
			p.ids = append(p.ids, row.PushID)
		}
		out = append(out, p)
		if a.PushScan == nil {
			t.Fatalf("page %d of the history narrowed by %+v answered no push_scan", len(out), f)
		}
		if a.Next == "" {
			return out
		}
		q.Cursor = a.Next
	}
}

// keptOf is every page's rows together.
func keptOf(pages []narrowedPage) []string {
	var out []string
	for _, p := range pages {
		out = append(out, p.ids...)
	}
	return out
}

// newest is repository name's whole history, newest first: each push's id and instant.
func newest(t *testing.T, n *node, name string) (ids []string, at []time.Time) {
	t.Helper()
	q := gitlet.ReadQuery{Limit: gitlet.MaxPushesPerPage}
	for {
		a := historyOf(t, n, name, q)
		for _, row := range a.Pushes {
			ids, at = append(ids, row.PushID), append(at, row.At)
		}
		if a.Next == "" {
			return ids, at
		}
		q.Cursor = a.Next
	}
}

// A NARROWED HISTORY HOLDS WHAT ITS PUSHERS' COUNTS HOLD: each pusher's narrowing answers as many pushes as its count,
// each its own, and the three pushers' and the expiry's together are the whole history.
// Mutation: narrow a seat by actor_seat alone (dev's merge and landing appear under dev).
func TestANarrowedHistoryHoldsWhatItsPushersCountsHold(t *testing.T) {
	t.Parallel()
	day := time.Date(2031, 4, 4, 0, 0, 0, 0, time.UTC)
	n, _ := activityNode(t, day.Add(48*time.Hour))
	id := n.ensure("app", described("one"), t1).RepoID
	on := day.Add(9 * time.Hour)
	devSeat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	founderCI := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:founder-ci", Seat: "founder"}
	system := gitlet.Principal{Kind: gitlet.PrincipalSystem}
	whose := map[string]string{}
	for _, p := range []struct {
		who gitlet.Principal
		via gitlet.PushVia
		as  string
	}{
		{devSeat, gitlet.ViaGit, "dev"}, {devSeat, gitlet.ViaEngine, "dev"}, {devSeat, gitlet.ViaMerge, "gitlet"},
		{devSeat, gitlet.ViaQueue, "gitlet"}, {ada, gitlet.ViaGit, "founder"}, {founderCI, gitlet.ViaGit, "founder"},
		{system, gitlet.ViaExpiry, ""}, {system, gitlet.ViaImport, "gitlet"}, {system, gitlet.ViaMirror, "gitlet"},
	} {
		whose[n.pushedAt(id, on, p.who, p.via)] = p.as
	}
	facts, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: "app", Asker: asker,
		RepoFacts: true, DayBounds: []time.Time{day, day.Add(24 * time.Hour)}})
	if err != nil || facts.Repo == nil || facts.Repo.Pushers == nil {
		t.Fatalf("the detail answered %+v, %v; want its pushers", facts.Repo, err)
	}
	all, _ := newest(t, n, "app")
	var narrowed []string
	for _, p := range facts.Repo.Pushers.Top {
		want := map[gitlet.PusherKind]string{gitlet.PusherAgent: p.Handle, gitlet.PusherPerson: p.Handle,
			gitlet.PusherGitlet: "gitlet"}[p.Kind]
		ids := keptOf(narrowedPages(t, n, "app", gitlet.PushFilter{PusherKind: p.Kind, Pusher: p.Handle}, 0, 5))
		if int64(len(ids)) != p.Pushes {
			t.Errorf("%s %s's narrowed history holds %d pushes, want the %d its count holds", p.Kind, p.Handle,
				len(ids), p.Pushes)
		}
		for _, pushID := range ids {
			if whose[pushID] != want {
				t.Errorf("%s %s's narrowed history holds push %s, which counts for %q", p.Kind, p.Handle, pushID,
					whose[pushID])
			}
		}
		narrowed = append(narrowed, ids...)
	}
	for pushID, as := range whose {
		if as == "" {
			narrowed = append(narrowed, pushID)
		}
	}
	slices.Sort(narrowed)
	slices.Sort(all)
	if len(facts.Repo.Pushers.Top) != 3 || !slices.Equal(narrowed, all) {
		t.Errorf("the pushers %+v and the expiry hold %d pushes, want the whole history's %d once each",
			facts.Repo.Pushers.Top, len(narrowed), len(all))
	}
}

// A NARROWED HISTORY'S SHORT PAGES CARRY A CURSOR TO THE END: dev's 3rd, 14th and 27th newest of forty, four read a
// page, answer [3rd], [], [], [14th], [], [], [27th], each cursor at the last push read; foreign cursors are refused.
// Mutation: drop the end by counts (the seventh page has a cursor), or end a page at its read bound with no cursor.
func TestANarrowedHistorysShortPagesCarryACursorToTheEnd(t *testing.T) {
	t.Parallel()
	base := time.Date(2031, 3, 10, 10, 0, 0, 0, time.UTC)
	n, _ := activityNode(t, base.Add(24*time.Hour))
	id := n.ensure("app", described("one"), t1).RepoID
	devSeat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	for i := range 40 {
		who := cto
		if newer := 39 - i; newer == 2 || newer == 13 || newer == 26 {
			who = devSeat
		}
		n.pushedAt(id, base.Add(time.Duration(i)*time.Minute), who, gitlet.ViaGit)
	}
	ids, at := newest(t, n, "app")
	gitlet.SetPushScanBound(n.reader, 4)
	dev := gitlet.PushFilter{PusherKind: gitlet.PusherAgent, Pusher: "dev"}
	pages := narrowedPages(t, n, "app", dev, 0, 12)
	want := [][]string{{ids[2]}, nil, nil, {ids[13]}, nil, nil, {ids[26]}}
	if len(pages) != len(want) {
		t.Fatalf("dev's narrowed history read %d pages, want %d", len(pages), len(want))
	}
	for i, p := range pages {
		if !slices.Equal(p.ids, want[i]) {
			t.Errorf("page %d answered %v, want %v", i+1, p.ids, want[i])
		}
		switch {
		case i < 6 && (p.next == "" || !p.scan.To.Equal(at[4*i+3]) || !strings.Contains(p.next, ids[4*i+3])):
			t.Errorf("page %d answered next %q and %+v, want a cursor at push %d, the last it read", i+1, p.next,
				p.scan, 4*i+4)
		case i == 6 && (p.next != "" || !p.scan.To.IsZero()):
			t.Errorf("the last page answered next %q and %+v, want none: dev's count is met", p.next, p.scan)
		}
	}
	none := narrowedPages(t, n, "app", gitlet.PushFilter{PusherKind: gitlet.PusherPerson, Pusher: "founder"}, 0, 2)
	if len(none) != 1 || len(none[0].ids) != 0 || !none[0].scan.To.IsZero() {
		t.Errorf("a person with no push answered %+v, want one empty page with no cursor", none)
	}
	cursor := pages[0].next
	for name, q := range map[string]gitlet.ReadQuery{
		"dev's cursor on founder's narrowing": {PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherPerson,
			Pusher: "founder"}, Cursor: cursor},
		"a plain cursor on dev's": {PushFilter: &dev, Cursor: fmt.Sprintf("%d/%s", 1, ids[4])},
		"dev's cursor beside a window": {PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherAgent,
			Pusher: "dev", Since: base}, Cursor: cursor},
		"dev's cursor on a window alone":    {PushFilter: &gitlet.PushFilter{Since: base}, Cursor: cursor},
		"dev's cursor on the whole history": {Cursor: cursor},
		"dev's cursor with -1 pending": {PushFilter: &dev,
			Cursor: cursor[:strings.LastIndex(cursor, "/")] + "/-1"},
	} {
		q.Kind, q.Repo, q.Asker = gitlet.ReadPushes, "app", asker
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, gitlet.ErrBadCursor) {
			t.Errorf("%s = %v, want ErrBadCursor", name, err)
		}
	}
}

// A WINDOW IS JUDGED BY EACH PUSH'S INSTANT ACROSS A LEADER CHANGE: at every scan bound, the four stamped in [since,
// until), newest first in log order; one stamped out of order past PushInstantSkew lies below the stop, as documented.
// Mutation: stop at the first push stamped before since (+10m is lost).
func TestAWindowIsJudgedByEachPushsInstantAcrossALeaderChange(t *testing.T) {
	t.Parallel()
	since := time.Date(2031, 3, 10, 10, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	n, _ := activityNode(t, since.Add(24*time.Hour))
	devSeat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	stamps := []time.Duration{-4 * time.Hour, -3 * time.Hour, 10 * time.Minute, -time.Hour - 49*time.Minute,
		30 * time.Minute, time.Hour - time.Microsecond, time.Hour, time.Hour + 20*time.Minute, 50 * time.Minute,
		3*time.Hour + 30*time.Minute}
	seed := func(name string, extra bool) map[time.Duration]string {
		id := n.ensure(name, described("one"), t1).RepoID
		out := map[time.Duration]string{}
		for i, d := range stamps {
			out[d] = n.pushedAt(id, since.Add(d), devSeat, gitlet.ViaGit)
			if extra && i == 1 {
				out[40*time.Minute] = n.pushedAt(id, since.Add(40*time.Minute), devSeat, gitlet.ViaGit)
				out[-2*time.Hour-30*time.Minute] = n.pushedAt(id, since.Add(-2*time.Hour-30*time.Minute), devSeat,
					gitlet.ViaGit)
			}
		}
		return out
	}
	pushes, beyond := seed("app", false), seed("far", true)
	want := []string{pushes[50*time.Minute], pushes[time.Hour-time.Microsecond], pushes[30*time.Minute],
		pushes[10*time.Minute]}
	window := gitlet.PushFilter{Since: since, Until: until}
	devWindow := gitlet.PushFilter{PusherKind: gitlet.PusherAgent, Pusher: "dev", Since: since, Until: until}
	for bound := 1; bound <= 10; bound++ {
		gitlet.SetPushScanBound(n.reader, bound)
		for name, f := range map[string]gitlet.PushFilter{"the window": window, "dev's window": devWindow} {
			if got := keptOf(narrowedPages(t, n, "app", f, 0, 20)); !slices.Equal(got, want) {
				t.Errorf("%s at a bound of %d answered %v, want %v", name, bound, got, want)
			}
		}
		farWant := []string{beyond[50*time.Minute], beyond[time.Hour-time.Microsecond], beyond[30*time.Minute],
			beyond[10*time.Minute]}
		for name, f := range map[string]gitlet.PushFilter{"the far window": window, "dev's far window": devWindow} {
			if got := keptOf(narrowedPages(t, n, "far", f, 0, 20)); !slices.Equal(got, farWant) {
				t.Errorf("%s at a bound of %d answered %v, want %v, the +40m push below the stop", name, bound,
					got, farWant)
			}
		}
	}
	gitlet.SetPushScanBound(n.reader, 1)
	pages := narrowedPages(t, n, "app", window, 0, 20)
	if last := pages[len(pages)-1]; len(pages) != 8 || len(last.ids) != 0 {
		t.Errorf("one push a page read %d pages, the last %+v; want 8, the -3h push's ending the walk", len(pages),
			last)
	}
	gitlet.SetPushScanBound(n.reader, gitlet.MaxPushesScanned)
	all, _ := newest(t, n, "far")
	devAll := keptOf(narrowedPages(t, n, "far", gitlet.PushFilter{PusherKind: gitlet.PusherAgent, Pusher: "dev"},
		0, 5))
	if len(all) != 12 || !slices.Equal(devAll, all) || !slices.Contains(all, beyond[40*time.Minute]) {
		t.Errorf("dev's history alone answered %v, want all twelve in log order: %v", devAll, all)
	}
}

// A NARROWED HISTORY JUMPS OVER WHAT NO KEPT PUSH CAN BE: a person's two pushes among six days of others' come on pages
// 9 and 17 of 17 (87 unnarrowed), a day's window jumps below the next day's 02:00, and shifted stamps are still found.
// Mutation: no day jump (87 pages), a day jump with no margin (the 23:30 push is lost), or no window jump.
func TestANarrowedHistoryJumpsOverWhatNoKeptPushCanBe(t *testing.T) {
	t.Parallel()
	day1 := time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC)
	n, _ := activityNode(t, day1.Add(10*24*time.Hour))
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	at := func(day int, h, m int) time.Time {
		return day1.Add(time.Duration(day-1)*24*time.Hour + time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	seed := func(name string, shifted bool) (person []string, late string) {
		id := n.ensure(name, described("one"), t1).RepoID
		for i := range 6 * 144 {
			stamp := day1.Add(time.Duration(i) * 10 * time.Minute)
			n.pushedAt(id, stamp, cto, gitlet.ViaGit)
			switch {
			case stamp.Equal(at(1, 12, 0)), !shifted && stamp.Equal(at(5, 12, 0)):
				person = append([]string{n.pushedAt(id, stamp, ada, gitlet.ViaGit)}, person...)
			case shifted && stamp.Equal(at(5, 1, 0)):
				person = append([]string{n.pushedAt(id, at(4, 23, 30), ada, gitlet.ViaGit)}, person...)
			case shifted && stamp.Equal(at(3, 0, 40)):
				late = n.pushedAt(id, at(2, 23, 55), cto, gitlet.ViaGit)
			}
		}
		return person, late
	}
	person, _ := seed("app", false)
	shiftedPerson, late := seed("late", true)
	gitlet.SetPushScanBound(n.reader, 10)
	founder := gitlet.PushFilter{PusherKind: gitlet.PusherPerson, Pusher: "founder"}
	pages := narrowedPages(t, n, "app", founder, 0, 100)
	if len(pages) != 17 || !slices.Equal(pages[8].ids, person[:1]) || !slices.Equal(pages[16].ids, person[1:]) ||
		len(keptOf(pages)) != 2 {
		var where []int
		var to []string
		for i, p := range pages {
			if len(p.ids) > 0 {
				where = append(where, i+1)
			}
			to = append(to, p.scan.To.Format("02 15:04"))
		}
		t.Errorf("the person's history read %d pages, their pushes on pages %v; want 17, on pages 9 and 17 "+
			"(read down to %v)", len(pages), where, to)
	}
	day2 := gitlet.PushFilter{Since: at(2, 0, 0), Until: at(3, 0, 0)}
	first := historyOf(t, n, "app", gitlet.ReadQuery{PushFilter: &day2})
	if len(first.Pushes) != 0 || first.Next == "" || first.PushScan == nil || !first.PushScan.To.Equal(at(3, 0, 20)) {
		t.Errorf("the 2nd day's first page answered %d rows, next %q, scan %+v; want none, read down to the 3rd "+
			"day's 00:20", len(first.Pushes), first.Next, first.PushScan)
	}
	second := historyOf(t, n, "app", gitlet.ReadQuery{PushFilter: &day2, Cursor: first.Next})
	if len(second.Pushes) == 0 || !second.Pushes[0].At.Equal(at(2, 23, 50)) {
		t.Errorf("the 2nd day's second page begins %+v, want its 23:50 push", second.Pushes)
	}
	if got := historyOf(t, n, "late", gitlet.ReadQuery{PushFilter: &day2}); !slices.ContainsFunc(got.Pushes,
		func(row gitlet.PushRow) bool { return row.PushID == late }) {
		t.Errorf("the 2nd day's first page of the shifted history misses its 23:55 push stamped after the 3rd " +
			"day's 00:40")
	}
	if got := keptOf(narrowedPages(t, n, "late", founder, 0, 100)); !slices.Equal(got, shiftedPerson) {
		t.Errorf("the person's shifted history answered %v, want both their pushes %v", got, shiftedPerson)
	}
}

// A PARTIAL DAY IS READ THROUGH BEFORE A DAY JUMP: until is noon of the 4th day, dev's Y stamped 00:30 that day
// lands below others' P stamped 23:00 the day before, and a page ends at P; the next answers Y before dev's 1st day.
// Mutation: hold the partial day open only from its own midnight, with no margin (the jump from P loses Y).
func TestAPartialDayIsReadThroughBeforeADayJump(t *testing.T) {
	t.Parallel()
	day1 := time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC)
	at := func(day int, h, m int) time.Time {
		return day1.Add(time.Duration(day-1)*24*time.Hour + time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	n, _ := activityNode(t, at(10, 0, 0))
	id := n.ensure("app", described("one"), t1).RepoID
	devSeat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	d1 := n.pushedAt(id, at(1, 10, 0), devSeat, gitlet.ViaGit)
	for i := range 5 {
		n.pushedAt(id, at(2, 10, i), cto, gitlet.ViaGit)
	}
	y := n.pushedAt(id, at(4, 0, 30), devSeat, gitlet.ViaGit)
	p := n.pushedAt(id, at(3, 23, 0), cto, gitlet.ViaGit)
	n.pushedAt(id, at(4, 1, 0), cto, gitlet.ViaGit)
	n.pushedAt(id, at(4, 1, 1), cto, gitlet.ViaGit)
	gitlet.SetPushScanBound(n.reader, 3)
	pages := narrowedPages(t, n, "app", gitlet.PushFilter{PusherKind: gitlet.PusherAgent, Pusher: "dev",
		Until: at(4, 12, 0)}, 0, 10)
	if len(pages) == 0 || len(pages[0].ids) != 0 || !strings.Contains(pages[0].next, p) {
		t.Fatalf("the first page is %+v, want no row and a cursor at P %s", pages, p)
	}
	if got := keptOf(pages); !slices.Equal(got, []string{y, d1}) {
		t.Errorf("dev's history before noon of the 4th day answered %v, want Y %s and the 1st day's %s", got, y, d1)
	}
}

// A NARROWED HISTORY STAYS EXACT ACROSS THE HORIZON: pushes 200 days old are found, a cursor taken before a horizon ends
// at the oldest push left, and pushes stamped days out of place, past the bound, are found where their counts say.
// Mutation: carry the count left in the cursor (it reads past N1), or end where pending goes below zero (P3 is lost).
func TestANarrowedHistoryStaysExactAcrossTheHorizon(t *testing.T) {
	t.Parallel()
	start := time.Date(2031, 1, 1, 9, 0, 0, 0, time.UTC)
	days := func(d int) time.Time { return start.Add(time.Duration(d) * 24 * time.Hour) }
	n, _ := activityNode(t, days(200))
	id := n.ensure("app", described("one"), t1).RepoID
	devSeat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	o1 := n.pushedAt(id, days(0), devSeat, gitlet.ViaGit)
	o2 := n.pushedAt(id, days(0).Add(time.Minute), devSeat, gitlet.ViaGit)
	for i := range 12 {
		n.pushedAt(id, days(70).Add(time.Duration(i)*time.Minute), cto, gitlet.ViaGit)
	}
	n1 := n.pushedAt(id, days(160), devSeat, gitlet.ViaGit)
	for i := range 3 {
		n.pushedAt(id, days(165).Add(time.Duration(i)*time.Minute), cto, gitlet.ViaGit)
	}
	n2 := n.pushedAt(id, days(169), devSeat, gitlet.ViaGit)
	gitlet.SetPushScanBound(n.reader, 8)
	dev := gitlet.PushFilter{PusherKind: gitlet.PusherAgent, Pusher: "dev"}
	if got := keptOf(narrowedPages(t, n, "app", dev, 0, 10)); !slices.Equal(got, []string{n2, n1, o2, o1}) {
		t.Errorf("dev's history answered %v, want N2, N1, O2 and O1", got)
	}
	first := historyOf(t, n, "app", gitlet.ReadQuery{PushFilter: &dev, Limit: 1})
	if len(first.Pushes) != 1 || first.Pushes[0].PushID != n2 || first.Next == "" {
		t.Fatalf("dev's first page of one answered %+v, next %q; want N2 and a cursor", first.Pushes, first.Next)
	}
	// THE HORIZON: a record past PushHistory of O1 and O2 deletes them.
	n.pushedAt(id, days(0).Add(gitlet.PushHistory+time.Hour), cto, gitlet.ViaGit)
	if left := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id IN (?, ?)`, o1, o2); left != 0 {
		t.Fatalf("%d of O1 and O2 are left past the horizon, want none", left)
	}
	next := historyOf(t, n, "app", gitlet.ReadQuery{PushFilter: &dev, Limit: 1, Cursor: first.Next})
	if len(next.Pushes) != 1 || next.Pushes[0].PushID != n1 || next.Next != "" {
		t.Errorf("the page after the horizon answered %+v, next %q; want N1 and the end", next.Pushes, next.Next)
	}
	if got := keptOf(narrowedPages(t, n, "app", dev, 0, 10)); !slices.Equal(got, []string{n2, n1}) {
		t.Errorf("dev's history after the horizon answered %v, want N2 and N1", got)
	}

	// PAST THE BOUND: P2 stamped three days ahead among day 100's rows, and in another repository P4 three
	// days ahead among day 40's, below P3.
	late := n.ensure("late", described("one"), t1).RepoID
	ahead := n.ensure("ahead", described("one"), t1).RepoID
	p4 := ""
	for i := range 6 {
		n.pushedAt(ahead, days(40).Add(time.Duration(i)*time.Minute), cto, gitlet.ViaGit)
		if i == 2 {
			p4 = n.pushedAt(ahead, days(43), devSeat, gitlet.ViaGit)
		}
	}
	var p1, p2, p3, q1, q3 string
	p3, q3 = n.pushedAt(late, days(50), devSeat, gitlet.ViaGit), n.pushedAt(ahead, days(50), devSeat, gitlet.ViaGit)
	for i := range 20 {
		n.pushedAt(late, days(100).Add(time.Duration(i)*time.Minute), cto, gitlet.ViaGit)
		n.pushedAt(ahead, days(100).Add(time.Duration(i)*time.Minute), cto, gitlet.ViaGit)
		if i == 10 {
			p2 = n.pushedAt(late, days(103), devSeat, gitlet.ViaGit)
		}
	}
	p1, q1 = n.pushedAt(late, days(100).Add(time.Hour), devSeat, gitlet.ViaGit),
		n.pushedAt(ahead, days(100).Add(time.Hour), devSeat, gitlet.ViaGit)
	if got := keptOf(narrowedPages(t, n, "late", dev, 0, 20)); !slices.Equal(got, []string{p1, p2, p3}) {
		t.Errorf("dev's history with P2 stamped ahead answered %v, want P1, P2 and P3", got)
	}
	if got := keptOf(narrowedPages(t, n, "ahead", dev, 0, 20)); !slices.Equal(got, []string{q1, q3, p4}) {
		t.Errorf("dev's history with P4 stamped ahead below P3 answered %v, want P1, P3 and P4", got)
	}
}

// A PUSH LANDING BETWEEN PAGES LOSES NO OLDER PUSH: dev's X stamped after midnight, then Y stamped before it
// (a step back of 30 minutes), read one a page: Y, then X with no cursor, whether or not dev pushed between.
// Mutation: start a later page with nothing pending (X is lost).
func TestAPushLandingBetweenPagesLosesNoOlderPush(t *testing.T) {
	t.Parallel()
	midnight := time.Date(2031, 3, 11, 0, 0, 0, 0, time.UTC)
	n, _ := activityNode(t, midnight.Add(48*time.Hour))
	devSeat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	dev := gitlet.PushFilter{PusherKind: gitlet.PusherAgent, Pusher: "dev"}
	for _, between := range []bool{true, false} {
		name := fmt.Sprintf("app-%t", between)
		id := n.ensure(name, described("one"), t1).RepoID
		x := n.pushedAt(id, midnight.Add(20*time.Minute), devSeat, gitlet.ViaGit)
		n.pushedAt(id, midnight.Add(21*time.Minute), cto, gitlet.ViaGit)
		y := n.pushedAt(id, midnight.Add(-10*time.Minute), devSeat, gitlet.ViaGit)
		for i := range 3 {
			n.pushedAt(id, midnight.Add(30*time.Minute+time.Duration(i)*time.Second), cto, gitlet.ViaGit)
		}
		first := historyOf(t, n, name, gitlet.ReadQuery{PushFilter: &dev, Limit: 1})
		if len(first.Pushes) != 1 || first.Pushes[0].PushID != y || first.Next == "" {
			t.Fatalf("%s's first page answered %+v, next %q; want Y and a cursor", name, first.Pushes, first.Next)
		}
		if between {
			n.pushedAt(id, midnight.Add(40*time.Minute), devSeat, gitlet.ViaGit)
		}
		next := historyOf(t, n, name, gitlet.ReadQuery{PushFilter: &dev, Limit: 1, Cursor: first.Next})
		if len(next.Pushes) != 1 || next.Pushes[0].PushID != x || next.Next != "" {
			t.Errorf("%s's second page answered %+v, next %q; want X and the end", name, next.Pushes, next.Next)
		}
	}
}

// A PUSH FILTER IS REFUSED BY FIELD: each narrowing that means nothing is ErrInvalid naming its field, and a
// window, a pusher, both, and day bounds inside a window are read.
// Mutation: drop any one case of validatePushFilter.
func TestAPushFilterIsRefusedByField(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	since := time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(48 * time.Hour)
	days := []time.Time{since, since.Add(24 * time.Hour), until}
	read := func(q gitlet.ReadQuery) error {
		q.Kind, q.Repo, q.Asker = gitlet.ReadPushes, "app", asker
		_, err := n.reader.Read(t.Context(), q)
		return err
	}
	for name, tc := range map[string]struct {
		q     gitlet.ReadQuery
		field string
	}{
		"beside one push": {gitlet.ReadQuery{Push: newPushID(), PushFilter: &gitlet.PushFilter{Since: since}},
			"push_filter:"},
		"naming nothing": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{}}, "push_filter:"},
		"a pusher with no kind": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{Pusher: "dev"}},
			"push_filter.pusher:"},
		"a kind it does not know": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{PusherKind: "robot",
			Pusher: "dev"}}, "push_filter.pusher_kind:"},
		"Gitlet with a handle": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherGitlet,
			Pusher: "dev"}}, "push_filter.pusher:"},
		"a seat with no handle": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherAgent}},
			"push_filter.pusher:"},
		"a person by no seat's handle": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{
			PusherKind: gitlet.PusherPerson, Pusher: "Not A Handle"}}, "push_filter.pusher:"},
		"since at 1970": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{Since: time.Unix(0, 0)}},
			"push_filter.since:"},
		"until before 1970": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{Until: time.Unix(-5, 0)}},
			"push_filter.until:"},
		"since finer than a microsecond": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{
			Since: since.Add(time.Nanosecond)}}, "push_filter.since:"},
		"until finer than a microsecond": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{
			Until: until.Add(time.Nanosecond)}}, "push_filter.until:"},
		"until not after since": {gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{Since: since, Until: since}},
			"push_filter.until:"},
		"days beside a pusher": {gitlet.ReadQuery{DayBounds: days, PushFilter: &gitlet.PushFilter{
			PusherKind: gitlet.PusherAgent, Pusher: "dev"}}, "day_bounds:"},
		"days before since": {gitlet.ReadQuery{DayBounds: days, PushFilter: &gitlet.PushFilter{
			Since: since.Add(time.Hour)}}, "day_bounds:"},
		"days after until": {gitlet.ReadQuery{DayBounds: days, PushFilter: &gitlet.PushFilter{
			Until: until.Add(-time.Hour)}}, "day_bounds:"},
	} {
		if err := read(tc.q); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), ": "+tc.field) {
			t.Errorf("%s = %v, want ErrInvalid naming %s", name, err, tc.field)
		}
	}
	for name, q := range map[string]gitlet.ReadQuery{
		"a window":             {PushFilter: &gitlet.PushFilter{Since: since, Until: until}},
		"a pusher":             {PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherGitlet}},
		"both":                 {PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherAgent, Pusher: "dev", Until: until}},
		"days inside a window": {DayBounds: days, PushFilter: &gitlet.PushFilter{Since: since, Until: until}},
	} {
		if err := read(q); err != nil {
			t.Errorf("%s = %v, want it read", name, err)
		}
	}
}

// A NARROWED PAGE READS NO MORE THAN ITS SCAN BOUND: dev's push below thirty others', ten read a page, comes after three
// empty pages each with a cursor at the push it read last; a page of three pushes' bytes reads three.
// Mutation: leave the reads uncounted, end a page at its read bound with no cursor, or weigh a window by its count.
func TestANarrowedPageReadsNoMoreThanItsScanBound(t *testing.T) {
	t.Parallel()
	base := time.Date(2031, 3, 10, 10, 0, 0, 0, time.UTC)
	n, _ := activityNode(t, base.Add(24*time.Hour))
	id := n.ensure("app", described("one"), t1).RepoID
	devSeat := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	devPush := n.pushedAt(id, base, devSeat, gitlet.ViaGit)
	for i := range 30 {
		n.pushedAt(id, base.Add(time.Duration(i+1)*time.Minute), cto, gitlet.ViaGit)
	}
	ids, at := newest(t, n, "app")
	gitlet.SetPushScanBound(n.reader, 10)
	for name, f := range map[string]gitlet.PushFilter{
		"dev":                 {PusherKind: gitlet.PusherAgent, Pusher: "dev"},
		"dev's push's window": {Since: base, Until: base.Add(time.Microsecond)},
	} {
		pages := narrowedPages(t, n, "app", f, 0, 8)
		if len(pages) != 4 || !slices.Equal(pages[3].ids, []string{devPush}) || pages[3].next != "" {
			t.Errorf("%s read %d pages, the last %+v; want 4, dev's push on the last with no cursor", name,
				len(pages), pages[len(pages)-1])
			continue
		}
		for i, p := range pages[:3] {
			if last := 10*i + 9; len(p.ids) != 0 || !strings.Contains(p.next, ids[last]) || !p.scan.To.Equal(at[last]) {
				t.Errorf("%s's page %d answered %v, next %q, %+v; want none and a cursor at push %d", name, i+1, p.ids,
					p.next, p.scan, last+1)
			}
		}
	}
	gitlet.SetPushScanBound(n.reader, gitlet.MaxPushesScanned)
	weight := n.count(`SELECT row_bytes FROM code_pushes WHERE push_id = ?`, ids[0])
	gitlet.SetPageLimits(n.reader, gitlet.MaxSnapshotItems, 3*weight)
	a := historyOf(t, n, "app", gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherAgent,
		Pusher: "dev"}})
	if len(a.Pushes) != 0 || !strings.Contains(a.Next, ids[2]) {
		t.Errorf("a page of three pushes' bytes answered %d rows and next %q, want none and a cursor at the 3rd",
			len(a.Pushes), a.Next)
	}
}

// TestTheScanBoundKeepsANarrowedPageQuick pins MaxPushesScanned at the bound that keeps every narrowed page under a
// tenth of the read budget (BenchmarkANarrowedPushHistory); a raise re-measures that.
func TestTheScanBoundKeepsANarrowedPageQuick(t *testing.T) {
	t.Parallel()
	if gitlet.MaxPushesScanned > 5_000 {
		t.Fatalf("MaxPushesScanned is %d: past 5 000 pushes a page that jumps at every push it keeps passes a "+
			"tenth of the read budget; re-measure BenchmarkANarrowedPushHistory before raising it",
			gitlet.MaxPushesScanned)
	}
}

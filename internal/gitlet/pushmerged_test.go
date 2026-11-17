package gitlet_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// historyOf is repository name's push history as asker reads it, q filling in the rest.
func historyOf(t *testing.T, n *node, name string, q gitlet.ReadQuery) gitlet.ReadAnswer {
	t.Helper()
	q.Kind, q.Repo, q.Asker = gitlet.ReadPushes, name, asker
	a, err := n.reader.Read(t.Context(), q)
	if err != nil {
		t.Fatalf("read %s's pushes: %v", name, err)
	}
	return a
}

// rowOf is the row of rows whose push id is id, nil where none is.
func rowOf(rows []gitlet.PushRow, id string) *gitlet.PushRow {
	for i := range rows {
		if rows[i].PushID == id {
			return &rows[i]
		}
	}
	return nil
}

// rowSetting is the row of rows a command of which set ref to commit, nil where none did.
func rowSetting(rows []gitlet.PushRow, ref, commit string) *gitlet.PushRow {
	for i := range rows {
		for _, c := range rows[i].Commands {
			if c.Ref == ref && c.New == commit && c.Result == gitlet.ResultOK {
				return &rows[i]
			}
		}
	}
	return nil
}

// namesChange fails t unless row names repository repo's change number with title, merged, and more others.
func namesChange(t *testing.T, what string, row *gitlet.PushRow, repo string, number int64, title string, more int) {
	t.Helper()
	want := gitlet.ChangeName{Repo: repo, Ref: gitlet.ChangeRef(repo, number), Title: title,
		State: gitlet.ChangeStateMerged}
	switch {
	case row == nil:
		t.Errorf("%s: no row", what)
	case row.Change == nil || *row.Change != want || row.ChangesMore != more:
		t.Errorf("%s names %+v and %d more, want %+v and %d more", what, row.Change, row.ChangesMore, want, more)
	}
}

// namesNone fails t unless row names no change.
func namesNone(t *testing.T, what string, row *gitlet.PushRow) {
	t.Helper()
	switch {
	case row == nil:
		t.Errorf("%s: no row", what)
	case row.Change != nil || row.ChangesMore != 0:
		t.Errorf("%s names %+v and %d more, want none: its record merged nothing on that branch", what,
			row.Change, row.ChangesMore)
	}
}

// A PUSH ROW NAMES THE CHANGE ITS OWN RECORD MERGED on a branch it set: a merge's, each landing's, a push's (the
// lowest-numbered first), never a later record's, another branch's or the home's landing's; brief, whole and narrowed alike.
// Mutation: keep a found change at another position or on another branch, rank by number alone, or gather after brief().
func TestAPushRowNamesTheChangeItsOwnRecordMerged(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	ctx := t.Context()

	// A DIRECT MERGE, then main moved away and set back to its commit by later records.
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	repo, change := mergeRepo(t, n, c, "web", gitlet.DirectPersons, mergeGate())
	approved(t, n, w, repo, change)
	converse(t, w, n.merging(repo, change, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	merge := merging(repo, change, dev, gitlet.MergeMethodMerge, oidOf("A"), oidOf("B"), oidOf("M"))
	res, err := c.ProposeMerge(ctx, merge)
	requireMerged(t, res, err, oidOf("M"))
	away, back := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	n.push(repo, entry(away, set(mainRef, oidOf("M"), oidOf("X"))))
	n.push(repo, entry(back, set(mainRef, oidOf("X"), oidOf("M"))))
	rows := historyOf(t, n, "web", gitlet.ReadQuery{}).Pushes
	namesChange(t, "the direct merge's push", rowOf(rows, merge.Merge.MergeID), "web", 1, "Retry the upload", 0)
	namesNone(t, "the push setting main back to the merge's commit", rowOf(rows, back))
	namesNone(t, "the push moving main away", rowOf(rows, away))

	// A FAST-FORWARD MERGE of the higher-numbered of two changes at one head: the other merges beside it.
	ff, ffChange := mergeRepo(t, n, c, "ff", gitlet.DirectPersons, mergeGate())
	n.push(ff, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/also", gitlet.ZeroOID, oidOf("B"))))
	second, err := c.ProposeChange(ctx, opening(ff, "refs/heads/also", mainRef, dev))
	requireChanged(t, second, err)
	approved(t, n, w, ff, second.ChangeID)
	converse(t, w, n.merging(ff, second.ChangeID, 1, oidOf("B"), mainRef, gitlet.MergeClean))
	fast := merging(ff, second.ChangeID, dev, gitlet.MergeMethodFastForward, oidOf("A"), oidOf("B"), oidOf("B"))
	res, err = c.ProposeMerge(ctx, fast)
	requireMerged(t, res, err, oidOf("B"))
	if state, _, _ := n.merged(ffChange); state != string(gitlet.ChangeStateMerged) {
		t.Fatalf("ff!1 is %s, want merged by the fast-forward's push beside ff!2", state)
	}
	namesChange(t, "the fast-forward's push", rowOf(historyOf(t, n, "ff", gitlet.ReadQuery{}).Pushes,
		fast.Merge.MergeID), "ff", 2, "Retry the upload", 1)

	// A TWO-ENTRY QUEUE LANDING: each landing's push names its own change.
	landedTrain(t, n, c, w, "api")
	rows = historyOf(t, n, "api", gitlet.ReadQuery{}).Pushes
	namesChange(t, "the landing of M1", rowSetting(rows, mainRef, oidOf("M1")), "api", 1, "Retry the upload", 0)
	namesChange(t, "the landing of M2", rowSetting(rows, mainRef, oidOf("M2")), "api", 2, "Retry the upload", 0)

	// A PERSON'S PUSHES: one change at a head, then two at one head; and in one record, another branch set to
	// the commit beside the push that merged.
	pc, _ := committerOn(t, n, n.pub)
	app := changeRepo(t, n, "app")
	opened, err := pc.ProposeChange(ctx, opening(app, featureRef, mainRef, dev))
	requireChanged(t, opened, err)
	merger, beside := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	n.push(app, entry(merger, set(mainRef, oidOf("A"), oidOf("B"))),
		entry(beside, set("refs/heads/copy", gitlet.ZeroOID, oidOf("B"))))
	n.push(app, entry(uuid.Must(uuid.NewV7()).String(), set(featureRef, oidOf("B"), oidOf("C")),
		set(releaseRef, oidOf("R"), oidOf("C"))))
	for _, source := range []string{featureRef, releaseRef} {
		o, err := pc.ProposeChange(ctx, opening(app, source, mainRef, dev))
		requireChanged(t, o, err)
	}
	both := uuid.Must(uuid.NewV7()).String()
	n.push(app, entry(both, set(mainRef, oidOf("B"), oidOf("C"))))
	rows = historyOf(t, n, "app", gitlet.ReadQuery{}).Pushes
	namesChange(t, "a person's push setting main to a change's head", rowOf(rows, merger), "app", 1,
		"Retry the upload", 0)
	namesNone(t, "the push of the merging record setting another branch to the head", rowOf(rows, beside))
	namesChange(t, "a person's push setting main to two changes' head", rowOf(rows, both), "app", 2,
		"Retry the upload", 1)

	// A PUSH PAST A CHANGE'S HEAD, which the home's landing merged afterwards: no push names it.
	home := changeRepo(t, n, "home")
	landed, err := pc.ProposeChange(ctx, opening(home, featureRef, mainRef, dev))
	requireChanged(t, landed, err)
	past := uuid.Must(uuid.NewV7()).String()
	n.push(home, entry(past, set(mainRef, oidOf("A"), oidOf("P"))))
	if _, err = pc.ProposeChange(ctx, landing(home, landed.ChangeID, 1, oidOf("B"), mainRef, oidOf("P"))); err != nil {
		t.Fatal(err)
	}
	if state, via, at := n.merged(landed.ChangeID); state != string(gitlet.ChangeStateMerged) || at != oidOf("P") ||
		via != string(gitlet.MergedViaPush) {
		t.Fatalf("home!1 is %s via %s at %s, want merged by its home's landing at P", state, via, at)
	}
	namesNone(t, "the push past a change's head the home landed", rowOf(historyOf(t, n, "home",
		gitlet.ReadQuery{}).Pushes, past))

	// IN BRIEF, ONE PUSH WHOLE, AND NARROWED: the merging command the seventh of seven.
	brief := changeRepo(t, n, "brief")
	cut, err := pc.ProposeChange(ctx, opening(brief, featureRef, mainRef, dev))
	requireChanged(t, cut, err)
	var cmds []gitlet.Command
	for i := range 6 {
		cmds = append(cmds, set(fmt.Sprintf("refs/heads/b%d", i), gitlet.ZeroOID, oidOf(fmt.Sprint("b", i))))
	}
	seventh := uuid.Must(uuid.NewV7()).String()
	n.push(brief, entry(seventh, append(cmds, set(mainRef, oidOf("A"), oidOf("B")))...))
	inBrief := rowOf(historyOf(t, n, "brief", gitlet.ReadQuery{Brief: true}).Pushes, seventh)
	if inBrief == nil || rowSetting([]gitlet.PushRow{*inBrief}, mainRef, oidOf("B")) != nil {
		t.Fatalf("the brief row is %+v, want one whose merging command is cut", inBrief)
	}
	namesChange(t, "the push read in brief", inBrief, "brief", 1, "Retry the upload", 0)
	namesChange(t, "the push read whole", rowOf(historyOf(t, n, "brief", gitlet.ReadQuery{Push: seventh}).Pushes,
		seventh), "brief", 1, "Retry the upload", 0)
	namesChange(t, "the push on a narrowed page", rowOf(historyOf(t, n, "brief", gitlet.ReadQuery{
		PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherPerson, Pusher: "harness"}}).Pushes, seventh),
		"brief", 1, "Retry the upload", 0)
}

// A PUSH HISTORY PAGE IS CUT AGAIN ONCE ITS CHANGES ARE NAMED: a page whose bytes are two named rows' holds two, the
// next begins at the third, and narrowed to Gitlet the pages answer each merge once with a cursor at the cut row.
// Mutation: drop the recut (three rows, past the bytes), or end the cut page at the walk's own place (a merge is lost).
func TestAPushHistoryPageIsCutAgainOnceItsChangesAreNamed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	heads := []string{oidOf("c0"), oidOf("c1"), oidOf("c2")}
	for i, head := range heads {
		source := fmt.Sprintf("refs/heads/c%d", i)
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(source, gitlet.ZeroOID, head)))
		o, err := c.ProposeChange(t.Context(), opening(repo, source, mainRef, dev))
		requireChanged(t, o, err)
	}
	var ids []string
	tip := oidOf("A")
	for _, head := range heads {
		id := uuid.Must(uuid.NewV7()).String()
		e := entry(id, set(mainRef, tip, head))
		e.Principal, e.Via = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}, gitlet.ViaMerge
		n.push(repo, e)
		ids, tip = append([]string{id}, ids...), head
	}
	whole := historyOf(t, n, "web", gitlet.ReadQuery{Limit: 3}).Pushes
	var lengths []int
	for i, row := range whole {
		if row.PushID != ids[i] || row.Change == nil {
			t.Fatalf("the history's row %d is %+v, want merge %s naming its change", i, row, ids[i])
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		lengths = append(lengths, len(encoded))
	}
	gitlet.SetPageLimits(n.reader, 50, lengths[0]+lengths[1])
	first := historyOf(t, n, "web", gitlet.ReadQuery{})
	if len(first.Pushes) != 2 || first.Pushes[1].PushID != ids[1] || first.Next == "" {
		t.Fatalf("the page answered %d rows and next %q, want the two newest merges and a cursor", len(first.Pushes),
			first.Next)
	}
	if next := historyOf(t, n, "web", gitlet.ReadQuery{Cursor: first.Next}); len(next.Pushes) == 0 ||
		next.Pushes[0].PushID != ids[2] {
		t.Errorf("the next page begins %+v, want the oldest merge", next.Pushes)
	}
	var got []string
	q := gitlet.ReadQuery{PushFilter: &gitlet.PushFilter{PusherKind: gitlet.PusherGitlet}}
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("the narrowed history never ended")
		}
		a := historyOf(t, n, "web", q)
		for _, row := range a.Pushes {
			got = append(got, row.PushID)
		}
		if pages == 0 && (len(a.Pushes) != 2 || a.Next == "" || a.PushScan == nil ||
			!a.PushScan.To.Equal(a.Pushes[1].At)) {
			t.Errorf("the narrowed first page answered %d rows, next %q, scan %+v, want two and a cursor at the "+
				"second", len(a.Pushes), a.Next, a.PushScan)
		}
		if a.Next == "" {
			break
		}
		q.Cursor = a.Next
	}
	if fmt.Sprint(got) != fmt.Sprint(ids) {
		t.Errorf("the narrowed pages answered %v, want each merge once, newest first: %v", got, ids)
	}
}

// A NARROWED PAGE THE RECUT CUTS RESUMES WITH WHAT IT HAD PENDING: Gitlet's M2 and M1 on the 5th day and M0 on the 1st,
// others' pushes between; the page cut at M1 is followed by one answering M0 and no cursor, its jump taken at once.
// Mutation: record the count left at each kept row rather than its pending (the next page reads on to the bottom).
func TestANarrowedPageTheRecutCutsResumesWithWhatItHadPending(t *testing.T) {
	t.Parallel()
	day1 := time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC)
	n, _ := activityNode(t, day1.Add(10*24*time.Hour))
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	heads := []string{oidOf("c0"), oidOf("c1"), oidOf("c2")}
	for i, head := range heads {
		source := fmt.Sprintf("refs/heads/c%d", i)
		n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set(source, gitlet.ZeroOID, head)))
		o, err := c.ProposeChange(t.Context(), opening(repo, source, mainRef, dev))
		requireChanged(t, o, err)
	}
	cto := gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "cto"}
	merge := func(at time.Time, from, to string) string {
		id := uuid.Must(uuid.NewV7()).String()
		e := entry(id, set(mainRef, from, to))
		e.Principal, e.Via = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"}, gitlet.ViaMerge
		n.pushAt(at, repo, e)
		return id
	}
	for i := range 20 {
		n.pushedAt(repo, day1.Add(10*time.Hour+time.Duration(i)*time.Minute), cto, gitlet.ViaGit)
	}
	m0 := merge(day1.Add(12*time.Hour), oidOf("A"), heads[0])
	for i := range 30 {
		n.pushedAt(repo, day1.Add(2*24*time.Hour+10*time.Hour+time.Duration(i)*time.Minute), cto, gitlet.ViaGit)
	}
	m1 := merge(day1.Add(4*24*time.Hour+12*time.Hour), heads[0], heads[1])
	m2 := merge(day1.Add(4*24*time.Hour+12*time.Hour+time.Minute), heads[1], heads[2])
	var length int
	for _, row := range historyOf(t, n, "web", gitlet.ReadQuery{Limit: 2}).Pushes {
		encoded, err := json.Marshal(row)
		if err != nil || row.Change == nil {
			t.Fatalf("the merge row %+v is not named (%v)", row, err)
		}
		length += len(encoded)
	}
	gitlet.SetPageLimits(n.reader, 50, length)
	gitlet.SetPushScanBound(n.reader, 5)
	pages := narrowedPages(t, n, "web", gitlet.PushFilter{PusherKind: gitlet.PusherGitlet}, 0, 8)
	if len(pages) != 2 || fmt.Sprint(pages[0].ids) != fmt.Sprint([]string{m2, m1}) || pages[0].next == "" ||
		fmt.Sprint(pages[1].ids) != fmt.Sprint([]string{m0}) || pages[1].next != "" {
		t.Errorf("Gitlet's history answered %+v, want [M2 M1] with a cursor, then [M0] with none (M2 %s, M1 %s, "+
			"M0 %s)", pages, m2, m1, m0)
	}
}

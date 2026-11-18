package gitlet_test

import (
	"database/sql"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM A CHANGE RECORD REACHED (apply_notices.go).

// storedBefore has n apply every record as stored age before the harness's
// batch instant, whatever the broker stamped it.
func storedBefore(n *node, age time.Duration) {
	n.storedAt = func(uint64, time.Time) time.Time { return harnessAt.Add(-age) }
}

// noticesOf is every notice n holds of the record at p, as
// "recipient:reason:kind".
func noticesOf(t *testing.T, n *node, p statelog.Position) []string {
	t.Helper()
	rec := n.recordAt(p)
	var out []string
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT recipient, reason, recipient_kind, change_id, log_seq
			FROM code_notifications WHERE record_id = ? ORDER BY recipient`, rec.OpID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var recipient, reason, kind, change string
			var seq int64
			if err := rows.Scan(&recipient, &reason, &kind, &change, &seq); err != nil {
				return err
			}
			if seq != p.Packed() || change == "" {
				t.Errorf("the notice of %s for %s names change %q at %d, want the record's change at %d",
					rec.OpID, recipient, change, seq, p.Packed())
			}
			out = append(out, recipient+":"+reason+":"+kind)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the notices of %s: %v", rec.OpID, err)
	}
	return out
}

// wantNotices is what the record at p concerns and tells, as noticesOf names
// it.
func wantNotices(t *testing.T, n *node, p statelog.Position) []string {
	t.Helper()
	concerns, err := gitlet.Candidates(n.recordAt(p))
	if err != nil {
		t.Fatalf("the concerns of the record at %s: %v", p, err)
	}
	var out []string
	for _, c := range concerns {
		if !c.Hears() {
			continue
		}
		kind := gitlet.PartyAgent
		if c.Change.Person(c.Handle) {
			kind = gitlet.PartyPerson
		}
		out = append(out, c.Handle+":"+string(c.Reason)+":"+string(kind))
	}
	slices.Sort(out)
	return out
}

// EVERY ROUTED RECORD WRITES ONE ROW PER RECIPIENT: an open, an ask, a
// comment, a review and a merging push each write exactly the recipients their
// routing tells, once, a person's marked a person's, at the record's position;
// none of their own write; and a record that tells nobody writes none.
//
// Mutation: write a row for the record's actor too, or one per concern before
// the precedence's add-once, and a record's rows are not its recipients.
func TestEveryRoutedRecordWritesOneRowPerRecipient(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "cto"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	change := opened.ChangeID
	w, _ := writerOn(t, n, n.pub)

	positions := map[string]statelog.Position{"open": opened.Position}
	positions["ask"] = converse(t, w, asking(repo, change, dev, "plat-lead")).Position
	said := converse(t, w, saying(repo, change, ada, "", "Why retry twice?"))
	positions["reply"] = converse(t, w, saying(repo, change, dev, said.ID, "It races.")).Position
	positions["review"] = converse(t, w, reviewing(repo, change, platLead, 1, gitlet.VerdictApprove, nil)).Position
	merged, err := c.Propose(t.Context(), proposal(repo, update(mainRef, oidOf("A"), oidOf("B"))))
	if err != nil || merged.Position.IsZero() {
		t.Fatalf("the merging push answered %+v, %v", merged, err)
	}
	positions["merge"] = merged.Position
	want := map[string][]string{
		"open":   {"cto:mention:agent", "founder:mention:person"},
		"ask":    {"plat-lead:review_requested:agent"},
		"reply":  {"founder:thread:person"},
		"review": {"dev:review_submitted:agent"},
		"merge":  {"dev:merged:agent"},
	}
	for what, p := range positions {
		got := noticesOf(t, n, p)
		if !slices.Equal(got, want[what]) || !slices.Equal(got, wantNotices(t, n, p)) {
			t.Errorf("the %s wrote the notices %v, want %v — its routing's %v", what, got, want[what],
				wantNotices(t, n, p))
		}
	}
	if got := noticesOf(t, n, said.Position); len(got) != 0 {
		t.Errorf("a comment that starts a thread nobody else is in wrote %v, want nothing", got)
	}
}

// A SEAT'S NOTICES AGE OUT AT THE TURN-DETAIL HORIZON: a record forty days old
// is past a seat's horizon (thirty days) and inside a person's (a year by
// default), so it writes the person's notice alone; and each kind's sweep
// deletes only its own kind's rows past their cutoff.
//
// Mutation: hold a seat's notice to the person's horizon → the forty-day
// record writes the seat's; sweep every kind at one cutoff → the person's
// row goes with the seats'.
func TestSeatRowsAgeOutAtTheTurnDetailHorizon(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, 40*24*time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "cto"}
	old, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, old, err)
	if got := noticesOf(t, n, old.Position); !slices.Equal(got, []string{"founder:mention:person"}) {
		t.Fatalf("a record forty days old wrote %v, want the person's notice alone", got)
	}

	storedBefore(n, 10*24*time.Hour)
	recent := opening(repo, releaseRef, mainRef, dev)
	recent.Open.Mentions = []string{"founder", "cto"}
	fresh, err := c.ProposeChange(t.Context(), recent)
	requireChanged(t, fresh, err)
	if got := noticesOf(t, n, fresh.Position); len(got) != 2 {
		t.Fatalf("a record ten days old wrote %v, want both notices", got)
	}
	jobs := gitlet.NoticeJobs(n.db, maintenance.Fixed(365*24*time.Hour))
	byName := map[string]maintenance.Job{}
	for _, j := range jobs {
		byName[j.Name] = j
		if j.Scope != maintenance.NodeLocal {
			t.Errorf("the %s sweep is %v, want per node: the table is divergent", j.Name, j.Scope)
		}
	}
	seats := byName["code_notifications_agents"]
	if seats.Horizon == nil || seats.Horizon() != gitlet.SeatNoticeHorizon || gitlet.SeatNoticeHorizon != 30*24*time.Hour {
		t.Fatalf("the seats' sweep keeps %v, want the turn-detail horizon of thirty days", seats.Horizon)
	}
	swept, err := seats.Run(t.Context(), harnessAt, harnessAt.Add(-5*24*time.Hour))
	if err != nil || swept != 1 {
		t.Fatalf("the seats' sweep deleted %d (%v), want the one seat's notice", swept, err)
	}
	if got := noticesOf(t, n, fresh.Position); !slices.Equal(got, []string{"founder:mention:person"}) {
		t.Fatalf("after the seats' sweep the record holds %v, want the person's notice", got)
	}
	swept, err = byName["code_notifications"].Run(t.Context(), harnessAt, harnessAt.Add(-5*24*time.Hour))
	if err != nil || swept != 2 {
		t.Fatalf("the persons' sweep deleted %d (%v), want both persons' notices", swept, err)
	}
}

// A RECORD PAST ITS HORIZON WRITES NO ROW: one stored longer ago than a
// person's inbox horizon when its batch applies it writes no notice, so a
// replay rebuilds no inbox the sweep would delete.
//
// Mutation: date the horizon from the record's own instant rather than the
// batch's → the record writes its notices.
func TestARecordPastItsHorizonWritesNoRow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, (inbox.RetentionDefaultDays+1)*24*time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "cto"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	if got := noticesOf(t, n, opened.Position); len(got) != 0 {
		t.Fatalf("a record past every horizon wrote %v, want nothing", got)
	}
}

// THE CODE APPLIER ANNOUNCES WHOSE INBOX MOVED: after a committed batch that
// wrote notices, its listener hears one movement a recipient (source code, the
// change, the reason, timed at the record's broker instant, which the socket's
// push is timed from); a batch that wrote none tells nothing.
//
// Mutation: announce from inside the transaction, or drop the source, and the
// movements are not these.
func TestTheCodeApplierAnnouncesWhoseInboxMoved(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	var (
		mu    sync.Mutex
		heard [][]inbox.Movement
	)
	// TOLD ON THE APPLY'S OWN GOROUTINE, the committer's here.
	n.applier = gitlet.NewApplier(func(moved []inbox.Movement) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, moved)
	})
	told := func() [][]inbox.Movement {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(heard)
	}
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	if heard := told(); len(heard) != 0 {
		t.Fatalf("records that route nobody announced %v", heard)
	}
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "cto"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	stored := harnessAt.Add(-time.Hour)
	want := []inbox.Movement{
		{Source: "code", Handle: "cto", UnreadDelta: 1, Subject: opened.ChangeID, Reason: "mention", At: stored},
		{Source: "code", Handle: "founder", UnreadDelta: 1, Subject: opened.ChangeID, Reason: "mention", At: stored},
	}
	if heard := told(); len(heard) != 1 || !slices.Equal(heard[0], want) {
		t.Fatalf("the open announced %v, want one batch %v", heard, want)
	}
	retitle, err := c.ProposeChange(t.Context(), retitled(repo, opened.ChangeID, dev, 1, "Retry the upload once"))
	requireChanged(t, retitle, err)
	if heard := told(); len(heard) != 1 {
		t.Fatalf("an edit that mentions nobody announced %v", heard[1:])
	}
}

// noticeCount is change's code_changes.notices on n.
func noticeCount(t *testing.T, n *node, change string) int {
	t.Helper()
	return n.count(`SELECT notices FROM code_changes WHERE id = ?`, change)
}

// A CHANGE COUNTS EVERY NOTICE ITS RECORDS MAY WRITE, whether or not the
// horizon let this node write the row, so the count is the same on every node
// and never below its rows: an open past every horizon writes no row and still
// counts its two mentions, an ask an hour old writes and counts its one, and an
// edit that tells nobody counts nothing.
//
// Mutation: count only the rows the horizon let the apply write, or count
// nothing, and the change's count is not what its records may write.
func TestAChangeCountsEveryNoticeItsRecordsMayWrite(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, (inbox.RetentionDefaultDays+1)*24*time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder", "cto"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	change := opened.ChangeID
	if rows := noticesOf(t, n, opened.Position); len(rows) != 0 {
		t.Fatalf("an open past every horizon wrote %v, want no row", rows)
	}
	told := len(wantNotices(t, n, opened.Position))
	if told != 2 {
		t.Fatalf("the open tells %d handles, want its two mentions", told)
	}
	if got := noticeCount(t, n, change); got != told {
		t.Fatalf("after an open that wrote no row the change counts %d notices, want the %d it tells: "+
			"the count is the records', never the horizon's", got, told)
	}
	storedBefore(n, time.Hour)
	w, _ := writerOn(t, n, n.pub)
	asked := converse(t, w, asking(repo, change, dev, "plat-lead")).Position
	told += len(wantNotices(t, n, asked))
	if rows := noticesOf(t, n, asked); len(rows) != 1 {
		t.Fatalf("the ask wrote %v, want the one reviewer's notice", rows)
	}
	retitle, err := c.ProposeChange(t.Context(), retitled(repo, change, dev, 1, "Retry the upload once"))
	requireChanged(t, retitle, err)
	if got := noticeCount(t, n, change); got != told || got != 3 {
		t.Fatalf("after the open, the ask and an edit that tells nobody the change counts %d notices, "+
			"want the %d its records tell", got, told)
	}
}

// A PURGE REMOVES ITS CHANGES' NOTICES WITHIN ITS PARTS, charged by count: no
// record deletes more than PurgePartRows; a cut takes the oldest and exactly
// its charge off the count; the final purge forms once the counts left fit a
// part; and the purge leaves no notice of the repository, announces every
// inbox it moved, and touches no other's. Ten changes of forty notices fit
// the first part (430 rows); the next holds PurgePartRows+100 and counts 50
// more a sweep took, so the part cuts its revision, author and 2 568 oldest;
// the purge takes the 532 left (counted 582), its row and five small changes.
//
// Mutation: leave notices out of a part's charge or the decide's bounded
// count, remove none of a whole or cut change, take none off the cut's count,
// cut newest first, or note no removal, and the parts or movements differ.
func TestAPurgeRemovesItsChangesNoticesWithinItsParts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var (
		mu    sync.Mutex
		moved = map[string]bool{}
	)
	n.applier = gitlet.NewApplier(func(batch []inbox.Movement) {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range batch {
			if m.UnreadDelta != 0 {
				t.Errorf("a purge announced %d unread notices for %s, want a removal's none", m.UnreadDelta,
					m.Handle)
			}
			moved[m.Handle] = true
		}
	})
	n.ensure("app", described("one"), t1)
	n.ensure("other", described("two"), t1)
	id, other := n.mustID("app"), n.mustID("other")
	n.deleteAt(id, harnessAt.Add(-2*keep))
	small := changeShape{revisions: 1, authors: 1, notices: 40}
	shapes := append(changesOf(10, small),
		changeShape{revisions: 1, authors: 1, notices: gitlet.PurgePartRows + 100, swept: 50})
	shapes = append(shapes, changesOf(5, changeShape{revisions: 1, authors: 1, notices: 10})...)
	ids := n.seedChanges(id, shapes)
	cut := ids[10]
	n.seedChanges(other, changesOf(2, changeShape{revisions: 1, authors: 1, notices: 30}))
	notices := func(repo string) int {
		return n.count(`SELECT COUNT(*) FROM code_notifications WHERE repo_id = ?`, repo)
	}
	left := func() int {
		changes, revisions, authors, bodies := n.changeRows(id)
		return changes + revisions + authors + bodies + notices(id)
	}
	var steps []int
	for range 4 {
		before := left()
		res, err := n.store.PurgeStep(t.Context(), id, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		steps = append(steps, before-left())
		if len(steps) == 1 {
			if got, want := noticeCount(t, n, cut), gitlet.PurgePartRows+150-2568; got != want {
				t.Errorf("after the first part the cut change counts %d notices, want %d: a cut takes off "+
					"its count exactly what it charged", got, want)
			}
			if oldest := n.count(`SELECT MIN(log_seq) FROM code_notifications WHERE change_id = ?`,
				cut); oldest != 2568+1 {
				t.Errorf("after the first part the cut change's oldest notice is at %d, want %d: a cut takes "+
					"a change's oldest notices first", oldest, 2568+1)
			}
		}
		if res.Done {
			break
		}
	}
	if want := []int{gitlet.PurgePartRows, 532 + 1 + 5*13}; !slices.Equal(steps, want) {
		t.Fatalf("the purge removed %v rows of the changes and their notices a record, want %v: parts of "+
			"at most PurgePartRows (%d), each change's notices charged by its count, then the purge",
			steps, want, gitlet.PurgePartRows)
	}
	if left := notices(id); left != 0 {
		t.Fatalf("the purge left %d notices of the repository's changes", left)
	}
	if left := notices(other); left != 60 {
		t.Fatalf("the neighbour holds %d notices after the purge, want the 60 it was given", left)
	}
	mu.Lock()
	defer mu.Unlock()
	for j := range 16 {
		if reader := noticeReader(id, j); !moved[reader] {
			t.Errorf("the purge took %s's notices out of their inbox and never said it moved", reader)
		}
		if reader := noticeReader(other, j); moved[reader] {
			t.Errorf("the purge announced %s's inbox, whose notices are another repository's", reader)
		}
	}
}

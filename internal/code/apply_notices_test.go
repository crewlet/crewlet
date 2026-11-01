package code_test

import (
	"database/sql"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHOM A CHANGE RECORD REACHED (apply_notices.go): the applier writes one row
// per recipient of every routed record, within each recipient's horizon, and
// says after the commit whose inbox it moved.

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
	concerns, err := code.Candidates(n.recordAt(p))
	if err != nil {
		t.Fatalf("the concerns of the record at %s: %v", p, err)
	}
	var out []string
	for _, c := range concerns {
		if !c.Hears() {
			continue
		}
		kind := code.PartyAgent
		if c.Change.Person(c.Handle) {
			kind = code.PartyPerson
		}
		out = append(out, c.Handle+":"+string(c.Reason)+":"+string(kind))
	}
	slices.Sort(out)
	return out
}

// EVERY ROUTED RECORD WRITES ONE ROW PER RECIPIENT: an open mentioning a person
// and a seat, an ask for review, a comment, a review, a merging push — each
// record's notices are exactly the recipients its routing tells, each once,
// a person's marked a person's, at the record's own position; nobody's of
// their own write; and a record that tells nobody writes none.
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
	positions["review"] = converse(t, w, reviewing(repo, change, platLead, 1, code.VerdictApprove, nil)).Position
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
// when its batch applies it is past a seat's horizon (store.EventHistory,
// thirty days) and inside a person's (the company's inbox horizon, a year by
// default), so it writes the person's notice and not the seat's; and the
// node's sweep of each kind deletes only its own kind's rows past their
// cutoff.
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
	jobs := code.NoticeJobs(n.db, maintenance.Fixed(365*24*time.Hour))
	byName := map[string]maintenance.Job{}
	for _, j := range jobs {
		byName[j.Name] = j
		if j.Scope != maintenance.NodeLocal {
			t.Errorf("the %s sweep is %v, want per node: the table is divergent", j.Name, j.Scope)
		}
	}
	seats := byName["code_notifications_agents"]
	if seats.Horizon == nil || seats.Horizon() != code.SeatNoticeHorizon || code.SeatNoticeHorizon != 30*24*time.Hour {
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
// person's inbox horizon when its batch applies it — a replay of a log a year
// and more old — writes no notice for anybody, so a replay rebuilds no inbox
// the sweep would delete.
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

// THE CODE APPLIER ANNOUNCES WHOSE INBOX MOVED: after a committed batch whose
// notices it wrote, the applier tells its listener one movement a recipient —
// the code domain as its source, a notice each, the change and the reason —
// and a batch that wrote none tells nothing.
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
	n.applier = code.NewApplier(func(moved []inbox.Movement) {
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
	want := []inbox.Movement{
		{Source: "code", Handle: "cto", UnreadDelta: 1, Subject: opened.ChangeID, Reason: "mention"},
		{Source: "code", Handle: "founder", UnreadDelta: 1, Subject: opened.ChangeID, Reason: "mention"},
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

// noticeCount is the count change keeps of its notices on n
// (code_changes.notices).
func noticeCount(t *testing.T, n *node, change string) int {
	t.Helper()
	return n.count(`SELECT notices FROM code_changes WHERE id = ?`, change)
}

// A CHANGE COUNTS EVERY NOTICE ITS RECORDS MAY WRITE: each record about it adds
// every handle it tells — what its routing concerns and hears — whether or not
// the recipient's horizon let this node write the row, so the count is a
// function of the records alone and the same on every node, and never below
// the rows this node holds: an open a year and more old when its batch
// applies it writes no row and still counts its two mentions, and an ask
// applied an hour after it was stored writes its one and counts it. An edit
// that tells nobody counts nothing.
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

// A PURGE REMOVES ITS CHANGES' NOTICES WITHIN ITS PARTS: a change's notices
// are charged to the part that removes them by the count the change keeps of
// them, so no record deletes more than PurgePartRows of a repository's
// changes' rows and notices together, a part cuts the change it stops at
// through its notices oldest first and takes exactly what it charged off its
// count, the final purge forms only once the counts left fit one part — and
// the purge leaves no notice of the repository behind, announces every inbox
// it took one out of, and touches no other repository's.
//
// The repository's ten changes of forty notices fit the first part whole, 430
// rows; the change after them holds PurgePartRows+100 notices and counts 50
// more a sweep took, so the part cuts it — its revision, its author and the
// 2 568 oldest of its notices, filling PurgePartRows — and the purge takes the
// 532 notices left (counted 582), its row and the five small changes after
// it, thirteen rows each.
//
// Mutation: leave a change's notices out of what a part charges, and the
// first part deletes every notice in one record; leave them out of the
// decide's bounded count, and the purge is one record over all of them;
// remove no notice of a change taken whole, or none of the cut one, and
// notices outlive their changes or the purge's one record takes thousands;
// take the cut's notices off its count by none, and the parts are not these;
// cut them newest first, and the
// oldest outlive the part; note no removal, and no inbox is told it moved.
func TestAPurgeRemovesItsChangesNoticesWithinItsParts(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	var (
		mu    sync.Mutex
		moved = map[string]bool{}
	)
	n.applier = code.NewApplier(func(batch []inbox.Movement) {
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
		changeShape{revisions: 1, authors: 1, notices: code.PurgePartRows + 100, swept: 50})
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
			if got, want := noticeCount(t, n, cut), code.PurgePartRows+150-2568; got != want {
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
	if want := []int{code.PurgePartRows, 532 + 1 + 5*13}; !slices.Equal(steps, want) {
		t.Fatalf("the purge removed %v rows of the changes and their notices a record, want %v: parts of "+
			"at most PurgePartRows (%d), each change's notices charged by its count, then the purge",
			steps, want, code.PurgePartRows)
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

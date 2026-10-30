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

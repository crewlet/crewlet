package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT A SEAT'S DAY COUNTS OF ITS CHANGES AND REVIEWS: `change_opened` and
// `review_submitted`, which a node's day of usage counts as a seat's
// changes_opened, reviews_given and the findings its changes received
// (store.DB.UsageForDay). An event is in one node's log, so each is announced
// once, by the node whose committer (an open) or change writer (a review)
// landed the record, after the write's answer is handed back: never by an
// applier, which every data node runs, nor for a call the operation ledger
// collapsed (statelog.Result's Collapsed) or a push its batch found already
// landed. An ambiguous landing is thus at worst not announced, which costs the
// day one count and leaves the rows on the code log whole.

// opening is one change a write opened, to announce: its id, and whether a
// push's options opened it.
type opening struct {
	changeID string
	byPush   bool
}

// agentOf is the agent id a seat's day is keyed by (ADR-0013), "" for a
// person, a seat the chart does not hold, or a node that resolves none. Only a
// process holding the chart can say it, so the engine hands its resolver in
// ([CommitterOptions.AgentIDs]), read from the live epoch when announced.
func agentOf(agentIDs func(string) (string, bool), handle string, kind PartyKind) string {
	if kind != PartyAgent || agentIDs == nil || handle == "" {
		return ""
	}
	if id, ok := agentIDs(handle); ok {
		return id
	}
	return ""
}

// announcedName is a repository's name as an announcement names it: its id
// where the row is gone — a deletion purged after the record applied.
func announcedName(ctx context.Context, tx *sql.Tx, repoID string) (string, error) {
	name, found, err := repoNameIn(ctx, tx, repoID)
	if err != nil || !found {
		return repoID, err
	}
	return name, nil
}

// openedEvents reads each opened change's row and forms its event: a change
// this node no longer holds — purged under the announcement — announces
// nothing.
func openedEvents(ctx context.Context, db store.ReplicatedReader, agentIDs func(string) (string, bool),
	repoID string, opened []opening) ([]types.ChangeOpened, error) {

	var out []types.ChangeOpened
	err := db.Read(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		name, err := announcedName(ctx, tx, repoID)
		if err != nil {
			return err
		}
		for _, o := range opened {
			row, found, err := changeRowIn(ctx, tx, o.changeID)
			if err != nil {
				return err
			}
			if !found || row.repoID != repoID {
				continue
			}
			out = append(out, types.ChangeOpened{RepoID: repoID, RepoName: name, ChangeID: row.id,
				Number: row.number, Target: row.target, Title: row.title, Owner: row.owner,
				OwnerKind: string(row.ownerKind), Agent: agentOf(agentIDs, row.owner, row.ownerKind),
				ByPush: o.byPush})
		}
		return nil
	})
	return out, err
}

// reviewRowSQL is one review's row by its id, as its announcement reads it:
// who gave it, its verdict, how many findings it raised and the revision it
// judged.
const reviewRowSQL = `SELECT change_id, reviewer, reviewer_kind, verdict, findings, revision
	FROM code_reviews WHERE id = ?`

// reviewEvent reads a review's row, its change's and its repository's name and
// forms its event, false for a review this node no longer holds.
func reviewEvent(ctx context.Context, db store.ReplicatedReader, agentIDs func(string) (string, bool),
	repoID, reviewID string) (types.ReviewSubmitted, bool, error) {

	var (
		e     types.ReviewSubmitted
		found bool
	)
	err := db.Read(ctx, func(tx *sql.Tx) error {
		var reviewerKind string
		switch err := tx.QueryRowContext(ctx, reviewRowSQL, reviewID).Scan(&e.ChangeID, &e.Reviewer, &reviewerKind,
			&e.Verdict, &e.Findings, &e.Revision); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		row, held, err := changeRowIn(ctx, tx, e.ChangeID)
		if err != nil || !held || row.repoID != repoID {
			return err
		}
		if e.RepoName, err = announcedName(ctx, tx, repoID); err != nil {
			return err
		}
		found = true
		e.RepoID, e.Number, e.ReviewID = repoID, row.number, reviewID
		e.ReviewerKind, e.Agent = reviewerKind, agentOf(agentIDs, e.Reviewer, PartyKind(reviewerKind))
		e.Owner, e.OwnerKind = row.owner, string(row.ownerKind)
		e.OwnerAgent = agentOf(agentIDs, row.owner, row.ownerKind)
		return nil
	})
	return e, found, err
}

// announceOn publishes one event on a node's event stream, as nodeID, stamped
// when — the instant its record's answer was handed back — bounded by
// [checkEventBudget] under ctx.
func announceOn[T any, P events.PayloadPtr[T]](ctx context.Context, pub EventPublisher, nodeID string,
	when time.Time, data T) error {

	ev := events.New[T, P](data, events.TraceContext{})
	ev.Source, ev.Timestamp = nodeID, when
	return pub.Publish(ctx, topics.Event(ev.Type), ev)
}

// unannounced logs an announcement that did not reach the event stream: the
// day of usage that counts it is one short, and the rows on the code log
// still hold what it announced.
func unannounced(ctx context.Context, log *slog.Logger, kind, repoID, id string, err error) {
	log.WarnContext(ctx, kind+"_unpublished", "repo", repoID, "id", id, "error", err.Error(),
		"detail", "this node's event log and activity feed do not hold it, nor does a day of usage that "+
			"counts it; its rows on the code log still hold it")
}

// announcement is one landed record's events, handed to the committer's
// announcer ([Committer.announceLater]): publish reads the record's rows and
// publishes its events stamped when, under one [checkEventBudget] for the read
// and every publish; kind, repoID and id name it in the log if it is dropped.
type announcement struct {
	kind, repoID, id string
	when             time.Time
	publish          func(ctx context.Context, when time.Time)
}

// errAnnouncerFull is why an announcement is dropped: the announcer holds as
// many unpublished as it may.
var errAnnouncerFull = errors.New("the announcer holds as many records' announcements not yet published as it " +
	"may (CommitterAnnouncements): the event stream is not keeping up with this node's landings")

// announceLater hands a to the announcer and returns at once. A repository's
// goroutine is its only writer on this node, and a read plus a publish the
// stream acknowledges would hold its next push for up to [checkEventBudget].
// A full queue ([CommitterAnnouncements]) drops a, logged, never waited for:
// the cost of a refused publish, the day one count and the feed one line. a
// is stamped now by the committer's clock ([CommitterOptions.Now]), the one
// its records are authored at, so a backlog crossing midnight still counts on
// the day the record landed. Called only from a repository's goroutine, which
// Stop waits for before it closes the queue, so no send meets a closed one.
func (c *Committer) announceLater(a announcement) {
	if c.announcements == nil {
		return
	}
	a.when = c.now()
	select {
	case c.announcements <- a:
	default:
		unannounced(c.life, c.log, a.kind, a.repoID, a.id, errAnnouncerFull)
	}
}

// announcer publishes every announcement handed to it, in landing order, each
// under one [checkEventBudget] of life, the committer's lifetime, until Stop
// closes its queue and it has drained it; should Stop's wait end first, every
// remaining publish is refused at once, logged. One announcer serves every
// repository and keeps each one's events in landing order: records (an
// opening, a merge, a queue's ejections) come at the rate changes do, not
// pushes, which one publish at a time keeps up with.
func (c *Committer) announcer(life context.Context) {
	defer close(c.announcerDone)
	for a := range c.announcements {
		ctx, cancel := context.WithTimeout(life, checkEventBudget)
		a.publish(ctx, a.when)
		cancel()
	}
}

// announceOpenings hands the announcer the changes a write of repoID opened.
func (c *Committer) announceOpenings(repoID string, opened []opening) {
	if len(opened) == 0 {
		return
	}
	c.announceLater(announcement{kind: "change_opened", repoID: repoID, id: opened[0].changeID,
		publish: func(ctx context.Context, when time.Time) { c.announceOpened(ctx, when, repoID, opened) }})
}

// announceOpened announces the changes a write of repoID opened, read from
// this node's rows, under ctx.
func (c *Committer) announceOpened(ctx context.Context, when time.Time, repoID string, opened []opening) {
	evs, err := openedEvents(ctx, c.db, c.agentIDs, repoID, opened)
	if err != nil {
		unannounced(ctx, c.log, "change_opened", repoID, opened[0].changeID,
			fmt.Errorf("read the opened changes: %w", err))
		return
	}
	for _, e := range evs {
		if err := announceOn(ctx, c.events, c.nodeID, when, e); err != nil {
			unannounced(ctx, c.log, "change_opened", repoID, e.ChangeID, err)
		}
	}
}

// announceReview announces the review cw landed, read from this node's rows,
// under the change writer's own lifetime and one [checkEventBudget].
func (w *ChangeWriter) announceReview(cw ConversationWrite) {
	if w.events == nil || cw.Review == nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.flights.life, checkEventBudget)
	defer cancel()
	reviewID := ReviewID(cw.OpID)
	e, found, err := reviewEvent(ctx, w.db, w.agentIDs, cw.RepoID(), reviewID)
	switch {
	case err != nil:
		unannounced(ctx, w.log, "review_submitted", cw.RepoID(), reviewID, fmt.Errorf("read the review: %w", err))
	case found:
		if err := announceOn(ctx, w.events, w.nodeID, time.Now().UTC(), e); err != nil {
			unannounced(ctx, w.log, "review_submitted", cw.RepoID(), reviewID, err)
		}
	}
}

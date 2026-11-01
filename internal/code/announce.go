package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// WHAT A SEAT'S DAY COUNTS OF ITS CHANGES AND REVIEWS: the events a change's
// opening and a review's landing announce (`change_opened`,
// `review_submitted`), which a node's day of usage counts — a seat's
// `changes_opened` and `reviews_given`, and the findings its own changes
// received (store.DB.UsageForDay).
//
// # Announced once, by the node that landed the record
//
// A count read off the replicated rows would be read on every data node; an
// event is in ONE node's log, the publishing node's, as every event is. So the
// node whose committer (an open) or change writer (a review) landed the record
// announces it, after the write's answer is handed back — never an applier,
// which every data node runs, and never a call the operation ledger collapsed
// onto an earlier landing, whose own call announced it (statelog.Result's
// Collapsed). A push that opened a change is announced by the committer whose
// batch landed it: its answer from that batch's own row, never a push the
// batch found landed before it. So a record is announced at most once, and a
// landing whose answer was ambiguous — answered from the ledger, collapsed —
// at worst not at all, which costs the day one count and leaves the rows on
// the code log whole.
//
// # The seats are named by their agent ids, resolved when announced
//
// A seat's day is keyed by its AGENT ID (ADR-0013), the `agent_id` column the
// day groups by, which only a process holding the chart can say — so the
// engine hands the committer and the change writer its resolver
// ([CommitterOptions.AgentIDs]), reading the live epoch at the moment of the
// announcement, as the event log resolves a participant's handle. A person is
// no seat and has no day: a party of kind person is named by no agent id, and
// a handle the chart no longer holds by none either.

// opening is one change a write opened, to announce: its id, and whether a
// push's options opened it.
type opening struct {
	changeID string
	byPush   bool
}

// agentOf is the agent id of a party a seat's day counts, "" for a person, a
// seat the chart does not hold, or a node that resolves none.
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

// announceOn publishes one event on a node's event stream, as nodeID, bounded
// by [checkEventBudget] under ctx — the announcing writer's own lifetime.
func announceOn[T any, P events.PayloadPtr[T]](ctx context.Context, pub EventPublisher, nodeID string,
	data T) error {

	ev := events.New[T, P](data, events.TraceContext{})
	ev.Source = nodeID
	return pub.Publish(ctx, topics.Event(ev.Type), ev)
}

// unannounced logs an announcement that did not reach the event stream: the
// seat's day it counts on is one short, and the rows on the code log still
// hold what it announced.
func unannounced(ctx context.Context, log *slog.Logger, kind, repoID, id string, err error) {
	log.WarnContext(ctx, kind+"_unpublished", "repo", repoID, "id", id, "error", err.Error(),
		"detail", "this node's day of usage does not count it and the activity feed does not show it; its "+
			"rows on the code log still hold it")
}

// announceOpened announces the changes a write of repoID opened, read from
// this node's rows, under the committer's own lifetime and one
// [checkEventBudget] for the read and every publish.
func (c *Committer) announceOpened(repoID string, opened []opening) {
	if c.events == nil || len(opened) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(c.life, checkEventBudget)
	defer cancel()
	evs, err := openedEvents(ctx, c.db, c.agentIDs, repoID, opened)
	if err != nil {
		unannounced(ctx, c.log, "change_opened", repoID, opened[0].changeID,
			fmt.Errorf("read the opened changes: %w", err))
		return
	}
	for _, e := range evs {
		if err := announceOn(ctx, c.events, c.nodeID, e); err != nil {
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
		if err := announceOn(ctx, w.events, w.nodeID, e); err != nil {
			unannounced(ctx, w.log, "review_submitted", cw.RepoID(), reviewID, err)
		}
	}
}

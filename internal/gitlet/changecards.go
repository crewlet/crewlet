package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A LISTED CHANGE'S CARD is what its row draws beside its readiness: its conversation's
// counts, its reviewers' faces, its head's checks and a recent ejection, each read for the
// page's listed changes at once.

// ChangeCard is one listed change's card, answered where [ReadQuery.Cards] asks.
type ChangeCard struct {
	// Comments, Reviews and ChangesRequested are its conversation's counters
	// (code_conversations): comments alone, reviews, and reviewers whose standing
	// verdict asks for changes; 0 for a change nobody has discussed.
	Comments         int64 `json:"comments"`
	Reviews          int64 `json:"reviews"`
	ChangesRequested int64 `json:"changes_requested"`
	// Reviewers is up to MaxCardReviewers faces, most pressing first ([faceRank]);
	// ReviewersOmitted how many more.
	Reviewers        []ReviewerFace `json:"reviewers,omitempty"`
	ReviewersOmitted int            `json:"reviewers_omitted,omitempty"`
	// Checks is where its head's checks for its target stand; nil where the rows keep
	// no attempt of them.
	Checks *CheckRollup `json:"checks,omitempty"`
	// Ejected is set where a live change's newest queue entry was ejected within
	// QueueEjectionsShown.
	Ejected *CardEjection `json:"ejected,omitempty"`
}

// ReviewerFace is one reviewer on a card: their standing verdict and the revision it
// judged, whether an ask of them waits, and why, and the globs a path owner's ask of them is for.
type ReviewerFace struct {
	Handle string    `json:"handle"`
	Kind   PartyKind `json:"kind"`
	// Verdict and VerdictRevision are set only where the reviewer holds a standing verdict.
	Verdict         Verdict `json:"verdict,omitempty"`
	VerdictRevision int64   `json:"verdict_revision,omitempty"`
	// Asked is set where an ask of them waits, and Reason only then.
	Asked  bool           `json:"asked,omitempty"`
	Reason ReviewerReason `json:"reason,omitempty"`
	// OwnedPaths and OwnedPathsOmitted are their path owner's ask's globs where it waits, was
	// answered by their review or ended with the change.
	OwnedPaths        []string `json:"owned_paths,omitempty"`
	OwnedPathsOmitted int      `json:"owned_paths_omitted,omitempty"`
}

// CardEjection is why and when a queue last put the change out, and the required
// checks that failed, at most MaxCardEjectionChecks, ChecksMore how many more, and
// the train they failed on.
type CardEjection struct {
	Reason     QueueEndReason `json:"reason"`
	At         time.Time      `json:"at"`
	Checks     []string       `json:"checks,omitempty"`
	ChecksMore int            `json:"checks_more,omitempty"`
	// TestedIn is the train its checks failed on, where its ejection names one.
	TestedIn *EjectionTrain `json:"tested_in,omitempty"`
}

// MaxCardReviewers is the most faces a card carries: a row draws a handful, and eight
// is past any gate's required approvals (as MaxGateApprovals).
const MaxCardReviewers = 8

// MaxCardEjectionChecks is the most failed checks a card's ejection names: a row's
// title names a few (as MaxRollupFailing).
const MaxCardEjectionChecks = MaxRollupFailing

// faceRank orders a card's faces, lowest first: changes requested, then a waiting ask,
// then an approval. A standing verdict is only ever approve or request_changes, so a
// reviewer who has only commented draws no face.
func faceRank(f ReviewerFace) int {
	switch {
	case f.Verdict == VerdictRequestChanges:
		return 0
	case f.Asked:
		return 1
	}
	return 2
}

// A CARD'S READS, each driven from the listed changes' ids, a seek of a key an id, and the
// newest queue entry by ORDER BY ... LIMIT 1, which the driver never unnests; registered in
// plan_test.go.
const (
	cardCountsSQL = `SELECT v.change_id, v.comments, v.reviews, v.changes_requested FROM json_each(?) AS j
		CROSS JOIN code_conversations AS v WHERE v.change_id = CAST(j.value AS TEXT)`
	cardVerdictsSQL = `SELECT p.change_id, p.handle, p.kind, p.verdict, p.verdict_revision FROM json_each(?) AS j
		CROSS JOIN code_participants AS p INDEXED BY ` + participantsKey + `
		WHERE p.change_id = CAST(j.value AS TEXT) AND p.verdict != ''`
	cardAsksSQL = `SELECT r.change_id, r.reviewer, r.reviewer_kind, r.reason, r.state, r.owned_paths,
			r.owned_paths_omitted FROM json_each(?) AS j
		CROSS JOIN code_reviewers AS r INDEXED BY ` + reviewersKey + `
		WHERE r.change_id = CAST(j.value AS TEXT)
			AND (r.state = 'requested' OR (r.owned_paths != '' AND r.state IN ('reviewed', 'ended')))`
	cardEjectionsSQL = `SELECT q.change_id, q.state, q.end_reason, q.ended_at, q.ejected FROM json_each(?) AS j
		CROSS JOIN code_queue AS q INDEXED BY ` + queueKey + `
		WHERE q.repo_id = CAST(j.value ->> 0 AS TEXT) AND q.entry_id = (SELECT n.entry_id
			FROM code_queue AS n INDEXED BY code_queue_change_idx
			WHERE n.change_id = CAST(j.value ->> 1 AS TEXT) ORDER BY n.seq DESC LIMIT 1)`
)

// changeCardsIn is the card of each of rows, by id, read in tx: one statement a part over
// their ids, and one rollup a distinct head and target.
func (r *Reader) changeCardsIn(ctx context.Context, tx *sql.Tx, rows []changeRow) (map[string]*ChangeCard, error) {
	cards := make(map[string]*ChangeCard, len(rows))
	if len(rows) == 0 {
		return cards, nil
	}
	ids := make([]string, len(rows))
	var live [][2]string
	for i, row := range rows {
		ids[i] = row.id
		cards[row.id] = &ChangeCard{}
		if row.state.Live() {
			live = append(live, [2]string{row.repoID, row.id})
		}
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	if err = cardCountsIn(ctx, tx, list, cards); err != nil {
		return nil, err
	}
	if err = cardFacesIn(ctx, tx, list, cards); err != nil {
		return nil, err
	}
	rollups := map[[3]string]*CheckRollup{}
	for _, row := range rows {
		key := [3]string{row.repoID, row.head, row.target}
		rollup, read := rollups[key]
		if !read {
			if rollup, err = rollupOfTarget(ctx, tx, row.repoID, row.head, row.target); err != nil {
				return nil, err
			}
			rollups[key] = rollup
		}
		cards[row.id].Checks = rollup
	}
	if len(live) > 0 {
		if err = r.cardEjectionsIn(ctx, tx, live, cards); err != nil {
			return nil, err
		}
	}
	return cards, nil
}

// cardCountsIn sets each card's conversation counters from the changes' list.
func cardCountsIn(ctx context.Context, tx *sql.Tx, list string, cards map[string]*ChangeCard) error {
	rows, err := tx.QueryContext(ctx, cardCountsSQL, list)
	if err != nil {
		return fmt.Errorf("gitlet: read the conversation counts of %d changes: %w", len(cards), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var (
			id                                  string
			comments, reviews, changesRequested int64
		)
		if err = s.Scan(&id, &comments, &reviews, &changesRequested); err != nil {
			return err
		}
		if c := cards[id]; c != nil {
			c.Comments, c.Reviews, c.ChangesRequested = comments, reviews, changesRequested
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("gitlet: read the conversation counts of %d changes: %w", len(cards), err)
	}
	return nil
}

// cardFacesIn sets each card's faces: its standing verdicts and waiting asks, one face a
// reviewer, in [faceRank] order and cut at MaxCardReviewers; and on a face either made, the
// globs of a path owner's ask of them that waits, was answered or ended.
func cardFacesIn(ctx context.Context, tx *sql.Tx, list string, cards map[string]*ChangeCard) error {
	faces := map[string]map[string]*ReviewerFace{}
	face := func(change, handle string) *ReviewerFace {
		of := faces[change]
		if of == nil {
			of = map[string]*ReviewerFace{}
			faces[change] = of
		}
		f := of[handle]
		if f == nil {
			f = &ReviewerFace{Handle: handle}
			of[handle] = f
		}
		return f
	}
	rows, err := tx.QueryContext(ctx, cardVerdictsSQL, list)
	if err != nil {
		return fmt.Errorf("gitlet: read the verdicts of %d changes: %w", len(cards), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var change, handle, kind, verdict string
		var revision int64
		if err = s.Scan(&change, &handle, &kind, &verdict, &revision); err != nil {
			return err
		}
		f := face(change, handle)
		f.Kind, f.Verdict, f.VerdictRevision = PartyKind(kind), Verdict(verdict), revision
		return nil
	})
	if err != nil {
		return fmt.Errorf("gitlet: read the verdicts of %d changes: %w", len(cards), err)
	}
	if rows, err = tx.QueryContext(ctx, cardAsksSQL, list); err != nil {
		return fmt.Errorf("gitlet: read the asks of %d changes: %w", len(cards), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var change, handle, kind, reason, state, owned string
		var omitted int
		if err = s.Scan(&change, &handle, &kind, &reason, &state, &owned, &omitted); err != nil {
			return err
		}
		var f *ReviewerFace
		if ReviewerState(state) == ReviewerRequested {
			f = face(change, handle)
			f.Kind, f.Asked, f.Reason = PartyKind(kind), true, ReviewerReason(reason)
		} else if f = faces[change][handle]; f == nil {
			// An answered or ended ask draws no face of its own: only one the verdicts made.
			return nil
		}
		if owned != "" {
			if err = json.Unmarshal([]byte(owned), &f.OwnedPaths); err != nil {
				return fmt.Errorf("change %s's ask of %s: its globs: %w", change, handle, err)
			}
			f.OwnedPathsOmitted = omitted
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("gitlet: read the asks of %d changes: %w", len(cards), err)
	}
	for change, of := range faces {
		c := cards[change]
		if c == nil {
			continue
		}
		listed := make([]ReviewerFace, 0, len(of))
		for _, f := range of {
			listed = append(listed, *f)
		}
		slices.SortFunc(listed, func(a, b ReviewerFace) int {
			return cmp.Or(cmp.Compare(faceRank(a), faceRank(b)), cmp.Compare(a.Handle, b.Handle))
		})
		if len(listed) > MaxCardReviewers {
			c.ReviewersOmitted = len(listed) - MaxCardReviewers
			listed = listed[:MaxCardReviewers]
		}
		c.Reviewers = listed
	}
	return nil
}

// cardEjectionsIn sets the ejection of each live change, given as its repository and id,
// whose newest queue entry was ejected within QueueEjectionsShown.
func (r *Reader) cardEjectionsIn(ctx context.Context, tx *sql.Tx, live [][2]string,
	cards map[string]*ChangeCard) error {

	pairs, err := json.Marshal(live)
	if err != nil {
		return fmt.Errorf("gitlet: encode a key list: %w", err)
	}
	since := store.EncodeTime(r.now().Add(-QueueEjectionsShown))
	rows, err := tx.QueryContext(ctx, cardEjectionsSQL, string(pairs))
	if err != nil {
		return fmt.Errorf("gitlet: read the queue entries of %d changes: %w", len(live), err)
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var change, state, reason, detail string
		var endedAt int64
		if err = s.Scan(&change, &state, &reason, &endedAt, &detail); err != nil {
			return err
		}
		c := cards[change]
		if c == nil || QueueState(state) != QueueEjected || endedAt < since {
			return nil
		}
		e := &CardEjection{Reason: QueueEndReason(reason), At: store.DecodeTime(endedAt)}
		if detail != "" {
			var x struct {
				Checks   []string       `json:"checks"`
				TestedIn *EjectionTrain `json:"tested_in"`
			}
			if err = json.Unmarshal([]byte(detail), &x); err != nil {
				return fmt.Errorf("change %s's ejection: %w", change, err)
			}
			if len(x.Checks) > MaxCardEjectionChecks {
				e.ChecksMore = len(x.Checks) - MaxCardEjectionChecks
				x.Checks = x.Checks[:MaxCardEjectionChecks]
			}
			e.Checks, e.TestedIn = x.Checks, x.TestedIn
		}
		c.Ejected = e
		return nil
	})
	if err != nil {
		return fmt.Errorf("gitlet: read the queue entries of %d changes: %w", len(live), err)
	}
	return nil
}

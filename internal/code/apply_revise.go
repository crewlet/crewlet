package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
)

// applyRevise writes a revision's facts ([Revise]) — the row of its revision,
// its trees and its classes among them, the facts of another target it
// replaces where the change was retargeted, or of an earlier tip — and asks
// each reviewer the record names as a path
// owner's, a participant of the change, where the change holds no ask of
// them: the decide asked only those whose ask it did not hold, and the
// broker's arbitration on the change's subject applies the record against
// the rows that decide read, so a conflict is a record the ledger would have
// collapsed, met again, and a skip. The conversation's text_bytes moves by
// what the facts' wide columns weigh, less what the row they replace weighed,
// so a purge weighs the change's subject's wide rows by its one narrow row.
//
// And it asks each reviewer the record asks to look again ([Revise.ReReview])
// for a review once more, where their review answered their ask.
//
// PURE IN THE RECORD, its position and the prior rows, as every apply here:
// whom the facts ask, skip and wake rides the record, stamped by the writer
// from its chart, and nothing is judged again.
func applyRevise(ctx context.Context, tx *sql.Tx, at applyContext, r Revise) (int, conversationCounts, error) {
	prior, held, err := factsIn(ctx, tx, r.ChangeID, r.N)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	wide, err := factsColumns(r)
	if err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: encode revision %d of %s's facts at %s: %w", r.N,
			r.ChangeID, at.position, err)
	}
	if _, err := tx.ExecContext(ctx, writeFactsSQL, r.ChangeID, r.N, r.TargetRef, r.TargetOID,
		r.TargetPosition.Packed(), r.BaseOID, r.BaseTree, r.HeadTree, r.PatchID, r.PatchIDOmitted, len(r.Touched),
		r.TouchesAll, r.Node, r.OwnersDigest, wide.bytes(), at.packed, wide.owners, wide.skipped, wide.classes,
		wide.touched); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: write revision %d of %s's facts at %s: %w", r.N,
			r.ChangeID, at.position, err)
	}
	counts := conversationCounts{textBytes: wide.bytes()}
	if held {
		counts.textBytes -= prior.bytes
	}
	rows := 1
	for _, rv := range r.Asked {
		if !rv.Kind.Valid() {
			return 0, conversationCounts{}, fmt.Errorf("code: the facts at %s ask reviewer %q of kind %q, which no "+
				"writer stamps", at.position, rv.Handle, rv.Kind)
		}
	}
	// THE OWNERS ASKED AND THEIR PARTICIPANT ROWS, A COLLECTION EACH, as an
	// ask for review writes its own.
	if _, err := store.InsertRows(ctx, tx, at.maxVariables, askPathOwnersSQL, `(?, ?, ?, ?, ?, '', '', '', ?)`,
		`ON CONFLICT (change_id, reviewer) DO NOTHING`, len(r.Asked), func(i int) []any {
			rv := r.Asked[i]
			return []any{r.ChangeID, rv.Handle, string(rv.Kind), string(ReviewerPathOwner), string(ReviewerRequested),
				at.packed}
		}); err != nil {
		return 0, conversationCounts{}, fmt.Errorf("code: ask change %s's path owners at %s: %w", r.ChangeID,
			at.position, err)
	}
	if err := addParticipants(ctx, tx, at, r.ChangeID, r.Asked); err != nil {
		return 0, conversationCounts{}, err
	}
	rows += 2 * len(r.Asked)
	// THE REVIEWERS ASKED TO LOOK AGAIN are waited on again: a reviewer whose
	// review answered their ask, and whose standing verdict asks for changes
	// on an earlier revision, owes the change a review of this one — the
	// re_review the record carries for the wakes. A reviewer nobody asked
	// gains no ask, and one whose ask was withdrawn keeps that word.
	again := make([]string, len(r.ReReview))
	for i, rv := range r.ReReview {
		again[i] = rv.Handle
	}
	reopened, err := moveAsks(ctx, tx, at, r.ChangeID, ReviewerReviewed, ReviewerRequested, again)
	if err != nil {
		return 0, conversationCounts{}, err
	}
	return rows + reopened, counts, nil
}

// factsWide is a revision's facts' wide columns as a row keeps them: its
// touched paths NUL-separated — no path of a tree holds a NUL — and its
// matched and skipped owners and its classes as JSON, each "" for none.
type factsWide struct {
	touched, owners, skipped, classes string
}

// bytes is what the wide columns weigh, as code_revision_facts.bytes and the
// conversation's text_bytes count them.
func (w factsWide) bytes() int64 {
	return int64(len(w.touched) + len(w.owners) + len(w.skipped) + len(w.classes))
}

// factsColumns is r's wide columns.
func factsColumns(r Revise) (factsWide, error) {
	w := factsWide{touched: strings.Join(r.Touched, "\x00")}
	if !r.Classes.IsZero() {
		encoded, err := json.Marshal(r.Classes)
		if err != nil {
			return factsWide{}, err
		}
		w.classes = string(encoded)
	}
	if len(r.RequestedOwners) > 0 {
		encoded, err := json.Marshal(r.RequestedOwners)
		if err != nil {
			return factsWide{}, err
		}
		w.owners = string(encoded)
	}
	if len(r.SkippedOwners) > 0 {
		encoded, err := json.Marshal(r.SkippedOwners)
		if err != nil {
			return factsWide{}, err
		}
		w.skipped = string(encoded)
	}
	return w, nil
}

// A revise's writes, every one with a WHERE registered in plan_test.go: the
// facts by their key, replaced whole by a later record of the same revision —
// another target's, or a later tip's — and a path owner's ask, which the
// decide made only where the change held none, a conflict on its key a skip.
const (
	writeFactsSQL = `INSERT INTO code_revision_facts (change_id, n, target_ref, target_oid, target_position,
			base_oid, base_tree, head_tree, patch_id, patch_id_omitted, touched_count, touches_all, node,
			owners_digest, bytes, position, owners, skipped, classes, touched)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (change_id, n) DO UPDATE SET target_ref = excluded.target_ref,
			target_oid = excluded.target_oid, target_position = excluded.target_position,
			base_oid = excluded.base_oid, base_tree = excluded.base_tree, head_tree = excluded.head_tree,
			patch_id = excluded.patch_id, patch_id_omitted = excluded.patch_id_omitted,
			touched_count = excluded.touched_count, touches_all = excluded.touches_all, node = excluded.node,
			owners_digest = excluded.owners_digest, bytes = excluded.bytes, position = excluded.position,
			owners = excluded.owners, skipped = excluded.skipped, classes = excluded.classes,
			touched = excluded.touched`
	// askPathOwnersSQL is the path owners a revision's facts ask, a collection
	// store.InsertRows writes, each where the change holds no ask of them.
	askPathOwnersSQL = `INSERT INTO code_reviewers (change_id, reviewer, reviewer_kind, reason, state, requested_by,
			requested_by_kind, note, position) VALUES `
)

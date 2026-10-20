package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE VERDICT: what the applier decided about one record on a room's message
// subject, identically on every data node.
//
// # Why a message record has one
//
// A message record is SEQUENCED ([statelog.PatternSequenced]): it carries no
// expectation, the log orders it, and the applier decides it at its position,
// as a pure function of the record and the rows it holds there. That is not
// additive, because the records do not all commute: whether a post lands
// depends on whether a record ahead of it archived the room or took its author
// out of a private one, whether a reaction lands on whether thirty-two other
// emoji are already on the message, whether an edit lands on whether a delete
// ahead of it left a tombstone. Each of those was decided at the applier and
// told to nobody: the writer answered `applied` for a record the applier had
// dropped.
//
// So the applier writes a verdict for every message record it consumes
// (`chat_verdicts`), and every reader that reports what a record did reads it:
// the writer before it answers ([Store.Post] and the other three), a retry the
// operation ledger answers, and the applier itself before it decides a second
// copy of an operation. ADR-0046 is the decision.
//
// # Why not arbitrated instead
//
// An expectation on the message subject would serialise the busiest subject in
// the company behind itself, and its failure is the one a chat system may not
// have: `ErrConflict` on a message somebody typed, because somebody else in the
// room said something at the same moment. Sequencing keeps every post landing
// in the order it was said; what the applier refuses is refused because of what
// the room IS at that position, never because of who else spoke.

// Verdict is the applier's decision about one message record.
type Verdict string

const (
	// VerdictApplied is a record whose effect landed — or was already
	// there: a reaction somebody already holds, a reaction taken back that
	// was never on, a delete of what is already a tombstone.
	VerdictApplied Verdict = "applied"

	// VerdictArchived is a post into a room a record ahead of it archived.
	VerdictArchived Verdict = "archived"

	// VerdictNotMember is a post into a room that admits its members only
	// (the record says so: [MessagePost.MembersOnly]) by an author a record
	// ahead of it took out of the room.
	VerdictNotMember Verdict = "not_member"

	// VerdictGone is an edit, a delete or a reaction of a message a record
	// ahead of it erased or pruned, or a post of a message id an erase
	// already destroyed.
	VerdictGone Verdict = "gone"

	// VerdictDeleted is an edit of a message a delete ahead of it took
	// back: its body was removed on purpose, and writing text into the
	// tombstone would undo a deletion nobody asked to undo.
	VerdictDeleted Verdict = "deleted"

	// VerdictReactionsFull is a reaction that would put a [MaxReactionEmoji]
	// + 1st distinct emoji on a message.
	VerdictReactionsFull Verdict = "reactions_full"

	// VerdictRepeated is a second copy of an operation a record at an
	// earlier position already decided — two publishers that each found
	// the operation's ledger silent — and it does nothing: the first copy's
	// verdict is the operation's.
	VerdictRepeated Verdict = "repeated"
)

// Verdicts is every verdict this build's applier writes.
func Verdicts() []Verdict {
	return []Verdict{VerdictApplied, VerdictArchived, VerdictNotMember, VerdictGone,
		VerdictDeleted, VerdictReactionsFull, VerdictRepeated}
}

// Valid reports whether v is a verdict this build writes. A newer build's
// applier may write one this build does not know; it reads back as a value,
// and [Verdict.refusal] answers it as a refusal it cannot describe.
func (v Verdict) Valid() bool { return slices.Contains(Verdicts(), v) }

// Landed reports whether the operation a verdict decided did what it asked.
//
// APPLIED, AND REPEATED: a repeated copy did nothing because its operation had
// already done it. A reader meets one only where the first copy's verdict has
// passed [VerdictRetention] — [verdictIn] reads an operation's FIRST verdict —
// which inside the mint horizon a retry never does; past it, the message the
// repeated post found is the operation's own.
func (v Verdict) Landed() bool { return v == VerdictApplied || v == VerdictRepeated }

// refusal is the domain's own refusal for a verdict that did not land, in the
// sentinel each would have been refused with had the writer's snapshot already
// held the record ahead of it — so a surface answers the two the same way.
func (v Verdict) refusal(op OpKind, opID, channelID string) error {
	switch v {
	case VerdictArchived:
		return fmt.Errorf("%w: %s was archived before this message reached it, "+
			"and an archived room takes no new messages; nothing was said",
			ErrArchived, channelID)
	case VerdictNotMember:
		return fmt.Errorf("%w: the author was taken out of %s before this message "+
			"reached it, and the room admits its members only; nothing was said",
			ErrForbidden, channelID)
	case VerdictGone:
		return fmt.Errorf("%w: the message this %s was about was erased or pruned "+
			"before the %s reached it; nothing was changed", ErrNotFound, op, op)
	case VerdictDeleted:
		return fmt.Errorf("%w: the message was taken back before this edit "+
			"reached it, and there is nothing left to rewrite", ErrNotFound)
	case VerdictReactionsFull:
		return invalid("emoji", "the message reached its %d distinct emoji before "+
			"this reaction did — add to one it already carries, or take one back",
			MaxReactionEmoji)
	}
	// A VERDICT THIS BUILD DOES NOT KNOW, written by a newer peer's applier
	// about a record that landed: it decided something this build cannot
	// describe, which is a conflict with the room's state rather than an
	// argument to change.
	return fmt.Errorf("%w: operation %s in %s was decided %q by a newer build's "+
		"applier, which this build cannot describe", ErrConflict, opID, channelID, v)
}

// verdictDropped is what [verdictIn] answers for a record every node's gate
// dropped: this node applied everything through its position and the record
// wrote nothing, which only a gate decides, and on every node alike. Never
// written; a reading only.
const verdictDropped Verdict = "dropped"

// verdictIn reads, inside tx, what this node's applier decided about operation
// opID, whose record is at: the verdict its FIRST copy was given, which is the
// operation's.
//
// THREE ANSWERS, and the third is why it reads the prefix. A verdict row is a
// decision. No row on a snapshot whose rows have APPLIED everything through at
// ([statelog.Prefix.Applied]) is a record a gate dropped — written by a node
// the fleet evicted before it — which wrote nothing here or anywhere
// ([verdictDropped]). No row on a snapshot that has not — the record not
// reached yet, or RETAINED, which moves the checkpoint without writing a row —
// is no answer: decided false, and the caller asks another node or asks again.
// A record whose verdict [VerdictRetention] has let go reads as dropped too,
// which only a reader a month behind the record can meet: the writer reads its
// own the moment its applier reaches it, and a retry is answered inside the
// mint horizon.
func verdictIn(ctx context.Context, tx *sql.Tx, opID string, at statelog.Position) (
	Verdict, bool, error) {

	var v string
	err := tx.QueryRowContext(ctx, verdictReadStatement, opID).Scan(&v)
	switch {
	case err == nil:
		return Verdict(v), true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, fmt.Errorf("chatlet: read the verdict of operation %s: %w", opID, err)
	}
	prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
	if err != nil {
		return "", false, fmt.Errorf("chatlet: read how far this node applied the log, for "+
			"operation %s: %w", opID, err)
	}
	if prefix.Applied().Packed() >= at.Packed() {
		return verdictDropped, true, nil
	}
	return "", false, nil
}

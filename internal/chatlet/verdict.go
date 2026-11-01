package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
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
// dropped, and the change feed woke seats about a post that is in no room.
//
// So the applier writes a verdict for every message record it consumes
// (`chat_verdicts`), and every reader that reports what a record did reads it:
// the writer before it answers ([Store.Post] and the other three), a retry the
// operation ledger answers, the change feed before it derives a wake
// ([Translator]), and the applier itself before it decides a second copy of an
// operation. ADR-0046 is the decision.
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
//
// THE ROOM IS NAMED AS A PERSON KNOWS IT ([Channel.called]): this sentence is
// a model's in a tool's answer, a seat's or a person's assistant's, and it
// named the room by its id, which nobody chose and nobody can place.
func (v Verdict) refusal(op OpKind, room Channel) error {
	switch v {
	case VerdictArchived:
		return fmt.Errorf("%w: this room was archived before the message "+
			"reached it, so nothing was said. %s", ErrArchived, reopenRoom)
	case VerdictNotMember:
		// NOT FOUND, AS THE DECIDE SAYS IT ([reachable]) — and the room
		// named all the same: this is the author's own post, decided while
		// they were in the room.
		return fmt.Errorf("%w: the author was taken out of %s before this message "+
			"reached it, and only its members can post there, so nothing was said",
			ErrNotFound, room.called())
	case VerdictGone:
		return fmt.Errorf("%w: the message this %s was about was erased or pruned "+
			"before the %s reached it; nothing was changed", ErrNotFound, op, op)
	case VerdictDeleted:
		return fmt.Errorf("%w: the message was taken back before this edit "+
			"reached it, and there is nothing left to rewrite", ErrNotFound)
	case VerdictReactionsFull:
		return fmt.Errorf("%w: this message reached %d different emoji, the most "+
			"it can, before the reaction did. Add to one it already has, or take "+
			"one off", ErrReactionsFull, MaxReactionEmoji)
	}
	// A VERDICT THIS BUILD DOES NOT KNOW, written by a newer peer's applier
	// about a record that landed: it decided something this build cannot
	// describe, which is a conflict with the room's state rather than an
	// argument to change. Named by what it was, never by the operation's id
	// or the room's, which nobody chose.
	return fmt.Errorf("%w: a newer node decided this %s in a way this node can't "+
		"describe. Read %s again to see what happened", ErrConflict, op, room.called())
}

// verdictDropped is what [verdictIn] answers for a record every node's gate
// dropped: this node applied everything through its position, the record wrote
// nothing, and no prune has reached its position — which leaves only a gate,
// deciding on every node alike. Never written; a reading only.
const verdictDropped Verdict = "dropped"

// verdictLost is what [verdictIn] answers for a record this node applied whose
// verdict, if it had one, the horizon may have let go ([VerdictRetention]): its
// position is at or below the highest the prune has removed
// (`chat_verdicts_lost`). Neither "decided" nor "dropped" — nothing here can
// say which — so a caller answers it as the framework answers an operation its
// ledger may have lost ([statelog.Result.Unvouched]): unknown. Never written; a
// reading only.
const verdictLost Verdict = "lost"

// verdictIn reads, inside tx, what this node's applier decided about operation
// opID, whose record is at: the verdict its FIRST copy was given, which is the
// operation's.
//
// FOUR ANSWERS, and the last two are why it reads the prefix and the prune's
// watermark. A verdict row is a decision. No row on a snapshot that has not
// applied everything through at ([statelog.Prefix.Applied]) — the record not
// reached yet, or RETAINED, which moves the checkpoint without writing a row —
// is no answer: decided false, and the caller asks another node or asks again.
// No row on a snapshot that HAS is one of two absences that leave the same
// bytes: a record a gate dropped — written by a node the fleet evicted before
// it, which wrote nothing here or anywhere ([verdictDropped]) — or a verdict
// [VerdictRetention] let go. The prune records the highest position it removed
// (writeVerdict, apply_verdict.go), so a record above it was never pruned and
// its absence is a gate's; one at or below it may have been, and its absence
// vouches for nothing ([verdictLost]). That is the framework's own reading of
// a ledger row a sweep may have taken ([statelog.Rows.LostBefore]), and for the
// same reason: answered `dropped`, a month-old post that may be in its room
// would have been told to the change feed as one nobody said. Only a reader a
// month behind the record meets it — the writer reads its own the moment its
// applier reaches it, and a retry is answered inside the mint horizon.
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
	if prefix.Applied().Packed() < at.Packed() {
		return "", false, nil
	}
	var through int64
	err = tx.QueryRowContext(ctx, verdictLostReadStatement).Scan(&through)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return verdictDropped, true, nil
	case err != nil:
		return "", false, fmt.Errorf("chatlet: read how far the horizon pruned the "+
			"verdicts, for operation %s: %w", opID, err)
	case at.Packed() <= through:
		return verdictLost, true, nil
	}
	return verdictDropped, true, nil
}

// Decisions is what the change feed reads before it derives a wake: what this
// node's applier decided about a record.
//
// A WAKE IS DERIVED FROM A DECISION WHERE ONE HAS BEEN TAKEN: a post the applier
// refused is in no room, and a seat woken for it would open a room to answer a
// message nobody can read. The feed runs only where the estate is held (its
// group needs data), so the applier it asks is this node's own.
//
// AND FROM THE RECORD WHERE NONE HAS, which is the opposite of what a refusal
// suggests and deliberate: the record carries its own routing snapshot so that
// the company's notifications never wait on any one node's applier, and a feed
// that held every wake until its own copy caught up would make them do exactly
// that — a node catching up after a restart, or one holding a record it cannot
// decode, would stop the whole fleet's chat wakes behind the record at the head
// of its consumer. What that costs is a wake for a post refused in a race this
// node has not applied yet, which the woken seat finds is not there.
type Decisions interface {
	// Decided is the verdict of operation opID, whose record is at, and
	// whether this node has one to give. ok false is no answer — not
	// reached inside the wait, held retained, or decided so long ago that
	// the horizon may have let the verdict go — and the caller derives the
	// wake from the record.
	Decided(ctx context.Context, opID string, at statelog.Position) (v Verdict, ok bool, err error)
}

// CommitWaiter is the half of this node's applier [NodeDecisions] asks: what
// a READER waits for (statelog.Runner's Reached), since a verdict is read
// from rows the applier wrote, never what is durable.
type CommitWaiter interface {
	Reached() statelog.Position
	WaitReached(ctx context.Context, p statelog.Position) error
}

// DecisionWait is how long the feed waits for this node's applier to reach a
// record before it derives the wake from the record alone.
//
// [statelog.ReadBudget], the two seconds a read is promised this node's
// applier reaches what it waits for: the feed is one more reader of a position
// this node is about to apply, and a node that cannot reach a record inside a
// read's budget is one whose reads are being refused `behind` too. The applier
// gives up its linger the instant somebody waits ([statelog.ApplyLinger]), so a
// current node answers in one batch's commit, well inside it.
const DecisionWait = statelog.ReadBudget

// DecisionLagRecords is how far behind a record this node's applier may be for
// the feed to wait for it at all: one pull ([statelog.FetchMessages]).
//
// AN APPLIER FURTHER BEHIND IS CATCHING UP, and its next batch will not reach
// the record however long the feed waits — so the feed derives the wake at once
// rather than spending [DecisionWait] on each record it is handed, which would
// slow a node rejoining the group to one wake per wait.
const DecisionLagRecords = statelog.FetchMessages

// NodeDecisions is [Decisions] over this node's own estate and applier.
//
// THE ZERO VALUE REFUSES, naming what is missing: a feed built without either
// would read no verdict and wake about every refused post with nothing to say
// so, and the error is what the feed logs on every record it decides without
// one.
type NodeDecisions struct {
	Estate  store.ReplicatedReader
	Applied CommitWaiter
}

// Decided implements [Decisions].
func (d NodeDecisions) Decided(ctx context.Context, opID string, at statelog.Position) (
	Verdict, bool, error) {

	switch {
	case d.Estate.IsZero():
		return "", false, errors.New("chatlet: the feed was given no replicated estate, " +
			"where the applier writes its verdicts")
	case d.Applied == nil:
		return "", false, errors.New("chatlet: the feed was given no applier to ask " +
			"whether it has decided a record")
	}
	committed := d.Applied.Reached()
	if committed.Stream == at.Stream && committed.Generation == at.Generation &&
		at.Seq > committed.Seq+uint64(DecisionLagRecords) {
		return "", false, nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, DecisionWait)
	defer cancel()
	if err := d.Applied.WaitReached(waitCtx, at); err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if waitCtx.Err() != nil {
			// NOT REACHED IN TIME is no answer, never an error: the
			// record is durable and this node's applier is behind it.
			return "", false, nil
		}
		return "", false, fmt.Errorf("chatlet: wait for this node's applier to reach %s: %w",
			at, err)
	}
	var (
		v  Verdict
		ok bool
	)
	err := d.Estate.Read(ctx, func(tx *sql.Tx) error {
		var err error
		v, ok, err = verdictIn(ctx, tx, opID, at)
		return err
	})
	switch {
	case err != nil:
		return "", false, err
	case v == verdictLost:
		// NO ANSWER, as the writer's is `unknown`: the record was
		// decided once and what it was is no longer kept, so the wake
		// is derived from the record like any record not yet decided.
		return "", false, nil
	}
	return v, ok, nil
}

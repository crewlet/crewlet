package assistantdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/store"
)

// THE PROPOSAL'S MOVES, AS RECORDS.
//
// Which moves are legal is internal/assistant/proposal's state machine
// (ADR-0051), and this package does not hold a second copy of it. What it holds
// is the record's half: each move names the state it was decided FROM and the
// state it moves TO, the broker arbitrates it on the proposal's subject, and the
// apply writes it only while the proposal is still in the state it names. Two
// approvals decided from `proposed` on two nodes are two records at one
// expectation: the broker accepts one, and the other's writer, deciding again,
// finds the proposal decided ([AlreadyDecided]).
//
// moveTargets binds each op to the states it may move a proposal to, so a
// record's op and its target never disagree: the op is what an operator reads
// in the log, the target is what the row says.
var moveTargets = map[OpKind][]wire.ProposalState{
	OpProposalApproved:  {wire.ProposalApproved},
	OpProposalExecuting: {wire.ProposalExecuting},
	OpProposalDeclined:  {wire.ProposalDeclined},
	OpProposalExpired:   {wire.ProposalExpired},
	OpProposalStale:     {wire.ProposalStale},
	OpProposalApplying:  {wire.ProposalApplying},
	OpProposalSettled:   {wire.ProposalApplied, wire.ProposalFailed},
	OpProposalUnknown:   {wire.ProposalUnknown},
	OpProposalRetried:   {wire.ProposalExecuting},
}

// moveOp is the op a move from from to to is recorded under, and false for a
// target no op moves a proposal to. A Retry is its own op, because the person
// asked for it, and an approved proposal's dispatch is another.
func moveOp(from, to wire.ProposalState) (OpKind, bool) {
	if to == wire.ProposalExecuting {
		if from == wire.ProposalUnknown {
			return OpProposalRetried, true
		}
		return OpProposalExecuting, true
	}
	for _, op := range proposalMoveOps {
		if op != OpProposalRetried && op != OpProposalExecuting &&
			slices.Contains(moveTargets[op], to) {
			return op, true
		}
	}
	return "", false
}

// applyProposal applies a record on a proposal's subject.
func applyProposal(ctx context.Context, tx *sql.Tx, c applyContext, payload any) (int, error) {
	conversation, proposal, ok := splitChild(c.record.Subject.ID)
	if !ok {
		return 0, nil
	}
	row, found, err := readConversation(ctx, tx, conversation)
	if err != nil || !found {
		return 0, err
	}
	var written int
	switch p := payload.(type) {
	case ProposalCreated:
		written, err = applyProposalCreated(ctx, tx, c, conversation, proposal, row, p)
	case ProposalMoved:
		written, err = applyProposalMoved(ctx, tx, c, proposal, p)
	default:
		return 0, fmt.Errorf("assistantdomain: the %s record at %s carries a %T",
			c.record.Op, c.position, payload)
	}
	if err != nil || written == 0 {
		return written, err
	}
	touched, err := touch(ctx, tx, c, conversation)
	return written + touched, err
}

// applyProposalCreated writes a proposal's row, create-only, for a turn of the
// conversation's own owner.
func applyProposalCreated(ctx context.Context, tx *sql.Tx, c applyContext, conversation,
	proposal string, row conversationRow, p ProposalCreated) (int, error) {

	if row.owner != p.Owner || !p.Scope.Valid() {
		return 0, nil
	}
	if ok := present(p.Turn); !ok {
		return 0, nil
	}
	stepUp := 0
	if p.StepUp {
		stepUp = 1
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_proposals
			(id, conversation_id, turn_id, owner_id, tool, act, scope, method, path,
			 query_sealed, body_sealed, request_digest, digest, op_key, source_revision,
			 step_up, card_sealed, state, expires_at, created_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		proposal, conversation, p.Turn, p.Owner, p.Tool, p.Act, string(p.Scope),
		p.Method, p.Path, p.QuerySealed, p.BodySealed, p.RequestDigest, p.Digest,
		p.OpKey, p.SourceRevision, stepUp, p.CardSealed, string(wire.ProposalProposed),
		store.EncodeTime(p.ExpiresAt), c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: create proposal %s at %s: %w",
			proposal, c.position, err)
	}
	return affected(res), nil
}

// proposalState is a proposal's state, and false where it has no row.
func proposalState(ctx context.Context, tx *sql.Tx, proposal string) (wire.ProposalState, bool, error) {
	var state string
	err := tx.QueryRowContext(ctx,
		`SELECT state FROM assistant_proposals WHERE id = ?`, proposal).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("assistantdomain: read proposal %s: %w", proposal, err)
	}
	return wire.ProposalState(state), true, nil
}

// applyProposalMoved writes one move while the proposal is still in the state
// the move was decided from.
func applyProposalMoved(ctx context.Context, tx *sql.Tx, c applyContext, proposal string,
	p ProposalMoved) (int, error) {

	if !slices.Contains(moveTargets[c.record.Op], p.To) {
		return 0, nil
	}
	state, found, err := proposalState(ctx, tx, proposal)
	if err != nil || !found || state != p.From {
		return 0, err
	}
	var res sql.Result
	if c.record.Op == OpProposalSettled {
		res, err = tx.ExecContext(ctx, `
			UPDATE assistant_proposals SET state = ?, settled_at = ?, outcome = ?,
				position = ?, receipt_sealed = ?, refusal_code = ?, version = ?
			WHERE id = ? AND version < ?`,
			string(p.To), c.at(), p.Outcome, p.Position, p.ReceiptSealed, p.RefusalCode,
			c.packed, proposal, c.packed)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE assistant_proposals SET state = ?, decided_at = ?, refusal_code = ?,
				version = ?
			WHERE id = ? AND version < ?`,
			string(p.To), c.at(), p.RefusalCode, c.packed, proposal, c.packed)
	}
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: move proposal %s to %s at %s: %w",
			proposal, p.To, c.position, err)
	}
	return affected(res), nil
}

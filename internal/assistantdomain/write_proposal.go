package assistantdomain

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// ProposalMethods are the methods a proposal's request may carry: the HTTP
// verbs that write, and PAGE for a page action, which is applied by the page
// and never sent (internal/assistant/proposal).
var ProposalMethods = []string{"POST", "PUT", "PATCH", "DELETE", "PAGE"}

// NewProposal is one proposal a running turn made: the exact request it would
// send, the preview's card and the digest that binds them (ADR-0051). The
// digest is computed by internal/assistant/proposal; this package stores what
// it is given and seals what is not an identifier.
type NewProposal struct {
	Turn     uuid.UUID
	Proposal uuid.UUID

	Tool  string
	Act   string
	Scope wire.ProposalScope

	Method string
	Path   string
	Query  url.Values
	Body   []byte

	RequestDigest  string
	Digest         string
	OpKey          string
	SourceRevision string
	StepUp         bool

	// Card is the proposal card the person is shown.
	Card wire.Card

	ExpiresAt time.Time
}

// validate refuses a proposal its record could not hold.
func (p NewProposal) validate() error {
	if err := errors.Join(checkID("the turn id", p.Turn),
		checkID("the proposal id", p.Proposal)); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(p.Tool) == "" || len(p.Tool) > TitleMaxBytes:
		return invalid("the tool is required and at most %d bytes", TitleMaxBytes)
	case len(p.Act) > TitleMaxBytes:
		return invalid("the act is at most %d bytes", TitleMaxBytes)
	case !p.Scope.Valid():
		return invalid("scope %q is not one of %s", p.Scope, words.Or(wire.ProposalScopes))
	case !slices.Contains(ProposalMethods, p.Method):
		return invalid("method %q is not one of %s", p.Method, words.Or(ProposalMethods))
	case !strings.HasPrefix(p.Path, "/") || len(p.Path) > QueryMaxBytes:
		return invalid("the path is a route's own path, from its leading slash, " +
			"of at most a query's bound")
	case len(p.Query.Encode()) > QueryMaxBytes:
		return invalid("the query is %d bytes encoded and the maximum is %d",
			len(p.Query.Encode()), QueryMaxBytes)
	case len(p.Body) > ProposalBodyMaxBytes:
		return invalid("the body is %d bytes and the maximum is %d", len(p.Body),
			ProposalBodyMaxBytes)
	case len(p.Body) > 0 && !json.Valid(p.Body):
		return invalid("the body is not JSON")
	case !validHexDigest(p.RequestDigest):
		return invalid("the request digest is 64 lowercase hex digits")
	case !wire.ValidDigest(p.Digest):
		return invalid("the digest is %q and 64 lowercase hex digits", wire.DigestPrefix)
	case len(p.SourceRevision) > TitleMaxBytes:
		return invalid("the source revision is at most %d bytes", TitleMaxBytes)
	case p.ExpiresAt.IsZero():
		return invalid("a proposal expires")
	case p.Card.Type != wire.CardProposal:
		return invalid("a proposal's card is a proposal card")
	}
	if err := statelog.CheckCallerOpID(p.OpKey); err != nil {
		return fmt.Errorf("%w: the operation key the approved request is sent with %w",
			ErrInvalid, err)
	}
	if err := p.Card.Validate(); err != nil {
		return fmt.Errorf("%w: the card: %w", ErrInvalid, err)
	}
	return nil
}

// validHexDigest reports a SHA-256 in lowercase hex.
func validHexDigest(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// CreateProposal records a proposal a running turn of the owner's made.
// Create-only on the proposal's own subject.
func (w *Writer) CreateProposal(ctx context.Context, c Call, p NewProposal) (statelog.Result, error) {
	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	if err := p.validate(); err != nil {
		return statelog.Result{}, err
	}
	card, err := json.Marshal(p.Card)
	if err != nil {
		return statelog.Result{}, fmt.Errorf("assistantdomain: encode proposal %s's card: %w",
			p.Proposal, err)
	}
	if len(card) > CardMaxBytes {
		return statelog.Result{}, invalid("the card is %d bytes encoded and the maximum "+
			"is %d", len(card), CardMaxBytes)
	}
	conversation, err := w.turnOf(ctx, c.Owner, p.Turn)
	if err != nil {
		return statelog.Result{}, err
	}
	seal := func(plain []byte, column string) (string, error) {
		return w.sealer.Seal(plain, AAD(conversation, RecordProposal, p.Proposal, column))
	}
	query, err := seal([]byte(p.Query.Encode()), ColumnQuery)
	if err != nil {
		return statelog.Result{}, err
	}
	body, err := seal(p.Body, ColumnBody)
	if err != nil {
		return statelog.Result{}, err
	}
	cardSealed, err := seal(card, ColumnCard)
	if err != nil {
		return statelog.Result{}, err
	}
	at := w.now()
	subject, scope := ProposalSubject(conversation, p.Proposal), ScopeSet{Subject: true}
	return w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternCreate,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := owned(ctx, tx, c.Owner, conversation.String()); err != nil {
				return statelog.Decision{}, err
			}
			state, found, err := turnState(ctx, tx, conversation.String(), p.Turn.String())
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !found:
				return statelog.Decision{}, fmt.Errorf("%w: turn %s", ErrNotFound, p.Turn)
			case state != wire.TurnRunning:
				return statelog.Decision{}, &TurnMoved{State: state}
			}
			return decision(stamp, c.By, subject, OpProposalCreated, scope, c.OpID,
				ProposalCreated{
					V: baseRecordVersion, Turn: p.Turn.String(), Owner: c.Owner.String(),
					Tool: p.Tool, Act: p.Act, Scope: p.Scope, Method: p.Method,
					Path: p.Path, QuerySealed: query, BodySealed: body,
					RequestDigest: p.RequestDigest, Digest: p.Digest, OpKey: p.OpKey,
					SourceRevision: p.SourceRevision, StepUp: p.StepUp,
					CardSealed: cardSealed, ExpiresAt: p.ExpiresAt.UTC(),
				}, at)
		}))
}

// ProposalMove is one move of a proposal: the state the caller decided it from
// (as internal/assistant/proposal's state machine allows), the state it moves
// to, and for a settlement what the executed request answered.
type ProposalMove struct {
	From wire.ProposalState
	To   wire.ProposalState

	// Outcome and Position are the state log's answer to the executed
	// request, for a server write that settled.
	Outcome  string
	Position string

	// Receipt is what an applied proposal did, and Failure the engine's
	// refusal of a failed one. Sealed.
	Receipt *wire.Receipt
	Failure *wire.Failure

	// RefusalCode is the refusal's code in the clear, for a failed or a
	// stale proposal.
	RefusalCode string
}

// settlement is what a settled proposal's sealed receipt holds.
type settlement struct {
	Receipt *wire.Receipt `json:"receipt,omitempty"`
	Failure *wire.Failure `json:"failure,omitempty"`
}

// proposalOf is the conversation one of the owner's proposals was made in.
func (w *Writer) proposalOf(ctx context.Context, owner, proposal uuid.UUID) (uuid.UUID, error) {
	var conversation string
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT conversation_id FROM assistant_proposals WHERE id = ? AND owner_id = ?`,
			proposal.String(), owner.String()).Scan(&conversation)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return uuid.Nil, fmt.Errorf("%w: proposal %s", ErrNotFound, proposal)
	case err != nil:
		return uuid.Nil, fmt.Errorf("assistantdomain: read proposal %s: %w", proposal, err)
	}
	return uuid.Parse(conversation)
}

// MoveProposal moves one of the owner's proposals, ARBITRATED on its subject
// and decided from the state the caller names: a proposal in any other state
// is refused [AlreadyDecided] naming the state it is in now, so of two
// approvals on two nodes exactly one lands.
func (w *Writer) MoveProposal(ctx context.Context, c Call, proposal uuid.UUID,
	m ProposalMove) (statelog.Result, error) {

	if err := c.validate(true); err != nil {
		return statelog.Result{}, err
	}
	if !m.From.Valid() || !m.To.Valid() {
		return statelog.Result{}, invalid("a move names the state it is decided from "+
			"and the state it moves to, each one of %s", words.Or(wire.ProposalStates))
	}
	op, ok := moveOp(m.From, m.To)
	if !ok {
		return statelog.Result{}, invalid("no move takes a proposal to %s", m.To)
	}
	settles := op == OpProposalSettled
	if (m.Receipt != nil || m.Failure != nil || m.Outcome != "" || m.Position != "") && !settles {
		return statelog.Result{}, invalid("only a settlement carries what the request answered")
	}
	for _, f := range []struct{ field, value string }{
		{"the outcome", m.Outcome}, {"the position", m.Position},
		{"the refusal code", m.RefusalCode},
	} {
		if err := checkText(f.field, f.value, TitleMaxBytes); err != nil {
			return statelog.Result{}, err
		}
	}
	conversation, err := w.proposalOf(ctx, c.Owner, proposal)
	if err != nil {
		return statelog.Result{}, err
	}
	var receipt string
	if settles && (m.Receipt != nil || m.Failure != nil) {
		plain, err := json.Marshal(settlement{Receipt: m.Receipt, Failure: m.Failure})
		if err != nil {
			return statelog.Result{}, fmt.Errorf("assistantdomain: encode proposal %s's "+
				"receipt: %w", proposal, err)
		}
		if receipt, err = w.sealer.Seal(plain,
			AAD(conversation, RecordProposal, proposal, ColumnReceipt)); err != nil {
			return statelog.Result{}, err
		}
	}
	at := w.now()
	subject, scope := ProposalSubject(conversation, proposal), ScopeSet{Subject: true}
	return w.publish(ctx, request(subject, scope, c.OpID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := owned(ctx, tx, c.Owner, conversation.String()); err != nil {
				return statelog.Decision{}, err
			}
			state, found, err := proposalState(ctx, tx, proposal.String())
			switch {
			case err != nil:
				return statelog.Decision{}, err
			case !found:
				return statelog.Decision{}, fmt.Errorf("%w: proposal %s", ErrNotFound, proposal)
			case state != m.From:
				return statelog.Decision{}, &AlreadyDecided{State: state}
			}
			return decision(stamp, c.By, subject, op, scope, c.OpID, ProposalMoved{
				V: baseRecordVersion, From: m.From, To: m.To, Outcome: m.Outcome,
				Position: m.Position, ReceiptSealed: receipt, RefusalCode: m.RefusalCode,
			}, at)
		}))
}

// Approve records the owner's approval of a proposed proposal, refused
// [AlreadyDecided] when the proposal has left `proposed`: the arbitrated record
// two approvals on two nodes resolve to one of.
func (w *Writer) Approve(ctx context.Context, c Call, proposal uuid.UUID) (statelog.Result, error) {
	return w.MoveProposal(ctx, c, proposal, ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalApproved,
	})
}

// Decline records the owner's decline of a proposed proposal.
func (w *Writer) Decline(ctx context.Context, c Call, proposal uuid.UUID) (statelog.Result, error) {
	return w.MoveProposal(ctx, c, proposal, ProposalMove{
		From: wire.ProposalProposed, To: wire.ProposalDeclined,
	})
}

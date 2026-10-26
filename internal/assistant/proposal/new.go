package proposal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// Draft is everything a running turn knows when a write tool's call becomes a
// proposal: whose turn it is, which tool and verb, and the server's own
// preview of the call.
type Draft struct {
	Owner        uuid.UUID
	Conversation uuid.UUID
	Turn         uuid.UUID

	// Tool is the catalogue tool the model called, and Act the verb it
	// executes as (the operator tool's name for an act route write, the
	// artifacts surface's verb for a share); empty for a page action.
	Tool string
	Act  string

	Scope   wire.ProposalScope
	Preview Preview

	// ChangesVisibility marks a write that changes who sees something,
	// which must name its audience (D09).
	ChangesVisibility bool

	// PageRevision is the open screen's revision, the turn's page context
	// `source_revision`: what a page action is prepared against. Only the
	// turn sees the page, so a page action's previewer leaves its own
	// revision empty and the turn names it here. Required for a page-scope
	// proposal, because a page action with nothing to compare against could
	// never be found stale.
	PageRevision string

	// Taint names the outside source the turn read before it proposed
	// (D25), shown on the card.
	Taint string

	// Now is the instant the proposal is made, and TTL how long it waits
	// (zero is [DefaultTTL]).
	Now time.Time
	TTL time.Duration
}

// ErrTooLarge reports a change too large to show on one card: nothing can be
// approved that the person could not be shown whole.
var ErrTooLarge = errors.New("proposal: the change is too large to show on one card")

// New is the proposal a draft becomes: a fresh id and operation key, the
// request digest and the digest over every field, and the card rendered from
// the preview and nothing else. The caller records it
// ([assistantdomain.Writer.CreateProposal]) as it is.
func New(d Draft) (assistantdomain.NewProposal, error) {
	p := d.Preview
	if err := p.Check(d.ChangesVisibility); err != nil {
		return assistantdomain.NewProposal{}, err
	}
	page := p.Request.Method == PageMethod
	switch {
	case d.Scope == wire.ScopePage && (!page || !strings.HasPrefix(p.Request.Path, PageActionPath)):
		return assistantdomain.NewProposal{}, fmt.Errorf("%w: a page action's request is "+
			"%s %s<action>", ErrInvalid, PageMethod, PageActionPath)
	case d.Scope == wire.ScopeServer && page:
		return assistantdomain.NewProposal{}, fmt.Errorf("%w: a server write is sent, so "+
			"its method is an HTTP verb", ErrInvalid)
	case d.Now.IsZero():
		return assistantdomain.NewProposal{}, fmt.Errorf("%w: a draft names its instant", ErrInvalid)
	case d.Scope == wire.ScopePage && d.PageRevision == "":
		return assistantdomain.NewProposal{}, fmt.Errorf("%w: a page action is prepared "+
			"against the open screen's revision, and the turn's page context names none",
			ErrInvalid)
	case d.Scope == wire.ScopePage && p.SourceRevision != "" && p.SourceRevision != d.PageRevision:
		return assistantdomain.NewProposal{}, fmt.Errorf("%w: the preview read revision %q "+
			"and the open screen is at %q", ErrInvalid, p.SourceRevision, d.PageRevision)
	case d.Scope == wire.ScopeServer && d.PageRevision != "":
		return assistantdomain.NewProposal{}, fmt.Errorf("%w: a server write is judged "+
			"against its record's revision, never the screen's", ErrInvalid)
	}
	if d.Scope == wire.ScopePage {
		p.SourceRevision = d.PageRevision
	}
	id, err := uuid.NewV7()
	if err != nil {
		return assistantdomain.NewProposal{}, fmt.Errorf("proposal: mint an id: %w", err)
	}
	key, err := uuid.NewV7()
	if err != nil {
		return assistantdomain.NewProposal{}, fmt.Errorf("proposal: mint an operation key: %w", err)
	}
	ttl := d.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	request, err := RequestDigest(p.Request)
	if err != nil {
		return assistantdomain.NewProposal{}, err
	}
	fields := Fields{
		Owner: d.Owner, Conversation: d.Conversation, Turn: d.Turn, Proposal: id,
		Scope: d.Scope, Tool: d.Tool, Act: d.Act, RequestDigest: request,
		SourceRevision: p.SourceRevision, StepUp: p.StepUp,
		// AT THE SECOND, which is what the digest binds, so the instant
		// the card shows is the instant that is hashed.
		ExpiresAt: d.Now.Add(ttl).UTC().Truncate(time.Second),
	}
	digest, err := Digest(fields)
	if err != nil {
		return assistantdomain.NewProposal{}, err
	}
	card := wire.Card{Type: wire.CardProposal, Proposal: &wire.ProposalCard{
		ProposalID: id, Scope: d.Scope, Tool: d.Tool, Title: p.Title, Target: p.Target,
		Changes: Rows(p.Changes), Summary: p.Summary, Note: p.Note, Risk: p.Risk,
		Technical: p.Technical, Audience: p.Audience,
		Taint:  d.Taint,
		StepUp: p.StepUp, Digest: digest, ExpiresAt: fields.ExpiresAt,
		State: wire.ProposalProposed, SourceRevision: p.SourceRevision,
		ApproveLabel: p.ApproveLabel,
	}}
	if d.Scope == wire.ScopePage {
		card.Proposal.PageAction = &wire.PageActionRef{
			Name:    strings.TrimPrefix(p.Request.Path, PageActionPath),
			Payload: p.Request.Body,
		}
	}
	if card, err = fit(card); err != nil {
		return assistantdomain.NewProposal{}, err
	}
	return assistantdomain.NewProposal{
		Turn: d.Turn, Proposal: id, Tool: d.Tool, Act: d.Act, Scope: d.Scope,
		Method: p.Request.Method, Path: p.Request.Path, Query: p.Request.Query,
		Body: p.Request.Body, RequestDigest: request, Digest: digest, OpKey: key.String(),
		SourceRevision: p.SourceRevision, StepUp: p.StepUp, Card: card,
		ExpiresAt: fields.ExpiresAt,
	}, nil
}

// fit keeps a card inside the bound one card is stored at. A text change
// carries its whole values beside its word runs, so a long change is shown in
// full behind "Show the whole change"; where that is over the bound, the whole
// values are dropped and the runs, which join to both, carry the change alone.
// A card still over the bound is [ErrTooLarge].
func fit(card wire.Card) (wire.Card, error) {
	if size(card) <= assistantdomain.CardMaxBytes {
		return card, nil
	}
	p := *card.Proposal
	p.Changes = append([]wire.Change(nil), p.Changes...)
	for i := range p.Changes {
		if p.Changes[i].Kind == wire.ChangeText {
			p.Changes[i].From, p.Changes[i].To = "", ""
		}
	}
	card.Proposal = &p
	if size(card) > assistantdomain.CardMaxBytes {
		return wire.Card{}, fmt.Errorf("%w: it is %d bytes and a card holds %d", ErrTooLarge,
			size(card), assistantdomain.CardMaxBytes)
	}
	return card, nil
}

// size is a card's encoded length.
func size(card wire.Card) int {
	b, err := json.Marshal(card)
	if err != nil {
		return assistantdomain.CardMaxBytes + 1
	}
	return len(b)
}

// CardAt is a stored proposal's card at state, carrying what it did or the
// refusal it met. The card is the one its preview drew; only its state and its
// outcome move.
func CardAt(p assistantdomain.Proposal, state wire.ProposalState, receipt *wire.Receipt,
	failure *wire.Failure) wire.Card {

	card := p.Card
	if card.Proposal == nil {
		return card
	}
	body := *card.Proposal
	body.State, body.Receipt, body.Failure = state, receipt, failure
	card.Proposal = &body
	return card
}

package assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE APPROVAL PATH'S HOOKS INTO A PARKED TURN (ADR-0051): what became of a
// proposal is handed back to the turn that proposed it, which carries on from
// the call it suspended on; a decision that leaves the turn parked (an
// approved page action the page has not reported yet) appends its frames to
// the turn's stream; and a Stop of a parked turn declines what it waits on.
// internal/assistant/proposal/approval decides; the Service owns every
// goroutine a turn runs on, so a decision never runs a turn itself.

var _ approval.Turns = (*Service)(nil)

// Decliner declines every proposal a stopped turn waits on:
// *approval.Approver's DeclineTurn.
type Decliner interface {
	DeclineTurn(ctx context.Context, s approval.Stop) error
}

// BindDecliner installs the approval path a Stop of a parked turn declines
// through. ONCE, at wiring time: the approver is built over this service as
// its [approval.Turns], so the two cannot be handed to each other at
// construction.
func (s *Service) BindDecliner(d Decliner) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case d == nil:
		return errors.New("assistant: bind no decliner")
	case s.decliner != nil:
		return errors.New("assistant: a decliner is already bound")
	}
	s.decliner = d
	return nil
}

// Resume takes a parked turn back up on this node with what became of its
// proposal, on the read grant the deciding request derived, and answers where
// its stream continues. ONCE: the resume record is arbitrated, so two
// decisions of one proposal resume it at most once.
//
// The person is the deciding request's, and must be the turn's owner: a
// decision is never another person's.
func (s *Service) Resume(ctx context.Context, r approval.Resumption) (wire.TurnAccepted, error) {
	started := false
	defer func() {
		if !started && r.Read != nil {
			s.revoker.Revoke(r.Read.ID())
		}
	}()
	person, how := iam.From(ctx)
	switch {
	case how != iam.Resolved || person.ID != r.Owner:
		return wire.TurnAccepted{}, fmt.Errorf("%w: a turn resumes on its owner's own "+
			"decision", ErrNotFound)
	case r.Read == nil:
		return wire.TurnAccepted{}, errors.New("assistant: a resumed turn needs the read " +
			"grant derived from the deciding request")
	}
	if err := s.eligible(person); err != nil {
		return wire.TurnAccepted{}, err
	}
	row, err := s.transcripts.Turn(ctx, r.Owner, r.Turn)
	if err != nil {
		return wire.TurnAccepted{}, s.readFailure(err)
	}
	if row.Conversation == r.Conversation && row.State == wire.TurnSuperseded {
		return s.settledPast(ctx, person, row, r, nil)
	}
	if row.State != wire.TurnWaitingForApproval || row.Conversation != r.Conversation {
		return wire.TurnAccepted{}, ErrTurnRunning
	}
	keep, err := s.parkedLoop(ctx, r.Owner, r.Turn)
	if err != nil {
		return wire.TurnAccepted{}, err
	}
	if !holds(person, keep.Apps) {
		s.dropParkedDeep(ctx, keep)
		// THE SECOND LOCK: whatever decided the proposal, a loop that read
		// through an app the person no longer holds is never handed back to
		// the model for them. The turn ends; the card shows its state.
		return s.withdrawn(ctx, person, row, r)
	}
	// READ BEFORE THE RESUME RECORD, like everything that can fail: once it
	// lands the turn is running on this node, and a return after it would
	// leave it running with no runner.
	transcript, err := s.transcripts.Transcript(ctx, r.Owner, r.Conversation)
	if err != nil {
		return wire.TurnAccepted{}, s.readFailure(err)
	}
	floor := s.continuing(ctx, r.Owner, r.Turn, keep.Seq)
	acts, err := s.acts(ctx, person)
	if err != nil {
		return wire.TurnAccepted{}, fmt.Errorf("assistant: the writes the person may "+
			"propose: %w", err)
	}
	if err = s.admit(ctx, person); err != nil {
		return wire.TurnAccepted{}, err
	}
	defer s.admissions.Done()
	admitted := true
	defer func() {
		if admitted {
			s.unadmit(person.ID)
		}
	}()
	meter, err := s.budget.Meter(ctx, person)
	if err != nil {
		return wire.TurnAccepted{}, fmt.Errorf("assistant: the turn's budget meter: %w", err)
	}
	if refusal := budgetRefusal(ctx, meter); refusal != nil {
		return wire.TurnAccepted{}, refusal
	}
	use := auxspend.Use{Stage: types.AuxStageOperator, Purpose: types.AuxAssistantTurn,
		TurnID: r.Turn.String()}
	provider, key, err := s.models.Chain(ctx, person, use)
	if err != nil {
		return wire.TurnAccepted{}, &Unavailable{Cause: err}
	}
	page, fresh := s.resumedPage(ctx, keep, r)
	t := s.newTurn(person, r.Conversation, r.Turn, "", r.Read, page, acts)
	t.derived = &Derivation{At: row.StartedAt, Name: "assistant_resume",
		Identity: []string{r.Turn.String(), r.Proposal.String()}}
	if provenance.Boundary(keep.Boundary).Valid() {
		t.boundary = provenance.Boundary(keep.Boundary)
	}
	if err = s.records.ResumeTurn(ctx, t.write().Step("resume"), r.Turn, s.node); err != nil {
		t.cancel(nil)
		return wire.TurnAccepted{}, s.writeFailure(err)
	}
	t.steps, t.cards, t.messages = keep.Steps, keep.Cards, keep.Count
	// EVERY STEP OF A PARKED SEGMENT HAD ENDED: a loop parks only between
	// its calls.
	t.finished = keep.Steps
	if anchor, err := uuid.Parse(keep.Anchor); err == nil {
		t.anchor = anchor
	}
	t.used(keep.Apps)
	// THE TAINT RIDES THE PARK, and what the decision hands the model taints
	// the turn as a read does: a proposal this segment makes after it names
	// the outside source its model holds, whichever segment read it (D25).
	t.taintedBy(provenance.Source(keep.Outside))
	decision, screen := resultOf(r, t.boundary, t.readAnswer), ""
	if fresh {
		screen = screenNow(t.boundary, page)
	}
	messages := slices.Concat(keep.Messages, []llm.Message{{Role: llm.RoleTool, Name: keep.PendingTool,
		ToolCallID: keep.PendingCall, Content: joinBlocks(decision, screen),
		Failed: r.State != wire.ProposalApplied}})

	after := s.openStream(t, transcript.Conversation, true, floor)
	s.settleWaiting(ctx, t.write(), waitingSteps(transcript, r.Turn), r.State, r.Failure, t.emit)
	if r.Card.Type != "" {
		if err := s.showProposal(ctx, t, r.Proposal, r.Card); err != nil {
			s.log.WarnContext(ctx, "assistant_proposal_card_unshown", "turn", r.Turn,
				"proposal", r.Proposal, "error", redact.Secrets(err.Error()))
		}
	}
	if r.Invalidate != nil {
		t.emit(invalidation(r.Proposal, r.Invalidate))
	}
	c, _ := s.company()
	assistantCfg := config.Assistant{}
	if c != nil {
		assistantCfg = c.Assistant
	}
	admitted, started = false, true
	s.track(t)
	go t.run(loopRun{ //nolint:contextcheck // the turn's own lifetime (t.ctx, under the service's base), never the request's: a turn outlives the POST that started it
		messages: messages, provider: provider, key: key, meter: meter,
		tools: s.withDeep(ctx, s.catalogue.Offered(person, acts)), steps: assistantCfg.Steps(),
		timeout: assistantCfg.TurnTimeout(), deep: keep.Deep, decision: decision, screen: screen})
	return wire.TurnAccepted{TurnID: r.Turn, StreamURL: StreamPath(r.Conversation, r.Turn),
		AfterSeq: after}, nil
}

// resumedPage is the screen a resumed segment runs on: the one the decision
// was made from, as it is after the decision, admitted exactly as a
// question's is; else the one the segment parked on. A turn resumed with no
// page would offer no page action and read no draft, though the person's
// screen accepted one moments before. fresh says the decision carried it,
// so the model is shown the screen again.
//
// A TURN ASKED WITHOUT A PAGE NEVER GAINS ONE: the person removed the chip
// from that question, and a decision made from whatever screen they are on
// later is not their consent to show it.
func (s *Service) resumedPage(ctx context.Context, keep parked, r approval.Resumption) (*wire.PageContext, bool) {
	if r.Page == nil || keep.Page == nil {
		return keep.Page, false
	}
	page, err := admitPage(r.Page)
	if err != nil {
		// The route refused an invalid one before deciding; a page that
		// fails here is the parked one's to stand in for.
		s.log.WarnContext(ctx, "assistant_resume_page_refused", "turn", r.Turn,
			"error", redact.Secrets(err.Error()))
		return keep.Page, false
	}
	return page, page != nil
}

// joinBlocks is the non-empty blocks of a tool's answer, a blank line apart.
func joinBlocks(blocks ...string) string {
	return strings.Join(slices.DeleteFunc(blocks, func(b string) bool { return b == "" }), "\n\n")
}

// screenNow is the screen a decision was made from, as the resumed model
// reads it after the decision's result.
func screenNow(b provenance.Boundary, page *wire.PageContext) string {
	return "The person's screen now, after this decision, described by the screen " +
		"itself (it replaces any earlier description of the screen):\n" + pageFence(b, page)
}

// answerTrust is how far a resumed turn may rely on what a write's request
// answered: what the write's catalogue tool declares its answer carries
// ([catalog.Tool.Trust]: a coding run's question beside answer_coding_run's
// receipt, read by the one rule for a coding run's words), and otherwise the
// engine's own records, a colleague's by the one rule for a surface the engine
// hosts.
func answerTrust(r approval.Resumption) provenance.Trust {
	if p := r.Card.Proposal; p != nil {
		if tool, ok := catalog.Lookup(p.Tool); ok && tool.Trust.Valid() {
			return tool.Trust
		}
	}
	return provenance.TrustForHosted("", nil, false)
}

// resultOf is the answer to the call a turn suspended on, as its model reads
// it: the engine's own words for what became of the proposal; the route's
// answer to a write that was sent, fenced as an API answer, since it carries
// text other people wrote; and a page's own report fenced as the page's,
// never as the engine's, since the engine cannot verify it. Each fenced
// answer is told to read, with its source and the trust it is fenced at,
// which taints the turn as a step's read does ([turn.readAnswer]).
func resultOf(r approval.Resumption, b provenance.Boundary,
	read func(provenance.Source, provenance.Trust)) string {

	var out strings.Builder
	// THE CHANGE IS NAMED BY ITS ACTION, which the catalogue wrote ("Create
	// task"); its target is the model's own argument, and may carry text it
	// read, so it stays out of the engine's words. The receipt names what
	// the write made.
	if p := r.Card.Proposal; p != nil && oneLine(p.Title) != "" {
		fmt.Fprintf(&out, "%s: ", oneLine(p.Title))
	}
	switch r.State {
	case wire.ProposalApplied:
		out.WriteString("The person approved the change and it was applied.")
	case wire.ProposalDeclined:
		out.WriteString("The person declined the change. Nothing was sent.")
	case wire.ProposalStale:
		out.WriteString("The record changed after the change was proposed, so nothing " +
			"was sent. Read it again before proposing anything else.")
	default:
		fmt.Fprintf(&out, "The change ended %s.", r.State)
	}
	if r.Failure != nil {
		fmt.Fprintf(&out, " Refusal: %s (%s).", redact.Secrets(r.Failure.Refusal.ForModel()),
			r.Failure.Refusal.Code)
	}
	if r.Receipt != nil && !r.ClientReported {
		fmt.Fprintf(&out, " %s", redact.Secrets(r.Receipt.Summary))
	}
	if len(r.Answer) > 0 {
		answer := redact.Secrets(string(r.Answer))
		if len(answer) > ModelResultBytes {
			answer = textcut.Within(answer, ModelResultBytes)
		}
		// AT THE TRUST WHAT IT CARRIES EARNS ([answerTrust]), naming none of
		// its writers, as every route's answer is read.
		trust := answerTrust(r)
		read(provenance.SourceAPIAnswer, trust)
		out.WriteString("\n" + provenance.Fence(b, provenance.SourceAPIAnswer, trust, nil,
			answer))
	}
	if r.ClientReported && r.Receipt != nil {
		out.WriteString("\n" + pageReport(b, r.Receipt.Summary, read))
	}
	return out.String()
}

// pageReport is what a page reported of a page action it applied, as the model
// reads it: fenced at the page's own trust, which no set of writers earns,
// because the engine cannot verify what a page says it did, and told to read
// at it.
func pageReport(b provenance.Boundary, summary string,
	read func(provenance.Source, provenance.Trust)) string {

	trust := provenance.TrustPage
	read(provenance.SourcePageContext, trust)
	return provenance.Fence(b, provenance.SourcePageContext, trust, nil,
		redact.Secrets(summary))
}

// Emit appends frames to a turn that stays parked, or that the person asked
// past (superseded): the proposal card at its state now and, for an approved
// page action, the action the page applies; then the segment ends again in
// the turn's state, so the client's stream closes cleanly. It answers where
// the client reads them.
func (s *Service) Emit(ctx context.Context, owner, conversation, turnID uuid.UUID,
	e approval.Emission) (wire.TurnAccepted, error) {

	row, err := s.transcripts.Turn(ctx, owner, turnID)
	if err != nil {
		return wire.TurnAccepted{}, s.readFailure(err)
	}
	if row.Conversation != conversation || (row.State != wire.TurnWaitingForApproval &&
		row.State != wire.TurnSuperseded) {
		return wire.TurnAccepted{}, ErrTurnRunning
	}
	person, how := iam.From(ctx)
	if how != iam.Resolved || person.ID != owner {
		return wire.TurnAccepted{}, fmt.Errorf("%w: frames are emitted on the owner's own "+
			"decision", ErrNotFound)
	}
	var parkedSeq uint64
	if row.State == wire.TurnWaitingForApproval {
		keep, err := s.parkedLoop(ctx, owner, turnID)
		if err != nil {
			return wire.TurnAccepted{}, err
		}
		parkedSeq = keep.Seq
	}
	return s.tell(person, row, s.continuing(ctx, owner, turnID, parkedSeq),
		func(t *turn) error {
			if e.Card != nil {
				if err := s.showProposal(ctx, t, uuid.Nil, *e.Card); err != nil {
					return err
				}
			}
			if e.Action != nil {
				action := *e.Action
				t.emit(wire.Frame{Kind: wire.EventAction, Action: &action})
			}
			return nil
		})
}

// tell appends frames to a turn no runner holds (one parked, or ended): a
// segment of its stream numbered after floor, what frames writes, then how
// the turn stands (the waiting status for a parked turn) and the done frame
// in its state. It answers where the client reads them.
func (s *Service) tell(person iam.Principal, row assistantdomain.Turn,
	floor uint64, frames func(t *turn) error) (wire.TurnAccepted, error) {

	t := s.newTurn(person, row.Conversation, row.ID, "", nil, nil, nil)
	defer t.cancel(nil)
	t.log = s.hub.Open(person.ID, row.Conversation, row.ID, floor)
	defer t.log.End()
	after := t.log.Last()
	if frames != nil {
		if err := frames(t); err != nil {
			return wire.TurnAccepted{}, err
		}
	}
	if row.State == wire.TurnWaitingForApproval {
		t.emit(wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{
			State: wire.StatusWaitingForApproval, StepsDone: row.Steps}})
	}
	t.emit(wire.Frame{Kind: wire.EventDone, Done: &wire.DoneEvent{State: row.State,
		Steps: row.Steps}})
	return wire.TurnAccepted{TurnID: row.ID, StreamURL: StreamPath(row.Conversation, row.ID),
		AfterSeq: after}, nil
}

// Carries reports whether the person the context resolves may have owner's
// parked turn carry on (approval.Turns): false where they no longer hold an
// app its loop read through. A turn no loop will resume (one superseded, or
// over) carries nothing back to the model, and answers true.
func (s *Service) Carries(ctx context.Context, owner, conversation, turnID uuid.UUID) (bool, error) {
	person, how := iam.From(ctx)
	if how != iam.Resolved || person.ID != owner {
		return false, fmt.Errorf("%w: a turn carries on for its owner alone", ErrNotFound)
	}
	row, err := s.transcripts.Turn(ctx, owner, turnID)
	if err != nil {
		return false, s.readFailure(err)
	}
	if row.Conversation != conversation || row.State != wire.TurnWaitingForApproval {
		return true, nil
	}
	keep, err := s.parkedLoop(ctx, owner, turnID)
	if err != nil {
		return false, err
	}
	return holds(person, keep.Apps), nil
}

// withdrawn ends a parked turn whose person no longer holds an app its loop
// read through, instead of resuming it: closed failed (access withdrawn),
// once across the fleet, and its stream shown the card at its state now and
// why the assistant did not carry on.
func (s *Service) withdrawn(ctx context.Context, person iam.Principal,
	row assistantdomain.Turn, r approval.Resumption) (wire.TurnAccepted, error) {

	w := Write{Owner: person.ID, By: iam.ActorFor(person), Derived: &Derivation{
		At: row.StartedAt, Name: "assistant_withdrawn",
		Identity: []string{row.ID.String(), r.Proposal.String()}}}
	state, err := s.closeDerived(ctx, w, row.ID, assistantdomain.TurnEnd{State: wire.TurnFailed,
		Steps: row.Steps, ErrorCode: string(wire.ErrorAccessWithdrawn)})
	if err != nil {
		return wire.TurnAccepted{}, err
	}
	s.revoker.RevokeTurn(row.ID)
	row.State = state
	return s.settledPast(ctx, person, row, r, &wire.ErrorEvent{Code: wire.ErrorAccessWithdrawn,
		Message: wire.ErrorAccessWithdrawn.Sentence(),
		Hint:    "Ask again to start from what you can open now."})
}

// settledPast shows what became of a proposal of a turn no loop resumes (one
// the person asked past, or one whose access they lost): its card at its state
// now, with the receipt, where a write landed the `context.invalidate` the
// open screens refresh by, and failure, if any. The loop is NOT resumed: a
// person who moved on to another question would have a model answer into the
// middle of it, and one who lost access may not be handed what it read. The
// decision's read grant ends unused ([Service.Resume] revokes a grant no
// runner takes).
func (s *Service) settledPast(ctx context.Context, person iam.Principal,
	row assistantdomain.Turn, r approval.Resumption, failure *wire.ErrorEvent) (wire.TurnAccepted, error) {

	transcript, err := s.transcripts.Transcript(ctx, r.Owner, r.Conversation)
	if err != nil {
		return wire.TurnAccepted{}, s.readFailure(err)
	}
	return s.tell(person, row, s.continuing(ctx, r.Owner, r.Turn, 0), func(t *turn) error {
		t.derived = &Derivation{At: row.StartedAt, Name: "assistant_settled_past",
			Identity: []string{r.Turn.String(), r.Proposal.String()}}
		s.settleWaiting(ctx, t.write(), waitingSteps(transcript, r.Turn), r.State, r.Failure,
			t.emit)
		if r.Card.Type != "" {
			if err := s.showProposal(ctx, t, r.Proposal, r.Card); err != nil {
				return err
			}
		}
		if r.Invalidate != nil {
			t.emit(invalidation(r.Proposal, r.Invalidate))
		}
		if failure != nil {
			t.emit(wire.Frame{Kind: wire.EventError, Error: failure})
		}
		return nil
	})
}

// invalidation is the `context.invalidate` action for a write proposal
// landed at, under an id derived from the proposal, so a repeat is one action.
func invalidation(proposal uuid.UUID, at *wire.Invalidate) wire.Frame {
	return wire.Frame{Kind: wire.EventAction, Action: &wire.ActionEvent{
		ActionID: uuid.NewSHA1(proposal, []byte(wire.ActionContextInvalidate)),
		Name:     wire.ActionContextInvalidate, Invalidate: at}}
}

// showProposal streams a proposal card at its state now under the id the
// card was first shown with, so the drawer replaces it in place. It records
// nothing: the transcript renders a stored proposal card at its state now on
// every read, so a second copy would be a second card for one proposal.
// proposal names it, or the card's own proposal where it is uuid.Nil.
func (s *Service) showProposal(ctx context.Context, t *turn, proposal uuid.UUID,
	card wire.Card) error {

	if card.Type != wire.CardProposal || card.Proposal == nil {
		return fmt.Errorf("assistant: a decision handed the turn a %s card, not a "+
			"proposal's", card.Type)
	}
	if proposal == uuid.Nil {
		proposal = card.Proposal.ProposalID
	}
	transcript, err := s.transcripts.Transcript(ctx, t.person.ID, t.conversation)
	if err != nil {
		return s.readFailure(err)
	}
	for _, c := range transcript.Cards {
		if c.Card.Type == wire.CardProposal && c.Card.Proposal != nil &&
			c.Card.Proposal.ProposalID == proposal {
			t.emit(wire.Frame{Kind: wire.EventCard, Card: &wire.CardEvent{
				CardID: c.ID, MessageID: c.Message, Card: card}})
			return nil
		}
	}
	return fmt.Errorf("assistant: no card of turn %s shows proposal %s", t.id, proposal)
}

// Render is a conversation as the reader the context resolves is shown it:
// the transcript a compliance read answers (internal/api/assistantapi's
// audited read) rendered by the same rule as the owner's own reload, so a
// card built from an app the reader does not hold is a notice for them too.
func (s *Service) Render(ctx context.Context, t assistantdomain.Transcript) (wire.Transcript, error) {
	reader, how := iam.From(ctx)
	if how != iam.Resolved {
		return wire.Transcript{}, errors.New("assistant: a transcript is rendered for a " +
			"resolved reader")
	}
	return s.render(t, reader), nil
}

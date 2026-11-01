package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/words"
)

// THE PAGE'S HALF OF A PAGE ACTION. An approved page action is sent to the
// page as an `action` frame whose action id is the proposal's id; the page
// CLAIMS it before applying it, and REPORTS what became of it after.
//
// THE CLAIM IS THE ARBITRATED MOVE from approved to applying, so of two tabs
// holding the same drawer exactly one applies the action: the other is told it
// was claimed and leaves its screen alone. The report moves applying to its
// end. Neither is authority over anything: nothing durable changed, the page
// re-ran its own queries under its own cookie, and the report is the page's
// statement, which the model reads as client-reported.

// Report is one page report: the action it names, what the page says, and the
// reporting request's operation.
type Report struct {
	Action uuid.UUID
	Report wire.ActionReport
	Key    string
}

// ErrInvalidReport is a report this build cannot record.
var ErrInvalidReport = errors.New("approval: the report is not one a page action records")

// Report records a page's claim on or report of one of the person's page
// actions.
func (a *Approver) Report(ctx context.Context, r Report) (wire.ActionAnswer, error) {
	d, err := a.deciderOf(ctx, r.Key, r.Report.PageContext)
	if err != nil {
		return wire.ActionAnswer{}, err
	}
	if !r.Report.State.Valid() {
		return wire.ActionAnswer{}, fmt.Errorf("%w: state %q is not one of %s",
			ErrInvalidReport, r.Report.State, words.Or(wire.ActionStates))
	}
	p, err := a.load(ctx, d.person.ID, r.Action)
	if err != nil {
		return wire.ActionAnswer{}, err
	}
	if p.Scope != wire.ScopePage {
		// A SERVER WRITE IS NOT THE PAGE'S TO REPORT: its outcome is the
		// act route's answer. Not found, because no page action has this id.
		return wire.ActionAnswer{}, ErrNotFound
	}
	if r.Report.State == wire.ActionApplying {
		if r.Report.Revision != p.SourceRevision {
			return a.pageMoved(ctx, d, p, r.Report.Revision)
		}
		return a.claim(ctx, d, p)
	}
	return a.reported(ctx, d, p, r.Report)
}

// claim takes an approved page action for one tab.
func (a *Approver) claim(ctx context.Context, d decider, p assistantdomain.Proposal) (wire.ActionAnswer, error) {
	_, err := a.move(ctx, d.call("claim"), p.ID, assistantdomain.ProposalMove{
		From: wire.ProposalApproved, To: wire.ProposalApplying})
	switch {
	case errors.Is(err, ErrAlreadyDecided):
		return wire.ActionAnswer{}, ErrActionClaimed
	case err != nil:
		return wire.ActionAnswer{}, err
	}
	return wire.ActionAnswer{ActionID: p.ID, State: wire.ActionApplying}, nil
}

// pageMoved records a page action whose screen moved since the action was
// prepared, refuses the claim, and tells the turn, so the model may prepare it
// again against the screen as it is now. Decided by the SERVER from the
// revision the claim names, so a page that would have applied a stale action
// cannot.
func (a *Approver) pageMoved(ctx context.Context, d decider, p assistantdomain.Proposal,
	revision string) (wire.ActionAnswer, error) {

	to, err := proposal.Next(wire.ProposalApproved, proposal.EventPageStale)
	if err != nil {
		return wire.ActionAnswer{}, err
	}
	if _, err := a.move(ctx, d.call("claim"), p.ID, assistantdomain.ProposalMove{
		From: wire.ProposalApproved, To: to, RefusalCode: CodeStale}); err != nil {
		if errors.Is(err, ErrAlreadyDecided) {
			return wire.ActionAnswer{}, ErrActionClaimed
		}
		return wire.ActionAnswer{}, err
	}
	failure := &wire.Failure{Refusal: wire.Refusal{Code: CodeStale,
		Message: "The page changed after this was prepared, so nothing was applied."}}
	if immediate(p) {
		return wire.ActionAnswer{}, ErrStale
	}
	if read, err := a.d.Grants.Read(ctx, p.Conversation, p.Turn); err == nil {
		a.resume(ctx, Resumption{Owner: d.person.ID, Page: d.page, Conversation: p.Conversation,
			Turn: p.Turn, Proposal: p.ID, Read: read, State: to, Failure: failure,
			ClientReported: true, Card: proposal.CardAt(p, to, nil, failure)})
	} else {
		log.InfoContext(ctx, "assistant_page_stale_not_resumed", "proposal", p.ID,
			"revision", revision, "error", err)
	}
	return wire.ActionAnswer{}, ErrStale
}

// immediate reports whether p is a page action its screen declares
// `confirm: none` (D11): the turn approved it itself and handed it to the page
// without parking, so what the page reports settles the proposal and resumes
// nothing; the turn carried on (or ended) the moment it sent the action.
func immediate(p assistantdomain.Proposal) bool {
	if p.Scope != wire.ScopePage {
		return false
	}
	action, ok := catalog.PageActionOf(strings.TrimPrefix(p.Path, proposal.PageActionPath))
	return ok && action.Confirm == wire.ConfirmNone
}

// pageEnd is where a page's report takes a claimed action.
func pageEnd(s wire.ActionState) (proposal.Event, bool) {
	switch s {
	case wire.ActionApplied:
		return proposal.EventApplied, true
	case wire.ActionStale:
		return proposal.EventPageStale, true
	case wire.ActionBlocked, wire.ActionInvalid, wire.ActionNoHandler:
		return proposal.EventRefused, true
	}
	return "", false
}

// reported settles a claimed page action with the page's report and resumes
// the turn with it.
func (a *Approver) reported(ctx context.Context, d decider, p assistantdomain.Proposal,
	report wire.ActionReport) (wire.ActionAnswer, error) {

	event, ok := pageEnd(report.State)
	if !ok {
		return wire.ActionAnswer{}, fmt.Errorf("%w: %s", ErrInvalidReport, report.State)
	}
	to, err := proposal.Next(wire.ProposalApplying, event)
	if err != nil {
		return wire.ActionAnswer{}, err
	}
	m := assistantdomain.ProposalMove{From: wire.ProposalApplying, To: to}
	var receipt *wire.Receipt
	var failure *wire.Failure
	switch to {
	case wire.ProposalApplied:
		summary := report.Receipt
		if summary == "" {
			summary = "Applied to the page"
		}
		receipt = &wire.Receipt{Summary: summary}
		m.Receipt, m.Outcome = receipt, "applied"
	case wire.ProposalStale:
		m.RefusalCode = string(report.State)
	default:
		failure = &wire.Failure{Refusal: wire.Refusal{Code: string(report.State),
			Message: report.Reason}}
		m.Failure, m.RefusalCode = failure, string(report.State)
	}
	if _, err := a.move(ctx, d.call("report"), p.ID, m); err != nil {
		var decided *Decided
		if errors.As(err, &decided) {
			if decided.State == to {
				// The same report again, after the first landed.
				return wire.ActionAnswer{ActionID: p.ID, State: report.State}, nil
			}
			return wire.ActionAnswer{}, ErrActionClaimed
		}
		return wire.ActionAnswer{}, err
	}
	if immediate(p) {
		return wire.ActionAnswer{ActionID: p.ID, State: report.State}, nil
	}
	if read, err := a.d.Grants.Read(ctx, p.Conversation, p.Turn); err == nil {
		a.resume(ctx, Resumption{Owner: d.person.ID, Page: d.page, Conversation: p.Conversation,
			Turn: p.Turn, Proposal: p.ID, Read: read, State: to, Receipt: receipt,
			Failure: failure, ClientReported: true,
			Card: proposal.CardAt(p, to, receipt, failure)})
	} else {
		log.InfoContext(ctx, "assistant_page_report_not_resumed", "proposal", p.ID,
			"error", err)
	}
	return wire.ActionAnswer{ActionID: p.ID, State: report.State}, nil
}

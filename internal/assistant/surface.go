package assistant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE ASSISTANT'S OWN SURFACE: the tools one turn offers its model, and what a
// call to one does. Never the agent runtime's tools.Surface, which acts as a
// seat: every call here is the person's own request through the API's one
// handler chain, decided by the route it asks (ADR-0049).
//
// # What a call can do
//
//   - A READ is checked and built by its catalogue tool ([catalog.Tool.Request]:
//     its schema, the identity arguments no tool takes, and a route it
//     declares), sent through the turn's read grant, and shaped by
//     [catalog.Shape] into the cards the person sees and the text the model
//     reads. Shape is the ONE place an answer is redacted, fenced and cut for
//     the model; this surface hands the model what Shape returns and nothing
//     else.
//   - A WRITE or PAGE ACTION is never sent: its tool's own preview reads the
//     object as the person, [proposal.New] binds the change to its digest and
//     revision, the proposal is recorded and carded, and the loop suspends with
//     the call unanswered until the person decides (ADR-0050).
//   - A NAVIGATION builds an open in app link, whose `permitted` is asked by
//     sending the record's own read through the grant, or by the person's
//     grants for a screen that names no record.

// errGrantEnded ends a turn whose read grant the guard no longer accepts: a
// 401 answered to the turn's own grant is the session ending, a sign-out or a
// revocation, never a refusal of the one request.
var errGrantEnded = errors.New("assistant: the turn's grant was refused")

// codeInvalidInput is the API's own code for a call refused as written.
const codeInvalidInput = "invalid_input"

// surface is one turn's tool surface.
type surface struct {
	t      *turn
	byName map[string]catalog.Tool
	defs   []llm.ToolDef
}

var _ toolloop.Surface = (*surface)(nil)

// newSurface offers tools in the catalogue's order.
func newSurface(t *turn, tools []catalog.Tool) *surface {
	s := &surface{t: t, byName: map[string]catalog.Tool{}}
	for _, tool := range tools {
		if _, dup := s.byName[tool.Name]; dup {
			continue
		}
		s.byName[tool.Name] = tool
		s.defs = append(s.defs, llm.ToolDef{Name: tool.Name, Description: tool.Description,
			Parameters: tool.Parameters})
	}
	return s
}

// ToolDefs is every tool the turn offers, in a stable order so the rendered
// catalogue is byte-identical from round to round and the prompt cache holds.
func (s *surface) ToolDefs() []llm.ToolDef { return s.defs }

// Phase names the loop for telemetry.
func (s *surface) Phase() string { return "assistant" }

// Execute carries out one call.
func (s *surface) Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	tool, offered := s.byName[call.Name]
	if !offered {
		return failed(fmt.Sprintf("There is no tool named %q here. Use only the tools "+
			"you are offered.", call.Name)), nil
	}
	switch tool.Kind {
	case catalog.KindRead:
		return s.read(ctx, tool, call.Arguments)
	case catalog.KindWrite, catalog.KindPageAction:
		return s.propose(ctx, tool, call.Arguments)
	case catalog.KindNavigate:
		return s.navigate(ctx, tool, call.Arguments)
	}
	return failed("This tool cannot be used here."), nil
}

// refuseBefore records a call refused before anything was sent, and answers
// the model why.
func (s *surface) refuseBefore(ctx context.Context, tool catalog.Tool, args map[string]any, summary string,
	err error) toolloop.ToolResult {

	message := redact.Secrets(err.Error())
	step := s.t.stepStart(tool, args, nil)
	s.t.stepEnd(ctx, step, wire.StepRefused, summary, &wire.Refusal{Code: codeInvalidInput,
		Message: message}, 0, 0)
	return failed("Refused before anything was sent: " + message)
}

// read sends one read tool's request and shapes its answer.
func (s *surface) read(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	req, err := tool.Request(args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not sent", err), nil
	}
	route, _ := tool.RouteOf(req)
	shown, _ := tool.StepRequest(req)
	step := s.t.stepStart(tool, args, shown)
	resp, err := s.t.svc.dispatcher.Do(ctx, s.t.read, req)
	if err != nil {
		return s.unsent(ctx, step, tool, route, err)
	}
	if resp.Status == http.StatusUnauthorized {
		s.t.stepEnd(ctx, step, wire.StepFailed, "Signed out", nil, resp.Status, len(resp.Body))
		return toolloop.ToolResult{}, errGrantEnded
	}
	shaped, err := catalog.Shape(tool, args, resp, s.t.boundary, s.t.svc.now())
	if err != nil {
		s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be read", nil, resp.Status, len(resp.Body))
		s.t.svc.log.ErrorContext(ctx, "assistant_step_unreadable", "turn", s.t.id,
			"tool", tool.Name, "route", route, "error", redact.Secrets(err.Error()))
		return failed("The answer could not be read. Tell the person it failed."), nil
	}
	if shaped.Refusal != nil {
		state := wire.StepRefused
		if resp.Status >= http.StatusInternalServerError {
			state = wire.StepFailed
		}
		refusal := *shaped.Refusal
		s.t.stepEnd(ctx, step, state, refusalSummary(resp.Status), &refusal, resp.Status, len(resp.Body))
	} else {
		s.t.stepEnd(ctx, step, wire.StepDone, answerSummary(shaped.Cards), nil, resp.Status,
			len(resp.Body))
		s.t.tainted(tool)
	}
	for _, card := range shaped.Cards {
		s.t.emitCard(ctx, card, appNames(tool.Apps))
	}
	return toolloop.ToolResult{Output: shaped.Model, Failed: shaped.Failed()}, nil
}

// unsent ends a step whose request could not be sent at all: the turn's own
// cancellation, or a dispatcher that could not build it.
func (s *surface) unsent(ctx context.Context, step stepClock, tool catalog.Tool, route string,
	err error) (toolloop.ToolResult, error) {

	if ctx.Err() != nil {
		s.t.stepEnd(ctx, step, wire.StepStopped, "Stopped", nil, 0, 0)
		return toolloop.ToolResult{}, context.Cause(ctx)
	}
	s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be sent", nil, 0, 0)
	s.t.svc.log.ErrorContext(ctx, "assistant_step_not_sent", "turn", s.t.id,
		"tool", tool.Name, "route", route, "error", redact.Secrets(err.Error()))
	return failed("The request could not be sent. Tell the person it failed; do not " +
		"retry it."), nil
}

// propose turns a write or a page action into a proposal the person approves,
// and suspends the loop on it.
func (s *surface) propose(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	scope := wire.ScopeServer
	pageRevision := ""
	if tool.Kind == catalog.KindPageAction {
		scope = wire.ScopePage
		var accepts []string
		if s.t.page != nil {
			accepts, pageRevision = s.t.page.Accepts, s.t.page.SourceRevision
		}
		if _, err := catalog.PageActionFor(args, accepts); err != nil {
			return s.refuseBefore(ctx, tool, args, "Not proposed", err), nil
		}
	}
	if err := tool.Check(args); err != nil {
		return s.refuseBefore(ctx, tool, args, "Not proposed", err), nil
	}
	step := s.t.stepStart(tool, args, nil)
	read := func(ctx context.Context, r exec.Request) (exec.Response, error) {
		return s.t.svc.dispatcher.Do(ctx, s.t.read, r)
	}
	preview, err := tool.Preview(ctx, read, args)
	var drafted assistantdomain.NewProposal
	if err == nil {
		drafted, err = s.t.draft(tool, scope, preview, pageRevision)
	}
	if err == nil {
		err = s.t.svc.records.CreateProposal(ctx,
			s.t.write().Step("proposal", strconv.Itoa(step.Index)), drafted)
	}
	if err == nil {
		s.t.stepEnd(ctx, step, wire.StepWaiting, "Waiting for approval", nil, 0, 0)
		s.t.emitCard(ctx, drafted.Card, appNames(tool.Apps))
		return toolloop.ToolResult{Output: "Proposed; the person decides whether to " +
			"approve it. Nothing has changed yet.", Suspend: true}, nil
	}
	switch {
	case ctx.Err() != nil:
		s.t.stepEnd(ctx, step, wire.StepStopped, "Stopped", nil, 0, 0)
		return toolloop.ToolResult{}, context.Cause(ctx)
	case errors.Is(err, proposal.ErrInvalid), errors.Is(err, proposal.ErrTooLarge),
		errors.Is(err, catalog.ErrInvalidArguments):
		message := redact.Secrets(err.Error())
		s.t.stepEnd(ctx, step, wire.StepRefused, "Not proposed", &wire.Refusal{
			Code: codeInvalidInput, Message: message}, 0, 0)
		return failed("The change could not be prepared: " + message), nil
	}
	s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be prepared", nil, 0, 0)
	s.t.svc.log.ErrorContext(ctx, "assistant_proposal_failed", "turn", s.t.id,
		"tool", tool.Name, "error", redact.Secrets(err.Error()))
	return failed("The change could not be prepared. Tell the person it failed."), nil
}

// draft is the proposal a write's preview becomes: bound by [proposal.New]
// to its digest and revision, with the page's own revision for a page action,
// the outside source the turn read before proposing, and the company's
// proposal lifetime.
func (t *turn) draft(tool catalog.Tool, scope wire.ProposalScope, preview proposal.Preview,
	pageRevision string) (assistantdomain.NewProposal, error) {

	c, _ := t.svc.company()
	var ttl time.Duration
	if c != nil {
		ttl = c.Assistant.ProposalTTL()
	}
	return proposal.New(proposal.Draft{Owner: t.person.ID, Conversation: t.conversation,
		Turn: t.id, Tool: tool.Name, Act: tool.Act, Scope: scope, Preview: preview,
		ChangesVisibility: tool.ChangesVisibility, PageRevision: pageRevision,
		Taint: t.taint(), Now: t.svc.now(), TTL: ttl})
}

// navigate builds an open in app link, asking whether the person may open it
// by sending the record's own read through the grant (an answer is permitted,
// and a refusal and an absence are both "not permitted", never told apart), or
// by their grants for a screen that names no record.
func (s *surface) navigate(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	if err := tool.Check(args); err != nil {
		return s.refuseBefore(ctx, tool, args, "Not opened", err), nil
	}
	link, probe, err := catalog.Open(args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not opened", err), nil
	}
	var request *wire.StepRequest
	if probe != nil {
		request = &wire.StepRequest{Method: probe.Method, Route: link.Screen}
		if shown, ok := tool.StepRequest(*probe); ok {
			request = shown
		}
	}
	step := s.t.stepStart(tool, args, request)
	permitted, status, size := cards.MayOpen(link.Screen, s.t.person), 0, 0
	if probe != nil {
		resp, err := s.t.svc.dispatcher.Do(ctx, s.t.read, *probe)
		switch {
		case err != nil && ctx.Err() != nil:
			s.t.stepEnd(ctx, step, wire.StepStopped, "Stopped", nil, 0, 0)
			return toolloop.ToolResult{}, context.Cause(ctx)
		case err != nil:
			permitted = false
		case resp.Status == http.StatusUnauthorized:
			s.t.stepEnd(ctx, step, wire.StepFailed, "Signed out", nil, resp.Status, 0)
			return toolloop.ToolResult{}, errGrantEnded
		default:
			permitted, status, size = cards.Permitted(resp.Status), resp.Status, len(resp.Body)
		}
	}
	summary := "Ready to open"
	if !permitted {
		summary = "Not permitted"
	}
	s.t.stepEnd(ctx, step, wire.StepDone, summary, nil, status, size)
	s.t.emitCard(ctx, cards.OpenInApp(link, permitted), nil)
	if !permitted {
		return toolloop.ToolResult{Output: "The person may not open that, or it does not " +
			"exist. Say you cannot open it for them."}, nil
	}
	return toolloop.ToolResult{Output: "A link to " + link.Label + " is shown to the person."}, nil
}

// failed is a call that failed, as the model reads it.
func failed(output string) toolloop.ToolResult {
	return toolloop.ToolResult{Output: output, Failed: true}
}

// appNames is a tool's apps as a card is recorded with them.
func appNames(apps []iam.Grant) []string {
	out := make([]string, 0, len(apps))
	for _, app := range apps {
		out = append(out, string(app))
	}
	return out
}

// outsideTrusts are the trusts a read's answer is written by somebody the
// company cannot vouch for under: a proposal made after one names its source
// (D25).
var outsideTrusts = map[provenance.Trust]bool{
	provenance.TrustExternal: true, provenance.TrustPage: true,
}

// refusalSummary is a refused step's outcome in a few words.
func refusalSummary(status int) string {
	switch status {
	case http.StatusForbidden:
		return "Not permitted"
	case http.StatusNotFound:
		return "Not found"
	case http.StatusTooManyRequests:
		return "Too many requests"
	}
	if status >= http.StatusInternalServerError {
		return "Unavailable"
	}
	return "Refused"
}

// answerSummary is a read step's outcome in a few words, from its cards.
func answerSummary(shown []wire.Card) string {
	for _, c := range shown {
		switch c.Type {
		case wire.CardTable:
			if c.Table.RowCount == 1 {
				return "1 row"
			}
			return fmt.Sprintf("%d rows", c.Table.RowCount)
		case wire.CardEntity:
			if n := len(c.Entity.Items); n > 0 {
				return fmt.Sprintf("%d items", n)
			}
			return "1 record"
		case wire.CardNotice:
			return "See the notice"
		}
	}
	return "Done"
}

// stepClock is when a step started, for its duration.
type stepClock struct {
	wire.StepEvent
	started time.Time
}

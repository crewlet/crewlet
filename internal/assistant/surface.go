package assistant

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE ASSISTANT'S OWN SURFACE: the tools one turn offers its model, and what a
// call to one does. Never the agent runtime's tools.Surface, which acts as a
// seat: every call here is the person's own request through the API's one
// handler chain, decided by the route it asks (ADR-0050).
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
//     the call unanswered until the person decides (ADR-0051).
//   - A DOCS call answers from the engine's own API reference and the route
//     table, and sends nothing: the routes the person may use, or one route's
//     documentation, the model's text with every fence marker defused.
//   - A NAVIGATION builds an open in app link, whose `permitted` is asked by
//     sending the record's own read through the grant, or by the person's
//     grants for a screen that names no record.

// errGrantEnded ends a turn whose read grant the guard no longer accepts: a
// 401 answered to the turn's own grant is the session ending, a sign-out or a
// revocation, never a refusal of the one request.
var errGrantEnded = errors.New("assistant: the turn's grant was refused")

// surface is one turn's tool surface.
type surface struct {
	t      *turn
	byName map[string]catalog.Tool
	defs   []llm.ToolDef
	// names resolves the arguments a tool declares as naming a seat, a
	// unit, a project or a space ([catalog.Tool.Names]), through the turn's
	// own grant, before a read is sent or a write is previewed, and holds
	// the call to its schema in the same pass, every fault of both in one
	// refusal ([catalog.Resolver.Prepare]).
	names *catalog.Resolver

	mu sync.Mutex
	// reads are the answers this segment's reads were given, by the call
	// ([readKey]): THE SAME READ IS NEVER SENT OR DRAWN TWICE IN ONE
	// SEGMENT. A model that asks again is handed what it was told, and the
	// person is not shown the same card a second time. A segment is one run
	// of the loop, so a turn resumed after an approval reads afresh.
	reads map[string]string
}

var (
	_ toolloop.Surface = (*surface)(nil)
	_ toolloop.Refuser = (*surface)(nil)
)

// newSurface offers tools in the catalogue's order.
func newSurface(t *turn, tools []catalog.Tool) *surface {
	s := &surface{t: t, byName: map[string]catalog.Tool{}, reads: map[string]string{},
		names: catalog.NewResolver(func(ctx context.Context, r exec.Request) (exec.Response, error) {
			return t.svc.dispatcher.Do(ctx, t.read, r)
		})}
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

// Revision is constant: a turn's tools never change while it runs, so its CLI
// never has a tool list to re-read.
func (s *surface) Revision() uint64 { return 1 }

// AnnotationsOf is a tool's hints as its kind states them, which a CLI reads
// to decide what may run beside what: a read, the docs and a navigation only
// read, and a change is proposed, so it destroys nothing until the person
// approves it.
func (s *surface) AnnotationsOf(name string) mcp.Annotations {
	tool, ok := s.byName[name]
	if !ok {
		return mcp.Annotations{}
	}
	switch tool.Kind {
	case catalog.KindRead, catalog.KindDocs, catalog.KindNavigate, catalog.KindBrief:
		return mcp.Annotations{ReadOnly: mcp.Yes, Destructive: mcp.No}
	}
	return mcp.Annotations{ReadOnly: mcp.No, Destructive: mcp.No}
}

// Detached reports a tool whose call may park the turn: a change waiting on
// the person's approval, or deep work, which can park on one of its own. Such
// a call runs alone ([toolloop.Surface.Detached]), since a parked turn waits
// on one decision.
func (s *surface) Detached(name string) bool {
	switch s.byName[name].Kind {
	case catalog.KindWrite, catalog.KindPageAction, catalog.KindDeep:
		return true
	}
	return false
}

// Admit binds one call to run once. The turn keeps its own record of its
// steps in the order they run, so nothing is fixed here.
func (s *surface) Admit(string, map[string]any) toolloop.Admitted { return s.Execute }

// Execute carries out one call.
func (s *surface) Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	tool, offered := s.byName[call.Name]
	if !offered {
		return failed(mcp.RefusalNotFound, fmt.Sprintf("There is no tool named %q here. Use only the tools "+
			"you are offered.", call.Name)), nil
	}
	switch tool.Kind {
	case catalog.KindRead:
		if s.t.briefing() {
			return s.briefRead(ctx, tool, call.Arguments)
		}
		return s.read(ctx, tool, call.Arguments)
	case catalog.KindWrite, catalog.KindPageAction:
		return s.propose(ctx, tool, call.Arguments)
	case catalog.KindNavigate:
		return s.navigate(ctx, tool, call.Arguments)
	case catalog.KindDocs:
		return s.docs(ctx, tool, call.Arguments)
	case catalog.KindDeep:
		return s.deep(ctx, tool, call.Arguments)
	case catalog.KindBrief:
		return s.answerBrief(ctx, tool, call.Arguments)
	}
	return failed(mcp.RefusalInternalError, "This tool cannot be used here."), nil
}

// refuseBefore records a call refused before anything was sent, and answers
// the model why.
func (s *surface) refuseBefore(ctx context.Context, tool catalog.Tool, args map[string]any, summary string,
	err error) toolloop.ToolResult {

	message := s.recordRefusal(ctx, tool, args, summary, err.Error())
	return failed(mcp.Classify(mcp.RefusalInvalid, err), "Refused before anything was sent: "+message)
}

// Refused records a call the tool loop answered without running it (a reply
// the model wrote in a form that could not be read, arguments that did not
// parse, a call after the one that stopped its round), so the person sees
// the step the model asked for and that it did not run, never a turn that
// asked for nothing. A call naming no tool this turn offers has no step to
// draw; the loop has already answered or re-asked it.
func (s *surface) Refused(ctx context.Context, call llm.ToolCall, reason string) {
	if tool, offered := s.byName[call.Name]; offered {
		s.recordRefusal(ctx, tool, call.Arguments, "Not run", reason)
	}
}

// recordRefusal records one step refused as written, before anything was
// sent, and returns the reason as the step shows it.
func (s *surface) recordRefusal(ctx context.Context, tool catalog.Tool, args map[string]any,
	summary, reason string) string {

	message := redact.Secrets(reason)
	step := s.t.stepStart(tool, args, s.cardOptions(ctx), nil, "")
	s.t.stepEnd(ctx, step, wire.StepRefused, summary, &wire.Refusal{Code: cards.CodeInvalidCall,
		Message: message}, 0, 0)
	return message
}

// read sends one read tool's request and shapes its answer.
func (s *surface) read(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	prepared, err := s.names.Prepare(ctx, tool, args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not sent", err), nil
	}
	args = prepared
	req, err := tool.Request(args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not sent", err), nil
	}
	key := readKey(tool, req)
	if earlier, done := s.answered(key); done {
		return toolloop.ToolResult{Output: alreadyRead + earlier}, nil
	}
	route, _ := tool.RouteOf(req)
	shown, _ := tool.StepRequest(req)
	step := s.t.stepStart(tool, args, s.cardOptions(ctx), shown, patternOf(route))
	resp, err := s.t.svc.dispatcher.Do(ctx, s.t.read, req)
	if err != nil {
		return s.unsent(ctx, step, tool, route, err)
	}
	if resp.Status == http.StatusUnauthorized {
		s.t.stepEnd(ctx, step, wire.StepFailed, "Signed out", nil, resp.Status, len(resp.Body))
		return toolloop.ToolResult{}, errGrantEnded
	}
	shaped, err := catalog.Shape(tool, args, resp, s.t.boundary, s.t.svc.now(), s.cardOptions(ctx))
	if err != nil {
		s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be read", nil, resp.Status, len(resp.Body))
		s.t.svc.log.ErrorContext(ctx, "assistant_step_unreadable", "turn", s.t.id,
			"tool", tool.Name, "route", route, "error", redact.Secrets(err.Error()))
		return failed(mcp.Classify(mcp.RefusalInternalError, err),
			"The answer could not be read. Tell the person it failed."), nil
	}
	if shaped.Refusal != nil {
		state := wire.StepRefused
		if resp.Status >= http.StatusInternalServerError {
			state = wire.StepFailed
		}
		refusal := *shaped.Refusal
		s.t.stepEnd(ctx, step, state, refusalSummary(resp.Status), &refusal, resp.Status, len(resp.Body))
	} else {
		summary := answerSummary(shaped.Cards)
		if shaped.Empty {
			summary = noMatches
		}
		s.t.stepEnd(ctx, step, wire.StepDone, summary, nil, resp.Status, len(resp.Body))
		s.t.tainted(tool, shaped.Trust)
		s.t.used(appNames(tool.Apps))
	}
	briefing := s.t.briefing()
	if !briefing {
		// A BRIEF'S READS DRAW NO CARD: the run chip shows each read, and
		// the brief card is the whole answer.
		for _, card := range shaped.Cards {
			s.t.emitCard(ctx, card, appNames(tool.Apps))
		}
	}
	if shaped.Refusal != nil {
		return failed(routeClass(resp.Status, shaped.Refusal.Code), shaped.Model), nil
	}
	if briefing {
		s.t.readForBrief(shaped.Model)
	}
	s.remember(key, shaped.Model)
	return toolloop.ToolResult{Output: shaped.Model}, nil
}

// alreadyRead leads the answer to a read the segment has made already.
const alreadyRead = "You already made this exact read in this turn, and its card is " +
	"shown to the person above, so it was not sent or drawn again. What it answered: "

// readKey is a read's identity within a segment: its tool and the request it
// sends, which the tool built from the resolved arguments, so two spellings
// of one call (a seat's name and its handle) are one read.
func readKey(tool catalog.Tool, req exec.Request) string {
	return tool.Name + " " + req.Method + " " + req.Path + "?" + req.Query.Encode()
}

// answered is what an earlier read with key answered, and whether there was
// one.
func (s *surface) answered(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	model, ok := s.reads[key]
	return model, ok
}

// remember keeps a read's answer for the rest of the segment. Only an
// answer: a refusal or a failure may be asked again.
func (s *surface) remember(key, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads[key] = model
}

// cardOptions are what a read's card and a step's label are told beyond the
// answer: each seat's name, from the seat directory the turn reads through its
// own grant, read only when a card has a seat to name; each chat room as the
// turn says it, read only when a call names one by its id; and the company's
// clock, which every instant the model reads is said on.
func (s *surface) cardOptions(ctx context.Context) cards.Options {
	o := cards.Options{SeatName: func(handle string) (string, bool) {
		return s.names.SeatName(ctx, handle)
	}, Room: func(ref string) (cards.Room, bool) {
		return s.names.Room(ctx, ref)
	}}
	if c, _ := s.t.svc.company(); c != nil {
		o.Zone = c.Location()
	}
	return o
}

// docs answers a [catalog.KindDocs] call: nothing is sent, and the step says
// what was looked up.
func (s *surface) docs(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	if err := tool.Check(args); err != nil {
		return s.refuseBefore(ctx, tool, args, "Not looked up", err), nil
	}
	answer, err := tool.Docs(args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not found", err), nil
	}
	step := s.t.stepStart(tool, args, s.cardOptions(ctx), nil, "")
	s.t.stepEnd(ctx, step, wire.StepDone, "Looked up", nil, 0, len(answer))
	return toolloop.ToolResult{Output: provenance.Neutralise(s.t.boundary, answer)}, nil
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
	return failed(mcp.Classify(mcp.RefusalInternalError, err), "The request could not be "+
		"sent. Tell the person it failed; do not retry it."), nil
}

// propose turns a write or a page action into a proposal the person approves,
// and suspends the loop on it.
func (s *surface) propose(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	scope := wire.ScopeServer
	pageRevision := ""
	immediate := false
	if tool.Kind == catalog.KindPageAction {
		scope = wire.ScopePage
		action, err := catalog.PageActionFor(args, s.t.page)
		if err == nil {
			pageRevision = s.t.page.RevisionFor(action.ID)
		}
		if err == nil && action.ID == catalog.DraftSet {
			err = s.t.draftChange(ctx, args)
		}
		if err != nil {
			return s.refuseBefore(ctx, tool, args, "Not proposed", err), nil
		}
		immediate = action.Confirm == wire.ConfirmNone
	}
	prepared, err := s.names.Prepare(ctx, tool, args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not proposed", err), nil
	}
	args = prepared
	step := s.t.stepStart(tool, args, s.cardOptions(ctx), nil, "")
	// A PREVIEW'S READ is decoded and diffed, never handed to the model, so
	// it reads to the preview's bound rather than a step's.
	read := func(ctx context.Context, r exec.Request) (exec.Response, error) {
		r.ReadLimit = exec.PreviewMaxResponseBytes
		return s.t.svc.dispatcher.Do(ctx, s.t.read, r)
	}
	preview, err := tool.Preview(ctx, read, s.t.svc.now(), args)
	if err == nil && tool.Kind == catalog.KindPageAction && s.t.page != nil &&
		catalog.Args(args).String("action") == catalog.DraftSet {
		preview.Changes = draftRows(preview.Changes, s.t.page.Draft)
	}
	var drafted assistantdomain.NewProposal
	if err == nil {
		drafted, err = s.t.draft(tool, scope, preview, pageRevision)
	}
	if err == nil {
		err = s.t.svc.records.CreateProposal(ctx,
			s.t.write().Step("proposal", strconv.Itoa(step.Index)), drafted)
	}
	if err == nil && immediate {
		return s.applyAtOnce(ctx, step, drafted)
	}
	if err == nil {
		s.t.stepEnd(ctx, step, wire.StepWaiting, "Waiting for approval", nil, 0, 0)
		s.t.emitCard(ctx, drafted.Card, appNames(tool.Apps))
		return toolloop.ToolResult{Output: "Proposed; the person decides whether to " +
			"approve it. Nothing has changed yet.", Suspend: true}, nil
	}
	return s.unprepared(ctx, step, tool, err)
}

// unprepared ends a write's step whose change could not be prepared: NO CARD,
// because nothing was proposed, and the reason on the step and in the model's
// result, so the model can read why and prepare a change that can apply. A
// card drawn for it would read as a proposal the engine rejected, when the
// change never reached the person at all.
//
// The route's own refusal of the preview's read ([catalog.PreviewRefusal]: a
// 403, a 404 under a PATCH or a DELETE) is the step's refusal in the route's
// words; a call refused as written is the step's refusal as
// `invalid_input`; a read that could not be had (a body over a step's read,
// no JSON) is a failed step naming why. Only a failure of the engine's own
// (the proposal's record) is logged and told to the person as a failure.
func (s *surface) unprepared(ctx context.Context, step stepClock, tool catalog.Tool,
	err error) (toolloop.ToolResult, error) {

	var refused *catalog.PreviewRefusal
	switch {
	case ctx.Err() != nil:
		s.t.stepEnd(ctx, step, wire.StepStopped, "Stopped", nil, 0, 0)
		return toolloop.ToolResult{}, context.Cause(ctx)
	case errors.As(err, &refused):
		refusal := refused.Refusal
		refusal.Message = redact.Secrets(refusal.Message)
		refusal.Hint = redact.Secrets(refusal.Hint)
		refusal.Detail = redact.Secrets(refusal.Detail)
		s.t.stepEnd(ctx, step, wire.StepRefused, "Not proposed", &refusal, refused.Status, 0)
		return failed(routeClass(refused.Status, refusal.Code),
			"The change could not be prepared, so nothing was proposed: "+
				refusalSentence(refusal)), nil
	case errors.Is(err, proposal.ErrInvalid), errors.Is(err, proposal.ErrTooLarge),
		errors.Is(err, catalog.ErrInvalidArguments):
		message := redact.Secrets(err.Error())
		s.t.stepEnd(ctx, step, wire.StepRefused, "Not proposed", &wire.Refusal{
			Code: cards.CodeInvalidCall, Message: message}, 0, 0)
		return failed(mcp.Classify(mcp.RefusalInvalid, err),
			"The change could not be prepared: "+message), nil
	case errors.Is(err, catalog.ErrPreviewRead):
		message := redact.Secrets(err.Error())
		s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be prepared", &wire.Refusal{
			Code: cards.CodePreviewUnreadable, Message: message}, 0, 0)
		return failed(mcp.Classify(mcp.RefusalUnavailable, err),
			"The change could not be prepared, so nothing was proposed: "+message), nil
	}
	s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be prepared", nil, 0, 0)
	s.t.svc.log.ErrorContext(ctx, "assistant_proposal_failed", "turn", s.t.id,
		"tool", tool.Name, "error", redact.Secrets(err.Error()))
	return failed(mcp.Classify(mcp.RefusalInternalError, err),
		"The change could not be prepared. Tell the person it failed."), nil
}

// refusalSentence is a route's refusal as the model reads it: its words, its
// code and the hint that says what to do instead.
func refusalSentence(r wire.Refusal) string {
	out := r.ForModel() + " (" + r.Code + ")."
	if r.Hint != "" {
		out += " " + r.Hint
	}
	return out
}

// applyAtOnce hands a `confirm: none` page action (a pure view change, D11)
// straight to the page: the turn approves the proposal it just recorded, sends
// the action frame, and carries on without parking. The page claims and
// applies it and shows its Undo receipt; its report settles the proposal and
// resumes nothing (approval's immediate). No proposal card is shown: the
// receipt is the page's.
func (s *surface) applyAtOnce(ctx context.Context, step stepClock,
	drafted assistantdomain.NewProposal) (toolloop.ToolResult, error) {

	if err := s.t.svc.records.ApproveProposal(ctx,
		s.t.write().Step("proposal", strconv.Itoa(step.Index), "approve"),
		drafted.Proposal); err != nil {
		s.t.stepEnd(ctx, step, wire.StepFailed, "Could not be applied", nil, 0, 0)
		s.t.svc.log.ErrorContext(ctx, "assistant_page_action_unapproved", "turn", s.t.id,
			"proposal", drafted.Proposal, "error", redact.Secrets(err.Error()))
		return failed(mcp.Classify(mcp.RefusalInternalError, err), "The change to the "+
			"screen could not be applied. Tell the person it failed."), nil
	}
	action := proposal.PageAction(drafted.Proposal, drafted.Path, drafted.SourceRevision,
		drafted.Body, wire.ConfirmNone)
	s.t.emit(wire.Frame{Kind: wire.EventAction, Action: &action})
	s.t.stepEnd(ctx, step, wire.StepDone, "Applied on your screen", nil, 0, 0)
	return toolloop.ToolResult{Output: "Applied on the person's screen at once; they can " +
		"undo it there. Nothing was saved."}, nil
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
	// A WRITE ON THE ROUTE ITS CALL NAMES is decided by that route's verb,
	// which its preview names; every other write executes as its tool's own.
	act := tool.Act
	if tool.Routed {
		act = preview.Act
	}
	return proposal.New(proposal.Draft{Owner: t.person.ID, Conversation: t.conversation,
		Turn: t.id, Tool: tool.Name, Act: act, Scope: scope, Preview: preview,
		ChangesVisibility: tool.ChangesVisibility, PageRevision: pageRevision,
		Taint: t.taint(), Now: t.svc.now(), TTL: ttl})
}

// navigate builds an open in app link, asking whether the person may open it
// by sending the record's own read through the grant (an answer is permitted,
// and a refusal and an absence are both "not permitted", never told apart), or
// by their grants for a screen that names no record.
func (s *surface) navigate(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	prepared, err := s.names.Prepare(ctx, tool, args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not opened", err), nil
	}
	args = prepared
	link, probe, err := catalog.Open(args)
	if err != nil {
		return s.refuseBefore(ctx, tool, args, "Not opened", err), nil
	}
	var request *wire.StepRequest
	pattern := ""
	if probe != nil {
		request, pattern = &wire.StepRequest{Method: probe.Method, Route: link.Screen}, link.Screen
		if shown, ok := tool.StepRequest(*probe); ok {
			route, _ := tool.RouteOf(*probe)
			request, pattern = shown, patternOf(route)
		}
	}
	step := s.t.stepStart(tool, args, s.cardOptions(ctx), request, pattern)
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
	// A LINK IS NOT AN ANSWER: the model is told so where it reads that the
	// link was shown, since "I've added a link above" was once a whole reply
	// to a question the turn had read the facts for.
	return toolloop.ToolResult{Output: "A link to " + link.Label + " is shown to the " +
		"person. It does not answer their question: still state in your reply the facts " +
		"they asked for."}, nil
}

// failed is a call that failed, as the model reads it, carrying its class in
// cause (an [mcp.Refusal], or one beneath the error that decided it through
// [mcp.Classify]). The model reads only the sentence; the class is for a
// caller that dispatches through this surface with no model behind it, the
// same contract every first-party tool keeps ([toolloop.ToolResult.Cause]).
func failed(cause error, output string) toolloop.ToolResult {
	return toolloop.ToolResult{Output: output, Failed: true, Cause: cause}
}

// routeClass is a route's refusal as a refusal class: the route's own code
// where the API spells it as a class (the operator surface answers each class
// with the code of the same name), else the class its status means. A status
// that is no refusal (a read whose body was not the envelope) is the node's
// own failure.
func routeClass(status int, code string) mcp.Refusal {
	if class := mcp.Refusal(code); class.Valid() {
		return class
	}
	switch {
	case status == http.StatusNotFound:
		return mcp.RefusalNotFound
	case status == http.StatusForbidden:
		return mcp.RefusalForbidden
	case status == http.StatusConflict:
		return mcp.RefusalConflict
	case status == http.StatusServiceUnavailable:
		return mcp.RefusalUnavailable
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return mcp.RefusalInvalid
	}
	return mcp.RefusalInternalError
}

// appNames is a tool's apps as a card is recorded with them.
func appNames(apps []iam.Grant) []string {
	out := make([]string, 0, len(apps))
	for _, app := range apps {
		out = append(out, string(app))
	}
	return out
}

// outsideTrust reports whether a read's answer was fenced at a trust that says
// somebody the company cannot vouch for wrote it: a proposal made after one
// names its source (D25). A comparison and never a decision: the answer's
// trust was decided where it was fenced.
func outsideTrust(trust provenance.Trust) bool {
	return trust == provenance.TrustExternal || trust == provenance.TrustPage
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
		case wire.CardOversize:
			return "Too large to show"
		}
	}
	return "Done"
}

// noMatches is the step's summary of a read whose answer held nothing, which
// draws no card (catalog.Shape): the run chip is where the person sees it.
const noMatches = "No matches"

// stepClock is when a step started, for its duration.
type stepClock struct {
	wire.StepEvent
	started time.Time
	// pattern is the route pattern the step's request matched, what the
	// audit record keeps in place of the path it was sent to.
	pattern string
}

// patternOf is a catalogue route's path pattern, without the method it opens
// with ("GET /work/{id}" is "/work/{id}").
func patternOf(route string) string {
	_, path, _ := strings.Cut(route, " ")
	return path
}

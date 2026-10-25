package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE ASSISTANT'S OWN SURFACE: the tools one turn offers its model, and what a
// call to one does. Never the agent runtime's tools.Surface, which acts as a
// seat: every call here is the person's own request through the API's one
// handler chain, decided by the route it asks (ADR-0046).
//
// # What a call can do
//
//   - A READ is built into its one request, held to a route its descriptor
//     declares, sent through the turn's read grant, and carded by the engine;
//     the model reads a summary, redacted, fenced where somebody else wrote it,
//     and cut to [ModelResultBytes].
//   - A WRITE or PAGE ACTION is never sent: it becomes a proposal the person
//     approves (internal/assistant/proposal), and the loop suspends with the
//     call unanswered. A turn with no approval path offers none.
//   - A NAVIGATION builds an open in app link, whose `permitted` is asked by
//     sending the record's own read through the grant.
//
// # Identity is the grant and nothing else
//
// A call naming the company, a principal, an actor, a login, an operation id
// or an operator is refused before anything is sent, whatever its tool's
// schema says, so an injected attempt to act as somebody else shows in the
// step list rather than being ignored downstream. An argument the tool's
// schema does not declare is refused the same way: the catalogue's builders
// read their schema's properties and nothing else.

// identityArguments are the argument names no call may carry.
var identityArguments = []string{
	"company", "company_id", "principal", "actor", "as", "login", "op_id",
	"operator", "operator_id", "via", "on_behalf_of",
}

// errGrantEnded ends a turn whose read grant the guard no longer accepts: a
// 401 answered to the turn's own grant is the session ending, a sign-out or a
// revocation, never a refusal of the one request.
var errGrantEnded = errors.New("assistant: the turn's grant was refused")

// surface is one turn's tool surface.
type surface struct {
	t      *turn
	byName map[string]catalog.Tool
	defs   []llm.ToolDef
}

var _ toolloop.Surface = (*surface)(nil)

// newSurface offers tools, in the catalogue's order, dropping every write and
// page action when the turn has no approval path.
func newSurface(t *turn, tools []catalog.Tool) *surface {
	s := &surface{t: t, byName: map[string]catalog.Tool{}}
	for _, tool := range tools {
		switch tool.Kind {
		case catalog.KindWrite, catalog.KindPageAction:
			if t.svc.proposer == nil {
				continue
			}
		case catalog.KindNavigate:
			if t.svc.cards == nil {
				continue
			}
		}
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
	if bad := undeclared(tool, call.Arguments); bad != "" {
		step := s.t.stepStart(tool, call.Arguments, nil)
		s.t.stepEnd(step, wire.StepRefused, "Not sent", &wire.Refusal{Code: codeInvalidInput,
			Message: "The call named an argument this tool does not take: " + bad + "."}, 0, 0)
		return failed("Refused before sending: the argument " + bad + " is not one this " +
			"tool takes. Identity is the person's own and is never an argument."), nil
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

// codeInvalidInput is the API's own code for a call refused as written.
const codeInvalidInput = "invalid_input"

// undeclared is the first argument call names that its tool may not take, or
// "": an identity argument, whatever the schema says, or one the schema does
// not declare.
func undeclared(tool catalog.Tool, args map[string]any) string {
	declared := map[string]bool{}
	if props, ok := tool.Parameters["properties"].(map[string]any); ok {
		for name := range props {
			declared[name] = true
		}
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if slices.Contains(identityArguments, strings.ToLower(name)) || !declared[name] {
			return name
		}
	}
	return ""
}

// read sends one read tool's request and cards its answer.
func (s *surface) read(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	req, route, err := s.build(tool, args)
	if err != nil {
		step := s.t.stepStart(tool, args, nil)
		s.t.stepEnd(step, wire.StepRefused, "Not sent", &wire.Refusal{Code: codeInvalidInput,
			Message: redact.Secrets(err.Error())}, 0, 0)
		return failed("Refused before sending: " + redact.Secrets(err.Error())), nil
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return toolloop.ToolResult{}, fmt.Errorf("assistant: read tool %s built a %s "+
			"request; a read sends GET or HEAD only", tool.Name, req.Method)
	}
	step := s.t.stepStart(tool, args, &wire.StepRequest{Method: req.Method, Route: route})
	resp, err := s.t.svc.dispatcher.Do(ctx, s.t.read, req)
	if err != nil {
		if ctx.Err() != nil {
			s.t.stepEnd(step, wire.StepStopped, "Stopped", nil, 0, 0)
			return toolloop.ToolResult{}, context.Cause(ctx)
		}
		s.t.stepEnd(step, wire.StepFailed, "Could not be sent", nil, 0, 0)
		s.t.svc.log.ErrorContext(ctx, "assistant_step_not_sent", "turn", s.t.id,
			"tool", tool.Name, "route", route, "error", redact.Secrets(err.Error()))
		return failed("The request could not be sent. Tell the person it failed; do " +
			"not retry it."), nil
	}
	if resp.Status == http.StatusUnauthorized {
		s.t.stepEnd(step, wire.StepFailed, "Signed out", nil, resp.Status, len(resp.Body))
		return toolloop.ToolResult{}, errGrantEnded
	}
	if resp.Status >= http.StatusBadRequest {
		refusal := refusalOf(resp)
		state := wire.StepRefused
		if resp.Status >= http.StatusInternalServerError {
			state = wire.StepFailed
		}
		s.t.stepEnd(step, state, refusalSummary(resp.Status), &refusal, resp.Status, len(resp.Body))
		if resp.Status == http.StatusForbidden && tool.Phrase != "" {
			s.t.emitCard(ctx, wire.Card{Type: wire.CardError, Error: &wire.ErrorCard{
				Refusal: refusal, Phrase: tool.Phrase}}, nil)
		}
		return failed(fmt.Sprintf("The engine answered %d as the person: %s (%s). Report "+
			"this plainly; do not try another tool to get around it.", resp.Status,
			refusal.Message, refusal.Code)), nil
	}
	if resp.Truncated {
		s.t.stepEnd(step, wire.StepDone, "Too large to show whole", nil, resp.Status, len(resp.Body))
		s.t.emitCard(ctx, wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{
			Tone: wire.NoticeWarning, Text: "That answer was too large to show whole. " +
				"Narrow the question to see all of it."}}, nil)
		return toolloop.ToolResult{Output: s.forModel(tool, string(resp.Body)) +
			"\n(This answer was cut because it was too large. Say so, and suggest a " +
			"narrower question.)"}, nil
	}
	resp.Body = redactedBody(resp.Body)
	carded, err := s.t.svc.cards.Read(tool, args, resp, s.t.svc.now())
	if err != nil {
		s.t.stepEnd(step, wire.StepFailed, "Could not be read", nil, resp.Status, len(resp.Body))
		s.t.svc.log.ErrorContext(ctx, "assistant_step_unreadable", "turn", s.t.id,
			"tool", tool.Name, "route", route, "error", redact.Secrets(err.Error()))
		return failed("The answer could not be read. Tell the person it failed."), nil
	}
	s.t.stepEnd(step, wire.StepDone, answerSummary(carded.Cards), nil, resp.Status, len(resp.Body))
	apps := make([]string, 0, len(tool.Apps))
	for _, app := range tool.Apps {
		apps = append(apps, string(app))
	}
	for _, card := range carded.Cards {
		s.t.emitCard(ctx, card, apps)
	}
	return toolloop.ToolResult{Output: s.forModel(tool, carded.Summary)}, nil
}

// forModel is text a tool's answer gives the model: redacted, fenced under
// the turn's boundary where the tool says somebody else wrote it, and cut to
// [ModelResultBytes] with the cut said in words, inside the fence.
func (s *surface) forModel(tool catalog.Tool, text string) string {
	text = redact.Secrets(text)
	const note = "\n(cut here: the rest did not fit)"
	if len(text) > ModelResultBytes {
		text = textcut.Bytes(text, ModelResultBytes-len(note)) + note
	}
	if !tool.Fence {
		return text
	}
	return provenance.Fence(s.t.boundary, provenance.SourceAPIAnswer, provenance.TrustColleague,
		nil, text)
}

// build is the one request a call sends, and the route pattern it is on. A
// request on no route the tool declares is refused: a builder that strays
// from its declaration is a catalogue fault, and the walks hold the
// declaration, not the builder.
func (s *surface) build(tool catalog.Tool, args map[string]any) (exec.Request, string, error) {
	if tool.Build == nil {
		return exec.Request{}, "", fmt.Errorf("the tool %s builds no request", tool.Name)
	}
	req, err := tool.Build(args)
	if err != nil {
		return exec.Request{}, "", err
	}
	route, ok := matchRoute(tool.Routes, req.Method, req.Path)
	if !ok {
		return exec.Request{}, "", fmt.Errorf("the tool %s built %s %s, which is no route it "+
			"declares", tool.Name, req.Method, req.Path)
	}
	return req, route, nil
}

// propose turns a write or a page action into a proposal the person approves,
// and suspends the loop on it.
func (s *surface) propose(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	step := s.t.stepStart(tool, args, nil)
	card, err := s.t.svc.proposer.Propose(ctx, ProposalCall{
		Person: s.t.person, Conversation: s.t.conversation, Turn: s.t.id,
		Write: s.t.write(), Index: step.Index, Tool: tool, Args: args, Read: s.t.read,
		Page: s.t.page,
	})
	if err != nil {
		if ctx.Err() != nil {
			s.t.stepEnd(step, wire.StepStopped, "Stopped", nil, 0, 0)
			return toolloop.ToolResult{}, context.Cause(ctx)
		}
		if errors.Is(err, ErrInvalid) {
			s.t.stepEnd(step, wire.StepRefused, "Not proposed", &wire.Refusal{
				Code: codeInvalidInput, Message: redact.Secrets(err.Error())}, 0, 0)
			return failed("The change could not be prepared: " + redact.Secrets(err.Error())), nil
		}
		s.t.stepEnd(step, wire.StepFailed, "Could not be prepared", nil, 0, 0)
		s.t.svc.log.ErrorContext(ctx, "assistant_proposal_failed", "turn", s.t.id,
			"tool", tool.Name, "error", redact.Secrets(err.Error()))
		return failed("The change could not be prepared. Tell the person it failed."), nil
	}
	s.t.stepEnd(step, wire.StepWaiting, "Waiting for approval", nil, 0, 0)
	s.t.emitCard(ctx, card, nil)
	return toolloop.ToolResult{Output: "Proposed; waiting for the person's approval.",
		Suspend: true}, nil
}

// navigate builds an open in app link, asking whether the person may open it
// by sending the record's own read through the grant: an answer is permitted,
// and a refusal and an absence are both "not permitted", never told apart.
func (s *surface) navigate(ctx context.Context, tool catalog.Tool, args map[string]any) (toolloop.ToolResult, error) {
	link, probe, err := s.t.svc.cards.Navigate(tool, args)
	if err != nil {
		step := s.t.stepStart(tool, args, nil)
		s.t.stepEnd(step, wire.StepRefused, "Not opened", &wire.Refusal{Code: codeInvalidInput,
			Message: redact.Secrets(err.Error())}, 0, 0)
		return failed("The link could not be built: " + redact.Secrets(err.Error())), nil
	}
	var request *wire.StepRequest
	if probe != nil {
		route, ok := matchRoute(tool.Routes, probe.Method, probe.Path)
		if !ok || (probe.Method != http.MethodGet && probe.Method != http.MethodHead) {
			return toolloop.ToolResult{}, fmt.Errorf("assistant: navigation tool %s probes "+
				"%s %s, which is no read route it declares", tool.Name, probe.Method, probe.Path)
		}
		request = &wire.StepRequest{Method: probe.Method, Route: route}
	}
	step := s.t.stepStart(tool, args, request)
	permitted, status, size := true, 0, 0
	if probe != nil {
		resp, err := s.t.svc.dispatcher.Do(ctx, s.t.read, *probe)
		switch {
		case err != nil && ctx.Err() != nil:
			s.t.stepEnd(step, wire.StepStopped, "Stopped", nil, 0, 0)
			return toolloop.ToolResult{}, context.Cause(ctx)
		case err != nil:
			permitted = false
		case resp.Status == http.StatusUnauthorized:
			s.t.stepEnd(step, wire.StepFailed, "Signed out", nil, resp.Status, 0)
			return toolloop.ToolResult{}, errGrantEnded
		default:
			permitted, status, size = resp.Status < http.StatusBadRequest, resp.Status, len(resp.Body)
		}
	}
	card := wire.OpenInAppCard{Link: link, Permitted: permitted}
	summary := "Ready to open"
	if !permitted {
		card.Reason, summary = wire.OpenNotPermitted, "Not permitted"
	}
	s.t.stepEnd(step, wire.StepDone, summary, nil, status, size)
	s.t.emitCard(ctx, wire.Card{Type: wire.CardOpenInApp, OpenInApp: &card}, nil)
	if !permitted {
		return toolloop.ToolResult{Output: "The person may not open that, or it does not " +
			"exist. Say you cannot open it for them."}, nil
	}
	return toolloop.ToolResult{Output: "A link to " + link.Label + " is shown to the person."}, nil
}

// redactedBody is an answer with every credential-shaped string in it
// redacted before anything is built from it: every string value of a JSON
// answer, so the JSON stays JSON, and the whole of any other.
func redactedBody(body []byte) []byte {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return []byte(redact.Secrets(string(body)))
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return []byte(redact.Secrets(string(body)))
	}
	return out
}

// redactValue is v with every string in it redacted, keys included.
func redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return redact.Secrets(x)
	case []any:
		for i := range x {
			x[i] = redactValue(x[i])
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[redact.Secrets(k)] = redactValue(val)
		}
		return out
	}
	return v
}

// failed is a call that failed, as the model reads it.
func failed(output string) toolloop.ToolResult {
	return toolloop.ToolResult{Output: output, Failed: true}
}

// refusalOf reads a refusal in the engine's own envelope: its code, its
// sentence and the grants that would admit the caller. A body that is no
// envelope is named by its status alone.
func refusalOf(r exec.Response) wire.Refusal {
	var body struct {
		Error   string   `json:"error"`
		Message string   `json:"message"`
		Grants  []string `json:"grants"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil || body.Error == "" {
		return wire.Refusal{Code: fmt.Sprintf("http_%d", r.Status),
			Message: http.StatusText(r.Status) + "."}
	}
	message := body.Message
	if message == "" {
		message = http.StatusText(r.Status) + "."
	}
	return wire.Refusal{Code: body.Error, Message: redact.Secrets(message), Grants: body.Grants}
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
func answerSummary(cards []wire.Card) string {
	for _, c := range cards {
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
		}
	}
	return "Done"
}

// matchRoute is the pattern among routes, each "METHOD /path" as the API
// mounts it, that method and path are a request on: a `{name}` segment
// matches any one non-empty segment, and a trailing `{name...}` the rest.
func matchRoute(routes []string, method, path string) (string, bool) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for _, route := range routes {
		m, pattern, ok := strings.Cut(route, " ")
		if !ok || m != method {
			continue
		}
		if matchPath(strings.Split(strings.Trim(pattern, "/"), "/"), segments) {
			return pattern, true
		}
	}
	return "", false
}

// matchPath reports whether path's segments are a request on pattern's.
func matchPath(pattern, path []string) bool {
	for i, p := range pattern {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "...}") {
			return len(path) > i
		}
		if i >= len(path) {
			return false
		}
		switch {
		case strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}"):
			if path[i] == "" {
				return false
			}
		case p != path[i]:
			return false
		}
	}
	return len(path) == len(pattern)
}

// stepClock is when a step started, for its duration.
type stepClock struct {
	wire.StepEvent
	started time.Time
}

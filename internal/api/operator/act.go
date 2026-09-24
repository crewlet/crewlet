package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ActPattern is the route the act transport is mounted at: ONE catalogue
// tool per request, named in the path, as the person the token is bound to.
//
// POST ONLY, because every tool it serves writes — a read is refused
// `read_only_tool` rather than served, since the socket already answers every
// question the dashboard asks and a second read path would be a second answer
// to them.
//
// REST RATHER THAN A SOCKET FRAME, because a write has to be able to say
// whether it happened: a frame sent into a socket that drops has no answer at
// all, while a request that loses its answer is retried under the same
// request id, as the same operations as the first attempt.
const ActPattern = "POST " + ActPathPrefix + "{tool}"

// ActPathPrefix is the path every act request starts with.
const ActPathPrefix = "/operator/act/"

// MaxActBody bounds one act request's body: twice the largest legal page, plus
// sixty-four kibibytes for everything else a request carries.
//
// DERIVED FROM THE LARGEST THING ANY ACT CARRIES, which is a page body on
// `write_page` or `save_page`. A page is capped at [pages.MaxBody] as TEXT,
// and the request carries it as a JSON STRING, where every quote, backslash
// and newline is two bytes — so a legal page of nothing but those encodes to
// twice its length, and a cap at the page's own size refused the page the
// tracker would have accepted. The browser's JSON.stringify escapes nothing
// else to more than two bytes but the other C0 controls, which are six, and a
// document is not made of them. The sixty-four kibibytes cover the envelope,
// a title at [pages.MaxTitle], labels, a message and a parent, with room to
// spare: every other tool's arguments are bounded far below it (a work item's
// description is 64 KiB of text).
const MaxActBody = 2*pages.MaxBody + 64<<10

// The transport's own refusals, beside the tool refusal classes
// ([crewletmcp.Refusals]) a call can come back with. Declared as this route's
// codes, in the idiom every route with codes of its own uses.
const (
	// CodeUnbound is a caller that is not a person: a disabled guard's
	// anonymous caller, or a token no seat binds with
	// `contact.crewlet_operator_id`. 403.
	CodeUnbound = httpjson.Code("unbound")

	// CodeUnknownTool is a tool this company's catalogue does not serve.
	// 404.
	CodeUnknownTool = httpjson.Code("unknown_tool")

	// CodeReadOnlyTool is a tool the catalogue serves as a proven read.
	// 400: the request is well formed and aimed at the wrong transport.
	CodeReadOnlyTool = httpjson.Code("read_only_tool")

	// CodeUnsupportedMediaType is a body that is not declared
	// `application/json`. 415.
	CodeUnsupportedMediaType = httpjson.Code("unsupported_media_type")

	// CodeInvalidRequestID is a `request_id` that is absent, not a UUID, or
	// the nil UUID. 400.
	CodeInvalidRequestID = httpjson.Code("invalid_request_id")
)

// ActTransportCodes is every `error` the act transport can answer with that
// is NOT a tool's refusal class: its own five, the shared body refusals, the
// guard's, the drain gate's, and the opaque answer to a tool failure that
// carried no class.
//
// THE ONE LIST, so the client's table of what it may be told is held against
// the engine's rather than against a copy: a code the client does not know is
// a refusal it renders as a generic failure, and one it knows that nothing
// sends is a sentence it will never show. No code here is also a refusal
// class — [TestEveryRefusalCodeMapsToOneStatus] holds that, because a caller
// branching on `error` must never need to know which half it came from.
var ActTransportCodes = []httpjson.Code{
	httpjson.CodeInvalidToken, CodeUnbound, CodeUnknownTool, CodeReadOnlyTool,
	CodeUnsupportedMediaType, CodeInvalidRequestID,
	httpjson.CodeInvalidBody, httpjson.CodeBodyTooLarge, httpjson.CodeUnreadableBody,
	httpjson.CodeDraining, httpjson.CodeInternalError,
}

// refusalStatus is the HTTP status each refusal class answers with.
//
// BY WHAT THE CALLER DOES NEXT, which is the only thing a status is for:
// 422 for an argument to change, 404 for an object that is not there, 403 for
// a gesture this person may not make, 409 for a state the request collided
// with and has to be re-read, and 503 for a node that could not serve it — the
// one class a retry of the same request can fix.
var refusalStatus = map[crewletmcp.Refusal]int{
	crewletmcp.RefusalInvalid:            http.StatusUnprocessableEntity,
	crewletmcp.RefusalNotFound:           http.StatusNotFound,
	crewletmcp.RefusalForbidden:          http.StatusForbidden,
	crewletmcp.RefusalStaleVersion:       http.StatusConflict,
	crewletmcp.RefusalConflict:           http.StatusConflict,
	crewletmcp.RefusalExists:             http.StatusConflict,
	crewletmcp.RefusalAlreadyAnswered:    http.StatusConflict,
	crewletmcp.RefusalReassignmentBudget: http.StatusConflict,
	crewletmcp.RefusalInboxFull:          http.StatusConflict,
	crewletmcp.RefusalNotRunning:         http.StatusConflict,
	crewletmcp.RefusalSteerUnsupported:   http.StatusConflict,
	crewletmcp.RefusalUnavailable:        http.StatusServiceUnavailable,
	crewletmcp.RefusalPeerUpgrading:      http.StatusServiceUnavailable,
}

// RefusalStatus is the status the act transport answers a refusal class
// with, and false for a class this build does not know — which the transport
// answers as an unclassified failure rather than guessing a status for.
func RefusalStatus(r crewletmcp.Refusal) (int, bool) {
	status, ok := refusalStatus[r]
	return status, ok
}

// statusClientClosedRequest is the status recorded for a request whose caller
// hung up before the tool answered. Nobody reads it — the connection is gone —
// but the access log does, and a 5xx there would count a closed tab as this
// node failing.
const statusClientClosedRequest = 499

// ActAnswer is what a write that went through answers.
//
// Outcome is the three-valued write outcome, and Position where its record
// landed — the floor a caller hands back as `min_position` so the read after
// the write includes it. A write that appended nothing (an update naming no
// change) answers `applied` at no position: the state asked for already
// holds, and there is nothing to wait for. Receipt is the tool's own answer,
// verbatim.
type ActAnswer struct {
	Tool     string           `json:"tool"`
	Outcome  statelog.Outcome `json:"outcome"`
	Position *string          `json:"position"`
	Receipt  json.RawMessage  `json:"receipt"`
}

// actRequest is the body: the caller's own request identity, and the tool's
// arguments.
//
// THE REQUEST ID IS IN THE BODY rather than a header so the preflight a
// cross-origin dashboard sends allows exactly the headers it always has
// (`Authorization`, `Content-Type`) — a custom header would widen the CORS
// posture of every guarded route to carry one field for one.
type actRequest struct {
	RequestID string         `json:"request_id"`
	Args      map[string]any `json:"args"`
}

// Acts names every tool the act transport serves, in catalogue order — the
// catalogue less its proven reads. What `viewer.acts` answers a bound
// person, so a screen enables exactly the controls a press of would be
// served. Nil for a nil server.
func (s *Server) Acts() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, name := range s.catalogue.names() {
		if !readOnly(name) {
			out = append(out, name)
		}
	}
	return out
}

// readOnly reports whether a tool is a PROVEN read — the catalogue's own hint,
// never a guess from its name. A tool whose read-only hint is unknown is
// served as the write it may be.
func readOnly(name string) bool {
	return crewletmcp.ReadOnlyProven(builtin.AnnotationsFor(name))
}

// ActHandler serves the act transport at [ActPattern].
//
// # The order of the checks
//
// What the caller names comes first (the tool, then the body), because those
// refusals are the same for everybody and cost nothing to answer; who the
// caller IS comes last, just before the call, so the one check the whole
// transport rests on sits beside the one line it protects.
//
// # Who the write is attributed to does not change here
//
// The author is the token, the kind `operator`, the bound seat rides beside
// them — all decided by [WorkActor] off the request's own credential, exactly
// as on the MCP transport. This handler adds nothing to the actor but the
// request id; see [WithRequestKey].
func (s *Server) ActHandler() http.Handler {
	return http.HandlerFunc(s.act)
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// THE GUARD IS THE APP'S: /operator is always guarded, so a request
	// that reaches here presented a valid token or found the guard
	// disabled. Refused rather than trusted if it somehow did not.
	operatorID, ok := auth.OperatorFrom(ctx)
	if !ok || operatorID == "" {
		log.WarnContext(ctx, "operator_act_unguarded",
			"detail", "a request reached the act transport with no operator on "+
				"its context; the auth guard is not in front of it")
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}

	name := r.PathValue("tool")
	if _, served := s.catalogue.lookup(name); !served {
		httpjson.FailWith(w, http.StatusNotFound, CodeUnknownTool, map[string]string{
			"tool":   name,
			"detail": fmt.Sprintf("this company's operator catalogue serves no tool %q", name),
		})
		return
	}
	if readOnly(name) {
		httpjson.FailWith(w, http.StatusBadRequest, CodeReadOnlyTool, map[string]string{
			"tool": name,
			"detail": fmt.Sprintf("%s is a read, and this transport only writes — "+
				"ask the question over the live socket or its REST route", name),
		})
		return
	}
	// JSON AND NOTHING ELSE, which is what shuts the door a browser leaves
	// open: a cross-site HTML form can post `text/plain`,
	// `application/x-www-form-urlencoded` or `multipart/form-data` without a
	// preflight, and a body that happens to parse as JSON would otherwise
	// reach a write. `application/json` from another origin needs a
	// preflight, which the CORS allow-list answers.
	if !declaresJSON(r.Header.Get("Content-Type")) {
		httpjson.FailWith(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType,
			map[string]string{
				"tool":   name,
				"detail": "the body must be declared Content-Type: application/json",
			})
		return
	}
	raw, err := httpjson.ReadBody(w, r, MaxActBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	req, err := decodeAct(raw)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"tool": name, "detail": err.Error()})
		return
	}
	requestID, err := parseRequestID(req.RequestID)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, CodeInvalidRequestID,
			map[string]string{"tool": name, "detail": err.Error()})
		return
	}

	// THE ONE RULE THIS TRANSPORT ADDS: the caller is a person. See the
	// package doc and ADR-0024.
	seat := seatFor(s.chart, operatorID)
	if seat == "" {
		httpjson.FailWith(w, http.StatusForbidden, CodeUnbound, unbound(name, operatorID))
		return
	}

	logged := []any{"tool", name, "operator_id", operatorID, "seat", seat,
		"request_id", requestID}
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	// THE KEY IS THE CREDENTIAL'S AND THE REQUEST'S TOGETHER, so a request
	// id only ever names operations made under the same token: one
	// person's retry is their own first attempt's operations, and no caller
	// can suppress somebody else's write by sending their id first.
	key := operatorID + "/" + requestID
	// THROUGH THE DISPATCH, which publishes the call's runtime audit
	// record whatever becomes of it — an interrupted call included, since
	// that is the one whose write nobody can vouch for.
	result, _, err := s.dispatch(WithRequestKey(ctx, key), types.TransportAct,
		requestID, name, args)
	if err != nil {
		interrupted(w, r, name, err, logged)
		return
	}
	if result.Failed {
		refuse(w, r, name, result.Output, result.Refusal, logged)
		return
	}
	answer := receiptOf(name, result.Output)
	log.InfoContext(ctx, "operator_act",
		append(logged, "outcome", string(answer.Outcome))...)
	httpjson.Write(w, http.StatusOK, answer)
}

// declaresJSON reports whether a Content-Type is JSON in UTF-8, which is the
// only charset JSON has.
func declaresJSON(header string) bool {
	media, params, err := mime.ParseMediaType(header)
	if err != nil || media != "application/json" {
		return false
	}
	charset, named := params["charset"]
	return !named || strings.EqualFold(charset, "utf-8")
}

// decodeAct reads the body as exactly one JSON object naming only the two
// fields the transport takes.
//
// A KEY THIS TRANSPORT DOES NOT READ IS REFUSED rather than ignored: a
// client that wrote `requestId` would otherwise send every write without an
// identity it believes it sent.
func decodeAct(raw []byte) (actRequest, error) {
	var req actRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return actRequest{}, fmt.Errorf("the body must be one JSON object "+
			"{\"request_id\": \"<uuid>\", \"args\": {…}}: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return actRequest{}, errors.New("the body carries something after its " +
			"JSON object; send exactly one")
	}
	return req, nil
}

// parseRequestID reads the caller's request identity, in its canonical
// spelling.
//
// CANONICAL, so one id written two ways — upper case, braces, a `urn:uuid:`
// prefix — is one request: the broker and the ledger compare the derived
// operation ids as strings, and a retry that spelled its id differently would
// be a second write. THE NIL UUID IS REFUSED, because a client that sent it
// for every request would have every write after its first derived as that
// one's.
func parseRequestID(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("`request_id` is required: a UUID the client mints " +
			"once per gesture and sends again, unchanged, on a retry")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("`request_id` is not a UUID: %w", err)
	}
	if id == uuid.Nil {
		return "", errors.New("`request_id` is the nil UUID, which would name " +
			"every request at once; mint a random one per gesture")
	}
	return id.String(), nil
}

// unbound is the refusal an act by somebody who is not a person gets, in the
// two forms the remedies take: a disabled guard is fixed in Tier A, an
// unbound token in the company chart.
func unbound(tool, operatorID string) map[string]string {
	if operatorID == auth.AnonymousOperator {
		return map[string]string{
			"tool": tool,
			"detail": "api.auth.disabled is set, so this caller was never checked " +
				"and is nobody — and a change on this surface is made by a person",
			"hint": "enable api.auth with a token for each person, and bind it " +
				"with contact.crewlet_operator_id on their seat",
		}
	}
	return map[string]string{
		"tool": tool,
		"detail": fmt.Sprintf("the token %q is bound to no seat, so there is no "+
			"person to act as", operatorID),
		"hint": fmt.Sprintf("give a human seat contact.crewlet_operator_id: %s; "+
			"an unbound token can still act through /operator/mcp", operatorID),
	}
}

// refuse answers a tool's refusal with its class and the status the class
// maps to — and the tool's own sentence as `detail`, since that sentence
// names the field or the object and the caller may show it.
//
// A FAILURE WITH NO CLASS, or one this build does not know, is a first-party
// tool that forgot to classify (`builtin`'s own suite walks for that): it is
// answered as an opaque internal error with the sentence in the log, because
// guessing a class for it would be a status the tool never chose.
func refuse(w http.ResponseWriter, r *http.Request, name, sentence string,
	class crewletmcp.Refusal, logged []any) {

	status, known := RefusalStatus(class)
	if !known {
		log.ErrorContext(r.Context(), "operator_act_unclassified",
			append(logged, "refusal", string(class), "detail", sentence)...)
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeInternalError,
			map[string]string{"tool": name})
		return
	}
	log.InfoContext(r.Context(), "operator_act", append(logged, "refusal", string(class))...)
	httpjson.FailWith(w, status, httpjson.Code(class),
		map[string]string{"tool": name, "detail": sentence})
}

// interrupted answers a call whose context ended before the tool did. NEVER A
// REFUSAL: nothing about the request was wrong, and whether its write landed
// is not known — so the answer says to send it again under the same request
// id, which names the same operations as whatever did land.
func interrupted(w http.ResponseWriter, r *http.Request, name string, err error, logged []any) {
	if r.Context().Err() != nil {
		// The caller hung up. There is nobody to answer; the status is for
		// the access log.
		log.InfoContext(r.Context(), "operator_act_abandoned",
			append(logged, "error", err.Error())...)
		w.WriteHeader(statusClientClosedRequest)
		return
	}
	log.WarnContext(r.Context(), "operator_act_interrupted",
		append(logged, "error", err.Error())...)
	httpjson.FailWith(w, http.StatusServiceUnavailable,
		httpjson.Code(crewletmcp.RefusalUnavailable), map[string]string{
			"tool": name,
			"detail": "the call was interrupted before it answered, so whether it " +
				"landed is unknown; send it again with the same request_id",
		})
}

// receiptOf is a successful call's answer: the tool's own, verbatim, with the
// outcome and position every write tool's answer states lifted beside it.
//
// READ FROM THE TOOL'S ANSWER rather than a second channel, because those two
// keys are already the tools' wire contract for read-your-writes — the same
// ones an operator's assistant hands back as `min_position`. An outcome this
// build does not know is reported `unknown`, never `applied`: it is the one
// answer that cannot tell a caller to stop looking at a write it cannot vouch
// for.
func receiptOf(name, output string) ActAnswer {
	answer := ActAnswer{Tool: name, Outcome: statelog.OutcomeApplied}
	if !json.Valid([]byte(output)) {
		// Every first-party write answers JSON; a sentence is carried as a
		// string rather than dropped.
		answer.Receipt, _ = json.Marshal(output)
		return answer
	}
	answer.Receipt = json.RawMessage(output)
	var stated struct {
		Outcome  *string `json:"outcome"`
		Position *string `json:"position"`
	}
	if err := json.Unmarshal([]byte(output), &stated); err != nil {
		return answer
	}
	if stated.Outcome != nil && *stated.Outcome != "" {
		answer.Outcome = statelog.Outcome(*stated.Outcome)
		if !answer.Outcome.Valid() {
			answer.Outcome = statelog.OutcomeUnknown
		}
	}
	if stated.Position != nil && *stated.Position != "" {
		answer.Position = stated.Position
	}
	return answer
}

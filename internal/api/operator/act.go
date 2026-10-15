package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ActPattern is the route the act transport is mounted at: ONE catalogue tool
// per request, named in the path, as the principal the request resolved to.
//
// POST ONLY, because every tool it serves writes — a read is refused
// `read_only_tool` rather than served, since the socket already answers every
// question the dashboard asks and a second read path would be a second answer
// to them.
//
// REST RATHER THAN A SOCKET FRAME, because a write has to be able to say
// whether it happened: a frame sent into a socket that drops has no answer at
// all, while a request that loses its answer is retried under the same
// operation key, as the same operations as the first attempt.
//
// ANY PRINCIPAL THE GUARD RESOLVED MAY CALL IT (ADR-0024), and every tool is
// decided by the authority table exactly as on every other surface: a person
// signed in writes as the seat the identity directory binds them to, a token
// bound to a seat writes as that seat, and a credential nobody is bound through
// writes under its own login. Nobody is refused for being unbound — that rule
// existed only while a token's binding lived in the chart, and a write
// attributed to a credential acting as itself is a write an audit can still
// ask about.
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
// knowledge base would have accepted. The browser's JSON.stringify escapes
// nothing else to more than two bytes but the other C0 controls, which are
// six, and a document is not made of them. The sixty-four kibibytes cover the
// envelope, a title at [pages.MaxTitle], labels, a message and a parent, with
// room to spare: every other tool's arguments are bounded far below it (a work
// item's description is 64 KiB of text).
const MaxActBody = 2*pages.MaxBody + 64<<10

// ActTransportCodes is every `error` the act transport can answer with that
// is NOT a tool's refusal class: its own three, the operation key's, the
// shared body refusals, the guard's, the identity estate's, the authority
// table's, the drain gate's and the unknown outcome's.
//
// THE ONE LIST, so the client's table of what it may be told is held against
// the engine's rather than against a copy: a code the client does not know is
// a refusal it renders as a generic failure, and one it knows that nothing
// sends is a sentence it will never show. No code here is also a refusal
// class's — [TestEveryRefusalCodeMapsToOneStatus] holds that, because a caller
// branching on `error` must never need to know which half it came from —
// except the four classes the envelope already spelled (`not_found`,
// `forbidden`, `unavailable`, `internal_error`), which are listed with the
// classes. `internal_error` is also what a failure that carried NO class is
// answered with, and a wiring that decided nothing: all three say the engine
// broke, and none has a remedy the caller can apply.
var ActTransportCodes = []httpjson.Code{
	// The request guard's: nobody, an identity this node could not read, a
	// seat the chart no longer holds, a session that may only enrol a
	// second factor.
	httpjson.CodeInvalidToken, httpjson.CodeIdentityUnavailable,
	httpjson.CodeSeatUnavailable, httpjson.CodeSecondFactorEnrolmentRequired,
	// The cross-site write gate's.
	httpjson.CodeCSRFOrigin,
	// The authority table's, through the one refusal mapping.
	httpjson.CodeUnauthorized, httpjson.CodeStepUpRequired,
	// This route's own.
	httpjson.CodeUnknownTool, httpjson.CodeReadOnlyTool,
	httpjson.CodeUnsupportedMediaType,
	// The operation key's, and a key that already names another write.
	httpjson.CodeOpIDInvalid, httpjson.CodeInvalidInput,
	// The shared body refusals, an argument the tool does not declare
	// among them.
	httpjson.CodeInvalidBody, httpjson.CodeBodyTooLarge, httpjson.CodeUnreadableBody,
	// A node that has not been handed a company, or is draining.
	httpjson.CodeNoActiveRevision, httpjson.CodeDraining,
}

// ActAnswer is what a write that went through answers.
//
// Outcome is the three-valued write outcome, and Position where its record
// landed — the floor a caller hands back as `min_position` so the read after
// the write includes it. A write that appended nothing (an update naming no
// change) answers `applied` at no position: the state asked for already
// holds, and there is nothing to wait for. OpID is the operation the request
// was made under — the key a retry sends back. Receipt is the tool's own
// answer, verbatim.
type ActAnswer struct {
	Tool     string           `json:"tool"`
	Outcome  statelog.Outcome `json:"outcome"`
	Position *string          `json:"position"`
	OpID     string           `json:"op_id"`
	Receipt  json.RawMessage  `json:"receipt"`
}

// actRequest is the body: the tool's arguments, and nothing else.
//
// THE OPERATION IS IN THE HEADER, not the body — internal/api/opkey's
// `Idempotency-Key`, which every surface that takes one reads it from — so a
// retry is one script whichever route it wrote to, and the key is held to one
// grammar and scoped by one rule.
type actRequest struct {
	Args map[string]any `json:"args"`
}

// Acts names the tools the act transport would serve this principal: the
// catalogue's non-read tools the authority table could admit them to before
// any object is named ([builtin.Admits]) — what the viewer's `acts` answers,
// so a screen enables exactly the controls a press of could be served.
//
// AN ERROR WHERE THIS NODE CANNOT DECIDE, which the viewer answers as the
// identity estate's 503 rather than as an empty list: "you may do nothing" is
// an answer a person acts on. A node that has not been handed a company serves
// no tool, so it answers an empty list — every press would be refused `503
// no_active_revision` — and never nil.
func (s *Server) Acts(ctx context.Context, p iam.Principal) ([]string, error) {
	out := []string{}
	if s == nil {
		return out, nil
	}
	cat, up := s.catalogueFor()
	if !up {
		return out, nil
	}
	ctx = iam.WithPrincipal(ctx, p)
	for _, name := range cat.names() {
		if readOnly(name) {
			continue
		}
		err := builtin.Admits(ctx, s.authorize, name)
		switch {
		case err == nil:
			out = append(out, name)
		case errors.Is(err, builtin.ErrRefused):
		default:
			return nil, fmt.Errorf("operator: whether %s may be offered: %w", name, err)
		}
	}
	return out, nil
}

// ActHandler serves the act transport at [ActPattern].
//
// # The order of the checks
//
// Who the caller is comes first — the guard already decided it, and a request
// it could not resolve has nobody to refuse in any other terms. Then what the
// caller names (the tool, then the body, then the operation), because those
// refusals are the same for everybody and cost nothing to answer; the tool's
// own authority comes last, inside the call, where the one decision every
// surface makes is made.
func (s *Server) ActHandler() http.Handler {
	return http.HandlerFunc(s.act)
}

func (s *Server) act(w http.ResponseWriter, r *http.Request) {
	// THE GUARD IS THE APP'S: this path is not on its exemption list, so
	// a request that reaches here carries its answer — refused rather than
	// trusted if it somehow does not, through [auth.Caller]'s two arms.
	if _, ok := auth.Caller(w, r); !ok {
		return
	}
	name := r.PathValue("tool")
	cat, up := s.catalogueFor()
	if !up {
		httpjson.NoActiveRevision(w, httpjson.Detail{
			"tool": name, "detail": httpjson.NativeHalvesNotUp})
		return
	}
	if _, served := cat.lookup(name); !served {
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeUnknownTool,
			httpjson.Detail{"tool": name, "detail": fmt.Sprintf(
				"this company's operator catalogue serves no tool %q", name)})
		return
	}
	if readOnly(name) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeReadOnlyTool,
			httpjson.Detail{"tool": name, "detail": fmt.Sprintf("%s is a read, and "+
				"this transport only writes — ask the question over the live "+
				"socket or its REST route", name)})
		return
	}
	// JSON AND NOTHING ELSE, which is what shuts the door a browser leaves
	// open: a cross-site HTML form can post `text/plain`,
	// `application/x-www-form-urlencoded` or `multipart/form-data` without a
	// preflight, and a body that happens to parse as JSON would otherwise
	// reach a write. `application/json` from another origin needs a
	// preflight, which the CORS allow-list answers — and the cross-site
	// write gate in front of every guarded route refuses its Origin anyway.
	if !declaresJSON(r.Header.Get("Content-Type")) {
		httpjson.FailWithFields(w, http.StatusUnsupportedMediaType,
			httpjson.CodeUnsupportedMediaType, httpjson.Detail{"tool": name,
				"detail": "the body must be declared Content-Type: application/json"})
		return
	}
	raw, err := httpjson.ReadBody(w, r, MaxActBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	req, err := decodeAct(raw)
	if err != nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"tool": name, "detail": err.Error()})
		return
	}
	// THE OPERATION IS REQUIRED: the dashboard mints one per gesture and
	// sends it again, unchanged, on a retry — a key minted here would be
	// handed back in exactly the answer a retry exists because it never
	// arrived. Scoped by the principal, so a key somebody copied names an
	// operation of theirs and never the owner's.
	key, ok := opkey.Require(w, r)
	if !ok {
		return
	}
	args := req.Args
	if args == nil {
		args = map[string]any{}
	}
	if !NoOperationArg(args) {
		RefuseOperationArg(w, name)
		return
	}
	logged := []any{"tool", name, "op_id", key}
	// THROUGH THE DISPATCH, which publishes the call's runtime audit
	// record whatever becomes of it — an interrupted call included, since
	// that is the one whose write nobody can vouch for.
	result, err := s.run(r.Context(), cat, Call{
		Transport: types.TransportAct, Key: key, Tool: name, Args: args,
	})
	if err != nil {
		Interrupted(w, r, key, name, err)
		return
	}
	if result.Failed {
		log.InfoContext(r.Context(), "operator_act", append(logged,
			"refused", true)...)
		Fail(w, result.Cause, result.Output, key, About(name, args))
		return
	}
	answer := receiptOf(name, result.Output)
	answer.OpID = key
	log.InfoContext(r.Context(), "operator_act",
		append(logged, "outcome", string(answer.Outcome))...)
	httpjson.Write(w, http.StatusOK, answer)
}

// RefuseOperationArg answers a call that names its operation in the header and
// ALSO carries an `op_id` argument: `400 invalid_body`, naming where the
// operation goes on this surface. See [NoOperationArg].
func RefuseOperationArg(w http.ResponseWriter, tool string) {
	httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
		httpjson.Detail{"tool": tool, "detail": "this surface takes the " +
			"operation from the " + opkey.Header + " header, which every write " +
			"the request makes derives its id from; leave `" + OperationArg +
			"` out of the arguments and send the op_id an earlier answer " +
			"returned as the header"})
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

// decodeAct reads the body as exactly one JSON object naming only the field the
// transport takes.
//
// A KEY THIS TRANSPORT DOES NOT READ IS REFUSED rather than ignored: a client
// naming its operation in the body (`op_id`) rather than in the
// Idempotency-Key header would otherwise send every write believing it named
// an operation it did not, and its retry would be a second write.
func decodeAct(raw []byte) (actRequest, error) {
	var req actRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return actRequest{}, fmt.Errorf("the body must be one JSON object "+
			"{\"args\": {…}}, with the operation in the %s header: %w",
			opkey.Header, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return actRequest{}, errors.New("the body carries something after its " +
			"JSON object; send exactly one")
	}
	return req, nil
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

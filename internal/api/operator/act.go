package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/modelnote"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
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
// `write_page` or `save_page`. A page is capped at [doclet.MaxBody] as TEXT,
// and the request carries it as a JSON STRING, where every quote, backslash
// and newline is two bytes — so a legal page of nothing but those encodes to
// twice its length, and a cap at the page's own size refused the page the
// knowledge base would have accepted. The browser's JSON.stringify escapes
// nothing else to more than two bytes but the other C0 controls, which are
// six, and a document is not made of them. The sixty-four kibibytes cover the
// envelope, a title at [doclet.MaxTitle], labels, a message and a parent, with
// room to spare: every other tool's arguments are bounded far below it (a work
// item's description is 64 KiB of text).
const MaxActBody = 2*doclet.MaxBody + 64<<10

// ActTransportCodes is every `error` the act transport can answer with that
// is NOT a tool's refusal class: its own three, the operation key's, the
// shared body refusals, the guard's, the identity estate's, the authority
// table's, the drain gate's, a starting node's and the unknown outcome's.
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
	// second factor, and a request made for a reader the credential is not.
	httpjson.CodeInvalidToken, httpjson.CodeIdentityUnavailable,
	httpjson.CodeSeatUnavailable, httpjson.CodeSecondFactorEnrolmentRequired,
	httpjson.CodeReaderChanged,
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
	// A node that has not been handed a company, or is draining — or is
	// still starting, which its listener answers for every route before the
	// node hands it this surface (internal/api's Boot).
	httpjson.CodeNoActiveRevision, httpjson.CodeDraining, httpjson.CodeStarting,
}

// ActAnswer is what a write that went through answers.
//
// Outcome is the three-valued write outcome, and Position where its record
// landed — the floor a caller hands back as `min_position` so the read after
// the write includes it. A write that appended nothing (an update naming no
// change) answers `applied` at no position: the state asked for already
// holds, and there is nothing to wait for. OpID is the operation the request
// was made under — the key a retry sends back. Receipt is the tool's own
// answer, verbatim but for its `warnings`, and Caveats is what those warnings
// say, in a person's words.
//
// # Caveats, never the receipt's warnings (ADR-0060)
//
// The tools are shared: a seat's model calls them too, and a tool's answer
// carries its caveats under `warnings` in the MODEL'S words, naming the tool
// and the argument it would call next ("Use ask: ana"). This route answers a
// person, so it answers each caveat's PERSON phrasing ([caveat.Caveat.Person])
// with the action it asks of them, and takes the model's list off the receipt
// ([builtin.WarningsKey]): a screen reading the receipt can then never show a
// person advice written for a model, however it is written. Caveats is always
// a list, empty when the write has nothing to add.
type ActAnswer struct {
	Tool     string           `json:"tool"`
	Outcome  statelog.Outcome `json:"outcome"`
	Position *string          `json:"position"`
	OpID     string           `json:"op_id"`
	Receipt  json.RawMessage  `json:"receipt"`
	Caveats  []caveat.Shown   `json:"caveats"`
}

// actRequest is the body: the tool's arguments, and nothing else.
//
// THE OPERATION IS IN THE HEADER, not the body — internal/api/opkey's
// `Idempotency-Key`, which every surface that takes one reads it from — so a
// retry is one script whichever route it wrote to, and the key is held to one
// grammar and scoped by one rule.
//
// THE ARGUMENTS ARE HELD RAW and read by the one decode every transport into
// a tool shares ([toolargs.Decode]), never by this envelope's decoder.
type actRequest struct {
	Args json.RawMessage `json:"args"`
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
	// THE ROUTE'S OWN REFUSALS NAME THE TOOL AND SAY NOTHING ELSE: each is
	// a request the client built wrong or a node not ready, which its code
	// says whole, and a person reads that code's sentence (ADR-0060). What a
	// developer needs beyond it is logged.
	name := r.PathValue("tool")
	about := httpjson.Detail{"tool": name}
	cat, up := s.catalogueFor()
	if !up {
		httpjson.NoActiveRevision(w, about)
		return
	}
	if _, served := cat.lookup(name); !served {
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeUnknownTool, about)
		return
	}
	if readOnly(name) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeReadOnlyTool, about)
		return
	}
	// JSON AND NOTHING ELSE, which shuts the door a cross-site form leaves
	// open ([httpjson.DeclaresJSON]).
	if !httpjson.DeclaresJSON(r.Header.Get("Content-Type")) {
		httpjson.FailWithFields(w, http.StatusUnsupportedMediaType,
			httpjson.CodeUnsupportedMediaType, about)
		return
	}
	raw, err := httpjson.ReadBody(w, r, MaxActBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	args, err := decodeAct(raw)
	if err != nil {
		log.InfoContext(r.Context(), "operator_act_body_refused", "tool", name,
			"error", err.Error())
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody, about)
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
	if !NoOperationArg(args) {
		log.InfoContext(r.Context(), "operator_act_body_refused", "tool", name,
			"error", "the arguments name "+OperationArg)
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"tool": name, "field": OperationArg})
		return
	}
	logged := []any{"tool", name, "op_id", key}
	transport, refusal := actTransport(r.Context(), name)
	if refusal != "" {
		AuditRefusal(r.Context(), s.audit, transport, name, key, false)
		log.WarnContext(r.Context(), "operator_act_assist_refused", append(logged,
			"detail", refusal)...)
		status, code, fields := authz.RefusalFields(authz.Decision{
			Reason: authz.ReasonAssistRefused})
		httpjson.FailWithFields(w, status, code, fields)
		return
	}
	// THROUGH THE DISPATCH, which publishes the call's runtime audit
	// record whatever becomes of it — an interrupted call included, since
	// that is the one whose write nobody can vouch for.
	result, err := s.run(r.Context(), cat, Call{
		Transport: transport, Key: key, Tool: name, Args: args,
	})
	if err != nil {
		Interrupted(w, r, key, name, err)
		return
	}
	if result.Failed {
		log.InfoContext(r.Context(), "operator_act", append(logged,
			"refused", true, "refusal", string(classOf(result.Cause)))...)
		FailTool(w, r, result, key, About(name, args))
		return
	}
	answer := receiptOf(name, result)
	answer.OpID = key
	TellNote(r, result)
	log.InfoContext(r.Context(), "operator_act",
		append(logged, "outcome", string(answer.Outcome))...)
	httpjson.Write(w, http.StatusOK, answer)
}

// actTransport is the transport an act call arrives on, and why an assistant
// call may not be made, or "".
//
// AN ASSISTANT CALL IS ONE APPROVED WRITE (ADR-0051). The request guard
// already admits an assistant grant here only as an exec grant carrying the
// one request its approval bound, once; this is the second lock, on the one
// surface every server write goes through: the grant must name the proposal
// the person approved and the tool it was approved for, so a human-only verb
// (pausing a seat, steering a turn, answering a run) is never carried by a
// grant that was not derived from that verb's own approval. A refusal is
// audited under the assistant transport.
func actTransport(ctx context.Context, tool string) (string, string) {
	claims, assisted := auth.AssistOf(ctx)
	if !assisted {
		return types.TransportAct, ""
	}
	switch {
	case claims.Scope != iam.AssistScopeExec:
		return types.TransportAssistant, "an assistant read grant never writes"
	case claims.Proposal == uuid.Nil:
		return types.TransportAssistant, "the grant names no approved proposal"
	case string(claims.Action) != tool:
		return types.TransportAssistant, fmt.Sprintf("the grant was approved for %q, "+
			"not %q", claims.Action, tool)
	}
	return types.TransportAssistant, ""
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

// decodeAct reads the body as exactly one JSON object naming only the field the
// transport takes, and answers the tool's arguments it carries — never nil.
//
// A KEY THIS TRANSPORT DOES NOT READ IS REFUSED rather than ignored: a client
// naming its operation in the body (`op_id`) rather than in the
// Idempotency-Key header would otherwise send every write believing it named
// an operation it did not, and its retry would be a second write.
//
// THE ARGUMENTS' NUMBERS ARRIVE AS json.Number ([toolargs.Decode]), as
// they do on every other path into these tools. Decoded by this envelope's
// own decoder they were float64s, so an integer past 2^53 — a vendor's
// 19-digit id, a whole-number custom-field value — was rounded before any tool
// saw it, and a person's press recorded a value they never sent.
func decodeAct(raw []byte) (map[string]any, error) {
	var req actRequest
	switch err := httpjson.DecodeStrict(raw, &req); {
	case errors.Is(err, httpjson.ErrTrailingData):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("the body must be one JSON object "+
			"{\"args\": {…}}, with the operation in the %s header: %w",
			opkey.Header, err)
	}
	args, err := toolargs.Decode(req.Args)
	if err != nil {
		return nil, fmt.Errorf("the body's `args`: %w", err)
	}
	return args, nil
}

// receiptOf is a successful call's answer: the tool's own, with the outcome and
// position every write tool's answer states lifted beside it, and its caveats
// in a person's words in place of the model's ([ActAnswer]).
//
// READ FROM THE TOOL'S ANSWER rather than a second channel, because those two
// keys are already the tools' wire contract for read-your-writes — the same
// ones an operator's assistant hands back as `min_position`. An outcome this
// build does not know is reported `unknown`, never `applied`: it is the one
// answer that cannot tell a caller to stop looking at a write it cannot vouch
// for.
func receiptOf(name string, result tools.Result) ActAnswer {
	receipt, caveats, readable := PersonsReceipt(name, result)
	answer := ActAnswer{Tool: name, Outcome: statelog.OutcomeApplied,
		Caveats: caveats, Receipt: json.RawMessage("null")}
	if !readable {
		// THE WRITE LANDED, and its tool answered something that is not a
		// receipt: the outcome stands, and the tool's words, written for
		// a model, are in this node's log and not in the answer.
		return answer
	}
	answer.Receipt = receipt
	var stated struct {
		Outcome  *string `json:"outcome"`
		Position *string `json:"position"`
	}
	if err := json.Unmarshal(receipt, &stated); err != nil {
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

// PersonsReceipt is a tool call that landed, as every surface answering a
// PERSON answers it (ADR-0060): the tool's receipt with every key that holds
// its model's words taken off ([builtin.ModelOnlyKeys]: its caveats under
// `warnings`, and a partial outcome's sentence under its own key), the rest
// as the tool wrote it, and its caveats in a person's words, always a list. The act route and the
// human write surface both answer through it, so neither can come to hand a
// person what the other strips.
//
// A RECEIPT THAT IS NOT A JSON OBJECT is not readable: every first-party
// write answers one, so this is a tool that changed shape, and what it said
// instead is written for a model. It is logged here, under the tool's name,
// and never answered; the caller says what the write came to without it.
func PersonsReceipt(tool string, result tools.Result) (json.RawMessage, []caveat.Shown, bool) {
	caveats := caveat.ForPeople(result.Caveats)
	receipt, readable := builtin.ForPerson(result.Output)
	if !readable {
		log.Warn("operator_receipt_unreadable", "tool", tool, "output", result.Output)
		return nil, caveats, false
	}
	return receipt, caveats, true
}

// FailTool answers a refused tool call to a person, as every surface answering
// one does (ADR-0060): in the person's words the refusal carries, or its
// code's own sentence ([Fail]), and never the tool's sentence, which is
// written for a model. That sentence is told in process to the one reader
// that can act on it, an assistant's model executing what its person approved
// (internal/modelnote), and to nobody else.
func FailTool(w http.ResponseWriter, r *http.Request, result tools.Result, key string,
	about httpjson.Detail) {

	Fail(w, result.Cause, result.PersonRefusal, key, about)
	modelnote.Tell(r.Context(), result.Output)
}

// TellNote tells the note on a successful call's arguments ([tools.Result.Note])
// to the one reader it is worded for, the model that wrote the call, in
// process (internal/modelnote), as [FailTool] tells a refusal's sentence. A
// person's answer never carries it (ADR-0059, ADR-0060): the act route and the
// human write surface answer the receipt through [PersonsReceipt], which reads
// only the tool's answer, and call this beside it.
func TellNote(r *http.Request, result tools.Result) {
	if result.Note != "" {
		modelnote.Tell(r.Context(), result.Note)
	}
}

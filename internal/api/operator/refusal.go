package operator

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ONE REFUSAL MAPPING: what a call that did not do what it was asked is
// answered with over HTTP — its status, its code and the envelope beside them —
// on the act route, the human write surface's tool-backed routes and its
// tool-less verbs alike. The MCP transport answers a refused call as the tool's
// own failed result (that is the protocol's shape for it) and reads the same
// classes for its audit record.
//
// # From the CAUSE, never from the sentence
//
// A tool's refusal carries the error underneath it as its cause, beneath the
// class it stated (internal/mcp's [crewletmcp.Refusal]). Reading a status back
// out of a sentence written for a model would make the wording of every
// refusal an API.
//
// # A person's words, or none (ADR-0060)
//
// Every caller of this mapping answers a PERSON: the dashboard, a person's
// assistant executing what they approved, a script run on a person's
// credential. So the `detail` is the refusal in a person's words
// ([caveat.Refusal]) and never the tool's sentence, which is written for a
// model and names the arguments and tools a model would use next. Where
// nobody has worded the refusal for a person yet, the answer carries NO
// detail, and its code's own sentence (the envelope's `message`, written for
// people) is the whole of what it says. The parameter is typed, so no caller
// can hand this mapping a free sentence: every detail it writes is one the
// catalog's walk has read.
//
// # The classes are the codes
//
// A refused call answers with its class's own spelling as the `error` —
// `stale_version`, `budget_exhausted`, `not_running` — whichever transport it
// came through, so a client reads one vocabulary and a screen branches on the
// class rather than the prose. What the class does NOT say, the cause does,
// and those arms come first: a write nobody can vouch for, an operation key
// that already names another write, a permanent deletion, nobody presented a
// credential, an argument the tool does not declare, the authority table's own
// answer (with the rule's reason and the grants that would have admitted the
// caller), a node that could not decide, a wiring that decided nothing.

// About is what a call's failure answer names beside its own fields: the tool,
// and the object its arguments named — the item, the page, the project, the
// person — so a refusal or an unknown outcome says which object to read to see
// what became of it. A failed tool answers a sentence and no receipt, and a 503
// that dropped what it was about left a client holding a key and nothing
// saying where to look whether it landed. The `about` every transport hands
// [Fail].
//
// THE ARGUMENTS ARE EVERY NATIVE DOMAIN'S OWN ([builtin.ObjectArgs]), declared
// once beside the domain's refusal classes, rather than a list here: a list
// here was one more place a domain added later had to be remembered in.
func About(tool string, args map[string]any) httpjson.Detail {
	about := httpjson.Detail{"tool": tool}
	for _, field := range builtin.ObjectArgs() {
		if named, ok := args[field].(string); ok && strings.TrimSpace(named) != "" {
			about[field] = named
		}
	}
	return about
}

// classAnswer is how one refusal class is answered.
type classAnswer struct {
	// status is the 4xx a refusal of the class is answered with — or the
	// 500 of a FAULT, which no request can change and no wait clears — and
	// ZERO for a class waiting can clear, which is answered `503` through
	// [httpjson.UnavailableWith] — the one writer that sets the
	// Retry-After beside the envelope, so no row here could state a 503
	// that goes out without one.
	status int
	code   httpjson.Code
}

// classAnswers is every refusal class's status and code.
//
// BY WHAT THE CALLER DOES NEXT, which is the only thing a status is for: 422
// for an argument to change, 404 for an object that is not there, 403 for a
// gesture the domain forbids this caller, 409 for a state the request collided
// with — re-read it, or (for a spent budget, a full inbox, a run that stopped)
// change it — 503 for a node that could not serve it, the one class a retry of
// the same request can fix, and 500 for a node that FAILED at something of its
// own ([crewletmcp.RefusalInternalError]): nothing the caller does next helps,
// and a 503 there told a client to come back in two seconds, for ever. A test
// answers every class this build knows through [Fail] and holds each to
// exactly one status.
var classAnswers = map[crewletmcp.Refusal]classAnswer{
	crewletmcp.RefusalInvalid:            {http.StatusUnprocessableEntity, httpjson.CodeInvalid},
	crewletmcp.RefusalNotFound:           {http.StatusNotFound, httpjson.CodeNotFound},
	crewletmcp.RefusalForbidden:          {http.StatusForbidden, httpjson.CodeForbidden},
	crewletmcp.RefusalStaleVersion:       {http.StatusConflict, httpjson.CodeStaleVersion},
	crewletmcp.RefusalConflict:           {http.StatusConflict, httpjson.CodeConflict},
	crewletmcp.RefusalExists:             {http.StatusConflict, httpjson.CodeExists},
	crewletmcp.RefusalAlreadyAnswered:    {http.StatusConflict, httpjson.CodeAlreadyAnswered},
	crewletmcp.RefusalReassignmentBudget: {http.StatusConflict, httpjson.CodeReassignmentBudget},
	crewletmcp.RefusalInboxFull:          {http.StatusConflict, httpjson.CodeInboxFull},
	crewletmcp.RefusalNotRunning:         {http.StatusConflict, httpjson.CodeNotRunning},
	crewletmcp.RefusalSteerUnsupported:   {http.StatusConflict, httpjson.CodeSteerUnsupported},
	crewletmcp.RefusalBudgetExhausted:    {http.StatusConflict, httpjson.CodeBudgetExhausted},
	crewletmcp.RefusalUnavailable:        {0, httpjson.CodeUnavailable},
	crewletmcp.RefusalInternalError:      {http.StatusInternalServerError, httpjson.CodeInternalError},
}

// Fail writes the answer to one call that was NOT made: cause is the failed
// result's cause (or a tool-less writer's error, through [ClassifyWrite]),
// said the refusal in a person's words, answered as `detail` (the zero value
// answers none, and the code's own sentence stands alone), key the operation
// the request named, carried on a 503 and a reused key's 409 so the retry it
// invites is the same operation, and about the fields that name what the call
// was about (`tool`, an item's key), written beneath the envelope's own.
func Fail(w http.ResponseWriter, cause error, said caveat.Refusal, key string,
	about httpjson.Detail) {

	detail := httpjson.Detail{}
	maps.Copy(detail, about)
	if !said.IsZero() {
		detail["detail"] = said.Person()
		// WHERE THE LIMIT THAT CAUSED IT IS CHANGED, beside the words, for
		// a refusal a configurable limit caused (ADR-0060): a value the
		// dashboard resolves to the screen that edits it.
		if remedy := said.Remedy(); !remedy.IsZero() {
			detail["remedy"] = remedy
		}
	}
	var unknown *builtin.UnknownOutcome
	var refused *builtin.DecisionRefused
	switch {
	case errors.As(cause, &unknown):
		// NOT A REFUSAL AND NOT A FAILURE: a write whose outcome nobody
		// can establish, and it may well have landed — so a client told
		// "nothing was written" would make it a second time. FIRST,
		// because the cause it wraps may be anything the step answered.
		UnknownOutcome(w, key, unknown.Unvouched, about)
	case errors.Is(cause, crewletmcp.ErrOutcomeUnknown):
		// The sentinel without its facts: nothing says another node could
		// do better, so it is the ordinary unknown.
		UnknownOutcome(w, key, false, about)
	case refusedFor(cause, statelog.ReasonOpReused):
		// AN OPERATION THIS REQUEST DERIVES ALREADY NAMES A WRITE TO
		// ANOTHER OBJECT — what the request names moved since the key
		// was first sent — and nothing was written. A conflict the
		// caller resolves with a new key, never by waiting.
		// The field and the key name the cause for a client; the
		// person reads [ReusedKey], since what the tool refused for
		// is not why nothing was written.
		detail["field"] = opkey.Header
		detail["op_id"] = key
		detail["detail"] = ReusedKey
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeInvalidInput, detail)
	case refusedFor(cause, statelog.ReasonDeleted):
		// A PERMANENT DELETION MARKER on what the write is about: it was
		// purged, and nothing will ever write it again — a 404 for what
		// no longer exists, rather than a 503 telling a client to find a
		// node that will.
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound, detail)
	case errors.Is(cause, builtin.ErrUnauthenticated):
		// "Present a credential" and "you may not" send a person to
		// opposite places.
		httpjson.FailWithFields(w, http.StatusUnauthorized, httpjson.CodeInvalidToken, detail)
	case errors.Is(cause, builtin.ErrUndeclaredArgument):
		// THE REQUEST IS THE CALLER'S TO CHANGE: an argument the tool does
		// not read, refused by name at the tools' own gate rather than
		// dropped, and named as `fields`.
		var unread *builtin.UndeclaredArguments
		if errors.As(cause, &unread) {
			detail[KeyFields] = unread.Names
		}
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody, detail)
	case errors.Is(cause, builtin.ErrNoAuthorizer):
		// A WIRING THAT DECIDED NOTHING, which no retry clears.
		log.Error("operator_no_authorizer", "error", cause.Error())
		httpjson.FailWithFields(w, http.StatusInternalServerError,
			httpjson.CodeInternalError, about)
	case errors.Is(cause, builtin.ErrUndecidable):
		// THIS NODE COULD NOT DECIDE — the organisation it runs or the
		// caller's identity could not be read — so waiting can clear it.
		detail["op_id"] = key
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			authz.RetryUndecidedSeconds, detail)
	case errors.As(cause, &refused):
		// THE AUTHORITY TABLE'S OWN REFUSAL, in the envelope every surface
		// answers one with: the rule's reason and the grants that would
		// have admitted the caller, which is what a screen says would
		// change it.
		refuseDecision(w, refused.Decision, detail)
	case errors.Is(cause, builtin.ErrRefused):
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized, detail)
	case errors.Is(cause, iam.ErrNoHolder), errors.Is(cause, iam.ErrHolderUnseated):
		// A LOGIN NOBODY HOLDS, and a holder whose seat the chart no longer
		// has, name no record — which the caller only learns once the
		// table admitted them on the name as typed, so this is never a
		// roster.
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound, detail)
	default:
		failClass(w, cause, key, detail, about)
	}
}

// failClass answers a refusal by the class its cause states.
//
// A FAULT ([crewletmcp.RefusalInternalError]) is `500 internal_error` with NO
// Retry-After, and its error is in the log and nowhere else.
//
// A FAILURE WITH NO CLASS this build knows is a first-party tool that forgot to
// classify (`builtin`'s own suite walks for that) or a writer error nothing
// classified: it is answered as an opaque internal error with its cause in the
// log, because guessing a class for it would be a status nobody chose, and
// with no detail at all.
func failClass(w http.ResponseWriter, cause error, key string, detail,
	about httpjson.Detail) {

	class := classOf(cause)
	answer, known := classAnswers[class]
	switch {
	case !known:
		errText := "<nil>"
		if cause != nil {
			errText = cause.Error()
		}
		log.Error("operator_refusal_unclassified", "refusal", string(class),
			"error", errText)
		httpjson.FailWithFields(w, http.StatusInternalServerError,
			httpjson.CodeInternalError, about)
	case answer.status == 0:
		// WHETHER AND WHEN TO COME BACK is the cause's to say: a refusal
		// waiting cannot clear — an evicted node, a full log, a record
		// this node cannot decode — carries no Retry-After, one that
		// derived its own hint carries that, and anything else this
		// node's undecided hint ([statelog.RetryAfter]'s rule, which
		// every surface answering a refusal reads).
		otherwise := time.Duration(authz.RetryUndecidedSeconds) * time.Second
		detail["op_id"] = key
		httpjson.UnavailableWith(w, answer.code,
			httpjson.RetrySeconds(statelog.RetryAfter(cause, otherwise)), detail)
	default:
		httpjson.FailWithFields(w, answer.status, answer.code, detail)
	}
}

// classOf is the refusal class a cause states, or "" where it states none.
func classOf(cause error) crewletmcp.Refusal {
	return crewletmcp.RefusalOf(crewletmcp.Result{Failed: true, Cause: cause})
}

// refusedFor reports a write the state log refused for one reason.
func refusedFor(err error, reason statelog.Reason) bool {
	var refused *statelog.Unavailable
	return errors.As(err, &refused) && refused.Reason == reason
}

// refuseDecision writes one authority REFUSAL in the engine's envelope: `403
// step_up_required` naming the window for a stale proof, `403 token_refused`
// for a machine token on a verb that needs a person present, and `403
// unauthorized` with the rule's reason and grants otherwise, each beside
// whatever else detail carries (the sentence a tool's gate worded it with).
//
// THE SAME CODES [authz.EnvelopeRefusal] ANSWERS, so a route that decides
// itself and one the router stopped cannot answer one refusal two ways. A
// token refusal answered `unauthorized` carried that code's sentence about a
// grant the credential lacks, which no grant on a token would supply.
func refuseDecision(w http.ResponseWriter, d authz.Decision, detail httpjson.Detail) {
	if d.Reason == authz.ReasonStepUp {
		for k, v := range authz.StepUpDetail(d) {
			detail[k] = v
		}
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeStepUpRequired, detail)
		return
	}
	for k, v := range authz.RefusalDetail(d.Reason, d.Grants) {
		detail[k] = v
	}
	code := httpjson.CodeUnauthorized
	if d.Reason == authz.ReasonTokenRefused {
		code = httpjson.CodeTokenRefused
	}
	httpjson.FailWithFields(w, http.StatusForbidden, code, detail)
}

// RefuseDecision answers an authority decision a SURFACE took itself — on a
// stored row it read before any tool ran — in the very envelope [Fail] answers
// the tools' own with: `401` for nobody, `503` for a caller or a chart this
// node could not read, `403 unauthorized` (or `step_up_required`) for a
// refusal. Like a tool's, it carries the rule's reason and the grants that
// would have admitted the caller, and no sentence of the engine's verbs: the
// code's own sentence is the one a person reads (ADR-0060).
func RefuseDecision(w http.ResponseWriter, r *http.Request, action authz.Action,
	d authz.Decision) {

	refused := httpjson.Detail{KeyAction: string(action)}
	switch _, how := iam.From(r.Context()); {
	case how == iam.Anonymous:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
	case how == iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable,
			auth.RetryIdentity(iam.Reason(r.Context())))
	case d.Unknown():
		log.InfoContext(r.Context(), "operator_decision_undecided",
			"action", string(action))
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			authz.RetryUndecidedSeconds, refused)
	default:
		refuseDecision(w, d, refused)
	}
}

// KeyAction names the verb a surface's own authority decision was about, on
// [RefuseDecision]'s answer: the value a sentence naming the verb used to
// carry.
const KeyAction = "action"

// UnknownOutcome is the 503 of a write this node cannot account for, carrying
// `outcome: "unknown"`, the request's operation key and whether ANOTHER node
// could say more — through [httpjson.UnknownOutcome], so it is told from a
// refusal that wrote nothing by a field rather than by a sentence.
//
// # Two unknowns, and they send a client opposite ways
//
// A LOST ACKNOWLEDGEMENT is settled by the same request again under the same
// key, HERE: the ledger answers what landed, and it lands once if it did not —
// so the answer carries the Retry-After this node's own undecided hint gives.
// An UNVOUCHED one ([statelog.Result.Unvouched]) was not published at all:
// this node's operation ledger may have lost the row the operation needs, so
// the same request asked here answers the same way until the write reaches
// this node — and the answer says so by carrying NO Retry-After and
// `unvouched`, and by sending the client to another node, or to read whether
// it landed. Either way the key is the one to keep: a fresh one is a second
// operation, and if the first landed it is a second write.
//
// about is the write's own receipt or the fields naming what it was about,
// carried beneath this answer's own fields, because a 503 that dropped it left
// a client holding a key and nothing saying which object to read to see
// whether it landed. The tool's own sentence about the write is NOT carried:
// it is written for a model (ADR-0060), and the key, `unvouched` and the
// object are what a client acts on.
func UnknownOutcome(w http.ResponseWriter, key string, unvouched bool,
	about map[string]any) {

	body := httpjson.Detail{}
	maps.Copy(body, about)
	body["detail"] = opkey.UnknownDetail(unvouched)
	// THE WRITER OWNS `outcome`, `op_id` and `unvouched`, over whatever the
	// receipt carried — a two-record tool's receipt has no `outcome` of its
	// own, only its halves' — and drops the Retry-After of an unvouched one.
	httpjson.UnknownOutcome(w, authz.RetryUndecidedSeconds, key, unvouched, body)
}

// statusClientClosedRequest is the status recorded for a request whose caller
// hung up before the tool answered. Nobody reads it — the connection is gone —
// but the access log does, and a 5xx there would count a closed tab as this
// node failing.
const statusClientClosedRequest = 499

// Interrupted answers a call whose context ended before the tool did. NEVER A
// REFUSAL: nothing about the request was wrong, and whether its write landed
// is not known — so the answer is the unknown outcome under the request's key,
// which names the same operations as whatever did land. A caller that hung up
// has nobody to answer; the status is for the access log.
func Interrupted(w http.ResponseWriter, r *http.Request, key, tool string, err error) {
	if r.Context().Err() != nil {
		log.InfoContext(r.Context(), "operator_call_abandoned", "tool", tool,
			"op_id", key, "error", err.Error())
		w.WriteHeader(statusClientClosedRequest)
		return
	}
	log.WarnContext(r.Context(), "operator_call_interrupted", "tool", tool,
		"op_id", key, "error", err.Error())
	UnknownOutcome(w, key, false, map[string]any{"tool": tool})
}

// ClassifyWrite states the refusal class of an error a WRITER returned to a
// surface that called it without a tool — the human write surface's purge,
// rename, trash, restore, comment take-down and remark edit — so [Fail]
// answers it exactly as it answers the same refusal from a tool.
//
// A CONTENT REFUSAL IS MARKED AND EVERYTHING UNMARKED IS THE NODE'S, the rule
// the tools' own classification follows: the tracker and the knowledge base
// wrap their sentinel on every refusal they write about what was asked, and
// what arrives unmarked — a SQL read, the log's last message, a ledger — is
// this node's failure. Read the other way, a write that hit a disk error would
// tell a person to fix their input. And "the node's" is the tools' TWO answers
// ([builtin.Condition]): a condition waiting clears is `unavailable`, and a
// fault of the node's own is `internal_error`.
//
// A WRITE THAT MAY HAVE LANDED IS NEITHER, and is read by the tools' own rule
// for that too ([builtin.UnknownCause]): a walk that stopped at a step nobody
// can confirm, and a write the estate's router sent to a data node that never
// answered, become an UNKNOWN outcome — which [Fail] answers with
// `outcome: "unknown"` under the request's key, and the audit records as
// `unknown` rather than as a refusal that wrote nothing.
func ClassifyWrite(err error) error {
	if err == nil || classOf(err) != "" {
		return err
	}
	// ASKED OF THE RESULT rather than by comparing it with err: an error
	// is an interface, and comparing two whose dynamic type is a struct
	// holding a slice panics.
	if cause := builtin.UnknownCause("", err); errors.Is(cause, crewletmcp.ErrOutcomeUnknown) {
		return cause
	}
	return crewletmcp.Classify(writeClass(err), err)
}

// FailWrite answers the error a WRITER returned to a surface that called it
// without a tool: classified by [ClassifyWrite] and answered by [Fail], in the
// person's words the writer put beside its error ([caveat.RefusalOf]) or in
// none, so the code's own sentence stands alone. A writer's error text is
// never the detail: a domain refusal's is written for a model, a condition's
// names the node's internals, and a store's is a driver's message or a
// database path. A FAULT's error is logged here, the one place it is read.
func FailWrite(ctx context.Context, w http.ResponseWriter, err error, key string,
	about httpjson.Detail) {

	cause := ClassifyWrite(err)
	if classOf(cause) == crewletmcp.RefusalInternalError {
		log.ErrorContext(ctx, "operator_write_fault", "error", err.Error())
	}
	Fail(w, cause, caveat.RefusalOf(err), key, about)
}

// KeyFields names the arguments a call carried that its tool does not read, on
// its `400 invalid_body`: values a client fixes its request by.
const KeyFields = "fields"

// ReusedKey is what a person is told of a write refused because its operation
// key already names a different change: what the request was about moved
// since it was first sent.
const ReusedKey = "What this change was about moved since it was first sent, " +
	"so nothing was written. Look again, then make the change."

// writeClass is the class a writer's error is refused under: the class its
// domain decided ([builtin.WriterClass] — the one table the tools' own
// failures are classed by, every native domain's, so a domain added beside the
// tracker and the knowledge base is never a 500 for a content refusal here
// while its tools class it), a condition's, or a fault.
func writeClass(err error) crewletmcp.Refusal {
	if class, ok := builtin.WriterClass(err); ok {
		return class
	}
	if _, ok := builtin.Condition(err); ok {
		return crewletmcp.RefusalUnavailable
	}
	return crewletmcp.RefusalInternalError
}

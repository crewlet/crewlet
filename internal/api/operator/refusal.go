package operator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
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
// refusal an API. The sentence is still the `detail`, because it is the only
// thing that says what to do.
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
func About(tool string, args map[string]any) httpjson.Detail {
	about := httpjson.Detail{"tool": tool}
	for _, field := range []string{"item", "page", "project", "handle"} {
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
	crewletmcp.RefusalPeerUpgrading:      {0, httpjson.CodePeerUpgrading},
	crewletmcp.RefusalInternalError:      {http.StatusInternalServerError, httpjson.CodeInternalError},
}

// peerUpgradingRetry is the wait a `peer_upgrading` refusal tells a caller to
// come back after: the interval a node's presence lease — which is where the
// fleet reads what each node's build carries out — is renewed at. An upgraded
// node announces its features on its first beat, so asking sooner reads the
// same leases and gets the same answer.
const peerUpgradingRetry = seat.HeartbeatInterval

// Fail writes the answer to one call that was NOT made: cause is the failed
// result's cause (or a tool-less writer's error, through [ClassifyWrite]),
// sentence the words a person reads as `detail`, key the operation the
// request named — carried on a 503 and a reused key's 409, so the retry it
// invites is the same operation — and about the fields that name what the
// call was about (`tool`, an item's key), written beneath the envelope's own.
func Fail(w http.ResponseWriter, cause error, sentence, key string, about httpjson.Detail) {
	detail := httpjson.Detail{}
	for k, v := range about {
		detail[k] = v
	}
	detail["detail"] = sentence
	var unknown *builtin.UnknownOutcome
	var refused *builtin.DecisionRefused
	switch {
	case errors.As(cause, &unknown):
		// NOT A REFUSAL AND NOT A FAILURE: a write whose outcome nobody
		// can establish, and it may well have landed — so a client told
		// "nothing was written" would make it a second time. FIRST,
		// because the cause it wraps may be anything the step answered.
		UnknownOutcome(w, key, unknown.Unvouched, sentence, about)
	case errors.Is(cause, crewletmcp.ErrOutcomeUnknown):
		// The sentinel without its facts: nothing says another node could
		// do better, so it is the ordinary unknown.
		UnknownOutcome(w, key, false, sentence, about)
	case refusedFor(cause, statelog.ReasonOpReused):
		// AN OPERATION THIS REQUEST DERIVES ALREADY NAMES A WRITE TO
		// ANOTHER OBJECT — what the request names moved since the key
		// was first sent — and nothing was written. A conflict the
		// caller resolves with a new key, never by waiting.
		detail["field"] = opkey.Header
		detail["op_id"] = key
		detail["detail"] = sentence + " — under this " + opkey.Header + " the " +
			"write already landed on something else, since what this request " +
			"names has changed; read it again and send this one under a new key"
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
		// dropped.
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody, detail)
	case errors.Is(cause, builtin.ErrNoAuthorizer):
		// A WIRING THAT DECIDED NOTHING, which no retry clears.
		log.Error("operator_no_authorizer", "detail", sentence)
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
		// have admitted the caller, beside the sentence the tool's gate
		// worded — so "you may not" reads the same in a turn, in an
		// assistant and here, and a screen can say what would change it.
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
// Retry-After, carrying the tool's sentence — which a fault's is written to
// keep free of the error, whose words are already in the log ([FailWrite]
// words a writer's the same way).
//
// A FAILURE WITH NO CLASS this build knows is a first-party tool that forgot to
// classify (`builtin`'s own suite walks for that) or a writer error nothing
// classified: it is answered as an opaque internal error with the sentence in
// the log, because guessing a class for it would be a status nobody chose —
// and with no sentence at all, because nothing vouches that it was written to
// leave the error out.
func failClass(w http.ResponseWriter, cause error, key string, detail,
	about httpjson.Detail) {

	class := classOf(cause)
	answer, known := classAnswers[class]
	switch {
	case !known:
		log.Error("operator_refusal_unclassified", "refusal", string(class),
			"detail", detail["detail"])
		httpjson.FailWithFields(w, http.StatusInternalServerError,
			httpjson.CodeInternalError, about)
	case answer.status == 0:
		// WHETHER AND WHEN TO COME BACK is the cause's to say: a refusal
		// waiting cannot clear — an evicted node, a full log, a record
		// this node cannot decode — carries no Retry-After, one that
		// derived its own hint carries that, and anything else this
		// node's undecided hint ([statelog.RetryAfter]'s rule, which
		// every surface answering a refusal reads). The fleet's upgrade
		// is read off presence leases, so it is asked again after one.
		otherwise := time.Duration(authz.RetryUndecidedSeconds) * time.Second
		if class == crewletmcp.RefusalPeerUpgrading {
			otherwise = peerUpgradingRetry
		}
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
// step_up_required` naming the window for a stale proof, and `403
// unauthorized` with the rule's reason and grants otherwise — each beside
// whatever else detail carries (the sentence a tool's gate worded it with).
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
	httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized, detail)
}

// RefuseDecision answers an authority decision a SURFACE took itself — on a
// stored row it read before any tool ran — in the very envelope [Fail] answers
// the tools' own with: `401` for nobody, `503` for a caller or a chart this
// node could not read, `403 unauthorized` (or `step_up_required`) for a
// refusal. The sentence is worded by [builtin.Refusal] over
// [builtin.DecisionError] — the tools' own two functions — so a refusal a route
// takes and one a tool takes read the same.
func RefuseDecision(w http.ResponseWriter, r *http.Request, action authz.Action,
	d authz.Decision) {

	switch _, how := iam.From(r.Context()); {
	case how == iam.Anonymous:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
	case how == iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable,
			auth.RetryIdentity(iam.Reason(r.Context())))
	case d.Unknown():
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable,
			authz.RetryUndecidedSeconds, httpjson.Detail{
				"detail": builtin.Refusal(string(action),
					builtin.DecisionError(action, d)),
			})
	default:
		refuseDecision(w, d, httpjson.Detail{"detail": builtin.Refusal(
			string(action), builtin.DecisionError(action, d))})
	}
}

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
// sentence is the tool's own sentence about THIS write — what may have landed,
// and what shows whether it did — carried as `tool_detail` beside this
// surface's own, or empty where there is none. about is the write's own
// receipt or the fields naming what it was about, carried beneath this
// answer's own fields, because a 503 that dropped it left a client holding a
// key and nothing saying which object to read to see whether it landed.
func UnknownOutcome(w http.ResponseWriter, key string, unvouched bool, sentence string,
	about map[string]any) {

	body := httpjson.Detail{}
	for k, v := range about {
		body[k] = v
	}
	body["detail"] = opkey.UnknownDetail(unvouched)
	if sentence != "" {
		body["tool_detail"] = sentence
	}
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
	UnknownOutcome(w, key, false, "the call was interrupted before "+tool+
		" answered, so whether it landed is unknown", map[string]any{"tool": tool})
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
func ClassifyWrite(err error) error {
	if err == nil || classOf(err) != "" {
		return err
	}
	return crewletmcp.Classify(writeClass(err), err)
}

// FailWrite answers the error a WRITER returned to a surface that called it
// without a tool: classified by [ClassifyWrite] and answered by [Fail], under
// a sentence a person may read — the domain's own refusal as it composed it,
// a condition in [builtin.Condition]'s words, and a FAULT in fixed words with
// its error in this node's log and nowhere else, because a store's error is a
// driver's message or a database path. The surface used to hand [Fail] the
// error's own text whatever it was, so a disk error's path reached the
// client's `detail`.
func FailWrite(ctx context.Context, w http.ResponseWriter, err error, key string,
	about httpjson.Detail) {

	cause := ClassifyWrite(err)
	Fail(w, cause, writeSentence(ctx, cause, err), key, about)
}

// writeFault is the sentence a writer's fault is answered with: whose it is,
// that sending it again does not clear it, and where its reason is.
const writeFault = "the write failed on a fault in this node itself — not " +
	"anything about this request, and not something sending it again clears; " +
	"the node's log has the details"

// writeSentence is the words [FailWrite] answers a writer's error with.
func writeSentence(ctx context.Context, cause, err error) string {
	if classOf(cause) == crewletmcp.RefusalInternalError {
		log.ErrorContext(ctx, "operator_write_fault", "error", err.Error())
		return writeFault
	}
	if why, ok := builtin.Condition(err); ok {
		return why
	}
	return err.Error()
}

// writeClass is the class a writer's error is refused under.
func writeClass(err error) crewletmcp.Refusal {
	switch {
	case errors.Is(err, tracker.ErrNoTask), errors.Is(err, tracker.ErrNoComment),
		errors.Is(err, tracker.ErrNoProject), errors.Is(err, pages.ErrNotFound):
		return crewletmcp.RefusalNotFound
	case errors.Is(err, tracker.ErrStaleVersion), errors.Is(err, pages.ErrStaleVersion):
		return crewletmcp.RefusalStaleVersion
	case errors.Is(err, pages.ErrTitleTaken), errors.Is(err, statelog.ErrExists):
		return crewletmcp.RefusalExists
	case errors.Is(err, pages.ErrConflict), errors.Is(err, statelog.ErrConflict):
		return crewletmcp.RefusalConflict
	case errors.Is(err, tracker.ErrForbidden), errors.Is(err, tracker.ErrNotAuthor),
		errors.Is(err, pages.ErrReserved):
		return crewletmcp.RefusalForbidden
	case errors.Is(err, tracker.ErrInvalid), errors.Is(err, pages.ErrInvalid):
		return crewletmcp.RefusalInvalid
	case errors.Is(err, tracker.ErrStepUnresolved):
		// A WALK THAT STOPPED AT A STEP NOBODY CAN CONFIRM, which the
		// same operation finishes: the tracker composed what to do, and
		// it is a state of the node rather than a fault of it.
		return crewletmcp.RefusalUnavailable
	}
	if _, ok := builtin.Condition(err); ok {
		return crewletmcp.RefusalUnavailable
	}
	return crewletmcp.RefusalInternalError
}

// Package httpjson is how every JSON surface in this engine answers.
//
// It exists because there were four byte-identical copies of the same response
// writer and three of the same body reader, and they had already drifted where
// it shows: one 413 said `body_too_large`, another `value_too_large`, and a
// third answered `payload too large` as plain text. A client cannot branch on
// a vocabulary that depends on which route it hit.
//
// The other half is subtler and was wrong at five call sites. [net/http.Error]
// sets `Content-Type: text/plain` and `X-Content-Type-Options: nosniff`, so
// handing it a JSON literal produces the one combination guaranteed to stop a
// strict client parsing it — the body says it is JSON, the headers swear it is
// not, and the sniffing that would otherwise paper over it is explicitly
// disabled. Every error here goes out as real JSON with the right type.
//
// # The refusal envelope
//
// Every refusal, from every surface, is ONE object with THREE parts:
//
//   - `error` — a [Code] from the table below. The machine-readable half:
//     clients branch on it, and only on it.
//   - `message` — ONE SENTENCE A PERSON READS. The dashboard renders it
//     verbatim, so each one is product copy rather than a log line, and it
//     belongs to the CODE rather than to the call site: the same refusal reads
//     the same wherever it came from, and a route with more to say says it in
//     the detail rather than rewording the sentence.
//   - the [Detail] — the machine-readable facts about THIS refusal, as typed
//     JSON: `{"missing_grant": "secrets.reveal"}`, `{"retry_after_ms": 4000}`,
//     `{"relation": "leads", "subject": "sarah-chen"}`.
//
// `error` and `message` are RESERVED KEYS and always win, so a client that
// branches on the code can never find it displaced by a route's own field of
// the same name. Everything else in the object is the detail, written BESIDE
// them rather than nested under a `detail` key: one object with two reserved
// words has exactly one place any given fact lives, where a nested container
// offers two and leaves every client to look in both.
package httpjson

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"time"

	"encoding/json"
)

// Code is the machine-readable `error` value a failed request carries.
//
// A NAMED TYPE over a closed set, so a route cannot invent a fifth spelling of
// "too large" the way three of them already had. Clients branch on these; the
// sentence a person reads is [Code.Message], and the reason an operator needs
// belongs in the log.
type Code string

// The codes every JSON surface answers with. snake_case throughout, which is
// what the dashboard and the websocket protocol already read.
//
// THE TABLE IS [codes], and these constants name its keys. A code that is not
// there is not in the vocabulary: [Code.Valid] is membership in it and
// [Code.Message] is what it carries, so a code cannot join without the
// sentence a person is shown.
const (
	// CodeEncodeFailed is the one this package can produce on its own: the
	// handler's own body would not marshal.
	CodeEncodeFailed Code = "encode_failed"

	// CodeBodyTooLarge is the single spelling of a 413, whether what
	// overflowed was a config document, a secret value or a webhook
	// delivery.
	CodeBodyTooLarge Code = "body_too_large"

	// CodeUnreadableBody is a body that could not be read to the end —
	// a client that hung up mid-request, not one that sent too much.
	CodeUnreadableBody Code = "unreadable_body"

	// CodeInvalidBody is a body that was read but is not what the route
	// accepts.
	CodeInvalidBody Code = "invalid_body"

	// CodeInvalidQuery is a query parameter that is not one of the values
	// the route accepts. Its own code rather than invalid_body, because the
	// body may be perfectly good, and a client told the body is wrong
	// changes the one thing that was right.
	CodeInvalidQuery Code = "invalid_query"

	// CodeInternalError is the deliberately opaque answer to a failure the
	// caller can do nothing about. The detail goes to the log.
	CodeInternalError Code = "internal_error"

	// CodeDraining is a request that would start work on a node that has
	// begun to drain. The request was fine and nothing was done with it; it
	// belongs on another node, or on this one once it has restarted.
	CodeDraining Code = "draining"

	// CodeInvalidToken is a credential that is missing, or present and not
	// one this engine accepts. ONE code for both, because telling them
	// apart in the answer tells an unauthenticated caller which half of the
	// guess was right.
	CodeInvalidToken Code = "invalid_token"
)

// The query-answer set: the codes the `/ws/stream` socket answers a question
// with, and the ones its REST twin answers the same question with.
//
// They are HERE rather than in the socket's own package so the two surfaces
// cannot call one refusal two things — a dashboard that asks over the socket
// and re-asks over HTTP would otherwise branch twice on one failure. The
// socket names these values in its own constants and a walk holds the two
// together; see internal/api/stream.
const (
	// CodeUnknownQuery is a question this build does not serve at all.
	// Waiting never changes that answer, which is what separates it from
	// CodeUnavailable.
	CodeUnknownQuery Code = "unknown_query"

	// CodeUnauthorized is a question that needs an operator credential the
	// caller did not present. DISTINCT from CodeInvalidToken, which refuses
	// the connection itself: this one is a socket that is legitimately open
	// for anonymous reads being asked an operator's question.
	CodeUnauthorized Code = "unauthorized"

	// CodeQueryFailed is a question this node understood and could not
	// answer. The reason reaches the log, never the caller: a failure here
	// can carry a database path.
	CodeQueryFailed Code = "query_failed"

	// CodeNotFound is a question about a record this node does not hold.
	// DISTINCT FROM CodeQueryFailed, because a client acts on them
	// differently: "no such item" is a dead link to show the person, and
	// "the query failed" is a retry.
	CodeNotFound Code = "not_found"

	// CodeBadParams is a question this node understood and REFUSED: a
	// parameter missing, malformed, or outside the set the field accepts.
	// The fault is the caller's, so it never succeeds however many times it
	// is sent — which is the opposite of what a client does with
	// CodeQueryFailed.
	CodeBadParams Code = "bad_params"

	// CodeUnavailable is a question this node understood and cannot answer
	// YET: a projection still catching up, or a coordination store that
	// could not be reached. It is the code that must never be flattened
	// into an empty result — "this company has no work" is an answer a
	// person acts on.
	CodeUnavailable Code = "unavailable"

	// CodeIdentityUnavailable is a request this node could not decide WHO
	// made, because the identity estate could not be read.
	//
	// DISTINCT FROM CodeInvalidToken, AND THE DISTINCTION IS THE WHOLE
	// POINT. That one is a refusal about the CALLER — this node checked
	// and the credential is not one it accepts — and the only sensible
	// response is to present a different one. This is a refusal about the
	// NODE: the credential may be perfect and nothing here could tell.
	// Answered as a 401 it would send everybody in the company to reset a
	// working password for as long as the outage lasted, which is the
	// three-valued rule internal/coord states, applied to authentication.
	CodeIdentityUnavailable Code = "identity_unavailable"

	// CodeCSRFOrigin is a state-changing request a cross-site page could
	// have caused: an `Origin` naming somewhere this deployment is not
	// reached at, or a cookie-authenticated request carrying none.
	//
	// ITS OWN CODE rather than a 403 sharing the authorization one,
	// because the two send a reader to opposite places: a missing grant
	// is somebody asking for more than they hold, and this is a request
	// that may be exactly what its holder is allowed to do and did not
	// ask for.
	CodeCSRFOrigin Code = "csrf_origin"
)

// The setup-pass set: refusals from running a third-party app's provisioning
// pass (`POST /setup/integrations/{kind}/provision` and its `check` twin).
//
// They were declared in the route file, which is where a fifth spelling of
// "too large" comes from: a code nobody can see from here is a code the next
// route invents again.
const (
	// CodePassInFlight is a second pass asked for while one is running.
	// The fleet lease is what refuses it, so this is a fact about the
	// company rather than about this node.
	CodePassInFlight Code = "pass_in_flight"

	// CodeNotProvisionable is an integration this build runs no
	// provisioning pass for. Its setup is the values on the surface, plus
	// whatever is done at the app itself.
	CodeNotProvisionable Code = "not_provisionable"

	// CodeNoExternalURL is a WRITING pass with no address to register a
	// webhook against. Refused by name rather than run to register nothing
	// and report success.
	//
	// It names the Tier A setting rather than the retired Tier B one, and
	// the two differ in what an operator has to do: the old field was
	// edited live through the dashboard, this one is a file on the node
	// and a restart. A code that still said `public_base_url` would send
	// somebody to a key the loader now refuses by name.
	CodeNoExternalURL Code = "no_external_url"

	// CodeRequirementsOutstanding is a pass asked for against a
	// half-configured integration: a pass writes at the app, so it is
	// refused with the missing fields rather than run half way.
	CodeRequirementsOutstanding Code = "requirements_outstanding"

	// CodeRunNotFound is a pass run this node does not hold. A run is
	// remembered by the node that executed it and only for the last few
	// passes, so this is as much "not here" as "not any more".
	CodeRunNotFound Code = "run_not_found"

	// CodeVendorRefused is a pass the third-party app refused. Nothing it
	// had already done is undone, so re-running is safe.
	CodeVendorRefused Code = "vendor_refused"
)

// THE IDENTITY CODES, and why there are so few of them.
//
// A sign-in surface's refusals are the one place in this vocabulary where
// SAYING LESS IS THE FEATURE. Every arm of a failed sign-in — no such login,
// wrong password, wrong second factor, a person suspended, a person removed —
// is one code, because a caller that could tell them apart has a roster and a
// way to test it. The specific ones below are the arms where being specific
// discloses nothing a stranger did not already know, or where a person is
// stuck without the detail.
const (
	// CodeSignInRefused is EVERY failed sign-in, whatever went wrong.
	//
	// ONE CODE FOR ALL OF THEM, deliberately. It is paired with the
	// timing defence in internal/iam/credential — both arms padded to one
	// wall-clock deadline measured from arrival — because a code that
	// distinguished them would make the pad pointless, and a pad with a
	// distinguishing code would make the code pointless. Neither half
	// works alone.
	CodeSignInRefused Code = "sign_in_refused"

	// CodeThrottled is too many failed attempts from one source.
	//
	// THE ONE SPECIFIC REFUSAL ON THIS SURFACE, and it is safe precisely
	// because it is keyed on the SOURCE rather than on the subject: a
	// stranger learns they have been rate-limited, which they already
	// knew. Keyed on a login it would be an oracle — "this account
	// exists and I can lock it".
	CodeThrottled Code = "throttled"

	// CodeSecondFactorRequired is a first factor that checked out where a
	// second is still needed.
	//
	// SPECIFIC BECAUSE THE PERSON IS ALREADY AUTHENTICATED by the first
	// factor, so it discloses nothing to a stranger — and because without
	// it a client cannot tell "your password is wrong" from "now type
	// your code", which are different screens.
	CodeSecondFactorRequired Code = "second_factor_required"

	// CodeStepUpRequired is a session that is valid and has not proved
	// identity recently enough for what it just asked to do.
	CodeStepUpRequired Code = "step_up_required"

	// CodeSessionRevoked is a bearer this node KNOWS is over: signed out,
	// revoked, expired, or ended by reuse detection.
	//
	// DISTINCT FROM [CodeInvalidToken], because a client acts on them
	// differently: this one means discard the cookie and sign in again,
	// and that one means the credential presented was never valid here.
	// Folded together a browser would discard a cookie on every malformed
	// Authorization header it sent.
	CodeSessionRevoked Code = "session_revoked"

	// CodeBootstrapClosed is the one-time founder route asked for on a
	// deployment where it may not run: somebody is already enrolled, or
	// `api.auth.bootstrap` is closed.
	CodeBootstrapClosed Code = "bootstrap_closed"

	// CodeInviteSpent is an invitation that is redeemed, withdrawn or
	// expired. SPECIFIC because the holder of the link needs to know to
	// ask for another one, and because holding the link is already
	// evidence it was issued to them.
	CodeInviteSpent Code = "invite_spent"

	// CodeSeatUnavailable is somebody whose session validated perfectly
	// and whose SEAT the org chart no longer holds.
	//
	// A 403 AND NEVER A 401, which is why it is not folded into
	// [CodeSessionRevoked]: the bearer is live and signing in again
	// changes nothing, so a browser told to discard its cookie would
	// loop through the sign-in page for ever. The detail beside it NAMES
	// the seat, because the person locked out and whoever removed it both
	// need to know which one.
	//
	// The wire value is internal/iam/session's own `CodeNoSeat`; the two
	// are held equal by a test in internal/api/auth, since this package
	// is a leaf and cannot import that one to share the constant.
	CodeSeatUnavailable Code = "seat_unavailable"
)

// codes is THE TABLE: every code this engine answers with, each with the one
// sentence a person is shown for it.
//
// ONE MAP RATHER THAN A SET AND A SWITCH, because the two halves are the same
// decision: admitting a code to the vocabulary IS writing the copy for it. A
// switch of valid codes beside a map of messages is two lists to keep in step,
// and the one that rots is the one nothing renders in a test.
//
// Every value is PRODUCT COPY — a sentence a person reads on a screen, saying
// what happened and what to do about it. The dashboard renders it verbatim
// (see docs/reference/dashboard-design.md), so a log line here is a log line
// shown to an operator in a toast.
var codes = map[Code]string{
	CodeEncodeFailed: "Something went wrong while building this answer. " +
		"The engine's log has the reason.",
	CodeBodyTooLarge: "The request body is larger than this endpoint accepts. " +
		"Send a smaller document, or split the change across more than one request.",
	CodeUnreadableBody: "The request body did not arrive in full. Send it again.",
	CodeInvalidBody:    "The request body is not in the shape this endpoint accepts.",
	CodeInvalidQuery:   "One of the query parameters is not a value this endpoint accepts.",
	CodeInternalError: "Something went wrong inside the engine. The reason is " +
		"in this node's log.",
	CodeDraining: "This node is shutting down and is not taking new work. " +
		"Try another node, or this one once it has restarted.",
	CodeInvalidToken: "This request needs an operator token, and none this " +
		"engine accepts was presented.",

	CodeUnknownQuery: "This node does not serve that query.",
	CodeUnauthorized: "That query needs an operator token.",
	CodeQueryFailed: "That query could not be answered. The reason is in " +
		"this node's log.",
	CodeNotFound:  "There is no such record here.",
	CodeBadParams: "That query was asked with a parameter this endpoint does not accept.",
	CodeUnavailable: "This node cannot answer that yet — something it reads " +
		"is still catching up. Ask again in a moment.",
	CodeCSRFOrigin: "That request came from a page this deployment does not " +
		"serve, so it was refused without being carried out.",
	CodeIdentityUnavailable: "This node cannot tell who you are at the moment — " +
		"the identity estate could not be read. Your credential is probably " +
		"fine; try again shortly.",

	CodePassInFlight: "A setup pass for this integration is already running. " +
		"Wait for it to finish rather than starting a second one.",
	CodeNotProvisionable: "This integration is not set up from here. Connect " +
		"it at the third-party app itself, or with the crewlet command line.",
	CodeNoExternalURL: "This deployment has no external address, so no webhook " +
		"can be registered for it. Set the external URL in this node's own " +
		"configuration file, restart it, and run the pass again.",
	CodeRequirementsOutstanding: "This integration is still missing values it " +
		"needs. Fill them in before running a pass.",
	CodeRunNotFound: "That setup run is not held here. A run is remembered by " +
		"the node that executed it, and only for the last few passes.",
	CodeVendorRefused: "The third-party app refused this pass. Nothing it had " +
		"already done is undone, so running it again is safe.",

	// THE COPY IS AS UNIFORM AS THE CODE. A message that said "no such
	// user" for one arm and "wrong password" for another would be the
	// oracle the single code exists to close, written out in the body.
	CodeSignInRefused: "Those sign-in details were not accepted. Check them " +
		"and try again.",
	CodeThrottled: "There have been too many failed sign-in attempts from " +
		"here. Wait a little and try again.",
	CodeSecondFactorRequired: "Enter the code from your authenticator app, or " +
		"one of your recovery codes.",
	CodeStepUpRequired: "This action needs you to confirm who you are. Sign " +
		"in again to continue.",
	CodeSessionRevoked: "This session has ended. Sign in again.",
	CodeBootstrapClosed: "The first-operator setup is not available on this " +
		"deployment. Ask somebody who already has an account to invite you.",
	CodeSeatUnavailable: "The seat you are bound to is no longer in this " +
		"company's org chart, so there is nothing for you to act as. An " +
		"administrator can bind you to another one.",

	CodeInviteSpent: "This invitation is no longer valid. Ask whoever sent it " +
		"for a new one.",
}

// Valid reports whether c is in the vocabulary — which is to say, whether the
// table carries a sentence for it.
func (c Code) Valid() bool {
	_, ok := codes[c]
	return ok
}

// Message is the sentence a person is shown for c, or "" for a code the table
// does not carry.
//
// A code with no sentence still ANSWERS: `error` is what a client branches on,
// and refusing over missing copy would turn a refusal into a failure. What it
// loses is the half a person reads, so [FailWithFields] logs it — a route that
// spells its own code is a code that has not landed on this table yet, and
// that log line is where the next author finds out.
func (c Code) Message() string { return codes[c] }

// Detail is the machine-readable half of a refusal: the facts about THIS
// refusal a client branches on, beside the `error` code and the `message` a
// person reads.
//
// VALUES, NOT TEXT. `{"retry_after_ms": 4000}` is a number a client can wait
// on; "4000" is a string it has to parse first, and "retry in about four
// seconds" is a sentence it can only show. A refusal that carries a list of
// located problems, a derived hierarchy or a revision id keeps their shape
// here, so nothing has to read a message to find a field.
//
// The keys are the route's own vocabulary; `error` and `message` are this
// package's and always win over them.
type Detail map[string]any

// The two keys the envelope reserves. Named rather than spelled at each use,
// because "always wins" is only true if every writer means the same key.
const (
	keyError   = "error"
	keyMessage = "message"
)

// ErrTooLarge is what a body over a route's cap surfaces as.
//
// A sentinel rather than a *http.MaxBytesError so callers do not each have to
// know that detail of net/http, and so [Refuse] can tell the two failures
// apart without a second type assertion.
var ErrTooLarge = errors.New("httpjson: body over the limit")

// Write answers with status and body as JSON.
//
// The status is written BEFORE the body because it has to be: once any byte of
// the body is written the header is gone, and a WriteHeader after it is a
// silent no-op that leaves the route answering 200 for a failure.
func Write(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		// The handler's body is unmarshalable, so the only thing left to
		// send is this package's own. Written by hand rather than through
		// Marshal, which is what just failed — and quoted through
		// strconv rather than concatenated, so the envelope stays valid
		// JSON whatever punctuation a sentence in [codes] carries.
		slog.Error("http_encode_failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"` + keyError + `":` +
			strconv.Quote(string(CodeEncodeFailed)) + `,"` + keyMessage + `":` +
			strconv.Quote(CodeEncodeFailed.Message()) + `}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// Fail answers with status and the refusal envelope for code, carrying no
// detail: the code and its sentence are the whole of what this refusal knows.
func Fail(w http.ResponseWriter, status int, code Code) {
	FailWithFields(w, status, code, nil)
}

// FailWith is [Fail] with a detail whose every value is text — which most of
// them are: a field path, a hint, the name of the thing that was wrong.
//
// It widens the map rather than being a second writer, so the reserved keys
// and the sentence cannot hold for one form and not the other.
func FailWith(w http.ResponseWriter, status int, code Code, detail map[string]string) {
	fields := make(Detail, len(detail))
	for k, v := range detail {
		fields[k] = v
	}
	FailWithFields(w, status, code, fields)
}

// FailWithFields is THE refusal writer: the envelope's three parts, assembled
// once, whatever the surface.
//
// ONE implementation under every Fail, so the rules that `error` and `message`
// are always present and always win cannot hold for one of them and not the
// other. The caller's map is not written to.
func FailWithFields(w http.ResponseWriter, status int, code Code, detail Detail) {
	body := make(map[string]any, len(detail)+2)
	maps.Copy(body, detail)
	body[keyError] = string(code)
	if message := code.Message(); message != "" {
		body[keyMessage] = message
	} else {
		// A refusal a person is shown nothing for. Not a failure of this
		// request — the code still answers it — so it is logged rather
		// than raised, under a name whoever adds the missing entry can
		// grep for.
		slog.Debug("http_refusal_without_message", "code", string(code))
	}
	Write(w, status, body)
}

// BodyReadTimeout bounds how long a client may take to deliver its body.
//
// A size cap is not a time bound, and the two failures are different: the size
// cap stops a client sending 25 MiB, and this stops one sending 25 bytes a
// minute apart. Without it a request that dribbles holds a handler goroutine
// and a connection slot for as long as the client cares to keep dribbling —
// the cheapest denial there is against a listener, and the listener is the one
// surface an unauthenticated caller can reach.
//
// It is NOT the server's ReadTimeout, which is why that field is still unset:
// ReadTimeout covers the whole exchange from the first header byte, so any
// value large enough for a 25 MiB webhook on a slow link is also large enough
// to be no bound at all on a small one. A deadline taken HERE starts when the
// handler asks for the body, so it bounds the body alone.
//
// THIRTY SECONDS, from the largest thing this reads: webhooks.MaxBodyBytes is
// 25 MiB, which needs roughly 7 Mbit/s sustained to arrive inside the bound —
// far below what any CI runner, forge or operator workstation delivers, and
// far above the trickle this exists to cut off. The server's own
// ReadHeaderTimeout (10 s) and IdleTimeout (60 s) bound the other two phases;
// this is the third.
const BodyReadTimeout = 30 * time.Second

// ReadBody reads at most max bytes of a request body, within
// [BodyReadTimeout].
//
// It reads the WHOLE body even when the request will be refused. An HTTP
// server that answers without draining leaves unread bytes in the socket and
// the client sees a connection reset instead of the status it was sent — which
// for a 401 means "retry forever" rather than "your signature is wrong".
//
// Over the cap it returns [ErrTooLarge]; anything else is the read's own
// error — a blown deadline included, since a body that never arrived and one
// that was cut off are the same thing to a caller.
func ReadBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	// http.ErrNotSupported is IGNORED rather than reported: a
	// ResponseWriter that cannot carry a deadline is a recorder or a
	// wrapper, never a real connection, so there is nothing to bound and
	// nothing a caller could do about it. Failing the read there would
	// break every handler under httptest for a property the test has no
	// way to violate.
	if err := http.NewResponseController(w).
		SetReadDeadline(time.Now().Add(BodyReadTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		return nil, err
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err == nil {
		return raw, nil
	}
	var overflow *http.MaxBytesError
	if errors.As(err, &overflow) {
		return nil, ErrTooLarge
	}
	return nil, err
}

// Refuse answers a [ReadBody] failure with the status it deserves: 413 for a
// body over the cap, 400 for one that could not be read.
func Refuse(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrTooLarge) {
		Fail(w, http.StatusRequestEntityTooLarge, CodeBodyTooLarge)
		return
	}
	Fail(w, http.StatusBadRequest, CodeUnreadableBody)
}

// Unavailable writes a 503 carrying a Retry-After, which is the pair a client
// needs to tell "come back" from "do not come back".
//
// ONE WRITER, because the header and the envelope have to agree: a 503 with no
// Retry-After is indistinguishable to a client from a node that is down for
// good, and a Retry-After on a refusal that is not retryable teaches a client
// to hammer one that never will be. Seconds rather than a duration, because
// that is what the header carries and converting at each call site is how two
// of them come to round differently.
//
// It is NOT a second Fail: the body is [FailWithFields]'s, so the envelope's
// three parts are assembled in exactly one place however a refusal is reached.
func Unavailable(w http.ResponseWriter, code Code, retryAfterSeconds int) {
	if retryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	FailWithFields(w, http.StatusServiceUnavailable, code, nil)
}

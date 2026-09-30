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
//
// # A 503 says when, or that waiting will not help
//
// A 503 is written by [Unavailable] or [UnavailableWith] and by nothing else,
// because those are the only writers that set the `Retry-After` and the
// envelope together: a dozen 503s written through [FailWith] told a client to
// come back and never said when. Zero seconds writes NO header, and that is a
// setting rather than an omission — a node missing its keyring has none after
// any wait, and the header's absence is how a client learns not to hammer it.
// `internal/api`'s source walk holds the settings surfaces to this,
// to hand-built bodies, and to codes minted outside the table.
//
// # And the answers no handler writes
//
// The router's own 404 and 405, which [Mux] turns into this envelope, so that
// EVERY refusal the engine writes carries a code — which is what lets a client
// tell the node's own answer from a proxy's page in front of it.
package httpjson

import (
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
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
	// CodeNonCanonicalPath is a request path carrying a `.` or `..` segment
	// or an empty one. Refused rather than redirected to where it resolves,
	// because a path that reads one way to a person and matches another
	// way at the router is exactly what an authority gate must not be
	// asked to agree with — see internal/authz's CanonicalPath.
	CodeNonCanonicalPath Code = "non_canonical_path"

	// CodeInternalError is the deliberately opaque answer to a failure the
	// caller can do nothing about. The detail goes to the log.
	CodeInternalError Code = "internal_error"

	// CodeDraining is a request that would start work on a node that has
	// begun to drain. The request was fine and nothing was done with it; it
	// belongs on another node, or on this one once it has restarted.
	CodeDraining Code = "draining"

	// CodeNoRoute is a path nothing on this node serves — a route that is
	// not registered, a surface its build or configuration does not mount,
	// an asset that is not in the build. Answered by [Mux] for the mux's
	// own 404.
	//
	// NOT `not_found`, which the routes that DO exist already answer for a
	// record they do not hold — a secret, an event, a project. A client
	// branches on that one as "there is no such thing", and `crewlet
	// secrets get` answers it by reporting the secret absent: a node with no
	// /secrets surface at all would have been read as a store holding
	// nothing.
	CodeNoRoute Code = "no_route"

	// CodeMethodNotAllowed is a path this node serves under another method.
	// Answered by [Mux] for the mux's own 405, which also carries the
	// `Allow` header naming the methods it does take.
	CodeMethodNotAllowed Code = "method_not_allowed"

	// CodeInvalidToken is a credential that is missing, or present and not
	// one this engine accepts — a Tier A token, a session, a machine token.
	// ONE code for both, because telling them apart in the answer tells an
	// unauthenticated caller which half of the guess was right.
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

	// CodeUnauthorized is a request from a caller this node KNOWS, refused
	// because their credential does not carry the grant it needs — or, for
	// a personal record, the relation. DISTINCT from CodeInvalidToken, which
	// refuses a caller nobody could identify: the remedy there is a
	// credential and here it is authority, and a reader told the first
	// when they meant the second goes and replaces a credential that
	// works. It is a 403 over HTTP, and wherever the authority table made
	// the refusal the detail names the rule's `reason` and the `grants`
	// that would have admitted the caller.
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
	// HERE: a projection still catching up, a coordination store that
	// could not be reached, or a refusal by its state log — including the
	// ones no wait clears, a full log or a record this node cannot decode.
	// Whether waiting helps is the Retry-After's to say ([Unavailable]),
	// and its absence says it will not. It is the code that must never be
	// flattened into an empty result — "this company has no work" is an
	// answer a person acts on.
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

	// CodeNoExternalURL is a gesture that has to hand somebody this
	// deployment's own address — a writing pass registering a webhook, an
	// invitation link — on a node that has none. Refused by name rather than
	// run to register nothing, or to mint a link nobody can follow, and
	// report success.
	//
	// ITS MESSAGE NAMES NEITHER GESTURE, because both surfaces answer with
	// it and the message is what a person is shown: worded for the webhook
	// pass, it told somebody sending an invitation to "run the pass again".
	// What each surface needed the address for is its own `detail`.
	//
	// It names the Tier A setting rather than the retired Tier B one, and
	// the two differ in what an operator has to do: the old field was
	// edited live through the dashboard, this one is a file on the node
	// and a restart. A code that still named the company document's key
	// would send somebody to a key the loader now refuses by name.
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

	// THE REST OF `/setup`'s refusals, which were declared in the route
	// file with no sentence behind them — so a screen that rendered the
	// envelope showed the code and nothing a person could act on. Two
	// spellings that route file had invented for codes this table already
	// held are gone with them: `bad_body` is [CodeInvalidBody], and
	// `no_public_url` — the retired Tier B field's name — is
	// [CodeNoExternalURL].

	// CodeUnknownKind is an integration this build does not know.
	CodeUnknownKind Code = "unknown_kind"
	// CodeSeatRequired is a per-seat gesture that named no seat.
	CodeSeatRequired Code = "seat_required"
	// CodeNoSuchSeat is a seat the org chart does not hold.
	CodeNoSuchSeat Code = "no_such_seat"
	// CodeLiteralInConfig is a credential the company document carries as a
	// literal where a ${VAR} pointer belongs, which the setup surface will
	// not write over.
	CodeLiteralInConfig Code = "literal_in_config"
	// CodeInvalidInput is a value the integration refused: a field it does
	// not declare, a value outside its set, or one the vendor itself
	// refused when it was checked.
	CodeInvalidInput Code = "invalid_input"
	// CodeSurfaceBusy is a write refused because something else is writing
	// at this surface right now — a reconcile tick, or an operator's own
	// pass. The one TRANSIENT refusal on `/setup`, so it is a 503 with a
	// Retry-After and a client repeats it.
	CodeSurfaceBusy Code = "surface_busy"
)

// The configuration set: `/config`'s refusals, and the ones `/setup` shares
// with it because a connection is written onto the same document.
//
// Every one of them used to be a bare `{"error": code}` object written by the
// route, so `/config` — the surface the org builder's every save goes through
// — was the one whose refusals carried no `message`.
const (
	// CodeNoActiveRevision is a request that needs an active company
	// configuration on a node that has none yet.
	CodeNoActiveRevision Code = "no_active_revision"

	// CodeRevisionAdvanced is a conditional write that lost: another write
	// was activated after the one this was based on.
	CodeRevisionAdvanced Code = "revision_advanced"

	// CodeAlreadyConfigured is a create-only write (If-None-Match: *) that
	// found a configuration where it expected none.
	CodeAlreadyConfigured Code = "already_configured"

	// CodeUnreadableRevision is a revision sealed under a key this
	// deployment no longer holds.
	CodeUnreadableRevision Code = "unreadable_revision"

	// CodeUnsealedRevision is a revision stored WITHOUT a seal, which a
	// node reads nowhere. Its own code rather than [CodeUnreadableRevision]
	// because the remedies share nothing: that one is a key to put back,
	// and this one depends on which revision it is, which the hint names.
	CodeUnsealedRevision Code = "unsealed_revision"

	// CodeInvalidRevisionID is a revision route with no revision named.
	CodeInvalidRevisionID Code = "invalid_revision_id"

	// CodeAgainstNotFound is a diff whose BASE is not held here. Its own
	// code rather than [CodeNotFound], which names the target: a client
	// has to know which of the two ids to correct.
	CodeAgainstNotFound Code = "against_not_found"

	// CodeUnsupportedPatchMediaType is a PATCH in a format `/config` does
	// not serve. The detail carries the Accept-Patch it does.
	CodeUnsupportedPatchMediaType Code = "unsupported_patch_media_type"

	// CodeChartNotWritableHere is a settings write that carried the org
	// chart, which lives on its own routes now.
	CodeChartNotWritableHere Code = "chart_not_writable_here"

	// CodeNoSuchEntity is an entity route naming something the active
	// document does not hold.
	CodeNoSuchEntity Code = "no_such_entity"

	// CodeIdentityMismatch is an entity write whose body renames what its
	// path names.
	CodeIdentityMismatch Code = "identity_mismatch"

	// CodeValidationError is a write whose resulting document is invalid.
	CodeValidationError Code = "validation_error"

	// CodeInvalidPatch is a patch that could not be applied to the document.
	CodeInvalidPatch Code = "invalid_patch"

	// CodeSummaryRequired is a configuration write carrying no audit
	// summary.
	CodeSummaryRequired Code = "summary_required"
)

// The credential set: `/secrets`' refusals. There is no "no keyring" code:
// every node holds one, and the surface refuses to be built without it.
const (
	// CodeInvalidName is a name a credential cannot be stored under.
	CodeInvalidName Code = "invalid_name"

	// CodeKeyIDMismatch is a rekey whose caller expects a different active
	// key from the one this node seals under.
	CodeKeyIDMismatch Code = "key_id_mismatch"

	// CodeRekeyIncomplete is a rekey that moved some rows and stopped. The
	// detail names the ones that moved.
	CodeRekeyIncomplete Code = "rekey_incomplete"

	// CodeReservedName is a name in the ENGINE's own namespace — a person's
	// key, a blind-index key — which no caller of `/secrets` may address,
	// whatever it holds. The detail names the gesture that does.
	CodeReservedName Code = "reserved_name"
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
	// wall-clock deadline measured from admission — because a code that
	// distinguished them would make the pad pointless, and a pad with a
	// distinguishing code would make the code pointless. Neither half
	// works alone.
	CodeSignInRefused Code = "sign_in_refused"

	// CodeThrottled is an attempt the throttle's curve says must wait
	// longer than it will hold a request open for. It is a 429 carrying
	// the time left in `Retry-After`, and [Throttled] is its one writer.
	//
	// THE ONE SPECIFIC REFUSAL ON A SIGN-IN SURFACE, and it is safe
	// precisely because the curve a stranger can reach is keyed on the
	// SOURCE and on what was TYPED, never on what it resolved to: they learn
	// they have failed recently from where they are, which they already
	// knew, and a name nobody holds is throttled exactly as a real one is.
	// Keyed on a resolved login it would be an oracle — "this account
	// exists". The one curve that IS keyed on the resolved person, a second
	// factor's, is reached only past the password, so it tells its caller
	// nothing the password did not — and its sentence cannot say "from
	// here", because it counts every address.
	CodeThrottled Code = "throttled"

	// CodeSecondFactorRequired is a first factor that checked out where a
	// second is still needed.
	//
	// SPECIFIC BECAUSE THE PERSON IS ALREADY AUTHENTICATED by the first
	// factor, so it discloses nothing to a stranger — and because without
	// it a client cannot tell "your password is wrong" from "now type
	// your code", which are different screens.
	//
	// A SIGN-IN'S 401, AND ALSO THE ENROLMENT'S 403: a session that may
	// only enrol a second factor, whose person has come to hold one since
	// it opened, has proved only the first factor and may not enrol over
	// the second — the remedy there is the same, sign in with it.
	CodeSecondFactorRequired Code = "second_factor_required"

	// CodeStepUpRequired is a session that is valid and has not proved
	// identity recently enough for what it just asked to do.
	CodeStepUpRequired Code = "step_up_required"

	// CodeSecondFactorEnrolmentRequired is a session opened on a password
	// alone where the deployment requires a second factor its person does
	// not hold: it may enrol one (`POST /auth/totp`), read who it is,
	// re-confirm its password and sign out, and every other route answers
	// this, 403.
	//
	// ITS OWN CODE, not `step_up_required`: no fresher proof of the same
	// kind changes the answer — enrolling a factor does — and a client that
	// read it as a step-up would ask for the password in a loop.
	CodeSecondFactorEnrolmentRequired Code = "second_factor_enrolment_required"

	// CodeSessionRevoked is a bearer this node KNOWS is over: signed out,
	// revoked or expired.
	//
	// DISTINCT FROM [CodeInvalidToken], because a client acts on them
	// differently: this one means discard the cookie and sign in again,
	// and that one means the credential presented was never valid here.
	// Folded together a browser would discard a cookie on every malformed
	// Authorization header it sent.
	CodeSessionRevoked Code = "session_revoked"

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

	// CodeForbidden is a WRITE the human write surface refused: a caller
	// this node knows, whose grants or relations do not reach the verb.
	//
	// ITS OWN CODE rather than [CodeUnauthorized] because the detail is a
	// different shape: it carries the verb's own refusal SENTENCE, worded
	// once in the tools and read identically in a turn, in the operator's
	// assistant and here, where [CodeUnauthorized]'s detail is the table's
	// `reason` and `grants` as values.
	CodeForbidden Code = "forbidden"

	// CodeStale is a write that lost to somebody else's: a version that
	// moved since it was read, a title somebody else took, a race lost too
	// many times. The one refusal whose remedy is "read it again".
	CodeStale Code = "stale"

	// CodeRefused is a write the DOMAIN refused on its own rules — a status
	// that does not exist, a field its declaration does not allow, a label
	// the project never declared. The detail is the domain's own sentence,
	// because it is the only thing that says what to change.
	CodeRefused Code = "refused"

	// THE DEPLOYMENT'S OWN CONTROLS: a backup, a budget reset, the
	// retention gestures and the capacity window. Each spelled its codes
	// inline in a bare `{"error": …}` body with no `message`, so the one
	// surface an operator reaches for when something is wrong was the one
	// whose refusals a screen had nothing to show for. The wire values are
	// the ones those routes always answered with, which `crewlet backup`
	// and `crewlet retention` already branch on.

	// CodeNoDestination is a backup asked for with no directory to write.
	CodeNoDestination Code = "no_destination"
	// CodeBackupFailed is a backup that did not finish. 400 when the
	// destination the caller named is the problem, and the detail names it;
	// 500 otherwise, with the reason in the log alone.
	CodeBackupFailed Code = "backup_failed"
	// CodeBudgetUnreadable is a reset that could not read the counters it
	// was about to clear, so it cleared nothing.
	CodeBudgetUnreadable Code = "budget_unreadable"
	// CodeBudgetResetFailed is a reset the counters refused.
	CodeBudgetResetFailed Code = "budget_reset_failed"
	// CodePositionRequired is a backup acknowledgement that named no stream
	// or no sequence: it moves the floor the trim deletes against, so
	// neither has a default.
	CodePositionRequired Code = "position_required"
	// CodeUnknownStream is a retention or capacity gesture naming a stream
	// this node does not have. 404 rather than 503, because nothing about
	// it is transient.
	CodeUnknownStream Code = "unknown_stream"
	// CodeAckFailed is a backup acknowledgement that was not recorded.
	CodeAckFailed Code = "ack_failed"
	// CodeNoStateLog is an eviction or readmission sent to a node whose API
	// holds no gate to write it through: one that ran no state log when it
	// started serving, because it started with no company. 503 with no
	// Retry-After, because waiting does not hand this surface a gate — it is
	// given one once, as the node starts serving — and the detail says to ask
	// a node that runs the company. It was `no_tracker`, from when the
	// tracker's log was the only one a gate was written to.
	CodeNoStateLog Code = "no_state_log"
	// CodeConfirmRequired is a destructive gesture whose confirmation did
	// not repeat what it acts on. The detail says what to repeat.
	CodeConfirmRequired Code = "confirm_required"
	// CodeOpIDInvalid is a write sent under an operation id this engine
	// would not have minted ([statelog.CheckCallerOpID]): no mint instant,
	// or bytes the broker would carry as a different id — or, where a route
	// derives what it creates from the id, one carrying more than the bare
	// uuid7 it needs. 400, with nothing judged or written, because an id
	// carrying no instant reads as older than every loss the ledger has had
	// — a write answered `unknown` without being published, on the first
	// attempt as on every retry.
	//
	// ONE CODE WHEREVER THE ID ARRIVED — the node gate's `?op_id=` and
	// every surface's `Idempotency-Key` (`/work`, `/pages`, `/chart`,
	// `/iam`) — so a client branches on one spelling for one mistake. A
	// refusal of a header also carries `field`, naming it.
	CodeOpIDInvalid Code = "op_id_invalid"
	// CodeInvalidGate is an eviction or readmission naming a node id no
	// node could run under. 400: nothing was judged or written, and the
	// detail says what a node id may hold.
	CodeInvalidGate Code = "invalid_gate"
	// CodeEvictionRefused is an eviction of a node still holding a live
	// presence lease, so still reaching the fleet. 409 with nothing written
	// anywhere; the detail, `hint` and `actions` say what to do.
	CodeEvictionRefused Code = "eviction_refused"
	// CodeEvictionUnjudged is an eviction this node could not judge,
	// because the presence leases it is decided against could not be read.
	// 503 — the condition is this node's, not the target's — carrying
	// `actions`, one of which is forcing it past the leases.
	CodeEvictionUnjudged Code = "eviction_unjudged"
	// CodeReadmissionRefused is a readmission of a node below a trim floor
	// it would be counted against. 409 with nothing written, carrying the
	// numbers the refusal is about.
	CodeReadmissionRefused Code = "readmission_refused"
	// CodeGateFailed is an eviction or readmission that was not recorded.
	CodeGateFailed Code = "gate_failed"
	// CodeStreamRequired is a capacity or reanchor question that named no
	// stream.
	CodeStreamRequired Code = "stream_required"
	// CodeStreamUnreadable is a reanchor question about a log this node
	// runs whose broker did not answer the live read. 503 with a
	// Retry-After, because the stream name was right and asking again is
	// the remedy — told apart from [CodeUnknownStream] so an operator does
	// not go looking for a typo.
	CodeStreamUnreadable Code = "stream_unreadable"
	// CodeReanchorRefused is a reanchor the stream refused.
	CodeReanchorRefused Code = "reanchor_refused"
	// CodeTargetRequired is a capacity change that named no stream or no
	// byte ceiling.
	CodeTargetRequired Code = "target_required"
	// CodeCapacityRefused is a capacity change the window refused. The
	// detail carries the operation already open, when there is one, since
	// "never opened" and "open and stuck" have opposite next steps.
	CodeCapacityRefused Code = "capacity_refused"
	// CodeMaintenanceUnreadable is a capacity window whose state could not
	// be read.
	CodeMaintenanceUnreadable Code = "maintenance_unreadable"

	// CodeFleetMixedVersion is a whole-chart import refused while a rolling
	// upgrade is in progress: every node applies the import, the older one
	// included, under its own reading of what a placement means. 409 rather
	// than 503, because the remedy is finishing the upgrade and not waiting.
	// The detail names the node still on the older protocol.
	CodeFleetMixedVersion Code = "fleet_mixed_version"

	// THE WEBHOOK EDGE. A delivery's sender is a vendor that reads the
	// status and the Retry-After and nothing else, so what these buy is the
	// OPERATOR: a delivery log at the vendor, a proxy's access log and the
	// engine's own all show the body, and the edge answered `{"error":
	// "invalid signature"}` — a code with a space in it that no client could
	// find in any vocabulary — and `{"status": "unavailable", "reason": …}`,
	// an envelope of its own. A node with no active company revision is
	// [CodeNoActiveRevision], and a verified delivery the broker would not
	// take is [CodeUnavailable]: the same facts other surfaces answer with.

	// CodeInvalidSignature is a delivery whose credential did not verify: an
	// HMAC signature, a shared token, or a Forge invocation token — and a
	// delivery to a per-seat route naming a seat with no app here, which is
	// the same answer for the same reason. 401. The log says which check
	// failed; the body never does, because that is what somebody probing the
	// route would read, and a code of its own for the seat with no app was a
	// roster of which handles have one.
	CodeInvalidSignature Code = "invalid_signature"
	// CodeNoWebhookSecret is a route with no secret to verify a delivery
	// against. 503 with a Retry-After sized to a person editing the
	// configuration, so the sender holds the delivery rather than dropping
	// it.
	CodeNoWebhookSecret Code = "no_webhook_secret"
	// CodeUnusableWebhookSecret is a route whose configured secret cannot
	// do the check it is for: a shared token too short to be the whole of
	// the authentication a provider that signs nothing leaves, or a GitLab
	// signing key in a form GitLab never signs with. The same 503 as an
	// absent secret, because the truth is the same — this route cannot check
	// a delivery — under its own code because the fix is not: an operator
	// told no secret is set goes looking for one they can see.
	CodeUnusableWebhookSecret Code = "unusable_webhook_secret"
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
	CodeNonCanonicalPath: "The request path has a dot segment or an empty " +
		"segment in it. Send the path it resolves to instead.",
	CodeInternalError: "Something went wrong inside the engine. The reason is " +
		"in this node's log.",
	CodeDraining: "This node is shutting down and is not taking new work. " +
		"Try another node, or this one once it has restarted.",
	CodeNoRoute: "Nothing on this node serves that path. Check the address — " +
		"or whether this node runs the surface it belongs to.",
	CodeMethodNotAllowed: "That path does not take this method. The Allow " +
		"header names the ones it does.",
	CodeInvalidToken: "This request needs a credential, and none this engine " +
		"accepts was presented. Sign in, or send an API token this deployment " +
		"issued.",

	CodeUnknownQuery: "This node does not serve that query.",
	// ABOUT THE GRANT, NOT A TOKEN. It said "that query needs an operator
	// token", which was true while an operator token was the only credential
	// and authority a single yes-or-no; a person signed in with a session
	// and refused one grant was told to go and find a token.
	CodeUnauthorized: "The credential you presented does not carry the grant " +
		"this request needs. Ask whoever runs this deployment for it, or use " +
		"a credential that carries it.",
	CodeQueryFailed: "That query could not be answered. The reason is in " +
		"this node's log.",
	CodeNotFound:  "There is no such record here.",
	CodeBadParams: "That query was asked with a parameter this endpoint does not accept.",
	CodeUnavailable: "This node cannot answer that right now — ask it again " +
		"when it says to, or, where it gives no time, ask another node or an " +
		"operator.",
	CodeCSRFOrigin: "That request came from a page this deployment does not " +
		"serve, so it was refused without being carried out.",
	CodeIdentityUnavailable: "This node cannot tell who you are at the moment — " +
		"the identity estate could not be read. Your credential is probably " +
		"fine; try again shortly.",

	CodePassInFlight: "A setup pass for this integration is already running. " +
		"Wait for it to finish rather than starting a second one.",
	CodeNotProvisionable: "This integration is not set up from here. Connect " +
		"it at the third-party app itself, or with the crewlet command line.",
	CodeNoExternalURL: "This deployment has no external address, so nothing " +
		"that has to point back at it can be made. Set the external URL in " +
		"this node's own configuration file and restart it.",
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
	CodeThrottled: "There have been too many failed attempts. Wait for the " +
		"time this answer names, then try again.",
	CodeSecondFactorRequired: "Enter the code from your authenticator app, or " +
		"one of your recovery codes.",
	CodeStepUpRequired: "This action needs you to have confirmed who you are " +
		"recently. Confirm it, then try again.",
	CodeSecondFactorEnrolmentRequired: "This company requires a second factor " +
		"and you have not set one up yet. Add an authenticator app to your " +
		"account, and everything else opens once you have.",
	CodeSessionRevoked: "This session has ended. Sign in again.",
	CodeSeatUnavailable: "The seat you are bound to is no longer in this " +
		"company's org chart, so there is nothing for you to act as. An " +
		"administrator can bind you to another one.",

	CodeInviteSpent: "This invitation is no longer valid. Ask whoever sent it " +
		"for a new one.",

	CodeForbidden: "You are signed in, and you may not make this change. The " +
		"detail names what it needs.",
	CodeStale: "Somebody changed this after you read it, so nothing was " +
		"written. Read it again and decide from what it says now.",
	CodeRefused: "That change was refused and nothing was written. The detail " +
		"says why.",

	CodeNoDestination: "Name the directory to write the backup into, as an " +
		"absolute path on the engine's host.",
	CodeBackupFailed: "The backup did not finish, and no manifest was written, " +
		"so nothing in that directory counts as a backup. If the detail names " +
		"the destination, choose another; otherwise the reason is in this " +
		"node's log.",
	CodeBudgetUnreadable: "The budget counters could not be read, so nothing " +
		"was reset. The reason is in this node's log.",
	CodeBudgetResetFailed: "The budget counters could not be reset. The reason " +
		"is in this node's log, and running the reset again is safe.",
	CodePositionRequired: "Name the stream and the sequence your copy reaches. " +
		"An acknowledgement moves what the trim may delete, so neither has a " +
		"default.",
	CodeUnknownStream: "This node has no stream by that name. Check it against " +
		"the retention status.",
	CodeAckFailed: "The acknowledgement was not recorded, so what the trim may " +
		"delete has not moved. The reason is in this node's log.",
	CodeNoStateLog: "This node has no state log to write an eviction or a " +
		"readmission to. Send this to a node that runs the company.",
	CodeConfirmRequired: "This change needs a confirmation that repeats what it " +
		"acts on. The detail says what to repeat.",
	CodeOpIDInvalid: "That operation id is not one this engine would have " +
		"issued for this request, so nothing was judged or written. Send back " +
		"the id an earlier answer carried, or none for a new request.",
	CodeInvalidGate: "That is not a node id any node can run under, so " +
		"nothing was judged or written. The detail says what a node id may hold.",
	CodeEvictionRefused: "That node is still reaching the fleet, so it was " +
		"not evicted and nothing was written. The hint says what to do.",
	CodeEvictionUnjudged: "This node could not tell whether that node is still " +
		"running, so nothing was written. The hint says how to go on.",
	CodeReadmissionRefused: "That node is too far behind to be counted again " +
		"yet, so nothing was written. The detail gives the positions.",
	CodeGateFailed: "The eviction or readmission was not recorded. The detail " +
		"says why.",
	CodeStreamRequired: "Name the stream this is about.",
	CodeStreamUnreadable: "This node could not read that stream just now, so " +
		"nothing was decided. Ask again in a moment.",
	CodeReanchorRefused: "The reanchor was refused and nothing changed. The " +
		"detail says why.",
	CodeTargetRequired: "Name the stream and the byte ceiling to move it to. A " +
		"target is fixed for the life of the operation, so neither has a default.",
	CodeCapacityRefused: "The capacity change was refused. The detail says why, " +
		"and names the operation already open when there is one.",
	CodeMaintenanceUnreadable: "The maintenance window's state could not be " +
		"read. The detail says why.",
	CodeFleetMixedVersion: "An upgrade is still rolling through the fleet, and " +
		"an import is applied by every node, the older ones included. Finish " +
		"the upgrade and import again.",

	CodeInvalidSignature: "This delivery's signature or token did not match " +
		"what this route checks, so it was not accepted. Make the secret at the " +
		"sender match the one this deployment holds for the integration.",
	CodeNoWebhookSecret: "This route has no secret configured to check a " +
		"delivery against, so it accepts none until one is set. The sender is " +
		"asked to retry, so nothing is lost in the meantime.",
	CodeUnusableWebhookSecret: "The secret configured for this route cannot " +
		"do the check it is for, so the route accepts nothing until it is " +
		"replaced. The engine's log says why; the sender is asked to retry " +
		"meanwhile.",

	CodeUnknownKind: "This build does not know that integration.",
	CodeSeatRequired: "Name the seat this is for. The detail says why one is " +
		"needed.",
	CodeNoSuchSeat: "The org chart has no seat by that name.",
	CodeLiteralInConfig: "The configuration holds this credential written out " +
		"rather than as a reference, so it was not overwritten. Move it into " +
		"the secret store first — the detail says which field.",
	CodeInvalidInput: "One of the values was not accepted, and nothing was " +
		"saved. The detail names the value and what it may be.",
	CodeSurfaceBusy: "Something else is writing to this integration right now. " +
		"Try again in a moment; nothing was changed.",

	CodeNoActiveRevision: "No company configuration is active on this node yet. " +
		"Import one first — the detail says how.",
	CodeRevisionAdvanced: "Somebody else's change to the configuration was " +
		"activated first, so this one was not. Read the configuration again and " +
		"make the change on top of it.",
	CodeAlreadyConfigured: "This write was only to land where no configuration " +
		"exists, and one does. Read it and edit that instead.",
	CodeUnreadableRevision: "That revision is sealed under a key this " +
		"deployment no longer holds, so it cannot be read. Put the key back in " +
		"the node's keyring first.",
	CodeUnsealedRevision: "That revision is stored without a seal, which a " +
		"build older than the mandatory keyring wrote, and a node reads only " +
		"sealed revisions because the seal is what authenticates one. The hint " +
		"says what brings its document back.",
	CodeInvalidRevisionID: "Name the revision this is about.",
	CodeAgainstNotFound:   "The revision to compare against is not held here.",
	CodeUnsupportedPatchMediaType: "This endpoint takes a JSON Merge Patch: an " +
		"object shaped like the document. The detail lists the formats it " +
		"accepts.",
	CodeChartNotWritableHere: "The org chart is no longer part of the " +
		"configuration, so nothing in this request was written. The detail " +
		"names the chart's own routes.",
	CodeNoSuchEntity: "The active configuration holds nothing by that name.",
	CodeIdentityMismatch: "The body renames what the path names, and this " +
		"route does not rename. Send it back under the name it already has.",
	CodeValidationError: "The configuration this change would produce is not " +
		"valid, so nothing was stored. The detail names what to fix.",
	CodeInvalidPatch: "The change could not be applied to the configuration, " +
		"so nothing was stored. The detail says why.",
	CodeSummaryRequired: "This change needs a one-line summary for the " +
		"configuration's history. Send it in the X-Summary header.",

	CodeInvalidName: "That is not a name a credential can be stored under. The " +
		"detail says what a name may be.",
	CodeKeyIDMismatch: "This node seals under a different key from the one " +
		"you expected. Make the two configurations agree before rekeying.",
	CodeRekeyIncomplete: "The rekey stopped part of the way through. The detail " +
		"names what moved, and running it again is safe once the missing key " +
		"is back.",
	CodeReservedName: "That name belongs to the engine's own identity " +
		"estate, not to the company's credentials, so nothing here can read " +
		"or change it. The detail names the command that does.",
}

// Codes is every code in the vocabulary, sorted.
//
// FOR THE WALKS that hold the table against what reads it: this package's own
// copy review, which must see every code rather than a list somebody kept by
// hand, and the gates elsewhere that hold a client's copy of the vocabulary
// against the engine's.
func Codes() []Code { return slices.Sorted(maps.Keys(codes)) }

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
	Write(w, status, Envelope(code, detail))
}

// Envelope is the refusal body [FailWithFields] writes, as a value: the
// detail with `error` and `message` over it.
//
// EXPORTED FOR A RENDERING THAT IS ALSO A FIXTURE — the node gate's refusals,
// whose bodies a golden file pins and the dashboard's suite reads — so the
// bytes a test records are the bytes the route sends, assembled here and
// nowhere else. The caller's map is not written to.
func Envelope(code Code, detail Detail) map[string]any {
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
	return body
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
//
// ZERO MEANS NOT RETRYABLE and writes no header: a node missing the
// configuration a route needs will not have it after any wait, and saying
// "come back" to a client is how it learns to hammer that node.
func Unavailable(w http.ResponseWriter, code Code, retryAfterSeconds int) {
	UnavailableWith(w, code, retryAfterSeconds, nil)
}

// RetrySeconds is a hint as the whole seconds a `Retry-After` carries: ROUNDED
// UP, so a derived hint of 1.2s is never sent as "come back in one" before the
// node could have got there, and zero — not retryable — stays zero, which
// [Unavailable] writes as no header at all.
//
// ONE CONVERSION for every surface that has a hint as a duration, because
// converting at each call site is how two of them came to round differently:
// one rounded to the nearest second, and a 1.4s drain estimate went out as 1.
func RetrySeconds(hint time.Duration) int {
	if hint <= 0 {
		return 0
	}
	return int((hint + time.Second - 1) / time.Second)
}

// Throttled writes `429 throttled` carrying the wait left as a Retry-After.
//
// ONE WRITER, for [Unavailable]'s reason: a 429 with no Retry-After tells a
// client it was refused and not when it may try again, so it retries at once,
// is refused again, and a person at a form reads a lockout where there is a
// wait of seconds. ROUNDED UP and never below a second, because a hint of
// 0.2s sent as 0 is a client told to come back before the curve could have
// admitted it — and zero here would read as "do not retry", which is never
// what a curve means.
func Throttled(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(RetrySeconds(retryAfter), 1)))
	Fail(w, http.StatusTooManyRequests, CodeThrottled)
}

// UnavailableWith is [Unavailable] carrying a detail: what could not be read,
// the operation id a retry must reuse, the setting to change.
//
// THE ONLY OTHER WAY a 503 is written, so a route with something to say beside
// the code cannot reach for [FailWithFields] and drop the header — which is how
// a dozen of them came to answer "come back" without saying when.
func UnavailableWith(w http.ResponseWriter, code Code, retryAfterSeconds int, detail Detail) {
	if retryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	}
	FailWithFields(w, http.StatusServiceUnavailable, code, detail)
}

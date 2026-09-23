package types

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// Authentication: who signed in and how, what ended a session, what somebody
// was refused, what changed about what a person may do, and whether a record
// on a fleet log was written by the fleet at all.
//
// # The live half of a trail that has a durable half elsewhere
//
// Every one of these is published on the ordinary event path, so it reaches
// the PUBLISHING node's store row, the activity feed and any OTLP sink the way
// every other event does. That row is written inline on the node that
// published it, with no consumer group — so a sign-in served by one ingress
// node is a row on that node alone, and a per-node trail is not an audit
// trail. The fleet-wide answer is `iam_history`, which the identity applier
// writes identically on every node from the records themselves; these are the
// LIVE FEED beside it and the export an operator's collector reads.
//
// # Nothing here carries what somebody presented
//
// Not a password, not a login somebody typed and got wrong, not a token value,
// and not an unsalted hash of any of them — a hash of a typed login is
// reversible against the company's own roster in one pass, and a password
// typed into the login box is exactly what a failed sign-in would otherwise
// carry into every backup. A failure is COUNTED (see [IAMLoginFailures]); an
// identity appears only once this engine resolved one itself.
//
// # Who authors the rate, type by type
//
// internal/events files each of these with its [events.RateAuthor], and the
// split is the design: a failed attempt is authored by whoever can reach the
// listener, so it is never a type at all — it is a metrics counter plus the
// one COALESCED row per client per minute the engine's own loop publishes.
// Everything else here follows a credential this engine verified, or the
// engine's own loop deciding once per key, and says so in its category entry.
//
// # What is deliberately NOT here
//
// `iam_identity_linked` is named by the design and has no type, because
// nothing in this build links an identity-provider subject to a person: the
// callback resolves a subject somebody ALREADY linked and refuses one nobody
// did. A registered type with no publisher is a filter chip that can never
// match and a docs row describing a fact that never happens, so it arrives
// with the link flow that produces it. `iam_token_rejected` is absent for the
// admission rule's own reason: a rejected bearer is authored by whoever holds
// the wrong value, so it is a failed attempt of method `bearer` inside
// [IAMLoginFailures] and never a row of its own.

func init() {
	events.Register[IAMSessionStarted]()
	events.Register[IAMSessionEnded]()
	events.Register[IAMSessionReuseDetected]()
	events.Register[IAMLoginFailures]()
	events.Register[IAMStepUpCompleted]()
	events.Register[IAMCredentialMinted]()
	events.Register[IAMCredentialRevoked]()
	events.Register[IAMGrantsChanged]()
	events.Register[IAMTokenFirstUse]()
	events.Register[IAMTokenOverreach]()
	events.Register[IAMRecoveryCodeUsed]()
	events.Register[IAMMFAReset]()
	events.Register[IAMSessionGenerationBumped]()
	events.Register[RecordUnverifiable]()
	events.Register[RecordTampered]()
}

// SignInMethod is how a session was opened.
type SignInMethod string

const (
	// SignInPassword is a password, and a second factor when the person
	// holds one.
	SignInPassword SignInMethod = "password"

	// SignInOIDC is an identity provider's assertion about a subject this
	// estate had already linked.
	SignInOIDC SignInMethod = "oidc"

	// SignInInvite is the session an invitation's redemption opens, for
	// the person it just enrolled.
	SignInInvite SignInMethod = "invite"

	// SignInBootstrap is the first person, opened by the one-time code a
	// node wrote beside its store.
	SignInBootstrap SignInMethod = "bootstrap"

	// SignInToken is a Tier A token exchanged for a session. The principal
	// is the TOKEN, acting as itself — never a person.
	SignInToken SignInMethod = "token"
)

// Valid reports whether m is a method this build names.
func (m SignInMethod) Valid() bool {
	switch m {
	case SignInPassword, SignInOIDC, SignInInvite, SignInBootstrap, SignInToken:
		return true
	}
	return false
}

// SecondFactor is which second factor a sign-in or a step-up presented.
//
// THE ZERO VALUE IS "NONE", and it is a real answer rather than an absence: a
// person who holds no second factor signs in with one proof, and a row that
// says so is the row an auditor asking "who never enrolled one" reads.
type SecondFactor string

const (
	// FactorTOTP is a code from an authenticator app.
	FactorTOTP SecondFactor = "totp"

	// FactorRecovery is one of the single-use codes that stand in for the
	// app when it is lost — which is why using one is also its own event,
	// [IAMRecoveryCodeUsed].
	FactorRecovery SecondFactor = "recovery"
)

// Valid reports whether f is none or a factor this build names.
func (f SecondFactor) Valid() bool {
	return f == "" || f == FactorTOTP || f == FactorRecovery
}

// SessionEndReason is why a session ended.
type SessionEndReason string

const (
	// EndLogout is the holder signing out of this session, or of one
	// named session of their own.
	EndLogout SessionEndReason = "logout"

	// EndLogoutAll is the holder signing out everywhere: their revocation
	// epoch moved, which ends every session they hold at once.
	EndLogoutAll SessionEndReason = "logout_all"

	// EndIdle is a session nobody used for its idle window — noticed when
	// its bearer was next presented, because an idle deadline lives in the
	// bearer and nowhere else, so no other frame can see it pass.
	EndIdle SessionEndReason = "idle"

	// EndAbsolute is a session past the lifetime it was minted with, which
	// no re-issue moves.
	EndAbsolute SessionEndReason = "absolute"

	// EndRevoked is somebody ELSE ending it: an administrator ending one
	// session or every session a person holds.
	EndRevoked SessionEndReason = "revoked"

	// EndIdPRevoked is the deactivation probe hearing `invalid_grant` from
	// the identity provider — an off-boarding done centrally.
	EndIdPRevoked SessionEndReason = "idp_revoked"

	// EndPersonRemoved is the person being removed, which ends every
	// session they hold along with everything else about them.
	EndPersonRemoved SessionEndReason = "person_removed"
)

// Valid reports whether r is a reason this build names.
func (r SessionEndReason) Valid() bool {
	switch r {
	case EndLogout, EndLogoutAll, EndIdle, EndAbsolute, EndRevoked,
		EndIdPRevoked, EndPersonRemoved:
		return true
	}
	return false
}

// FailureMethod is what a failed attempt was trying to prove itself with.
type FailureMethod string

const (
	// FailPassword is a sign-in or a step-up whose first proof failed —
	// no such login, a wrong password, a person who may not act. One
	// method for all of them on purpose: the arms are distinguishable in
	// the node's own log line, and a count keyed on which arm fired is a
	// roster read back out of the metrics.
	FailPassword FailureMethod = "password"

	// FailSecondFactor is a correct password with a wrong code.
	FailSecondFactor FailureMethod = "second_factor"

	// FailOIDC is a provider round trip that did not end in somebody this
	// estate holds: a missing or stale flight, a state mismatch, a refused
	// ID token, a subject nobody linked.
	FailOIDC FailureMethod = "oidc"

	// FailBootstrap is a wrong one-time founder code.
	FailBootstrap FailureMethod = "bootstrap"

	// FailInvite is an invitation redemption the throttle refused. A link
	// that does not resolve answers 410 to whoever holds it and is not a
	// guess at a credential, so nothing else about a redemption counts.
	FailInvite FailureMethod = "invite"

	// FailBearer is a credential presented on a request and refused: an
	// `Authorization` value matching no Tier A token, or a session cookie
	// whose signature does not verify under this deployment's keyring. A
	// cookie that verified and whose session had simply ENDED is not a
	// failure — it is a browser holding yesterday's session, and counting
	// it would page somebody for every tab left open over a weekend.
	FailBearer FailureMethod = "bearer"
)

// Valid reports whether m is a method this build names.
func (m FailureMethod) Valid() bool {
	switch m {
	case FailPassword, FailSecondFactor, FailOIDC, FailBootstrap, FailInvite,
		FailBearer:
		return true
	}
	return false
}

// FailureMethods is every method, for a metrics dimension and a test that
// walks the set.
var FailureMethods = []FailureMethod{
	FailPassword, FailSecondFactor, FailOIDC, FailBootstrap, FailInvite,
	FailBearer,
}

// CredentialKind is what sort of credential a mint or a revocation was about.
//
// The same spellings internal/iamdomain stores a credential's method under,
// declared again here rather than imported because this package is the wire
// catalogue and depends on nothing but the envelope.
type CredentialKind string

const (
	// CredentialToken is a machine token a person minted.
	CredentialToken CredentialKind = "token"

	// CredentialTOTP is an authenticator app's seed.
	CredentialTOTP CredentialKind = "totp"

	// CredentialRecovery is a set of single-use recovery codes.
	CredentialRecovery CredentialKind = "recovery"

	// CredentialPassword is a password.
	CredentialPassword CredentialKind = "password"

	// CredentialOIDC is a provider subject.
	CredentialOIDC CredentialKind = "oidc"
)

// IAMSessionStarted is a session opened: somebody proved who they are and now
// holds a cookie.
//
// THE REMOTE ADDRESS IS THE CLIENT, resolved through `api.trusted_proxies`,
// which is the same value the sign-in throttle keys on — behind a proxy the
// peer is the proxy, and a row naming it says nothing about who signed in.
type IAMSessionStarted struct {
	Person string       `json:"person"`
	Login  string       `json:"login"`
	Method SignInMethod `json:"method"`

	// Lineage is the session's own id. It is in every row about this
	// session and is not a secret: a bearer is, and nothing here carries
	// one.
	Lineage string `json:"lineage"`

	Remote       string       `json:"remote"`
	SecondFactor SecondFactor `json:"second_factor"`

	// ACR is the authentication context an identity provider asserted,
	// empty on every other method.
	ACR string `json:"acr"`

	// ExpiresAt is the absolute deadline the session was minted with.
	ExpiresAt time.Time `json:"expires_at"`
}

// EventType is the "iam_session_started" wire type.
func (IAMSessionStarted) EventType() string { return "iam_session_started" }

// Actor is the login that signed in: a person acts as themselves, and a
// token's exchange as the token.
func (e IAMSessionStarted) Actor() string { return e.Login }

// Summary names the method and the second factor, which are the two things an
// auditor scanning a feed of sign-ins is looking for.
func (e IAMSessionStarted) Summary() string {
	how := string(e.Method)
	if how == "" {
		how = "an unnamed method"
	}
	if e.SecondFactor != "" {
		how += " + " + string(e.SecondFactor)
	}
	return lead(orSomebody(e.Login, e.Person), "signed in ("+how+")")
}

// IAMSessionEnded is a session that stopped being one.
//
// LINEAGE IS EMPTY WHEN THE GESTURE ENDED EVERY SESSION A PERSON HELD — a
// sign-out everywhere, an administrator's revocation, a removal — because
// those move the person's revocation epoch rather than closing rows one at a
// time, and the event says what was done rather than inventing a list of the
// sessions it happened to reach.
type IAMSessionEnded struct {
	Person  string           `json:"person"`
	Lineage string           `json:"lineage"`
	Reason  SessionEndReason `json:"reason"`

	// By is who ended it: the holder for a logout, an administrator for a
	// revocation or a removal, and EMPTY for a deadline or a provider
	// verdict, which nobody authored.
	By string `json:"by"`
}

// EventType is the "iam_session_ended" wire type.
func (IAMSessionEnded) EventType() string { return "iam_session_ended" }

// Actor is whoever ended it, when somebody did. A deadline has no author, and
// the chain then falls back to the node that noticed.
func (e IAMSessionEnded) Actor() string { return e.By }

// Summary says whose, which one and why.
func (e IAMSessionEnded) Summary() string {
	which := "every session"
	if e.Lineage != "" {
		which = "session " + shortLineage(e.Lineage)
	}
	why := string(e.Reason)
	if why == "" {
		why = "no reason recorded"
	}
	return fmt.Sprintf("%s of %s ended (%s)", upperFirst(which),
		orSomebody(e.Person, ""), why)
}

// IAMSessionReuseDetected is a session cookie presented with a rotation index
// this engine could not have issued — ahead of the clock past the overlap —
// which is the one positive evidence of a copied cookie a derived rotation can
// give. Every session the person holds was ended in response.
type IAMSessionReuseDetected struct {
	Person  string `json:"person"`
	Lineage string `json:"lineage"`

	// Rotation is the index the replayed bearer carried.
	Rotation uint64 `json:"rotation"`

	Remote string `json:"remote"`
}

// EventType is the "iam_session_reuse_detected" wire type.
func (IAMSessionReuseDetected) EventType() string { return "iam_session_reuse_detected" }

// Summary says what was done about it, because the reader's next question is
// whether anything was.
func (e IAMSessionReuseDetected) Summary() string {
	return fmt.Sprintf("A replayed session cookie for %s was refused; every "+
		"session they held was ended", orSomebody(e.Person, ""))
}

// IAMLoginFailures is every failed attempt one client made inside one minute,
// counted.
//
// # ONE ROW PER CLIENT PER MINUTE, published by the engine's own loop
//
// A failed sign-in is authored by whoever can reach the listener. A row per
// attempt hands the size of the node estate — and of every backup, snapshot
// and integrity check taken from it — to them: the design this replaced wrote
// one synchronous row per attempt, which was 6.9 million rows a day from one
// host. So each attempt is a counter on the metrics recorder, and what becomes
// a row is this, paced by a ticker rather than by the caller.
//
// # What it carries, and what it never carries
//
// Counts. The DISTINCT-SUBJECT count is computed under a key this process
// generated and never wrote anywhere, so it says how many different names one
// client tried without being able to say which — and nothing here is the
// presented value or an unsalted hash of it, since the latter reverses against
// the company's own roster in one pass. A person's id appears only where the
// ENGINE resolved one: a wrong password for somebody real.
type IAMLoginFailures struct {
	// Client is the address the throttle keys on, or "*" for the one row
	// a minute folds every client past the per-minute cap into.
	Client string `json:"client"`

	// Minute is the start of the minute these attempts fell in, UTC.
	Minute time.Time `json:"minute"`

	// Attempts is how many attempts were verified and refused.
	Attempts int `json:"attempts"`

	// Throttled is how many were refused at the throttle's ceiling before
	// anything was verified — the source had already failed too often.
	// It is the "ceiling reached" of the design, expressed as the count of
	// requests the ceiling actually turned away.
	Throttled int `json:"throttled"`

	// Subjects is how many DISTINCT names or bearers the attempts
	// presented. It saturates at a cap stated in internal/iam/authevents,
	// so a value at the cap reads "at least".
	Subjects int `json:"subjects"`

	// Clients is how many distinct clients this row covers: one, except
	// on the "*" row, where it is the size of what was folded — which is
	// the size of a distributed attack rather than of one guesser.
	Clients int `json:"clients"`

	// Methods are what the attempts tried to prove themselves with,
	// sorted and distinct.
	Methods []FailureMethod `json:"methods,omitempty"`

	// People are the persons the engine resolved a failed attempt to —
	// a wrong password for a real login — sorted, distinct and capped.
	People []string `json:"people,omitempty"`
}

// EventType is the "iam_login_failures" wire type.
func (IAMLoginFailures) EventType() string { return "iam_login_failures" }

// Summary leads with the count and the client, which is what somebody reading
// a feed of these is sorting by.
func (e IAMLoginFailures) Summary() string {
	client := e.Client
	switch {
	case client == "*":
		client = strconv.Itoa(e.Clients) + " clients past the per-minute cap"
	case client == "":
		client = "an unidentified client"
	}
	methods := make([]string, 0, len(e.Methods))
	for _, m := range e.Methods {
		methods = append(methods, string(m))
	}
	detail := strings.Join(methods, ", ")
	if e.Throttled > 0 {
		detail = joinDetail(detail, strconv.Itoa(e.Throttled)+" throttled")
	}
	if detail != "" {
		detail = " (" + detail + ")"
	}
	return fmt.Sprintf("%d failed attempt(s) from %s in one minute%s",
		e.Attempts+e.Throttled, client, detail)
}

// IAMStepUpCompleted is a signed-in person confirming who they are again, which
// is what the surfaces that change what a company IS ask for.
//
// It opens a FRESH session carrying the new proof; Replaces is the one the
// confirmation was made from, so a reader can join the two.
type IAMStepUpCompleted struct {
	Person       string       `json:"person"`
	Login        string       `json:"login"`
	Lineage      string       `json:"lineage"`
	Replaces     string       `json:"replaces"`
	SecondFactor SecondFactor `json:"second_factor"`
	Remote       string       `json:"remote"`
}

// EventType is the "iam_stepup_completed" wire type.
func (IAMStepUpCompleted) EventType() string { return "iam_stepup_completed" }

// Actor is the person who confirmed.
func (e IAMStepUpCompleted) Actor() string { return e.Login }

// Summary says what was presented.
func (e IAMStepUpCompleted) Summary() string {
	how := "password"
	if e.SecondFactor != "" {
		how += " + " + string(e.SecondFactor)
	}
	return lead(orSomebody(e.Login, e.Person), "confirmed their identity ("+how+")")
}

// IAMCredentialMinted is a credential added to somebody: a machine token, an
// authenticator app, a fresh set of recovery codes.
//
// NEVER THE VALUE. A token is shown once to whoever minted it and stored as a
// hash; this says that one exists, whose it is and what it may do.
type IAMCredentialMinted struct {
	Credential string         `json:"credential"`
	Kind       CredentialKind `json:"kind"`
	Owner      string         `json:"owner"`

	// Grants and Colleague are what a TOKEN was minted to carry — a subset
	// of its owner's, which is what an investigation asks first. Empty on
	// a second factor, which carries no authority of its own.
	Grants    []string `json:"grants,omitempty"`
	Colleague string   `json:"colleague"`

	// ExpiresAt is when it stops working, zero for a credential with no
	// deadline of its own.
	ExpiresAt time.Time `json:"expires_at"`

	By     string `json:"by"`
	Reason string `json:"reason"`
}

// EventType is the "iam_credential_minted" wire type.
func (IAMCredentialMinted) EventType() string { return "iam_credential_minted" }

// Actor is who minted it.
func (e IAMCredentialMinted) Actor() string { return e.By }

// Summary names the kind and the owner.
func (e IAMCredentialMinted) Summary() string {
	return fmt.Sprintf("%s credential minted for %s", credentialWord(e.Kind),
		orSomebody(e.Owner, ""))
}

// IAMCredentialRevoked is a credential withdrawn. The row stays, carrying when
// and by whom — "this token was withdrawn on the 3rd by Ana" is the sentence an
// investigation is looking for.
type IAMCredentialRevoked struct {
	Credential string         `json:"credential"`
	Kind       CredentialKind `json:"kind"`
	Owner      string         `json:"owner"`
	By         string         `json:"by"`
	Reason     string         `json:"reason"`
}

// EventType is the "iam_credential_revoked" wire type.
func (IAMCredentialRevoked) EventType() string { return "iam_credential_revoked" }

// Actor is who revoked it.
func (e IAMCredentialRevoked) Actor() string { return e.By }

// Summary names the kind and the owner.
func (e IAMCredentialRevoked) Summary() string {
	return fmt.Sprintf("%s credential revoked for %s", credentialWord(e.Kind),
		orSomebody(e.Owner, ""))
}

// IAMGrantsChanged is one write that changed what a person may do to the
// deployment — including the enrolment that first gave them anything.
//
// ONE PER PERSON WRITE, published by the writer that decided it: the before
// and the after are read inside the snapshot the record was formed in, so the
// difference is exactly what the record changed rather than what a caller
// believed was there.
type IAMGrantsChanged struct {
	Person  string   `json:"person"`
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	By      string   `json:"by"`

	// Version is the record's position, packed — the same number the
	// person's row carries afterwards, so a reader can find the write in
	// `iam_history`.
	Version int64 `json:"version"`
}

// EventType is the "iam_grants_changed" wire type.
func (IAMGrantsChanged) EventType() string { return "iam_grants_changed" }

// Actor is who changed them.
func (e IAMGrantsChanged) Actor() string { return e.By }

// Summary lists both directions, because a grant added and a grant taken away
// are the two halves of the same review.
func (e IAMGrantsChanged) Summary() string {
	var parts []string
	if len(e.Added) > 0 {
		parts = append(parts, "+"+strings.Join(e.Added, ", +"))
	}
	if len(e.Removed) > 0 {
		parts = append(parts, "-"+strings.Join(e.Removed, ", -"))
	}
	change := strings.Join(parts, "; ")
	if change == "" {
		change = "no net change"
	}
	return fmt.Sprintf("Grants of %s changed: %s", orSomebody(e.Person, ""), change)
}

// IAMTokenFirstUse is a Tier A token used — once per token per hour, unless its
// entry sets `audit_every_use`, in which case once per request.
//
// COALESCED BY DEFAULT because a token driving the operator MCP makes a
// request per tool call, and a row per request turns an assistant's ordinary
// session into thousands an hour: the trail stops being more useful and the
// one interesting row becomes unfindable.
type IAMTokenFirstUse struct {
	Token string `json:"token"`

	// Route is the CLASS of what was reached — the first path segment —
	// rather than the path, which carries ids a feed has no use for.
	Route  string `json:"route"`
	Remote string `json:"remote"`

	// EveryUse says this row is one of a per-request series rather than
	// the first use in an hour, so a reader counting rows knows which
	// they are counting.
	EveryUse bool `json:"every_use"`
}

// EventType is the "iam_token_first_use" wire type.
func (IAMTokenFirstUse) EventType() string { return "iam_token_first_use" }

// Actor is the token, under the login it acts as.
func (e IAMTokenFirstUse) Actor() string { return tokenActor(e.Token) }

// Summary says where it was used from.
func (e IAMTokenFirstUse) Summary() string {
	what := "used"
	if !e.EveryUse {
		what = "used (first use this hour)"
	}
	return fmt.Sprintf("Token %s %s on %s from %s", orSomebody(e.Token, ""),
		what, orSomebody(e.Route, "/"), orSomebody(e.Remote, "an unknown address"))
}

// IAMTokenOverreach is a Tier A token refused by a route its grants do not
// cover — coalesced per token per hour unless the entry sets
// `audit_every_use`.
//
// It is authenticated: the token matched, so there is a credential to revoke
// and a name on the row. What it says is that something holding the token is
// trying to do more with it than it was pinned for, which is the question a
// break-glass credential's owner most wants answered.
type IAMTokenOverreach struct {
	Token  string `json:"token"`
	Route  string `json:"route"`
	Remote string `json:"remote"`

	// Status is the refusal the route answered with.
	Status   int  `json:"status"`
	EveryUse bool `json:"every_use"`
}

// EventType is the "iam_token_overreach" wire type.
func (IAMTokenOverreach) EventType() string { return "iam_token_overreach" }

// Actor is the token.
func (e IAMTokenOverreach) Actor() string { return tokenActor(e.Token) }

// Summary says what it reached for.
func (e IAMTokenOverreach) Summary() string {
	return fmt.Sprintf("Token %s was refused on %s (%d) from %s",
		orSomebody(e.Token, ""), orSomebody(e.Route, "/"), e.Status,
		orSomebody(e.Remote, "an unknown address"))
}

// IAMRecoveryCodeUsed is a recovery code spent to sign in or to confirm an
// identity. Spent means gone: it will never work again, and Remaining is how
// many the person has left.
type IAMRecoveryCodeUsed struct {
	Person    string `json:"person"`
	Login     string `json:"login"`
	Remaining int    `json:"remaining"`
	Remote    string `json:"remote"`
}

// EventType is the "iam_recovery_code_used" wire type.
func (IAMRecoveryCodeUsed) EventType() string { return "iam_recovery_code_used" }

// Actor is the person who spent it.
func (e IAMRecoveryCodeUsed) Actor() string { return e.Login }

// Summary says how many are left, because none left is somebody one lost phone
// away from an administrator.
func (e IAMRecoveryCodeUsed) Summary() string {
	return lead(orSomebody(e.Login, e.Person),
		fmt.Sprintf("used a recovery code (%d left)", e.Remaining))
}

// IAMMFAReset is an administrator clearing a person's second factor, which also
// ends every session they hold.
type IAMMFAReset struct {
	Person string `json:"person"`
	By     string `json:"by"`
	Reason string `json:"reason"`
}

// EventType is the "iam_mfa_reset" wire type.
func (IAMMFAReset) EventType() string { return "iam_mfa_reset" }

// Actor is the administrator.
func (e IAMMFAReset) Actor() string { return e.By }

// Summary names whose.
func (e IAMMFAReset) Summary() string {
	return "Second factor of " + orSomebody(e.Person, "") + " reset"
}

// IAMSessionGenerationBumped is the fleet-wide session generation moving, which
// ends every session in the company at once — the restore runbook's last step.
type IAMSessionGenerationBumped struct {
	// Generation is the new value; every bearer minted below it is over.
	Generation uint64 `json:"generation"`
	By         string `json:"by"`
	Reason     string `json:"reason"`
}

// EventType is the "iam_session_generation_bumped" wire type.
func (IAMSessionGenerationBumped) EventType() string { return "iam_session_generation_bumped" }

// Actor is who bumped it.
func (e IAMSessionGenerationBumped) Actor() string { return e.By }

// Summary states the consequence rather than the number.
func (e IAMSessionGenerationBumped) Summary() string {
	return fmt.Sprintf("Every session in the company was ended (generation %d)",
		e.Generation)
}

// RecordUnverifiable is a record on a state log signed under a key this node's
// keyring does not hold. It is RETAINED and reprocessed once the key arrives —
// expected briefly during a keyring rotation, and an operator error if it
// persists. (A record that installs an apply gate is the exception: that one
// stops the domain's applier, because a gate this node cannot authenticate
// would license every record above it.)
//
// # Named for the framework, not for identity
//
// The design called this `iam_record_gated`. The verdict is the state-log
// framework's and every domain's log is signed and verified by the same code,
// so a record the tracker's applier cannot authenticate is the same fact as
// one on the identity log — and a name scoped to one domain would either be
// published for the other four under a wrong name or leave them unrecorded.
// "Gated" is also already taken: the framework counts records an APPLY GATE
// dropped (a removal, an eviction) as `records_gated`, and "unverifiable" is
// what the same framework calls a record retained for its key. The domain is a
// field.
//
// ONCE PER DOMAIN AND KEY ID PER NODE PROCESS, and at most
// statelog.MaxWitnessedKeys distinct ids per domain: a key id is whatever the
// frame says, and whoever can reach a cluster port can write frames. The cap
// is what keeps this type's rate the engine's rather than that writer's.
type RecordUnverifiable struct {
	Domain string `json:"domain"`
	KeyID  string `json:"key_id"`

	// Held are the key ids this node's keyring does hold, which is the
	// half of the comparison an operator needs to fix it.
	Held []string `json:"held,omitempty"`

	// Position is where the first such record sat, as `stream@generation:seq`.
	Position string `json:"position"`
}

// EventType is the "statelog_record_unverifiable" wire type.
func (RecordUnverifiable) EventType() string { return "statelog_record_unverifiable" }

// Summary names the key and what this node holds instead.
func (e RecordUnverifiable) Summary() string {
	return fmt.Sprintf("%s record signed under key %q, which this node does not "+
		"hold (it holds %s); nothing under it is applied until it does",
		orSomebody(e.Domain, "A"), e.KeyID, heldKeys(e.Held))
}

// RecordTampered is a record on a state log whose signature fails under a key
// this node DOES hold, or that is not a signed frame at all. Something that
// is not the fleet wrote it, and the domain's applier has stopped rather than
// apply it or anything after it.
//
// Named for the framework for [RecordUnverifiable]'s reason. Once per domain
// and key id per node process: the applier stops at the first one, so what
// repeats it is a restart.
type RecordTampered struct {
	Domain string   `json:"domain"`
	KeyID  string   `json:"key_id"`
	Held   []string `json:"held,omitempty"`

	// Position is where the record sat, as `stream@generation:seq`: the
	// log's own coordinates, since a record this node refused was never
	// applied at a position of its own.
	Position string `json:"position"`
}

// EventType is the "statelog_record_tampered" wire type.
func (RecordTampered) EventType() string { return "statelog_record_tampered" }

// Summary says what the node did about it.
func (e RecordTampered) Summary() string {
	key := strconv.Quote(e.KeyID)
	if e.KeyID == "" {
		key = "no key (not a signed frame)"
	}
	return fmt.Sprintf("%s record at %s fails its signature under %s; the "+
		"applier stopped", orSomebody(e.Domain, "A"), orSomebody(e.Position, "?"), key)
}

// orSomebody is a value for a sentence, or a fallback when it is empty — so a
// line built from a row missing a field reads as a sentence rather than as two
// spaces.
func orSomebody(value, fallback string) string {
	if value != "" {
		return value
	}
	if fallback != "" {
		return fallback
	}
	return "somebody"
}

// shortLineage is the first eight characters of a lineage, which is how a
// person reading a feed tells two sessions apart without reading a uuid.
func shortLineage(lineage string) string {
	if len(lineage) <= 8 {
		return lineage
	}
	return lineage[:8]
}

// credentialWord is a kind as the first word of a sentence.
func credentialWord(kind CredentialKind) string {
	if kind == "" {
		return "A"
	}
	return upperFirst(string(kind))
}

// tokenActor is the login a Tier A token acts under, which is what every other
// row about it names — `token:ops`, never the bare `ops`.
func tokenActor(id string) string {
	if id == "" {
		return ""
	}
	return "token:" + id
}

// heldKeys renders the key ids a node holds, sorted, or says it holds none.
func heldKeys(held []string) string {
	if len(held) == 0 {
		return "none"
	}
	sorted := slices.Clone(held)
	slices.Sort(sorted)
	return strings.Join(sorted, ", ")
}

// joinDetail joins two clauses of a parenthetical, either of which may be
// empty.
func joinDetail(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

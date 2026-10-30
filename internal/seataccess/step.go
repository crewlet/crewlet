package seataccess

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Op is one transition the shared rules decide a lane needs. [Plan] decides
// WHICH ops a lane needs and in what order; the vendor turns each into its own
// steps ([Vendor.Steps]) and takes them ([Session.Apply]).
//
// The split is the point. Whether an account somebody disabled by hand may be
// re-enabled, whether remove is allowed, whether a changed role is drift or an
// edit: those are decided here, once, so no vendor can answer them
// differently. How a role is changed at GitLab is GitLab's.
type Op string

const (
	// OpCreate makes the seat's account with its level and scope, and seals
	// its credential: a seat that is on with no account.
	OpCreate Op = "create"

	// OpEnable turns an account back on: one this engine disabled, or one
	// somebody disabled at the vendor that a person asked to turn back on.
	// It mints a fresh credential, since off revoked the old one.
	OpEnable Op = "enable"

	// OpLevel moves the account to the document's level or permissions: an
	// edit, or a restore of drift ([Step.Drift]).
	OpLevel Op = "level"

	// OpScope moves the account to the document's scope.
	OpScope Op = "scope"

	// OpMint seals a credential for an account that is on and holds none.
	OpMint Op = "mint"

	// OpDisable is off (D1): the account is disabled at the vendor, its
	// credential revoked and its sealed value cleared, and the account kept.
	OpDisable Op = "disable"

	// OpRemove is the separate, confirmed removal, allowed only when the seat
	// is off. Never from a document edit or a timer.
	OpRemove Op = "remove"

	// OpRotate mints a new credential, seals it, and revokes the old one, on
	// request.
	OpRotate Op = "rotate"
)

// Valid reports whether o is an op this build knows.
func (o Op) Valid() bool {
	return slices.Contains([]Op{
		OpCreate, OpEnable, OpLevel, OpScope, OpMint, OpDisable, OpRemove, OpRotate,
	}, o)
}

// StepVerb is the word a step is drawn with on the dashboard. The values are
// the wire's (`verb` on a plan step).
type StepVerb string

// The verbs a step can carry.
const (
	VerbCreate  StepVerb = "create"
	VerbGrant   StepVerb = "grant"
	VerbMint    StepVerb = "mint"
	VerbEnable  StepVerb = "enable"
	VerbRerole  StepVerb = "rerole"
	VerbScope   StepVerb = "scope"
	VerbDisable StepVerb = "disable"
	VerbRevoke  StepVerb = "revoke"
	VerbRemove  StepVerb = "remove"
	VerbRotate  StepVerb = "rotate"
	VerbRestore StepVerb = "restore"
	VerbApprove StepVerb = "approve"
)

// Valid reports whether v is a verb this build draws.
func (v StepVerb) Valid() bool {
	return slices.Contains([]StepVerb{
		VerbCreate, VerbGrant, VerbMint, VerbEnable, VerbRerole, VerbScope,
		VerbDisable, VerbRevoke, VerbRemove, VerbRotate, VerbRestore, VerbApprove,
	}, v)
}

// StepActor is who takes a step: this engine, or a person at the vendor (a
// GitHub App's creation and a raise past its registered permissions, a Slack
// install, an app registration only a person can delete). The values are the
// wire's (`by` on a plan step).
type StepActor string

const (
	// ByCrewlet is a step a pass takes.
	ByCrewlet StepActor = "crewlet"

	// ByYou is a step only a person can take; a sync reaching it waits
	// ([OutcomeWaiting]) with the link in [Step.ActionURL].
	ByYou StepActor = "you"
)

// Valid reports whether a is an actor this build names.
func (a StepActor) Valid() bool { return a == ByCrewlet || a == ByYou }

// Step is one thing a sync does at the vendor, or one thing a person must do
// there, in the order it is done. The wire shape is the dashboard's (a `plan`
// entry).
type Step struct {
	// Op is the transition this step belongs to, which the vendor's Apply
	// dispatches on.
	Op Op `json:"op"`

	// Verb, Text and Note are how the step is drawn: Text is one sentence in
	// the vendor's own words ("Add it to gophrase/infra as Developer"), Note
	// a caveat printed under it.
	Verb StepVerb `json:"verb"`
	Text string   `json:"text"`
	Note string   `json:"note,omitempty"`

	// By is who takes it.
	By StepActor `json:"by"`

	// ActionURL is where a person takes a [ByYou] step.
	ActionURL string `json:"action_url,omitempty"`

	// Drift marks a step that restores the document's setting over a change
	// somebody made at the vendor (D2). The sync records a drift_restored
	// event with the before and after when it takes it.
	Drift bool `json:"drift,omitempty"`

	// Index is the step's place within its op, so a vendor whose op takes
	// several calls (create, then grant, then mint) knows which this is.
	Index int `json:"-"`
}

// Ask is a one-off a person asked for on one seat's lane, carried by the sync
// that takes it. None of these can come from a document edit or a timer.
type Ask struct {
	// Rotate mints a new credential and revokes the old.
	Rotate bool

	// Remove removes the account. Refused while the seat is on
	// ([ErrStillOn]).
	Remove bool

	// Reenable turns back on an account somebody disabled at the vendor
	// ("Turn back on"), which no pass does unasked (D3).
	Reenable bool
}

// Zero reports an ask for nothing.
func (a Ask) Zero() bool { return a == Ask{} }

// ErrStillOn refuses a removal asked for a seat that is still on. Remove is
// allowed only when off (D1): the caller turns it off first.
var ErrStillOn = errors.New("seataccess: turn the agent off on this integration before removing its account")

// Outcome is how a sync of one seat's lane ended. The values are the wire's
// (`outcome` on a sync result).
type Outcome string

const (
	// OutcomeOK is a lane whose steps all succeeded, or which needed none.
	OutcomeOK Outcome = "ok"

	// OutcomeFailed is a lane stopped at a vendor refusal or a fault. The
	// step is retried on every pass and on Retry.
	OutcomeFailed Outcome = "failed"

	// OutcomeWaiting is a lane stopped at a step only a person can take.
	OutcomeWaiting Outcome = "waiting"
)

// StepOutcome is one step a sync took, with whether it got done.
type StepOutcome struct {
	Step
	Done bool `json:"done"`
}

// Failure is why a sync stopped: the failed step's verb, the vendor's own
// reason, the remedy, who acts, and where.
type Failure struct {
	Step      StepVerb  `json:"step"`
	Said      string    `json:"said"`
	Remedy    string    `json:"remedy"`
	Actor     StepActor `json:"actor"`
	ActionURL string    `json:"action_url,omitempty"`
}

// SyncResult is what the last sync did for one seat's lane. The wire shape is
// the dashboard's (`sync`).
type SyncResult struct {
	At      time.Time     `json:"at"`
	Outcome Outcome       `json:"outcome"`
	Steps   []StepOutcome `json:"steps"`
	Error   *Failure      `json:"error,omitempty"`
}

// Refusal is a vendor's refusal of a step, which a vendor's Apply returns so
// the sync can name the vendor's reason, the remedy and who acts. Any other
// error from Apply is a fault (a transport failure, a 5xx): reported the same
// way, with the remedy to retry, because it may clear on its own.
//
// THE REASON IS THE VENDOR'S, AND NEVER A CREDENTIAL. Said ends up in the
// fleet's coordination store and on the access route, so a vendor quotes the
// vendor's message and never a value it sent.
type Refusal struct {
	Said      string
	Remedy    string
	Actor     StepActor
	ActionURL string
}

// Error implements error.
func (r *Refusal) Error() string {
	return fmt.Sprintf("%s (%s)", r.Said, r.Remedy)
}

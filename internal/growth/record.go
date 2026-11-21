package growth

import "slices"

// Step is one step of a growth act as the access log records it, one record
// per step (ADR-0171): a call made at once is one record; a held call is its
// ask, its answer and, once allowed, its outcome.
type Step string

// The steps.
const (
	// StepMade is a read or operate call that reached the vendor.
	StepMade Step = "made"
	// StepAsked is a public, spend or admin call put to an administrator.
	StepAsked Step = "asked"
	// StepAllowed and StepDenied are an administrator's answer.
	StepAllowed Step = "allowed"
	StepDenied  Step = "denied"
	// StepLapsed is the engine's denial of an ask nobody answered in a day.
	StepLapsed Step = "lapsed"
	// StepApplied is an allowed call made at the vendor.
	StepApplied Step = "applied"
	// StepVoid is an allowed call whose target moved since the ask: nothing
	// was sent.
	StepVoid Step = "void"
	// StepFailed is a call that did not reach its result ([Record.Failure]).
	StepFailed Step = "failed"
)

// Steps is every step, in the order an act takes them.
func Steps() []Step {
	return []Step{StepMade, StepAsked, StepAllowed, StepDenied, StepLapsed, StepApplied, StepVoid, StepFailed}
}

// Valid reports whether s is a step this build knows.
func (s Step) Valid() bool { return slices.Contains(Steps(), s) }

// Failure is why a growth call failed, in a closed set of words: never the
// vendor's text, which may quote what was sent or a credential.
type Failure string

// The failures.
const (
	// FailureVendorRefused is the vendor refusing the call: its validation,
	// its policy, or a state it would not change.
	FailureVendorRefused Failure = "vendor_refused"
	// FailureUnauthorized is the vendor refusing the credential.
	FailureUnauthorized Failure = "unauthorized"
	// FailureRateLimited is the vendor's rate limit or quota.
	FailureRateLimited Failure = "rate_limited"
	// FailureVendorUnavailable is a vendor that could not be reached or
	// answered with its own failure.
	FailureVendorUnavailable Failure = "vendor_unavailable"
	// FailureTimedOut is a call that ran past its deadline.
	FailureTimedOut Failure = "timed_out"
	// FailureUnknownOutcome is an answer that could not be read, so what the
	// call did is not known.
	FailureUnknownOutcome Failure = "unknown_outcome"
	// FailureCapExceeded is an allowed spend the spend guard refused at the
	// apply: nothing was sent.
	FailureCapExceeded Failure = "cap_exceeded"
	// FailureAccessLowered is an allowed call the seat's level or scope no
	// longer allows at the apply: nothing was sent.
	FailureAccessLowered Failure = "access_lowered"
	// FailureEngineError is the engine's own failure before the call.
	FailureEngineError Failure = "engine_error"
)

// Failures is every failure.
func Failures() []Failure {
	return []Failure{FailureVendorRefused, FailureUnauthorized, FailureRateLimited, FailureVendorUnavailable,
		FailureTimedOut, FailureUnknownOutcome, FailureCapExceeded, FailureAccessLowered, FailureEngineError}
}

// Valid reports whether f is a failure this build knows.
func (f Failure) Valid() bool { return slices.Contains(Failures(), f) }

// Record is one step of a growth act as a product hands it to the access log
// (internal/engine's GrowthAudit fills in its step). It never carries a token,
// a credential or a person's personal data: its values are the vendor's own
// fields, a published text goes as its digest and length, and people are
// counted, never named.
type Record struct {
	// Product is the integration kind, Connection the connection's id,
	// Account the vendor account and Target the object the act changes ("" for
	// an act on several, as a public ask's replies are).
	Product, Connection, Account, Target string
	// Tool is the engine tool called, and Class its class.
	Tool  string
	Class Class
	// Handle is the agent seat that called, and Turn the turn it called in.
	Handle, Turn string
	// Call is a call made at once's own id; HeldCall a held call's. One of the
	// two names the act.
	Call, HeldCall string
	// Answerer is the human seat of the administrator who allowed or denied a
	// held call; "" for every other step, a lapse included.
	Answerer string
	// Operation is the SHA-256, in lowercase hex, of the exact vendor
	// operation; Before the target's before-state's, "" where it held none.
	Operation, Before string
	Changes           []RecordChange
	// Money is a spend's figures, nil for an act that spends nothing.
	Money *CardMoney
	// Failure is a failed step's reason, "" for every other step.
	Failure Failure
}

// RecordChange is one field an act changes, before and after. A money value is
// integer micros of the account's currency as a decimal string. Text marks
// words the act publishes, which the log keeps as their SHA-256 and length,
// never the words.
type RecordChange struct {
	Field, Label, Before, After string
	Money, Text                 bool
}

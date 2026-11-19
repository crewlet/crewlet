// Package provision is the integration-agnostic half of minting credentials,
// and of asking a third-party app who they belong to.
//
// Every provisioning CLI does the same thing in a different third-party
// app's API: walk the company config for `${VAR}` references that name a
// credential, create or rotate that credential with the third-party app,
// and record the value where the engine will resolve it from. Only the
// middle step is integration-specific. This package is the other two.
//
// [ResolveConcurrently] is the same argument one step later: every
// third-party app's reconcile, and the engine's own credential resolvers,
// fan out one identity lookup per seat, and the bound on that fan-out is a
// property of talking to a third-party app rather than of any one of them.
// It lives here because this is the leaf all of them already share. See
// identity.go for what the number is anchored to.
package provision

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
)

// TokenSink records the credentials a provisioning run mints.
//
// Every method takes a context because a sink may persist REMOTELY (the
// encrypted secret store is a database write). Buffering and flushing at the
// end opens a window where a credential exists at the third-party app and
// nowhere else: a process dying there leaves a live, unrecorded token nobody
// knows to revoke. Write-through closes it.
//
// Discard is the other half: a run that mints three tokens and cannot persist
// the third must revoke all three and remove the two that WERE recorded, since
// a dead token reads exactly like a live one.
type TokenSink interface {
	// Record persists one minted credential under the variable that
	// references it. It returns only after the value is durable.
	Record(ctx context.Context, name, value string) error

	// Discard removes everything this run recorded, for a run that is
	// being rolled back. It is best effort — a sink that cannot be
	// reached to remove a value is a worse problem than the one being
	// cleaned up — and reports what it could not remove so the operator
	// can finish by hand.
	Discard(ctx context.Context) error

	// Forget removes values this run did NOT write, because the accounts
	// they belonged to have been removed.
	//
	// Discard is a ROLLBACK of this run's own writes; here a teardown deleted
	// the accounts, and their sealed credentials read like live ones. Not
	// best effort: a failure must fail the teardown, so the block stays, the
	// surface holds in PhaseDisconnecting and the next attempt retries; a
	// swallowed one drops the block with the credentials still sealed.
	//
	// ONLY WHAT A TEARDOWN NAMED: the names off a [Removed], a seat's OWN
	// credentials, never a company-level one, which survives a disconnect by
	// design and may be shared with another deployment.
	Forget(ctx context.Context, names ...string) error

	// Flush completes the run. A sink that batches finishes here; a
	// write-through one has nothing to do and says so.
	Flush(ctx context.Context) error

	// Value reads back what this sink carries for a variable.
	//
	// It makes a re-run safe: apps serve a credential once, so without it the
	// only option is minting fresh every run, an outage, since the engine
	// runs with the OLD value and rotating it revokes what every agent uses.
	//
	// It returns the VALUE, not a yes/no, because the honest test is using
	// the credential: "a value here and the account has some token" reads as
	// provisioned when an older env file was restored beside a live token.
	// A sink that persists nothing answers not-held: nothing was kept.
	Value(ctx context.Context, name string) (string, bool, error)

	// Describe names where the values went, for the run's report. It is
	// what tells an operator whether to go looking in a file or in the
	// store.
	Describe() string

	// NextStep is what still has to happen for a value recorded here to
	// reach a RUNNING engine, phrased as an instruction.
	//
	// Separate from Describe because the two are different questions and
	// the answers do not line up. "The encrypted secret store" is where a
	// value went AND requires no file to source — and a running engine
	// still will not see it, because the resolver is a snapshot rebuilt on
	// apply. A report that stopped at Describe told an operator who chose
	// -secret-store that they were finished, and the next delivery was
	// refused by a process holding the previous secret.
	NextStep() string
}

// ErrNoSink reports a run with nowhere to put what it mints.
//
// Refused UP FRONT rather than discovered after the first token: a
// provisioning run with no sink would mint live credentials at the
// third-party app and print none of them, which is the worst outcome
// available — every one of them has to be found and revoked by hand.
var ErrNoSink = errors.New("provision: name where minted credentials should go")

// ReferencedVars is the set of ${VAR} names one config value points at.
//
// A helper rather than a call to envref because the callers here ask a
// narrower question — "which variable does this seat's token live in" — and
// the answer has to be exactly one for a capture contract to make sense.
func ReferencedVars(value string) []string {
	return envref.Names(value)
}

// Shape says what is wrong with a value [SoleVar] refused, WITHOUT REPEATING
// IT.
//
// The value is a credential or holds one, and this sentence goes into an
// operator's report and `State.LastError`, written to the coordination store
// and served on the integrations query, anonymous unless turned off. A `%q`
// of the value there publishes the credential.
//
// A literal and a composite are kept apart because their fixes differ (a
// variable, or the variable as the whole value). An empty slot is said to be
// EMPTY, not "a literal", which sent operators looking for a value not there.
func Shape(value string) string {
	if strings.TrimSpace(value) == "" {
		return "empty"
	}
	if len(ReferencedVars(value)) == 0 {
		return "a literal"
	}
	return "a reference embedded in other text"
}

// FreshSwitch is a run's ask for a fresh value where it would otherwise keep
// the one the company document resolves (a webhook secret minted again, the
// only recovery for a lost one), spelled as its surface spells the switch:
// `-recreate-webhooks` on `crewlet github provision`, `-recreate-webhook` on
// jira's, `recreate_webhooks` in the body of the setup route. The zero value
// is not asked.
//
// The CALLER's spelling travels with the switch, since every surface runs the
// same vendor pass and a refusal must name it ([RefuseFresh]). ONE VALUE, not
// a switch and a spelling beside it: a pair would hold "asked, spelled
// nothing", a blank where the remedy belongs.
type FreshSwitch string

// Asked reports whether the run asked for a fresh value.
func (f FreshSwitch) Asked() bool { return f != "" }

// RefuseFresh is the refusal for a run that asked for a fresh value (fresh,
// as its surface spells the switch) over the config value ref at path, where
// ref is a literal or a reference embedded in other text. It is nil when the
// run did not ask, ref IS a whole reference (the fresh value is minted into
// it), or ref is empty (the caller's own refusal covers it).
//
// Without the switch such a value resolves and the pass registers it as it
// is; with it, the pass goes to [SoleVar] for a variable to mint into. The
// refusal for a slot that resolved to nothing is untrue of a literal and
// omits the remedy needing no document change, so the switch is named, and
// both ways on. THE SHAPE, NEVER THE VALUE ([Shape]): this sentence becomes
// State.LastError.
func RefuseFresh(path string, fresh FreshSwitch, ref string) error {
	if !fresh.Asked() {
		return nil
	}
	if _, whole := SoleVar(ref); whole || strings.TrimSpace(ref) == "" {
		return nil
	}
	return fmt.Errorf("%s is %s, a value the company document owns rather "+
		"than a whole ${VAR} reference, so %s has no variable to mint a fresh "+
		"one into: point %s at a whole ${VAR} and re-run with %s, or run "+
		"without %s to register the value it holds as it is",
		path, Shape(ref), fresh, path, fresh, fresh)
}

// SoleVar reports the one variable a value is a whole reference to.
//
// # Exactly one, and the WHOLE value
//
// This is the capture contract: a provisioner writes a minted credential into
// the variable the config points at, so the config value must BE the
// reference. Two cases are refused and reported, not guessed at:
//
//   - a LITERAL (`token: glpat-abc`): no variable to write into, and
//     overwriting it would edit the company config from a provisioning run;
//   - a COMPOSITE (`url: https://${HOST}/api`): the variable holds a
//     fragment, so minting a token into it would replace a hostname.
func SoleVar(value string) (string, bool) {
	return envref.Whole(strings.TrimSpace(value))
}

// Verdict is what probing a recorded credential concluded.
//
// FOUR OUTCOMES, not two, because the two that are easy to merge are the
// two that must not be: "the third-party app refused this credential" and "I
// could not reach the third-party app" lead to opposite actions.
type Verdict int

const (
	// VerdictUnknown means the probe could not reach a conclusion — a
	// transport failure, a 5xx. The seat is LEFT ALONE and reported:
	// re-minting on "cannot tell" destroys a credential that works, and
	// the recovery for one that does not is one -rotate away.
	VerdictUnknown Verdict = iota

	// VerdictSelf means the value authenticates as this seat's own account.
	// Nothing to do.
	VerdictSelf

	// VerdictRejected means the third-party app refused it. Whatever is in the
	// variable is not a credential, so minting is unambiguously right.
	VerdictRejected

	// VerdictOther means it authenticates as a DIFFERENT account. This is a
	// copy-pasted variable, and minting over it would hand this seat a
	// second identity while the other seat keeps authenticating as one
	// account from two places. It stops the run.
	VerdictOther
)

// Seat is one agent seat a provisioner has work to do for.
//
// The integration-specific scan produces these; everything below is shared.
// Held as a struct rather than passed as four arguments because a report
// groups by it and a rollback iterates it.
type Seat struct {
	// Handle is the seat: what a report prints, what `-handles` filters on,
	// and what its account at every third-party app is named after. A
	// handle is immutable (ADR-0013), so a name derived from it is stable
	// for the life of the seat.
	Handle string

	// Role is the seat's role name, which is what a third-party app account is
	// usually named after.
	Role string

	// TokenVar is the variable this seat's credential is written into.
	TokenVar string

	// Email is the address a third-party app account is created with, when the
	// third-party app needs one.
	Email string

	// AlsoVars are the other variables the same pass seals for this seat
	// that its tool reads beside the credential, which a launch waits on as
	// it waits on TokenVar.
	//
	// Atlassian's is why it exists. It names a service account itself, and
	// its product APIs authenticate that account as Basic
	// base64(address:token), so a seat holding only the token is refused,
	// and the address is not something anybody can write down in advance
	// because Atlassian invents it at creation time. Scaleway's is its API
	// key's access key, which the scw CLI refuses to run without.
	AlsoVars []string

	// AccessLevel is the membership level the company's settings give THIS
	// seat at an app that grades its members — GitLab's `developer` or
	// `maintainer` — and empty for the app's own default.
	//
	// RESOLVED BY THE SCAN, which is the one step holding the company's
	// settings.
	AccessLevel string
}

// Plan is what a provisioning run intends to do, before it does any of it.
//
// # Why a plan exists at all
//
// Every one of these runs is partly destructive at the third-party app: it
// creates accounts, rotates tokens that something is currently authenticating
// with, and removes seats a config no longer has. An operator needs to see
// that list before it happens, and a --dry-run that re-walks the config
// separately would be a second implementation that can disagree with the real
// one about what it was going to do.
type Plan struct {
	// Seats are the seats to provision, in a stable order.
	Seats []Seat

	// Notes are the drift and the caveats: a seat whose token is a
	// literal, a capability the third-party app does not offer. They do not stop
	// the run — they are what the report ends with.
	Notes []string
}

// Add records a seat, keeping the plan ordered by handle.
//
// ORDERED, because the report is read side by side with a previous run's
// and a plan whose order came from a map iteration cannot be compared with
// anything.
func (p *Plan) Add(s Seat) {
	p.Seats = append(p.Seats, s)
	slices.SortFunc(p.Seats, func(a, b Seat) int { return cmp.Compare(a.Handle, b.Handle) })
}

// Note records a caveat.
func (p *Plan) Note(format string, args ...any) {
	p.Notes = append(p.Notes, fmt.Sprintf(format, args...))
}

// Empty reports a plan with nothing to do.
func (p *Plan) Empty() bool { return len(p.Seats) == 0 }

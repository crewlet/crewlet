// Package provision is the integration-agnostic half of minting credentials.
//
// Every provisioning CLI does the same thing in a different vendor's API:
// walk the company config for `${VAR}` references that name a credential,
// create or rotate that credential with the vendor, and record the value
// where the engine will resolve it from. Only the middle step is
// vendor-specific. This package is the other two.
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
// # Every method is async, and that is not decoration
//
// A sink may persist REMOTELY — the encrypted secret store is a database
// write. Buffering in memory and flushing at the end opens a window where a
// credential exists at the vendor and nowhere else: if the process dies
// there, the token is live, unrecorded, and nobody knows to revoke it.
// Write-through closes that window, and a write-through sink needs a
// context.
//
// # Discard is the other half, and the one that is easy to leave out
//
// A run that mints three tokens and cannot persist the third has to revoke
// all three — a credential that exists and is not recorded is one nobody can
// use and nobody will remember to remove. But revoking them is not enough:
// the two that WERE recorded must be removed from the sink as well, because
// a dead token reads exactly like a live one, and an operator debugging the
// resulting 401s has no way to tell which of their credentials is real.
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

	// Flush completes the run. A sink that batches finishes here; a
	// write-through one has nothing to do and says so.
	Flush(ctx context.Context) error

	// Value reads back what this sink carries for a variable.
	//
	// # It is what makes a re-run safe to run
	//
	// A vendor that serves a credential once — which is all of them —
	// gives a provisioner no way to check that the value it recorded last
	// time still matches. Without this the only option is to mint fresh
	// every run, and that is an outage: the engine is running with the
	// OLD value, and rotating it revokes the credential every agent is
	// currently authenticating with. An operator adding one seat would
	// take the other nine down.
	//
	// It returns the VALUE rather than a yes/no because the honest test
	// is using the credential. "There is a value here and the account has
	// some token" reads as provisioned in exactly the case that matters:
	// an operator who restored an older env file has a stale value beside
	// a live token that is not it, and the seat then authenticates with
	// nothing.
	//
	// A sink that persists nothing answers not-held, which is correct
	// rather than a degradation: nothing was kept, so the operator needs
	// the value again.
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
// provisioning run with no sink would mint live credentials at the vendor
// and print none of them, which is the worst outcome available — every one
// of them has to be found and revoked by hand.
var ErrNoSink = errors.New("provision: name where minted credentials should go")

// ReferencedVars is the set of ${VAR} names one config value points at.
//
// A helper rather than a call to envref because the callers here ask a
// narrower question — "which variable does this seat's token live in" — and
// the answer has to be exactly one for a capture contract to make sense.
func ReferencedVars(value string) []string {
	return envref.Names(value)
}

// SoleVar reports the one variable a value is a whole reference to.
//
// # Exactly one, and the WHOLE value
//
// This is the capture contract: a provisioner mints a credential and writes
// it into the variable the config points at. That only works if the config
// value IS the reference. Two cases it must refuse:
//
//   - a LITERAL (`token: glpat-abc`) — there is no variable to write into,
//     and overwriting the literal would edit the company config from a
//     provisioning run, which is not this command's job;
//   - a COMPOSITE (`url: https://${HOST}/api`) — the variable holds a
//     fragment, so minting a token into it would replace a hostname.
//
// Both are configuration mistakes with a clear fix, so they are reported
// rather than guessed at.
func SoleVar(value string) (string, bool) {
	return envref.Whole(strings.TrimSpace(value))
}

// Verdict is what probing a recorded credential concluded.
//
// FOUR OUTCOMES, not two, because the two that are easy to merge are the
// two that must not be: "the vendor refused this credential" and "I could
// not reach the vendor" lead to opposite actions.
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

	// VerdictRejected means the vendor refused it. Whatever is in the variable
	// is not a credential, so minting is unambiguously right.
	VerdictRejected

	// VerdictOther means it authenticates as a DIFFERENT account. This is a
	// copy-pasted variable, and minting over it would hand this seat a
	// second identity while the other seat keeps authenticating as one
	// account from two places. It stops the run.
	VerdictOther
)

// Seat is one agent seat a provisioner has work to do for.
//
// The vendor-specific scan produces these; everything below is shared. Held
// as a struct rather than passed as four arguments because a report groups
// by it and a rollback iterates it.
type Seat struct {
	// Handle is the seat, for the report.
	Handle string

	// Role is the seat's role name, which is what a vendor account is
	// usually named after.
	Role string

	// TokenVar is the variable this seat's credential is written into.
	TokenVar string

	// Email is the address a vendor account is created with, when the
	// vendor needs one.
	Email string
}

// Plan is what a provisioning run intends to do, before it does any of it.
//
// # Why a plan exists at all
//
// Every one of these runs is partly destructive at the vendor: it creates
// accounts, rotates tokens that something is currently authenticating with,
// and removes seats a config no longer has. An operator needs to see that
// list before it happens, and a --dry-run that re-walks the config
// separately would be a second implementation that can disagree with the
// real one about what it was going to do.
type Plan struct {
	// Seats are the seats to provision, in a stable order.
	Seats []Seat

	// Notes are the drift and the caveats: a seat whose token is a
	// literal, a capability the vendor does not offer. They do not stop
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

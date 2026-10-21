package provision

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Secret is what [MintSecret] settled on for one company-level credential.
type Secret struct {
	// Value is what the caller should register with. Empty only where an
	// error was returned.
	Value string

	// Minted is whether a FRESH value was created on this run.
	//
	// It is the one fact a converged check cannot get anywhere else. Every
	// third-party app here takes a webhook signing secret write-only and
	// gives nothing back, so "the hook already points at the right
	// address" is only enough when this run did not change the key it has
	// to be signed with. A caller that ignores it leaves the app signing
	// with a key this deployment no longer holds.
	//
	// It is NOT the only way to answer that question, and where a better
	// one exists it should be used instead: a value the app itself stores
	// and returns settles it for every rotation rather than only the ones
	// this process made — gitlab's HookDigest puts a digest of the key in
	// the hook's description for exactly that reason.
	Minted bool
}

// MintSecret settles what a run should register a webhook with, and mints one
// only where there is genuinely nothing to use.
//
// The caller has already settled the resolver's answer ([Resolved]) and found
// nothing to use in it (or been told to recreate), and established that the
// field is a whole `${VAR}` it may write into — because the refusal for a
// value that is not names that vendor's own field and offers that vendor's own
// escape hatch. What is left is the sequence, and the sequence is the same
// everywhere.
//
// # Minting every run is an outage
//
// The engine is running with the OLD secret, and re-registering with a fresh
// one makes the third-party app sign every delivery with a key the running
// process does not hold — every delivery refused at the edge, from a pass
// whose whole promise is that it is safe to re-run. So a value that already
// resolves is used as it is, and minting happens when there is none or when
// the operator asked to recreate having planned the restart.
//
// # And "nothing to use" includes what this deployment already sealed
//
// The reconcile contract in internal/integration states it outright — check
// what the sink recorded and keep a working credential — and three of the four
// passes that mint a signing secret did not. The resolver answers from a
// SNAPSHOT taken at apply time, so the variable a previous pass minted into
// resolves to nothing until something rebuilds it; every pass in that window
// minted a second secret, sealed it over the first, and re-registered the
// app with it. The engine's own webhook route verifies with the snapshot, so
// each rotation moved the app further from the value the running process
// holds — on the reconcile loop's timer, for as long as the window lasted.
//
// So the sink is asked first, and only a name nothing holds is minted into.
// The read is THREE-VALUED like every other in this engine: held, definitively
// not held, and "the store could not say" — and the third RAISES, because
// minting over a value that may exist is the outage above.
//
// # And "nothing" has to be something the run could see
//
// The caller's empty answer is only a reason to mint where the resolver it
// came from could have answered otherwise. A `crewlet <vendor> provision` run
// on a node whose engine is stopped reads that node's own rows, and the
// fleet's store — which may well hold the variable — is out of its reach: its
// empty answer is UNKNOWN, the third value again, and minting on it re-points
// a working hook at a key no node holds. So unvouched is asked last, once the
// sink has nothing either, and a name it cannot vouch for is refused rather
// than minted into (see [Unvouched]). A value that run's environment filled in
// for such a name is no better a reason to register it, which [Resolved]
// settles before the caller gets here.
//
// # One implementation, because it was four and they disagreed
//
// jira asked the sink; gitlab, confluence and github did not. Four copies of
// one rule, three of them missing its first half.
//
// The caller supplies mint because the SHAPE is the one genuinely
// vendor-specific part: gitlab's self-hosted host requires a `whsec_` value
// over exactly 32 bytes, while the rest accept any unguessable string.
func MintSecret(
	ctx context.Context, sink TokenSink, name string, recreate bool,
	unvouched Unvouched, mint func() (string, error),
) (Secret, error) {
	if sink == nil {
		// THE COMMAND LINE'S CASE, and it stays a refusal: a run told to
		// mint with nowhere to put the result would leave a live signing
		// secret at the third-party app and print none of it.
		return Secret{}, ErrNoSink
	}
	if !recreate {
		// ASKED BEFORE ANYTHING IS MINTED, and skipped only where the
		// operator asked for a fresh key having planned the restart that
		// rotating one costs.
		held, ok, err := reusable(ctx, sink, name, unvouched)
		if err != nil {
			return Secret{}, err
		}
		if ok {
			return Secret{Value: held}, nil
		}
	}
	fresh, err := mint()
	if err != nil {
		return Secret{}, err
	}
	if err := sink.Record(ctx, name, fresh); err != nil {
		return Secret{}, fmt.Errorf("record %s: %w", name, err)
	}
	return Secret{Value: fresh, Minted: true}, nil
}

// Resolved settles the answer a run's resolver gave for ref — the config value
// naming a webhook or signing secret — before the run registers it with a
// vendor or mints in its place: the value to register and true where there is
// one; nothing and no error where there is none and the caller may mint
// ([MintSecret]); and an error where the run cannot say.
//
// # An answer the run cannot vouch for is no answer, empty or not
//
// A command line run on a node whose engine is stopped reads that node's own
// rows and the ENVIRONMENT behind them, as the engine resolves the store and
// then its own environment. For a variable the rows lack, what this shell
// exports is a guess: the fleet's store, out of the run's reach, may hold
// another value — and the store outranks the environment in the engine, so the
// running process verifies with the store's. Registering the shell's value
// re-points a working hook at a key no node holds, which is the very outage
// refusing the mint prevents; only the answer's emptiness differs. So a
// non-empty answer is asked about exactly as an empty one is: where unvouched
// cannot vouch for every variable ref reads, the sink answers first — what
// the run's own record holds is what it would register anyway — and nothing
// in it is refused rather than registered.
//
// Exported so a command that plans before it touches anything can refuse up
// front on exactly the terms the pass itself will, rather than on a second
// copy of them.
func Resolved(ctx context.Context, sink TokenSink, ref, resolved string, unvouched Unvouched) (string, bool, error) {
	resolved = strings.TrimSpace(resolved)
	unknown := unvouched.Ref(ref)
	if unknown == nil {
		return resolved, resolved != "", nil
	}
	name, whole := SoleVar(ref)
	if !whole || sink == nil {
		// NOTHING TO READ BACK: a value embedded in other text has no
		// variable a sink records, and a run with no sink holds no record.
		return "", false, unknown
	}
	return reusable(ctx, sink, name, unvouched)
}

// reusable is what [MintSecret] settles before it would mint into name: the
// value the sink already holds, ok where there is one to use; nothing and no
// error where nothing holds it AND the run can vouch for that; and an error
// where it cannot say — the sink could not be read, or unvouched cannot vouch
// for the run's own answer for name.
func reusable(ctx context.Context, sink TokenSink, name string, unvouched Unvouched) (string, bool, error) {
	held, ok, err := sink.Value(ctx, name)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", name, err)
	}
	if held = strings.TrimSpace(held); ok && held != "" {
		return held, true, nil
	}
	if err := unvouched.Of(name); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// ErrUnvouched marks a variable whose value a run cannot vouch for: what the
// store holds under it is unknown, because the store that would say is out of
// the run's reach — so the run's answer, empty or filled from its environment,
// is neither the value the deployment uses nor proof that it uses none. Every
// refusal an [Unvouched] answers wraps it.
var ErrUnvouched = errors.New("what the store holds for it is unknown to this run")

// Unvouched answers, for a variable a run resolved, why the run's answer for
// it — empty, or filled from the run's own environment — is not one it can act
// on as the deployment's value: an error wrapping [ErrUnvouched], naming the
// variable, or nil where it is.
//
// # Why a resolver's answer needs a second question
//
// A resolver answers a string, and the string cannot say where it came from.
// "" covers two different facts — nothing holds the variable, and the run
// could not see the store that would — and a value covers two more: the
// store's own, and what the environment behind the store supplied for a name
// the store was not seen to hold. The engine's own pass resolves against a
// snapshot of the WHOLE secret store, so for it the facts never differ and it
// passes no Unvouched at all. A command line run on a node whose engine is
// stopped reads only that node's own rows, while the fleet's store holds
// nearly every value — so for it they differ for every variable those rows
// lack, and the two decisions that turn on the difference — minting into an
// empty variable, and registering a value with a vendor as the deployment's —
// must ask ([Resolved]).
//
// A NIL Unvouched vouches for every answer.
type Unvouched func(name string) error

// Of is u's answer for name, nil where u is nil.
func (u Unvouched) Of(name string) error {
	if u == nil {
		return nil
	}
	return u(name)
}

// Ref is u's answer for a config value: the first refusal among the variables
// ref reads, in the order it reads them, or nil where u vouches for every one —
// so always for a literal, which reads none.
func (u Unvouched) Ref(ref string) error {
	for _, name := range ReferencedVars(ref) {
		if err := u.Of(name); err != nil {
			return err
		}
	}
	return nil
}

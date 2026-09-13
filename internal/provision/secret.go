package provision

import (
	"context"
	"fmt"
	"strings"
)

// Secret is what [MintSecret] settled on for one company-level credential.
type Secret struct {
	// Value is what the caller should register with. Empty only where
	// NoKeyring is set or an error was returned.
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

	// NoKeyring is a run that had to mint and had nowhere to seal the
	// result. It is a POSTURE TO REPORT, not an error: no pass will ever
	// succeed until somebody sets secrets.keys, so raising it makes the
	// loop retry for ever while telling an operator the engine is working
	// on it. See [ReadOnly].
	NoKeyring bool
}

// MintSecret settles what a run should register a webhook with, and mints one
// only where there is genuinely nothing to use.
//
// The caller has already resolved the config value, found it empty (or been
// told to recreate), and established that the field is a whole `${VAR}` it
// may write into — because the refusal for a value that is not names that
// vendor's own field and offers that vendor's own escape hatch. What is left
// is the sequence, and the sequence is the same everywhere.
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
// # One implementation, because it was four and they disagreed
//
// jira asked the sink and reported the keyring; gitlab asked neither;
// confluence and github asked neither and faulted for ever on a node with no
// keyring. Three copies of one rule, each missing a different half of it.
//
// The caller supplies mint because the SHAPE is the one genuinely
// vendor-specific part: gitlab's self-hosted host requires a `whsec_` value
// over exactly 32 bytes, while the rest accept any unguessable string.
func MintSecret(
	ctx context.Context, sink TokenSink, name string, recreate bool,
	mint func() (string, error),
) (Secret, error) {
	if sink == nil {
		// THE COMMAND LINE'S CASE, and it stays a refusal: a run told to
		// mint with nowhere to put the result would leave a live signing
		// secret at the third-party app and print none of it.
		return Secret{}, ErrNoSink
	}
	if !CanMint(sink) {
		return Secret{NoKeyring: true}, nil
	}
	if !recreate {
		// ASKED BEFORE ANYTHING IS MINTED, and skipped only where the
		// operator asked for a fresh key having planned the restart that
		// rotating one costs.
		held, ok, err := sink.Value(ctx, name)
		if err != nil {
			return Secret{}, fmt.Errorf("read %s: %w", name, err)
		}
		if held = strings.TrimSpace(held); ok && held != "" {
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

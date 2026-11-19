package provision

import (
	"context"
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
	// The one fact a converged check cannot get elsewhere: every app here
	// takes a webhook signing secret write-only, so "the hook points at the
	// right address" suffices only when this run did not change the key. A
	// caller that ignores it leaves the app signing with a key this
	// deployment no longer holds.
	//
	// Prefer a value the app itself returns, which settles every rotation,
	// not only this process's: gitlab's HookDigest keeps a digest of the key
	// in the hook's description.
	Minted bool
}

// MintSecret settles what a run should register a webhook with, and mints one
// only where there is genuinely nothing to use.
//
// The caller has already resolved the config value, found it empty (or been
// told to recreate), and established that the field is a whole `${VAR}` it
// may write into. Minting every run is an outage: the engine runs with the
// OLD secret, so a fresh one makes the app sign every delivery with a key the
// process does not hold. A value that resolves is used as it is.
//
// The caller supplies mint because the SHAPE is vendor-specific: gitlab's
// self-hosted host requires a `whsec_` value over exactly 32 bytes, the rest
// accept any unguessable string.
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
	if !recreate {
		// ASKED BEFORE ANYTHING IS MINTED, skipped only where the operator
		// asked for a fresh key having planned the restart it costs. The
		// resolver answers from a SNAPSHOT taken at apply time, so a variable
		// a previous pass minted resolves to nothing until something rebuilds
		// it, and each pass in that window would seal another secret over the
		// first. The read is THREE-VALUED (held, definitively not held, "the
		// store could not say") and the third RAISES: minting over a value
		// that may exist is the outage.
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

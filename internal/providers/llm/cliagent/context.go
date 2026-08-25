package cliagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// callKey is a private context key type so nothing outside this package
// can write the value by another route — the same rule internal/workkey
// follows, for the same reason.
type callKey struct{}

// WithCall binds an identifier for one call, which names its scratch
// directory.
func WithCall(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callKey{}, id)
}

// CallOf is the call id bound to this context, or a fresh random one.
//
// Fresh rather than fixed: the id names a working directory that is created
// empty and removed afterwards, so two concurrent unbound calls sharing one
// name would delete each other's scratch mid-run. Sub-agents batched inside
// one seat are exactly that case.
func CallOf(ctx context.Context) string {
	if ctx != nil {
		if id, _ := ctx.Value(callKey{}).(string); id != "" {
			return id
		}
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand does not fail on any supported platform, and a
		// collision here costs one call's scratch directory rather than
		// any correctness — so a fixed name beats refusing the call.
		return "call"
	}
	return hex.EncodeToString(raw[:])
}

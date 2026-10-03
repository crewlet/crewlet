package api

import (
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// CredentialFeed is where this node hears whose credentials the identity
// estate moved: the engine, whose identity applier says so after every
// committed batch, and after an adoption that replaced the estate's rows.
//
// CONSUMER-DEFINED and one method wide. The engine satisfies it with
// [engine.Engine.SetOnIdentityMoved]; a test hands in a feed it fires by hand.
type CredentialFeed interface {
	SetOnIdentityMoved(func(iamdomain.Moved))
}

// pushIdentityMoved decides again every open socket whose credential a
// committed identity batch moved — see internal/api/stream's lifetime.go.
//
// NO FORWARDING BETWEEN NODES, and none is missing: every node applies every
// identity record, whatever its roles, so every node hears every move from its
// own applier and decides the sockets it holds.
//
// NEVER BLOCKS, because its caller is the identity applier's post-commit hook,
// on the apply loop's own goroutine: the service only signals each socket it
// names, and each socket decides on its own goroutine.
func (a *App) pushIdentityMoved(moved iamdomain.Moved) {
	a.stream.CredentialsMoved(stream.Moved{
		People:   moved.People,
		Sessions: moved.Sessions,
		Everyone: moved.Everyone,
	})
}

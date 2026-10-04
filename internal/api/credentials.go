package api

import (
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
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

// pushIdentityMoved hands every open socket the identity move it is reached
// by — see internal/api/stream's lifetime.go.
//
// NO FORWARDING BETWEEN NODES, and none is missing: every node applies every
// identity record, whatever its roles, so every node hears every move from its
// own applier and decides the sockets it holds.
//
// NEVER BLOCKS, because its caller is the identity applier's post-commit hook,
// on the apply loop's own goroutine: the service only signals each socket, and
// each socket acts on its own goroutine.
func (a *App) pushIdentityMoved(moved iamdomain.Moved) {
	a.stream.CredentialsMoved(stream.Moved{
		People:   moved.People,
		Logins:   moved.Logins,
		Sessions: moved.Sessions,
		Everyone: moved.Everyone,
	})
}

// seatOf is what the company this node has published says about the seat name
// addresses — by [org.Organization.Role]'s rule, which resolves a live handle,
// the handle a seat was created under and a handle it gave up — read from the
// roster in memory. No published company holds no seat.
func seatOf(company func() (*config.Company, *org.Organization),
	name string) (stream.SeatState, bool) {

	_, roster := company()
	if roster == nil {
		return stream.SeatState{}, false
	}
	role := roster.Role(name)
	if role == nil {
		return stream.SeatState{}, false
	}
	return stream.SeatState{Origin: role.Origin(), Handle: role.Handle(),
		Human: role.IsHuman()}, true
}

// Package derive turns what the engine already keeps into atlas claims: pure
// functions from the company settings and from the engine's own records to
// [atlas.Claim]s, computed every time the atlas is read and never stored.
//
// # The settings are read redacted, and only redacted
//
// [Settings] holds the redacting copy of the company document and nothing
// else, and [FromSettings] is the only way to make one, so no derivation can
// reach a credential even by reading the wrong field
// (TestTheSettingsTheAtlasReadsHoldNoSecret). A ${VAR} reference is never
// resolved: the atlas reads the settings as they are written.
//
// # What is not read, and what is withheld
//
// Each derivation maps the fields a map needs into typed facts, so a field
// nobody mapped never reaches the atlas: a tool server's arguments, a
// repository's description, a seat's credentials, a person's email address.
// A mapped value its fact's grammar refuses (a label shaped like a token, an
// address carrying credentials) is withheld: named by where it sits and why,
// counted, never shown ([Withheld]).
//
// # Inputs are closed projections
//
// A record the engine keeps elsewhere reaches this package as a projection
// declared here ([GitletRepo], [GitletCheck]): the fields a map needs and
// nothing more. internal/atlas/gather reads the records and fills them, so
// this package reads no store and calls nothing (TestDeriveImportsNoWritePath).
//
// # The engine's own infrastructure
//
// fleet.go draws the engine from its live nodes' presence ([FleetNode]): each
// node, the broker, the object store, and the service over them. An address
// the fleet says is an administrator's and always private (ADR-0142).
package derive

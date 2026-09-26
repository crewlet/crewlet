// Package jsapi decides which JetStream API a connection speaks.
//
// # Why there are two
//
// A node reaches JetStream in one of three ways, and they are two APIs:
//
//   - A MEMBER of the engine's own embedded fleet — solo, or one of a
//     clustered set — runs JetStream in its own process.
//   - A LEAF of that fleet — a node that keeps no durable state, holds no
//     stream replica and is in no raft group — runs a broker with JetStream
//     switched OFF and reaches the members' JetStream across its leaf link.
//   - A CLIENT of an external cluster dials somebody else's broker.
//
// A leaf's own broker answers nothing on `$JS.API.>`: that subject is served
// by a JetStream in the connection's account, and a leaf has none. What
// crosses a leaf link is a JetStream DOMAIN — `$JS.<domain>.API.>` — which a
// member serves for its whole account and a leaf forwards. So every member of
// the embedded fleet serves one fixed domain, [Domain], and every client of
// that fleet addresses it: a member's own clients, whose local server answers
// the domain's subjects exactly as it answers the plain ones, and a leaf's,
// for whom it is the only JetStream there is. One API on every node of the
// fleet, so no caller ever has to know which kind of node it is on — the
// shape that fails is the one where a caller picks, because the wrong pick is
// a node that boots, reaches its broker, and times out on its first stream.
//
// An external cluster is the other API. Its domain, if it has one, is its
// operator's; the engine addresses the connection account's own JetStream,
// which is what `stream.type: nats` has always meant.
//
// # Why it is a package
//
// Five subsystems build a JetStream client over a connection they were
// handed — the queue, both coordination stores, the memory changelog and the
// backup — and a rule written five times is five places to disagree (see
// adr/0008). Measured on this build's broker: a leaf's client on the plain API
// timed out on its first call, and the same client on the domain created a
// stream, published under a per-subject expectation, fetched from a durable
// consumer and compare-and-set a KV key.
package jsapi

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Domain is the JetStream domain every broker of the engine's own embedded
// fleet serves.
//
// A CONSTANT rather than a setting, because it names nothing an operator has
// to choose between: it is the fleet's internal address for its own
// JetStream, every member must agree on it or a leaf reaches half of them,
// and it never leaves the fleet. A knob here would be one that can only be
// set wrong.
const Domain = "crewlet"

// ErrUnnamed reports an API nobody chose.
var ErrUnnamed = errors.New("jsapi: no JetStream API was named — a connection " +
	"is to the embedded fleet (Embedded) or to an external cluster (Account)")

// API is the JetStream a client addresses.
//
// THE ZERO VALUE IS REFUSED rather than read as either API. Read as the
// account's own it works on every member of the embedded fleet and times out
// on every leaf; read as the domain it times out on every external cluster.
// Both are a node that boots and then fails on first use, so a caller that
// forgot to say is told at construction instead.
type API struct {
	domain string
	named  bool
}

// Embedded is the API of the engine's own embedded fleet: its domain.
func Embedded() API { return API{domain: Domain, named: true} }

// Account is the API of the connection account's own JetStream — an
// external cluster the engine dials.
func Account() API { return API{named: true} }

// Named reports whether this API was chosen.
func (a API) Named() bool { return a.named }

// Domain is the JetStream domain this API addresses, empty for an account's
// own.
func (a API) Domain() string { return a.domain }

// String names the API for a log line.
func (a API) String() string {
	switch {
	case !a.named:
		return "unnamed"
	case a.domain == "":
		return "account"
	}
	return "domain " + a.domain
}

// Client is a JetStream client over nc that speaks this API.
func (a API) Client(nc *nats.Conn) (jetstream.JetStream, error) {
	switch {
	case !a.named:
		return nil, ErrUnnamed
	case nc == nil:
		return nil, errors.New("jsapi: a NATS connection is required")
	case a.domain == "":
		return jetstream.New(nc)
	}
	return jetstream.NewWithDomain(nc, a.domain)
}

// accountPrefix is the prefix of every subject the account's own JetStream
// answers.
const accountPrefix = "$JS.API."

// Subject turns a subject of the account's own API — one of nats-server's
// `JSApi…` constants, which is what a caller reaching past the client uses —
// into the subject this API answers the same request on.
//
// For the ONE caller that reaches past the client: the backup's stream
// snapshot, a request the client library has no call for.
func (a API) Subject(subject string) (string, error) {
	switch {
	case !a.named:
		return "", ErrUnnamed
	case !strings.HasPrefix(subject, accountPrefix):
		return "", fmt.Errorf("jsapi: %q is not a JetStream API subject", subject)
	case a.domain == "":
		return subject, nil
	}
	return "$JS." + a.domain + ".API." + strings.TrimPrefix(subject, accountPrefix), nil
}

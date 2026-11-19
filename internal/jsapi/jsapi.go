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
//
// # And the requests a caller makes past the client
//
// A caller that needs a request the client does not make in the shape it
// needs makes it through this package, addressed here: a stream leader's read
// of one message ([Leader]), the paged listings ([Leader.Subjects], [Meta]),
// and a reader's pulls of a consumer it made for one read ([Pulls]) — asked by
// hand on one inbox the reader holds for the whole read, every word the server
// sends about a pull named ([Event]), and which of them ends it counted as the
// server counts it, so one rule serves every reader that pulls a consumer
// itself (internal/queue/jetstream's copy reads and dead-letter listings; its
// domain consumers' standing pull reads its statuses with [PullStatus]).
//
// # The leader's reads, and their time
//
// A direct get can be answered "not found" by a replica that is behind, so a
// read whose "none" must be true goes to the stream's leader, and
// [ErrNoMessage] is only the leader's word. Only the leader answers, and a
// member that still believes in a leader its group just lost takes the request
// and says nothing, so no read is asked once for its caller's deadline or the
// client's five seconds: it is hedged ([jsprovision.ReadBoundedWith]), and
// each page of a listing is its own hedged read, never the whole listing,
// which would restart from its first page every two seconds.
//
// How long a read waits is ONE rule per direction, so callers do not size it
// apiece. A reply that may be a whole large message is given the time it takes
// to ARRIVE ([Arrival]: its handling in the engine's processes, which the race
// detector slows by orders of magnitude, and its crossing of a link at the
// floor), or each re-ask builds another whole reply beside the first. A write
// crosses raw ([Sending], [WriteBound], [Budget]), each send after the first
// given one more crossing up to [WriteSends], and the end is fixed when the
// budget starts, since counting per send gave a write met by an in-flight
// answer a crossing every time and no end. A reply past the transport never
// crosses a leaf's link, which the far server closes whole ([ReplyBytes]), so
// such a message is read raw from a consumer instead ([Leader.Leading]).
//
// # A window across a link
//
// [WindowCrossing] sizes what a node's bulk transfers hold out across a link
// at once, because what waits behind them is any request sent after them on
// the link, or whose answer crosses back after them: a read, a small write or
// a lease renewal on the same connection, the coordination store's, which
// every lease renewal of the node rides, or on another connection crossing
// the same link or a bottleneck on it that queues first in first out. Every
// one of them is held to [RequestTerm], whatever lease the node holds, and one
// not answered inside it is unknown. One read term is the least the request's
// own round trip is given anywhere ([jsprovision.ReadTerm]).
//
// THE SUM OF BOTH DIRECTIONS is what such a request waits behind: its bytes
// queue behind what the node sent, and its answer behind what the link is
// bringing back. So a link has ONE window ([Window], [LinkWindow]), owned by
// its queue's connection and shared first come first served by the object
// store's puts, its downloads' and walks' pulls, and a memory replay's pulls;
// one window each way held such a request 7.75 seconds at the floor
// (ADR-0161).
package jsapi

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
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

// RequestTerm is how long one request of a client [API.Client] builds waits
// for its answer when its caller set no deadline: the client's DefaultTimeout,
// the client library's own five seconds, stated here so a release cannot move
// what the engine is sized against: a hedged read's bound ([Leader]), a
// coordination compare-and-set's wait on a leader's in-flight answer
// (internal/jsinflight's Decide), boot steps and duty ticks.
//
// A write's acknowledgement is milliseconds (a raft commit, ~1.5 ms idle), so
// a request unanswered at five seconds will never be answered. A write cannot
// be re-asked (a second publish is a second write), so it holds ONE request;
// a read is re-asked at none, two and four seconds ([jsprovision.ReAsk]). A
// WRITE ACROSS A LINK IS GIVEN MORE ([WriteSends], [WriteBound], [Budget]).
const RequestTerm = 5 * time.Second

// WindowCrossing is the longest the bytes a link's window holds out, both
// directions together, may take to cross the link and still leave a request
// sent behind them answered inside its [RequestTerm]: the term less one
// [jsprovision.ReadTerm], the request's own round trip, four seconds. The
// window holds no more than the slowest link the engine allows
// (objstore.MiBPace) carries in that ([LinkWindow]). Sized by the request's
// term, never a lease's heartbeat (the package doc, "A window across a link").
const WindowCrossing = RequestTerm - jsprovision.ReadTerm

// Client is a JetStream client over nc that speaks this API, its every
// request held to [RequestTerm] where its caller set no deadline.
func (a API) Client(nc *nats.Conn) (jetstream.JetStream, error) {
	switch {
	case !a.named:
		return nil, ErrUnnamed
	case nc == nil:
		return nil, errors.New("jsapi: a NATS connection is required")
	case a.domain == "":
		return jetstream.New(nc, jetstream.WithDefaultTimeout(RequestTerm))
	}
	return jetstream.NewWithDomain(nc, a.domain, jetstream.WithDefaultTimeout(RequestTerm))
}

// accountPrefix is the prefix of every subject the account's own JetStream
// answers.
const accountPrefix = "$JS.API."

// Subject turns a subject of the account's own API (one of nats-server's
// `JSApi…` constants) into the subject this API answers the same request on,
// for callers reaching past the client so a leaf of the embedded fleet reaches
// the same JetStream a member does:
//
//   - the backup's stream snapshot, which the client has no call for;
//   - `STREAM.MSG.GET` from a stream's LEADER ([Leader]): on a stream allowing
//     direct gets the client sends only `DIRECT.GET`, which a lagging replica
//     answers too;
//   - coord/kv's marker sweep, a `STREAM.PURGE` bounded at a marker's revision,
//     whose answer carries how many messages went, which the client drops.
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

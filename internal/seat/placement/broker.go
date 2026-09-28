package placement

import "slices"

// BrokerKind is how a node's broker takes part in the fleet's: a member of the
// JetStream cluster, a leaf of those members, or a plain client of a cluster
// somebody else runs.
//
// # Why it rides the presence profile beside the roles
//
// Holding the company's durable state and holding a vote in the broker were
// one fact once — `data` meant both — and they are two now: a data node may
// reach the broker as a leaf, and a member may hold no data. Every question
// that is really about the BROKER has to be asked of this rather than of the
// roles, and the one that forced it is the capacity seal. Its proof is that
// every broker process restarted, because what retires a request the broker
// has already queued is the process holding it going away; a member that
// holds no data holds queued requests all the same, and a seal counted over
// data nodes would pass without it.
//
// DERIVED, NEVER DECLARED. A node's kind is what its Tier A stream block
// makes it — see config.Bootstrap.BrokerKind — so there is no setting that
// could disagree with the broker the process actually started.
//
// # Three values and an absence
//
// The zero value is [BrokerUnknown]: a presence row that says nothing about
// its broker, which is what a build older than the field writes. It is NOT a
// leaf and not a client. A reader that has to count members treats it as one,
// because leaving out a node that might be a member is the reading that can
// pass a seal it should hold, while counting one that is not merely waits for
// an acknowledgement an operator can exclude.
type BrokerKind string

const (
	// BrokerMember runs JetStream in this process, holds stream replicas
	// and votes in the metadata group: an embedded broker that joins no
	// leaf link.
	BrokerMember BrokerKind = "member"

	// BrokerLeaf runs an embedded broker with JetStream switched off, which
	// reaches the members' across a leaf link. It holds no replica and
	// votes in nothing.
	BrokerLeaf BrokerKind = "leaf"

	// BrokerClient dials an external NATS cluster (`stream.type: nats`).
	// Nothing about that cluster's membership is this fleet's to know or
	// change.
	BrokerClient BrokerKind = "client"

	// BrokerUnknown is a presence row that does not say — see [BrokerKind].
	BrokerUnknown BrokerKind = ""
)

// brokerKinds is every kind a node can advertise, in the order a surface lists
// them.
var brokerKinds = []BrokerKind{BrokerMember, BrokerLeaf, BrokerClient}

// BrokerKinds is every kind a node can advertise — a fresh slice per call, so
// the dashboard's copy of the list can be held against this one.
func BrokerKinds() []BrokerKind { return slices.Clone(brokerKinds) }

// Valid reports whether a kind off the wire is one a node can advertise.
// [BrokerUnknown] is not: it is what a reader makes of a row that advertised
// nothing it knows.
func (b BrokerKind) Valid() bool { return slices.Contains(brokerKinds, b) }

// String is the kind as an operator reads it: its wire value, or "unknown" for
// a row that did not say — never the empty string, which a screen renders as
// nothing at all.
func (b BrokerKind) String() string {
	if b.Valid() {
		return string(b)
	}
	return "unknown"
}

// brokerFromMeta reads a peer's advertised kind, failing to [BrokerUnknown]
// on anything this build does not know — an absent key, a value of the wrong
// type, or a kind a newer build added. Never to a leaf: see [BrokerKind].
func brokerFromMeta(raw any) BrokerKind {
	s, ok := raw.(string)
	if !ok {
		return BrokerUnknown
	}
	if kind := BrokerKind(s); kind.Valid() {
		return kind
	}
	return BrokerUnknown
}

package placement

import "slices"

// BrokerKind is how a node's broker takes part in the fleet's: a member of the
// JetStream cluster, a leaf of those members, or a plain client of a cluster
// somebody else runs.
//
// It rides the presence profile because holding data and holding a broker vote
// are two facts (a data node may be a leaf, a member may hold no data), so
// BROKER questions, such as the capacity seal's proof that every broker process
// restarted, are asked of this and not the roles. DERIVED, NEVER DECLARED: it
// is what the Tier A stream block makes it (config.Bootstrap.BrokerKind). The
// zero value is [BrokerUnknown], NOT a leaf or a client: a reader counting
// members counts it as one, since omitting a possible member can pass a seal
// that should hold.
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

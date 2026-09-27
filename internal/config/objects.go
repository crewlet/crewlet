package config

import (
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// Objects is the company's object store: how many copies of every file chunk
// it keeps, and what those copies are spread across.
//
// # Why this is the company's and not the node's
//
// The copies a file has are a decision about the company's DATA, taken once
// for the whole fleet. It used to be read off whichever node held the
// placement map's duty — that node's own `stream.replicas` — and the duty moves
// between nodes on a lease: the day it landed on a node left at the default of
// one, every group dropped to a single copy and every collector in the fleet
// deleted the rest. A Tier B value is one document every node applies, stamped
// with the activation it came from, so a node a revision behind cannot set it
// back.
//
// What stays on the node is what is genuinely a fact about the node — where
// its chunks live and how large a share its disk offers (`store.objects`, Tier
// A) — and the labels its failure domain is read from (`node.labels`).
type Objects struct {
	// Replicas is how many copies of every chunk the fleet keeps, on that
	// many different data nodes. Zero takes [DefaultObjectReplicas].
	//
	// A fleet with fewer data nodes keeps one copy per node rather than
	// refusing to store anything, and reaches the full count as nodes join.
	Replicas int `yaml:"replicas,omitempty" json:"replicas,omitempty" js:"min=0;max=10" desc:"Copies of every file chunk, each on a different data node, 1..10; 0 is the default, 3. A fleet with fewer data nodes keeps one copy on each."`

	// FailureDomain is a node label KEY (one written under `node.labels` on
	// every data node — `zone`, `rack`, `host`): no two copies of a chunk
	// are placed on nodes sharing that label's value, as long as there are
	// enough distinct values to go round. Empty spreads copies across nodes
	// with no further constraint.
	//
	// A data node that does not carry the label counts as a domain of its
	// own, so a half-labelled fleet degrades to node-level spreading rather
	// than refusing to place — and `crewlet validate` warns on the node
	// missing it ([TierWarnings]).
	FailureDomain string `yaml:"failure_domain,omitempty" json:"failure_domain,omitempty" desc:"A node label key (e.g. zone); no two copies of a chunk share its value when enough values exist. Empty spreads across nodes only."`
}

// DefaultObjectReplicas is the copies a company keeps when it names none.
//
// THREE, Ceph's default pool size, for Ceph's reason: it is the smallest count
// that survives losing one copy WHILE a second is being rebuilt. At two, the
// window in which a member is being replaced is a window with one copy, and a
// disk failing inside it loses data; at three it costs a second simultaneous
// failure.
const DefaultObjectReplicas = 3

// MaxObjectReplicas is the most copies a company may ask for — the placement
// map's own ceiling, referenced rather than restated, because a count this
// accepted and the map refused would be a revision every node applies and no
// maintainer can write.
const MaxObjectReplicas = objplacement.MaxReplicas

// ReplicaCount is the copies the company keeps, with the default applied.
func (o *Objects) ReplicaCount() int {
	if o.Replicas == 0 {
		return DefaultObjectReplicas
	}
	return o.Replicas
}

func (o *Objects) validate(path Path) error {
	var p problems
	if o.Replicas < 0 || o.Replicas > MaxObjectReplicas {
		p.add(at(path, "replicas"), ErrOutOfRange,
			"must be 0 (the default, %d) or 1..%d, got %d: every write sends "+
				"this many copies across the broker, and past %d a write costs "+
				"more than any failure it survives", DefaultObjectReplicas,
			MaxObjectReplicas, o.Replicas, MaxObjectReplicas)
	}
	// THE LABEL GRAMMAR node.labels IS HELD TO, and exactly it: the key is
	// matched against every data node's labels, so one this accepted and a
	// node's labels could never carry would spread copies across nothing.
	if o.FailureDomain != "" {
		if err := placement.CheckLabelKey(o.FailureDomain); err != nil {
			p.add(at(path, "failure_domain"), ErrUnknownValue,
				"%v — it names the node label (under node.labels on every "+
					"data node) whose values copies are spread across", err)
		}
	}
	return p.err()
}

package config

import (
	mapplacement "github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// Estate is the company's replicated estate — its tracker, its knowledge base
// and their search vectors — as the estate map places it once a layout divides
// it into partitions: how many copies of each partition it keeps, and what
// those copies are spread across.
//
// # Why this is the company's and not the node's
//
// For [Objects]' reason, which cost that block a fleet's copies: the copies a
// partition has are a decision about the company's data, taken once for the
// whole fleet, and a value read off whichever node holds the map's duty would
// move with the duty. A Tier B value is one document every node applies,
// stamped with the activation it came from, so a node a revision behind cannot
// set it back (ADR-0020). What stays on the node is what is a fact about the
// node: the share its disk offers (`store.estate.weight`, Tier A) and the
// labels its failure domain is read from (`node.labels`).
//
// # What it does while the estate is one file
//
// NOTHING, and that is stated rather than hidden. Under the single-file layout
// — layout 0, the one this build runs — every data node holds the whole
// estate, so there is no partition to place and no estate map to place it:
// the map's duty reads this block on every tick, finds no partitioned layout,
// and writes nothing. The block is validated and carried now so that the
// revision a company is running when its estate is partitioned already says
// how many copies it wants.
type Estate struct {
	// Replicas is how many copies of each partition the fleet keeps, on
	// that many different data nodes. Zero takes [DefaultEstateReplicas].
	//
	// A fleet with fewer data nodes keeps one copy per node rather than
	// leaving a partition unplaced, and reaches the full count as nodes
	// join.
	Replicas int `yaml:"replicas,omitempty" json:"replicas,omitempty" js:"min=0;max=10" desc:"Copies of each partition of the replicated estate, each on a different data node, 1..10; 0 is the default, 3. A fleet with fewer data nodes keeps one copy on each. Read once the estate is divided into partitions; under the single-file layout every data node holds all of it."`

	// FailureDomain is a node label KEY (one written under `node.labels` on
	// every data node — `zone`, `rack`, `host`): no two copies of a
	// partition are placed on nodes sharing that label's value, as long as
	// there are enough distinct values to go round. Empty spreads copies
	// across nodes with no further constraint.
	//
	// A data node that does not carry the label counts as a domain of its
	// own, as it does for [Objects.FailureDomain] — and `crewlet validate`
	// warns on the node missing it ([TierWarnings]), in one warning for both
	// blocks when they name the same key.
	FailureDomain string `yaml:"failure_domain,omitempty" json:"failure_domain,omitempty" desc:"A node label key (e.g. zone); no two copies of a partition share its value when enough values exist. Empty spreads across nodes only."`
}

// DefaultEstateReplicas is the copies of each partition a company keeps when it
// names none.
//
// THREE, for two reasons that agree. It is the smallest count that survives
// losing one copy WHILE a second is being rebuilt, which is [Objects]' reason
// and Ceph's. And it is the smallest at which a partition still holds the two
// verified snapshots its log's trim waits for (statelog.SnapshotDonorsRequired)
// while one of its holders has lost its store: at two copies a lost disk
// leaves one, and the partition's logs stop trimming until the copy is
// rebuilt.
const DefaultEstateReplicas = 3

// MaxEstateReplicas is the most copies a company may ask for — the estate map's
// own ceiling, referenced rather than restated, because a count this accepted
// and the map refused would be a revision every node applies and no maintainer
// can write.
const MaxEstateReplicas = mapplacement.MaxReplicas

// ReplicaCount is the copies the company keeps, with the default applied.
func (e *Estate) ReplicaCount() int {
	if e.Replicas == 0 {
		return DefaultEstateReplicas
	}
	return e.Replicas
}

func (e *Estate) validate(path Path) error {
	var p problems
	if e.Replicas < 0 || e.Replicas > MaxEstateReplicas {
		p.add(at(path, "replicas"), ErrOutOfRange,
			"must be 0 (the default, %d) or 1..%d, got %d: every write to a "+
				"partition is applied by each of its copies, and past %d a write "+
				"costs more than any failure it survives", DefaultEstateReplicas,
			MaxEstateReplicas, e.Replicas, MaxEstateReplicas)
	}
	// THE LABEL GRAMMAR node.labels IS HELD TO, for objects.failure_domain's
	// reason: a key a node's labels could never carry spreads copies across
	// nothing.
	if e.FailureDomain != "" {
		if err := placement.CheckLabelKey(e.FailureDomain); err != nil {
			p.add(at(path, "failure_domain"), ErrUnknownValue,
				"%v — it names the node label (under node.labels on every "+
					"data node) whose values copies are spread across", err)
		}
	}
	return p.err()
}

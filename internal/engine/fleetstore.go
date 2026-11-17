package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// ErrNoFleetStoreHere reports a node whose fleet store this process cannot
// reach on its own while the node's engine is stopped: a node that holds no
// data, and a member of a cluster. Its message names which, and the way to the
// store from there.
var ErrNoFleetStoreHere = errors.New("engine: the fleet's store cannot be reached from this node alone")

// FleetStore is the fleet's coordination store, reached by a process that is
// not this node's engine — a `crewlet` command, while the engine is stopped.
type FleetStore struct {
	// Fleet is the store, its buckets opened through the engine's own boot
	// configuration ([coordinationStoresOf], kv.OpenFleet), so a bucket this
	// creates on a node that never booted is the one the engine adopts.
	Fleet coord.Fleet
	// Where names the broker it was reached through, for the reader of a
	// command's output: "this node's broker on <stream.store_dir>", or
	// "the external broker at <host:port>".
	Where string

	broker *jetstream.Coordination
}

// Close lets the store go, and stops the broker [OpenFleetStore] started for
// it, if it started one — which frees the store directory for the engine.
// Safe on nil.
func (s *FleetStore) Close() {
	if s != nil {
		s.broker.Close()
	}
}

// OpenFleetStore reaches the fleet's coordination store the way a node
// configured as b does, for a process that is not its engine. A solo member
// starts a broker on its own store directory, whose lock
// ([jetstream.StoreDirLock]) keeps this and the engine apart, with no leaf
// listener: leaves would rejoin a broker about to go. A node on an external
// broker dials it. A cluster member (alone it never reaches quorum; beside its
// peers it would rejoin and catch up every group for one value) and a node
// holding no data (its store is the members') get [ErrNoFleetStoreHere].
// Only the fleet store, whose buckets two processes would otherwise each
// decide: never the queue (it would provision streams on a store no engine
// booted), the lease bucket (its TTL is the engine's) or the object store (its
// reservation is agreed fleet-wide).
func OpenFleetStore(ctx context.Context, b *config.Bootstrap) (*FleetStore, error) {
	switch {
	case b.BrokerKind() == placement.BrokerLeaf:
		return nil, fmt.Errorf("%w: this node holds no data — it joins the "+
			"members' broker as a leaf, and the fleet's store is theirs, which "+
			"they serve while they run", ErrNoFleetStoreHere)
	case b.BrokerKind() == placement.BrokerMember && b.Stream.Cluster.Name != "":
		return nil, fmt.Errorf("%w: this node is a member of the cluster %q, and "+
			"a member started alone cannot reach the quorum the fleet's store "+
			"needs — the store is served by the members that are running",
			ErrNoFleetStoreHere, b.Stream.Cluster.Name)
	}
	cfg, err := streamConfig(b)
	if err != nil {
		return nil, err
	}
	cfg.LeafPort, cfg.LeafHost, cfg.LeafAdvertise = 0, "", ""
	broker, err := jetstream.OpenCoordination(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("engine: reach the fleet's store: %w", err)
	}
	// THE ENGINE'S OWN CEILING over the bring-up — see [attachCoordination].
	clustered := clusteredStream(b)
	openCtx, cancel := context.WithTimeout(ctx,
		jsprovision.Clustered(clustered).SequenceBudget())
	defer cancel()
	fleet, err := kv.OpenFleet(openCtx, broker.JetStream(), coordinationStoresOf(b).fleet)
	if err != nil {
		broker.Close()
		return nil, fmt.Errorf("engine: open the fleet's store on %s: %w", broker.Where(), err)
	}
	return &FleetStore{Fleet: fleet, Where: broker.Where(), broker: broker}, nil
}

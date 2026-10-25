package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
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
	// Fleet is the store. Its buckets are the engine's own, opened through
	// the engine's own constructor ([openFleet]) with the engine's own
	// settings, so a bucket this creates on a node that has never booted is
	// the one the engine's first boot would have created, and adopts.
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
// configured as b does, for a process that is not its engine.
//
// # Which nodes can, alone
//
// A SOLO MEMBER starts its own broker on its own store directory — the store
// is in there, and with the engine stopped nothing else is running it. The
// directory's lock ([jetstream.StoreDirLock]) refuses this while the engine
// holds it, and refuses the engine while this does. It opens no leaf listener
// even where the node has one: the fleet's leaves would rejoin a broker that
// is about to go away, and nothing a leaf asks of it is this process's to
// answer.
//
// A NODE ON AN EXTERNAL BROKER dials it: the store lives there whether or not
// any engine runs.
//
// A MEMBER OF A CLUSTER cannot: started alone it never reaches the quorum
// every read and write of a replicated bucket needs, and started beside
// running peers it would rejoin the cluster as this node — catching up every
// replicated group, for a command that wanted one value — and leave it again
// moments later. Its peers run the store, so it is reached through one of them.
//
// A NODE THAT HOLDS NO DATA has no store to start: it is a leaf of the
// members', which run it.
//
// Both of those are [ErrNoFleetStoreHere], naming what to do instead.
//
// # Only the fleet store
//
// Not the queue, which would provision the engine's streams on a store whose
// engine may never have booted; not the lease bucket, whose expiry is the
// lease TTL the engine decides; not the object store, whose reservation is
// sized against the broker and agreed fleet-wide. The fleet store's buckets
// are the one thing here two processes would otherwise each decide, so they
// come from the one constructor the engine boots with.
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
	fleet, err := openFleet(openCtx, broker.JetStream(), b.Stream.Replicas, clustered)
	if err != nil {
		broker.Close()
		return nil, fmt.Errorf("engine: open the fleet's store on %s: %w", broker.Where(), err)
	}
	return &FleetStore{Fleet: fleet, Where: broker.Where(), broker: broker}, nil
}

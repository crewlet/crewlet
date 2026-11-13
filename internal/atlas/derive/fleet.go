package derive

import (
	"cmp"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
)

// FleetNode is one live node as its presence row says it: its id, whether
// its own broker is a member of the embedded cluster, and what it says of
// what it runs on. Hosts and names only: the node reduced every URL of its
// own file to a host before publishing, and no credential has a field here.
type FleetNode struct {
	ID string
	// BrokerMember is a node whose embedded broker holds the streams.
	BrokerMember bool
	// Address is the host peers reach the node at, "" where it names none.
	Address string
	// ExternalBroker is a node whose stream is an external NATS cluster, and
	// BrokerServers that cluster's server hosts.
	ExternalBroker bool
	BrokerServers  []string
	// ObjectsS3 is a node keeping the company's files in an S3 bucket, named
	// by Bucket and Region; false for the bucket in the fleet's own broker.
	// ObjectsKnown says the node said which.
	ObjectsKnown bool
	ObjectsS3    bool
	Bucket       string
	Region       string
}

// The names the engine's own entries carry, as a reader is shown them.
const (
	engineName  = "Crewlet"
	brokerName  = "Crewlet event stream"
	objectsName = "Crewlet object store"
)

// fleet derives the engine's own infrastructure from its live nodes: each
// node as a server, the broker as a queue on the members that hold it (or on
// an external cluster's servers), the object store as a bucket, and the
// engine as the service that runs on the nodes and depends on both. Every
// claim is the fleet's own record, so it lasts exactly as long as the nodes
// that say it are live. No node, no claim: a fleet nobody could list draws
// nothing of itself.
func fleet(nodes []FleetNode, at time.Time) Derived {
	b := newBuilder(atlas.RecordSource(atlas.RecordFleet), at)
	if len(nodes) == 0 {
		return b.out
	}
	nodes = slices.Clone(nodes)
	slices.SortFunc(nodes, func(x, y FleetNode) int { return cmp.Compare(x.ID, y.ID) })

	engine, ok := b.ref("fleet", atlas.KindService, atlas.EngineService)
	if !ok {
		return b.out
	}
	b.says("fleet", engine, atlas.FactName, engineName)
	broker, ok := b.ref("fleet.broker", atlas.KindQueue, atlas.EnginePrefix+"/broker")
	if ok {
		b.says("fleet.broker", broker, atlas.FactName, brokerName)
		b.says("fleet.broker", broker, atlas.FactEngine, "nats")
		b.relates("fleet.broker", engine, atlas.RelationDependsOn, broker)
	}
	servers, buckets := map[string]bool{}, map[string]atlas.Ref{}
	for _, n := range nodes {
		where := "fleet.node:" + n.ID
		node, named := b.ref(where, atlas.KindHost, atlas.EnginePrefix+"/"+n.ID)
		if !named {
			continue
		}
		b.says(where, node, atlas.FactName, n.ID)
		b.address(where+".address", node, n.Address)
		b.relates(where, engine, atlas.RelationRunsOn, node)
		if ok && n.BrokerMember {
			b.relates(where+".broker", broker, atlas.RelationRunsOn, node)
		}
		if ok && n.ExternalBroker {
			for _, host := range n.BrokerServers {
				if servers[host] {
					continue
				}
				servers[host] = true
				at := where + ".broker_server"
				server, named := b.ref(at, atlas.KindHost, atlas.BrokerServerPrefix+"/"+host)
				if !named {
					continue
				}
				b.address(at, server, host)
				b.relates(at, broker, atlas.RelationRunsOn, server)
			}
		}
		if !n.ObjectsKnown {
			continue
		}
		key, name := atlas.EnginePrefix+"/objects", objectsName
		if n.ObjectsS3 {
			key, name = atlas.ObjectsS3Prefix+"/"+n.Bucket, n.Bucket
		}
		bucket, seen := buckets[key]
		if !seen {
			var named bool
			if bucket, named = b.ref(where+".objects", atlas.KindBucket, key); !named {
				continue
			}
			buckets[key] = bucket
			b.says(where+".objects", bucket, atlas.FactName, name)
			b.relates(where+".objects", engine, atlas.RelationDependsOn, bucket)
		}
		if n.ObjectsS3 {
			b.says(where+".objects.region", bucket, atlas.FactRegion, n.Region)
		}
	}
	return b.out
}

// address claims the fleet's own address for r: always private, whatever
// network it is on ([atlas.FleetAddress]). An empty raw says nothing.
func (b *builder) address(where string, r atlas.Ref, raw string) {
	if raw == "" {
		return
	}
	c, err := atlas.FleetAddress(r, raw, b.at)
	b.add(where, c, err)
}

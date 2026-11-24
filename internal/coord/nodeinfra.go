package coord

import "slices"

// The bounds a summary is held to, so one node's record cannot grow with a
// list somebody pasted: an external broker is a handful of servers, and a
// name is a label.
const (
	MaxInfraServers = 16
	MaxInfraText    = 255
)

// NodeInfra is what a node says of what it runs on, read from its own node
// file at boot: where peers reach it, how its broker takes part and where an
// external one is, and where the company's files are kept. HOSTS AND NAMES
// ONLY. The node file holds the keyring, the tokens and the object store's
// keys, and none of them has a field here: a URL is reduced to its host
// before it is written, so a credential in its userinfo never leaves the node.
type NodeInfra struct {
	// Address is the host peers reach this node at, "" where its file names
	// none: what it advertises to the cluster, or the interface it binds.
	Address string
	// Cluster is the embedded cluster's name, and BrokerServers the hosts of
	// an external broker's servers. How the node's broker takes part is the
	// profile's to say, on the same row.
	Cluster       string
	BrokerServers []string
	// ObjectsBackend is `nats` or `s3`; Bucket, Region and Endpoint (a host)
	// are the S3 bucket's.
	ObjectsBackend string
	Bucket         string
	Region         string
	Endpoint       string
}

// Meta is the summary as a node's status record carries it ([NodeStatusRecord]),
// nil for one that says nothing. Every text is cut to [MaxInfraText] as stored,
// whole runes, and the servers to [MaxInfraServers].
func (n NodeInfra) Meta() map[string]any {
	out := map[string]any{}
	put := func(key, value string) {
		if value != "" {
			out[key] = fitBytes(value, MaxInfraText)
		}
	}
	put("address", n.Address)
	put("cluster", n.Cluster)
	put("objects", n.ObjectsBackend)
	put("bucket", n.Bucket)
	put("region", n.Region)
	put("endpoint", n.Endpoint)
	if len(n.BrokerServers) > 0 {
		servers := make([]any, 0, min(len(n.BrokerServers), MaxInfraServers))
		for _, s := range n.BrokerServers[:min(len(n.BrokerServers), MaxInfraServers)] {
			servers = append(servers, fitBytes(s, MaxInfraText))
		}
		out["broker_servers"] = servers
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// infraFromMeta reads a status record's summary. A field of another type
// reads as absent, as a status row's does.
func infraFromMeta(raw map[string]any) NodeInfra {
	n := NodeInfra{
		Address: stringFromMeta(raw["address"]), Cluster: stringFromMeta(raw["cluster"]),
		ObjectsBackend: stringFromMeta(raw["objects"]),
		Bucket:         stringFromMeta(raw["bucket"]), Region: stringFromMeta(raw["region"]),
		Endpoint: stringFromMeta(raw["endpoint"]),
	}
	if list, ok := raw["broker_servers"].([]any); ok {
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" && len(n.BrokerServers) < MaxInfraServers {
				n.BrokerServers = append(n.BrokerServers, s)
			}
		}
		slices.Sort(n.BrokerServers)
		n.BrokerServers = slices.Compact(n.BrokerServers)
	}
	return n
}

package engine

import (
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// NodeInfra is what this node publishes of what it runs on, read from its own
// node file: the atlas draws the fleet from every node's summary (ADR-0142).
// HOSTS AND NAMES ONLY: each URL is reduced to its host here, before anything
// leaves the node, and no credential field of the file is read at all. The
// zero summary for a node built without a node file.
func NodeInfra(boot *config.Bootstrap) coord.NodeInfra {
	if boot == nil {
		return coord.NodeInfra{}
	}
	out := coord.NodeInfra{
		Address:        nodeAddress(boot),
		ObjectsBackend: string(boot.Store.Objects.BackendOrDefault()),
	}
	if boot.Stream.Type == config.StreamNATS {
		out.BrokerServers = jetstream.ServerHosts(boot.Stream.URL)
	} else {
		out.Cluster = boot.Stream.Cluster.Name
	}
	if boot.Store.Objects.BackendOrDefault() == config.ObjectBackendS3 {
		s3 := boot.Store.Objects.S3
		out.Bucket, out.Region = s3.Bucket, s3.Region
		if u, err := url.Parse(s3.Endpoint); err == nil {
			out.Endpoint = u.Hostname()
		}
	}
	return out
}

// nodeAddress is the host peers reach this node at, as its file names it: what
// it advertises to the cluster, else the interface its route listener binds,
// else the one its API binds. An interface that means "every one" names no
// host, and neither does a file that sets none: the node then says nothing,
// never a guess read off the machine.
func nodeAddress(boot *config.Bootstrap) string {
	for _, raw := range []string{boot.Stream.Cluster.Advertise, boot.Stream.Cluster.Host, boot.API.Host} {
		host := strings.TrimSpace(raw)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host == "" {
			continue
		}
		if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
			continue
		}
		return host
	}
	return ""
}

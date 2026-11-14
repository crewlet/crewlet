package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// WHAT A NODE PUBLISHES OF ITSELF HOLDS NO CREDENTIAL. Every field of the
// node file that can carry one is filled with a canary, a token in each half
// of a stream URL's userinfo included, and none reaches the summary: a URL
// is reduced to its host, and the object store's keys have no field at all.
//
// Mutation: publish stream.url or the endpoint as written, and a canary is in
// the summary.
func TestANodesSummaryOfItselfHoldsNoCredential(t *testing.T) {
	t.Parallel()
	const canary = "CANARYs3cr3tCANARY"
	boot := &config.Bootstrap{}
	boot.Stream.Type = config.StreamNATS
	boot.Stream.URL = "nats://" + canary + "@nats-1.internal:4222,nats://user:" + canary + "@nats-2.internal:4222/" + canary
	boot.Store.Objects.Backend = config.ObjectBackendS3
	boot.Store.Objects.S3 = config.ObjectsS3{
		Endpoint: "https://" + canary + ":" + canary + "@s3.acme.example:9000/" + canary + "?k=" + canary,
		Region:   "eu-west-1", Bucket: "acme-files", Prefix: canary,
		AccessKeyID: canary, SecretAccessKey: canary,
	}
	boot.API.Host = "10.0.0.4"
	raw, err := json.Marshal(NodeInfra(boot).Meta())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("the summary holds a credential: %s", raw)
	}
	got := NodeInfra(boot)
	if got.Address != "10.0.0.4" || got.Endpoint != "s3.acme.example" || got.Bucket != "acme-files" ||
		len(got.BrokerServers) != 2 || got.BrokerServers[0] != "nats-1.internal" {
		t.Errorf("summary = %+v, want the hosts and names and nothing else", got)
	}
}

// A NODE'S ADDRESS IS WHAT ITS FILE NAMES, never a guess: what it advertises
// to the cluster first, then the interface its route listener binds, then the
// one its API binds; an interface that means every one names no host.
func TestANodesAddressIsWhatItsFileNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ advertise, cluster, api, want string }{
		{"node-a.internal:6222", "10.0.0.4", "10.0.0.9", "node-a.internal"},
		{"", "10.0.0.4", "10.0.0.9", "10.0.0.4"},
		{"", "", "10.0.0.9", "10.0.0.9"},
		{"", "0.0.0.0", "::", ""},
		{"[2001:db8::7]:6222", "", "", "2001:db8::7"},
		{"", "", "", ""},
	} {
		boot := &config.Bootstrap{}
		boot.Stream.Cluster.Advertise, boot.Stream.Cluster.Host, boot.API.Host = tc.advertise, tc.cluster, tc.api
		if got := NodeInfra(boot).Address; got != tc.want {
			t.Errorf("advertise %q, cluster host %q, api host %q: address %q, want %q",
				tc.advertise, tc.cluster, tc.api, got, tc.want)
		}
	}
	if got := NodeInfra(nil); got.Meta() != nil {
		t.Errorf("a node with no node file publishes %+v", got)
	}
}

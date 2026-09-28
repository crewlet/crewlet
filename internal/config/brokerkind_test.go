package config

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/seat/placement"
)

// TestBrokerMembershipComesFromTheStreamBlock: a node's broker kind is DERIVED
// from its stream block, whatever its roles, and it is what the node advertises
// on its presence.
//
// Derived rather than declared, because a setting could say `member` over a
// stream block that starts a leaf — and the capacity seal, which counts every
// member, would then wait on a node whose broker holds nothing, or pass
// without one that does.
func TestBrokerMembershipComesFromTheStreamBlock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stream func(*Stream)
		want   placement.BrokerKind
	}{
		{"a solo embedded node", func(*Stream) {}, placement.BrokerMember},
		{"an embedded member of a cluster", func(s *Stream) {
			s.Cluster = StreamCluster{Name: "acme", Peers: []string{"nats://b.example.com:6222"}}
		}, placement.BrokerMember},
		{"a member opening a leaf listener", func(s *Stream) { s.Leaf.Port = 7422 },
			placement.BrokerMember},
		{"an unset stream type", func(s *Stream) { s.Type = "" }, placement.BrokerMember},
		{"an embedded node joining through leaf urls", func(s *Stream) {
			s.Leaf.URLs = []string{"nats-leaf://data-a.example.com:7422"}
		}, placement.BrokerLeaf},
		{"an external cluster", func(s *Stream) {
			s.Type, s.URL = StreamNATS, "nats://nats.example.com:4222"
		}, placement.BrokerClient},
	} {
		// EVERY ROLE SET, the refused pairings included: the kind of a
		// stream block is no less definite for Tier A refusing what the
		// roles pair it with under the single-file layout.
		for _, roles := range [][]string{nil, {"data"}, {"seats"}, {"ingress", "workers"}} {
			t.Run(fmt.Sprintf("%s/%v", tc.name, roles), func(t *testing.T) {
				t.Parallel()
				b := DefaultBootstrap()
				b.Node.Roles = roles
				tc.stream(&b.Stream)
				if got := b.BrokerKind(); got != tc.want {
					t.Fatalf("broker kind %q, want %q: the roles must not decide it", got, tc.want)
				}
				if got := b.Profile("n1").Broker; got != tc.want {
					t.Fatalf("the presence profile advertises %q over a stream block that "+
						"makes a %q", got, tc.want)
				}
			})
		}
	}
}

package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A MALFORMED ADVERTISE ADDRESS IS REFUSED BY THE CONFIG, not by the broker.
//
// nats-server validates its advertise address while STARTING: it logs the
// failure and shuts the server down, so the same typo surfaces as a node that
// boots, dies, and leaves the operator reading broker logs for a mistake in
// their own file. The field path is the thing they can search for.
func TestClusterAdvertiseIsCheckedBeforeTheBrokerSeesIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		advertise string
		want      string
	}{
		"a port with no host":    {":6222", "no host"},
		"a port out of range":    {"node-a.internal:70000", "1..65535"},
		"a port that is not one": {"node-a.internal:six", "1..65535"},
		"an unbracketed IPv6":    {"fd00::1:6222", "not a host or host:port"},
		"a trailing colon":       {"node-a.internal:", "1..65535"},
	} {
		t.Run(name, func(t *testing.T) {
			b := fleetBootstrap(t)
			b.Stream.Cluster.Advertise = tc.advertise
			err := b.Validate()
			if err == nil {
				t.Fatalf("accepted advertise %q", tc.advertise)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "cluster.advertise") {
				t.Errorf("refusal = %q, want it to name stream.cluster.advertise", err)
			}
		})
	}
}

// AND THE TWO SHAPES A REAL DEPLOYMENT USES ARE ACCEPTED: host and port for a
// mapped container port, a bare host where only the address differs.
func TestClusterAdvertiseAcceptsTheShapesADeploymentUses(t *testing.T) {
	t.Parallel()
	for name, advertise := range map[string]string{
		"host and port":    "node-a.internal:6222",
		"a bare host":      "node-a.internal",
		"an IPv4 literal":  "203.0.113.9:6222",
		"a bracketed IPv6": "[fd00::1]:6222",
		"unset":            "",
	} {
		t.Run(name, func(t *testing.T) {
			b := fleetBootstrap(t)
			b.Stream.Cluster.Advertise = advertise
			if err := b.Validate(); err != nil {
				t.Fatalf("advertise %q was refused: %v", advertise, err)
			}
		})
	}
}

// A CLUSTER BLOCK CARRYING ONLY host OR advertise IS STILL A CLUSTER BLOCK.
//
// IsZero decides whether the block survives a JSON round trip, so a field it
// does not know is a field an operator sets, saves, and finds gone.
func TestClusterBlockIsNotZeroForItsAddressFields(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]config.StreamCluster{
		"host only":      {Host: "10.0.0.11"},
		"advertise only": {Advertise: "node-a.internal:6222"},
	} {
		t.Run(name, func(t *testing.T) {
			if c.IsZero() {
				t.Errorf("%+v reports zero, so it would be dropped from a "+
					"stored config the operator wrote it into", c)
			}
		})
	}
}

// fleetBootstrap is a valid three-member fleet, so a case about one field
// fails for that field and nothing else.
func fleetBootstrap(t *testing.T) config.Bootstrap {
	t.Helper()
	b := config.DefaultBootstrap()
	b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
	b.Stream = config.Stream{
		Replicas: 3, StoreDir: t.TempDir(),
		Cluster: config.StreamCluster{
			Name:  "crewlet",
			Port:  6222,
			Host:  "10.0.0.11",
			Peers: []string{"nats://node-b.internal:6222", "nats://node-c.internal:6222"},
		},
	}
	return b
}

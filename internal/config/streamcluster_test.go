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

// AN EXTERNAL CLUSTER IS FORMED BY ITS OWN OPERATOR, so the block that
// configures the EMBEDDED server's membership is refused against one.
//
// The same rule `url`, `store_dir`, `debug` and `sync` already keep. Not one
// field here is read when the stream is external: the embedded options are
// built on a branch that stream never reaches, and the engine answers
// "clustered" from the URL alone. Accepted, it records a membership, a route
// port and an advertise address that never reach the thing forming the
// cluster — and an operator who wrote them has no way to find that out.
//
// EVERY FIELD, because a rule written as a list of the ones somebody thought
// of is how this block's siblings went unvalidated the first time.
func TestAnExternalStreamRefusesTheEmbeddedClusterBlock(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]config.StreamCluster{
		"a name":         {Name: "crewlet"},
		"a route port":   {Port: 6222},
		"a peer list":    {Peers: []string{"nats://node-b.internal:6222"}},
		"a bind host":    {Host: "10.0.0.11"},
		"an advertise":   {Advertise: "node-a.internal:6222"},
		"the whole file": {Name: "crewlet", Port: 6222, Host: "10.0.0.11", Peers: []string{"nats://node-b.internal:6222"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			// An external stream is a fleet, so its coordination has to be
			// the fleet's. Left at the default it is refused for THAT,
			// which is an ErrConflict this case would otherwise pass on
			// without the block ever having been looked at.
			b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
			b.Stream = config.Stream{
				Type: config.StreamNATS,
				URL:  "nats://broker.example.com:4222",
				// Replicas, because an external cluster's own membership
				// is not in this file: the count is the operator's to
				// state and is not what is under test here.
				Replicas: 3,
				Cluster:  c,
			}

			err := b.Validate()
			if err == nil {
				t.Fatalf("accepted %+v against an external stream, where nothing reads it", c)
			}
			// THE PROBLEM ITSELF, not the rendered text:
			// `stream.cluster.name` contains `stream.cluster`, so a
			// substring match is satisfied by the rule that tells an
			// operator to NAME the block, which is the opposite advice
			// from the one under test.
			problems := config.Problems(err)
			if len(problems) != 1 {
				t.Fatalf("problems = %v, want exactly the one about the block", problems)
			}
			if got := problems[0].Path; got != "stream.cluster" {
				t.Errorf("path = %q, want %q", got, "stream.cluster")
			}
			if got := problems[0].Kind; got != "conflict" {
				t.Errorf("kind = %q, want conflict", got)
			}
		})
	}
}

// AND IT IS NOT JUDGED BY A PEER COUNT NOBODY READS.
//
// validateTopology counts stream.cluster.peers to refuse a two-node fleet,
// which has no quorum. Against an external stream those peers are inert, so
// naming one used to be refused as a fleet of two on the strength of a block
// the engine never looks at — a refusal an operator cannot act on, since
// removing the peer is what they are being told to do for the wrong reason.
func TestAnExternalStreamIsNotJudgedByTheClusterBlocksPeerCount(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
	b.Stream = config.Stream{
		Type:    config.StreamNATS,
		URL:     "nats://broker.example.com:4222",
		Cluster: config.StreamCluster{Name: "crewlet", Peers: []string{"nats://node-b.internal:6222"}},
	}

	err := b.Validate()
	if err == nil {
		t.Fatal("accepted a cluster block on an external stream")
	}
	if strings.Contains(err.Error(), "no coordination quorum") {
		t.Errorf("refusal = %q, want it to name the inert block rather than a "+
			"two-node quorum derived from peers nothing reads", err)
	}
}

// AND THE EMBEDDED STREAM THE BLOCK IS FOR STILL TAKES IT.
//
// The guard against the rule above being written so broadly that it refuses
// the topology the whole block exists to describe.
func TestAnEmbeddedStreamStillTakesTheClusterBlock(t *testing.T) {
	t.Parallel()
	b := fleetBootstrap(t)
	if err := b.Validate(); err != nil {
		t.Fatalf("refused a three-member embedded fleet: %v", err)
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

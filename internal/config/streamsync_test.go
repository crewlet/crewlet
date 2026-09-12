package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// STREAM.SYNC DEFAULTS TO THE STRONG VALUE, at every replica count.
//
// The inference this replaces made the default silently weaker on exactly the
// deployments that matter most: a replicated fleet got no fsync, on the
// argument that its quorum was the durability — which is true of one failure
// class out of five.
func TestStreamSyncDefaultsToAlways(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]config.Stream{
		"unset, solo":       {Replicas: 1},
		"unset, replicated": {Replicas: 3},
		"said explicitly":   {Replicas: 3, Sync: "always"},
	} {
		t.Run(name, func(t *testing.T) {
			if !s.SyncAlways() {
				t.Error("SyncAlways() is false: unset takes the strong value, " +
					"because a default that is weaker on a fleet than on a " +
					"laptop is the wrong way round")
			}
			if got := s.SyncInterval(); got != 0 {
				t.Errorf("SyncInterval() = %v with the fsync on, want 0", got)
			}
		})
	}
}

// A DURATION IS THE WINDOW, parsed once at the edge.
func TestStreamSyncDurationIsTheWindow(t *testing.T) {
	t.Parallel()
	s := config.Stream{Replicas: 3, Sync: "30s"}
	if s.SyncAlways() {
		t.Fatal("SyncAlways() is true for a duration")
	}
	if got := s.SyncInterval(); got != 30*time.Second {
		t.Errorf("SyncInterval() = %v, want 30s", got)
	}
}

// THE THREE REFUSALS, each a place where declining the fsync would be recorded
// and then not honoured — which is worse than either answer, because the
// operator believes the number they wrote.
func TestStreamSyncRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		stream config.Stream
		want   string
	}{
		"an external cluster stores its own data": {
			config.Stream{Type: config.StreamNATS, URL: "nats://broker.example.com:4222",
				Replicas: 3, Sync: "30s"},
			"external NATS cluster",
		},
		"below three replicas there is no quorum to trade for": {
			config.Stream{Replicas: 1, Sync: "30s"},
			"no quorum to trade for",
		},
		"a same-host cluster is one failure domain": {
			config.Stream{
				Replicas: 3, Sync: "30s",
				Cluster: config.StreamCluster{
					Name:  "crewlet",
					Peers: []string{"nats://127.0.0.1:6222", "nats://localhost:6223"},
				},
			},
			"one power supply",
		},
		"a value that is neither": {
			config.Stream{Replicas: 3, Sync: "sometimes"},
			"neither",
		},
		"a non-positive window": {
			config.Stream{Replicas: 3, Sync: "0s"},
			"must be positive",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := config.DefaultBootstrap()
			b.Stream = tc.stream
			err := b.Validate()
			if err == nil {
				t.Fatalf("accepted: %+v", tc.stream)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.want)
			}
		})
	}
}

// AND A REAL FLEET IS ACCEPTED: three replicas on distinct hosts may decline
// the fsync, which is the whole point of the field being a choice.
func TestStreamSyncAcceptsARealFleet(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
	b.Stream = config.Stream{
		Replicas: 3, Sync: "30s", StoreDir: t.TempDir(),
		Cluster: config.StreamCluster{
			Name:  "crewlet",
			Port:  6222,
			Peers: []string{"nats://node-b.internal:6222", "nats://node-c.internal:6222"},
		},
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("a three-host fleet was refused a declared window: %v", err)
	}
}

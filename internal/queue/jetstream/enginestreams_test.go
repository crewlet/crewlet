package jetstream

import (
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ENGINE STREAMS ARE WHAT A QUEUE PROVISIONS.
//
// A test that stands a broker up without the queue, so that its account holds
// what a node's broker holds, provisions it from [EngineStreams]. What that
// makes and what a queue opened with the same Config makes are the same
// streams: read back from each broker rather than compared as the structs
// that asked for them, every field of every stream alike but the creation
// stamp, which each create mints for itself and both must carry.
func TestEngineStreamsAreWhatAQueueProvisions(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		onDisk bool
		events time.Duration
	}{
		"in memory, the default event retention": {},
		"on disk, a day of events":               {onDisk: true, events: 24 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{EventRetention: tc.events}
			if tc.onDisk {
				cfg.StoreDir, cfg.StoreMaxBytes = brokertest.TempDir(t), brokertest.StoreMaxBytes
			}
			want := reportedStreams(t, newQueueWith(t, cfg).js)
			if declared := len(engineStreams(cfg.EventRetention)); len(want) != declared {
				t.Fatalf("a queue's broker holds %d streams once it opens, and the engine declares %d",
					len(want), declared)
			}

			bare := bareBroker(t)
			for _, stream := range EngineStreams(cfg, time.Now()) {
				if _, err := bare.CreateStream(t.Context(), stream); err != nil {
					t.Fatalf("provision %s from EngineStreams: %v", stream.Name, err)
				}
			}
			got := reportedStreams(t, bare)

			if names, wantNames := slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)); !slices.Equal(names, wantNames) {
				t.Fatalf("EngineStreams makes %v, and a queue provisions %v", names, wantNames)
			}
			for name, provisioned := range want {
				if declared := got[name]; !reflect.DeepEqual(declared, provisioned) {
					t.Errorf("%s from EngineStreams is\n\t%+v\nand as a queue provisions it\n\t%+v",
						name, declared, provisioned)
				}
			}
		})
	}
}

// reportedStreams is every stream js's account holds, each as its broker
// reports its configuration, with the creation stamp checked present and
// taken out: the one value two creates of the same declaration never share.
func reportedStreams(t *testing.T, js jetstream.JetStream) map[string]jetstream.StreamConfig {
	t.Helper()
	streams := map[string]jetstream.StreamConfig{}
	list := js.ListStreams(t.Context())
	for info := range list.Info() {
		config := info.Config
		if _, err := statelog.StreamCreatedAt(info); err != nil || config.Metadata[statelog.CreatedAtKey] == "" {
			t.Errorf("%s carries no creation stamp a node can read (%v): its identity would "+
				"be the broker's instant, which a restore does not keep", config.Name, err)
		}
		config.Metadata = maps.Clone(config.Metadata)
		delete(config.Metadata, statelog.CreatedAtKey)
		streams[config.Name] = config
	}
	if err := list.Err(); err != nil {
		t.Fatalf("list the streams: %v", err)
	}
	return streams
}

// bareBroker is a broker of its own with JetStream and nothing provisioned on
// it, reached in process.
func bareBroker(t *testing.T) jetstream.JetStream {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "engine-streams", JetStream: true, Port: -1,
		DontListen: true, NoLog: true, NoSigs: true, StoreDir: brokertest.TempDir(t),
		JetStreamMaxStore: brokertest.StoreMaxBytes,
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	if !ns.ReadyForConnections(30 * time.Second) {
		t.Fatal("the broker never became ready")
	}
	conn, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

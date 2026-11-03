package replicated_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/objstore/objstoretest"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// replicas is the copies the bucket is held at: what `stream.replicas` is on
// every fleet of three data nodes, and the smallest a quorum survives a
// member's loss at.
const replicas = 3

// ceiling is the byte reservation the bucket is created with, which every
// bucket must hold (natsobj.Config.MaxBytes): four gibibytes, since the one
// bucket every benchmark case shares holds the stat case's ten thousand
// objects of up to 256 KiB at once (some 840 MiB with their metadata) and a
// 128 MiB object beside the ranged case, and a put past it would be refused
// as the bucket filling, which no figure may be made of.
const ceiling = 4 << 30

// fleet is a cluster of [replicas] members at `stream.sync: always`,
// production's default, each reached through a JetStream client of its own
// member's — the API every node of the fleet addresses ([jsapi.Embedded]).
//
// A CLIENT OF THE MEMBER, never the engine's queue over it, which would
// provision every stream the engine runs on all three members before the
// first case: replicated streams nothing here writes, kept beside the one
// being measured.
type fleet struct {
	cluster *jetstreamtest.Cluster
	clients []jetstream.JetStream
}

func startFleet(tb testing.TB) *fleet {
	tb.Helper()
	c := jetstreamtest.StartCluster(tb, replicas, js.Config{SyncAlways: true})
	f := &fleet{cluster: c}
	for i, srv := range c.Servers {
		nc, err := srv.Conn()
		if err != nil {
			tb.Fatalf("connect to member %d: %v", i, err)
		}
		tb.Cleanup(nc.Close)
		client, err := jsapi.Embedded().Client(nc)
		if err != nil {
			tb.Fatalf("a JetStream client of member %d: %v", i, err)
		}
		f.clients = append(f.clients, client)
	}
	return f
}

// bucket is the bucket through member i's client, opened as a data node of a
// clustered fleet opens it.
func (f *fleet) bucket(tb testing.TB, i int) *natsobj.Backend {
	tb.Helper()
	b, err := natsobj.Open(tb.Context(), f.clients[i],
		natsobj.Config{MaxBytes: ceiling, Replicas: replicas, Clustered: true})
	if err != nil {
		tb.Fatalf("open the bucket through member %d: %v", i, err)
	}
	return b
}

// stream is the bucket's stream as member i's client reads it.
func (f *fleet) stream(tb testing.TB, i int) jetstream.Stream {
	tb.Helper()
	s, err := f.clients[i].Stream(tb.Context(), natsobj.Stream)
	if err != nil {
		tb.Fatalf("the %s stream through member %d: %v", natsobj.Stream, i, err)
	}
	return s
}

// leader is the member leading the bucket's stream — which a bucket opened
// through any member has made — waiting for one to be elected.
//
// THE CLUSTER'S OWN VIEW, which a test may read to label which member an arm
// drives — and which no read path may, since it is exactly what an isolated
// former leader gets wrong.
func (f *fleet) leader(tb testing.TB) int {
	tb.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := f.stream(tb, 0).Info(tb.Context())
		if err == nil && info.Cluster != nil && info.Cluster.Leader != "" {
			for i, cfg := range f.cluster.Configs {
				if cfg.ServerName == info.Cluster.Leader {
					return i
				}
			}
			tb.Fatalf("%s is led by %q, which is no member of the fleet", natsobj.Stream, info.Cluster.Leader)
		}
		if time.Now().After(deadline) {
			tb.Fatalf("%s elected no leader in thirty seconds: %v", natsobj.Stream, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// AN OBJECT PUT THROUGH ONE MEMBER IS WHOLE THROUGH EVERY MEMBER, on a bucket
// held at three copies — the wiring the benchmark measures through, held by a
// case so it cannot rot unseen: the bucket each data node opens is the one
// replicated bucket, a put through a follower's client is acknowledged by the
// stream's leader, and a stat and a read through any member answer it.
func TestAnObjectPutThroughOneMemberIsWholeThroughEvery(t *testing.T) {
	f := startFleet(t)
	buckets := make([]*natsobj.Backend, replicas)
	for i := range replicas {
		buckets[i] = f.bucket(t, i)
	}
	info, err := f.stream(t, 0).Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Config.Replicas; got != replicas {
		t.Fatalf("the bucket is held at %d copies, want %d", got, replicas)
	}

	follower := (f.leader(t) + 1) % replicas
	data := bytes.Repeat([]byte("held at three copies "), 3*natsobj.MessageBytes/21)
	name := objstore.KeyAt(time.Now()).Name()
	if err := buckets[follower].Put(t.Context(), name, bytes.NewReader(data), objstore.PutMeta{}); err != nil {
		t.Fatalf("put through member %d, a follower: %v", follower, err)
	}
	for i, b := range buckets {
		stat, err := b.Stat(t.Context(), name)
		if err != nil || stat.Size != int64(len(data)) {
			t.Fatalf("stat through member %d = %+v, %v; want %d bytes", i, stat, err, len(data))
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		r, err := b.Get(ctx, name, 0, -1)
		if err != nil {
			cancel()
			t.Fatalf("get through member %d: %v", i, err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		cancel()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read through member %d: %d of %d bytes, %v", i, len(got), len(data), err)
		}
	}
}

// BenchmarkBackend measures the broker's object store at three replicas
// through the suite every backend runs, driven from a FOLLOWER's client
// (`from=follower`): every put and every read is answered by the bucket's
// stream leader whichever member's client sends it, so two of a fleet's three
// data nodes reach it from a follower, across a route.
//
// THE THREE MEMBERS SHARE ONE MACHINE — three brokers on its CPUs and three
// copies of every byte fsynced to its one disk, with no network hop between
// them — so the figures are pessimistic per member for the disk and the CPU,
// and say nothing about a datacenter's round trip between members.
//
// ONE CLUSTER FOR EVERY CASE, the bucket's stream PURGED before each, so a
// case starts from an empty bucket without paying a cluster's bring-up.
func BenchmarkBackend(b *testing.B) {
	quiet(b)
	f := startFleet(b)
	f.bucket(b, 0)
	follower := (f.leader(b) + 1) % replicas
	b.Run(fmt.Sprintf("replicas=%d/from=follower", replicas), func(b *testing.B) {
		objstoretest.Bench(b, func(b *testing.B) objstoretest.BenchBackend {
			bucket := f.bucket(b, follower)
			if err := f.stream(b, follower).Purge(b.Context()); err != nil {
				b.Fatalf("empty the bucket: %v", err)
			}
			return objstoretest.BenchBackend{Backend: bucket}
		})
	})
}

// quiet raises the process's log level to Warn while a benchmark runs, for
// internal/statelog's reason: three members log what they do from their own
// goroutines, and an INFO line landing inside a result line breaks the line a
// comparison tool reads. Warn and above still print, because one of those
// during a measurement is a finding. Put back to the level the logging package
// starts a process at.
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

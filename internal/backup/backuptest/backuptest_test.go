package backuptest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/backup/backuptest"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

const (
	logStream = "CREWLET_PROBE_LOG"
	bucket    = "crewlet_probe"
	// bucketStream is the stream JetStream keeps bucket in, the name a
	// manifest lists it under.
	bucketStream = "KV_" + bucket
	records      = 1000
	consumed     = 300
	// probeHeader is a header every record carries beside its operation id,
	// so a restore that kept the id and lost the rest of a record's headers
	// is seen.
	probeHeader = "Crewlet-Probe"
	// duplicates is the log's duplicate window: wider than any run of this
	// test however loaded the machine, so every retry below is inside it
	// and only the restored stream's own window can collapse one.
	duplicates = 30 * time.Minute
)

// opID is record i's operation id, the Nats-Msg-Id a state log publishes every
// append under.
func opID(i int) string { return fmt.Sprintf("op-%04d", i) }

// broker starts a solo JetStream with NO LISTENER, in the fleet's domain, as
// every embedded member runs — a restore has to be asked inside the process
// for exactly the reason a snapshot does.
func broker(t *testing.T, name string) *nats.Conn {
	t.Helper()
	return brokerIn(t, name, t.TempDir()).conn
}

// soloBroker is one running broker and the connection to it.
type soloBroker struct {
	ns   *server.Server
	conn *nats.Conn
}

// brokerIn is [broker] on the store directory dir, so the broker can be
// stopped and started again on what it stored — the start of a node whose
// broker was restored for that purpose.
func brokerIn(t *testing.T, name, dir string) soloBroker {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName:      name,
		JetStream:       true,
		JetStreamDomain: jsapi.Domain,
		Port:            -1,
		DontListen:      true,
		StoreDir:        dir,
	})
	if err != nil {
		t.Fatalf("configure the broker %s: %v", name, err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatalf("the broker %s did not become ready", name)
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	conn, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect to the broker %s: %v", name, err)
	}
	t.Cleanup(conn.Close)
	return soloBroker{ns: ns, conn: conn}
}

// stop closes the connection and shuts the broker down, waiting until it has.
func (b soloBroker) stop() {
	b.conn.Close()
	b.ns.Shutdown()
	b.ns.WaitForShutdown()
}

// publishOp appends one record under the operation id id, as a state log
// appends, and answers the broker's acknowledgement.
func publishOp(ctx context.Context, t *testing.T, conn *nats.Conn, id string) *jetstream.PubAck {
	t.Helper()
	ack, err := client(t, conn).PublishMsg(ctx, &nats.Msg{
		Subject: "crewlet.probe.log.a", Data: []byte("operation " + id),
	}, jetstream.WithMsgID(id))
	if err != nil {
		t.Fatalf("publish operation %s: %v", id, err)
	}
	return ack
}

func client(t *testing.T, conn *nats.Conn) jetstream.JetStream {
	t.Helper()
	js, err := jsapi.Embedded().Client(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// source is what the restore has to bring back: a log of a thousand records
// across two subjects, stamped by its create as the engine's queue stamps every
// stream, each record published as a state log publishes an append — under its
// operation id (Nats-Msg-Id), with a header of its own beside it; a durable
// reader that has acknowledged the first three hundred; and a
// coordination-shaped bucket holding a key written twice. It answers the
// bucket key's revision.
func source(t *testing.T, conn *nats.Conn) uint64 {
	t.Helper()
	ctx := t.Context()
	js := client(t, conn)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name:       logStream,
		Subjects:   []string{"crewlet.probe.log.>"},
		Storage:    jetstream.FileStorage,
		Duplicates: duplicates,
		Metadata:   statelog.StampCreatedAt(nil, time.Now()),
	}); err != nil {
		t.Fatalf("create the log: %v", err)
	}
	for i := range records {
		msg := &nats.Msg{
			Subject: []string{"crewlet.probe.log.a", "crewlet.probe.log.b"}[i%2],
			Data:    fmt.Appendf(nil, "record-%d", i),
			Header:  nats.Header{probeHeader: []string{fmt.Sprintf("value-%d", i)}},
		}
		ack, err := js.PublishMsg(ctx, msg, jetstream.WithMsgID(opID(i)))
		if err != nil {
			t.Fatalf("publish record %d: %v", i, err)
		}
		if ack.Sequence != uint64(i+1) || ack.Duplicate {
			t.Fatalf("record %d landed at %d (duplicate %v), want %d", i, ack.Sequence,
				ack.Duplicate, i+1)
		}
	}
	reader, err := js.CreateConsumer(ctx, logStream, jetstream.ConsumerConfig{
		Durable: "reader", AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create the reader: %v", err)
	}
	batch, err := reader.Fetch(consumed, jetstream.FetchMaxWait(10*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	read := 0
	for msg := range batch.Messages() {
		if err := msg.DoubleAck(ctx); err != nil {
			t.Fatalf("acknowledge record %d: %v", read, err)
		}
		read++
	}
	if read != consumed {
		t.Fatalf("the reader was handed %d records, want %d", read, consumed)
	}

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket})
	if err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	if _, err := kv.Create(ctx, "lease", []byte("first holder")); err != nil {
		t.Fatalf("create the key: %v", err)
	}
	revision, err := kv.Put(ctx, "lease", []byte("second holder"))
	if err != nil {
		t.Fatalf("put the key: %v", err)
	}
	return revision
}

// backUp takes a backup of conn's streams through internal/backup's own
// snapshot — the artefact a restore is handed — and answers its directory
// and manifest.
func backUp(t *testing.T, conn *nats.Conn) (string, backup.Manifest) {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	fleet := memory.NewFleet()
	service, err := backup.New(backup.Options{
		Store: db, Estate: backup.HoldsNodeOnly, Conn: conn, API: jsapi.Embedded(),
		NodeID: "restore-probe", Holds: fleet, Backups: fleet,
	})
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "backup")
	manifest, err := service.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take the backup: %v", err)
	}
	return dir, manifest
}

func stream(ctx context.Context, t *testing.T, conn *nats.Conn, name string) *jetstream.StreamInfo {
	t.Helper()
	s, err := client(t, conn).Stream(ctx, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return s.CachedInfo()
}

// A RESTORED STREAM HOLDS WHAT WAS SNAPSHOTTED — every record at its own
// sequence with its own subject, headers, body and storage instant, the
// duplicate window those headers rebuild once its broker starts again, the
// stream's identity, a durable reader at its own position, and a bucket's
// revisions.
//
// Each is something a restored node stands on. Its rows name records by
// sequence and its checkpoint names the record it stands on by the broker's
// instant for it. A state log publishes every append under its operation id
// (Nats-Msg-Id), and a stream rebuilds its duplicate window from those headers
// — what collapses an append repeated inside the window onto the record it
// already landed as; the server rebuilds it when it recovers a stream from its
// store, NOT at the restore (package doc), and a node's broker starts again on
// what was restored before the node serves from it. Every row is keyed to the
// stream's identity, which the broker does NOT keep — it dates a restored
// stream to the restore — so it is read as the instant the stream's creator
// stamped, carried in the configuration the restore hands back. A mailbox is a
// durable reader, and one restored at its first record redelivers everything
// it had acknowledged. And the coordination store's create-only and
// compare-and-set records are judged by revision, so a bucket restored at other
// revisions would take a create over a live record or refuse every write to
// one.
func TestARestoredStreamHoldsWhatWasSnapshotted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	src := broker(t, "source")
	revision := source(t, src)
	dir, manifest := backUp(t, src)

	dstDir := t.TempDir()
	restored := brokerIn(t, "restored", dstDir)
	dst := restored.conn
	if err := backuptest.RestoreStreams(ctx, dst, jsapi.Embedded(), dir, manifest); err != nil {
		t.Fatalf("restore: %v", err)
	}

	before, after := stream(ctx, t, src, logStream), stream(ctx, t, dst, logStream)
	if after.State.FirstSeq != before.State.FirstSeq || after.State.LastSeq != before.State.LastSeq ||
		after.State.Msgs != before.State.Msgs {
		t.Fatalf("the restored log holds %d..%d (%d records), the snapshotted one %d..%d (%d)",
			after.State.FirstSeq, after.State.LastSeq, after.State.Msgs,
			before.State.FirstSeq, before.State.LastSeq, before.State.Msgs)
	}
	original, err := statelog.StreamCreatedAt(before)
	if err != nil {
		t.Fatalf("identify the snapshotted log: %v", err)
	}
	restoredAt, err := statelog.StreamCreatedAt(after)
	if err != nil {
		t.Fatalf("identify the restored log: %v", err)
	}
	if !restoredAt.Equal(original) {
		t.Fatalf("the restored log is identified as created at %v and the snapshotted one at "+
			"%v — every node restored from this backup would refuse its own log as recreated",
			restoredAt, original)
	}
	sameRecords(ctx, t, client(t, src), client(t, dst), before.State.LastSeq)

	// THE DUPLICATE WINDOW IS NOT REBUILT AT THE RESTORE: an operation the
	// restored log holds, repeated now, is appended again. A fact about the
	// server pinned here because the package doc and the runbook's restart
	// rest on it — the day it stops holding, both can say less.
	probe := publishOp(ctx, t, dst, opID(records-1))
	if probe.Duplicate {
		t.Fatalf("operation %s repeated on the stream just restored was collapsed onto "+
			"sequence %d — the server now rebuilds a restored stream's duplicate window "+
			"at the restore, so the package doc's paragraph saying it does not is stale",
			opID(records-1), probe.Sequence)
	}

	// AND IT IS ONCE THE BROKER STARTS AGAIN on what it restored, as a node's
	// does: an operation repeated inside the window collapses onto the record
	// it landed as, the identity is the one the restore kept, and a new
	// operation is appended where the log ends.
	restored.stop()
	restored = brokerIn(t, "restored", dstDir)
	dst = restored.conn
	const retried = records / 2
	if ack := publishOp(ctx, t, dst, opID(retried)); !ack.Duplicate || ack.Sequence != retried+1 {
		t.Fatalf("operation %s repeated on the restored log answered sequence %d "+
			"(duplicate %v), want the duplicate at %d — the restored log has no "+
			"duplicate window over the records it holds, so an append repeated "+
			"inside it lands twice", opID(retried), ack.Sequence, ack.Duplicate, retried+1)
	}
	if ack := publishOp(ctx, t, dst, opID(records)); ack.Duplicate || ack.Sequence != probe.Sequence+1 {
		t.Fatalf("a new operation on the restored log answered sequence %d (duplicate "+
			"%v), want %d", ack.Sequence, ack.Duplicate, probe.Sequence+1)
	}
	if again, err := statelog.StreamCreatedAt(stream(ctx, t, dst, logStream)); err != nil ||
		!again.Equal(original) {
		t.Fatalf("started again, the restored log is identified as created at %v (%v), "+
			"the snapshotted one at %v", again, err, original)
	}

	reader, err := client(t, dst).Consumer(ctx, logStream, "reader")
	if err != nil {
		t.Fatalf("the restored log has no reader: %v", err)
	}
	if got := reader.CachedInfo().AckFloor.Stream; got != consumed {
		t.Fatalf("the restored reader has acknowledged through %d, want %d — a restored "+
			"mailbox would deliver what it had already handed over", got, consumed)
	}

	kv, err := client(t, dst).KeyValue(ctx, bucket)
	if err != nil {
		t.Fatalf("the restored broker has no bucket: %v", err)
	}
	entry, err := kv.Get(ctx, "lease")
	if err != nil {
		t.Fatalf("read the restored key: %v", err)
	}
	if entry.Revision() != revision || string(entry.Value()) != "second holder" {
		t.Fatalf("the restored key is %q at revision %d, want %q at %d",
			entry.Value(), entry.Revision(), "second holder", revision)
	}
	if _, err := kv.Create(ctx, "lease", []byte("an usurper")); !errors.Is(err, jetstream.ErrKeyExists) {
		t.Fatalf("a create over the restored key = %v, want ErrKeyExists", err)
	}
	if _, err := kv.Update(ctx, "lease", []byte("third holder"), revision); err != nil {
		t.Fatalf("a compare-and-set at the restored revision was refused: %v", err)
	}
}

// sameRecords holds every record of two logs to one another: subject, headers,
// body and the broker's storage instant, at every sequence through last.
func sameRecords(ctx context.Context, t *testing.T, a, b jetstream.JetStream, last uint64) {
	t.Helper()
	sa, err := a.Stream(ctx, logStream)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Stream(ctx, logStream)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= last; seq++ {
		want, err := sa.GetMsg(ctx, seq)
		if err != nil {
			t.Fatalf("read snapshotted record %d: %v", seq, err)
		}
		got, err := sb.GetMsg(ctx, seq)
		if err != nil {
			t.Fatalf("read restored record %d: %v", seq, err)
		}
		if got.Subject != want.Subject || !bytes.Equal(got.Data, want.Data) || !got.Time.Equal(want.Time) {
			t.Fatalf("restored record %d is %s %q at %v, the snapshotted one %s %q at %v",
				seq, got.Subject, got.Data, got.Time, want.Subject, want.Data, want.Time)
		}
		// THE HEADERS, the operation id among them — and never vacuously: a
		// snapshotted record without its own is a source this case did not
		// build.
		if want.Header.Get(jetstream.MsgIDHeader) == "" || want.Header.Get(probeHeader) == "" {
			t.Fatalf("snapshotted record %d carries headers %v, want its operation id "+
				"and %s", seq, want.Header, probeHeader)
		}
		if !maps.EqualFunc(got.Header, want.Header, slices.Equal) {
			t.Fatalf("restored record %d carries headers %v, the snapshotted one %v",
				seq, got.Header, want.Header)
		}
	}
}

// WHAT A RESTORE REFUSES, and what it leaves out when told to.
//
// A restore creates its streams, so one into a broker already holding the
// stream is refused by name; a skip is a runbook step, so one naming a stream
// the manifest does not hold — a misspelling — is refused before anything is
// restored, and a right one leaves exactly its stream out; and a stream that
// does not end where the manifest says its snapshot ended is refused naming
// both, since that figure is the one any restore must keep.
func TestARestoreRefusesWhatItCannotRestoreWhole(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	src := broker(t, "source")
	source(t, src)
	dir, manifest := backUp(t, src)

	t.Run("a stream the broker already holds", func(t *testing.T) {
		t.Parallel()
		dst := broker(t, "occupied")
		if err := backuptest.RestoreStreams(ctx, dst, jsapi.Embedded(), dir, manifest); err != nil {
			t.Fatalf("the first restore: %v", err)
		}
		err := backuptest.RestoreStreams(ctx, dst, jsapi.Embedded(), dir, manifest)
		if err == nil || !strings.Contains(err.Error(), manifest.Streams[0].Name) {
			t.Fatalf("a restore over a stream the broker holds = %v, want a refusal naming %s",
				err, manifest.Streams[0].Name)
		}
	})

	t.Run("a skip", func(t *testing.T) {
		t.Parallel()
		dst := broker(t, "skipping")
		err := backuptest.RestoreStreams(ctx, dst, jsapi.Embedded(), dir, manifest,
			backuptest.Skip("KV_crewlet_prob"))
		if err == nil || !strings.Contains(err.Error(), "KV_crewlet_prob") {
			t.Fatalf("a skip naming no snapshot = %v, want a refusal naming it", err)
		}
		if _, err := client(t, dst).Stream(ctx, logStream); !errors.Is(err, jetstream.ErrStreamNotFound) {
			t.Fatalf("a refused skip restored the log anyway: %v", err)
		}
		if err := backuptest.RestoreStreams(ctx, dst, jsapi.Embedded(), dir, manifest,
			backuptest.Skip(bucketStream)); err != nil {
			t.Fatalf("a restore leaving the bucket out: %v", err)
		}
		if _, err := client(t, dst).KeyValue(ctx, bucket); !errors.Is(err, jetstream.ErrBucketNotFound) {
			t.Fatalf("the skipped bucket = %v, want ErrBucketNotFound", err)
		}
		stream(ctx, t, dst, logStream)
	})

	t.Run("a stream ending elsewhere than its snapshot", func(t *testing.T) {
		t.Parallel()
		dst := broker(t, "short")
		tampered := manifest
		tampered.Streams = append([]backup.StreamArtifact(nil), manifest.Streams...)
		for i, artifact := range tampered.Streams {
			if artifact.Name != logStream {
				continue
			}
			var state server.StreamState
			if err := json.Unmarshal(artifact.State, &state); err != nil {
				t.Fatalf("read the log's recorded state: %v", err)
			}
			state.LastSeq++
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			tampered.Streams[i].State = raw
		}
		err := backuptest.RestoreStreams(ctx, dst, jsapi.Embedded(), dir, tampered)
		want := fmt.Sprintf("its snapshot ended at %d", records+1)
		if err == nil || !strings.Contains(err.Error(), logStream) || !strings.Contains(err.Error(), want) {
			t.Fatalf("a restore ending short of its recorded snapshot = %v, want a refusal "+
				"naming %s and %q", err, logStream, want)
		}
	})
}

package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// embeddedBroker is a solo broker with no listener, in the fleet's domain —
// the topology a single node boots.
func embeddedBroker(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName:      "backup-streams-test",
		JetStream:       true,
		JetStreamDomain: jsapi.Domain,
		Port:            -1,
		DontListen:      true,
		StoreDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("configure embedded server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded nats server did not become ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect to embedded server: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := jsapi.Embedded().Client(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return nc, js
}

// A CONSUMER THAT ENDS UNDER A SNAPSHOT IS A RACE THE BACKUP TAKES AGAIN, held
// to the server this build links by making it lose the race for real.
//
// The server copies a stream's consumers, from the list it took as the
// snapshot started, before a single message, and abandons the snapshot with a
// 500 when one has ended by the time it reads that consumer's state. A
// working engine ends a consumer with every listing of a coordination bucket,
// so a backup under load lost the race and answered 500 for a sound node
// (measured: cmd/crewlet's TestANodeNamedByTheEnvironmentAnswersAsItself, 3 of
// 160 runs with sixteen engines contending for four CPUs, every one on
// KV_crewlet_statelog_positions). In the wild the race is an instant wide; to
// lose it on purpose this case drives the snapshot protocol itself and holds
// the server mid-copy: one chunk in flight and that chunk left unanswered
// stops the server reading the copy, which stops the copy once its
// compressor's buffers are full — before it reaches the last consumer, given
// more consumer state than those buffers hold. That consumer is deleted, the
// chunk answered, and the server finds its store closed.
//
// The first attempt is that lost race, received by the backup's own reader;
// the second is the backup's own snapshot. The loop has to read the server's
// sentence, find the consumer gone and take the stream again.
func TestAConsumerEndingUnderASnapshotIsTakenAgain(t *testing.T) {
	t.Parallel()
	nc, js := embeddedBroker(t)
	ctx := t.Context()
	api := jsapi.Embedded()
	const stream = "CREWLET_AGENT"
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: stream, Subjects: []string{"crewlet.agent.>"}, Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	if _, err := js.Publish(ctx, "crewlet.agent.a", []byte("held")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	last := seedConsumersPastTheCompressor(t, js, stream)

	// Where snapshotStreams puts every snapshot it takes.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, streamDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	artifact, err := retakeOverEnded(ctx, js, stream, func() (StreamArtifact, error) {
		attempts++
		if attempts == 1 {
			return StreamArtifact{}, loseTheRace(ctx, t, nc, js, api, stream, last, root)
		}
		return snapshotOnce(ctx, nc, api, stream, root)
	})
	if err != nil {
		t.Fatalf("a snapshot whose consumer ended under it failed the backup "+
			"after %d attempt(s): %v", attempts, err)
	}
	if attempts != 2 {
		t.Fatalf("took %d attempts, want the lost one and one more", attempts)
	}
	if artifact.Name != stream || artifact.Messages != 1 {
		t.Errorf("the retaken snapshot is %+v, want %s with its one message", artifact, stream)
	}
	if info, err := os.Stat(filepath.Join(root, artifact.File)); err != nil || info.Size() != artifact.Bytes {
		t.Errorf("the retaken snapshot's file = %v (%v), want %d bytes", info, err, artifact.Bytes)
	}
}

// seedConsumersPastTheCompressor gives stream more consumer state than the
// server's snapshot compressor can buffer before it must write to a reader
// that is not reading — a block per CPU in flight, one being written and one
// filling, with two to spare — and answers the last consumer's name.
func seedConsumersPastTheCompressor(t *testing.T, js jetstream.JetStream, stream string) string {
	t.Helper()
	// The largest a consumer's metadata may be is 128 KiB; random bytes in
	// hex so the compressor cannot make the state any smaller to hold.
	pad := make([]byte, 60<<10)
	if _, err := rand.Read(pad); err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"pad": hex.EncodeToString(pad)}
	count := (runtime.GOMAXPROCS(0)+4)<<20/len(metadata["pad"]) + 2
	var last string
	for i := range count {
		last = fmt.Sprintf("seat-%05d", i)
		if _, err := js.CreateConsumer(t.Context(), stream, jetstream.ConsumerConfig{
			Durable: last, AckPolicy: jetstream.AckExplicitPolicy,
			MemoryStorage: true, Metadata: metadata,
		}); err != nil {
			t.Fatalf("create consumer %s: %v", last, err)
		}
	}
	return last
}

// loseTheRace takes one snapshot of stream and deletes consumer while the
// server is copying the consumers, so the server abandons it — answering what
// the backup's own reader made of the copy.
func loseTheRace(ctx context.Context, t *testing.T, nc *nats.Conn, js jetstream.JetStream,
	api jsapi.API, stream, consumer, root string,
) error {
	t.Helper()
	inbox := nc.NewRespInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	// ONE CHUNK IN FLIGHT, the smallest the server takes: it reads no more
	// of the copy until that chunk is answered.
	request, err := json.Marshal(server.JSApiStreamSnapshotRequest{
		DeliverSubject: inbox, ChunkSize: 1024, WindowSize: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	subject, err := api.Subject(fmt.Sprintf(server.JSApiStreamSnapshotT, stream))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nc.Request(subject, request, snapshotRequestTimeout); err != nil {
		t.Fatalf("ask for the snapshot: %v", err)
	}
	first, err := sub.NextMsg(snapshotChunkWait)
	if err != nil {
		t.Fatalf("the first chunk: %v", err)
	}
	// Inside the server's five seconds for an answer, or it gives the
	// snapshot up as stalled instead.
	if err := js.DeleteConsumer(ctx, stream, consumer); err != nil {
		t.Fatalf("end consumer %s under the copy: %v", consumer, err)
	}
	if err := first.Respond(nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "lost.snapshot")
	_, err = receiveSnapshot(ctx, sub, path, stream)
	var abandoned *abandonedSnapshot
	if !errors.As(err, &abandoned) {
		t.Fatalf("the server did not abandon a snapshot whose consumer ended under "+
			"it (%v), so this case no longer makes it lose the race", err)
	}
	if got := abandoned.consumer(stream); got != consumer {
		t.Fatalf("the server's abandonment %q names consumer %q, want %q: its "+
			"sentence is no longer one consumer reads", abandoned.Description, got, consumer)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the abandoned snapshot left %s behind (%v)", path, statErr)
	}
	return err
}

// A CONSUMER THE SERVER STILL HOLDS IS NOT THE RACE. An abandonment naming a
// consumer that is there — it could not be read, rather than having ended —
// fails the backup on the first attempt, as every other failure does.
func TestAnAbandonmentNamingALiveConsumerIsNotTakenAgain(t *testing.T) {
	t.Parallel()
	_, js := embeddedBroker(t)
	ctx := t.Context()
	const stream = "CREWLET_AGENT"
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: stream, Subjects: []string{"crewlet.agent.>"},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	if _, err := js.CreateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable: "seat-live", AckPolicy: jetstream.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("create consumer: %v", err)
	}
	attempts := 0
	_, err := retakeOverEnded(ctx, js, stream, func() (StreamArtifact, error) {
		attempts++
		return StreamArtifact{}, fmt.Errorf("backup: receive: %w", &abandonedSnapshot{
			Status: "500", Description: consumerStatePrefix + "seat-live" +
				consumerStateSuffix + "input/output error",
		})
	})
	var abandoned *abandonedSnapshot
	if !errors.As(err, &abandoned) || attempts != 1 {
		t.Fatalf("an abandonment naming a consumer the server holds answered %v "+
			"after %d attempts, want the abandonment itself after one", err, attempts)
	}
}

// A STREAM THAT LOSES EVERY ATTEMPT IS REPORTED, NAMING THE CONSUMER. The
// race is an instant wide, so losing it [snapshotAttempts] times in a row is
// something churning the stream's consumers, and the backup says so rather
// than trying for ever.
func TestAStreamThatLosesEveryAttemptIsReportedNamingTheConsumer(t *testing.T) {
	t.Parallel()
	_, js := embeddedBroker(t)
	ctx := t.Context()
	const stream = "KV_crewlet_statelog_positions"
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: stream, Subjects: []string{"$KV.crewlet_statelog_positions.>"},
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	attempts := 0
	_, err := retakeOverEnded(ctx, js, stream, func() (StreamArtifact, error) {
		attempts++
		return StreamArtifact{}, &abandonedSnapshot{Status: "500", Description: consumerStatePrefix +
			fmt.Sprintf("gone%d", attempts) + consumerStateSuffix + "store is closed"}
	})
	if attempts != snapshotAttempts {
		t.Fatalf("took %d attempts, want %d", attempts, snapshotAttempts)
	}
	var abandoned *abandonedSnapshot
	if !errors.As(err, &abandoned) || !strings.Contains(err.Error(), fmt.Sprintf("%q", fmt.Sprintf("gone%d", attempts))) {
		t.Fatalf("the report = %v, want the last abandonment, naming its consumer", err)
	}
}

// THE SERVER'S TWO SENTENCES, and nothing that only resembles them: a
// description read as naming a consumer is a snapshot taken again, so a
// failure that merely mentions one must not be.
func TestOnlyTheServersConsumerSentencesNameAConsumer(t *testing.T) {
	t.Parallel()
	const stream = "CREWLET_AGENT"
	for _, tc := range []struct {
		name, status, description, want string
	}{
		{"a stream on one server", "500",
			"couldn't load consumer 'lkp7LhL5' state: store is closed", "lkp7LhL5"},
		{"a name holding a quote", "500",
			"couldn't load consumer 'it's' state: store is closed", "it's"},
		{"a replicated stream", "500",
			"failed to get consumer state for 'CREWLET_AGENT > seat-cto'", "seat-cto"},
		{"another stream's consumer", "500",
			"failed to get consumer state for 'CREWLET_EVENTS > seat-cto'", ""},
		{"a stalled transfer", "408", "No Flow Response", ""},
		{"the sentence on another status", "408",
			"couldn't load consumer 'lkp7LhL5' state: store is closed", ""},
		{"a message the server could not read", "500",
			"couldn't load next message after seq 7: store is closed", ""},
		{"no consumer named", "500", "couldn't load consumer '' state: store is closed", ""},
		{"no reason", "500", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&abandonedSnapshot{Status: tc.status, Description: tc.description}).consumer(stream)
			if got != tc.want {
				t.Errorf("consumer(%q, %q) = %q, want %q", tc.status, tc.description, got, tc.want)
			}
		})
	}
}

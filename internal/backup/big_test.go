package backup_test

import (
	mrand "math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backup"
)

// A snapshot larger than the server's in-flight window has to be CREDITED to
// keep flowing: the server hands out a fixed number of slots (window divided
// by chunk size, 64 here) and refills one per chunk answered. A receiver that
// never answers gets that many chunks, then stalls until the ack timeout aborts.
//
// So this test is deliberately big enough to cross the window: every smaller
// stream here passes whether or not credit is sent. The data is random per
// message because a JetStream snapshot is compressed: megabytes of zeroes
// snapshot to a few kilobytes and never reach a second chunk.
func TestASnapshotPastTheInFlightWindowStillCompletes(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateOrUpdateStream(t.Context(), jetstream.StreamConfig{
		Name: "CREWLET_EVENTS", Subjects: []string{"crewlet.events.>"},
		Storage: jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create stream: %v", err)
	}
	// Fixed seed: incompressible but reproducible, so a failure is the
	// same failure on a re-run.
	rng := mrand.New(mrand.NewSource(1))
	payload := make([]byte, 24*1024)
	const messages = 420
	for range messages {
		rng.Read(payload)
		if _, err := js.PublishMsg(t.Context(), &nats.Msg{
			Subject: "crewlet.events.x", Data: payload,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	dir := filepath.Join(t.TempDir(), "big")
	manifest, err := service(t, openStore(t), nc).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	var snap backup.StreamArtifact
	for _, s := range manifest.Streams {
		if s.Name == "CREWLET_EVENTS" {
			snap = s
		}
	}
	if snap.Messages != messages {
		t.Fatalf("captured %d messages, want %d", snap.Messages, messages)
	}
	// The floor is the window itself: a transfer that stalled for want of
	// credit stops at 64 chunks, so anything at or below that is the bug
	// this test exists for.
	const window = 64 * 128 * 1024
	if snap.Bytes <= window {
		t.Fatalf("snapshot is %d bytes, at or under the %d-byte in-flight window — "+
			"the transfer stalled rather than completing", snap.Bytes, window)
	}
	body, err := os.Stat(filepath.Join(dir, snap.File))
	if err != nil {
		t.Fatalf("stat the snapshot: %v", err)
	}
	if body.Size() != snap.Bytes {
		t.Errorf("the manifest claims %d bytes, the file holds %d", snap.Bytes, body.Size())
	}
}

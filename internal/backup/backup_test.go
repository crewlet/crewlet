package backup_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/store"
)

var clock = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// embeddedNATS starts a broker inside the test process with NO LISTENER —
// the same topology a solo node boots, and the whole reason the snapshot has
// to happen in-process. Nothing outside this process can reach it.
func embeddedNATS(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "backup-test",
		JetStream:  true,
		Port:       -1,
		DontListen: true,
		StoreDir:   t.TempDir(),
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
	return nc
}

// seedStream creates a stream and publishes messages into it.
func seedStream(t *testing.T, nc *nats.Conn, name, subject string, messages int) {
	t.Helper()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.CreateOrUpdateStream(t.Context(), jetstream.StreamConfig{
		Name:     name,
		Subjects: []string{subject},
		Storage:  jetstream.FileStorage,
	}); err != nil {
		t.Fatalf("create stream %s: %v", name, err)
	}
	for i := range messages {
		if _, err := js.Publish(t.Context(), subject, fmt.Appendf(nil, "message-%d", i)); err != nil {
			t.Fatalf("publish to %s: %v", subject, err)
		}
	}
}

// seedBucket creates a coordination-shaped KV bucket with a value in it.
func seedBucket(t *testing.T, nc *nats.Conn, bucket, key, value string) {
	t.Helper()
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket})
	if err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	if _, err := kv.PutString(t.Context(), key, value); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func service(t *testing.T, db *store.DB, nc *nats.Conn) *backup.Service {
	t.Helper()
	s := backup.New(backup.Options{
		Store: db, Conn: nc, NodeID: "node-0",
		Now: func() time.Time { return clock },
	})
	if s == nil {
		t.Fatal("New returned nil with a store and a connection")
	}
	return s
}

// The whole point, end to end: both estates captured from inside the process
// that owns them, into one directory, described by one manifest.
func TestABackupCapturesBothEstates(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	seedStream(t, nc, "CREWLET_AGENT", "crewlet.agent.>", 5)
	seedBucket(t, nc, "crewlet_budgets", "org", "12345")
	db := openStore(t)
	if err := db.Events().Append(t.Context(), store.EventRecord{
		ID: "e1", Type: "task_created", Source: "pm", Time: clock, Category: "task",
	}); err != nil {
		t.Fatalf("seed the store: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "backup-1")
	manifest, err := service(t, db, nc).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	if manifest.Store == nil {
		t.Fatal("the manifest describes no store copy")
	}
	if manifest.Store.Bytes <= 0 {
		t.Errorf("store copy is %d bytes", manifest.Store.Bytes)
	}
	if !slicesContain(manifest.Store.Migrations, store.SchemaVersions()[0]) {
		t.Errorf("the manifest records schema %v", manifest.Store.Migrations)
	}
	if _, err := os.Stat(filepath.Join(dir, manifest.Store.File)); err != nil {
		t.Errorf("the store copy the manifest names is not there: %v", err)
	}

	// Both the stream AND the coordination bucket — a bucket is a stream,
	// which is what makes enumerating streams enough to capture the
	// fleet's leases, ledgers, counters and credentials.
	byName := map[string]backup.StreamArtifact{}
	for _, s := range manifest.Streams {
		byName[s.Name] = s
	}
	agent, ok := byName["CREWLET_AGENT"]
	if !ok {
		t.Fatalf("the agent stream was not captured; got %v", names(manifest.Streams))
	}
	if agent.Messages != 5 {
		t.Errorf("captured %d messages from the agent stream, want 5", agent.Messages)
	}
	if agent.Bytes <= 0 {
		t.Errorf("the agent snapshot is %d bytes", agent.Bytes)
	}
	if len(agent.Config) == 0 || len(agent.State) == 0 {
		t.Error("the artifact carries no config/state, so a restore has nothing to hand back")
	}
	// The coordination bucket, captured without ever being named as a
	// bucket: it is a stream, so enumerating streams gets it.
	bucket, ok := byName["KV_crewlet_budgets"]
	if !ok {
		t.Fatalf("the coordination bucket was not captured; got %v", names(manifest.Streams))
	}
	if bucket.Messages == 0 {
		t.Errorf("the bucket snapshot carries no messages: %+v", bucket)
	}
	for _, s := range manifest.Streams {
		if _, err := os.Stat(filepath.Join(dir, s.File)); err != nil {
			t.Errorf("snapshot for %s is missing: %v", s.Name, err)
		}
	}
}

// The manifest's PRESENCE is the claim that the backup is complete, so it has
// to be readable back as one and be the last thing written.
func TestTheManifestIsWhatMarksABackupFinished(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	seedStream(t, nc, "CREWLET_EVENTS", "crewlet.events.>", 1)
	dir := filepath.Join(t.TempDir(), "backup-2")

	if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an untouched directory already carries the mark of a finished backup")
	}
	taken, err := service(t, openStore(t), nc).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, backup.ManifestName))
	if err != nil {
		t.Fatalf("the finished backup carries no manifest: %v", err)
	}
	var read backup.Manifest
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("the manifest on disk is not readable: %v", err)
	}
	if read.NodeID != taken.NodeID || !read.TakenAt.Equal(taken.TakenAt) {
		t.Errorf("the manifest did not round-trip: %+v vs %+v", read, taken)
	}
	if len(read.Streams) != len(taken.Streams) {
		t.Errorf("round-tripped %d streams, took %d", len(read.Streams), len(taken.Streams))
	}
	// No leftover part file: a reader globbing the directory must not find
	// two things that both look like a manifest.
	if _, err := os.Stat(filepath.Join(dir, backup.ManifestName+".part")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the manifest's temporary name survived the write")
	}
}

// A backup is a SET whose meaning depends on being one set. Writing a second
// one into the same directory would leave a store copy and stream snapshots
// from different moments, indistinguishable from a consistent pair.
func TestASecondBackupIntoTheSameDirectoryIsRefused(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	seedStream(t, nc, "CREWLET_CONFIG", "crewlet.config.>", 1)
	svc := service(t, openStore(t), nc)
	dir := filepath.Join(t.TempDir(), "backup-3")

	if _, err := svc.Take(t.Context(), dir); err != nil {
		t.Fatalf("first take: %v", err)
	}
	_, err := svc.Take(t.Context(), dir)
	if !errors.Is(err, backup.ErrNotEmpty) {
		t.Fatalf("second take into the same directory: %v, want ErrNotEmpty", err)
	}
}

// The destination is resolved on the ENGINE's host, not the caller's, so a
// relative path lands somewhere the operator driving this over HTTP cannot
// see. Refused rather than guessed at.
func TestARelativeDestinationIsRefused(t *testing.T) {
	t.Parallel()
	_, err := service(t, openStore(t), embeddedNATS(t)).Take(t.Context(), "backups/tonight")
	if err == nil {
		t.Fatal("a relative destination was accepted")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

// A node running without one estate still backs the other up, and the
// manifest says which it got — rather than the service refusing to exist or,
// worse, writing a manifest that claims more than it holds.
func TestANodeWithOnlyOneEstateBacksUpWhatItHas(t *testing.T) {
	t.Parallel()

	storeOnly := backup.New(backup.Options{Store: openStore(t), NodeID: "n", Now: func() time.Time { return clock }})
	dir := filepath.Join(t.TempDir(), "store-only")
	manifest, err := storeOnly.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("store-only take: %v", err)
	}
	if manifest.Store == nil {
		t.Error("the store-only backup describes no store")
	}
	if len(manifest.Streams) != 0 {
		t.Errorf("a node with no broker reported %d streams", len(manifest.Streams))
	}

	nc := embeddedNATS(t)
	seedStream(t, nc, "CREWLET_DLQ", "dlq.>", 1)
	streamOnly := backup.New(backup.Options{Conn: nc, NodeID: "n", Now: func() time.Time { return clock }})
	dir = filepath.Join(t.TempDir(), "stream-only")
	manifest, err = streamOnly.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("stream-only take: %v", err)
	}
	if manifest.Store != nil {
		t.Error("a node with no store described one")
	}
	if len(manifest.Streams) == 0 {
		t.Error("the stream-only backup captured nothing")
	}

	// Neither: there is nothing to back up, and saying so beats writing a
	// manifest describing an empty directory.
	if backup.New(backup.Options{}) != nil {
		t.Error("a node with neither estate produced a backup service")
	}
}

// The snapshot must be the real JetStream artifact, not an empty file that
// happens to exist — a truncated snapshot is the failure mode the terminator
// status exists to catch, and the cheapest proof it worked is that the bytes
// are a snapshot the server itself accepts back.
func TestASnapshotIsAWholeArtifactNotAnEmptyFile(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	// Enough messages that the transfer is more than one chunk's worth of
	// flow control, which is where a missing credit reply would show up.
	seedStream(t, nc, "CREWLET_NOTIFICATIONS", "crewlet.notifications.>", 400)

	dir := filepath.Join(t.TempDir(), "backup-4")
	manifest, err := service(t, openStore(t), nc).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	var snap backup.StreamArtifact
	for _, s := range manifest.Streams {
		if s.Name == "CREWLET_NOTIFICATIONS" {
			snap = s
		}
	}
	if snap.Messages != 400 {
		t.Fatalf("captured %d messages, want 400", snap.Messages)
	}
	body, err := os.ReadFile(filepath.Join(dir, snap.File))
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if int64(len(body)) != snap.Bytes {
		t.Errorf("the manifest says %d bytes, the file holds %d", snap.Bytes, len(body))
	}
	// A JetStream snapshot is a tar stream; an empty or truncated transfer
	// is what this guards, so any plausible floor beats none.
	if len(body) < 1024 {
		t.Errorf("a 400-message stream snapshotted to %d bytes, which is not a whole stream", len(body))
	}
	// The config the artifact carries is the stream's own, verbatim — a
	// restore hands it straight back.
	var config struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(snap.Config, &config); err != nil {
		t.Fatalf("the recorded config is not readable: %v", err)
	}
	if config.Name != "CREWLET_NOTIFICATIONS" {
		t.Errorf("recorded config names %q", config.Name)
	}
}

func names(artifacts []backup.StreamArtifact) []string {
	out := make([]string, len(artifacts))
	for i, a := range artifacts {
		out[i] = a.Name
	}
	return out
}

func slicesContain(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}

// A destination the operator created before running this must end up private.
//
// `mkdir -p /backups/today && crewlet backup -dir /backups/today` is how
// anyone drives this, and under the usual umask that leaves a directory the
// whole machine can list. Every file written inside is 0600, so what a loose
// directory leaks is not the credentials but the shape of the estate: that
// this company keeps a secrets bucket, how large it is, when it was last
// copied. MkdirAll's mode applies only to a directory it CREATES, so nothing
// but an explicit chmod closes this.
func TestAPreExistingDestinationIsMadePrivate(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	seedBucket(t, nc, "crewlet_secrets", "anthropic", "sealed")
	db := openStore(t)

	dir := filepath.Join(t.TempDir(), "backup-preexisting")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create the destination: %v", err)
	}
	// Prove the starting state, so this cannot pass by never having been
	// loose in the first place.
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat: %v", err)
	} else if info.Mode().Perm() != 0o755 {
		t.Fatalf("the destination starts at %#o, wanted a loose 0755", info.Mode().Perm())
	}

	if _, err := service(t, db, nc).Take(t.Context(), dir); err != nil {
		t.Fatalf("take: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("the backup directory is %#o — group and other can list "+
			"the estate's shape", info.Mode().Perm())
	}
	// And what is inside stays unreadable to anyone else regardless.
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %#o", path, fi.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

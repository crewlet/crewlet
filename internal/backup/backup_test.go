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
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
		// THE FLEET'S DOMAIN, as every embedded member serves it, so the
		// snapshot's raw request is asked in the API a real node speaks
		// rather than one no node answers any more.
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
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "store.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A BACKUP COPIES WHAT THE NODE HOLDS, NOT WHAT HAPPENS TO BE OPEN.
//
// A data node's replicated estate holds rows no other artefact of the backup
// carries. While it is closed — an adoption between its rename and its
// reopen, a reopen that failed — the backup must refuse rather than write a
// manifest for the node's own file alone and call that complete; and a
// replicated estate open on a node configured to hold none is refused the
// same way, since copying the one and skipping the other is as silent. A node
// that holds none copies its own file, and that is complete.
func TestABackupCopiesWhatTheNodeHolds(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	service := func(db *store.DB, holding backup.EstateHolding) *backup.Service {
		return build(t, backup.Options{
			Store: db, Estate: holding, NodeID: "n", Holds: fleet, Backups: fleet,
			Now: func() time.Time { return clock },
		})
	}

	t.Run("a held replicated estate that is closed", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		if err := db.CloseReplicated(); err != nil {
			t.Fatalf("close the replicated estate: %v", err)
		}
		dir := filepath.Join(t.TempDir(), "b")
		_, err := service(db, backup.HoldsReplicated).Take(t.Context(), dir)
		if !errors.Is(err, store.ErrNoEstate) {
			t.Fatalf("a backup of a data node whose replicated estate is closed = %v, "+
				"want ErrNoEstate — it would otherwise be missing every row that "+
				"estate holds", err)
		}
		if !strings.Contains(err.Error(), "replicated estate") {
			t.Errorf("the refusal does not name the replicated estate: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused backup wrote its manifest: %v", err)
		}
	})
	t.Run("an open replicated estate on a node that holds none", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		_, err := service(db, backup.HoldsNodeOnly).Take(t.Context(), filepath.Join(t.TempDir(), "b"))
		if err == nil || !strings.Contains(err.Error(), db.ReplicatedFile()) {
			t.Fatalf("a backup that would leave an open replicated estate out = %v, "+
				"want a refusal naming its file", err)
		}
	})
	t.Run("a node holding none", func(t *testing.T) {
		t.Parallel()
		node := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
		t.Cleanup(func() { _ = node.Close() })
		manifest, err := service(node, backup.HoldsNodeOnly).Take(t.Context(), filepath.Join(t.TempDir(), "b"))
		if err != nil {
			t.Fatalf("a backup of a node holding no replicated estate: %v", err)
		}
		if len(manifest.Stores) != 1 || manifest.Stores[0].Estate != store.EstateNode {
			t.Errorf("a node holding none backed up %+v, want its own file alone", manifest.Stores)
		}
	})
}

func service(t *testing.T, db *store.DB, nc *nats.Conn) *backup.Service {
	t.Helper()
	fleet := memory.NewFleet()
	return build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, Conn: nc, API: jsapi.Embedded(), NodeID: "node-0", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
	})
}

// build is New for a case that supplies every required field, failing the
// test on a wiring mistake.
func build(t *testing.T, opts backup.Options) *backup.Service {
	t.Helper()
	s, err := backup.New(opts)
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}
	return s
}

// The whole point, end to end: both estates captured from inside the process
// that owns them, into one directory, described by one manifest.
func TestABackupCapturesBothEstates(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	seedStream(t, nc, "CREWLET_AGENT", "crewlet.agent.>", 5)
	seedBucket(t, nc, "crewlet_token_windows", "org", "12345")
	db := openStore(t)
	if err := db.Events().Append(t.Context(), store.EventRecord{
		ID: "e1", Type: "agent_phase_started", Source: "pm", Time: clock, Category: "task",
	}); err != nil {
		t.Fatalf("seed the store: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "backup-1")
	manifest, err := service(t, db, nc).Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	// BOTH ESTATES, each as its own file with its own digest. A restore
	// that found one of them would rebuild a company whose audit log and
	// whose tracker came from different moments.
	if len(manifest.Stores) != 2 {
		t.Fatalf("the manifest describes %d store copies, want both estates: %+v",
			len(manifest.Stores), manifest.Stores)
	}
	seen := map[store.Estate]bool{}
	for _, st := range manifest.Stores {
		seen[st.Estate] = true
		if st.Bytes <= 0 {
			t.Errorf("the %s copy is %d bytes", st.Estate, st.Bytes)
		}
		if len(st.SHA256) != 64 {
			t.Errorf("the %s copy carries digest %q, want a sha256 taken at the "+
				"moment the copy passed its integrity check", st.Estate, st.SHA256)
		}
		if _, err := os.Stat(filepath.Join(dir, st.File)); err != nil {
			t.Errorf("the %s copy the manifest names is not there: %v", st.Estate, err)
		}
	}
	if !seen[store.EstateNode] || !seen[store.EstateReplicated] {
		t.Errorf("the manifest covers %v, want both estates", seen)
	}
	for _, st := range manifest.Stores {
		if st.Estate != store.EstateNode {
			continue
		}
		if !slicesContain(st.Migrations, store.SchemaVersions(store.EstateNode)[0]) {
			t.Errorf("the manifest records schema %v", st.Migrations)
		}
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
	bucket, ok := byName["KV_crewlet_token_windows"]
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

// A STORE COPY IS RECORDED BYTE FOR BYTE AS EVERY EARLIER MANIFEST RECORDED IT.
//
// A manifest outlives the binary that wrote it: a restore procedure, a
// shipping script and an operator's own tooling read `stores` from backups
// taken by builds long gone, by its key names and by the estate's value. So
// one copy of each estate is pinned here as the exact bytes it encodes to —
// `estate` is the store's own name for the file, "node" or "replicated", and
// there is no other field naming which file a copy is — and a real backup's
// manifest on disk is held to the same keys, so a field added to
// [backup.StoreArtifact] is a decision this test makes somebody take.
func TestAStoreCopyIsRecordedAsEveryEarlierManifestRecordedIt(t *testing.T) {
	t.Parallel()
	pinned, err := json.Marshal([]backup.StoreArtifact{
		{
			Estate: store.EstateNode, File: "store.db", Source: "/data/company.db",
			Bytes: 4096, SHA256: strings.Repeat("a", 64), Migrations: []string{"0001_init.sql"},
		},
		{
			Estate: store.EstateReplicated, File: "store-replicated.db",
			Source: "/data/crewlet-replicated.db", Bytes: 8192,
			SHA256: strings.Repeat("b", 64), Migrations: []string{"0001_statelog.sql"},
		},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `[{"estate":"node","file":"store.db","source":"/data/company.db","bytes":4096,` +
		`"sha256":"` + strings.Repeat("a", 64) + `","migrations":["0001_init.sql"]},` +
		`{"estate":"replicated","file":"store-replicated.db","source":"/data/crewlet-replicated.db",` +
		`"bytes":8192,"sha256":"` + strings.Repeat("b", 64) + `","migrations":["0001_statelog.sql"]}]`
	if string(pinned) != want {
		t.Errorf("a pair of store copies encodes as\n%s\nwant\n%s", pinned, want)
	}

	dir := filepath.Join(t.TempDir(), "backup-golden")
	if _, err := service(t, openStore(t), embeddedNATS(t)).Take(t.Context(), dir); err != nil {
		t.Fatalf("take: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, backup.ManifestName))
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	var raw struct {
		Stores []map[string]json.RawMessage `json:"stores"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	keys := []string{"bytes", "estate", "file", "migrations", "sha256", "source"}
	var copies []string
	for _, st := range raw.Stores {
		got := make([]string, 0, len(st))
		for k := range st {
			got = append(got, k)
		}
		slices.Sort(got)
		if !slices.Equal(got, keys) {
			t.Errorf("a store copy on disk carries the keys %v, want exactly %v", got, keys)
		}
		copies = append(copies, string(st["estate"])+" "+string(st["file"]))
	}
	if wantCopies := []string{`"node" "store.db"`, `"replicated" "store-replicated.db"`}; !slices.Equal(copies, wantCopies) {
		t.Errorf("a data node's backup recorded its copies as %v, want %v", copies, wantCopies)
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

// A DESTINATION THE HOST CANNOT PREPARE IS THE CALLER'S MISTAKE, not the
// engine's failure. A path that runs through a regular file, or names one,
// fails in mkdir before a byte is copied; answered as anything but
// ErrBadDestination it reaches an operator as a 500 that sends them to the
// engine's log for a typo in their own request.
func TestADestinationTheHostCannotCreateIsTheCallersMistake(t *testing.T) {
	t.Parallel()
	svc := service(t, openStore(t), embeddedNATS(t))
	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"the destination is a file":             file,
		"a parent of the destination is a file": filepath.Join(file, "tonight"),
	} {
		_, err := svc.Take(t.Context(), dir)
		if !errors.Is(err, backup.ErrBadDestination) {
			t.Errorf("%s: %v, want ErrBadDestination", name, err)
			continue
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("%s: the refusal does not name the directory: %v", name, err)
		}
	}
}

// A NODE THAT DIALLED AN EXTERNAL BROKER BACKS UP ITS STORE, AND NO STREAMS.
//
// It holds no connection to snapshot over: the queue owns that connection and
// the streams belong to a cluster backed up with its own tooling. The manifest
// then describes both store estates and nothing else, which the CLI reads as
// the cue to say where the rest of the state lives.
func TestANodeOnAnExternalBrokerBacksUpItsStoreAlone(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	storeOnly := build(t, backup.Options{
		Store: openStore(t), Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock },
	})
	dir := filepath.Join(t.TempDir(), "store-only")
	manifest, err := storeOnly.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("store-only take: %v", err)
	}
	if len(manifest.Stores) != 2 {
		t.Errorf("the store-only backup describes %d copies, want both estates",
			len(manifest.Stores))
	}
	if len(manifest.Streams) != 0 {
		t.Errorf("a node with no broker connection reported %d streams", len(manifest.Streams))
	}
}

// A STORE, WHAT IT HOLDS, BOTH FLEET REGISTERS AND A NODE ID ARE REQUIRED, and
// a missing one is refused by name.
//
// The engine beside every API holds all three registers, and a backup that did
// less around a nil would be either missing the node's own estate or invisible
// to the trim. Without the node's holding it could only copy what happens to
// be open, which is how a data node whose company ran no state log backed up
// without its replicated estate — and the holding's ZERO value is refused for
// that reason, rather than read as a node holding its own file alone. A blank
// node id is the subtler one: it keys the hold
// and the announced point, so every node that named itself through
// CREWLET_NODE_ID would share one hold and announce a point the register
// refuses.
func TestNewRefusesAMissingStoreOrRegister(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	fleet := memory.NewFleet()
	held := backup.HoldsReplicated
	for field, opts := range map[string]backup.Options{
		"Store":   {NodeID: "n", Estate: held, Holds: fleet, Backups: fleet},
		"Estate":  {NodeID: "n", Store: db, Holds: fleet, Backups: fleet},
		"Holds":   {NodeID: "n", Store: db, Estate: held, Backups: fleet},
		"Backups": {NodeID: "n", Store: db, Estate: held, Holds: fleet},
		"NodeID":  {NodeID: "  ", Store: db, Estate: held, Holds: fleet, Backups: fleet},
	} {
		svc, err := backup.New(opts)
		if err == nil {
			t.Errorf("no %s built a backup service: %v", field, svc)
			continue
		}
		if !strings.Contains(err.Error(), "Options."+field) {
			t.Errorf("the refusal does not name Options.%s: %v", field, err)
		}
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

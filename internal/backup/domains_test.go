package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// registerOfThree is a build's register in which only the tracker's log has
// applied a record: the other two have no checkpoint row in the estate.
var registerOfThree = []string{
	trackerLogStream, topics.TrackerVectorsStream, topics.ArtifactDataLogStream,
}

// A BACKUP NAMES EVERY LOG THE BUILD REGISTERS, a log its copy has applied
// nothing on at the zero position: the manifest, the trim hold and the point
// the fleet's trim and backup alarm read all cover it. Read off the copy's
// checkpoint rows alone, a log nobody had written yet was covered by no backup,
// and its alarm stayed raised after every backup the node took.
func TestABackupNamesEveryLogTheBuildRegisters(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, trackerLogStream, 1, 41)
	fleet := memory.NewFleet()
	held := &watchedHolds{HoldRegister: fleet}

	manifest, err := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "node-0", Holds: held,
		Backups: fleet, Logs: registerOfThree, Now: func() time.Time { return clock },
	}).Take(t.Context(), filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	want := map[string]statelog.Position{
		trackerLogStream:             {Stream: trackerLogStream, Generation: 1, Seq: 41},
		topics.TrackerVectorsStream:  {Stream: topics.TrackerVectorsStream},
		topics.ArtifactDataLogStream: {Stream: topics.ArtifactDataLogStream},
	}
	if len(manifest.Domains) != len(want) {
		t.Fatalf("the manifest names %d logs (%v), want every one of the %d the build "+
			"registers", len(manifest.Domains), manifest.Domains, len(want))
	}
	for stream, at := range want {
		if got, named := manifest.Domains[stream]; !named || got != at {
			t.Errorf("the manifest names %s as %+v (named %v), want %+v", stream, got, named, at)
		}
	}

	if len(held.put) != 1 {
		t.Fatalf("the backup took %d hold(s), want one", len(held.put))
	}
	points, err := fleet.BackupPoints(t.Context())
	if err != nil {
		t.Fatalf("BackupPoints: %v", err)
	}
	for stream, at := range want {
		pinned, ok := held.put[0].Streams[stream]
		if !ok || pinned.Seq != at.Seq || pinned.Generation != at.Generation {
			t.Errorf("the hold pins %s at %+v (pinned %v), want %+v: the trim may take "+
				"the records a restore replays it from while the copy runs", stream, pinned, ok, at)
		}
		newest, covered := coord.NewestBackupOf(points, stream)
		if !covered {
			t.Errorf("no announced backup covers %s, so its trim and its backup alarm "+
				"wait for a backup no backup can give", stream)
			continue
		}
		if reach := newest.Streams[stream]; reach.Seq != at.Seq || reach.Generation != at.Generation {
			t.Errorf("the announced point reaches %s at %+v, want %+v", stream, reach, at)
		}
	}
}

// A CHECKPOINT ON A LOG THE BUILD DOES NOT REGISTER REFUSES THE BACKUP, naming
// the log: no applier of this build replays it, so the file and the build
// disagree about what the estate is, and no manifest is written over that.
func TestABackupRefusesACheckpointOnALogTheBuildDoesNotRegister(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, trackerLogStream, 1, 41)
	seedCursor(t, db, "CREWLET_RETIRED_LOG", 1, 7)
	fleet := memory.NewFleet()
	dir := filepath.Join(t.TempDir(), "b")

	_, err := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "node-0", Holds: fleet,
		Backups: fleet, Logs: registerOfThree, Now: func() time.Time { return clock },
	}).Take(t.Context(), dir)
	if err == nil || !strings.Contains(err.Error(), "CREWLET_RETIRED_LOG") {
		t.Fatalf("a checkpoint on an unregistered log was answered %v, want a refusal "+
			"naming it", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the refused backup wrote a manifest (%v)", serr)
	}
	if points, _ := fleet.BackupPoints(t.Context()); len(points) != 0 {
		t.Fatalf("the refused backup announced %+v", points)
	}
}

// A DATA NODE'S REGISTER NAMES EACH LOG ONCE, by its stream (its absence is
// [TestNewRefusesAMissingStoreOrRegister]'s). A node holding no replicated
// estate names no log, and needs none.
func TestABackupRegisterNamesEachLogOnce(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	opts := func(holding backup.EstateHolding, logs []string) backup.Options {
		return backup.Options{Store: openStore(t), Estate: holding, NodeID: "node-0",
			Holds: fleet, Backups: fleet, Keyring: suiteRing, Logs: logs}
	}
	for name, logs := range map[string][]string{
		"blank":    {trackerLogStream, " "},
		"repeated": {trackerLogStream, trackerLogStream},
	} {
		if _, err := backup.New(opts(backup.HoldsReplicated, logs)); err == nil ||
			!strings.Contains(err.Error(), "Options.Logs") {
			t.Errorf("%s: a data node's backup built with the register %q was answered "+
				"%v, want a refusal naming Options.Logs", name, logs, err)
		}
	}
	if _, err := backup.New(opts(backup.HoldsNodeOnly, nil)); err != nil {
		t.Errorf("a node holding no replicated estate was refused for naming no log: %v", err)
	}
}

// A LOG THE COPY HAS APPLIED NOTHING ON IS HELD TO THE ASSERTION like any
// other: a restore replays it from its first record, so a log that no longer
// holds that record refuses the backup, naming it. Left out of the manifest,
// it was never asserted, and the backup claimed a log it could not restore.
func TestALogTheCopyHasAppliedNothingOnIsAsserted(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	// A LOG NO DECLARED TABLE IS WRITTEN BY, so the window reads none of it.
	seedStream(t, nc, topics.TrackerVectorsStream, "crewlet.tracker.vectors.test.>", 5)
	db := openStore(t)
	fleet := memory.NewFleet()
	service := func() *backup.Service {
		return build(t, backup.Options{
			Store: db, Estate: backup.HoldsReplicated, Conn: nc, API: jsapi.Embedded(),
			NodeID: "node-0", Holds: fleet, Backups: fleet, Logs: registerOfThree,
			Now: func() time.Time { return clock },
		})
	}

	manifest, err := service().Take(t.Context(), filepath.Join(t.TempDir(), "whole"))
	if err != nil {
		t.Fatalf("a log holding every record a restore replays refused the backup: %v", err)
	}
	if at := manifest.Domains[topics.TrackerVectorsStream]; !at.IsZero() ||
		at.Stream != topics.TrackerVectorsStream {
		t.Fatalf("the manifest names the unapplied log as %+v, want its zero position", at)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(t.Context(), topics.TrackerVectorsStream)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Purge(t.Context(), jetstream.WithPurgeSequence(4)); err != nil {
		t.Fatalf("trim the log: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "trimmed")
	if _, err = service().Take(t.Context(), dir); !errors.Is(err, backup.ErrLogGap) ||
		!strings.Contains(err.Error(), topics.TrackerVectorsStream) {
		t.Fatalf("a log trimmed past a copy that applied nothing on it was answered %v, "+
			"want %v naming the log", err, backup.ErrLogGap)
	}
	if _, serr := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("the refused backup wrote a manifest (%v)", serr)
	}
}

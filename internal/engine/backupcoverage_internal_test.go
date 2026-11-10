package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/backup/backuptest"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// coverageNodeID is the backed-up node's id, kept by its restore: a broker's
// replicas are placed by it.
const coverageNodeID = "coverage-node"

// mixedNode boots a data node and applies one record on the identity log and
// none on any other, so most of its logs hold no checkpoint.
func mixedNode(t *testing.T) *Engine {
	t.Helper()
	e := bootDirectoryNodeWith(t, nil, func(b *config.Bootstrap) { b.Node.ID = coverageNodeID })
	written, err := e.IAMWriter().InvalidateAll(t.Context(),
		statelog.NewOpID(time.Now(), "backup-coverage"), "a record on one log")
	if err != nil {
		t.Fatalf("write a record to the identity log: %v", err)
	}
	if err := e.WaitReached(t.Context(), written.Position); err != nil {
		t.Fatalf("apply the identity record: %v", err)
	}
	return e
}

// backUp takes a backup of e through the engine's own wiring, as `crewlet run`
// builds it, and answers its directory and manifest.
func backUp(t *testing.T, e *Engine) (string, backup.Manifest) {
	t.Helper()
	svc, err := backup.New(e.BackupOptions())
	if err != nil {
		t.Fatalf("backup.New(e.BackupOptions()): %v", err)
	}
	dir := filepath.Join(t.TempDir(), "backup")
	manifest, err := svc.Take(t.Context(), dir)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if at := manifest.Domains[iamdomain.Domain{}.Stream().Name]; at.Seq == 0 {
		t.Fatalf("the copy stands at %+v on the identity log, so this case backs up "+
			"no applied record", at)
	}
	return dir, manifest
}

// EVERY LOG THE REGISTER HOLDS IS COVERED BY A BACKUP, and its backup alarm
// clears: through the engine's own wiring, on a node that has applied a record
// on one log and none on the rest. The gate for a domain registered later,
// which reaches the backup only through the register.
func TestABackupCoversEveryRegisteredLogAndClearsItsAlarm(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := mixedNode(t)
	r := quietRetention(t, e)
	r.beat(ctx)
	if !alarmed(r.Report(ctx), statelog.KindBackupAge) {
		t.Fatal("a node that has never taken a backup raised no backup_age, so this " +
			"case shows nothing")
	}
	_, manifest := backUp(t, e)

	unapplied := 0
	for _, domain := range registeredDomains() {
		at, named := manifest.Domains[domain.Stream().Name]
		if !named {
			t.Errorf("the manifest does not name the %s log (%s): a backup covers it "+
				"nowhere, and its backup alarm stays raised after every backup",
				domain.Name(), domain.Stream().Name)
			continue
		}
		if at.IsZero() {
			unapplied++
		}
	}
	if len(manifest.Domains) != len(registeredDomains()) {
		t.Errorf("the manifest names %d logs, want the %d the build registers: %v",
			len(manifest.Domains), len(registeredDomains()), manifest.Domains)
	}
	if unapplied == 0 {
		t.Fatal("every log holds a checkpoint, so this case does not cover a log the " +
			"node has applied nothing on")
	}

	r.beat(ctx)
	if a, up := alarmOf(r.Report(ctx), statelog.KindBackupAge); up {
		t.Fatalf("backup_age still stands on %+v after a backup of every log", a.Domains)
	}
}

// A BACKUP OF A NODE THAT HAS APPLIED NOTHING ON MOST OF ITS LOGS RESTORES
// WHOLE, by the runbook (docs/guides/backup.md, "Restoring"): its store copies
// and stream snapshots put back, the restored node resumes every log at the
// copy's position and applies each to its end. Nothing a restore does reads the
// manifest's positions, so a backup is restorable only because it names them.
func TestABackupOfLogsNobodyWroteRestoresWhole(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := mixedNode(t)
	dir, manifest := backUp(t, e)
	boot := *e.boot
	e.Stop(context.Background())

	into := t.TempDir()
	nodePath := filepath.Join(into, "crewlet.db")
	for _, copied := range manifest.Stores {
		to := nodePath
		if copied.Estate == store.EstateReplicated {
			to = store.ReplicatedPath(nodePath, "")
		}
		copyInto(t, filepath.Join(dir, copied.File), to)
	}
	streamDir := filepath.Join(into, "stream")
	broker, err := jetstream.StartServer(ctx, jetstream.Config{StoreDir: streamDir,
		StoreMaxBytes: brokertest.StoreMaxBytes, ServerName: coverageNodeID, Replicas: 1})
	if err != nil {
		t.Fatalf("start the broker to restore into: %v", err)
	}
	conn, err := broker.Conn()
	if err != nil {
		broker.Shutdown()
		t.Fatalf("connect to the broker to restore into: %v", err)
	}
	err = backuptest.RestoreStreams(ctx, conn, jetstream.Config{}.API(), dir, manifest)
	conn.Close()
	broker.Shutdown()
	if err != nil {
		t.Fatalf("restore the streams: %v", err)
	}

	boot.Store.Path = nodePath
	boot.Store.ReplicatedPath = ""
	brokertest.StoreIn(&boot.Stream, streamDir)
	restored, err := New(ctx, Options{Bootstrap: &boot, Company: directoryConfig(t)})
	if err != nil {
		t.Fatalf("boot the restored node: %v", err)
	}
	t.Cleanup(func() { restored.Stop(context.Background()) })

	// EVERY LOG TO ITS END, the ones the copy applied nothing on from their
	// first record.
	for _, domain := range registeredDomains() {
		running := restored.core.Load().log.Domain(domain.Name())
		if running == nil {
			t.Fatalf("the restored node runs no %s log", domain.Name())
		}
		copied := manifest.Domains[domain.Stream().Name]
		if at := running.runner.Committed(); at.Seq < copied.Seq {
			t.Fatalf("the restored %s log resumed at %+v, below the copy's %+v",
				domain.Name(), at, copied)
		}
		end, err := running.log.End(ctx)
		if err != nil {
			t.Fatalf("read the restored %s log's end: %v", domain.Name(), err)
		}
		if err := restored.WaitReached(ctx, statelog.Position{Stream: domain.Stream().Name,
			Generation: copied.Generation, Seq: end}); err != nil {
			t.Fatalf("the restored node did not apply the %s log to its end %d: %v",
				domain.Name(), end, err)
		}
		// AND NOTHING REFUSED IT: a restored log a node reads as recreated or
		// diverged stops its applier rather than applying.
		if stopped := running.runner.Stopped(); stopped != nil {
			t.Errorf("the restored %s log's applier stopped: %v", domain.Name(), stopped)
		}
	}
}

// copyInto copies one file whole to a path that does not exist yet.
func copyInto(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatalf("open %s: %v", from, err)
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		t.Fatalf("copy %s to %s: %v", from, to, err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close %s: %v", to, err)
	}
}

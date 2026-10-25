package backup_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A SECOND BACKUP WHILE ONE RUNS IS REFUSED, writing nothing. The trim hold is
// keyed on the node and the purpose, so two backups at once share one pin, and
// the first to finish releases the pin the other's gap still needs — the trim
// can then delete what the slower copy has to replay. The refusal comes before
// the directory is made, so it leaves nothing for anybody to clear, and the
// flag is let go on every path: a backup after the first succeeds, and so does
// one after a backup that failed.
func TestASecondBackupWhileOneRunsIsRefused(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedCursor(t, db, "CREWLET_TRACKER_LOG", 1, 5)
	files, _ := filesStore(t)
	held := upload(t, files, "a file the first backup is copying")
	filesNaming(t, db, held...)
	g := newGate()
	defer g.release()
	fleet := memory.NewFleet()
	holds := &watchedHolds{HoldRegister: fleet}
	svc := build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: holds, Backups: fleet,
		Now: func() time.Time { return clock },
		Objects: &backup.Objects{Open: func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
			if err := g.pass(ctx); err != nil {
				return nil, err
			}
			return files.Open(ctx, o)
		}},
	})
	root := t.TempDir()
	done := takeAsync(t, svc, filepath.Join(root, "first"))
	g.waitHeld(t, 1)

	second := filepath.Join(root, "second")
	// BOUNDED, so a backup that is not refused fails here rather than
	// waiting on the gate the first one holds.
	bounded, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := svc.Take(bounded, second); !errors.Is(err, backup.ErrBackupRunning) {
		t.Fatalf("a backup asked for while one runs = %v, want ErrBackupRunning", err)
	}
	if errors.Is(backup.ErrBackupRunning, backup.ErrBadDestination) {
		t.Error("a running backup is reported as the caller's bad destination")
	}
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused backup made its directory (stat: %v)", err)
	}
	if len(holds.put) != 1 {
		t.Fatalf("%d holds were taken while one backup ran; the refused one took one too", len(holds.put))
	}

	g.release()
	if err := <-done; err != nil {
		t.Fatalf("the first backup: %v", err)
	}
	if _, err := svc.Take(t.Context(), filepath.Join(root, "third")); err != nil {
		t.Fatalf("a backup after the first finished was refused: %v", err)
	}
	// A BACKUP THAT FAILED LETS GO TOO.
	occupied := filepath.Join(root, "occupied")
	if err := os.MkdirAll(filepath.Join(occupied, "something"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Take(t.Context(), occupied); !errors.Is(err, backup.ErrNotEmpty) {
		t.Fatalf("a backup into an occupied directory = %v, want ErrNotEmpty", err)
	}
	if _, err := svc.Take(t.Context(), filepath.Join(root, "fourth")); err != nil {
		t.Fatalf("a backup after one that failed was refused: %v", err)
	}
}

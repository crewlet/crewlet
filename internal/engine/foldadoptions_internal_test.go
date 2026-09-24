package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE UPGRADED FROM A BUILD WHOSE ADOPTIONS SCRUBBED THE LEDGER BOOTS WITH
// THAT LOSS IN ITS LEDGER'S WATERMARK, before anything publishes.
//
// Such a node runs the file an earlier adoption installed, whose ledger holds
// none of the donor's rows, and its watermark — which that build did not keep
// — says nothing was lost. Booted without the fold, it reads the scrubbed
// ledger's silence as conclusive and decides again every retry of an
// operation minted before that adoption. The adoption row in its own estate is
// the one thing that still says so.
func TestAnUpgradedNodeBootsWithItsEarlierAdoptionInTheWatermark(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })

	// THE ROW AN EARLIER BUILD WROTE: every column it knew, so the one
	// that marks a row folded takes its default.
	adopted := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	if err := back.Store.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_adoption (started_at, donor, manifest, completed_at)
			VALUES (?, 'node-b', 'sum', ?)`,
			store.EncodeTime(adopted.Add(-time.Minute)), store.EncodeTime(adopted))
		return err
	}); err != nil {
		t.Fatalf("write the earlier build's adoption row: %v", err)
	}

	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	rows, err := tracker.NewRows(back.Store)
	if err != nil {
		t.Fatalf("build the tracker's read seam: %v", err)
	}
	before, lost, err := rows.LostBefore(t.Context())
	if err != nil {
		t.Fatalf("read the tracker ledger's watermark: %v", err)
	}
	if !lost || !before.Equal(adopted) {
		t.Fatalf("the tracker ledger's watermark = (%s, %v), want the earlier "+
			"adoption's %s — its scrubbed ledger's silence would read as "+
			"conclusive, and a retry of anything minted before it would be "+
			"decided twice", before, lost, adopted)
	}
}

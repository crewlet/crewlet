package a2a_test

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/a2a/a2atest"
	"github.com/crewlet/crewlet/internal/store"
)

func TestMemoryStoreConformance(t *testing.T) {
	t.Parallel()
	a2atest.Run(t, func(*testing.T) a2a.Store { return a2a.NewMemoryStore() })
}

func TestSQLStoreConformance(t *testing.T) {
	t.Parallel()
	a2atest.Run(t, func(t *testing.T) a2a.Store {
		db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "a2a.db"), store.Options{})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return a2a.NewSQLStore(db)
	})
}

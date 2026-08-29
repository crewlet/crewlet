package a2a_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/a2a/a2atest"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

func TestCoordStoreConformance(t *testing.T) {
	t.Parallel()
	a2atest.Run(t, func(*testing.T) a2a.Store {
		return a2a.NewCoordStore(memory.NewFleet())
	})
}

package engine

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// homeOwners is every seat of c whose executor can launch in a container box:
// the seats whose cached homes a node keeps ([sandbox.Manager.CollectHomes]).
func homeOwners(c *config.Company) []string {
	var owners []string
	for box := range c.SeatBoxes() {
		if box.Cell == config.PlacementContainer && !slices.Contains(owners, box.Seat.IdentityKey()) {
			owners = append(owners, box.Seat.IdentityKey())
		}
	}
	return owners
}

// homeMeter records the cached homes' cuts into the node's recorder
// ([sandbox.HomeMeter]), as [spoolMeter] does the spools'. A nil recorder
// records nothing.
type homeMeter struct {
	metrics *metrics.Recorder
}

func (m homeMeter) HomeCut(reason string) {
	m.metrics.Add(metrics.ExecutorHomeCuts, 1, metrics.Attrs{"reason": reason})
}

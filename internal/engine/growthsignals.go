package engine

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/growth"
)

// GrowthSignals is what this node's growth products met at their vendors (a
// clock its vendor's disagrees with, a deprecated API version): a product
// reports to it, and the node's status record publishes it for every node's
// platform notices (internal/notice).
func (e *Engine) GrowthSignals() *growth.Signals {
	if s := e.growthSignals.Load(); s != nil {
		return s
	}
	e.growthSignals.CompareAndSwap(nil, growth.NewSignals(nil))
	return e.growthSignals.Load()
}

// growthSignalRows is this node's growth signals as its status record carries
// them, nil where its products met nothing.
func (e *Engine) growthSignalRows() *coord.GrowthSignals {
	skews, deprecated := e.GrowthSignals().Snapshot()
	if len(skews) == 0 && len(deprecated) == 0 {
		return nil
	}
	out := &coord.GrowthSignals{}
	for _, c := range skews {
		out.ClockSkews = append(out.ClockSkews, coord.GrowthClockSkew{Product: c.Product, Vendor: c.Vendor,
			OffsetSeconds: int(c.Offset.Round(time.Second) / time.Second), Since: c.Since})
	}
	for _, d := range deprecated {
		out.Deprecations = append(out.Deprecations, coord.GrowthDeprecation{Product: d.Product,
			Version: d.Version, Retires: d.Retires, Since: d.Since})
	}
	return out
}

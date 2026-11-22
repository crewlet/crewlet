package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
)

// A NODE'S STATUS RECORD CARRIES WHAT ITS GROWTH PRODUCTS MET, so every node's
// notices read it: a clock skew a product reports is on the record, to the
// second and signed node minus vendor, and gone from it once a later signed
// request is accepted; a node whose products met nothing carries none.
//
// Mutation: leave Growth out of statusRecordRows' rows, or round the offset's
// sign away, and the notice never opens or says the wrong way.
func TestANodesStatusRecordCarriesWhatItsGrowthProductsMet(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	rowsOf := func() *coord.GrowthSignals { return e.statusRecordRows(t.Context()).Growth }
	if rows := rowsOf(); rows != nil {
		t.Fatalf("a node whose products met nothing carries %+v", rows)
	}
	e.GrowthSignals().ClockRefused("appstoreconnect", "Apple", -(90*time.Second + 400*time.Millisecond))
	e.GrowthSignals().Deprecated("metaads", "v23.0", time.Time{})
	rows := rowsOf()
	if rows == nil || len(rows.ClockSkews) != 1 || rows.ClockSkews[0].OffsetSeconds != -90 ||
		rows.ClockSkews[0].Vendor != "Apple" || len(rows.Deprecations) != 1 || rows.Deprecations[0].Version != "v23.0" {
		t.Fatalf("the record carries %+v", rows)
	}
	e.GrowthSignals().ClockAgreed("appstoreconnect")
	if rows := rowsOf(); rows == nil || len(rows.ClockSkews) != 0 {
		t.Errorf("an accepted request left %+v on the record", rows)
	}
}

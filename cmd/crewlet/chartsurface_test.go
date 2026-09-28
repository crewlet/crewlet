package main

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE WITH NO CHART OPEN ANSWERS UNAVAILABLE ON EVERY CALL, and never
// dereferences the reader it does not have.
//
// The engine's accessors answer a nil *chart.Reader and a nil *chart.Writer on
// a node that started with no active company. Handed to the surface directly,
// the reader was an interface holding a typed nil — past the surface's own
// "required" check — and the first /chart read panicked inside the request;
// the writer panicked one call later, in `As`. Each call is asked here, and
// each must answer an error the surface renders as its ordinary 503.
func TestANodeWithNoChartAnswersUnavailableOnEveryCall(t *testing.T) {
	t.Parallel()
	c := liveChart{
		reader: func() *chart.Reader { return nil },
		writer: func() *chart.Writer { return nil },
	}
	ctx := context.Background()
	fresh := statelog.Freshness{Level: statelog.ReadStale}

	_, readErr := c.Read(ctx, fresh)
	_, unitErr := c.Unit(ctx, "platform", fresh)
	_, seatErr := c.Seat(ctx, "jane", fresh)
	_, _, historyErr := c.History(ctx, 10, fresh)
	_, _, importsErr := c.Imports(ctx, 10, fresh)
	_, _, _, importErr := c.Import(ctx, "rev", fresh)

	w := c.authority("jane.doe", chart.AuthorHuman, []iam.Grant{iam.GrantConfigWrite},
		chart.Provenance{})
	_, writeUnitErr := w.WriteUnit(ctx, "op", chart.UnitContent{})
	_, writeSeatErr := w.WriteSeat(ctx, "op", chart.SeatContent{})
	_, batchErr := w.WriteBatch(ctx, "op", chart.Batch{})
	_, removalErr := w.WriteRemoval(ctx, "op", chart.Batch{})
	_, importWriteErr := w.WriteImport(ctx, "op", "rev", nil)

	for name, err := range map[string]error{
		"Read": readErr, "Unit": unitErr, "Seat": seatErr, "History": historyErr,
		"Imports": importsErr, "Import": importErr, "WriteUnit": writeUnitErr,
		"WriteSeat": writeSeatErr, "WriteBatch": batchErr,
		"WriteRemoval": removalErr, "WriteImport": importWriteErr,
	} {
		if !errors.Is(err, statelog.ErrUnavailable) {
			t.Errorf("%s = %v, want statelog.ErrUnavailable — the 503 the "+
				"surface answers for a node with nothing to answer from", name, err)
		}
	}
}

// AND A CHART OPENED LATER IS SEEN, which is the control and the reason the
// lookup is per call: a writer handed out once the chart exists is the node's
// own, not the refusing stand-in.
func TestAChartOpenedLaterIsSeenWithoutRebuildingTheSurface(t *testing.T) {
	t.Parallel()
	var writer *chart.Writer
	c := liveChart{
		reader: func() *chart.Reader { return nil },
		writer: func() *chart.Writer { return writer },
	}
	if _, refusing := c.authority("jane.doe", chart.AuthorHuman, nil,
		chart.Provenance{}).(noChartWriter); !refusing {
		t.Fatal("with no chart open the surface was handed a real writer")
	}
	writer = &chart.Writer{}
	if _, refusing := c.authority("jane.doe", chart.AuthorHuman, nil,
		chart.Provenance{}).(noChartWriter); refusing {
		t.Error("a chart opened after the surface was built was never seen: " +
			"the surface still refuses every write")
	}
}

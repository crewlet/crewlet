package main

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// liveChart is the org chart's read and write sides as /chart reaches them:
// looked up on EVERY request rather than captured when the surface is built.
//
// # Why not simply hand over the engine's reader
//
// Because on a node that started with no active company the engine has no
// chart open, and its reader is a nil *chart.Reader. Handed to
// [chartapi.Options.Reader] that is an interface holding a typed nil: it
// passes the surface's own "a reader is required" check, and the first read
// dereferenced it and panicked inside the request. Its writer was the same
// hazard one call later, `As` on a nil *chart.Writer.
//
// A node in that posture has nothing to answer from, which is a fact about
// this node right now rather than a fault in the request, so every call is
// refused as UNAVAILABLE — the 503 the surface already answers for a node
// behind its log — naming why. And it is looked up per call rather than
// decided once, because the chart this node lacks is one it can open later
// without the surface being rebuilt.
type liveChart struct {
	reader func() *chart.Reader
	writer func() *chart.Writer
}

// errNoChart is every call on a node with no chart open.
var errNoChart = fmt.Errorf("%w: this node has no org chart open — it started "+
	"with no active company, so it runs no state-log domain yet; import one "+
	"(crewlet config import) and read the chart from a node that runs it",
	statelog.ErrUnavailable)

func (c liveChart) open() (*chart.Reader, error) {
	if r := c.reader(); r != nil {
		return r, nil
	}
	return nil, errNoChart
}

func (c liveChart) Read(ctx context.Context, fresh statelog.Freshness) (chart.Chart, error) {
	r, err := c.open()
	if err != nil {
		return chart.Chart{}, err
	}
	return r.Read(ctx, fresh)
}

func (c liveChart) Unit(ctx context.Context, key string, fresh statelog.Freshness) (
	chart.UnitDetail, error) {
	r, err := c.open()
	if err != nil {
		return chart.UnitDetail{}, err
	}
	return r.Unit(ctx, key, fresh)
}

func (c liveChart) Seat(ctx context.Context, handle string, fresh statelog.Freshness) (
	chart.SeatDetail, error) {
	r, err := c.open()
	if err != nil {
		return chart.SeatDetail{}, err
	}
	return r.Seat(ctx, handle, fresh)
}

func (c liveChart) History(ctx context.Context, limit int, fresh statelog.Freshness) (
	[]chart.Change, chart.Answer, error) {
	r, err := c.open()
	if err != nil {
		return nil, chart.Answer{}, err
	}
	return r.History(ctx, limit, fresh)
}

func (c liveChart) Imports(ctx context.Context, limit int, fresh statelog.Freshness) (
	[]chart.Import, chart.Answer, error) {
	r, err := c.open()
	if err != nil {
		return nil, chart.Answer{}, err
	}
	return r.Imports(ctx, limit, fresh)
}

func (c liveChart) Import(ctx context.Context, revision string, fresh statelog.Freshness) (
	chart.Import, bool, chart.Answer, error) {
	r, err := c.open()
	if err != nil {
		return chart.Import{}, false, chart.Answer{}, err
	}
	return r.Import(ctx, revision, fresh)
}

// authority is [chartapi.Authority] over the node's own writer: one party's
// writer where the chart is open, and one refusing every write where it is
// not.
func (c liveChart) authority(actor string, kind chart.AuthorKind, grants []iam.Grant,
	provenance chart.Provenance) chartapi.Writer {
	if w := c.writer(); w != nil {
		return w.As(actor, kind, grants, provenance)
	}
	return noChartWriter{}
}

// noChartWriter refuses every write as [errNoChart]: nothing is sealed,
// published or recorded on a node with no chart to write to.
type noChartWriter struct{}

func (noChartWriter) WriteUnit(context.Context, string, chart.UnitContent) (
	chart.WriteResult, error) {
	return chart.WriteResult{}, errNoChart
}

func (noChartWriter) WriteSeat(context.Context, string, chart.SeatContent) (
	chart.WriteResult, error) {
	return chart.WriteResult{}, errNoChart
}

func (noChartWriter) WriteBatch(context.Context, string, chart.Batch) (
	chart.WriteResult, error) {
	return chart.WriteResult{}, errNoChart
}

func (noChartWriter) WriteRemoval(context.Context, string, chart.Batch) (
	chart.WriteResult, error) {
	return chart.WriteResult{}, errNoChart
}

func (noChartWriter) WriteImport(context.Context, string, string, []chart.Edge) (
	chart.WriteResult, error) {
	return chart.WriteResult{}, errNoChart
}

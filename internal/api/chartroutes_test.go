package api_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// TestEveryRouteTheConfigRefusalNamesIsOneTheChartSurfaceServes.
//
// `PUT /config` refuses a body carrying `roles:` or `units:` and POINTS
// somewhere, and a hint naming a route this build does not serve is worse
// than a refusal that says only no: it sends an operator to a 404 with the
// engine's own words behind it, and they have no way to tell a typo in their
// request from a lie in the answer.
//
// Neither package can check this alone — they are siblings, importable from
// neither direction — so the walk lives HERE, where the app that mounts both
// of them lives.
func TestEveryRouteTheConfigRefusalNamesIsOneTheChartSurfaceServes(t *testing.T) {
	t.Parallel()
	mounted := chartPatterns(t)
	if len(mounted) == 0 {
		t.Fatal("the chart surface mounted nothing, so this walk certifies nothing")
	}
	named := configapi.ChartRoutePatterns()
	if len(named) == 0 {
		t.Fatal("the config refusals name no route at all")
	}
	for _, pattern := range named {
		if !slices.Contains(mounted, pattern) {
			t.Errorf("a /config refusal points at %q, which the chart surface "+
				"does not serve — it mounts %v", pattern, mounted)
		}
	}
}

// chartPatterns is every route the chart surface mounts.
func chartPatterns(t *testing.T) []string {
	t.Helper()
	seen := &patternMux{}
	svc, err := chartapi.New(chartapi.Options{
		Reader:    chartReaderStub{},
		Authority: func(string, chart.AuthorKind, []iam.Grant) chartapi.Writer { return nil },
		Principal: resolved(func() iam.Principal { return iam.Principal{} }),
		Chart:     authz.NoChart{},
	})
	if err != nil {
		t.Fatalf("chartapi.New: %v", err)
	}
	if err := svc.Routes(seen); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	return seen.patterns
}

// patternMux records what was mounted on it and serves nothing.
type patternMux struct{ patterns []string }

func (m *patternMux) Handle(pattern string, _ http.Handler) {
	m.patterns = append(m.patterns, pattern)
}

// chartReaderStub satisfies the read seam and is never called: this walk
// mounts routes and reads none of them.
type chartReaderStub struct{}

func (chartReaderStub) Read(context.Context, statelog.Freshness) (chart.Chart, error) {
	return chart.Chart{}, nil
}

func (chartReaderStub) Unit(context.Context, string, statelog.Freshness) (
	chart.UnitDetail, error) {
	return chart.UnitDetail{}, nil
}

func (chartReaderStub) Seat(context.Context, string, statelog.Freshness) (
	chart.SeatDetail, error) {
	return chart.SeatDetail{}, nil
}

func (chartReaderStub) History(context.Context, int, statelog.Freshness) (
	[]chart.Change, chart.Answer, error) {
	return nil, chart.Answer{}, nil
}

func (chartReaderStub) Imports(context.Context, int, statelog.Freshness) (
	[]chart.Import, chart.Answer, error) {
	return nil, chart.Answer{}, nil
}

func (chartReaderStub) Import(context.Context, string, statelog.Freshness) (
	chart.Import, bool, chart.Answer, error) {
	return chart.Import{}, false, chart.Answer{}, nil
}

// resolved adapts a principal source to the three-valued seam.
//
// EVERY CASE HERE IS ABOUT THE AUTHORITY TABLE rather than about the
// resolution, so they all say [iam.Resolved] and this says it once. The
// unknown arm has its own case, which is the only place a test should be
// spelling a resolution out.
func resolved(of func() iam.Principal) chartapi.Principal {
	return func(*http.Request) (iam.Principal, iam.Resolution) {
		return of(), iam.Resolved
	}
}

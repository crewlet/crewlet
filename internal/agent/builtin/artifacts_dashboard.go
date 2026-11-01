package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- list_dashboard_sources --------------------------------------------- //

// dashboardSources lists the engine source catalogue a dashboard's `engine`
// datasets may read. It holds no row of any domain: what a source answers is
// decided as each board's viewer when the board is read (adr/0043), so the
// agent that writes a board is told the reads it may name, never what they
// answer.
type dashboardSources struct{}

var _ tools.SeatCallable = (*dashboardSources)(nil)

func (t *dashboardSources) Name() string { return DashboardSourcesTool }

func (t *dashboardSources) Description() string {
	return "List the engine sources a dashboard's `engine` datasets may read: each " +
		"source's name, what it reads, who may read it, its typed parameters, its " +
		"columns and how it is kept current. Every viewer of a dashboard sees what " +
		"they themselves may read from a source, so a board you publish shows each " +
		"person their own rows. Pass `name` for one source."
}

func (t *dashboardSources) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type": "string", "enum": stringsToAny(dashspec.SourceNames()),
				"description": "One source. Omit for the whole catalogue.",
			},
		},
	}
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func (t *dashboardSources) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *dashboardSources) CallForTurn(_ context.Context, _ *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	sources := dashspec.Catalogue()
	if name := strings.TrimSpace(argString(args, "name")); name != "" {
		src, ok := dashspec.Source(name)
		if !ok {
			return failed(fmt.Sprintf(DashboardSourcesTool+" has no source %q: the catalogue is %s.",
				name, strings.Join(dashspec.SourceNames(), ", "))), nil
		}
		sources = []dashspec.EngineSource{src}
	}
	return jsonResult(map[string]any{
		"sources":             sources,
		"min_refresh_seconds": artifacts.MinRefreshSeconds,
		"note": "A dataset reads one source: {\"id\": ..., \"title\": ..., \"source\": \"engine\", " +
			"\"engine\": {\"name\": ..., \"params\": {...}}}, a parameter a literal or " +
			"{\"param\": \"<filter key>\"}. Every key of dashboard.json is one " + ArtifactFormatTool + " call away.",
	})
}

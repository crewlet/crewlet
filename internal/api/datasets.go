package api

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/api/datasetsapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// DatasetsSource is what the datasets surface reads a board and its store
// datasets from: the artifacts surface (its admission and its board cache)
// and the datastore. Engine datasets are this app's own questions.
type DatasetsSource struct {
	Boards datasetsapi.Boards
	Data   datasetsapi.Data
}

// mountDatasets builds and mounts the datasets surface over this app's
// question registry and its agents overview, decided on the request's own
// context: the viewer's.
func (a *App) mountDatasets(mux authz.Mux, src *DatasetsSource, chart authz.Chart) error {
	surface, err := datasetsapi.New(datasetsapi.Options{
		Boards: src.Boards, Chart: chart, Questions: a.queries, Data: src.Data,
		Agents: a.agentsOverview,
	})
	if err != nil {
		return err
	}
	return surface.Routes(mux)
}

// agentsOverview is `GET /agents` for the principal ctx carries: decided by
// the socket's own audience table and shaped as its push is, through the one
// [App.overview] the route answers by.
func (a *App) agentsOverview(ctx context.Context) (any, error) {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return nil, fmt.Errorf("%w: the agents overview is read as a resolved caller", queries.ErrUnauthenticated)
	}
	return a.overview(ctx, principal, stream.KindAgents, func(stream.Audience) any { return a.stream.Roster() })
}

// overview is a value the stream service builds, for principal, if their
// audience receives a push of kind, in the shape that push hands them; else
// the refusal, carrying the rule's reason and the grants that would admit
// them. The one decision `GET /agents`, `/org`, `/tools`, the snapshot and a
// dashboard's agents source all take.
func (a *App) overview(ctx context.Context, principal iam.Principal, kind stream.Kind,
	build func(stream.Audience) any) (any, error) {

	if !principal.Stage.MayAct() {
		// THE AUTHORITY TABLE'S FIRST CHECK, as every question's: a person
		// whose stage may not act reads nothing, the overview included.
		return nil, queries.NewRefusal(string(kind), authz.Decision{Reason: authz.ReasonStage})
	}
	audience := a.stream.AudienceFor(ctx, principal)
	if !audience.Receives(kind) {
		need, _ := stream.NeedOf(kind)
		return nil, queries.NewRefusal(string(kind),
			authz.Decision{Reason: authz.ReasonNoGrant, Grants: need.Grants()})
	}
	return stream.ShapeFor(build(audience), audience), nil
}

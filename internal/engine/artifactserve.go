package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/config"
)

// THE ARTIFACT ORIGIN'S ENGINE WIRING: the api.artifacts listener's handler over
// this engine's estate router, object store, chart and recorder, told every
// artifacts movement this node's applier commits.
//
// EVERY INGRESS NODE THAT SETS THE LISTENER SERVES IT, and ingress requires
// `data`, so each hears every movement from its own applier and nothing is
// forwarded; a node whose copy is out of service or lagging decides through a
// data node whose copy answers, the router's own order.

// ArtifactServing is the ticket codec the API mints a viewer's frame through
// and the origin it is served under, from a node's Tier A. ONE CONSTRUCTION for
// both, so a frame and its document read the keyring and external URL once.
func ArtifactServing(boot *config.Bootstrap) (*ticket.Codec, artifactserve.Origins, error) {
	if boot == nil {
		return nil, artifactserve.Origins{}, errors.New("engine: the artifact origin needs Tier A")
	}
	tickets, err := ticket.New(boot.Secrets.TokenMaterial(), nil)
	if err != nil {
		return nil, artifactserve.Origins{}, fmt.Errorf("engine: the view ticket codec: %w", err)
	}
	origins, err := artifactserve.NewOrigins(artifactserve.OriginsConfigOf(boot))
	if err != nil {
		return nil, artifactserve.Origins{}, fmt.Errorf("engine: %w", err)
	}
	return tickets, origins, nil
}

// ArtifactOrigin builds the api.artifacts listener's handler over this engine,
// and answers the function that stops it hearing this node's artifacts
// movements. creds re-composes a ticket's principal (internal/api/auth, over
// the API's own guard), and embedded is the binary's static tree, whose
// `artifacts/` directory holds the runtime and the kit.
func ArtifactOrigin(e *Engine, boot *config.Bootstrap, creds artifactserve.Credentials,
	embedded fs.FS) (*artifactserve.Handler, func(), error) {

	tickets, origins, err := ArtifactServing(boot)
	if err != nil {
		return nil, nil, err
	}
	reads, ok := ArtifactsEstate(e)
	if !ok {
		return nil, nil, errors.New("engine: the artifact origin needs the estate router, " +
			"and this engine was built without one")
	}
	objects := e.Objects()
	if objects == nil {
		return nil, nil, errors.New("engine: this node runs no object store, so it cannot " +
			"serve an artifact's bytes; set api.artifacts.port only where the object " +
			"store is configured")
	}
	tree, err := fs.Sub(embedded, "artifacts")
	if err != nil {
		return nil, nil, fmt.Errorf("engine: the embedded artifact tree: %w", err)
	}
	deps := artifactserve.Deps{
		Tickets:     tickets,
		Origins:     origins,
		Artifacts:   reads,
		Objects:     objects,
		Credentials: creds,
		Chart:       ChartAuthorityOf(e),
		Local:       e,
		Embedded:    tree,
		Objectives:  e.objectives,
	}
	// A NIL RECORDER IS HANDED ON AS NONE: a nil pointer inside the interface
	// would be a recorder that panics.
	if rec := e.Recorder(); rec != nil {
		deps.Metrics = rec
	}
	h, err := artifactserve.New(deps)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: %w", err)
	}
	return h, e.OnArtifactsMoved(h.ArtifactsMoved), nil
}

// ArtifactsCopyServes reports whether this node answers artifacts reads from
// its own copy right now, WITHOUT WAITING: it holds the estate, and the last
// verdict found the artifacts copy sound and not lagging, by the artifacts
// domain's own verdict (ADR-0079), since another domain's wrong copy is no
// reason to send an artifacts read away. False means the router decides
// through another data node.
//
// FROM THE ROUTER'S OWN VERDICT, never a second judgement or a broker round
// trip, since the origin asks before every authorization; a copy nobody has
// judged yet does not serve.
func (e *Engine) ArtifactsCopyServes(ctx context.Context) bool {
	if e.local == nil {
		return false
	}
	c := e.core.Load()
	if c == nil || c.log == nil {
		return false
	}
	v, known, _ := e.local.peek(ctx, c)
	artifacts := artifactsdomain.Domain{}.Name()
	return known && v.serves(artifacts) && v.answers(artifacts)
}

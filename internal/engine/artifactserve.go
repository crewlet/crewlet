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
// EVERY INGRESS NODE THAT SETS THE LISTENER SERVES IT, and every ingress node
// holds the estate (ingress requires `data`), so each hears every movement from
// its own applier and nothing is forwarded between nodes; a node whose copy is
// out of service or lagging decides through a data node whose copy answers,
// which is the router's own order.

// ArtifactServing is the ticket codec and the origin a node's Tier A gives:
// what the API mints a viewer's frame through, and what the origin serves it
// under. ONE CONSTRUCTION for both halves, so a frame and the document it
// loads are built from one reading of the keyring and of the external URL.
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
	// A NIL RECORDER RECORDS NOTHING, and is handed on as no recorder at all:
	// a nil pointer inside the interface would be a recorder that panics.
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
// its own copy right now, as far as it can tell WITHOUT WAITING: it holds the
// estate, the last verdict read of its artifacts copy found it sound, and that
// copy answers rather than lags its log — the artifacts domain's own verdict
// (ADR-0072), since a wrong copy of another domain is no reason to send an
// artifacts read away. False is a decision the estate router
// takes through another data node.
//
// FROM THE VERDICT THE ROUTER ITSELF ANSWERS BY, never a second judgement and
// never a broker round trip: a request on the artifact origin asks it before
// every authorization, and a copy nobody has judged yet is not one it may
// claim.
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

package main

import (
	"context"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE ARTIFACTS SURFACE'S WIRING: its routes and its read questions over the
// engine's estate router (so a node whose own copy is out of service answers
// through a peer's, and a node without `data` through a data node), the chart
// the rest of the API decides by, the company's own artifacts settings read per
// decision, the Tier A plan quotas this node stamps, the upload pipeline the
// engine builds on first use, and the frames minted for the artifact origin
// where the deployment names one.

// artifactsSurfaces builds the artifacts surface and its questions' source, or
// neither on an engine built with no estate router (which serves no estate to
// route).
//
// ONE GATE FOR BOTH, so a route and a question decide who may read a private
// artifact by one admission, and record the admin path's override by one rule.
func artifactsSurfaces(e *engine.Engine, boot *config.Bootstrap) (
	*artifactsapi.Service, *queries.ArtifactsSource, error) {

	arts, routed := engine.ArtifactsEstate(e)
	if !routed {
		return nil, nil, nil
	}
	gate := artifactsGate(e, boot, arts)
	opts := artifactsapi.Options{
		Gate: gate, Reads: arts, Writes: arts, Objects: engineObjects{e: e},
		// THE PIPELINE IS ASKED FOR PER REQUEST: the engine builds it on
		// first use, and a node that cannot (no object store, no
		// coordination store) answers the upload routes with why.
		Uploads: func() (artifactsapi.Pipeline, error) {
			pipeline, err := e.ArtifactUploads()
			if err != nil {
				return nil, err
			}
			return pipeline, nil
		},
	}
	// THE FRAMES, only where the deployment names an artifact origin: the
	// ticket codec and the origin are the ones the origin's own handler is
	// built from (engine.ArtifactServing), so a frame minted here and the
	// document it loads there are one reading of the keyring and the URL.
	if boot.API.Artifacts.ExternalBase() != "" {
		tickets, origins, err := engine.ArtifactServing(boot)
		if err != nil {
			return nil, nil, fmt.Errorf("api: the artifact frames: %w", err)
		}
		frames, err := artifactserve.NewFrames(tickets, origins, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("api: the artifact frames: %w", err)
		}
		opts.Frames = frames
	}
	surface, err := artifactsapi.New(opts)
	if err != nil {
		return nil, nil, err
	}
	return surface, &queries.ArtifactsSource{Gate: gate, Reads: arts}, nil
}

// artifactsGate is THE admission to one artifact on this node, for every
// surface that touches one (the artifacts' routes and questions and the
// datastore's alike): the artifact's head and its reader records through the
// estate router, the chart the rest of the API decides by, the company's own
// artifacts settings read per decision, the company's roster, and the Tier A
// plan quotas this node stamps.
func artifactsGate(e *engine.Engine, boot *config.Bootstrap, arts estate.Artifacts) artifactsapi.Gate {
	return artifactsapi.Gate{
		Heads: arts, Chart: engine.ChartAuthorityOf(e), Marks: arts,
		Roster: companyRoster{e: e}, Limits: boot.Artifacts.Stamp,
		Company: func() config.CompanyArtifacts {
			if company, _ := companyConfig(e); company != nil {
				return company.Artifacts
			}
			return config.CompanyArtifacts{}
		},
	}
}

// engineObjects is the engine's object store as the artifacts surface reads a
// version's bytes through it, verified: a node running none answers every read
// with the engine's own refusal.
type engineObjects struct{ e *engine.Engine }

func (o engineObjects) Open(ctx context.Context, obj objstore.Object) (io.ReadCloser, error) {
	return o.e.OpenObject(ctx, obj)
}

// companyRoster is the running company's units and seats, as the chart holds
// them on this node: what a listing's relations are enumerated over.
type companyRoster struct{ e *engine.Engine }

// Units is every unit key of the running company.
func (c companyRoster) Units(context.Context) ([]string, error) {
	_, o := companyConfig(c.e)
	if o == nil {
		return nil, artifactsapi.ErrNoRoster
	}
	var keys []string
	for u := range o.AllUnits() {
		keys = append(keys, u.Key())
	}
	return keys, nil
}

// Seats is every seat of the running company, each marked as an agent's or a
// person's.
func (c companyRoster) Seats(context.Context) ([]artifactsapi.RosterSeat, error) {
	_, o := companyConfig(c.e)
	if o == nil {
		return nil, artifactsapi.ErrNoRoster
	}
	var seats []artifactsapi.RosterSeat
	for r := range o.AllRoles() {
		seats = append(seats, artifactsapi.RosterSeat{Handle: r.Handle(), Agent: r.IsAgent()})
	}
	return seats, nil
}

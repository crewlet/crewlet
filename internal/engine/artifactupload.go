package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"sync"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/artifactupload/vendor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/static"
)

// THE ARTIFACT UPLOAD PIPELINE'S ENGINE WIRING: one per node, over the object
// store every node holds, the fleet's coordination store for the reservations,
// and the artifacts estate through the router, so a node without `data` takes
// uploads and publishes exactly as a data node does.
//
// BUILT ON FIRST USE, not at boot: it needs the estate router and the object
// store, both of which [New] builds, and nothing about a node's boot should
// wait on, or fail for, a surface no caller has asked for yet.

// artifactUploads is the node's pipeline, built once.
type artifactUploads struct {
	once    sync.Once
	service *artifactupload.Service
	err     error
}

// ErrNoUploads is a node that cannot take artifact uploads: it runs no object
// store, no coordination store or no estate router.
var ErrNoUploads = errors.New("engine: this node takes no artifact uploads")

// The embedded trees the pipeline reads its releases from, one directory per
// version: the page runtime's contracts (the tree the artifact origin serves
// `/_crewlet/runtime/` from) and the artifact kit's releases (served under
// [artifacts.KitPrefix]).
const (
	runtimeTree = "artifacts/runtime"
	kitTree     = "artifacts/kit"
)

// ArtifactUploads is this node's upload and publish pipeline: every surface
// that publishes an artifact (the API, the harness through it, the agent tools)
// calls this one.
func (e *Engine) ArtifactUploads() (*artifactupload.Service, error) {
	u := &e.uploads
	u.once.Do(func() { u.service, u.err = e.buildArtifactUploads() })
	return u.service, u.err
}

// buildArtifactUploads wires the pipeline from what the node runs.
func (e *Engine) buildArtifactUploads() (*artifactupload.Service, error) {
	switch {
	case e.objects == nil:
		return nil, fmt.Errorf("%w: it runs no object store", ErrNoUploads)
	case e.backends == nil || e.backends.Fleet == nil:
		return nil, fmt.Errorf("%w: it runs no coordination store to reserve bytes in", ErrNoUploads)
	case e.router == nil:
		return nil, fmt.Errorf("%w: it reaches no artifacts estate", ErrNoUploads)
	}
	runtimes, err := embeddedReleases(runtimeTree)
	if err != nil {
		return nil, err
	}
	kits, err := embeddedKits()
	if err != nil {
		return nil, err
	}
	hosts := lint.DefaultAllowlist()
	cache, err := vendor.New(vendor.Options{Hosts: hosts.Names()})
	if err != nil {
		return nil, fmt.Errorf("engine: the CDN copy cache: %w", err)
	}
	store := e.objects.store
	estate := e.router.Artifacts()
	deps := artifactupload.Deps{
		Objects: store, Reservations: e.backends.Fleet, Writes: estate, Reads: estate,
		Limits: e.artifactLimits, Vendor: cache, Hosts: hosts, Runtimes: runtimes, Kits: kits,
	}
	// A DIRECT PUT ONLY WHERE THE BACKEND PROVED IT: the s3 backend's boot
	// probe decided it once, and the broker's own bucket never offers one.
	if direct, _ := store.DirectUpload(); direct {
		deps.Presign = store
	}
	if e.metrics != nil {
		deps.Meter = e.metrics
	}
	deps.Objectives = e.objectives
	service, err := artifactupload.New(deps)
	if err != nil {
		return nil, fmt.Errorf("engine: the artifact upload pipeline: %w", err)
	}
	return service, nil
}

// artifactLimits is the Tier A quotas this node stamps into every artifact
// write and holds every reservation to: the configured ones over their
// defaults, the defaults alone on an engine built with no Tier A.
func (e *Engine) artifactLimits() artifacts.Limits {
	if e.boot == nil {
		return config.Artifacts{}.Stamp()
	}
	return e.boot.Artifacts.Stamp()
}

// embeddedKits is the artifact kit releases this build embeds: the tree is
// compiled into the binary, so it is read once and answers the same for the
// life of the process.
var embeddedKits = sync.OnceValues(func() (artifactupload.ReleaseSet, error) {
	return embeddedReleases(kitTree)
})

// kitVersion is the newest artifact kit release this build embeds, the one
// version a page linked now is sure to be served at: what a seat's skill names
// as `${crewlet_kit_version}` and what get_company_brand answers. Empty for a
// build whose kit tree cannot be read, which the pipeline refuses to be built
// over anyway; that reason is logged here once per call.
func kitVersion() string {
	kits, err := embeddedKits()
	if err != nil {
		log.Warn("artifact_kit_tree_unreadable", "error", err.Error(),
			"detail", "skills render ${crewlet_kit_version} literally and the brand tool "+
				"answers no kit version")
		return ""
	}
	return kits.Latest()
}

// embeddedReleases is the releases one embedded tree holds.
func embeddedReleases(dir string) (artifactupload.ReleaseSet, error) {
	tree, err := fs.Sub(static.FS(), dir)
	if err != nil {
		return artifactupload.ReleaseSet{}, fmt.Errorf("engine: the embedded tree %s: %w", dir, err)
	}
	set, err := artifactupload.ReleasesIn(tree)
	if err != nil {
		return artifactupload.ReleaseSet{}, fmt.Errorf("engine: the embedded tree %s: %w", dir, err)
	}
	return set, nil
}

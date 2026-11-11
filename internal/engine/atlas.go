package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// AtlasToolDeps is what the atlas tools read the atlas through on this node:
// the settings it runs, read per call so an apply is read from the next call,
// with the revision they were activated as and the addresses the deployment
// answers at. Gitlet is read through the code tools' own half, as the caller,
// so it is not repeated here (ADR-0127).
//
// EXPORTED for the operator surface, which builds its catalogue for every
// call.
func AtlasToolDeps(e *Engine) builtin.AtlasDeps {
	if e == nil {
		return builtin.AtlasDeps{}
	}
	return builtin.AtlasDeps{Settings: e.atlasSettings}
}

// atlasSettings is the settings this node runs as the atlas reads them, and
// false where it runs none yet.
func (e *Engine) atlasSettings(ctx context.Context) (builtin.AtlasSettings, bool) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return builtin.AtlasSettings{}, false
	}
	public := ""
	if e.webhookBase != e.externalBase {
		public = e.webhookBase
	}
	return builtin.AtlasSettings{
		Company:    c.Config,
		Revision:   e.runningRevision(ctx, c),
		Deployment: derive.Deployment{APIAddress: e.externalBase, PublicAddress: public},
	}, true
}

// runningRevision is the revision the epoch c was applied from, or "" where
// this node cannot tell. The epoch keeps its activation's instant and not its
// revision, so the fleet's pointer names it only while it names that same
// activation; in the moments between an activation and this node's apply of
// it the pointer names the next one, and a fact read from these settings
// labelled with it would be attributed to a document this node has not read.
func (e *Engine) runningRevision(ctx context.Context, c *Company) string {
	if e.backends == nil || e.backends.Fleet == nil {
		return ""
	}
	target, found, err := e.backends.Fleet.Target(ctx)
	if err != nil {
		log.WarnContext(ctx, "atlas_revision_unread", "error", err,
			"detail", "the fleet's activation could not be read, so this atlas read names no revision "+
				"for the settings it read")
		return ""
	}
	return revisionRunning(target, found, c)
}

// revisionRunning is target's revision where target is the activation the
// epoch c was applied from, compared as the engine compares activation
// instants ([store.EncodeTime]), and "" otherwise or for an epoch no
// activation named.
func revisionRunning(target coord.Activation, found bool, c *Company) string {
	if !found || c == nil || c.ActivatedAt.IsZero() ||
		store.EncodeTime(target.At) != store.EncodeTime(c.ActivatedAt) {
		return ""
	}
	return target.RevisionID
}

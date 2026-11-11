package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// AtlasToolDeps is what the atlas tools read on this node: the settings it
// runs, read per call, their revision and the deployment's addresses. Gitlet
// comes through the code tools' own half, as the caller (ADR-0127).
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
// this node cannot tell: the fleet's pointer names it only while it names the
// activation c was applied from, never one this node has not applied yet.
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

// revisionRunning is target's revision where target is the activation c was
// applied from, compared as the engine compares activation instants
// ([store.EncodeTime]), and "" otherwise.
func revisionRunning(target coord.Activation, found bool, c *Company) string {
	if !found || c == nil || c.ActivatedAt.IsZero() ||
		store.EncodeTime(target.At) != store.EncodeTime(c.ActivatedAt) {
		return ""
	}
	return target.RevisionID
}

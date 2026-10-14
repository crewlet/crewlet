package engine

import (
	"context"
	"log/slog"

	"github.com/crewlet/crewlet/internal/config"
)

// warnTiers logs every cross-tier warning ([config.NodeTierWarnings]) the
// company cfg raises beside this node's Tier A and the code cache budget this
// node resolved, one config_tier_warning line each, carrying which company it
// is about (attrs: the revision and the epoch it was applied at).
//
// PER NODE, because one of the two documents is this node's own: the same
// company is a warning on a git node and none on a node that serves no git —
// and the cache budget compared is the one THIS node derived from its volume,
// which neither document holds.
//
// ONCE PER APPLIED EPOCH, and ONLY at an apply — on its success branch, for
// [Reconciler.warnAdmission]'s reason (a refused apply is retried, and would
// repeat the line for a revision this node is not serving). Not at boot as
// well: the reconciler's first tick applies the very activation the node
// booted with, so a boot line said every warning twice. A node that runs no
// reconciler applies nothing and warns about nothing, like every other apply
// warning.
//
// A WARNING, NEVER A REFUSAL: each describes a configuration that runs, and is
// worth knowing before production says it — that it was also printed by
// `crewlet validate` given both files, and carried in the answer of the
// configuration write that applied it, does not reach an operator whose node
// was handed the revision by a peer.
func warnTiers(ctx context.Context, logger *slog.Logger, boot *config.Bootstrap, cfg *config.Company,
	cache config.CacheBudget, attrs ...any) {
	for _, w := range config.NodeTierWarnings(boot, cfg, cache) {
		logger.WarnContext(ctx, "config_tier_warning",
			append([]any{"path", w.Path, "detail", w.Message}, attrs...)...)
	}
}

// codeCacheBudget is the code cache budget this node resolved when it built its
// code host, zero where it runs none — read at each apply, after the runtime
// it may have just built is published.
func (e *Engine) codeCacheBudget() config.CacheBudget {
	if n := e.native.Load(); n != nil && n.codeHost != nil {
		return n.codeCache.cacheBudget()
	}
	return config.CacheBudget{}
}

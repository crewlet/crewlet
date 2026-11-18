package engine

import (
	"context"
	"log/slog"

	"github.com/crewlet/crewlet/internal/config"
)

// warnTiers logs each cross-tier warning ([config.NodeTierWarnings]) cfg raises
// beside this node's Tier A and resolved code cache budget, one
// config_tier_warning line each, attrs naming the revision and its epoch.
//
// Per node, because this node's own Tier A and derived cache budget are half of
// each warning. Once per applied epoch, only on an apply's success branch
// ([Reconciler.warnAdmission]'s reason: a refused apply is retried), and not
// at boot, whose activation the reconciler's first tick applies anyway; a node
// with no reconciler warns about nothing. A warning, never a refusal: the
// configuration runs. Logged because `crewlet validate` and the write's answer
// never reach the operator of a node handed the revision by a peer.
func warnTiers(ctx context.Context, logger *slog.Logger, boot *config.Bootstrap, cfg *config.Company,
	cache config.CacheBudget, attrs ...any) {
	for _, w := range config.NodeTierWarnings(boot, cfg, cache) {
		logger.WarnContext(ctx, "config_tier_warning",
			append([]any{"path", w.Path, "detail", w.Message}, attrs...)...)
	}
}

// codeCacheBudget is the code cache budget this node resolved when it built
// Gitlet, zero where it did not; read at each apply, after the runtime it may
// have just built is published.
func (e *Engine) codeCacheBudget() config.CacheBudget {
	if n := e.native.Load(); n != nil && n.gitletHost != nil {
		return n.gitCacheSize.cacheBudget()
	}
	return config.CacheBudget{}
}

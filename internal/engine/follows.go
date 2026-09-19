package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/notify/followsync"
)

// migrateFollows carries this node's own thread-follow rows onto the fleet,
// once.
//
// # Where it sits, and why exactly there
//
// Beside [Engine.migrateSecrets] and for the same two reasons: it is the only
// moment both stores are reachable, and it is BEFORE anything routes. The chat
// transports are built during the epoch install below this call, so no inbound
// message is matched against the follows bucket until this has finished —
// which is what makes the handoff a move rather than a race against live
// traffic.
//
// # Why there is a handoff at all
//
// Migration 0028 moved the follows to coordination and deliberately does NOT
// drop the table, because a migration runs before any Go code on every boot: a
// drop there — or in any later numbered file, since a database can apply both
// in one pass — would leave this with an empty table and silently destroy
// every ACTIVE thread subscription. See [followsync] for why that is not the
// ninety-day horizon arriving early.
//
// # A FAILURE DOES NOT FAIL THE BOOT
//
// [followsync.Migrate] removes nothing it did not manage to carry, so a broken
// pass leaves the rows where they are and the next start retries. The node is
// no worse off than it was, and the data is still there. Failing the boot
// instead would take a working company down over a store blip; carrying on
// silently would be worse still, which is what the error log is for.
func (e *Engine) migrateFollows(ctx context.Context) {
	if e.backends == nil || e.backends.Store == nil || e.backends.Fleet == nil {
		return
	}
	created, err := followsync.Migrate(ctx,
		e.backends.Store.ThreadFollows(), e.backends.Fleet)
	if err != nil {
		log.ErrorContext(ctx, "follow_migration_incomplete", "error", err,
			"created", created,
			"detail", "the follows that did not move are still in this node's own "+
				"database, where no peer can see them — a thread reply that is not "+
				"a mention will not reach its seat when another node claims it. "+
				"Retried at the next start")
	}
}

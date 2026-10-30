package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The migration onto per-seat access, wired to this process: the config
// surface it writes the document through and the fleet's secret store it
// re-seals credentials in. What it does is internal/accessmigrate's, and the
// vendors and the ledger it reads are the engine's
// ([engine.Engine.MigrateSeatAccess]).

// migrationStartup bounds the first attempt, made before the node starts its
// seats: an integration whose vendor does not answer in it stays gated and is
// tried again by [migrationRetry].
const migrationStartup = 45 * time.Second

// migrationRetry is how often the migration looks again: an integration left
// gated because its vendor did not answer, a write that lost to another
// node's, or an integration a person connected since.
const migrationRetry = time.Minute

// migrateSeatAccessOnce makes the first attempt, before the node starts its
// seats. Its outcome is logged rather than refused: the integrations it could
// not migrate stay gated, and the node runs every other part of the company.
func migrateSeatAccessOnce(ctx context.Context, e *engine.Engine, cfg *configapi.Service,
	store *fleetsecrets.Store, log *slog.Logger) {

	startup, cancel := context.WithTimeout(ctx, migrationStartup)
	defer cancel()
	migrateSeatAccessStep(startup, e, cfg, store, log)
}

// keepMigratingSeatAccess tries again every [migrationRetry] for as long as the
// node runs. Every node holding the estate runs it; the revision's
// compare-and-set lets one write and the rest find nothing left to do.
func keepMigratingSeatAccess(ctx context.Context, e *engine.Engine, cfg *configapi.Service,
	store *fleetsecrets.Store, log *slog.Logger) {

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(migrationRetry):
		}
		migrateSeatAccessStep(ctx, e, cfg, store, log)
	}
}

// migrateSeatAccessStep is one attempt.
func migrateSeatAccessStep(ctx context.Context, e *engine.Engine, cfg *configapi.Service,
	store *fleetsecrets.Store, log *slog.Logger) {

	result, err := e.MigrateSeatAccess(ctx, migrationDocument{svc: cfg},
		migrationSecrets{engine: e, store: store}, cfg.Managed())
	var raced *configapi.RacedError
	switch {
	case err == nil:
		if result.Revision != "" {
			log.InfoContext(ctx, "seat_access_migration_written", "revision", result.Revision,
				"moved", result.Moved, "notices", result.Notices, "seeded", result.Seeded)
		}
	case errors.Is(err, configapi.ErrNoActiveRevision):
		// NOTHING TO MIGRATE YET: a company imported later is migrated by
		// the next attempt.
	case errors.As(err, &raced):
		log.InfoContext(ctx, "seat_access_migration_raced", "detail",
			"the document moved while it was being migrated; the next attempt reads it again")
	default:
		log.WarnContext(ctx, "seat_access_migration_failed", "error", err.Error(),
			"retry_in", migrationRetry.String())
	}
}

// migrationDocument is the config surface as [accessmigrate.Document].
type migrationDocument struct{ svc *configapi.Service }

func (d migrationDocument) Stored(ctx context.Context) (map[string]any, string, error) {
	return d.svc.StoredDocument(ctx)
}

func (d migrationDocument) Rewrite(ctx context.Context, expect, summary string, by iam.Actor,
	transform func(map[string]any) (bool, error)) (string, bool, error) {

	applied, err := d.svc.Migrate(ctx, configapi.MigrateRequest{Transform: transform,
		Summary: summary, By: by, Expect: expect})
	if errors.Is(err, configapi.ErrNothingToMigrate) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return applied.RevisionID, true, nil
}

// migrationSecrets reads a value through the engine's chain and seals it in the
// fleet's store, recorded as a provisioning write by the migration.
type migrationSecrets struct {
	engine *engine.Engine
	store  *fleetsecrets.Store
}

func (s migrationSecrets) Value(name string) (string, bool) { return s.engine.LookupSecret(name) }

func (s migrationSecrets) Seal(ctx context.Context, name, value string, by secrets.Author,
	now time.Time) error {
	return s.store.Set(ctx, name, value, by, secrets.SourceProvision, now)
}

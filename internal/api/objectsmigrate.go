package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Moving the company's files to an S3 bucket over HTTP.
//
// A route rather than a command, for the backup's reason: the objects and the
// estate that names them are reachable only from inside the running engine,
// and on the default topology so is the coordination record the move ends
// with. `crewlet objects migrate --to s3` is a client of this route, and the
// procedure (a copy pass on a live fleet, then a final pass in maintenance
// mode) is the engine's: see internal/engine's objectsmigrate.go.

// objectsMigrator is the slice of the engine this route needs, declared here
// by its consumer.
type objectsMigrator interface {
	MigrateObjects(ctx context.Context, req engine.ObjectMigration) (engine.ObjectMigrationReport, error)
}

// serveMigrateObjects answers POST /objects/migrate.
//
// `?target=` is a Tier A file ON THE ENGINE'S HOST whose store.objects names
// the bucket, and `?final=true` runs the final pass. The target is a path and
// never a body: its ${VAR} references resolve from the engine's environment,
// so the bucket's credential never crosses this API.
//
// SYNCHRONOUS, for the backup's reason, and run under a context the client's
// cannot end: a pass the client gave up on still finishes, and one cut off by
// a drain resumes from what the bucket holds when it is run again.
func (a *App) serveMigrateObjects(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if target == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeMigrationRefused, map[string]string{
			"detail": "name the target with ?target=: an absolute path to a Tier A file whose " +
				"store.objects block names the bucket",
			"hint": "the path is on the engine's host, not the caller's",
		})
		return
	}
	final := r.URL.Query().Get("final") == "true"
	if _, ok := auth.Caller(w, r); !ok {
		return
	}
	by := auth.AttributionOf(r.Context())
	report, err := a.objects.MigrateObjects(context.WithoutCancel(r.Context()),
		engine.ObjectMigration{Target: target, Final: final})
	switch {
	case errors.Is(err, engine.ErrMigrationRefused):
		log.Warn("api_objects_migration_refused", "by", by.Name, "operator", by.OperatorID,
			"target", target, "final", final, "error", err)
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeMigrationRefused, httpjson.Detail{
			"detail": err.Error(), "report": report,
		})
		return
	case err != nil:
		// THE STORES' OWN WORDS GO TO THE LOG, for the backup's reason:
		// they name objects and the files that hold them. The body says
		// what was copied before the pass stopped.
		log.Warn("api_objects_migration_failed", "by", by.Name, "operator", by.OperatorID,
			"target", target, "final", final, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeMigrationFailed,
			httpjson.RetrySeconds(statelog.ElectionRetryHint), httpjson.Detail{
				"report": report,
				"hint": "a store did not answer; this node's log names which. Run the " +
					"migration again: it copies only what the bucket does not hold",
			})
		return
	}
	log.Info("objects_migration", "by", by.Name, "operator", by.OperatorID, "target", target,
		"final", final, "from", report.From, "to", report.To, "moved", report.Moved,
		"copied", report.Pass.Copied, "present", report.Pass.Present)
	writeJSON(w, http.StatusOK, report)
}

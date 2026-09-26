package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/backup"
)

// Taking a backup over HTTP.
//
// For the same reason the budget reset is a route rather than a command: what
// has to be copied is only reachable from inside the running engine. The
// store is one file this process holds an exclusive lock on, and the
// coordination estate is a broker embedded in this process that binds no
// socket — so a CLI run while the engine is down cannot read the second, and
// one run while the engine is UP cannot safely open the first.
//
// `crewlet backup` is a client of this route.

// backupTaker is the slice of the backup subsystem this route needs.
//
// Declared here, by the consumer. One method, because a route that could also
// READ a backup back would be a route that can serve the company's sealed
// credentials over HTTP.
type backupTaker interface {
	Take(ctx context.Context, dir string) (backup.Manifest, error)
}

// serveBackup answers POST /backup.
//
// `?dir=` is the destination ON THE ENGINE'S HOST — this writes files where
// the engine runs, not where the caller does, which is why the parameter is
// required rather than defaulted: a default would put a company's entire
// durable state somewhere nobody chose.
//
// SYNCHRONOUS, and it can take a while — the copy is bounded by the size of
// the store and the stream estate rather than by anything this handler
// decides. That is deliberate: the alternative is a job that outlives its
// request, which needs somewhere durable to record what it did, and the only
// place to record it is the very store being copied. The work is safe to be
// cut off — the store copy renames into place only after it verifies, and a
// backup is only a backup once its manifest is written — so a client that
// gives up, or a drain that ends the process mid-copy, leaves an unfinished
// directory rather than a false one.
func (a *App) serveBackup(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "no_destination",
			"detail": "name an absolute directory with ?dir=",
			"hint":   "it is created on the engine's host, not the caller's",
		})
		return
	}

	operator, _ := auth.OperatorFrom(r.Context())
	// WithoutCancel: a backup that has begun copying should finish and
	// leave one coherent artifact rather than a directory abandoned
	// halfway because the client hung up. The pieces already written are
	// not harmful — without a manifest they are visibly unfinished — but a
	// half-copied store file left where a complete one was about to land
	// is worth the few seconds it takes to finish.
	manifest, err := a.backup.Take(context.WithoutCancel(r.Context()), dir)
	if err != nil {
		// The reason goes to the LOG rather than the body, like every
		// other route here — except the two an operator can actually act
		// on, which are their own fault and are named.
		log.Warn("api_backup_failed", "operator", operator, "dir", dir, "error", err)
		// THE DETAIL IS RETURNED ONLY FOR THE CALLER'S OWN MISTAKE.
		// Everywhere else on this surface the internal reason goes to
		// the log alone, and that holds here: a copy that failed on the
		// engine's disk says "backup_failed" and nothing more. A
		// destination the caller named badly is the exception, because
		// the thing to fix is in their command rather than in this
		// process, and sending them to the engine's logs to find it
		// would be the wrong instruction.
		answer := map[string]string{"error": "backup_failed"}
		status := backupStatus(err)
		switch status {
		case http.StatusBadRequest:
			answer["detail"] = err.Error()
		case http.StatusServiceUnavailable:
			// THE FLEET'S STATE, NOT THIS NODE'S: named by a code and a
			// fixed hint, never the chunk list, which is the log's.
			answer["error"] = "objects_unreachable"
			answer["hint"] = "the company's files name chunks no reachable data " +
				"node could supply; bring back any data node that is down, check " +
				"the objects_missing alarm, and take the backup again"
		}
		writeJSON(w, status, answer)
		return
	}
	log.Info("backup_taken", "operator", operator, "dir", dir,
		"streams", len(manifest.Streams))
	writeJSON(w, http.StatusOK, manifest)
}

// backupStatus separates the caller's mistakes from the engine's failures.
//
// A destination that is occupied or relative is a 400: nothing is wrong with
// the node, the request named somewhere it cannot write, and answering 500
// would send an operator looking at the engine instead of at their own
// command.
//
// A copy naming chunks no data node could supply is a 503: this node is
// sound and so is the command, and what fails is the fleet holding the
// company's files — which clears when the node holding them returns, so it is
// the one failure here that trying again later can fix.
func backupStatus(err error) int {
	switch {
	case errors.Is(err, backup.ErrBadDestination):
		return http.StatusBadRequest
	case errors.Is(err, backup.ErrObjectsUnreachable):
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	apiOperator "github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Taking a backup over HTTP.
//
// A route rather than a command, because what has to be copied is only
// reachable, consistently, from inside the running engine. The store is two
// files this process holds an exclusive lock on, and the coordination estate
// is a broker embedded in this process whose store directory it holds too —
// so a CLI run while the engine is UP can open neither. With the engine down a
// command could reach the coordination store (engine.OpenFleetStore), but a
// backup is the store, the streams and the estate taken together, and only
// the engine that writes them can snapshot them as one.
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
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeNoDestination, map[string]string{
			"detail": "name an absolute directory with ?dir=",
			"hint":   "it is created on the engine's host, not the caller's",
		})
		return
	}

	if _, ok := auth.Caller(w, r); !ok {
		return
	}
	// WHO ASKED, in the three halves every record names a caller by — the
	// author, its kind and the credential — read through the one function
	// every surface attributes a write with, so the audit row and the log
	// line below cannot name two different people.
	by := auth.AttributionOf(r.Context())
	// WithoutCancel: a backup that has begun copying should finish and
	// leave one coherent artifact rather than a directory abandoned
	// halfway because the client hung up. The pieces already written are
	// not harmful — without a manifest they are visibly unfinished — but a
	// half-copied store file left where a complete one was about to land
	// is worth the few seconds it takes to finish.
	manifest, err := a.backup.Take(context.WithoutCancel(r.Context()), dir)
	// AUDITED EITHER WAY, once the copy was attempted: a backup is every
	// credential the company holds written to a directory somebody named,
	// and the one that failed halfway left files there too. A request
	// refused before the copy began wrote nothing and is not recorded — and
	// a DESTINATION refusal is exactly that: backup.ErrBadDestination is
	// returned only before a byte is copied, and a refusal that arrives with
	// an estate already in the directory is returned without it (see its
	// doc), so it is audited as the failed backup it is.
	// Recorded, a mistyped path read as a failed backup in the history and
	// in the failures a person counts, beside the field that said why. A
	// backup refused because another is running is the same: refused before
	// anything was written (backup.ErrBackupRunning), and no failed backup.
	if !errors.Is(err, backup.ErrBadDestination) && !errors.Is(err, backup.ErrBackupRunning) {
		a.auditBackup(r, by, dir, manifest, err)
	}
	if err != nil {
		// The reason goes to the LOG rather than the body, like every
		// other route here — except the two an operator can actually act
		// on, which are their own fault and are named.
		log.Warn("api_backup_failed", "by", by.Name, "operator", by.OperatorID,
			"dir", dir, "error", err)
		// THE DETAIL IS RETURNED ONLY FOR THE CALLER'S OWN MISTAKE.
		// Everywhere else on this surface the internal reason goes to
		// the log alone, and that holds here: a copy that failed on the
		// engine's disk says "backup_failed" and nothing more. A
		// destination the caller named badly is the exception, because
		// the thing to fix is in their command rather than in this
		// process, and sending them to the engine's logs to find it
		// would be the wrong instruction.
		refusal := backupRefusal(err)
		switch {
		case refusal.retryAfter > 0:
			// A STATE THAT CLEARS, with the wait it clears in: a 503
			// carrying its Retry-After, through the one writer that sets
			// the header and the envelope together.
			httpjson.UnavailableWith(w, refusal.code, refusal.retryAfter,
				httpjson.Detail{"hint": refusal.hint})
		case refusal.status == http.StatusBadRequest:
			httpjson.FailWith(w, refusal.status, refusal.code,
				map[string]string{"detail": err.Error()})
		case refusal.hint != "":
			httpjson.FailWith(w, refusal.status, refusal.code,
				map[string]string{"hint": refusal.hint})
		default:
			httpjson.Fail(w, refusal.status, refusal.code)
		}
		return
	}
	log.Info("backup_taken", "by", by.Name, "operator", by.OperatorID, "dir", dir,
		"streams", len(manifest.Streams))
	writeJSON(w, http.StatusOK, manifest)
}

// refusal is how POST /backup answers a backup it did not finish: a status, a
// code, and — for a state of the fleet with a remedy the engine knows — a
// fixed hint saying what to do. The hint is a sentence of this route's own,
// never the error's text, which names files, objects and positions and is the
// log's.
type refusal struct {
	status int
	code   httpjson.Code
	hint   string
	// retryAfter is the seconds a 503 tells its caller to wait, and zero
	// for every refusal waiting cannot clear.
	retryAfter int
}

// logGapRetry is how soon a backup a log gap refused may be taken again: AT
// ONCE, since the next take makes a fresh store copy whose position stands past
// the records the log no longer holds, so nothing has to happen in between — a
// second, the smallest wait a Retry-After carries, because zero would tell the
// caller not to come back at all.
const logGapRetry = time.Second

// backupRefusal separates the caller's mistakes and the fleet's states from
// the engine's failures.
//
// A destination that is occupied, relative or one this host cannot prepare is
// a 400: nothing is wrong with the node, the request named somewhere it cannot
// write, and answering 500 would send an operator looking at the engine
// instead of at their own command.
//
// A backup asked for while another runs on this node is a 409: the node is
// busy with exactly what was asked for, and the request can be made again once
// that one finishes. So are the refusals A ROLLING UPGRADE, a keyring
// rotation or a rebuilt log makes — a record on a log the backup replays that
// this build cannot read, one signed under a key this node does not hold, and
// a copy keyed to an earlier instance of a log — because this node is sound
// and so is the command: what it holds conflicts with the fleet's newer state,
// and another node, or this one once it has caught up, takes the backup. A
// record whose signature no key opens is not among them: it is bytes no node
// of the fleet wrote, and a 500 like every failure of the engine's own.
//
// A copy naming objects the object store did not answer for is a 503, and so
// is a log a restore could not replay to the snapshot: what failed is the
// store holding the company's files, or the log the copy needed, and each
// clears — the store answers again, the next copy stands past the gap — so
// these are the failures here that trying again later can fix.
//
// EVERY OTHER FAILURE IS A 500, whose reason is in this node's log alone.
func backupRefusal(err error) refusal {
	switch {
	case errors.Is(err, backup.ErrBadDestination):
		return refusal{status: http.StatusBadRequest, code: httpjson.CodeBackupFailed}
	case errors.Is(err, backup.ErrBackupRunning):
		return refusal{status: http.StatusConflict, code: httpjson.CodeBackupRunning,
			hint: "a backup is already running on this node; take this one when it finishes"}
	case errors.Is(err, objstore.ErrRecordUnreadable):
		return refusal{status: http.StatusConflict, code: httpjson.CodeRecordUnreadable,
			hint: "a log this backup replays holds a record a newer build wrote, which may " +
				"name a file this node's build cannot read; take the backup from a node " +
				"running that build, or from this one once it is upgraded"}
	case errors.Is(err, statelog.ErrRecordUnverified):
		return refusal{status: http.StatusConflict, code: httpjson.CodeRecordUnverified,
			hint: "a log this backup replays holds a record signed under a key this " +
				"node does not hold, which may name a file the backup would not carry; " +
				"add the key to this node's secrets.keys and restart it, or take the " +
				"backup from a node that holds it"}
	case errors.Is(err, statelog.ErrStreamRecreated):
		return refusal{status: http.StatusConflict, code: httpjson.CodeLogRecreated,
			hint: "this node's copy is keyed to an earlier instance of a log that has " +
				"since been recreated; take the backup once this node has re-keyed to the " +
				"live log, or from a node that has"}
	case errors.Is(err, backup.ErrObjectsUnreachable):
		// THE FLEET'S STATE, NOT THIS NODE'S: named by a code and a
		// fixed hint, never the list of objects, which is the log's. The
		// wait is the broker's own election: the default object store is
		// the data nodes' NATS bucket, which answers again no sooner than
		// its group elects a leader, and an S3 bucket that did not answer
		// is a blip of no longer scale.
		return refusal{status: http.StatusServiceUnavailable,
			code:       httpjson.CodeObjectsUnreachable,
			retryAfter: httpjson.RetrySeconds(statelog.ElectionRetryHint),
			hint: "the company's files name objects the object store did not answer for; " +
				"check that this node reaches the store store.objects names (the S3 " +
				"bucket, or the data nodes' NATS bucket) and take the backup again"}
	case errors.Is(err, backup.ErrLogGap):
		return refusal{status: http.StatusServiceUnavailable, code: httpjson.CodeLogGap,
			retryAfter: httpjson.RetrySeconds(logGapRetry),
			hint: "a log a restore would replay from the store copy no longer holds every " +
				"record the copy needs; take the backup again, whose copy stands past them, " +
				"and if it is refused the same way look for backup_hold_not_renewed in " +
				"this node's log"}
	}
	return refusal{status: http.StatusInternalServerError, code: httpjson.CodeBackupFailed}
}

// auditBackup publishes the backup's runtime audit record: who asked, through
// which credential, where, and whether it finished.
//
// WHICH NODE'S DISK is the envelope's `node`, which the queue stamps on the
// way out — and the queue this publishes through is this node's own, the node
// that just took the copy. Naming it here as well would state one fact twice.
//
// THROUGH THE OPERATOR SURFACE'S [apiOperator.Audit], so the envelope's source
// and the detached, logged-not-answered publish are decided once for every
// runtime audit record rather than once per route.
func (a *App) auditBackup(r *http.Request, by iam.Actor, dir string,
	manifest backup.Manifest, err error) {

	record := types.BackupRequested{
		ActorName:  by.Name,
		ActorKind:  string(by.Kind),
		OperatorID: by.OperatorID,
		Dir:        dir,
		Outcome:    types.AuditApplied,
		Streams:    len(manifest.Streams),
	}
	if err != nil {
		record.Outcome, record.Streams = types.AuditFailed, 0
	}
	apiOperator.Audit(r.Context(), a.audit, types.NewBackupRequested(record))
}

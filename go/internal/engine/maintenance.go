package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
)

// maintenanceDutyName is the fleet singleton the retention sweep claims.
const maintenanceDutyName = "maintenance"

// maintenanceDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, matching the sandbox waiter's ratio: one missed tick must not
// hand the duty to a peer, because a sweep is a burst of range deletes and
// two nodes doing it at once is exactly what the singleton avoids. Three
// ticks is long enough to ride out a slow claim and short enough that a dead
// node's duty is picked up within the hour.
const maintenanceDutyTTL = 3 * maintenance.Interval

// startMaintenance arms the retention sweep.
//
// EVERY short-horizon table in this process's stores, in one worker. They
// were all designed to be swept — each migration says so and each ships the
// index for it — and in the Python engine none of them ever were, because
// `purge` existed on every store and nothing called it. Wiring them here,
// rather than each subsystem arming its own loop, is what makes that
// impossible to repeat quietly: a store with a Purge and no entry in this
// function is visible in one place.
//
// Started LAST, like the sandbox waiter, because the duty is claimed under
// the node's own incarnation.
func (e *Engine) startMaintenance(ctx context.Context) {
	var jobs []maintenance.Job
	if db := e.backends.Store; db != nil {
		// Every one of these tables lives in this one store, which is
		// what makes a single list of them honest rather than a
		// coincidence: a node's short-horizon state IS its local index.
		jobs = append(jobs, maintenance.StoreJobs(db)...)
		jobs = append(jobs, maintenance.ChannelJobs(a2a.NewSQLStore(db))...)
		jobs = append(jobs, maintenance.ScheduleJobs(sqlledger.New(db.SQL()))...)
		jobs = append(jobs, maintenance.LedgerJobs(
			ledgerstore.NewCompletions(db),
			ledgerstore.NewConversations(db),
			e.ConversationRetention())...)
	}

	e.maintenance = maintenance.New(maintenance.Options{
		Jobs: jobs,
		ClaimDuty: maintenance.DutyFunc(
			e.workerDuty(maintenanceDutyName, maintenanceDutyTTL)),
	})
	// Detached, for the same reason the node's loops are: a sweep loop
	// bound to a signal context stops at SIGTERM, which is harmless here
	// but would make the worker's lifetime differ from every other loop's
	// for no reason a reader could find.
	e.maintenance.Start(context.WithoutCancel(ctx))
}

// ConversationRetention reads the operator's horizon for the conversation
// ledger, in days, off the ACTIVE epoch.
//
// Exported alongside [Engine.Maintenance] for the same reason: what a
// company actually forgets, and when, is an operator question.
//
// Read once at start, so a live config change lands at the next process
// start like every other sweep parameter — the alternative is a worker whose
// horizons move under it mid-tick, for a table whose horizon is measured in
// weeks.
//
// The field has existed since the ledger shipped and nothing ever read it:
// an operator setting `retention_days: 7` got thirty days of conversations,
// silently, because there was no sweep to honour it.
func (e *Engine) ConversationRetention() time.Duration {
	// No zero check: validation refuses retention_days below 1 and fills
	// the shipped default when it is unset, so a config that reached an
	// engine always carries a positive number. [maintenance.LedgerJobs]
	// holds the floor for callers that did not come through a parse.
	days := e.Company().Config.TurnEngine.ConversationSession.RetentionDays
	return time.Duration(days) * 24 * time.Hour
}

// Maintenance exposes the retention sweep.
//
// Exported because "is housekeeping running, and over what?" is a question
// an operator has to be able to ask: the failure this whole package fixes
// was invisible precisely because nothing anywhere could answer it.
func (e *Engine) Maintenance() *maintenance.Worker { return e.maintenance }

// stopMaintenance ends the sweep, waiting for an in-flight tick.
func (e *Engine) stopMaintenance() {
	if e.maintenance != nil {
		e.maintenance.Stop()
	}
}

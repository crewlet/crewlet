package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// The sweep of agents' lapsed grants, wired to this process: the config
// surface it writes the document through. Which grants have lapsed and are off
// every credential is the engine's ([engine.Engine.LapsedGrants]); a lapsed
// grant is never applied whether or not it is swept, so the sweep only keeps
// the document to the grants that hold.

// grantSweep is how often the sweep looks: the lapse lead's own length
// ([seataccess.LapseLead]), so a lapsed grant leaves the Agents tab within a
// minute of leaving the credential.
const grantSweep = seataccess.LapseLead

// grantSweeper is who a sweep's revision records as its author.
func grantSweeper() iam.Actor { return iam.Actor{Name: "grant expiry", Kind: iam.ActorSystem} }

// keepSweepingGrants runs [sweepGrantsOnce] every [grantSweep] for as long as
// the node runs. Every node holding the estate runs it; the revision's
// compare-and-set lets one write and the rest find nothing left to do.
func keepSweepingGrants(ctx context.Context, e *engine.Engine, cfg *configapi.Service, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(grantSweep):
		}
		sweepGrantsOnce(ctx, e, cfg, log)
	}
}

// sweepGrantsOnce drops every lapsed grant no credential holds from the
// document in one revision, records each as expired, and drops every agent's
// request past its month. An operator-managed document is the operator's:
// its lapsed grants are left there, never applied.
func sweepGrantsOnce(ctx context.Context, e *engine.Engine, cfg *configapi.Service, log *slog.Logger) {
	now := time.Now().UTC()
	if requests, err := e.GrantRequests(); err == nil {
		if err := requests.Sweep(ctx, now); err != nil {
			log.WarnContext(ctx, "grant_requests_unswept", "error", err.Error())
		}
	}
	if cfg.Managed() {
		return
	}
	// THE REVISION THE SWEEP BUILDS ON, so two nodes sweeping at once write
	// one revision: the other's write is a race, read again next time.
	_, revision, err := cfg.StoredDocument(ctx)
	if err != nil {
		if !errors.Is(err, configapi.ErrNoActiveRevision) {
			log.WarnContext(ctx, "grant_sweep_failed", "error", err.Error(), "retry_in", grantSweep.String())
		}
		return
	}
	lapsed, err := e.LapsedGrants(ctx, now)
	if err != nil {
		log.WarnContext(ctx, "grant_sweep_failed", "error", err.Error(), "retry_in", grantSweep.String())
		return
	}
	if len(lapsed) == 0 {
		return
	}
	var handles []string
	var edits []configapi.SeatEdit
	for _, handle := range lapsedHandles(lapsed) {
		mine := slices.DeleteFunc(slices.Clone(lapsed), func(c seataccess.GrantChange) bool { return c.Handle != handle })
		handles = append(handles, handle)
		edits = append(edits, configapi.SeatEdit{Handle: handle, Edit: func(role *config.Role) error {
			return dropGrants(role, mine)
		}})
	}
	applied, err := cfg.ApplySeats(ctx, configapi.ApplySeatsRequest{
		Seats: edits, By: grantSweeper(),
		Summary: "Remove lapsed grants from " + strings.Join(handles, ", "),
		Expect:  revision,
	})
	var raced *configapi.RacedError
	switch {
	case errors.As(err, &raced):
		log.InfoContext(ctx, "grant_sweep_raced", "detail", "the document moved; the next sweep reads it again")
		return
	case err != nil:
		log.WarnContext(ctx, "grant_sweep_failed", "error", err.Error(), "retry_in", grantSweep.String())
		return
	}
	e.RecordLapsed(ctx, lapsed, now)
	log.InfoContext(ctx, "grant_sweep_written", "revision", applied.RevisionID, "seats", strings.Join(handles, ","))
}

// lapsedHandles is each seat a sweep changes, sorted and once.
func lapsedHandles(changes []seataccess.GrantChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Handle)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// dropGrants removes each change's grants from role's block for its lane, by
// id, leaving every other setting as it was.
func dropGrants(role *config.Role, changes []seataccess.GrantChange) error {
	for _, c := range changes {
		d, ok := seataccess.DesiredOf(role, c.Lane)
		if !ok {
			continue
		}
		d.Grants = slices.DeleteFunc(d.Grants, func(g seataccess.Grant) bool {
			return slices.ContainsFunc(c.Removed, func(r seataccess.Grant) bool { return r.ID == g.ID })
		})
		if err := seataccess.SetDesired(role, c.Lane, d); err != nil {
			return err
		}
	}
	return nil
}

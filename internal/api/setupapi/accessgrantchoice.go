package setupapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Choosing a request's grant option saves the grant here before the answer posts
// (ADR-0118), by the Agents tab's own rules for a grant.

// grantSaveAttempts bounds re-judging a save the document moved under.
const grantSaveAttempts = 3

// SaveRequestedGrant saves request id's grant for asker as the person ctx
// resolves to, after admit(raise) admits it, and saves nothing twice. Errors:
// [seataccess.ErrNoRequest], [*seataccess.GrantRefused], [*configapi.ManagedError].
func (s *Service) SaveRequestedGrant(ctx context.Context, asker, id string, permanent bool,
	admit func(raise bool) error) error {

	by := auth.AttributionOf(ctx)
	if err := s.config.Authorize(by); err != nil {
		return err
	}
	req, err := s.access.GrantRequest(ctx, id)
	if err != nil {
		return err
	}
	if req.Handle != asker {
		return &seataccess.GrantRefused{Reason: "the request " + id + " was asked by another agent"}
	}
	for range grantSaveAttempts {
		err = s.saveRequestedGrant(ctx, req, permanent, admit)
		var raced *configapi.RacedError
		if !errors.As(err, &raced) {
			return err
		}
	}
	return fmt.Errorf("setupapi: the company document changed %d times while the grant was "+
		"saved; send the answer again: %w", grantSaveAttempts, err)
}

// saveRequestedGrant is one attempt, on the fleet's revision as it reads now.
func (s *Service) saveRequestedGrant(ctx context.Context, req seataccess.GrantRequest, permanent bool,
	admit func(raise bool) error) error {

	fc, err := s.readCompany(ctx)
	if err != nil {
		return err
	}
	q := AccessQuery{Kind: req.Kind, Company: fc.company, Roster: fc.roster, Revision: fc.revision}
	current, err := s.access.Answer(ctx, q)
	if err != nil {
		return err
	}
	seat, found := seatOf(current, req.Handle)
	if !found || seat.Departed {
		return &seataccess.GrantRefused{Reason: req.Handle + " is no longer an agent on " +
			seataccess.LaneName(req.Lane)}
	}
	before := seat.Lanes[req.Lane].Desired
	now := time.Now().UTC()
	if slices.ContainsFunc(before.Grants, func(g seataccess.Grant) bool {
		return g.Request == req.ID && g.Live(now) && g.ExpiresAt.IsZero() == permanent
	}) {
		return nil
	}
	if !before.Enabled {
		return &seataccess.GrantRefused{Reason: req.Handle + " is off at " + seataccess.LaneName(req.Lane) +
			"; turn it on on the integration's Agents tab first"}
	}
	by := auth.AttributionOf(ctx)
	after := before
	after.Grants = append(slices.Clone(before.Grants), req.Grant(permanent, now))
	stamped, err := seataccess.StampGrants(before, after, by.Name, now)
	if err != nil {
		return &seataccess.GrantRefused{Reason: err.Error()}
	}
	var offer seataccess.Options
	for _, o := range current.Options {
		if o.Lane == req.Lane {
			offer = o
		}
	}
	if refusal := grantRefusal(offer, before, stamped); refusal != nil {
		return &seataccess.GrantRefused{Reason: refusal.Detail}
	}
	if err = admit(seataccess.NeedsStepUp(offer, before, stamped)); err != nil {
		return err
	}
	seats := map[string]map[seataccess.Lane]seataccess.Desired{req.Handle: {req.Lane: stamped}}
	edits, changed := seatEdits(current, seats)
	applied, err := s.config.ApplySeats(ctx, configapi.ApplySeatsRequest{
		Seats: edits, By: by, Expect: q.Revision,
		Summary: fmt.Sprintf("Grant %s %s on %s, as its request %s asked", req.Handle, req.Permission,
			seataccess.LaneName(req.Lane), req.ID),
	})
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "setup_access_request_granted", "kind", req.Kind, "lane", req.Lane,
		"seat", req.Handle, "request", req.ID, "permanent", permanent, "revision", applied.RevisionID,
		"by", by.Name, "operator", by.OperatorID)
	s.access.RecordGrants(ctx, req.Kind, by, grantChanges(changed))
	s.startSync(ctx, AccessSync{Kind: req.Kind, Revision: applied.RevisionID, By: by,
		Handles: []string{req.Handle}, AfterSave: true})
	return nil
}

package googlegrowth

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// SeatAccess is a Google growth product's half of per-seat access. A seat's
// access is its level and its scope (properties, sites, accounts), as the
// company document writes them, enforced by the engine on every call; its
// identity is its Google growth identity, which the Google identity pass makes
// and turns off (internal/googleauth), never this lane. So no step is drawn:
// what a seat holds is what the document says, and the role a person gave the
// identity at the vendor is the ceiling, which a call refused there names.
type SeatAccess struct {
	Product integration.Kind
	Lane    seataccess.Lane
	// Name is the product's name for a person.
	Name string
	// Levels builds the product's levels, least first, when one is asked
	// for, never at init: a product registers at init, which every push's
	// hook pays for (cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit).
	Levels func() []seataccess.Level
	// Scope is the company's narrowing of what seats may be given, nil where
	// it names none.
	Scope func(c *config.Company) []string
}

// Kind is the product the lane is.
func (v SeatAccess) Kind() integration.Kind { return v.Product }

// Catalog is the per-seat offer: fixed levels on the scope a seat names, which
// it must name. Nothing is sealed: tokens are minted at use.
func (v SeatAccess) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: v.Product, Lanes: []seataccess.LaneSpec{{
		Lane: v.Lane, Levels: true, Scoped: true, ScopeRequired: true,
		CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, EngineHeld: true,
	}}}
}

// Steps is empty for every op: the identity is the core's, the rest the
// document.
func (SeatAccess) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

// Reach is the company's scope where it narrows it, and open where it names
// none: the identity's own reach at the vendor then bounds it.
func (v SeatAccess) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil || !c.DeclaresIntegration(string(v.Product)) {
		return seataccess.Reach{}
	}
	if scope := v.Scope(c); len(scope) > 0 {
		return seataccess.Reach{Scope: scope}
	}
	return seataccess.Reach{Open: true}
}

// FixedLevels is the product's levels, least first.
func (v SeatAccess) FixedLevels(seataccess.Lane) []seataccess.Level { return v.Levels() }

// Open builds a session from the company's block. A product that is off, or
// without the Google identity it signs in through, is not configured.
func (v SeatAccess) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.GrowthEnabled(string(v.Product)) {
		return nil, fmt.Errorf("googlegrowth: %w: integrations.%s is off", integration.ErrNotConfigured, v.Product)
	}
	gc := in.Config.Integrations.GoogleCloud
	if gc == nil || !gc.Enabled || gc.Provisioning == nil {
		return nil, fmt.Errorf("googlegrowth: %w: %s signs in through integrations.google_cloud, which is off or "+
			"has no provisioning block", integration.ErrNotConfigured, v.Name)
	}
	identity, _ := config.GoogleCloudServiceAccount(gc.Provisioning.Provisioner)
	return &seatSession{access: v, company: in.Config, identity: identity}, nil
}

// seatSession reads the document it was opened on, and nothing else.
type seatSession struct {
	access   SeatAccess
	company  *config.Company
	identity string
}

// Options is the levels; the scope is the document's ([SeatAccess.Reach]),
// which [seataccess.ReadOptions] applies.
func (s *seatSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: s.access.Levels()}, nil
}

// Observe is each seat's setting in the document, its account the growth
// identity's email: a seat that is on holds exactly what it is granted, since
// the engine enforces it on every call. A seat that is off holds nothing.
func (s *seatSession) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	roles := map[string]*config.Role{}
	for _, seat := range seataccess.SeatsOf(s.company) {
		roles[seat.Handle] = seat.Role
	}
	out := seataccess.Observation{}
	for _, handle := range in.Handles() {
		var observed seataccess.Observed
		if d, ok := seataccess.DesiredOf(roles[handle], s.access.Lane); ok && d.Enabled {
			email := googleauth.AccountEmail(googleauth.GrowthAccountID(s.company.Name, handle), s.identity)
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: email, AccountID: email,
				Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{s.access.Lane: observed}
	}
	return out, nil
}

// Apply has nothing to take: [SeatAccess.Steps] draws no step.
func (s *seatSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("googlegrowth: no step %s exists for %s: %s access is the "+
		"document and the identity's roles there", in.Step.Op, in.Seat.Handle, s.access.Name)
}

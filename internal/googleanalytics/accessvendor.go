package googleanalytics

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Google Analytics' half of per-seat access. A seat's access is its level and
// its properties, as the company document writes them, enforced by the engine
// on every call; its identity is its Google growth identity, which the Google
// identity pass makes and turns off (internal/googleauth), never this lane.
// So every step is empty: what a seat holds is what the document says, and
// the role a person gave the identity in each property is the vendor's
// ceiling, which a call refused at Google names.

func init() { seataccess.Register(accessVendor{}) }

// accessVendor is Google Analytics' [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return Kind }

// Catalog is the per-seat offer: three fixed levels on the properties a seat
// names, which it must name. Nothing is sealed: tokens are minted at use.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind: Kind,
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneGoogleAnalytics, Levels: true, Scoped: true, ScopeRequired: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, EngineHeld: true,
		}},
	}
}

// Steps is empty for every op: the identity is the core's, and the rest is
// the document.
func (accessVendor) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

// Reach is the company's properties where it narrows them, and open where it
// names none: the identity's own reach at Google then bounds it.
func (accessVendor) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil || c.Integrations.GoogleAnalytics == nil {
		return seataccess.Reach{}
	}
	if props := c.Integrations.GoogleAnalytics.Properties; len(props) > 0 {
		return seataccess.Reach{Scope: props}
	}
	return seataccess.Reach{Open: true}
}

// FixedLevels is the three levels, least first. Operate and administer each
// raise what the agent may do, so granting either needs a step-up.
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: "Read", Note: "Reports and every setting, nothing changed. Add the " +
			"agent's identity to each property at Viewer or above."},
		{ID: string(growth.LevelOperate), Name: "Operate", Administers: true, Note: "Read, plus reversible " +
			"changes made at once and recorded: key events, custom definitions, audiences, settings. Needs " +
			"Marketer or Editor on the property."},
		{ID: string(growth.LevelAdminister), Name: "Administer", Administers: true, Note: "Operate, plus asking " +
			"an administrator to approve each account, access, link, key or irreversible change. Needs Editor, " +
			"or Administrator for access."},
	}
}

// Open builds a session from the company's block. An integration that is
// absent, off, or without the Google identity it signs in through is not
// configured.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.GrowthEnabled(string(Kind)) {
		return nil, fmt.Errorf("googleanalytics: %w: integrations.google_analytics is off", integration.ErrNotConfigured)
	}
	gc := in.Config.Integrations.GoogleCloud
	if gc == nil || !gc.Enabled || gc.Provisioning == nil {
		return nil, fmt.Errorf("googleanalytics: %w: Google Analytics signs in through integrations.google_cloud, "+
			"which is off or has no provisioning block", integration.ErrNotConfigured)
	}
	identity, _ := config.GoogleCloudServiceAccount(gc.Provisioning.Provisioner)
	return &accessSession{company: in.Config, identity: identity}, nil
}

// accessSession reads the document it was opened on, and nothing else.
type accessSession struct {
	company  *config.Company
	identity string
}

// Options is the three levels and the company's properties.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	reach := accessVendor{}.Reach(s.company, nil, lane)
	return seataccess.Options{
		Lane: lane, Levels: accessVendor{}.FixedLevels(lane), Scoped: true, ScopeRequired: true,
		Scope: reach.Scope, ScopeOpen: reach.Open, ScopeLive: true,
	}, nil
}

// Observe is each seat's setting in the document, its account the growth
// identity's email: a seat that is on holds exactly what it is granted, since
// the engine enforces it on every call. A seat that is off holds nothing.
func (s *accessSession) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	roles := map[string]*config.Role{}
	for _, seat := range seataccess.SeatsOf(s.company) {
		roles[seat.Handle] = seat.Role
	}
	out := seataccess.Observation{}
	for _, handle := range in.Handles() {
		var observed seataccess.Observed
		if d, ok := seataccess.DesiredOf(roles[handle], seataccess.LaneGoogleAnalytics); ok && d.Enabled {
			email := googleauth.AccountEmail(googleauth.GrowthAccountID(s.company.Name, handle), s.identity)
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: email, AccountID: email,
				Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneGoogleAnalytics: observed}
	}
	return out, nil
}

// Apply has nothing to take: [accessVendor.Steps] draws no step.
func (*accessSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("googleanalytics: no step %s exists for %s: Google Analytics "+
		"access is the document and the identity's roles in Google Analytics", in.Step.Op, in.Seat.Handle)
}

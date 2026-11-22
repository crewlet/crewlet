package appstoreconnect

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// App Store Connect's half of per-seat access, SSH's shape: a seat's access is
// its level and its apps as the document writes them, and the engine makes
// every call with the company's keys, so there is no account anywhere to
// create, level, disable or delete and every step is empty.

func init() { seataccess.Register(accessVendor{}) }

// The levels' words, as the Agents tab draws them.
const (
	readNote       = "Reads apps, listings, reviews, TestFlight and reports. No approval."
	operateNote    = "Also edits drafts and internal testing at once, and asks an administrator for anything customers or external testers see."
	administerNote = "Also asks an administrator for account changes: users, certificates, devices, profiles, agreements and deletes."
)

type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindAppStoreConnect }

// Catalog is the lane: three fixed levels on the apps a seat names, which it
// must name. Nothing is issued: no seat holds a key (ADR-0170).
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: integration.KindAppStoreConnect, Lanes: []seataccess.LaneSpec{{
		Lane: seataccess.LaneAppStoreConnect, Levels: true, Scoped: true, ScopeRequired: true,
		CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, EngineHeld: true,
	}}}
}

// Steps is empty for every op: nothing exists outside the document to change.
func (accessVendor) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

// FixedLevels are the three, least access first; operate and administer ask
// for a step-up to give.
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: growth.LevelRead.Label(), Note: readNote},
		{ID: string(growth.LevelOperate), Name: growth.LevelOperate.Label(), Note: operateNote, Administers: true},
		{ID: string(growth.LevelAdminister), Name: growth.LevelAdminister.Label(), Note: administerNote,
			Administers: true},
	}
}

// Reach is the apps the company names, which a seat's apps must be within;
// open where it names none, since the keys' own reach is read live.
func (accessVendor) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil || c.Integrations.AppStoreConnect == nil || len(c.Integrations.AppStoreConnect.Apps) == 0 {
		return seataccess.Reach{Open: true}
	}
	return seataccess.Reach{Scope: c.Integrations.AppStoreConnect.Apps}
}

// Open builds a session from the company's block; one absent or off is not
// configured.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.AppStoreConnect.GrowthEnabled() {
		return nil, fmt.Errorf("appstoreconnect: %w: integrations.app_store_connect is off",
			integration.ErrNotConfigured)
	}
	return &accessSession{company: in.Config}, nil
}

type accessSession struct{ company *config.Company }

// Options are the three levels and the apps the company names.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: accessVendor{}.FixedLevels(lane), Scoped: true, ScopeRequired: true,
		Scope: s.company.Integrations.AppStoreConnect.Apps, ScopeLive: true}, nil
}

// Observe is each seat's setting in the document, which there is nothing
// outside the document to drift from.
func (s *accessSession) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	roles := map[string]*config.Role{}
	for _, seat := range seataccess.SeatsOf(s.company) {
		roles[seat.Handle] = seat.Role
	}
	out := seataccess.Observation{}
	for _, handle := range in.Handles() {
		var observed seataccess.Observed
		if d, ok := seataccess.DesiredOf(roles[handle], seataccess.LaneAppStoreConnect); ok && d.Enabled {
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: "crewlet/" + handle,
				AccountID: "crewlet/" + handle, Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneAppStoreConnect: observed}
	}
	return out, nil
}

// Apply has nothing to take: [accessVendor.Steps] draws no step.
func (*accessSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("appstoreconnect: no step %s exists for %s: access is the document alone",
		in.Step.Op, in.Seat.Handle)
}

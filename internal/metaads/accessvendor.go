package metaads

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Meta Ads' half of per-seat access, SSH's shape: a seat's access is its level
// and its ad accounts as the document writes them, and the engine makes every
// call with the company's system user, so there is no account anywhere to
// create, level, disable or delete and every step is empty.

func init() { seataccess.Register(accessVendor{}) }

// The levels' words, as the Agents tab draws them.
const (
	readNote       = "Reads ad accounts, campaigns, ad sets, ads, creatives, audiences and insights. No approval."
	operateNote    = "Also pauses, lowers budgets and bids and makes what nothing serves yet at once, and asks an administrator for any spend or anything customers see."
	administerNote = "Also asks an administrator for account changes, production events and deletes."
)

type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindMetaAds }

// Catalog is the lane: three fixed levels on the ad accounts a seat names,
// which it must name. Nothing is issued: no seat holds a token (ADR-0170).
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: integration.KindMetaAds, Lanes: []seataccess.LaneSpec{{
		Lane: seataccess.LaneMetaAds, Levels: true, Scoped: true, ScopeRequired: true,
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
		{ID: string(growth.LevelRead), Name: "Read", Note: readNote},
		{ID: string(growth.LevelOperate), Name: "Operate", Note: operateNote, Administers: true},
		{ID: string(growth.LevelAdminister), Name: "Administer", Note: administerNote, Administers: true},
	}
}

// Reach is the ad accounts the company names, which a seat's must be within;
// open where it names none, since the system user's own reach is read live.
func (accessVendor) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil || c.Integrations.MetaAds == nil || len(c.Integrations.MetaAds.AdAccounts) == 0 {
		return seataccess.Reach{Open: true}
	}
	return seataccess.Reach{Scope: c.Integrations.MetaAds.AdAccountIDs()}
}

// Open builds a session from the company's block; one absent or off is not
// configured.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.MetaAds.GrowthEnabled() {
		return nil, fmt.Errorf("metaads: %w: integrations.meta_ads is off", integration.ErrNotConfigured)
	}
	return &accessSession{company: in.Config}, nil
}

type accessSession struct{ company *config.Company }

// Options are the three levels; the ad accounts are the document's
// ([accessVendor.Reach]), which [seataccess.ReadOptions] applies.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: accessVendor{}.FixedLevels(lane)}, nil
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
		if d, ok := seataccess.DesiredOf(roles[handle], seataccess.LaneMetaAds); ok && d.Enabled {
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: "crewlet/" + handle,
				AccountID: "crewlet/" + handle, Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneMetaAds: observed}
	}
	return out, nil
}

// Apply has nothing to take: [accessVendor.Steps] draws no step.
func (*accessSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("metaads: no step %s exists for %s: access is the document alone",
		in.Step.Op, in.Seat.Handle)
}

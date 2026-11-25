package appstoreconnect

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// App Store Connect's half of per-seat access, SSH's shape: a seat's access is
// its level and its apps as the document writes them, so there is no account
// anywhere to create, level, disable or delete and every step is empty. Every
// call of a seat's is signed with its own team key (identity.go, ADR-0179).

func init() { seataccess.Register(accessVendor{}) }

// The levels' words, as the Agents tab draws them.
const (
	readNote       = "Reads apps, listings, reviews, TestFlight and reports. No approval."
	operateNote    = "Also edits drafts and internal testing at once, and asks an administrator for anything customers or external testers see."
	administerNote = "Also asks an administrator for account changes: users, certificates, devices, profiles and deletes."
)

type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindAppStoreConnect }

// Catalog is the lane: three fixed levels on the apps a seat names, which it
// must name, and the seat's own team key, which a person hands in.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: integration.KindAppStoreConnect, Lanes: []seataccess.LaneSpec{{
		Lane: seataccess.LaneAppStoreConnect, Levels: true, Scoped: true, ScopeRequired: true,
		CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, EngineHeld: true,
		Identity: identitySpec,
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

// Reach is open: a seat names any app of the connected teams, which the
// session lists by name once an agent's key listed them.
func (accessVendor) Reach(*config.Company, *org.Organization, seataccess.Lane) seataccess.Reach {
	return seataccess.Reach{Open: true}
}

// Open builds a session from the company's block and the apps the engine's
// product read with the agents' keys ([Reach], handed in as in.Reads); one
// absent or off is not configured.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.AppStoreConnect.GrowthEnabled() {
		return nil, fmt.Errorf("appstoreconnect: %w: integrations.app_store_connect is off",
			integration.ErrNotConfigured)
	}
	s := &accessSession{company: in.Config}
	switch r := in.Reads.(type) {
	case Reach:
		s.reach = r
	case Reads:
		s.reach, s.product = r.Reach, r.product
	}
	return s, nil
}

type accessSession struct {
	company *config.Company
	// reach is the teams' apps, nil where nothing was handed in.
	reach Reach
	// product is the node's product, which checks a seat's own key with
	// Apple; nil where the pass handed none in.
	product *Product
}

// errNoProduct is a session opened without the node's product, which cannot
// read a key at Apple.
var errNoProduct = errors.New("appstoreconnect: this node holds no App Store Connect product to check a key with")

// KeepIdentity checks the key a person handed in for the seat.
func (s *accessSession) KeepIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept,
	error) {
	if s.product == nil {
		return seataccess.IdentityKept{}, errNoProduct
	}
	if in.Way != seataccess.WayHandedIn {
		return seataccess.IdentityKept{}, &seataccess.Refusal{Said: "Apple makes a team key only by hand.",
			Remedy: "Make the key at Apple and hand in its file."}
	}
	return s.product.keepKey(ctx, in)
}

// CheckIdentity reads the seat's kept key back.
func (s *accessSession) CheckIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityCheck,
	error) {
	if s.product == nil {
		return seataccess.IdentityCheck{}, errNoProduct
	}
	return s.product.checkKept(ctx, in)
}

// DropIdentity gives the seat's key up, asking a person to revoke it at Apple.
func (s *accessSession) DropIdentity(_ context.Context, in seataccess.IdentityInput) (seataccess.IdentityDropped,
	error) {
	if s.product == nil {
		return seataccess.IdentityDropped{}, errNoProduct
	}
	return s.product.dropKey(in), nil
}

// Options are the three levels and every app of the connected teams the
// agents' keys listed, by name with its bundle ID, for the chooser.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	out := seataccess.Options{Lane: lane, Levels: accessVendor{}.FixedLevels(lane),
		ScopeTyped: config.AppStoreConnectAppPattern}
	for _, a := range s.reach.Apps() {
		out.Scope = append(out.Scope, a.ID)
		if a.Name != "" {
			if out.ScopeNames == nil {
				out.ScopeNames = map[string]string{}
			}
			out.ScopeNames[a.ID] = a.Name
		}
		if a.BundleID != "" {
			if out.ScopeDetails == nil {
				out.ScopeDetails = map[string]string{}
			}
			out.ScopeDetails[a.ID] = a.BundleID
		}
	}
	return out, nil
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

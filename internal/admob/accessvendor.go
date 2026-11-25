package admob

import (
	"context"
	"fmt"
	"net/http"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// AdMob's half of per-seat access, SSH's shape: a seat's access is its level
// and its accounts as the document writes them, and every call goes as the
// company's consent grant, so there is no account at Google to create, level,
// disable or delete and every step is empty.

func init() { seataccess.Register(accessVendor{}) }

// The levels' words, as the Agents tab draws them.
const (
	readNote       = "Reads accounts, apps, ad units, mediation and reports. No approval."
	operateNote    = "Also creates apps, ad units and mappings, and changes mediation groups and A/B experiments, at once; each change is on the access log."
	administerNote = "Also asks an administrator for what cannot be undone: linking an app to its store listing, removing a mediation line."
)

type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return Kind }

// Catalog is the lane: three fixed levels on the accounts a seat names, which
// it must name. Nothing is issued: no seat holds a token (ADR-0170).
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: Kind, Lanes: []seataccess.LaneSpec{{
		Lane: seataccess.LaneAdMob, Levels: true, Scoped: true, ScopeRequired: true,
		CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, EngineHeld: true,
		// THE COMPANY'S IDENTITY ONLY: the vendor offers no identity per agent (ADR-0179).
		Identity: seataccess.CompanyOnly,
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

// Reach is the accounts the company names, which a seat's accounts must be
// within; open where it names none, since the sign-in's reach is read live.
func (accessVendor) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil || c.Integrations.AdMob == nil || len(c.Integrations.AdMob.Accounts) == 0 {
		return seataccess.Reach{Open: true}
	}
	return seataccess.Reach{Scope: c.Integrations.AdMob.Accounts}
}

// Open builds a session from the company's block; one absent or off is not
// configured.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.AdMob.GrowthEnabled() {
		return nil, fmt.Errorf("admob: %w: integrations.admob is off", integration.ErrNotConfigured)
	}
	return &accessSession{company: in.Config, tokens: in.Tokens}, nil
}

// accessSession reads the company document, and lists the accounts the
// company's sign-in reaches with the tokens the engine issues.
type accessSession struct {
	company *config.Company
	tokens  seataccess.IssuedTokens
}

// Options are the three levels and the publisher accounts the company's
// sign-in lists; the reach is the document's ([accessVendor.Reach]), which
// [seataccess.ReadOptions] applies.
func (s *accessSession) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return googlegrowth.GrantOptions(ctx, googlegrowth.Granted{Product: Kind, Lane: lane,
		Levels: accessVendor{}.FixedLevels(lane), Name: "AdMob", Entries: "publisher accounts",
		Typed: config.GooglePublisherPattern, List: listAccounts}, s.tokens), nil
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
		if d, ok := seataccess.DesiredOf(roles[handle], seataccess.LaneAdMob); ok && d.Enabled {
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: "crewlet/" + handle,
				AccountID: "crewlet/" + handle, Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneAdMob: observed}
	}
	return out, nil
}

// Apply has nothing to take: [accessVendor.Steps] draws no step.
func (*accessSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("admob: no step %s exists for %s: access is the document alone",
		in.Step.Op, in.Seat.Handle)
}

// listAccounts is every publisher account the token reaches. AdMob names
// none, so each is offered by its publisher ID.
func listAccounts(ctx context.Context, in googlegrowth.Lister) ([]googlegrowth.Listed, error) {
	var page struct {
		Account []struct {
			PublisherID string `json:"publisherId"`
		} `json:"account"`
	}
	if err := in.Send.Do(ctx, http.MethodGet, api+"/v1/accounts", in.Token, nil, &page); err != nil {
		return nil, err
	}
	out := make([]googlegrowth.Listed, 0, len(page.Account))
	for _, a := range page.Account {
		out = append(out, googlegrowth.Listed{ID: a.PublisherID})
	}
	return out, nil
}

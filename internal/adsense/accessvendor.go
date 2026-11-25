package adsense

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// AdSense's half of per-seat access, SSH's shape: a seat's access is its level
// and its accounts as the document writes them, and every call goes as the
// company's consent grant, so there is no account at Google to create, level,
// disable or delete and every step is empty.

func init() { seataccess.Register(accessVendor{}) }

// The levels' words, as the Agents tab draws them.
const (
	readNote       = "Reads accounts, sites, ad units, channels, alerts, policy issues, payments and reports. No approval."
	operateNote    = "Also creates and renames ad units and custom channels at once, each change on the access log, and asks an administrator before a live ad unit's size changes or a site goes to review."
	administerNote = "Also asks an administrator for what cannot be undone: deleting a custom channel, a platform's sub-accounts, their sites and events, and moving a site to another revenue group."
)

type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return Kind }

// Catalog is the lane: three fixed levels on the accounts a seat names, which
// it must name. Nothing is issued: no seat holds a token (ADR-0170).
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: Kind, Lanes: []seataccess.LaneSpec{{
		Lane: seataccess.LaneAdSense, Levels: true, Scoped: true, ScopeRequired: true,
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
	if c == nil || c.Integrations.AdSense == nil || len(c.Integrations.AdSense.Accounts) == 0 {
		return seataccess.Reach{Open: true}
	}
	return seataccess.Reach{Scope: c.Integrations.AdSense.Accounts}
}

// Open builds a session from the company's block; one absent or off is not
// configured.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.AdSense.GrowthEnabled() {
		return nil, fmt.Errorf("adsense: %w: integrations.adsense is off", integration.ErrNotConfigured)
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
		Levels: accessVendor{}.FixedLevels(lane), Name: "AdSense", Entries: "publisher accounts",
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
		if d, ok := seataccess.DesiredOf(roles[handle], seataccess.LaneAdSense); ok && d.Enabled {
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: "crewlet/" + handle,
				AccountID: "crewlet/" + handle, Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneAdSense: observed}
	}
	return out, nil
}

// Apply has nothing to take: [accessVendor.Steps] draws no step.
func (*accessSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("adsense: no step %s exists for %s: access is the document alone",
		in.Step.Op, in.Seat.Handle)
}

// maxAccountPages bounds the accounts listing a picker reads: a sign-in that
// reaches more is an agency, which connects each client's company.
const maxAccountPages = 10

// listAccounts is every publisher account the token reaches, by its name.
func listAccounts(ctx context.Context, in googlegrowth.Lister) ([]googlegrowth.Listed, error) {
	var out []googlegrowth.Listed
	token := ""
	for range maxAccountPages {
		q := url.Values{"pageSize": {"50"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Accounts []struct {
				Name        string `json:"name"`
				DisplayName string `json:"displayName"`
			} `json:"accounts"`
			Next string `json:"nextPageToken"`
		}
		if err := in.Send.Do(ctx, http.MethodGet, management+"/accounts?"+q.Encode(), in.Token, nil,
			&page); err != nil {
			return nil, err
		}
		for _, a := range page.Accounts {
			out = append(out, googlegrowth.Listed{ID: strings.TrimPrefix(a.Name, "accounts/"), Name: a.DisplayName})
		}
		if token = page.Next; token == "" {
			break
		}
	}
	return out, nil
}

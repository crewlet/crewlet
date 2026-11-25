package config

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/org"
)

// AppStoreConnect connects the company's App Store Connect teams: each agent
// signs every call with its own team key (ADR-0179), so the company names only
// each team's issuer, which a key signs in with.
type AppStoreConnect struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Approvers route the integration's held acts; they never decide who may
	// answer (an administrator, ADR-0171).
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Optional. Handles of human seats asked first about a reply, a submission, a price or an account change an agent asks for. Only an administrator may answer; without this list every administrator is asked."`

	// Connections are the teams, by connection id.
	Connections map[string]AppStoreConnectTeam `yaml:"connections,omitempty" json:"connections,omitempty" desc:"The App Store Connect teams the agents work in, each under its connection id (lowercase letters, digits and hyphens, starting with a letter). Required when enabled."`
}

// AppStoreConnectTeam is one team: the issuer its agents' keys sign in with.
type AppStoreConnectTeam struct {
	IssuerID string `yaml:"issuer_id,omitempty" json:"issuer_id,omitempty" desc:"The team's issuer ID, a UUID shown on Users and Access, Integrations, App Store Connect API."`
}

// ConnectionIDs are the block's connection ids, sorted: the order a key is
// tried against the teams in.
func (a *AppStoreConnect) ConnectionIDs() []string {
	if a == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(a.Connections))
}

// RoleAppStoreConnect is a seat's access to App Store Connect.
type RoleAppStoreConnect struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may use App Store Connect through the engine's tools, each call signed with its own team key, which a person hands in on the Agents tab."`

	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): reads apps, listings, reviews, TestFlight, reports. operate: also edits drafts and internal testing, and asks an administrator for anything customers or external testers see. administer: also asks for account changes (users, certificates, devices, profiles) and deletes."`

	Apps []string `yaml:"apps,omitempty" json:"apps,omitempty" desc:"Apple IDs of the apps this agent may touch, in the team its own key is in. Required while enabled."`

	TeamResources bool `yaml:"team_resources,omitempty" json:"team_resources,omitempty" desc:"Optional. Let this agent reach the team's resources no app owns (users, devices, certificates, profiles, sandbox testers, reports across apps), within its level."`
}

// AppStoreConnectAppPattern is an app's Apple ID, its digits; the seat
// picker offers a typed app by it too.
const AppStoreConnectAppPattern = `^[0-9]{1,20}$`

// AppStoreConnect's own shapes.
var (
	ascDigits = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(AppStoreConnectAppPattern) })
	ascUUID   = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	})
)

// GrowthKind is the block's integration kind.
func (a *AppStoreConnect) GrowthKind() string { return "app_store_connect" }

// GrowthEnabled reports whether the block is on.
func (a *AppStoreConnect) GrowthEnabled() bool { return a != nil && a.Enabled }

// GrowthApprovers are the people its held acts are routed to first.
func (a *AppStoreConnect) GrowthApprovers() []string { return a.Approvers }

// GrowthSpend is nil: App Store Connect spends nothing.
func (a *AppStoreConnect) GrowthSpend() *GrowthSpend { return nil }

func (a *AppStoreConnect) validate(path Path) error {
	var p problems
	if !a.Enabled {
		return p.err()
	}
	if len(a.Connections) == 0 {
		p.add(at(path, "connections"), ErrMissing, "name at least one team: its issuer ID")
	}
	for _, id := range a.ConnectionIDs() {
		c := a.Connections[id]
		here := entry(at(path, "connections"), id)
		switch issuer := strings.TrimSpace(c.IssuerID); {
		case issuer == "":
			p.add(at(here, "issuer_id"), ErrMissing, "the team's issuer ID, on Users and Access, Integrations")
		case !envref.Has(issuer) && !ascUUID().MatchString(issuer):
			p.add(at(here, "issuer_id"), ErrUnknownValue, "%q is not an issuer ID: a UUID", c.IssuerID)
		}
	}
	checkGrowthConnections(&p, at(path, "connections"), a.ConnectionIDs())
	return p.err()
}

// checkAppStoreConnect holds a seat's level to the three and its apps to
// Apple IDs, each once.
func (c *Company) checkAppStoreConnect(p *problems, path Path, b *RoleAppStoreConnect) {
	if b == nil {
		return
	}
	if msg := growthLevelProblem(b.Level); msg != "" {
		p.add(at(path, "level"), ErrUnknownValue, "%s", msg)
	}
	seen := map[string]bool{}
	for i, app := range b.Apps {
		here := idx(at(path, "apps"), i)
		switch {
		case !ascDigits().MatchString(app):
			p.add(here, ErrUnknownValue, "%q is not an Apple ID: the app's digits", app)
		case seen[app]:
			p.add(here, ErrConflict, "%q is listed twice", app)
		}
		seen[app] = true
	}
}

// AppStoreConnectSeat is agent seat handle's App Store Connect block while it
// and the company's block are on, nil otherwise.
func (c *Company) AppStoreConnectSeat(handle string) *RoleAppStoreConnect {
	if c == nil || !c.Integrations.AppStoreConnect.GrowthEnabled() {
		return nil
	}
	for role := range c.EachRole() {
		if role.Kind == org.KindHuman || role.IdentityKey() != handle {
			continue
		}
		if b := role.Integrations.AppStoreConnect; b != nil && b.Enabled != nil && *b.Enabled {
			return b
		}
		return nil
	}
	return nil
}

// GrowthKind is the seat block's integration kind, the company block's.
func (*RoleAppStoreConnect) GrowthKind() string { return (*AppStoreConnect)(nil).GrowthKind() }

// SeatAccess is the block as seat access reads it.
func (r *RoleAppStoreConnect) SeatAccess() GrowthSeatAccess {
	return GrowthSeatAccess{On: r.Enabled, Level: string(growth.LevelOf(r.Level)), Scope: r.Apps}
}

// SetSeatAccess writes the block from seat access; team_resources is kept.
func (r *RoleAppStoreConnect) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Apps = a.On, a.Level, a.Scope
}

func (r *RoleAppStoreConnect) seatScopeKey() string { return "apps" }
func (r *RoleAppStoreConnect) seatEnabled() **bool  { return &r.Enabled }

// check holds the block to the company's reach.
func (r *RoleAppStoreConnect) check(c *Company, p *problems, here Path) {
	c.checkAppStoreConnect(p, here, r)
}

// GrowthSeatLevel is the seat's level while its block is on, "" while off.
func (r *RoleAppStoreConnect) GrowthSeatLevel() string {
	if r == nil || r.Enabled == nil || !*r.Enabled {
		return ""
	}
	return string(growth.LevelOf(r.Level))
}

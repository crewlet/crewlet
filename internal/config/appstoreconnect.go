package config

import (
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// AppStoreConnect connects the company's App Store Connect teams: the engine
// holds each team's API keys, one per Apple role, and makes every call for
// the agents, through the least-privileged key that can make it (ADR-0170).
type AppStoreConnect struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Approvers route the integration's held acts; they never decide who may
	// answer (an administrator, ADR-0171).
	Approvers []string `yaml:"approvers,omitempty" json:"approvers,omitempty" desc:"Optional. Handles of human seats asked first about a reply, a submission, a price or an account change an agent asks for. Only an administrator may answer; without this list every administrator is asked."`

	// Apps narrows the apps agents may be given; absent is every app the keys reach.
	Apps []string `yaml:"apps,omitempty" json:"apps,omitempty" desc:"Optional. Apple IDs (digits) of the apps agents may be given. Absent: every app the connections' keys reach."`

	// Connections are the teams, by connection id.
	Connections map[string]AppStoreConnectTeam `yaml:"connections,omitempty" json:"connections,omitempty" desc:"The App Store Connect teams the engine reaches, each under its connection id (lowercase letters, digits and hyphens, starting with a letter). Required when enabled."`
}

// AppStoreConnectTeam is one team's issuer and its keys.
type AppStoreConnectTeam struct {
	IssuerID string `yaml:"issuer_id,omitempty" json:"issuer_id,omitempty" desc:"The team's issuer ID, a UUID shown on Users and Access, Integrations, App Store Connect API."`

	VendorNumber string `yaml:"vendor_number,omitempty" json:"vendor_number,omitempty" desc:"Optional. The team's vendor number (digits, on Payments and Financial Reports), which sales and finance reports need."`

	Keys AppStoreConnectKeys `yaml:"keys,omitempty" json:"keys,omitempty" desc:"The team's API keys, one per Apple role, each optional; at least one. The engine calls through the least-privileged key that can make each call and never falls back to another when one is refused."`
}

// ConnectionIDs are the block's connection ids, sorted: the order a call
// tries the teams in.
func (a *AppStoreConnect) ConnectionIDs() []string {
	if a == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(a.Connections))
}

// AppStoreConnectKeys is a team's keys by Apple role: each slot holds a team
// key created with that role. A struct of named slots, so a slot this engine
// does not know is a decode error naming the slots.
type AppStoreConnectKeys struct {
	Admin           *AppStoreConnectKey `yaml:"admin,omitempty" json:"admin,omitempty" desc:"Optional. A key with the Admin role: users, keys, agreements, and every account change, each held for an administrator."`
	AppManager      *AppStoreConnectKey `yaml:"app_manager,omitempty" json:"app_manager,omitempty" desc:"Optional. A key with the App Manager role: versions, submissions, prices, TestFlight external testing, in-app purchases."`
	Developer       *AppStoreConnectKey `yaml:"developer,omitempty" json:"developer,omitempty" desc:"Optional. A key with the Developer role (with access to certificates, identifiers and profiles where given): builds, certificates, devices, bundle IDs and profiles."`
	Marketing       *AppStoreConnectKey `yaml:"marketing,omitempty" json:"marketing,omitempty" desc:"Optional. A key with the Marketing role: listings, reviews, custom product pages, in-app events, internal TestFlight groups."`
	Finance         *AppStoreConnectKey `yaml:"finance,omitempty" json:"finance,omitempty" desc:"Optional. A key with the Finance role: sales, finance and analytics reports."`
	Sales           *AppStoreConnectKey `yaml:"sales,omitempty" json:"sales,omitempty" desc:"Optional. A key with the Sales role: sales and analytics reports."`
	CustomerSupport *AppStoreConnectKey `yaml:"customer_support,omitempty" json:"customer_support,omitempty" desc:"Optional. A key with the Customer Support role: replies to customer reviews."`
	AccessToReports *AppStoreConnectKey `yaml:"access_to_reports,omitempty" json:"access_to_reports,omitempty" desc:"Optional. A key with the Access to Reports role: sales and trends reports."`
}

// AppStoreConnectKey is one team key: its ID and its .p8 private key.
type AppStoreConnectKey struct {
	KeyID      string `yaml:"key_id,omitempty" json:"key_id,omitempty" desc:"The key's ID: ten capital letters and digits, shown beside the key in App Store Connect."`
	PrivateKey string `secret:"true" yaml:"private_key,omitempty" json:"private_key,omitempty" desc:"Reference to the sealed .p8 private key App Store Connect gave when the key was created, from BEGIN PRIVATE KEY to END PRIVATE KEY. Apple keeps no copy."`
}

// AppStoreConnectSlot is a key slot by its document name.
type AppStoreConnectSlot struct {
	Name string
	Key  *AppStoreConnectKey
}

// Slots is every filled slot of k, by document name, in declaration order.
func (k AppStoreConnectKeys) Slots() []AppStoreConnectSlot {
	var out []AppStoreConnectSlot
	v, t := reflect.ValueOf(k), reflect.TypeOf(k)
	for i := range t.NumField() {
		if key, _ := v.Field(i).Interface().(*AppStoreConnectKey); key != nil {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
			out = append(out, AppStoreConnectSlot{Name: name, Key: key})
		}
	}
	return out
}

// AppStoreConnectSlots is every slot a team may fill, by document name.
func AppStoreConnectSlots() []string {
	t := reflect.TypeFor[AppStoreConnectKeys]()
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		out = append(out, name)
	}
	return out
}

// RoleAppStoreConnect is a seat's access to App Store Connect.
type RoleAppStoreConnect struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent may use App Store Connect through the engine's tools. No agent holds a key."`

	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): reads apps, listings, reviews, TestFlight, reports. operate: also edits drafts and internal testing, and asks an administrator for anything customers or external testers see. administer: also asks for account changes (users, keys, certificates, devices, profiles, agreements)."`

	Apps []string `yaml:"apps,omitempty" json:"apps,omitempty" desc:"Apple IDs of the apps this agent may touch. Required while enabled."`

	TeamResources bool `yaml:"team_resources,omitempty" json:"team_resources,omitempty" desc:"Optional. Let this agent reach the team's resources no app owns (users, devices, certificates, profiles, sandbox testers, reports across apps), within its level."`

	Identity string `yaml:"identity,omitempty" json:"identity,omitempty" js:"enum=own|company" desc:"Optional. own: calls go as this agent's own team key (a person makes it at Apple with the role its level needs and hands in its file), which Apple records under it and Crewlet checks before it keeps it and on every pass; until one is kept the agent reads Needs you. company (the default): calls go as the company's team keys."`
}

// AppStoreConnect's own shapes.
var (
	ascKeyID  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z0-9]{10}$`) })
	ascDigits = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9]{1,20}$`) })
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
	for i, app := range a.Apps {
		if !ascDigits().MatchString(app) {
			p.add(idx(at(path, "apps"), i), ErrUnknownValue, "%q is not an Apple ID: the app's digits", app)
		}
	}
	if !a.Enabled {
		return p.err()
	}
	if len(a.Connections) == 0 {
		p.add(at(path, "connections"), ErrMissing, "name at least one team: its issuer ID and a key")
	}
	keyIDs := map[string]string{}
	for _, id := range a.ConnectionIDs() {
		c := a.Connections[id]
		here := entry(at(path, "connections"), id)
		switch issuer := strings.TrimSpace(c.IssuerID); {
		case issuer == "":
			p.add(at(here, "issuer_id"), ErrMissing, "the team's issuer ID, on Users and Access, Integrations")
		case !envref.Has(issuer) && !ascUUID().MatchString(issuer):
			p.add(at(here, "issuer_id"), ErrUnknownValue, "%q is not an issuer ID: a UUID", c.IssuerID)
		}
		if v := strings.TrimSpace(c.VendorNumber); v != "" && !envref.Has(v) && !ascDigits().MatchString(v) {
			p.add(at(here, "vendor_number"), ErrUnknownValue, "%q is not a vendor number: digits", c.VendorNumber)
		}
		slots := c.Keys.Slots()
		if len(slots) == 0 {
			p.add(at(here, "keys"), ErrMissing, "add at least one key: %s", words.Or(AppStoreConnectSlots()))
		}
		for _, s := range slots {
			at := at(here, "keys."+s.Name)
			switch id := strings.TrimSpace(s.Key.KeyID); {
			case id == "":
				p.add(atField(at, "key_id"), ErrMissing, "the key's ID, shown beside it in App Store Connect")
			case !envref.Has(id) && !ascKeyID().MatchString(id):
				p.add(atField(at, "key_id"), ErrUnknownValue, "%q is not a key ID: ten capital letters and digits", s.Key.KeyID)
			case keyIDs[id] != "":
				p.add(atField(at, "key_id"), ErrConflict, "key %s is also the %s key: each slot holds its own key", id,
					keyIDs[id])
			default:
				keyIDs[id] = s.Name
			}
			if _, whole := envref.Whole(s.Key.PrivateKey); !whole {
				p.add(atField(at, "private_key"), ErrUnknownValue, "a ${VAR} naming the sealed .p8 key, never the key itself")
			}
		}
	}
	checkGrowthConnections(&p, at(path, "connections"), a.ConnectionIDs())
	return p.err()
}

// checkAppStoreConnect holds a seat's level to the three and its apps to the
// company's reach, each once.
func (c *Company) checkAppStoreConnect(p *problems, path Path, b *RoleAppStoreConnect) {
	if b == nil {
		return
	}
	if msg := growthLevelProblem(b.Level); msg != "" {
		p.add(at(path, "level"), ErrUnknownValue, "%s", msg)
	}
	reach := c.Integrations.AppStoreConnect
	seen := map[string]bool{}
	for i, app := range b.Apps {
		here := idx(at(path, "apps"), i)
		switch {
		case !ascDigits().MatchString(app):
			p.add(here, ErrUnknownValue, "%q is not an Apple ID: the app's digits", app)
		case seen[app]:
			p.add(here, ErrConflict, "%q is listed twice", app)
		case reach != nil && len(reach.Apps) > 0 && !slices.Contains(reach.Apps, app):
			p.add(here, ErrUnknownValue, "%q is not among integrations.app_store_connect.apps", app)
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
	return GrowthSeatAccess{On: r.Enabled, Level: string(growth.LevelOf(r.Level)), Scope: r.Apps,
		Identity: r.Identity}
}

// SetSeatAccess writes the block from seat access; team_resources is kept.
func (r *RoleAppStoreConnect) SetSeatAccess(a GrowthSeatAccess) {
	r.Enabled, r.Level, r.Apps, r.Identity = a.On, a.Level, a.Scope, a.Identity
}

func (r *RoleAppStoreConnect) seatIdentity() *string { return &r.Identity }

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

package cloudflare

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/words"
)

// What each seat level grants, by Cloudflare's own permission group names.
// Names are resolved to IDs from the account's live catalogue on every pass,
// never hardcoded, because Cloudflare treats the name as cosmetic.

// permission is one Cloudflare permission group: its API name, its name in the
// token builder (from the Dashboard tab of Cloudflare's permissions reference,
// which renames some), and whether the builder lists it under the zones.
type permission struct {
	name    string
	builder string
	zone    bool
}

// The permission groups more than one list here names.
//
//nolint:gochecknoglobals // immutable descriptions, not state
var (
	zoneRead           = permission{"Zone Read", "Zone: Read", true}
	accountSettings    = permission{"Account Settings Read", "Account Settings: Read", false}
	accountTokensWrite = permission{"Account API Tokens Write", "Account API Tokens: Edit", false}
)

// level is one seat level: the ID a document names, how a person reads it,
// the groups it grants on top of every level before it, and whether it is an
// administration grant ([seataccess.Level.Administers]).
type level struct {
	id, name, note string
	administers    bool
	adds           []permission
}

// levels is every seat level, least access first, each granting everything
// the levels before it do. DNS and SSL certificate writes are in none of them.
//
//nolint:gochecknoglobals // an immutable table, not state
var levels = []level{
	{
		id: config.CloudflareLevelRead, name: "Read",
		note: "Reads zones, zone settings, DNS, cache rules, transform rules, firewall and WAF rules, IP lists, " +
			"SSL and analytics.",
		// Account Settings Read is what Cloudflare's MCP server needs to find
		// the account at all. Firewall Services covers the legacy firewall
		// only: WAF custom rules are rulesets, read with Zone WAF, and the IP
		// lists they match on are the account's filter lists.
		adds: []permission{
			zoneRead,
			{"Zone Settings Read", "Zone Settings: Read", true},
			{"Analytics Read", "Analytics: Read", true},
			{"DNS Read", "DNS: Read", true},
			{"Cache Settings Read", "Cache Rules: Read", true},
			{"Zone Transform Rules Read", "Transform Rules: Read", true},
			{"Firewall Services Read", "Firewall Services: Read", true},
			{"Zone WAF Read", "Zone WAF: Read", true},
			{"SSL and Certificates Read", "SSL and Certificates: Read", true},
			{"Account Analytics Read", "Account Analytics: Read", false},
			{"Account Rule Lists Read", "Account Filter Lists: Read", false},
			accountSettings,
		},
	},
	{
		id: config.CloudflareLevelOperate, name: "Operate",
		note: "Everything Read does, plus purging the cache and editing cache rules.",
		adds: []permission{
			{"Cache Purge", "Cache Purge: Purge", true},
			{"Cache Settings Write", "Cache Rules: Edit", true},
		},
	},
	{
		id: config.CloudflareLevelConfigure, name: "Configure",
		note: "Everything Operate does, plus editing transform rules, compression rules and every zone " +
			"setting, the SSL mode and security level included.",
		// Cloudflare grants every zone setting as one group, the cache, tiered
		// cache, protocol and SSL mode settings alike, and every transform rule
		// as one: URL rewrites and request and response header changes.
		adds: []permission{
			{"Zone Settings Write", "Zone Settings: Edit", true},
			{"Response Compression Write", "Response Compression: Edit", true},
			{"Zone Transform Rules Write", "Transform Rules: Edit", true},
		},
	},
	{
		id: config.CloudflareLevelSecure, name: "Secure", administers: true,
		note: "Everything Configure does, plus editing the zones' WAF rules and the account's IP lists. " +
			"It can leave access behind that outlives the agent's token: an allow or skip rule, or an " +
			"address added to an IP list.",
		// A level of its own, so that changing who reaches a zone is granted
		// knowingly, with a step-up, and never by widening Configure.
		adds: []permission{
			{"Zone WAF Write", "Zone WAF: Edit", true},
			{"Account Rule Lists Write", "Account Filter Lists: Edit", false},
		},
	},
}

// provisionerOwnPermissions is what the provisioner token needs for itself:
// to mint and manage tokens, to read the account and to list its zones.
//
//nolint:gochecknoglobals // an immutable list, not state
var provisionerOwnPermissions = []permission{accountTokensWrite, accountSettings, zoneRead}

// atlasPermission is one permission group the atlas reads more with, and the
// part of its read that needs it.
type atlasPermission struct {
	permission
	part, shows string
}

// atlasPermissions is what the provisioner token MAY hold for the atlas and
// never has to (ADR-0145): one read group per product the atlas draws beyond
// DNS and certificates. NONE IS A PROVISIONER PERMISSION: no level grants one,
// so no agent's token is ever minted with it, and a token without them is
// whole.
//
//nolint:gochecknoglobals // an immutable table, not state
var atlasPermissions = []atlasPermission{
	{permission{"Workers Scripts Read", "Workers Scripts: Read", false}, partWorkers, "Workers"},
	{permission{"Pages Read", "Cloudflare Pages: Read", false}, partPages, "Pages projects"},
	{permission{"Workers R2 Storage Read", "Workers R2 Storage: Read", false}, partR2, "R2 buckets"},
	{permission{"Cloudflare Tunnel Read", "Cloudflare Tunnel: Read", false}, partTunnels, "Tunnels"},
}

// AtlasGroups is every optional permission group the atlas reads more with,
// by name.
func AtlasGroups() []string {
	out := make([]string, 0, len(atlasPermissions))
	for _, p := range atlasPermissions {
		out = append(out, p.name)
	}
	return out
}

// atlasPermissionOf is the optional permission a part of the atlas's read
// needs.
func atlasPermissionOf(part string) atlasPermission {
	for _, p := range atlasPermissions {
		if p.part == part {
			return p
		}
	}
	return atlasPermission{}
}

func names(ps []permission) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.name)
	}
	return out
}

// Levels is every seat level, least access first.
func Levels() []string {
	out := make([]string, 0, len(levels))
	for _, l := range levels {
		out = append(out, l.id)
	}
	return out
}

// LevelName is a level as a person reads it.
func LevelName(id string) string {
	if i := slices.IndexFunc(levels, func(l level) bool { return l.id == id }); i >= 0 {
		return levels[i].name
	}
	return id
}

// LevelNote is the sentence saying what a level grants.
func LevelNote(id string) string {
	if i := slices.IndexFunc(levels, func(l level) bool { return l.id == id }); i >= 0 {
		return levels[i].note
	}
	return ""
}

// Groups is the permission group names a level grants, and false for a level
// this build does not offer.
func Groups(level string) ([]string, bool) {
	ps, ok := levelPermissions(level)
	return names(ps), ok
}

// levelPermissions is everything a level grants: its own groups and those of
// every level before it.
func levelPermissions(id string) ([]permission, bool) {
	var out []permission
	for _, l := range levels {
		out = append(out, l.adds...)
		if l.id == id {
			return out, true
		}
	}
	return nil, false
}

// ProvisionerGroups is every permission group the provisioner token has to
// hold, by name: its own, then every group a seat level grants, each once. A
// token is only trusted to grant what it holds itself.
func ProvisionerGroups() []string { return names(provisionerPermissions()) }

func provisionerPermissions() []permission {
	out := slices.Clone(provisionerOwnPermissions)
	for _, level := range Levels() {
		ps, _ := levelPermissions(level)
		for _, p := range ps {
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// DashboardName is a permission group as Cloudflare's token builder shows it
// ("Cache Purge: Purge"), a provisioner's or one of the atlas's optional
// ones, or the name itself for one this integration does not name.
func DashboardName(name string) string {
	for _, p := range provisionerPermissions() {
		if p.name == name {
			return p.builder
		}
	}
	for _, p := range atlasPermissions {
		if p.name == name {
			return p.builder
		}
	}
	return name
}

// dashboardList is names as the token builder shows them, for a finding's
// list.
func dashboardList(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, DashboardName(n))
	}
	return out
}

// dashboardNames is [dashboardList] for a sentence, unquoted.
func dashboardNames(names []string) []words.Said {
	out := make([]words.Said, 0, len(names))
	for _, n := range dashboardList(names) {
		out = append(out, words.Said(n))
	}
	return out
}

// provisionerGrants is the provisioner token's permissions as Cloudflare's
// token builder names them, the account's and the zones', for the steps a
// person follows in the builder.
func provisionerGrants() (account, zone []string) {
	for _, p := range provisionerPermissions() {
		if p.zone {
			zone = append(zone, p.builder)
		} else {
			account = append(account, p.builder)
		}
	}
	return account, zone
}

// Resource scopes a permission group applies at.
const (
	scopeAccount = "com.cloudflare.api.account"
	scopeZone    = "com.cloudflare.api.account.zone"
)

// grant is one level resolved against the account: the group IDs it grants
// at the account and at its zones, each sorted.
type grant struct {
	account []string
	zone    []string
}

// catalogue is the account's permission groups and zones as one pass read
// them, which every policy the pass writes or compares is built from.
type catalogue struct {
	accountID string
	groups    []PermissionGroup

	// zoneIDs is the company's zones resolved to IDs, sorted; nil scopes a
	// token to every zone in the account.
	zoneIDs []string

	// missingZones are the configured zones the account does not have.
	missingZones []string
}

// newCatalogue resolves the company's zone names against the account's zones.
func newCatalogue(accountID string, groups []PermissionGroup, zones []Zone, configured []string) catalogue {
	c := catalogue{accountID: accountID, groups: groups}
	if len(configured) == 0 {
		return c
	}
	byName := make(map[string]string, len(zones))
	for _, z := range zones {
		byName[config.CloudflareZoneKey(z.Name)] = z.ID
	}
	c.zoneIDs = []string{}
	for _, name := range configured {
		key := config.CloudflareZoneKey(name)
		if id, ok := byName[key]; ok {
			c.zoneIDs = append(c.zoneIDs, id)
			continue
		}
		c.missingZones = append(c.missingZones, key)
	}
	slices.Sort(c.zoneIDs)
	c.zoneIDs = slices.Compact(c.zoneIDs)
	return c
}

// missingGroups is every name in names that no selectable group carries at
// the account or zone scope.
func (c catalogue) missingGroups(names []string) []string {
	var out []string
	for _, name := range names {
		if len(c.idsNamed(name, scopeAccount))+len(c.idsNamed(name, scopeZone)) == 0 {
			out = append(out, name)
		}
	}
	return out
}

// idsNamed is the selectable groups with exactly this name that apply at
// scope.
func (c catalogue) idsNamed(name, scope string) []string {
	var out []string
	for _, g := range c.groups {
		if g.Selectable && g.Name == name && slices.Contains(g.Scopes, scope) {
			out = append(out, g.ID)
		}
	}
	return out
}

// grantFor resolves a level, or says which of its groups the account lacks.
func (c catalogue) grantFor(level string) (grant, error) {
	names, ok := Groups(level)
	if !ok {
		return grant{}, fmt.Errorf("%q is not a Cloudflare level; give %s", level, words.Or(Levels()))
	}
	if missing := c.missingGroups(names); len(missing) > 0 {
		return grant{}, &missingGroupsError{level: level, names: missing}
	}
	var g grant
	for _, name := range names {
		g.account = append(g.account, c.idsNamed(name, scopeAccount)...)
		g.zone = append(g.zone, c.idsNamed(name, scopeZone)...)
	}
	slices.Sort(g.account)
	slices.Sort(g.zone)
	g.account, g.zone = slices.Compact(g.account), slices.Compact(g.zone)
	return g, nil
}

// missingGroupsError is a level naming groups the account's catalogue lacks.
type missingGroupsError struct {
	level string
	names []string
}

func (e *missingGroupsError) Error() string {
	return fmt.Sprintf("Cloudflare lists no permission group a token can be granted named %s, "+
		"which the %s level grants", words.And(e.names), LevelName(e.level))
}

// policies is a level's grant as a token's policy: one statement for the
// account and one for its zones. A configured zone the account lacks is left
// out rather than guessed at, and reported by the reconcile.
func (c catalogue) policies(g grant) []Policy {
	var out []Policy
	if len(g.account) > 0 {
		out = append(out, Policy{
			Effect: "allow", PermissionGroups: refs(g.account),
			Resources: map[string]any{scopeAccount + "." + c.accountID: "*"},
		})
	}
	if len(g.zone) > 0 && (c.zoneIDs == nil || len(c.zoneIDs) > 0) {
		out = append(out, Policy{Effect: "allow", PermissionGroups: refs(g.zone), Resources: c.zoneResources()})
	}
	return out
}

// zoneResources is every zone a token reaches: the company's, or every zone
// in the account.
func (c catalogue) zoneResources() map[string]any {
	if c.zoneIDs == nil {
		return map[string]any{scopeAccount + "." + c.accountID: map[string]any{scopeZone + ".*": "*"}}
	}
	out := make(map[string]any, len(c.zoneIDs))
	for _, id := range c.zoneIDs {
		out[scopeZone+"."+id] = "*"
	}
	return out
}

func refs(ids []string) []GroupRef {
	out := make([]GroupRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, GroupRef{ID: id})
	}
	return out
}

// levelOf is the level whose policy a token holds exactly, or "" where it
// holds none of them: a level's grant that does not resolve matches nothing.
func (c catalogue) levelOf(held []Policy) string {
	have := policyKeys(held)
	for _, level := range Levels() {
		g, err := c.grantFor(level)
		if err != nil {
			continue
		}
		if slices.Equal(have, policyKeys(c.policies(g))) {
			return level
		}
	}
	return ""
}

// policyKeys is a policy as a sorted set of comparable statements: the effect,
// the sorted group IDs and the resources in canonical JSON.
func policyKeys(policies []Policy) []string {
	out := make([]string, 0, len(policies))
	for _, p := range policies {
		ids := make([]string, 0, len(p.PermissionGroups))
		for _, g := range p.PermissionGroups {
			ids = append(ids, g.ID)
		}
		slices.Sort(ids)
		// json.Marshal sorts map keys at every depth, which makes this canonical.
		resources, _ := json.Marshal(p.Resources)
		out = append(out, strings.ToLower(p.Effect)+"|"+strings.Join(ids, ",")+"|"+string(resources))
	}
	slices.Sort(out)
	return out
}

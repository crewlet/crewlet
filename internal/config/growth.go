package config

import (
	"fmt"
	"maps"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/spendguard"
	"github.com/crewlet/crewlet/internal/words"
)

// The rules every growth integration's block shares (internal/growth; ADR-0170,
// ADR-0171). A block joins them by implementing growthBlock: they find it by
// reflection over [Integrations], so a product adds no row here.

// growthBlock is a growth integration's company block.
type growthBlock interface {
	// GrowthKind is the block's integration kind.
	GrowthKind() string
	// GrowthEnabled reports whether the block is on.
	GrowthEnabled() bool
	// GrowthApprovers are the human seats its held acts are routed to first.
	GrowthApprovers() []string
	// GrowthSpend is the block's spending caps, nil for a product that never
	// spends.
	GrowthSpend() *GrowthSpend
}

// growthSeat is a growth integration's seat block.
type growthSeat interface {
	// GrowthKind is the block's integration kind, which its catalog and
	// internal/authz's grants name.
	GrowthKind() string
	// GrowthSeatLevel is the seat's level where its block is on, and ""
	// where it is off.
	GrowthSeatLevel() string
	// SeatAccess is the block as seat access reads it; SetSeatAccess writes it.
	SeatAccess() GrowthSeatAccess
	SetSeatAccess(GrowthSeatAccess)
	// seatScopeKey is the key its scope is written under.
	seatScopeKey() string
	// seatEnabled is where its `enabled` is kept.
	seatEnabled() **bool
	// check holds the block to the rest of c, at here.
	check(c *Company, p *problems, here Path)
}

// growthSeatKeys is every growth seat block's key under
// `roles[].integrations`, in the order the blocks are declared.
var growthSeatKeys = sync.OnceValue(func() []string {
	var out []string
	seat := reflect.TypeFor[growthSeat]()
	for f := range reflect.TypeFor[RoleIntegrations]().Fields() {
		if f.IsExported() && f.Type.Implements(seat) {
			out = append(out, jsonName(f))
		}
	}
	return out
})

// isGrowthSeat reports whether lane is a growth product's seat block.
func isGrowthSeat(lane string) bool { return slices.Contains(growthSeatKeys(), lane) }

// growthBlockKeys is every growth block's key under `integrations`, each its
// product's kind.
var growthBlockKeys = sync.OnceValue(func() []string {
	var out []string
	block := reflect.TypeFor[growthBlock]()
	for f := range reflect.TypeFor[Integrations]().Fields() {
		if f.IsExported() && f.Type.Implements(block) {
			out = append(out, jsonName(f))
		}
	}
	return out
})

// holdsGrowthSeat reports whether r holds any growth seat block.
func (r RoleIntegrations) holdsGrowthSeat() bool {
	v := reflect.ValueOf(r)
	seat := reflect.TypeFor[growthSeat]()
	for n := range v.NumField() {
		if f := v.Type().Field(n); f.IsExported() && f.Type.Implements(seat) && !v.Field(n).IsNil() {
			return true
		}
	}
	return false
}

// GrowthSeatAccess is a growth seat block as internal/seataccess reads and
// writes every lane: on or off, its level (the default where it names none)
// and the accounts, properties or apps it names.
type GrowthSeatAccess struct {
	On    *bool
	Level string
	// Role is the product role the level runs at where the product offers
	// a choice (Google Analytics' operate as Marketer); "" for the level's own.
	Role  string
	Scope []string

	// Identity is who signs the seat's calls ([SeatIdentityOwn] or
	// [SeatIdentityCompany]), "" for the lane's default; only a block whose
	// vendor offers a seat its own identity keeps it ([identitySeat]).
	Identity string
}

// The values of a growth seat block's `identity`: who the vendor sees when the
// engine calls it for the seat (internal/seataccess.IdentityMode).
const (
	SeatIdentityOwn     = "own"
	SeatIdentityCompany = "company"
)

// identitySeat is a growth seat block whose vendor offers each seat an
// identity of its own beside the company's, so the block says which signs.
type identitySeat interface {
	growthSeat
	// seatIdentity is where its `identity` is kept.
	seatIdentity() *string
}

// checkSeatIdentity holds a seat block's `identity` to the two values.
func checkSeatIdentity(p *problems, here Path, s growthSeat) {
	is, ok := s.(identitySeat)
	if !ok {
		return
	}
	switch v := *is.seatIdentity(); v {
	case "", SeatIdentityOwn, SeatIdentityCompany:
	default:
		p.add(at(here, "identity"), ErrUnknownValue, "%q is not an identity; give %s", v,
			words.Or([]string{SeatIdentityOwn, SeatIdentityCompany}))
	}
}

// GrowthSeatAccess is the role's seat block for growth product kind, false
// where the role has none.
func (r *Role) GrowthSeatAccess(kind string) (GrowthSeatAccess, bool) {
	s := r.growthSeatOf(kind)
	if s == nil {
		return GrowthSeatAccess{}, false
	}
	return s.SeatAccess(), true
}

// SetGrowthSeatAccess writes the role's seat block for growth product kind,
// making it where absent; false where kind names no growth seat block.
func (r *Role) SetGrowthSeatAccess(kind string, a GrowthSeatAccess) bool {
	f, ok := r.growthSeatField(kind)
	if !ok {
		return false
	}
	if f.IsNil() {
		f.Set(reflect.New(f.Type().Elem()))
	}
	f.Interface().(growthSeat).SetSeatAccess(a)
	return true
}

// growthSeatOf is the role's seat block for growth product kind, nil where
// it holds none.
func (r *Role) growthSeatOf(kind string) growthSeat {
	f, ok := r.growthSeatField(kind)
	if !ok || f.IsNil() {
		return nil
	}
	return f.Interface().(growthSeat)
}

// growthSeatField is the field of the role's integrations holding kind's
// growth seat block, found by its key, so a product adds no row here.
func (r *Role) growthSeatField(kind string) (reflect.Value, bool) {
	v := reflect.ValueOf(&r.Integrations).Elem()
	seat := reflect.TypeFor[growthSeat]()
	for n := range v.NumField() {
		field := v.Type().Field(n)
		if field.IsExported() && jsonName(field) == kind && field.Type.Implements(seat) {
			return v.Field(n), true
		}
	}
	return reflect.Value{}, false
}

// partialGrowthSeats is every field of blocks, a struct of seat blocks, whose
// block has a GrowthSeatLevel but not the rest of [growthSeat]: a block the
// walks would pass over, so its seats would silently carry no grant.
func partialGrowthSeats(blocks reflect.Type) []string {
	var out []string
	seat := reflect.TypeFor[growthSeat]()
	for f := range blocks.Fields() {
		if _, level := f.Type.MethodByName("GrowthSeatLevel"); level && !f.Type.Implements(seat) {
			out = append(out, jsonName(f))
		}
	}
	return out
}

// GrowthLevels is the seat's level on each growth lane it is on at, by the
// lane's kind: what internal/authz's growth grants admit to that product's
// tools (org.Role.GrowthLevels). Found by reflection over the seat's blocks,
// so a product adds no row here.
func (r *Role) GrowthLevels() map[string]string { return growthLevelsIn(&r.Integrations) }

// growthLevelsIn is [Role.GrowthLevels] over blocks, a pointer to a struct of
// seat blocks. KEYED BY THE BLOCK'S KIND, never its field's name: the grants
// name the kind, and a key spelled otherwise would grant nothing, silently.
func growthLevelsIn(blocks any) map[string]string {
	out := map[string]string{}
	v := reflect.ValueOf(blocks).Elem()
	for n := range v.NumField() {
		f := v.Field(n)
		if f.Kind() != reflect.Pointer || f.IsNil() || !v.Type().Field(n).IsExported() {
			continue
		}
		if s, ok := f.Interface().(growthSeat); ok {
			if level := s.GrowthSeatLevel(); level != "" {
				out[s.GrowthKind()] = level
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// growthAt is one growth block and where the document holds it.
type growthAt struct {
	path  Path
	block growthBlock
}

// growthBlocks is every growth block the document holds, on or off.
func (i *Integrations) growthBlocks() []growthAt {
	if i == nil {
		return nil
	}
	var out []growthAt
	v := reflect.ValueOf(i).Elem()
	for n := range v.NumField() {
		f := v.Field(n)
		if f.Kind() != reflect.Pointer || f.IsNil() || !v.Type().Field(n).IsExported() {
			continue
		}
		if b, ok := f.Interface().(growthBlock); ok {
			out = append(out, growthAt{path: at(field("integrations"), jsonName(v.Type().Field(n))), block: b})
		}
	}
	return out
}

// growthBlock is kind's block, nil where the document holds none.
func (i *Integrations) growthBlock(kind string) growthBlock {
	for _, b := range i.growthBlocks() {
		if b.block.GrowthKind() == kind {
			return b.block
		}
	}
	return nil
}

// GrowthEnabled reports whether kind's growth block is present and on.
func (i *Integrations) GrowthEnabled(kind string) bool {
	b := i.growthBlock(kind)
	return b != nil && b.GrowthEnabled()
}

// GrowthApprovers are the people kind's held acts are routed to first: who
// sees and is told of one before the other administrators. Never who may
// answer, which is an administrator alone (ADR-0171).
func (i *Integrations) GrowthApprovers(kind string) []string {
	if b := i.growthBlock(kind); b != nil {
		return slices.Clone(b.GrowthApprovers())
	}
	return nil
}

// GrowthCaps is the spend guard's caps for account of kind, false where the
// block names none for it: every spend call on it is then refused.
func (i *Integrations) GrowthCaps(kind, account string) (spendguard.Caps, bool) {
	b := i.growthBlock(kind)
	if b == nil || b.GrowthSpend() == nil {
		return spendguard.Caps{}, false
	}
	for _, c := range b.GrowthSpend().Accounts {
		if c.Account == account {
			return c.caps(), true
		}
	}
	return spendguard.Caps{}, false
}

// GrowthSpend is a spending product's caps, per ad account, each in the
// account's own currency.
type GrowthSpend struct {
	Accounts []GrowthCap `yaml:"accounts,omitempty" json:"accounts,omitempty" desc:"The spending caps of each ad account agents may ask to spend on. An account without both caps refuses every spend call; reads, operate calls and pausing still work."`
}

// GrowthCap is one ad account's two caps. Every key is optional; the guard
// refuses spend on an account missing either cap (internal/spendguard).
type GrowthCap struct {
	Account   string `yaml:"account" json:"account" desc:"The ad account's ID, as the vendor writes it."`
	Currency  string `yaml:"currency,omitempty" json:"currency,omitempty" desc:"The account's currency as an ISO 4217 code (EUR). It must equal the account's own currency, read live, or every spend call on the account is refused."`
	PerChange string `yaml:"per_change,omitempty" json:"per_change,omitempty" desc:"The most one approved change may add to the account's daily capacity, a decimal amount in the account's currency (50.00)."`
	Daily     string `yaml:"daily,omitempty" json:"daily,omitempty" desc:"The ceiling on the account's daily capacity after any approved change, a decimal amount in the account's currency (200.00). It limits the budgets agents may set, not what the vendor delivers on one day."`
}

// caps is the cap as the guard reads it; an amount that does not parse is
// unset, which refuses spend, and validation names it.
func (c GrowthCap) caps() spendguard.Caps {
	out := spendguard.Caps{Currency: strings.TrimSpace(c.Currency)}
	if m, err := growth.ParseAmount(c.PerChange); err == nil && strings.TrimSpace(c.PerChange) != "" {
		out.PerChangeMicros = &m
	}
	if m, err := growth.ParseAmount(c.Daily); err == nil && strings.TrimSpace(c.Daily) != "" {
		out.DailyMicros = &m
	}
	return out
}

// growthMaxCapAccounts bounds a block's capped accounts: a company spending on
// more ad accounts than this is an agency, which caps per client company.
const growthMaxCapAccounts = 200

func (s *GrowthSpend) validate(p *problems, here Path) {
	if s == nil {
		return
	}
	if n := len(s.Accounts); n > growthMaxCapAccounts {
		p.add(at(here, "accounts"), ErrOutOfRange, "%d accounts is more than %d", n, growthMaxCapAccounts)
	}
	seen := map[string]bool{}
	for i, c := range s.Accounts {
		at := idx(at(here, "accounts"), i)
		account := strings.TrimSpace(c.Account)
		switch {
		case account == "":
			p.add(atField(at, "account"), ErrMissing, "name the ad account these caps bound")
		case seen[account]:
			p.add(atField(at, "account"), ErrConflict, "%q has caps twice", account)
		}
		seen[account] = true
		if cur := strings.TrimSpace(c.Currency); cur != "" && !growth.ValidCurrency(cur) {
			p.add(atField(at, "currency"), ErrUnknownValue, "%q is not an ISO 4217 code such as EUR", c.Currency)
		}
		var perChange, daily int64
		for _, a := range []struct {
			name, value string
			into        *int64
		}{{"per_change", c.PerChange, &perChange}, {"daily", c.Daily, &daily}} {
			if strings.TrimSpace(a.value) == "" {
				continue
			}
			m, err := growth.ParseAmount(a.value)
			if err != nil {
				p.add(atField(at, a.name), ErrUnknownValue, "%v", err)
				continue
			}
			*a.into = m
		}
		if perChange > 0 && daily > 0 && perChange > daily {
			p.add(atField(at, "per_change"), ErrOutOfRange,
				"%s is more than the daily cap %s: one change could never pass", c.PerChange, c.Daily)
		}
	}
}

// atField is a field below a path.
func atField(parent Path, name string) Path { return at(parent, name) }

// growthConnectionID is a connection's name in its block.
var growthConnectionID = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
})

// checkGrowthConnections holds a block's connection ids, the keys of its
// connections, to their shape: a block may hold several connections to its
// vendor (several teams, organizations or business portfolios), each under its
// id, a map key so a setup form's merge patch can address one.
func checkGrowthConnections(p *problems, here Path, ids []string) {
	for _, id := range ids {
		if !growthConnectionID().MatchString(id) {
			p.add(entry(here, id), ErrUnknownValue, "%q is not a connection id: lowercase letters, digits and "+
				"hyphens, starting with a letter, at most 32", id)
		}
	}
}

// validateGrowth holds every growth block to the framework's rules: its
// approvers are people, its caps are amounts, its credential variables are
// named at its own fields alone, and no vendor's own MCP server runs beside
// it.
func (c *Company) validateGrowth() error {
	var p problems
	humans := map[string]bool{}
	for role := range c.EachRole() {
		if role.Kind == org.KindHuman {
			humans[role.IdentityKey()] = true
		}
	}
	for _, b := range c.Integrations.growthBlocks() {
		seen := map[string]bool{}
		for i, handle := range b.block.GrowthApprovers() {
			here := idx(at(b.path, "approvers"), i)
			switch {
			case !humans[handle]:
				p.add(here, ErrUnknownValue, "%q is not a person's seat; only people are asked about "+
					"spend, publishing and account changes", handle)
			case seen[handle]:
				p.add(here, ErrConflict, "%q is listed twice", handle)
			}
			seen[handle] = true
		}
		b.block.GrowthSpend().validate(&p, at(b.path, "spend"))
	}
	c.checkGrowthCredentials(&p)
	c.checkRefusedServers(&p)
	return p.err()
}

// growthSecret is one ${VAR} a growth block's credential field names.
type growthSecret struct {
	path  Path
	share string
}

// collectGrowthSecrets gathers every variable a growth block's secret field
// names, with the fields that name it. A field tagged `growthshare:"<group>"`
// may name the same variable as another field of the same group: one OAuth
// client's secret, shared by the consent products that sign in with it.
func collectGrowthSecrets(v reflect.Value, here Path, secret bool, share string, out map[string][]growthSecret) {
	switch v.Kind() {
	case reflect.String:
		if secret {
			for _, name := range envref.Names(v.String()) {
				out[name] = append(out[name], growthSecret{path: here, share: share})
			}
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			collectGrowthSecrets(v.Elem(), here, secret, share, out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			collectGrowthSecrets(v.Field(i), at(here, jsonName(f)), secret || secrets.Field(f),
				cmpOr(f.Tag.Get("growthshare"), share), out)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			collectGrowthSecrets(v.Index(i), idx(here, i), secret, share, out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			collectGrowthSecrets(v.MapIndex(k), entry(here, k.String()), secret, share, out)
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// checkGrowthCredentials refuses a growth credential's variable named
// anywhere but its own field (ADR-0170): bound to a vendor's MCP server, a
// box's environment or another integration, it would reach the vendor around
// the caps, the decisions and the seat's scope. Every position the document
// has, now or later, is walked ([References]), so none is missed by a list.
func (c *Company) checkGrowthCredentials(p *problems) {
	checkGrowthSecrets(p, c.Integrations.growthBlocks(), c)
}

// checkGrowthSecrets is [Company.checkGrowthCredentials] over blocks, found in
// the document root.
func checkGrowthSecrets(p *problems, blocks []growthAt, root any) {
	owned := map[string][]growthSecret{}
	for _, b := range blocks {
		collectGrowthSecrets(reflect.ValueOf(b.block), b.path, false, "", owned)
	}
	if len(owned) == 0 {
		return
	}
	names := slices.Sorted(maps.Keys(owned))
	for _, name := range names {
		at := owned[name]
		for _, other := range at[1:] {
			if other.share == "" || other.share != at[0].share {
				p.add(other.path, ErrConflict, "${%s} is the growth credential of %s, which no other "+
					"field may name", name, at[0].path)
			}
		}
	}
	walkStrings(reflect.ValueOf(root), nil, func(here Path, s string) {
		for _, name := range envref.Names(s) {
			at, ok := owned[name]
			if !ok || slices.ContainsFunc(at, func(g growthSecret) bool { return g.path.String() == here.String() }) {
				continue
			}
			p.add(here, ErrConflict, "${%s} is the growth credential of %s, which only that field may "+
				"name: agents reach the vendor through the engine's tools alone", name, at[0].path)
		}
	})
}

// refusedServer is a vendor's own MCP server for a growth product, refused in
// mcp_servers while the product's block is on (ADR-0170): a second path to the
// same accounts around the caps, the decisions and the seat's scope.
type refusedServer struct {
	kind string
	// commands match a word of a local server's command or arguments.
	commands []string
	// addresses match a remote server's host and the start of its path.
	addresses []string
	name      string
}

// refusedServers is every vendor server refused beside its product.
var refusedServers = []refusedServer{
	{kind: "google_ads", commands: []string{"google-ads-mcp"}, name: "Google's Google Ads MCP server"},
	{kind: "google_analytics", commands: []string{"analytics-mcp", "google-analytics-mcp"},
		name: "Google's Analytics MCP server"},
	{kind: "meta_ads", addresses: []string{"mcp.facebook.com/ads"}, name: "Meta's Ads MCP server"},
}

// refused is the row l matches, false for none.
func refusedFor(l catalogue.Launch) (refusedServer, bool) {
	for _, r := range refusedServers {
		if l.Transport == catalogue.TransportStdio {
			for _, word := range append([]string{l.Command}, l.Args...) {
				if slices.Contains(r.commands, packageName(word)) {
					return r, true
				}
			}
		}
		if l.Transport == catalogue.TransportHTTP {
			for _, a := range r.addresses {
				host, prefix, _ := strings.Cut(a, "/")
				if catalogue.HostOf(l.URL) == host && strings.HasPrefix(urlPath(l.URL), "/"+prefix) {
					return r, true
				}
			}
		}
	}
	return refusedServer{}, false
}

// packageName is the package a launch word names, as its runner reads it:
// the path's last element, without a scope, a version pin, extras or a .git
// suffix, so `@vendor/analytics-mcp@1.2`, `analytics-mcp==0.3` and a git
// address of it all name analytics-mcp, and `my-analytics-mcp` does not.
func packageName(word string) string {
	name := strings.TrimSuffix(path.Base(strings.TrimSpace(word)), ".git")
	if i := strings.IndexAny(name, "@=<>~![ "); i > 0 {
		name = name[:i]
	}
	return name
}

func urlPath(raw string) string {
	_, rest, ok := strings.Cut(strings.TrimSpace(raw), "://")
	if !ok {
		return ""
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return "/"
}

// checkRefusedServers refuses a vendor's own MCP server while its growth
// product is on.
func (c *Company) checkRefusedServers(p *problems) {
	checkRefused(p, c.MCPServers, c.Integrations.GrowthEnabled)
}

// checkRefused refuses each server of servers whose product is enabled.
func checkRefused(p *problems, servers []MCPServer, enabled func(kind string) bool) {
	for i, m := range servers {
		r, ok := refusedFor(m.Launch())
		if !ok || !enabled(r.kind) {
			continue
		}
		p.add(idx(field("mcp_servers"), i), ErrConflict, "%s reaches the same accounts as integrations.%s "+
			"around its spending caps, decisions and seat scopes; agents use the integration's tools instead",
			r.name, r.kind)
	}
}

// growthLevelProblem is the refusal of a seat level no growth lane offers, ""
// for a level it does.
func growthLevelProblem(written string) string {
	if growth.LevelOf(written).Valid() {
		return ""
	}
	return fmt.Sprintf("%q is not a level this engine offers; give %s", written, words.Or(growth.LevelNames()))
}

package cloudflare

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Extra grants: one permission group on a seat's token beside its level,
// named as the account's live catalogue lists it, on some of the company's
// zones or the account. The token is rewritten in place to the level's groups
// and the live grants, so its value and its tool stay as they were.

// builderNames is the token builder's name for a group no level grants whose
// name there differs from the API's, from the Dashboard tab of Cloudflare's
// permissions reference. Any other group is shown by its API name.
//
//nolint:gochecknoglobals // an immutable table, not state
var builderNames = map[string]string{
	"Origin Read":  "Origin Rules: Read",
	"Origin Write": "Origin Rules: Edit",
}

// neverGranted is why no agent is ever granted a group beside its level, or
// "" where one may be: anything that mints or changes tokens, manages the
// account's members (Cloudflare files them under Account Settings Write),
// reads or changes billing, or administers Access's organization, identity
// providers, groups or service tokens, each of which outlives a token.
func neverGranted(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "api token"):
		return "it manages API tokens, which would let an agent give itself anything"
	case strings.Contains(n, "member"):
		return "it manages the account's members"
	case name == "Account Settings Write":
		return "it changes the account's settings and its members"
	case strings.Contains(n, "billing"):
		return "it reaches the account's billing"
	case strings.HasPrefix(n, "access: organizations"), strings.HasPrefix(n, "access: identity providers"),
		strings.HasPrefix(n, "access: groups"), strings.HasPrefix(n, "access: service tokens"):
		return "it administers who can sign in through Cloudflare Access"
	}
	return ""
}

// writeGroup reports a group that may change anything: every group whose name
// does not mark it read-only, failing closed on one Cloudflare names otherwise.
func writeGroup(name string) bool { return !strings.HasSuffix(name, " Read") }

// zoneKind is a grant's scope entries on a zone group: zone names.
const zoneKind seataccess.GrantScopeKind = "zone"

// grantEntries is every group the account offers that a seat may be granted
// beside its level, each once by name: selectable, at the account or its
// zones, never one [neverGranted] names, and marked unavailable where the
// provisioner token does not hold it, since Crewlet grants nothing it does
// not hold itself.
func (c catalogue) grantEntries(own ownPolicy, tokensURL string) []seataccess.GrantEntry {
	held := map[string]bool{}
	for _, p := range own.policies {
		if strings.EqualFold(p.Effect, "allow") {
			for _, g := range p.PermissionGroups {
				held[g.ID] = true
			}
		}
	}
	seen := map[string]bool{}
	var out []seataccess.GrantEntry
	for _, g := range c.groups {
		if seen[g.Name] || neverGranted(g.Name) != "" {
			continue
		}
		account, zone := c.idsNamed(g.Name, scopeAccount), c.idsNamed(g.Name, scopeZone)
		if len(account)+len(zone) == 0 {
			continue
		}
		seen[g.Name] = true
		e := seataccess.GrantEntry{
			ID: g.Name, Name: DashboardName(g.Name), Group: "Account",
			ScopeKind: seataccess.GrantScopeAccount, Write: writeGroup(g.Name), InLevel: lowestLevelOf(g.Name),
		}
		if len(zone) > 0 {
			e.Group, e.ScopeKind = "Zone", zoneKind
		}
		switch {
		case !own.shown:
			e.Unavailable = "Cloudflare will not show the provisioner token its own permissions, so " +
				"Crewlet cannot tell it holds " + e.Name
		case !slices.ContainsFunc(slices.Concat(account, zone), func(id string) bool { return held[id] }):
			e.Unavailable = "The provisioner token does not hold " + e.Name
		}
		if e.Unavailable != "" {
			e.Missing, e.ActionURL = []string{e.Name}, tokensURL
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b seataccess.GrantEntry) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// lowestLevelOf is the first level that grants the group, "" for none.
func lowestLevelOf(name string) string {
	for _, l := range levels {
		if names, _ := Groups(l.id); slices.Contains(names, name) {
			return l.id
		}
	}
	return ""
}

// grantScope is the zones a grant may name: the company's, or every zone in
// the account where the company names none, by name and sorted.
func (c catalogue) grantScope() []string {
	out := []string{}
	for _, z := range c.zones {
		if c.zoneIDs == nil || slices.Contains(c.zoneIDs, z.ID) {
			out = append(out, config.CloudflareZoneKey(z.Name))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// resolvedGrant is one grant resolved against the account: its group's IDs at
// the account and at the zones, and the zones those apply to, nil being the
// company's.
type resolvedGrant struct {
	account, zone []string
	zones         []string
}

// resolveGrant resolves a grant, or says why it cannot be: a group no agent is
// granted, one the account does not offer, zones on an account group, or a
// zone outside the account or the company's zones.
func (c catalogue) resolveGrant(g seataccess.Grant) (resolvedGrant, error) {
	if why := neverGranted(g.Permission); why != "" {
		return resolvedGrant{}, fmt.Errorf("%s is never granted to an agent: %s", g.Permission, why)
	}
	r := resolvedGrant{account: c.idsNamed(g.Permission, scopeAccount), zone: c.idsNamed(g.Permission, scopeZone)}
	switch {
	case len(r.account)+len(r.zone) == 0:
		return resolvedGrant{}, fmt.Errorf("no permission group a token can be granted at Cloudflare is named %s",
			g.Permission)
	case len(r.zone) == 0 && len(g.Scope) > 0:
		return resolvedGrant{}, fmt.Errorf("%s is an account permission and takes no zones", g.Permission)
	}
	if len(g.Scope) == 0 {
		return r, nil
	}
	byName := make(map[string]string, len(c.zones))
	for _, z := range c.zones {
		byName[config.CloudflareZoneKey(z.Name)] = z.ID
	}
	for _, name := range g.Scope {
		id, ok := byName[config.CloudflareZoneKey(name)]
		switch {
		case !ok:
			return resolvedGrant{}, fmt.Errorf("the account has no zone %s", name)
		case c.zoneIDs != nil && !slices.Contains(c.zoneIDs, id):
			return resolvedGrant{}, fmt.Errorf("%s is not one of the company's zones", name)
		}
		r.zones = append(r.zones, id)
	}
	slices.Sort(r.zones)
	r.zones = slices.Compact(r.zones)
	return r, nil
}

// policiesWith is a level's grant and the seat's grants as a token's policy:
// one statement per set of resources, the account's first, then the company's
// zones, then each other set of zones, each statement's groups sorted, so one
// access is always one policy and a level with no grants is the policy it
// always was.
func (c catalogue) policiesWith(g grant, extra []resolvedGrant) []Policy {
	account := map[string]any{scopeAccount + "." + c.accountID: "*"}
	companyZones := c.zoneIDs == nil || len(c.zoneIDs) > 0
	type statement struct {
		resources map[string]any
		ids       []string
	}
	byKey := map[string]*statement{}
	add := func(resources map[string]any, ids []string) {
		if len(ids) == 0 {
			return
		}
		key := resourcesKey(resources)
		st, ok := byKey[key]
		if !ok {
			st = &statement{resources: resources}
			byKey[key] = st
		}
		st.ids = append(st.ids, ids...)
	}
	add(account, g.account)
	if companyZones {
		add(c.zoneResources(), g.zone)
	}
	for _, r := range extra {
		add(account, r.account)
		switch {
		case r.zones != nil:
			add(zoneResourcesOf(r.zones), r.zone)
		case companyZones:
			add(c.zoneResources(), r.zone)
		}
	}
	first := []string{resourcesKey(account), resourcesKey(c.zoneResources())}
	keys := slices.Collect(maps.Keys(byKey))
	slices.SortFunc(keys, func(a, b string) int {
		ia, ib := slices.Index(first, a), slices.Index(first, b)
		if ia < 0 {
			ia = len(first)
		}
		if ib < 0 {
			ib = len(first)
		}
		return cmp.Or(cmp.Compare(ia, ib), cmp.Compare(a, b))
	})
	out := make([]Policy, 0, len(keys))
	for _, key := range keys {
		st := byKey[key]
		ids := slices.Clone(st.ids)
		slices.Sort(ids)
		out = append(out, Policy{Effect: "allow", PermissionGroups: refs(slices.Compact(ids)), Resources: st.resources})
	}
	return out
}

// zoneResourcesOf is a statement's resources for these zones.
func zoneResourcesOf(ids []string) map[string]any {
	out := make(map[string]any, len(ids))
	for _, id := range ids {
		out[scopeZone+"."+id] = "*"
	}
	return out
}

// resourcesKey is a statement's resources as one comparable string;
// json.Marshal sorts map keys at every depth, which makes it canonical.
func resourcesKey(resources map[string]any) string {
	raw, _ := json.Marshal(resources)
	return string(raw)
}

// resolveGrants resolves every grant, or says which cannot be.
func (c catalogue) resolveGrants(grants []seataccess.Grant) ([]resolvedGrant, error) {
	out := make([]resolvedGrant, 0, len(grants))
	for _, g := range grants {
		r, err := c.resolveGrant(g)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// accessOf is a token's policy in a seat's terms. Where it is exactly the
// policy applied's level and grants make, it is those, so grants that
// overlap a higher level never read as that level and flap. Otherwise it is
// the highest level the policy holds whole and every other group it holds as
// a grant, which the plan restores (D2). A policy with any statement that is
// not an allow holds no level.
func (c catalogue) accessOf(held []Policy, applied *seataccess.Desired) (string, []seataccess.Grant) {
	if applied != nil {
		if g, err := c.grantFor(applied.Level); err == nil {
			live := plainGrants(applied.Grants)
			if extra, err := c.resolveGrants(live); err == nil &&
				slices.Equal(policyKeys(held), policyKeys(c.policiesWith(g, extra))) {
				return applied.Level, live
			}
		}
	}
	type pair struct{ id, resources string }
	have := map[pair]bool{}
	resourcesOf := map[string]map[string]any{}
	for _, p := range held {
		if !strings.EqualFold(p.Effect, "allow") {
			return "", nil
		}
		key := resourcesKey(p.Resources)
		resourcesOf[key] = p.Resources
		for _, g := range p.PermissionGroups {
			have[pair{g.ID, key}] = true
		}
	}
	accountKey, zonesKey := resourcesKey(map[string]any{scopeAccount + "." + c.accountID: "*"}),
		resourcesKey(c.zoneResources())
	companyZones := c.zoneIDs == nil || len(c.zoneIDs) > 0
	level := ""
	for i := len(levels) - 1; i >= 0 && level == ""; i-- {
		g, err := c.grantFor(levels[i].id)
		if err != nil {
			continue
		}
		var want []pair
		for _, id := range g.account {
			want = append(want, pair{id, accountKey})
		}
		if companyZones {
			for _, id := range g.zone {
				want = append(want, pair{id, zonesKey})
			}
		}
		if len(want) > 0 && !slices.ContainsFunc(want, func(p pair) bool { return !have[p] }) {
			level = levels[i].id
			for _, p := range want {
				delete(have, p)
			}
		}
	}
	byGrant := map[string]*seataccess.Grant{}
	for p := range have {
		name := c.nameOf(p.id)
		scope := []string(nil)
		if p.resources != accountKey && p.resources != zonesKey {
			scope = c.zoneNamesOf(resourcesOf[p.resources])
		}
		g := seataccess.Grant{Permission: name, Scope: scope}
		byGrant[seataccess.GrantKey(g)] = &g
	}
	grants := make([]seataccess.Grant, 0, len(byGrant))
	for _, g := range byGrant {
		grants = append(grants, *g)
	}
	return level, seataccess.NormalGrants(grants)
}

// plainGrants is grants as a credential holds them: permission and scope.
func plainGrants(grants []seataccess.Grant) []seataccess.Grant {
	out := make([]seataccess.Grant, 0, len(grants))
	for _, g := range seataccess.NormalGrants(grants) {
		out = append(out, seataccess.Grant{Permission: g.Permission, Scope: g.Scope})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// nameOf is a group's name by its ID, or the ID where the catalogue lacks it.
func (c catalogue) nameOf(id string) string {
	if i := slices.IndexFunc(c.groups, func(g PermissionGroup) bool { return g.ID == id }); i >= 0 {
		return c.groups[i].Name
	}
	return id
}

// zoneNamesOf is a statement's zones by name, each resource the account does
// not name kept as it is, so it never reads as a zone it is not.
func (c catalogue) zoneNamesOf(resources map[string]any) []string {
	var out []string
	for key := range resources {
		id, ok := strings.CutPrefix(key, scopeZone+".")
		if i := slices.IndexFunc(c.zones, func(z Zone) bool { return z.ID == id }); ok && i >= 0 {
			out = append(out, config.CloudflareZoneKey(c.zones[i].Name))
			continue
		}
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

// GrantCatalogue is every group a seat may be granted beside its level, and
// the zones a grant may name ([seataccess.Granter]), read once per session.
func (s *accessSession) GrantCatalogue(ctx context.Context, _ seataccess.Lane) (
	[]seataccess.GrantEntry, []string, error) {

	cat, err := s.catalogue(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.levelGaps(ctx); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	own := ownPolicy{}
	if s.own != nil {
		own = *s.own
	}
	s.mu.Unlock()
	return cat.grantEntries(own, s.client.TokensURL()), cat.grantScope(), nil
}

// grantSteps is an [seataccess.OpGrants] op as steps: one per grant added and
// one per grant taken off, each naming the group as the token builder does.
func grantSteps(op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
	opts seataccess.Options) []seataccess.Step {
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	var out []seataccess.Step
	for _, g := range seataccess.NormalGrants(want.Grants) {
		if slices.ContainsFunc(have.Grants, func(h seataccess.Grant) bool {
			return seataccess.GrantKey(h) == seataccess.GrantKey(g)
		}) {
			continue
		}
		until := "permanently"
		if !g.ExpiresAt.IsZero() {
			until = "until " + g.ExpiresAt.UTC().Format("2 Jan 15:04 UTC")
		}
		out = append(out, step(seataccess.VerbGrant,
			fmt.Sprintf("Grant its token %s on %s, %s", DashboardName(g.Permission), grantReach(g, opts), until)))
	}
	for _, g := range seataccess.NormalGrants(have.Grants) {
		if slices.ContainsFunc(want.Grants, func(w seataccess.Grant) bool {
			return seataccess.GrantKey(w) == seataccess.GrantKey(g)
		}) {
			continue
		}
		out = append(out, step(seataccess.VerbUngrant,
			fmt.Sprintf("Take %s on %s off its token", DashboardName(g.Permission), grantReach(g, opts))))
	}
	if len(out) == 0 {
		out = append(out, step(seataccess.VerbGrant, "Narrow its token to its level and grants"))
	}
	return out
}

// grantReach is where a grant applies, as a step says it: its zones, or its
// whole reach, named by its group where the catalogue was read.
func grantReach(g seataccess.Grant, opts seataccess.Options) string {
	if len(g.Scope) > 0 {
		return strings.Join(g.Scope, ", ")
	}
	entry, ok := opts.Grantable(g.Permission)
	switch {
	case ok && entry.ScopeKind == seataccess.GrantScopeAccount:
		return "the account"
	case ok:
		return "every zone it reaches"
	default:
		return "its whole reach"
	}
}

package scaleway

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// A seat's extra grant is one permission set in its own policy beside its
// level: a project set on the projects the grant names (the seat's own where it
// names none), an organization set on the Organization. Scaleway lists every
// set with its scope type, which is the catalogue; [neverGranted] is never
// offered, and the policy is rewritten whole, so a lapsed grant leaves it on
// the next pass with the key unchanged.

// projectKind is a grant's scope entries on a project set: project IDs.
const projectKind seataccess.GrantScopeKind = "project"

// grantWrite reports a set that may change anything: every one whose name
// Scaleway does not end in Read or ReadOnly, failing closed.
func grantWrite(name string) bool {
	return !strings.HasSuffix(name, "ReadOnly") && !strings.HasSuffix(name, "Read")
}

// lowestLevelOf is the first level that grants set, "" for none: every level
// holds the organization reads.
func lowestLevelOf(set string) string {
	if slices.Contains(orgSets, set) {
		return levels[0].id
	}
	for _, l := range levels {
		if sets, _ := ProjectSets(l.id); slices.Contains(sets, set) {
			return l.id
		}
	}
	return ""
}

// grantKindOf is the scope a set is granted at, by Scaleway's scope type, and
// false for a type no rule of the seat's policy takes.
func grantKindOf(scopeType string) (seataccess.GrantScopeKind, bool) {
	switch scopeType {
	case "projects":
		return projectKind, true
	case "organization":
		return seataccess.GrantScopeAccount, true
	}
	return "", false
}

// grantEntries is every set the catalogue lists that a seat may be granted
// beside its level, by category then name.
func grantEntries(catalogue []PermissionSet) []seataccess.GrantEntry {
	out := []seataccess.GrantEntry{}
	for _, s := range catalogue {
		kind, ok := grantKindOf(s.ScopeType)
		if !ok || slices.Contains(neverGranted, s.Name) {
			continue
		}
		group := ""
		if len(s.Categories) > 0 {
			group = s.Categories[0]
		}
		out = append(out, seataccess.GrantEntry{
			ID: s.Name, Name: s.Name, Group: group, ScopeKind: kind,
			Write: grantWrite(s.Name), InLevel: lowestLevelOf(s.Name),
		})
	}
	slices.SortFunc(out, func(a, b seataccess.GrantEntry) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// scopeTypes is each listed set's scope type, by name.
func scopeTypes(catalogue []PermissionSet) map[string]string {
	out := make(map[string]string, len(catalogue))
	for _, s := range catalogue {
		out[s.Name] = s.ScopeType
	}
	return out
}

// rulesWith is a seat's policy at a level on its projects with its grants: the
// level's two rules, a grant on the seat's own projects or the Organization
// joining the matching one, and one more rule per other set of projects.
func rulesWith(levelID, org string, projects []string, grants []seataccess.Grant,
	types map[string]string) ([]RuleSpec, error) {

	rules, err := rulesFor(levelID, org, projects)
	if err != nil {
		return nil, err
	}
	byProjects := map[string]*RuleSpec{}
	for _, g := range seataccess.NormalGrants(grants) {
		kind, ok := grantKindOf(types[g.Permission])
		switch {
		case slices.Contains(neverGranted, g.Permission):
			return nil, fmt.Errorf("%s is never granted to an agent", g.Permission)
		case !ok:
			return nil, fmt.Errorf("Scaleway lists no permission set a policy can grant named %s", g.Permission)
		case kind == seataccess.GrantScopeAccount:
			rules[1].PermissionSetNames = append(rules[1].PermissionSetNames, g.Permission)
		case len(g.Scope) == 0:
			rules[0].PermissionSetNames = append(rules[0].PermissionSetNames, g.Permission)
		default:
			scope := normalProjects(g.Scope)
			key := strings.Join(scope, ",")
			if byProjects[key] == nil {
				byProjects[key] = &RuleSpec{ProjectIDs: scope}
			}
			byProjects[key].PermissionSetNames = append(byProjects[key].PermissionSetNames, g.Permission)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(byProjects)) {
		rules = append(rules, *byProjects[key])
	}
	for i := range rules {
		slices.Sort(rules[i].PermissionSetNames)
		rules[i].PermissionSetNames = slices.Compact(rules[i].PermissionSetNames)
	}
	return rules, nil
}

// ruleKeys is rules as a sorted set of comparable lines: where each applies
// and its sorted sets.
func ruleKeys[R interface{ key() string }](rules []R) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.key())
	}
	slices.Sort(out)
	return out
}

func (r RuleSpec) key() string {
	where := "org:" + r.OrganizationID
	if r.OrganizationID == "" {
		where = "projects:" + strings.Join(normalProjects(r.ProjectIDs), ",")
	}
	sets := slices.Sorted(slices.Values(r.PermissionSetNames))
	return where + "|" + strings.Join(slices.Compact(sets), ",")
}

func (r Rule) key() string {
	spec := RuleSpec{PermissionSetNames: r.PermissionSetNames, ProjectIDs: r.ProjectIDs}
	if r.OnOrganization() {
		spec = RuleSpec{PermissionSetNames: r.PermissionSetNames, OrganizationID: "*"}
	}
	return spec.key()
}

// accessOf is a policy's rules in a seat's terms. Where they are exactly what
// applied's level, projects and grants write, they are those, so a grant that
// overlaps a higher level never reads as that level. Otherwise the level is
// the highest one a project rule holds whole, the seat's projects that rule's,
// and every other set a grant on where it applies, which the plan restores.
func accessOf(rules []Rule, applied *seataccess.Desired, types map[string]string) (string, []string, []seataccess.Grant) {
	if applied != nil {
		live := plainGrants(applied.Grants)
		if want, err := rulesWith(applied.Level, "*", applied.Scope, live, types); err == nil &&
			slices.Equal(ruleKeys(rules), ruleKeys(orgAny(want))) {
			return applied.Level, normalProjects(applied.Scope), live
		}
	}
	var org []string
	var project []Rule
	for _, r := range rules {
		if r.OnOrganization() {
			org = append(org, r.PermissionSetNames...)
			continue
		}
		project = append(project, r)
	}
	level, projects, levelRule := "", []string(nil), -1
	if !slices.ContainsFunc(orgSets, func(s string) bool { return !slices.Contains(org, s) }) {
		for i := len(levels) - 1; i >= 0 && levelRule < 0; i-- {
			sets, _ := ProjectSets(levels[i].id)
			for j, r := range project {
				if !slices.ContainsFunc(sets, func(s string) bool { return !slices.Contains(r.PermissionSetNames, s) }) {
					level, projects, levelRule = levels[i].id, normalProjects(r.ProjectIDs), j
					break
				}
			}
		}
	}
	if levelRule < 0 {
		var all []string
		for _, r := range project {
			all = append(all, r.ProjectIDs...)
		}
		return "", normalProjects(all), nil
	}
	var grants []seataccess.Grant
	levelSets, _ := ProjectSets(level)
	for _, s := range org {
		if !slices.Contains(orgSets, s) {
			grants = append(grants, seataccess.Grant{Permission: s})
		}
	}
	for j, r := range project {
		for _, s := range r.PermissionSetNames {
			switch {
			case j == levelRule && slices.Contains(levelSets, s):
			case j == levelRule:
				grants = append(grants, seataccess.Grant{Permission: s})
			default:
				grants = append(grants, seataccess.Grant{Permission: s, Scope: normalProjects(r.ProjectIDs)})
			}
		}
	}
	return level, projects, plainGrants(grants)
}

// orgAny is rules with every organization rule's ID written as the reading
// side spells it, so a written policy compares with one read back.
func orgAny(rules []RuleSpec) []RuleSpec {
	out := slices.Clone(rules)
	for i := range out {
		if out[i].OrganizationID != "" {
			out[i].OrganizationID = "*"
		}
	}
	return out
}

// plainGrants is grants as a policy holds them: permission and scope, each
// key once, sorted.
func plainGrants(grants []seataccess.Grant) []seataccess.Grant {
	var out []seataccess.Grant
	for _, g := range seataccess.NormalGrants(grants) {
		p := seataccess.Grant{Permission: g.Permission, Scope: g.Scope}
		if !slices.ContainsFunc(out, func(h seataccess.Grant) bool { return seataccess.GrantKey(h) == seataccess.GrantKey(p) }) {
			out = append(out, p)
		}
	}
	return out
}

// permissionSets reads, once per pass, the Organization's permission sets.
func (s *accessSession) permissionSets(ctx context.Context) ([]PermissionSet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sets != nil {
		return *s.sets, nil
	}
	sets, err := s.client.PermissionSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("scaleway: read the Organization's permission sets: %w", reject(err))
	}
	s.sets = &sets
	return sets, nil
}

// GrantCatalogue is every set a seat may be granted beside its level, and the
// projects a grant may name ([seataccess.Granter]), read once per session.
func (s *accessSession) GrantCatalogue(ctx context.Context, _ seataccess.Lane) (
	[]seataccess.GrantEntry, []string, error) {

	sets, err := s.permissionSets(ctx)
	if err != nil {
		return nil, nil, err
	}
	projects, err := s.orgProjects(ctx)
	if err != nil {
		return nil, nil, err
	}
	scope := make([]string, 0, len(projects))
	for _, p := range projects {
		scope = append(scope, p.ID)
	}
	return grantEntries(sets), normalProjects(scope), nil
}

// grantSteps is an [seataccess.OpGrants] op as steps: one per grant added and
// one per grant taken off, each naming the set as the console does.
func grantSteps(op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
	opts seataccess.Options) []seataccess.Step {
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	holds := func(list []seataccess.Grant, g seataccess.Grant) bool {
		return slices.ContainsFunc(list, func(h seataccess.Grant) bool { return seataccess.GrantKey(h) == seataccess.GrantKey(g) })
	}
	var out []seataccess.Step
	for _, g := range seataccess.NormalGrants(want.Grants) {
		if holds(have.Grants, g) {
			continue
		}
		until := "permanently"
		if !g.ExpiresAt.IsZero() {
			until = "until " + g.ExpiresAt.UTC().Format(time.DateOnly+" 15:04 UTC")
		}
		out = append(out, step(seataccess.VerbGrant,
			fmt.Sprintf("Add %s to its policy on %s, %s", g.Permission, grantReach(g, opts), until)))
	}
	for _, g := range seataccess.NormalGrants(have.Grants) {
		if holds(want.Grants, g) {
			continue
		}
		out = append(out, step(seataccess.VerbUngrant,
			fmt.Sprintf("Take %s on %s off its policy", g.Permission, grantReach(g, opts))))
	}
	if len(out) == 0 {
		out = append(out, step(seataccess.VerbGrant, "Write its policy at its level and grants"))
	}
	return out
}

// grantReach is where a grant applies, as a step says it: its projects by
// name, the Organization, or the seat's own projects.
func grantReach(g seataccess.Grant, opts seataccess.Options) string {
	if entry, ok := opts.Grantable(g.Permission); ok && entry.ScopeKind == seataccess.GrantScopeAccount {
		return "the Organization"
	}
	if len(g.Scope) == 0 {
		return "its projects"
	}
	names := make([]words.Said, 0, len(g.Scope))
	for _, id := range g.Scope {
		if name := opts.ScopeNames[id]; name != "" {
			id = name
		}
		names = append(names, words.Said(id))
	}
	return words.And(names)
}

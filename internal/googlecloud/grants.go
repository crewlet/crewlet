package googlecloud

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// A seat's extra grant is one role bound to its service account on some of
// its projects beside its level. Google lets the provisioner change only the
// roles its conditions list, so the owner adds one more condition to a
// project, the GRANTS LIST: a hasOnly list of the roles Crewlet may grant
// there on request, read live as that project's grant catalogue. A role in it
// that could mint credentials, act as another identity or grant access is
// never accepted, and the project is not offered while one is listed.

// grantRoles is the roles project's grants lists hold, sorted: every hasOnly
// list on the provisioner's Project IAM Admin that is no level's list.
func (c containment) grantRoles(project string) []string {
	levelLists := Lists(project)
	var out []string
	for _, l := range c.lists {
		if slices.ContainsFunc(levelLists, func(want []string) bool {
			return slices.Equal(l, slices.Sorted(slices.Values(want)))
		}) {
			continue
		}
		out = append(out, l...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// grantBarred is the permissions of a role no grant may hold, sorted: [never]
// and [credential] alike, whatever level the seat is at, since a grant is
// handed out on request and access that outlives a token never is.
func grantBarred(permissions []string) []string {
	var out []string
	for _, p := range permissions {
		if never().MatchString(p) || credential().MatchString(p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// readVerbs are the method names of a permission that only reads.
//
//nolint:gochecknoglobals // an immutable list, not state
var readVerbs = []string{"get", "list", "search", "aggregatedList", "query", "read", "view", "getIamPolicy"}

// RoleWrites reports a role holding any permission that is not a read,
// failing closed: a verb this package does not know is a write.
func RoleWrites(permissions []string) bool {
	for _, p := range permissions {
		verb := p[strings.LastIndex(p, ".")+1:]
		if !slices.ContainsFunc(readVerbs, func(v string) bool { return strings.HasPrefix(verb, v) }) {
			return true
		}
	}
	return false
}

// roleReads reads each role once per discovery, however many projects'
// grants lists name it.
type roleReads struct {
	mu   sync.Mutex
	read map[string]roleRead
}

type roleRead struct {
	role  Role
	found bool
	err   error
}

func (r *roleReads) role(ctx context.Context, c *Client, name string) (Role, bool, error) {
	r.mu.Lock()
	got, ok := r.read[name]
	r.mu.Unlock()
	if ok {
		return got.role, got.found, got.err
	}
	role, found, err := c.Role(ctx, name)
	r.mu.Lock()
	if r.read == nil {
		r.read = map[string]roleRead{}
	}
	r.read[name] = roleRead{role, found, err}
	r.mu.Unlock()
	return role, found, err
}

// grantProblems reads each role project's grants lists hold, keeps those a
// grant may carry on setup, and says why the project is not offered where one
// may not: a role Google does not have, or one holding a barred permission.
func grantProblems(ctx context.Context, c *Client, reads *roleReads, setup *projectSetup, roles []string) error {
	const fix = "Take it out of the provisioner's grants condition on the project"
	for _, name := range roles {
		role, found, err := reads.role(ctx, c, name)
		switch {
		case isStage(err):
			return err
		case err != nil:
			setup.Problems = append(setup.Problems, problem{unreadable("the grants list's "+name, err),
				"Grant the provisioner Role Viewer on the project"})
		case !found || role.Deleted:
			setup.Problems = append(setup.Problems, problem{"the grants list names " + name +
				", which Google does not have", fix})
		default:
			if barred := grantBarred(role.Permissions); len(barred) > 0 {
				setup.Problems = append(setup.Problems, problem{"the grants list names " + name + ", which holds " +
					barred[0] + ", which no grant may hold", fix})
				continue
			}
			setup.Grantable = append(setup.Grantable, role)
		}
	}
	return nil
}

// lists reports a role project's grants list names and a grant may carry.
func (d discovery) lists(project, role string) bool {
	i := slices.IndexFunc(d, func(p projectSetup) bool { return p.ID == project })
	return i >= 0 && slices.ContainsFunc(d[i].Grantable, func(r Role) bool { return r.Name == role })
}

// grantRolesOn is the roles grants bind on project, sorted: each grant naming
// it, or naming none while project is one of the seat's own.
func grantRolesOn(grants []seataccess.Grant, scope []string, project string) []string {
	var out []string
	for _, g := range grants {
		if slices.Contains(g.Scope, project) || (len(g.Scope) == 0 && slices.Contains(scope, project)) {
			out = append(out, g.Permission)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// grantProjects is every project grants name, which a step reads besides the
// seat's own.
func grantProjects(grants []seataccess.Grant) []string {
	var out []string
	for _, g := range grants {
		out = append(out, g.Scope...)
	}
	return out
}

// accessOf is what an account holds, by project, in a seat's terms. Where it
// is exactly what applied's level, projects and grants bind, it is those, so a
// grant of a higher level's role never reads as that level. Otherwise a
// project is the seat's where it holds a level's roles whole, the highest such
// level, and every other role there is a grant; the level is "" where the
// seat's projects disagree, which the plan restores.
func accessOf(bound map[string][]string, applied *seataccess.Desired) (string, []string, []seataccess.Grant) {
	if applied != nil && applied.Enabled && heldExactly(bound, *applied) {
		return applied.Level, slices.Sorted(slices.Values(applied.Scope)), plainGrants(applied.Grants)
	}
	level, mixed := "", false
	var scope []string
	byRole := map[string][]string{}
	for _, project := range slices.Sorted(maps.Keys(bound)) {
		roles := bound[project]
		held := ""
		for i := len(levels) - 1; i >= 0 && held == ""; i-- {
			want, _ := LevelRoles(levels[i].id, project)
			if !slices.ContainsFunc(want, func(r string) bool { return !slices.Contains(roles, r) }) {
				held = levels[i].id
			}
		}
		levelRoles, _ := LevelRoles(held, project)
		for _, r := range roles {
			if !slices.Contains(levelRoles, r) {
				byRole[r] = append(byRole[r], project)
			}
		}
		if held == "" {
			continue
		}
		scope = append(scope, project)
		switch {
		case level == "":
			level = held
		case held != level:
			mixed = true
		}
	}
	if mixed {
		level = ""
	}
	var grants []seataccess.Grant
	for role, projects := range byRole {
		g := seataccess.Grant{Permission: role, Scope: projects}
		if slices.Equal(projects, scope) {
			g.Scope = nil
		}
		grants = append(grants, g)
	}
	return level, scope, plainGrants(grants)
}

// heldExactly reports an account holding on each project exactly what want's
// level and grants bind there, and nothing anywhere else.
func heldExactly(bound map[string][]string, want seataccess.Desired) bool {
	projects := slices.Concat(want.Scope, grantProjects(want.Grants))
	slices.Sort(projects)
	projects = slices.Compact(projects)
	if len(bound) != len(projects) {
		return false
	}
	for _, project := range projects {
		roles := grantRolesOn(want.Grants, want.Scope, project)
		if slices.Contains(want.Scope, project) {
			level, _ := LevelRoles(want.Level, project)
			roles = append(roles, level...)
		}
		slices.Sort(roles)
		if !slices.Equal(slices.Sorted(slices.Values(bound[project])), slices.Compact(roles)) {
			return false
		}
	}
	return true
}

// plainGrants is grants as bindings hold them: permission and scope, each key
// once, sorted.
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

// GrantCatalogue is every role a project's grants list offers a seat beside its
// level, and the projects a grant may name ([seataccess.Granter]): the
// company's offered projects. A role is offered where any project lists it,
// and a grant naming a project whose list does not is refused at its step.
func (s *accessSession) GrantCatalogue(ctx context.Context, _ seataccess.Lane) (
	[]seataccess.GrantEntry, []string, error) {

	d, err := s.discovered(ctx)
	if err != nil {
		return nil, nil, err
	}
	scope, _ := d.offered()
	byName := map[string]Role{}
	for _, p := range d {
		if !p.Offered() {
			continue
		}
		for _, r := range p.Grantable {
			byName[r.Name] = r
		}
	}
	first := predefinedRoles()
	out := make([]seataccess.GrantEntry, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		r := byName[name]
		title := strings.TrimSpace(r.Title)
		if title == "" {
			title = name
		}
		out = append(out, seataccess.GrantEntry{
			ID: name, Name: title, Group: serviceOf(name), ScopeKind: projectKind,
			Write: RoleWrites(r.Permissions), InLevel: first[name],
		})
	}
	return out, scope, nil
}

// projectKind is a grant's scope entries: project IDs.
const projectKind seataccess.GrantScopeKind = "project"

// serviceOf is the service a predefined role belongs to (roles/pubsub.editor
// is pubsub), "" for a custom role.
func serviceOf(role string) string {
	name, ok := strings.CutPrefix(role, "roles/")
	if !ok {
		return ""
	}
	service, _, _ := strings.Cut(name, ".")
	return service
}

// grantSteps is an [seataccess.OpGrants] op as steps: one per grant added and
// one per grant taken off, each naming the role and where it applies.
func grantSteps(want seataccess.Desired, have seataccess.Observed, opts seataccess.Options) []seataccess.Step {
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: seataccess.OpGrants, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	holds := func(list []seataccess.Grant, g seataccess.Grant) bool {
		return slices.ContainsFunc(list, func(h seataccess.Grant) bool { return seataccess.GrantKey(h) == seataccess.GrantKey(g) })
	}
	reach := func(g seataccess.Grant) string {
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
	var out []seataccess.Step
	for _, g := range seataccess.NormalGrants(want.Grants) {
		if holds(have.Grants, g) {
			continue
		}
		until := "permanently"
		if !g.ExpiresAt.IsZero() {
			until = "until " + g.ExpiresAt.UTC().Format(time.DateOnly+" 15:04 UTC")
		}
		out = append(out, step(seataccess.VerbGrant, fmt.Sprintf("Grant %s on %s, %s", g.Permission, reach(g), until)))
	}
	for _, g := range seataccess.NormalGrants(have.Grants) {
		if !holds(want.Grants, g) {
			out = append(out, step(seataccess.VerbUngrant, fmt.Sprintf("Remove %s on %s", g.Permission, reach(g))))
		}
	}
	if len(out) == 0 {
		out = append(out, step(seataccess.VerbGrant, "Grant its roles on every project as its level and grants bind them"))
	}
	return out
}

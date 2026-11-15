package googlecloud

import (
	"context"
	"slices"
	"strings"
	"sync"
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

package googlecloud

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/words"
)

// The company's projects are discovered, never configured: every project the
// provisioner can read, less the identity project, where the owner granted
// the provisioner something that administers it. One is offered to agents
// only when all three level conditions are there with exactly their lists, no
// admin role is held without a condition, and crewletOperator holds exactly
// its permissions. The owner's grant is the boundary, and nothing in the
// document can drift from it.

// discoverReads bounds the policy and role reads one discovery makes at once:
// two per project, so a company with dozens of projects is read in a few
// round trips without a burst Google's per-minute read quota would notice.
const discoverReads = 8

// projectSetup is one of the company's projects as the provisioner's grants
// there leave it.
type projectSetup struct {
	ID, Name string
	// Parent is the project's organization or folder, which a domain
	// restricted sharing remedy names.
	Parent string
	// Problems are why the project is not offered, each a sentence with its
	// fix; none on a project agents may be given.
	Problems []string
}

// Offered reports a project agents may be given.
func (p projectSetup) Offered() bool { return len(p.Problems) == 0 }

// discovery is the company's projects, by ID.
type discovery []projectSetup

// offered is the IDs agents may be given, sorted, and the display names Google
// gives them.
func (d discovery) offered() ([]string, map[string]string) {
	var ids []string
	names := map[string]string{}
	for _, p := range d {
		if !p.Offered() {
			continue
		}
		ids = append(ids, p.ID)
		if name := strings.TrimSpace(p.Name); name != "" {
			names[p.ID] = name
		}
	}
	return ids, names
}

// project is the setup of one of the company's projects, and false where it
// is none of the company's.
func (d discovery) project(id string) (projectSetup, bool) {
	i := slices.IndexFunc(d, func(p projectSetup) bool { return p.ID == id })
	if i < 0 {
		return projectSetup{}, false
	}
	return d[i], true
}

// discover reads the company's projects. A project whose policy cannot be read
// is named with that, since its setup cannot be told.
func discover(ctx context.Context, c *Client) (discovery, error) {
	found, err := c.SearchProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("googlecloud: list the projects the provisioner can read: %w", err)
	}
	var candidates []Project
	for _, p := range found {
		if p.ProjectID != c.IdentityProject() && p.State == "ACTIVE" {
			candidates = append(candidates, p)
		}
	}
	out := make([]*projectSetup, len(candidates))
	errs := make([]error, len(candidates))
	gate := make(chan struct{}, discoverReads)
	var wg sync.WaitGroup
	for i, p := range candidates {
		wg.Go(func() {
			gate <- struct{}{}
			defer func() { <-gate }()
			out[i], errs[i] = setupOf(ctx, c, p)
		})
	}
	wg.Wait()
	var d discovery
	for i := range candidates {
		switch {
		case errs[i] != nil:
			return nil, errs[i]
		case out[i] != nil:
			d = append(d, *out[i])
		}
	}
	slices.SortFunc(d, func(a, b projectSetup) int { return cmp.Compare(a.ID, b.ID) })
	return d, nil
}

// setupOf is one project's setup, nil where the provisioner holds nothing
// there that administers it. Only a provisioner refused outright is an error:
// one project's unreadable policy or role is that project's problem.
func setupOf(ctx context.Context, c *Client, p Project) (*projectSetup, error) {
	out := &projectSetup{ID: p.ProjectID, Name: p.DisplayName, Parent: p.Parent}
	pol, err := c.ProjectPolicy(ctx, p.ProjectID)
	switch {
	case isStage(err):
		return nil, err
	case err != nil:
		out.Problems = append(out.Problems, "its policy cannot be read ("+err.Error()+")")
		return out, nil
	}
	cont := containmentOf(pol, member(c.Provisioner()))
	if !cont.ours() {
		return nil, nil
	}
	out.Problems = append(out.Problems, cont.problems(p.ProjectID)...)
	switch role, found, err := c.Role(ctx, CustomRole(p.ProjectID)); {
	case isStage(err):
		return nil, err
	case err != nil:
		out.Problems = append(out.Problems, "crewletOperator cannot be read: grant the provisioner Role Viewer "+
			"on the project ("+err.Error()+")")
	case !found:
		out.Problems = append(out.Problems, "it has no crewletOperator role: run the Prepare a project block")
	default:
		if diff := customRoleDiff(role); diff != "" {
			out.Problems = append(out.Problems, diff+": make it hold exactly "+words.And(customRolePerms))
		}
	}
	return out, nil
}

// isStage reports a failure to reach Google as the provisioner at all, which
// is every project's alike.
func isStage(err error) bool {
	_, ok := errors.AsType[*stageError](err)
	return ok
}

// problems is why the provisioner's grants on project keep it from agents:
// each level whose exact condition is missing, a condition that is no level's,
// and an admin role it holds with no condition.
func (c containment) problems(project string) []string {
	var out []string
	if len(c.unconditional) > 0 {
		out = append(out, "the provisioner holds "+words.And(c.unconditional)+" with no condition, so it could "+
			"grant anything there: remove that grant and keep the three level conditions")
	}
	if len(c.malformed) > 0 {
		out = append(out, "a condition on the provisioner's Project IAM Admin is not a level list, so it could "+
			"grant more than a level: replace it with the Prepare a project block's conditions")
	}
	lists := Lists(project)
	for _, l := range c.lists {
		if !slices.ContainsFunc(lists, func(want []string) bool {
			return slices.Equal(l, slices.Sorted(slices.Values(want)))
		}) {
			out = append(out, "a condition lists "+words.And(l)+", which is no level's list: replace it with the "+
				"Prepare a project block's conditions")
		}
	}
	var missing []words.Said
	for i, list := range lists {
		if !c.holds(list) {
			missing = append(missing, words.Said(levels[i].name))
		}
	}
	if len(missing) > 0 {
		out = append(out, "no condition lists exactly the "+words.And(missing)+" roles: run the Prepare a "+
			"project block")
	}
	return out
}

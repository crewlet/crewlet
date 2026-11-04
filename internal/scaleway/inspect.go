package scaleway

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// inspection is one read of the provisioner key and the organization it
// provisions in: the read both the Test button and the company pass make, in
// this order.
type inspection struct {
	// key is the provisioner's own API key, and app the application it
	// belongs to.
	key APIKey
	app Application

	// direct is false where Scaleway refused the read of the key by its
	// access key and the listing found it instead.
	direct bool

	// grants is what the application's own policies grant it.
	grants grants

	gaps     []seataccess.LevelGap
	projects []Project
}

// grants is what the provisioner application's policies grant it.
type grants struct {
	// missing are the provisioner sets no rule on the organization grants,
	// nor any set standing in for it ([standsIn]), under any condition.
	missing []string

	// keyCondition is the condition on the resource that every rule
	// granting IAMApplicationManager carries, "" where one grants it with
	// none or with a condition on the request alone. A key has no attribute
	// naming the application it belongs to, so such a condition refuses
	// operations on the agents' keys.
	keyCondition string

	// conditions are every condition on its rules, each once, as Scaleway
	// writes them.
	conditions []string

	// extra are the sets it holds beyond the three it needs, sorted.
	extra []string
}

// pinned reports a provisioner limited to a source address by a condition on
// the request ([config.ScalewayProvisioning]'s recommended IP pin).
func (g grants) pinned() bool {
	return slices.ContainsFunc(g.conditions, func(c string) bool { return strings.Contains(c, "request.ip") })
}

// stage is one read of an inspection, and what it needs.
type stage struct {
	name  string
	needs string
}

//nolint:gochecknoglobals // immutable descriptions, not state
var (
	stageKey       = stage{name: "its own API key", needs: "IAMApplicationManager"}
	stageApp       = stage{name: "its own IAM application", needs: "IAMApplicationManager"}
	stagePolicies  = stage{name: "its own policies", needs: "IAMPolicyManager"}
	stageCatalogue = stage{name: "the Organization's permission sets", needs: "IAMPolicyManager"}
	stageProjects  = stage{name: "the Organization's projects", needs: "ProjectReadOnly"}
	stageApps      = stage{name: "the Organization's IAM applications", needs: "IAMApplicationManager"}
	stageSeat      = stage{name: "an agent's IAM application", needs: "IAMApplicationManager and IAMPolicyManager"}
)

// stageError is a read of an inspection that failed, naming which.
type stageError struct {
	stage stage
	err   error
}

func (e *stageError) Error() string { return fmt.Sprintf("read %s: %v", e.stage.name, e.err) }
func (e *stageError) Unwrap() error { return e.err }

// bearerError is a key that is not an IAM application's in the configured
// organization: a user's key, or one of another organization.
type bearerError struct{ said string }

func (e *bearerError) Error() string { return e.said }

// inspect reads the provisioner key, its application and that application's
// rules, the organization's catalogue and its projects. bound, where set,
// gives each call its own deadline inside ctx's.
func inspect(
	ctx context.Context, c *Client, access string,
	bound func(context.Context) (context.Context, context.CancelFunc),
) (inspection, error) {
	call := func(s stage, fn func(context.Context) error) error {
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if bound != nil {
			callCtx, cancel = bound(ctx)
		}
		defer cancel()
		if err := fn(callCtx); err != nil {
			return &stageError{stage: s, err: err}
		}
		return nil
	}
	var in inspection
	if err := call(stageKey, func(ctx context.Context) error {
		var err error
		in.key, in.direct, err = c.OwnKey(ctx, access)
		return err
	}); err != nil {
		return inspection{}, err
	}
	if strings.TrimSpace(in.key.ApplicationID) == "" {
		return inspection{}, &bearerError{said: "This API key belongs to a user, not to an IAM application. " +
			"A user's key carries everything the user can do, so Crewlet takes an application's key only"}
	}
	var found bool
	if err := call(stageApp, func(ctx context.Context) error {
		var err error
		in.app, found, err = c.Application(ctx, in.key.ApplicationID)
		return err
	}); err != nil {
		return inspection{}, err
	}
	if !found || config.ScalewayID(in.app.OrganizationID) != c.OrganizationID() {
		return inspection{}, &bearerError{said: "This API key's application is not in the Organization " +
			c.OrganizationID() + ". Check the Organization ID, or create the key in that Organization"}
	}
	var rules []Rule
	if err := call(stagePolicies, func(ctx context.Context) error {
		policies, err := c.PoliciesOf(ctx, in.app.ID)
		if err != nil {
			return err
		}
		for _, p := range policies {
			held, err := c.Rules(ctx, p.ID)
			if err != nil {
				return err
			}
			rules = append(rules, held...)
		}
		return nil
	}); err != nil {
		return inspection{}, err
	}
	in.grants = grantsOf(rules, c.OrganizationID())
	if err := call(stageCatalogue, func(ctx context.Context) error {
		catalogue, err := c.PermissionSets(ctx)
		in.gaps = levelGaps(catalogue)
		return err
	}); err != nil {
		return inspection{}, err
	}
	if err := call(stageProjects, func(ctx context.Context) error {
		var err error
		in.projects, err = c.Projects(ctx)
		return err
	}); err != nil {
		return inspection{}, err
	}
	return in, nil
}

// grantsOf is what rules grant the provisioner on org.
func grantsOf(rules []Rule, org string) grants {
	var g grants
	// open holds what a rule grants with no condition on the resource, and
	// onResource the first such condition granting each set otherwise.
	open := map[string]bool{}
	onResource := map[string]string{}
	all := map[string]bool{}
	for _, r := range rules {
		cond := strings.TrimSpace(r.Condition)
		if cond != "" && !slices.Contains(g.conditions, cond) {
			g.conditions = append(g.conditions, cond)
		}
		onOrg := r.OnOrganization() && config.ScalewayID(*r.OrganizationID) == org
		for _, name := range r.PermissionSetNames {
			all[name] = true
			switch {
			case !onOrg:
			case !resourceCondition(cond):
				open[name] = true
			case onResource[name] == "":
				onResource[name] = cond
			}
		}
	}
	for _, need := range provisionerSets {
		names := append([]string{need}, standsIn[need]...)
		if slices.ContainsFunc(names, func(n string) bool { return open[n] }) {
			continue
		}
		i := slices.IndexFunc(names, func(n string) bool { return onResource[n] != "" })
		switch {
		case i < 0:
			g.missing = append(g.missing, need)
		case need == "IAMApplicationManager":
			g.keyCondition = onResource[names[i]]
		}
	}
	for name := range all {
		if !slices.Contains(provisionerSets, name) {
			g.extra = append(g.extra, name)
		}
	}
	slices.Sort(g.extra)
	return g
}

// resourceCondition reports a condition on the resource a call acts on, as
// opposed to one on the request alone (its source address, its user agent).
func resourceCondition(cond string) bool { return strings.Contains(cond, "resource.") }

// keysRefused reports a provisioner that cannot act on the agents' keys: its
// rule granting IAMApplicationManager carries a condition on the resource, or
// Scaleway refused it reading its own key by its access key while the
// listing showed the key.
func (in inspection) keysRefused() bool { return in.grants.keyCondition != "" || !in.direct }

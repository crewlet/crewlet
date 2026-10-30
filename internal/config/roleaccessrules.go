package config

import (
	"errors"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// THE DOCUMENT'S RULES FOR PER-SEAT ACCESS.
//
// ADMISSION RULES, refused on submit and reported as a warning on a stored
// document ([Company.AdmissionWarnings]), for the reason every rule added after
// companies existed is one: a stored revision from before them runs today as
// it ran yesterday, and refusing to apply it would take a working company down
// on upgrade over a rule its author never saw. The access sync holds the same
// line again at the vendor, as defence in depth (internal/seataccess).
//
// Four things are refused:
//
//   - a block that does not say `enabled`, one on a human seat, or one that
//     turns a seat on at an integration the company has not connected;
//   - access the vendor does not offer, where the vendor's set is fixed (a
//     GitLab role, Mattermost roles, a GitHub permission); a live-listed id (a
//     Datadog role, a Jira project role, a Confluence space role, a GitLab
//     custom role) is checked by the integration's sync against the vendor,
//     and one that no longer exists is a finding there;
//   - a scope wider than the company's reach, so a seat cannot widen it;
//   - a seat credential written where the engine owns it, shared with a
//     company block or another seat, or reading another seat's
//     (seatcredentialrules.go).

// seatAccessOnHuman is the refusal of a per-seat access block on a human seat.
const seatAccessOnHuman = "a human seat holds no account the engine manages"

// validateSeatAccess is the per-seat access rules, run together.
func (c *Company) validateSeatAccess() error {
	refs, err := c.References()
	if err != nil {
		return err
	}
	return errors.Join(c.validateSeatBlocks(), c.validateSeatCredentials(refs))
}

// validateSeatBlocks checks every seat's per-integration blocks.
func (c *Company) validateSeatBlocks() error {
	var p problems
	projects, spaces := c.namedContainers()
	for role, path := range c.EachRole() {
		in := role.Integrations
		base := at(path, "integrations")
		blocks := []struct {
			lane    string
			present bool
			enabled *bool
		}{}
		for _, lane := range SeatAccessLanes() {
			on, present := role.seatEnabled(lane)
			blocks = append(blocks, struct {
				lane    string
				present bool
				enabled *bool
			}{lane, present, on})
		}
		for _, b := range blocks {
			if !b.present {
				continue
			}
			here := at(base, b.lane)
			if role.Kind == org.KindHuman {
				// GitHub's, Slack's and Mattermost's are refused for a human
				// seat already, each with its own remedy: the GitHub App by
				// [Company.validateHumanSeatApps], the chat apps by the org
				// model's agent-only rule.
				if slices.Contains([]string{"datadog", "gitlab", "jira", "confluence"}, b.lane) {
					p.add(here, ErrConflict, "%s: a person signs in as themselves. "+
						"Remove this block, or make the seat an agent seat", seatAccessOnHuman)
				}
				continue
			}
			if b.enabled == nil {
				p.add(at(here, "enabled"), ErrMissing,
					"required: say whether this agent is on at %s. An agent is off "+
						"until turned on, and the document states it rather than "+
						"leaving it to be inferred", b.lane)
				continue
			}
			if *b.enabled && !c.connected(b.lane) {
				p.add(at(here, "enabled"), ErrConflict,
					"this agent is turned on at %s, which the company has not "+
						"connected: add integrations.%s first, or turn the agent off",
					b.lane, b.lane)
			}
		}
		if role.Kind == org.KindHuman {
			continue
		}
		c.checkDatadog(&p, at(base, "datadog"), in.Datadog)
		c.checkGitLab(&p, at(base, "gitlab"), in.GitLab)
		c.checkJira(&p, at(base, "jira"), in.Jira, projects)
		c.checkConfluence(&p, at(base, "confluence"), in.Confluence, spaces)
		c.checkGitHubAccess(&p, at(base, "github"), in.GitHub)
		c.checkMattermost(&p, at(base, "mattermost"), in.Mattermost)
	}
	return p.err()
}

// connected reports whether the company holds the connection a lane cannot
// work without, where there is one: Mattermost's server and team, which the
// agent's bot posts through, and Jira's and Confluence's sites, which the
// engine's own tools read (a Data Center site included, whose seats'
// credentials are external, D4b).
//
// THE OTHER LANES NEED NONE OF THEIR OWN. Where the engine provisions Slack,
// GitHub, GitLab or Datadog the company block is there by definition
// ([Company.SeatCredentialMode]); where it does not, an agent's app or token
// was made outside Crewlet and is bound to its tool from the seat alone, so
// `enabled` there needs no company block.
func (c *Company) connected(lane string) bool {
	in := c.Integrations
	switch lane {
	case "mattermost":
		return in.Mattermost != nil && in.Mattermost.Enabled
	case "jira":
		return in.Jira != nil
	case "confluence":
		return in.Confluence != nil
	default:
		return true
	}
}

// levelRequired reports whether a seat that is on at kind must name a level.
//
// ONLY WHERE THE ENGINE PROVISIONS THE KIND ([Company.documentMode]). An
// agent whose account or token was made outside Crewlet (D4b) holds whatever
// access it was given there; the engine binds its credential and grants
// nothing, so a level would be a setting nothing reads, and requiring one left
// a migrated company with external seats failing its own admission.
func (c *Company) levelRequired(kind string) bool {
	return c.documentMode(kind) == SeatCredentialEngine
}

// namedContainers is every tracker project and knowledge space the document
// names, which is the company's reach at Jira and Confluence.
func (c *Company) namedContainers() (projects, spaces []string) {
	add := func(list []string, v string) []string {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(list, v) {
			list = append(list, v)
		}
		return list
	}
	for role := range c.EachRole() {
		projects, spaces = add(projects, role.Project), add(spaces, role.Space)
	}
	for unit := range c.EachUnit() {
		projects, spaces = add(projects, unit.Project), add(spaces, unit.Space)
	}
	return projects, spaces
}

// withinReach refuses every scope entry outside reach, where the company lists
// a reach at all: an empty list is a connection that does not narrow, so any
// entry the vendor accepts is within it. Both sides are compared under key,
// the vendor's spelling of an entry ([JiraProjectKey], [ConfluenceSpaceKey]).
func withinReach(p *problems, path Path, scope, reach []string, key func(string) string, noun, where string) {
	if len(reach) == 0 {
		return
	}
	for i, entry := range scope {
		k := key(entry)
		if !slices.ContainsFunc(reach, func(r string) bool { return key(r) == k }) {
			p.add(idx(path, i), ErrConflict,
				"%q is outside the company's reach: a seat cannot widen it. Add the "+
					"%s to %s first, or remove it here", entry, noun, where)
		}
	}
}

func (c *Company) checkDatadog(p *problems, path Path, b *RoleDatadog) {
	if b == nil || !SeatAccessOn(b.Enabled) {
		return
	}
	if strings.TrimSpace(b.Role) == "" && c.levelRequired("datadog") {
		p.add(at(path, "role"), ErrMissing,
			"required while this agent is on: the Datadog role id its service "+
				"account holds, as GET /api/v2/roles lists it")
	}
}

func (c *Company) checkGitLab(p *problems, path Path, b *RoleGitLab) {
	if b == nil {
		return
	}
	if b.AccessLevel != 0 && !slices.Contains(GitLabRoles(), b.AccessLevel) {
		p.add(at(path, "access_level"), ErrUnknownValue,
			"%d is not a GitLab role this engine offers; give 10 Guest, 15 Planner, "+
				"20 Reporter, 30 Developer, 40 Maintainer or 50 Owner", b.AccessLevel)
	}
	if b.AccessLevel != 0 && b.MemberRoleID != 0 {
		p.add(at(path, "member_role_id"), ErrConflict,
			"a GitLab member holds a default role or a custom role, not both: "+
				"remove access_level or member_role_id")
	}
	if b.MemberRoleID < 0 {
		p.add(at(path, "member_role_id"), ErrOutOfRange, "%d is not a GitLab custom role id", b.MemberRoleID)
	}
	if SeatAccessOn(b.Enabled) && b.AccessLevel == 0 && b.MemberRoleID == 0 && c.levelRequired("gitlab") {
		p.add(at(path, "access_level"), ErrMissing,
			"required while this agent is on: its GitLab role (access_level) or "+
				"a custom role (member_role_id)")
	}
	var reach []string
	if gl := c.Integrations.GitLab; gl != nil && gl.Provisioning != nil {
		reach = gl.Provisioning.Projects
	}
	withinReach(p, at(path, "projects"), b.Projects, reach, strings.TrimSpace, "project",
		"integrations.gitlab.provisioning.projects")
}

func (c *Company) checkJira(p *problems, path Path, b *RoleJira, projects []string) {
	if b == nil {
		return
	}
	// NO ROLE IS PRODUCT ACCESS ONLY: the account has Jira and the site's
	// permission schemes decide what it sees. A project list names where a
	// role is held, so it needs one.
	if SeatAccessOn(b.Enabled) && strings.TrimSpace(b.ProjectRole) == "" && len(b.Projects) > 0 {
		p.add(at(path, "project_role"), ErrMissing,
			"required with a projects list: the Jira project role id it holds in "+
				"them, as the site's GET /rest/api/3/role lists it. Leave both out "+
				"for product access only")
	}
	withinReach(p, at(path, "projects"), b.Projects, projects, JiraProjectKey, "project",
		"a unit's or a root seat's `project`")
}

func (c *Company) checkConfluence(p *problems, path Path, b *RoleConfluence, spaces []string) {
	if b == nil {
		return
	}
	role, ops := strings.TrimSpace(b.SpaceRole), b.Operations
	if role != "" && len(ops) > 0 {
		p.add(at(path, "operations"), ErrConflict,
			"a Confluence seat holds a space role or space permissions, not both: "+
				"remove space_role or operations")
	}
	// NEITHER IS PRODUCT ACCESS ONLY: the account has Confluence and the
	// site's and each space's permissions decide what it sees. A space list
	// names where access is held, so it needs one.
	if SeatAccessOn(b.Enabled) && role == "" && len(ops) == 0 && len(b.Spaces) > 0 {
		p.add(at(path, "space_role"), ErrMissing,
			"required with a spaces list: its Confluence space role in them, or "+
				"its space permissions where the site has no space roles. Leave "+
				"all three out for product access only")
	}
	for i, op := range ops {
		operation, target, ok := strings.Cut(strings.TrimSpace(op), ":")
		if !ok || operation == "" || target == "" || strings.ContainsAny(op, " \t") {
			p.add(idx(at(path, "operations"), i), ErrUnknownValue,
				"%q is not a Confluence space permission; give it as "+
					"<operation>:<target>, as Confluence names them (read:space, "+
					"create:page)", op)
		}
	}
	withinReach(p, at(path, "spaces"), b.Spaces, spaces, ConfluenceSpaceKey, "space",
		"a unit's or a root seat's `space`")
}

func (c *Company) checkGitHubAccess(p *problems, path Path, b *RoleGitHub) {
	if b == nil {
		return
	}
	for _, key := range sortedKeys(b.Permissions) {
		access := b.Permissions[key]
		here := at(at(path, "permissions"), key)
		switch {
		case !slices.Contains(GitHubPermissionKeys(), key):
			p.add(here, ErrUnknownValue,
				"%q is not a GitHub App permission an agent's app holds; give one of %s",
				key, strings.Join(GitHubPermissionKeys(), ", "))
		case !access.Valid():
			p.add(here, ErrUnknownValue,
				"%q is not a GitHub grant; give read or write, or leave the "+
					"permission out for No access", access)
		case key == GitHubPermWorkflows && access != GitHubWrite:
			p.add(here, ErrUnknownValue,
				"GitHub grants Workflows as write or not at all; give write, or "+
					"leave it out")
		}
	}
	var reach []string
	if gh := c.Integrations.GitHub; gh != nil && gh.Provisioning != nil {
		reach = gh.Provisioning.Repos
	}
	withinReach(p, at(path, "repos"), b.Repos, reach, strings.TrimSpace, "repository",
		"integrations.github.provisioning.repos")
}

func (c *Company) checkMattermost(p *problems, path Path, b *RoleMattermost) {
	if b == nil {
		return
	}
	if len(b.Roles) > 0 {
		held := normalRoleSet(b.Roles)
		offered := slices.ContainsFunc(MattermostRoleSets(), func(set []string) bool {
			return slices.Equal(normalRoleSet(set), held)
		})
		if !offered {
			p.add(at(path, "roles"), ErrUnknownValue,
				"%v is not a set of Mattermost roles this engine offers a bot; give "+
					"[system_user], [system_user, system_post_all_public], "+
					"[system_user, system_post_all] or [system_admin, system_user]", b.Roles)
		}
	}
	if SeatAccessOn(b.Enabled) && len(b.Roles) == 0 && c.levelRequired("mattermost") {
		p.add(at(path, "roles"), ErrMissing,
			"required while this agent is on: the Mattermost roles its bot holds")
	}
	var reach []string
	if mm := c.Integrations.Mattermost; mm != nil && mm.Provisioning != nil {
		reach = mm.Provisioning.Channels
	}
	withinReach(p, at(path, "channels"), b.Channels, reach, strings.TrimSpace, "channel",
		"integrations.mattermost.provisioning.channels")
}

func normalRoleSet(roles []string) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// EngineWrittenChanges is every engine-written field next changes from prior,
// as a document path: a GitHub App's `app_id`, `app_slug` and
// `installation_id`, which only the engine writes after GitHub has answered,
// and `seat_access_migrated`, which only the engine's migration writes.
//
// A WRITE RULE, NOT A DOCUMENT RULE: the document cannot say who wrote it, so
// the write path refuses a change here from any writer but the engine's own.
// Seats are matched by the derived handle the org model keys them by, and a
// seat new to the document is held to an empty record: a writer adding a seat
// with an app id would point a new agent at an app that is not its own. A
// seat removed whole is not a change to these fields.
func EngineWrittenChanges(prior, next *Company) []string {
	if prior == nil || next == nil {
		return nil
	}
	var out []string
	if !sameSet(prior.SeatAccessMigrated, next.SeatAccessMigrated) {
		out = append(out, field("seat_access_migrated").String())
	}
	before := map[string]*RoleGitHub{}
	for role := range prior.EachRole() {
		if handle := role.IdentityKey(); handle != "" {
			before[handle] = role.Integrations.GitHub
		}
	}
	for role, path := range next.EachRole() {
		var a, b RoleGitHub
		if was := before[role.IdentityKey()]; was != nil {
			a = *was
		}
		if now := role.Integrations.GitHub; now != nil {
			b = *now
		}
		here := at(at(path, "integrations"), "github")
		if a.AppID != b.AppID {
			out = append(out, at(here, "app_id").String())
		}
		if a.AppSlug != b.AppSlug {
			out = append(out, at(here, "app_slug").String())
		}
		if a.InstallationID != b.InstallationID {
			out = append(out, at(here, "installation_id").String())
		}
	}
	return out
}

// sameSet reports whether a and b hold the same entries, in any order.
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

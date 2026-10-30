package config

// PER-SEAT ACCESS, IN THE VENDOR'S OWN TERMS.
//
// Each agent seat carries one block per integration the engine manages an
// account on, under `roles[].integrations.<lane>`, and each block says three
// things: whether the seat is on there (`enabled`, a stored setting, never
// inferred from a variable's presence), what it may do (in the identifier the
// vendor's API takes, never a cross-vendor word), and where (a scope within
// the company block's reach). internal/seataccess reads these as its desired
// state and holds the vendor to them.
//
// THE SEAT'S CREDENTIALS ARE NOT HERE. The engine seals each under a name it
// derives (`<KIND>_<FIELD>_<HANDLE>`, seataccess.SinkName) and binds it to the
// seat's tool server itself, so no writer can point a seat's credential at a
// variable anything else reads (D4).
//
// The vendor vocabularies below are RESTATED rather than imported, because
// config is the leaf every vendor package depends on (the argument
// [CodeAccessTiers] makes); each vendor package owns the real catalogue, and a
// test holds the two to the same set.

// SeatAccessOn reports a block's stored setting: on only where it says so.
//
// `enabled` is a pointer in every block so an absent value is told from false.
// An absent value is refused on submit, because every block states it, and a
// stored document that lacks one reads as off (D9: a seat is off until turned
// on).
func SeatAccessOn(enabled *bool) bool { return enabled != nil && *enabled }

// SeatAccessLanes is every per-seat block, by its key under
// `roles[].integrations`.
func SeatAccessLanes() []string {
	return []string{"slack", "mattermost", "jira", "confluence", "github", "gitlab", "datadog"}
}

// seatEnabled is a seat's `enabled` for lane, and whether the block is there.
func (r *Role) seatEnabled(lane string) (*bool, bool) {
	in := r.Integrations
	switch lane {
	case "datadog":
		if in.Datadog != nil {
			return in.Datadog.Enabled, true
		}
	case "gitlab":
		if in.GitLab != nil {
			return in.GitLab.Enabled, true
		}
	case "jira":
		if in.Jira != nil {
			return in.Jira.Enabled, true
		}
	case "confluence":
		if in.Confluence != nil {
			return in.Confluence.Enabled, true
		}
	case "github":
		if in.GitHub != nil {
			return in.GitHub.Enabled, true
		}
	case "slack":
		if in.Slack != nil {
			return in.Slack.Enabled, true
		}
	case "mattermost":
		if in.Mattermost != nil {
			return in.Mattermost.Enabled, true
		}
	}
	return nil, false
}

// GitHubAccess is GitHub's grant on one App permission: read or write. Absent
// is GitHub's "No access".
type GitHubAccess string

// GitHub's two grants.
const (
	GitHubRead  GitHubAccess = "read"
	GitHubWrite GitHubAccess = "write"
)

// Valid reports whether a is a grant GitHub knows.
func (a GitHubAccess) Valid() bool { return a == GitHubRead || a == GitHubWrite }

// Rank is a grant's place in GitHub's order: no access, read, write.
func (a GitHubAccess) Rank() int {
	switch a {
	case GitHubRead:
		return 1
	case GitHubWrite:
		return 2
	default:
		return 0
	}
}

// The repository permissions a seat's GitHub App can hold, by GitHub's own
// keys. Metadata is not here: GitHub grants every App read on it.
const (
	GitHubPermContents     = "contents"
	GitHubPermIssues       = "issues"
	GitHubPermPullRequests = "pull_requests"
	GitHubPermChecks       = "checks"
	GitHubPermActions      = "actions"
	GitHubPermDeployments  = "deployments"
	GitHubPermWorkflows    = "workflows"
)

// GitHubPermissionKeys is the closed set, in GitHub's settings-page order.
func GitHubPermissionKeys() []string {
	return []string{
		GitHubPermContents, GitHubPermIssues, GitHubPermPullRequests,
		GitHubPermChecks, GitHubPermActions, GitHubPermDeployments, GitHubPermWorkflows,
	}
}

// GitLab's member roles, by the integer its Members API takes (D6). Security
// Manager and Minimal Access are deliberately absent: the first is a beta
// disabled by default outside GitLab.com, and the second grants an agent
// nothing by itself.
const (
	GitLabRoleGuest      = 10
	GitLabRolePlanner    = 15
	GitLabRoleReporter   = 20
	GitLabRoleDeveloper  = 30
	GitLabRoleMaintainer = 40
	GitLabRoleOwner      = 50
)

// GitLabRoles is the closed set, least access first.
func GitLabRoles() []int {
	return []int{
		GitLabRoleGuest, GitLabRolePlanner, GitLabRoleReporter,
		GitLabRoleDeveloper, GitLabRoleMaintainer, GitLabRoleOwner,
	}
}

// Mattermost's role names a bot account can hold, in its own spelling.
const (
	MattermostSystemUser          = "system_user"
	MattermostSystemPostAllPublic = "system_post_all_public"
	MattermostSystemPostAll       = "system_post_all"
	MattermostSystemAdmin         = "system_admin"
)

// MattermostRoleSets is every set of roles a seat's bot may hold, least access
// first: Member; Member with post:channels; Member with post:all; System
// Admin. Each is in Mattermost's own space-separated order.
func MattermostRoleSets() [][]string {
	return [][]string{
		{MattermostSystemUser},
		{MattermostSystemUser, MattermostSystemPostAllPublic},
		{MattermostSystemUser, MattermostSystemPostAll},
		{MattermostSystemAdmin, MattermostSystemUser},
	}
}

// RoleDatadog is a seat's Datadog service account.
type RoleDatadog struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has a Datadog service account. Off disables the account and keeps it."`

	// Role is a role id as Datadog's `GET /api/v2/roles` returns it, managed
	// or custom (D7). Checked against the organization by the Datadog pass:
	// an id Datadog no longer has is a finding, never a silent fallback.
	Role string `yaml:"role,omitempty" json:"role,omitempty" desc:"Datadog role id (GET /api/v2/roles), managed or custom."`
}

// RoleGitLab is a seat's GitLab service account.
type RoleGitLab struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has a GitLab service account. Off blocks the account and keeps it."`

	// AccessLevel is GitLab's integer: 10 Guest, 15 Planner, 20 Reporter,
	// 30 Developer, 40 Maintainer, 50 Owner. Zero is unset.
	AccessLevel int `yaml:"access_level,omitempty" json:"access_level,omitempty" js:"enum=10|15|20|30|40|50" desc:"GitLab role: 10 Guest, 15 Planner, 20 Reporter, 30 Developer, 40 Maintainer, 50 Owner."`

	// MemberRoleID is a custom role on GitLab Ultimate, held INSTEAD of a
	// default role. Checked against the group by the GitLab pass.
	MemberRoleID int64 `yaml:"member_role_id,omitempty" json:"member_role_id,omitempty" desc:"GitLab custom role id (Ultimate), instead of access_level."`

	// Projects narrow the account to these projects, as group/project;
	// empty is the whole group. Each must be in provisioning.projects where
	// that list is set.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty" desc:"Projects this agent is a member of; empty means the whole group."`
}

// RoleJira is a seat's access to Jira, through its Atlassian account.
type RoleJira struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has Jira access. Off removes its project roles and its token's Jira scopes, and keeps the account."`

	// ProjectRole is a project role id as the site's `GET /rest/api/3/role`
	// returns it (D8). What the role may do is each project's permission
	// scheme's, not the seat's. Empty is product access only: the account
	// has Jira and holds no project role, so the site's permission schemes
	// alone decide what it sees.
	ProjectRole string `yaml:"project_role,omitempty" json:"project_role,omitempty" desc:"Jira project role id (GET /rest/api/3/role); empty is product access only."`

	// Projects are the project keys the role is held in. Each must be a
	// project the company document names, and a list needs a ProjectRole.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty" desc:"Jira project keys this agent holds the role in."`
}

// RoleConfluence is a seat's access to Confluence, through its Atlassian
// account.
type RoleConfluence struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has Confluence access. Off removes its space access and its token's Confluence scopes, and keeps the account."`

	// SpaceRole is a space role id from the site's space roles (role-based
	// access, a beta at Atlassian).
	SpaceRole string `yaml:"space_role,omitempty" json:"space_role,omitempty" desc:"Confluence space role id, where the site has role-based access; with operations also empty, product access only."`

	// Operations are Confluence's space permissions by operation, as
	// `<operation>:<target>` (`read:space`, `create:page`), for a site whose
	// role-based access is off (D8). At most one of SpaceRole and Operations;
	// neither is product access only: the account has Confluence, and the
	// site's and each space's permissions alone decide what it sees.
	Operations []string `yaml:"operations,omitempty" json:"operations,omitempty" desc:"Confluence space permissions as operation:target, where the site has no space roles."`

	// Spaces are the space keys the role or permissions are held in. Each
	// must be a space the company document names, and a list needs a
	// SpaceRole or Operations.
	Spaces []string `yaml:"spaces,omitempty" json:"spaces,omitempty" desc:"Confluence space keys this agent holds the role in."`
}

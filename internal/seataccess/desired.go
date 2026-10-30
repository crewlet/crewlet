package seataccess

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
)

// The company document's per-seat blocks, read as [Desired] and written back.
//
// ONE MAPPING, both directions. The reconcile reads the document through
// [DesiredOf]; the access routes' save, the "Keep the vendor's setting" action
// and the migration write it through [SetDesired]; so the lane a pass holds the
// vendor to and the lane a save wrote cannot be two spellings of one setting.

// memberRolePrefix marks a GitLab custom role in a level: `member_role:<id>`.
const memberRolePrefix = "member_role:"

// Seat is one agent seat of the document with its blocks.
type Seat struct {
	SeatRef
	Role *config.Role
}

// SeatsOf is every agent seat in the document, in document order. A human
// seat is a person with their own accounts and is never provisioned.
func SeatsOf(c *config.Company) []Seat {
	if c == nil {
		return nil
	}
	var out []Seat
	for role := range c.EachRole() {
		// BY THE DERIVED HANDLE, the org model's key and the one every
		// sealed name is spelled from: a seat that declares none takes its
		// handle from its name.
		handle := strings.TrimSpace(role.IdentityKey())
		if role.Kind == org.KindHuman || handle == "" {
			continue
		}
		out = append(out, Seat{
			SeatRef: SeatRef{Handle: handle, Name: role.Name},
			Role:    role,
		})
	}
	return out
}

// OnAt reports whether the seat is on at any of kind's lanes.
func (s Seat) OnAt(kind integration.Kind) bool {
	for _, lane := range LanesOf(kind) {
		if s.Role.SeatAccessEnabled(string(lane)) {
			return true
		}
	}
	return false
}

// DesiredOf is what role's block for lane asks for, and whether the block is
// there at all. An absent block is off (D9), and so is a block whose
// `enabled` is unset; the document's rules refuse the second on submit.
func DesiredOf(role *config.Role, lane Lane) (Desired, bool) {
	if role == nil {
		return Desired{}, false
	}
	in := role.Integrations
	var d Desired
	switch lane {
	case LaneDatadog:
		b := in.Datadog
		if b == nil {
			return Desired{}, false
		}
		d = Desired{Enabled: config.SeatAccessOn(b.Enabled), Level: strings.TrimSpace(b.Role)}
	case LaneGitLab:
		b := in.GitLab
		if b == nil {
			return Desired{}, false
		}
		d = Desired{Enabled: config.SeatAccessOn(b.Enabled), Level: gitlabLevel(b), Scope: b.Projects}
	case LaneJira:
		b := in.Jira
		if b == nil {
			return Desired{}, false
		}
		d = Desired{Enabled: config.SeatAccessOn(b.Enabled), Level: strings.TrimSpace(b.ProjectRole), Scope: b.Projects}
	case LaneConfluence:
		b := in.Confluence
		if b == nil {
			return Desired{}, false
		}
		d = Desired{Enabled: config.SeatAccessOn(b.Enabled), Level: confluenceLevel(b), Scope: b.Spaces}
	case LaneMattermost:
		b := in.Mattermost
		if b == nil {
			return Desired{}, false
		}
		d = Desired{Enabled: config.SeatAccessOn(b.Enabled), Level: MattermostLevel(b.Roles), Scope: b.Channels}
	case LaneSlack:
		b := in.Slack
		if b == nil {
			return Desired{}, false
		}
		d = Desired{Enabled: config.SeatAccessOn(b.Enabled)}
	case LaneGitHub:
		b := in.GitHub
		if b == nil {
			return Desired{}, false
		}
		d = Desired{Enabled: config.SeatAccessOn(b.Enabled), Scope: b.Repos}
		if len(b.Permissions) > 0 {
			d.Permissions = Permissions(b.Permissions)
		}
	default:
		return Desired{}, false
	}
	return d.Normalized(), true
}

// SetDesired writes d into role's block for lane, creating the block where it
// is absent, and leaves every field the setting does not cover as it was: the
// engine-written GitHub app fields above all. The block's `enabled` is always
// written, so the document states it.
func SetDesired(role *config.Role, lane Lane, d Desired) error {
	if role == nil {
		return fmt.Errorf("seataccess: no seat to write the %s setting into", lane)
	}
	d = d.Normalized()
	on := d.Enabled
	in := &role.Integrations
	switch lane {
	case LaneDatadog:
		if in.Datadog == nil {
			in.Datadog = &config.RoleDatadog{}
		}
		in.Datadog.Enabled, in.Datadog.Role = &on, d.Level
	case LaneGitLab:
		if in.GitLab == nil {
			in.GitLab = &config.RoleGitLab{}
		}
		level, member, err := parseGitLabLevel(d.Level)
		if err != nil {
			return err
		}
		in.GitLab.Enabled, in.GitLab.AccessLevel, in.GitLab.MemberRoleID = &on, level, member
		in.GitLab.Projects = d.Scope
	case LaneJira:
		if in.Jira == nil {
			in.Jira = &config.RoleJira{}
		}
		in.Jira.Enabled, in.Jira.ProjectRole, in.Jira.Projects = &on, d.Level, d.Scope
	case LaneConfluence:
		if in.Confluence == nil {
			in.Confluence = &config.RoleConfluence{}
		}
		in.Confluence.Enabled, in.Confluence.Spaces = &on, d.Scope
		if strings.Contains(d.Level, ":") {
			in.Confluence.SpaceRole, in.Confluence.Operations = "", strings.Fields(d.Level)
		} else {
			in.Confluence.SpaceRole, in.Confluence.Operations = d.Level, nil
		}
	case LaneMattermost:
		if in.Mattermost == nil {
			in.Mattermost = &config.RoleMattermost{}
		}
		in.Mattermost.Enabled, in.Mattermost.Roles = &on, strings.Fields(d.Level)
		in.Mattermost.Channels = d.Scope
	case LaneSlack:
		if in.Slack == nil {
			in.Slack = &config.RoleSlack{}
		}
		in.Slack.Enabled = &on
	case LaneGitHub:
		if in.GitHub == nil {
			in.GitHub = &config.RoleGitHub{}
		}
		in.GitHub.Enabled, in.GitHub.Repos = &on, d.Scope
		in.GitHub.Permissions = nil
		if len(d.Permissions) > 0 {
			in.GitHub.Permissions = map[string]config.GitHubAccess(d.Permissions)
		}
	default:
		return fmt.Errorf("seataccess: %q is not a lane this build manages", lane)
	}
	return nil
}

// gitlabLevel is a GitLab block's level.
func gitlabLevel(b *config.RoleGitLab) string { return GitLabLevelID(b.AccessLevel, b.MemberRoleID) }

// GitLabLevelID is a GitLab level as one identifier: the custom role where
// memberRoleID names one (`member_role:<id>`), the access level otherwise
// (`30`), empty for neither. [GitLabLevel] reads it back.
func GitLabLevelID(accessLevel int, memberRoleID int64) string {
	switch {
	case memberRoleID != 0:
		return memberRolePrefix + strconv.FormatInt(memberRoleID, 10)
	case accessLevel != 0:
		return strconv.Itoa(accessLevel)
	default:
		return ""
	}
}

// GitLabLevel reads a GitLab level back: the access level, or the custom
// role's id. Exactly one is non-zero for a level that is set.
func GitLabLevel(level string) (accessLevel int, memberRoleID int64, err error) {
	return parseGitLabLevel(level)
}

func parseGitLabLevel(level string) (int, int64, error) {
	level = strings.TrimSpace(level)
	if level == "" {
		return 0, 0, nil
	}
	if id, ok := strings.CutPrefix(level, memberRolePrefix); ok {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, fmt.Errorf("seataccess: %q is not a GitLab custom role; give it as %s<id>", level, memberRolePrefix)
		}
		return 0, n, nil
	}
	n, err := strconv.Atoi(level)
	if err != nil || !slices.Contains(config.GitLabRoles(), n) {
		return 0, 0, fmt.Errorf("seataccess: %q is not a GitLab role; give one of 10, 15, 20, 30, 40, 50 or %s<id>", level, memberRolePrefix)
	}
	return n, 0, nil
}

// confluenceLevel is a Confluence block's level: its space role, or its space
// permissions as sorted `<operation>:<target>` entries.
func confluenceLevel(b *config.RoleConfluence) string {
	if role := strings.TrimSpace(b.SpaceRole); role != "" {
		return role
	}
	return strings.Join(normalSet(b.Operations), " ")
}

// MattermostLevel is a set of Mattermost roles as one level: in the order
// [config.MattermostRoleSets] lists them where the set is one it offers, and
// sorted otherwise (which the document's rules refuse), so two spellings of one
// set compare equal.
func MattermostLevel(roles []string) string {
	have := normalSet(roles)
	for _, set := range config.MattermostRoleSets() {
		if slices.Equal(normalSet(set), have) {
			return strings.Join(set, " ")
		}
	}
	return strings.Join(have, " ")
}

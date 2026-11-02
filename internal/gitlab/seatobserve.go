package gitlab

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// Reading a seat's GitLab access back, every pass.

// roster is the group's and the read projects' direct memberships, read once
// per pass and kept in step with this pass's own writes, so a second step
// that reads it sees what the first one did.
type roster struct {
	group    map[int]Access
	projects map[string]map[int]Access
}

// readRoster reads the group's members once, and each project in the pass's
// universe once: the company's project list, every project a seat's block
// names, and the extra lists a caller passes (the scope a seat held last).
// A project the instance does not have reads as empty, so a step that needs
// it is refused with GitLab's own 404.
func (s *session) readRoster(ctx context.Context, extra ...[]string) (*roster, error) {
	if s.roster == nil {
		members, err := s.client.GroupMembers(ctx, s.group.ID)
		if err != nil {
			return nil, refuse(s, fmt.Errorf("gitlab: list group members: %w", err))
		}
		r := &roster{group: map[int]Access{}, projects: map[string]map[int]Access{}}
		for _, m := range members {
			r.group[m.ID] = m.Access()
		}
		s.roster = r
	}
	for _, project := range s.universe(extra...) {
		if _, read := s.roster.projects[project]; read {
			continue
		}
		members, err := s.client.ProjectMembers(ctx, project)
		if err != nil && !isNotFound(err) {
			return nil, refuse(s, fmt.Errorf("gitlab: list %s's members: %w", project, err))
		}
		levels := map[int]Access{}
		for _, m := range members {
			levels[m.ID] = m.Access()
		}
		s.roster.projects[project] = levels
	}
	return s.roster, nil
}

// universe is every project whose memberships this pass reads.
func (s *session) universe(extra ...[]string) []string {
	all := slices.Clone(s.cfg.Provisioning.Projects)
	for _, block := range s.seats {
		all = append(all, block.Projects...)
	}
	for _, list := range extra {
		all = append(all, list...)
	}
	// The group's own path rides in an observed scope (see [observedScope])
	// and is not a project.
	all = slices.DeleteFunc(all, func(p string) bool { return p == s.group.FullPath })
	return normalScope(all)
}

// projectsOf is every read project the account is a direct member of.
func (r *roster) projectsOf(userID int) []string {
	var out []string
	for project, members := range r.projects {
		if _, ok := members[userID]; ok {
			out = append(out, project)
		}
	}
	slices.Sort(out)
	return out
}

// setGroup brings the account's group membership to access: added where
// absent, moved where it differs, left alone where it holds.
func (r *roster) setGroup(ctx context.Context, c *Client, groupID, userID int, access Access) error {
	current, member := r.group[userID]
	switch {
	case !member:
		if err := c.AddGroupMember(ctx, groupID, userID, access); err != nil {
			return err
		}
	case !current.Equal(access):
		if err := c.SetGroupMember(ctx, groupID, userID, access); err != nil {
			return err
		}
	default:
		return nil
	}
	r.group[userID] = access
	return nil
}

// setProject is [roster.setGroup] for one project.
func (r *roster) setProject(ctx context.Context, c *Client, project string, userID int, access Access) error {
	members := r.projects[project]
	current, member := members[userID]
	switch {
	case !member:
		if err := c.AddProjectMember(ctx, project, userID, access); err != nil {
			return err
		}
	case !current.Equal(access):
		if err := c.SetProjectMember(ctx, project, userID, access); err != nil {
			return err
		}
	default:
		return nil
	}
	if members == nil {
		members = map[int]Access{}
		r.projects[project] = members
	}
	members[userID] = access
	return nil
}

// dropGroup ends the account's direct group membership, where it has one.
func (r *roster) dropGroup(ctx context.Context, c *Client, groupID, userID int) error {
	if _, member := r.group[userID]; !member {
		return nil
	}
	if err := c.RemoveGroupMember(ctx, groupID, userID); err != nil {
		return err
	}
	delete(r.group, userID)
	return nil
}

// dropProject ends the account's direct membership of one project.
func (r *roster) dropProject(ctx context.Context, c *Client, project string, userID int) error {
	if _, member := r.projects[project][userID]; !member {
		return nil
	}
	if err := c.RemoveProjectMember(ctx, project, userID); err != nil {
		return err
	}
	delete(r.projects[project], userID)
	return nil
}

// Observe implements [seataccess.Session].
//
// Every document seat and every ledger record is read: a record whose seat
// left the document is what lets a pass reach an account nothing else would.
// A read that fails RAISES, because "no account" and "cannot tell" send the
// plan down opposite paths.
func (s *session) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	var extra [][]string
	for _, rec := range in.Records {
		if lane, ok := rec.Lanes[seataccess.LaneGitLab]; ok {
			extra = append(extra, lane.Observed.Scope)
			if lane.Applied != nil {
				extra = append(extra, lane.Applied.Scope)
			}
		}
	}
	r, err := s.readRoster(ctx, extra...)
	if err != nil {
		return nil, err
	}
	handles := map[string]bool{}
	for _, seat := range in.Seats {
		handles[seat.Handle] = true
	}
	for handle := range in.Records {
		handles[handle] = true
	}
	out := seataccess.Observation{}
	for _, handle := range slices.Sorted(maps.Keys(handles)) {
		var rec *seataccess.Record
		if held, ok := in.Records[handle]; ok {
			rec = &held
		}
		observed, err := s.observe(ctx, r, handle, rec)
		if err != nil {
			return nil, fmt.Errorf("gitlab: observe %s: %w", handle, err)
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneGitLab: observed}
	}
	return out, nil
}

// observe reads one seat's account, memberships and tokens.
func (s *session) observe(ctx context.Context, r *roster, handle string, rec *seataccess.Record) (seataccess.Observed, error) {
	user, found, err := s.account(ctx, handle, rec)
	if err != nil || !found {
		return seataccess.Observed{}, err
	}
	out := seataccess.Observed{
		Exists: true, Account: user.Username, AccountID: strconv.Itoa(user.ID),
	}
	if rec != nil && rec.Account != nil {
		out.CreatedAt = rec.Account.CreatedAt
	}
	var applied string
	if rec != nil {
		if lane, ok := rec.Lanes[seataccess.LaneGitLab]; ok && lane.Applied != nil {
			applied = lane.Applied.Level
		}
	}
	held := r.held(user.ID)
	out.Level = observedLevel(held, applied)
	out.Scope = observedScope(r, user.ID, s.group.FullPath)

	tokens, err := s.client.Tokens(ctx, s.mintGroup(), user.ID)
	if err != nil {
		return seataccess.Observed{}, fmt.Errorf("list tokens: %w", err)
	}
	newest, live := newestOwn(tokens, handle)
	switch {
	case !live || renewDue(newest.ExpiresAt.Time, time.Now()):
		// GONE, OR ABOUT TO BE: the account holds no token of this
		// engine's (revoked, or expired, which GitLab reports inactive), or
		// its token lapses inside the renewal window. Either way whatever
		// is sealed for the seat is, or soon will be, refused, so the plan
		// mints a replacement while the seat is on.
		out.CredentialGone = true
	case rec != nil && rec.Credential != nil:
		cred := *rec.Credential
		out.Credential = &cred
	}

	disabledByUs := rec != nil && rec.Held() && rec.Account.State == seataccess.AccountDisabled
	switch {
	case user.Blocked():
		out.DisabledBy = seataccess.DisabledByHand
		if disabledByUs {
			out.DisabledBy = seataccess.DisabledByCrewlet
		}
	case disabledByUs && !s.admin && len(held) == 0:
		// OFF THE WAY A GROUP OWNER TURNS IT OFF: no membership, on an
		// account the ledger says this engine disabled. Its tokens are
		// the credential's business ([seataccess.OpRevoke]), not the
		// account's state.
		out.DisabledBy = seataccess.DisabledByCrewlet
	default:
		out.Enabled = true
	}
	return out, nil
}

// renewWithin is how long before its expiry a token is replaced, so an agent
// never goes quiet on the day GitLab lapses it.
const renewWithin = 30 * 24 * time.Hour

// renewDue reports a token that expires inside the renewal window. A token
// with no expiry never is.
func renewDue(expires, now time.Time) bool {
	return !expires.IsZero() && expires.Sub(now) < renewWithin
}

// held is every membership the account holds that this pass reads, the
// group's first and then each project's in order.
func (r *roster) held(userID int) []Access {
	var out []Access
	if access, ok := r.group[userID]; ok {
		out = append(out, access)
	}
	for _, project := range r.projectsOf(userID) {
		out = append(out, r.projects[project][userID])
	}
	return out
}

// observedLevel is the role the account holds, as a stored level.
//
// One role everywhere is that role. Mixed roles are drift by definition, so
// the one reported is the first that differs from what the last sync
// applied, which is what makes the plan restore it. No membership at all is
// the empty level: the account holds no role.
func observedLevel(held []Access, applied string) string {
	if len(held) == 0 {
		return ""
	}
	first := held[0].LevelID()
	for _, access := range held {
		if id := access.LevelID(); id != applied {
			return id
		}
	}
	return first
}

// observedScope is where the account's memberships are, in [Desired]'s
// terms: empty for a membership of the whole group alone, otherwise each
// project, with the group's own path where the group membership rides
// alongside (a scope no document can ask for, so the plan narrows it).
func observedScope(r *roster, userID int, groupPath string) []string {
	projects := r.projectsOf(userID)
	_, groupMember := r.group[userID]
	if groupMember && len(projects) == 0 {
		return nil
	}
	if groupMember {
		projects = append(projects, groupPath)
		slices.Sort(projects)
	}
	return projects
}

package agentaccess

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// session is one pass's connection to the organization and both products.
type session struct {
	org        *atlassian.Client
	key, orgID string

	// jira and conf manage roles; nil where the product's block is absent
	// or did not resolve, with the reason in jiraMissing and confMissing.
	jira        *jira.Client
	conf        *confluence.Client
	jiraMissing string
	confMissing string

	// seats is the document's blocks per seat; jiraIn and confIn the
	// company's own projects and spaces, which bound a seat's scope.
	seats  map[string]seatDoc
	jiraIn []string
	confIn []string

	cache *cache

	// now stamps a mint and judges a token's expiry.
	now func() time.Time
}

// cache is what one pass reads once and keeps in step with its own writes.
type cache struct {
	site     *atlassian.Site
	accounts map[string]atlassian.ServiceAccount // by handle
	listed   bool

	jiraRoles     []jira.ProjectRole
	jiraRolesRead bool
	// roleMembers is each project role's direct accounts per project.
	roleMembers map[projectRole]map[string]bool

	mode         confluence.RoleMode
	modeRead     bool
	spaceRoles   []confluence.SpaceRole
	spaceRolesOK bool
	spaces       map[string]confluence.Space // by key; ID "" is a space the site lacks
	// spaceRole is the role an account holds in a space, by space id then
	// account; spacePerms the permissions it holds there.
	spaceRole  map[string]map[string]string
	spacePerms map[string]map[string][]confluence.SpacePermission
}

type projectRole struct {
	project string
	role    int64
}

func newCache() *cache {
	return &cache{
		accounts:    map[string]atlassian.ServiceAccount{},
		roleMembers: map[projectRole]map[string]bool{},
		spaces:      map[string]confluence.Space{},
		spaceRole:   map[string]map[string]string{},
		spacePerms:  map[string]map[string][]confluence.SpacePermission{},
	}
}

// doc is one seat's document blocks.
func (s *session) doc(handle string) seatDoc { return s.seats[handle] }

// site is the organization's site, discovered once per pass.
func (s *session) site(ctx context.Context) (atlassian.Site, error) {
	if s.cache.site == nil {
		site, err := s.org.DiscoverSite(ctx, s.key, s.orgID)
		if err != nil {
			return atlassian.Site{}, refuseOrg(s, err)
		}
		s.cache.site = site
	}
	return *s.cache.site, nil
}

// accountByHandle reads the organization's service accounts once, keyed by
// the marker this engine writes in each display name ([atlassian.HandleFrom]).
func (s *session) accountByHandle(ctx context.Context, handle string) (atlassian.ServiceAccount, bool, error) {
	if !s.cache.listed {
		all, err := s.org.ListServiceAccounts(ctx, s.key, s.orgID)
		if err != nil {
			return atlassian.ServiceAccount{}, false, refuseOrg(s, err)
		}
		for _, account := range all {
			if h := atlassian.HandleFrom(account.DisplayName); h != "" {
				s.cache.accounts[h] = account
			}
		}
		s.cache.listed = true
	}
	account, ok := s.cache.accounts[handle]
	return account, ok, nil
}

// account finds the seat's account: the ledger's id where it holds one,
// confirmed against the listing (Atlassian has no read of one service
// account), and the display-name marker otherwise.
func (s *session) account(ctx context.Context, handle string, rec *seataccess.Record) (atlassian.ServiceAccount, bool, error) {
	account, ok, err := s.accountByHandle(ctx, handle)
	if err != nil || !ok {
		return account, ok, err
	}
	if rec != nil && rec.Held() && rec.Account.ID != "" && rec.Account.ID != account.ID {
		// THE LEDGER'S ACCOUNT IS GONE and another carries the marker:
		// one somebody made by hand, or a duplicate. It is not adopted
		// silently in place of the one this engine recorded.
		for _, a := range s.cache.accounts {
			if a.ID == rec.Account.ID {
				return a, true, nil
			}
		}
		return atlassian.ServiceAccount{}, false, nil
	}
	return account, true, nil
}

// mustAccount is [session.account] for a step that cannot proceed without
// one.
func (s *session) mustAccount(ctx context.Context, in seataccess.ApplyInput) (atlassian.ServiceAccount, error) {
	account, ok, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil {
		return atlassian.ServiceAccount{}, err
	}
	if !ok {
		return atlassian.ServiceAccount{}, errNoAccount
	}
	return account, nil
}

// errNoAccount is a step that needs the seat's account and finds none, which
// the next pass resolves by creating it.
var errNoAccount = errors.New("atlassian: the seat has no account yet")

// Options implements [seataccess.Session].
func (s *session) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	switch lane {
	case seataccess.LaneJira:
		roles, err := s.jiraRoles(ctx)
		if err != nil {
			return seataccess.Options{}, err
		}
		out := seataccess.Options{Lane: lane, Live: true, Scope: slices.Clone(s.jiraIn)}
		out.Levels = append(out.Levels, productOnly(productJira))
		for _, role := range roles {
			out.Levels = append(out.Levels, seataccess.Level{
				ID: strconv.FormatInt(role.ID, 10), Name: role.Name,
				Administers: role.Administration(),
			})
		}
		out.ScopeOpen = len(out.Scope) == 0
		return out, nil
	case seataccess.LaneConfluence:
		mode, err := s.roleMode(ctx)
		if err != nil {
			return seataccess.Options{}, err
		}
		out := seataccess.Options{Lane: lane, Live: true, Scope: slices.Clone(s.confIn)}
		out.ScopeOpen = len(out.Scope) == 0
		if !mode.Roles() {
			// SPACE PERMISSIONS BY OPERATION, under Confluence's names,
			// on a site without role-based access (D8). A level is then a
			// set of these ([confluence.OperationsLevel]), chosen with a
			// multi-select.
			out.Live, out.Multiple = false, true
			out.RequiredLevel = confluence.OperationRead.ID()
			out.Levels = append(out.Levels, productOnly(productConfluence))
			for _, op := range confluence.Operations {
				out.Levels = append(out.Levels, seataccess.Level{
					ID: op.ID(), Name: op.Name,
					Administers: op.Operation == confluence.OperationAdminister,
				})
			}
			return out, nil
		}
		roles, err := s.spaceRoles(ctx)
		if err != nil {
			return seataccess.Options{}, err
		}
		out.Levels = append(out.Levels, productOnly(productConfluence))
		for _, role := range roles {
			out.Levels = append(out.Levels, seataccess.Level{
				ID: role.ID, Name: role.Name, Custom: role.Type == "CUSTOM",
				Administers: role.Administration(),
			})
		}
		return out, nil
	}
	return seataccess.Options{}, fmt.Errorf("atlassian: no lane %q", lane)
}

// needJira is the Jira client, or the refusal naming why there is none.
func (s *session) needJira() (*jira.Client, error) {
	if s.jira != nil {
		return s.jira, nil
	}
	said := "Jira is not connected, so its project roles cannot be managed"
	if s.jiraMissing != "" {
		said += ": " + s.jiraMissing
	}
	return nil, &seataccess.Refusal{
		Said:   said,
		Remedy: "Connect Jira with an administrator's credential, then retry.",
		Actor:  seataccess.ByYou,
	}
}

// needConfluence is [session.needJira] for Confluence.
func (s *session) needConfluence() (*confluence.Client, error) {
	if s.conf != nil {
		return s.conf, nil
	}
	said := "Confluence is not connected, so its space access cannot be managed"
	if s.confMissing != "" {
		said += ": " + s.confMissing
	}
	return nil, &seataccess.Refusal{
		Said:   said,
		Remedy: "Connect Confluence with an administrator's credential, then retry.",
		Actor:  seataccess.ByYou,
	}
}

// jiraRoles is the site's project roles, read once per pass.
func (s *session) jiraRoles(ctx context.Context) ([]jira.ProjectRole, error) {
	if s.cache.jiraRolesRead {
		return s.cache.jiraRoles, nil
	}
	client, err := s.needJira()
	if err != nil {
		return nil, err
	}
	roles, err := client.ProjectRoles(ctx)
	if err != nil {
		return nil, refuseJira(s, err)
	}
	s.cache.jiraRoles, s.cache.jiraRolesRead = roles, true
	return roles, nil
}

// members is one project role's direct accounts in one project, read once
// per pass. A project the site does not have reads as empty.
func (s *session) members(ctx context.Context, project string, role int64) (map[string]bool, error) {
	key := projectRole{project, role}
	if held, ok := s.cache.roleMembers[key]; ok {
		return held, nil
	}
	client, err := s.needJira()
	if err != nil {
		return nil, err
	}
	accounts, err := client.ProjectRoleMembers(ctx, project, role)
	if err != nil && jira.Status(err) != 404 {
		return nil, refuseJira(s, err)
	}
	held := make(map[string]bool, len(accounts))
	for _, id := range accounts {
		held[id] = true
	}
	s.cache.roleMembers[key] = held
	return held, nil
}

// roleMode is how the Confluence site grants space access, read once.
func (s *session) roleMode(ctx context.Context) (confluence.RoleMode, error) {
	if s.cache.modeRead {
		return s.cache.mode, nil
	}
	client, err := s.needConfluence()
	if err != nil {
		return "", err
	}
	mode, err := client.SpaceRoleMode(ctx)
	if err != nil {
		return "", refuseConfluence(s, err)
	}
	s.cache.mode, s.cache.modeRead = mode, true
	return mode, nil
}

// spaceRoles is the site's space roles, read once.
func (s *session) spaceRoles(ctx context.Context) ([]confluence.SpaceRole, error) {
	if s.cache.spaceRolesOK {
		return s.cache.spaceRoles, nil
	}
	client, err := s.needConfluence()
	if err != nil {
		return nil, err
	}
	roles, err := client.SpaceRoles(ctx)
	if err != nil {
		return nil, refuseConfluence(s, err)
	}
	s.cache.spaceRoles, s.cache.spaceRolesOK = roles, true
	return roles, nil
}

// space resolves a space key once per pass; found is false for a space the
// site does not have.
func (s *session) space(ctx context.Context, key string) (confluence.Space, bool, error) {
	if space, ok := s.cache.spaces[key]; ok {
		return space, space.ID != "", nil
	}
	client, err := s.needConfluence()
	if err != nil {
		return confluence.Space{}, false, err
	}
	space, found, err := client.SpaceByKey(ctx, key)
	if err != nil {
		return confluence.Space{}, false, refuseConfluence(s, err)
	}
	if !found {
		space = confluence.Space{Key: key}
	}
	s.cache.spaces[key] = space
	return space, found, nil
}

// universe is every project or space a lane's access is read in: the
// company's own, every seat's document scope, and the lists a caller adds
// (what a seat held or was applied last).
func (s *session) universe(lane seataccess.Lane, extra ...[]string) []string {
	var all []string
	if lane == seataccess.LaneJira {
		all = slices.Clone(s.jiraIn)
		for _, d := range s.seats {
			if d.jira != nil {
				all = append(all, d.jira.Projects...)
			}
		}
	} else {
		all = slices.Clone(s.confIn)
		for _, d := range s.seats {
			if d.confluence != nil {
				all = append(all, d.confluence.Spaces...)
			}
		}
	}
	for _, list := range extra {
		all = append(all, list...)
	}
	return normalKeys(lane, all)
}

// normalKeys is a lane's scope as keys: each spelled as the product matches it
// ([seataccess.ScopeKey]: a Confluence personal space's `~` key keeps its
// case, every other key is upper-cased), deduplicated and sorted.
func normalKeys(lane seataccess.Lane, keys []string) []string {
	seen := map[string]bool{}
	for _, key := range keys {
		if key = seataccess.ScopeKey(lane, key); key != "" {
			seen[key] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

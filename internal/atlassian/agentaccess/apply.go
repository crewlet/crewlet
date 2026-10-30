package agentaccess

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// adminConsoleURL is where a person acts on the organization's service
// accounts by hand.
const adminConsoleURL = "https://admin.atlassian.com"

// Apply implements [seataccess.Session].
func (s *session) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	p, ok := productOf(in.Lane)
	if !ok {
		return seataccess.StepResult{}, fmt.Errorf("atlassian: no lane %q", in.Lane)
	}
	// REDRAWN WITHOUT OPTIONS: they change a step's words, never which step
	// sits at an index.
	drawn := actions(in.Seat, p, in.Step.Op, in.Want, in.Have.Access(), seataccess.Options{})
	if in.Step.Index < 0 || in.Step.Index >= len(drawn) {
		return seataccess.StepResult{}, fmt.Errorf("atlassian: %s has no step %d",
			in.Step.Op, in.Step.Index)
	}
	return drawn[in.Step.Index].apply(s, ctx, in, p)
}

// create makes the seat's service account, or adopts the one carrying this
// engine's marker: one account serves both lanes, so the second lane's create
// finds the first one's.
func (s *session) create(ctx context.Context, in seataccess.ApplyInput, _ product) (seataccess.StepResult, error) {
	account, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if !found {
		created, err := s.org.CreateServiceAccount(ctx, s.key, s.orgID,
			atlassian.AccountName(in.Seat.Name, in.Seat.Handle), "")
		if err != nil {
			return seataccess.StepResult{}, refuseOrg(s, err)
		}
		account = *created
		s.cache.accounts[in.Seat.Handle] = account
	}
	return seataccess.StepResult{AccountID: account.ID, Account: accountName(account)}, nil
}

// grant gives the account the lane's product. Idempotent at Atlassian, so a
// lane turned back on grants again whether or not the grant had lapsed.
func (s *session) grant(ctx context.Context, in seataccess.ApplyInput, p product) (seataccess.StepResult, error) {
	account, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	site, err := s.site(ctx)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	err = s.org.Grant(ctx, s.key, s.orgID, account.ID, atlassian.GrantsOf(site.CloudID, p.grant))
	if errors.Is(err, atlassian.ErrAccountNotReady) {
		// A STATE THAT CLEARS ITSELF: Atlassian has not finished making a
		// new account grantable. A fault, so the sync retries rather than
		// sending a person to act.
		return seataccess.StepResult{}, fmt.Errorf("atlassian: %s: %w", in.Seat.Handle, err)
	}
	if err != nil {
		return seataccess.StepResult{}, refuseOrg(s, err)
	}
	return seataccess.StepResult{}, nil
}

// target is the projects or spaces a lane's role is held in: the seat's
// scope, or the company's own list where the scope is empty.
func (s *session) target(lane seataccess.Lane, scope []string) []string {
	if keys := normalKeys(lane, scope); len(keys) > 0 {
		return keys
	}
	if lane == seataccess.LaneJira {
		return normalKeys(lane, s.jiraIn)
	}
	return normalKeys(lane, s.confIn)
}

// converge brings the lane's roles to the document: the role in each
// project or space of the target, and nothing in any other this pass reads.
func (s *session) converge(ctx context.Context, in seataccess.ApplyInput, p product) (seataccess.StepResult, error) {
	account, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	target := s.target(p.lane, in.Want.Scope)
	universe := s.universe(p.lane, target, in.Have.Scope)
	if p.lane == seataccess.LaneJira {
		return seataccess.StepResult{}, s.convergeJira(ctx, account.ID, in.Want.Level, target, universe)
	}
	return seataccess.StepResult{}, s.convergeConfluence(ctx, account.ID, in.Want.Level, target, universe)
}

// convergeJira holds roleID in each target project and no role anywhere else
// in the universe. An empty roleID holds nothing anywhere.
func (s *session) convergeJira(ctx context.Context, accountID, level string, target, universe []string) error {
	client, err := s.needJira()
	if err != nil {
		return err
	}
	roles, err := s.jiraRoles(ctx)
	if err != nil {
		return err
	}
	var want int64
	if level != "" {
		want, err = strconv.ParseInt(level, 10, 64)
		if _, known := jira.ProjectRoleByID(roles, want); err != nil || !known {
			return &seataccess.Refusal{
				Said:   fmt.Sprintf("the site has no project role %q", level),
				Remedy: "Choose one of the site's project roles for this agent on the Agents tab.",
				Actor:  seataccess.ByYou, ActionURL: client.URL(),
			}
		}
	}
	for _, project := range universe {
		inTarget := level != "" && slices.Contains(target, project)
		for _, role := range roles {
			members, err := s.members(ctx, project, role.ID)
			if err != nil {
				return err
			}
			hold := inTarget && role.ID == want
			switch {
			case hold && !members[accountID]:
				if err := client.AddProjectRoleMember(ctx, project, role.ID, accountID); err != nil {
					return refuseJira(s, err)
				}
				members[accountID] = true
			case !hold && members[accountID]:
				if err := client.RemoveProjectRoleMember(ctx, project, role.ID, accountID); err != nil {
					return refuseJira(s, err)
				}
				delete(members, accountID)
			}
		}
	}
	return nil
}

// convergeConfluence holds the level in each target space and nothing in any
// other space of the universe: a space role where the site has role-based
// access, the set of space permissions otherwise. An empty level holds
// nothing anywhere.
func (s *session) convergeConfluence(ctx context.Context, accountID, level string, target, universe []string) error {
	client, err := s.needConfluence()
	if err != nil {
		return err
	}
	mode, err := s.roleMode(ctx)
	if err != nil {
		return err
	}
	var want []confluence.Operation
	if level != "" && mode.Roles() {
		roles, rolesErr := s.spaceRoles(ctx)
		if rolesErr != nil {
			return rolesErr
		}
		if _, known := confluence.SpaceRoleByID(roles, level); !known {
			return &seataccess.Refusal{
				Said:   fmt.Sprintf("the site has no space role %q", level),
				Remedy: "Choose one of the site's space roles for this agent on the Agents tab.",
				Actor:  seataccess.ByYou, ActionURL: client.URL(),
			}
		}
	}
	if level != "" && !mode.Roles() {
		if want, err = confluence.ParseOperationsLevel(level); err != nil {
			return &seataccess.Refusal{
				Said:   err.Error(),
				Remedy: "This site grants space permissions by operation; choose them for this agent on the Agents tab.",
				Actor:  seataccess.ByYou, ActionURL: client.URL(),
			}
		}
	}
	for _, key := range universe {
		inTarget := level != "" && slices.Contains(target, key)
		space, found, err := s.space(ctx, key)
		if err != nil {
			return err
		}
		if !found {
			if inTarget {
				return &seataccess.Refusal{
					Said:   "Confluence has no space " + key,
					Remedy: "Create the space or take it out of this agent's spaces, then retry.",
					Actor:  seataccess.ByYou, ActionURL: client.URL(),
				}
			}
			continue
		}
		if mode.Roles() {
			role := ""
			if inTarget {
				role = level
			}
			err = s.setSpaceRole(ctx, client, space, accountID, role)
		} else {
			var ops []confluence.Operation
			if inTarget {
				ops = want
			}
			err = s.setSpacePermissions(ctx, client, space, accountID, ops)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// setSpaceRole holds role in a space, or no role where it is empty.
func (s *session) setSpaceRole(ctx context.Context, client *confluence.Client, space confluence.Space, accountID, role string) error {
	current, err := s.spaceRoleOf(ctx, space.ID, accountID)
	if err != nil || current == role {
		return err
	}
	if err := client.SetSpaceRole(ctx, space.ID, accountID, role); err != nil {
		return refuseConfluence(s, err)
	}
	s.cache.spaceRole[space.ID][accountID] = role
	return nil
}

// setSpacePermissions holds exactly ops in a space. View goes in first and
// out last: Confluence requires it for every other permission and removes
// all of them with it.
func (s *session) setSpacePermissions(ctx context.Context, client *confluence.Client, space confluence.Space, accountID string, ops []confluence.Operation) error {
	held, err := s.spacePermsOf(ctx, space.ID, accountID)
	if err != nil {
		return err
	}
	has := func(op confluence.Operation) bool {
		return slices.ContainsFunc(held, func(p confluence.SpacePermission) bool { return p.Operation == op })
	}
	for _, named := range confluence.Operations {
		op := named.Operation
		if !slices.Contains(ops, op) || has(op) {
			continue
		}
		if err := client.AddSpacePermission(ctx, space.Key, accountID, op); err != nil {
			return refuseConfluence(s, err)
		}
	}
	removeRead := ""
	for _, p := range held {
		switch {
		case slices.Contains(ops, p.Operation):
		case p.Operation == confluence.OperationRead:
			removeRead = p.ID
		default:
			if err := client.RemoveSpacePermission(ctx, space.Key, p.ID); err != nil {
				return refuseConfluence(s, err)
			}
		}
	}
	if removeRead != "" {
		if err := client.RemoveSpacePermission(ctx, space.Key, removeRead); err != nil {
			return refuseConfluence(s, err)
		}
	}
	// THE SPACE IS READ AGAIN on its next use: an add returns no id this
	// pass could record, and a stale cache would remove what was just added.
	delete(s.cache.spacePerms, space.ID)
	return nil
}

// mintIfShort mints where the seat's live token does not carry exactly the
// lanes the document turns on: a create or an enable on an account whose
// other lane already minted one with both needs nothing.
func (s *session) mintIfShort(ctx context.Context, in seataccess.ApplyInput, p product) (seataccess.StepResult, error) {
	account, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	want := s.doc(in.Seat.Handle).lanes()
	tokens, err := s.org.ListTokens(ctx, s.key, account.ID)
	if err != nil {
		return seataccess.StepResult{}, refuseOrg(s, err)
	}
	if own := ownTokens(tokens, in.Seat.Handle, s.now()); len(own) > 0 && own[0].lanes == want {
		return seataccess.StepResult{Did: "Kept its API token: it already carries the scopes of " +
			lanesName(want)}, nil
	}
	return s.mintFor(ctx, in, account, want)
}

// mintAlways mints whatever the seat holds: a missing credential, or a
// rotation a person asked for.
func (s *session) mintAlways(ctx context.Context, in seataccess.ApplyInput, _ product) (seataccess.StepResult, error) {
	account, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	return s.mintFor(ctx, in, account, s.doc(in.Seat.Handle).lanes())
}

// mintFor issues a token carrying exactly the given lanes' scopes and hands
// it, with the account's address, to the sync to seal. The token's own
// revoke rides along, so a value that cannot be sealed is revoked rather
// than left live with nobody holding it.
func (s *session) mintFor(ctx context.Context, in seataccess.ApplyInput, account atlassian.ServiceAccount, on lanes) (seataccess.StepResult, error) {
	if !on.any() {
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said:   in.Seat.Handle + " is on neither Jira nor Confluence, so it holds no token",
			Remedy: "Turn the agent on for Jira or Confluence first.",
			Actor:  seataccess.ByYou,
		}
	}
	token, err := s.org.MintToken(ctx, s.key, account.ID, tokenLabel(in.Seat.Handle, on),
		atlassian.ScopesFor(on.jira, on.confluence), s.now())
	if err != nil {
		return seataccess.StepResult{}, refuseOrg(s, err)
	}
	id := token.ID
	minted := []seataccess.Minted{{
		Field: FieldToken, Value: token.Token, CredentialID: id,
		Revoke: func(ctx context.Context) error {
			return s.org.RevokeToken(ctx, s.key, account.ID, id)
		},
	}}
	if email := strings.TrimSpace(account.Email); email != "" {
		minted = append(minted, seataccess.Minted{Field: FieldEmail, Value: email})
	}
	return seataccess.StepResult{Minted: minted,
		Did: "Minted an API token with the scopes of " + lanesName(on)}, nil
}

// lanesName says which products a set of lanes is, for a step's sentence.
func lanesName(l lanes) string {
	switch {
	case l.jira && l.confluence:
		return "Jira and Confluence"
	case l.jira:
		return "Jira only"
	case l.confluence:
		return "Confluence only"
	}
	return "no product"
}

// retire revokes every token this engine minted for the seat except the
// newest, which is the one the sync has just sealed.
func (s *session) retire(ctx context.Context, in seataccess.ApplyInput, _ product) (seataccess.StepResult, error) {
	account, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil || !found {
		return seataccess.StepResult{}, err
	}
	tokens, err := s.org.ListTokens(ctx, s.key, account.ID)
	if err != nil {
		return seataccess.StepResult{}, refuseOrg(s, err)
	}
	// THE ONE THIS OP JUST SEALED is kept, by the id its mint reported;
	// failing that, the newest by the time its label carries.
	own := ownTokens(tokens, in.Seat.Handle, s.now())
	keep := in.PriorCredentialID(FieldToken)
	if keep == "" && len(own) > 0 {
		keep = own[0].ID
	}
	revoked := 0
	for _, t := range own {
		if t.ID == keep {
			continue
		}
		if err := s.org.RevokeToken(ctx, s.key, account.ID, t.ID); err != nil {
			return seataccess.StepResult{}, refuseOrg(s, err)
		}
		revoked++
	}
	return seataccess.StepResult{Did: fmt.Sprintf("Revoked %d earlier API token(s) Crewlet minted on it",
		revoked)}, nil
}

// disable takes the lane's roles away everywhere this pass reads. The
// account is disabled for the ledger only when no lane is left on: one
// account serves both products.
func (s *session) disable(ctx context.Context, in seataccess.ApplyInput, p product) (seataccess.StepResult, error) {
	account, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil || !found {
		return seataccess.StepResult{}, err
	}
	remaining := s.doc(in.Seat.Handle).lanes().without(p.lane)
	keeps := "; the account keeps its " + p.name + " product access, which Atlassian " +
		"offers no route to remove per product"
	if !s.connected(p.lane) {
		// SAID AS IT HAPPENED: with the product's site credential absent
		// there is nothing to read its roles with, so none were removed;
		// the token losing the product's scopes is what turns it off.
		return seataccess.StepResult{Disabled: !remaining.any(),
			Did: p.name + " is not connected, so its " + p.roleWord() + " could not be " +
				"read or removed; its token loses " + p.name + "'s scopes next" + keeps}, nil
	}
	universe := s.universe(p.lane, in.Have.Scope, in.Want.Scope)
	if p.lane == seataccess.LaneJira {
		err = s.convergeJira(ctx, account.ID, "", nil, universe)
	} else {
		err = s.convergeConfluence(ctx, account.ID, "", nil, universe)
	}
	if err != nil {
		return seataccess.StepResult{}, err
	}
	return seataccess.StepResult{Disabled: !remaining.any(),
		Did: "Removed its " + p.name + " " + p.roleWord() + " in every " + strings.TrimSuffix(p.where, "s") +
			" the company or this agent names" + keeps}, nil
}

// narrow replaces the seat's token with one that leaves out the lane being
// turned off, where the seat is still on the other product. Where it is on
// neither, the token is left for [seataccess.OpRevoke], which the plan takes
// once after the last lane's disable.
func (s *session) narrow(ctx context.Context, in seataccess.ApplyInput, p product) (seataccess.StepResult, error) {
	remaining := s.doc(in.Seat.Handle).lanes().without(p.lane)
	if !remaining.any() {
		return seataccess.StepResult{Did: "Left its API token for the revoke that follows: " +
			"it is on neither Jira nor Confluence"}, nil
	}
	account, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	return s.mintFor(ctx, in, account, remaining)
}

// revokeAll revokes every token this engine minted for the seat, expired ones
// included, and tells the sync the sealed token is dead. The address stays:
// it grants nothing by itself, and the account is kept.
func (s *session) revokeAll(ctx context.Context, in seataccess.ApplyInput, _ product) (seataccess.StepResult, error) {
	account, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if !found {
		return seataccess.StepResult{Revoked: []string{FieldToken}}, nil
	}
	tokens, err := s.org.ListTokens(ctx, s.key, account.ID)
	if err != nil {
		return seataccess.StepResult{}, refuseOrg(s, err)
	}
	// EXPIRED ONES TOO, which the zero clock admits: off leaves no token of
	// this engine's on the account at all.
	for _, t := range ownTokens(tokens, in.Seat.Handle, time.Time{}) {
		if revokeErr := s.org.RevokeToken(ctx, s.key, account.ID, t.ID); revokeErr != nil {
			return seataccess.StepResult{}, refuseOrg(s, revokeErr)
		}
	}
	return seataccess.StepResult{Revoked: []string{FieldToken},
		Did: "Revoked its API token; the account and its address are kept"}, nil
}

// remove deletes the service account, which is allowed only when the seat is
// off on both products: deleting it for one lane would take the other with
// it. The plan refuses it too ([seataccess.ErrStillOn]); this holds the
// document as the session read it to the same rule.
func (s *session) remove(ctx context.Context, in seataccess.ApplyInput, _ product) (seataccess.StepResult, error) {
	if on := s.doc(in.Seat.Handle).lanes(); on.any() {
		still := productJira.name
		if !on.jira {
			still = productConfluence.name
		}
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said: "the account also serves " + still + ", where this agent is still on",
			Remedy: "Turn the agent off on " + still + " too, then remove the account; " +
				"one Atlassian account serves both products.",
			Actor: seataccess.ByYou,
		}
	}
	account, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if found {
		if err := s.org.DeleteServiceAccount(ctx, s.key, account.ID); err != nil &&
			atlassian.Status(err) != http.StatusNotFound {
			return seataccess.StepResult{}, refuseOrg(s, err)
		}
		delete(s.cache.accounts, in.Seat.Handle)
	}
	return seataccess.StepResult{Gone: true, Revoked: []string{FieldToken, FieldEmail}}, nil
}

// refusal builds the sync's [seataccess.Refusal] from a vendor's status and
// own words, or returns the error unchanged where it is a fault (a transport
// failure, a 5xx or a 429), which may clear on its own.
func refusal(err error, status int, detail string, remedies map[int]string, actionURL string) error {
	if status == 0 || status >= http.StatusInternalServerError || status == http.StatusTooManyRequests {
		return err
	}
	remedy, ok := remedies[status]
	if !ok {
		remedy = "Correct what Atlassian refused, then retry."
	}
	if strings.TrimSpace(detail) == "" {
		detail = http.StatusText(status)
	}
	return &seataccess.Refusal{Said: detail, Remedy: remedy, Actor: seataccess.ByYou, ActionURL: actionURL}
}

// refuseOrg is a refusal from the organization's admin APIs.
func refuseOrg(_ *session, err error) error {
	var api *atlassian.APIError
	if !errors.As(err, &api) {
		return err
	}
	return refusal(err, api.Status, api.Detail, map[int]string{
		http.StatusUnauthorized: "Replace integrations.atlassian.api_key: Atlassian no longer accepts it.",
		http.StatusForbidden: "Use an organization API key created without scopes; Atlassian's " +
			"account management refuses a scoped key.",
		http.StatusNotFound: "Check that the account still exists in the organization, then retry.",
	}, adminConsoleURL)
}

// refuseJira is a refusal from the Jira site.
func refuseJira(s *session, err error) error {
	var api *jira.APIError
	if !errors.As(err, &api) {
		return err
	}
	url := ""
	if s.jira != nil {
		url = s.jira.URL()
	}
	return refusal(err, api.Status, api.Detail, map[int]string{
		http.StatusUnauthorized: "Replace integrations.jira.token: the site no longer accepts it.",
		http.StatusForbidden: "Give the Jira credential's account Administer Projects on these " +
			"projects (or make it a Jira administrator), then retry.",
		http.StatusNotFound: "Check that the project and the project role still exist on the site, then retry.",
	}, url)
}

// refuseConfluence is a refusal from the Confluence site.
func refuseConfluence(s *session, err error) error {
	var api *confluence.APIError
	if !errors.As(err, &api) {
		return err
	}
	url := ""
	if s.conf != nil {
		url = s.conf.URL()
	}
	return refusal(err, api.Status, api.Detail, map[int]string{
		http.StatusUnauthorized: "Replace integrations.confluence.token: the site no longer accepts it.",
		http.StatusForbidden: "Give the Confluence credential's account permission to manage " +
			"these spaces' access (space admin, or a Confluence administrator), then retry.",
		http.StatusNotFound: "Check that the space still exists on the site, then retry.",
	}, url)
}

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// GitLab's half of per-seat access (design sections 4 and 5.2, D6).
//
// Each agent seat that is on gets a service account, a direct membership at
// the seat's role in each project of its scope (or of the group, where its
// scope is empty), and one access token whose scopes follow that role. Every
// pass reads all of it back ([session.Observe]) and the shared plan decides
// what to change; this file says how each change is made at GitLab.

func init() { seataccess.Register(Vendor{}) }

// FieldToken is the one credential sealed per seat: the account's access
// token, under seataccess.SinkName(gitlab, TOKEN, handle).
const FieldToken = "TOKEN"

// Vendor implements [seataccess.Vendor] for GitLab.
type Vendor struct{}

// Kind implements [seataccess.Vendor].
func (Vendor) Kind() integration.Kind { return integration.KindGitLab }

// Catalog implements [seataccess.Vendor]. The seat's token reaches its tool
// server through the binding table in internal/config, not from here.
func (Vendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindGitLab,
		Credentials: []string{FieldToken},
		Lanes: []seataccess.LaneSpec{{
			Lane:       seataccess.LaneGitLab,
			Levels:     true,
			LiveLevels: true, // custom roles, on Ultimate
			Scoped:     true,
			CreatedBy:  seataccess.ByCrewlet,
			EnabledBy:  seataccess.ByCrewlet,
			Rotatable:  true,
		}},
	}
}

// LevelID is the stored form of what a membership holds, through the one
// formatter the document mapping uses ([seataccess.GitLabLevelID]): a default
// role's access level ("30"), or a custom role's "member_role:<id>".
func (a Access) LevelID() string {
	return seataccess.GitLabLevelID(a.Level, int64(a.MemberRoleID))
}

// ParseLevel reads a stored level through the one parser the document
// mapping uses, refusing an empty one: a seat that is on holds a role.
func ParseLevel(id string) (level, memberRoleID int, err error) {
	level, role, err := seataccess.GitLabLevel(id)
	if err == nil && level == 0 && role == 0 {
		err = errors.New("gitlab: no role is set")
	}
	return level, int(role), err
}

// levelName is GitLab's name for a stored level, for a step's text: the
// name the lane's Options carry (a custom role's is live at GitLab), GitLab's
// own name for a default role, and the id marked as missing where GitLab has
// no such role.
func levelName(id string, opts seataccess.Options) string {
	if strings.TrimSpace(id) == "" {
		return "no role"
	}
	if level, _, ok := opts.Level(id); ok {
		return level.Name
	}
	if level, role, err := ParseLevel(id); err == nil && role == 0 {
		name, _ := RoleName(level)
		return name
	}
	return id + " (not found at GitLab)"
}

// scopeText says where a seat's memberships are, in a step's words.
func scopeText(scope []string) string {
	if len(scope) == 0 {
		return "the group"
	}
	return strings.Join(scope, ", ")
}

// scopesText says which token scopes a level earns, for a step's text. A
// custom role's base level is live at GitLab, so it is described rather than
// listed.
func scopesText(id string) string {
	level, _, err := ParseLevel(id)
	if err != nil || level == 0 {
		return "the scopes its role allows"
	}
	return strings.Join(ScopesFor(level), " and ")
}

// sameScopesDrawn reports two stored levels whose tokens carry the same
// scopes, as far as can be told without asking GitLab: a custom role's base
// level is not known here, so a change to or from one is drawn as a re-mint
// and [session.mint] skips it where the scopes turn out equal.
func sameScopesDrawn(a, b string) bool {
	la, ra, errA := ParseLevel(a)
	lb, rb, errB := ParseLevel(b)
	if errA != nil || errB != nil || ra != 0 || rb != 0 {
		return a == b
	}
	return SameScopes(ScopesFor(la), ScopesFor(lb))
}

// action is one step of one op: how it is drawn and how it is taken. The
// same list backs [Vendor.Steps] and [session.Apply], so the step a preview
// shows and the step a sync takes at the same index cannot differ.
type action struct {
	verb  seataccess.StepVerb
	text  string
	note  string
	apply func(s *session, ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error)
}

// actions is the steps one op takes for one seat, in order. PURE: it reads
// only the document's setting, what the vendor showed, and the lane's Options,
// which change the words and never which steps there are.
func actions(seat seataccess.SeatRef, op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
	opts seataccess.Options,
) []action {
	role, where := levelName(want.Level, opts), scopeText(want.Scope)
	grant := action{
		verb:  seataccess.VerbGrant,
		text:  "Make it a member of " + where + " as " + role,
		apply: (*session).converge,
	}
	mint := action{
		verb:  seataccess.VerbMint,
		text:  "Mint its access token with " + scopesText(want.Level),
		apply: (*session).mint,
	}
	retire := action{
		verb:  seataccess.VerbRevoke,
		text:  "Revoke any earlier token Crewlet minted on it",
		apply: (*session).retire,
	}
	switch op {
	case seataccess.OpCreate:
		// RETIRE TOO: the create adopts an account this engine's naming
		// already finds (one made before the ledger), and its earlier
		// tokens must not outlive the one sealed now.
		return []action{{
			verb:  seataccess.VerbCreate,
			text:  "Create a GitLab service account for " + seatName(seat),
			apply: (*session).create,
		}, grant, mint, retire}
	case seataccess.OpEnable:
		// AT THE DOCUMENT'S ROLE AND SCOPE, as a create is, and with a new
		// token only where the seat holds none: off revoked it.
		steps := []action{{
			verb: seataccess.VerbEnable,
			text: "Turn its account back on",
			note: "GitLab unblocks it where Crewlet's token is an instance " +
				"administrator's; otherwise its memberships are restored.",
			apply: (*session).enable,
		}, grant}
		if have.Credential == nil {
			steps = append(steps, mint, retire)
		}
		return steps
	case seataccess.OpLevel:
		steps := []action{{
			verb:  seataccess.VerbRerole,
			text:  "Change its role from " + levelName(have.Level, opts) + " to " + role + " in " + where,
			apply: (*session).converge,
		}}
		if !sameScopesDrawn(have.Level, want.Level) {
			mint.text = "Mint a new access token with " + scopesText(want.Level) + ", which " + role + " allows"
			retire.text = "Revoke its previous token"
			steps = append(steps, mint, retire)
		}
		return steps
	case seataccess.OpScope:
		return []action{{
			verb:  seataccess.VerbScope,
			text:  scopeChange(have.Scope, want.Scope, role),
			apply: (*session).converge,
		}}
	case seataccess.OpMint:
		return []action{mint, retire}
	case seataccess.OpDisable:
		return []action{{
			verb: seataccess.VerbDisable,
			text: "Turn its account off",
			note: "GitLab blocks it where Crewlet's token is an instance " +
				"administrator's; otherwise its memberships are removed. The " +
				"account and its history are kept.",
			apply: (*session).disable,
		}}
	case seataccess.OpRevoke:
		return []action{{
			verb:  seataccess.VerbRevoke,
			text:  "Revoke the access tokens Crewlet minted on it",
			apply: (*session).revokeAll,
		}}
	case seataccess.OpRemove:
		return []action{{
			verb:  seataccess.VerbRemove,
			text:  "Delete the service account",
			note:  "GitLab keeps its contributions, attributed to the Ghost User.",
			apply: (*session).remove,
		}}
	case seataccess.OpRotate:
		return []action{{
			verb:  seataccess.VerbRotate,
			text:  "Mint a new access token with " + scopesText(want.Level),
			apply: (*session).mint,
		}, {
			verb:  seataccess.VerbRevoke,
			text:  "Revoke its previous token",
			apply: (*session).retire,
		}}
	}
	return nil
}

// scopeChange draws a scope move as the memberships it adds and removes.
func scopeChange(have, want []string, role string) string {
	if len(want) == 0 {
		return "Make it a member of the whole group as " + role +
			", in place of its project memberships"
	}
	var added, removed []string
	for _, p := range want {
		if !slices.Contains(have, p) {
			added = append(added, p)
		}
	}
	for _, p := range have {
		if !slices.Contains(want, p) {
			removed = append(removed, p)
		}
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "Add it to "+strings.Join(added, ", ")+" as "+role)
	}
	if len(removed) > 0 {
		parts = append(parts, "Remove it from "+strings.Join(removed, ", "))
	}
	if len(have) == 0 {
		parts = append(parts, "End its membership of the whole group")
	}
	if len(parts) == 0 {
		return "Bring its memberships to " + strings.Join(want, ", ")
	}
	return strings.Join(parts, "; ")
}

func seatName(seat seataccess.SeatRef) string {
	if strings.TrimSpace(seat.Name) != "" {
		return seat.Name
	}
	return seat.Handle
}

// Steps implements [seataccess.Vendor], naming what it can without the
// lane's Options. See [Vendor.StepsFor].
func (v Vendor) Steps(
	seat seataccess.SeatRef, lane seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	return v.StepsFor(seataccess.StepsInput{Seat: seat, Lane: lane, Op: op, Want: want, Have: have})
}

// StepsFor implements [seataccess.NamedSteps]: the steps, with every role
// named as GitLab names it.
func (Vendor) StepsFor(in seataccess.StepsInput) []seataccess.Step {
	if in.Lane != seataccess.LaneGitLab {
		return nil
	}
	op := in.Op
	drawn := actions(in.Seat, op, in.Want, in.Have, in.Options)
	out := make([]seataccess.Step, 0, len(drawn))
	for i, a := range drawn {
		out = append(out, seataccess.Step{
			Op: op, Verb: a.verb, Text: a.text, Note: a.note,
			By: seataccess.ByCrewlet, Index: i,
		})
	}
	return out
}

// Open implements [seataccess.Vendor].
func (Vendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("gitlab: no company document: %w", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, fmt.Errorf("gitlab: the company does not provision GitLab: %w",
			integration.ErrNotConfigured)
	}
	value := strings.TrimSpace
	if in.Env != nil {
		value = func(v string) string { return strings.TrimSpace(in.Env.Value(v)) }
	}
	token := strings.TrimSpace(in.Operator)
	if token == "" {
		token = value(cfg.Provisioning.AdminToken)
	}
	if token == "" {
		return nil, fmt.Errorf("gitlab: integrations.gitlab.provisioning.admin_token "+
			"did not resolve: %w", integration.ErrNotConfigured)
	}
	client, err := NewClient(ClientOptions{URL: value(cfg.URL), Token: token})
	if err != nil {
		return nil, fmt.Errorf("gitlab: %w: %w", err, integration.ErrNotConfigured)
	}
	group, found, err := client.GroupByPath(ctx, cfg.Provisioning.Group)
	if err != nil {
		return nil, fmt.Errorf("gitlab: resolve group %q: %w", cfg.Provisioning.Group,
			integration.Reject(err, Status(err)))
	}
	if !found {
		return nil, fmt.Errorf("gitlab: no group %q on this instance; every seat's "+
			"access is granted within it", cfg.Provisioning.Group)
	}
	admin, err := client.Administrator(ctx)
	if err != nil {
		return nil, fmt.Errorf("gitlab: read whether the provisioning token is an "+
			"administrator's: %w", integration.Reject(err, Status(err)))
	}
	return &session{
		client: client, cfg: cfg, group: group, admin: admin,
		mode:  Mode(cfg.Provisioning.ModeOrDefault()),
		seats: seatBlocks(in.Config),
	}, nil
}

// seatBlocks is each agent seat's gitlab block in the document, by handle.
func seatBlocks(cfg *config.Company) map[string]*config.RoleGitLab {
	out := map[string]*config.RoleGitLab{}
	for role := range cfg.EachRole() {
		if seat := role.Seat(); seat != nil && seat.IsAgent() && role.Integrations.GitLab != nil {
			out[seat.Handle()] = role.Integrations.GitLab
		}
	}
	return out
}

// Administrator reports whether this client's token belongs to an instance
// administrator, which is what blocking and unblocking need. GitLab serves
// `is_admin` on /user to an administrator and omits it for anybody else,
// which decodes as false.
func (c *Client) Administrator(ctx context.Context) (bool, error) {
	var out struct {
		IsAdmin bool `json:"is_admin"`
	}
	if err := c.get(ctx, "/user", nil, &out); err != nil {
		return false, err
	}
	return out.IsAdmin, nil
}

// session is one pass's connection to GitLab.
type session struct {
	client *Client
	cfg    *config.GitLab
	group  Group
	mode   Mode
	// admin is a provisioning token that may block and unblock. Read once
	// per pass, because it decides how off is done and how off is read.
	admin bool
	// seats is the document's gitlab block per seat, which decides the
	// projects whose memberships are read back.
	seats map[string]*config.RoleGitLab

	roster *roster

	roles          []MemberRole
	rolesAvailable bool
	rolesRead      bool
}

// mintGroup is the group tokens are minted through, or zero on the instance
// route: an account the group owns is minted on through the group, which is
// what a group Owner may call, and an instance account through the
// administrator's route.
func (s *session) mintGroup() int {
	if s.mode.Or() == ModeInstance {
		return 0
	}
	return s.group.ID
}

// memberRoles reads the custom roles once per pass: the group's on
// GitLab.com, the instance's on a self-managed instance.
func (s *session) memberRoles(ctx context.Context) ([]MemberRole, bool, error) {
	if s.rolesRead {
		return s.roles, s.rolesAvailable, nil
	}
	roles, available, err := s.client.MemberRoles(ctx, s.group.ID, false)
	if err == nil && !available {
		roles, available, err = s.client.MemberRoles(ctx, s.group.ID, true)
	}
	if err != nil {
		return nil, false, err
	}
	s.roles, s.rolesAvailable, s.rolesRead = roles, available, true
	return roles, available, nil
}

// Options implements [seataccess.Session].
func (s *session) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	if lane != seataccess.LaneGitLab {
		return seataccess.Options{}, fmt.Errorf("gitlab: no lane %q", lane)
	}
	out := seataccess.Options{Lane: lane}
	for _, role := range Roles {
		out.Levels = append(out.Levels, seataccess.Level{
			ID: strconv.Itoa(role.Level), Name: role.Name,
			Administers: Administration(role.Level),
		})
	}
	custom, available, err := s.memberRoles(ctx)
	if err != nil {
		return seataccess.Options{}, err
	}
	out.Live = available
	for _, role := range custom {
		out.Levels = append(out.Levels, seataccess.Level{
			ID:   Access{Level: role.BaseAccessLevel, MemberRoleID: role.ID}.LevelID(),
			Name: role.Name, Custom: true,
			Administers: Administration(role.BaseAccessLevel),
		})
	}
	out.Scope = slices.Clone(s.cfg.Provisioning.Projects)
	out.ScopeOpen = len(out.Scope) == 0
	return out, nil
}

// access resolves a stored level to what a membership is written with. A
// custom role GitLab no longer has is a refusal naming it, never a fallback
// to some default role.
func (s *session) access(ctx context.Context, id string) (Access, error) {
	level, roleID, err := ParseLevel(id)
	if err != nil {
		return Access{}, &seataccess.Refusal{
			Said:   err.Error(),
			Remedy: "Choose one of GitLab's roles for this agent on the Agents tab.",
			Actor:  seataccess.ByYou,
		}
	}
	if roleID == 0 {
		return Access{Level: level}, nil
	}
	roles, _, err := s.memberRoles(ctx)
	if err != nil {
		return Access{}, err
	}
	role, ok := MemberRoleByID(roles, roleID)
	if !ok {
		return Access{}, &seataccess.Refusal{
			Said: fmt.Sprintf("GitLab has no custom role %d, or this token "+
				"cannot read it", roleID),
			Remedy: "Choose another role for this agent, or recreate the custom role at GitLab.",
			Actor:  seataccess.ByYou, ActionURL: s.membersURL(),
		}
	}
	return Access{Level: role.BaseAccessLevel, MemberRoleID: role.ID}, nil
}

// membersURL is the group's members page, where a person acts on a
// membership or an account by hand.
func (s *session) membersURL() string {
	if s.group.FullPath == "" {
		return ""
	}
	return s.client.URL() + "/groups/" + s.group.FullPath + "/-/group_members"
}

// errNoAccount is a step that needs the seat's account and finds none, which
// the next pass resolves by creating it.
var errNoAccount = errors.New("gitlab: the seat has no account yet")

// account finds the seat's account: by the ledger's id where it has one, by
// the username this engine derives otherwise (an account created before the
// ledger, or by a step earlier in this sync).
func (s *session) account(ctx context.Context, handle string, rec *seataccess.Record) (User, bool, error) {
	if rec != nil && rec.Held() {
		if id, err := strconv.Atoi(rec.Account.ID); err == nil {
			return s.client.UserByID(ctx, id)
		}
	}
	user, found, err := s.named(ctx, handle)
	if err != nil || !found || !user.Bot {
		// ONLY A MACHINE ACCOUNT IS THE SEAT'S: a person who happens to
		// hold the derived username is never read as this engine's account,
		// and the create step says so instead.
		return User{}, false, err
	}
	return user, true, nil
}

// named reads the account holding the seat's derived username in full: the
// listing finds it, and the single-user read is the one that says whether it
// is a machine account ([User.Bot]).
func (s *session) named(ctx context.Context, handle string) (User, bool, error) {
	listed, found, err := s.client.UserByUsername(ctx, Username(s.cfg.Provisioning, handle))
	if err != nil || !found {
		return User{}, found, err
	}
	return s.client.UserByID(ctx, listed.ID)
}

// mustAccount is [session.account] for a step that cannot proceed without
// one.
func (s *session) mustAccount(ctx context.Context, in seataccess.ApplyInput) (User, error) {
	user, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil {
		return User{}, refuse(s, err)
	}
	if !found {
		return User{}, errNoAccount
	}
	return user, nil
}

// Apply implements [seataccess.Session].
func (s *session) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	if in.Lane != seataccess.LaneGitLab {
		return seataccess.StepResult{}, fmt.Errorf("gitlab: no lane %q", in.Lane)
	}
	// REDRAWN WITHOUT OPTIONS: they change a step's words, never which step
	// sits at an index.
	drawn := actions(in.Seat, in.Step.Op, in.Want, in.Have, seataccess.Options{})
	if in.Step.Index < 0 || in.Step.Index >= len(drawn) {
		return seataccess.StepResult{}, fmt.Errorf("gitlab: %s has no step %d",
			in.Step.Op, in.Step.Index)
	}
	return drawn[in.Step.Index].apply(s, ctx, in)
}

// create makes the seat's service account, or adopts the one this engine's
// naming finds already there.
func (s *session) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	username := Username(s.cfg.Provisioning, in.Seat.Handle)
	user, found, err := s.named(ctx, in.Seat.Handle)
	if err != nil {
		return seataccess.StepResult{}, refuse(s, err)
	}
	if found && !user.Bot {
		// ADOPTED ONLY WHERE IT IS A MACHINE ACCOUNT. A person holding the
		// username would otherwise be given this agent's memberships and
		// token, and be blocked when the agent is turned off.
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said: "the username " + username + " belongs to a GitLab account that is " +
				"not a service account, so it is not this agent's",
			Remedy: "Set integrations.gitlab.provisioning.username_prefix to a prefix no " +
				"person's username starts with, or rename that account at GitLab.",
			Actor: seataccess.ByYou, ActionURL: s.membersURL(),
		}
	}
	if !found {
		if s.mode.Or() == ModeInstance {
			user, err = s.client.CreateInstanceServiceAccount(ctx, seatName(in.Seat), username)
		} else {
			user, err = s.client.CreateServiceAccount(ctx, s.group.ID, seatName(in.Seat), username)
		}
		if err != nil {
			if nameReserved(err) {
				// A deletion GitLab has not finished: the next pass
				// creates it. A fault rather than a refusal, so the sync
				// reports a retry and nobody is sent to act.
				return seataccess.StepResult{}, fmt.Errorf("%w: %w", ErrNameReserved, err)
			}
			refused := refuse(s, err)
			var refusal *seataccess.Refusal
			if errors.As(refused, &refusal) {
				if remedy := createRemedy(s.mode, Status(err)); remedy != "" {
					refusal.Remedy = remedy
				}
			}
			return seataccess.StepResult{}, refused
		}
	}
	return seataccess.StepResult{
		AccountID: strconv.Itoa(user.ID), Account: user.Username,
	}, nil
}

// enable turns an account back on: unblocks one that is blocked (the plan
// reaches this only for an account this engine blocked, or one a person
// asked to turn back on, D3). Memberships and the token follow as their own
// steps.
func (s *session) enable(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	user, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if !user.Blocked() {
		return seataccess.StepResult{Did: user.Username + " was not blocked at GitLab; " +
			"its memberships are restored by the next step"}, nil
	}
	// THE ROUTE FOLLOWS THE STATE: GitLab undoes each kind of off with its
	// own administrator route, and refuses the others for it.
	var (
		turnOn func(context.Context, int) error
		did    string
	)
	switch strings.ToLower(user.State) {
	case "blocked":
		turnOn, did = s.client.UnblockUser, "Unblocked "+user.Username+" at GitLab"
	case "deactivated":
		turnOn, did = s.client.ActivateUser, "Reactivated "+user.Username+" at GitLab"
	case "banned":
		turnOn, did = s.client.UnbanUser, "Lifted the ban on "+user.Username+" at GitLab"
	default:
		// LDAP-BLOCKED, or a state this build does not know: only the
		// directory or a person at GitLab can lift it.
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said: user.Username + " is " + user.State + " at GitLab, which no API route " +
				"this engine holds can undo",
			Remedy: "Lift the " + user.State + " state where it was set (for ldap_blocked, " +
				"in the directory), then retry.",
			Actor: seataccess.ByYou, ActionURL: s.membersURL(),
		}
	}
	if err := turnOn(ctx, user.ID); err != nil {
		refused := refuse(s, err)
		var refusal *seataccess.Refusal
		if errors.As(refused, &refusal) && Status(err) == http.StatusForbidden {
			refusal.Remedy = "Turning " + user.Username + " back on from " + user.State +
				" needs an instance administrator's token; ask one to do it at GitLab, " +
				"then retry."
		}
		return seataccess.StepResult{}, refused
	}
	return seataccess.StepResult{Did: did}, nil
}

// converge brings the seat's memberships to the document's role and scope:
// the group where the scope is empty, otherwise each listed project and not
// the group, and no other project this pass reads.
func (s *session) converge(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	user, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	want, err := s.access(ctx, in.Want.Level)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	r, err := s.readRoster(ctx, in.Want.Scope, in.Have.Scope)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	scope := normalScope(in.Want.Scope)
	if len(scope) == 0 {
		if err := r.setGroup(ctx, s.client, s.group.ID, user.ID, want); err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
	}
	for _, project := range scope {
		if err := r.setProject(ctx, s.client, project, user.ID, want); err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
	}
	for _, project := range r.projectsOf(user.ID) {
		if slices.Contains(scope, project) {
			continue
		}
		if err := r.dropProject(ctx, s.client, project, user.ID); err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
	}
	if len(scope) > 0 {
		if err := r.dropGroup(ctx, s.client, s.group.ID, user.ID); err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
	}
	return seataccess.StepResult{}, nil
}

// mint issues the seat's token with the scopes its role earns, and proves it
// before handing it to the sync to seal.
//
// A TOKEN THAT ALREADY CARRIES THOSE SCOPES IS KEPT where the op is a role
// change: the step is drawn wherever the scopes might differ (a custom role's
// base level is only known here), and minting one that changes nothing would
// rotate a working credential for no reason.
func (s *session) mint(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	user, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	want, err := s.access(ctx, in.Want.Level)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	scopes := ScopesFor(want.Level)
	if in.Step.Op == seataccess.OpLevel {
		tokens, listErr := s.client.Tokens(ctx, s.mintGroup(), user.ID)
		if listErr != nil {
			return seataccess.StepResult{}, refuse(s, listErr)
		}
		if newest, ok := newestOwn(tokens, in.Seat.Handle); ok && SameScopes(newest.Scopes, scopes) {
			return seataccess.StepResult{Did: "Kept its access token: it already carries " +
				strings.Join(scopes, " and ") + ", the scopes its new role allows"}, nil
		}
	}
	group := s.mintGroup()
	token, err := s.client.CreateToken(ctx, group, user.ID, TokenName(in.Seat.Handle), scopes, noExpiry)
	if err != nil {
		return seataccess.StepResult{}, refuse(s, err)
	}
	revoke := func(ctx context.Context) error {
		return s.client.RevokeToken(ctx, group, user.ID, token.ID)
	}
	// THE FRESH TOKEN IS THE PROBE. If GitLab refuses a credential minted
	// seconds ago, the account itself cannot authenticate (an unconfirmed
	// address, see [Username]), and minting again on every pass is a loop
	// that once left 144 live tokens on one account, not a recovery.
	if verdict := s.client.verify(ctx, token.Value, user.ID); verdict == provision.VerdictRejected {
		if err := revoke(context.WithoutCancel(ctx)); err != nil {
			return seataccess.StepResult{}, fmt.Errorf("gitlab: %s cannot authenticate, "+
				"and the token just minted for it could not be revoked: revoke tokens "+
				"named %q on user %d at GitLab: %w",
				user.Username, TokenName(in.Seat.Handle), user.ID, err)
		}
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said: "GitLab refused a token minted seconds earlier, so the account " +
				user.Username + " cannot authenticate",
			Remedy: "Check the account at GitLab (its state and address), then retry.",
			Actor:  seataccess.ByYou, ActionURL: s.membersURL(),
		}
	}
	return seataccess.StepResult{Minted: []seataccess.Minted{{
		Field: FieldToken, Value: token.Value, Revoke: revoke,
		CredentialID: strconv.Itoa(token.ID),
	}}}, nil
}

// noExpiry mints at the instance's own default lifetime (a year on
// GitLab.com, which requires one). Nothing has to renew it on a schedule: a
// token that lapses stops being a live token of this engine's, the next pass
// reads the seat as holding no credential, and the plan mints one.
var noExpiry time.Time

// newestOwn is the most recent live token this engine minted for a seat.
func newestOwn(tokens []Token, handle string) (Token, bool) {
	var newest Token
	found := false
	for _, t := range tokens {
		if !ownLive(t, handle) {
			continue
		}
		if !found || t.ID > newest.ID {
			newest, found = t, true
		}
	}
	return newest, found
}

// ownLive reports a live token this engine minted for the seat. Tokens an
// administrator made by hand are named differently and never touched.
func ownLive(t Token, handle string) bool {
	return t.Name == TokenName(handle) && !t.Revoked && t.Active
}

// retire revokes every token this engine minted for the seat except the
// newest, which is the one the sync has just sealed.
func (s *session) retire(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	user, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	tokens, err := s.client.Tokens(ctx, s.mintGroup(), user.ID)
	if err != nil {
		return seataccess.StepResult{}, refuse(s, err)
	}
	// THE ONE THIS OP JUST SEALED is kept, by the id its mint reported;
	// failing that, the newest, which is the same token on an instance whose
	// ids only grow.
	keep, _ := newestOwn(tokens, in.Seat.Handle)
	if id, err := strconv.Atoi(in.PriorCredentialID(FieldToken)); err == nil {
		keep.ID = id
	}
	revoked := 0
	for _, t := range tokens {
		if t.ID == keep.ID || !ownLive(t, in.Seat.Handle) {
			continue
		}
		if err := s.client.RevokeToken(ctx, s.mintGroup(), user.ID, t.ID); err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
		revoked++
	}
	return seataccess.StepResult{Did: fmt.Sprintf("Revoked %d earlier token(s) Crewlet minted on %s",
		revoked, user.Username)}, nil
}

// disable turns the account off and keeps it (D1): blocked where this token
// may block, otherwise stripped of every membership this pass reads, which
// leaves a service account that can reach nothing once its tokens go.
func (s *session) disable(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	user, err := s.mustAccount(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if s.admin {
		if user.Blocked() {
			return seataccess.StepResult{Disabled: true,
				Did: user.Username + " was already blocked at GitLab"}, nil
		}
		if blockErr := s.client.BlockUser(ctx, user.ID); blockErr != nil {
			return seataccess.StepResult{}, refuse(s, blockErr)
		}
		return seataccess.StepResult{Disabled: true, Did: "Blocked " + user.Username + " at GitLab"}, nil
	}
	r, err := s.readRoster(ctx, in.Want.Scope, in.Have.Scope)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	dropped := r.projectsOf(user.ID)
	for _, project := range dropped {
		if err := r.dropProject(ctx, s.client, project, user.ID); err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
	}
	if _, member := r.group[user.ID]; member {
		if err := r.dropGroup(ctx, s.client, s.group.ID, user.ID); err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
		dropped = append([]string{s.group.FullPath}, dropped...)
	}
	// SAID AS IT HAPPENED: this token cannot block, so the account stays
	// active with no membership, which is what off means here.
	did := user.Username + " held no membership to remove"
	if len(dropped) > 0 {
		did = "Removed " + user.Username + "'s memberships in " + strings.Join(dropped, ", ")
	}
	return seataccess.StepResult{Disabled: true, Did: did + " (this GitLab token cannot block " +
		"accounts, so the account stays active with no access)"}, nil
}

// revokeAll revokes every token this engine minted for the seat, and tells
// the sync the sealed value is dead.
func (s *session) revokeAll(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	user, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil {
		return seataccess.StepResult{}, refuse(s, err)
	}
	revoked := 0
	if found {
		tokens, err := s.client.Tokens(ctx, s.mintGroup(), user.ID)
		if err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
		for _, t := range tokens {
			if !ownLive(t, in.Seat.Handle) {
				continue
			}
			if err := s.client.RevokeToken(ctx, s.mintGroup(), user.ID, t.ID); err != nil {
				return seataccess.StepResult{}, refuse(s, err)
			}
			revoked++
		}
	}
	return seataccess.StepResult{Revoked: []string{FieldToken},
		Did: fmt.Sprintf("Revoked %d access token(s) Crewlet minted", revoked)}, nil
}

// remove deletes the service account. GitLab keeps what it authored under
// the Ghost User, and the route is the one that matches the account's owner,
// so it can never remove a person.
func (s *session) remove(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	user, found, err := s.account(ctx, in.Seat.Handle, in.Record)
	if err != nil {
		return seataccess.StepResult{}, refuse(s, err)
	}
	if found {
		if s.mode.Or() == ModeInstance {
			err = s.client.DeleteInstanceServiceAccount(ctx, user.ID)
		} else {
			err = s.client.DeleteServiceAccount(ctx, s.group.ID, user.ID)
		}
		if err != nil {
			return seataccess.StepResult{}, refuse(s, err)
		}
	}
	return seataccess.StepResult{Gone: true, Revoked: []string{FieldToken}}, nil
}

// refuse turns a GitLab refusal into the sync's [seataccess.Refusal]: the
// vendor's own words, the remedy, and who acts. A transport failure, a 5xx
// or a 429 stays a fault, because it may clear on its own.
func refuse(s *session, err error) error {
	var api *APIError
	if !errors.As(err, &api) || api.Status >= http.StatusInternalServerError ||
		api.Status == http.StatusTooManyRequests {
		return err
	}
	var remedy string
	switch api.Status {
	case http.StatusUnauthorized:
		remedy = "Replace integrations.gitlab.provisioning.admin_token: GitLab no longer accepts it."
	case http.StatusForbidden:
		remedy = "Give the token's owner the Owner role on " + s.group.FullPath +
			" (blocking, unblocking and instance service accounts need an " +
			"instance administrator's token), then retry."
	case http.StatusNotFound:
		remedy = "Check that the project, group or account still exists at GitLab, then retry."
	default:
		remedy = "Correct what GitLab refused, then retry."
	}
	return &seataccess.Refusal{
		Said: refusalDetail(api), Remedy: remedy,
		Actor: seataccess.ByYou, ActionURL: s.membersURL(),
	}
}

// refusalDetail is GitLab's own words for a refusal, never a value this
// engine sent.
func refusalDetail(api *APIError) string {
	if d := strings.TrimSpace(api.Detail); d != "" {
		return d
	}
	return http.StatusText(api.Status)
}

func normalScope(scope []string) []string {
	var out []string
	for _, s := range scope {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// ErrNameReserved reports a username or email GitLab is still releasing from
// an account it is deleting.
//
// A STATE THAT CLEARS ITSELF, and the whole reason it is named: the pass has
// nothing to fix and nobody to tell, it simply has to be run again once
// GitLab's own deletion finishes. The caller reports it as work in progress
// rather than as a failure to read the integration.
var ErrNameReserved = errors.New(
	"gitlab is still releasing the name of an account it is deleting")

// nameReserved reports a create refused because GitLab has not yet released
// the username or address of an account it is deleting.
//
// GitLab removes a user asynchronously: the account is gone from every
// listing the moment the delete is accepted, and its username and email stay
// reserved until a background job finishes. So a removal followed by a fresh
// create inside that window honestly finds no account, creates one, and is
// refused with "has already been taken". That is a state that clears itself,
// not a refusal anybody acts on: see [ErrNameReserved].
func nameReserved(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusBadRequest &&
		strings.Contains(api.Detail, "already been taken")
}

// createRemedy is the sentence that names the credential a service-account
// create needs, which differs by where the account is owned.
//
// A 403 IS UNREADABLE ON ITS OWN here: group creation is satisfied by a group
// Owner and instance creation is not, so the same status means two different
// remedies. A 404 on the instance route means the same thing a third way:
// GitLab.com does not serve it at all. Empty where the status says nothing
// mode-specific.
func createRemedy(mode Mode, status int) string {
	forbidden := status == http.StatusForbidden || status == http.StatusUnauthorized
	switch {
	case mode.Or() != ModeInstance && forbidden:
		return "Creating a group service account needs a token whose owner is an " +
			"Owner of provisioning.group (or an instance administrator)."
	case mode.Or() == ModeInstance && forbidden:
		return "Instance mode creates service accounts the instance owns, which " +
			"only an instance administrator may do: use an administrator's token, " +
			"or set integrations.gitlab.provisioning.mode: group to create them " +
			"under provisioning.group."
	case mode.Or() == ModeInstance && status == http.StatusNotFound:
		return "This GitLab does not serve instance service accounts, which is " +
			"how GitLab.com answers: set integrations.gitlab.provisioning.mode: group."
	}
	return ""
}

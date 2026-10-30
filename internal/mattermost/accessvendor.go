package mattermost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Mattermost's half of per-seat access (design sections 4 and 5.2).
//
// ACCESS IS THE BOT'S OWN ROLES, BY MATTERMOST'S NAMES: Member, Member with
// post:channels, Member with post:all, or System Admin, stored as the role
// names Mattermost's API takes. SCOPE IS CHANNELS within the company's list.
// Every pass reads each bot back (its roles, its channels, whether it is
// deactivated and by whom, whether it holds a token) and the shared plan
// decides what to do about it: this file only says how each step is taken at
// Mattermost.

// BotTokenField is the one credential the engine seals per seat for
// Mattermost: the bot's personal access token, under
// MATTERMOST_BOT_TOKEN_<HANDLE>.
const BotTokenField = "BOT_TOKEN"

func init() { seataccess.Register(accessVendor{}) }

// accessVendor is Mattermost's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindMattermost }

// Catalog is Mattermost's per-seat offer. There is no tool binding: the
// engine's own chat transport reads the seat's token directly, so there is no
// server to bind it to ([config.SeatToolBindings]).
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindMattermost,
		Credentials: []string{BotTokenField},
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneMattermost, Levels: true, Scoped: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
			Rotatable: true,
		}},
	}
}

// Steps is each op as Mattermost takes it, in order. Pure.
func (accessVendor) Steps(
	seat seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	sink := seataccess.SinkName(integration.KindMattermost, BotTokenField, seat.Handle)
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	mint := step(seataccess.VerbMint, "Mint its access token and seal it as "+sink)
	var out []seataccess.Step
	switch op {
	case seataccess.OpCreate:
		out = []seataccess.Step{
			step(seataccess.VerbCreate, "Create a Mattermost bot for "+seatLabel(seat)),
			step(seataccess.VerbGrant, "Add it to the team as "+RolesLabel(levelRoles(want.Level))),
		}
		if len(want.Scope) > 0 {
			out = append(out, step(seataccess.VerbScope, "Add it to "+channelList(want.Scope)))
		}
		out = append(out, mint)
	case seataccess.OpEnable:
		out = []seataccess.Step{step(seataccess.VerbEnable, "Reactivate its Mattermost bot"), mint}
	case seataccess.OpLevel:
		out = []seataccess.Step{step(seataccess.VerbRerole,
			"Set its roles to "+RolesLabel(levelRoles(want.Level)))}
	case seataccess.OpScope:
		join, leave := scopeChange(want.Scope, have.Scope)
		var parts []string
		if len(join) > 0 {
			parts = append(parts, "add it to "+channelList(join))
		}
		if len(leave) > 0 {
			parts = append(parts, "remove it from "+channelList(leave))
		}
		if len(parts) == 0 {
			return nil
		}
		text := strings.Join(parts, " and ")
		out = []seataccess.Step{step(seataccess.VerbScope, strings.ToUpper(text[:1])+text[1:])}
	case seataccess.OpMint:
		out = []seataccess.Step{mint}
	case seataccess.OpDisable:
		deactivate := step(seataccess.VerbDisable, "Deactivate its Mattermost bot")
		deactivate.Note = "Mattermost keeps the bot and everything it posted, so turning it " +
			"back on needs no person."
		out = []seataccess.Step{deactivate}
	case seataccess.OpRevoke:
		out = []seataccess.Step{step(seataccess.VerbRevoke, "Revoke its access tokens and clear "+sink)}
	case seataccess.OpRemove:
		remove := step(seataccess.VerbRemove, "Stop managing its deactivated Mattermost bot")
		remove.Note = "The bot stays deactivated, because deleting it would delete " +
			"everything it posted."
		out = []seataccess.Step{remove}
	case seataccess.OpRotate:
		mint.Text = "Mint a new access token and seal it as " + sink
		out = []seataccess.Step{mint, step(seataccess.VerbRevoke, "Revoke the token it replaces")}
	default:
		return nil
	}
	for i := range out {
		out[i].Index = i
	}
	return out
}

func seatLabel(seat seataccess.SeatRef) string {
	if strings.TrimSpace(seat.Name) != "" {
		return seat.Name
	}
	return seat.Handle
}

// levelRoles is a stored level as its role list.
func levelRoles(level string) []string { return strings.Fields(level) }

func channelList(names []string) string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, "~"+name)
	}
	return strings.Join(out, ", ")
}

// scopeChange is the channels a move from have to want joins and leaves.
func scopeChange(want, have []string) (join, leave []string) {
	for _, name := range want {
		if !slices.Contains(have, name) {
			join = append(join, name)
		}
	}
	for _, name := range have {
		if !slices.Contains(want, name) {
			leave = append(leave, name)
		}
	}
	slices.Sort(join)
	slices.Sort(leave)
	return join, leave
}

// AdminToken is the token a Mattermost pass provisions with: the transient
// operator credential a person handed this run, or the company's
// provisioning.admin_token.
func AdminToken(cfg *config.Mattermost, env *config.Resolver, override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if cfg == nil || cfg.Provisioning == nil || env == nil {
		return ""
	}
	return strings.TrimSpace(env.Value(cfg.Provisioning.AdminToken))
}

// Open builds a session from the company's Mattermost block.
func (accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("mattermost: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, fmt.Errorf("mattermost: %w: integrations.mattermost is off or has no "+
			"provisioning block", integration.ErrNotConfigured)
	}
	if strings.TrimSpace(cfg.Team) == "" {
		return nil, fmt.Errorf("mattermost: %w: integrations.mattermost.team is unset, and "+
			"channels are team-scoped", integration.ErrNotConfigured)
	}
	admin := AdminToken(cfg, in.Env, in.Operator)
	if admin == "" {
		return nil, fmt.Errorf("mattermost: %w: no administrator token resolved, and the "+
			"bots' own tokens are what this pass mints", integration.ErrNotConfigured)
	}
	base := cfg.URL
	if in.Env != nil {
		base = in.Env.Value(cfg.URL)
	}
	client, err := NewClient(ClientOptions{URL: base, Token: admin})
	if err != nil {
		return nil, err
	}
	return &accessSession{client: client, cfg: cfg}, nil
}

// accessSession is one pass's connection to Mattermost.
type accessSession struct {
	client *Client
	cfg    *config.Mattermost

	// minted is the token id this session minted for each seat, by handle.
	// A rotation's second step needs it: it revokes every other token this
	// engine minted and never that one, and Mattermost lists no creation
	// time to tell them apart. A step that finds none revokes nothing.
	minted sync.Map

	teamOnce sync.Once
	team     Team
	teamErr  error
}

// teamID resolves the company's team once per session.
func (s *accessSession) teamID(ctx context.Context) (string, error) {
	s.teamOnce.Do(func() {
		team, found, err := s.client.TeamByName(ctx, s.cfg.Team)
		switch {
		case err != nil:
			s.teamErr = fmt.Errorf("mattermost: resolve team %q: %w", s.cfg.Team,
				integration.Reject(err, Status(err)))
		case !found:
			s.teamErr = &seataccess.Refusal{
				Said:   fmt.Sprintf("Mattermost has no team %q", s.cfg.Team),
				Remedy: "Name the team the bots belong to on the Mattermost connection",
				Actor:  seataccess.ByYou,
			}
		default:
			s.team = team
		}
	})
	return s.team.ID, s.teamErr
}

// companyChannels is the company's list a seat's channels must be within.
func (s *accessSession) companyChannels() []string {
	if s.cfg.Provisioning == nil {
		return nil
	}
	var out []string
	for _, name := range s.cfg.Provisioning.Channels {
		if name = strings.TrimSpace(name); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Options is Mattermost's four accesses and the company's channels.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	levels := make([]seataccess.Level, 0, len(roleOptions))
	for _, option := range roleOptions {
		levels = append(levels, seataccess.Level{
			ID: strings.Join(option.Roles, " "), Name: option.Label,
			Administers: option.Administration,
		})
	}
	scope := s.companyChannels()
	return seataccess.Options{
		Lane: lane, Levels: levels, Scope: scope, ScopeOpen: len(scope) == 0,
	}, nil
}

// Observe reads back every seat's bot: the document's seats and every seat
// the ledger still records.
//
// A BOT THE LEDGER RECORDS IS READ BY ID; a seat with no record is matched by
// the username this engine derives for it, all of them in one request, which
// is how a bot made before the ledger existed is adopted rather than
// duplicated (design section 8). Only a BOT is adopted: a person who happens
// to hold the username is somebody's account, never this engine's.
func (s *accessSession) Observe(
	ctx context.Context, in seataccess.ObserveInput,
) (seataccess.Observation, error) {
	teamID, err := s.teamID(ctx)
	if err != nil {
		return nil, err
	}
	handles := observedHandles(in)
	ids, err := s.botIDs(ctx, handles, in.Records)
	if err != nil {
		return nil, err
	}
	out := seataccess.Observation{}
	for _, handle := range handles {
		observed := seataccess.Observed{}
		if id := ids[handle]; id != "" {
			bot, found, err := s.client.ObserveBot(ctx, id, teamID)
			if err != nil {
				return nil, fmt.Errorf("mattermost: %s: %w", handle,
					integration.Reject(err, Status(err)))
			}
			if found {
				rec, recorded := in.Records[handle]
				observed = s.observedOf(bot, handle, rec, recorded)
			}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneMattermost: observed}
	}
	return out, nil
}

// botIDs is each handle's bot id: the ledger's, or the bot found under the
// derived username.
func (s *accessSession) botIDs(
	ctx context.Context, handles []string, records map[string]seataccess.Record,
) (map[string]string, error) {
	out := map[string]string{}
	byName := map[string]string{}
	var names []string
	for _, handle := range handles {
		if rec, ok := records[handle]; ok && rec.Held() {
			out[handle] = rec.Account.ID
			continue
		}
		name := BotUsername(s.cfg.Provisioning, handle)
		byName[name] = handle
		names = append(names, name)
	}
	if len(names) == 0 {
		return out, nil
	}
	users, err := s.client.UsersByUsernames(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("mattermost: look up the agents' bots: %w",
			integration.Reject(err, Status(err)))
	}
	for _, user := range users {
		if handle, ok := byName[NormalizeUsername(user.Username)]; ok && user.IsBot {
			out[handle] = user.ID
		}
	}
	return out, nil
}

// observedOf is one bot as the shared plan reads it.
func (s *accessSession) observedOf(
	bot BotAccount, handle string, rec seataccess.Record, recorded bool,
) seataccess.Observed {
	out := seataccess.Observed{
		Exists: true, Enabled: !bot.Deactivated,
		Account: "@" + bot.Username, AccountID: bot.ID,
		Level: seataccess.MattermostLevel(bot.Roles), Scope: s.scopeOf(bot),
	}
	if bot.Deactivated {
		// THIS ENGINE'S OFF, by either record of it: the ledger, or the
		// marker on the bot, which survives a lost ledger (D3).
		out.DisabledBy = seataccess.DisabledByHand
		if bot.DeactivatedHere || (recorded && rec.Account != nil &&
			rec.Account.State == seataccess.AccountDisabled) {
			out.DisabledBy = seataccess.DisabledByCrewlet
		}
	}
	if recorded && rec.Account != nil {
		out.CreatedAt = rec.Account.CreatedAt
	}
	return out
}

// scopeOf is the channels a bot is in, in the document's terms: those in the
// company's list where it has one, every channel in the team where it has
// none.
func (s *accessSession) scopeOf(bot BotAccount) []string {
	company := s.companyChannels()
	if len(company) == 0 {
		return slices.Clone(bot.Channels)
	}
	var out []string
	for _, name := range bot.Channels {
		if slices.Contains(company, name) {
			out = append(out, name)
		}
	}
	return out
}

// managedFor is the channels a scope change may leave: the company's list, or
// every channel the bot is in where the company lists none.
func (s *accessSession) managedFor(bot BotAccount) []string {
	if company := s.companyChannels(); len(company) > 0 {
		return company
	}
	return slices.Clone(bot.Channels)
}

// observedHandles is every handle a pass observes, the document's first, in a
// stable order.
func observedHandles(in seataccess.ObserveInput) []string {
	var out []string
	for _, seat := range in.Seats {
		if !slices.Contains(out, seat.Handle) {
			out = append(out, seat.Handle)
		}
	}
	var departed []string
	for handle := range in.Records {
		if !slices.Contains(out, handle) {
			departed = append(departed, handle)
		}
	}
	slices.Sort(departed)
	return append(out, departed...)
}

// Apply takes one step at Mattermost, chosen by its op and verb.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	teamID, err := s.teamID(ctx)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	if in.Step.Op == seataccess.OpCreate && in.Step.Verb == seataccess.VerbCreate {
		return s.create(ctx, in)
	}
	return s.withBot(ctx, in, teamID, func(bot BotAccount) (seataccess.StepResult, error) {
		switch in.Step.Op {
		case seataccess.OpCreate:
			switch in.Step.Verb {
			case seataccess.VerbGrant:
				return seataccess.StepResult{}, s.grant(ctx, in, teamID, bot)
			case seataccess.VerbScope:
				return seataccess.StepResult{}, s.scope(ctx, in, teamID, bot)
			default:
				return s.mint(ctx, in, bot, false)
			}
		case seataccess.OpEnable:
			if in.Step.Verb == seataccess.VerbEnable {
				return seataccess.StepResult{}, s.refused(in, s.client.TurnOn(ctx, bot))
			}
			return s.mint(ctx, in, bot, false)
		case seataccess.OpMint:
			return s.mint(ctx, in, bot, true)
		case seataccess.OpLevel:
			_, err := s.client.SetRoles(ctx, bot, levelRoles(in.Want.Level))
			return seataccess.StepResult{}, s.refused(in, err)
		case seataccess.OpScope:
			return seataccess.StepResult{}, s.scope(ctx, in, teamID, bot)
		case seataccess.OpDisable:
			if err := s.client.Deactivate(ctx, bot); err != nil {
				return seataccess.StepResult{}, s.refused(in, err)
			}
			return seataccess.StepResult{Disabled: true}, nil
		case seataccess.OpRevoke:
			if _, err := s.client.RevokeEngineTokens(ctx, bot, in.Seat.Handle); err != nil {
				return seataccess.StepResult{}, s.refused(in, err)
			}
			return seataccess.StepResult{Revoked: []string{BotTokenField}}, nil
		case seataccess.OpRemove:
			// NOTHING IS DELETED AT MATTERMOST: deleting a bot deletes
			// everything it posted. Off is checked again here rather
			// than trusted, so a remove can never leave a live bot
			// unmanaged.
			if bot.Deactivated && bot.DeactivatedHere && len(bot.MintedTokens(in.Seat.Handle)) == 0 {
				return seataccess.StepResult{}, nil
			}
			if err := s.client.TurnOff(ctx, bot, in.Seat.Handle); err != nil {
				return seataccess.StepResult{}, s.refused(in, err)
			}
			return seataccess.StepResult{Disabled: true, Revoked: []string{BotTokenField}}, nil
		case seataccess.OpRotate:
			if in.Step.Verb == seataccess.VerbMint {
				return s.mint(ctx, in, bot, false)
			}
			keep, _ := s.minted.Load(in.Seat.Handle)
			id, _ := keep.(string)
			_, err := s.client.RetireTokens(ctx, bot, in.Seat.Handle, id)
			return seataccess.StepResult{}, s.refused(in, err)
		default:
			return seataccess.StepResult{}, fmt.Errorf("mattermost: no step for %q", in.Step.Op)
		}
	})
}

// create makes the seat's bot.
func (s *accessSession) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	username := BotUsername(s.cfg.Provisioning, in.Seat.Handle)
	bot, err := s.client.CreateBot(ctx, username,
		BotDisplayName(s.cfg.Provisioning, seatLabel(in.Seat)))
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	return seataccess.StepResult{AccountID: bot.UserID, Account: "@" + bot.Username}, nil
}

// grant adds a new bot to the team and gives it the seat's roles.
func (s *accessSession) grant(
	ctx context.Context, in seataccess.ApplyInput, teamID string, bot BotAccount,
) error {
	member, err := s.client.TeamMember(ctx, teamID, bot.ID)
	if err != nil {
		return s.refused(in, err)
	}
	if !member {
		if err := s.client.AddTeamMember(ctx, teamID, bot.ID); err != nil {
			return s.refused(in, err)
		}
	}
	_, err = s.client.SetRoles(ctx, bot, levelRoles(in.Want.Level))
	return s.refused(in, err)
}

// scope moves a bot's channels to the seat's. A channel the team does not have
// is a refusal naming it: the seat names a room that is not there.
func (s *accessSession) scope(
	ctx context.Context, in seataccess.ApplyInput, teamID string, bot BotAccount,
) error {
	change, err := s.client.SetChannels(ctx, teamID, bot, in.Want.Scope, s.managedFor(bot))
	if err != nil {
		return s.refused(in, err)
	}
	if len(change.Missing) > 0 {
		return &seataccess.Refusal{
			Said: fmt.Sprintf("Team %q has no channel %s", s.cfg.Team,
				channelList(change.Missing)),
			Remedy: "Check the channel's URL name (not its display name), or take it off " +
				"this agent's channels",
			Actor: seataccess.ByYou,
		}
	}
	return nil
}

// mint creates one access token and hands it to the sync to seal.
//
// A TOKEN THE SYNC CANNOT SEAL IS REVOKED through its Revoke, which the sync
// calls with a context that outlives its own cancellation (design 5.2).
// replaceUnheld first revokes this engine's earlier tokens, which a seat being
// given a credential because nothing is sealed for it holds values nobody has.
func (s *accessSession) mint(
	ctx context.Context, in seataccess.ApplyInput, bot BotAccount, replaceUnheld bool,
) (seataccess.StepResult, error) {
	if replaceUnheld {
		if _, err := s.client.RevokeEngineTokens(ctx, bot, in.Seat.Handle); err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
	}
	token, err := s.client.CreateAccessToken(ctx, bot.ID, TokenDescription(in.Seat.Handle))
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	s.minted.Store(in.Seat.Handle, token.ID)
	tokenID := token.ID
	return seataccess.StepResult{Minted: []seataccess.Minted{{
		Field: BotTokenField, Value: token.Value,
		Revoke: func(ctx context.Context) error { return s.client.RevokeToken(ctx, tokenID) },
	}}}, nil
}

// withBot reads the step's bot fresh and runs fn on it, or answers Gone where
// Mattermost no longer has it.
//
// The bot is the ledger's, which the sync updates as soon as a create
// returns, so a create's later steps reach the bot the create made; one the
// pass adopted rather than created is the one it observed.
func (s *accessSession) withBot(
	ctx context.Context, in seataccess.ApplyInput, teamID string,
	fn func(BotAccount) (seataccess.StepResult, error),
) (seataccess.StepResult, error) {
	id := in.Have.AccountID
	if in.Record != nil && in.Record.Held() {
		id = in.Record.Account.ID
	}
	if id == "" {
		return seataccess.StepResult{}, fmt.Errorf(
			"mattermost: %s has no bot to %s", in.Seat.Handle, in.Step.Verb)
	}
	bot, found, err := s.client.ObserveBot(ctx, id, teamID)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	if !found {
		return seataccess.StepResult{Gone: true}, nil
	}
	return fn(bot)
}

// refused turns a Mattermost refusal into the shared [seataccess.Refusal],
// with the remedy and who acts; anything else stays a fault the sync retries.
func (s *accessSession) refused(in seataccess.ApplyInput, err error) error {
	if err == nil {
		return nil
	}
	var refusal *seataccess.Refusal
	if errors.As(err, &refusal) {
		return err
	}
	var api *Error
	if !errors.As(err, &api) || api.Status == http.StatusTooManyRequests || api.Status >= 500 {
		return err
	}
	said := api.Message
	if said == "" {
		said = fmt.Sprintf("the request was refused with status %d", api.Status)
	}
	return &seataccess.Refusal{
		Said: "Mattermost: " + said, Remedy: remedyFor(in.Step, api.Status),
		Actor: seataccess.ByYou, ActionURL: s.client.URL() + "/admin_console/user_management/users",
	}
}

// remedyFor is what a person does about a refusal, by what was refused.
func remedyFor(step seataccess.Step, status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Replace the administrator token on the Mattermost connection"
	case http.StatusForbidden:
		if step.Verb == seataccess.VerbCreate || step.Verb == seataccess.VerbMint {
			return "Use an administrator token from a System Admin, and check that " +
				"EnableBotAccountCreation and EnableUserAccessTokens are on in the " +
				"System Console, then retry"
		}
		return "Use an administrator token from a System Admin, then retry"
	case http.StatusNotFound:
		return "Check the team and channel names on the Mattermost connection, then retry"
	default:
		if step.Verb == seataccess.VerbCreate {
			return "Another account may already hold this agent's username; rename it or " +
				"change provisioning.username_prefix, then retry"
		}
		return "Settle what Mattermost names in the System Console, then retry"
	}
}

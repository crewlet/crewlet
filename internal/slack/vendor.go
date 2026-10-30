package slack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Slack's half of per-seat access ([seataccess.Vendor]).
//
// # No roles, and no scope per seat
//
// Each agent is its own app holding the bot scopes in [BotScopes], and what
// it reaches is decided per channel by who invites it. So a seat is on or
// off, and the scopes are shown, never chosen. The account the ledger records
// is the app (its id), and it is "there" once a workspace install has given
// it a bot token.
//
// # Drift is the manifest's
//
// The manifest is this engine's, not the document's, so drift is a change
// somebody made to the app in Slack's console. It is observed as a sentence in
// [seataccess.Observed.Level] (empty when the app matches), which the plan
// restores like any other drift (D2); there is no vendor setting to keep.

func init() { seataccess.Register(vendor{}) }

type vendor struct{}

// FieldClientID is the app's OAuth client id, sealed beside its secret: it is
// not a secret, and nothing else holds it once the app id is the ledger's.
const FieldClientID = "CLIENT_ID"

// ToolServer is the `mcp_servers` entry a seat's bot token is bound to, and
// ToolTokenEnv the variable that server reads it from.
const (
	ToolServer   = "slack"
	ToolTokenEnv = "SLACK_MCP_XOXB_TOKEN"
)

// Kind implements [seataccess.Vendor].
func (vendor) Kind() integration.Kind { return integration.KindSlack }

// Catalog implements [seataccess.Vendor].
func (vendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindSlack,
		Credentials: []string{FieldBotToken, FieldSigningSecret, FieldClientSecret, FieldClientID},
		// ONE TOKEN, TWO READERS: the transport reads the sealed bot token
		// directly, and the seat's Slack tool server is handed the same
		// one, so the two never hold different tokens (design 12.1).
		Tool: &seataccess.ToolBinding{
			Server: ToolServer,
			Keys:   []seataccess.BoundKey{{Field: FieldBotToken, Env: ToolTokenEnv}},
		},
		Lanes: []seataccess.LaneSpec{{
			Lane:      seataccess.LaneSlack,
			CreatedBy: seataccess.ByCrewlet,
			// AN INSTALL IS A PERSON'S, every time: back on and a rotate
			// both end in one.
			EnabledBy: seataccess.ByYou,
			Rotatable: true,
		}},
	}
}

// Scopes is what every seat's app holds, for the tab's read-only list.
func Scopes() []string { return slices.Clone(BotScopes) }

// Steps implements [seataccess.Vendor]. Pure: no call to Slack.
func (vendor) Steps(
	seat seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	_ seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	name := seatName(seat)
	var steps []seataccess.Step
	add := func(step seataccess.Step) {
		step.Op, step.Index = op, len(steps)
		steps = append(steps, step)
	}
	install := seataccess.Step{
		Verb: seataccess.VerbApprove, By: seataccess.ByYou,
		Text: "Approve " + name + "'s app in the workspace",
		Note: "Installing an app is a person's decision at Slack.",
	}
	switch op {
	case seataccess.OpCreate:
		add(seataccess.Step{
			Verb: seataccess.VerbCreate, By: seataccess.ByCrewlet,
			Text: "Create " + name + "'s Slack app from its manifest",
			Note: "Its bot scopes are " + strings.Join(BotScopes, ", ") + ".",
		})
		add(install)
	case seataccess.OpEnable, seataccess.OpMint:
		add(install)
	case seataccess.OpLevel:
		add(seataccess.Step{
			Verb: seataccess.VerbRestore, By: seataccess.ByCrewlet,
			Text: "Restore " + name + "'s app manifest",
			Note: have.Level,
		})
		if driftNeedsReinstall(have.Level) {
			add(install)
		}
	case seataccess.OpDisable:
		add(seataccess.Step{
			Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
			Text: "Revoke " + name + "'s bot token",
			Note: "The app is kept. Turning it back on needs a person to approve the install again.",
		})
	case seataccess.OpRotate:
		add(seataccess.Step{
			Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
			Text: "Revoke " + name + "'s bot token",
			Note: "Slack issues a new bot token only on an install, so the agent is off until it is approved.",
		})
		add(install)
	case seataccess.OpRemove:
		add(seataccess.Step{
			Verb: seataccess.VerbRemove, By: seataccess.ByCrewlet,
			Text: "Delete " + name + "'s Slack app",
		})
	}
	return steps
}

// Open implements [seataccess.Vendor].
//
// THE CONFIGURATION TOKEN IS MADE USABLE HERE, before any step: a held token
// with life left is used as it is, and an expiring one is rotated and sealed
// through [seataccess.OpenInput.Sink] before use ([Admin.ConfigToken]). A
// preview has no sink and rotates nothing, so it reads with a token that is
// still good or observes without the manifest. A company that has sealed no
// token is not refused: its seats are observed, and the steps that need the
// token say so with the remedy.
func (vendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || in.Config.Integrations.Slack == nil {
		return nil, fmt.Errorf("slack: %w", integration.ErrNotConfigured)
	}
	sessionIn := sessionInput{
		admin: NewAdmin(nil),
		base:  strings.TrimSpace(in.WebhookBase),
		seats: map[string]seatDoc{},
	}
	if in.Env != nil {
		sessionIn.resolve = in.Env.Value
	}
	for role := range in.Config.EachRole() {
		seat := role.Seat()
		if seat == nil || !seat.IsAgent() || role.Integrations.Slack == nil {
			continue
		}
		sessionIn.seats[seat.Handle()] = seatDoc{name: seat.Name, block: role.Integrations.Slack}
	}
	sessionIn.configToken, sessionIn.noToken = openConfigToken(ctx, sessionIn.admin, in)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newSession(sessionIn), nil
}

// openConfigToken is the usable configuration token for a session, or why
// there is none. Only a context that is done is an error the caller sees;
// every other failure is the reason the steps needing the token report.
func openConfigToken(ctx context.Context, admin *Admin, in seataccess.OpenInput) (string, error) {
	held := ConfigToken{}
	if in.Env != nil {
		raw, _ := in.Env.LookupOK(ConfigTokenVar)
		decoded, err := DecodeConfigToken(raw)
		if err != nil {
			return "", err
		}
		held = decoded
	}
	if held.Usable(time.Now()) {
		return held.Token, nil
	}
	if in.Sink == nil {
		return "", errors.New("slack: the app-configuration token needs rotating, " +
			"which a preview does not do")
	}
	return admin.ConfigToken(ctx, SinkConfigTokenStore{Sink: in.Sink, Var: ConfigTokenVar},
		"", time.Now(), false)
}

// sessionInput is what a session is built from.
type sessionInput struct {
	admin *Admin

	// configToken is a usable app-configuration token, "" where none is,
	// with noToken saying why.
	configToken string
	noToken     error

	// base is the deployment's public webhook base, "" where unknown.
	base string

	resolve func(ref string) string
	seats   map[string]seatDoc
	http    *http.Client
}

// seatDoc is one agent seat's Slack block as the document holds it.
type seatDoc struct {
	name  string
	block *config.RoleSlack
}

// session is one pass's connection to Slack.
type session struct {
	sessionInput
	cached map[string]Observed
}

func newSession(in sessionInput) *session {
	return &session{sessionInput: in, cached: map[string]Observed{}}
}

// Options implements [seataccess.Session]: nothing to choose, per seat.
func (s *session) Options(context.Context, seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: seataccess.LaneSlack}, nil
}

// Observe implements [seataccess.Session].
func (s *session) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	for _, handle := range observedHandles(in) {
		record := recordOf(in.Records, handle)
		seat := s.seatApp(handle, record)
		obs, err := ObserveSeat(ctx, s.admin, s.configToken, s.base, s.http, seat)
		if err != nil {
			return nil, fmt.Errorf("slack: observe %s: %w", handle, err)
		}
		s.cached[handle] = obs
		out[handle] = map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneSlack: observed(seat, obs, record),
		}
	}
	return out, nil
}

// observed is one seat's observation in the contract's terms.
//
// THE ACCOUNT IS THERE ONCE AN INSTALL GAVE IT A TOKEN. An app created and
// never installed has no bot to act as, so it observes as no account and the
// plan draws the create, whose first step is already done and whose second is
// the install a person still owes. A token sealed and refused by Slack is an
// account switched off: by this engine where the ledger says so, by somebody
// at Slack otherwise (D3).
func observed(seat SeatApp, obs Observed, record *seataccess.Record) seataccess.Observed {
	out := seataccess.Observed{AccountID: seat.AppID, Account: obs.BotName}
	if record != nil && record.Account != nil {
		out.CreatedAt = record.Account.CreatedAt
	}
	if !obs.AppExists || strings.TrimSpace(seat.BotToken) == "" {
		return out
	}
	out.Exists, out.Enabled = true, obs.TokenWorks
	if !out.Enabled {
		out.DisabledBy = seataccess.DisabledByHand
		if record != nil && record.Account != nil && record.Account.State == seataccess.AccountDisabled {
			out.DisabledBy = seataccess.DisabledByCrewlet
		}
	}
	out.Level = obs.Diff.Describe()
	out.Credential = &seataccess.Credential{
		Variable: SeatVar(seat.Handle, FieldBotToken),
		Others:   []string{SeatVar(seat.Handle, FieldSigningSecret)},
	}
	if record != nil && record.Credential != nil {
		out.Credential.MintedAt, out.Credential.By = record.Credential.MintedAt, record.Credential.By
	}
	return out
}

// Apply implements [seataccess.Session].
func (s *session) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	seat := s.seatApp(in.Seat.Handle, in.Record)
	if in.Step.By == seataccess.ByYou {
		return s.awaitInstall(ctx, seat)
	}
	switch in.Step.Op {
	case seataccess.OpCreate:
		return s.applyCreate(ctx, seat)
	case seataccess.OpLevel:
		if err := s.needs(true); err != nil {
			return seataccess.StepResult{}, err
		}
		if err := restoreManifest(ctx, s.admin, s.configToken, s.base, seat); err != nil {
			return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRestore)
		}
		return seataccess.StepResult{}, nil
	case seataccess.OpDisable, seataccess.OpRotate:
		if err := revokeToken(ctx, s.http, seat.BotToken); err != nil {
			return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRevoke)
		}
		return seataccess.StepResult{
			Revoked:  []string{FieldBotToken},
			Disabled: in.Step.Op == seataccess.OpDisable,
		}, nil
	case seataccess.OpRemove:
		return s.applyRemove(ctx, seat)
	}
	return seataccess.StepResult{}, fmt.Errorf("slack: no step for %q", in.Step.Op)
}

// applyCreate creates the seat's app, unless the ledger already records one
// Slack still has. The values Slack serves once go back to the sync to seal,
// each carrying the undo that deletes the app again.
func (s *session) applyCreate(ctx context.Context, seat SeatApp) (seataccess.StepResult, error) {
	if seat.AppID != "" && seat.ClientSecret != "" {
		obs, err := s.observe(ctx, seat)
		if err != nil {
			return seataccess.StepResult{}, err
		}
		if obs.AppExists {
			return seataccess.StepResult{AccountID: seat.AppID, Account: seat.RoleName}, nil
		}
	}
	if err := s.needs(true); err != nil {
		return seataccess.StepResult{}, err
	}
	created, err := createApp(ctx, s.admin, s.configToken, s.base, seat)
	if err != nil {
		return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbCreate)
	}
	undo := func(ctx context.Context) error {
		return s.admin.DeleteApp(ctx, s.configToken, created.AppID)
	}
	return seataccess.StepResult{
		AccountID: created.AppID, Account: seat.RoleName,
		Minted: []seataccess.Minted{
			{Field: FieldClientSecret, Value: created.ClientSecret, Revoke: undo},
			{Field: FieldSigningSecret, Value: created.SigningSecret, Revoke: undo},
			// GRANTS NOTHING BY ITSELF: an id, useless without the
			// secret beside it.
			{Field: FieldClientID, Value: created.ClientID},
		},
	}, nil
}

// awaitInstall is every step a person takes at Slack: the install, again
// after off or a rotate. Done once the sealed token works; otherwise it waits
// with the install link. The landing that link returns to seals the token
// ([CompleteSeatInstall]).
func (s *session) awaitInstall(ctx context.Context, seat SeatApp) (seataccess.StepResult, error) {
	if strings.TrimSpace(seat.BotToken) != "" {
		obs, err := s.observe(ctx, seat)
		if err != nil {
			return seataccess.StepResult{}, err
		}
		if obs.TokenWorks {
			return seataccess.StepResult{}, nil
		}
	}
	if seat.ClientID == "" || seat.ClientSecret == "" {
		// THE APP'S OAUTH CLIENT IS NOT SEALED, so no install can be
		// exchanged: the app has to be made again.
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said: "the app's OAuth client is not sealed, and Slack serves it once, at creation",
			Remedy: "Remove " + seat.Handle + "'s Slack app and turn the agent on again, " +
				"which creates a new one",
			Actor:     seataccess.ByYou,
			ActionURL: ManageURL(seat.AppID),
		}
	}
	// NO LINK OF ITS OWN. The install's redirect seals a bot token into a
	// seat, so the link has to carry a state this fleet sealed for this
	// seat, minted when a person asks for it ([InstallLink]); a link drawn
	// into a plan would be a bearer credential sitting in the ledger. The
	// Agents tab's Install action mints it, as GitHub's Create does.
	return seataccess.StepResult{Waiting: &seataccess.Waiting{
		Detail: "Approve " + seat.Handle + "'s app in the workspace with Install on " +
			"the Agents tab. Slack issues its bot token only to an install a person approves",
	}}, nil
}

// applyRemove deletes the app, or hands over its page where no configuration
// token can. Every sealed value is dead once the app is gone.
func (s *session) applyRemove(ctx context.Context, seat SeatApp) (seataccess.StepResult, error) {
	gone := seataccess.StepResult{
		Gone:    true,
		Revoked: []string{FieldBotToken, FieldSigningSecret, FieldClientSecret, FieldClientID},
	}
	if seat.AppID == "" {
		return gone, nil
	}
	if err := s.needs(false); err != nil {
		return seataccess.StepResult{Waiting: &seataccess.Waiting{
			ActionURL: ManageURL(seat.AppID),
			Detail: "Delete " + seat.Handle + "'s app at Slack: " + ManagePath() +
				". No app-configuration token is sealed, so this engine cannot",
		}}, nil
	}
	if err := s.admin.DeleteApp(ctx, s.configToken, seat.AppID); err != nil {
		return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRemove)
	}
	return gone, nil
}

// needs refuses a step that has nothing to act with: no configuration token,
// or (for a step that builds a manifest) no public base.
func (s *session) needs(base bool) error {
	if s.admin == nil || s.configToken == "" {
		why := s.noToken
		if why == nil {
			why = ErrNoConfigToken
		}
		return &seataccess.Refusal{
			Said: why.Error(),
			Remedy: "Create an app-configuration token at api.slack.com/apps and " +
				"enter its refresh token in Slack's settings",
			Actor:     seataccess.ByYou,
			ActionURL: "https://api.slack.com/apps",
		}
	}
	if base && strings.TrimSpace(s.base) == "" {
		return ErrNoBaseURL
	}
	return nil
}

// observe answers the cached observation, or reads Slack.
func (s *session) observe(ctx context.Context, seat SeatApp) (Observed, error) {
	if obs, ok := s.cached[seat.Handle]; ok && obs.TokenWorks {
		return obs, nil
	}
	obs, err := ObserveSeat(ctx, s.admin, s.configToken, s.base, s.http, seat)
	if err != nil {
		return obs, err
	}
	s.cached[seat.Handle] = obs
	return obs, nil
}

// seatApp is one seat's app: the ledger's app id and the engine-named sealed
// values. A seat that left the document keeps its ledger record and its
// handle-derived names, so it is still addressable for off and remove.
func (s *session) seatApp(handle string, record *seataccess.Record) SeatApp {
	seat := SeatApp{Handle: handle, RoleName: handle}
	if doc, ok := s.seats[handle]; ok && strings.TrimSpace(doc.name) != "" {
		seat.RoleName = doc.name
	}
	if record != nil && record.Account != nil && record.Account.State != seataccess.AccountClosed {
		seat.AppID = record.Account.ID
	}
	if s.resolve == nil {
		return seat
	}
	value := func(field string) string {
		return strings.TrimSpace(s.resolve("${" + SeatVar(handle, field) + "}"))
	}
	seat.ClientID, seat.ClientSecret = value(FieldClientID), value(FieldClientSecret)
	seat.SigningSecret, seat.BotToken = value(FieldSigningSecret), value(FieldBotToken)
	return seat
}

// refusal turns Slack's answer to a step into the vendor's reason, the
// remedy and who acts. A fault Slack did not answer is returned as it is.
func (s *session) refusal(err error, seat SeatApp, verb seataccess.StepVerb) error {
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	refusal := &seataccess.Refusal{Said: api.Error(), Actor: seataccess.ByYou}
	switch {
	case configTokenRefused(api.Code):
		refusal.Remedy = "Slack refused the app-configuration token. Create a new one " +
			"at api.slack.com/apps and enter its refresh token in Slack's settings"
		refusal.ActionURL = "https://api.slack.com/apps"
	case appGone(err):
		refusal.Remedy = "Slack no longer has " + seat.Handle + "'s app. Check again, " +
			"and the next sync creates a new one"
		refusal.Actor = seataccess.ByCrewlet
	case api.Code == "invalid_manifest" || len(api.Messages) > 0:
		refusal.Remedy = "Slack refused " + seat.Handle + "'s manifest. Shorten the " +
			"agent's name if Slack names it, then retry the " + string(verb)
	default:
		refusal.Remedy = "Check " + seat.Handle + "'s app at Slack, then retry the " +
			string(verb)
		refusal.ActionURL = ManageURL(seat.AppID)
	}
	return refusal
}

// configTokenRefused reports the codes the manifest methods answer for an
// app-configuration token that no longer works.
func configTokenRefused(code string) bool {
	switch code {
	case "invalid_auth", "token_revoked", "token_expired", "not_authed",
		"not_allowed_token_type", "invalid_token":
		return true
	}
	return false
}

// observedHandles is every seat a pass reads back: the document's, and every
// one the ledger still records, sorted.
func observedHandles(in seataccess.ObserveInput) []string {
	var out []string
	for _, seat := range in.Seats {
		out = append(out, seat.Handle)
	}
	for handle := range in.Records {
		out = append(out, handle)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// recordOf is a handle's ledger record, or nil.
func recordOf(records map[string]seataccess.Record, handle string) *seataccess.Record {
	if record, ok := records[handle]; ok {
		return &record
	}
	return nil
}

// seatName is how a step names the seat.
func seatName(seat seataccess.SeatRef) string {
	if name := strings.TrimSpace(seat.Name); name != "" {
		return name
	}
	return seat.Handle
}

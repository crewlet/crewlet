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
	"github.com/crewlet/crewlet/internal/envref"
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

// ToolTokenEnv is the variable the Slack MCP server this engine documents
// (docs/integrations/slack.md) reads a seat's bot token from. The binding
// table that hands it over is the org model's ([seataccess.ToolFor]); this is
// what that table must say for Slack, and a test holds the two together.
const ToolTokenEnv = "SLACK_MCP_XOXB_TOKEN"

// Kind implements [seataccess.Vendor].
func (vendor) Kind() integration.Kind { return integration.KindSlack }

// Catalog implements [seataccess.Vendor].
func (vendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindSlack,
		Credentials: []string{FieldBotToken, FieldSigningSecret, FieldClientSecret, FieldClientID},
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
	name := seat.Label()
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
	case seataccess.OpEnable:
		add(install)
	case seataccess.OpMint:
		if have.Credential != nil {
			// SEALED BY THE INSTALL, outside any sync: nothing to mint.
			break
		}
		add(install)
	case seataccess.OpLevel:
		add(seataccess.Step{
			Verb: seataccess.VerbRestore, By: seataccess.ByCrewlet,
			Text: "Restore " + name + "'s app manifest",
			Note: have.Level,
		})
		if driftNeedsReinstall(have.Level) {
			// A BOT TOKEN KEEPS THE SCOPES IT WAS INSTALLED WITH, so a
			// scope added at Slack stays on the live token after the
			// manifest drops it, and one removed there is not back on it
			// until an install. The token is retired, and the install
			// issues one at the manifest's scopes.
			add(seataccess.Step{
				Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
				Text: "Revoke " + name + "'s bot token",
				Note: "It holds the scopes changed at Slack. The install below issues one with the manifest's.",
			})
			add(install)
		}
	case seataccess.OpDisable:
		add(seataccess.Step{
			Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
			Text: "Revoke " + name + "'s bot token",
			Note: "The app is kept. Turning it back on needs a person to approve the install again.",
		})
	case seataccess.OpRevoke:
		add(seataccess.Step{
			Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
			Text: "Clear " + name + "'s sealed bot token",
			Note: "The app and its OAuth client are kept, so a person can install it again.",
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
// NOT CONFIGURED WITHOUT A CONFIGURATION TOKEN (D11): the loop creates and
// keeps the apps with it, so a company that has sealed none has not handed
// this engine its Slack apps, and the access sync leaves them alone.
//
// THE TOKEN IS MADE USABLE HERE, before any step: a held token with life left
// is used as it is, and an expiring one is rotated and sealed through
// [seataccess.OpenInput.Sink] before use ([Admin.ConfigToken]). A preview has
// no sink and rotates nothing, so it reads with a token that is still good or
// observes without the manifest; a rotation Slack refuses is the reason the
// steps needing the token report, with the remedy.
func (vendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	s, err := open(ctx, in, nil)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// open is [vendor.Open] through httpClient, nil for the default.
//
// ENGINE-PROVISIONED ONLY (D4b): a company whose block names no configuration
// token hands its agents' apps in from outside Crewlet, and the sync leaves
// them alone whatever the store happens to hold.
func open(ctx context.Context, in seataccess.OpenInput, httpClient *http.Client) (*session, error) {
	if in.Config == nil || in.Config.Integrations.Slack == nil ||
		in.Config.SeatCredentialMode(string(integration.KindSlack), false) != config.SeatCredentialEngine ||
		!configTokenSealed(in) {
		return nil, fmt.Errorf("slack: %w", integration.ErrNotConfigured)
	}
	sessionIn := sessionInput{
		admin: NewAdmin(httpClient),
		base:  strings.TrimSpace(in.WebhookBase),
		seats: map[string]seatDoc{},
		http:  httpClient,
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
	opened := openConfigToken(ctx, sessionIn.admin, in)
	sessionIn.configToken, sessionIn.noToken, sessionIn.rotate = opened.token, opened.why, opened.rotate
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newSession(sessionIn), nil
}

// configTokenName is the variable the company's app-configuration token pair
// is sealed under: the one `integrations.slack.app_config_token` names, which
// the connect form writes as [ConfigTokenVar]. "" where the block names none.
func configTokenName(in seataccess.OpenInput) string {
	if in.Config != nil && in.Config.Integrations.Slack != nil {
		if name, whole := envref.Whole(strings.TrimSpace(in.Config.Integrations.Slack.AppConfigToken)); whole {
			return name
		}
	}
	return ""
}

// configTokenSealed reports a company that has sealed the app-configuration
// token its block names, whatever its state.
func configTokenSealed(in seataccess.OpenInput) bool {
	name := configTokenName(in)
	if in.Env == nil || name == "" {
		return false
	}
	raw, ok := in.Env.LookupOK(name)
	return ok && strings.TrimSpace(raw) != ""
}

// openedToken is a session's app-configuration token: the token, why there is
// none, and how to renew it by force where a sink can keep the renewal.
type openedToken struct {
	token  string
	why    error
	rotate func(context.Context) (string, error)
}

// openConfigToken is the usable configuration token for a session, or why
// there is none. Only a context that is done is an error the caller sees;
// every other failure is the reason the steps needing the token report.
func openConfigToken(ctx context.Context, admin *Admin, in seataccess.OpenInput) openedToken {
	raw, _ := in.Env.LookupOK(configTokenName(in))
	held, err := DecodeConfigToken(raw)
	if err != nil {
		return openedToken{why: err}
	}
	var out openedToken
	if in.Sink != nil {
		// THE RESOLVED PAIR IS THE BOOTSTRAP: the sink answers the same
		// store in the loop, and where it holds nothing yet (a pair sealed
		// before the sink was given) the resolver's refresh token is still
		// the live one.
		store := SinkConfigTokenStore{Sink: in.Sink, Var: configTokenName(in)}
		out.rotate = func(ctx context.Context) (string, error) {
			return admin.ConfigToken(ctx, store, held.RefreshToken, time.Now(), true)
		}
		if !held.Usable(time.Now()) {
			out.token, out.why = admin.ConfigToken(ctx, store, held.RefreshToken, time.Now(), false)
			return out
		}
	}
	if !held.Usable(time.Now()) {
		out.why = errors.New("slack: the app-configuration token needs rotating, " +
			"which a preview does not do")
		return out
	}
	out.token = held.Token
	return out
}

// sessionInput is what a session is built from.
type sessionInput struct {
	admin *Admin

	// configToken is a usable app-configuration token, "" where none is,
	// with noToken saying why.
	configToken string
	noToken     error

	// rotate renews the configuration token by force, nil on a preview,
	// which rotates nothing.
	rotate func(context.Context) (string, error)

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

	// revoked is each seat's fields a step of this pass revoked at Slack,
	// which the resolver, a snapshot taken when the pass began, still reads
	// as sealed.
	revoked map[string][]string

	// renewed reports the one forced renewal a session makes.
	renewed bool
}

func newSession(in sessionInput) *session {
	return &session{sessionInput: in, cached: map[string]Observed{}, revoked: map[string][]string{}}
}

// Options implements [seataccess.Session]: nothing to choose, per seat.
func (s *session) Options(context.Context, seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: seataccess.LaneSlack}, nil
}

// Observe implements [seataccess.Session].
//
// ONE SEAT'S TOKEN, ONE SEAT'S FAULT: a bot token Slack could not check for one
// seat says nothing about the others ([seataccess.SeatFaults]). A refused
// configuration token is every seat's, so it is the kind's, as is a pass whose
// context is done.
func (s *session) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	for _, handle := range in.Handles() {
		record := in.Record(handle)
		seat := s.seatApp(handle, record)
		obs, err := s.read(ctx, seat)
		if err != nil {
			var api *APIError
			if ctx.Err() != nil || (errors.As(err, &api) && configTokenRefused(api.Code)) {
				return nil, fmt.Errorf("slack: observe %s: %w", handle, err)
			}
			faults[handle] = fmt.Errorf("slack: read %s's app back: %w", handle, err)
			continue
		}
		s.cached[handle] = obs
		out[handle] = map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneSlack: observed(seat, obs, record),
		}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observed is one seat's observation in the contract's terms.
//
// THE ACCOUNT IS THE APP, installed or not: reported as gone, an app somebody
// merely uninstalled would have its sealed OAuth client cleared with the
// record, and Slack serves that once. It is ON while its sealed bot token
// works. A token sealed and refused is an app switched off: by this engine
// where the ledger says so, by somebody at Slack otherwise (D3). With no
// token sealed at all, the ledger alone says whose doing that is, which the
// sync reads there.
func observed(seat SeatApp, obs Observed, record *seataccess.Record) seataccess.Observed {
	out := seataccess.Observed{AccountID: seat.AppID, Account: obs.BotName}
	if out.AccountID == "" {
		out.AccountID = obs.AppID
	}
	if record != nil && record.Account != nil {
		out.CreatedAt = record.Account.CreatedAt
	}
	if !obs.AppExists {
		return out
	}
	out.Exists, out.Enabled = true, obs.TokenWorks
	if obs.ManifestRead {
		out.Level = obs.Diff.Describe()
	}
	if strings.TrimSpace(seat.BotToken) == "" {
		return out
	}
	if !out.Enabled {
		out.DisabledBy = seataccess.DisabledByHand
		if record != nil && record.Account != nil && record.Account.State == seataccess.AccountDisabled {
			out.DisabledBy = seataccess.DisabledByCrewlet
		}
	}
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
	seat := s.seatAt(in)
	if in.Step.By == seataccess.ByYou {
		return s.awaitInstall(ctx, seat)
	}
	switch in.Step.Op {
	case seataccess.OpCreate:
		return s.applyCreate(ctx, seat)
	case seataccess.OpLevel:
		if in.Step.Verb == seataccess.VerbRevoke {
			return s.revoke(ctx, seat, false)
		}
		if err := s.needs(true); err != nil {
			return seataccess.StepResult{}, err
		}
		if err := s.withConfigToken(ctx, func(token string) error {
			return restoreManifest(ctx, s.admin, token, s.base, seat)
		}); err != nil {
			return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRestore)
		}
		return seataccess.StepResult{}, nil
	case seataccess.OpDisable, seataccess.OpRevoke, seataccess.OpRotate:
		return s.revoke(ctx, seat, in.Step.Op != seataccess.OpRotate)
	case seataccess.OpRemove:
		return s.applyRemove(ctx, seat)
	}
	return seataccess.StepResult{}, fmt.Errorf("slack: no step for %q", in.Step.Op)
}

// revoke withdraws the seat's bot token at Slack.
//
// ONLY THE BOT TOKEN. The OAuth client and the signing secret stay sealed,
// because an install a person approves later needs both, and Slack serves them
// once.
func (s *session) revoke(ctx context.Context, seat SeatApp, disabled bool) (seataccess.StepResult, error) {
	if err := revokeToken(ctx, s.http, seat.BotToken); err != nil {
		return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRevoke)
	}
	s.revokedAt(seat.Handle, FieldBotToken)
	return seataccess.StepResult{Revoked: []string{FieldBotToken}, Disabled: disabled}, nil
}

// revokedAt records fields a step revoked at Slack for the rest of the pass,
// and drops the seat's cached observation, which still says its token works.
func (s *session) revokedAt(handle string, fields ...string) {
	s.revoked[handle] = append(s.revoked[handle], fields...)
	delete(s.cached, handle)
}

// seatAt is the seat's app as a step finds it: the values sealed when the
// pass began, with what this pass revoked since and what this op's earlier
// steps sealed laid over them.
//
// THE RESOLVER IS A SNAPSHOT taken when the pass began. Read alone, a create's
// install step found the OAuth client its own first step had just sealed
// missing, and refused; and a rotate's install step found the token its first
// step had just revoked still sealed, read the cached observation that it
// worked, and marked the reinstall done.
func (s *session) seatAt(in seataccess.ApplyInput) SeatApp {
	seat := s.seatApp(in.Seat.Handle, in.Record)
	for _, prior := range in.Prior {
		if prior.AccountID != "" {
			seat.AppID = prior.AccountID
		}
		for _, minted := range prior.Minted {
			seat.set(minted.Field, minted.Value)
		}
		for _, field := range prior.Revoked {
			seat.set(field, "")
		}
	}
	return seat
}

// withConfigToken runs call with the session's configuration token, and once
// more with a renewed one where Slack answers token_expired before the expiry
// it recorded. A token Slack retired early is renewed rather than reported,
// once per session, so a refresh token is never spent in a loop.
func (s *session) withConfigToken(ctx context.Context, call func(token string) error) error {
	err := call(s.configToken)
	var api *APIError
	if err == nil || s.rotate == nil || s.renewed || !errors.As(err, &api) || api.Code != "token_expired" {
		return err
	}
	s.renewed = true
	token, renewErr := s.rotate(ctx)
	if renewErr != nil {
		return errors.Join(err, renewErr)
	}
	s.configToken = token
	return call(token)
}

// read reads one seat's app and token back from Slack.
func (s *session) read(ctx context.Context, seat SeatApp) (Observed, error) {
	var obs Observed
	err := s.withConfigToken(ctx, func(token string) error {
		var readErr error
		obs, readErr = ObserveSeat(ctx, s.admin, token, s.base, s.http, seat)
		return readErr
	})
	return obs, err
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
	var created AppCredentials
	if err := s.withConfigToken(ctx, func(token string) error {
		var createErr error
		created, createErr = createApp(ctx, s.admin, token, s.base, seat)
		return createErr
	}); err != nil {
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
// token can: none is sealed, or Slack refused it. The record closes (Gone)
// only once Slack has confirmed the app deleted, and every sealed value is
// dead from then on.
func (s *session) applyRemove(ctx context.Context, seat SeatApp) (seataccess.StepResult, error) {
	gone := seataccess.StepResult{
		Gone:    true,
		Revoked: []string{FieldBotToken, FieldSigningSecret, FieldClientSecret, FieldClientID},
	}
	if seat.AppID == "" {
		return gone, nil
	}
	byHand := func(why string) seataccess.StepResult {
		return seataccess.StepResult{Waiting: &seataccess.Waiting{
			ActionURL: ManageURL(seat.AppID),
			Detail: "Delete " + seat.Handle + "'s app at Slack: " + ManagePath() + ". " + why +
				". The record closes once Slack confirms the app is gone, which " +
				"this engine asks with an app-configuration token",
		}}
	}
	if s.admin == nil || s.configToken == "" {
		return byHand("No usable app-configuration token is held, so this engine cannot"), nil
	}
	err := s.withConfigToken(ctx, func(token string) error {
		return s.admin.DeleteApp(ctx, token, seat.AppID)
	})
	var api *APIError
	switch {
	case err == nil:
		s.revokedAt(seat.Handle, gone.Revoked...)
		return gone, nil
	case errors.As(err, &api) && configTokenRefused(api.Code):
		return byHand("Slack refused the app-configuration token (" + api.Code + ")"), nil
	default:
		return seataccess.StepResult{}, s.refusal(err, seat, seataccess.VerbRemove)
	}
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
	obs, err := s.read(ctx, seat)
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
	for _, field := range []string{FieldClientID, FieldClientSecret, FieldSigningSecret, FieldBotToken} {
		seat.set(field, strings.TrimSpace(s.resolve("${"+SeatVar(handle, field)+"}")))
	}
	for _, field := range s.revoked[handle] {
		seat.set(field, "")
	}
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

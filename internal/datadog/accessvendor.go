package datadog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Datadog's half of per-seat access (design sections 4 and 5.2).
//
// ACCESS IS A DATADOG ROLE, BY ID. A seat's document stores the id
// `GET /api/v2/roles` returns, managed or custom (D7), and the Agents tab
// shows it under Datadog's exact name. Every pass reads each account back
// (its roles, whether it is disabled and by whom, whether it holds a key) and
// the shared plan decides what to do about it: this file only says how each
// step is taken at Datadog.

// AppKeyField is the one credential the engine seals per seat for Datadog:
// the service account's application key, under DATADOG_APP_KEY_<HANDLE>.
const AppKeyField = "APP_KEY"

func init() { seataccess.Register(accessVendor{}) }

// accessVendor is Datadog's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindDatadog }

// Catalog is Datadog's per-seat offer. The seat's key reaches its tool through
// the engine's binding table ([config.SeatToolBindings]): Datadog's MCP server
// reads it as DD_APPLICATION_KEY, beside the organization's API key in the
// server's own headers.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindDatadog,
		Credentials: []string{AppKeyField},
		Lanes: []seataccess.LaneSpec{{
			Lane:   seataccess.LaneDatadog,
			Levels: true, LiveLevels: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
			Rotatable: true,
		}},
	}
}

// Steps is each op as Datadog takes it, in the order Apply takes it. Pure.
func (accessVendor) Steps(
	seat seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	_ seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	out := datadogSteps(seat, op, have)
	for i := range out {
		out[i].Index = i
	}
	return out
}

func datadogSteps(seat seataccess.SeatRef, op seataccess.Op, have seataccess.Observed) []seataccess.Step {
	sink := seataccess.SinkName(integration.KindDatadog, AppKeyField, seat.Handle)
	mint := seataccess.Step{
		Op: op, Verb: seataccess.VerbMint, By: seataccess.ByCrewlet,
		Text: "Mint its application key and seal it as " + sink,
	}
	switch op {
	case seataccess.OpCreate:
		return []seataccess.Step{{
			Op: op, Verb: seataccess.VerbCreate, By: seataccess.ByCrewlet,
			Text: "Create a Datadog service account for " + seatLabel(seat) +
				", holding the role this seat names",
		}, mint}
	case seataccess.OpEnable:
		// BACK ON AT THE DOCUMENT'S ROLE, as a create is, and with a fresh
		// key only where none is held: off revoked it, so one is minted
		// unless an account somebody disabled by hand still holds its own.
		out := []seataccess.Step{{
			Op: op, Verb: seataccess.VerbEnable, By: seataccess.ByCrewlet,
			Text: "Turn its Datadog service account back on, holding the role this seat names",
		}}
		if have.Credential == nil || have.CredentialGone {
			out = append(out, mint)
		}
		return out
	case seataccess.OpLevel:
		return []seataccess.Step{{
			Op: op, Verb: seataccess.VerbRerole, By: seataccess.ByCrewlet,
			Text: "Give it the role this seat names and take it out of any other role",
		}}
	case seataccess.OpMint:
		return []seataccess.Step{mint}
	case seataccess.OpDisable:
		return []seataccess.Step{{
			Op: op, Verb: seataccess.VerbDisable, By: seataccess.ByCrewlet,
			Text: "Disable its Datadog service account",
			Note: "Datadog keeps the account and everything it authored, so turning it " +
				"back on needs no person.",
		}}
	case seataccess.OpRevoke:
		return []seataccess.Step{{
			Op: op, Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
			Text: "Delete its application keys and clear " + sink,
		}}
	case seataccess.OpRemove:
		return []seataccess.Step{{
			Op: op, Verb: seataccess.VerbRemove, By: seataccess.ByCrewlet,
			Text: "Stop managing its disabled Datadog service account",
			Note: "Datadog never deletes a user: the account stays disabled, with its " +
				"history, under the organization's users.",
		}}
	case seataccess.OpRotate:
		mint.Text = "Mint a new application key and seal it as " + sink
		return []seataccess.Step{mint, {
			Op: op, Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
			Text: "Delete the application key it replaces",
		}}
	default:
		// OpScope: Datadog grants by role alone, so there is no scope to
		// move, and an op this build does not know takes no step.
		return nil
	}
}

func seatLabel(seat seataccess.SeatRef) string {
	if strings.TrimSpace(seat.Name) != "" {
		return seat.Name
	}
	return seat.Handle
}

// Open builds a session from the company's Datadog block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil)
}

// open is [accessVendor.Open] with the transport a test points at a fake
// region; nil is the default client.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("datadog: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Datadog
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, fmt.Errorf("datadog: %w: integrations.datadog is off or has no provisioning block",
			integration.ErrNotConfigured)
	}
	client, err := NewClient(ClientOptions{Site: cfg.Provisioning.Site, HTTP: hc})
	if err != nil {
		return nil, err
	}
	creds := Credentials{}
	if in.Env != nil {
		creds.APIKey = strings.TrimSpace(in.Env.Value(cfg.Provisioning.APIKey))
		creds.AppKey = strings.TrimSpace(in.Env.Value(cfg.Provisioning.AppKey))
	}
	if creds.APIKey == "" || creds.AppKey == "" {
		return nil, fmt.Errorf("datadog: %w: the organization's API key and application "+
			"key did not both resolve, and Datadog refuses a write carrying only one",
			integration.ErrNotConfigured)
	}
	return &accessSession{client: client, creds: creds, provisioning: cfg.Provisioning}, nil
}

// accessSession is one pass's connection to Datadog.
type accessSession struct {
	client       *Client
	creds        Credentials
	provisioning *config.DatadogProvisioning
}

// Options is every role the organization has, managed first in Datadog's own
// order of what they grant, then custom roles by name.
func (s *accessSession) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	roles, err := s.client.Roles(ctx, s.creds)
	if err != nil {
		return seataccess.Options{}, fmt.Errorf("datadog: read the organization's roles: %w",
			integration.Reject(err, Status(err)))
	}
	return seataccess.Options{Lane: lane, Levels: levelsOf(roles), Live: true}, nil
}

// levelsOf orders the catalogue least access first: the three managed roles
// in Datadog's order, then every custom role by name, since what a custom
// role grants is set at Datadog and has no place in that order.
func levelsOf(roles []Role) []seataccess.Level {
	managed := []string{RoleReadOnly, RoleStandard, RoleAdmin}
	out := make([]seataccess.Level, 0, len(roles))
	for _, name := range managed {
		for _, role := range roles {
			if role.Name == name {
				out = append(out, seataccess.Level{
					ID: role.ID, Name: role.Name, Administers: AdministrationRole(role.Name),
				})
			}
		}
	}
	for _, role := range roles {
		if !slices.Contains(managed, role.Name) {
			out = append(out, seataccess.Level{ID: role.ID, Name: role.Name, Custom: true})
		}
	}
	return out
}

// Observe reads back every seat's account: the document's seats and every
// seat the ledger still records.
//
// ONE LISTING FOR THE WHOLE COMPANY, which already carries each account's
// roles and state, and one key read per account found. An account the ledger
// records by id is matched by id; a seat with no record is matched by the
// address this engine derives for it, which is how an account made before the
// ledger existed is adopted rather than duplicated (design section 8).
func (s *accessSession) Observe(
	ctx context.Context, in seataccess.ObserveInput,
) (seataccess.Observation, error) {
	listed, err := s.client.ListServiceAccounts(ctx, s.creds, emailDomain(s.provisioning))
	if err != nil {
		return nil, fmt.Errorf("datadog: list the service accounts: %w",
			integration.Reject(err, Status(err)))
	}
	byID := make(map[string]User, len(listed))
	byEmail := make(map[string]User, len(listed))
	for _, user := range listed {
		byID[user.ID] = user
		byEmail[strings.ToLower(user.Email)] = user
	}

	out := seataccess.Observation{}
	for _, handle := range observedHandles(in) {
		rec, recorded := in.Records[handle]
		user, found, err := s.find(ctx, handle, rec, recorded, byID, byEmail)
		if err != nil {
			return nil, err
		}
		if !found {
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneDatadog: {}}
			continue
		}
		keys, err := s.client.ListAppKeys(ctx, s.creds, user.ID)
		if err != nil {
			return nil, fmt.Errorf("datadog: read %s's application keys: %w", handle,
				integration.Reject(err, Status(err)))
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{
			seataccess.LaneDatadog: observedOf(accountOf(user, keys), rec, recorded),
		}
	}
	return out, nil
}

// find is one seat's account: by the ledger's id where it records one, by the
// derived address otherwise.
func (s *accessSession) find(
	ctx context.Context, handle string, rec seataccess.Record, recorded bool,
	byID, byEmail map[string]User,
) (User, bool, error) {
	if recorded && rec.Held() {
		if user, ok := byID[rec.Account.ID]; ok {
			return user, true, nil
		}
		// NOT IN THE LISTING is not gone: the listing is filtered by the
		// email domain, and a company that changed it still owns the
		// accounts made under the old one. Asked by id, which answers
		// "gone" only when Datadog says so.
		user, found, err := s.client.User(ctx, s.creds, rec.Account.ID)
		if err != nil {
			return User{}, false, fmt.Errorf("datadog: read %s's account: %w", handle,
				integration.Reject(err, Status(err)))
		}
		return user, found, nil
	}
	user, ok := byEmail[strings.ToLower(AccountEmail(s.provisioning, handle))]
	return user, ok, nil
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

// observedOf is one account as the shared plan reads it.
func observedOf(account Account, rec seataccess.Record, recorded bool) seataccess.Observed {
	out := seataccess.Observed{
		Exists: true, Enabled: !account.Disabled,
		Account: account.Email, AccountID: account.ID,
		Level: strings.Join(account.RoleIDs, " "),
	}
	if account.Disabled {
		// THIS ENGINE'S OFF, by either record of it: the ledger, or the
		// marker on the account, which survives a lost ledger (D3).
		out.DisabledBy = seataccess.DisabledByHand
		if account.Marked || (recorded && rec.Account != nil &&
			rec.Account.State == seataccess.AccountDisabled) {
			out.DisabledBy = seataccess.DisabledByCrewlet
		}
	}
	if recorded && rec.Account != nil {
		out.CreatedAt = rec.Account.CreatedAt
	}
	// A KEY DELETED AT DATADOG leaves whatever is sealed for the seat
	// answering 403 for ever. The account's own key list is the proof: this
	// engine mints every key under one name, so an account holding none of
	// them holds nothing the seat can authenticate with.
	out.CredentialGone = len(account.EngineKeys()) == 0
	return out
}

// Apply takes one step at Datadog, chosen by its op and verb.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch in.Step.Op {
	case seataccess.OpCreate:
		if in.Step.Verb == seataccess.VerbCreate {
			return s.create(ctx, in)
		}
		return s.mint(ctx, in, false)
	case seataccess.OpEnable:
		if in.Step.Verb == seataccess.VerbEnable {
			return s.withAccount(ctx, in, func(account Account) (seataccess.StepResult, error) {
				if err := s.roleExists(ctx, in); err != nil {
					return seataccess.StepResult{}, err
				}
				if err := s.client.EnableUser(ctx, s.creds, account.ID); err != nil {
					return seataccess.StepResult{}, s.refused(in, err)
				}
				_, err := s.client.SetRole(ctx, s.creds, account, in.Want.Level)
				return seataccess.StepResult{}, s.refused(in, err)
			})
		}
		return s.mint(ctx, in, true)
	case seataccess.OpMint:
		return s.mint(ctx, in, true)
	case seataccess.OpLevel:
		return s.withAccount(ctx, in, func(account Account) (seataccess.StepResult, error) {
			if err := s.roleExists(ctx, in); err != nil {
				return seataccess.StepResult{}, err
			}
			_, err := s.client.SetRole(ctx, s.creds, account, in.Want.Level)
			return seataccess.StepResult{}, s.refused(in, err)
		})
	case seataccess.OpDisable:
		return s.withAccount(ctx, in, func(account Account) (seataccess.StepResult, error) {
			if err := s.client.Disable(ctx, s.creds, account); err != nil {
				return seataccess.StepResult{}, s.refused(in, err)
			}
			return seataccess.StepResult{Disabled: true}, nil
		})
	case seataccess.OpRevoke:
		return s.withAccount(ctx, in, func(account Account) (seataccess.StepResult, error) {
			if err := s.client.RevokeKeys(ctx, s.creds, account); err != nil {
				return seataccess.StepResult{}, s.refused(in, err)
			}
			return seataccess.StepResult{Revoked: []string{AppKeyField}}, nil
		})
	case seataccess.OpRemove:
		// NOTHING IS DELETED AT DATADOG, which has no hard delete for a
		// user: the account stays disabled with its history, and the
		// ledger stops recording it. Off is checked again here rather than
		// trusted, so a remove can never leave a live account unmanaged.
		return s.withAccount(ctx, in, func(account Account) (seataccess.StepResult, error) {
			if account.Disabled && account.Marked && len(account.Keys) == 0 {
				return seataccess.StepResult{}, nil
			}
			if err := s.client.TurnOff(ctx, s.creds, account); err != nil {
				return seataccess.StepResult{}, s.refused(in, err)
			}
			return seataccess.StepResult{Disabled: true, Revoked: []string{AppKeyField}}, nil
		})
	case seataccess.OpRotate:
		if in.Step.Verb == seataccess.VerbMint {
			return s.mint(ctx, in, false)
		}
		return s.withAccount(ctx, in, func(account Account) (seataccess.StepResult, error) {
			// THE KEY THE FIRST STEP MINTED is kept, and every other key
			// this engine minted is deleted; with none minted, nothing is.
			_, err := s.client.RetireKeys(ctx, s.creds, account, in.PriorCredentialID(AppKeyField))
			return seataccess.StepResult{}, s.refused(in, err)
		})
	default:
		return seataccess.StepResult{}, fmt.Errorf("datadog: no step for %q", in.Step.Op)
	}
}

// create makes the seat's service account, already holding its role.
func (s *accessSession) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	if err := s.roleExists(ctx, in); err != nil {
		return seataccess.StepResult{}, err
	}
	email := AccountEmail(s.provisioning, in.Seat.Handle)
	created, err := s.client.CreateServiceAccount(ctx, s.creds, email,
		AccountName(in.Seat.Name, in.Seat.Handle), in.Want.Level)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	return seataccess.StepResult{AccountID: created.ID, Account: email}, nil
}

// roleExists refuses a step whose role id Datadog no longer has (D7): a
// finding the person settles by choosing a role, never a fallback to another.
func (s *accessSession) roleExists(ctx context.Context, in seataccess.ApplyInput) error {
	roleID := strings.TrimSpace(in.Want.Level)
	if roleID == "" {
		return &seataccess.Refusal{
			Said:   "This seat names no Datadog role",
			Remedy: "Choose one of the organization's roles for this agent on the Agents tab",
			Actor:  seataccess.ByYou,
		}
	}
	_, found, err := s.client.Role(ctx, s.creds, roleID)
	if err != nil {
		return s.refused(in, err)
	}
	if !found {
		return &seataccess.Refusal{
			Said: "Datadog has no role with the id " + roleID + "; it was deleted " +
				"or renamed at Datadog",
			Remedy:    "Choose one of the organization's roles for this agent on the Agents tab",
			Actor:     seataccess.ByYou,
			ActionURL: s.rolesURL(),
		}
	}
	return nil
}

// mint creates one application key and hands it to the sync to seal.
//
// A KEY THE SYNC CANNOT SEAL IS DELETED through its Revoke, which the sync
// calls with a context that outlives its own cancellation (design 5.2).
// replaceUnheld first deletes this engine's earlier keys, which a seat being
// given a credential because nothing is sealed for it holds values nobody has.
func (s *accessSession) mint(
	ctx context.Context, in seataccess.ApplyInput, replaceUnheld bool,
) (seataccess.StepResult, error) {
	return s.withAccount(ctx, in, func(account Account) (seataccess.StepResult, error) {
		if replaceUnheld {
			for _, key := range account.EngineKeys() {
				if err := s.client.DeleteAppKey(ctx, s.creds, account.ID, key.ID); err != nil {
					return seataccess.StepResult{}, s.refused(in, err)
				}
			}
		}
		key, err := s.client.CreateAppKey(ctx, s.creds, account.ID, appKeyName)
		if err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		if key.Key == "" {
			// THE KEY EXISTS AND ITS VALUE NEVER ARRIVED, so it is taken
			// back now rather than left live and held by nobody.
			revokeErr := s.client.DeleteAppKey(context.WithoutCancel(ctx), s.creds, account.ID, key.ID)
			return seataccess.StepResult{}, errors.Join(errors.New(
				"datadog: minted an application key and Datadog returned no value for it"),
				revokeErr)
		}
		accountID, keyID := account.ID, key.ID
		return seataccess.StepResult{Minted: []seataccess.Minted{{
			Field: AppKeyField, Value: key.Key, CredentialID: keyID,
			Revoke: func(ctx context.Context) error {
				return s.client.DeleteAppKey(ctx, s.creds, accountID, keyID)
			},
		}}}, nil
	})
}

// withAccount reads the step's account fresh and runs fn on it, or answers
// Gone where Datadog no longer has it.
//
// FRESH, not the observation the pass began with: a step acts on what the
// account holds now, and an account deleted between the read and the step is
// a ledger record to close rather than a refusal to report. The account is
// the ledger's, which the sync updates as soon as a create returns, so a
// create's mint reaches the account the create made; one the pass adopted
// rather than created is the one it observed.
func (s *accessSession) withAccount(
	ctx context.Context, in seataccess.ApplyInput,
	fn func(Account) (seataccess.StepResult, error),
) (seataccess.StepResult, error) {
	accountID := in.Have.AccountID
	if in.Record != nil && in.Record.Held() {
		accountID = in.Record.Account.ID
	}
	if accountID == "" {
		return seataccess.StepResult{}, fmt.Errorf(
			"datadog: %s has no account to %s", in.Seat.Handle, in.Step.Verb)
	}
	user, found, err := s.client.User(ctx, s.creds, accountID)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	if !found {
		return seataccess.StepResult{Gone: true}, nil
	}
	keys, err := s.client.ListAppKeys(ctx, s.creds, accountID)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	return fn(accountOf(user, keys))
}

// refused turns a Datadog refusal into the shared [seataccess.Refusal], with
// the remedy and who acts; anything else stays a fault the sync retries.
func (s *accessSession) refused(in seataccess.ApplyInput, err error) error {
	if err == nil {
		return nil
	}
	var api *APIError
	if !errors.As(err, &api) || api.Status == http.StatusTooManyRequests || api.Status >= 500 {
		return err
	}
	said := api.Detail
	if said == "" || said == noDetail {
		said = fmt.Sprintf("Datadog refused the request with status %d", api.Status)
	}
	return &seataccess.Refusal{
		Said: "Datadog: " + said, Remedy: remedyFor(in.Step.Op, api.Status),
		Actor: seataccess.ByYou, ActionURL: s.client.UsersURL(),
	}
}

// remedyFor is what a person does about a refusal, by what was refused.
func remedyFor(op seataccess.Op, status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Replace the organization's API key or application key on the Datadog connection"
	case http.StatusForbidden:
		switch op {
		case seataccess.OpCreate, seataccess.OpLevel, seataccess.OpEnable,
			seataccess.OpDisable, seataccess.OpRemove:
			return "Give the user behind the organization's application key the User " +
				"Access Manage permission (Datadog Admin Role holds it), then retry"
		default:
			return "Give the user behind the organization's application key the Service " +
				"Account Write permission (Datadog Admin Role holds it), then retry"
		}
	case http.StatusNotFound:
		return "Choose a role the organization still has on the Agents tab, then retry"
	case http.StatusConflict:
		return "Another user already has this agent's address at Datadog; remove or " +
			"rename that user, then retry"
	default:
		return "Settle what Datadog names at Datadog, then retry"
	}
}

// rolesURL is where a person sees the organization's roles.
func (s *accessSession) rolesURL() string {
	if base := s.client.UsersURL(); base != "" {
		return strings.TrimSuffix(base, "/users") + "/roles"
	}
	return ""
}

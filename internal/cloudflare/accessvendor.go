package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Cloudflare's half of per-seat access. The seat's account is its account API
// token, recorded in the ledger by token ID; the credential is the token's
// value. See the package doc for why nothing is adopted by name.

func init() { seataccess.Register(accessVendor{}) }

// accessVendor is Cloudflare's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindCloudflare }

// Catalog is Cloudflare's per-seat offer: fixed levels, no per-seat
// scope (a token reaches the company's zones), and a rotatable value. The
// token reaches the seat's tool through the MCP catalogue's recognition of
// Cloudflare's server, as a bearer Authorization header.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindCloudflare,
		Credentials: []string{TokenField},
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneCloudflare, Levels: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
			Rotatable: true,
		}},
	}
}

// Steps is each op as Cloudflare takes it, in the order Apply takes it. Pure.
func (accessVendor) Steps(
	seat seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	out := cloudflareSteps(seat, op, want, have)
	for i := range out {
		out[i].Index = i
	}
	return out
}

func cloudflareSteps(
	seat seataccess.SeatRef, op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	sink := seataccess.SinkName(integration.KindCloudflare, TokenField, seat.Handle)
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	level := LevelName(want.Level)
	roll := step(seataccess.VerbMint, "Roll its token for a new value, sealed as "+sink)
	// THE EXPIRY AFTER THE VALUE, so a value never outlives 90 days: a renewal
	// that fails leaves the new value expiring early, and the next pass rolls
	// again.
	renew := step(seataccess.VerbGrant, "Renew its expiry to 90 days from now")
	switch op {
	case seataccess.OpCreate:
		return []seataccess.Step{step(seataccess.VerbCreate,
			"Create its Cloudflare account API token at "+level+", sealed as "+sink)}
	case seataccess.OpEnable:
		out := []seataccess.Step{step(seataccess.VerbEnable, "Turn its token back on at "+level)}
		if have.Credential == nil || have.CredentialGone {
			out = append(out, roll, renew)
		}
		return out
	case seataccess.OpLevel:
		return []seataccess.Step{step(seataccess.VerbRerole, "Narrow its token to "+level)}
	case seataccess.OpMint, seataccess.OpRotate:
		return []seataccess.Step{roll, renew}
	case seataccess.OpDisable:
		// DISABLED FIRST, so the agent stops even where the delete fails;
		// a token left disabled is deleted by the next pass's revoke.
		gone := step(seataccess.VerbRevoke, "Delete its token")
		gone.Note = "Cloudflare keeps no history for a token, so turning the agent back on mints a new one."
		return []seataccess.Step{step(seataccess.VerbDisable, "Disable its token"), gone}
	case seataccess.OpRevoke:
		return []seataccess.Step{step(seataccess.VerbRevoke, "Clear "+sink)}
	case seataccess.OpRemove:
		return []seataccess.Step{step(seataccess.VerbRemove, "Delete its token and stop managing it")}
	default:
		// OpScope: a token reaches the company's zones, with no per-seat
		// scope to move.
		return nil
	}
}

// Open builds a session from the company's Cloudflare block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil, nil)
}

// open is [accessVendor.Open] with the transport and clock a test supplies;
// nil takes the defaults.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client, now func() time.Time) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("cloudflare: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Cloudflare
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, fmt.Errorf("cloudflare: %w: integrations.cloudflare is off or has no provisioning block",
			integration.ErrNotConfigured)
	}
	var account, token string
	if in.Env != nil {
		account = strings.TrimSpace(in.Env.Value(cfg.Provisioning.AccountID))
		token = strings.TrimSpace(in.Env.Value(cfg.Provisioning.APIToken))
	}
	if account == "" || token == "" {
		return nil, fmt.Errorf("cloudflare: %w: the account ID and the provisioner token did not both resolve",
			integration.ErrNotConfigured)
	}
	client, err := NewClient(ClientOptions{AccountID: account, Token: token, HTTP: hc})
	if err != nil {
		return nil, &seataccess.Refusal{
			Said:   "integrations.cloudflare.provisioning.account_id is not a Cloudflare account ID",
			Remedy: "Give the 32-character account ID from the account's overview page in the Cloudflare dashboard",
			Actor:  seataccess.ByYou,
		}
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	s := &accessSession{
		client: client, company: in.Config.Name, zones: cfg.Provisioning.Zones, now: now,
	}
	// THE PASS'S OWN INSPECTION, where the company half of this pass made
	// one over the same account: the catalogue and the levels it cannot
	// grant are what the session would read again, call for call, from the
	// same token and the same configured zones.
	if read, ok := in.Reads.(*Inspection); ok && read != nil && read.account == account {
		cat := read.in.cat
		gaps := levelGaps(read.in.unresolved, read.in.unheld, client.TokensURL())
		s.cat, s.gaps = &cat, &gaps
	}
	return s, nil
}

// accessSession is one pass's connection to Cloudflare.
type accessSession struct {
	client  *Client
	company string
	zones   []string
	now     func() time.Time

	// The account's catalogue, read once per pass and only where a step
	// writes or compares a policy, and the levels it and the provisioner's
	// own policy cannot grant, read once where a level is offered or granted.
	mu   sync.Mutex
	cat  *catalogue
	gaps *[]seataccess.LevelGap
}

// catalogue reads the account's permission groups and zones once.
func (s *accessSession) catalogue(ctx context.Context) (catalogue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cat != nil {
		return *s.cat, nil
	}
	groups, err := s.client.PermissionGroups(ctx)
	if err != nil {
		return catalogue{}, fmt.Errorf("cloudflare: read the account's permission groups: %w",
			reject(err))
	}
	zones, err := s.client.Zones(ctx)
	if err != nil {
		return catalogue{}, fmt.Errorf("cloudflare: read the account's zones: %w",
			reject(err))
	}
	cat := newCatalogue(s.client.AccountID(), groups, zones, s.zones)
	s.cat = &cat
	return cat, nil
}

// levelGaps reads, once per pass, the levels the account's catalogue or the
// provisioner's own policy cannot grant yet ([levelGaps]).
func (s *accessSession) levelGaps(ctx context.Context) ([]seataccess.LevelGap, error) {
	cat, err := s.catalogue(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	held := s.gaps
	s.mu.Unlock()
	if held != nil {
		return *held, nil
	}
	status, err := s.client.Verify(ctx)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: read the provisioner token: %w", reject(err))
	}
	unheld, err := readUnheld(ctx, s.client, cat, status.ID)
	if err != nil {
		return nil, fmt.Errorf("cloudflare: read the provisioner token's own permissions: %w", reject(err))
	}
	gaps := levelGaps(cat.unresolvedLevels(), unheld, s.client.TokensURL())
	s.mu.Lock()
	s.gaps = &gaps
	s.mu.Unlock()
	return gaps, nil
}

// Options is Cloudflare's levels, least access first, each it cannot grant yet
// offered with why. The set is fixed: a level is this engine's set of
// permission groups, not something Cloudflare lists.
func (s *accessSession) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	gaps, err := s.levelGaps(ctx)
	if err != nil {
		return seataccess.Options{}, err
	}
	return seataccess.Options{Lane: lane, Levels: seataccess.OfferGaps(accessVendor{}.FixedLevels(lane), gaps),
		Rotatable: true}, nil
}

// FixedLevels is Cloudflare's levels, least access first: this engine's own
// set of permission groups ([seataccess.FixedLevels]).
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	out := make([]seataccess.Level, 0, len(levels))
	for _, l := range levels {
		out = append(out, seataccess.Level{ID: l.id, Name: l.name, Note: l.note, Administers: l.administers})
	}
	return out
}

// Observe reads back the token the ledger records for each seat, by ID. A
// seat with no recorded token holds none: nothing is adopted by name.
//
// AN EXPIRED TOKEN IS NOT THE SEAT'S ANY MORE: it is reported absent, so the
// sync clears its dead value and a seat that is on gets a new token, whose
// create deletes the expired one.
func (s *accessSession) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	now := s.now()
	for _, handle := range in.Handles() {
		rec := in.Record(handle)
		if rec == nil || !rec.Held() {
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneCloudflare: {}}
			continue
		}
		tok, found, err := s.client.Token(ctx, rec.Account.ID)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil && errors.Is(reject(err), integration.ErrCredentialRejected):
			// THE PROVISIONER IS REFUSED, which is every seat's read alike.
			return nil, fmt.Errorf("cloudflare: read %s's token: %w", handle, reject(err))
		case err != nil:
			faults[handle] = err
			continue
		case !found || expired(tok, now):
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneCloudflare: {}}
			continue
		}
		cat, err := s.catalogue(ctx)
		if err != nil {
			return nil, err
		}
		observed := seataccess.Observed{
			Exists: true, Enabled: tok.Status == StatusActive,
			Account: tok.Name, AccountID: tok.ID, Level: cat.levelOf(tok.Policies),
			CreatedAt: rec.Account.CreatedAt,
		}
		// ABOUT TO LAPSE: the plan mints a fresh value, with a fresh expiry,
		// while the seat is on.
		observed.CredentialGone = observed.Enabled && renewDue(tok.ExpiresOn, now)
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneCloudflare: observed}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// expired reports a token Cloudflare reports expired, or whose expiry passed.
func expired(tok Token, now time.Time) bool {
	return tok.Status == StatusExpired || (!tok.ExpiresOn.IsZero() && !tok.ExpiresOn.After(now))
}

// Apply takes one step at Cloudflare, chosen by its op and verb.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch in.Step.Op {
	case seataccess.OpCreate:
		return s.create(ctx, in)
	case seataccess.OpEnable:
		switch in.Step.Verb {
		case seataccess.VerbEnable:
			return s.write(ctx, in, true)
		case seataccess.VerbMint:
			return s.roll(ctx, in)
		default:
			return s.renew(ctx, in)
		}
	case seataccess.OpLevel:
		return s.write(ctx, in, false)
	case seataccess.OpMint, seataccess.OpRotate:
		if in.Step.Verb == seataccess.VerbMint {
			return s.roll(ctx, in)
		}
		return s.renew(ctx, in)
	case seataccess.OpDisable:
		if in.Step.Verb == seataccess.VerbDisable {
			return s.disable(ctx, in)
		}
		return s.delete(ctx, in)
	case seataccess.OpRevoke, seataccess.OpRemove:
		return s.delete(ctx, in)
	default:
		return seataccess.StepResult{}, fmt.Errorf("cloudflare: no step for %q", in.Step.Op)
	}
}

// policiesFor is the policy a level grants on the company's zones, or the
// refusal saying why it cannot be built.
func (s *accessSession) policiesFor(ctx context.Context, in seataccess.ApplyInput) ([]Policy, error) {
	cat, err := s.catalogue(ctx)
	if err != nil {
		return nil, err
	}
	g, err := cat.grantFor(in.Want.Level)
	if err != nil {
		return nil, &seataccess.Refusal{
			Said:   "Cloudflare: " + err.Error(),
			Remedy: "Choose another level for this agent on the Agents tab",
			Actor:  seataccess.ByYou,
		}
	}
	// A LEVEL THE PROVISIONER CANNOT GRANT is refused here rather than asked
	// of Cloudflare, which does not document whether it would grant it.
	gaps, err := s.levelGaps(ctx)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(gaps, func(g seataccess.LevelGap) bool { return g.Level == in.Want.Level }); i >= 0 {
		return nil, &seataccess.Refusal{
			Said:      "Cloudflare: " + gaps[i].Reason,
			Remedy:    gaps[i].Remedy + ", or choose a level it holds for this agent on the Agents tab",
			Actor:     seataccess.ByYou,
			ActionURL: gaps[i].ActionURL,
		}
	}
	return cat.policies(g), nil
}

// create mints the seat's token at its level, expiring in 90 days, and hands
// its value to the sync to seal. A value the sync cannot seal is deleted with
// its token through Revoke.
func (s *accessSession) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	policies, err := s.policiesFor(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	// THE TOKEN A CLOSED RECORD NAMES goes first: it is one this engine
	// minted that has expired, and a seat holds one token.
	if rec := in.Record; rec != nil && rec.Account != nil && !rec.Held() && rec.Account.ID != "" {
		if derr := s.client.DeleteToken(ctx, rec.Account.ID); derr != nil {
			return seataccess.StepResult{}, s.refused(in, derr)
		}
	}
	name := TokenName(s.company, in.Seat.Handle)
	tok, value, err := s.client.CreateToken(ctx, TokenSpec{
		Name: name, Policies: policies, ExpiresOn: expiry(s.now()),
	})
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	if strings.TrimSpace(value) == "" {
		revokeErr := s.client.DeleteToken(context.WithoutCancel(ctx), tok.ID)
		return seataccess.StepResult{}, errors.Join(
			errors.New("cloudflare: created a token and Cloudflare returned no value for it"), revokeErr)
	}
	return seataccess.StepResult{
		AccountID: tok.ID, Account: name,
		Minted: []seataccess.Minted{s.minted(tok.ID, value)},
	}, nil
}

// minted is a value for the sync to seal, whose Revoke deletes the token: a
// token whose current value is sealed nowhere is one nobody can use, and
// deleting it lets the next pass mint a fresh one.
func (s *accessSession) minted(tokenID, value string) seataccess.Minted {
	return seataccess.Minted{
		Field: TokenField, Value: value, CredentialID: tokenID,
		Revoke: func(ctx context.Context) error { return s.client.DeleteToken(ctx, tokenID) },
	}
}

// write moves the token to the seat's level on the company's zones, keeping
// its name and expiry: an enable turns it on, a level change keeps its status.
func (s *accessSession) write(ctx context.Context, in seataccess.ApplyInput, enable bool) (seataccess.StepResult, error) {
	return s.withToken(ctx, in, func(tok Token) (seataccess.StepResult, error) {
		policies, err := s.policiesFor(ctx, in)
		if err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		status := tok.Status
		if enable {
			status = StatusActive
		}
		_, err = s.client.UpdateToken(ctx, tok.ID, TokenSpec{
			Name: tok.Name, Policies: policies, ExpiresOn: tok.ExpiresOn, Status: status,
		})
		return seataccess.StepResult{}, s.refused(in, err)
	})
}

// roll replaces the token's value and hands the new one to the sync to seal.
// The old value stops working at once.
func (s *accessSession) roll(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withToken(ctx, in, func(tok Token) (seataccess.StepResult, error) {
		value, err := s.client.RollToken(ctx, tok.ID)
		if err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		return seataccess.StepResult{Minted: []seataccess.Minted{s.minted(tok.ID, value)}}, nil
	})
}

// renew sets the token's expiry to 90 days from now, keeping everything else
// it holds.
func (s *accessSession) renew(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withToken(ctx, in, func(tok Token) (seataccess.StepResult, error) {
		_, err := s.client.UpdateToken(ctx, tok.ID, TokenSpec{
			Name: tok.Name, Policies: tok.Policies, ExpiresOn: expiry(s.now()), Status: tok.Status,
		})
		return seataccess.StepResult{}, s.refused(in, err)
	})
}

// disable turns the token off at once, keeping everything else it holds.
func (s *accessSession) disable(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withToken(ctx, in, func(tok Token) (seataccess.StepResult, error) {
		if _, err := s.client.UpdateToken(ctx, tok.ID, TokenSpec{
			Name: tok.Name, Policies: tok.Policies, ExpiresOn: tok.ExpiresOn, Status: StatusDisabled,
		}); err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		return seataccess.StepResult{Disabled: true}, nil
	})
}

// delete deletes the seat's token, which is the end of off, a revoke and a
// remove alike: a Cloudflare token keeps no history worth keeping a disabled
// one for. One already gone is the state asked for.
func (s *accessSession) delete(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	id := tokenID(in)
	if id == "" {
		return seataccess.StepResult{Gone: true, Revoked: []string{TokenField}}, nil
	}
	if err := s.client.DeleteToken(ctx, id); err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	return seataccess.StepResult{Gone: true, Revoked: []string{TokenField}}, nil
}

// tokenID is the step's token: the ledger's, else the one the pass observed.
func tokenID(in seataccess.ApplyInput) string {
	if in.Record != nil && in.Record.Account != nil && in.Record.Account.ID != "" {
		return in.Record.Account.ID
	}
	return in.Have.AccountID
}

// withToken reads the step's token fresh and runs fn on it, or answers Gone
// where Cloudflare no longer has it.
func (s *accessSession) withToken(
	ctx context.Context, in seataccess.ApplyInput, fn func(Token) (seataccess.StepResult, error),
) (seataccess.StepResult, error) {
	id := tokenID(in)
	if id == "" {
		return seataccess.StepResult{}, fmt.Errorf("cloudflare: %s has no token to %s", in.Seat.Handle, in.Step.Verb)
	}
	tok, found, err := s.client.Token(ctx, id)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	if !found || expired(tok, s.now()) {
		return seataccess.StepResult{Gone: true}, nil
	}
	return fn(tok)
}

// refused turns a Cloudflare refusal into the shared [seataccess.Refusal],
// with the remedy and who acts; a rate limit, a 5xx or a transport failure
// stays a fault the sync retries.
func (s *accessSession) refused(in seataccess.ApplyInput, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*seataccess.Refusal](err); ok {
		return err
	}
	api, ok := errors.AsType[*APIError](err)
	if !ok || api.Status == http.StatusTooManyRequests || api.Status >= http.StatusInternalServerError {
		return err
	}
	said := api.Detail
	if said == "" || said == noDetail {
		said = fmt.Sprintf("Cloudflare refused the request with status %d", api.Status)
	}
	return &seataccess.Refusal{
		Said: "Cloudflare: " + said, Remedy: remedyFor(in.Step.Op, api.Status, TokenRefused(err)),
		Actor: seataccess.ByYou, ActionURL: s.client.TokensURL(),
	}
}

// remedyFor is what a person does about a refusal, by what was refused.
// tokenRefused is Cloudflare's codes refusing the token itself ([TokenRefused]).
func remedyFor(op seataccess.Op, status int, tokenRefused bool) string {
	switch {
	case tokenRefused || status == http.StatusUnauthorized:
		return "Replace the provisioner token on the Cloudflare connection"
	case status == http.StatusForbidden:
		if op == seataccess.OpCreate || op == seataccess.OpLevel || op == seataccess.OpEnable {
			return "Give the provisioner token Account API Tokens Write and every permission " +
				"an agent level grants, over the account and its zones, then retry"
		}
		return "Give the provisioner token Account API Tokens Write, then retry"
	default:
		if op == seataccess.OpCreate {
			return "Settle what Cloudflare names at Cloudflare, then retry. An account holds " +
				"at most 500 account tokens, so delete the ones nobody uses if it is full"
		}
		return "Settle what Cloudflare names at Cloudflare, then retry"
	}
}

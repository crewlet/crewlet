package digitalocean

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// DigitalOcean's half of per-seat access. DigitalOcean lets no API create a
// token, so a person creates each seat's token in the control panel and pastes
// it; the engine proves its scopes, seals it, and revokes it itself when the
// seat is turned off or its token replaced.

func init() { seataccess.Register(accessVendor{}) }

// fingerprintInfo names the key a token's fingerprint is made with, apart from
// every other key derived from the fleet's keyring.
const fingerprintInfo = "crewlet/digitalocean/token-fingerprint"

// accessVendor is DigitalOcean's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindDigitalOcean }

// Catalog is DigitalOcean's per-seat offer: three fixed levels, no per-seat
// scope, a token a person creates, and a token doctl reads as
// DIGITALOCEAN_ACCESS_TOKEN. No release: a kept token is live access nobody
// uses or checks.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindDigitalOcean,
		Credentials: []string{TokenField, RetiredField},
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneDigitalOcean, Levels: true,
			CreatedBy: seataccess.ByYou, EnabledBy: seataccess.ByYou,
			Rotatable: true,
			BoxEnv:    []seataccess.BoxVar{{Field: TokenField, Env: ToolEnv}},
		}},
	}
}

// HandInName is the name a person gives the seat's token at its level.
func (accessVendor) HandInName(c *config.Company, seat seataccess.SeatRef, _ seataccess.Lane,
	want seataccess.Desired) string {
	return TokenName(c.Name, seat.Handle, levelOr(want.Level))
}

// levelOr is level, or the default where it names none.
func levelOr(level string) string {
	if strings.TrimSpace(level) == "" {
		return config.DigitalOceanLevelDefault
	}
	return level
}

// Steps is each op as it is taken, in order, without the team's name. Pure.
func (v accessVendor) Steps(
	seat seataccess.SeatRef, lane seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	return v.StepsFor(seataccess.StepsInput{Seat: seat, Lane: lane, Op: op, Want: want, Have: have})
}

// StepsFor is [accessVendor.Steps] naming the team the options carry.
func (accessVendor) StepsFor(in seataccess.StepsInput) []seataccess.Step {
	out := doSteps(in.Seat, in.Op, in.Want, in.Have, in.Options.VendorAccount, in.HandIn)
	for i := range out {
		out[i].Index = i
	}
	return out
}

func doSteps(
	seat seataccess.SeatRef, op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
	team, name string,
) []seataccess.Step {
	sink := "`" + seataccess.SinkName(integration.KindDigitalOcean, TokenField, seat.Handle) + "`"
	level := LevelName(levelOr(want.Level))
	in := "the connected team"
	if team != "" {
		in = "team " + team
	}
	you := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByYou, Text: text}
	}
	crewlet := func(text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet, Text: text}
	}
	token := "a token"
	if name != "" {
		token = "a token named " + name
	}
	paste := you(seataccess.VerbMint, "Create a new "+strings.TrimPrefix(token, "a ")+" with Custom Scopes for the "+
		level+" level in "+in+", then paste it here")
	seal := seataccess.Step{Op: op, Verb: seataccess.VerbMint, By: seataccess.ByCrewlet,
		Text: "Check the token and seal it as " + sink}
	revokeOld := crewlet("Revoke the previous token at DigitalOcean")
	clear := crewlet("Clear " + sink + ", so the agent loses DigitalOcean at once")
	revoke := crewlet("Revoke the token at DigitalOcean")
	switch op {
	case seataccess.OpCreate:
		return []seataccess.Step{
			you(seataccess.VerbCreate, "In the team menu, switch to "+strings.TrimPrefix(in, "team ")+
				". Then create a personal access "+strings.TrimPrefix(token, "a ")+" with Custom Scopes for the "+
				level+" level"),
			you(seataccess.VerbMint, "Paste the token here"), seal}
	case seataccess.OpMint, seataccess.OpEnable:
		if have.CredentialGone {
			drop := clear
			drop.Text = "Clear " + sink + ", which DigitalOcean no longer accepts"
			return []seataccess.Step{drop, paste, seal}
		}
		return []seataccess.Step{paste, seal}
	case seataccess.OpLevel:
		return []seataccess.Step{paste, seal, revokeOld}
	case seataccess.OpRotate:
		regen := you(seataccess.VerbMint, "Regenerate Token on its row at DigitalOcean (it keeps the name "+
			"and scopes), or create a new token with the "+level+" scopes, then paste it here")
		return []seataccess.Step{regen, seal, revokeOld}
	case seataccess.OpRetire:
		return []seataccess.Step{revokeOld}
	case seataccess.OpDisable, seataccess.OpRemove, seataccess.OpRevoke:
		return []seataccess.Step{clear, revoke}
	default:
		// OpScope: a token acts on the whole team.
		return nil
	}
}

// Open builds a session from the company's DigitalOcean block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil)
}

// open is [accessVendor.Open] with the transport a test supplies; nil takes
// the default.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("digitalocean: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.DigitalOcean
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil || in.Env == nil {
		return nil, fmt.Errorf("digitalocean: %w: integrations.digitalocean is off or names no team",
			integration.ErrNotConfigured)
	}
	team := strings.TrimSpace(in.Env.Value(cfg.Provisioning.Team))
	if team == "" {
		return nil, fmt.Errorf("digitalocean: %w: integrations.digitalocean.provisioning.team resolved to nothing",
			integration.ErrNotConfigured)
	}
	env := in.Env
	return &accessSession{
		team: team, company: in.Config.Name, http: hc, derive: in.Derive,
		sealed: func(handle, field string) string {
			return strings.TrimSpace(env.Value("${" + seataccess.SinkName(integration.KindDigitalOcean, field, handle) + "}"))
		},
	}, nil
}

// accessSession is one pass's connection to DigitalOcean, through each seat's
// own token.
type accessSession struct {
	team    string
	company string
	http    *http.Client
	derive  func(info string) []byte

	// sealed is the value sealed for one seat's field, "" where none is.
	sealed func(handle, field string) string
}

// client is a client carrying one token.
func (s *accessSession) client(token string) (*Client, error) {
	return NewClient(ClientOptions{Token: token, HTTP: s.http})
}

// accountID is what the ledger records for a token: its team's ID, which pins
// the team for later seats, and a keyed fingerprint of the token, which tells
// seats apart and can be reversed by nobody.
func (s *accessSession) accountID(teamUUID, token string) (string, error) {
	var key []byte
	if s.derive != nil {
		key = s.derive(fingerprintInfo)
	}
	if len(key) == 0 {
		return "", errors.New("digitalocean: this node holds no secrets keyring to fingerprint a token with")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(token))
	return teamUUID + ":" + hex.EncodeToString(mac.Sum(nil)[:16]), nil
}

// teamOf is the team ID an account ID records, "" where it records none.
func teamOf(accountID string) string {
	team, _, ok := strings.Cut(accountID, ":")
	if !ok {
		return ""
	}
	return team
}

// display is how a token's owner reads in a sentence: the person, then the
// team.
func display(acct Account) string {
	name := strings.TrimSpace(acct.Name)
	if name == "" {
		name = "the token's user"
	}
	return name + " on " + acct.Team.Name
}

// retiredLabel names a seat's replaced token without its level, which the pass
// no longer knows: every token of the seat begins with it.
func (s *accessSession) retiredLabel(handle string) string {
	return strings.TrimSuffix(TokenName(s.company, handle, ""), "-") + "-…"
}

// Options is the three levels, each with the scopes it grants, and the scopes
// no level grants. Fixed: a level is Crewlet's own set of DigitalOcean scopes.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	// THE TOKEN LIST, where every row's Regenerate Token is and Generate New
	// Token is one click away; a wait for a new token links its form itself.
	return seataccess.Options{Lane: lane, Levels: accessVendor{}.FixedLevels(lane), Rotatable: true,
		ActionURL: TokensURL, Refused: Refused(), Unneeded: Unneeded(), VendorAccount: s.team}, nil
}

// FixedLevels is the three levels, least access first, each with the scopes it
// grants ([seataccess.FixedLevels]).
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	out := make([]seataccess.Level, 0, len(levels))
	for _, id := range Levels() {
		out = append(out, seataccess.Level{ID: id, Name: LevelName(id), Note: LevelNote(id),
			Administers: Administers(id), Grants: Grants(id)})
	}
	return out
}

// Observe proves each seat's token with the token itself. A 401 is reported
// gone and the token not used again; a token whose proven scopes are no
// level's reports no level, which the sync plans a new token for.
func (s *accessSession) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	for _, handle := range in.Handles() {
		observed, err := s.observe(ctx, handle, in.Record(handle))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			faults[handle] = err
			continue
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneDigitalOcean: observed}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observe is one seat's lane.
func (s *accessSession) observe(ctx context.Context, handle string, rec *seataccess.Record) (seataccess.Observed, error) {
	sink := &seataccess.Credential{Variable: seataccess.SinkName(integration.KindDigitalOcean, TokenField, handle)}
	recorded := seataccess.Observed{Exists: true, Enabled: true}
	held := rec != nil && rec.Held()
	if held {
		recorded.AccountID, recorded.Account = rec.Account.ID, rec.Account.Name
		recorded.CreatedAt = rec.Account.CreatedAt
		if lr, ok := rec.Lanes[seataccess.LaneDigitalOcean]; ok && lr.Applied != nil {
			recorded.Level = lr.Applied.Level
		}
	}
	var retired []seataccess.Retired
	if s.sealed(handle, RetiredField) != "" {
		// A REVOKE NOT DONE YET: the retire step, or the off's, retries it.
		retired = []seataccess.Retired{{Label: s.retiredLabel(handle)}}
	}
	token := s.sealed(handle, TokenField)
	switch {
	case token == "" && retired != nil:
		recorded.Retired = retired
		return recorded, nil
	case token == "" && !held:
		return seataccess.Observed{}, nil
	case token == "":
		// A RECORDED TOKEN NO LONGER SEALED: the one DigitalOcean refused was
		// cleared.
		recorded.CredentialGone = true
		return recorded, nil
	}
	c, err := s.client(token)
	if err != nil {
		return seataccess.Observed{}, err
	}
	acct, err := c.Account(ctx)
	if Status(err) == http.StatusUnauthorized {
		recorded.CredentialGone, recorded.Credential, recorded.Retired = true, sink, retired
		return recorded, nil
	}
	if err != nil {
		return seataccess.Observed{}, fmt.Errorf("digitalocean: read %s's token: %w", handle, err)
	}
	out := seataccess.Observed{Exists: true, Enabled: true, Account: display(acct),
		AccountID: recorded.AccountID, CreatedAt: recorded.CreatedAt, Credential: sink, Retired: retired}
	if out.AccountID == "" {
		if out.AccountID, err = s.accountID(acct.Team.UUID, token); err != nil {
			return seataccess.Observed{}, err
		}
	}
	if strings.TrimSpace(acct.Team.Name) != s.team {
		// ANOTHER TEAM'S TOKEN proves no level here.
		return out, nil
	}
	proven, err := prove(ctx, c, readChecks, separatorChecks, administerChecks)
	if errors.Is(err, errTokenRefused) {
		recorded.CredentialGone, recorded.Credential, recorded.Retired = true, sink, retired
		return recorded, nil
	}
	if err != nil {
		return seataccess.Observed{}, fmt.Errorf("digitalocean: prove %s's token: %w", handle, err)
	}
	out.Level = proven.Level()
	return out, nil
}

// Apply takes one step: a person's step answers Waiting until a token is
// pasted, and Crewlet's steps check and seal a pasted token, clear one, or
// revoke one at DigitalOcean.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch {
	case in.Step.By == seataccess.ByCrewlet && in.Step.Verb == seataccess.VerbMint:
		return s.seal(ctx, in)
	case in.Step.By == seataccess.ByYou:
		return s.awaitToken(in), nil
	case in.Step.Index == 0 && (in.Step.Op == seataccess.OpMint || in.Step.Op == seataccess.OpEnable):
		// THE DEAD TOKEN GOES FIRST, so DigitalOcean is never sent it again.
		return seataccess.StepResult{Revoked: []string{TokenField}}, nil
	case in.Step.Index == 0 && isOff(in.Step.Op):
		return s.clear(in), nil
	default:
		return s.revoke(ctx, in)
	}
}

// isOff is an op that ends a seat's access.
func isOff(op seataccess.Op) bool {
	return op == seataccess.OpDisable || op == seataccess.OpRemove || op == seataccess.OpRevoke
}

// awaitToken is done once a token was pasted for the seat, and otherwise says
// what to make at DigitalOcean.
func (s *accessSession) awaitToken(in seataccess.ApplyInput) seataccess.StepResult {
	if in.HandIn != "" {
		return seataccess.StepResult{}
	}
	level := levelOr(in.Want.Level)
	name := TokenName(s.company, in.Seat.Handle, level)
	made := fmt.Sprintf("a personal access token named %s with Custom Scopes for the %s level in team %s",
		name, LevelName(level), s.team)
	switch {
	case in.Have.CredentialGone:
		return s.waiting("DigitalOcean no longer accepts this agent's token: it expired, was deleted or was " +
			"regenerated. Create " + made + ", and paste it here.")
	case in.Have.Exists && in.Have.Account != "" && !strings.HasSuffix(in.Have.Account, " on "+s.team):
		return s.waiting("This agent's token acts in another team than " + s.team + ". Create " + made +
			", and paste it here. Crewlet then revokes the previous token.")
	case in.Have.Exists && in.Have.Level == "" && in.Step.Op != seataccess.OpRotate:
		return s.waiting("This agent's token no longer proves the " + LevelName(level) + " level: its " +
			"creator's team role may have been narrowed. Restore the role at DigitalOcean, or create " + made +
			", and paste it here.")
	case in.Step.Op == seataccess.OpLevel:
		return s.waiting("DigitalOcean cannot change a token's scopes. Create " + made +
			", and paste it here. Crewlet then revokes the previous token.")
	case in.Step.Op == seataccess.OpRotate:
		return s.waiting("Regenerate Token on this agent's token row at DigitalOcean, which keeps its name " +
			"and scopes, or create " + made + ". Paste the new token here.")
	default:
		return s.waiting(fmt.Sprintf("In the team menu at the top of the control panel, switch to %s. "+
			"Then create %s, and paste it here.", s.team, made))
	}
}

// seal checks the pasted token for the seat and hands it to the sync to seal,
// keeping the token it replaces where no box reads it until it is revoked.
func (s *accessSession) seal(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	if in.HandIn == "" {
		return s.waiting("Paste the agent's token here."), nil
	}
	checked, id, err := s.check(ctx, in.Seat, in.Want, in.Record, nil, in.HandIn)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	res := seataccess.StepResult{AccountID: id, Account: display(checked.Account),
		Minted: []seataccess.Minted{{Field: TokenField, Value: in.HandIn,
			CredentialID: TokenName(s.company, in.Seat.Handle, levelOr(in.Want.Level))}}}
	if old := s.sealed(in.Seat.Handle, TokenField); old != "" && old != in.HandIn {
		if s.sealed(in.Seat.Handle, RetiredField) != "" {
			// AN EARLIER REVOKE STILL PENDING is taken now, so its value is
			// not overwritten while it still works.
			if err := s.revokeValue(ctx, s.sealed(in.Seat.Handle, RetiredField)); err != nil {
				return seataccess.StepResult{}, err
			}
		}
		res.Minted = append(res.Minted, seataccess.Minted{Field: RetiredField, Value: old})
	}
	return res, nil
}

// clear takes the seat's token away from its box at once, keeping its value
// where no box reads it for the revoke step after.
func (s *accessSession) clear(in seataccess.ApplyInput) seataccess.StepResult {
	res := seataccess.StepResult{Revoked: []string{TokenField}}
	if token := s.sealed(in.Seat.Handle, TokenField); token != "" {
		res.Minted = []seataccess.Minted{{Field: RetiredField, Value: token}}
	}
	return res
}

// retiredValue is the token a revoke step acts on: the one an earlier step of
// this op set aside, else the one sealed aside before.
func (s *accessSession) retiredValue(in seataccess.ApplyInput) string {
	for i := len(in.Prior) - 1; i >= 0; i-- {
		for _, m := range in.Prior[i].Minted {
			if m.Field == RetiredField && m.Value != "" {
				return m.Value
			}
		}
	}
	return s.sealed(in.Seat.Handle, RetiredField)
}

// revoke revokes the token set aside and clears it. A refusal DigitalOcean
// keeps giving while the token still works waits for a person to delete it;
// on a disconnect nothing waits, since no pass would read the seat again.
func (s *accessSession) revoke(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	done := seataccess.StepResult{Revoked: []string{RetiredField}, Gone: isOff(in.Step.Op)}
	value := s.retiredValue(in)
	if value == "" {
		return done, nil
	}
	err := s.revokeValue(ctx, value)
	switch {
	case err == nil:
		return done, nil
	case in.Disconnect:
		done.Did = "Crewlet could not revoke this agent's token at DigitalOcean; delete " +
			s.retiredLabel(in.Seat.Handle) + " in the control panel."
		return done, nil
	}
	if status := Status(err); status >= 400 && status < 500 && status != http.StatusTooManyRequests {
		return seataccess.StepResult{Waiting: &seataccess.Waiting{ActionURL: TokensURL,
			Detail: fmt.Sprintf("DigitalOcean refused to revoke this agent's previous token. Delete %s, "+
				"the older one, in the control panel.", s.retiredLabel(in.Seat.Handle))}}, nil
	}
	return seataccess.StepResult{}, err
}

// revokeValue revokes one token at DigitalOcean. A token that already answers
// 401 is revoked already; a refused revoke of a token that still works is the
// refusal.
func (s *accessSession) revokeValue(ctx context.Context, value string) error {
	c, err := s.client(value)
	if err != nil {
		return err
	}
	err = c.Revoke(ctx)
	if err == nil {
		return nil
	}
	if _, aerr := c.Account(ctx); Status(aerr) == http.StatusUnauthorized {
		return nil
	}
	return err
}

// waiting is a person's step at DigitalOcean, pointing at the new token page.
func (s *accessSession) waiting(detail string) seataccess.StepResult {
	return seataccess.StepResult{Waiting: &seataccess.Waiting{ActionURL: NewTokenURL, Detail: detail}}
}

// check proves value for the seat, refusing as the shared [seataccess.Refusal],
// and answers the account ID the ledger records for it. records are every
// seat's of the kind, nil where the caller has none to hand.
func (s *accessSession) check(
	ctx context.Context, seat seataccess.SeatRef, want seataccess.Desired, rec *seataccess.Record,
	records map[string]seataccess.Record, value string,
) (Checked, string, error) {
	c, err := s.client(value)
	if err != nil {
		return Checked{}, "", err
	}
	in := CheckInput{Token: value, Team: s.team, Level: levelOr(want.Level),
		HeldBy: func(token string) string { return s.heldBy(seat.Handle, records, token) }}
	for handle, other := range records {
		if handle != seat.Handle && other.Held() && other.Account != nil {
			if team := teamOf(other.Account.ID); team != "" {
				in.TeamUUID = team
				break
			}
		}
	}
	if in.TeamUUID == "" && rec != nil && rec.Held() {
		in.TeamUUID = teamOf(rec.Account.ID)
	}
	checked, err := Check(ctx, c, in)
	if r, ok := IsRefusal(err); ok {
		return Checked{}, "", &seataccess.Refusal{Said: r.Detail, Remedy: r.Hint, Actor: seataccess.ByYou,
			ActionURL: NewTokenURL}
	}
	if err != nil {
		return Checked{}, "", err
	}
	id, err := s.accountID(checked.Account.Team.UUID, value)
	if err != nil {
		return Checked{}, "", err
	}
	return checked, id, nil
}

// heldBy is the other seat whose sealed token is token, compared by keyed
// fingerprint with the current key, so no stored digest goes stale when the
// keyring rotates. "" where none is.
func (s *accessSession) heldBy(self string, records map[string]seataccess.Record, token string) string {
	mine, err := s.accountID("", token)
	if err != nil {
		return ""
	}
	for handle := range records {
		if handle == self {
			continue
		}
		other := s.sealed(handle, TokenField)
		if other == "" {
			continue
		}
		if theirs, err := s.accountID("", other); err == nil && hmac.Equal([]byte(theirs), []byte(mine)) {
			return handle
		}
	}
	return ""
}

// CheckHandIn implements [seataccess.HandsIn]: the paste route's check, with
// requests that cannot have an effect, before the sync seals anything.
func (s *accessSession) CheckHandIn(ctx context.Context, in seataccess.HandInInput) (seataccess.HandedIn, error) {
	// A ROTATE replaces a token the seat holds and DigitalOcean still accepts.
	rotate := in.Record != nil && in.Record.Held() && s.sealed(in.Seat.Handle, TokenField) != ""
	if rotate {
		last := in.Record.Lanes[in.Lane].Observed
		rotate = last.Exists && !last.CredentialGone
	}
	checked, id, err := s.check(ctx, in.Seat, in.Want, in.Record, in.Records, in.Value)
	if err != nil {
		return seataccess.HandedIn{}, err
	}
	level := levelOr(in.Want.Level)
	return seataccess.HandedIn{
		Rotate: rotate, Account: display(checked.Account), AccountID: id,
		Label: TokenName(s.company, in.Seat.Handle, level),
		Verdict: setup.Works("Works. The token is sealed for this agent.",
			setup.Fact{Label: "Account", Value: checked.Account.Name},
			setup.Fact{Label: "Team", Value: checked.Account.Team.Name},
			setup.Fact{Label: "Team ID", Value: checked.Account.Team.UUID},
			setup.Fact{Label: "Level", Value: LevelName(level)},
			setup.Fact{Label: "Sealed as", Value: seataccess.SinkName(integration.KindDigitalOcean, TokenField, in.Seat.Handle)}),
	}, nil
}

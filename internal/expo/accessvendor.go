package expo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Expo's half of per-seat access. The seat's account is its robot user,
// recorded in the ledger by robot ID; the credential is an access token on
// that robot. See the package doc for why nothing is adopted by name.

func init() { seataccess.Register(accessVendor{}) }

// accessVendor is Expo's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindExpo }

// Catalog is Expo's per-seat offer: four fixed levels, no per-seat scope (a
// robot acts on the whole account) and a rotatable token the EAS CLI in the
// seat's box reads as EXPO_TOKEN.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindExpo,
		Credentials: []string{TokenField},
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneExpo, Levels: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
			Rotatable: true, BoxEnv: []seataccess.BoxVar{{Field: TokenField, Env: ToolEnv}},
		}},
	}
}

// Steps is each op as Expo takes it, in the order Apply takes it. Pure.
func (accessVendor) Steps(
	seat seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	out := expoSteps(seat, op, want, have)
	for i := range out {
		out[i].Index = i
	}
	return out
}

func expoSteps(
	seat seataccess.SeatRef, op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	sink := seataccess.SinkName(integration.KindExpo, TokenField, seat.Handle)
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	level := LevelName(want.Level)
	// THE NEW TOKEN FIRST, sealed before the ones it replaces are retired, so
	// the seat is never left holding nothing.
	mint := step(seataccess.VerbMint, "Create an access token for its robot user, sealed as "+sink)
	retire := step(seataccess.VerbRevoke, "Revoke and delete the tokens it replaces")
	switch op {
	case seataccess.OpCreate:
		return []seataccess.Step{step(seataccess.VerbCreate, "Create an Expo robot user at "+level), mint}
	case seataccess.OpEnable:
		out := []seataccess.Step{step(seataccess.VerbEnable, "Give its robot user back its permissions at "+level)}
		if have.Credential == nil || have.CredentialGone {
			out = append(out, mint, retire)
		}
		return out
	case seataccess.OpLevel:
		return []seataccess.Step{step(seataccess.VerbRerole, "Move its robot user to "+level)}
	case seataccess.OpMint:
		return []seataccess.Step{mint, retire}
	case seataccess.OpRotate:
		mint.Text = "Create a new access token for its robot user, sealed as " + sink
		return []seataccess.Step{mint, retire}
	case seataccess.OpDisable:
		off := step(seataccess.VerbDisable, "Revoke its tokens and delete its robot user")
		off.Note = "Expo keeps no switched-off robot, so turning the agent back on creates a new one."
		return []seataccess.Step{off}
	case seataccess.OpRevoke:
		return []seataccess.Step{step(seataccess.VerbRevoke, "Revoke and delete its tokens and clear "+sink)}
	case seataccess.OpRemove:
		return []seataccess.Step{step(seataccess.VerbRemove,
			"Revoke its tokens, delete its robot user and stop managing it")}
	default:
		// OpScope: a robot acts on the whole account, with no per-seat scope.
		return nil
	}
}

// listingGrace is how long a new robot missing from the listing reads as not
// listed yet rather than gone: a listing shows one within seconds, and ten
// minutes bounds how long a robot deleted at Expo leaves its seat without one.
const listingGrace = 10 * time.Minute

// Open builds a session from the company's Expo block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil, nil)
}

// open is [accessVendor.Open] with the transport and clock a test supplies;
// nil takes the defaults.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client, now func() time.Time) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("expo: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Expo
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, fmt.Errorf("expo: %w: integrations.expo is off or has no provisioning block",
			integration.ErrNotConfigured)
	}
	var account, token string
	if in.Env != nil {
		account = strings.TrimSpace(in.Env.Value(cfg.Provisioning.AccountName))
		token = strings.TrimSpace(in.Env.Value(cfg.Provisioning.AccessToken))
	}
	if account == "" || token == "" {
		return nil, fmt.Errorf("expo: %w: the account name and the provisioner token did not both resolve",
			integration.ErrNotConfigured)
	}
	client, err := NewClient(ClientOptions{Token: token, HTTP: hc})
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &accessSession{client: client, account: account, company: in.Config.Name, now: now}, nil
}

// accessSession is one pass's connection to Expo.
type accessSession struct {
	client  *Client
	account string
	company string
	now     func() time.Time

	// The account's ID, read once per pass where a step writes.
	mu        sync.Mutex
	accountID string
}

// accountRef is the account's ID, read once.
func (s *accessSession) accountRef(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accountID != "" {
		return s.accountID, nil
	}
	acct, err := s.client.Account(ctx, s.account)
	if err != nil {
		return "", err
	}
	s.accountID = acct.ID
	return acct.ID, nil
}

// Options is Expo's four levels, least access first. Fixed: a level is this
// engine's set of Expo permissions, not something Expo lists.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	out := make([]seataccess.Level, 0, len(Levels()))
	for _, id := range Levels() {
		out = append(out, seataccess.Level{ID: id, Name: LevelName(id), Note: LevelNote(id)})
	}
	return seataccess.Options{Lane: lane, Levels: out, Rotatable: true}, nil
}

// Observe reads each seat's recorded robot, by ID, from one listing; nothing is
// adopted by name. A robot being deleted or missing is absent, except one
// created within [listingGrace], which is that seat's fault to retry.
func (s *accessSession) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	var byID map[string]Robot
	for _, handle := range in.Handles() {
		rec := in.Record(handle)
		if rec == nil || !rec.Held() || rec.Account.ID == "" {
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneExpo: {}}
			continue
		}
		if byID == nil {
			robots, err := s.client.Robots(ctx, s.account)
			if err != nil {
				return nil, fmt.Errorf("expo: list the account's robot users: %w",
					integration.Reject(err, Status(err)))
			}
			byID = make(map[string]Robot, len(robots))
			for _, r := range robots {
				byID[r.ID] = r
			}
		}
		robot, found := byID[rec.Account.ID]
		switch {
		case found && !robot.Deleting:
			out[handle] = map[seataccess.Lane]seataccess.Observed{
				seataccess.LaneExpo: observedOf(robot, handle, rec),
			}
		case !found && s.now().Sub(rec.Account.CreatedAt) < listingGrace:
			// NOT LISTED YET: read as gone, it would be created a second
			// time and the first robot left holding a live token.
			faults[handle] = fmt.Errorf("expo: the account's listing does not show robot user %s, "+
				"created %s, yet", rec.Account.ID, rec.Account.CreatedAt.Format(time.RFC3339))
		default:
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneExpo: {}}
		}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observedOf is one robot as the shared plan reads it. A robot holding no
// permission on the account was emptied by somebody else: the engine never
// does that, it deletes.
func observedOf(robot Robot, handle string, rec *seataccess.Record) seataccess.Observed {
	out := seataccess.Observed{
		Exists: true, Enabled: len(robot.Permissions) > 0,
		Account: robot.Name, AccountID: robot.ID, Level: levelOf(robot.Permissions),
		CreatedAt: rec.Account.CreatedAt,
		// A TOKEN DELETED OR REVOKED AT EXPO leaves whatever is sealed for
		// the seat refused for ever; the robot's own token list is the proof.
		CredentialGone: len(engineTokens(robot, handle)) == 0,
	}
	if !out.Enabled {
		out.DisabledBy = seataccess.DisabledByHand
	}
	return out
}

// Apply takes one step at Expo, chosen by its op and verb.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch in.Step.Op {
	case seataccess.OpCreate:
		if in.Step.Verb == seataccess.VerbCreate {
			return s.create(ctx, in)
		}
		return s.mint(ctx, in)
	case seataccess.OpEnable:
		switch in.Step.Verb {
		case seataccess.VerbEnable:
			return s.setLevel(ctx, in)
		case seataccess.VerbMint:
			return s.mint(ctx, in)
		default:
			return s.retire(ctx, in)
		}
	case seataccess.OpLevel:
		return s.setLevel(ctx, in)
	case seataccess.OpMint, seataccess.OpRotate:
		if in.Step.Verb == seataccess.VerbMint {
			return s.mint(ctx, in)
		}
		return s.retire(ctx, in)
	case seataccess.OpDisable, seataccess.OpRemove:
		return s.deleteRobot(ctx, in)
	case seataccess.OpRevoke:
		return s.revokeTokens(ctx, in)
	default:
		return seataccess.StepResult{}, fmt.Errorf("expo: no step for %q", in.Step.Op)
	}
}

// permissionsFor is what the seat's level grants, or the refusal saying why
// it cannot be granted.
func permissionsFor(in seataccess.ApplyInput) ([]Permission, error) {
	perms, ok := Permissions(in.Want.Level)
	if !ok {
		return nil, &seataccess.Refusal{
			Said:   fmt.Sprintf("Expo: %q is not a level this engine offers", in.Want.Level),
			Remedy: "Choose a level for this agent on the Agents tab",
			Actor:  seataccess.ByYou,
		}
	}
	return perms, nil
}

// create makes the seat's robot user, already holding its level's
// permissions. Observe reports a seat robotless only where its robot is gone
// or already being deleted, so there is no earlier robot to clear first.
func (s *accessSession) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	perms, err := permissionsFor(in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	accountID, err := s.accountRef(ctx)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	name := RobotName(s.company, in.Seat.Handle)
	robot, err := s.client.CreateRobot(ctx, accountID, name, perms)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	return seataccess.StepResult{AccountID: robot.ID, Account: name}, nil
}

// mint creates one access token on the seat's robot and hands its value to the
// sync to seal. A value the sync cannot seal is revoked and deleted through
// its Revoke; the tokens it replaces go in the op's next step.
func (s *accessSession) mint(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withRobot(ctx, in, func(robot Robot) (seataccess.StepResult, error) {
		tok, value, err := s.client.CreateToken(ctx, robot.ID, TokenNote(in.Seat.Handle))
		if err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		if strings.TrimSpace(value) == "" {
			// THE TOKEN EXISTS AND ITS VALUE NEVER ARRIVED, so it is taken
			// back now rather than left live and held by nobody.
			revokeErr := s.client.RetireToken(context.WithoutCancel(ctx), tok.ID)
			return seataccess.StepResult{}, errors.Join(
				errors.New("expo: created an access token and Expo returned no value for it"), revokeErr)
		}
		tokenID := tok.ID
		return seataccess.StepResult{Minted: []seataccess.Minted{{
			Field: TokenField, Value: value, CredentialID: tokenID,
			Revoke: func(ctx context.Context) error { return s.client.RetireToken(ctx, tokenID) },
		}}}, nil
	})
}

// retire revokes and deletes the engine's tokens on the robot other than the
// one this op's earlier step minted. With nothing minted before it, it
// refuses and retires nothing, so a seat is never left without a token.
func (s *accessSession) retire(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	keep := in.PriorCredentialID(TokenField)
	if keep == "" {
		return seataccess.StepResult{}, fmt.Errorf(
			"expo: %s: no token was minted before this step, so none is retired", in.Seat.Handle)
	}
	return s.withRobot(ctx, in, func(robot Robot) (seataccess.StepResult, error) {
		for _, tok := range engineTokens(robot, in.Seat.Handle) {
			if tok.ID == keep {
				continue
			}
			if err := s.client.RetireToken(ctx, tok.ID); err != nil {
				return seataccess.StepResult{}, s.refused(in, err)
			}
		}
		return seataccess.StepResult{}, nil
	})
}

// setLevel moves the robot to exactly the seat's level: the permissions it
// lacks are granted first and the extra ones revoked after, so it never holds
// less than the lower of the two levels in between.
func (s *accessSession) setLevel(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	perms, err := permissionsFor(in)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	return s.withRobot(ctx, in, func(robot Robot) (seataccess.StepResult, error) {
		accountID, err := s.accountRef(ctx)
		if err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		if err := s.client.Grant(ctx, accountID, robot.ID, difference(perms, robot.Permissions)); err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		if err := s.client.Revoke(ctx, accountID, robot.ID, difference(robot.Permissions, perms)); err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		return seataccess.StepResult{}, nil
	})
}

// deleteRobot is off and remove alike: every token on the robot is revoked
// and deleted, then the robot's deletion is scheduled. One already gone is
// the state asked for.
func (s *accessSession) deleteRobot(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	done := seataccess.StepResult{Gone: true, Revoked: []string{TokenField}}
	id := robotID(in)
	if id == "" {
		return done, nil
	}
	robot, found, err := s.findRobot(ctx, id)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	if !found || robot.Deleting {
		return done, nil
	}
	if err := s.dropRobot(ctx, robot); err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	return done, nil
}

// dropRobot revokes and deletes every live token on robot, then schedules the
// robot's deletion: the tokens first, so a deletion Expo runs later leaves no
// token working in between.
func (s *accessSession) dropRobot(ctx context.Context, robot Robot) error {
	for _, tok := range robot.Tokens {
		if tok.Revoked {
			continue
		}
		if err := s.client.RetireToken(ctx, tok.ID); err != nil {
			return err
		}
	}
	return s.client.DeleteRobot(ctx, robot.ID)
}

// revokeTokens revokes and deletes the engine's tokens on the seat's robot and
// keeps the robot. A robot already gone holds none.
func (s *accessSession) revokeTokens(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	done := seataccess.StepResult{Revoked: []string{TokenField}}
	id := robotID(in)
	if id == "" {
		return done, nil
	}
	robot, found, err := s.findRobot(ctx, id)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	if !found || robot.Deleting {
		return done, nil
	}
	for _, tok := range engineTokens(robot, in.Seat.Handle) {
		if err := s.client.RetireToken(ctx, tok.ID); err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
	}
	return done, nil
}

// robotID is the step's robot: the ledger's, else the one the pass observed.
func robotID(in seataccess.ApplyInput) string {
	if in.Record != nil && in.Record.Account != nil && in.Record.Account.ID != "" {
		return in.Record.Account.ID
	}
	return in.Have.AccountID
}

// findRobot reads the account's robots fresh and answers the one with id.
func (s *accessSession) findRobot(ctx context.Context, id string) (Robot, bool, error) {
	robots, err := s.client.Robots(ctx, s.account)
	if err != nil {
		return Robot{}, false, err
	}
	for _, r := range robots {
		if r.ID == id {
			return r, true, nil
		}
	}
	return Robot{}, false, nil
}

// withRobot reads the step's robot fresh and runs fn on it, or answers Gone
// where Expo no longer has it. A robot a step leaves on carries the engine's
// name again, so the account's robot list keeps naming its agent.
func (s *accessSession) withRobot(
	ctx context.Context, in seataccess.ApplyInput, fn func(Robot) (seataccess.StepResult, error),
) (seataccess.StepResult, error) {
	for _, prior := range in.Prior {
		if prior.AccountID != "" {
			// A ROBOT THIS OP JUST CREATED is taken as created: a listing read
			// straight after may not show it yet, and Gone would leak it.
			return fn(Robot{ID: prior.AccountID, Name: prior.Account})
		}
	}
	id := robotID(in)
	if id == "" {
		return seataccess.StepResult{}, fmt.Errorf("expo: %s has no robot user to %s", in.Seat.Handle, in.Step.Verb)
	}
	robot, found, err := s.findRobot(ctx, id)
	if err != nil {
		return seataccess.StepResult{}, s.refused(in, err)
	}
	if !found || robot.Deleting {
		return seataccess.StepResult{Gone: true}, nil
	}
	if name := RobotName(s.company, in.Seat.Handle); in.Step.Op.LeavesOn() && robot.Name != name {
		if err := s.client.RenameRobot(ctx, robot.ID, name); err != nil {
			return seataccess.StepResult{}, s.refused(in, err)
		}
		robot.Name = name
	}
	return fn(robot)
}

// refused turns an Expo refusal into the shared [seataccess.Refusal], with the
// remedy and who acts; a rate limit, a 5xx or a transport failure stays a
// fault the sync retries.
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
		said = fmt.Sprintf("Expo refused the request with status %d", api.Status)
	}
	return &seataccess.Refusal{
		Said: "Expo: " + said, Remedy: remedyFor(in.Step.Op, api.Status),
		Actor: seataccess.ByYou, ActionURL: AccessTokensURL(s.account),
	}
}

// remedyFor is what a person does about a refusal, by what was refused.
func remedyFor(op seataccess.Op, status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "Replace the provisioner token on the Expo connection"
	case http.StatusForbidden:
		return "Give the provisioner token's user or robot the Admin role on the Expo account, then retry"
	case http.StatusNotFound:
		if op == seataccess.OpCreate || op == seataccess.OpLevel || op == seataccess.OpEnable {
			return "Check that integrations.expo.provisioning.account_name names the Expo account, then retry"
		}
		return "Settle what Expo names at Expo, then retry"
	default:
		return "Settle what Expo names at Expo, then retry"
	}
}

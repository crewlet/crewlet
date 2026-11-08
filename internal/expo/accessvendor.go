package expo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// Expo's half of per-seat access. Expo lets no access token create a robot,
// so a person creates each seat's robot at Expo and pastes its token; the
// engine checks it, seals it, and reads everything after with it.

func init() { seataccess.Register(accessVendor{}) }

// RetiredField is the token of a seat turned off, sealed where no box reads
// it, so the pass can tell when the robot is deleted at Expo.
const RetiredField = "RETIRED_TOKEN"

// accessVendor is Expo's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindExpo }

// Catalog is Expo's per-seat offer: four fixed levels, no per-seat scope, a
// robot a person creates, and a token the EAS CLI reads as EXPO_TOKEN.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindExpo,
		Credentials: []string{TokenField, RetiredField},
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneExpo, Levels: true,
			CreatedBy: seataccess.ByYou, EnabledBy: seataccess.ByYou,
			Rotatable: true, Releasable: true,
			BoxEnv: []seataccess.BoxVar{{Field: TokenField, Env: ToolEnv}},
		}},
	}
}

// HandInName is the name a person gives the seat's robot at Expo.
func (accessVendor) HandInName(c *config.Company, seat seataccess.SeatRef, _ seataccess.Lane,
	_ seataccess.Desired) string {
	return RobotName(c.Name, seat.Handle)
}

// StepsFor is [accessVendor.Steps] naming the robot to create before any pass
// has seen it, from the name a person gives it.
func (v accessVendor) StepsFor(in seataccess.StepsInput) []seataccess.Step {
	have := in.Have
	if have.Account == "" {
		have.Account = in.HandIn
	}
	return v.Steps(in.Seat, in.Lane, in.Op, in.Want, have)
}

// Steps is each op as it is taken, in order. Pure.
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
	sink := "`" + seataccess.SinkName(integration.KindExpo, TokenField, seat.Handle) + "`"
	you := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByYou, Text: text}
	}
	role := LevelName(want.Level)
	// The robot by name where the pass knows it: before it exists, the name
	// to give it.
	robot := "its robot"
	if have.Account != "" {
		robot = "robot `" + have.Account + "`"
	}
	paste := you(seataccess.VerbMint, "Create a new access token for "+robot+", then paste it here")
	seal := seataccess.Step{Op: op, Verb: seataccess.VerbMint, By: seataccess.ByCrewlet,
		Text: "Check the token and seal it as " + sink}
	clear := seataccess.Step{Op: op, Verb: seataccess.VerbRevoke, By: seataccess.ByCrewlet,
		Text: "Clear " + sink + ", so the agent loses Expo at once"}
	switch op {
	case seataccess.OpCreate:
		named := "a robot for this agent"
		if have.Account != "" {
			named = "a robot named `" + have.Account + "`"
		}
		paste.Text = "Create an access token for it, then paste it here"
		return []seataccess.Step{
			you(seataccess.VerbCreate, "Add "+named+" with the "+role+" role at Expo"),
			paste, seal}
	case seataccess.OpEnable:
		return []seataccess.Step{you(seataccess.VerbEnable,
			"Give "+robot+" the "+role+" role again with Edit robot at Expo")}
	case seataccess.OpLevel:
		return []seataccess.Step{you(seataccess.VerbRerole,
			"Change "+robot+"'s role to "+role+" with Edit robot at Expo")}
	case seataccess.OpMint:
		// WHAT IS SEALED NOW GOES FIRST: a token Expo refused is never sent
		// again, and a turned-off robot's is never reused once back on.
		drop := clear
		drop.Text = "Clear the turned-off robot's old token, so it is never reused"
		if have.CredentialGone {
			drop.Text = "Clear " + sink + ", which Expo no longer accepts"
		}
		return []seataccess.Step{drop, paste, seal}
	case seataccess.OpRotate:
		return []seataccess.Step{paste, seal, you(seataccess.VerbRevoke, "Delete "+robot+"'s other tokens at Expo")}
	case seataccess.OpRetire:
		return []seataccess.Step{you(seataccess.VerbRevoke, "Delete "+robot+"'s other tokens at Expo")}
	case seataccess.OpDisable:
		return []seataccess.Step{clear, you(seataccess.VerbRemove, "Delete "+robot+" at Expo")}
	case seataccess.OpRevoke:
		return []seataccess.Step{clear}
	case seataccess.OpRemove:
		return []seataccess.Step{clear,
			you(seataccess.VerbRemove, "Delete "+robot+" at Expo, so Crewlet stops tracking it")}
	default:
		// OpScope: a robot acts on the whole account.
		return nil
	}
}

// Open builds a session from the company's Expo block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil)
}

// open is [accessVendor.Open] with the transport a test supplies; nil takes
// the default.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("expo: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Expo
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil || in.Env == nil {
		return nil, fmt.Errorf("expo: %w: integrations.expo is off or names no account",
			integration.ErrNotConfigured)
	}
	account := strings.TrimSpace(in.Env.Value(cfg.Provisioning.AccountName))
	if account == "" {
		return nil, fmt.Errorf("expo: %w: integrations.expo.provisioning.account_name resolved to nothing",
			integration.ErrNotConfigured)
	}
	env := in.Env
	return &accessSession{
		account: account, company: in.Config.Name, http: hc,
		sealed: func(handle, field string) string {
			return strings.TrimSpace(env.Value("${" + seataccess.SinkName(integration.KindExpo, field, handle) + "}"))
		},
	}, nil
}

// accessSession is one pass's connection to Expo, through each seat's own
// token.
type accessSession struct {
	account string
	company string
	http    *http.Client

	// sealed is the value sealed for one seat's field, "" where none is.
	sealed func(handle, field string) string

	// robots are the seat each recorded robot belongs to, by robot ID.
	robots map[string]string
}

// client is a client carrying one token.
func (s *accessSession) client(token string) (*Client, error) {
	return NewClient(ClientOptions{Token: token, HTTP: s.http})
}

// remember records which seat holds each recorded robot.
func (s *accessSession) remember(records map[string]seataccess.Record) {
	s.robots = map[string]string{}
	for handle, rec := range records {
		if rec.Held() && rec.Account.ID != "" {
			s.robots[rec.Account.ID] = handle
		}
	}
}

// Options is Expo's four levels under the roles' own names. Fixed: a level is
// one of Expo's robot roles, not something Expo lists.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: accessVendor{}.FixedLevels(lane), Rotatable: true,
		ActionURL: AccessTokensURL(s.account), VendorAccount: s.account}, nil
}

// FixedLevels is Expo's roles, least access first ([seataccess.FixedLevels]).
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	out := make([]seataccess.Level, 0, len(Levels()))
	for _, id := range Levels() {
		out = append(out, seataccess.Level{ID: id, Name: LevelName(id), Note: LevelNote(id)})
	}
	return out
}

// Observe reads each seat's robot with the seat's own sealed token. A seat
// with none has no robot here yet, and Observed.Account is the name to give
// it; a token Expo refuses is reported gone and not used again.
func (s *accessSession) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	s.remember(in.Records)
	for _, handle := range in.Handles() {
		observed, err := s.observe(ctx, handle, in.Record(handle))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			faults[handle] = err
			continue
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneExpo: observed}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observe is one seat's lane.
func (s *accessSession) observe(ctx context.Context, handle string, rec *seataccess.Record) (seataccess.Observed, error) {
	sink := &seataccess.Credential{Variable: seataccess.SinkName(integration.KindExpo, TokenField, handle)}
	recorded := seataccess.Observed{Exists: true, Enabled: true}
	if rec != nil && rec.Held() {
		recorded.AccountID, recorded.Account = rec.Account.ID, rec.Account.Name
		recorded.CreatedAt = rec.Account.CreatedAt
		if lr, ok := rec.Lanes[seataccess.LaneExpo]; ok && lr.Applied != nil {
			recorded.Level = lr.Applied.Level
		}
	}
	token := s.sealed(handle, TokenField)
	switch {
	case token == "" && recorded.AccountID == "":
		return seataccess.Observed{Account: RobotName(s.company, handle)}, nil
	case token == "" && s.sealed(handle, RetiredField) != "":
		// OFF, AND ITS ROBOT NOT DELETED YET: the remove step watches it.
		return recorded, nil
	case token == "":
		// A RECORDED ROBOT WITH NO TOKEN: the one Expo refused was cleared.
		recorded.CredentialGone = true
		return recorded, nil
	}
	c, err := s.client(token)
	if err != nil {
		return seataccess.Observed{}, err
	}
	me, err := c.Viewer(ctx)
	if Status(err) == http.StatusUnauthorized {
		recorded.CredentialGone, recorded.Credential = true, sink
		return recorded, nil
	}
	if err != nil {
		return seataccess.Observed{}, fmt.Errorf("expo: read %s's robot: %w", handle, err)
	}
	out := seataccess.Observed{Exists: true, Account: me.Name, AccountID: me.ID,
		CreatedAt: recorded.CreatedAt, Credential: sink}
	for _, t := range othersOf(me, token) {
		out.Retired = append(out.Retired, seataccess.Retired{Label: t.Prefix + "…", Since: t.Created})
	}
	role, err := c.RoleOn(ctx, s.account)
	switch status := Status(err); {
	case errors.Is(err, ErrRoleUnsaid):
		return seataccess.Observed{}, fmt.Errorf("expo: Expo did not say robot %s's role on %s: %w",
			me.Name, s.account, err)
	case status == http.StatusForbidden || status == http.StatusNotFound:
		// NO ROLE ON THE ACCOUNT: somebody took it away at Expo.
		out.DisabledBy = seataccess.DisabledByHand
	case err != nil:
		return seataccess.Observed{}, fmt.Errorf("expo: read %s's role: %w", handle, err)
	default:
		out.Enabled, out.Level = true, levelOf(role)
	}
	return out, nil
}

// Apply takes one step: a person's step answers Waiting until Expo shows it
// done, and Crewlet's steps check and seal a pasted token or clear one.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch {
	case in.Step.By == seataccess.ByCrewlet && in.Step.Verb == seataccess.VerbMint:
		return s.seal(ctx, in)
	case in.Step.By == seataccess.ByCrewlet && in.Step.Op == seataccess.OpMint:
		return seataccess.StepResult{Revoked: []string{TokenField, RetiredField}}, nil
	case in.Step.By == seataccess.ByCrewlet:
		return s.clear(in), nil
	case in.Step.Verb == seataccess.VerbCreate || in.Step.Verb == seataccess.VerbMint:
		return s.awaitToken(in), nil
	case in.Step.Verb == seataccess.VerbEnable || in.Step.Verb == seataccess.VerbRerole:
		return s.awaitRole(ctx, in)
	case in.Step.Verb == seataccess.VerbRevoke:
		return s.awaitOtherTokens(ctx, in)
	case in.Step.Verb == seataccess.VerbRemove:
		return s.awaitDeleted(ctx, in)
	default:
		return seataccess.StepResult{}, fmt.Errorf("expo: no step for %q %q", in.Step.Op, in.Step.Verb)
	}
}

// awaitToken is done once a token was pasted for the seat, and otherwise says
// what to do at Expo.
func (s *accessSession) awaitToken(in seataccess.ApplyInput) seataccess.StepResult {
	if in.HandIn != "" {
		return seataccess.StepResult{}
	}
	switch {
	case in.Have.CredentialGone:
		return s.waiting(fmt.Sprintf("Expo no longer accepts robot %s's token. Create a new token "+
			"on its row and paste it here.", in.Have.Account))
	case in.Step.Op == seataccess.OpRotate || in.Step.Op == seataccess.OpMint:
		return s.waiting(fmt.Sprintf("Create a new access token on robot %s's row and paste it here.",
			in.Have.Account))
	default:
		return s.waiting(fmt.Sprintf("Add a robot named %s with the %s role on %s's access tokens "+
			"page, create an access token for it, and paste the token here.",
			RobotName(s.company, in.Seat.Handle), LevelName(in.Want.Level), s.account))
	}
}

// seal checks the pasted token for the seat and hands it to the sync to seal,
// recording the robot it proved.
func (s *accessSession) seal(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	if in.HandIn == "" {
		return s.waiting("Paste the robot's token here."), nil
	}
	checked, err := s.check(ctx, in.Seat, in.Want, in.Record, in.Step.Op == seataccess.OpRotate, in.HandIn)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	res := seataccess.StepResult{
		AccountID: checked.Robot.ID, Account: checked.Robot.Name,
		Minted: []seataccess.Minted{{Field: TokenField, Value: in.HandIn, CredentialID: checked.Prefix}},
	}
	if s.sealed(in.Seat.Handle, RetiredField) != "" {
		res.Revoked = []string{RetiredField}
	}
	return res, nil
}

// check proves value for the seat, refusing as the shared [seataccess.Refusal].
// sameRobot holds a replacement token to the robot the seat already has.
func (s *accessSession) check(
	ctx context.Context, seat seataccess.SeatRef, want seataccess.Desired, rec *seataccess.Record,
	sameRobot bool, value string,
) (Checked, error) {
	c, err := s.client(value)
	if err != nil {
		return Checked{}, err
	}
	in := CheckInput{Token: value, Account: s.account, Level: want.Level, Seat: seat.Label(),
		OwnerOf: func(id string) string {
			if h := s.robots[id]; h != "" && h != seat.Handle {
				return h
			}
			return ""
		}}
	if sameRobot && rec != nil && rec.Held() {
		in.Robot = rec.Account.ID
	}
	checked, err := Check(ctx, c, in)
	if r, ok := IsRefusal(err); ok {
		return Checked{}, &seataccess.Refusal{Said: r.Detail, Remedy: r.Hint, Actor: seataccess.ByYou,
			ActionURL: AccessTokensURL(s.account)}
	}
	return checked, err
}

// clear takes the seat's token away from its box at once, keeping its value
// where no box reads it until the robot is deleted at Expo. A disconnect keeps
// nothing, since no pass would read it again.
func (s *accessSession) clear(in seataccess.ApplyInput) seataccess.StepResult {
	if in.Disconnect {
		return seataccess.StepResult{Revoked: []string{TokenField, RetiredField}}
	}
	res := seataccess.StepResult{Revoked: []string{TokenField}}
	if token := s.sealed(in.Seat.Handle, TokenField); token != "" {
		res.Minted = []seataccess.Minted{{Field: RetiredField, Value: token}}
	}
	return res
}

// awaitRole is done once the robot holds the seat's role, read with its own
// token, and otherwise names both roles.
func (s *accessSession) awaitRole(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	c, err := s.client(s.sealed(in.Seat.Handle, TokenField))
	if err != nil {
		return seataccess.StepResult{}, err
	}
	role, err := c.RoleOn(ctx, s.account)
	status := Status(err)
	if err != nil && status != http.StatusForbidden && status != http.StatusNotFound {
		return seataccess.StepResult{}, err
	}
	if want, _ := RoleOf(in.Want.Level); err == nil && role == want {
		return seataccess.StepResult{}, nil
	}
	held := "no role"
	if err == nil {
		held = "a " + roleName(role)
	}
	return s.waiting(fmt.Sprintf("Robot %s holds %s on %s, and you chose %s. Change its role with "+
		"Edit robot at Expo, or choose its role here.", in.Have.Account, held, s.account,
		LevelName(in.Want.Level))), nil
}

// awaitOtherTokens is done once the robot holds no live token but the sealed
// one, read with that token.
func (s *accessSession) awaitOtherTokens(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	token := in.HandIn
	if token == "" {
		token = s.sealed(in.Seat.Handle, TokenField)
	}
	c, err := s.client(token)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	me, err := c.Viewer(ctx)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	var others []string
	for _, t := range othersOf(me, token) {
		others = append(others, t.Prefix+"…")
	}
	if len(others) == 0 {
		return seataccess.StepResult{}, nil
	}
	return s.waiting(fmt.Sprintf("Delete robot %s's other tokens (%s) on %s's access tokens page.",
		nameOf(me), strings.Join(others, ", "), s.account)), nil
}

// awaitDeleted is done once the turned-off robot's token answers 401, which is
// Expo saying the robot or its token is gone.
func (s *accessSession) awaitDeleted(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch {
	case in.Disconnect:
		return seataccess.StepResult{Gone: true, Did: fmt.Sprintf("Crewlet no longer tracks robot %s; "+
			"delete it on %s's access tokens page at Expo.", in.Have.Account, s.account)}, nil
	case in.Release:
		return seataccess.StepResult{Gone: true, Revoked: []string{RetiredField},
			Did: fmt.Sprintf("Robot %s stays at Expo, as you chose. Crewlet no longer tracks it.",
				in.Have.Account)}, nil
	}
	retired := s.sealed(in.Seat.Handle, RetiredField)
	if retired == "" {
		return seataccess.StepResult{Gone: true}, nil
	}
	c, err := s.client(retired)
	if err != nil {
		return seataccess.StepResult{}, err
	}
	_, err = c.Viewer(ctx)
	if Status(err) == http.StatusUnauthorized {
		return seataccess.StepResult{Gone: true, Revoked: []string{RetiredField}}, nil
	}
	if err != nil {
		return seataccess.StepResult{}, err
	}
	return s.waiting(fmt.Sprintf("Robot %s still exists at Expo. Delete it with Delete robot on "+
		"%s's access tokens page.", in.Have.Account, s.account)), nil
}

// othersOf is every live token of the robot me but the one value, which the
// robot's own token reads.
func othersOf(me Actor, value string) []Token {
	own := prefixOf(me, value)
	var out []Token
	for _, t := range me.Tokens {
		if !t.Revoked && t.Prefix != own {
			out = append(out, t)
		}
	}
	return out
}

// waiting is a person's step at Expo, pointing at the account's access tokens
// page.
func (s *accessSession) waiting(detail string) seataccess.StepResult {
	return seataccess.StepResult{Waiting: &seataccess.Waiting{ActionURL: AccessTokensURL(s.account), Detail: detail}}
}

// CheckHandIn implements [seataccess.HandsIn]: the paste route's check, with
// reads only, before the sync seals anything.
func (s *accessSession) CheckHandIn(ctx context.Context, in seataccess.HandInInput) (seataccess.HandedIn, error) {
	s.remember(in.Records)
	// A ROTATE replaces a token the seat holds and Expo still accepts, so the
	// new one must be the same robot's; any other paste may name a new robot.
	held := in.Record != nil && in.Record.Held() && s.sealed(in.Seat.Handle, TokenField) != ""
	if held {
		last := in.Record.Lanes[in.Lane].Observed
		held = last.Exists && !last.CredentialGone
	}
	checked, err := s.check(ctx, in.Seat, in.Want, in.Record, held, in.Value)
	if err != nil {
		return seataccess.HandedIn{}, err
	}
	return seataccess.HandedIn{
		Rotate: held, Account: nameOf(checked.Robot), AccountID: checked.Robot.ID, Label: checked.Prefix + "…",
		Verdict: setup.Works("Works. The token is sealed for this agent.",
			setup.Fact{Label: "Robot", Value: nameOf(checked.Robot)},
			setup.Fact{Label: "Role", Value: roleName(checked.Role)},
			setup.Fact{Label: "Account", Value: s.account},
			setup.Fact{Label: "Sealed as", Value: seataccess.SinkName(integration.KindExpo, TokenField, in.Seat.Handle)}),
	}, nil
}

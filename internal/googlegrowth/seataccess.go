package googlegrowth

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seatname"
)

// SeatAccess is a Google growth product's half of per-seat access. A seat's
// access is its level and its scope (properties, sites, accounts), as the
// company document writes them, enforced by the engine on every call; its
// identity is its Google growth identity, which the Google identity pass makes
// and turns off (internal/googleauth), never this lane. So no step is drawn:
// what a seat holds is what the document says, and the role a person gave the
// identity at the product is the ceiling, which the identity's check reads
// (package doc).
type SeatAccess struct {
	Product integration.Kind
	Lane    seataccess.Lane
	// Name is the product's name for a person.
	Name string
	// Levels builds the product's levels, least first, when one is asked
	// for, never at init: a product registers at init, which every push's
	// hook pays for (cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit).
	Levels func() []seataccess.Level
	// Scope is the company's narrowing of what seats may be given, nil where
	// it names none.
	Scope func(c *config.Company) []string
	// Identity is the product's half of each seat's own identity there,
	// built when first asked for, never at init (as Levels).
	Identity func() *Identity
	// Adds is whether Crewlet can add a seat's identity at the product with
	// the admin sign-in: exactly where [Identity.Admin] is set, which each
	// product's suite holds.
	Adds bool
	// List lists every entry a token reaches at the product, which the
	// picker offers as the company's reader sees them; nil where the product
	// has no listing.
	List func(ctx context.Context, in Lister) ([]Listed, error)
	// Typed is the shape of an entry a person may type (the company
	// document's own rule, from internal/config).
	Typed string
	// HTTP is the transport Google is reached through; nil in production.
	HTTP *http.Client
}

// Kind is the product the lane is.
func (v SeatAccess) Kind() integration.Kind { return v.Product }

// Catalog is the per-seat offer: fixed levels on the scope a seat names, which
// it must name. Nothing is sealed: tokens are minted at use.
func (v SeatAccess) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: v.Product, Lanes: []seataccess.LaneSpec{{
		Lane: v.Lane, Levels: true, Scoped: true, ScopeRequired: true,
		CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, EngineHeld: true,
		Identity: v.identitySpec(),
	}}}
}

// identitySpec is the lane's own identity: the seat's Google growth identity,
// its own by construction, which a person adds at the product or, where the
// product has an API for it, Crewlet adds with the admin sign-in.
func (v SeatAccess) identitySpec() seataccess.IdentitySpec {
	ways := []seataccess.IdentityWay{seataccess.WayInvited}
	if v.Adds {
		ways = append(ways, seataccess.WayMade)
	}
	return seataccess.IdentitySpec{Ways: ways, Default: seataccess.IdentityOwn, Name: seatname.Google, Standing: true}
}

// IdentityGuide is the sheet that adds seat's identity at the product.
func (v SeatAccess) IdentityGuide(c *config.Company, seat seataccess.SeatRef, _ seataccess.Lane,
	want seataccess.Desired) seataccess.IdentityGuide {
	return v.guide(c, seat, want, IdentityEmail(c, seat.Handle))
}

// IdentityEmail is the email of the growth identity the identity pass makes
// for c's seat handle, "" where the Google Cloud connection names no identity
// project.
func IdentityEmail(c *config.Company, handle string) string {
	if c == nil || c.Integrations.GoogleCloud == nil || c.Integrations.GoogleCloud.Provisioning == nil {
		return ""
	}
	project, ok := config.GoogleCloudServiceAccount(c.Integrations.GoogleCloud.Provisioning.Provisioner)
	if !ok {
		return ""
	}
	return googleauth.AccountEmail(googleauth.GrowthAccountID(c.Name, handle), project)
}

// Steps is empty for every op: the identity is the core's, the rest the
// document.
func (SeatAccess) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

// Reach is the company's scope where it narrows it, and open where it names
// none: the identity's own reach at the vendor then bounds it.
func (v SeatAccess) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil || !c.DeclaresIntegration(string(v.Product)) {
		return seataccess.Reach{}
	}
	if scope := v.Scope(c); len(scope) > 0 {
		return seataccess.Reach{Scope: scope}
	}
	return seataccess.Reach{Open: true}
}

// FixedLevels is the product's levels, least first.
func (v SeatAccess) FixedLevels(seataccess.Lane) []seataccess.Level { return v.Levels() }

// Open builds a session from the company's block. A product that is off, or
// without the Google identity it signs in through, is not configured.
func (v SeatAccess) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.GrowthEnabled(string(v.Product)) {
		return nil, fmt.Errorf("googlegrowth: %w: integrations.%s is off", integration.ErrNotConfigured, v.Product)
	}
	gc := in.Config.Integrations.GoogleCloud
	if gc == nil || !gc.Enabled || gc.Provisioning == nil {
		return nil, fmt.Errorf("googlegrowth: %w: %s signs in through integrations.google_cloud, which is off or "+
			"has no provisioning block", integration.ErrNotConfigured, v.Name)
	}
	return &seatSession{access: v, company: in.Config, tokens: in.Tokens, send: googleauth.NewTransport(v.HTTP),
		now: time.Now}, nil
}

// seatSession reads the document it was opened on, and checks each seat's
// identity at the product with the tokens the engine issues.
type seatSession struct {
	access  SeatAccess
	company *config.Company
	tokens  seataccess.IssuedTokens
	send    *googleauth.Transport
	now     func() time.Time
}

// Options is the levels and what the company's reader lists; the reach is
// the document's ([SeatAccess.Reach]), which [seataccess.ReadOptions] applies.
func (s *seatSession) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return s.access.options(ctx, lane, s.company, s.tokens, s.send), nil
}

// Observe is each seat's setting in the document, its account the growth
// identity's email: a seat that is on holds exactly what it is granted, since
// the engine enforces it on every call. A seat that is off holds nothing.
func (s *seatSession) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	roles := map[string]*config.Role{}
	for _, seat := range seataccess.SeatsOf(s.company) {
		roles[seat.Handle] = seat.Role
	}
	out := seataccess.Observation{}
	for _, handle := range in.Handles() {
		var observed seataccess.Observed
		if d, ok := seataccess.DesiredOf(roles[handle], s.access.Lane); ok && d.Enabled {
			email := IdentityEmail(s.company, handle)
			if rec := in.Record(handle); rec != nil {
				// THE CHECK NAMED IT AS ITS RECORD KEEPS IT, past a rename of the company.
				email = cmp.Or(checkedName(rec.Lanes[s.access.Lane].Identity), email)
			}
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: email, AccountID: email,
				Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{s.access.Lane: observed}
	}
	return out, nil
}

// Apply has nothing to take: [SeatAccess.Steps] draws no step.
func (s *seatSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("googlegrowth: no step %s exists for %s: %s access is the "+
		"document and the identity's roles there", in.Step.Op, in.Seat.Handle, s.access.Name)
}

// KeepIdentity checks the seat's identity at the product, after adding it
// there with the admin sign-in where the person asked Crewlet to
// ([seataccess.WayMade]). An identity that does not pass is answered with
// nothing to keep, as the check found it.
func (s *seatSession) KeepIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept, error) {
	if s.tokens == nil {
		return seataccess.IdentityKept{}, errNoTokens
	}
	way := seataccess.WayInvited
	if in.Way == seataccess.WayMade {
		if err := s.add(ctx, in); err != nil {
			return seataccess.IdentityKept{}, err
		}
		way = seataccess.WayMade
	}
	check, err := s.CheckIdentity(ctx, in)
	if err != nil {
		return seataccess.IdentityKept{}, err
	}
	return seataccess.IdentityKept{Check: check, Way: way, ID: check.ID, Name: check.Name}, nil
}

// CheckIdentity reads the seat's identity at the product: alive, every entry
// reached, and the role where the product shows it.
func (s *seatSession) CheckIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityCheck, error) {
	if s.tokens == nil {
		return seataccess.IdentityCheck{}, errNoTokens
	}
	return s.access.check(ctx, checkInput{company: s.company, seat: in.Seat, want: in.Want, tokens: s.tokens,
		send: s.send, now: s.now().UTC()})
}

// DropIdentity takes the identity off the seat's entries with the admin
// sign-in where the product has one and somebody signed in; elsewhere the
// identity keeps its roles at the product, and the identity pass disables it
// once the seat's last Google growth lane is off (ADR-0172).
func (s *seatSession) DropIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityDropped, error) {
	admin := s.access.Identity().Admin
	if admin == nil || s.tokens == nil || admin.Unavailable(s.company) != "" {
		return seataccess.IdentityDropped{}, nil
	}
	call, err := s.adminCall(ctx, in, false)
	_, unknown := errors.AsType[*seataccess.Refusal](err)
	switch {
	case errors.Is(err, ErrNoAdmin), unknown:
		// NOBODY TO ASK, OR NO IDENTITY MADE: nothing at the product to undo.
		return seataccess.IdentityDropped{}, nil
	case err != nil:
		return seataccess.IdentityDropped{}, err
	}
	if err := admin.Remove(ctx, call); err != nil {
		return seataccess.IdentityDropped{}, fmt.Errorf("googlegrowth: take %s off %s: %w", call.Email,
			s.access.Name, err)
	}
	return seataccess.IdentityDropped{}, nil
}

// add gives the seat's identity its level's role on each entry with the
// admin sign-in.
func (s *seatSession) add(ctx context.Context, in seataccess.IdentityInput) error {
	admin := s.access.Identity().Admin
	if admin == nil {
		return &seataccess.Refusal{Said: s.access.Name + " has no way for Crewlet to add an identity",
			Remedy: "Add it by hand with the steps shown", Actor: seataccess.ByYou}
	}
	if why := admin.Unavailable(s.company); why != "" {
		return &seataccess.Refusal{Said: why, Remedy: "Or add it by hand with the steps shown",
			Actor: seataccess.ByYou}
	}
	if manual := s.access.Identity().need(in.Want).Manual; manual != "" {
		return &seataccess.Refusal{Said: manual, Remedy: "Add it by hand with the steps shown", Actor: seataccess.ByYou}
	}
	call, err := s.adminCall(ctx, in, true)
	if errors.Is(err, ErrNoAdmin) {
		return &seataccess.Refusal{Said: "Nobody has signed in as " + admin.Who + " yet",
			Remedy: "Sign in on " + s.access.Name + "'s Settings tab, or add it by hand with the steps shown",
			Actor:  seataccess.ByYou}
	}
	if err != nil {
		return err
	}
	if err := admin.Add(ctx, call); err != nil {
		if r, ok := errors.AsType[*seataccess.Refusal](err); ok {
			return r
		}
		if api, ok := errors.AsType[*googleauth.APIError](err); ok && api.Status < http.StatusInternalServerError {
			return &seataccess.Refusal{Said: s.access.Name + " refused to add " + call.Email + ": " + api.Answer(),
				Remedy: "Sign in again as " + admin.Who + ", or add it by hand with the steps shown",
				Actor:  seataccess.ByYou}
		}
		return fmt.Errorf("googlegrowth: add %s at %s: %w", call.Email, s.access.Name, err)
	}
	return nil
}

// adminCall is the admin's call for the seat's identity: its email and,
// where reach is asked for, what its own reads find, which the admin's reads
// name each entry's parent from. [ErrNoAdmin] where nobody signed in.
func (s *seatSession) adminCall(ctx context.Context, in seataccess.IdentityInput, reach bool) (AdminCall, error) {
	id := s.access.Identity()
	token, err := s.tokens.AdminToken(ctx, s.access.Product)
	switch {
	case errors.Is(err, seataccess.ErrNotIssued):
		return AdminCall{}, ErrNoAdmin
	case err != nil:
		return AdminCall{}, fmt.Errorf("googlegrowth: %s's admin sign-in: %w", s.access.Name, err)
	}
	call := AdminCall{Company: s.company, Scope: in.Want.Scope, Need: id.need(in.Want), Token: token,
		Send: s.send}
	seat, err := s.tokens.SeatToken(ctx, s.access.Product, in.Seat.Handle, id.Scopes)
	email := seat.Account
	call.Email = email
	switch {
	case email == "" || (err != nil && reach):
		return AdminCall{}, &seataccess.Refusal{Said: "Crewlet cannot sign in as " + in.Seat.Label() +
			"'s Google identity yet", Remedy: "Add it again after the next pass", Actor: seataccess.ByCrewlet}
	case !reach:
		return call, nil
	}
	if call.Reached, err = id.Reach(ctx, Probe{Company: s.company, Email: email, Scope: in.Want.Scope,
		Token: seat.Token, Send: s.send}); err != nil {
		return AdminCall{}, fmt.Errorf("googlegrowth: read what %s reaches: %w", email, err)
	}
	return call, nil
}

// errNoTokens is a session opened with no token issuer: a preview's, which
// checks no identity.
var errNoTokens = errors.New("googlegrowth: this session was opened with no token issuer, so it checks no identity")

// checkedName is the identity the seat's last check named, "" where none ran.
func checkedName(rec *seataccess.IdentityRecord) string {
	switch {
	case rec.Kept():
		return cmp.Or(rec.Name, rec.ID)
	case rec != nil && rec.Attempt != nil:
		return cmp.Or(rec.Attempt.Name, rec.Attempt.ID)
	}
	return ""
}

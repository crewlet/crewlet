package metaads

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seatname"
	"github.com/crewlet/crewlet/internal/words"
)

// Meta Ads' half of per-seat access: access is the document's level and ad
// accounts, so no step changes Meta; a seat signs as the company's system user
// or its own (package doc, "Seat identities").

func init() { seataccess.Register(accessVendor{}) }

// SeatTokenField is the field a seat's own system user's token is sealed
// under: META_ADS_TOKEN_<HANDLE>, which no box reads (ADR-0170).
const SeatTokenField = "TOKEN"

// RetiringTokenField keeps the token of each of a seat's former system users
// sealed while Crewlet could not revoke it yet (seataccess.RetiringField), so
// each pass tries again and reads Meta refuse it.
const RetiringTokenField = "RETIRING_TOKEN"

// The levels' words, as the Agents tab draws them.
const (
	readNote       = "Reads ad accounts, campaigns, ad sets, ads, creatives, audiences and insights. No approval."
	operateNote    = "Also pauses, lowers budgets and bids and makes what nothing serves yet at once, and asks an administrator for any spend or anything customers see."
	administerNote = "Also asks an administrator for account changes, production events and deletes."
)

type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindMetaAds }

// identitySpec is how the lane offers a seat its own system user: made by
// Crewlet first, handed in by a person otherwise, the company's by default.
var identitySpec = seataccess.IdentitySpec{
	Ways:    []seataccess.IdentityWay{seataccess.WayMade, seataccess.WayHandedIn},
	Default: seataccess.IdentityCompany, Company: true, Name: seatname.MetaAds, Credential: SeatTokenField,
	Retiring: RetiringTokenField,
}

// Catalog is the lane: three fixed levels on the ad accounts a seat names,
// which it must name. Nothing is issued to a box (ADR-0170).
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: integration.KindMetaAds, Lanes: []seataccess.LaneSpec{{
		Lane: seataccess.LaneMetaAds, Levels: true, Scoped: true, ScopeRequired: true,
		CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, EngineHeld: true,
		Identity: identitySpec,
	}}}
}

// Steps is empty for every op: nothing exists outside the document to change.
func (accessVendor) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

// FixedLevels are the three, least access first; operate and administer ask
// for a step-up to give.
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	return []seataccess.Level{
		{ID: string(growth.LevelRead), Name: "Read", Note: readNote},
		{ID: string(growth.LevelOperate), Name: "Operate", Note: operateNote, Administers: true},
		{ID: string(growth.LevelAdminister), Name: "Administer", Note: administerNote, Administers: true},
	}
}

// Reach is the ad accounts the company names, which a seat's must be within;
// open where it names none, since the system user's own reach is read live.
func (accessVendor) Reach(c *config.Company, _ *org.Organization, _ seataccess.Lane) seataccess.Reach {
	if c == nil || c.Integrations.MetaAds == nil || len(c.Integrations.MetaAds.AdAccounts) == 0 {
		return seataccess.Reach{Open: true}
	}
	return seataccess.Reach{Scope: c.Integrations.MetaAds.AdAccountIDs()}
}

// SeatUserName is the name Crewlet gives seat handle's own system user.
func SeatUserName(c *config.Company, handle string) string {
	var company string
	if c != nil {
		company = c.Name
	}
	return seatname.For(seatname.MetaAds, company, handle)
}

// seatScope is a seat's level and ad accounts as the document writes them.
func seatScope(want seataccess.Desired) (growth.Level, []string) {
	var accounts []string
	for _, v := range want.Scope {
		if id := config.MetaAdAccountID(v); id != "" {
			accounts = append(accounts, id)
		}
	}
	return growth.LevelOf(want.Level), accounts
}

// canMake reports whether the document connects what Crewlet makes a system
// user with: the admin system user's token and the business.
func canMake(b *config.MetaAds) bool {
	return b != nil && strings.TrimSpace(b.AdminSystemUserToken) != "" && strings.TrimSpace(b.BusinessID) != ""
}

// IdentityGuide is the sheet that sets up a seat's own system user, in
// Business settings' own labels. Pure.
func (accessVendor) IdentityGuide(c *config.Company, seat seataccess.SeatRef, _ seataccess.Lane,
	want seataccess.Desired) seataccess.IdentityGuide {
	level, accounts := seatScope(want)
	name := SeatUserName(c, seat.Handle)
	var block *config.MetaAds
	if c != nil {
		block = c.Integrations.MetaAds
	}
	app := "the company's app"
	if block != nil && block.AppID != "" && !strings.HasPrefix(block.AppID, "${") {
		app = block.AppID
	}
	assets := "the agent's ad accounts"
	if len(accounts) > 0 {
		assets = words.And(accounts)
	}
	g := seataccess.IdentityGuide{
		Vendor: "Meta", Noun: "system user", Records: "its changes",
		Why:      "Meta's activity history then names the agent, and its token can be cut off alone.",
		Company:  "the company's system user",
		Name:     name,
		NameRule: seatname.MetaAds.Words, NameWhat: seatname.MetaAds.What,
		Role:  TasksWords(level),
		Never: "Admin",
		Steps: []seataccess.GuideStep{
			{Text: "Open Business settings, then Users, then System users."},
			{Text: "Press Add."},
			{Text: "Name:", Copy: &seataccess.GuideCopy{Label: "system user's name", Value: name,
				Rule: seatname.MetaAds.Words}},
			{Text: "Role: Employee. Press Create system user."},
			{Text: "Press Assign assets, choose " + assets + " and turn on " + TasksWords(level) + "."},
			{Text: "Press Generate token. App: " + app + ". Expiry: 60 days. Tick " + words.And(seatBase(level)) + "."},
			{Text: "Copy the token. Meta shows it once."},
		},
		Link: seataccess.GuideLink{Label: "Open System users", URL: systemUsersPage},
		Note: "Meta allows a business 10 system users at Full access and 1 at Limited, shared with every " +
			"integration there.",
		Back: seataccess.GuideBack{Kind: seataccess.BackToken, Label: "Token",
			Hint: "Crewlet checks it with Meta and seals it."},
		Checks: []seataccess.GuideCheck{
			{ID: "token", Label: "Read the token: the company's app, its permissions"},
			{ID: "user", Label: "Read its system user: an Employee of the business"},
			{ID: "accounts", Label: "Read its ad accounts: " + TasksWords(level) + " on " + assets},
		},
	}
	mk := &seataccess.GuideMake{Label: "Crewlet can make it for you",
		Does: "With the business's admin system user, Crewlet makes " + name + " an Employee system user, gives it " +
			assets + " at " + TasksWords(level) + ", and mints a 60-day token it renews.",
		Reads: []seataccess.GuideCheck{
			{ID: "make", Label: "Make " + name + ", an Employee system user"},
			{ID: "assign", Label: "Give it " + assets + ": " + TasksWords(level)},
			{ID: "mint", Label: "Mint its token: " + words.And(seatBase(level))},
		}}
	if !canMake(block) {
		mk.Unavailable = "Paste the Admin system user's token and the business portfolio ID on the Settings tab, " +
			"and Crewlet makes it for you."
	}
	g.Make = mk
	return g
}

// Open builds a session from the company's block; one absent or off is not
// configured.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, BaseURL, nil)
}

// open is [accessVendor.Open] at base, with the transport a test supplies; nil
// takes httpx's guarded client.
func (accessVendor) open(in seataccess.OpenInput, base string, hc *http.Client) (seataccess.Session, error) {
	if in.Config == nil || !in.Config.Integrations.MetaAds.GrowthEnabled() {
		return nil, fmt.Errorf("metaads: %w: integrations.meta_ads is off", integration.ErrNotConfigured)
	}
	s := &accessSession{company: in.Config, resolve: func(string) string { return "" }, now: time.Now,
		fresh: map[string]string{}}
	if in.Env != nil {
		s.resolve = func(ref string) string { return strings.TrimSpace(in.Env.Value(ref)) }
	}
	b := in.Config.Integrations.MetaAds
	appID, secret := s.resolve(b.AppID), s.resolve(b.AppSecret)
	if appID != "" && secret != "" {
		s.graph = &graph{client: NewClient(base, secret, hc), appID: appID, business: s.resolve(b.BusinessID),
			admin: s.resolve(b.AdminSystemUserToken)}
	}
	return s, nil
}

// accessSession is one pass's connection to Meta for the seats' identities.
type accessSession struct {
	company *config.Company
	resolve func(string) string
	// graph is nil where the app's ID or secret does not resolve, so no
	// identity can be made or checked.
	graph *graph
	now   func() time.Time

	// fresh are the tokens this pass sealed, by handle, which the resolver the
	// session opened with does not hold yet: a check after a renewal reads
	// the new token, never the old one it revoked.
	mu    sync.Mutex
	fresh map[string]string
}

// Options are the three levels; the ad accounts are the document's
// ([accessVendor.Reach]), which [seataccess.ReadOptions] applies.
func (s *accessSession) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: accessVendor{}.FixedLevels(lane)}, nil
}

// Observe is each seat's setting in the document, which there is nothing
// outside the document to drift from; the account is its own system user
// where it keeps one.
func (s *accessSession) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	roles := map[string]*config.Role{}
	for _, seat := range seataccess.SeatsOf(s.company) {
		roles[seat.Handle] = seat.Role
	}
	out := seataccess.Observation{}
	for _, handle := range in.Handles() {
		var observed seataccess.Observed
		if d, ok := seataccess.DesiredOf(roles[handle], seataccess.LaneMetaAds); ok && d.Enabled {
			account, id := "crewlet/"+handle, "crewlet/"+handle
			if rec := in.Record(handle); rec != nil {
				if ir := rec.Lanes[seataccess.LaneMetaAds].Identity; ir.Kept() {
					account, id = ir.Name, ir.ID
				}
			}
			observed = seataccess.Observed{Exists: true, Enabled: true, Account: account, AccountID: id,
				Level: d.Level, Scope: d.Scope}
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneMetaAds: observed}
	}
	return out, nil
}

// Apply has nothing to take: [accessVendor.Steps] draws no step.
func (*accessSession) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, fmt.Errorf("metaads: no step %s exists for %s: access is the document alone",
		in.Step.Op, in.Seat.Handle)
}

// errNoApp refuses an identity while the app's ID or secret does not resolve.
var errNoApp = &seataccess.Refusal{Said: "Meta Ads' app ID or app secret is not in the secret store, so no " +
	"token can be checked.", Remedy: "Paste them on the integration's Settings tab.", Actor: seataccess.ByYou}

// sealed is the seat's own token as sealed, "" where none is: one this pass
// sealed first.
func (s *accessSession) sealed(in seataccess.IdentityInput) string {
	s.mu.Lock()
	value, ok := s.fresh[in.Seat.Handle]
	s.mu.Unlock()
	if ok {
		return value
	}
	return s.sealedField(in, SeatTokenField)
}

// sealedField is what the store holds at the seat's field, "" for nothing.
func (s *accessSession) sealedField(in seataccess.IdentityInput, field string) string {
	sink := in.Sinks[field]
	if sink == "" {
		return ""
	}
	return s.resolve("${" + sink + "}")
}

// sealedNow notes value sealed for handle in this pass.
func (s *accessSession) sealedNow(handle, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh[handle] = value
}

// companyScopes are the permissions each of the company's tokens holds, as
// debug_token answers them: what the app proves it may grant a seat's token.
func (s *accessSession) companyScopes(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	b := s.company.Integrations.MetaAds
	for _, sl := range slots {
		value := s.resolve(sl.ref(b))
		if value == "" {
			continue
		}
		if scopes, err := s.graph.scopesOf(ctx, value); err == nil {
			out[sl.name] = scopes
		}
	}
	return out
}

// companyUser is the company's system user, read from its read token; "" where
// it does not answer.
func (s *accessSession) companyUser(ctx context.Context) string {
	value := s.resolve(s.company.Integrations.MetaAds.ReadToken)
	if value == "" {
		return ""
	}
	user, _ := s.graph.userOf(ctx, value)
	return user
}

// KeepIdentity makes the seat's own system user (WayMade) or checks the token
// a person handed in (WayHandedIn), and answers what to keep: only a token
// whose check passes. Keeping a new token revokes the one it replaces.
func (s *accessSession) KeepIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept, error) {
	if s.graph == nil {
		return seataccess.IdentityKept{}, errNoApp
	}
	level, accounts := seatScope(in.Want)
	agent := in.Seat.Label()
	var value string
	var minted []seataccess.Minted
	switch in.Way {
	case seataccess.WayMade:
		if !canMake(s.company.Integrations.MetaAds) || s.graph.admin == "" || s.graph.business == "" {
			return seataccess.IdentityKept{}, &seataccess.Refusal{
				Said: "Crewlet holds no admin system user token, so it cannot make the system user.",
				Remedy: "Paste the Admin system user's token and the business portfolio ID on the Settings tab, or " +
					"make the system user in Business settings and hand in its token.",
				Actor: seataccess.ByYou, ActionURL: systemUsersPage}
		}
		var had []string
		if old := s.sealed(in); old != "" && in.Record.Kept() && in.Record.Way == seataccess.WayMade {
			had = s.graph.accountsOf(ctx, old)
		}
		made, err := s.graph.make(ctx, SeatUserName(s.company, in.Seat.Handle), level, accounts, had,
			seatScopes(level, s.companyScopes(ctx)))
		if capErr, ok := errors.AsType[*CapError](err); ok {
			return seataccess.IdentityKept{Check: seataccess.IdentityCheck{Status: seataccess.IdentityNone,
				Blocked: fmt.Sprintf("Meta allows this business %d system users and all %d are used, so %s stays on "+
					"the company's system user.", capErr.Cap, capErr.Used, agent)}}, nil
		}
		if refusal := makeRefusal(err); refusal != nil {
			return seataccess.IdentityKept{}, refusal
		}
		if err != nil {
			return seataccess.IdentityKept{}, err
		}
		value = made.Token
		g := s.graph
		minted = []seataccess.Minted{{Field: SeatTokenField, Value: value, CredentialID: made.UserID,
			Revoke: func(ctx context.Context) error { return g.revoke(ctx, value, g.admin) }}}
	case seataccess.WayHandedIn:
		value = strings.TrimSpace(in.Value)
		if value == "" {
			return seataccess.IdentityKept{}, &seataccess.Refusal{Said: "No token was handed in.",
				Remedy: "Paste the system user's token.", Actor: seataccess.ByYou}
		}
		minted = []seataccess.Minted{{Field: SeatTokenField, Value: value}}
	default:
		return seataccess.IdentityKept{}, fmt.Errorf("metaads: %w: %q", seataccess.ErrNoIdentity, in.Way)
	}
	read, err := s.graph.check(ctx, value, level, accounts)
	check, ferr := s.checkOf(read, err, in, level)
	if ferr != nil {
		// A FAULT KEEPS NOTHING, and the sync seals nothing on one: what this
		// keep minted is revoked here.
		return seataccess.IdentityKept{}, errors.Join(ferr, revokeAll(ctx, minted))
	}
	kept := seataccess.IdentityKept{Check: check, Minted: minted, Way: in.Way, ID: read.UserID, Name: read.Name}
	if check.Status != seataccess.IdentityReady {
		return kept, nil
	}
	if refusal := s.held(ctx, in, read.UserID); refusal != nil {
		return seataccess.IdentityKept{}, errors.Join(refusal, revokeAll(ctx, minted))
	}
	kept.Minted[0].CredentialID = read.UserID
	if in.Way == seataccess.WayMade {
		kept.Check.Reads = append(madeReads(level, accounts, SeatUserName(s.company, in.Seat.Handle)),
			kept.Check.Reads...)
	}
	old := s.sealed(in)
	if in.Record.Kept() && in.Record.ID != read.UserID && old != "" {
		// A FORMER SYSTEM USER'S TOKEN stays sealed until Meta refuses it.
		kept.Minted = append(kept.Minted, seataccess.Minted{Field: seataccess.RetiringField(identitySpec,
			in.Record.ID), Value: old})
		if s.graph.admin == "" {
			kept.Check.Said += " Crewlet revokes " + in.Record.Name + "'s token; remove its ad accounts in " +
				"Business settings."
		}
	}
	kept.Retire = s.retireFor(in, value, read.UserID, old)
	return kept, nil
}

// retireFor revokes what keeping fresh for user replaces, once fresh is
// sealed: a former system user whole where the admin system user's token is
// held, else the token old.
func (s *accessSession) retireFor(in seataccess.IdentityInput, fresh, user, old string) func(context.Context) error {
	prev := in.Record
	return func(ctx context.Context) error {
		s.sealedNow(in.Seat.Handle, fresh)
		switch {
		case prev.Kept() && prev.ID != user && s.graph.admin != "" && s.graph.business != "":
			_, accounts := seatScope(in.Want)
			if old != "" {
				accounts = union(accounts, s.graph.accountsOf(ctx, old))
			}
			return s.graph.retire(ctx, prev.ID, old, accounts)
		case old != "" && old != fresh:
			return s.graph.revoke(ctx, old, fresh)
		}
		return nil
	}
}

// union is a with b's members it lacks.
func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// revokeAll undoes what a keep minted and is not keeping.
func revokeAll(ctx context.Context, minted []seataccess.Minted) error {
	var errs []error
	for _, m := range minted {
		if m.Revoke != nil {
			errs = append(errs, m.Revoke(context.WithoutCancel(ctx)))
		}
	}
	return errors.Join(errs...)
}

// held refuses a system user another seat keeps, or the company's own: each
// seat's calls must be told apart at Meta.
func (s *accessSession) held(ctx context.Context, in seataccess.IdentityInput, user string) error {
	for handle, rec := range in.Records {
		if handle == in.Seat.Handle {
			continue
		}
		if ir := rec.Lanes[seataccess.LaneMetaAds].Identity; ir.Kept() && ir.ID == user {
			return &seataccess.Refusal{Said: fmt.Sprintf("System user %s is @%s's own already.", ir.Name, handle),
				Remedy: "Make a system user for this agent.", Actor: seataccess.ByYou, ActionURL: systemUsersPage}
		}
	}
	if user != "" && user == s.companyUser(ctx) {
		return &seataccess.Refusal{Said: "This is the company's system user's token, which every agent on the " +
			"company's identity shares.", Remedy: "Make a system user for this agent.", Actor: seataccess.ByYou,
			ActionURL: systemUsersPage}
	}
	return nil
}

// outageCodes are Meta's errors that say it is down, not refusing: 0 for an
// answer that held no error object, 1 unknown, 2 service unavailable.
var outageCodes = []int{0, 1, 2}

// makeRefusal is Meta refusing to make, assign or mint, as the person's to fix;
// nil for any other error, an outage included, which is a fault to retry.
func makeRefusal(err error) *seataccess.Refusal {
	if errors.Is(err, errSeatUser) {
		return &seataccess.Refusal{Said: strings.TrimPrefix(err.Error(), "metaads: "),
			Remedy: "Rename that system user in Business settings, then try again.", Actor: seataccess.ByYou,
			ActionURL: systemUsersPage}
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && !apiErr.Throttled() && !apiErr.Transient && apiErr.Status < 500 &&
		!slices.Contains(outageCodes, apiErr.Code) {
		return &seataccess.Refusal{Said: "Meta refused it: " + apiErr.Error() + ".",
			Remedy: "Check that the admin system user's token holds business_management for the company's app, " +
				"then try again.", Actor: seataccess.ByYou, ActionURL: systemUsersPage}
	}
	return nil
}

// madeReads are the steps a make took, as the check lists them.
func madeReads(level growth.Level, accounts []string, name string) []seataccess.IdentityRead {
	return []seataccess.IdentityRead{
		{Label: "Make " + name + ", an Employee system user", OK: true},
		{Label: "Give it " + words.And(accounts) + ": " + TasksWords(level), OK: true},
		{Label: "Mint its token: " + words.And(seatBase(level)), OK: true},
	}
}

// statusOf is a check problem's status.
func statusOf(kind string) seataccess.IdentityStatus {
	switch kind {
	case ProblemRevoked:
		return seataccess.IdentityRefused
	case ProblemWrongTeam:
		return seataccess.IdentityWrongTeam
	}
	return seataccess.IdentityWrongRole
}

// checkOf is a check's result as the record keeps it; a fault is answered as
// the error.
func (s *accessSession) checkOf(read SeatRead, err error, in seataccess.IdentityInput,
	level growth.Level) (seataccess.IdentityCheck, error) {
	labels := []string{"Read the token: the company's app, its permissions",
		"Read its system user: an Employee of the business", "Read its ad accounts and tasks"}
	out := seataccess.IdentityCheck{ID: read.UserID, Name: read.Name, Needs: TasksWords(level),
		Role: s.roleOf(read, in), Team: s.team()}
	problem, isProblem := errors.AsType[*SeatProblem](err)
	if err != nil && !isProblem {
		return out, err
	}
	for i, label := range labels {
		r := seataccess.IdentityRead{Label: label, OK: !isProblem || i < problem.At}
		if isProblem && i == problem.At {
			r.Said = problem.Said
		}
		out.Reads = append(out.Reads, r)
		if isProblem && i == problem.At {
			break
		}
	}
	if isProblem {
		out.Status, out.Said, out.Remedy = statusOf(problem.Kind), problem.Said, problem.Remedy
		if problem.Held != "" {
			out.Role = problem.Held
		}
		return out, nil
	}
	out.Status, out.Broader = seataccess.IdentityReady, read.Broader
	out.Said = strings.TrimSpace("Ready. " + in.Seat.Label() + " now calls Meta as " + read.Name + ". " +
		strings.Join(read.Warnings, " "))
	if !read.Expires.IsZero() {
		out.Said += " Its token ends on " + read.Expires.Format("2 January 2006") + "; Crewlet renews it before then."
	}
	return out, nil
}

// roleOf is what the system user holds on the seat's ad accounts, in Business
// settings' words.
func (s *accessSession) roleOf(read SeatRead, in seataccess.IdentityInput) string {
	_, accounts := seatScope(in.Want)
	var held []string
	for _, a := range accounts {
		for _, t := range read.Tasks[a] {
			if !slices.Contains(held, t) {
				held = append(held, t)
			}
		}
	}
	if len(held) == 0 {
		return ""
	}
	return tasksText(held)
}

// team is the business the system users belong to, as the box names it.
func (s *accessSession) team() string {
	if s.graph != nil && s.graph.business != "" {
		return "business " + s.graph.business
	}
	if s.graph != nil {
		return "the business of app " + s.graph.appID
	}
	return ""
}

// CheckIdentity reads back the kept token against the seat's level and ad
// accounts, and each former system user's sealed token, gone once Meta
// refuses it.
func (s *accessSession) CheckIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityCheck, error) {
	level, accounts := seatScope(in.Want)
	if s.graph == nil {
		return seataccess.IdentityCheck{Status: seataccess.IdentityRefused, Said: errNoApp.Said,
			Remedy: errNoApp.Remedy}, nil
	}
	gone, err := s.retiringGone(ctx, in)
	if err != nil || !in.Record.Kept() {
		return seataccess.IdentityCheck{RetiringGone: gone}, err
	}
	value := s.sealed(in)
	if value == "" {
		return seataccess.IdentityCheck{Status: seataccess.IdentityRefused, ID: in.Record.ID, Name: in.Record.Name,
			Said: "The system user's token is no longer in the secret store.", Remedy: "Set up the identity again.",
			RetiringGone: gone}, nil
	}
	read, err := s.graph.check(ctx, value, level, accounts)
	check, err := s.checkOf(read, err, in, level)
	check.RetiringGone = gone
	return check, err
}

// retiringGone are the former system users whose token Crewlet keeps sealed
// and Meta refuses now, or with none sealed.
func (s *accessSession) retiringGone(ctx context.Context, in seataccess.IdentityInput) ([]string, error) {
	if in.Record == nil {
		return nil, nil
	}
	var gone []string
	for _, r := range in.Record.Retiring {
		retiring := s.sealedField(in, seataccess.RetiringField(identitySpec, r.ID))
		if retiring == "" {
			gone = append(gone, r.ID)
			continue
		}
		d, err := s.graph.debug(ctx, retiring)
		if err != nil {
			return gone, err
		}
		if !d.Data.IsValid {
			gone = append(gone, r.ID)
		}
	}
	return gone, nil
}

// DropIdentity revokes the seat's own token, and with the admin system user's
// token every other one and its ad accounts; a token it cannot revoke keeps
// the identity rather than be forgotten alive.
func (s *accessSession) DropIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityDropped, error) {
	out := seataccess.IdentityDropped{Revoked: []string{SeatTokenField}}
	value := s.sealed(in)
	if !in.Record.Kept() || value == "" && (s.graph == nil || s.graph.admin == "") {
		return out, nil
	}
	if s.graph == nil {
		return out, fmt.Errorf("metaads: revoke %s's own token: %w", in.Seat.Handle, errNoApp)
	}
	if s.graph.admin != "" && s.graph.business != "" {
		_, accounts := seatScope(in.Want)
		if value != "" {
			accounts = union(accounts, s.graph.accountsOf(ctx, value))
		}
		if err := s.graph.retire(ctx, in.Record.ID, value, accounts); err != nil {
			return out, fmt.Errorf("metaads: give up %s's system user %s: %w", in.Seat.Handle, in.Record.Name, err)
		}
		return out, nil
	}
	if err := s.graph.revoke(ctx, value, value); err != nil {
		return out, fmt.Errorf("metaads: revoke %s's own token: %w", in.Seat.Handle, err)
	}
	return out, nil
}

// RefreshIdentity brings a made system user to a changed level or ad account
// list, and renews a token within [renewAt] of its end; the old token is
// revoked once the new one is sealed.
func (s *accessSession) RefreshIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityRefreshed, error) {
	if s.graph == nil || !in.Record.Kept() {
		return seataccess.IdentityRefreshed{}, nil
	}
	g := s.graph
	for _, r := range in.Record.Retiring {
		// A FORMER SYSTEM USER'S TOKEN Crewlet could not revoke: try again; the
		// check reads whether Meta refuses it now.
		if retiring := s.sealedField(in, seataccess.RetiringField(identitySpec, r.ID)); retiring != "" {
			_ = g.revoke(ctx, retiring, retiring)
		}
	}
	value := s.sealed(in)
	if value == "" {
		return seataccess.IdentityRefreshed{}, nil
	}
	debug, err := g.debug(ctx, value)
	if err != nil || !debug.Data.IsValid {
		// THE CHECK SAYS IT: a token Meta no longer accepts renews nothing.
		return seataccess.IdentityRefreshed{}, err
	}
	level, accounts := seatScope(in.Want)
	retire := func(ctx context.Context, fresh string) error {
		s.sealedNow(in.Seat.Handle, fresh)
		return g.revoke(ctx, value, fresh)
	}
	if in.Record.Way == seataccess.WayMade && g.admin != "" && g.business != "" {
		read := SeatRead{Scopes: debug.Data.Scopes, Tasks: map[string][]string{}}
		if err = g.readTasks(ctx, value, &read); err != nil {
			return seataccess.IdentityRefreshed{}, err
		}
		if drifted(level, accounts, read) {
			var made SeatMade
			made, err = g.remake(ctx, systemUser{ID: in.Record.ID, Name: in.Record.Name}, level, accounts,
				slices.Collect(maps.Keys(read.Tasks)), seatScopes(level, s.companyScopes(ctx)))
			if err != nil {
				return seataccess.IdentityRefreshed{}, fmt.Errorf("metaads: bring %s's system user to its level: %w",
					in.Seat.Handle, err)
			}
			return refreshed(g, made.Token, retire), nil
		}
	}
	expires := time.Unix(debug.Data.ExpiresAt, 0).UTC()
	if debug.Data.ExpiresAt == 0 || expires.Sub(s.now().UTC()) > renewAt {
		return seataccess.IdentityRefreshed{}, nil
	}
	fresh, err := g.renewal(ctx, in.Record.ID, value, seatScopes(level, s.companyScopes(ctx)))
	if err != nil {
		return seataccess.IdentityRefreshed{}, fmt.Errorf("metaads: renew %s's own token: %w", in.Seat.Handle, err)
	}
	if fresh == value {
		return seataccess.IdentityRefreshed{}, nil
	}
	return refreshed(g, fresh, retire), nil
}

// refreshed is a fresh token to seal, revoked itself if it cannot be, with the
// old one's revoke once it is.
func refreshed(g *graph, fresh string, retire func(context.Context, string) error) seataccess.IdentityRefreshed {
	return seataccess.IdentityRefreshed{
		Minted: []seataccess.Minted{{Field: SeatTokenField, Value: fresh,
			Revoke: func(ctx context.Context) error { return g.revoke(ctx, fresh, fresh) }}},
		Retire: func(ctx context.Context) error { return retire(ctx, fresh) },
	}
}

// drifted reports a system user whose ad accounts, tasks or token permissions
// no longer match the seat's level and scope.
func drifted(level growth.Level, accounts []string, read SeatRead) bool {
	if seatPolicy(level, read.Scopes) != "" {
		return true
	}
	for _, a := range accounts {
		if held, ok := read.Tasks[a]; !ok || len(missing(held, seatTasks(level))) > 0 {
			return true
		}
	}
	for a, held := range read.Tasks {
		if !slices.Contains(accounts, a) || level != growth.LevelAdminister && slices.Contains(held, taskManage) {
			return true
		}
	}
	return false
}

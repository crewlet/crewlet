package googlecloud

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// Google Cloud's half of per-seat access. The seat's account is its service
// account, recorded in the ledger by unique ID; its access is its roles on
// each project it names, granted through the provisioner's three conditions;
// its credential is an access token the engine issues at use, never sealed.
//
// THE DISABLE IS WHAT "OFF" MEANS. Off disables the account first, so the seat
// stops everywhere at once even where a later write fails, then removes its
// roles, so an account somebody turns back on at Google holds nothing. The
// account and its audit history stay, and an account disabled at Google by
// somebody else is never turned back on unasked (D3).

func init() { seataccess.Register(accessVendor{}) }

// accessVendor is Google Cloud's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindGoogleCloud }

// Catalog is Google Cloud's per-seat offer: three fixed levels on the projects
// a seat names, which it must name. NOTHING IS SEALED: the seat's credential
// is the access token the engine issues at use for an hour ([AccessTokenField]),
// so there is nothing to rotate, and no box variable is a person's to set: the
// seat's tools are Google's MCP servers, which take the issued token.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind: integration.KindGoogleCloud,
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneGoogleCloud, Levels: true, Scoped: true, ScopeRequired: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, Issued: AccessTokenField,
		}},
	}
}

// leftBehind is the caveat on a step that takes Administer away.
const leftBehind = "Administer could leave access behind on what it reached: SSH keys or startup scripts " +
	"on VMs, database users and certificates, and bucket or DNS zone policies. Review and remove those."

// Steps is each op as Google takes it, in the order Apply takes it. Pure.
func (accessVendor) Steps(
	_ seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	out := googleSteps(op, want, have)
	for i := range out {
		out[i].Index = i
	}
	return out
}

func googleSteps(op seataccess.Op, want seataccess.Desired, have seataccess.Observed) []seataccess.Step {
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	grant := LevelName(want.Level) + " on " + integration.Count(len(want.Scope), "project")
	losesAdminister := have.Level == config.GoogleCloudLevelAdminister && want.Level != config.GoogleCloudLevelAdminister
	switch op {
	case seataccess.OpCreate:
		return []seataccess.Step{
			step(seataccess.VerbCreate, "Create its own service account in the identity project"),
			step(seataccess.VerbGrant, "Let Crewlet issue its token, on this account alone"),
			step(seataccess.VerbGrant, "Grant "+grant+", one write per level"),
		}
	case seataccess.OpEnable:
		return []seataccess.Step{step(seataccess.VerbEnable, "Turn its service account back on and grant "+grant)}
	case seataccess.OpLevel:
		s := step(seataccess.VerbRerole, "Change it to "+grant)
		if losesAdminister {
			s.Note = leftBehind
		}
		return []seataccess.Step{s}
	case seataccess.OpScope:
		s := step(seataccess.VerbScope, scopeText(have.Scope, want.Scope, want.Level))
		if have.Level == config.GoogleCloudLevelAdminister && slices.ContainsFunc(have.Scope, func(p string) bool {
			return !slices.Contains(want.Scope, p)
		}) {
			s.Note = leftBehind
		}
		return []seataccess.Step{s}
	case seataccess.OpDisable:
		off := step(seataccess.VerbDisable, "Disable its service account, so it stops at once")
		if have.Level == config.GoogleCloudLevelAdminister {
			off.Note = leftBehind
		}
		return []seataccess.Step{off, step(seataccess.VerbRevoke,
			"Remove its roles on every project. The account and its audit history stay")}
	case seataccess.OpRemove:
		return []seataccess.Step{step(seataccess.VerbRemove,
			"Delete its service account. Cloud Audit Logs keep its entries")}
	default:
		// A mint, rotate, revoke or retire has nothing to take: the token
		// is issued at use for an hour and never sealed.
		return nil
	}
}

// scopeText is a project change as a step draws it.
func scopeText(have, want []string, level string) string {
	var add, drop int
	for _, p := range want {
		if !slices.Contains(have, p) {
			add++
		}
	}
	for _, p := range have {
		if !slices.Contains(want, p) {
			drop++
		}
	}
	var parts []words.Said
	if add > 0 {
		parts = append(parts, words.Said("grant "+LevelName(level)+" on "+integration.Count(add, "more project")))
	}
	if drop > 0 {
		parts = append(parts, words.Said("remove its roles on "+integration.Count(drop, "project")))
	}
	if len(parts) == 0 {
		return "Grant " + LevelName(level) + " on its projects"
	}
	text := words.And(parts)
	return strings.ToUpper(text[:1]) + text[1:]
}

// Open builds a session from the company's Google Cloud block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil, nil)
}

// open is [accessVendor.Open] with the transport and clock a test supplies;
// nil takes the defaults.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client, clk *clock) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("googlecloud: %w: no company document", integration.ErrNotConfigured)
	}
	client, err := ClientFor(in.Config, in.Env, hc)
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		return nil, err
	case err != nil:
		return nil, &seataccess.Refusal{
			Said: "Google Cloud: " + err.Error(), Actor: seataccess.ByYou, ActionURL: ProvidersURL,
			Remedy: "Correct the value it names on the Google Cloud connection",
		}
	}
	if clk != nil {
		client.clock = *clk
	}
	return &accessSession{client: client, company: in.Config.Name}, nil
}

// ClientFor is the provisioner's client for company, from its document as env
// resolves it. An error wrapping [integration.ErrNotConfigured] is a company
// that does not provision Google Cloud yet: the block absent or off, or its
// provider or key not set or not resolving. Any other error names the value
// that is wrong. hc is nil in production.
func ClientFor(c *config.Company, env *config.Resolver, hc *http.Client) (*Client, error) {
	if c == nil || c.Integrations.GoogleCloud == nil || !c.Integrations.GoogleCloud.Enabled ||
		c.Integrations.GoogleCloud.Provisioning == nil {
		return nil, fmt.Errorf("googlecloud: %w: integrations.google_cloud is off or has no provisioning block",
			integration.ErrNotConfigured)
	}
	if env == nil {
		return nil, fmt.Errorf("googlecloud: %w: nothing resolves the federation key", integration.ErrNotConfigured)
	}
	prov := c.Integrations.GoogleCloud.Provisioning
	values := map[string]string{
		"provisioner":    strings.TrimSpace(env.Value(prov.Provisioner)),
		"provider":       strings.TrimSpace(env.Value(prov.Provider)),
		"federation_key": strings.TrimSpace(env.Value(prov.FederationKey)),
	}
	for _, field := range []string{"provisioner", "provider", "federation_key"} {
		if values[field] == "" {
			return nil, fmt.Errorf("googlecloud: %w: integrations.google_cloud.provisioning.%s is not set or "+
				"did not resolve", integration.ErrNotConfigured, field)
		}
	}
	return NewClient(ClientOptions{
		Company: c.Name, Provisioner: values["provisioner"], Provider: values["provider"],
		FederationKey: values["federation_key"], HTTP: hc,
	})
}

// accessSession is one pass's connection to Google as the provisioner.
type accessSession struct {
	client  *Client
	company string

	// The company's projects and the levels Google's roles leave grantable,
	// each read once per pass and only where a step or an offer needs it.
	mu    sync.Mutex
	found *discovery
	gaps  *[]seataccess.LevelGap
}

// discovered reads, once per pass, the company's projects.
func (s *accessSession) discovered(ctx context.Context) (discovery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.found != nil {
		return *s.found, nil
	}
	d, err := discover(ctx, s.client)
	if err != nil {
		return nil, err
	}
	s.found = &d
	return d, nil
}

// levelGaps reads, once per pass, the levels Google's roles leave grantable.
func (s *accessSession) levelGaps(ctx context.Context) ([]seataccess.LevelGap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gaps != nil {
		return *s.gaps, nil
	}
	gaps, err := levelGaps(ctx, s.client)
	if err != nil {
		return nil, err
	}
	s.gaps = &gaps
	return gaps, nil
}

// Options is Google Cloud's three levels, least access first, each Google's
// roles cannot be granted at offered with why, and the company's projects set
// up for Crewlet, by ID with Google's display names.
func (s *accessSession) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	d, err := s.discovered(ctx)
	if err != nil {
		return seataccess.Options{}, err
	}
	gaps, err := s.levelGaps(ctx)
	if err != nil {
		return seataccess.Options{}, err
	}
	scope, names := d.offered()
	return seataccess.Options{
		Lane: lane, Levels: seataccess.OfferGaps(accessVendor{}.FixedLevels(lane), gaps),
		Scope: scope, ScopeNames: names, ScopeLive: true, Scoped: true, ScopeRequired: true,
		ActionURL: IAMURL,
	}, nil
}

// FixedLevels is Google Cloud's levels, least access first
// ([seataccess.FixedLevels]).
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	out := make([]seataccess.Level, 0, len(levels))
	for _, l := range levels {
		out = append(out, seataccess.Level{ID: l.id, Name: l.name, Note: l.note, Administers: l.administers})
	}
	return out
}

// Observe reads back the account the ledger records for each seat, by unique
// ID. A seat with no recorded account holds none: nothing is adopted by name.
func (s *accessSession) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	for _, handle := range in.Handles() {
		rec := in.Record(handle)
		if rec == nil || !rec.Held() {
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneGoogleCloud: {}}
			continue
		}
		observed, err := s.observe(ctx, *rec)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil && isStage(err):
			// THE PROVISIONER IS UNREACHABLE, which is every seat's read alike.
			return nil, fmt.Errorf("googlecloud: read %s's service account: %w", handle, reject(err))
		case err != nil:
			faults[handle] = err
			continue
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneGoogleCloud: observed}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observe is one seat's account as the shared plan reads it.
//
// A STALE ANSWER INSIDE THE BOUND IS NEVER DRIFT: within observeBudget of the
// lane's last write, an account that does not read back as that sync left it
// is read again until it does or the bound ends, and only then is what Google
// shows what the plan judges.
func (s *accessSession) observe(ctx context.Context, rec seataccess.Record) (seataccess.Observed, error) {
	settled := lastWrite(rec).Add(observeBudget)
	var got seataccess.Observed
	_, err := s.client.clock.awaitObserved(ctx, readLastWrites, func() (bool, error) {
		var err error
		if got, err = s.read(ctx, rec); err != nil {
			return true, err
		}
		return !s.client.clock.now().Before(settled) || agrees(rec, got), nil
	})
	if _, settling := SettlingReason(err); settling {
		return got, nil
	}
	return got, err
}

// lastWrite is when the engine last wrote this seat's account: its creation,
// the last sync of its lane that took a step, or the write its recorded
// settling waits on.
//
// THE SETTLING IS THE DURABLE MARK: a quiet pass after a write replaces the
// lane's sync with one that took no step, and the write it waits on is still
// the last one, so a stale read then is still inside the bound.
func lastWrite(rec seataccess.Record) time.Time {
	var at time.Time
	if rec.Account != nil {
		at = rec.Account.CreatedAt
	}
	lr, ok := rec.Lanes[seataccess.LaneGoogleCloud]
	if !ok {
		return at
	}
	if lr.Sync != nil && lr.Sync.At.After(at) &&
		slices.ContainsFunc(lr.Sync.Steps, func(s seataccess.StepOutcome) bool { return s.Done }) {
		at = lr.Sync.At
	}
	if written := writtenAt(lr.Settling); written.After(at) {
		at = written
	}
	return at
}

// agrees reports an observation that is what the lane's last converged sync
// left, or a lane with no such sync to compare with.
func agrees(rec seataccess.Record, got seataccess.Observed) bool {
	lr, ok := rec.Lanes[seataccess.LaneGoogleCloud]
	if !ok || lr.Applied == nil || lr.Sync == nil || lr.Sync.Outcome != seataccess.OutcomeOK {
		return true
	}
	if !lr.Applied.Enabled {
		return got.Exists && !got.Enabled && len(got.Scope) == 0
	}
	return got.Exists && got.Enabled && got.Level == lr.Applied.Level && got.Access().SameScope(*lr.Applied)
}

// read is one seat's account as Google holds it now: absent where Google no
// longer has it, its level only where Crewlet can issue its token and every
// project it is bound on holds exactly one level's roles.
func (s *accessSession) read(ctx context.Context, rec seataccess.Record) (seataccess.Observed, error) {
	sa, found, err := s.client.ServiceAccount(ctx, rec.Account.ID)
	if err != nil || !found {
		return seataccess.Observed{}, err
	}
	out := seataccess.Observed{
		Exists: true, Enabled: !sa.Disabled, Account: sa.Email, AccountID: sa.UniqueID,
		CreatedAt: rec.Account.CreatedAt,
	}
	if sa.Disabled {
		out.DisabledBy = seataccess.DisabledByOf(false, rec, true)
	}
	delegated, err := s.delegated(ctx, sa.Email)
	if err != nil {
		return seataccess.Observed{}, err
	}
	bound, err := s.bound(ctx, sa.Email, recordedScope(rec))
	if err != nil {
		return seataccess.Observed{}, err
	}
	level, mixed := "", false
	for _, project := range slices.Sorted(mapKeysOf(bound)) {
		out.Scope = append(out.Scope, project)
		switch held := levelHeld(bound[project], project); {
		case held == "":
			mixed = true
		case level == "":
			level = held
		case held != level:
			mixed = true
		}
	}
	// THE PROVISIONER'S GRANT IS PART OF THE LEVEL: without Token Creator on
	// the account no token can be issued, so it holds no level in effect,
	// and the next level step grants it again.
	if delegated && !mixed {
		out.Level = level
	}
	return out, nil
}

// delegated reports an account whose own policy lets the provisioner issue
// its token.
func (s *accessSession) delegated(ctx context.Context, email string) (bool, error) {
	pol, err := s.client.AccountPolicy(ctx, email)
	if err != nil {
		return false, err
	}
	return slices.Contains(heldBy(pol, member(s.client.Provisioner())).plain, tokenCreator), nil
}

// tokenCreator is the role that lets the provisioner issue a seat's token,
// granted on each seat's own account and never on a project.
const tokenCreator = "roles/iam.serviceAccountTokenCreator"

// recordedScope is every project the ledger last saw or applied for a seat.
func recordedScope(rec seataccess.Record) []string {
	lr := rec.Lanes[seataccess.LaneGoogleCloud]
	out := slices.Clone(lr.Observed.Scope)
	if lr.Applied != nil {
		out = append(out, lr.Applied.Scope...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// bound is every role email holds on each project that binds it: the
// company's projects, and every project the ledger names for the seat. A
// project the ledger names that cannot be read is the seat's fault; another
// that cannot be read is one Crewlet never granted on.
func (s *accessSession) bound(ctx context.Context, email string, recorded []string) (map[string][]string, error) {
	d, err := s.discovered(ctx)
	if err != nil {
		return nil, err
	}
	projects := slices.Clone(recorded)
	for _, p := range d {
		projects = append(projects, p.ID)
	}
	slices.Sort(projects)
	out := map[string][]string{}
	for _, project := range slices.Compact(projects) {
		pol, err := s.client.ProjectPolicy(ctx, project)
		switch {
		case err != nil && (isStage(err) || slices.Contains(recorded, project)):
			return nil, fmt.Errorf("googlecloud: read the policy of %s: %w", project, err)
		case err != nil:
			continue
		}
		if roles := heldBy(pol, member(email)).all(); len(roles) > 0 {
			out[project] = roles
		}
	}
	return out, nil
}

func mapKeysOf[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// writes is what one step wrote at Google, which decides what the seat's tool
// waits on after it ([settleFor]): a grant, any other change, or nothing.
type writes struct {
	granted, changed bool
	// at is when the last write was made.
	at time.Time
}

// note records a write made at at: a grant of access, or any other change.
// A nil writes records nothing, for a step whose seat is turned off.
func (w *writes) note(grant bool, at time.Time) {
	if w == nil {
		return
	}
	if grant {
		w.granted = true
	} else {
		w.changed = true
	}
	w.at = at
}

// result is the step's answer: what the seat's tool waits on, from the last
// write, or nothing where the step wrote nothing.
func (w *writes) result() seataccess.StepResult {
	switch {
	case w.granted:
		return seataccess.StepResult{Settling: settleFor(ReasonGranting, w.at)}
	case w.changed:
		return seataccess.StepResult{Settling: settleFor(ReasonApplying, w.at)}
	}
	return seataccess.StepResult{}
}

// Apply takes one step at Google, chosen by its op and its place in the op.
//
// A STEP THAT WROTE SAYS WHAT THE SEAT WAITS ON ([seataccess.StepResult.Settling]):
// Google is still creating a new account, still applying a grant, which its
// token waits on, or still applying any other change. The sync ends OK all
// the same, since nobody owes a step.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch in.Step.Op {
	case seataccess.OpCreate:
		switch in.Step.Index {
		case 0:
			return s.create(ctx, in)
		case 1:
			return s.withAccount(ctx, in, func(sa ServiceAccount) (seataccess.StepResult, error) {
				var w writes
				err := s.delegate(ctx, sa, &w)
				return w.result(), err
			})
		default:
			return s.grantLevel(ctx, in)
		}
	case seataccess.OpEnable:
		return s.enable(ctx, in)
	case seataccess.OpLevel, seataccess.OpScope:
		return s.grantLevel(ctx, in)
	case seataccess.OpDisable:
		if in.Step.Index == 0 {
			return s.disable(ctx, in)
		}
		return s.withAccount(ctx, in, func(sa ServiceAccount) (seataccess.StepResult, error) {
			return seataccess.StepResult{}, s.converge(ctx, sa.Email, seataccess.Desired{}, known(in), nil)
		})
	case seataccess.OpRemove:
		return s.remove(ctx, in)
	default:
		return seataccess.StepResult{}, fmt.Errorf("googlecloud: no step for %q", in.Step.Op)
	}
}

// create makes the seat's service account in the identity project and waits
// until it reads back. Its token grant and its roles are the op's next steps.
func (s *accessSession) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	id := AccountID(s.company, in.Seat.Handle)
	email := AccountEmail(id, s.client.IdentityProject())
	sa, err := s.client.CreateServiceAccount(ctx, id, displayName(s.company, in.Seat.Handle),
		description(s.company, in.Seat.Handle))
	made := s.client.clock.now()
	switch {
	case Status(err) == http.StatusConflict && recordedAs(in.Record, email):
		// THE ACCOUNT THIS SEAT HAD, still being deleted at Google: the
		// next pass makes the new one once the old has gone.
		return seataccess.StepResult{}, &Settling{What: string(readDeleted), Reason: ReasonCreating, Last: err}
	case Status(err) == http.StatusConflict:
		return seataccess.StepResult{}, &seataccess.Refusal{
			Said: "Google Cloud: a service account " + email + " already exists, and Crewlet has no record of making it",
			Remedy: "Nothing is adopted by its name. Delete it in the identity project if nobody uses it, " +
				"then retry",
			Actor: seataccess.ByYou, ActionURL: projectURL(ServiceAccountsURL, s.client.IdentityProject()),
		}
	case err != nil:
		return seataccess.StepResult{}, s.refused(err, "")
	}
	if _, err := s.client.clock.awaitObserved(ctx, readCreated, func() (bool, error) {
		_, found, err := s.client.ServiceAccount(ctx, sa.UniqueID)
		return found || (err != nil && !gone(err)), err
	}); err != nil {
		return seataccess.StepResult{AccountID: sa.UniqueID, Account: sa.Email}, s.refused(err, "")
	}
	return seataccess.StepResult{AccountID: sa.UniqueID, Account: sa.Email,
		Settling: settleFor(ReasonCreating, made)}, nil
}

// recordedAs reports a ledger record whose account had email.
func recordedAs(rec *seataccess.Record, email string) bool {
	return rec != nil && rec.Account != nil && rec.Account.Name == email
}

// delegate grants the provisioner Token Creator on the seat's account alone,
// waiting out a new account its own policy does not know yet, and until the
// grant reads back. A grant it writes is noted in w, since the seat's token
// waits on it.
func (s *accessSession) delegate(ctx context.Context, sa ServiceAccount, w *writes) error {
	prov := member(s.client.Provisioner())
	if _, err := s.client.clock.awaitObserved(ctx, readDelegated, func() (bool, error) {
		wrote := false
		err := s.client.EditAccountPolicy(ctx, sa.Email, func(p *Policy) bool {
			wrote = grant(p, prov, []string{tokenCreator})
			return wrote
		})
		switch Status(err) {
		case http.StatusNotFound, http.StatusBadRequest, http.StatusConflict:
			return false, err
		}
		if err == nil && wrote {
			w.note(true, s.client.clock.now())
		}
		return true, err
	}); err != nil {
		return s.refused(err, "")
	}
	_, err := s.client.clock.awaitObserved(ctx, readDelegated, func() (bool, error) {
		ok, err := s.delegated(ctx, sa.Email)
		return ok || err != nil, err
	})
	return s.refused(err, "")
}

// grantLevel grants the seat's level on its projects and takes it off every
// other, its token grant first.
func (s *accessSession) grantLevel(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withAccount(ctx, in, func(sa ServiceAccount) (seataccess.StepResult, error) {
		if err := s.grantable(ctx, in.Want.Level); err != nil {
			return seataccess.StepResult{}, err
		}
		var w writes
		if err := s.delegate(ctx, sa, &w); err != nil {
			return seataccess.StepResult{}, err
		}
		err := s.converge(ctx, sa.Email, in.Want, known(in), &w)
		return w.result(), err
	})
}

// grantable refuses a level Google's roles cannot be granted at now.
func (s *accessSession) grantable(ctx context.Context, level string) error {
	if _, _, ok := levelOf(level); !ok {
		return &seataccess.Refusal{
			Said:   fmt.Sprintf("Google Cloud: %q is not a level; give %s", level, words.Or(Levels())),
			Remedy: "Choose a level for this agent on the Agents tab", Actor: seataccess.ByYou,
		}
	}
	gaps, err := s.levelGaps(ctx)
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(gaps, func(g seataccess.LevelGap) bool { return g.Level == level }); i >= 0 {
		return &seataccess.Refusal{
			Said: "Google Cloud: " + gaps[i].Reason, Actor: seataccess.ByYou, ActionURL: gaps[i].ActionURL,
			Remedy: gaps[i].Remedy + ", or choose another level for this agent on the Agents tab",
		}
	}
	return nil
}

// known is every project a step may find the seat bound on: the document's,
// what the pass observed, and what the ledger last applied.
func known(in seataccess.ApplyInput) []string {
	out := slices.Concat(in.Want.Scope, in.Have.Scope)
	if in.Record != nil {
		out = append(out, recordedScope(*in.Record)...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// converge makes email hold want's level on want's projects and nothing on
// every other of projects, one write per level list a project crosses: on each
// project the lists it holds go first, least access first, then the lists it
// no longer holds, most access first, so no moment holds a level below the
// one it is moving to. A new account Google does not know on a project yet is
// waited out, and every project's roles are read back as written.
//
// A ROLE NO LEVEL GRANTS IS NOT CREWLET'S TO TAKE: the provisioner's
// conditions refuse revoking it, so it is left and named after the rest.
//
// Each write is noted in w: a list granted, or a list taken off.
func (s *accessSession) converge(
	ctx context.Context, email string, want seataccess.Desired, projects []string, w *writes,
) error {
	who := member(email)
	var foreign []string
	for _, project := range projects {
		pol, err := s.client.ProjectPolicy(ctx, project)
		if err != nil {
			return s.refused(err, project)
		}
		target := []string{}
		top := -1
		if want.Enabled && slices.Contains(want.Scope, project) {
			target, _ = LevelRoles(want.Level, project)
			_, top, _ = levelOf(want.Level)
		}
		for _, role := range heldBy(pol, who).all() {
			if !levelRole(role, project) {
				foreign = append(foreign, project+": "+role)
			}
		}
		lists := Lists(project)
		for i := range lists {
			if i > top {
				break
			}
			if err := s.writeList(ctx, project, email, lists[i], true, w); err != nil {
				return err
			}
		}
		for i := len(lists) - 1; i > top; i-- {
			if err := s.writeList(ctx, project, email, lists[i], false, w); err != nil {
				return err
			}
		}
		if _, err := s.client.clock.awaitObserved(ctx, readBindings, func() (bool, error) {
			pol, err := s.client.ProjectPolicy(ctx, project)
			if err != nil {
				return true, err
			}
			h := heldBy(pol, who)
			var levelHeld []string
			for _, role := range h.plain {
				if levelRole(role, project) {
					levelHeld = append(levelHeld, role)
				}
			}
			return slices.Equal(levelHeld, target) && !slices.ContainsFunc(h.conditional, func(r string) bool {
				return levelRole(r, project)
			}), nil
		}); err != nil {
			return s.refused(err, project)
		}
	}
	if len(foreign) > 0 {
		return &seataccess.Refusal{
			Said: "Google Cloud: the agent's account also holds " + words.And(foreign) + ", which no level grants " +
				"and Crewlet's provisioner cannot remove",
			Remedy: "Remove those roles from the agent's account on the project's IAM page",
			Actor:  seataccess.ByYou, ActionURL: IAMURL,
		}
	}
	return nil
}

// writeList is one write of one level list on project: on, so email holds
// every role of it unconditionally, or off, so it holds none of it. A write
// that changes nothing is not made; one that is made is noted in w.
func (s *accessSession) writeList(
	ctx context.Context, project, email string, list []string, on bool, w *writes,
) error {
	who := member(email)
	wrote := false
	edit := func(p *Policy) bool {
		if !on {
			wrote = revoke(p, who, list)
		} else {
			wrote = clearConditional(p, who, list) || grant(p, who, list)
		}
		return wrote
	}
	write := func() error {
		err := s.client.EditProjectPolicy(ctx, project, edit)
		if err == nil && wrote {
			w.note(on, s.client.clock.now())
		}
		return err
	}
	if !on {
		return s.refused(write(), project)
	}
	_, err := s.client.clock.awaitObserved(ctx, readGrantable, func() (bool, error) {
		err := write()
		return !accountNotYet(err, email), err
	})
	return s.refused(err, project)
}

// enable turns the seat's account back on, waits until it reads back so, and
// grants its level again. Somebody's disable at Google reaches here only when
// a person asked for it back (D3); the plan decides that.
func (s *accessSession) enable(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withAccount(ctx, in, func(sa ServiceAccount) (seataccess.StepResult, error) {
		if err := s.grantable(ctx, in.Want.Level); err != nil {
			return seataccess.StepResult{}, err
		}
		var w writes
		if sa.Disabled {
			if err := s.client.EnableServiceAccount(ctx, sa.Email); err != nil {
				return seataccess.StepResult{}, s.refused(err, "")
			}
			w.note(false, s.client.clock.now())
			if err := s.awaitDisabled(ctx, sa.UniqueID, false); err != nil {
				return seataccess.StepResult{}, err
			}
		}
		if err := s.delegate(ctx, sa, &w); err != nil {
			return seataccess.StepResult{}, err
		}
		err := s.converge(ctx, sa.Email, in.Want, known(in), &w)
		return w.result(), err
	})
}

// disable turns the seat's account off, which stops its tokens at once, and
// waits until it reads back so. Its roles go in the op's next step.
func (s *accessSession) disable(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withAccount(ctx, in, func(sa ServiceAccount) (seataccess.StepResult, error) {
		if !sa.Disabled {
			if err := s.client.DisableServiceAccount(ctx, sa.Email); err != nil {
				return seataccess.StepResult{}, s.refused(err, "")
			}
		}
		if err := s.awaitDisabled(ctx, sa.UniqueID, true); err != nil {
			return seataccess.StepResult{}, err
		}
		return seataccess.StepResult{Disabled: true}, nil
	})
}

// awaitDisabled waits until the account reads back disabled, or enabled.
func (s *accessSession) awaitDisabled(ctx context.Context, uid string, disabled bool) error {
	what := readEnabled
	if disabled {
		what = readDisabled
	}
	_, err := s.client.clock.awaitObserved(ctx, what, func() (bool, error) {
		sa, found, err := s.client.ServiceAccount(ctx, uid)
		return err != nil || (found && sa.Disabled == disabled), err
	})
	return s.refused(err, "")
}

// remove takes any role the account still holds, deletes it, and waits until
// it reads back as gone. One already gone is the state asked for.
func (s *accessSession) remove(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	done := seataccess.StepResult{Gone: true}
	uid := accountOf(in)
	if uid == "" {
		return done, nil
	}
	sa, found, err := s.client.ServiceAccount(ctx, uid)
	switch {
	case err != nil:
		return seataccess.StepResult{}, s.refused(err, "")
	case !found:
		return done, nil
	}
	if err := s.converge(ctx, sa.Email, seataccess.Desired{}, known(in), nil); err != nil {
		return seataccess.StepResult{}, err
	}
	if err := s.client.DeleteServiceAccount(ctx, sa.Email); err != nil {
		return seataccess.StepResult{}, s.refused(err, "")
	}
	if _, err := s.client.clock.awaitObserved(ctx, readDeleted, func() (bool, error) {
		_, found, err := s.client.ServiceAccount(ctx, uid)
		return err != nil || !found, err
	}); err != nil {
		return seataccess.StepResult{}, s.refused(err, "")
	}
	return done, nil
}

// accountOf is the step's account by unique ID: the ledger's, else the one an
// earlier step of the op created, else the one the pass observed.
func accountOf(in seataccess.ApplyInput) string {
	if in.Record != nil && in.Record.Account != nil && in.Record.Account.ID != "" {
		return in.Record.Account.ID
	}
	for _, p := range in.Prior {
		if p.AccountID != "" {
			return p.AccountID
		}
	}
	return in.Have.AccountID
}

// withAccount reads the step's account fresh and runs fn on it, or answers
// Gone where Google no longer has it. An account this op has just created is
// waited for, since a fresh account can read back absent.
func (s *accessSession) withAccount(
	ctx context.Context, in seataccess.ApplyInput, fn func(ServiceAccount) (seataccess.StepResult, error),
) (seataccess.StepResult, error) {
	uid := accountOf(in)
	if uid == "" {
		return seataccess.StepResult{}, fmt.Errorf("googlecloud: %s has no service account to %s", in.Seat.Handle, in.Step.Verb)
	}
	var sa ServiceAccount
	var found bool
	fresh := in.Step.Op == seataccess.OpCreate
	_, err := s.client.clock.awaitObserved(ctx, readCreated, func() (bool, error) {
		var err error
		sa, found, err = s.client.ServiceAccount(ctx, uid)
		return found || err != nil || !fresh, err
	})
	if err != nil {
		return seataccess.StepResult{}, s.refused(err, "")
	}
	if !found {
		return seataccess.StepResult{Gone: true}, nil
	}
	return fn(sa)
}

// clearConditional takes who out of every conditional binding of each role,
// which a level grants only unconditionally, and reports whether pol changed.
func clearConditional(pol *Policy, who string, roles []string) bool {
	changed := false
	out := pol.Bindings[:0]
	for _, b := range pol.Bindings {
		if b.Condition != nil && slices.Contains(roles, b.Role) && slices.Contains(b.Members, who) {
			b.Members = slices.DeleteFunc(slices.Clone(b.Members), func(m string) bool { return m == who })
			changed = true
		}
		if len(b.Members) > 0 {
			out = append(out, b)
		}
	}
	pol.Bindings = out
	return changed
}

// refused turns a Google refusal into the shared [seataccess.Refusal], with
// the remedy and who acts; Google still settling, a rate limit, a 5xx and a
// transport failure stay a fault the sync retries. project is the project a
// write was refused on, "" for the identity project's account.
func (s *accessSession) refused(err error, project string) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*seataccess.Refusal](err); ok {
		return err
	}
	if _, ok := errors.AsType[*Settling](err); ok {
		return err
	}
	if st, ok := errors.AsType[*stageError](err); ok && errors.Is(reject(err), integration.ErrCredentialRejected) {
		return &seataccess.Refusal{
			Said: "Google Cloud: Crewlet's provisioner was refused at " + string(st.stage) + ": " + st.err.Error(),
			Remedy: "Run the Cloud Shell block on the Google Cloud connection again, so the provider holds the " +
				"engine's key and the provisioner trusts the company's subject, then press Test",
			Actor: seataccess.ByYou, ActionURL: ProvidersURL,
		}
	}
	api, ok := errors.AsType[*APIError](err)
	if !ok || api.Status == http.StatusTooManyRequests && !accountQuota(err) || api.Status >= http.StatusInternalServerError {
		return err
	}
	remedy, url := s.remedyFor(err, project)
	return &seataccess.Refusal{Said: "Google Cloud: " + api.Message, Remedy: remedy, Actor: seataccess.ByYou, ActionURL: url}
}

// remedyFor is what a person does about a refusal, and where.
func (s *accessSession) remedyFor(err error, project string) (string, string) {
	identity := s.client.IdentityProject()
	switch status := Status(err); {
	case accountQuota(err):
		return "The identity project " + identity + " holds as many service accounts as its quota allows. " +
			"Delete the ones nobody uses, or ask Google to raise its Service Account Count, then retry", QuotasURL
	case domainRestricted(err):
		return "The organization's domain restricted sharing refuses service accounts of " + identity + ". " +
			"Add the identity project's organization to the allowed values of iam.allowedPolicyMemberDomains " +
			"(or iam.managed.allowedPolicyMembers) on " + project + ", then retry", projectURL(IAMURL, project)
	case status == http.StatusForbidden && project != "":
		return "Crewlet's provisioner may not grant this level on " + project + ". Run the Prepare a project " +
				"block there so its three conditions and crewletOperator are exact, then retry",
			projectURL(IAMURL, project)
	case status == http.StatusForbidden:
		return "Grant the provisioner Service Account Admin on the identity project " + identity +
			" (the Cloud Shell block does), then retry", projectURL(IAMURL, identity)
	default:
		return "Settle what Google names in the console, then retry", projectURL(IAMURL, cmp.Or(project, identity))
	}
}

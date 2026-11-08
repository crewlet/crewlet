package scaleway

import (
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

// Scaleway's half of per-seat access. The seat's account is its IAM
// application, recorded in the ledger by ID; its access is the one policy the
// engine wrote on it, named as the ledger names the application; its
// credential is its API key's secret, sealed beside the access key that names
// it. See the package doc for why nothing is adopted by name.
//
// THE POLICY IS WHAT "ON" MEANS. Scaleway has no disabled state for an
// application or a key, so an application whose engine policy is gone holds
// nothing: it is observed off, and a policy somebody deleted at Scaleway is
// read as a person turning the agent off there (D3), which no pass undoes
// unasked. Off itself deletes the keys, the policy and the application, and
// turning the agent back on makes new ones.

func init() { seataccess.Register(accessVendor{}) }

// accessVendor is Scaleway's [seataccess.Vendor].
type accessVendor struct{}

func (accessVendor) Kind() integration.Kind { return integration.KindScaleway }

// Catalog is Scaleway's per-seat offer: three fixed levels on the projects a
// seat names, which it must name, and a rotatable key. The scw CLI reads the
// key from SCW_SECRET_KEY and SCW_ACCESS_KEY, in the seat's box and in the
// MCP catalogue's entry for the CLI's own server alike.
func (accessVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{
		Kind:        integration.KindScaleway,
		Credentials: []string{SecretKeyField, AccessKeyField},
		Lanes: []seataccess.LaneSpec{{
			Lane: seataccess.LaneScaleway, Levels: true, Scoped: true, ScopeRequired: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
			Rotatable: true,
			BoxEnv: []seataccess.BoxVar{
				{Field: SecretKeyField, Env: SecretKeyEnv},
				{Field: AccessKeyField, Env: AccessKeyEnv},
			},
		}},
	}
}

// BoxDefaults is what the scw CLI in a seat's box reads beside its key: the
// organization, the seat's default project, and no update check. The
// organization is the document's value as written, a `${VAR}` reference
// included, which the launch resolves.
func (accessVendor) BoxDefaults(c *config.Company, role *config.Role) seataccess.BoxSet {
	env := map[string]string{CheckVersionEnv: "true"}
	if c != nil && c.Integrations.Scaleway != nil && c.Integrations.Scaleway.Provisioning != nil {
		if org := strings.TrimSpace(c.Integrations.Scaleway.Provisioning.OrganizationID); org != "" {
			env[OrganizationEnv] = org
		}
	}
	if d, ok := seataccess.DesiredOf(role, seataccess.LaneScaleway); ok {
		if project := defaultProject(d.Scope); project != "" {
			env[ProjectEnv] = project
		}
	}
	return seataccess.BoxSet{Env: env}
}

// defaultProject is the project a seat's key and CLI act in when a command
// names none: the first of its projects by ID, so the key's default and the
// box's agree whatever order the document lists them in.
func defaultProject(scope []string) string {
	if projects := normalProjects(scope); len(projects) > 0 {
		return projects[0]
	}
	return ""
}

// Steps is each op as Scaleway takes it, in the order Apply takes it. Pure.
func (accessVendor) Steps(
	seat seataccess.SeatRef, _ seataccess.Lane, op seataccess.Op,
	want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	out := scalewaySteps(seat, op, want, have)
	for i := range out {
		out[i].Index = i
	}
	return out
}

// rotateAfterAdminister is the caveat on a step that takes Administer away,
// which can leave access behind in what it reached.
const rotateAfterAdminister = "Administer could leave access behind in what it reached: SSH keys and " +
	"startup scripts on servers, database users, kubeconfigs and public images. Rotate those."

func scalewaySteps(
	seat seataccess.SeatRef, op seataccess.Op, want seataccess.Desired, have seataccess.Observed,
) []seataccess.Step {
	secret := seataccess.SinkName(integration.KindScaleway, SecretKeyField, seat.Handle)
	access := seataccess.SinkName(integration.KindScaleway, AccessKeyField, seat.Handle)
	step := func(verb seataccess.StepVerb, text string) seataccess.Step {
		return seataccess.Step{Op: op, Verb: verb, By: seataccess.ByCrewlet, Text: text}
	}
	grant := LevelName(want.Level) + " on " + integration.Count(len(want.Scope), "project")
	mint := step(seataccess.VerbMint, "Mint an API key expiring in 90 days, sealed as "+secret+" and "+access)
	// THE NEW KEY FIRST, sealed before anything is deleted, so the seat
	// always holds a key that works.
	retire := step(seataccess.VerbRevoke, "Delete the API keys it replaces")
	losesAdminister := Administers(have.Level) && !Administers(want.Level)
	switch op {
	case seataccess.OpCreate:
		return []seataccess.Step{
			step(seataccess.VerbCreate, "Create its Scaleway IAM application with a policy granting "+grant),
			mint,
		}
	case seataccess.OpEnable:
		// BACK ON AT THE DOCUMENT'S LEVEL: its policy is written again,
		// and a key minted only where none it holds still works.
		out := []seataccess.Step{step(seataccess.VerbEnable, "Write its policy again, granting "+grant)}
		if have.Credential == nil || have.CredentialGone {
			out = append(out, mint, retire)
		}
		return out
	case seataccess.OpLevel:
		s := step(seataccess.VerbRerole, "Change its policy to grant "+grant)
		if losesAdminister {
			s.Note = rotateAfterAdminister
		}
		return []seataccess.Step{s}
	case seataccess.OpScope:
		return []seataccess.Step{step(seataccess.VerbScope, scopeText(have.Scope, want.Scope, want.Level))}
	case seataccess.OpMint:
		return []seataccess.Step{mint, retire}
	case seataccess.OpRotate:
		mint.Text = "Mint a new API key expiring in 90 days, sealed as " + secret + " and " + access
		return []seataccess.Step{mint, retire}
	case seataccess.OpDisable:
		// THE KEYS FIRST, so the agent stops at once even where a later
		// delete fails; what is left is deleted by the next pass's revoke.
		off := step(seataccess.VerbDisable, "Delete its API keys and its policy, so it stops at once")
		if Administers(have.Level) {
			off.Note = rotateAfterAdminister
		}
		gone := step(seataccess.VerbRevoke, "Delete its application")
		gone.Note = "Scaleway keeps no history for an application, so turning the agent back on makes a new one."
		return []seataccess.Step{off, gone}
	case seataccess.OpRevoke:
		return []seataccess.Step{step(seataccess.VerbRevoke,
			"Delete its application with its keys, and clear "+secret+" and "+access)}
	case seataccess.OpRemove:
		return []seataccess.Step{step(seataccess.VerbRemove,
			"Delete its application with its keys and stop managing it")}
	default:
		// OpRetire is a person's, and Scaleway's replaced keys are deleted
		// by the mint that replaced them.
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
		parts = append(parts, words.Said("add "+integration.Count(add, "project")))
	}
	if drop > 0 {
		parts = append(parts, words.Said("remove "+integration.Count(drop, "project")))
	}
	if len(parts) == 0 {
		return "Grant " + LevelName(level) + " on its projects"
	}
	text := words.And(parts)
	return strings.ToUpper(text[:1]) + text[1:] + " in its policy, at " + LevelName(level)
}

// Open builds a session from the company's Scaleway block.
func (v accessVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return v.open(in, nil, nil)
}

// open is [accessVendor.Open] with the transport and clock a test supplies;
// nil takes the defaults.
func (accessVendor) open(in seataccess.OpenInput, hc *http.Client, now func() time.Time) (seataccess.Session, error) {
	if in.Config == nil {
		return nil, fmt.Errorf("scaleway: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Scaleway
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, fmt.Errorf("scaleway: %w: integrations.scaleway is off or has no provisioning block",
			integration.ErrNotConfigured)
	}
	if in.Env == nil {
		return nil, fmt.Errorf("scaleway: %w: nothing resolves the provisioner key", integration.ErrNotConfigured)
	}
	org := strings.TrimSpace(in.Env.Value(cfg.Provisioning.OrganizationID))
	secret := strings.TrimSpace(in.Env.Value(cfg.Provisioning.SecretKey))
	if org == "" || secret == "" {
		return nil, fmt.Errorf("scaleway: %w: the Organization ID and the provisioner secret key did not both resolve",
			integration.ErrNotConfigured)
	}
	client, err := NewClient(ClientOptions{OrganizationID: org, SecretKey: secret, HTTP: hc})
	if err != nil {
		return nil, &seataccess.Refusal{
			Said:   "integrations.scaleway.provisioning.organization_id is not a Scaleway Organization ID",
			Remedy: "Give the Organization ID (a UUID) from the Organization settings in the Scaleway console",
			Actor:  seataccess.ByYou, ActionURL: ProjectsURL,
		}
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	env := in.Env
	return &accessSession{
		client: client, company: in.Config.Name, now: now,
		sealed: func(handle, field string) string {
			return strings.TrimSpace(env.Value("${" + seataccess.SinkName(integration.KindScaleway, field, handle) + "}"))
		},
	}, nil
}

// accessSession is one pass's connection to Scaleway.
type accessSession struct {
	client  *Client
	company string
	now     func() time.Time

	// sealed is the value sealed for one seat's field, "" where none is. Only
	// the access key is read, which names the seat's key and grants nothing.
	sealed func(handle, field string) string

	// The organization's catalogue and projects, each read once per pass and
	// only where a step or an offer needs it.
	mu       sync.Mutex
	gaps     *[]seataccess.LevelGap
	projects *[]Project
}

// levelGaps reads, once per pass, the levels the catalogue cannot grant yet.
func (s *accessSession) levelGaps(ctx context.Context) ([]seataccess.LevelGap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gaps != nil {
		return *s.gaps, nil
	}
	catalogue, err := s.client.PermissionSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("scaleway: read the Organization's permission sets: %w", reject(err))
	}
	gaps := levelGaps(catalogue)
	s.gaps = &gaps
	return gaps, nil
}

// orgProjects reads, once per pass, the organization's projects.
func (s *accessSession) orgProjects(ctx context.Context) ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.projects != nil {
		return *s.projects, nil
	}
	projects, err := s.client.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("scaleway: read the Organization's projects: %w", reject(err))
	}
	s.projects = &projects
	return projects, nil
}

// Options is Scaleway's three levels, least access first, each the catalogue
// cannot grant yet offered with why, and the organization's projects, which a
// seat's projects must be among.
func (s *accessSession) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	gaps, err := s.levelGaps(ctx)
	if err != nil {
		return seataccess.Options{}, err
	}
	projects, err := s.orgProjects(ctx)
	if err != nil {
		return seataccess.Options{}, err
	}
	scope := make([]string, 0, len(projects))
	names := make(map[string]string, len(projects))
	for _, p := range projects {
		id := config.ScalewayID(p.ID)
		scope = append(scope, id)
		if name := strings.TrimSpace(p.Name); name != "" {
			names[id] = name
		}
	}
	slices.Sort(scope)
	return seataccess.Options{
		Lane: lane, Levels: seataccess.OfferGaps(accessVendor{}.FixedLevels(lane), gaps),
		Scope: slices.Compact(scope), ScopeNames: names, ScopeLive: true,
		Scoped: true, ScopeRequired: true, Rotatable: true, ActionURL: ApplicationsURL,
	}, nil
}

// FixedLevels is Scaleway's three levels, least access first
// ([seataccess.FixedLevels]).
func (accessVendor) FixedLevels(seataccess.Lane) []seataccess.Level {
	out := make([]seataccess.Level, 0, len(levels))
	for _, l := range levels {
		out = append(out, seataccess.Level{ID: l.id, Name: l.name, Note: l.note, Administers: l.administers})
	}
	return out
}

// seatApp is one seat's application as a step and a read see it: the
// application, the policies the engine wrote on it, its keys, and the access
// key sealed for the seat.
type seatApp struct {
	app      Application
	policies []Policy
	keys     []APIKey
	sealed   string
}

// engineKeys is the keys this engine minted on the application, by the
// description every one of them carries.
func (a seatApp) engineKeys(handle string) []APIKey {
	var out []APIKey
	for _, k := range a.keys {
		if k.Description == KeyDescription(handle) {
			out = append(out, k)
		}
	}
	return out
}

// policyName is the name the engine writes the seat's policy under
// ([policyNameOf]).
func (s *accessSession) policyName(handle string, rec *seataccess.Record) string {
	recorded := ""
	if rec != nil && rec.Account != nil {
		recorded = rec.Account.Name
	}
	return policyNameOf(recorded, s.company, handle)
}

// read reads one seat's application, its engine policies and its keys, and
// false where Scaleway no longer has the application.
func (s *accessSession) read(ctx context.Context, handle, appID string, rec *seataccess.Record) (seatApp, bool, error) {
	app, found, err := s.client.Application(ctx, appID)
	if err != nil || !found {
		return seatApp{}, false, err
	}
	policies, err := s.client.PoliciesOf(ctx, app.ID)
	if err != nil {
		return seatApp{}, false, err
	}
	name := s.policyName(handle, rec)
	var own []Policy
	for _, p := range policies {
		if p.Name == name {
			own = append(own, p)
		}
	}
	// THE OLDEST FIRST, which is the one kept where a lost answer made a
	// second: [accessSession.grant] deletes the rest.
	slices.SortFunc(own, func(a, b Policy) int { return createdAt(a.CreatedAt).Compare(createdAt(b.CreatedAt)) })
	keys, err := s.client.APIKeysOf(ctx, app.ID)
	if err != nil {
		return seatApp{}, false, err
	}
	return seatApp{app: app, policies: own, keys: keys, sealed: s.sealed(handle, AccessKeyField)}, true, nil
}

// createdAt is a creation time, zero where Scaleway gave none.
func createdAt(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// Observe reads back the application the ledger records for each seat, by ID.
// A seat with no recorded application holds none: nothing is adopted by name.
func (s *accessSession) Observe(ctx context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	faults := seataccess.SeatFaults{}
	now := s.now()
	for _, handle := range in.Handles() {
		rec := in.Record(handle)
		if rec == nil || !rec.Held() {
			out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneScaleway: {}}
			continue
		}
		observed, err := s.observe(ctx, handle, *rec, now)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil && errors.Is(reject(err), integration.ErrCredentialRejected):
			// THE PROVISIONER IS REFUSED, which is every seat's read alike.
			return nil, fmt.Errorf("scaleway: read %s's application: %w", handle, reject(err))
		case err != nil:
			faults[handle] = err
			continue
		}
		out[handle] = map[seataccess.Lane]seataccess.Observed{seataccess.LaneScaleway: observed}
	}
	if len(faults) > 0 {
		return out, faults
	}
	return out, nil
}

// observe is one seat's application as the shared plan reads it: absent where
// Scaleway no longer has it.
func (s *accessSession) observe(
	ctx context.Context, handle string, rec seataccess.Record, now time.Time,
) (seataccess.Observed, error) {
	a, found, err := s.read(ctx, handle, rec.Account.ID, &rec)
	if err != nil || !found {
		return seataccess.Observed{}, err
	}
	out := seataccess.Observed{
		Exists: true, Enabled: len(a.policies) > 0,
		Account: a.app.Name, AccountID: a.app.ID, CreatedAt: rec.Account.CreatedAt,
	}
	if !out.Enabled {
		out.DisabledBy = seataccess.DisabledByOf(false, rec, true)
	} else {
		rules, err := s.client.Rules(ctx, a.policies[0].ID)
		if err != nil {
			return seataccess.Observed{}, err
		}
		out.Level, out.Scope = heldBy(rules)
	}
	out.CredentialGone = credentialGone(a, handle, now)
	return out, nil
}

// credentialGone is the proof the seat holds no key that works: the access key
// sealed for it is not among its application's keys, or has expired, or is
// due for renewal. Where nothing is sealed, the newest key this engine minted
// on the application answers instead.
func credentialGone(a seatApp, handle string, now time.Time) bool {
	if a.sealed != "" {
		i := slices.IndexFunc(a.keys, func(k APIKey) bool { return k.AccessKey == a.sealed })
		return i < 0 || renewDue(a.keys[i].Expires(), now)
	}
	keys := a.engineKeys(handle)
	if len(keys) == 0 {
		return true
	}
	newest := slices.MaxFunc(keys, func(x, y APIKey) int { return createdAt(x.CreatedAt).Compare(createdAt(y.CreatedAt)) })
	return renewDue(newest.Expires(), now)
}

// Apply takes one step at Scaleway, chosen by its op and verb.
func (s *accessSession) Apply(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	switch in.Step.Op {
	case seataccess.OpCreate:
		if in.Step.Verb == seataccess.VerbCreate {
			return s.create(ctx, in)
		}
		return s.mint(ctx, in)
	case seataccess.OpEnable, seataccess.OpMint, seataccess.OpRotate:
		switch in.Step.Verb {
		case seataccess.VerbEnable:
			return s.grant(ctx, in)
		case seataccess.VerbMint:
			return s.mint(ctx, in)
		default:
			return s.retire(ctx, in)
		}
	case seataccess.OpLevel, seataccess.OpScope:
		return s.grant(ctx, in)
	case seataccess.OpDisable:
		if in.Step.Verb == seataccess.VerbDisable {
			return s.disable(ctx, in)
		}
		return s.delete(ctx, in)
	case seataccess.OpRevoke, seataccess.OpRemove:
		return s.delete(ctx, in)
	default:
		return seataccess.StepResult{}, fmt.Errorf("scaleway: no step for %q", in.Step.Op)
	}
}

// rulesFor is the policy's rules at the seat's level on its projects, or the
// refusal saying why they cannot be written.
func (s *accessSession) rulesFor(ctx context.Context, in seataccess.ApplyInput) ([]RuleSpec, error) {
	rules, err := rulesFor(in.Want.Level, s.client.OrganizationID(), in.Want.Scope)
	if err != nil {
		return nil, &seataccess.Refusal{
			Said:   "Scaleway: " + err.Error(),
			Remedy: "Choose a level and at least one project for this agent on the Agents tab",
			Actor:  seataccess.ByYou,
		}
	}
	gaps, err := s.levelGaps(ctx)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(gaps, func(g seataccess.LevelGap) bool { return g.Level == in.Want.Level }); i >= 0 {
		return nil, &seataccess.Refusal{
			Said:      "Scaleway: " + gaps[i].Reason,
			Remedy:    gaps[i].Remedy + ", or choose another level for this agent on the Agents tab",
			Actor:     seataccess.ByYou,
			ActionURL: gaps[i].ActionURL,
		}
	}
	return rules, nil
}

// create makes the seat's application with its policy. A policy Scaleway
// refuses takes the application back with it, so no application is left
// holding nothing for the ledger never to record.
func (s *accessSession) create(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	rules, err := s.rulesFor(ctx, in)
	if err != nil {
		return seataccess.StepResult{}, s.refused(err)
	}
	name := ApplicationName(s.company, in.Seat.Handle)
	app, err := s.client.CreateApplication(ctx, name, description(in.Seat.Handle))
	if err != nil {
		return seataccess.StepResult{}, s.refused(err)
	}
	if _, err := s.client.CreatePolicy(ctx, PolicySpec{
		Name: name, Description: description(in.Seat.Handle), ApplicationID: app.ID, Rules: rules,
	}); err != nil {
		undo := s.client.DeleteApplication(context.WithoutCancel(ctx), app.ID)
		return seataccess.StepResult{}, errors.Join(s.refused(err), undo)
	}
	return seataccess.StepResult{AccountID: app.ID, Account: name}, nil
}

// grant writes the seat's policy at its level on its projects: in place where
// the engine's policy exists, as a new one where it does not. A second policy
// under the engine's name, which only a lost answer to a create leaves, is
// deleted; any other policy on the application is somebody else's grant,
// which the company pass reports and nothing here touches.
//
// THE KEYS' DEFAULT PROJECT FOLLOWS: a key whose default project the seat no
// longer names is moved to the seat's default, since Object Storage acts in a
// key's default project whatever the policy grants.
func (s *accessSession) grant(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withApp(ctx, in, func(a seatApp) (seataccess.StepResult, error) {
		rules, err := s.rulesFor(ctx, in)
		if err != nil {
			return seataccess.StepResult{}, s.refused(err)
		}
		var result seataccess.StepResult
		if len(a.policies) == 0 {
			if _, err := s.client.CreatePolicy(ctx, PolicySpec{
				Name: s.policyName(in.Seat.Handle, in.Record), Description: description(in.Seat.Handle),
				ApplicationID: a.app.ID, Rules: rules,
			}); err != nil {
				return seataccess.StepResult{}, s.refused(err)
			}
			if in.Step.Op != seataccess.OpEnable {
				result.Did = "Wrote its policy again, granting " + LevelName(in.Want.Level) +
					", since it was gone at Scaleway"
			}
		} else {
			if err := s.client.SetRules(ctx, a.policies[0].ID, rules); err != nil {
				return seataccess.StepResult{}, s.refused(err)
			}
			for _, extra := range a.policies[1:] {
				if err := s.client.DeletePolicy(ctx, extra.ID); err != nil {
					return seataccess.StepResult{}, s.refused(err)
				}
			}
		}
		project := defaultProject(in.Want.Scope)
		scope := normalProjects(in.Want.Scope)
		for _, k := range a.engineKeys(in.Seat.Handle) {
			if slices.Contains(scope, config.ScalewayID(k.DefaultProjectID)) {
				continue
			}
			if err := s.client.SetDefaultProject(ctx, k.AccessKey, project); err != nil {
				return seataccess.StepResult{}, s.refused(err)
			}
		}
		return result, nil
	})
}

// mint creates one API key, expiring KeyLifetime from now in the seat's
// default project, and hands its secret and its access key to the sync to
// seal.
//
// A KEY THE SYNC CANNOT SEAL IS DELETED through the secret's Revoke, which the
// sync calls with a context that outlives its own cancellation. It deletes
// nothing else: the keys a new one replaces go in the op's next step, once the
// new one is sealed.
func (s *accessSession) mint(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withApp(ctx, in, func(a seatApp) (seataccess.StepResult, error) {
		key, err := s.client.CreateAPIKey(ctx, KeySpec{
			ApplicationID: a.app.ID, Description: KeyDescription(in.Seat.Handle),
			DefaultProjectID: defaultProject(in.Want.Scope), ExpiresAt: expiry(s.now()),
		})
		if err != nil {
			return seataccess.StepResult{}, s.refused(err)
		}
		if !accessKeyPattern().MatchString(key.AccessKey) || !uuidPattern().MatchString(key.SecretKey) {
			// THE KEY EXISTS AND ITS VALUE NEVER ARRIVED WHOLE, so it is
			// taken back now rather than left live and held by nobody.
			var undo error
			if key.AccessKey != "" {
				undo = s.client.DeleteAPIKey(context.WithoutCancel(ctx), key.AccessKey)
			}
			return seataccess.StepResult{}, errors.Join(errors.New(
				"scaleway: created an API key and Scaleway returned no usable secret for it"), undo)
		}
		accessKey := key.AccessKey
		return seataccess.StepResult{Minted: []seataccess.Minted{
			{
				Field: SecretKeyField, Value: key.SecretKey, CredentialID: accessKey,
				Revoke: func(ctx context.Context) error { return s.client.DeleteAPIKey(ctx, accessKey) },
			},
			// THE ACCESS KEY NAMES THE KEY AND GRANTS NOTHING ALONE, so
			// the secret's Revoke is what undoes the mint.
			{Field: AccessKeyField, Value: accessKey, CredentialID: accessKey},
		}}, nil
	})
}

// retire deletes the keys this engine minted on the application other than
// the one this op's earlier step minted ([seataccess.ApplyInput.PriorCredentialID]).
// With nothing minted before it, it deletes nothing: a key it cannot tell
// from the sealed one is never deleted on a guess. A key somebody else added
// is the company pass's report, never deleted here.
func (s *accessSession) retire(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	keep := in.PriorCredentialID(SecretKeyField)
	if keep == "" {
		return seataccess.StepResult{}, fmt.Errorf(
			"scaleway: %s's retire step follows no mint, so it cannot tell which key to keep", in.Seat.Handle)
	}
	return s.withApp(ctx, in, func(a seatApp) (seataccess.StepResult, error) {
		for _, k := range a.engineKeys(in.Seat.Handle) {
			if k.AccessKey == keep {
				continue
			}
			if err := s.client.DeleteAPIKey(ctx, k.AccessKey); err != nil {
				return seataccess.StepResult{}, s.refused(err)
			}
		}
		return seataccess.StepResult{}, nil
	})
}

// disable deletes every key on the application, which stops the agent at
// once, then the engine's policy. The application goes in the op's next step.
func (s *accessSession) disable(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	return s.withApp(ctx, in, func(a seatApp) (seataccess.StepResult, error) {
		if err := s.deleteKeys(ctx, a); err != nil {
			return seataccess.StepResult{}, s.refused(err)
		}
		for _, p := range a.policies {
			if err := s.client.DeletePolicy(ctx, p.ID); err != nil {
				return seataccess.StepResult{}, s.refused(err)
			}
		}
		// THE SEALED VALUES STAY RECORDED until the application is gone,
		// so a delete that fails here is the next pass's revoke.
		return seataccess.StepResult{Disabled: true}, nil
	})
}

// delete deletes every key on the application, the engine's policy and the
// application, which is the end of off, a revoke and a remove alike. One
// already gone is the state asked for.
//
// EVERY KEY, the ones somebody else added included: the application is the
// engine's and is going, and a key outliving it is impossible, so none is
// spared by a delete that could fail half way. A policy somebody else wrote on
// it is left to Scaleway, which grants nothing through an application that is
// gone.
func (s *accessSession) delete(ctx context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	done := seataccess.StepResult{Gone: true, Revoked: []string{SecretKeyField, AccessKeyField}}
	id := appID(in)
	if id == "" {
		return done, nil
	}
	a, found, err := s.read(ctx, in.Seat.Handle, id, in.Record)
	if err != nil {
		return seataccess.StepResult{}, s.refused(err)
	}
	if !found {
		return done, nil
	}
	if err := s.deleteKeys(ctx, a); err != nil {
		return seataccess.StepResult{}, s.refused(err)
	}
	for _, p := range a.policies {
		if err := s.client.DeletePolicy(ctx, p.ID); err != nil {
			return seataccess.StepResult{}, s.refused(err)
		}
	}
	if err := s.client.DeleteApplication(ctx, a.app.ID); err != nil {
		return seataccess.StepResult{}, s.refused(err)
	}
	return done, nil
}

// deleteKeys deletes every key on the application.
func (s *accessSession) deleteKeys(ctx context.Context, a seatApp) error {
	for _, k := range a.keys {
		if err := s.client.DeleteAPIKey(ctx, k.AccessKey); err != nil {
			return err
		}
	}
	return nil
}

// appID is the step's application: the ledger's, else the one the pass
// observed.
func appID(in seataccess.ApplyInput) string {
	if in.Record != nil && in.Record.Account != nil && in.Record.Account.ID != "" {
		return in.Record.Account.ID
	}
	return in.Have.AccountID
}

// withApp reads the step's application fresh and runs fn on it, or answers
// Gone where Scaleway no longer has it.
//
// FRESH, not the observation the pass began with: a step acts on what the
// application holds now. The application is the ledger's, which the sync
// updates as soon as a create returns, so a create's mint reaches the
// application the create made.
func (s *accessSession) withApp(
	ctx context.Context, in seataccess.ApplyInput, fn func(seatApp) (seataccess.StepResult, error),
) (seataccess.StepResult, error) {
	id := appID(in)
	if id == "" {
		return seataccess.StepResult{}, fmt.Errorf("scaleway: %s has no application to %s", in.Seat.Handle, in.Step.Verb)
	}
	a, found, err := s.read(ctx, in.Seat.Handle, id, in.Record)
	if err != nil {
		return seataccess.StepResult{}, s.refused(err)
	}
	if !found {
		return seataccess.StepResult{Gone: true}, nil
	}
	return fn(a)
}

// refused turns a Scaleway refusal into the shared [seataccess.Refusal], with
// the remedy and who acts; a rate limit, a 5xx or a transport failure stays a
// fault the sync retries.
func (s *accessSession) refused(err error) error {
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
		said = fmt.Sprintf("Scaleway refused the request with status %d", api.Status)
	}
	remedy, url := remedyFor(err)
	return &seataccess.Refusal{Said: "Scaleway: " + said, Remedy: remedy, Actor: seataccess.ByYou, ActionURL: url}
}

// remedyFor is what a person does about a refusal, and where.
func remedyFor(err error) (string, string) {
	switch status := Status(err); {
	case QuotaExceeded(err):
		return "The Organization holds as many IAM applications, policies or API keys as its quota " +
			"allows. Delete the ones nobody uses, or ask Scaleway support to raise the quota, then retry", QuotasURL
	case ExpiryRefused(err):
		return "The Organization's security settings cap an API key's lifetime below the 90 days an " +
			"agent's key is minted with. Raise the cap to at least 90 days, then retry", SecurityURL
	case status == http.StatusUnauthorized:
		return "Replace the provisioner secret key on the Scaleway connection", APIKeysURL
	case status == http.StatusForbidden:
		return "Give the provisioner application IAMApplicationManager, IAMPolicyManager and " +
			"ProjectReadOnly on the Organization, with no condition, then retry", PoliciesURL
	default:
		return "Settle what Scaleway names in the Scaleway console, then retry", ApplicationsURL
	}
}

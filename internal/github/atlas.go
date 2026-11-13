package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// GitHub's half of the atlas (ADR-0131): the organization's repositories,
// each one's workflows and the environments it deploys to, read whole through
// the installations of the agents' own apps, which are the only access to
// GitHub the engine holds. Each installation is read under a token minted for
// the read, carrying Read-only of what the installation holds and nothing it
// does not, and revoked when the read ends.
//
// EACH PART TAKES A PERMISSION OF ITS OWN: repositories take Metadata, which
// every app holds; workflows take Actions; deployments take Deployments. A
// part no installation covering a repository may read is a finding on the
// source's standing, and what an earlier read recorded of it stands.

func init() { collect.Register(atlasSource{}) }

// atlasSource is GitHub's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindGitHub }

// The parts of GitHub a read can be refused.
const (
	partInstallations = "installations"
	partWorkflows     = "workflows"
	partDeployments   = "deployments"
)

// The two things a repository's pipelines are, each a namespace of the
// pipeline's key after the repository's: a workflow by its file's name, a
// deployment environment by its own.
const (
	workflowKeyPrefix    = "workflow:"
	environmentKeyPrefix = "environment:"
)

// workflowDir is where a repository's own workflows live. A workflow outside
// it is one GitHub runs for a feature of its own (Pages, Dependabot, code
// scanning), which the repository does not declare and the atlas does not read.
const workflowDir = ".github/workflows/"

// maxEnvironments bounds the deployment environments read for one repository,
// the most recently deployed first. Each costs one request for its newest
// status, so the bound is what keeps a repository with an environment per
// customer from spending the installation's hourly allowance.
const maxEnvironments = 20

// errTooManyEnvironments is an environment past [maxEnvironments].
var errTooManyEnvironments = fmt.Errorf("past the %d most recently deployed environments of its repository",
	maxEnvironments)

// atlasAccess is what a read of the organization is made with: the seats to
// read through, the addresses, and a client that counts its requests.
type atlasAccess struct {
	org     string
	host    string
	apiBase string
	webBase string
	seats   []SeatApp
	http    *http.Client
	calls   func() int
}

// openAtlas resolves the access a read is made with, and answers
// [integration.ErrNotConfigured] where the company gave none.
func openAtlas(in collect.Input) (atlasAccess, error) {
	if in.Config == nil {
		return atlasAccess{}, fmt.Errorf("github: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.GitHub
	org := strings.ToLower(providedOrg(cfg))
	if cfg == nil || !cfg.Enabled || org == "" {
		return atlasAccess{}, fmt.Errorf("github: %w: integrations.github is off or names no organization",
			integration.ErrNotConfigured)
	}
	var lookup func(string) (string, bool)
	if in.Env != nil {
		lookup = in.Env.LookupOK
	}
	a := atlasAccess{org: org}
	a.apiBase, a.webBase = cfg.Bases(lookup)
	if a.seats = atlasSeats(in.Config, in.Env); len(a.seats) == 0 {
		return atlasAccess{}, fmt.Errorf("github: %w: no agent has a GitHub App switched on to read through",
			integration.ErrNotConfigured)
	}
	web, err := url.Parse(a.webBase)
	if err != nil || web.Hostname() == "" {
		return atlasAccess{}, errors.New("github: integrations.github.url is not an address with a host")
	}
	a.host = strings.ToLower(web.Hostname())
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	a.http, a.calls = collect.Counted(base)
	return a, nil
}

// Collect reads the organization through every agent's installation the
// engine can read as, and reports it as claims in the integration's voice.
// It writes nothing at GitHub but the tokens it mints and revokes.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	access, err := openAtlas(in)
	if err != nil {
		return collect.Read{}, err
	}
	r := &atlasRead{
		b: collect.NewBuilder(integration.KindGitHub, in.Now), org: access.org, apiBase: access.apiBase,
		host: access.host, http: access.http, calls: access.calls,
		listed: map[string]bool{}, unread: map[string]map[string]bool{partWorkflows: {}, partDeployments: {}},
	}
	err = r.read(ctx, access.seats)
	// EVERY TOKEN THE READ MINTED IS WITHDRAWN, however it ended: a token
	// nobody holds a reason for would otherwise live out its hour.
	r.close(context.WithoutCancel(ctx))
	if err != nil {
		return collect.Read{}, err
	}
	r.b.Calls(r.calls())
	return r.b.Read(), nil
}

// atlasSeats is every agent seat whose app is switched on, with its key
// resolved, by handle. A seat switched off is not read through: its
// installation is suspended on purpose, and it is access the company has
// withdrawn rather than access the engine holds.
func atlasSeats(c *config.Company, env *config.Resolver) []SeatApp {
	var out []SeatApp
	for role := range c.EachRole() {
		seat := role.Seat()
		if seat == nil || !seat.IsAgent() || seat.GitHub == nil || !seat.GitHub.Enabled || seat.GitHub.AppID == 0 {
			continue
		}
		app := seat.GitHub
		keyRef := strings.TrimSpace(app.PrivateKey)
		if keyRef == "" {
			keyRef = "${" + SeatVar(seat.Handle(), FieldAppKey) + "}"
		}
		s := SeatApp{Handle: seat.Handle(), Name: seat.Name, AppID: app.AppID, InstallationID: app.InstallationID}
		if env != nil {
			s.Key = strings.TrimSpace(env.Value(keyRef))
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, z SeatApp) int { return cmp.Compare(a.Handle, z.Handle) })
	return out
}

// atlasRead is one read in flight.
type atlasRead struct {
	b       *collect.Builder
	org     string
	host    string
	apiBase string
	http    *http.Client
	calls   func() int

	// installs is every installation the read is made as, in seat order.
	installs []*atlasInstall
	// listed is every repository an installation listed, by its entry's key.
	listed map[string]bool
	// unread is the repositories each part could not be read for, by key.
	unread map[string]map[string]bool
}

// atlasInstall is one installation the read is made as.
type atlasInstall struct {
	reader      installationReader
	actions     bool
	deployments bool
	repos       map[string]InventoryRepo
}

// refused reports GitHub refusing this credential the request, as opposed to
// failing to answer it: a dead credential, one that may not, or the 404 it
// answers for what a credential cannot see. Never its rate limit, which a
// wait clears and which fails the read whole.
func refused(err error, more ...int) bool {
	var api *APIError
	if !errors.As(err, &api) || api.RateLimited {
		return false
	}
	return api.Status == http.StatusUnauthorized || api.Forbidden() || api.NotFound() ||
		slices.Contains(more, api.Status)
}

// reads reports whether an installation holds key at Read-only or above.
func reads(held map[string]string, key string) bool {
	return held[key] == string(LevelRead) || held[key] == string(LevelWrite)
}

// signs reports whether a seat's key signs an assertion for its app.
func signs(seat SeatApp) bool {
	_, err := AppJWT(seat.AppID, seat.Key, time.Now())
	return err == nil
}

// openInstallation reads as one seat's installation: it reads the
// installation back and mints the read's token, carrying what wants picks of
// what the installation holds. Nil is an installation the read cannot be made
// as, which is never a fault: it is not recorded, is gone, is suspended, sits
// on another account, its key does not sign for it, or wants declines it.
func openInstallation(ctx context.Context, hc *http.Client, apiBase, org string, seat SeatApp,
	wants func(held map[string]string) (map[string]string, bool)) (*atlasInstall, error) {

	if seat.Key == "" || seat.InstallationID == 0 {
		return nil, nil
	}
	// A KEY THAT DOES NOT PARSE signs nothing, which is a seat to finish
	// setting up, never a fault of the read.
	if !signs(seat) {
		return nil, nil
	}
	app, err := NewAppClient(seat.AppID, seat.Key, apiBase, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build an agent's app client: %w", err)
	}
	app.http = hc
	inst, err := app.Installation(ctx, seat.InstallationID)
	switch {
	case refused(err):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("github: read an agent's installation: %w", err)
	}
	if inst.Suspended() || !strings.EqualFold(strings.TrimSpace(inst.Account.Login), org) {
		return nil, nil
	}
	// READ-ONLY OF WHAT THE INSTALLATION HOLDS, whatever the seat's own
	// tokens carry: the read never writes, and asking for a permission the
	// installation lacks mints nothing at all.
	perms, ok := wants(inst.Permissions)
	if !ok {
		return nil, nil
	}
	token, err := app.mint(ctx, seat.InstallationID, perms, nil)
	switch {
	// 422 is GitHub refusing a permission the installation has just lost.
	case refused(err, http.StatusUnprocessableEntity):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("github: mint a token for the read: %w", err)
	}
	return &atlasInstall{reader: installationReader{app: app, token: token.Token},
		actions: perms[PermissionActions] != "", deployments: perms[PermissionDeployments] != "",
		repos: map[string]InventoryRepo{}}, nil
}

// list fills the installation's repositories, by lowered full name.
func (in *atlasInstall) list(ctx context.Context) error {
	repos, err := in.reader.app.listRepositories(ctx, in.reader.token)
	if err != nil {
		// A LISTING CUT SHORT IS NO READ: Metadata is every token's, so no
		// refusal of this listing is a permission's.
		return fmt.Errorf("github: list an installation's repositories: %w", err)
	}
	for _, repo := range repos {
		in.repos[strings.ToLower(repo.FullName)] = repo
	}
	return nil
}

// revokeAll withdraws the token of every installation a read was made as.
func revokeAll(ctx context.Context, installs []*atlasInstall) {
	ctx, cancel := context.WithTimeout(ctx, ClientTimeout)
	defer cancel()
	for _, in := range installs {
		if err := in.reader.app.revoke(ctx, in.reader.token); err != nil {
			log.WarnContext(ctx, "github_atlas_token_not_revoked", "error", err,
				"detail", "a read-only token minted for the atlas read lives until it expires, within the hour")
		}
	}
}

// open reads as one seat's installation, under a token carrying Read-only of
// what it holds among Metadata, Actions and Deployments, and lists what it
// covers. False is an installation the read cannot be made as.
func (r *atlasRead) open(ctx context.Context, seat SeatApp) (bool, error) {
	in, err := openInstallation(ctx, r.http, r.apiBase, r.org, seat, func(held map[string]string) (map[string]string, bool) {
		perms := map[string]string{PermissionMetadata: string(LevelRead)}
		for _, key := range []string{PermissionActions, PermissionDeployments} {
			if reads(held, key) {
				perms[key] = string(LevelRead)
			}
		}
		return perms, true
	})
	if err != nil || in == nil {
		return false, err
	}
	// KEPT BEFORE IT IS LISTED, so its token is withdrawn however the
	// listing ends.
	r.installs = append(r.installs, in)
	if err := in.list(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// close withdraws every token the read minted.
func (r *atlasRead) close(ctx context.Context) { revokeAll(ctx, r.installs) }

// read is the read itself: the organization, then every repository an
// installation lists, each with the parts an installation may read.
func (r *atlasRead) read(ctx context.Context, seats []SeatApp) error {
	b := r.b
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	account, ok := b.Ref("org", atlas.KindAccount, string(atlas.VendorGitHub)+"/"+r.host+"/"+r.org)
	if !ok {
		return fmt.Errorf("github: the organization %q is not an entry the atlas can name", r.org)
	}
	closed := 0
	for _, seat := range seats {
		opened, err := r.open(ctx, seat)
		if err != nil {
			return err
		}
		if !opened {
			closed++
		}
	}
	names := map[string]bool{}
	for _, in := range r.installs {
		for name := range in.repos {
			names[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if err := r.repository(ctx, account, name); err != nil {
			return err
		}
	}
	if closed > 0 {
		listed := r.listed
		b.Unread(collect.Unread{Part: partInstallations,
			Detail: strconv.Itoa(closed) + " of " + strconv.Itoa(len(seats)) + " agents' GitHub Apps could not be " +
				"read through: each is not installed on the organization, is suspended there, or its key " +
				"does not resolve. What only they cover stands as an earlier read left it. Finish each " +
				"agent's GitHub setup on the integration's Agents tab.",
			// EVERYTHING OF A REPOSITORY THIS READ DID NOT LIST: which
			// repositories the closed installations cover is not known.
			Holds: func(c atlas.Claim) bool {
				repo, _ := partOf(c)
				return repo != "" && !listed[repo]
			}})
	}
	for _, part := range []struct{ name, permission, noun string }{
		{partWorkflows, "Actions", "workflows"},
		{partDeployments, "Deployments", "deployment environments"},
	} {
		repos := r.unread[part.name]
		if len(repos) == 0 {
			continue
		}
		b.Unread(collect.Unread{Part: part.name,
			Detail: "The " + part.noun + " of " + strconv.Itoa(len(repos)) + " repositories were not read: no " +
				"agent's GitHub App that covers them holds " + part.permission + " (Read-only), or GitHub " +
				"refused the listing. Give an agent that works in them " + part.permission + " (Read-only).",
			Holds: func(c atlas.Claim) bool {
				repo, of := partOf(c)
				return of == part.name && repos[repo]
			}})
	}
	return nil
}

// partOf is the repository a claim of this voice is about, by its entry's
// key, and the part of a read that made the claim ("" for the repository's
// own). Read off the claim alone, so a read refused a part carries exactly
// what an earlier read of it recorded. The organization's claims are no
// repository's.
func partOf(c atlas.Claim) (repo, part string) {
	pipeline := c.Subject
	switch {
	case c.Subject.Kind == atlas.KindRepository:
		return c.Subject.Key, ""
	case c.Facet.Kind == atlas.FacetRelation && c.Facet.Relation == atlas.RelationDeployedBy:
		pipeline = c.Facet.Target.Entry
	}
	if pipeline.Kind != atlas.KindPipeline {
		return "", ""
	}
	repo, name, _ := strings.Cut(pipeline.Key, "#")
	switch {
	case strings.HasPrefix(name, workflowKeyPrefix):
		return repo, partWorkflows
	case strings.HasPrefix(name, environmentKeyPrefix):
		return repo, partDeployments
	}
	return "", ""
}

// repository claims one repository and reads its parts. A repository several
// installations cover is read as the first, in seat order, that may read the
// part.
func (r *atlasRead) repository(ctx context.Context, account atlas.Ref, name string) error {
	var row InventoryRepo
	var workflows, deployments *atlasInstall
	for _, in := range slices.Backward(r.installs) {
		repo, ok := in.repos[name]
		if !ok {
			continue
		}
		row = repo
		if in.actions {
			workflows = in
		}
		if in.deployments {
			deployments = in
		}
	}
	b := r.b
	where := "repositories[" + row.FullName + "]"
	repo, ok := b.Ref(where+".full_name", atlas.KindRepository, r.host+"/"+row.FullName)
	if !ok {
		return nil
	}
	r.listed[repo.Key] = true
	b.Relates(where, repo, atlas.RelationBelongsTo, account)
	b.Says(where+".default_branch", repo, atlas.FactDefaultBranch, row.DefaultBranch)
	status := "active"
	if row.Archived {
		status = "archived"
	}
	b.Says(where+".archived", repo, atlas.FactStatus, status)
	// GITHUB'S OWN WORD, of this host alone: a mirror on Gitlet has Gitlet's.
	visibility := row.Visibility
	if visibility == "" && row.Private {
		visibility = "private"
	}
	b.Says(where+".visibility", repo, atlas.FactVisibility, visibility)
	// AN ARCHIVED REPOSITORY RUNS AND DEPLOYS NOTHING, so its pipelines are
	// not read, and leave the atlas as any pipeline that stopped being listed.
	if row.Archived {
		return nil
	}
	for _, part := range []struct {
		name string
		as   *atlasInstall
		read func(installationReader) error
	}{
		{partWorkflows, workflows, func(as installationReader) error {
			return r.workflows(ctx, as, account, repo, row, where)
		}},
		{partDeployments, deployments, func(as installationReader) error {
			return r.deployments(ctx, as, account, repo, row, where)
		}},
	} {
		if part.as == nil {
			r.unread[part.name][repo.Key] = true
			continue
		}
		err := part.read(part.as.reader)
		switch {
		case err == nil:
		case refused(err):
			r.unread[part.name][repo.Key] = true
		default:
			// ANYTHING ELSE IS A READ THAT DID NOT FINISH, GitHub's rate
			// limit included, and fails whole: a listing cut short would
			// retire what it did not reach.
			return fmt.Errorf("github: read the %s of a repository: %w", part.name, err)
		}
	}
	return nil
}

// workflows claims each workflow a repository declares: its name, whether
// GitHub still runs it, the result of its newest completed run on the default
// branch, and the repository it is built from. Both listings are read before
// anything is claimed, so a refusal of the second leaves no half of the part.
func (r *atlasRead) workflows(ctx context.Context, as installationReader, account, repo atlas.Ref,
	row InventoryRepo, where string) error {

	flows, err := as.workflows(ctx, row.FullName)
	if err != nil {
		return err
	}
	flows = slices.DeleteFunc(flows, func(w Workflow) bool {
		file, declared := strings.CutPrefix(w.Path, workflowDir)
		return !declared || file == "" || strings.Contains(file, "/") || w.State == "deleted"
	})
	var concluded map[int64]string
	if len(flows) > 0 && row.DefaultBranch != "" {
		if concluded, err = as.lastConclusions(ctx, row.FullName, row.DefaultBranch); err != nil {
			return err
		}
	}
	slices.SortFunc(flows, func(a, z Workflow) int { return cmp.Compare(a.Path, z.Path) })
	b := r.b
	for _, w := range flows {
		at := where + ".workflows[" + strconv.FormatInt(w.ID, 10) + "]"
		file := strings.TrimPrefix(w.Path, workflowDir)
		p, ok := b.Pipeline(at+".path", repo, workflowKeyPrefix, file)
		if !ok {
			continue
		}
		b.Relates(at, p, atlas.RelationBuiltFrom, repo)
		b.Relates(at, p, atlas.RelationBelongsTo, account)
		b.Says(at+".name", p, atlas.FactName, collect.PlainLabel(w.Name))
		b.Says(at+".state", p, atlas.FactPipelineState, workflowState(w.State))
		b.Says(at+".conclusion", p, atlas.FactLastResult, runResult(concluded[w.ID]))
	}
	return nil
}

// deployments claims each lasting environment a repository deploys to: its
// name, the state of its newest deployment, the environment the name spells
// where it spells one, and the address and the name that deployment put the
// code behind. Every status is read before anything is claimed.
func (r *atlasRead) deployments(ctx context.Context, as installationReader, account, repo atlas.Ref,
	row InventoryRepo, where string) error {

	latest, err := as.latestDeployments(ctx, row.FullName)
	if err != nil {
		return err
	}
	b := r.b
	var past []Deployment
	if len(latest) > maxEnvironments {
		latest, past = latest[:maxEnvironments], latest[maxEnvironments:]
	}
	statuses := make([]DeploymentStatus, len(latest))
	for i, d := range latest {
		if statuses[i], _, err = as.deploymentStatus(ctx, row.FullName, d.ID); err != nil {
			return err
		}
	}
	for _, d := range past {
		b.Withhold(where+".deployments["+strconv.FormatInt(d.ID, 10)+"].environment", errTooManyEnvironments)
	}
	for i, d := range latest {
		at := where + ".deployments[" + strconv.FormatInt(d.ID, 10) + "]"
		p, ok := b.Pipeline(at+".environment", repo, environmentKeyPrefix, d.Environment)
		if !ok {
			continue
		}
		b.Relates(at, p, atlas.RelationBuiltFrom, repo)
		b.Relates(at, p, atlas.RelationBelongsTo, account)
		b.Says(at+".environment", p, atlas.FactName, collect.PlainLabel(d.Environment))
		// A SUGGESTION, as every fact of meaning from an integration is.
		b.Says(at+".environment", p, atlas.FactEnvironment,
			collect.EnvironmentWord(strings.ToLower(strings.TrimSpace(d.Environment))))
		b.Says(at+".statuses.state", p, atlas.FactLastResult, deploymentResult(statuses[i].State))
		b.Deployed(at+".statuses.environment_url", p, statuses[i].EnvironmentURL)
	}
	return nil
}

// workflowState folds GitHub's workflow states into the atlas's words: active
// as it is, and every way a workflow is switched off (by hand, for inactivity,
// in a fork) as disabled. A state this build has not met says nothing.
func workflowState(vendor string) string {
	switch {
	case vendor == "active":
		return atlas.PipelineActive
	case strings.HasPrefix(vendor, "disabled_"):
		return atlas.PipelineDisabled
	}
	return ""
}

// runResult folds a completed run's conclusion into the atlas's words. A
// conclusion that says nothing about the code (stale, waiting on a person,
// one this build has not met) says nothing here.
func runResult(conclusion string) string {
	switch conclusion {
	case "success", "neutral":
		return atlas.ResultPassed
	case "failure":
		return atlas.ResultFailed
	case "timed_out", "startup_failure":
		return atlas.ResultErrored
	case "cancelled":
		return atlas.ResultCancelled
	case "skipped":
		return atlas.ResultSkipped
	}
	return ""
}

// deploymentResult folds a deployment's newest status into the atlas's words.
// Inactive is a deployment a newer one replaced or somebody took down, which
// says nothing about a result.
func deploymentResult(state string) string {
	switch state {
	case "success":
		return atlas.ResultPassed
	case "failure":
		return atlas.ResultFailed
	case "error":
		return atlas.ResultErrored
	case "in_progress":
		return atlas.ResultRunning
	case "queued", "pending", "waiting":
		return atlas.ResultQueued
	}
	return ""
}

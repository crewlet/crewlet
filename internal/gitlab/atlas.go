package gitlab

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// GitLab's half of the atlas (ADR-0135): the group's projects, each one's
// pipeline and the environments it deploys to, read whole under the read
// credential the company gave the engine, or under the provisioning token
// where it gave none. Every call is a GET.
//
// EACH PART IS REFUSED ON ITS OWN: a project's pipelines and environments
// take the Reporter role where the project is private, which a credential
// that lists the project may lack. A part GitLab refuses for a project is a
// finding on the source's standing, and what an earlier read recorded of it
// stands.

func init() { collect.Register(atlasSource{}) }

// atlasSource is GitLab's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindGitLab }

// The parts of GitLab a read can be refused.
const (
	partGroup        = "group"
	partProjects     = "projects"
	partPipelines    = "pipelines"
	partEnvironments = "environments"
)

// The two things a project's pipelines are, each named after the project's
// key: the pipeline its CI configuration runs, of which a project has one,
// and a deployment environment by its own name.
const (
	pipelineKeyName      = "pipeline"
	environmentKeyPrefix = "environment:"
)

// maxEnvironments bounds the environments read for one project, the nearest
// production first. Each costs one request for its newest deployment, so the
// bound is what keeps a project with an environment per customer from
// spending the read's fifteen minutes. The same bound as GitHub's.
const maxEnvironments = 20

// errTooManyEnvironments is an environment past [maxEnvironments].
var errTooManyEnvironments = fmt.Errorf("past the %d environments of its project nearest production",
	maxEnvironments)

// The settings a read's credential comes from, as a finding names them.
const (
	settingToken      = "integrations.gitlab.token"
	settingAdminToken = "integrations.gitlab.provisioning.admin_token"
)

// atlasCredential is the credential a read is made under and the setting that
// holds it: the read credential where one resolves, and otherwise the
// provisioning token, which the reconcile loop already reads the group with.
// The narrower one first, so a token that may write is on this path only
// where the company gave the engine nothing narrower.
func atlasCredential(cfg *config.GitLab, env *config.Resolver) (token, setting string) {
	if env == nil {
		return "", ""
	}
	if token = strings.TrimSpace(env.Value(cfg.Token)); token != "" {
		return token, settingToken
	}
	return strings.TrimSpace(env.Value(cfg.Provisioning.AdminToken)), settingAdminToken
}

// atlasAccess is what a read of the group is made with: the client under the
// read's one credential, counted, and what the settings say of the group.
type atlasAccess struct {
	client *Client
	calls  func() int
	web    string
	host   string
	group  string
	// setting is where the credential is held, for a finding to name.
	setting string
	// extras is the projects the settings name beside the group's own.
	extras []string
}

// openAtlas resolves the access a read is made with, and answers
// [integration.ErrNotConfigured] where the company gave none.
func openAtlas(in collect.Input) (atlasAccess, error) {
	if in.Config == nil {
		return atlasAccess{}, fmt.Errorf("gitlab: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.GitLab
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil || strings.TrimSpace(cfg.Provisioning.Group) == "" {
		return atlasAccess{}, fmt.Errorf("gitlab: %w: integrations.gitlab is off or names no group",
			integration.ErrNotConfigured)
	}
	token, setting := atlasCredential(cfg, in.Env)
	if token == "" {
		return atlasAccess{}, fmt.Errorf("gitlab: %w: neither %s nor %s resolved, so there is nothing to read with",
			integration.ErrNotConfigured, settingToken, settingAdminToken)
	}
	web := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(cfg.URL), "/"), APIPath)
	at, err := url.Parse(web)
	if err != nil || at.Hostname() == "" {
		return atlasAccess{}, errors.New("gitlab: integrations.gitlab.url is not an address with a host")
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	client, err := NewClient(ClientOptions{URL: web, Token: token, HTTP: counted})
	if err != nil {
		return atlasAccess{}, fmt.Errorf("%w: %w", integration.ErrNotConfigured, err)
	}
	return atlasAccess{client: client, calls: calls, web: web, host: strings.ToLower(at.Hostname()),
		group: strings.ToLower(strings.TrimSpace(cfg.Provisioning.Group)), setting: setting,
		extras: cfg.Provisioning.Projects}, nil
}

// extraPaths is the projects the settings name beside the group's own, each
// by its whole path: a bare name is one of the group's.
func (a atlasAccess) extraPaths() []string {
	var out []string
	for _, path := range a.extras {
		path = strings.Trim(strings.TrimSpace(path), "/")
		if path != "" && !strings.Contains(path, "/") {
			path = a.group + "/" + path
		}
		if path != "" {
			out = append(out, path)
		}
	}
	return out
}

// Collect reads the group and reports it as claims in the integration's
// voice. It writes nothing at GitLab.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	access, err := openAtlas(in)
	if err != nil {
		return collect.Read{}, err
	}
	r := &atlasRead{
		atlasAccess: access, b: collect.NewBuilder(integration.KindGitLab, in.Now),
		unread: map[string]map[string]bool{partProjects: {}, partPipelines: {}, partEnvironments: {}},
	}
	if err := r.read(ctx); err != nil {
		return collect.Read{}, err
	}
	r.b.Calls(access.calls())
	return r.b.Read(), nil
}

// atlasRead is one read in flight.
type atlasRead struct {
	atlasAccess
	b *collect.Builder
	// unread is the repositories each part could not be read for, by key.
	unread map[string]map[string]bool
}

// refused reports GitLab refusing this credential the request, as opposed to
// failing to answer it: a role the credential's owner lacks, or the 404 it
// answers for what a credential cannot see. Never a 401, which is a
// credential that stopped working mid-read, and never its rate limit, which a
// wait clears: both fail the read whole.
func refused(err error) bool {
	status := Status(err)
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// ofRepository reports a claim about any repository, whichever part made it.
func ofRepository(c atlas.Claim) bool {
	repo, _ := partOf(c)
	return repo != ""
}

// read is the read itself: the group, every project it holds, the projects
// the settings name beside it, and each project's parts.
func (r *atlasRead) read(ctx context.Context) error {
	b := r.b
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	account, ok := b.Ref("group", atlas.KindAccount, string(atlas.VendorGitLab)+"/"+r.host+"/"+r.group)
	if !ok {
		return fmt.Errorf("gitlab: the group %q is not an entry the atlas can name", r.group)
	}
	// THE GROUP FIRST, AND IT IS NOT OPTIONAL: it proves the credential, so
	// a later refusal is a role's and never a dead token's.
	group, found, err := r.client.GroupByPath(ctx, r.group)
	if err != nil {
		return fmt.Errorf("gitlab: read the group: %w", integration.Reject(err, Status(err)))
	}
	if !found {
		b.Unread(collect.Unread{Part: partGroup, Holds: ofRepository,
			Detail: "GitLab does not show the group " + r.group + " to the credential in " + r.setting +
				", so none of its projects is read. Make the credential's owner a member of the group."})
		return nil
	}
	projects, err := r.client.InventoryProjects(ctx, group.ID)
	switch {
	case errors.Is(err, ErrInventoryTooLarge):
		b.Unread(collect.Unread{Part: partProjects, Holds: ofRepository,
			Detail: "The group holds more projects than one read walks (" +
				strconv.Itoa(inventoryProjectCeiling) + "), so none is read."})
		return nil
	case err != nil:
		// A LISTING CUT SHORT IS NO READ, GitLab's rate limit included: the
		// credential has just read the group, so no refusal here is a role's.
		return fmt.Errorf("gitlab: list the group's projects: %w", err)
	}
	if projects, err = r.withExtras(ctx, projects); err != nil {
		return err
	}
	slices.SortFunc(projects, func(a, z InventoryProject) int { return cmp.Compare(a.ID, z.ID) })
	for _, p := range projects {
		if err := r.project(ctx, account, p); err != nil {
			return err
		}
	}
	r.findings()
	return nil
}

// withExtras adds the projects the settings name that the group's listing
// did not hold: each is read by its path. One the credential may not see is
// unread, and everything an earlier read recorded of it stands.
func (r *atlasRead) withExtras(ctx context.Context, projects []InventoryProject) ([]InventoryProject, error) {
	listed := map[string]bool{}
	for _, p := range projects {
		listed[strings.ToLower(p.Path)] = true
	}
	for _, path := range r.extraPaths() {
		if listed[strings.ToLower(path)] {
			continue
		}
		listed[strings.ToLower(path)] = true
		p, err := r.client.InventoryProject(ctx, path)
		switch {
		case err == nil:
			projects = append(projects, p)
		case refused(err):
			// A PATH THAT IS NO KEY names nothing an earlier read recorded.
			if key, kerr := atlas.ForgeRepositoryKey(r.web, path); kerr == nil {
				r.unread[partProjects][key] = true
			}
		default:
			return nil, fmt.Errorf("gitlab: read a project the settings name: %w", err)
		}
	}
	return projects, nil
}

// findings reports every part a project was refused, once a part.
func (r *atlasRead) findings() {
	b := r.b
	if repos := r.unread[partProjects]; len(repos) > 0 {
		b.Unread(collect.Unread{Part: partProjects,
			Detail: strconv.Itoa(len(repos)) + " of the projects integrations.gitlab.provisioning.projects names " +
				"were not read: GitLab does not show them to the credential in " + r.setting + ". Make the " +
				"credential's owner a member of each, or take a project that is gone off the list.",
			Holds: func(c atlas.Claim) bool {
				repo, _ := partOf(c)
				return repos[repo]
			}})
	}
	for _, part := range []struct{ name, noun string }{
		{partPipelines, "pipelines"},
		{partEnvironments, "environments"},
	} {
		repos := r.unread[part.name]
		if len(repos) == 0 {
			continue
		}
		b.Unread(collect.Unread{Part: part.name,
			Detail: "The " + part.noun + " of " + strconv.Itoa(len(repos)) + " projects were not read: GitLab " +
				"refused the credential in " + r.setting + ". Give the credential's owner the Reporter role " +
				"on the group, which reads the " + part.noun + " of a private project.",
			Holds: func(c atlas.Claim) bool {
				repo, of := partOf(c)
				return of == part.name && repos[repo]
			}})
	}
}

// partOf is the repository a claim of this voice is about, by its entry's
// key, and the part of a read that made the claim ("" for the repository's
// own). Read off the claim alone, so a read refused a part carries exactly
// what an earlier read of it recorded. The group's claims are no repository's.
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
	case name == pipelineKeyName:
		return repo, partPipelines
	case strings.HasPrefix(name, environmentKeyPrefix):
		return repo, partEnvironments
	}
	return "", ""
}

// project claims one project as a repository and reads its parts.
func (r *atlasRead) project(ctx context.Context, account atlas.Ref, p InventoryProject) error {
	b := r.b
	where := "projects[" + strconv.FormatInt(p.ID, 10) + "]"
	// THE KEY A MIRROR'S ADDRESS REDUCES TO, so a project Gitlet mirrors is
	// one entry.
	key, err := atlas.ForgeRepositoryKey(r.web, p.Path)
	if err != nil {
		b.Withhold(where+".path_with_namespace", err)
		return nil
	}
	repo, ok := b.Ref(where+".path_with_namespace", atlas.KindRepository, key)
	if !ok {
		return nil
	}
	b.Relates(where, repo, atlas.RelationBelongsTo, account)
	b.Says(where+".default_branch", repo, atlas.FactDefaultBranch, p.DefaultBranch)
	status := "active"
	if p.Archived {
		status = "archived"
	}
	b.Says(where+".archived", repo, atlas.FactStatus, status)
	// GITLAB'S OWN WORD, of this host alone: a mirror on Gitlet has Gitlet's.
	b.Says(where+".visibility", repo, atlas.FactVisibility, p.Visibility)
	// AN ARCHIVED OR EMPTY PROJECT RUNS AND DEPLOYS NOTHING, so its parts are
	// not read, and leave the atlas as any pipeline that stopped being listed.
	if p.Archived || p.EmptyRepo {
		return nil
	}
	for _, part := range []struct {
		name string
		on   bool
		read func() error
	}{
		{partPipelines, p.RunsPipelines() && p.DefaultBranch != "",
			func() error { return r.pipeline(ctx, account, repo, p, where) }},
		{partEnvironments, p.KeepsEnvironments(),
			func() error { return r.environments(ctx, account, repo, p, where) }},
	} {
		if !part.on {
			continue
		}
		err := part.read()
		switch {
		case err == nil:
		case refused(err):
			r.unread[part.name][repo.Key] = true
		default:
			// ANYTHING ELSE IS A READ THAT DID NOT FINISH, GitLab's rate
			// limit included, and fails whole: a listing cut short would
			// retire what it did not reach.
			return fmt.Errorf("gitlab: read the %s of a project: %w", part.name, err)
		}
	}
	return nil
}

// pipeline claims the pipeline a project's CI configuration runs, once one
// has finished on the default branch: built from the repository, with that
// pipeline's result. A project has one, so it is named for the project.
func (r *atlasRead) pipeline(ctx context.Context, account, repo atlas.Ref, p InventoryProject, where string) error {
	status, found, err := r.client.LastFinishedPipeline(ctx, p.ID, p.DefaultBranch)
	if err != nil || !found {
		return err
	}
	b := r.b
	at := where + ".pipelines"
	pipeline, ok := b.Ref(at, atlas.KindPipeline, repo.Key+"#"+pipelineKeyName)
	if !ok {
		return nil
	}
	b.Relates(at, pipeline, atlas.RelationBuiltFrom, repo)
	b.Relates(at, pipeline, atlas.RelationBelongsTo, account)
	name := p.Path[strings.LastIndexByte(p.Path, '/')+1:]
	b.Says(where+".path_with_namespace", pipeline, atlas.FactName, collect.PlainLabel(name+" pipeline"))
	b.Says(at+".status", pipeline, atlas.FactLastResult, pipelineResult(status))
	return nil
}

// environments claims each available environment of a project: its name, the
// environment GitLab's tier says it is, the state of its newest deployment,
// and the address and the name it serves the code at. Every status is read
// before anything is claimed, so a refusal leaves no half of the part.
func (r *atlasRead) environments(ctx context.Context, account, repo atlas.Ref, p InventoryProject, where string) error {
	envs, err := r.client.AvailableEnvironments(ctx, p.ID)
	if err != nil {
		return err
	}
	// THE NEAREST PRODUCTION FIRST, THEN BY NAME: an order that does not move
	// between reads, so the bound keeps the same environments every hour.
	slices.SortFunc(envs, func(a, z InventoryEnvironment) int {
		return cmp.Or(cmp.Compare(tierRank(a.Tier), tierRank(z.Tier)), cmp.Compare(a.Name, z.Name),
			cmp.Compare(a.ID, z.ID))
	})
	var past []InventoryEnvironment
	if len(envs) > maxEnvironments {
		envs, past = envs[:maxEnvironments], envs[maxEnvironments:]
	}
	statuses := make([]string, len(envs))
	for i, e := range envs {
		if statuses[i], err = r.client.LastDeploymentStatus(ctx, p.ID, e.ID); err != nil {
			return err
		}
	}
	b := r.b
	for _, e := range past {
		b.Withhold(where+".environments["+strconv.FormatInt(e.ID, 10)+"].name", errTooManyEnvironments)
	}
	for i, e := range envs {
		at := where + ".environments[" + strconv.FormatInt(e.ID, 10) + "]"
		env, ok := b.Pipeline(at+".name", repo, environmentKeyPrefix, e.Name)
		if !ok {
			continue
		}
		b.Relates(at, env, atlas.RelationBuiltFrom, repo)
		b.Relates(at, env, atlas.RelationBelongsTo, account)
		b.Says(at+".name", env, atlas.FactName, collect.PlainLabel(e.Name))
		// A SUGGESTION, as every fact of meaning from an integration is.
		b.Says(at+".tier", env, atlas.FactEnvironment, environmentOf(e))
		b.Says(at+".last_deployment.status", env, atlas.FactLastResult, deploymentResult(statuses[i]))
		b.Deployed(at+".external_url", env, e.ExternalURL)
	}
	return nil
}

// tiers is GitLab's deployment tiers nearest production first, each with the
// atlas's word for it. `other` is an environment GitLab could not place.
var tiers = []struct{ tier, word string }{
	{"production", "production"}, {"staging", "staging"}, {"testing", "test"},
	{"development", "development"}, {"other", ""},
}

// tierRank orders an environment by its tier; one this build has not met, or
// none, sorts last.
func tierRank(tier string) int {
	if i := slices.IndexFunc(tiers, func(t struct{ tier, word string }) bool { return t.tier == tier }); i >= 0 {
		return i
	}
	return len(tiers)
}

// environmentOf is the atlas's word for what an environment is for: its tier
// where GitLab sends one, and what its name spells on an instance too old to.
func environmentOf(e InventoryEnvironment) string {
	if e.Tier == "" {
		return collect.EnvironmentWord(strings.ToLower(strings.TrimSpace(e.Name)))
	}
	if i := tierRank(e.Tier); i < len(tiers) {
		return tiers[i].word
	}
	return ""
}

// pipelineResult folds a finished pipeline's status into the atlas's words. A
// status this build has not met says nothing.
func pipelineResult(status string) string {
	switch status {
	case "success":
		return atlas.ResultPassed
	case "failed":
		return atlas.ResultFailed
	case "canceled", "canceling":
		return atlas.ResultCancelled
	case "skipped":
		return atlas.ResultSkipped
	}
	return ""
}

// deploymentResult folds a deployment's status into the atlas's words.
// Blocked is a deployment waiting on an approval, which has not run yet.
func deploymentResult(status string) string {
	switch status {
	case "success":
		return atlas.ResultPassed
	case "failed":
		return atlas.ResultFailed
	case "running":
		return atlas.ResultRunning
	case "created", "blocked":
		return atlas.ResultQueued
	case "canceled":
		return atlas.ResultCancelled
	case "skipped":
		return atlas.ResultSkipped
	}
	return ""
}

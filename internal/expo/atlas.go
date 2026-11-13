package expo

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// Expo's half of the atlas (ADR-0146): the account's projects and the latest
// build of each on each platform, read as a view-only robot a person made
// for the atlas alone. No agent's robot is ever read as, and a robot with any
// role but Viewer reads nothing.

func init() { collect.Register(atlasSource{}) }

// atlasSource is Expo's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindExpo }

// accountKeyPrefix starts the key of the account, and projectHost the keys
// of a project's builds: a project's own address is expo.dev, its account
// and its slug.
const (
	accountKeyPrefix = string(atlas.VendorExpo) + "/"
	projectHost      = "expo.dev"
	githubWeb        = "https://github.com"
)

// atlasToken is the atlas's token and the account it reads, from the
// company's block, or [integration.ErrNotConfigured] where the company does
// not connect Expo or gave the atlas no token: the token is optional, and
// without it nothing at Expo is read.
func atlasToken(in collect.Input) (token, account string, err error) {
	if in.Config == nil {
		return "", "", fmt.Errorf("expo: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Expo
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil || cfg.Atlas == nil || in.Env == nil {
		return "", "", fmt.Errorf("expo: %w: integrations.expo is off or holds no atlas token",
			integration.ErrNotConfigured)
	}
	account = strings.TrimSpace(in.Env.Value(cfg.Provisioning.AccountName))
	token = strings.TrimSpace(in.Env.Value(cfg.Atlas.Token))
	if account == "" || token == "" {
		return "", "", fmt.Errorf("expo: %w: the account name and integrations.expo.atlas.token did not "+
			"both resolve", integration.ErrNotConfigured)
	}
	return token, account, nil
}

// agentOn is the agent whose sealed token is one of robot's own, "" where
// none is. The atlas never reads as an agent's robot: Expo's own audit log
// would then say the agent made the atlas's reads.
func agentOn(in collect.Input, robot Actor) string {
	for role := range in.Config.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		handle := role.Seat().Handle()
		sealed := strings.TrimSpace(in.Env.Value(
			"${" + seataccess.SinkName(integration.KindExpo, TokenField, handle) + "}"))
		if sealed != "" && prefixOf(robot, sealed) != "" {
			return handle
		}
	}
	return ""
}

// Collect proves the atlas's robot is a Viewer and no agent's, then reads the
// account's projects and reports them as claims in the integration's voice.
// It writes nothing at Expo.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	token, name, err := atlasToken(in)
	if err != nil {
		return collect.Read{}, err
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	client, err := NewClient(ClientOptions{Token: token, HTTP: counted})
	if err != nil {
		return collect.Read{}, err
	}
	robot, err := CheckAtlas(ctx, client, name)
	if refusal, ok := IsRefusal(err); ok {
		// A TOKEN THE CHECK TURNS DOWN READS NOTHING, a robot that can change
		// something above all: the read fails, so nothing is recorded or retired.
		said := fmt.Errorf("expo: the atlas's token is refused: %s %s", refusal.Detail, refusal.Hint)
		if refusal.dead {
			return collect.Read{}, fmt.Errorf("%w: %w", integration.ErrCredentialRejected, said)
		}
		return collect.Read{}, said
	}
	if err != nil {
		return collect.Read{}, fmt.Errorf("expo: check the atlas's token: %w", integration.Reject(err, Status(err)))
	}
	if agent := agentOn(in, robot); agent != "" {
		return collect.Read{}, fmt.Errorf("expo: integrations.expo.atlas.token is a token of %s's own robot; "+
			"add a robot named %s for the atlas alone and paste its token", agent, AtlasRobotName)
	}
	said, projects, err := client.Projects(ctx, name)
	if err != nil {
		// A LISTING THAT DID NOT FINISH FAILS WHOLE: one cut short would
		// retire the projects it did not reach.
		return collect.Read{}, fmt.Errorf("expo: list the account's projects: %w", integration.Reject(err, Status(err)))
	}
	b := collect.NewBuilder(integration.KindExpo, in.Now)
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	account, ok := b.Ref("account.name", atlas.KindAccount, accountKeyPrefix+strings.ToLower(name))
	if !ok {
		return collect.Read{}, fmt.Errorf("expo: the account %q is not an entry the atlas can name", name)
	}
	b.Says("account.name", account, atlas.FactAccountName, collect.PlainLabel(said))
	slices.SortFunc(projects, func(a, z Project) int { return cmp.Compare(a.ID, z.ID) })
	for _, p := range projects {
		readProject(b, account, name, p)
	}
	b.Calls(calls())
	return b.Read(), nil
}

// readProject claims one project as the service its slug names, the
// repository it is built from, and one pipeline per platform it was built
// for, with that platform's latest result.
func readProject(b *collect.Builder, account atlas.Ref, accountName string, p Project) {
	where := "apps[" + p.ID + "]"
	// A PROJECT IS THE SERVICE ITS SLUG NAMES, so what a monitor or a
	// repository says of a service of that name is said of this one.
	app, ok := b.Ref(where+".slug", atlas.KindService, p.Slug)
	if !ok {
		return
	}
	name := collect.PlainLabel(p.Name)
	b.Says(where+".name", app, atlas.FactName, name)
	b.Relates(where, app, atlas.RelationBelongsTo, account)
	b.Relates(where, app, atlas.RelationDependsOn, account)
	var repository atlas.Ref
	linked := false
	if p.RepoOwner != "" && p.RepoName != "" {
		at := where + ".githubRepository"
		if key, err := atlas.ForgeRepositoryKey(githubWeb, p.RepoOwner+"/"+p.RepoName); err != nil {
			b.Withhold(at, err)
		} else if repository, linked = b.Names(at, atlas.KindRepository, key); linked {
			b.Relates(at, app, atlas.RelationBuiltFrom, repository)
		}
	}
	// THE PROJECT'S OWN ADDRESS KEYS ITS BUILDS, named and never claimed: a
	// project need not be linked to a repository, and two may share one.
	project, ok := b.Names(where+".slug", atlas.KindRepository, projectHost+"/"+accountName+"/"+p.Slug)
	if !ok {
		return
	}
	for _, build := range p.Builds {
		label, known := platformLabel(build.Platform)
		result, said := buildResult(build.Status)
		if !known || !said {
			continue
		}
		at := where + ".builds[" + build.Platform + "]"
		pipeline, ok := b.Pipeline(at, project, "", strings.ToLower(build.Platform))
		if !ok {
			continue
		}
		if strings.TrimSpace(name) != "" {
			b.Says(at, pipeline, atlas.FactName, collect.PlainLabel(name+" "+label+" build"))
		}
		b.Says(at+".status", pipeline, atlas.FactLastResult, result)
		b.Says(at, pipeline, atlas.FactRunner, "external")
		b.Relates(at, pipeline, atlas.RelationBelongsTo, account)
		b.Relates(at, app, atlas.RelationDeployedBy, pipeline)
		if linked {
			b.Relates(at, pipeline, atlas.RelationBuiltFrom, repository)
		}
	}
}

// platformLabel is a platform as a person reads it, false for one this build
// does not know.
func platformLabel(platform string) (string, bool) {
	switch platform {
	case PlatformAndroid:
		return "Android", true
	case PlatformIOS:
		return "iOS", true
	}
	return "", false
}

// buildResult is the atlas's word for where a build stands, false for a
// status this build does not know. Expo's one failure, ERRORED, is a build
// that ran and did not pass.
func buildResult(status string) (string, bool) {
	switch status {
	case "NEW", "IN_QUEUE":
		return atlas.ResultQueued, true
	case "IN_PROGRESS", "PENDING_CANCEL":
		return atlas.ResultRunning, true
	case "FINISHED":
		return atlas.ResultPassed, true
	case "ERRORED":
		return atlas.ResultFailed, true
	case "CANCELED":
		return atlas.ResultCancelled, true
	}
	return "", false
}

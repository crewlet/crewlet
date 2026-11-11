package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
)

// atlasCompany is acme's settings: its repositories on Gitlet, and a
// deployment answering at one address.
func atlasCompany() builtin.AtlasSettings {
	c := &config.Company{Name: "acme"}
	c.Code.Backend = config.CodeGitlet
	return builtin.AtlasSettings{Company: c, Revision: "r1",
		Deployment: derive.Deployment{APIAddress: "https://crewlet.acme.example"}}
}

// atlasTool is the operator catalogue's atlas_search over host, deciding as
// every surface decides, and reading the settings running answers.
func atlasTool(t *testing.T, host builtin.CodeReader,
	running func(context.Context) (builtin.AtlasSettings, bool)) tools.SeatCallable {

	t.Helper()
	deps := codeDeps(host)
	deps.Actor = builtin.PrincipalActor
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Code: deps, Authorize: builtin.Decide(chartLeads), Atlas: builtin.AtlasDeps{Settings: running},
	}) {
		if tool.Name() == builtin.AtlasSearchTool {
			return tool.(tools.SeatCallable)
		}
	}
	t.Fatalf("the operator catalogue holds no %s", builtin.AtlasSearchTool)
	return nil
}

func runsAcme(context.Context) (builtin.AtlasSettings, bool) { return atlasCompany(), true }

// atlasAs is a person of the company holding grants, as the call's context
// carries them.
func atlasAs(grants ...iam.Grant) context.Context {
	return iam.WithPrincipal(context.Background(), founderPrincipal(grants...))
}

// atlasAnswer is atlas_search's answer as a caller reads it.
type atlasAnswer struct {
	Entries  []atlas.Entry     `json:"entries"`
	Total    int               `json:"total"`
	Coverage []gather.Coverage `json:"coverage"`
}

func searchAtlas(ctx context.Context, t *testing.T, tool tools.SeatCallable, args map[string]any) atlasAnswer {
	t.Helper()
	got, err := tool.CallForTurn(ctx, nil, args)
	if err != nil || got.Failed {
		t.Fatalf("%s %v = %v %q, want an answer", builtin.AtlasSearchTool, args, err, got.Output)
	}
	var answer atlasAnswer
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("%s answered no JSON object: %v\n%s", builtin.AtlasSearchTool, err, got.Output)
	}
	return answer
}

func kindsOf(entries []atlas.Entry) []atlas.Kind {
	var out []atlas.Kind
	for _, e := range entries {
		if !slices.Contains(out, e.Ref.Kind) {
			out = append(out, e.Ref.Kind)
		}
	}
	return out
}

func gitletCoverage(t *testing.T, a atlasAnswer) gather.Coverage {
	t.Helper()
	i := slices.IndexFunc(a.Coverage, func(c gather.Coverage) bool { return c.Source == gather.SourceGitlet })
	if i < 0 {
		t.Fatalf("the answer's coverage names no Gitlet source: %+v", a.Coverage)
	}
	return a.Coverage[i]
}

// THE ATLAS IS READ ONLY BY A HOLDER OF ITS APP (ADR-0127): every other app is
// refused, naming atlas, before Gitlet is read. Mutation: name another app on
// the atlas_search row and the first half goes red.
func TestTheAtlasIsReadOnlyByAHolderOfItsApp(t *testing.T) {
	t.Parallel()
	host := newGitletHost()
	tool := atlasTool(t, host, runsAcme)
	others := slices.DeleteFunc(slices.Clone(iam.Apps), func(g iam.Grant) bool { return g == iam.GrantAtlas })
	got, err := tool.CallForTurn(atlasAs(others...), nil, map[string]any{})
	refused, ok := errors.AsType[*builtin.DecisionRefused](got.Cause)
	if err != nil || !got.Failed || !ok || !slices.Equal(refused.Decision.Grants, []iam.Grant{iam.GrantAtlas}) {
		t.Fatalf("a person without atlas = %v %q (%v), want a refusal naming the atlas app", err, got.Output,
			got.Cause)
	}
	if len(host.reads) != 0 {
		t.Errorf("a refused call read Gitlet: %+v", host.reads)
	}
	if a := searchAtlas(atlasAs(iam.GrantAtlas), t, tool, map[string]any{}); a.Total == 0 {
		t.Error("a holder of atlas was answered with no entry")
	}
}

// THE ATLAS LISTS REPOSITORIES ONLY FOR A CALLER WHO USES GITLET (ADR-0110), read
// as them; without gitlet Gitlet is asked nothing and the coverage says why.
// Mutation: drop the code.read decision and the second half reads the repository.
func TestTheAtlasListsRepositoriesOnlyForAGitletHolder(t *testing.T) {
	t.Parallel()
	both := newGitletHost()
	a := searchAtlas(atlasAs(iam.GrantAtlas, iam.GrantGitlet), t, atlasTool(t, both, runsAcme), map[string]any{})
	if !slices.Contains(kindsOf(a.Entries), atlas.KindRepository) {
		t.Errorf("a holder of gitlet saw kinds %v, want the company's repository among them", kindsOf(a.Entries))
	}
	if c := gitletCoverage(t, a); !c.Complete {
		t.Errorf("Gitlet's coverage for a holder of gitlet = %+v, want read in full", c)
	}
	if len(both.reads) == 0 || both.reads[0].Asker.Kind != code.PrincipalOperator {
		t.Errorf("Gitlet was asked %+v, want the listing read as the person", both.reads)
	}

	alone := newGitletHost()
	a = searchAtlas(atlasAs(iam.GrantAtlas), t, atlasTool(t, alone, runsAcme), map[string]any{})
	if slices.Contains(kindsOf(a.Entries), atlas.KindRepository) {
		t.Errorf("a person without gitlet was shown a repository: %v", kindsOf(a.Entries))
	}
	if len(alone.reads) != 0 {
		t.Errorf("Gitlet was asked %+v for a person without gitlet, want nothing", alone.reads)
	}
	if c := gitletCoverage(t, a); c.Complete || !strings.Contains(c.Reason, "do not use Gitlet") {
		t.Errorf("Gitlet's coverage for a person without gitlet = %+v, want unread, saying why", c)
	}
	if a.Total == 0 {
		t.Error("a person without gitlet was answered with no entry, want the settings' own")
	}
}

// A QUERY AND A KIND NARROW THE LISTING; an unknown kind, a limit out of bounds
// and a node that runs no company yet are refused, saying so.
func TestTheAtlasIsNarrowedByKindAndQuery(t *testing.T) {
	t.Parallel()
	tool := atlasTool(t, newGitletHost(), runsAcme)
	holder := atlasAs(iam.GrantAtlas, iam.GrantGitlet)
	every := searchAtlas(holder, t, tool, map[string]any{})
	repos := searchAtlas(holder, t, tool, map[string]any{"kind": string(atlas.KindRepository)})
	if len(repos.Entries) == 0 || !slices.Equal(kindsOf(repos.Entries), []atlas.Kind{atlas.KindRepository}) ||
		repos.Total >= every.Total {
		t.Errorf("kind repository answered %d of %d, kinds %v; want repositories alone",
			repos.Total, every.Total, kindsOf(repos.Entries))
	}
	named := searchAtlas(holder, t, tool, map[string]any{"query": "SITE", "kind": string(atlas.KindRepository)})
	if named.Total != 1 || named.Entries[0].Name != "site" {
		t.Fatalf("query SITE among repositories answered %+v, want the one it names", named.Entries)
	}
	// A KEY HOLDING THE QUERY MATCHES TOO: the repository's check is keyed
	// under it.
	if keyed := searchAtlas(holder, t, tool, map[string]any{"query": "site"}); keyed.Total < 2 {
		t.Errorf("query site answered %d entries, want the repository and its check", keyed.Total)
	}
	byID := searchAtlas(holder, t, tool, map[string]any{"query": named.Entries[0].ID.String()})
	if byID.Total != 1 || byID.Entries[0].ID != named.Entries[0].ID {
		t.Errorf("a query naming an entry's id answered %+v, want that entry", byID.Entries)
	}
	if one := searchAtlas(holder, t, tool, map[string]any{"limit": 1}); len(one.Entries) != 1 ||
		one.Total != every.Total {
		t.Errorf("limit 1 answered %d entries of %d, want one entry and the whole count", len(one.Entries), one.Total)
	}
	for _, args := range []map[string]any{{"kind": "spaceship"}, {"limit": 0}, {"limit": 501}} {
		if got, err := tool.CallForTurn(holder, nil, args); err != nil || !got.Failed {
			t.Errorf("%v = %v %q, want it refused", args, err, got.Output)
		}
	}
	idle := atlasTool(t, newGitletHost(), func(context.Context) (builtin.AtlasSettings, bool) {
		return builtin.AtlasSettings{}, false
	})
	if got, err := idle.CallForTurn(holder, nil, map[string]any{}); err != nil || !got.Failed ||
		!strings.Contains(got.Output, "runs no company yet") {
		t.Errorf("a node running no company = %v %q, want it said", err, got.Output)
	}
}

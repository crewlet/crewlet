package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
)

var atlasNow = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// atlasCompany is acme as its settings declare it: Cloudflare and a zone the
// deployment answers in, two servers, a tool server, a model, and Gitlet.
func atlasCompany() *config.Company {
	c := &config.Company{Name: "acme"}
	c.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: "0123456789abcdef0123456789abcdef", Zones: []string{"acme.example"}}}
	c.Integrations.SSH = &config.SSH{Enabled: true, Hosts: []config.SSHHost{
		{ID: "web-1", Address: "9.9.9.9"}, {ID: "db-1", Address: "10.0.0.5"}}}
	c.MCPServers = []config.MCPServer{{Name: "tickets", Transport: config.TransportHTTP,
		URL: "https://mcp.tickets.example/mcp"}}
	c.Providers.LLM = map[string]config.LLMProvider{"main": {Harness: config.HarnessClaudeCode, Model: "claude-opus-5-5"}}
	c.Code.Backend = config.CodeGitlet
	return c
}

type atlasStates []integration.State

func (s atlasStates) LoadIntegrations(context.Context) ([]integration.State, error) { return s, nil }

type atlasAlarms []statelog.Alarm

func (a atlasAlarms) LatestAlarms() []statelog.Alarm { return a }
func (a atlasAlarms) AlarmsEvaluated() bool          { return true }

// atlasSources is acme's atlas: its settings, an integration waiting on a
// person, and a standing alarm on its model, and Gitlet answered by code.
func atlasSources(code queries.CodeSource) queries.Sources {
	return queries.Sources{
		Now:  func() time.Time { return atlasNow },
		Code: code,
		Atlas: &builtin.AtlasDeps{
			Settings: func(context.Context) (builtin.AtlasSettings, bool) {
				return builtin.AtlasSettings{Company: atlasCompany(), Revision: "r9",
					Deployment: derive.Deployment{APIAddress: "https://crewlet.acme.example"}}, true
			},
			Records: func(context.Context) gather.Sources {
				return gather.Sources{
					Integrations: atlasStates{{Kind: integration.KindCloudflare, Endpoint: "https://crewlet.acme.example",
						Outcome: integration.OutcomeBlocked, LastAttemptAt: atlasNow.Add(-time.Hour)}},
					Alarms: atlasAlarms{{Kind: statelog.KindLLMSignInRefused,
						Entries: []statelog.AlarmEntry{{Key: "main", Since: atlasNow.Add(-time.Hour)}}}},
				}
			},
		},
	}
}

// atlasCode is Gitlet serving one repository, api, with one check on its
// default branch.
func atlasCode() *stubCode {
	return &stubCode{answer: func(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
		switch q.Kind {
		case gitlet.ReadRepos:
			return gitlet.ReadAnswer{Repos: []gitlet.RepoSummary{{RepoRef: gitlet.RepoRef{Name: "api", Status: gitlet.RepoStatusActive,
				DefaultBranch: "main", Visibility: gitlet.VisibilityCompany,
				Owner: gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "platform"}}, Head: stubOID}}}, nil
		case gitlet.ReadRepo:
			return gitlet.ReadAnswer{Repo: stubRepo(q.Repo)}, nil
		case gitlet.ReadChecks:
			return gitlet.ReadAnswer{Checks: []gitlet.CheckRow{{CheckKey: gitlet.CheckKey{Name: "test"}, Repo: q.Repo,
				State: gitlet.CheckStatePassed}}}, nil
		}
		return gitlet.ReadAnswer{}, errors.New("not asked of the atlas")
	}}
}

// asAtlasReader is a person holding the atlas, and the further grants named.
func asAtlasReader(t *testing.T, grants ...iam.Grant) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "ada", Kind: iam.KindPerson,
		Seat: "ada", Stage: iam.StageActive, Grants: append([]iam.Grant{iam.GrantAtlas}, grants...)})
}

func askAtlas(ctx context.Context, t *testing.T, s queries.Sources, what string, params map[string]any) map[string]any {
	t.Helper()
	r := queries.NewRegistry()
	queries.Register(r, s)
	answer, err := r.Answer(ctx, what, params)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return asMap(t, answer)
}

// listOf is a list member of an answer as rows.
func listOf(t *testing.T, v any) []map[string]any {
	t.Helper()
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

func entryID(k atlas.Kind, key string) string { return atlas.MustRef(k, key).ID().String() }

// THE ATLAS IS NEVER A WAY ROUND ANOTHER PERMISSION: a holder without Gitlet
// is read no repository, and Gitlet is never asked, and only an
// administrator reads an integration's standing. A standing alarm on a model
// is every holder's, as the Models screen is.
func TestTheAtlasIsNeverAWayRoundAnotherPermission(t *testing.T) {
	t.Parallel()
	cf := entryID(atlas.KindAccount, "cloudflare/0123456789abcdef0123456789abcdef")
	model := entryID(atlas.KindModel, "llm/main")
	member := atlasCode()
	s := atlasSources(member)
	ctx := asAtlasReader(t)
	signals := func(ctx context.Context, s queries.Sources, id string) []map[string]any {
		entry, _ := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": id})["entry"].(map[string]any)
		return listOf(t, entry["signals"])
	}
	if got := signals(ctx, s, cf); len(got) != 0 {
		t.Errorf("a member read an integration's standing: %v", got)
	}
	if got := signals(ctx, s, model); len(got) != 1 || got[0]["alarm"] != string(statelog.KindLLMSignInRefused) {
		t.Errorf("a member reads the model's standing alarm as %v", got)
	}
	answer := askAtlas(ctx, t, s, "atlas_entries", map[string]any{"kind": "repository"})
	if len(member.asked) != 0 || answer["total"] != float64(0) {
		t.Errorf("a holder without Gitlet was read %v repositories after %d questions to Gitlet",
			answer["total"], len(member.asked))
	}
	coverage := listOf(t, answer["coverage"])
	i := slices.IndexFunc(coverage, func(c map[string]any) bool { return c["source"] == "gitlet" })
	if i < 0 || coverage[i]["complete"] != false || !strings.Contains(coverage[i]["reason"].(string), "Gitlet") {
		t.Errorf("the read does not say why Gitlet was not read: %v", coverage)
	}

	admin := atlasCode()
	s = atlasSources(admin)
	ctx = asAtlasReader(t, iam.GrantAdmin, iam.GrantGitlet)
	if got := signals(ctx, s, cf); len(got) != 1 || got[0]["kind"] != string(atlas.SignalIntegrationBlocked) {
		t.Errorf("an administrator reads Cloudflare's standing as %v", got)
	}
	answer = askAtlas(ctx, t, s, "atlas_entries", map[string]any{"kind": "repository"})
	if answer["total"] != float64(1) || len(admin.asked) == 0 {
		t.Errorf("a Gitlet holder was read %v repositories", answer["total"])
	}
	for _, q := range admin.asked {
		if q.Asker.Seat != "ada" {
			t.Errorf("Gitlet was asked %s as %+v, not as the reader", q.Kind, q.Asker)
		}
	}
}

// AN ID NO ENTRY ANSWERS TO IS NOT FOUND, AND A BAD PARAMETER IS REFUSED,
// naming it.
func TestAnAtlasQuestionRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()
	r := queries.NewRegistry()
	queries.Register(r, atlasSources(atlasCode()))
	ctx := asAtlasReader(t)
	if _, err := r.Answer(ctx, "atlas_entry", map[string]any{"id": uuid.NewString()}); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("an unknown id: %v", err)
	}
	for what, params := range map[string]map[string]any{
		"atlas_entries": {"kind": "server"},
		"atlas_map":     {"health": "fine"},
		"atlas_trace":   {"id": entryID(atlas.KindHost, "ssh/web-1"), "depth": 7},
		"atlas_entry":   {"id": "web-1"},
		"atlas_facets":  {"unconfirmed": "maybe"},
	} {
		if _, err := r.Answer(ctx, what, params); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s %v: %v, want bad params", what, params, err)
		}
	}
	if _, err := r.Answer(ctx, "atlas_facets", nil); err != nil {
		t.Errorf("a holder's facets: %v", err)
	}
	noApp := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "bo", Kind: iam.KindPerson,
		Seat: "bo", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}})
	if _, err := r.Answer(noApp, "atlas_entries", nil); err == nil {
		t.Error("a person without the atlas read it")
	}
}

// rich is a fold of claims from every kind of source, so every optional
// member of an entry's shape is sent somewhere: a person's correction an
// integration contradicts, an agent's observation, an integration's suggestions, a
// person's denial the settings outrank, an owner, use, holders, a standing
// alarm and a tool server nothing used.
func rich(t *testing.T) atlas.View {
	t.Helper()
	ok := func(c atlas.Claim, err error) atlas.Claim {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ada, sre := atlas.PersonSource("ada"), atlas.AgentSource("sre", "turn-1", atlas.EvidenceCall)
	cf, settings := atlas.IntegrationSource("cloudflare"), atlas.SettingsSource("r9")
	db := atlas.MustRef(atlas.KindHost, "ssh/db-1")
	svc := atlas.MustRef(atlas.KindService, "checkout-api")
	tool := atlas.MustRef(atlas.KindToolServer, "mcp/tickets")
	address := ok(atlas.Says(db, atlas.FactAddress, "10.0.0.5", settings, atlasNow))
	depends := ok(atlas.Relates(svc, atlas.RelationRunsOn, db, settings, atlasNow))
	claims := []atlas.Claim{
		ok(atlas.Exists(db, settings, atlasNow)), address, ok(atlas.Denial(address, ada, atlasNow.Add(-time.Hour))),
		depends, ok(atlas.Denial(depends, ada, atlasNow.Add(-time.Hour))),
		ok(atlas.Exists(tool, settings, atlasNow)),
		ok(atlas.Names(svc, atlas.RelationOwnedBy, atlas.ExternalSeat, "sre", cf, atlasNow)),
		ok(atlas.Exists(svc, ada, atlasNow.Add(-2*time.Hour))),
		ok(atlas.Says(svc, atlas.FactName, "Checkout", ada, atlasNow.Add(-2*time.Hour))),
		ok(atlas.Says(svc, atlas.FactName, "Checkout API", cf, atlasNow.Add(-time.Hour))),
		ok(atlas.Says(svc, atlas.FactEnvironment, "production", cf, atlasNow)),
		ok(atlas.Says(db, atlas.FactPort, "5432", sre, atlasNow)),
		ok(atlas.Names(svc, atlas.RelationOwnedBy, atlas.ExternalUnit, "payments", ada, atlasNow)),
	}
	return atlas.Join(atlas.Fold(claims, atlasNow), atlas.Joined{
		Use: map[atlas.Ref][]atlas.Use{db: {{Channel: atlas.UseCommand, Counted: true, Week: 2, Month: 5,
			Last: atlasNow.Add(-time.Hour), Seats: []string{"sre"}}}},
		Holders: map[atlas.Ref][]atlas.Holder{db: {{Seat: "sre", Lane: "account", Level: "read", Enabled: true,
			ObservedAt: atlasNow}}},
		Signals: map[atlas.Ref][]atlas.Signal{svc: {{Kind: atlas.SignalStandingAlarm, Severity: atlas.SeverityDanger,
			Audience: atlas.AudienceHolders, Since: atlasNow, Alarm: "mcp_server_failing"}}},
		Counted: map[atlas.UseChannel]bool{atlas.UseCommand: true, atlas.UseToolCall: true},
	}, atlasNow)
}

// THE ATLAS ANSWERS ARE THEIR DECLARED SHAPES: every answer and every row
// the dashboard declares in contract/atlas.ts is what the questions send,
// both ways, read from the real questions over acme and, for the members only
// people and agents give, from a fold of their claims.
func TestTheAtlasAnswersAreTheirDeclaredShapes(t *testing.T) {
	t.Parallel()
	s := atlasSources(atlasCode())
	ctx := asAtlasReader(t, iam.GrantAdmin, iam.GrantGitlet)
	entries := askAtlas(ctx, t, s, "atlas_entries", map[string]any{"limit": 2})
	mp := askAtlas(ctx, t, s, "atlas_map", nil)
	cf := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": entryID(atlas.KindAccount,
		"cloudflare/0123456789abcdef0123456789abcdef")})
	model := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": entryID(atlas.KindModel, "llm/main")})
	host := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": entryID(atlas.KindHostname, "crewlet.acme.example")})
	// A repository, whose visibility is a fact of its host and says which.
	repo := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": entryID(atlas.KindRepository, "gitlet/api")})
	trace := askAtlas(ctx, t, s, "atlas_trace", map[string]any{"id": entryID(atlas.KindDomain, "acme.example")})
	facets := askAtlas(ctx, t, s, "atlas_facets", nil)
	answers := []map[string]any{entries, mp, cf, trace, facets}

	richEntries := listOf(t, asMap(t, rich(t))["entries"])
	full := append([]map[string]any{cf["entry"].(map[string]any), model["entry"].(map[string]any),
		host["entry"].(map[string]any), repo["entry"].(map[string]any)}, richEntries...)
	pluck := func(rows []map[string]any, member string) []map[string]any {
		var out []map[string]any
		for _, r := range rows {
			if v, ok := r[member].(map[string]any); ok {
				out = append(out, v)
				continue
			}
			out = append(out, listOf(t, r[member])...)
		}
		return out
	}
	facts, edges := pluck(full, "facts"), pluck(full, "relations")
	summaries := append(append(listOf(t, entries["rows"]), listOf(t, mp["nodes"])...), pluck(listOf(t, cf["used_by"]), "entry")...)
	for iface, rows := range map[string][]map[string]any{
		"AtlasRead":          answers,
		"AtlasCoverage":      pluck(answers, "coverage"),
		"AtlasSummary":       summaries,
		"AtlasEntriesAnswer": {entries},
		"AtlasMapLane":       listOf(t, mp["lanes"]),
		"AtlasMapGroup":      listOf(t, mp["groups"]),
		"AtlasMapEdge":       append(listOf(t, mp["edges"]), listOf(t, trace["edges"])...),
		"AtlasMapAnswer":     {mp},
		"AtlasEntry":         full,
		"AtlasRef":           append(pluck(full, "ref"), pluck(full, "members")...),
		"AtlasFactView":      facts,
		"AtlasValue":         append(pluck(facts, "value"), pluck(pluck(facts, "others"), "value")...),
		"AtlasSource":        append(pluck(facts, "source"), pluck(full, "sources")...),
		"AtlasClaimed":       pluck(append(facts, edges...), "others"),
		"AtlasEdge":          edges,
		"AtlasUse":           pluck(full, "use"),
		"AtlasHolder":        pluck(full, "holders"),
		"AtlasSignal":        pluck(full, "signals"),
		"AtlasLink":          listOf(t, cf["used_by"]),
		"AtlasEntryAnswer":   {cf, model},
		"AtlasTraceHop":      listOf(t, trace["hops"]),
		"AtlasTraceAnswer":   {trace},
		"AtlasFacetsAnswer":  {facets},
	} {
		holdShape(t, iface, rows, iface == "AtlasRead")
	}
}

// THE DASHBOARD NAMES EVERY ATLAS WORD: each closed set the atlas answers in
// is the engine's, member for member, both ways.
func TestTheDashboardNamesEveryAtlasWord(t *testing.T) {
	t.Parallel()
	strs := func(set any) []string {
		raw, _ := json.Marshal(set)
		var out []string
		_ = json.Unmarshal(raw, &out)
		return out
	}
	for name, set := range map[string][]string{
		"AtlasKind":           strs(atlas.Kinds),
		"AtlasLane":           strs(atlas.Lanes),
		"AtlasRelation":       strs(atlas.Relations),
		"AtlasVendor":         strs(atlas.Vendors),
		"AtlasFact":           strs(atlas.Facts),
		"AtlasValueType":      strs(atlas.ValueTypes),
		"AtlasSourceKind":     strs(atlas.SourceKinds),
		"AtlasEvidence":       strs(atlas.Evidences),
		"AtlasConfidence":     strs(atlas.Confidences),
		"AtlasWord":           strs(atlas.Words),
		"AtlasState":          strs(atlas.States),
		"AtlasStanding":       strs(atlas.Standings),
		"AtlasExternal":       strs(atlas.Externals),
		"AtlasSignalKind":     strs(atlas.SignalKinds),
		"AtlasSeverity":       strs(atlas.Severities),
		"AtlasAudience":       strs(atlas.Audiences),
		"AtlasHealth":         strs(atlas.Healths),
		"AtlasUseChannel":     strs(atlas.UseChannels),
		"AtlasDirection":      strs(atlas.Directions),
		"AtlasCoverageSource": strs(gather.SourceNames),
	} {
		declared, err := clientsource.Union(clientsource.Tree(t), name)
		if err != nil {
			t.Fatal(err)
		}
		if len(set) == 0 {
			t.Fatalf("the engine's set behind %s is empty, so this certifies nothing", name)
		}
		slices.Sort(declared)
		want := slices.Sorted(slices.Values(set))
		if !slices.Equal(declared, want) {
			t.Errorf("the dashboard's %s is %v, and the engine's %v", name, declared, want)
		}
	}
}

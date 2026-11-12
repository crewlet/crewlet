package queries_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/crewlet/crewlet/internal/atlasdomain"
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

// atlasLog is the atlas's own log as a data node answers it: the claims it
// holds live, and every line it took.
type atlasLog struct {
	live  []atlas.Claim
	lines []atlasdomain.LogLine
}

func (l *atlasLog) Live(context.Context, statelog.Freshness) (atlasdomain.Live, error) {
	return atlasdomain.Live{Claims: l.live, Complete: true}, nil
}

// newestFirst is the log's order.
func newestFirst(a, b atlasdomain.LogLine) int {
	w := func(l atlasdomain.LogLine) int {
		if l.Withdrawn {
			return 1
		}
		return 0
	}
	//fixedwait:stated both are lines' instants, which the fixture states
	return cmp.Or(b.At.Compare(a.At), cmp.Compare(w(b), w(a)), cmp.Compare(b.ID, a.ID))
}

func (l *atlasLog) Log(_ context.Context, q atlasdomain.LogQuery) (atlasdomain.LogPage, error) {
	lines := slices.Clone(l.lines)
	slices.SortFunc(lines, newestFirst)
	from := 0
	if q.Cursor != "" {
		at := slices.IndexFunc(lines, func(x atlasdomain.LogLine) bool { return x.Cursor() == q.Cursor })
		if at < 0 {
			return atlasdomain.LogPage{}, fmt.Errorf("%w: not a cursor", atlasdomain.ErrInvalid)
		}
		from = at + 1
	}
	var page atlasdomain.LogPage
	for _, line := range lines[from:] {
		if len(q.Entries) > 0 && !slices.Contains(q.Entries, line.Entry) {
			continue
		}
		if len(page.Lines) == q.Limit {
			page.Next = page.Lines[len(page.Lines)-1].Cursor()
			break
		}
		page.Lines = append(page.Lines, line)
	}
	page.Complete = true
	return page, nil
}

func (l *atlasLog) Activity(_ context.Context, since time.Time, _ statelog.Freshness) (atlasdomain.Activity, error) {
	out := atlasdomain.Activity{Complete: true}
	newest := map[string]time.Time{}
	for _, line := range l.lines {
		if line.At.After(newest[line.Entry]) { //fixedwait:stated both are lines' instants, which the fixture states
			newest[line.Entry] = line.At
		}
		if line.At.Before(since) { //fixedwait:stated a line's instant the fixture states, against the window the question derives from its own clock
			continue
		}
		target := ""
		if t := line.Claim.Facet.Target.Entry; t != (atlas.Ref{}) {
			target = t.ID().String()
		}
		out.Days = append(out.Days, atlasdomain.ActivityDay{Entry: line.Entry, Target: target,
			Day: line.At.Truncate(24 * time.Hour), Lines: 1})
	}
	for _, line := range l.lines {
		if line.At.Equal(newest[line.Entry]) {
			out.Last = append(out.Last, line)
		}
	}
	return out, nil
}

// The entries acme's log talks about.
var (
	atlasWeb      = atlas.MustRef(atlas.KindHost, "ssh/web-1")
	atlasRepo     = atlas.MustRef(atlas.KindRepository, "gitlet/api")
	atlasCheckout = atlas.MustRef(atlas.KindService, "checkout-api")
	atlasWWW      = atlas.MustRef(atlas.KindHostname, "www.acme.example")
	atlasGuess    = atlas.MustRef(atlas.KindHost, "addr/8.8.8.8")
	atlasAccount  = atlas.MustRef(atlas.KindAccount, "cloudflare/0123456789abcdef0123456789abcdef")
)

// acmeLog is what people, an integration and an agent recorded about acme:
// ada's purpose and owner for a server, her purpose for the Gitlet
// repository and the service she added as built from it, Cloudflare's host
// name (seen three weeks ago, its proxying withdrawn this morning), and a
// server an agent reported with nothing to check.
func acmeLog(t *testing.T) *atlasLog {
	t.Helper()
	ok := func(c atlas.Claim, err error) atlas.Claim {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ada, cf := atlas.PersonSource("ada"), atlas.IntegrationSource("cloudflare")
	agent := atlas.AgentSource("sre", "turn-1", atlas.EvidenceNone)
	hours := func(n int) time.Time { return atlasNow.Add(-time.Duration(n) * time.Hour) }
	log := &atlasLog{}
	add := func(id string, at time.Time, withdrawn bool, c atlas.Claim) {
		log.lines = append(log.lines, atlasdomain.LogLine{ID: id, Entry: c.Subject.ID().String(), Claim: c, At: at,
			Withdrawn: withdrawn})
		if !withdrawn {
			log.live = append(log.live, c)
		}
	}
	add("l1", hours(2), false, ok(atlas.Says(atlasWeb, atlas.FactPurpose, "Serves the storefront", ada, hours(2))))
	add("l2", hours(26), false, ok(atlas.Names(atlasWeb, atlas.RelationOwnedBy, atlas.ExternalUnit, "platform", ada, hours(26))))
	add("l3", hours(3), false, ok(atlas.Says(atlasRepo, atlas.FactPurpose, "The API's code", ada, hours(3))))
	add("l4", hours(50), false, ok(atlas.Exists(atlasCheckout, ada, hours(50))))
	add("l5", hours(49), false, ok(atlas.Relates(atlasCheckout, atlas.RelationBuiltFrom, atlasRepo, ada, hours(49))))
	add("l6", hours(20*24), false, ok(atlas.Exists(atlasWWW, cf, hours(20*24))))
	add("l7", hours(5), true, ok(atlas.Relates(atlasWWW, atlas.RelationFrontedBy, atlasAccount, cf, hours(20*24))))
	add("l8", hours(1), false, ok(atlas.Exists(atlasGuess, agent, hours(1))))
	return log
}

// atlasSources is acme's atlas: its settings, an integration waiting on a
// person, and a standing alarm on its model, and Gitlet answered by code.
// Nobody recorded anything in it.
func atlasSources(code queries.CodeSource) queries.Sources { return atlasSourcesWith(code, nil) }

// atlasSourcesWith is acme's atlas with log as its own log, nil for a node
// that reaches none.
func atlasSourcesWith(code queries.CodeSource, log *atlasLog) queries.Sources {
	return queries.Sources{
		Now:  func() time.Time { return atlasNow },
		Code: code,
		Atlas: &builtin.AtlasDeps{
			Settings: func(context.Context) (builtin.AtlasSettings, bool) {
				return builtin.AtlasSettings{Company: atlasCompany(), Revision: "r9",
					Deployment: derive.Deployment{APIAddress: "https://crewlet.acme.example"}}, true
			},
			Log: func(context.Context) (builtin.AtlasLogReads, bool) { return log, log != nil },
			Records: func(context.Context) gather.Sources {
				var recorded gather.Log
				if log != nil {
					recorded = log
				}
				return gather.Sources{
					Log: recorded,
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
		"atlas_entries":  {"kind": "server"},
		"atlas_map":      {"health": "fine"},
		"atlas_trace":    {"id": entryID(atlas.KindHost, "ssh/web-1"), "depth": 7},
		"atlas_entry":    {"id": "web-1"},
		"atlas_facets":   {"unconfirmed": "maybe"},
		"atlas_log":      {"id": "web-1"},
		"atlas_activity": {"unconfirmed": "maybe"},
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
	s := atlasSourcesWith(atlasCode(), acmeLog(t))
	ctx := asAtlasReader(t, iam.GrantAdmin, iam.GrantGitlet)
	entries := askAtlas(ctx, t, s, "atlas_entries", map[string]any{"limit": 2})
	mp := askAtlas(ctx, t, s, "atlas_map", map[string]any{"relations": "all"})
	log := askAtlas(ctx, t, s, "atlas_log", map[string]any{"limit": 6})
	activity := askAtlas(ctx, t, s, "atlas_activity", nil)
	cf := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": entryID(atlas.KindAccount,
		"cloudflare/0123456789abcdef0123456789abcdef")})
	model := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": entryID(atlas.KindModel, "llm/main")})
	host := askAtlas(ctx, t, s, "atlas_entry", map[string]any{"id": entryID(atlas.KindHostname, "crewlet.acme.example")})
	trace := askAtlas(ctx, t, s, "atlas_trace", map[string]any{"id": entryID(atlas.KindDomain, "acme.example")})
	facets := askAtlas(ctx, t, s, "atlas_facets", nil)
	answers := []map[string]any{entries, mp, cf, trace, facets, log, activity}
	lines := append(listOf(t, log["lines"]), pluck2(t, listOf(t, activity["entries"]), "last")...)

	richEntries := listOf(t, asMap(t, rich(t))["entries"])
	full := append([]map[string]any{cf["entry"].(map[string]any), model["entry"].(map[string]any),
		host["entry"].(map[string]any)}, richEntries...)
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
		"AtlasRead":           answers,
		"AtlasCoverage":       pluck(answers, "coverage"),
		"AtlasSummary":        summaries,
		"AtlasEntriesAnswer":  {entries},
		"AtlasMapLane":        listOf(t, mp["lanes"]),
		"AtlasMapGroup":       listOf(t, mp["groups"]),
		"AtlasMapEdge":        append(listOf(t, mp["edges"]), listOf(t, trace["edges"])...),
		"AtlasMapAnswer":      {mp},
		"AtlasEntry":          full,
		"AtlasRef":            append(pluck(full, "ref"), pluck(full, "members")...),
		"AtlasFactView":       facts,
		"AtlasValue":          append(pluck(facts, "value"), pluck(pluck(facts, "others"), "value")...),
		"AtlasSource":         append(pluck(facts, "source"), pluck(full, "sources")...),
		"AtlasClaimed":        pluck(append(facts, edges...), "others"),
		"AtlasEdge":           edges,
		"AtlasUse":            pluck(full, "use"),
		"AtlasHolder":         pluck(full, "holders"),
		"AtlasSignal":         pluck(full, "signals"),
		"AtlasLink":           listOf(t, cf["used_by"]),
		"AtlasEntryAnswer":    {cf, model},
		"AtlasTraceHop":       listOf(t, trace["hops"]),
		"AtlasTraceAnswer":    {trace},
		"AtlasFacetsAnswer":   {facets},
		"AtlasOwner":          pluck(summaries, "owner"),
		"AtlasRelationCount":  pluck(summaries, "relations"),
		"AtlasLine":           lines,
		"AtlasLineTarget":     pluck(lines, "target"),
		"AtlasLogAnswer":      {log},
		"AtlasEntryActivity":  listOf(t, activity["entries"]),
		"AtlasActivityAnswer": {activity},
	} {
		holdShape(t, iface, rows, iface == "AtlasRead")
	}
}

// pluck2 is the member of each row that is an object, where it has one.
func pluck2(t *testing.T, rows []map[string]any, member string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range rows {
		if v, ok := r[member].(map[string]any); ok {
			out = append(out, v)
		}
	}
	return out
}

// THE ATLAS'S LOG SHOWS A READER ONLY WHAT THEIR ATLAS SHOWS THEM: a line on
// a repository is a Gitlet holder's, as the repository is, and so is a line
// that names it; a line on an entry only an agent reported stays out until a
// read asks for those; and the counts are of exactly the lines shown. Pages
// of any size hold every shown line once, newest first.
//
// Mutation: answer every line in atlasLog (drop the `shown` check, or the
// target's in View.Line) and a holder without Gitlet reads the repository's
// purpose, or its name on the service's line.
func TestTheAtlasLogShowsAReaderOnlyWhatTheirAtlasShows(t *testing.T) {
	t.Parallel()
	s := atlasSourcesWith(atlasCode(), acmeLog(t))
	ids := func(answer map[string]any) []string {
		var out []string
		for _, l := range listOf(t, answer["lines"]) {
			out = append(out, l["id"].(string))
		}
		return out
	}
	member := asAtlasReader(t)
	if got, want := ids(askAtlas(member, t, s, "atlas_log", nil)), []string{"l1", "l7:withdrawn", "l2", "l4", "l6"}; !slices.Equal(got, want) {
		t.Errorf("a holder without Gitlet reads %v, want %v: nothing on or naming the repository, nothing unconfirmed", got, want)
	}
	coder := asAtlasReader(t, iam.GrantGitlet)
	whole := askAtlas(coder, t, s, "atlas_log", nil)
	if got, want := ids(whole), []string{"l1", "l3", "l7:withdrawn", "l2", "l5", "l4", "l6"}; !slices.Equal(got, want) {
		t.Errorf("a Gitlet holder reads %v, want %v", got, want)
	}
	if got := ids(askAtlas(coder, t, s, "atlas_log", map[string]any{"unconfirmed": true})); len(got) != 8 || got[0] != "l8" {
		t.Errorf("a read that shows unconfirmed entries reads %v, want the agent's line first", got)
	}
	var paged []string
	for cursor, pages := "", 0; pages < 10; pages++ {
		params := map[string]any{"limit": 2}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page := askAtlas(member, t, s, "atlas_log", params)
		paged = append(paged, ids(page)...)
		if cursor, _ = page["next_cursor"].(string); cursor == "" {
			break
		}
	}
	if want := []string{"l1", "l7:withdrawn", "l2", "l4", "l6"}; !slices.Equal(paged, want) {
		t.Errorf("pages of two hold %v, want every shown line once: %v", paged, want)
	}

	own := askAtlas(coder, t, s, "atlas_log", map[string]any{"id": atlasCheckout.ID().String()})
	if got := ids(own); !slices.Equal(got, []string{"l5", "l4"}) {
		t.Errorf("the service's own log is %v", got)
	}
	built := listOf(t, own["lines"])[0]
	if target, _ := built["target"].(map[string]any); built["change"] != "related" || built["relation"] != "built_from" ||
		target["id"] != atlasRepo.ID().String() || built["text"] != "Recorded a relation: built from" {
		t.Errorf("the service's relation reads %v", built)
	}
	if got := ids(askAtlas(coder, t, s, "atlas_log", map[string]any{"id": atlasGuess.ID().String()})); !slices.Equal(got, []string{"l8"}) {
		t.Errorf("an unconfirmed entry asked for by its id reads %v, want its own line", got)
	}
	r := queries.NewRegistry()
	queries.Register(r, s)
	if _, err := r.Answer(member, "atlas_log", map[string]any{"id": atlasRepo.ID().String()}); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a holder without Gitlet asked for the repository's log: %v, want not found", err)
	}
	if _, err := r.Answer(member, "atlas_log", map[string]any{"cursor": "yesterday"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a cursor no page answered: %v, want bad params", err)
	}

	counted := func(ctx context.Context, params map[string]any) (days []any, entries map[string]map[string]any) {
		answer := askAtlas(ctx, t, s, "atlas_activity", params)
		entries = map[string]map[string]any{}
		for _, e := range listOf(t, answer["entries"]) {
			entries[e["id"].(string)] = e
		}
		if answer["from"] != "2026-10-29T00:00:00Z" {
			t.Errorf("the window starts at %v, want fourteen days ending today", answer["from"])
		}
		return answer["days"].([]any), entries
	}
	sum := func(days []any) (n float64) {
		for _, d := range days {
			n += d.(float64)
		}
		return n
	}
	days, entries := counted(member, nil)
	if len(days) != atlas.ActivityDays || sum(days) != 4 || days[13] != float64(2) || days[12] != float64(1) || days[11] != float64(1) {
		t.Errorf("a holder without Gitlet counts %v, want four lines: two today, one yesterday, one the day before", days)
	}
	if _, told := entries[atlasRepo.ID().String()]; told {
		t.Error("the counts tell a holder without Gitlet of the repository")
	}
	www := entries[atlasWWW.ID().String()]
	if last, _ := www["last"].(map[string]any); last["change"] != "withdrawn" || last["id"] != "l7:withdrawn" ||
		sum(www["days"].([]any)) != 1 {
		t.Errorf("the host name's activity is %v, want its withdrawal as its last line and its only one in the window", www)
	}
	// The service's newest record names the repository, so a holder without
	// Gitlet is shown no last change for it rather than an older one as if it
	// were the newest.
	if svc := entries[atlasCheckout.ID().String()]; svc["last"] != nil || sum(svc["days"].([]any)) != 1 {
		t.Errorf("the service's activity without Gitlet is %v, want its own line counted and no last change", svc)
	}
	days, entries = counted(coder, map[string]any{"unconfirmed": true})
	if sum(days) != 7 || entries[atlasCheckout.ID().String()]["last"].(map[string]any)["id"] != "l5" {
		t.Errorf("a Gitlet holder shown unconfirmed entries counts %v", days)
	}

	bare := queries.NewRegistry()
	queries.Register(bare, atlasSources(atlasCode()))
	for _, what := range []string{"atlas_log", "atlas_activity"} {
		if _, err := bare.Answer(member, what, nil); !errors.Is(err, queries.ErrUnavailable) {
			t.Errorf("%s on a node that reaches no log: %v, want unavailable", what, err)
		}
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
		"AtlasFamily":         strs(atlas.Families),
		"AtlasChange":         strs(atlas.Changes),
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

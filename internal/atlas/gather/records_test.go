package gather_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// Each record source is the engine's own read as it stands.
var (
	_ gather.IntegrationStates = (*integration.CoordStore)(nil)
	_ gather.SeatAccess        = (*seataccess.CoordLedger)(nil)
	_ gather.ToolServers       = (*kv.FleetStore)(nil)
	_ gather.ServerTests       = (*kv.FleetStore)(nil)
	_ gather.Nodes             = (*kv.Store)(nil)
	_ gather.AccessLog         = (*accessdomain.Reader)(nil)
	_ gather.Usage             = usage.Reader{}
)

type fakeIntegrations []integration.State

func (f fakeIntegrations) LoadIntegrations(context.Context) ([]integration.State, error) {
	return f, nil
}

type fakeLedger map[integration.Kind]map[string]seataccess.Record

func (f fakeLedger) Records(_ context.Context, k integration.Kind) (map[string]seataccess.Record, error) {
	return f[k], nil
}

type fakeTools struct {
	listings []coord.MCPListing
	grants   []coord.MCPGrant
}

func (f fakeTools) ListMCPListings(context.Context) ([]coord.MCPListing, error) {
	return f.listings, nil
}
func (f fakeTools) ListMCPGrants(context.Context) ([]coord.MCPGrant, error) { return f.grants, nil }

type fakeNodes []coord.NodeStatus

// ListLive answers each status as a lease carries it: under the status key,
// through the JSON the broker stores it as.
func (f fakeNodes) ListLive(context.Context, coord.Class) ([]coord.Lease, error) {
	out := make([]coord.Lease, 0, len(f))
	for i, s := range f {
		raw, err := json.Marshal(map[string]any{coord.StatusKey: s.Meta()})
		if err != nil {
			return nil, err
		}
		var meta map[string]any
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, err
		}
		out = append(out, coord.Lease{Resource: fmt.Sprintf("node/n%d", i), Meta: meta})
	}
	return out, nil
}

type fakeTests []coord.SSHTest

func (f fakeTests) ListSSHTests(context.Context) ([]coord.SSHTest, error) { return f, nil }

type fakeAccess struct {
	use []accessdomain.HostUse
	err error
}

func (f fakeAccess) HostUse(context.Context, time.Time, time.Time, statelog.Freshness) ([]accessdomain.HostUse, error) {
	return f.use, f.err
}

type fakeUsage struct {
	spend []usage.SpendRow
	calls map[string][]usage.ServerToolCallsRow
	err   error
}

func (f fakeUsage) Spend(context.Context, usage.SpendQuery) ([]usage.SpendRow, error) {
	return f.spend, f.err
}
func (f fakeUsage) ServerToolCalls(_ context.Context, q usage.ServerToolCallsQuery) ([]usage.ServerToolCallsRow, error) {
	return f.calls[q.Server], f.err
}

// company is acme with Cloudflare, two servers, two tool servers and a model.
func company() *config.Company {
	c := acme()
	c.Integrations.Cloudflare = &config.Cloudflare{Enabled: true,
		Provisioning: &config.CloudflareProvisioning{AccountID: "0123456789abcdef0123456789abcdef"}}
	c.Integrations.SSH = &config.SSH{Enabled: true, Hosts: []config.SSHHost{
		{ID: "web-1", Address: "9.9.9.9"}, {ID: "db-1", Address: "10.0.0.5"}}}
	c.MCPServers = []config.MCPServer{
		{Name: "tickets", Transport: config.TransportHTTP, URL: "https://mcp.tickets.example/mcp"},
		{Name: "files", Transport: config.TransportStdio, Command: "files-mcp"},
	}
	c.Providers.LLM = map[string]config.LLMProvider{"main": {Harness: config.HarnessClaudeCode, Model: "claude-opus-5-5"}}
	return c
}

func hostDigest(t *testing.T, c *config.Company, id string) string {
	t.Helper()
	h, ok := c.Integrations.SSH.Host(id)
	if !ok {
		t.Fatalf("no host %s", id)
	}
	return ssh.HostDigest(h)
}

// records is every record source answering about acme, each carrying an
// email address where the real record can, to prove none is read.
func records(t *testing.T, c *config.Company) gather.Sources {
	expiry, failed := now.Add(3*24*time.Hour), now.Add(-time.Hour)
	return gather.Sources{
		Gitlet: &fakeGitlet{page: 10},
		Integrations: fakeIntegrations{
			{Kind: integration.KindCloudflare, Endpoint: "https://hooks.acme.example", Outcome: integration.OutcomeBlocked,
				LastAttemptAt: now.Add(-time.Hour), RequestedBy: integration.Requester{Name: "ops@acme.example"}},
		},
		SeatAccess: fakeLedger{integration.KindCloudflare: {
			"sre": {Kind: integration.KindCloudflare, Handle: "sre", Name: "sre@acme.example",
				Account: &seataccess.Account{ID: "u-1", Name: "sre@acme.example"},
				Lanes: map[seataccess.Lane]seataccess.LaneRecord{"account": {
					Observed:   seataccess.Observed{Exists: true, Enabled: true, Level: "edit", Account: "sre@acme.example"},
					ObservedAt: now.Add(-time.Hour)}}},
		}},
		ToolServers: fakeTools{
			listings: []coord.MCPListing{{Server: "tickets", Node: "n0", At: now.Add(-time.Minute), ToolCount: 12,
				Info: &coord.MCPServerInfo{Name: "Tickets", Version: "2.4.1"}, Instructions: "atlas-instructions-canary",
				Tools: []coord.MCPListedTool{{Name: "search", Description: "atlas-description-canary"}}}},
			grants: []coord.MCPGrant{{Server: "tickets", Issuer: "https://auth.tickets.example", Expiry: &expiry,
				By: "ops@acme.example"}},
		},
		Nodes: fakeNodes{{
			MCP: []coord.MCPServerStatus{{Server: "files", Failed: 1, DownSince: failed, Error: "atlas-error-canary"}},
			Models: []coord.ModelStatus{{Key: "main", LastAnswer: now.Add(-2 * time.Hour),
				LastFailure: &coord.ModelFailure{At: failed, Said: "atlas-said-canary"},
				HostLogin:   &coord.HostLogin{Account: "owner@acme.example"}}},
		}},
		ServerTests: fakeTests{
			{Host: "db-1", HostDigest: hostDigest(t, c, "db-1"), At: failed, OK: false, By: "ops@acme.example"},
			{Host: "web-1", HostDigest: "a-digest-of-settings-since-changed", At: failed, OK: false},
		},
		AccessLog: fakeAccess{use: []accessdomain.HostUse{
			{Host: "web-1", Handle: "sre", Week: 4, Month: 9, Last: now.Add(-3 * time.Hour)},
		}},
		Usage: fakeUsage{
			spend: []usage.SpendRow{{Day: "2026-11-10", Handle: "sre", Tokens: usage.Tokens{ProviderKey: "main", Calls: 7}}},
			calls: map[string][]usage.ServerToolCallsRow{"tickets": {{Tool: "search", Handle: "sre", Calls: 5,
				LastAt: now.Add(-time.Hour)}}},
		},
	}
}

func read(t *testing.T, c *config.Company, src gather.Sources) gather.Answer {
	t.Helper()
	return gather.Read(context.Background(), src, gather.Request{Company: c, Revision: "r1", Asker: asker, Now: now})
}

func signalsOf(e atlas.Entry) []atlas.SignalKind {
	var out []atlas.SignalKind
	for _, s := range e.Signals {
		out = append(out, s.Kind)
	}
	return out
}

// WHAT THE ENGINE'S RECORDS SAY JOINS THE ENTRY IT IS ABOUT: an integration's
// inbound address and standing on its account, the agents holding accounts
// there, a tool server's own name, version and sign-in, the health the nodes
// beat, a server's current test, and each entry's use.
func TestRecordsJoinTheEntryTheyAreAbout(t *testing.T) {
	t.Parallel()
	c := company()
	a := read(t, c, records(t, c))
	find := func(k atlas.Kind, key string) atlas.Entry {
		t.Helper()
		e, ok := a.View.Find(atlas.MustRef(k, key).ID())
		if !ok {
			t.Fatalf("%s %s is not in the atlas", k, key)
		}
		return e
	}
	value := func(e atlas.Entry, f atlas.Fact) string {
		for _, v := range e.Facts {
			if v.Fact == f {
				return v.Value.Text
			}
		}
		return ""
	}
	cf := find(atlas.KindAccount, "cloudflare/0123456789abcdef0123456789abcdef")
	if value(cf, atlas.FactInboundURL) != "https://hooks.acme.example" {
		t.Errorf("Cloudflare's inbound address: %+v", cf.Facts)
	}
	if got := cf.Signals; len(got) != 1 || got[0].Kind != atlas.SignalIntegrationBlocked || got[0].Audience != atlas.AudienceAdmins {
		t.Errorf("Cloudflare's signals: %+v, want one for administrators", got)
	}
	if len(cf.Holders) != 1 || cf.Holders[0].Seat != "sre" || cf.Holders[0].Level != "edit" {
		t.Errorf("the agents holding a Cloudflare account: %+v", cf.Holders)
	}
	tickets := find(atlas.KindToolServer, "mcp/tickets")
	for f, want := range map[atlas.Fact]string{atlas.FactServerName: "Tickets", atlas.FactServerVersion: "2.4.1",
		atlas.FactToolCount: "12", atlas.FactOAuthIssuer: "https://auth.tickets.example"} {
		if got := value(tickets, f); got != want {
			t.Errorf("the tickets server's %s is %q, want %q", f, got, want)
		}
	}
	if got := signalsOf(tickets); !slices.Equal(got, []atlas.SignalKind{atlas.SignalSignInExpiring}) {
		t.Errorf("a sign-in lapsing in three days: %v", got)
	}
	if len(tickets.Use) != 1 || tickets.Use[0].Month != 5 || tickets.NotUsedRecently {
		t.Errorf("the tickets server's use: %+v, quiet %v", tickets.Use, tickets.NotUsedRecently)
	}
	files := find(atlas.KindToolServer, "mcp/files")
	if got := signalsOf(files); !slices.Equal(got, []atlas.SignalKind{atlas.SignalToolServerDown}) || !files.NotUsedRecently {
		t.Errorf("a tool server with a failed instance and no calls: %v, quiet %v", got, files.NotUsedRecently)
	}
	model := find(atlas.KindModel, "llm/main")
	if got := signalsOf(model); !slices.Equal(got, []atlas.SignalKind{atlas.SignalModelFailing}) {
		t.Errorf("a model whose last call failed after its last answer: %v", got)
	}
	if len(model.Use) != 1 || model.Use[0].Week != 7 {
		t.Errorf("the model's use: %+v", model.Use)
	}
	db, web := find(atlas.KindHost, "ssh/db-1"), find(atlas.KindHost, "ssh/web-1")
	if got := signalsOf(db); !slices.Equal(got, []atlas.SignalKind{atlas.SignalServerTestFailed}) {
		t.Errorf("a server whose current test failed: %v", got)
	}
	if len(web.Signals) != 0 {
		t.Errorf("a test of settings since changed raised %v", signalsOf(web))
	}
	if web.NotUsedRecently || !db.NotUsedRecently || web.Use[0].Seats[0] != "sre" {
		t.Errorf("the servers' use: web %+v, db quiet %v", web.Use, db.NotUsedRecently)
	}
	for _, cov := range a.Coverage {
		if !cov.Complete {
			t.Errorf("%s was not read in full: %s", cov.Source, cov.Reason)
		}
	}
}

// AN EMAIL ADDRESS IN A RECORD IS NEVER READ: a vendor account's name, the
// account an agent's lane observed, who signed a tool server in, who ran a
// test, a model plan's sign-in account and who asked for an integration all
// carry one, and none reaches the atlas. Nor does a tool server's
// instructions, a tool's description or an error a node beat.
func TestAnEmailAddressInARecordIsNeverRead(t *testing.T) {
	t.Parallel()
	c := company()
	out, err := json.Marshal(read(t, c, records(t, c)))
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"@acme.example", "atlas-instructions-canary", "atlas-description-canary",
		"atlas-error-canary", "atlas-said-canary"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("%q reached the atlas", leak)
		}
	}
}

// A USE THAT WAS NOT READ IS UNKNOWN, NOT ZERO: with the usage history
// unreadable no tool server or model is said to be unused, and the read says
// which source it could not read.
func TestAUseThatWasNotReadIsUnknown(t *testing.T) {
	t.Parallel()
	c := company()
	src := records(t, c)
	src.Usage = fakeUsage{err: errors.New("estate unavailable")}
	src.AccessLog = fakeAccess{err: errors.New("estate unavailable")}
	a := read(t, c, src)
	for _, e := range a.View.Entries {
		if e.NotUsedRecently {
			t.Errorf("%s is said to be unused with its use unread", e.Ref)
		}
	}
	for _, name := range []gather.SourceName{gather.SourceUsage, gather.SourceAccessLog} {
		i := slices.IndexFunc(a.Coverage, func(c gather.Coverage) bool { return c.Source == name })
		if i < 0 || a.Coverage[i].Complete || !strings.Contains(a.Coverage[i].Reason, "estate unavailable") {
			t.Errorf("an unread %s: %+v", name, a.Coverage)
		}
	}
}

// BenchmarkRead is the full read of a realistic company: 100 agent seats, 15
// integrations each holding an account for every seat, 50 servers, 30 tool
// servers, 10 models, 300 Gitlet repositories each with 4 checks, and a month
// of use on every channel.
func BenchmarkRead(b *testing.B) {
	c, src := realistic(b)
	req := gather.Request{Company: c, Revision: "r1", Asker: asker, Now: now}
	b.ReportAllocs()
	var entries int
	for b.Loop() {
		a := gather.Read(context.Background(), src, req)
		entries = len(a.View.Entries)
	}
	b.ReportMetric(float64(entries), "entries")
}

func realistic(tb testing.TB) (*config.Company, gather.Sources) {
	tb.Helper()
	c := company()
	c.Integrations.SSH.Hosts = nil
	for i := range 50 {
		c.Integrations.SSH.Hosts = append(c.Integrations.SSH.Hosts,
			config.SSHHost{ID: fmt.Sprintf("srv-%02d", i), Address: fmt.Sprintf("10.0.%d.%d", i/250, i%250+1)})
	}
	c.MCPServers = nil
	for i := range 30 {
		c.MCPServers = append(c.MCPServers, config.MCPServer{Name: fmt.Sprintf("tools-%02d", i),
			Transport: config.TransportHTTP, URL: fmt.Sprintf("https://mcp-%02d.tools.example/mcp", i)})
	}
	c.Providers.LLM = map[string]config.LLMProvider{}
	for i := range 10 {
		c.Providers.LLM[fmt.Sprintf("model-%d", i)] = config.LLMProvider{Harness: config.HarnessClaudeCode, Model: "claude-opus-5-5"}
	}
	seats := make([]string, 100)
	for i := range seats {
		seats[i] = fmt.Sprintf("seat-%03d", i)
	}
	ledger := fakeLedger{}
	for _, k := range integration.Kinds {
		ledger[k] = map[string]seataccess.Record{}
		for _, s := range seats {
			ledger[k][s] = seataccess.Record{Kind: k, Handle: s, Lanes: map[seataccess.Lane]seataccess.LaneRecord{
				"account": {Observed: seataccess.Observed{Exists: true, Enabled: true, Level: "read"}, ObservedAt: now}}}
		}
	}
	var tools fakeTools
	var health []coord.MCPServerStatus
	calls := map[string][]usage.ServerToolCallsRow{}
	for _, m := range c.MCPServers {
		tools.listings = append(tools.listings, coord.MCPListing{Server: m.Name, Node: "n0", At: now, ToolCount: 20,
			Info: &coord.MCPServerInfo{Name: m.Name, Version: "1.0.0"}})
		health = append(health, coord.MCPServerStatus{Server: m.Name, Started: 3})
		for _, s := range seats[:40] {
			calls[m.Name] = append(calls[m.Name], usage.ServerToolCallsRow{Tool: "search", Handle: s, Calls: 30, LastAt: now})
		}
	}
	var hostUse []accessdomain.HostUse
	for _, h := range c.Integrations.SSH.Hosts {
		for _, s := range seats[:20] {
			hostUse = append(hostUse, accessdomain.HostUse{Host: h.ID, Handle: s, Week: 3, Month: 12, Last: now})
		}
	}
	var spend []usage.SpendRow
	for day := range 30 {
		label := now.AddDate(0, 0, -day).Format(time.DateOnly)
		for _, s := range seats {
			spend = append(spend, usage.SpendRow{Day: label, Handle: s,
				Tokens: usage.Tokens{ProviderKey: fmt.Sprintf("model-%d", day%10), Calls: 40}})
		}
	}
	repos := make([]code.RepoSummary, 0, 300)
	for i := range 300 {
		r := repo(fmt.Sprintf("repo-%03d", i))
		r.LastPushAt = now.Add(-time.Duration(i) * time.Hour)
		repos = append(repos, r)
	}
	gitlet := &fakeGitlet{repos: repos, page: 500, checksPerRepo: 4}
	return c, gather.Sources{Gitlet: gitlet, Integrations: fakeIntegrations{}, SeatAccess: ledger, ToolServers: tools,
		Nodes: fakeNodes{{MCP: health}}, ServerTests: fakeTests{}, AccessLog: fakeAccess{use: hostUse},
		Usage: fakeUsage{spend: spend, calls: calls}}
}

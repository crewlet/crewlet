package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGrowth is a growth block as a product's would be: a connection's secret,
// a shared consent client's secret, and caps.
type fakeGrowth struct {
	Enabled     bool               `json:"enabled"`
	Approvers   []string           `json:"approvers"`
	Connections []fakeConnection   `json:"connections"`
	OAuth       *fakeOAuth         `json:"oauth"`
	Spend       *GrowthSpend       `json:"spend"`
	Notes       map[string]string  `json:"notes"`
	Other       []fakeGrowthHeader `json:"other"`
}

type fakeConnection struct {
	ID    string `json:"id"`
	Token string `json:"token" secret:"true"`
}

type fakeOAuth struct {
	ClientSecret string `json:"client_secret" secret:"true" growthshare:"google-oauth-client"`
}

type fakeGrowthHeader struct {
	Value string `json:"value"`
}

func (f *fakeGrowth) GrowthKind() string        { return "fake_ads" }
func (f *fakeGrowth) GrowthEnabled() bool       { return f.Enabled }
func (f *fakeGrowth) GrowthApprovers() []string { return f.Approvers }
func (f *fakeGrowth) GrowthSpend() *GrowthSpend { return f.Spend }

// docRoot is a document with two growth blocks, a server and a box's env.
type docRoot struct {
	Integrations struct {
		Ads   *fakeGrowth `json:"ads"`
		Sense *fakeGrowth `json:"sense"`
	} `json:"integrations"`
	Servers []struct {
		Headers map[string]string `json:"headers"`
	} `json:"mcp_servers"`
	Env map[string]string `json:"env"`
}

func problemPaths(err error) []string {
	var out []string
	for _, e := range unwrapAll(err) {
		var f *Fault
		if errors.As(e, &f) {
			out = append(out, f.Path.String())
		}
	}
	return out
}

func unwrapAll(err error) []error {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, unwrapAll(e)...)
		}
		return out
	}
	if err == nil {
		return nil
	}
	return []error{err}
}

// A growth credential's variable is named at its own field alone: a server's
// header, a box's env or a second block naming it is refused at that
// position, and one consent client's secret may be shared by the blocks of
// its group.
//
// Mutation: skip the walk of other positions, or let any two blocks share a
// variable, and a pasted ads token is bound to a vendor's MCP server.
func TestAGrowthCredentialIsNamedAtItsOwnFieldAlone(t *testing.T) {
	t.Parallel()
	var root docRoot
	root.Integrations.Ads = &fakeGrowth{Enabled: true,
		Connections: []fakeConnection{{ID: "eu", Token: "${ADS_TOKEN}"}},
		OAuth:       &fakeOAuth{ClientSecret: "${GOOGLE_CLIENT_SECRET}"}}
	root.Integrations.Sense = &fakeGrowth{OAuth: &fakeOAuth{ClientSecret: "${GOOGLE_CLIENT_SECRET}"},
		Other: []fakeGrowthHeader{{Value: "Bearer ${ADS_TOKEN}"}}}
	root.Servers = append(root.Servers, struct {
		Headers map[string]string `json:"headers"`
	}{Headers: map[string]string{"Authorization": "Bearer ${ADS_TOKEN}"}})
	root.Env = map[string]string{"SAFE": "${UNRELATED}", "LEAK": "x${ADS_TOKEN}"}
	blocks := []growthAt{{path: field("integrations.ads"), block: root.Integrations.Ads},
		{path: field("integrations.sense"), block: root.Integrations.Sense}}
	var p problems
	checkGrowthSecrets(&p, blocks, &root)
	got := strings.Join(problemPaths(p.err()), " ")
	for _, want := range []string{"mcp_servers[0].headers.Authorization", "env.LEAK", "integrations.sense.other[0].value"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s naming the growth credential was not refused: %s", want, got)
		}
	}
	for _, kept := range []string{"integrations.ads.connections[0].token", "client_secret", "env.SAFE"} {
		if strings.Contains(got, kept) {
			t.Errorf("%s was refused: %s", kept, got)
		}
	}
	root.Integrations.Sense.OAuth = nil
	root.Integrations.Sense.Connections = []fakeConnection{{ID: "x", Token: "${ADS_TOKEN}"}}
	var again problems
	checkGrowthSecrets(&again, blocks, &root)
	if !strings.Contains(strings.Join(problemPaths(again.err()), " "), "integrations.sense.connections[0].token") {
		t.Errorf("two blocks share a credential outside a share group: %v", again.err())
	}
}

// A vendor's own MCP server is refused while its product is on, matched on
// its command or its address, and left alone while the product is off or the
// address is only alike.
//
// Mutation: match a host by substring, or refuse while the product is off,
// and a look-alike host is refused or a stored document stops validating.
func TestAVendorsOwnServerIsRefusedBesideItsProduct(t *testing.T) {
	t.Parallel()
	servers := []MCPServer{
		{Command: "uvx", Args: []string{"google-ads-mcp"}},
		{Transport: TransportHTTP, URL: "https://mcp.facebook.com/ads"},
		{Transport: TransportHTTP, URL: "https://mcp.facebook.com.example.net/ads"},
		{Transport: TransportHTTP, URL: "https://mcp.facebook.com/other"},
		{Command: "npx", Args: []string{"-y", "@vendor/analytics-mcp"}},
	}
	on := map[string]bool{"google_ads": true, "meta_ads": true}
	var p problems
	checkRefused(&p, servers, func(kind string) bool { return on[kind] })
	got := strings.Join(problemPaths(p.err()), " ")
	if got != "mcp_servers[0] mcp_servers[1]" {
		t.Errorf("refused %q, want the Google Ads and Meta servers alone", got)
	}
}

// Caps are amounts in a currency, each account once, per_change within daily,
// and a cap that does not parse is unset, which refuses spend.
//
// Mutation: parse an amount as a float, or read a bad amount as zero caps
// set, and spend passes on a cap nobody wrote.
func TestGrowthCapsAreAmountsInTheAccountsCurrency(t *testing.T) {
	t.Parallel()
	s := &GrowthSpend{Accounts: []GrowthCap{
		{Account: "act_1", Currency: "EUR", PerChange: "50.00", Daily: "200.00"},
		{Account: "act_1", Currency: "eur", PerChange: "300", Daily: "200"},
		{Account: "", Daily: "1e3"},
	}}
	var p problems
	s.validate(&p, field("integrations.ads.spend"))
	got := strings.Join(problemPaths(p.err()), " ")
	for _, want := range []string{"accounts[1].account", "accounts[1].currency", "accounts[1].per_change",
		"accounts[2].account", "accounts[2].daily"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was not refused: %s", want, got)
		}
	}
	caps := s.Accounts[0].caps()
	if caps.PerChangeMicros == nil || *caps.PerChangeMicros != 50_000_000 || *caps.DailyMicros != 200_000_000 {
		t.Errorf("caps read %+v", caps)
	}
	if c := s.Accounts[2].caps(); c.DailyMicros != nil {
		t.Error("a daily cap that does not parse reads as set")
	}
}

// A connection's id is a short lowercase name, each once; a growth seat level
// is one of the three.
func TestGrowthConnectionIDsAndLevels(t *testing.T) {
	t.Parallel()
	var p problems
	checkGrowthConnections(&p, field("integrations.ads.connections"), []string{"eu", "eu", "Team A", ""})
	got := strings.Join(problemPaths(p.err()), " ")
	for _, want := range []string{"connections[1].id", "connections[2].id", "connections[3].id"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was not refused: %s", want, got)
		}
	}
	if strings.Contains(got, "connections[0]") {
		t.Errorf("a good id was refused: %s", got)
	}
	if growthLevelProblem("") != "" || growthLevelProblem("administer") != "" || growthLevelProblem("admin") == "" {
		t.Error("a growth level is misjudged")
	}
}

// EVERY DOCUMENT STORED TODAY STILL VALIDATES: the shipped example and every
// golden company, each with every integration block it holds, pass the growth
// rules, which add nothing to a document with no growth block.
//
// Mutation: make validateGrowth refuse a document with no growth block (an
// approvers list required, say), and a stored company stops booting.
func TestTodaysStoredDocumentsStillValidate(t *testing.T) {
	t.Parallel()
	docs := map[string][]byte{"examples/nimbus.company.yaml": readRepoFile(t, "examples", "nimbus.company.yaml")}
	entries, err := os.ReadDir(filepath.Join("testdata", "derived"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".company.yaml") {
			data, err := os.ReadFile(filepath.Join("testdata", "derived", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			docs[e.Name()] = data
		}
	}
	for name, data := range docs {
		cfg, err := ParseCompany(data)
		if err != nil {
			t.Errorf("%s no longer validates: %v", name, err)
			continue
		}
		if err := cfg.validateGrowth(); err != nil {
			t.Errorf("%s is refused by the growth rules: %v", name, err)
		}
		if len(cfg.Integrations.growthBlocks()) != 0 {
			t.Errorf("%s holds a growth block today", name)
		}
	}
}

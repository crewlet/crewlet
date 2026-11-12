package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// reachCompany is acme with two servers, web-1 on a public address and db-1
// on a private one, and an agent, sre, whose SSH access reaches web-1 alone;
// a host name on a private network; and a deployment in a declared zone.
func reachCompany(t *testing.T) (builtin.AtlasSettings, *org.Organization) {
	t.Helper()
	on := true
	c := &config.Company{Name: "acme"}
	c.Integrations.SSH = &config.SSH{Enabled: true,
		Provisioning: &config.SSHProvisioning{CertificateAuthority: "${SSH_CA}"},
		Hosts:        []config.SSHHost{{ID: "web-1", Address: "9.9.9.9"}, {ID: "db-1", Address: "10.0.0.5"}}}
	c.Providers.LLM = map[string]config.LLMProvider{"zulu": {Harness: config.HarnessClaudeCode, Model: "claude-opus-5-5"}}
	c.Roles = []config.Role{
		{Name: "SRE", Handle: "sre", LLM: config.PhaseLLM{Default: config.ProviderKeys{"zulu"}},
			Integrations: config.RoleIntegrations{SSH: &config.RoleSSH{Enabled: &on, Hosts: []string{"web-1"}}}},
		{Name: "Dev", Handle: "dev", LLM: config.PhaseLLM{Default: config.ProviderKeys{"zulu"}}},
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	return builtin.AtlasSettings{Company: c, Revision: "r1"}, o
}

// atlasTools is the operator catalogue's three atlas tools over settings,
// by name.
func atlasTools(t *testing.T, settings builtin.AtlasSettings) map[string]tools.SeatCallable {
	t.Helper()
	deps := codeDeps(newGitletHost())
	deps.Actor = builtin.PrincipalActor
	out := map[string]tools.SeatCallable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Code: deps, Authorize: builtin.Decide(chartLeads),
		Atlas: builtin.AtlasDeps{Settings: func(context.Context) (builtin.AtlasSettings, bool) { return settings, true }},
	}) {
		switch tool.Name() {
		case builtin.AtlasSearchTool, builtin.AtlasGetTool, builtin.AtlasTraceTool:
			out[tool.Name()] = tool.(tools.SeatCallable)
		}
	}
	if len(out) != 3 {
		t.Fatalf("the operator catalogue holds %d atlas tools, want three", len(out))
	}
	return out
}

func callAtlas(ctx context.Context, t *testing.T, tool tools.SeatCallable, turn *turnctx.Turn,
	args map[string]any) map[string]any {
	t.Helper()
	got, err := tool.CallForTurn(ctx, turn, args)
	if err != nil || got.Failed {
		t.Fatalf("%s %v = %v %q", tool.Name(), args, err, got.Output)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.Output), &out); err != nil {
		t.Fatalf("%s answered no JSON object: %v\n%s", tool.Name(), err, got.Output)
	}
	return out
}

// AN AGENT READS A PRIVATE ADDRESS ONLY WITHIN ITS OWN REACH (ADR-0109): sre
// reaches web-1, so it reads web-1 whole, and db-1, which its access does not
// reach, without its address; the answer counts what it left out. A person
// holding the atlas reads both addresses.
//
// Mutation: answer every entry as within reach, and db-1's address is read.
func TestAnAgentReadsAPrivateAddressOnlyWithinItsReach(t *testing.T) {
	t.Parallel()
	settings, o := reachCompany(t)
	get := atlasTools(t, settings)[builtin.AtlasGetTool]
	agent := iam.WithPrincipal(context.Background(), turnctx.SeatPrincipal(o, o.AgentSeatByHandle("sre")))
	turn := &turnctx.Turn{RunID: "run-1", Seat: o.AgentSeatByHandle("sre"), Org: o}
	db := callAtlas(agent, t, get, turn, map[string]any{"name": "db-1"})
	if raw, _ := json.Marshal(db); strings.Contains(string(raw), "10.0.0.5") {
		t.Errorf("an agent outside db-1's reach read its address: %s", raw)
	}
	if db["private_withheld"] != float64(1) {
		t.Errorf("the answer counts %v private values left out, want 1", db["private_withheld"])
	}
	web := callAtlas(agent, t, get, turn, map[string]any{"name": "web-1"})
	if raw, _ := json.Marshal(web); !strings.Contains(string(raw), "9.9.9.9") {
		t.Errorf("an agent lost a server its own access reaches: %s", raw)
	}
	person := atlasAs(iam.GrantAtlas)
	if raw, _ := json.Marshal(callAtlas(person, t, get, nil, map[string]any{"name": "db-1"})); !strings.Contains(
		string(raw), "10.0.0.5") {
		t.Errorf("a person holding the atlas does not read db-1's address: %s", raw)
	}
}

// AN ATLAS ANSWER SAYS WHO WROTE WHAT IT CARRIES: its envelope names the
// writers at the least trust any of them earns, so a model reading an entry
// knows it is text somebody else wrote.
func TestAnAtlasAnswerSaysWhoWroteWhatItCarries(t *testing.T) {
	t.Parallel()
	settings, _ := reachCompany(t)
	get := atlasTools(t, settings)[builtin.AtlasGetTool]
	answer := callAtlas(atlasAs(iam.GrantAtlas), t, get, nil, map[string]any{"name": "web-1"})
	envelope, ok := answer["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("atlas_get answered without a provenance envelope: %v", answer)
	}
	if raw, _ := json.Marshal(envelope); !strings.Contains(string(raw), "operator") {
		t.Errorf("the envelope does not name the settings' writer: %s", raw)
	}
}

// ONE ENTRY IS NAMED BY ITS ID OR ITS EXACT NAME, and a trace walks from it:
// what depends on the zone is the host name in it. A name nothing answers to,
// and an id that is no entry's, are refused.
func TestAnAtlasToolNamesOneEntryAndTracesFromIt(t *testing.T) {
	t.Parallel()
	settings, _ := reachCompany(t)
	settings.Company.Integrations.Cloudflare = &config.Cloudflare{Enabled: true,
		Provisioning: &config.CloudflareProvisioning{AccountID: "0123456789abcdef0123456789abcdef",
			Zones: []string{"acme.example"}}}
	settings.Deployment.APIAddress = "https://crewlet.acme.example"
	tools := atlasTools(t, settings)
	person := atlasAs(iam.GrantAtlas)
	zone := atlas.MustRef(atlas.KindDomain, "acme.example").ID().String()
	byID := callAtlas(person, t, tools[builtin.AtlasGetTool], nil, map[string]any{"id": zone})
	if e, _ := byID["entry"].(map[string]any); e == nil || e["id"] != zone {
		t.Fatalf("atlas_get by id: %v", byID)
	}
	trace := callAtlas(person, t, tools[builtin.AtlasTraceTool], nil, map[string]any{"name": "acme.example"})
	hops, _ := trace["hops"].([]any)
	if len(hops) != 1 || !strings.Contains(mustJSON(t, hops), "crewlet.acme.example") {
		t.Errorf("what depends on the zone: %s", mustJSON(t, trace))
	}
	for _, args := range []map[string]any{{"name": "nothing-by-this-name"}, {"id": uuid.NewString()}, {"id": "zone"}, {}} {
		if got, _ := tools[builtin.AtlasGetTool].CallForTurn(person, nil, args); !got.Failed {
			t.Errorf("atlas_get %v answered: %s", args, got.Output)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

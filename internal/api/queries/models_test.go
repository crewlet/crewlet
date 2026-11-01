package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/modelcatalog"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// modelsCompany is two models: default, which pm runs on by fallback and qa
// names, and codex-review, which qa names for its reviewing.
const modelsCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: claude-opus-5-5
    codex-review:
      harness: codex
      model: gpt-6.1-sol
roles:
  - name: PM
    handle: pm
  - name: QA
    handle: qa
    llm: default
    llm_review: codex-review
`

// fakeModels is the engine's half: what this node resolved, and its alarms.
type fakeModels struct {
	local  []queries.LocalModel
	alarms []statelog.Alarm
}

func (f fakeModels) LocalModels() []queries.LocalModel { return f.local }
func (f fakeModels) LatestAlarms() []statelog.Alarm    { return f.alarms }

type fakeSecretRows []secrets.Record

func (f fakeSecretRows) List(context.Context) ([]secrets.Record, error) { return f, nil }

type fakeDevices []coord.DeviceSignIn

func (f fakeDevices) ListDeviceSignIns(context.Context) ([]coord.DeviceSignIn, error) {
	return f, nil
}

// modelSources is the fixture company over a lease table, with this node's
// default signing in with a stored token and codex-review signed out.
func modelSources(t *testing.T, backend coord.Backend) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(modelsCompany))
	if err != nil {
		t.Fatal(err)
	}
	return queries.Sources{Coord: backend, Company: companySource(t, cfg), NodeID: "node-a",
		Models: fakeModels{
			local: []queries.LocalModel{
				{Key: "default", Route: "token", Variable: "CLAUDE_CODE_OAUTH_TOKEN",
					Resolves: true, Hint: "sk-ant-oat01-…Qm4x",
					TokenLifetime: 365 * 24 * time.Hour},
				{Key: "codex-review", Route: "none"},
			},
			alarms: []statelog.Alarm{
				{Kind: statelog.KindLLMSignInRefused, Detail: "codex-review was refused"},
				{Kind: statelog.KindApplyLag, Detail: "not a model's"},
			},
		},
		Secrets: fakeSecretRows{{Name: "CLAUDE_CODE_OAUTH_TOKEN", UpdatedBy: "miles",
			UpdatedAt: pinned.Add(-360 * 24 * time.Hour)}},
		DeviceSignIns: fakeDevices{},
	}
}

func agentIDOf(t *testing.T, s queries.Sources, handle string) uuid.UUID {
	t.Helper()
	_, organization := s.Company()
	id, ok := organization.AgentIDFor(organization.AgentSeatByHandle(handle))
	if !ok {
		t.Fatalf("no seat %s", handle)
	}
	return id
}

func entryOf(t *testing.T, answer queries.ModelEntriesAnswer, key string) queries.ModelEntry {
	t.Helper()
	for _, e := range answer.Entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry %s in %+v", key, answer.Entries)
	return queries.ModelEntry{}
}

// A MODEL'S STATE IS THE FLEET'S HEARTBEATS, JOINED.
//
// Each node publishes what it knows of each model on its presence lease; the
// answer is the join: the latest sit-out window and every node sitting it out,
// the latest answer and the latest failure (named by its node), and the seats
// parked on it, by handle. A node whose beat carried no report is a column of
// its own, never a row of zeros.
func TestModelStateIsTheFleetsHeartbeats(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	s := modelSources(t, backend)
	qa := agentIDOf(t, s, "qa")
	soon, later := pinned.Add(time.Hour), pinned.Add(3*time.Hour)
	presentNode(t, backend, "node-a", &coord.NodeStatus{Models: []coord.ModelStatus{
		{Key: "default", Plan: "3f9a1c0e22b1", SignIn: "token", Resolves: true,
			LastAnswer: pinned.Add(-time.Hour),
			SittingOut: &coord.ModelSitOut{Until: soon, Since: pinned.Add(-time.Minute),
				Window: "session", Said: "You've hit your session limit"}},
		{Key: "codex-review", SignIn: "none",
			LastFailure: &coord.ModelFailure{Kind: "auth", At: pinned.Add(-2 * time.Hour),
				Said: "Not logged in"},
			HostLogin: &coord.HostLogin{State: coord.HostSignedOut, CheckedAt: pinned}},
	}})
	presentNode(t, backend, "node-b", &coord.NodeStatus{Models: []coord.ModelStatus{
		{Key: "default", Plan: "3f9a1c0e22b1", SignIn: "token", Resolves: true,
			LastAnswer: pinned.Add(-10 * time.Minute),
			SittingOut: &coord.ModelSitOut{Until: later, Since: pinned.Add(-time.Minute),
				Window: "weekly", ResetsAt: later},
			Parked: []coord.ParkedSeat{{Seat: qa, Since: pinned.Add(-20 * time.Minute)}}},
	}})
	presentNode(t, backend, "node-c", nil)

	got, ok := answer(t, s, "llm_entries", nil).(queries.ModelEntriesAnswer)
	if !ok {
		t.Fatalf("llm_entries answered %T", got)
	}
	if got.Reading != queries.ReadingAdmin || got.Node != "node-a" {
		t.Errorf("reading %q node %q", got.Reading, got.Node)
	}
	if want := []queries.ModelNode{{"node-a", true}, {"node-b", true}, {"node-c", false}}; !slices.Equal(got.Nodes, want) {
		t.Errorf("nodes = %v, want %v", got.Nodes, want)
	}
	def := entryOf(t, got, "default")
	if def.Health != queries.HealthSittingOut || def.SittingOut == nil {
		t.Fatalf("default = %+v, want sitting out", def)
	}
	if !def.SittingOut.Until.Equal(later) || def.SittingOut.Window != "weekly" ||
		!slices.Equal(def.SittingOut.Nodes, []string{"node-a", "node-b"}) {
		t.Errorf("sitting_out = %+v, want the latest window and both nodes", def.SittingOut)
	}
	if def.LastSuccess == nil || !def.LastSuccess.Equal(pinned.Add(-10*time.Minute)) {
		t.Errorf("last_success = %v, want node-b's later answer", def.LastSuccess)
	}
	if def.Waiting == nil || !slices.Equal(def.Waiting.Seats, []string{"qa"}) {
		t.Errorf("waiting = %+v, want qa by its handle", def.Waiting)
	}
	if def.Plan != "3f9a1c0e22b1" {
		t.Errorf("plan = %q", def.Plan)
	}
	codex := entryOf(t, got, "codex-review")
	if codex.Health != queries.HealthNoSignIn || codex.LastFailure == nil ||
		codex.LastFailure.Node != "node-a" || codex.LastFailure.Said != "Not logged in" {
		t.Errorf("codex-review = %+v", codex)
	}
	if len(codex.SignIn.HostLogins) != 1 || codex.SignIn.HostLogins[0].State != "signed_out" ||
		codex.SignIn.HostLogins[0].Command != "crewlet llm login codex-review" {
		t.Errorf("host_logins = %+v", codex.SignIn.HostLogins)
	}
	// THIS NODE'S LATEST ALARMS, the model kinds alone.
	if len(got.Alarms) != 1 || got.Alarms[0].Kind != statelog.KindLLMSignInRefused {
		t.Errorf("alarms = %+v, want the one model alarm", got.Alarms)
	}
}

// A STORED TOKEN IS DATED BY ITS ROW, AND ITS EXPIRY IS COMPUTED AND SAYS SO.
//
// The token is opaque, so its expiry is when it was stored plus the lifetime
// the vendor documents, and the answer names that basis; inside the warning
// window the model reads `expiring`.
func TestAStoredTokensExpiryIsComputedFromItsRow(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{Models: []coord.ModelStatus{
		{Key: "default", SignIn: "token", Resolves: true, LastAnswer: pinned.Add(-time.Hour)},
	}})
	got := answer(t, modelSources(t, backend), "llm_entries", nil).(queries.ModelEntriesAnswer)
	def := entryOf(t, got, "default")
	in := def.SignIn
	if in.Variable == nil || *in.Variable != "CLAUDE_CODE_OAUTH_TOKEN" || in.Source != "store" ||
		in.StoredBy == nil || *in.StoredBy != "miles" || in.Hint == nil {
		t.Fatalf("sign_in = %+v", in)
	}
	if in.ExpiresAt == nil || !in.ExpiresAt.Equal(pinned.Add(5*24*time.Hour)) ||
		in.ExpiryBasis == nil || *in.ExpiryBasis != "stored_at+365d" {
		t.Errorf("expiry = %v basis %v, want stored_at + 365 days, said so", in.ExpiresAt, in.ExpiryBasis)
	}
	if def.Health != queries.HealthExpiring {
		t.Errorf("health = %s, want expiring five days out", def.Health)
	}
	if in.Mode != "subscription" {
		t.Errorf("mode = %q", in.Mode)
	}
}

// WHO USES A MODEL IS THE ENGINE'S OWN RESOLUTION.
//
// pm writes no model, so every job of pm's runs on `default` by fallback;
// qa writes default for its executor and codex-review for its reviewing.
func TestAModelsUsersAreTheEnginesResolution(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{})
	got := answer(t, modelSources(t, backend), "llm_entries", nil).(queries.ModelEntriesAnswer)

	uses := func(key string) []string {
		var out []string
		for _, u := range entryOf(t, got, key).UsedBy {
			out = append(out, u.Seat+"/"+u.Job+"@"+string(rune('0'+u.Position))+":"+u.Source)
		}
		return out
	}
	if got := uses("codex-review"); !slices.Equal(got, []string{"qa/review@0:seat"}) {
		t.Errorf("codex-review is used by %v", got)
	}
	def := uses("default")
	for _, want := range []string{"pm/executor@0:default-entry", "pm/review@0:default-entry",
		"qa/executor@0:seat", "qa/subagent@0:executor"} {
		if !slices.Contains(def, want) {
			t.Errorf("default's users %v lack %s", def, want)
		}
	}
	for _, u := range entryOf(t, got, "default").UsedBy {
		if u.Seat == "pm" && (u.Written == nil || *u.Written) {
			t.Errorf("pm's %s reads as written: %+v", u.Job, u)
		}
	}
}

// A MEMBER READS EVERY MODEL'S CONDITION, AND NOTHING THAT NAMES A CREDENTIAL.
//
// Decision D7: the plan, every sign-in field but the route and whether it
// resolves, the vendor's sentence, the last failure, the usage and the alarms
// are dropped, and the answer says which reading it is.
func TestAMemberReadsNoCredentialAccountOrSentence(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{Models: []coord.ModelStatus{
		{Key: "default", Plan: "3f9a1c0e22b1", SignIn: "token", Resolves: true,
			SittingOut: &coord.ModelSitOut{Until: pinned.Add(time.Hour), Window: "session",
				Said: "You've hit your session limit"},
			LastFailure: &coord.ModelFailure{Kind: "rate_limit", At: pinned, Said: "limit"}},
	}})
	s := modelSources(t, backend)
	s.Now = func() time.Time { return pinned }
	r := queries.NewRegistry()
	queries.Register(r, s)
	member := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "jane",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantWorklet}})
	data, err := r.Answer(member, "llm_entries", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	body := string(raw)
	for _, leak := range []string{"3f9a1c0e22b1", "CLAUDE_CODE_OAUTH_TOKEN", "Qm4x", "miles",
		"session limit", `"said"`, `"usage"`, `"plan"`, `"last_failure"`, `"hint"`, `"variable"`,
		`"stored_at"`, "cost_usd", `"nodes":["`} {
		if strings.Contains(body, leak) {
			t.Errorf("a member's reading carries %s: %s", leak, body)
		}
	}
	out := asMap(t, data)
	if out["reading"] != "member" || out["alarms"] != nil {
		t.Errorf("reading %v alarms %v, want member and null", out["reading"], out["alarms"])
	}
	for _, want := range []string{`"health":"sitting_out"`, `"route":"token"`, `"window":"session"`,
		`"used_by"`} {
		if !strings.Contains(body, want) {
			t.Errorf("a member's reading lacks %s: %s", want, body)
		}
	}
}

// A MODEL'S HEALTH IS THE FIRST READING THAT HOLDS, in the order a person has
// to act on them.
func TestAModelsHealthIsTheFirstThatHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		row  coord.ModelStatus
		want queries.ModelHealth
	}{
		{"untested", coord.ModelStatus{Key: "default", SignIn: "token", Resolves: true},
			queries.HealthUntested},
		{"ready", coord.ModelStatus{Key: "default", SignIn: "token", Resolves: true,
			LastAnswer: pinned}, queries.HealthReady},
		{"rejected after its last answer", coord.ModelStatus{Key: "default", SignIn: "token",
			Resolves: true, LastAnswer: pinned.Add(-time.Hour),
			LastFailure: &coord.ModelFailure{Kind: "auth", At: pinned}}, queries.HealthRejected},
		{"an older refusal than its answer", coord.ModelStatus{Key: "default", SignIn: "token",
			Resolves: true, LastAnswer: pinned,
			LastFailure: &coord.ModelFailure{Kind: "auth", At: pinned.Add(-time.Hour)}},
			queries.HealthReady},
		{"a sit-out over everything", coord.ModelStatus{Key: "default", SignIn: "token",
			Resolves: true, LastFailure: &coord.ModelFailure{Kind: "auth", At: pinned},
			SittingOut: &coord.ModelSitOut{Until: pinned.Add(time.Minute)}}, queries.HealthSittingOut},
		{"a window that ended", coord.ModelStatus{Key: "default", SignIn: "token",
			Resolves: true, LastAnswer: pinned,
			SittingOut: &coord.ModelSitOut{Until: pinned.Add(-time.Minute)}}, queries.HealthReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := coordmemory.New()
			presentNode(t, backend, "node-a", &coord.NodeStatus{Models: []coord.ModelStatus{tc.row}})
			s := modelSources(t, backend)
			// NO STORED TOKEN, so the expiry plays no part here.
			s.Secrets = fakeSecretRows{}
			got := answer(t, s, "llm_entries", nil).(queries.ModelEntriesAnswer)
			if h := entryOf(t, got, "default").Health; h != tc.want {
				t.Errorf("health = %s, want %s", h, tc.want)
			}
		})
	}
}

// EVERY RUNNER CELL ON EVERY NODE, and whether the company declares it.
func TestRunnersAreEachNodesProbe(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	presentNode(t, backend, "node-a", &coord.NodeStatus{Runners: &coord.RunnerStatus{
		Direct:    coord.RunnerCell{State: coord.RunnerReady, Detail: "Landlock", CheckedAt: pinned},
		Container: coord.RunnerCell{State: coord.RunnerNotReady, Detail: "no docker", Fix: "Install Docker"},
		E2B:       coord.RunnerCell{State: coord.RunnerUnconfigured},
	}})
	presentNode(t, backend, "node-b", nil)
	got := answer(t, modelSources(t, backend), "llm_runners", nil).(queries.RunnersAnswer)
	if got.DefaultRunIn != "direct" {
		t.Errorf("default_run_in = %q", got.DefaultRunIn)
	}
	if c := got.Cells["direct"]; !c.Declared || !slices.Equal(c.ReadyOn, []string{"node-a"}) {
		t.Errorf("direct = %+v", c)
	}
	if c := got.Cells["container"]; !slices.Equal(c.NotReadyOn, []string{"node-a"}) {
		t.Errorf("container = %+v", c)
	}
	if len(got.Nodes) != 2 || !got.Nodes[0].Reported || got.Nodes[1].Reported ||
		got.Nodes[0].Container.Fix == nil || *got.Nodes[0].Container.Fix != "Install Docker" {
		t.Errorf("nodes = %+v", got.Nodes)
	}
}

type fakeCatalogue struct{}

func (fakeCatalogue) Provider(_ context.Context, provider string) (modelcatalog.Answer, error) {
	if provider == "nowhere" {
		return modelcatalog.Answer{}, modelcatalog.ErrUnknownProvider
	}
	return modelcatalog.Answer{Source: modelcatalog.SourceModelsDev, FetchedAt: pinned,
		Models: []modelcatalog.Model{{ID: "m-1", Name: "Model One"}}}, nil
}

// A MODEL STEP IS OFFERED THE CATALOGUE'S MODELS, THE HARNESS'S OWN ALIASES AND
// THE VENDOR'S PAGE, and OpenCode its provider's models in its own grammar.
func TestAModelStepIsOfferedTheCataloguesModels(t *testing.T) {
	t.Parallel()
	s := queries.Sources{Catalogue: fakeCatalogue{}}
	got := answer(t, s, "llm_models", map[string]any{"harness": "claude-code"}).(queries.ModelsAnswer)
	if got.Provider != "anthropic" || got.Source != "models.dev" || len(got.Models) != 1 ||
		got.DocsURL == "" || !slices.ContainsFunc(got.Aliases, func(a queries.ModelAliasRow) bool {
		return a.ID == "sonnet"
	}) {
		t.Errorf("claude-code = %+v", got)
	}
	got = answer(t, s, "llm_models", map[string]any{"harness": "opencode",
		"provider": "openrouter"}).(queries.ModelsAnswer)
	if got.Models[0].ID != "openrouter/m-1" {
		t.Errorf("opencode's model id = %q, want <provider>/<id>", got.Models[0].ID)
	}

	r := queries.NewRegistry()
	queries.Register(r, s)
	for _, params := range []map[string]any{{"harness": "gemini-cli"}, {"harness": "opencode"}, {}} {
		if _, err := r.Answer(everyGrant(t), "llm_models", params); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%v = %v, want bad params", params, err)
		}
	}
	if _, err := r.Answer(everyGrant(t), "llm_models", map[string]any{"harness": "opencode",
		"provider": "nowhere"}); !errors.Is(err, queries.ErrUnknownProvider) {
		t.Errorf("an unknown provider = %v", err)
	}
}

// A DEVICE SIGN-IN IS READ BY ITS ID WHILE ITS RECORD LINGERS, and its
// approval names the revision it re-activated.
func TestADeviceSignInIsReadUntilItLapses(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	s := queries.Sources{DeviceSignIns: fakeDevices{{ID: id, Entry: "codex-review", Node: "node-a",
		Status: coord.DeviceApproved, StartedAt: pinned.Add(-5 * time.Minute),
		ExpiresAt: pinned.Add(10 * time.Minute), RevisionID: "r-9", Epoch: 42}}}
	got := asMap(t, answer(t, s, "llm_device_sign_in", map[string]any{"id": id.String()}))
	if got["status"] != "approved" || got["entry"] != "codex-review" {
		t.Errorf("answer = %v", got)
	}
	if reload, _ := got["reload"].(map[string]any); reload["revision_id"] != "r-9" || reload["epoch"] != float64(42) {
		t.Errorf("reload = %v", got["reload"])
	}
	s.Now = func() time.Time { return pinned.Add(time.Hour) }
	r := queries.NewRegistry()
	queries.Register(r, s)
	if _, err := r.Answer(everyGrant(t), "llm_device_sign_in", map[string]any{"id": id.String()}); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a lapsed record = %v, want not found", err)
	}
}

// THE ADMIN'S QUESTIONS ARE ADMIN'S, AND THE CONDITION IS EVERYBODY'S.
func TestTheModelQuestionsTakeTheirGrants(t *testing.T) {
	t.Parallel()
	s := modelSources(t, coordmemory.New())
	s.Catalogue = fakeCatalogue{}
	r := queries.NewRegistry()
	queries.Register(r, s)
	for _, what := range []string{"llm_runners", "llm_models", "llm_device_sign_in"} {
		requiresExactly(t, r, what, iam.GrantAdmin)
	}
	if !r.PostureOf("llm_entries").IsEverybody() {
		t.Error("llm_entries is not everybody's")
	}
}

// THE MODELS ANSWER CARRIES NO MONEY, at any depth.
//
// The dashboard renders tokens and never a price (dashboard-design rule 19),
// and the Models screen is this answer's one reader: a `cost_usd` here is a
// figure the screen may not show, and a partial one, since one backend quotes
// a price and the rest do not. Usage is calls and tokens.
func TestTheModelsAnswerCarriesNoMoney(t *testing.T) {
	t.Parallel()
	s := modelSources(t, coordmemory.New())
	r := queries.NewRegistry()
	queries.Register(r, s)
	admin := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "ops",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour)})
	data, err := r.Answer(admin, "llm_entries", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch node := v.(type) {
		case map[string]any:
			for key, child := range node {
				lower := strings.ToLower(key)
				for _, money := range []string{"cost", "usd", "price", "priced", "spend", "dollar"} {
					if strings.Contains(lower, money) {
						t.Errorf("the models answer carries a money field at %s.%s", path, key)
					}
				}
				walk(path+"."+key, child)
			}
		case []any:
			for i, child := range node {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("$", tree)
	if !strings.Contains(string(raw), `"total_tokens"`) {
		t.Errorf("the answer lost its token usage: %s", raw)
	}
}

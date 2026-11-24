package configapi_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// modelSecretsDoc is a company whose models sign in every way a model stores a
// credential: default with a subscription token and a login bundle, work with a
// named API key (which the search server reads too), and spare and other with
// the API key both fall back to.
const modelSecretsDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm_order: [default, work, spare, other]
  llm:
    default:
      model: claude-opus-5-5
    work:
      model: sonnet
      auth: {mode: api-key, api_key: "${WORK_KEY}"}
    spare:
      model: haiku
      auth: {mode: api-key}
    other:
      model: haiku
      auth: {mode: api-key}
mcp_servers:
  - name: search
    command: search-mcp
    env: {KEY: "${WORK_KEY}"}
    tools: {default: allow}
roles:
  - name: PM
    handle: pm
    llm: spare
`

// modelValues are the values the store holds for modelSecretsDoc: a fake
// each, which no answer may carry. default's bundle is not stored.
var modelValues = map[string]string{
	"CLAUDE_CODE_OAUTH_TOKEN": "fake-token-0001", "ANTHROPIC_API_KEY": "fake-key-0002",
	"WORK_KEY": "fake-work-0003", "CREWLET_LLM_CLI_WORK_CREDENTIALS": "fake-bundle-0004",
	"CREWLET_LLM_CLI_SPARE_CREDENTIALS": "fake-bundle-0005",
}

// modelSecretsSurface is a surface running modelSecretsDoc with its values stored.
func modelSecretsSurface(t *testing.T) *surface {
	t.Helper()
	s := newSurface(t)
	s.seed(t, modelSecretsDoc)
	for name, value := range modelValues {
		if err := s.secrets.Set(t.Context(), name, value, secrets.Author{Name: "test", Kind: string(iam.ActorOperator)},
			secrets.SourceModels, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// removeModelHolding is removeModel, refusing an answer that carries a value.
func removeModelHolding(t *testing.T, s *surface, key, query, body string) (int, map[string]any) {
	t.Helper()
	res := s.do(t, http.MethodDelete, "/config/llm-providers/"+key+query, body, nil)
	for _, value := range modelValues {
		if strings.Contains(res.Body.String(), value) {
			t.Fatalf("the answer carries a stored value: %s", res.Body)
		}
	}
	return res.Code, decode(t, res)
}

// A MODEL'S REMOVAL NAMES EVERY VALUE ITS CREDENTIALS READ: the ones nothing
// reads after it, which it may delete, and the ones something it leaves still
// reads, kept with where. A fallback the entry signs in from without naming it
// counts, as `/secrets` counts it. The references read and the dry run answer
// one list.
//
// Mutation: list only the ${VAR}s the entry names, and spare's and default's
// values are missed; drop the kept list, and ANTHROPIC_API_KEY vanishes unsaid.
func TestAModelRemovalNamesEveryValueItsCredentialsRead(t *testing.T) {
	t.Parallel()
	s := modelSecretsSurface(t)
	for key, want := range map[string]string{
		"default": `{"secrets":["CLAUDE_CODE_OAUTH_TOKEN"],"kept_secrets":[]}`,
		"work": `{"secrets":["CREWLET_LLM_CLI_WORK_CREDENTIALS"],` +
			`"kept_secrets":[{"name":"WORK_KEY","read_by":["mcp_servers[0].env.KEY"]}]}`,
		"spare": `{"secrets":["CREWLET_LLM_CLI_SPARE_CREDENTIALS"],` +
			`"kept_secrets":[{"name":"ANTHROPIC_API_KEY","read_by":["providers.llm.other.auth.api_key"]}]}`,
	} {
		res := s.do(t, http.MethodGet, "/config/llm-providers/"+key+"/references", "", nil)
		read := decode(t, res)
		if res.Code != http.StatusOK {
			t.Fatalf("%s's references = %d %v", key, res.Code, read)
		}
		if got := jsonOf(t, read["dependants"]); got != normal(t, want) {
			t.Errorf("%s's references read dependants\n got %s\nwant %s", key, got, normal(t, want))
		}
		body := `{"repoint": [{"at": {"seat": "pm", "job": "executor"}, "to": "other"}]}`
		if key != "spare" {
			body = ""
		}
		code, checked := removeModelHolding(t, s, key, "?dry_run=true", body)
		if code != http.StatusOK {
			t.Fatalf("%s's dry run = %d %v", key, code, checked)
		}
		if got := jsonOf(t, checked["dependants"]); got != normal(t, want) {
			t.Errorf("%s's dry run dependants\n got %s\nwant %s", key, got, normal(t, want))
		}
	}
}

// A MODEL'S REMOVAL DELETES ITS OWN VALUES AND KEEPS SHARED ONES.
//
// Asked to delete spare's bundle and the API key it falls back to, the removal
// deletes the bundle once the revision is written, and keeps the key, which
// other still signs in with, saying where it is read. The model is gone in
// the same revision that repoints pm.
//
// Mutation: delete what the request names without asking the fleet's revision
// again, and other is signed out.
func TestAModelRemovalDeletesItsOwnValuesAndKeepsSharedOnes(t *testing.T) {
	t.Parallel()
	s := modelSecretsSurface(t)
	code, body := removeModelHolding(t, s, "spare", "", `{
		"repoint": [{"at": {"seat": "pm", "job": "executor"}, "to": "other"}],
		"delete_secrets": ["CREWLET_LLM_CLI_SPARE_CREDENTIALS", "ANTHROPIC_API_KEY"],
		"_summary": "remove spare"}`)
	if code != http.StatusCreated {
		t.Fatalf("the removal = %d %v", code, body)
	}
	want := `[{"name":"CREWLET_LLM_CLI_SPARE_CREDENTIALS","outcome":"deleted"},` +
		`{"name":"ANTHROPIC_API_KEY","outcome":"kept","read_by":["providers.llm.other.auth.api_key"]}]`
	if got := jsonOf(t, body["secrets"]); got != want {
		t.Errorf("secrets:\n got %s\nwant %s", got, want)
	}
	for name, held := range map[string]bool{"CREWLET_LLM_CLI_SPARE_CREDENTIALS": false,
		"ANTHROPIC_API_KEY": true, "WORK_KEY": true} {
		if s.stored(t, name) != held {
			t.Errorf("%s stored = %v, want %v", name, !held, held)
		}
	}
	if _, still := activeCompany(t, s).Providers.LLM["spare"]; still {
		t.Error("spare is still configured")
	}

	// NOTHING NAMED, NOTHING DELETED, and the answer still lists the outcomes.
	code, body = removeModelHolding(t, s, "default", "", `{"_summary": "remove default"}`)
	if code != http.StatusCreated || jsonOf(t, body["secrets"]) != `[]` {
		t.Fatalf("a removal deleting nothing = %d %v, want 201 with secrets []", code, body)
	}
	if !s.stored(t, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("a removal that named no value deleted one")
	}
}

// A MODEL'S REMOVAL DELETES ONLY A VALUE ITS ENTRY READ: naming another
// model's key is refused at its index, with nothing written or deleted.
//
// Mutation: drop the check, and removing spare deletes work's key.
func TestAModelRemovalDeletesOnlyAValueItsEntryRead(t *testing.T) {
	t.Parallel()
	s := modelSecretsSurface(t)
	before, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"?dry_run=true", ""} {
		code, body := removeModelHolding(t, s, "default", query,
			`{"delete_secrets": ["CLAUDE_CODE_OAUTH_TOKEN", "WORK_KEY"], "_summary": "remove default"}`)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("deleting work's key with default (%q) = %d %v, want 422", query, code, body)
		}
		problems, _ := body["problems"].([]any)
		if len(problems) != 1 {
			t.Fatalf("problems = %v, want WORK_KEY's alone", problems)
		}
		problem := problems[0].(map[string]any)
		if jsonOf(t, problem["segments"]) != `["delete_secrets",1]` || problem["kind"] != "unknown_value" ||
			!strings.Contains(problem["message"].(string), `WORK_KEY is not read by the model "default"`) {
			t.Errorf("WORK_KEY's problem = %v", problem)
		}
	}
	if after, _, _ := s.active(t.Context()); after.ID != before.ID {
		t.Error("a refused removal stored a revision")
	}
	if !s.stored(t, "WORK_KEY") || !s.stored(t, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("a refused removal deleted a value")
	}
	for _, bad := range []string{`{"delete_secrets": ["A", "A"]}`, `{"delete_secrets": [""]}`} {
		if code, body := removeModelHolding(t, s, "default", "?dry_run=true", bad); code != http.StatusBadRequest ||
			body["error"] != "invalid_body" {
			t.Errorf("%s = %d %v, want 400 invalid_body", bad, code, body)
		}
	}
}

// DELETING A MODEL'S VALUES ASKS `secrets.write` ON TOP OF `config.write`.
//
// The Dashboard Assistant may take `config.write` on a person's approval and
// never `secrets.write`: a removal naming a value is refused before anything
// is written, its dry run included, and the same removal naming none lands.
//
// Mutation: drop the admission from the shared path, and the assistant
// deletes the model's token.
func TestDeletingAModelsValuesAsksSecretsWrite(t *testing.T) {
	t.Parallel()
	s := modelSecretsSurface(t)
	assistant := iam.Principal{ID: uuid.New(), Login: "ana.admin", Kind: iam.KindPerson,
		Seat: "ana", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: time.Now().Add(time.Hour),
		Via:      iam.AssistName(uuid.Must(uuid.NewV7())), AssistScope: iam.AssistScopeExec}
	s.caller = &assistant
	before, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	deleting := `{"delete_secrets": ["CLAUDE_CODE_OAUTH_TOKEN"], "_summary": "remove default"}`
	for _, query := range []string{"?dry_run=true", ""} {
		code, body := removeModelHolding(t, s, "default", query, deleting)
		if code != http.StatusForbidden || body["reason"] != string(authz.ReasonAssistRefused) {
			t.Errorf("the assistant deleting a value (%q) = %d %v, want 403 for the assistant",
				query, code, body)
		}
	}
	if after, _, _ := s.active(t.Context()); after.ID != before.ID || !s.stored(t, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatal("a refused removal wrote a revision or deleted a value")
	}
	if code, body := removeModelHolding(t, s, "default", "", `{"_summary": "remove default"}`); code != http.StatusCreated {
		t.Errorf("the assistant removing the model alone = %d %v, want 201", code, body)
	}
}

// A MODEL VALUE THE STORE DID NOT DELETE IS SAID beside a removal that
// happened: 201, the value `failed`, the model gone and the value stored.
func TestAModelValueTheStoreDidNotDeleteIsSaid(t *testing.T) {
	t.Parallel()
	s := newSurfaceWith(t, func(o *configapi.Options) { o.Secrets = failingUnset{o.Secrets} })
	s.seed(t, modelSecretsDoc)
	if err := s.secrets.Set(t.Context(), "CLAUDE_CODE_OAUTH_TOKEN", modelValues["CLAUDE_CODE_OAUTH_TOKEN"],
		secrets.Author{Name: "test", Kind: string(iam.ActorOperator)}, secrets.SourceModels, time.Now()); err != nil {
		t.Fatal(err)
	}
	code, body := removeModelHolding(t, s, "default", "",
		`{"delete_secrets": ["CLAUDE_CODE_OAUTH_TOKEN"], "_summary": "remove default"}`)
	if code != http.StatusCreated {
		t.Fatalf("the removal = %d %v, want 201", code, body)
	}
	if got := jsonOf(t, body["secrets"]); got != `[{"name":"CLAUDE_CODE_OAUTH_TOKEN","outcome":"failed"}]` {
		t.Errorf("secrets = %s, want the token failed", got)
	}
	if _, still := activeCompany(t, s).Providers.LLM["default"]; still || !s.stored(t, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Error("want the model removed and the value still stored")
	}
}

// THE REMOVE DIALOGS READ WHAT A REMOVAL SAYS OF ITS VALUES.
//
// Settings › Models & Fleets and Settings › MCP & tools both draw the values a removal
// takes from `dependants` (a dry run's, and the models' references read) and
// what became of each from a write's `secrets`, typed once in
// `contract/config.ts`. Every row both routes send is held to its declaration
// both ways, and the outcomes to the engine's own.
//
// Mutation: drop `read_by` from KeptSecret, or an outcome from SecretOutcome,
// and this is red.
func TestTheRemoveDialogsReadWhatARemovalSaysOfItsValues(t *testing.T) {
	t.Parallel()
	models, servers := modelSecretsSurface(t), serverSurface(t)
	res := models.do(t, http.MethodGet, "/config/llm-providers/work/references", "", nil)
	read := decode(t, res)
	_, modelChecked := removeModelHolding(t, models, "work", "?dry_run=true", "")
	_, serverChecked := removeServer(t, servers, "linear", "?dry_run=true", `{"clear": true}`, nil)
	_, modelWritten := removeModelHolding(t, models, "work", "",
		`{"delete_secrets": ["CREWLET_LLM_CLI_WORK_CREDENTIALS", "WORK_KEY"], "_summary": "remove work"}`)
	_, serverWritten := removeServer(t, servers, "linear", "",
		`{"clear": true, "delete_secrets": ["LINEAR_ORG"], "_summary": "remove linear"}`, nil)

	object := func(v any) map[string]any {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("expected an object, the answer sent %T", v)
		}
		return m
	}
	rows := func(v any) []map[string]any {
		t.Helper()
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			t.Fatalf("expected a list with rows, the answer sent %v", v)
		}
		out := make([]map[string]any, 0, len(list))
		for _, row := range list {
			out = append(out, object(row))
		}
		return out
	}
	values := []map[string]any{object(read["dependants"]), object(modelChecked["dependants"])}
	var kept []map[string]any
	for _, d := range append(slices.Clone(values), object(serverChecked["dependants"])) {
		kept = append(kept, rows(d["kept_secrets"])...)
	}
	for iface, sent := range map[string][]map[string]any{
		"RemovalSecrets": values,
		"KeptSecret":     kept,
		"SecretDeletion": slices.Concat(rows(modelWritten["secrets"]), rows(serverWritten["secrets"])),
	} {
		holdDeclared(t, iface, sent)
	}

	got, err := clientsource.Union(clientsource.Tree(t), "SecretOutcome")
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	want := []string{string(configapi.SecretDeleted), string(configapi.SecretAbsent),
		string(configapi.SecretKept), string(configapi.SecretFailed)}
	if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("the dashboard's SecretOutcome is %v; the engine sends %v", got, want)
	}
}

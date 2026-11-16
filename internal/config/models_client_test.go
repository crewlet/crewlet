package config_test

import (
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// THE MODELS SCREEN READS AN ENTRY AS THE ENGINE DOES: the auth modes its
// editor and its words are keyed on are exactly the engine's closed set, and
// what it says an unset field means is the default the engine applies — so a
// page never names claude-code, subscription or ten minutes for an entry the
// engine runs some other way. And the harnesses it says can run a seat's executor
// are exactly the ones config holds to the in-box runners, so the screen never
// tells an operator an entry runs executors that every launch skips. The jobs
// and the places a run happens are the engine's closed sets, and the field the
// page writes for each seat job is the one the engine reports that job at, so
// an assignment made on the page is the one a seat runs on. And the name its
// wizard lets a person give a model is one the key grammar takes, and no other.
func TestTheModelsScreenReadsAnEntryAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"HarnessAuthMode", strs(config.HarnessAuthModes)},
		{"ModelJob", strs(config.ProviderJobs)},
		{"RunnerCellName", strs(config.Placements)},
	} {
		got, err := clientsource.Union(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(tc.want))) {
			t.Errorf("contract/models.ts %s is %v, want exactly the engine's %v", tc.name, got, tc.want)
		}
	}
	seatJobFields(t, tree)
	body, err := clientsource.Literal(tree, "EXECUTOR_HARNESSES")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := clientsource.Strings(body), strs(config.ExecutorHarnesses); !slices.Equal(
		slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("contract/models.ts EXECUTOR_HARNESSES is %v, want exactly the engine's "+
			"executor harnesses %v", got, want)
	}
	var unset config.LLMProvider
	for _, tc := range []struct{ name, want string }{
		{"DEFAULT_HARNESS", unset.HarnessName()},
		{"DEFAULT_HARNESS_AUTH_MODE", string(unset.ResolvedAuthMode())},
		{"DEFAULT_HARNESS_TIMEOUT_SECONDS", strconv.FormatFloat(unset.Timeout(), 'f', -1, 64)},
		{"DEFAULT_HARNESS_MAX_CONCURRENT", strconv.Itoa(unset.Concurrency())},
		{"MODEL_KEY_PATTERN", config.ProviderKeyPattern().String()},
	} {
		got, err := clientsource.Scalar(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("contract/models.ts %s is %q, and the engine's is %q", tc.name, got, tc.want)
		}
	}
}

// EVERY BUILT-IN HARNESS HAS A NAME A PERSON READS. A seat's condition names the
// harness its entry drives ("Claude Code subscription limit reached"), and a
// profile the table lacked would be shown by its machine name, the one word a
// person reading the Inbox has never seen. Exactly the built-in profiles, so a
// name left behind for a profile removed is caught too, bar `custom`: an entry
// on it describes a harness of its own, which the dashboard calls by the entry's
// key, the only name it has.
func TestEveryBuiltInHarnessHasAProductName(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "HARNESS_PRODUCTS")
	if err != nil {
		t.Fatal(err)
	}
	got, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(harnessprofile.BuiltinNames(), func(name string) bool {
		return name == string(config.HarnessCustom)
	})
	if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("contract/models.ts HARNESS_PRODUCTS names %v, want exactly the built-in profiles %v",
			slices.Sorted(slices.Values(got)), want)
	}
}

// seatJobFields holds SEAT_JOB_FIELDS to the fields
// [config.Company.ProviderReferences] reports for a seat that writes its
// executor as a scalar `llm` and every satellite flat, the shape the page
// writes: each job's field, and exactly the seat jobs.
func seatJobFields(t *testing.T, tree string) {
	t.Helper()
	body, err := clientsource.Literal(tree, "SEAT_JOB_FIELDS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseCompanyDocument([]byte(`
name: Acme
providers:
  llm:
    big: {model: sonnet}
roles:
  - name: Flat
    handle: flat
    llm: big
    llm_review: big
    llm_subagent: big
    llm_auxiliary: big
    llm_judge: big
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{}
	for _, ref := range cfg.ProviderReferences("big") {
		last, _ := ref.Segments[len(ref.Segments)-1].(string)
		want[string(ref.Job)] = last
	}
	if !slices.Equal(slices.Sorted(slices.Values(keys)), slices.Sorted(maps.Keys(want))) {
		t.Errorf("contract/models.ts SEAT_JOB_FIELDS keys %v, want exactly the seat jobs %v",
			slices.Sorted(slices.Values(keys)), slices.Sorted(maps.Keys(want)))
	}
	// Each value is one string, so the values read in order pair with the
	// keys read in order.
	values := clientsource.Strings(body)
	if len(values) != len(keys) {
		t.Fatalf("contract/models.ts SEAT_JOB_FIELDS holds %d strings for %d jobs: each job's value "+
			"is its one field name", len(values), len(keys))
	}
	for i, job := range keys {
		if want[job] != values[i] {
			t.Errorf("contract/models.ts SEAT_JOB_FIELDS.%s is %q, the engine reports the job at %q",
				job, values[i], want[job])
		}
	}
}

// THE MODELS SCREEN KNOWS EVERY HARNESS AS ITS PROFILE READS: the sign-ins, key
// variable, login, web and Ask Crewlet rules, and effort levels HARNESS_FACTS
// gives each built-in harness are its profile's, so the wizard offers nothing the engine refuses.
func TestTheModelsScreenKnowsEveryHarnessAsItsProfileReads(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "HARNESS_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(harnessprofile.BuiltinNames(), func(name string) bool {
		return name == string(config.HarnessCustom)
	})
	if !slices.Equal(slices.Sorted(slices.Values(keys)), want) {
		t.Fatalf("HARNESS_FACTS names %v, want exactly the built-in profiles %v", keys, want)
	}
	one := func(facts clientsource.Body, name string) string {
		values := clientsource.Scalars(facts, name)
		if len(values) == 0 {
			return ""
		}
		return values[0]
	}
	for _, name := range keys {
		p, err := harnessprofile.Load(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := clientsource.Property(body, name)
		if err != nil {
			t.Fatal(err)
		}
		var signIns []string
		if p.APIKeyEnv != "" || p.EnvSignIn {
			signIns = append(signIns, "api-key")
		}
		if p.KeepsLogin() {
			signIns = append(signIns, "host-login")
		}
		if p.TokenEnv != "" {
			signIns = append(signIns, "token")
		}
		if len(p.DeviceLoginArgs) > 0 {
			signIns = append(signIns, "device")
		}
		if len(p.SignInEnv) > 0 {
			signIns = append(signIns, "cloud")
		}
		listed, err := clientsource.Property(facts, "signIns")
		if err != nil {
			t.Fatal(err)
		}
		if got := clientsource.Strings(listed); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(signIns))) {
			t.Errorf("%s: signIns %v, want the engine's %v", name, got, signIns)
		}
		login := ""
		if p.KeepsLogin() {
			login = "adopt"
			if len(p.LoginArgs) > 0 {
				login = "command"
			}
		}
		for _, tc := range []struct{ field, got, want string }{
			{"binary", one(facts, "binary"), string(p.Binary)},
			{"keyEnv", one(facts, "keyEnv"), p.APIKeyEnv},
			{"keyShared", one(facts, "keyShared"), map[bool]string{true: "true"}[p.APIKeyEnvShared]},
			{"providerKeys", one(facts, "providerKeys"), map[bool]string{true: "true"}[p.EnvSignIn]},
			{"login", one(facts, "login"), login},
			{"effortNote", one(facts, "effortNote"), p.EffortNote},
			{"webDeniable", one(facts, "webDeniable"), map[bool]string{true: "true"}[slices.Contains(harnessprofile.WebDeniable(), name)]},
			{"assistant", one(facts, "assistant"), map[bool]string{true: "true"}[slices.Contains(harnessprofile.AssistantEligible(), name)]},
		} {
			if tc.got != tc.want {
				t.Errorf("%s: %s %q, want the profile's %q", name, tc.field, tc.got, tc.want)
			}
		}
		levels, _ := p.EffortLevels("")
		efforts, err := clientsource.Property(facts, "efforts")
		if err != nil {
			t.Fatal(err)
		}
		if got := clientsource.Strings(efforts); !slices.Equal(got, levels) {
			t.Errorf("%s: efforts %v, want the profile's %v", name, got, levels)
		}
		// Each rule as its strings in order: the match, then its levels.
		var wantRules, gotRules []string
		if p.Effort != nil {
			for _, rule := range p.Effort.Models {
				wantRules = append(append(wantRules, rule.Match), rule.Levels...)
			}
		}
		if models, err := clientsource.Property(facts, "effortModels"); err == nil {
			gotRules = clientsource.Strings(models)
		}
		if !slices.Equal(gotRules, wantRules) {
			t.Errorf("%s: effortModels %q, want the profile's %q", name, gotRules, wantRules)
		}
	}
}

// EVERY SIGN-IN THE MODELS WIZARD OFFERS WRITES AN ENTRY THE ENGINE TAKES: the
// shape draft.ts entityOf writes for each method, on every harness at its first
// effort level, passes every runnable and admission rule.
func TestEverySignInTheModelsWizardOffersIsOneTheEngineTakes(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "HARNESS_FACTS")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	// What entityOf writes for each method, as YAML flow.
	shapes := map[string]func(name string, p harnessprofile.Profile) string{
		"api-key": func(name string, p harnessprofile.Profile) string {
			if p.EnvSignIn {
				return `env: {OPENROUTER_API_KEY: "${OPENROUTER_API_KEY}"}`
			}
			out := `auth: {mode: api-key, api_key: "${MODEL_KEY}"}`
			if name == "qwen-code" {
				out += "\n      env: {OPENAI_BASE_URL: https://dashscope-intl.aliyuncs.com/compatible-mode/v1}"
			}
			return out
		},
		"host-login": func(string, harnessprofile.Profile) string { return "auth: {mode: subscription}" },
		"device":     func(string, harnessprofile.Profile) string { return "auth: {mode: subscription}" },
		"token": func(string, harnessprofile.Profile) string {
			return `auth: {mode: subscription, token: "${CLAUDE_CODE_OAUTH_TOKEN}"}`
		},
		"cloud": func(string, harnessprofile.Profile) string {
			return "auth: {mode: env}\n      env: {CLAUDE_CODE_USE_BEDROCK: \"1\", AWS_REGION: us-west-2, " +
				`AWS_BEARER_TOKEN_BEDROCK: "${AWS_BEARER_TOKEN_BEDROCK_M}"}`
		},
	}
	for _, name := range keys {
		p, err := harnessprofile.Load(name, nil)
		if err != nil {
			t.Fatal(err)
		}
		facts, err := clientsource.Property(body, name)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := clientsource.Property(facts, "signIns")
		if err != nil {
			t.Fatal(err)
		}
		effort := ""
		if levels, _ := p.EffortLevels(""); len(levels) > 0 {
			effort = "\n      effort: " + levels[0]
		}
		model := "m"
		if p.ModelNamesProvider {
			model = "openrouter/m"
		}
		for _, method := range clientsource.Strings(listed) {
			shape, ok := shapes[method]
			if !ok {
				t.Errorf("%s offers %q, which this gate has no entry shape for", name, method)
				continue
			}
			doc := "name: Acme\nproviders:\n  llm:\n    m:\n      harness: " + name +
				"\n      model: " + model + effort + "\n      " + shape(name, p) + "\n"
			if _, err := config.ParseCompany([]byte(doc)); err != nil {
				t.Errorf("%s by %s: the entry the wizard writes is refused:\n%s\n%v", name, method, doc, err)
			}
		}
	}
}

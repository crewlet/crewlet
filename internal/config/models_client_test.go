package config_test

import (
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// THE MODELS SCREEN READS AN ENTRY AS THE ENGINE DOES: the auth modes its
// editor and its words are keyed on are exactly the engine's closed set, and
// what it says an unset field means is the default the engine applies — so a
// page never names claude-code, subscription or ten minutes for an entry the
// engine runs some other way. And the CLIs it says can run a seat's executor
// are exactly the ones config holds to the in-box runners, so the screen never
// tells an operator an entry runs executors that every launch skips. The jobs
// and the places a run happens are the engine's closed sets, and the field the
// page writes for each seat job is the one the engine reports that job at, so
// an assignment made on the page is the one a seat runs on.
func TestTheModelsScreenReadsAnEntryAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"CLIAuthMode", strs(config.CLIAgentAuthModes)},
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
	body, err := clientsource.Literal(tree, "EXECUTOR_CLI_AGENTS")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := clientsource.Strings(body), strs(config.CodingAgents); !slices.Equal(
		slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("contract/models.ts EXECUTOR_CLI_AGENTS is %v, want exactly the engine's "+
			"executor CLIs %v", got, want)
	}
	var unset config.LLMProvider
	for _, tc := range []struct{ name, want string }{
		{"DEFAULT_CLI_AGENT", unset.AgentName()},
		{"DEFAULT_CLI_AUTH_MODE", string(unset.ResolvedAuthMode())},
		{"DEFAULT_CLI_TIMEOUT_SECONDS", strconv.FormatFloat(unset.Timeout(), 'f', -1, 64)},
		{"DEFAULT_CLI_MAX_CONCURRENT", strconv.Itoa(unset.Concurrency())},
	} {
		got, err := clientsource.Scalar(tree, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("contract/models.ts %s is %q, the engine's default is %q", tc.name, got, tc.want)
		}
	}
}

// EVERY BUILT-IN CLI HAS A NAME A PERSON READS. A seat's condition names the
// CLI its entry drives ("Claude Code subscription limit reached"), and a
// profile the table lacked would be shown by its machine name, the one word a
// person reading the Inbox has never seen. Exactly the built-in profiles, so a
// name left behind for a profile removed is caught too, bar `custom`: an entry
// on it describes a CLI of its own, which the dashboard calls by the entry's
// key, the only name it has.
func TestEveryBuiltInCLIHasAProductName(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "CLI_PRODUCTS")
	if err != nil {
		t.Fatal(err)
	}
	got, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.DeleteFunc(cliprofile.BuiltinNames(), func(name string) bool {
		return name == string(config.CLICustom)
	})
	if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("contract/models.ts CLI_PRODUCTS names %v, want exactly the built-in profiles %v",
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

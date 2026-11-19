package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// fakeSearchArg is on the fake's search-only argv alone, so the fake tells Ask
// Crewlet's search run from the entry's own calls.
const fakeSearchArg = "--fake-search-only"

// searchFixture is the stream of a measured Claude Code search run.
const searchFixture = "testdata/claude-websearch-2.1.325.jsonl"

// fakeSearchHarness plays a JSONL harness with a search-only run. On the search
// argv the search prompt is answered with the stream at FAKE_SEARCH_STREAM, or
// with no search where that is "none", and the fetch and shell probes with
// FAKE_SEARCH_FETCH and FAKE_SEARCH_SHELL. Other calls answer as the probe fake's.
func fakeSearchHarness() {
	prompt, _ := io.ReadAll(os.Stdin)
	text := string(prompt)
	search := slices.Contains(os.Args, fakeSearchArg)
	var answer string
	switch {
	case search && strings.Contains(text, "Search the web once"):
		if stream := os.Getenv("FAKE_SEARCH_STREAM"); stream != "none" {
			raw, err := os.ReadFile(stream) //nolint:gosec // the suite's own fixture
			if err != nil {
				fmt.Fprint(os.Stderr, "UNREADABLE: ", err)
				os.Exit(3)
			}
			fmt.Print(string(raw))
			return
		}
		answer = "I have no web search tool here."
	case strings.Contains(text, "read the file at "):
		answer = noLocalToolsReply
	case strings.Contains(text, webProbeURL) && search:
		answer = os.Getenv("FAKE_SEARCH_FETCH")
	case strings.Contains(text, webProbeURL):
		answer = os.Getenv("FAKE_WEB_PROBE_STDOUT")
	case strings.Contains(text, noLocalToolsReply) && search:
		answer = os.Getenv("FAKE_SEARCH_SHELL")
	case strings.Contains(text, noLocalToolsReply):
		answer = os.Getenv("FAKE_SHELL_PROBE_STDOUT")
	default:
		answer = os.Getenv("FAKE_STDOUT")
	}
	event, _ := json.Marshal(map[string]any{"type": "result", "result": answer})
	fmt.Println(string(event))
}

// searchProvider is a fake entry on a JSONL profile with a web_search block:
// its own calls fetch and refuse a shell, and its search run answers the
// measured stream and refuses both, each but where env says otherwise.
func searchProvider(t *testing.T, env map[string]string, overrides map[string]any) *Provider {
	t.Helper()
	vars, profile := searchEntry(t, env, overrides)
	return fakeProvider(t, vars, profile)
}

// searchEntry is [searchProvider]'s environment and profile overrides.
func searchEntry(t *testing.T, env map[string]string, overrides map[string]any) (map[string]string, map[string]any) {
	t.Helper()
	fixture, err := filepath.Abs(searchFixture)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"FAKE_SEARCH_STREAM":      fixture,
		"FAKE_STDOUT":             "ok",
		"FAKE_SHELL_PROBE_STDOUT": noLocalToolsReply,
		"FAKE_WEB_PROBE_STDOUT":   "ts=" + nowEpoch(),
		"FAKE_SEARCH_FETCH":       noWebReply,
		"FAKE_SEARCH_SHELL":       noLocalToolsReply,
	}
	maps.Copy(vars, env)
	profile := map[string]any{
		"local_tools":     string(harnessprofile.LocalToolsDenied),
		"version_args":    []any{"-test.run=NoSuchTest"},
		"output":          "jsonl",
		"event_type_path": []any{"type"},
		"text_events":     []any{"result"},
		"text_paths":      []any{[]any{"result"}},
		"web_search": map[string]any{
			"complete_args": []any{"-test.run=TestFakeHarness", fakeSearchArg},
			"args":          []any{[]any{fakeSearchArg}},
			"tool":          "WebSearch",
			"format":        string(harnessprofile.SearchClaudeStreamJSON),
			"note":          "measured on the fake",
		},
	}
	maps.Copy(profile, overrides)
	return vars, profile
}

// searchCalls are the recorded completions that ran the search-only argv.
func searchCalls(t *testing.T, rec string) []recordedCall {
	t.Helper()
	var out []recordedCall
	for _, call := range recordedEnv(t, rec) {
		if slices.Contains(call.Argv, fakeSearchArg) {
			out = append(out, call)
		}
	}
	return out
}

// The doctor runs the search run the engine builds, on the entry's login and
// state with slots of its own, and reports its results and refusals on the web
// line. Mutation: drop the search probe, run it on the entry's argv, or share p.slots.
func TestTheDoctorReportsTheSearchRunAnsweredAndFetchAndShellRefused(t *testing.T) {
	t.Parallel()
	rec := t.TempDir()
	p := searchProvider(t, map[string]string{"FAKE_RECORD_ENV": rec}, nil)
	d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: true})

	raw, err := os.ReadFile(searchFixture)
	if err != nil {
		t.Fatal(err)
	}
	reader := &searchReader{format: harnessprofile.SearchClaudeStreamJSON, tool: "WebSearch",
		calls: map[string]bool{}, seen: map[string]bool{}}
	for line := range strings.SplitSeq(string(raw), "\n") {
		reader.read(line)
	}
	want := fmt.Sprintf("allowed; ok: fetched %s; search run ok: %d results, fetch and shell refused",
		webProbeURL, len(reader.result()))
	if web, _ := d.Check(CheckWeb); d.Web != want || !web.OK {
		t.Errorf("web = %q (ok %v, remedy %q), want %q", d.Web, web.OK, web.Remedy, want)
	}
	calls := searchCalls(t, rec)
	if len(calls) != 3 {
		t.Fatalf("%d calls ran the search argv, want the search, fetch and shell probes", len(calls))
	}
	for _, call := range calls {
		home := envValue(strings.Join(call.Env, "\n"), "HOME")
		if !strings.HasPrefix(home, d.StateDir+string(os.PathSeparator)) {
			t.Errorf("a search-run call ran in %q, outside the entry's state dir %q", home, d.StateDir)
		}
	}
	switch run := p.searchRun(); {
	case run == nil:
		t.Fatal("an entry with web_search has no search run")
	case run.ws != p.ws || run.plans != p.plans || run.auth != p.auth:
		t.Error("the search run does not share the entry's workspace, sign-in record and auth")
	case run.slots == p.slots || cap(run.slots) != cap(p.slots):
		t.Error("the search run takes the entry's slots rather than its own")
	}
}

// A search run that answers no result, fetches or runs a shell is a problem on
// the web line naming web_search, beside the web probe's own. Mutation: drop a
// branch of searchProbe, or report the search's problem only where the web probe has none.
func TestASearchRunThatFallsShortIsAWebProblemNamingWebSearch(t *testing.T) {
	t.Parallel()
	fetched := "allowed; ok: fetched " + webProbeURL + "; "
	for name, tc := range map[string]struct {
		env      map[string]string
		web      string
		problems int
	}{
		"no result": {map[string]string{"FAKE_SEARCH_STREAM": "none"}, fetched + "search run: no results", 1},
		"fetched": {map[string]string{"FAKE_SEARCH_FETCH": "ts=" + nowEpoch()},
			fetched + "search run: FETCHED " + webProbeURL, 1},
		"shell ran": {map[string]string{"FAKE_SEARCH_SHELL": nowEpoch()}, fetched + "search run: SHELL RAN", 1},
		"beside a failed fetch": {map[string]string{"FAKE_SEARCH_STREAM": "none", "FAKE_WEB_PROBE_STDOUT": noWebReply},
			"allowed; failed; search run: no results", 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := searchProvider(t, tc.env, nil).Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
			web, _ := d.Check(CheckWeb)
			if d.Web != tc.web {
				t.Errorf("web = %q, want %q", d.Web, tc.web)
			}
			if web.OK || web.Kind != CheckKindConfig || len(web.problems) != tc.problems ||
				!strings.Contains(web.problems[len(web.problems)-1], "overrides.web_search") ||
				!strings.Contains(web.Remedy, "measured on the fake") {
				t.Errorf("web check ok %v kind %q, want %d config problems, the last naming web_search "+
					"and its note: %q", web.OK, web.Kind, tc.problems, web.problems)
			}
		})
	}
}

// A profile with no web_search runs no search probe: four completions, and a
// web line that says nothing of a search. Mutation: drop the CanSearchWeb gate.
func TestAProfileWithoutWebSearchRunsNoSearchProbe(t *testing.T) {
	t.Parallel()
	rec := t.TempDir()
	vars, profile := searchEntry(t, map[string]string{"FAKE_RECORD_ENV": rec}, nil)
	delete(profile, "web_search")
	d := fakeProvider(t, vars, profile).Diagnose(t.Context(), DiagnoseOptions{Smoke: true})
	if web, _ := d.Check(CheckWeb); !web.OK || strings.Contains(d.Web, "search") {
		t.Errorf("web = %q (remedy %q), want the web probe alone", d.Web, web.Remedy)
	}
	if calls := recordedEnv(t, rec); len(calls) != 4 {
		t.Errorf("recorded %d completions, want the smoke test and three isolation probes", len(calls))
	}
}

// The doctor's search run of an entry, web denied or not, is the profile the
// engine's search run loads. Mutation: leave complete_args alone in
// withWebSearch, and a denied entry's search run keeps the denial's argv.
func TestTheDoctorsSearchRunIsTheOneTheEngineLoads(t *testing.T) {
	t.Parallel()
	for _, name := range harnessprofile.BuiltinNames() {
		builtin, _ := harnessprofile.Builtin(name)
		if !builtin.CanSearchWeb() {
			continue
		}
		want, err := harnessprofile.Load(name, map[string]any{"web_tools": string(harnessprofile.WebToolsSearch)})
		if err != nil {
			t.Fatal(err)
		}
		for _, stance := range []harnessprofile.WebTools{harnessprofile.WebToolsAllowed, harnessprofile.WebToolsDenied} {
			entry, err := harnessprofile.Load(name, map[string]any{"web_tools": string(stance)})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := entry.SearchRun(); !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("%s, web %s: the doctor's search run is not the engine's:\n%+v\nwant\n%+v",
					name, stance, got, want)
			}
		}
	}
}

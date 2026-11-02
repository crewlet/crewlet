package cliagent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// The invariants the built-in gemini-cli profile keeps, each measured against
// Gemini CLI 0.58.0 and 0.65.0 (see the profile's comments): a call offers the
// two web tools and nothing else, trusts its own directory and reads no
// environment file above it, sends nothing to the vendor but the call, and
// every call — a seat's phase or a side pass — replaces the CLI's prompt,
// which has no channel that keeps it.

// geminiCLI is the shipped profile, which every case here reads.
func geminiCLI(t *testing.T) cliprofile.Profile {
	t.Helper()
	p, ok := cliprofile.Builtin("gemini-cli")
	if !ok {
		t.Fatal("no built-in gemini-cli profile")
	}
	return p
}

// geminiSeed returns the shipped profile's seed at path, failing when there is
// none.
func geminiSeed(t *testing.T, path string) cliprofile.SeedFile {
	t.Helper()
	for _, f := range geminiCLI(t).SeedFiles {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("the gemini-cli profile seeds no %s", path)
	return cliprofile.SeedFile{}
}

// geminiProvider drives the shipped profile against the fake CLI.
func geminiProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "gemini", Model: "gemini-2.5-pro", Agent: "gemini-cli", StateDir: dir,
		Overrides: map[string]any{"binary": os.Args[0]},
		Timeout:   20 * time.Second, MaxConcurrent: 1,
		Env:  fakeChildEnv(env),
		Auth: Auth{Mode: AuthAPIKey, APIKey: "sk-test-dummy"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// policyRule is one [[rule]] of a Gemini CLI policy file, read by the few
// lines this profile writes: `toolName = "x"` or `toolName = ["x", "y"]`,
// `decision = "d"` and `priority = n`.
type policyRule struct {
	tools    []string
	decision string
	priority int
}

// policyRules reads the rules of a policy file as this profile writes it. Not
// a TOML parser: a line it does not know fails the case, so a seed that grows
// a key this reading would skip has to be read here too.
func policyRules(t *testing.T, content string) []policyRule {
	t.Helper()
	var rules []policyRule
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == "[[rule]]":
			rules = append(rules, policyRule{})
		case len(rules) == 0:
			t.Fatalf("policy line %q comes before any [[rule]]", line)
		default:
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				t.Fatalf("policy line %q is not key = value", line)
			}
			rule := &rules[len(rules)-1]
			value = strings.TrimSpace(value)
			switch strings.TrimSpace(key) {
			case "toolName":
				var names []string
				if strings.HasPrefix(value, "[") {
					if err := json.Unmarshal([]byte(value), &names); err != nil {
						t.Fatalf("toolName %s: %v", value, err)
					}
				} else {
					names = []string{strings.Trim(value, `"`)}
				}
				rule.tools = names
			case "decision":
				rule.decision = strings.Trim(value, `"`)
			case "priority":
				n, err := strconv.Atoi(value)
				if err != nil {
					t.Fatalf("priority %s: %v", value, err)
				}
				rule.priority = n
			default:
				t.Fatalf("policy key %q is one this reading does not know", key)
			}
		}
	}
	return rules
}

// A CALL OFFERS THE WEB TOOLS AND NOTHING ELSE, by the vendor's policy engine:
// every tool denied, the two web tools allowed above the denial. A deny list of
// the file and shell tools left invoke_agent (three sub-agents), enter_plan_mode,
// update_topic and activate_skill on offer, and every tool a later release adds.
// web_fetch has to be ALLOWED, not merely spared: its default asks first, and
// a headless run registers no tool that would ask — measured, without the
// allow the model had no web_fetch at all.
func TestGeminiOffersTheWebToolsAndNothingElse(t *testing.T) {
	t.Parallel()
	seed := geminiSeed(t, ".gemini/policies/crewlet.toml")
	if seed.Scope() != cliprofile.SeedHome {
		t.Errorf("the policy is seeded in %q, want the seat's home, where the CLI reads "+
			"its user policies", seed.Scope())
	}
	var deny, allow *policyRule
	rules := policyRules(t, seed.Content)
	for i := range rules {
		switch {
		case rules[i].decision == "deny" && slices.Equal(rules[i].tools, []string{"*"}):
			deny = &rules[i]
		case rules[i].decision == "allow":
			if allow != nil {
				t.Errorf("more than one allow rule: %v and %v", allow.tools, rules[i].tools)
			}
			allow = &rules[i]
		default:
			t.Errorf("rule %+v is neither the denial of every tool nor the web tools' allow", rules[i])
		}
	}
	if deny == nil {
		t.Fatalf("no rule denies every tool (toolName = \"*\"), so the CLI offers whatever "+
			"its build ships: %+v", rules)
	}
	if allow == nil {
		t.Fatalf("no rule allows the web tools: %+v", rules)
	}
	tools := slices.Sorted(slices.Values(allow.tools))
	if want := []string{"google_web_search", "web_fetch"}; !slices.Equal(tools, want) {
		t.Errorf("the allow rule names %q, want exactly %q", tools, want)
	}
	if allow.priority <= deny.priority {
		t.Errorf("allow priority %d is not above deny priority %d: the higher rule decides, "+
			"so the web tools would be denied", allow.priority, deny.priority)
	}
}

// NO DEPRECATED TOOL KEYS: tools.exclude and tools.allowed print a warning on
// every run of both releases measured, and the policy file is what replaces
// them. And no usage statistics: on by default, they open a connection to the
// vendor's logging service beside every call.
func TestGeminiSettingsSendOnlyTheCall(t *testing.T) {
	t.Parallel()
	seed := geminiSeed(t, ".gemini/settings.json")
	var settings struct {
		Privacy struct {
			UsageStatisticsEnabled *bool `json:"usageStatisticsEnabled"`
		} `json:"privacy"`
		Tools        map[string]any `json:"tools"`
		ExcludeTools any            `json:"excludeTools"`
	}
	if err := json.Unmarshal([]byte(seed.Content), &settings); err != nil {
		t.Fatalf("settings.json is not JSON: %v", err)
	}
	if on := settings.Privacy.UsageStatisticsEnabled; on == nil || *on {
		t.Errorf("privacy.usageStatisticsEnabled is not false, so every call also reports to " +
			"the vendor's logging service")
	}
	if settings.Tools != nil || settings.ExcludeTools != nil {
		t.Errorf("settings.json carries a tool list (%v, %v): the policy file holds the "+
			"tools, and these keys warn on every run", settings.Tools, settings.ExcludeTools)
	}
}

// EVERY CALL TRUSTS ITS OWN DIRECTORY: untrusted, a headless run exits 55
// before it sends anything, on both releases. The variable rather than a
// setting, because the CLI reads it before any settings file.
func TestGeminiCallsTrustTheirOwnDirectory(t *testing.T) {
	t.Parallel()
	env := childEnv(geminiCLI(t), &Checkout{Home: t.TempDir(), Cache: t.TempDir(), Work: t.TempDir()},
		nil, Auth{Mode: AuthAPIKey, APIKey: "sk-test-dummy"})
	if got := env["GEMINI_CLI_TRUST_WORKSPACE"]; got != "true" {
		t.Errorf("GEMINI_CLI_TRUST_WORKSPACE = %q, want true", got)
	}
}

// A SUBSCRIPTION CALL SIGNS IN WITH THE LOGIN, AN API-KEY CALL WITH ITS KEY. The
// CLI picks its sign-in from its environment — GOOGLE_GENAI_USE_GCA=true ahead
// of GEMINI_API_KEY — and a copied login selects nothing: without the variable
// every subscription call exited 41, and with it under api-key the key would
// go unused. Set over the operator's env, as part of the mode; and NO_BROWSER
// on every call, so one whose login is missing exits rather than read its own
// prompt as the answer to "open a browser?".
func TestGeminiSignsInWithWhatTheModeNames(t *testing.T) {
	t.Parallel()
	for mode, want := range map[AuthMode]string{
		AuthSubscription: "true",
		AuthAPIKey:       "",
		AuthInheritEnv:   "",
	} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			env := childEnv(geminiCLI(t), &Checkout{Home: t.TempDir(), Cache: t.TempDir(), Work: t.TempDir()},
				map[string]string{"GOOGLE_GENAI_USE_GCA": "false"}, Auth{Mode: mode, APIKey: "sk-test-dummy"})
			got := env["GOOGLE_GENAI_USE_GCA"]
			if want == "" {
				// The operator's own value, untouched.
				want = "false"
			}
			if got != want {
				t.Errorf("GOOGLE_GENAI_USE_GCA = %q, want %q", got, want)
			}
			if env["NO_BROWSER"] != "true" {
				t.Errorf("NO_BROWSER = %q, want true", env["NO_BROWSER"])
			}
		})
	}
}

// A KEY CALL BEHIND A GATEWAY STAYS ON ITS KEY, AND A LOGIN CALL ON ITS LOGIN.
// Naming no sign-in in its settings, the CLI takes one from its environment,
// where GOOGLE_GEMINI_BASE_URL — an operator's gateway in front of the API —
// outranks GEMINI_API_KEY and selects a sign-in 0.65.0 refuses: exit 41,
// "Invalid auth method selected", before any request (measured through the
// engine). Settings outrank the environment, so the home a key call runs in
// names the key sign-in, and the home a subscription call runs in names none,
// leaving GOOGLE_GENAI_USE_GCA to pick the login — the key sign-in there would
// send it to a key it does not have.
//
// Mutation: drop the profile's key_seed_files, and the key home names no
// sign-in; put its content in seed_files instead, and the login home names the
// key.
func TestGeminiKeyHomeNamesTheKeySignIn(t *testing.T) {
	t.Parallel()
	selected := func(t *testing.T, home string) (string, bool) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json")) //nolint:gosec // the test's own home
		if err != nil {
			t.Fatalf("the home has no settings: %v", err)
		}
		var settings struct {
			Privacy struct {
				UsageStatisticsEnabled *bool `json:"usageStatisticsEnabled"`
			} `json:"privacy"`
			Security struct {
				Auth struct {
					SelectedType *string `json:"selectedType"`
				} `json:"auth"`
			} `json:"security"`
		}
		if err := json.Unmarshal(raw, &settings); err != nil {
			t.Fatalf("settings.json is not JSON: %v", err)
		}
		if on := settings.Privacy.UsageStatisticsEnabled; on == nil || *on {
			t.Errorf("the home's settings report usage statistics: %s", raw)
		}
		if settings.Security.Auth.SelectedType == nil {
			return "", false
		}
		return *settings.Security.Auth.SelectedType, true
	}

	keyed := geminiProvider(t, nil)
	co, err := keyed.ws.AcquireForKey(t.Context(), "pm", "call-1")
	if err != nil {
		t.Fatalf("AcquireForKey: %v", err)
	}
	t.Cleanup(func() { _ = co.Release(t.Context()) })
	if got, ok := selected(t, co.Home); got != "gemini-api-key" {
		t.Errorf("the key home names sign-in %q (named: %v), want gemini-api-key: behind a gateway "+
			"the CLI picks its gateway sign-in and exits 41", got, ok)
	}

	login := geminiProvider(t, nil)
	lc, err := login.ws.Acquire(t.Context(), "pm", "call-1", login)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lc.Release(t.Context()) })
	if got, ok := selected(t, lc.Home); ok {
		t.Errorf("the login home names sign-in %q: a subscription call would go to a key it does not have", got)
	}
}

// NO ENVIRONMENT FILE ABOVE THE CALL IS READ. The CLI loads the first
// `.gemini/.env` or `.env` it meets walking up from its working directory to
// the root — past the engine's `.git` marker — and takes every key its
// environment lacks: measured, an ancestor's file redirected a call to another
// base URL and signed a keyless one in with its own key. Both names stand in
// the call's directory when the CLI starts, so the walk ends there, and they
// assign nothing.
func TestGeminiCallsReadNoEnvironmentFileAboveTheirDirectory(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".gemini/.env", ".env"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seed := geminiSeed(t, name)
			if seed.Scope() != cliprofile.SeedWork {
				t.Errorf("%s is seeded in %q, want the call's own directory, where the "+
					"walk starts", name, seed.Scope())
			}
			for line := range strings.SplitSeq(seed.Content, "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					t.Errorf("%s assigns %q: it is there to end the walk, not to set anything", name, line)
				}
			}
			p := geminiProvider(t, map[string]string{"FAKE_READ_DISCOVERED": name})
			comp, err := ask(t, p, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "status?"}}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			parts := strings.SplitN(comp.Content, "|", 3)
			if len(parts) != 3 {
				t.Fatalf("the CLI found no %s in its working directory: %s", name, comp.Content)
			}
			body, err := base64.StdEncoding.DecodeString(parts[2])
			if err != nil || string(body) != seed.Content {
				t.Errorf("the CLI read %q from %s, want the seed %q", body, name, seed.Content)
			}
		})
	}
}

// A SEAT AND A SIDE PASS BOTH REPLACE THE CLI'S PROMPT, through the file
// GEMINI_SYSTEM_MD names, and neither carries the harness note: the CLI has no
// channel that keeps its own prompt (none in either release measured), so a
// seat takes the replacement, and the note is for a call whose vendor prompt
// stays in force.
func TestGeminiSeatsAndSidePassesReplaceTheCLIsPrompt(t *testing.T) {
	t.Parallel()
	p := geminiCLI(t)
	if p.AppendSystemPrompt.Declared() {
		t.Fatalf("append_system_prompt is declared (%+v), but Gemini CLI has no channel that "+
			"keeps its own prompt", p.AppendSystemPrompt)
	}
	for name, agent := range map[string]bool{"a seat at work": true, "a side pass": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			route := p.RouteFor(agent)
			if route.Field != "system_prompt" || route.KeepsDefault {
				t.Errorf("route = %+v, want the replacing system_prompt", route)
			}
			prov := geminiProvider(t, map[string]string{"FAKE_READ_SYSTEM_PROMPT": "GEMINI_SYSTEM_MD"})
			comp, err := ask(t, prov, llm.Request{Agent: agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: "You are Agent PM at Nimbus."},
				{Role: llm.RoleUser, Content: "status?"},
			}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			_, body, ok := strings.Cut(comp.Content, "|")
			if !ok {
				t.Fatalf("the CLI could not read GEMINI_SYSTEM_MD: %s", comp.Content)
			}
			if body != "You are Agent PM at Nimbus." {
				t.Errorf("GEMINI_SYSTEM_MD held %q, want the engine's text alone", body)
			}
		})
	}
}

// A real Gemini CLI answer, from a 0.65.0 call that fetched a page: the model
// it was asked for answered twice, and web_fetch asked a second model on the
// side, whose prompt carried the fetched page as the tool's own (`tool`).
// Pinned verbatim from that run, trimmed to the fields the profile reads and
// the per-role breakdown beside them, so the sum is over the models and never
// over a model's roles as well.
const geminiAnswer = `{"session_id":"3e6bd605-11d3-49d9-b990-66e67eab4716",
"response":"STUB-ANSWER for probe-webfetch","stats":{"models":{
"gemini-2.5-pro":{"api":{"totalRequests":2,"totalErrors":0,"totalLatencyMs":94,"errorsByType":{}},
"tokens":{"input":1234,"prompt":2234,"candidates":76,"total":2317,"cached":1000,"thoughts":7,"tool":0},
"roles":{"main":{"totalRequests":2,"totalErrors":0,"totalLatencyMs":94,
"tokens":{"input":1234,"prompt":2234,"candidates":76,"total":2317,"cached":1000,"thoughts":7,"tool":0}}}},
"gemini-3-flash-preview":{"api":{"totalRequests":1,"totalErrors":0,"totalLatencyMs":4,"errorsByType":{}},
"tokens":{"input":100,"prompt":100,"candidates":10,"total":5110,"cached":0,"thoughts":0,"tool":5000},
"roles":{"utility_tool":{"totalRequests":1,"totalErrors":0,"totalLatencyMs":4,
"tokens":{"input":100,"prompt":100,"candidates":10,"total":5110,"cached":0,"thoughts":0,"tool":5000}}}}}}}`

// EVERY MODEL A CALL USED IS CHARGED, AND EVERY TOKEN THE VENDOR BILLS. The
// counts are keyed by model, so each is summed over the models; the cached
// part of the prompt and the reasoning are read apart, as the CLI reports
// them; and the page a web fetch put in front of the side model is input. The
// whole call: 2,334 prompt tokens and 5,000 of the tool's, 86 answered and 7
// reasoned.
func TestTheGeminiProfileChargesEveryModelACallUsed(t *testing.T) {
	t.Parallel()
	p := geminiCLI(t)
	got := extract(p, geminiAnswer)
	if got.text != "STUB-ANSWER for probe-webfetch" {
		t.Errorf("text = %q", got.text)
	}
	if !got.reported {
		t.Fatal("usage went unread, so every call would be charged an estimate")
	}
	if got.input != 1334 || got.cacheRead != 1000 || got.output != 86 || got.reasoning != 7 || got.toolPrompt != 5000 {
		t.Errorf("usage = in %d cached %d out %d reasoning %d tool %d, want 1334 1000 86 7 5000",
			got.input, got.cacheRead, got.output, got.reasoning, got.toolPrompt)
	}
	prov := &Provider{profile: p, model: "gemini-2.5-pro", now: time.Now}
	comp, err := prov.completion(t.Context(), "prompt", &rawResult{stdout: geminiAnswer}, "")
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	if comp.InputTokens != 7334 || comp.OutputTokens != 93 || comp.CacheRead != 1000 {
		t.Errorf("completion = in %d out %d cached %d, want 7334 93 1000",
			comp.InputTokens, comp.OutputTokens, comp.CacheRead)
	}
}

// NOTHING ONE CALL LEAVES IN THE SEAT'S HOME OUTLIVES THE CALLS THAT SHARE IT:
// the transcripts and per-call temporary directories under .gemini/tmp, the
// per-call history directories, and projects.json, the registry that gains an
// entry for every call's directory.
func TestGeminiPrunesWhatEachCallLeaves(t *testing.T) {
	t.Parallel()
	p := geminiCLI(t)
	for _, rel := range []string{".gemini/tmp", ".gemini/history", ".gemini/projects.json"} {
		if !slices.Contains(p.VolatilePaths, rel) {
			t.Errorf("volatile_paths = %q lacks %s", p.VolatilePaths, rel)
		}
	}
}

// THE CLI'S OWN FAILURES ARE NAMED. Measured on both releases: a spent daily
// quota is printed as a TerminalQuotaError, and a per-minute limit is retried
// and then printed as a RetryableQuotaError, neither carrying the API's
// RESOURCE_EXHAUSTED; a refused login is the API's UNAUTHENTICATED, and a
// refused key the API's own sentence, both relayed whole. Each is the first
// line of what the CLI wrote to stderr, beside a non-zero exit.
func TestGeminiRecognisesItsOwnQuotaAndSignInFailures(t *testing.T) {
	t.Parallel()
	p := geminiCLI(t)
	const report = "Error when talking to Gemini API Full report available at: /home/tmp/gemini-client-error.json "
	for name, tc := range map[string]struct {
		stderr string
		want   llm.ErrorKind
	}{
		"a spent daily quota": {report + "TerminalQuotaError: You have exhausted your daily quota on this model.\n" +
			"    at classifyGoogleError (file:///bundle/chunk.js:306678:16)", llm.KindRateLimit},
		"a rate limit past its retries": {report + "RetryableQuotaError: Resource has been exhausted (e.g. check quota).\n" +
			"    at classifyGoogleError (file:///bundle/chunk.js:306678:16)", llm.KindRateLimit},
		"a refused login": {report + `_ApiError: {"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`, llm.KindAuth},
		"a refused key":   {report + `_ApiError: {"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`, llm.KindAuth},
		"a login it could not use": {"FatalAuthenticationError: Manual authorization is required but the current session is " +
			"non-interactive. Please run the Gemini CLI in an interactive terminal to log in, provide a GEMINI_API_KEY, or " +
			"ensure Application Default Credentials are configured.\n  exitCode: 41", llm.KindAuth},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hit, ok := classifyMarkers(p, cliOutput{stderr: tc.stderr, failed: true})
			if !ok || hit.Kind != tc.want {
				t.Errorf("classified %v (found %v), want %v", hit.Kind, ok, tc.want)
			}
		})
	}
}

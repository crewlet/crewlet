package cliprofile

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// Every shipped profile must be usable as-is. A profile that needs an
// override to work at all is one an operator discovers is broken by running
// it, which defeats the point of shipping it.
func TestEveryShippedProfileLoadsWithNoOverrides(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		if name == "custom" {
			// `custom` ships nothing on purpose, and its emptiness is
			// asserted by TestCustomShipsNothingAndSaysWhatIsMissing.
			continue
		}
		if _, err := Load(name, nil); err != nil {
			t.Errorf("Load(%q): %v", name, err)
		}
	}
}

// The `custom` profile must fail with a message naming what to set, not with
// a nil-pointer or a silent success that produces an empty command line.
func TestCustomShipsNothingAndSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	_, err := Load("custom", nil)
	if err == nil {
		t.Fatal("Load(custom) succeeded with no overrides")
	}
	for _, want := range []string{"binary", "cli.overrides"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A fully declared custom profile must work with no built-in help at all —
// this is the escape hatch for an operator's own wrapper or a self-hosted
// gateway CLI.
func TestCustomWorksWhenFullyDeclared(t *testing.T) {
	t.Parallel()
	p, err := Load("custom", map[string]any{
		"binary":        "my-gateway-llm",
		"complete_args": []any{"--json"},
		"output":        "json",
		"text_paths":    []any{[]any{"answer"}},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Binary != "my-gateway-llm" {
		t.Errorf("Binary = %q", p.Binary)
	}
}

// The rule the docs promise: maps merge key-wise, lists replace wholesale.
// An element-wise list merge would produce an argv neither side wrote, and
// the failure would look like a vendor bug rather than a config one.
func TestOverridesReplaceListsWholesale(t *testing.T) {
	t.Parallel()
	base, _ := Builtin("claude-code")
	if len(base.CompleteArgs) < 3 {
		t.Fatalf("the built-in profile has too few args to make this test meaningful: %v", base.CompleteArgs)
	}
	p, err := Load("claude-code", map[string]any{
		"complete_args": []any{"-p", "--json"},
		"env":           map[string]any{"EXTRA": "1"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := strings.Join(p.CompleteArgs, " "); got != "-p --json" {
		t.Errorf("complete_args = %q, want the override verbatim", got)
	}
	if p.Env["EXTRA"] != "1" {
		t.Errorf("env override lost: %v", p.Env)
	}
	// Untouched fields survive.
	if p.Binary != base.Binary {
		t.Errorf("binary = %q, want the built-in %q", p.Binary, base.Binary)
	}
}

// A typo in an override must fail validation, not be ignored until the first
// turn. This is the whole reason the merge round-trips through YAML with
// KnownFields rather than reflecting field by field.
func TestAnOverrideTypoIsRefused(t *testing.T) {
	t.Parallel()
	_, err := Load("claude-code", map[string]any{"complete_arg": []any{"-p"}})
	if err == nil {
		t.Fatal("a misspelled override field was accepted")
	}
	if !strings.Contains(err.Error(), "complete_arg") {
		t.Errorf("error %q does not name the offending field", err)
	}
}

// passthrough_env is forwarded BEFORE auth.mode is consulted, so a credential
// named there reaches every seat whatever the mode says — the exact
// metered-bill-on-a-flat-rate-plan failure auth.mode exists to prevent. An
// ADMISSION rule: the profile still loads, because it still runs.
func TestAProfileMayNotPassThroughACredential(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "MY_SECRET", "DB_PASSWORD"} {
		p, err := Load("claude-code", map[string]any{"passthrough_env": []any{name}})
		if err != nil {
			t.Errorf("passthrough_env %q failed the runnable rules, which it does not break: %v", name, err)
			continue
		}
		err = p.ValidateCredentials("claude-code")
		if err == nil {
			t.Errorf("passthrough_env accepted %q", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error for %q does not name it: %v", name, err)
		}
	}
	// And a genuine non-secret is still allowed, or the check is useless.
	p, err := Load("claude-code", map[string]any{"passthrough_env": []any{"GOOGLE_CLOUD_PROJECT"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := p.ValidateCredentials("claude-code"); err != nil {
		t.Errorf("a non-credential passthrough was refused: %v", err)
	}
}

// THE PREDICATE READS WHOLE NAME COMPONENTS. It decides whether an entry is
// signed in, which keys travel into a coding box and which profile entries are
// refused, so a loose match is wrong three ways: as substrings it read
// HERMES_MAX_TOKENS as a key — the doctor then called an entry with no key
// healthy — and refused Claude Code's own CLAUDE_CODE_MAX_OUTPUT_TOKENS.
func TestACredentialNameIsJudgedByItsWholeComponents(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"OPENROUTER_API_KEY":             true,
		"GH_TOKEN":                       true,
		"AWS_SECRET_ACCESS_KEY":          true,
		"GOOGLE_APPLICATION_CREDENTIALS": true,
		"ANTHROPIC_AUTH_TOKEN":           true,
		"CLAUDE_CODE_OAUTH_TOKEN":        true,
		"DB_PASSWORD":                    true,
		"MINIO_ACCESSKEY":                true,
		"openai_api_key":                 true,
		"GITHUB_PAT":                     true,
		"GEMINI_API_KEYS":                true,
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS":  false,
		"HERMES_MAX_TOKENS":              false,
		"OPENAI_BASE_URL":                false,
		"HTTPS_PROXY":                    false,
		"MONKEY_PATH":                    false,
		"AUTHOR_NAME":                    false,
	} {
		if got := IsCredentialName(name); got != want {
			t.Errorf("IsCredentialName(%q) = %v, want %v", name, got, want)
		}
	}
}

// Overriding one entry's profile must not rewrite the table every later
// entry reads — a shared map value would make the second provider inherit
// the first one's overrides.
func TestOverridesDoNotLeakIntoTheBuiltinTable(t *testing.T) {
	t.Parallel()
	if _, err := Load("codex", map[string]any{"binary": "/opt/custom/codex"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	fresh, err := Load("codex", nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fresh.Binary == "/opt/custom/codex" {
		t.Error("one entry's override leaked into the built-in table")
	}
}

// A sentinel is matched as a plain substring against whatever the CLI printed,
// which on a healthy call is the MODEL'S OWN ANSWER — so one that can occur in
// ordinary text does not recognise a spent plan, it misclassifies replies as
// one. Both kinds a marker produces bench the credential, so the cost is a
// working subscription taken out of service.
func TestASentinelWithNoLettersIsRefused(t *testing.T) {
	t.Parallel()
	base := Profile{
		Binary:       "x",
		CompleteArgs: []string{"-p"},
		Output:       OutputText,
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*Profile)
		wantErr string
	}{
		{"a bare HTTP status as a limit marker",
			func(p *Profile) { p.LimitMarkers = []LimitMarker{{Sentinel: "429"}} },
			"limit_markers[0].sentinel"},
		{"punctuation only",
			func(p *Profile) { p.LimitMarkers = []LimitMarker{{Sentinel: "!!!"}} },
			"limit_markers[0].sentinel"},
		// Auth markers were not checked AT ALL — not even for emptiness —
		// and KindAuth exhausts the credential exactly as a spent plan does.
		{"an auth marker with no letters",
			func(p *Profile) { p.AuthMarkers = []AuthMarker{{Sentinel: "401"}} },
			"auth_markers[0].sentinel"},
		{"an empty auth marker",
			func(p *Profile) { p.AuthMarkers = []AuthMarker{{Sentinel: ""}} },
			"auth_markers[0].sentinel is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := base
			tc.mutate(&p)
			err := p.Validate("test")
			if err == nil {
				t.Fatal("a sentinel that matches ordinary text was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("message does not name the field: %v", err)
			}
		})
	}

	// The rule is "it has to contain a letter" and nothing more: a short
	// vendor string is legitimate, and a length floor would be a constant
	// nobody can defend.
	for _, ok := range []string{"quota", "429 Too Many Requests", "Quota exceeded"} {
		p := base
		p.LimitMarkers = []LimitMarker{{Sentinel: ok}}
		if err := p.Validate("test"); err != nil {
			t.Errorf("sentinel %q was refused: %v", ok, err)
		}
	}
}

// No shipped profile may carry one, which is the half a rule alone does not
// give you: `429` sat in the opencode profile until this test existed.
func TestNoShippedSentinelCanMatchOrdinaryText(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		if name == "custom" {
			// Ships nothing on purpose, so it does not load — see
			// TestCustomShipsNothingAndSaysWhatIsMissing.
			continue
		}
		p, err := Load(name, nil)
		if err != nil {
			t.Errorf("Load(%q): %v", name, err)
			continue
		}
		for _, m := range p.LimitMarkers {
			if problem := sentinelProblem(m.Sentinel); problem != "" {
				t.Errorf("%s limit marker %s", name, problem)
			}
		}
		for _, m := range p.AuthMarkers {
			if problem := sentinelProblem(m.Sentinel); problem != "" {
				t.Errorf("%s auth marker %s", name, problem)
			}
		}
	}
}

// A profile's env is forwarded whatever auth.mode says, and cli.overrides is
// stored and shown unredacted — so a key written there is both a silent bill
// and a credential in plain text. The field's own comment always said "never
// a credential"; nothing held it to that. An ADMISSION rule, like the
// passthrough one: the profile loads and runs.
func TestAProfileMayNotCarryACredentialInItsEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		agent, name string
		extra       map[string]any
		want        string
	}{
		{"hermes", "OPENROUTER_API_KEY", nil, "cli.env"},
		{"claude-code", "ANTHROPIC_AUTH_TOKEN", nil, "cli.env"},
		// Named by the profile as its key variable: refused as auth's
		// to set, even though the name does not look like a credential.
		{"grok", "XAI_LOGIN", map[string]any{"api_key_env": "XAI_LOGIN"}, "api_key_env"},
	} {
		overrides := map[string]any{"env": map[string]any{tc.name: "sk-literal"}}
		for k, v := range tc.extra {
			overrides[k] = v
		}
		p, err := Load(tc.agent, overrides)
		if err != nil {
			t.Errorf("%s: env %s failed the runnable rules, which it does not break: %v", tc.agent, tc.name, err)
			continue
		}
		err = p.ValidateCredentials(tc.agent)
		if err == nil {
			t.Errorf("%s: env %s was accepted", tc.agent, tc.name)
			continue
		}
		for _, want := range []string{tc.name, tc.want} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error does not mention %q: %v", tc.agent, want, err)
			}
		}
	}
	// And genuine settings pass, or the rule refuses what env is for — the
	// second one a real Claude Code variable a substring match refused.
	for agent, env := range map[string]string{
		"muse-code":   "MUSE_NO_AUTO_UPDATE",
		"claude-code": "CLAUDE_CODE_MAX_OUTPUT_TOKENS",
	} {
		p, err := Load(agent, map[string]any{"env": map[string]any{env: "1"}})
		if err != nil {
			t.Fatalf("Load %s: %v", agent, err)
		}
		if err := p.ValidateCredentials(agent); err != nil {
			t.Errorf("%s: a non-credential env %s was refused: %v", agent, env, err)
		}
	}
}

// The rule judges the DEFAULTED output mode, and so must its sentence: a
// profile naming no output is a json profile, and the message used to print
// the empty field — "a  profile must say where the answer is".
func TestTheTextPathsRuleNamesTheDefaultedOutput(t *testing.T) {
	t.Parallel()
	_, err := Load("custom", map[string]any{"binary": "x", "complete_args": []any{"-p"}})
	if err == nil {
		t.Fatal("a json profile with no text_paths loaded")
	}
	if !strings.Contains(err.Error(), "a json profile must say where the answer is") {
		t.Errorf("message does not name the defaulted mode: %v", err)
	}
}

// A typo in cli.overrides is refused with a pointer to the field list in the
// docs, so the list has to exist and be THE list: every field the decoder
// accepts, and nothing it refuses. It did not exist when the pointer was
// first written.
func TestTheDocsListEveryProfileField(t *testing.T) {
	t.Parallel()
	page := filepath.Join(sourcetree.Root(t), "docs", "concepts", "subscription-llm-backends.md")
	raw, err := os.ReadFile(page) //nolint:gosec // a fixed path under the module root
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(raw), "\n### Profile fields\n")
	if !found {
		t.Fatal(`the page has no "### Profile fields" section, which Load's error points at`)
	}
	section, _, _ = strings.Cut(section, "\n#")
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|").FindAllStringSubmatch(section, -1) {
		documented[m[1]] = true
	}

	accepted := map[string]bool{}
	profile := reflect.TypeFor[Profile]()
	for i := range profile.NumField() {
		name, _, _ := strings.Cut(profile.Field(i).Tag.Get("yaml"), ",")
		accepted[name] = true
	}
	for name := range accepted {
		if !documented[name] {
			t.Errorf("profile field %q is accepted by cli.overrides but missing from the docs' field list", name)
		}
	}
	for name := range documented {
		if !accepted[name] {
			t.Errorf("the docs list %q, which cli.overrides refuses", name)
		}
	}
}

// Which variable each CLI reads a key and a headless token from, as each
// profile's note records it from the vendor. A variable missing here is a key
// `auth.mode: api-key` has nowhere to put — cursor-agent's CURSOR_API_KEY was
// — and one named here that the CLI ignores is a key that reaches nothing.
func TestEachProfileNamesTheVariablesItsCLIReads(t *testing.T) {
	t.Parallel()
	want := map[string][2]string{ // agent: {api_key_env, token_env}
		"claude-code":  {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		"codex":        {"OPENAI_API_KEY", ""},
		"gemini-cli":   {"GEMINI_API_KEY", ""},
		"qwen-code":    {"DASHSCOPE_API_KEY", ""},
		"opencode":     {"", ""},
		"cursor-agent": {"CURSOR_API_KEY", ""},
		"copilot":      {"GITHUB_TOKEN", ""},
		"grok":         {"XAI_API_KEY", ""},
		"muse-code":    {"META_API_KEY", ""},
		"kimi-code":    {"", ""},
		"hermes":       {"", ""},
		"pi":           {"", ""},
		"custom":       {"", ""},
	}
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		w, ok := want[name]
		if !ok {
			t.Errorf("profile %q ships with no expectation here", name)
			continue
		}
		if p.APIKeyEnv != w[0] || p.TokenEnv != w[1] {
			t.Errorf("%s: api_key_env %q token_env %q, want %q %q",
				name, p.APIKeyEnv, p.TokenEnv, w[0], w[1])
		}
	}
}

// Which CLIs read their model provider's key from their own environment, as
// each profile's note records it from the vendor. Declared where it is not
// true, `doctor` reports a key the CLI ignores as a sign-in (kimi-code is
// handed KIMI_API_KEY and reads only config.toml); missing where it is, the
// supported route is reported as no sign-in at all, which is what hermes
// suffered.
func TestEnvSignInIsDeclaredExactlyWhereTheCLIReadsItsKeyFromItsEnvironment(t *testing.T) {
	t.Parallel()
	reads := []string{"hermes", "opencode", "pi"}
	for _, name := range BuiltinNames() {
		p, _ := Builtin(name)
		if got, want := p.EnvSignIn, slices.Contains(reads, name); got != want {
			t.Errorf("%s: env_sign_in = %v, want %v", name, got, want)
		}
	}
	// And like every field it is an operator's to declare for a CLI this
	// build does not ship.
	p, err := Load("custom", map[string]any{
		"binary": "my-gateway-cli", "complete_args": []any{"-p"}, "output": "text",
		"env_sign_in": true,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !p.EnvSignIn {
		t.Error("cli.overrides.env_sign_in did not reach the profile")
	}
}

package config

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Parsing must leave a reference ALONE. `crewlet validate` runs on a laptop
// where no credential exists, and a stored revision that carried resolved
// secrets would leak them into every export and every dashboard view.
func TestTierBKeepsReferencesVerbatim(t *testing.T) {
	t.Setenv("ACME_KEY", "sk-real-value")
	cfg, err := ParseCompany([]byte(`
name: Acme
providers:
  llm:
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ACME_KEY}"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Providers.LLM["default"].APIKeys[0]; got != "${ACME_KEY}" {
		t.Fatalf("parsing resolved a Tier B reference: %q", got)
	}
}

// Tier A is the opposite: the store path, the broker URL and the API tokens
// are needed the instant the process starts, so they are resolved before
// the document is decoded.
func TestTierAResolvesAtLoad(t *testing.T) {
	t.Setenv("ACME_DB", "/srv/acme.db")
	t.Setenv("ACME_TOKEN", "tok-123")
	cfg, err := ParseBootstrap([]byte(`
store:
  path: "${ACME_DB}"
api:
  port: 8000
  auth:
    tokens:
      - id: founder
        token: "${ACME_TOKEN}"
`), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != "/srv/acme.db" {
		t.Fatalf("store path = %q", cfg.Store.Path)
	}
	if cfg.API.Auth.Tokens[0].Token != "tok-123" {
		t.Fatalf("token = %q", cfg.API.Auth.Tokens[0].Token)
	}
}

// THE STORE WINS. Env-first would let a stale .env shadow a freshly rotated
// secret, which surfaces days later as an auth error from a provider rather
// than at the point of rotation — the exact failure a secret store exists
// to remove.
func TestStoreBeatsEnvironment(t *testing.T) {
	t.Setenv("ROTATING", "stale-from-dotenv")
	r := WithStore(MapSource{"ROTATING": "fresh-from-store"})
	if got := r.Value("${ROTATING}"); got != "fresh-from-store" {
		t.Fatalf("the store must win; got %q", got)
	}
}

// A stored EMPTY value is authoritative, not a miss: an operator who
// deliberately stored an empty credential has said something, and falling
// through to a stale export would undo it.
func TestStoredEmptyValueIsAuthoritative(t *testing.T) {
	t.Setenv("MAYBE", "from-env")
	r := WithStore(MapSource{"MAYBE": ""})
	if got := r.Value("${MAYBE}"); got != "" {
		t.Fatalf("a stored empty value must stop the chain; got %q", got)
	}
}

func TestEnvironmentIsTheFallback(t *testing.T) {
	t.Setenv("ONLY_ENV", "from-env")
	r := WithStore(MapSource{"SOMETHING_ELSE": "x"})
	if got := r.Value("${ONLY_ENV}"); got != "from-env" {
		t.Fatalf("got %q", got)
	}
}

// An unresolved reference expands to the empty string — matching the shell
// — and is REPORTED, because the empty string is not detectable
// downstream: "Bearer ${TOKEN}" with TOKEN unset becomes "Bearer ", which
// is truthy-but-broken and reads as a rejected credential rather than a
// missing one.
func TestUnresolvedReferenceIsEmptyAndReported(t *testing.T) {
	t.Parallel()
	r := NewResolver(MapSource{})
	value, missing := r.Expand("Bearer ${ACME_NOT_SET}")
	if value != "Bearer " {
		t.Fatalf("value = %q", value)
	}
	if !slices.Equal(missing, []string{"ACME_NOT_SET"}) {
		t.Fatalf("missing = %v", missing)
	}
}

func TestDocumentResolutionReportsThePath(t *testing.T) {
	t.Parallel()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(`
api:
  auth:
    tokens:
      - id: founder
        token: "${ACME_ABSENT_TOKEN}"
`), &doc); err != nil {
		t.Fatal(err)
	}
	missing := NewResolver(MapSource{}).Document(&doc)
	if len(missing) != 1 {
		t.Fatalf("missing = %#v", missing)
	}
	if missing[0].Path != "api.auth.tokens[0].token" {
		t.Fatalf("path = %q", missing[0].Path)
	}
	if !slices.Equal(missing[0].Names, []string{"ACME_ABSENT_TOKEN"}) {
		t.Fatalf("names = %v", missing[0].Names)
	}
}

// A config's key space is its schema, and a schema that changed with the
// environment would not be one.
func TestDocumentResolutionNeverTouchesKeys(t *testing.T) {
	t.Setenv("KEYNAME", "port")
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("\"${KEYNAME}\": 1\n"), &doc); err != nil {
		t.Fatal(err)
	}
	EnvOnly().Document(&doc)
	var out map[string]any
	if err := doc.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["${KEYNAME}"]; !ok {
		t.Fatalf("a key was substituted: %v", out)
	}
}

// A substituted scalar must stay a string. An unquoted ${PORT} resolving to
// "8080" that was retagged as an integer would decode into a string field
// as a type error.
func TestSubstitutedScalarStaysAString(t *testing.T) {
	t.Setenv("ACME_STORE_NAME", "8080")
	cfg, err := ParseBootstrap([]byte("store:\n  path: ${ACME_STORE_NAME}\n"), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != "8080" {
		t.Fatalf("path = %q", cfg.Store.Path)
	}
}

// A shadowed name is logged by NAME. A value must never reach a log line —
// that is the whole reason the warning exists rather than a diff.
func TestShadowedNameIsLoggedOnceByNameOnly(t *testing.T) {
	t.Setenv("SHADOWED", "env-credential-aaa")
	var buf strings.Builder
	r := WithStore(MapSource{"SHADOWED": "store-credential-bbb"})
	r.log = testLogger(&buf)

	r.Value("${SHADOWED}")
	r.Value("${SHADOWED}")

	out := buf.String()
	if strings.Count(out, "secret_shadowed_env") != 1 {
		t.Fatalf("want exactly one warning, got:\n%s", out)
	}
	if !strings.Contains(out, "SHADOWED") {
		t.Fatalf("the warning must name the variable:\n%s", out)
	}
	for _, value := range []string{"env-credential-aaa", "store-credential-bbb"} {
		if strings.Contains(out, value) {
			t.Fatalf("a credential value reached the log:\n%s", out)
		}
	}
}

// Agreeing sources are not a shadow: warning on every name the store and a
// correctly-synced .env both hold would bury the one that matters.
func TestAgreeingSourcesAreNotShadowed(t *testing.T) {
	t.Setenv("AGREES", "same")
	var buf strings.Builder
	r := WithStore(MapSource{"AGREES": "same"})
	r.log = testLogger(&buf)
	r.Value("${AGREES}")
	if strings.Contains(buf.String(), "secret_shadowed_env") {
		t.Fatalf("agreeing sources warned:\n%s", buf.String())
	}
}

func TestMapResolutionReportsPerKey(t *testing.T) {
	t.Setenv("KNOWN", "yes")
	out, missing := EnvOnly().Map("role.mcp_env.atlassian", map[string]string{
		"JIRA_TOKEN": "${KNOWN}",
		"JIRA_EMAIL": "${ACME_UNSET_EMAIL}",
	})
	if out["JIRA_TOKEN"] != "yes" || out["JIRA_EMAIL"] != "" {
		t.Fatalf("out = %v", out)
	}
	if len(missing) != 1 || missing[0].Path != "role.mcp_env.atlassian.JIRA_EMAIL" {
		t.Fatalf("missing = %#v", missing)
	}
}

// A setup step's env is left VERBATIM: it is resolved exactly once, with
// the rest of the sandbox env at launch. Resolving here too would
// double-resolve a secret whose real value contains a literal ${...}.
func TestSetupStepResolvesFilesAndCommandsOnly(t *testing.T) {
	t.Setenv("ACME_STEP_TOKEN", "tok")
	step := SandboxSetupStep{
		Name:     "git-auth",
		Files:    map[string]string{"/tmp/creds": "password=${ACME_STEP_TOKEN}"},
		Commands: []string{"echo ${ACME_STEP_TOKEN}"},
		Env:      map[string]string{"GITLAB_TOKEN": "${ACME_STEP_TOKEN}"},
		Brief:    "use ${ACME_STEP_TOKEN}",
	}
	out, missing := step.Resolve("providers.sandbox.setup[0]", EnvOnly())
	if len(missing) != 0 {
		t.Fatalf("missing = %#v", missing)
	}
	if out.Files["/tmp/creds"] != "password=tok" || out.Commands[0] != "echo tok" {
		t.Fatalf("files/commands not resolved: %+v", out)
	}
	if out.Env["GITLAB_TOKEN"] != "${ACME_STEP_TOKEN}" {
		t.Fatalf("env must stay verbatim, got %q", out.Env["GITLAB_TOKEN"])
	}
	if out.Brief != "use ${ACME_STEP_TOKEN}" {
		t.Fatalf("brief must stay verbatim, got %q", out.Brief)
	}
}

// The pre-launch presence check tests the REFERENCES, not the resolved
// value: an embedded "Bearer ${TOKEN}" with TOKEN unset resolves to a
// non-empty "Bearer ", so a caller checking the result would miss exactly
// the composite shapes config allows.
func TestResolvableTestsTheReferencesNotTheValue(t *testing.T) {
	t.Parallel()
	r := NewResolver(MapSource{"SET": "v", "BLANK": ""})
	cases := []struct {
		value string
		want  bool
	}{
		{"literal", true},
		{"${SET}", true},
		{"Bearer ${SET}", true},
		{"Bearer ${MISSING}", false},
		// An empty credential is not a credential, even though a source
		// answered for it — which is where this differs from Expand.
		{"${BLANK}", false},
		{"${SET}${MISSING}", false},
	}
	for _, tc := range cases {
		if got := r.Resolvable(tc.value); got != tc.want {
			t.Fatalf("Resolvable(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

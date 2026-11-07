package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// EVERY SECRET A LAUNCH HANDS ITS BOX IS KEPT FOR ITS SESSION'S SCRUB, AND
// NOTHING ELSE IS, class by class, from the very request the job is started
// with: what the engine says sits in a credential position
// ([LaunchRequest.Secrets]), its MCP servers' env and headers and what their
// url's references resolved to, the run's own export token, and every string of
// the login the box was seeded with, read from the file it was seeded from; and
// where that login lives, so a capture reads it again in the box and on the
// host. Not the seat's handle or email, a setting, the telemetry's protocol, the
// unresolved text of a reference or a setup step's file: none is a secret, and
// each, scrubbed by value, was lost from every kept transcript that named it.
// Nothing blank and nothing twice, and the whole of it round-trips through what
// the row's seal covers.
//
// Mutation: leave a class out of handedSecrets, add the run's environment or
// its setup steps back whole, read the login only at the capture, or keep a
// blank value, and a check below goes red.
func TestEverySecretALaunchHandsItsBoxIsKeptForTheScrubAndNothingElse(t *testing.T) {
	t.Parallel()
	login := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(login, []byte(`{"claudeAiOauth":{"accessToken":"login-access-value",`+
		`"refreshToken":"login-refresh-value","scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := LaunchRequest{
		Spec:    Spec{CredentialFiles: map[string]string{".claude/.credentials.json": login}},
		Secrets: []string{"resolved-env-token", "credential-named-token", "  ", "resolved-env-token"},
		Setup: []SetupStep{{
			Env: map[string]string{"NPM_TOKEN": "${NPM_TOKEN}"},
			Files: map[string]string{
				"/home/user/.npmrc":     "//registry.example.com/:_authToken=${NPM_TOKEN}",
				"/home/user/git-env.sh": "git config user.name agent-swe-the-seat",
			},
		}},
		MCPServers: map[string]MCPServer{
			"zapier": {Name: "zapier", Transport: TransportHTTP,
				URL:     "https://mcp.example.com/s/url-path-secret-value/mcp",
				Headers: map[string]string{"Authorization": "Bearer mcp-header-value"},
				Secrets: []string{"url-path-secret-value"}},
			"files": {Name: "files", Transport: TransportStdio, Command: "mcp-files",
				Env: map[string]string{"API_TOKEN": "mcp-env-value"}},
		},
	}
	env := map[string]string{
		"CREWLET_AGENT_HANDLE": "agent-swe-the-seat", "CREWLET_AGENT_EMAIL": "agent-swe@example.com",
		"NODE_ENV": "production-settings", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		otelBoxEndpointVar: "http://host.docker.internal:8080" + otelTokenPath + "the-runs-export-token",
		"GITHUB_TOKEN":     "resolved-env-token",
		GitTokenVar:        "the-runs-gitlet-token",
	}
	handed := handedSecrets(req, env)
	for class, want := range map[string]string{
		"a value the engine resolved from a reference": "resolved-env-token",
		"a variable named for a credential":            "credential-named-token",
		"an MCP server's header":                       "Bearer mcp-header-value",
		"an MCP server's env":                          "mcp-env-value",
		"what an MCP server's url's reference held":    "url-path-secret-value",
		"the run's own export token":                   "the-runs-export-token",
		"the run's Gitlet credential":                  "the-runs-gitlet-token",
		"the seeded login's access token":              "login-access-value",
		"the seeded login's refresh token":             "login-refresh-value",
	} {
		if !slices.Contains(handed.Values, want) {
			t.Errorf("%s (%q) is not kept for the scrub: %q", class, want, handed.Values)
		}
	}
	for class, not := range map[string]string{
		"the seat's handle":                  "agent-swe-the-seat",
		"the seat's email":                   "agent-swe@example.com",
		"a setting":                          "production-settings",
		"the telemetry's protocol":           "http/protobuf",
		"the unresolved text of a reference": "${NPM_TOKEN}",
		"a setup step's file":                "//registry.example.com/:_authToken=${NPM_TOKEN}",
		"a setup step's script":              "git config user.name agent-swe-the-seat",
	} {
		if slices.Contains(handed.Values, not) {
			t.Errorf("%s (%q) is kept for the scrub, and is no secret: %q", class, not, handed.Values)
		}
	}
	seen := map[string]bool{}
	for _, v := range handed.Values {
		if strings.TrimSpace(v) == "" || seen[v] {
			t.Errorf("kept %q blank or twice: %q", v, handed.Values)
		}
		seen[v] = true
	}
	if handed.Logins[".claude/.credentials.json"] != login {
		t.Errorf("the login's place is kept as %v, want it read again from %s", handed.Logins, login)
	}
	raw, err := handed.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := decodeHanded(raw)
	if err != nil || !reflect.DeepEqual(back, handed) {
		t.Fatalf("the secrets read back as %+v, %v; want %+v", back, err, handed)
	}
}

// SECRETS PAST WHAT A ROW KEEPS ARE NEVER KEPT IN PART: encoded past
// [MaxHandedBytes] they are refused whole, so the job's captures are refused
// saying so rather than scrubbed of only some of them.
//
// Mutation: cut the values to fit, and a capture is scrubbed of less than the
// box was handed.
func TestSecretsPastWhatARowKeepsAreRefusedWhole(t *testing.T) {
	t.Parallel()
	handed := HandedSecrets{Values: []string{strings.Repeat("k", MaxHandedBytes)}}
	if _, err := handed.encode(); err == nil {
		t.Fatal("secrets past the row's bound were encoded")
	}
}

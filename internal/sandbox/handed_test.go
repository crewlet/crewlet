package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// EVERY SECRET A LAUNCH HANDS ITS BOX IS KEPT FOR ITS SESSION'S SCRUB, class by
// class, from the very request the job is started with: the job's environment
// — a token written in the company's document as much as one resolved from the
// vault — its MCP servers' env and headers, its setup steps' env and the files
// they write, and every string of the login the box was seeded with, read from
// the file it was seeded from; and where that login lives, so a capture reads
// it again in the box and on the host. Nothing blank and nothing twice, and the
// whole of it round-trips through what the row's seal covers.
//
// Mutation: leave a class out of handedSecrets, read the login only at the
// capture, or keep a blank value, and a check below goes red.
func TestEverySecretALaunchHandsItsBoxIsKeptForTheScrub(t *testing.T) {
	t.Parallel()
	login := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(login, []byte(`{"claudeAiOauth":{"accessToken":"login-access-value",`+
		`"refreshToken":"login-refresh-value","scopes":["user:inference"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := LaunchRequest{
		Spec: Spec{CredentialFiles: map[string]string{".claude/.credentials.json": login}},
		Setup: []SetupStep{{
			Env:   map[string]string{"NPM_TOKEN": "setup-env-value"},
			Files: map[string]string{"/home/user/.npmrc": "//registry.example.com/:_authToken=setup-file-value"},
		}},
		MCPServers: map[string]MCPServer{
			"linear": {Name: "linear", Transport: TransportHTTP, URL: "https://example.com/mcp",
				Headers: map[string]string{"Authorization": "Bearer mcp-header-value"}},
			"files": {Name: "files", Transport: TransportStdio, Command: "mcp-files",
				Env: map[string]string{"API_TOKEN": "mcp-env-value"}},
		},
	}
	env := map[string]string{
		"GITHUB_TOKEN": "literal-env-token", "VAULT_TOKEN": "resolved-env-token",
		"BLANK": "  ", "AGAIN": "literal-env-token",
	}
	handed := handedSecrets(req, env)
	for class, want := range map[string]string{
		"a token written in the document":  "literal-env-token",
		"a token resolved from the vault":  "resolved-env-token",
		"an MCP server's header":           "Bearer mcp-header-value",
		"an MCP server's env":              "mcp-env-value",
		"a setup step's env":               "setup-env-value",
		"a file a setup step writes":       "//registry.example.com/:_authToken=setup-file-value",
		"the seeded login's access token":  "login-access-value",
		"the seeded login's refresh token": "login-refresh-value",
	} {
		if !slices.Contains(handed.Values, want) {
			t.Errorf("%s (%q) is not kept for the scrub: %q", class, want, handed.Values)
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

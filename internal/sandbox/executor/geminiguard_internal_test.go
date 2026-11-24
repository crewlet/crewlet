package executor

import (
	"cmp"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/prereqtest"
)

// These cases hold a Gemini CLI run's guard against another MCP server
// spelling the bridge's tools (geminiShadowGuard, the helper's mode
// `unshadowed`) and the record's word on it.

// A SESSION RECORD READS UNSHADOWED ONLY AS WRITTEN: exactly `unshadowed yes`
// before the release, and only for a harness whose run guards it; `no`, a
// padded or later line, a second line after a first `no`, or a harness with no
// guard, read as not checked.
//
// Mutation: read the line by its prefix, after the release, or for any
// harness, and a case goes red.
func TestASessionRecordReadsUnshadowedOnlyAsWritten(t *testing.T) {
	t.Parallel()
	head := "id " + ghSession + "\ncontinued no\nwhy \n"
	for name, tc := range map[string]struct {
		h    Harness
		text string
		read bool
	}{
		"as written":              {GeminiCLI{}, head + "unshadowed yes\nrelease 0.70.0\n", true},
		"none":                    {GeminiCLI{}, head + "release 0.70.0\n", false},
		"no":                      {GeminiCLI{}, head + "unshadowed no\nrelease 0.70.0\n", false},
		"padded":                  {GeminiCLI{}, head + "unshadowed yes \nrelease 0.70.0\n", false},
		"after the release":       {GeminiCLI{}, head + "release 0.70.0\nunshadowed yes\n", false},
		"a second line":           {GeminiCLI{}, head + "unshadowed no\nunshadowed yes\nrelease 0.70.0\n", false},
		"a harness with no guard": {QwenCode{}, head + "unshadowed yes\nrelease 0.28.4\n", false},
	} {
		if got := parseSessionRecord(tc.text).unshadowedBy(tc.h); got != tc.read {
			t.Errorf("%s: unshadowed %t; want %t", name, got, tc.read)
		}
	}
}

// ghGuardCase is a box's settings for the helper's guard: files under the
// home and the workspace, the environment beside them (ROOT the files' root),
// and the working directory, the workspace where dir is "".
type ghGuardCase struct {
	files map[string]string
	env   []string
	dir   string
	yes   bool
}

// THE GEMINI CLI HELPER FINDS EVERY SERVER THAT SPELLS THE BRIDGE'S TOOLS: in
// each source 0.70.0 loads a server from (the user file but its own
// `crewlet`, the workspace's, both system files at their variables' paths, an
// extension, a linked one), a server sanitizing to `crewlet_*`, `crewlet x`,
// `mcp_crewlet_*`, `mcp` or `crewlet` itself, and `mcp.serverCommand`; an
// unparsable file; and a sign-in other than a key or Vertex AI each fail it.
// Names that spell no such tool (`Crewlet_x`, `crewlet-x`, `crewlet.x`),
// comments, Vertex AI and a workspace at the home, the user's file, pass.
//
// Mutation: drop a source, the sanitizing, a predicate, the sign-in's check or
// the home's workspace rule, and a case goes red.
func TestTheGeminiHelperFindsEveryServerThatSpellsTheBridgesTools(t *testing.T) {
	t.Parallel()
	node := geminiNodeInternal(t)
	user := func(servers, auth string) string {
		return `{"mcpServers":{"crewlet":{"httpUrl":"http://127.0.0.1:1/mcp"}` + servers +
			`},"security":{"auth":{"selectedType":"` + auth + `"}}}`
	}
	server := func(name string) string { return `{"mcpServers":{` + ohQuote(name) + `:{"command":"x"}}}` }
	key := user("", "gemini-api-key")
	for name, tc := range map[string]ghGuardCase{
		"the bridge alone":        {files: map[string]string{"home/.gemini/settings.json": key}, yes: true},
		"vertex":                  {files: map[string]string{"home/.gemini/settings.json": user("", "vertex-ai")}, yes: true},
		"comments":                {files: map[string]string{"home/.gemini/settings.json": "// a note\n" + key + "/* end */"}, yes: true},
		"upper case":              {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("Crewlet_x")}, yes: true},
		"a dash":                  {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("crewlet-x")}, yes: true},
		"a dot":                   {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("crewlet.x")}, yes: true},
		"a login":                 {files: map[string]string{"home/.gemini/settings.json": user("", "oauth-personal")}},
		"no sign-in":              {files: map[string]string{"home/.gemini/settings.json": `{"mcpServers":{"crewlet":{}}}`}},
		"a login override":        {files: map[string]string{"home/.gemini/settings.json": key}, env: []string{"GEMINI_CLI_AUTH_OVERRIDE=oauth-personal"}},
		"a workspace login":       {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": `{"security":{"auth":{"selectedType":"oauth-personal"}}}`}},
		"user crewlet_x":          {files: map[string]string{"home/.gemini/settings.json": user(`,"crewlet_x":{}`, "gemini-api-key")}},
		"workspace crewlet_x":     {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("crewlet_x")}},
		"workspace crewlet x":     {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("crewlet x")}},
		"workspace mcp_crewlet_x": {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("mcp_crewlet_x")}},
		"workspace mcp":           {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("mcp")}},
		"workspace crewlet":       {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": server("crewlet")}},
		"a server command":        {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": `{"mcp":{"serverCommand":"x"}}`}},
		"system crewlet_x":        {files: map[string]string{"home/.gemini/settings.json": key, "sys/settings.json": server("crewlet_x")}},
		"system defaults crewlet": {files: map[string]string{"home/.gemini/settings.json": key, "sys/system-defaults.json": server("crewlet")}},
		"system defaults at its variable's path": {files: map[string]string{"home/.gemini/settings.json": key, "elsewhere/defaults.json": server("crewlet_x")},
			env: []string{"GEMINI_CLI_SYSTEM_DEFAULTS_PATH=ROOT/elsewhere/defaults.json"}},
		"the workspace at the home": {files: map[string]string{"home/.gemini/settings.json": key}, dir: "home", yes: true},
		"an extension":              {files: map[string]string{"home/.gemini/settings.json": key, "home/.gemini/extensions/foo/gemini-extension.json": server("crewlet_x")}},
		"a locked extension":        {files: map[string]string{"home/.gemini/settings.json": key, "home/.gemini/extensions/foo.lock/gemini-extension.json": server("crewlet_x")}, yes: true},
		"a linked extension":        {files: map[string]string{"home/.gemini/settings.json": key, "home/.gemini/extensions/foo/.gemini-extension-install.json": `{"type":"link","source":"LINKED"}`, "linked/gemini-extension.json": server("crewlet_x")}},
		"an unparsable user file":   {files: map[string]string{"home/.gemini/settings.json": key + "}"}},
		"an unparsable workspace":   {files: map[string]string{"home/.gemini/settings.json": key, "work/.gemini/settings.json": "{"}},
	} {
		if got := ghGuard(t, node, tc); got != tc.yes {
			t.Errorf("%s: the guard says %t; want %t", name, got, tc.yes)
		}
	}
}

// THE GEMINI CLI HELPER'S SIGN-IN HOLDS WHICHEVER LATER FILE 0.70.0 SKIPS: a
// login under a system file or system defaults naming a key fails, since
// 0.70.0 skips a system file that is not root-owned and signs in by the login,
// and so do a user file naming no sign-in under a system key and a key under a
// system login; a key under system Vertex AI, or over system defaults naming a
// login, passes.
//
// Mutation: check the merged sign-in alone, or fail a user file naming none
// and check the merged one otherwise, and a case goes red.
func TestTheGeminiHelpersSignInHoldsWhicheverLaterFileGeminiSkips(t *testing.T) {
	t.Parallel()
	node := geminiNodeInternal(t)
	bridge := `"mcpServers":{"crewlet":{"httpUrl":"http://127.0.0.1:1/mcp"}}`
	signIn := func(auth string) string { return `{"security":{"auth":{"selectedType":"` + auth + `"}}}` }
	none := `{` + bridge + `}`
	login := `{` + bridge + `,"security":{"auth":{"selectedType":"oauth-personal"}}}`
	key := `{` + bridge + `,"security":{"auth":{"selectedType":"gemini-api-key"}}}`
	for name, tc := range map[string]ghGuardCase{
		"a login under a system key":            {files: map[string]string{"home/.gemini/settings.json": login, "sys/settings.json": signIn("gemini-api-key")}},
		"a login under system defaults' key":    {files: map[string]string{"home/.gemini/settings.json": login, "sys/system-defaults.json": signIn("gemini-api-key")}},
		"no sign-in under a system key":         {files: map[string]string{"home/.gemini/settings.json": none, "sys/settings.json": signIn("gemini-api-key")}},
		"no sign-in under system defaults' key": {files: map[string]string{"home/.gemini/settings.json": none, "sys/system-defaults.json": signIn("gemini-api-key")}},
		"a key under a system login":            {files: map[string]string{"home/.gemini/settings.json": key, "sys/settings.json": signIn("oauth-personal")}},
		"a key under system Vertex AI":          {files: map[string]string{"home/.gemini/settings.json": key, "sys/settings.json": signIn("vertex-ai")}, yes: true},
		"a key over system defaults' login":     {files: map[string]string{"home/.gemini/settings.json": key, "sys/system-defaults.json": signIn("oauth-personal")}, yes: true},
	} {
		if got := ghGuard(t, node, tc); got != tc.yes {
			t.Errorf("%s: the guard says %t; want %t", name, got, tc.yes)
		}
	}
}

// geminiNodeInternal is the node the helper is run under here.
func geminiNodeInternal(t *testing.T) string {
	t.Helper()
	return prereqtest.OnPath(t, "node", "the Gemini CLI helper runs under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
}

// ghGuard runs the helper's guard over tc's files in a home and a workspace of
// their own, the system files at their variables' paths, and answers whether
// it said yes.
func ghGuard(t *testing.T, node string, tc ghGuardCase) bool {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"home", "work", "sys"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range tc.files {
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(strings.ReplaceAll(body, "LINKED", filepath.Join(root, "linked"))),
			0o600); err != nil {
			t.Fatal(err)
		}
	}
	helper, err := filepath.Abs("gemini_helper.mjs")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), node, helper, "unshadowed", config.ToolServerName)
	cmd.Dir = filepath.Join(root, cmp.Or(tc.dir, "work"))
	cmd.Env = []string{"HOME=" + filepath.Join(root, "home"), "PATH=" + os.Getenv("PATH"),
		"GEMINI_CLI_SYSTEM_SETTINGS_PATH=" + filepath.Join(root, "sys", "settings.json")}
	for _, e := range tc.env {
		cmd.Env = append(cmd.Env, strings.ReplaceAll(e, "ROOT", root))
	}
	out, err := cmd.CombinedOutput()
	var exit interface{ ExitCode() int }
	switch {
	case err == nil:
		return strings.TrimSpace(string(out)) == ""
	case errors.As(err, &exit) && exit.ExitCode() == 1 && strings.HasPrefix(string(out), "no: "):
		return false
	}
	t.Fatalf("the helper's guard: %v: %s", err, out)
	return false
}

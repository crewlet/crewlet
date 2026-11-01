package harness

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A PER-CALL CONFIGURATION DIRECTORY IS THE CALL'S OWN AND HOLDS WHAT THE HARNESS
// READS WHEN IT STARTS.
//
// work_config_env exists for OpenCode 2, which reads its configuration —
// the tool policy, and the agent file that carries the seat's system prompt —
// from the one directory a variable names once its project walk is off. So
// the variable must point inside the child's own working directory, and the
// profile's `in: work` seed and its discovered agent file must both be there
// at exactly that path when the child starts. Asked of the child: the
// directory is removed on release.
func TestAWorkConfigDirIsInsideTheCallAndHoldsWhatTheHarnessReads(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t,
		map[string]string{"FAKE_READ_UNDER_ENV": "VENDOR_CONFIG_DIR:settings.json,agents/seat.md"},
		map[string]any{
			"work_config_env": map[string]any{"VENDOR_CONFIG_DIR": ".vendor-config"},
			"seed_files": []any{map[string]any{
				"path": ".vendor-config/settings.json", "in": "work", "content": `{"tools":"none"}`,
			}},
			"system_prompt": map[string]any{
				"args": []any{"--agent", "seat"},
				"file": map[string]any{
					"name": ".vendor-config/agents/seat.md", "discovered": "selected",
					"template": "---\nmode: primary\n---\n{system}\n",
				},
			},
		})
	comp, err := ask(t, p, llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent CTO at Nimbus."},
		{Role: llm.RoleUser, Content: "hello"},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	cwd, dir := envValue(comp.Content, "CWD"), envValue(comp.Content, "VENDOR_CONFIG_DIR")
	// Compared RESOLVED: the child's working directory is what the kernel
	// reports, symbolic links followed (macOS keeps temporary directories
	// under /var, a link to /private/var), while the variable is the path
	// the engine built. Both name one directory, which is the claim.
	if cwd == "" || resolved(t, dir) != filepath.Join(resolved(t, cwd), ".vendor-config") {
		t.Fatalf("VENDOR_CONFIG_DIR = %q with the child in %q — want the call's own "+
			"directory's .vendor-config:\n%s", dir, cwd, comp.Content)
	}
	for rel, want := range map[string]string{
		"settings.json":  `{"tools":"none"}`,
		"agents/seat.md": "---\nmode: primary\n---\nYou are Agent CTO at Nimbus.\n",
	} {
		encoded := envValue(comp.Content, rel)
		body, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if encoded == "" || decodeErr != nil || string(body) != want {
			t.Errorf("%s under the config dir = %q, want %q — the harness reads it from "+
				"there when it starts:\n%s", rel, body, want, comp.Content)
		}
	}
}

// THE BUILT-IN OPENCODE PROFILE'S CHILD READS NO PROJECT CONFIGURATION AND ONLY
// THE CALL'S OWN. Both variables, with exactly these values: without the
// first, every ancestor's opencode.json and .opencode/ is configuration for
// every seat — measured on 2.0.32, an allow-all one handed every call a shell
// and an ancestor plugin ran its code — and without the second the seat's agent
// file and tool policy are read from nowhere.
//
// The switch is the name OpenCode 2 reads FIRST, OPENCODE_CONFIG_PROJECT_DISABLE:
// it reads `OPENCODE_CONFIG_PROJECT_DISABLE ?? OPENCODE_DISABLE_PROJECT_CONFIG`,
// so a child given only the second, beside any value of the first — an empty
// one included — walked every ancestor again. Which is why the operator's
// own spelling of the second, handed through env, changes nothing here:
// the first is set, so the second is never read.
//
// And ONLY A CALL gets the directory: a command run in a home — a credential
// store's import and export, a login — has no working directory, and pointing
// the variable at the home would put one call's system prompt where every
// concurrent call of the seat reads it. The walk stays off for those too.
func TestTheOpenCodeChildReadsOnlyTheCallsOwnConfiguration(t *testing.T) {
	t.Parallel()
	p, ok := harnessprofile.Builtin("opencode")
	if !ok {
		t.Fatal("no built-in opencode profile")
	}
	root := t.TempDir()
	call := &Checkout{
		Home:  filepath.Join(root, "seats", "s", "home"),
		Cache: filepath.Join(root, "seats", "s", "cache"),
		Work:  filepath.Join(root, "seats", "s", "work", "call"),
	}
	operator := map[string]string{"OPENCODE_DISABLE_PROJECT_CONFIG": "0"}
	env := childEnv(p, call, operator, Auth{Mode: AuthSubscription})
	if got := env["OPENCODE_CONFIG_PROJECT_DISABLE"]; got != "1" {
		t.Errorf("OPENCODE_CONFIG_PROJECT_DISABLE = %q, want 1 — the name OpenCode reads "+
			"first, which any value of overrules the older spelling", got)
	}
	if got, want := env["OPENCODE_CONFIG_DIR"], filepath.Join(call.Work, ".crewlet-config"); got != want {
		t.Errorf("OPENCODE_CONFIG_DIR = %q, want %q", got, want)
	}

	home := childEnv(p, &Checkout{Home: call.Home, Cache: call.Cache}, operator, Auth{Mode: AuthSubscription})
	if got := home["OPENCODE_CONFIG_PROJECT_DISABLE"]; got != "1" {
		t.Errorf("a command in a home: OPENCODE_CONFIG_PROJECT_DISABLE = %q, want 1 — an "+
			"ancestor plugin is code, whichever command loads it", got)
	}
	if got, set := home["OPENCODE_CONFIG_DIR"]; set {
		t.Errorf("a command in a home was handed OPENCODE_CONFIG_DIR=%q, which only a call "+
			"has a directory for", got)
	}
}

// EVERY ISOLATION VARIABLE THE PROFILE DECLARES IS ONE THE CHILD IS GIVEN, AND
// GIVEN INSIDE THE SEAT — or, for a vendor's switch, given exactly the value
// the profile declares. [harnessprofile.Profile.IsolationVariables] is what config
// and the profile validator refuse an override of; a name it lists that the
// child is not given would be refused for nothing, a directory the child is
// given outside the seat's is no isolation at all, and a switch the child is
// given any other value is a switch turned off.
func TestTheChildIsGivenEveryIsolationVariableInsideTheSeat(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seat := filepath.Join(root, "seats", "s")
	call := &Checkout{
		Home:  filepath.Join(seat, "home"),
		Cache: filepath.Join(seat, "cache"),
		Work:  filepath.Join(seat, "work", "call"),
	}
	for _, name := range harnessprofile.BuiltinNames() {
		p, _ := harnessprofile.Builtin(name)
		env := childEnv(p, call, map[string]string{"ANYTHING": "x"}, Auth{Mode: AuthSubscription})
		for _, variable := range p.IsolationVariables() {
			value, set := env[variable]
			if want, isSwitch := p.IsolationEnv[variable]; isSwitch {
				if value != want {
					t.Errorf("%s: switch %s = %q (set %v), want %q", name, variable, value, set, want)
				}
				continue
			}
			if !set || !strings.HasPrefix(value, seat+string(filepath.Separator)) {
				t.Errorf("%s: %s = %q (set %v), want a path inside %s", name, variable, value, set, seat)
			}
		}
	}
}

// resolved is path with every symbolic link in it followed. The call's
// directory is removed on release, so the links are resolved on the deepest
// ancestor that still exists and the rest is joined back on.
func resolved(t *testing.T, path string) string {
	t.Helper()
	rest := ""
	for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return path
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
}

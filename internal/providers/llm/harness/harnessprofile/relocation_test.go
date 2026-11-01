package harnessprofile

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A RELOCATION POINTS INSIDE ITS ROOT OR IT RELOCATES NOTHING. config_env and
// work_config_env each move a vendor's directory into the seat's home or the
// call's working directory; one that climbs out of it, names nothing, or names
// a variable the engine already points at the home undoes the isolation it is
// declared for. config_env's directory was never checked: `../..` loaded and
// moved the vendor's sessions and login outside the seat.
func TestARelocationStaysInsideItsRoot(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"config_env climbing out":     {map[string]any{"config_env": map[string]any{"VENDOR_HOME": "../.."}}, "stay inside the seat home"},
		"config_env empty":            {map[string]any{"config_env": map[string]any{"VENDOR_HOME": " "}}, "config_env.VENDOR_HOME is empty"},
		"config_env naming HOME":      {map[string]any{"config_env": map[string]any{"HOME": ".x"}}, "may not name HOME"},
		"work_config_env climbing":    {map[string]any{"work_config_env": map[string]any{"VENDOR_CONFIG": "../cfg"}}, "stay inside the per-call"},
		"work_config_env absolute":    {map[string]any{"work_config_env": map[string]any{"VENDOR_CONFIG": "/etc/vendor"}}, "stay inside the per-call"},
		"work_config_env empty":       {map[string]any{"work_config_env": map[string]any{"VENDOR_CONFIG": ""}}, "work_config_env.VENDOR_CONFIG is empty"},
		"work_config_env naming HOME": {map[string]any{"work_config_env": map[string]any{"HOME": ".x"}}, "may not name HOME"},
		"work_config_env naming an XDG home": {
			map[string]any{"work_config_env": map[string]any{"XDG_CONFIG_HOME": ".x"}}, "may not name XDG_CONFIG_HOME",
		},
		"config_env naming an XDG home": {
			map[string]any{"config_env": map[string]any{"XDG_CONFIG_HOME": ".x"}}, "config_env may not name XDG_CONFIG_HOME",
		},
		"config_env naming TMPDIR": {
			map[string]any{"config_env": map[string]any{"TMPDIR": ".tmp"}}, "config_env may not name TMPDIR",
		},
		"one variable, two roots": {map[string]any{
			"config_env":      map[string]any{"VENDOR_DIR": ".vendor"},
			"work_config_env": map[string]any{"VENDOR_DIR": ".vendor"},
		}, "both name VENDOR_DIR"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refused(t, tc.extra, tc.want)
		})
	}
	p, err := loadCustom(map[string]any{
		"config_env":      map[string]any{"VENDOR_HOME": ".vendor"},
		"work_config_env": map[string]any{"VENDOR_CONFIG": ".vendor-config"},
	})
	if err != nil {
		t.Fatalf("a relocation inside each root was refused: %v", err)
	}
	if got := p.IsolationVariables(); !slices.Equal(got, append(slices.Clone(HomeIsolation), "VENDOR_HOME", "VENDOR_CONFIG")) {
		t.Errorf("IsolationVariables = %v", got)
	}
}

// NOTHING LAYERED OVER THE ISOLATION MAY REPLACE IT. The child's environment
// applies the profile's env and passthrough after the variables that point the
// CLI at its own home, so one named there replaces the seat's home — or the
// call's configuration directory — with whatever it says, and says nothing.
func TestTheProfilesEnvMayNotReplaceTheIsolation(t *testing.T) {
	t.Parallel()
	relocated := map[string]any{
		"config_env":      map[string]any{"VENDOR_HOME": ".vendor"},
		"work_config_env": map[string]any{"VENDOR_CONFIG": ".vendor-config"},
	}
	with := func(more map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range relocated {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}
	for _, name := range append(slices.Clone(HomeIsolation), "VENDOR_HOME", "VENDOR_CONFIG") {
		refused(t, with(map[string]any{"env": map[string]any{name: "/somewhere"}}), "env names "+name)
		refused(t, with(map[string]any{"passthrough_env": []any{name}}), "passthrough_env names "+name)
	}
	if _, err := loadCustom(with(map[string]any{
		"env":             map[string]any{"VENDOR_NO_UPDATE": "1"},
		"passthrough_env": []any{"VENDOR_REGION"},
	})); err != nil {
		t.Errorf("an ordinary env and passthrough were refused: %v", err)
	}
}

// AN ISOLATION VARIABLE'S LAYER IS THE ONE THE CHILD IS GIVEN. A refusal that
// tells an operator where a variable's value is changed reads it here, and the
// three profile fields refuse each other's variables, so every variable has
// exactly one layer — the engine's own for HOME, the XDG homes and TMPDIR,
// which no field may relocate — and a layer for exactly the names
// IsolationVariables lists, so the two can never describe different sets.
func TestAnIsolationVariablesLayerIsTheOneTheChildIsGiven(t *testing.T) {
	t.Parallel()
	p, err := loadCustom(map[string]any{
		"config_env":      map[string]any{"VENDOR_HOME": ".vendor"},
		"work_config_env": map[string]any{"VENDOR_CONFIG": ".vendor-config"},
		"isolation_env":   map[string]any{"VENDOR_NO_WALK": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]IsolationLayer{
		"HOME":            IsolationHome,
		"TMPDIR":          IsolationHome,
		"XDG_CACHE_HOME":  IsolationHome,
		"XDG_CONFIG_HOME": IsolationHome,
		"VENDOR_HOME":     IsolationConfigEnv,
		"VENDOR_CONFIG":   IsolationWorkConfigEnv,
		"VENDOR_NO_WALK":  IsolationSwitch,
	}
	names := p.IsolationVariables()
	for _, name := range names {
		layer, ok := p.IsolationLayerOf(name)
		if !ok {
			t.Errorf("%s is an isolation variable with no layer", name)
			continue
		}
		if expected, listed := want[name]; listed && layer != expected {
			t.Errorf("IsolationLayerOf(%s) = %s, want %s", name, layer, expected)
		}
	}
	for name := range want {
		if !slices.Contains(names, name) {
			t.Errorf("%s has a layer but is not an isolation variable", name)
		}
	}
	if len(names) != len(slices.Compact(slices.Sorted(slices.Values(names)))) {
		t.Errorf("IsolationVariables lists a name twice, so two layers set it: %v", names)
	}
	if layer, ok := p.IsolationLayerOf("VENDOR_LOG_LEVEL"); ok {
		t.Errorf("an ordinary variable is reported as set by %s", layer)
	}
}

// A RELOCATION TAKES WHAT THE PROFILE KEEPS UNDER IT, OR IT DOES NOT LOAD. The
// CLI finds a login, a session store, a tool policy or a discovered agent file
// through the variable that points at the directory holding it, so an override
// moving the directory and leaving those paths behind loaded and ran every
// call without them: OpenCode answered "Agent not found" with its agent file
// and policy left in `.crewlet-config` — and with the agent file alone moved,
// ran with its own default tools — and Codex ran signed in to nothing, never
// pruning a session. Judged against the built-in's layout, so a custom
// profile keeping a path beside its relocation is its own business.
func TestAMovedRelocationTakesWhatItKeepsWithIt(t *testing.T) {
	t.Parallel()
	const agent = "---\ndescription: d\n---\n{system}\n"
	opencodePrompt := func(dir string) map[string]any {
		return map[string]any{"file": map[string]any{
			"name": dir + "/agents/crewlet-seat.md", "discovered": "selected", "template": agent,
		}}
	}
	opencodeSeed := func(dir string) []any {
		return []any{map[string]any{"path": dir + "/opencode.json", "in": "work", "content": "{}"}}
	}
	opencodeAppend := func(dir string) map[string]any {
		return map[string]any{"file": map[string]any{"name": dir + "/AGENTS.md", "discovered": "always"}}
	}
	for name, tc := range map[string]struct {
		agent     string
		overrides map[string]any
		left      []string // nil: loads
	}{
		"opencode's directory moved alone": {
			agent:     "opencode",
			overrides: map[string]any{"work_config_env": map[string]any{"OPENCODE_CONFIG_DIR": ".elsewhere"}},
			left: []string{
				`work_config_env moves OPENCODE_CONFIG_DIR from .crewlet-config to .elsewhere`,
				`seed_files[0].path ".crewlet-config/opencode.json"`,
				`system_prompt.file.name ".crewlet-config/agents/crewlet-seat.md"`,
				`append_system_prompt.file.name ".crewlet-config/AGENTS.md"`,
				"re-root them from .crewlet-config to .elsewhere",
			},
		},
		"opencode's directory moved with its agent file but not its policy": {
			agent: "opencode",
			overrides: map[string]any{
				"work_config_env": map[string]any{"OPENCODE_CONFIG_DIR": ".elsewhere"},
				"system_prompt":   opencodePrompt(".elsewhere"),
			},
			left: []string{
				`seed_files[0].path ".crewlet-config/opencode.json"`,
				`append_system_prompt.file.name ".crewlet-config/AGENTS.md"`,
			},
		},
		"opencode's directory moved with both prompt files but not its policy": {
			agent: "opencode",
			overrides: map[string]any{
				"work_config_env":      map[string]any{"OPENCODE_CONFIG_DIR": ".elsewhere"},
				"system_prompt":        opencodePrompt(".elsewhere"),
				"append_system_prompt": opencodeAppend(".elsewhere"),
			},
			left: []string{`seed_files[0].path ".crewlet-config/opencode.json"`},
		},
		"opencode's directory moved with everything under it": {
			agent: "opencode",
			overrides: map[string]any{
				"work_config_env":      map[string]any{"OPENCODE_CONFIG_DIR": ".elsewhere"},
				"system_prompt":        opencodePrompt(".elsewhere"),
				"append_system_prompt": opencodeAppend(".elsewhere"),
				"seed_files":           opencodeSeed(".elsewhere"),
			},
		},
		"codex's home moved alone": {
			agent:     "codex",
			overrides: map[string]any{"config_env": map[string]any{"CODEX_HOME": ".codex-elsewhere"}},
			left: []string{
				`config_env moves CODEX_HOME from .codex to .codex-elsewhere`,
				`credential_paths[0] ".codex/auth.json"`,
				`volatile_paths[0] ".codex/sessions"`,
			},
		},
		"codex's home moved with its login but not its sessions": {
			agent: "codex",
			overrides: map[string]any{
				"config_env":       map[string]any{"CODEX_HOME": ".codex-elsewhere"},
				"credential_paths": []any{".codex-elsewhere/auth.json"},
			},
			left: []string{`volatile_paths[0] ".codex/sessions"`},
		},
		"codex's home moved with everything under it": {
			agent: "codex",
			overrides: map[string]any{
				"config_env":       map[string]any{"CODEX_HOME": ".codex-elsewhere"},
				"credential_paths": []any{".codex-elsewhere/auth.json"},
				"volatile_paths":   []any{".codex-elsewhere/sessions"},
			},
		},
		"codex's home spelled with a trailing separator": {
			agent:     "codex",
			overrides: map[string]any{"config_env": map[string]any{"CODEX_HOME": ".codex/"}},
		},
		"codex's home moved to the home itself": {
			agent:     "codex",
			overrides: map[string]any{"config_env": map[string]any{"CODEX_HOME": "."}},
			left: []string{
				`config_env moves CODEX_HOME from .codex to .`,
				`credential_paths[0] ".codex/auth.json"`,
				`(".codex/auth.json" becomes "auth.json")`,
			},
		},
		// Another variable's directory reaches nothing CODEX_HOME's paths are
		// read through.
		"codex's home moved with another variable on its old directory": {
			agent:     "codex",
			overrides: map[string]any{"config_env": map[string]any{"CODEX_HOME": ".", "CODEX_EXTRA": ".codex"}},
			left:      []string{`credential_paths[0] ".codex/auth.json"`},
		},
		"codex's home moved to the home and its paths re-rooted": {
			agent: "codex",
			overrides: map[string]any{
				"config_env":       map[string]any{"CODEX_HOME": "."},
				"credential_paths": []any{"auth.json"},
				"volatile_paths":   []any{"sessions"},
			},
		},
		"codex's home moved inside its old directory, with its paths": {
			agent: "codex",
			overrides: map[string]any{
				"config_env":       map[string]any{"CODEX_HOME": ".codex/inner"},
				"credential_paths": []any{".codex/inner/auth.json"},
				"volatile_paths":   []any{".codex/inner/sessions"},
			},
		},
		// The parent of pi's .pi/agent holds every path pi kept there, and pi
		// reads none of them: it looks for $PI_CODING_AGENT_DIR/auth.json.
		"pi's directory moved to its parent": {
			agent:     "pi",
			overrides: map[string]any{"config_env": map[string]any{"PI_CODING_AGENT_DIR": ".pi"}},
			left:      []string{`config_env moves PI_CODING_AGENT_DIR from .pi/agent to .pi`},
		},
		"opencode's directory moved to the call's directory itself": {
			agent:     "opencode",
			overrides: map[string]any{"work_config_env": map[string]any{"OPENCODE_CONFIG_DIR": "."}},
			left:      []string{`work_config_env moves OPENCODE_CONFIG_DIR from .crewlet-config to .`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(tc.agent, tc.overrides)
			if tc.left == nil {
				if err != nil {
					t.Fatalf("a relocation that took its paths with it was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %v", tc.overrides)
			}
			for _, want := range tc.left {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}

	// A null is the empty value config_env already refuses, not a move that
	// strands anything: one refusal, not two.
	_, err := Load("codex", map[string]any{"config_env": map[string]any{"CODEX_HOME": nil}})
	if err == nil || !strings.Contains(err.Error(), "config_env.CODEX_HOME is empty") ||
		strings.Contains(err.Error(), "still lie under") {
		t.Errorf("a null CODEX_HOME: %v, want the empty value refused and nothing else", err)
	}
	// A custom profile has no built-in layout to strand anything from.
	if _, err := loadCustom(map[string]any{
		"config_env":       map[string]any{"VENDOR_HOME": ".vendor"},
		"credential_paths": []any{".vendor-login/auth.json"},
	}); err != nil {
		t.Errorf("a custom profile keeping its login beside its relocation was refused: %v", err)
	}
}

// WHAT MOVES WITH A DIRECTORY IS WHAT THE PROFILE KEEPS UNDER IT. The refusal
// of an entry's env naming a relocation variable says which fields have to
// move with a different directory, read from here.
func TestKeptUnderNamesTheFieldsThatMoveWithADirectory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		agent, variable string
		want            []string
	}{
		{"codex", "CODEX_HOME", []string{"credential_paths", "volatile_paths"}},
		{"opencode", "OPENCODE_CONFIG_DIR", []string{"seed_files", "system_prompt.file.name", "append_system_prompt.file.name"}},
		{"opencode", "OPENCODE_CONFIG_PROJECT_DISABLE", nil},
		{"codex", "HOME", nil},
	} {
		p, err := Load(tc.agent, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.KeptUnder(tc.variable); !slices.Equal(got, tc.want) {
			t.Errorf("%s KeptUnder(%s) = %v, want %v", tc.agent, tc.variable, got, tc.want)
		}
	}
}

// A seed path is checked at load the way seeding checks it at every call. The
// load-time check used to be UnderRoot against a stand-in root named `/seat`,
// so a path climbing out and back in by that very name loaded and was refused
// at every call.
func TestASeedPathThatClimbsOutAndBackInIsRefusedAtLoad(t *testing.T) {
	t.Parallel()
	refused(t, map[string]any{"seed_files": []any{map[string]any{
		"path": "../seat/settings.json", "content": "{}",
	}}}, "must be relative and stay inside")
}

// A LOGIN OR VOLATILE PATH IS CHECKED AT LOAD, AS A SEED PATH IS. Each is
// joined onto a home only at run time, where UnderRoot refuses one that climbs
// out on every call — so an override like `credential_paths: [../auth.json]`
// loaded, was admitted by every write path, and failed every call's seed. A
// login path names a FILE besides, read and carried by its base name, and two
// sharing a base name are one file in the shared directory and every bundle —
// as one named like a bundle's manifest is one entry with it. And a host path
// is adopted under its base name, so one whose base name no credential path
// has is a login `-from-host` adopts and no seat reads, which is what copilot
// did with gh's hosts.yml.
func TestALoginOrVolatilePathIsCheckedAtLoad(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"a credential path climbing out": {
			map[string]any{"credential_paths": []any{"../auth.json"}},
			`credential_paths[0] "../auth.json" must be relative and stay inside the seat home`,
		},
		"an absolute credential path": {
			map[string]any{"credential_paths": []any{"/etc/auth.json"}},
			`credential_paths[0] "/etc/auth.json" must be relative`,
		},
		"a credential path naming the home": {
			map[string]any{"credential_paths": []any{"."}},
			`credential_paths[0] "." must name a login FILE inside the seat home`,
		},
		"a credential path climbing back to a directory": {
			map[string]any{"credential_paths": []any{".vendor/auth.json", "x/.."}},
			`credential_paths[1] "x/.." must name a login FILE`,
		},
		"a credential path spelled as a directory": {
			map[string]any{"credential_paths": []any{".vendor/"}},
			`credential_paths[0] ".vendor/" must name a login FILE`,
		},
		"two credential paths with one file name": {
			map[string]any{"credential_paths": []any{".a/auth.json", ".b/auth.json"}},
			`credential_paths names ".a/auth.json" and ".b/auth.json", which share the file name auth.json`,
		},
		"a credential path named like a bundle's manifest": {
			map[string]any{"credential_paths": []any{".vendor/auth.json", ".vendor/" + BundleManifest}},
			`credential_paths[1] ".vendor/` + BundleManifest + `" has the file name ` + BundleManifest +
				`, which every bundle keeps its manifest under`,
		},
		"a host credential path climbing out": {
			map[string]any{"host_credential_paths": []any{"../.ssh/id_ed25519"}},
			`host_credential_paths[0] "../.ssh/id_ed25519" must be relative and stay inside the person's home`,
		},
		"a host credential path naming the home": {
			map[string]any{"host_credential_paths": []any{"./"}},
			`host_credential_paths[0] "./" must name a login FILE inside the person's home`,
		},
		"a host credential path no seat reads": {
			map[string]any{
				"credential_paths":      []any{".vendor/auth.json"},
				"host_credential_paths": []any{".vendor/auth.json", ".config/gh/hosts.yml"},
			},
			`host_credential_paths[1] ".config/gh/hosts.yml" has a file name (hosts.yml) no credential_paths entry has`,
		},
		"a volatile path climbing out": {
			map[string]any{"volatile_paths": []any{".vendor/sessions", "../sessions"}},
			`volatile_paths[1] "../sessions" must be relative and stay inside the seat home`,
		},
		"an absolute volatile path": {
			map[string]any{"volatile_paths": []any{"/tmp"}},
			`volatile_paths[0] "/tmp" must be relative`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refused(t, tc.extra, tc.want)
		})
	}
	if _, err := loadCustom(map[string]any{
		"credential_paths": []any{".vendor/auth.json", ".vendor/a/../token.json"},
		// A host file lives wherever the person's CLI keeps it, under a
		// file name a seat reads.
		"host_credential_paths": []any{".vendor/auth.json", ".config/vendor/token.json"},
		// A volatile path names a directory as often as a file.
		"volatile_paths": []any{".vendor/sessions/", ".vendor/history.jsonl"},
	}); err != nil {
		t.Errorf("login and volatile paths inside their homes were refused: %v", err)
	}
}

// A SEED PATH NAMES A FILE, OR EVERY CALL FAILS. Seeding writes the content to
// exactly the path, and staying inside the scope is not enough: `.` and `x/..`
// clean to the scope directory itself, a write onto which is EISDIR, and
// `.crewlet-config/` writes a regular file where OpenCode's configuration
// directory has to be, so the seat's agent file under it is ENOTDIR — on every
// call, with a profile that loaded and an entry every write path accepted.
// Judged on the path AS WRITTEN, as system_prompt.file.name is, because
// cleaning is what hides a trailing separator.
func TestASeedPathMustNameAFile(t *testing.T) {
	t.Parallel()
	for _, path := range []string{".", "./", ".crewlet-config/", ".crewlet-config/..", "x/.."} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			refused(t, map[string]any{"seed_files": []any{map[string]any{
				"path": path, "in": "work", "content": "{}",
			}}}, "seed_files[0].path "+strconv.Quote(path)+" must name a FILE inside the work directory")
		})
	}
	// Climbing within the scope is still a file in it: the rule is about the
	// last element, not about every `..`.
	if _, err := loadCustom(map[string]any{"seed_files": []any{map[string]any{
		"path": "a/../.crewlet-config/opencode.json", "in": "work", "content": "{}",
	}}}); err != nil {
		t.Errorf("a seed path naming a file inside its scope was refused: %v", err)
	}
}

// A FILE THE ENGINE WRITES IS NO PATH ANOTHER FIELD NEEDS. Each path's own
// shape cannot show it, because the conflict is between two fields:
// `.crewlet-config` names a file, and opencode needs that name as
// OPENCODE_CONFIG_DIR's directory, so a seed there made the agent file's write
// under it fail with ENOTDIR on every call — as did `.crewlet-config/agents`,
// and in the seat's home codex's `.codex`, which CODEX_HOME and its login
// need as a directory. Two writes of one file are refused for the same reason
// one of them is never read: a seed at the login path was carried back out as
// the login, and one at the system prompt's path replaced before the call.
func TestAFileTheProfileWritesIsNoPathAnotherFieldNeeds(t *testing.T) {
	t.Parallel()
	seed := func(in string, paths ...string) map[string]any {
		var files []any
		for _, path := range paths {
			files = append(files, map[string]any{"path": path, "in": in, "content": "{}"})
		}
		return map[string]any{"seed_files": files}
	}
	for name, tc := range map[string]struct {
		agent     string
		overrides map[string]any
		want      []string // nil: loads
	}{
		"opencode's configuration directory": {
			agent: "opencode", overrides: seed("work", ".crewlet-config"),
			want: []string{`seed_files[0].path ".crewlet-config" is the directory ` +
				`work_config_env.OPENCODE_CONFIG_DIR points the CLI at, in the work directory`},
		},
		"the directory opencode's agent file goes in": {
			agent: "opencode", overrides: seed("work", ".crewlet-config/agents"),
			want: []string{`seed_files[0].path ".crewlet-config/agents" is a FILE where ` +
				`system_prompt.file.name needs a directory`},
		},
		"opencode's agent file": {
			agent: "opencode", overrides: seed("work", ".crewlet-config/agents/crewlet-seat.md"),
			want: []string{"system_prompt.file.name", `is the file seed_files[0].path names too`},
		},
		"a file handed to the CLI by its path": {
			agent: "kimi-code", overrides: seed("work", "crewlet-seat.md"),
			want: []string{"system_prompt.file.name", `is the file seed_files[0].path names too`},
		},
		"codex's home": {
			agent: "codex", overrides: seed("home", ".codex"),
			want: []string{`seed_files[0].path ".codex" is the directory config_env.CODEX_HOME ` +
				`points the CLI at, in the home directory`},
		},
		"codex's login": {
			agent: "codex", overrides: seed("home", ".codex/auth.json"),
			want: []string{`seed_files[0].path ".codex/auth.json" is the file credential_paths[0] names too`},
		},
		"one seed under another": {
			agent: "codex", overrides: seed("home", ".codex/config.toml", ".codex/config.toml/x"),
			want: []string{`seed_files[0].path ".codex/config.toml" is a FILE where ` +
				`seed_files[1].path needs a directory`},
		},
		"one seed twice, spelled two ways": {
			agent: "codex", overrides: seed("home", ".codex/config.toml", ".codex/./config.toml"),
			want: []string{`seed_files[1].path ".codex/config.toml" is the file seed_files[0].path names too`},
		},
		"a seed where a relocation needs a directory": {
			agent: "custom",
			overrides: map[string]any{
				"binary": "my-cli", "complete_args": []any{"run"}, "output": "text",
				"config_env": map[string]any{"VENDOR_HOME": ".vendor/home"},
				"seed_files": []any{map[string]any{"path": ".vendor", "content": "{}"}},
			},
			want: []string{`seed_files[0].path ".vendor" is a FILE where config_env.VENDOR_HOME needs a directory`},
		},
		"settings beside opencode's policy": {
			agent:     "opencode",
			overrides: seed("work", ".crewlet-config/opencode.json", ".crewlet-config/tui.json"),
		},
		"settings in codex's home": {agent: "codex", overrides: seed("home", ".codex/config.toml")},
		"one name in each scope": {
			agent: "codex",
			overrides: map[string]any{"seed_files": []any{
				map[string]any{"path": "settings.json", "in": "home", "content": "{}"},
				map[string]any{"path": "settings.json", "in": "work", "content": "{}"},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(tc.agent, tc.overrides)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("paths that collide with nothing were refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %v", tc.overrides)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// THE OPENCODE PROFILE READS ONLY THE CALL'S OWN CONFIGURATION, AND WRITES
// EVERYTHING IT READS THERE. OpenCode 2 read `opencode.json`, agents and
// plugins from every ancestor of the working directory, so an allow-all config
// above the state directory handed every seat a shell; the profile switches the
// walk off and points OPENCODE_CONFIG_DIR inside the call. That variable
// replaces the global directory under the seat's home, so the tool policy and
// the seat's agent file must both be written under it, per call — a policy
// left in the home would be read by nothing, and the model would get the
// vendor's default tools.
func TestTheOpenCodeProfileWritesWhatItReadsIntoTheCallsOwnConfigDir(t *testing.T) {
	t.Parallel()
	p, err := Load("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.IsolationEnv["OPENCODE_CONFIG_PROJECT_DISABLE"] != "1" {
		t.Errorf("isolation_env OPENCODE_CONFIG_PROJECT_DISABLE = %q, want 1 — without it "+
			"every ancestor's opencode.json and .opencode/ is configuration for every seat",
			p.IsolationEnv["OPENCODE_CONFIG_PROJECT_DISABLE"])
	}
	dir := p.WorkConfigEnv["OPENCODE_CONFIG_DIR"]
	if dir == "" {
		t.Fatalf("work_config_env = %v, want OPENCODE_CONFIG_DIR inside the call's directory", p.WorkConfigEnv)
	}
	if _, inHome := p.ConfigEnv["OPENCODE_CONFIG_DIR"]; inHome {
		t.Error("OPENCODE_CONFIG_DIR is relocated into the seat home, which every concurrent call shares")
	}
	under := func(rel string) bool {
		return strings.HasPrefix(filepath.Clean(rel), filepath.Clean(dir)+string(filepath.Separator))
	}
	var policy *SeedFile
	for i := range p.SeedFiles {
		f := &p.SeedFiles[i]
		if strings.HasSuffix(f.Path, "opencode.json") {
			policy = f
		}
	}
	switch {
	case policy == nil:
		t.Error("the profile seeds no opencode.json, so nothing denies the CLI's local tools")
	case policy.Scope() != SeedWork || filepath.Clean(policy.Path) != filepath.Join(dir, "opencode.json"):
		t.Errorf("opencode.json is seeded at %s/%s, want work/%s — the only global config "+
			"OpenCode reads is the directory OPENCODE_CONFIG_DIR names",
			policy.Scope(), policy.Path, filepath.Join(dir, "opencode.json"))
	}
	if spf := p.SystemPrompt.File; !spf.Found() ||
		!under(spf.Name) || filepath.Dir(filepath.Clean(spf.Name)) != filepath.Join(dir, "agents") {
		t.Errorf("system_prompt.file = %+v, want a discovered file in %s/agents/", spf, dir)
	}
}

// AN ISOLATION SWITCH IS A VENDOR VARIABLE OF ITS OWN, WITH A VALUE, AND
// NOTHING LAYERED OVER IT MAY SET IT. isolation_env exists because OpenCode's
// project switch, written in env, was turned back off by
// `env: {OPENCODE_CONFIG_PROJECT_DISABLE: "0"}` — or by an empty value,
// which OpenCode reads as off — and every ancestor's configuration reached
// every seat again (measured on 2.0.32). So the switch is an isolation
// variable: refused in env and passthrough_env here, and in an entry's env by config
// through the same list. An empty value is refused because a vendor that reads
// `1` as on reads it as off; a name the engine already points at a directory
// would give the child two values; and a credential has no place in a value
// forwarded on every command and stored unredacted.
func TestAnIsolationSwitchIsAVendorVariableWithAValue(t *testing.T) {
	t.Parallel()
	switched := map[string]any{"isolation_env": map[string]any{"VENDOR_NO_PROJECT": "1"}}
	with := func(more map[string]any) map[string]any {
		out := map[string]any{"isolation_env": switched["isolation_env"]}
		for k, v := range more {
			out[k] = v
		}
		return out
	}
	for name, tc := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"an empty value": {
			map[string]any{"isolation_env": map[string]any{"VENDOR_NO_PROJECT": " "}},
			"isolation_env.VENDOR_NO_PROJECT is empty",
		},
		"a home variable": {
			map[string]any{"isolation_env": map[string]any{"HOME": "1"}},
			"isolation_env names HOME",
		},
		"a relocated variable": {map[string]any{
			"work_config_env": map[string]any{"VENDOR_CONFIG": ".vendor-config"},
			"isolation_env":   map[string]any{"VENDOR_CONFIG": "1"},
		}, "isolation_env names VENDOR_CONFIG"},
		"the switch in env":             {with(map[string]any{"env": map[string]any{"VENDOR_NO_PROJECT": "0"}}), "env names VENDOR_NO_PROJECT"},
		"the switch in passthrough_env": {with(map[string]any{"passthrough_env": []any{"VENDOR_NO_PROJECT"}}), "passthrough_env names VENDOR_NO_PROJECT"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refused(t, tc.extra, tc.want)
		})
	}

	p, err := loadCustom(with(map[string]any{
		"config_env": map[string]any{"VENDOR_HOME": ".vendor"},
		"env":        map[string]any{"VENDOR_NO_UPDATE": "1"},
	}))
	if err != nil {
		t.Fatalf("a switch beside an ordinary env was refused: %v", err)
	}
	if got := p.IsolationVariables(); !slices.Equal(got, append(slices.Clone(HomeIsolation), "VENDOR_HOME", "VENDOR_NO_PROJECT")) {
		t.Errorf("IsolationVariables = %v, want the switch listed, so an entry's env is refused it", got)
	}
	if clone := p.Clone(); clone.IsolationEnv["VENDOR_NO_PROJECT"] != "1" {
		t.Errorf("Clone dropped isolation_env: %v", clone.IsolationEnv)
	}

	for _, credential := range []string{"VENDOR_API_KEY", "VENDOR_TOKEN"} {
		p, err := loadCustom(map[string]any{"isolation_env": map[string]any{credential: "1"}})
		if err != nil {
			t.Fatalf("%s: %v", credential, err)
		}
		if err := p.ValidateCredentials("custom"); err == nil || !strings.Contains(err.Error(), "isolation_env names \""+credential+"\"") {
			t.Errorf("isolation_env naming %s: ValidateCredentials = %v, want a refusal naming it", credential, err)
		}
	}
}

// THE OPENCODE SWITCHES ARE THE NAMES OPENCODE 2 READS FIRST. It reads
// `OPENCODE_CONFIG_PROJECT_DISABLE ?? OPENCODE_DISABLE_PROJECT_CONFIG` and
// `OPENCODE_FILEWATCHER_DISABLE ?? OPENCODE_DISABLE_FILEWATCHER` (2.0.0 and
// 2.0.32 bundles), so a profile setting the second of either is overruled by
// any value of the first. The project switch is isolation and lives in
// isolation_env; the watcher is a setting and lives in env.
func TestTheOpenCodeSwitchesAreTheNamesItReadsFirst(t *testing.T) {
	t.Parallel()
	p, err := Load("opencode", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, fallback := range []string{"OPENCODE_DISABLE_PROJECT_CONFIG", "OPENCODE_DISABLE_FILEWATCHER"} {
		if _, set := p.Env[fallback]; set {
			t.Errorf("env sets %s, the spelling OpenCode reads only when the first is unset", fallback)
		}
		if _, set := p.IsolationEnv[fallback]; set {
			t.Errorf("isolation_env sets %s, the spelling OpenCode reads only when the first is unset", fallback)
		}
	}
	if _, inEnv := p.Env["OPENCODE_CONFIG_PROJECT_DISABLE"]; inEnv {
		t.Error("the project switch is in env, where an entry's env may replace it")
	}
	if p.Env["OPENCODE_FILEWATCHER_DISABLE"] != "1" {
		t.Errorf("env OPENCODE_FILEWATCHER_DISABLE = %q, want 1", p.Env["OPENCODE_FILEWATCHER_DISABLE"])
	}
	if !slices.Contains(p.IsolationVariables(), "OPENCODE_CONFIG_PROJECT_DISABLE") {
		t.Error("the project switch is not an isolation variable, so an entry's env is not refused it")
	}
}

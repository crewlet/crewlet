package cliprofile

import (
	"path/filepath"
	"slices"
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
	if p.Env["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" {
		t.Errorf("env OPENCODE_DISABLE_PROJECT_CONFIG = %q, want 1 — without it every "+
			"ancestor's opencode.json and .opencode/ is configuration for every seat",
			p.Env["OPENCODE_DISABLE_PROJECT_CONFIG"])
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
	if spf := p.SystemPromptFile; spf == nil || !spf.Discovered ||
		!under(spf.Name) || filepath.Dir(filepath.Clean(spf.Name)) != filepath.Join(dir, "agents") {
		t.Errorf("system_prompt_file = %+v, want a discovered file in %s/agents/", spf, dir)
	}
}

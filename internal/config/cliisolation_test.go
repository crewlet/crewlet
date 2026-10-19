package config

import (
	"errors"
	"strings"
	"testing"
)

// OPENCODE'S ISOLATION IS NOT AN OPERATOR'S TO UNDO FROM cli.env.
//
// The opencode profile sets OPENCODE_CONFIG_PROJECT_DISABLE=1 so OpenCode reads
// no configuration from the call's working directory or any ancestor of it:
// with the walk on, an allow-all opencode.json above the state directory handed
// every seat a shell and an ancestor plugin ran its code (measured on 2.0.32).
// cli.env is layered over the profile's environment, so a value written there —
// "0", or an empty one, which OpenCode reads as off — reopened the walk for
// every seat on the entry while validating clean. The switch is an isolation
// variable now, and cli.env naming it is refused as HOME is: on a write, and
// warned about on apply, because the entry still runs. OPENCODE_CONFIG_DIR, the
// call's own configuration directory, is refused the same way.
//
// And the refusal names the field that DOES take each of them. It named
// cli.overrides.config_env for every isolation variable, which refuses both of
// these: OPENCODE_CONFIG_DIR as work_config_env's, the switch as a variable
// "the engine already points at a directory" — so an operator who followed it
// met a second refusal.
//
// A passthrough of the engine's own value through cli.overrides is refused by
// the profile's validator, at the overrides.
func TestOpenCodesIsolationCannotBeUndoneFromCLIEnv(t *testing.T) {
	t.Parallel()
	const model = "anthropic/claude-sonnet-5"
	for _, tc := range []struct {
		variable string
		values   []string
		takenBy  string
	}{
		{"OPENCODE_CONFIG_PROJECT_DISABLE", []string{`"0"`, `""`, `"1"`}, "cli.overrides.isolation_env"},
		{"OPENCODE_CONFIG_DIR", []string{"/srv/opencode", `""`}, "cli.overrides.work_config_env"},
	} {
		for _, value := range tc.values {
			doc := opencodeEntry(model, "env: {"+tc.variable+": "+value+"}")
			path := "providers.llm.oc.cli.env." + tc.variable
			err := rejects(t, doc, path)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("%s=%s: want %v, got %v", tc.variable, value, ErrConflict, err)
			}
			if !strings.Contains(err.Error(), "a value here would replace that isolation for every seat") {
				t.Errorf("%s=%s: the refusal does not say why:\n%v", tc.variable, value, err)
			}
			if !strings.Contains(err.Error(), tc.takenBy) {
				t.Errorf("%s=%s: the refusal does not name %s, the field that takes it:\n%v",
					tc.variable, value, tc.takenBy, err)
			}
			if strings.Contains(err.Error(), "cli.overrides.config_env") {
				t.Errorf("%s=%s: the refusal sends the operator to cli.overrides.config_env, "+
					"which refuses this variable:\n%v", tc.variable, value, err)
			}
			assertAdmissionOnly(t, doc, path)
		}
	}

	doc := opencodeEntry(model, `overrides: {passthrough_env: [OPENCODE_CONFIG_PROJECT_DISABLE]}`)
	err := rejects(t, doc, "providers.llm.oc.cli.overrides")
	if !strings.Contains(err.Error(), "passthrough_env names OPENCODE_CONFIG_PROJECT_DISABLE") {
		t.Errorf("a passthrough of the switch is not refused by name:\n%v", err)
	}

	// An ordinary setting beside it is still the operator's.
	mustCompany(t, opencodeEntry(model, "env: {OPENCODE_LOG_LEVEL: WARN}"))
}

// A cli.env ISOLATION REFUSAL NAMES THE FIELD THAT TAKES THE VARIABLE, AND
// FOLLOWING IT LOADS. Each isolation variable is set by one layer — the
// engine's own home variables, a profile's config_env, its work_config_env or
// its isolation_env — and the three fields refuse each other's variables, so a
// remedy is right only if it names the layer that sets this one. Held by
// following it with a value that is not the profile's own: the override the
// refusal describes, written instead, must load — and where the remedy says
// paths move with a directory, the directory moved without them must not, or
// the remedy would be naming a step nobody needs. The engine's home variables
// are changed by no field, which every field's refusal of them proves.
func TestACLIEnvIsolationRefusalNamesTheFieldThatTakesIt(t *testing.T) {
	t.Parallel()
	// The fields a custom profile needs before it runs at all.
	const custom = "binary: my-cli, complete_args: [run], output: text, model_args: [--model, '{model}']"
	for name, tc := range map[string]struct {
		agent, model, variable string
		names                  []string
		// alone moves only the variable, which the remedy says is not
		// enough; follow is everything the remedy describes. Both are cli
		// lines, and empty where the remedy describes no field.
		alone, follow string
	}{
		"the engine's own home variable": {
			agent: "claude-code", model: "m", variable: "HOME",
			names: []string{"no field changes it"},
		},
		"the engine's own XDG home": {
			agent: "claude-code", model: "m", variable: "XDG_CONFIG_HOME",
			names: []string{"no field changes it"},
		},
		"a config_env relocation": {
			agent: "codex", model: "m", variable: "CODEX_HOME",
			names: []string{
				"points it at .codex under the seat's home with config_env",
				"moving the credential_paths and volatile_paths kept under .codex beneath it",
				"cli.overrides.config_env",
			},
			alone: "cli: {agent: codex, overrides: {config_env: {CODEX_HOME: .codex-elsewhere}}}",
			follow: "cli: {agent: codex, overrides: {config_env: {CODEX_HOME: .codex-elsewhere}, " +
				"credential_paths: [.codex-elsewhere/auth.json], volatile_paths: [.codex-elsewhere/sessions]}}",
		},
		"a work_config_env relocation": {
			agent: "opencode", model: "anthropic/claude-sonnet-5", variable: "OPENCODE_CONFIG_DIR",
			names: []string{
				"points it at .crewlet-config under the call's working directory with work_config_env",
				"moving the seed_files and system_prompt_file.name kept under .crewlet-config beneath it",
				"cli.overrides.work_config_env",
			},
			alone: "cli: {agent: opencode, overrides: {work_config_env: {OPENCODE_CONFIG_DIR: .elsewhere}}}",
			follow: "cli: {agent: opencode, overrides: {work_config_env: {OPENCODE_CONFIG_DIR: .elsewhere}, " +
				"system_prompt_file: {name: .elsewhere/agents/crewlet-seat.md}, " +
				"seed_files: [{path: .elsewhere/opencode.json, in: work, content: '{}'}]}}",
		},
		"a custom profile's relocation, with nothing kept under it": {
			agent: "custom", model: "m", variable: "VENDOR_HOME",
			names: []string{
				"points it at .vendor under the seat's home with config_env; a different " +
					"directory is set with cli.overrides.config_env",
			},
			follow: "cli: {agent: custom, overrides: {" + custom + ", config_env: {VENDOR_HOME: .vendor-elsewhere}}}",
		},
		"an isolation switch": {
			agent: "opencode", model: "anthropic/claude-sonnet-5", variable: "OPENCODE_CONFIG_PROJECT_DISABLE",
			names:  []string{`it is the profile's isolation switch, which the profile sets to "1"`, "cli.overrides.isolation_env"},
			follow: `cli: {agent: opencode, overrides: {isolation_env: {OPENCODE_CONFIG_PROJECT_DISABLE: "0"}}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := func(cli string) string {
				return strings.Replace(cliEntry(cli), "model: m", "model: "+tc.model, 1)
			}
			overrides := ""
			if tc.agent == "custom" {
				overrides = "overrides: {" + custom + ", config_env: {VENDOR_HOME: .vendor}}, "
			}
			doc := entry("cli: {agent: " + tc.agent + ", " + overrides + "env: {" + tc.variable + ": /srv/elsewhere}}")
			err := rejects(t, doc, "providers.llm.sub.cli.env."+tc.variable)
			if !strings.Contains(err.Error(), "so drop it — ") {
				t.Errorf("the refusal does not say to drop it:\n%v", err)
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if tc.follow == "" {
				// No field changes it: each that could take a variable
				// refuses this one.
				for _, field := range []string{"config_env", "work_config_env", "isolation_env"} {
					rejects(t, entry("cli: {agent: "+tc.agent+", overrides: {"+field+": {"+
						tc.variable+": x}}}"), "providers.llm.sub.cli.overrides")
				}
				return
			}
			if tc.alone != "" {
				rejects(t, entry(tc.alone), "providers.llm.sub.cli.overrides")
			}
			mustCompany(t, entry(tc.follow))
		})
	}
}

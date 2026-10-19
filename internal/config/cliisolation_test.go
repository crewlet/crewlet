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
// following it: the override the refusal names, written instead, must be an
// entry every write path accepts. The engine's home variables are changed by no
// field, and the refusal says so rather than name one.
func TestACLIEnvIsolationRefusalNamesTheFieldThatTakesIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		agent, model, variable string
		names                  string
		// follow is the cli line the remedy describes, or empty where the
		// remedy is to drop the value and nothing more.
		follow string
	}{
		"the engine's own home variable": {
			agent: "claude-code", model: "m", variable: "HOME",
			names: "no field changes it",
		},
		"a config_env relocation": {
			agent: "codex", model: "m", variable: "CODEX_HOME",
			names: "cli.overrides.config_env",
			follow: "cli: {agent: codex, overrides: {config_env: {CODEX_HOME: .codex-elsewhere}, " +
				"credential_paths: [.codex-elsewhere/auth.json], volatile_paths: [.codex-elsewhere/sessions]}}",
		},
		"a work_config_env relocation": {
			agent: "opencode", model: "anthropic/claude-sonnet-5", variable: "OPENCODE_CONFIG_DIR",
			names:  "cli.overrides.work_config_env",
			follow: "cli: {agent: opencode, overrides: {work_config_env: {OPENCODE_CONFIG_DIR: .crewlet-config}}}",
		},
		"an isolation switch": {
			agent: "opencode", model: "anthropic/claude-sonnet-5", variable: "OPENCODE_CONFIG_PROJECT_DISABLE",
			names:  `cli.overrides.isolation_env`,
			follow: `cli: {agent: opencode, overrides: {isolation_env: {OPENCODE_CONFIG_PROJECT_DISABLE: "1"}}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := func(cli string) string {
				return strings.Replace(cliEntry(cli), "model: m", "model: "+tc.model, 1)
			}
			doc := entry("cli: {agent: " + tc.agent + ", env: {" + tc.variable + ": /srv/elsewhere}}")
			err := rejects(t, doc, "providers.llm.sub.cli.env."+tc.variable)
			if !strings.Contains(err.Error(), "so drop it — ") || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("the refusal does not say to drop it and %q:\n%v", tc.names, err)
			}
			if tc.follow != "" {
				mustCompany(t, entry(tc.follow))
			}
		})
	}
}

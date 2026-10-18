package config

import (
	"errors"
	"strings"
	"testing"
)

// A VENDOR'S ISOLATING SWITCH IS NOT AN OPERATOR'S TO TURN OFF FROM cli.env.
//
// The opencode profile sets OPENCODE_CONFIG_PROJECT_DISABLE=1 so OpenCode reads
// no configuration from the call's working directory or any ancestor of it:
// with the walk on, an allow-all opencode.json above the state directory handed
// every seat a shell and an ancestor plugin ran its code (measured on 2.0.32).
// cli.env is layered over the profile's environment, so a value written there —
// "0", or an empty one, which OpenCode reads as off — reopened the walk for
// every seat on the entry while validating clean. The switch is an isolation
// variable now, and cli.env naming it is refused as HOME is: on a write, and
// warned about on apply, because the entry still runs.
//
// A passthrough of the engine's own value through cli.overrides is refused by
// the profile's validator, at the overrides.
func TestOpenCodesProjectSwitchCannotBeTurnedOffFromCLIEnv(t *testing.T) {
	t.Parallel()
	const model = "anthropic/claude-sonnet-5"
	for _, value := range []string{`"0"`, `""`, `"1"`} {
		doc := opencodeEntry(model, "env: {OPENCODE_CONFIG_PROJECT_DISABLE: "+value+"}")
		path := "providers.llm.oc.cli.env.OPENCODE_CONFIG_PROJECT_DISABLE"
		err := rejects(t, doc, path)
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s: want %v, got %v", value, ErrConflict, err)
		}
		if !strings.Contains(err.Error(), "a value here would replace that isolation for every seat") {
			t.Errorf("%s: the refusal does not say why:\n%v", value, err)
		}
		assertAdmissionOnly(t, doc, path)
	}

	doc := opencodeEntry(model, `overrides: {passthrough_env: [OPENCODE_CONFIG_PROJECT_DISABLE]}`)
	err := rejects(t, doc, "providers.llm.oc.cli.overrides")
	if !strings.Contains(err.Error(), "passthrough_env names OPENCODE_CONFIG_PROJECT_DISABLE") {
		t.Errorf("a passthrough of the switch is not refused by name:\n%v", err)
	}

	// An ordinary setting beside it is still the operator's.
	mustCompany(t, opencodeEntry(model, "env: {OPENCODE_LOG_LEVEL: WARN}"))
}

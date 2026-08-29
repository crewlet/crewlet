package org

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestProviderKeysAcceptBothShapes pins the one thing this type exists for:
// an operator writes a bare string or a list, and no reader downstream ever
// learns which.
func TestProviderKeysAcceptBothShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		json string
		want []string
	}{
		{"scalar", "llm: claude-sonnet", `{"llm":"claude-sonnet"}`, []string{"claude-sonnet"}},
		{"list", "llm: [a, b, c]", `{"llm":["a","b","c"]}`, []string{"a", "b", "c"}},
		{"absent", "name: x", `{"name":"x"}`, nil},
		{"null", "llm:", `{"llm":null}`, nil},
		{"empty list", "llm: []", `{"llm":[]}`, nil},
		{"empty scalar", `llm: ""`, `{"llm":""}`, nil},
		{"empties dropped", "llm: ['', b]", `{"llm":["","b"]}`, []string{"b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var fromYAML Role
			if err := yaml.Unmarshal([]byte(tc.yaml), &fromYAML); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			if !slices.Equal(fromYAML.LLM, tc.want) {
				t.Errorf("yaml %q -> %#v, want %#v", tc.yaml, fromYAML.LLM, tc.want)
			}
			var fromJSON Role
			if err := json.Unmarshal([]byte(tc.json), &fromJSON); err != nil {
				t.Fatalf("json: %v", err)
			}
			if !slices.Equal(fromJSON.LLM, tc.want) {
				t.Errorf("json %q -> %#v, want %#v", tc.json, fromJSON.LLM, tc.want)
			}
		})
	}
}

// TestProviderKeysRejectOtherShapes: a mapping under llm: is a config
// mistake, and reporting it beats decoding it to nothing.
func TestProviderKeysRejectOtherShapes(t *testing.T) {
	t.Parallel()
	var r Role
	if err := yaml.Unmarshal([]byte("llm:\n  plan: x\n"), &r); err == nil {
		t.Fatal("a mapping decoded as a provider chain")
	}
	if err := json.Unmarshal([]byte(`{"llm":{"plan":"x"}}`), &r); err == nil {
		t.Fatal("a JSON object decoded as a provider chain")
	}
}

// TestProviderKeysRoundTrip: a one-key chain is written back as the scalar
// it was authored as, so re-serialising a config does not rewrite lines
// nobody edited.
func TestProviderKeysRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		keys      ProviderKeys
		wantYAML  string
		wantJSON  string
		wantEmpty bool
	}{
		{name: "one", keys: ProviderKeys{"a"}, wantYAML: "a\n", wantJSON: `"a"`},
		{name: "many", keys: ProviderKeys{"a", "b"}, wantYAML: "- a\n- b\n", wantJSON: `["a","b"]`},
		{name: "none", keys: nil, wantYAML: "null\n", wantJSON: `null`, wantEmpty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotYAML, err := yaml.Marshal(tc.keys)
			if err != nil {
				t.Fatalf("yaml: %v", err)
			}
			if string(gotYAML) != tc.wantYAML {
				t.Errorf("yaml = %q, want %q", gotYAML, tc.wantYAML)
			}
			gotJSON, err := json.Marshal(tc.keys)
			if err != nil {
				t.Fatalf("json: %v", err)
			}
			if string(gotJSON) != tc.wantJSON {
				t.Errorf("json = %q, want %q", gotJSON, tc.wantJSON)
			}
			if tc.keys.IsZero() != tc.wantEmpty {
				t.Errorf("IsZero = %v, want %v", tc.keys.IsZero(), tc.wantEmpty)
			}
		})
	}
}

// TestToggleIsThreeValued is the whole reason the type exists: unset is not
// false. A schedule nobody annotated is enabled; a seat nobody annotated
// inherits the system learning setting.
func TestToggleIsThreeValued(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		yaml    string
		json    string
		wantSet bool
		wantOn  bool // resolved with a true default
	}{
		{"unset", "name: x", `{"name":"x"}`, false, true},
		{"null", "learning_enabled:", `{"learning_enabled":null}`, false, true},
		{"true", "learning_enabled: true", `{"learning_enabled":true}`, true, true},
		{"false", "learning_enabled: false", `{"learning_enabled":false}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var fromYAML Role
			if err := yaml.Unmarshal([]byte(tc.yaml), &fromYAML); err != nil {
				t.Fatalf("yaml: %v", err)
			}
			var fromJSON Role
			if err := json.Unmarshal([]byte(tc.json), &fromJSON); err != nil {
				t.Fatalf("json: %v", err)
			}
			for source, got := range map[string]Toggle{"yaml": fromYAML.LearningEnabled, "json": fromJSON.LearningEnabled} {
				if got.IsSet() != tc.wantSet {
					t.Errorf("%s IsSet = %v, want %v", source, got.IsSet(), tc.wantSet)
				}
				if got.Or(true) != tc.wantOn {
					t.Errorf("%s Or(true) = %v, want %v", source, got.Or(true), tc.wantOn)
				}
			}
		})
	}
	if !On().Or(false) || Off().Or(true) {
		t.Error("On/Off do not override the default")
	}
}

// TestToggleOmitsUnset: writing a config back must not freeze today's
// default into the document as an explicit value.
func TestToggleOmitsUnset(t *testing.T) {
	t.Parallel()
	type doc struct {
		Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero"`
	}
	gotYAML, err := yaml.Marshal(doc{})
	if err != nil {
		t.Fatalf("yaml: %v", err)
	}
	if string(gotYAML) != "{}\n" {
		t.Errorf("yaml = %q, want %q", gotYAML, "{}\n")
	}
	gotJSON, err := json.Marshal(doc{})
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if string(gotJSON) != "{}" {
		t.Errorf("json = %q, want %q", gotJSON, "{}")
	}
	gotJSON, err = json.Marshal(doc{Enabled: Off()})
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if string(gotJSON) != `{"enabled":false}` {
		t.Errorf("json = %q, want %q", gotJSON, `{"enabled":false}`)
	}
}

// TestAnchoredValuesDecode: a config that defines a fallback chain once and
// points several seats at it with an anchor is ordinary YAML, and a custom
// decoder that only understood inline values would reject it.
func TestAnchoredValuesDecode(t *testing.T) {
	t.Parallel()
	const doc = `
defaults:
  chain: &chain [claude-sonnet, gpt-4o]
  learning: &learning false
roles:
  - name: Engineer
    llm: *chain
    learning_enabled: *learning
`
	var parsed struct {
		Roles []*Role `yaml:"roles"`
	}
	if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Roles) != 1 {
		t.Fatalf("decoded %d roles", len(parsed.Roles))
	}
	if got := parsed.Roles[0].LLM; !slices.Equal(got, ProviderKeys{"claude-sonnet", "gpt-4o"}) {
		t.Errorf("llm = %v", got)
	}
	if got := parsed.Roles[0].LearningEnabled; !got.IsSet() || got.Or(true) {
		t.Errorf("learning_enabled = %+v, want an explicit false", got)
	}
}

func TestToggleMarshalsBothWays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		toggle   Toggle
		wantYAML string
		wantJSON string
	}{
		{"unset", Toggle{}, "null\n", "null"},
		{"on", On(), "true\n", "true"},
		{"off", Off(), "false\n", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotYAML, err := yaml.Marshal(tc.toggle)
			if err != nil {
				t.Fatalf("yaml: %v", err)
			}
			if string(gotYAML) != tc.wantYAML {
				t.Errorf("yaml = %q, want %q", gotYAML, tc.wantYAML)
			}
			gotJSON, err := json.Marshal(tc.toggle)
			if err != nil {
				t.Fatalf("json: %v", err)
			}
			if string(gotJSON) != tc.wantJSON {
				t.Errorf("json = %q, want %q", gotJSON, tc.wantJSON)
			}
		})
	}
}

// TestMCPEnvWithDefaults pins the merge direction and its granularity: a
// seat overriding one variable must not lose the token beside it.
func TestMCPEnvWithDefaults(t *testing.T) {
	t.Parallel()
	unit := MCPEnv{
		"atlassian": {"JIRA_URL": "https://acme.example.com", "JIRA_API_TOKEN": "team"},
		"github":    {"Authorization": "Bearer team"},
	}
	seat := MCPEnv{
		"atlassian": {"JIRA_API_TOKEN": "mine"},
		"gitlab":    {"GITLAB_TOKEN": "mine"},
	}
	got := seat.WithDefaults(unit)

	want := MCPEnv{
		"atlassian": {"JIRA_URL": "https://acme.example.com", "JIRA_API_TOKEN": "mine"},
		"github":    {"Authorization": "Bearer team"},
		"gitlab":    {"GITLAB_TOKEN": "mine"},
	}
	if !mcpEnvEqual(got, want) {
		t.Errorf("merged = %#v, want %#v", got, want)
	}
	// Neither input may be aliased into the result: the unit's map is
	// shared by every member of the team.
	if unit["atlassian"]["JIRA_API_TOKEN"] != "team" {
		t.Error("merging wrote back into the unit's credentials")
	}
	if len(seat["atlassian"]) != 1 {
		t.Error("merging wrote back into the seat's own credentials")
	}
}

func TestMCPEnvWithDefaultsHandlesEmpty(t *testing.T) {
	t.Parallel()
	base := MCPEnv{"a": {"K": "v"}}
	if got := MCPEnv(nil).WithDefaults(base); !mcpEnvEqual(got, base) {
		t.Errorf("nil.WithDefaults = %#v, want %#v", got, base)
	}
	if got := base.WithDefaults(nil); !mcpEnvEqual(got, base) {
		t.Errorf("WithDefaults(nil) = %#v, want %#v", got, base)
	}
	if got := MCPEnv(nil).WithDefaults(nil); got != nil {
		t.Errorf("nil.WithDefaults(nil) = %#v, want nil", got)
	}
}

func mcpEnvEqual(a, b MCPEnv) bool {
	if len(a) != len(b) {
		return false
	}
	for server, vars := range a {
		if !maps.Equal(vars, b[server]) {
			return false
		}
	}
	return true
}

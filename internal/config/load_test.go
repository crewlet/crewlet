package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A typo'd key is the failure this whole package is shaped around: every
// field has a default, so an ignored key does not fail — it silently does
// nothing, and the company runs without the setting its operator believes
// they set.
func TestUnknownFieldIsRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		key  string
	}{
		{"top level", "name: Acme\nmisson: typo\n", "misson"},
		{"nested block", "name: Acme\nturn_engine:\n  max_iteratons: 4\n", "max_iteratons"},
		{"seat", "name: Acme\nroles:\n  - name: CEO\n    backstroy: oops\n", "backstroy"},
		{"unit", "name: Acme\nunits:\n  - name: Core\n    lede: CEO\n", "lede"},
		{"seat inside a unit", "name: Acme\nunits:\n  - name: Core\n    roles:\n      - name: CEO\n        gaol: ship\n", "gaol"},
		{"mcp server", "name: Acme\nmcp_servers:\n  - name: calc\n    commnad: uvx\n", "commnad"},
		{"per-phase llm mapping", "name: Acme\nroles:\n  - name: CEO\n    llm:\n      default: fast\n      pln: big\n", "pln"},
		{"tool annotations", "name: Acme\nmcp_servers:\n  - name: calc\n    command: uvx\n    tool_annotations:\n      calc_add: {read_onl: true}\n", "read_onl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseCompany([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("a typo'd %q was accepted", tc.key)
			}
			if !errors.Is(err, ErrUnknownField) {
				t.Fatalf("want ErrUnknownField, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("the error must name the key the operator typed; got %v", err)
			}
		})
	}
}

// Tier A rejects a typo for the same reason, and it also rejects the Tier B
// keys that used to live beside it — a company config pointed at `crewlet
// run` must not half-load.
func TestBootstrapRejectsUnknownAndTierBKeys(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		"logging:\n  level: debug\nstore:\n  paht: x.db\n",
		"name: Acme\nlogging:\n  level: debug\n",
		"roles:\n  - name: CEO\n",
	} {
		_, err := ParseBootstrap([]byte(doc), EnvOnly())
		if !errors.Is(err, ErrUnknownField) {
			t.Fatalf("want ErrUnknownField for %q, got %v", doc, err)
		}
	}
}

// A key written with nothing under it is ordinary authoring for "empty",
// and it is what a config API can produce. It must read as UNSET — the
// default applies — rather than as a wiped block.
func TestValuelessKeyIsUnset(t *testing.T) {
	t.Parallel()
	cfg, err := ParseCompany([]byte(`
name: Acme
policies:
turn_engine:
learning:
roles:
units:
`))
	if err != nil {
		t.Fatalf("valueless keys should load: %v", err)
	}
	if got := cfg.TurnEngine.MaxIterations; got != DefaultTurnEngine().MaxIterations {
		t.Fatalf("a valueless block wiped its defaults: max_iterations = %d", got)
	}
	if !cfg.Learning.On() {
		t.Fatal("a valueless learning block should keep the on-by-default reading")
	}
	if len(cfg.Roles) != 0 || len(cfg.Policies) != 0 {
		t.Fatal("a valueless list should read as empty")
	}
}

func TestParseCompanyRejectsNonMappings(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{"", "- a\n- b\n", "just a string\n"} {
		if _, err := ParseCompany([]byte(doc)); err == nil {
			t.Fatalf("a %q document was accepted as a company config", doc)
		}
	}
}

func TestParseCompanyRequiresAName(t *testing.T) {
	t.Parallel()
	_, err := ParseCompany([]byte("mission: ship\n"))
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("the error must name the field; got %v", err)
	}
}

// An empty Tier A file is legitimate — every field defaults, and a company
// runs on the defaults alone.
func TestEmptyBootstrapIsTheDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap(nil, EnvOnly())
	if err != nil {
		t.Fatalf("an empty bootstrap should load: %v", err)
	}
	want := DefaultBootstrap()
	if cfg.Store.Path != want.Store.Path || cfg.Stream.Type != want.Stream.Type ||
		cfg.Coordination.Type != want.Coordination.Type || !cfg.API.Auth.AllowAnonymousRead {
		t.Fatalf("empty bootstrap did not take the defaults: %+v", cfg)
	}
}

func TestLoadFromDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(path, []byte("name: Acme\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadCompany(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "Acme" {
		t.Fatalf("name = %q", cfg.Name)
	}
	if _, err := LoadCompany(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("a missing file should fail")
	}
}

// Every validator reports EVERY failure it finds. A config author fixing
// one field at a time through a validate-edit loop pays a round trip per
// mistake, and the mistakes here are usually made together.
func TestValidationReportsEveryFailureAtOnce(t *testing.T) {
	t.Parallel()
	_, err := ParseCompany([]byte(`
name: Acme
token_budget: -1
notification_coalesce_max_batch: 0
turn_engine:
  max_iterations: 0
  max_tool_rounds: 0
`))
	if err == nil {
		t.Fatal("expected failures")
	}
	for _, want := range []string{
		"token_budget", "notification_coalesce_max_batch",
		"turn_engine.max_iterations", "turn_engine.max_tool_rounds",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %s; got:\n%v", want, err)
		}
	}
}

// A FRACTION IN A WHOLE-NUMBER FIELD IS REFUSED, NAMING THE FIELD — never
// truncated.
//
// The decoder read `api.port: 8080.9` as 8080 and `max_tool_rounds: 3.7` as 3
// without a word, so a config ran on a value its author never wrote. A number
// that IS whole in another spelling (`8080.0`, `1e3`) is the number it spells,
// and a field that takes fractions takes one.
func TestAFractionInAWholeNumberFieldIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		parse func() error
		path  string
	}{
		{"a Tier A port", func() error {
			_, err := ParseBootstrap([]byte("api:\n  port: 8080.9\n"), NewResolver(MapSource{}))
			return err
		}, "api.port"},
		{"a Tier B round cap", func() error {
			_, err := ParseCompany([]byte("name: Acme\nturn_engine:\n  max_tool_rounds: 3.7\n"))
			return err
		}, "turn_engine.max_tool_rounds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.parse()
			if !errors.Is(err, ErrShape) {
				t.Fatalf("err = %v, want a wrong shape — the fraction would be dropped", err)
			}
			problems := Problems(err)
			if len(problems) != 1 || problems[0].Path != tc.path || problems[0].Line == 0 {
				t.Fatalf("problems = %+v, want one at %s with its line", problems, tc.path)
			}
			if !strings.Contains(problems[0].Message, "not a whole number") {
				t.Errorf("message = %q, want it to say the value is not a whole number", problems[0].Message)
			}
		})
	}

	for _, whole := range []string{"8080.0", "8.08e3"} {
		cfg, err := ParseBootstrap([]byte("api:\n  port: "+whole+"\n"), NewResolver(MapSource{}))
		if err != nil || cfg.API.Port != 8080 {
			t.Errorf("api.port: %s = (%v, %v), want 8080 — a whole number in another spelling", whole, cfg, err)
		}
	}
	cfg, err := ParseBootstrap([]byte("coordination:\n  lease_ttl_seconds: 12.5\n"), NewResolver(MapSource{}))
	if err != nil || cfg.Coordination.LeaseTTLSeconds != 12.5 {
		t.Errorf("a fractional lease TTL = (%v, %v), want 12.5 — that field takes fractions", cfg, err)
	}
}

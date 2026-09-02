package config

import (
	"errors"
	"testing"
)

// A PLACEMENT NEEDS THE BACKEND THAT SERVES IT.
//
// `run_in: e2b` with no `e2b:` block reads as a working choice and configures
// nothing — the same silence this package spends most of its rules on, and the
// one that let a remote-only default stand for as long as it did. The
// catalogue reshape moved the failure here from a `type:` that could only ever
// answer for the whole company.
func TestAPlacementNeedsItsBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, yaml, field string }{
		{"a company default naming an unconfigured backend",
			"local: {}\n    default_run_in: e2b", "providers.sandbox.default_run_in"},
		{"a seat naming an unconfigured backend",
			"e2b: {api_key: k}\nroles:\n  - name: SWE\n    sandbox: {enabled: true, run_in: direct}",
			"roles[0].sandbox.run_in"},
		// The seats inside `units:` are checked too. They were not, for as
		// long as the walk that reaches them was written inline in one
		// rule — which exempted most of a real company's seats from every
		// cross-field rule there is.
		{"a unit's seat naming an unconfigured backend",
			"e2b: {api_key: k}\nunits:\n  - name: Platform\n    roles:\n" +
				"      - name: SWE\n        sandbox: {enabled: true, run_in: container}",
			"units[0].roles[0].sandbox.run_in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    "+tc.yaml+"\n", tc.field)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("want ErrConflict, got %v", err)
			}
		})
	}
}

// AN AMBIGUOUS CATALOGUE HAS TO BE TOLD WHERE A SILENT SEAT RUNS.
//
// `local:` alone is TWO cells, not one: it serves `direct`, which runs the
// coding agent as the engine's user, and `container`, which does not. Picking
// either for an operator who wrote neither is the default that gets discovered
// afterwards.
func TestAnAmbiguousCatalogueNeedsAnExplicitDefault(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n",
		"providers.sandbox.default_run_in")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	// Both backends is the same ambiguity, one cell wider.
	err = rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    e2b: {api_key: k}\n",
		"providers.sandbox.default_run_in")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	// THE UNAMBIGUOUS ONES RESOLVE THEMSELVES, which is the direction that
	// matters: a rule that only ever refuses would make the simplest remote
	// catalogue unwritable.
	cfg := mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    e2b: {api_key: \"${E2B_API_KEY}\"}\n")
	if got := cfg.Providers.Sandbox.RunIn(); got != PlacementE2B {
		t.Fatalf("a remote-only catalogue resolved to %q, want %q", got, PlacementE2B)
	}
}

// THE REMOTE BACKEND REQUIRES ITS KEY. The API authenticates every call,
// including against a self-hosted cluster — `domain` changes which API is
// talked to, not whether it authenticates — so a run without one 401s at its
// first create, minutes into a turn that has already spent its own rounds.
func TestTheRemoteBackendRequiresItsKey(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    e2b: {}\n",
		"providers.sandbox.e2b.api_key")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	// A COMPLETE REMOTE BLOCK LOADS.
	mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    e2b:\n"+
		"      api_key: \"${E2B_API_KEY}\"\n      domain: cluster.example\n"+
		"      template: crewlet-box\n")
}

// AN EMPTY BLOCK CONFIGURES NOTHING, and it is the shape an unfinished edit
// leaves behind: it parses, and without this rule it would apply — every
// sandbox-enabled seat planning around a box it never gets, with code work
// quietly never happening and nothing anywhere saying why.
func TestAnEmptyCatalogueIsRefused(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox: {}\n", "providers.sandbox")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
}

// THE DOUBLE REPLACES EVERY BACKEND rather than joining them, so a real block
// underneath it reads as configuration and configures nothing.
func TestTheDoubleRefusesARealBackendBesideIt(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    fake: true\n    local: {}\n",
		"providers.sandbox.fake")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    fake: true\n")
}

// A CONTAINER IMAGE IS REQUIRED WHERE A CONTAINER IS ACTUALLY RUN, and only
// there. Required unconditionally it would refuse a perfectly good direct-only
// company; unchecked, the seat's first coding run fails at container create,
// minutes into a turn.
func TestTheContainerImageIsRequiredOnlyWhereAContainerRuns(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n"+
		"    default_run_in: container\n", "providers.sandbox.local.image")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	// A direct-only company needs none.
	mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    local: {}\n    default_run_in: direct\n")
	// And a container field nobody reads is reported rather than ignored:
	// it looks like configuration and configures nothing.
	err = rejects(t, "name: Acme\nproviders:\n  sandbox:\n"+
		"    local: {image: \"example.invalid/box:1\"}\n    default_run_in: direct\n",
		"providers.sandbox.local.image")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

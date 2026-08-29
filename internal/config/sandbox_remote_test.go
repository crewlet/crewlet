package config

import (
	"errors"
	"testing"
)

// THE REMOTE FIELDS ARE REFUSED WHERE NOTHING READS THEM.
//
// `domain:` beside `type: local` reads as a cluster address and configures
// nothing — the same silence this package spends most of its rules on, and
// the one that let a remote-only default stand for as long as it did.
func TestRemoteSandboxFieldsNeedTheRemoteBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, yaml, field string }{
		{"an api key on a local box",
			"type: local\n    local: {containment: direct}\n    api_key: k", "api_key"},
		{"a cluster domain on a fake box", "type: fake\n    domain: cluster.example", "domain"},
		{"a template on a fake box", "type: fake\n    template: custom", "template"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    "+tc.yaml+"\n",
				"providers.sandbox."+tc.field)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("want ErrConflict, got %v", err)
			}
		})
	}

	// AND THE REMOTE BACKEND REQUIRES ITS KEY. The API authenticates every
	// call, including against a self-hosted cluster — `domain` changes
	// which API is talked to, not whether it authenticates — so a run
	// without one 401s at its first create, minutes into a turn that has
	// already spent a Plan phase.
	err := rejects(t, "name: Acme\nproviders:\n  sandbox:\n    type: e2b\n",
		"providers.sandbox.api_key")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}

	// A COMPLETE REMOTE BLOCK LOADS, which is the direction that matters:
	// a rule that only ever refuses would be one an operator cannot
	// satisfy.
	mustCompany(t, "name: Acme\nproviders:\n  sandbox:\n    type: e2b\n"+
		"    api_key: \"${E2B_API_KEY}\"\n    domain: cluster.example\n"+
		"    template: crewlet-box\n")
}

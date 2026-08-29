package mcp

import (
	"errors"
	"testing"
	"time"
)

func TestSpecValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec Spec
		ok   bool
	}{
		{"stdio with a command", Spec{Name: "a", Command: "uvx"}, true},
		{"transport defaults to stdio", Spec{Name: "a", Transport: "", Command: "uvx"}, true},
		{"http with a url", Spec{Name: "a", Transport: TransportHTTP, URL: "https://x/mcp"}, true},
		// The name keys the client index, the per-role instance prefix, and
		// the server name the model is shown. An empty one propagates
		// silently through all three.
		{"an empty name is refused", Spec{Command: "uvx"}, false},
		{"stdio with no command is refused", Spec{Name: "a"}, false},
		{"http with no url is refused", Spec{Name: "a", Transport: TransportHTTP}, false},
		{"an unknown transport is refused", Spec{Name: "a", Transport: "carrier-pigeon"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.validate()
			if tc.ok && err != nil {
				t.Fatalf("validate = %v, want nil", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("validate accepted a spec the engine cannot act on")
				}
				if !errors.Is(err, ErrInvalidSpec) {
					t.Fatalf("err = %v, want ErrInvalidSpec so a caller can tell bad config from a failed start", err)
				}
			}
		})
	}
}

func TestSpecDefaults(t *testing.T) {
	t.Parallel()
	// The two headline deadlines, pinned literally. Comparing a zero Spec
	// against the constants only proves the plumbing; these numbers are the
	// contract, and their provenance is in timeouts.go.
	if DefaultStartupTimeout != 120*time.Second {
		t.Fatalf("DefaultStartupTimeout = %s: sized for a cold uvx/npx fetch on a "+
			"modest link — re-derive it in timeouts.go before moving it", DefaultStartupTimeout)
	}
	if DefaultRequestTimeout != 300*time.Second {
		t.Fatalf("DefaultRequestTimeout = %s: one tool must not have two ceilings "+
			"depending on a transport the agent cannot see", DefaultRequestTimeout)
	}
	var s Spec
	if s.startupTimeout() != DefaultStartupTimeout || s.requestTimeout() != DefaultRequestTimeout {
		t.Fatalf("zero Spec did not take the package defaults: %s / %s",
			s.startupTimeout(), s.requestTimeout())
	}
	s.StartupTimeout, s.RequestTimeout = time.Second, 2*time.Second
	if s.startupTimeout() != time.Second || s.requestTimeout() != 2*time.Second {
		t.Fatal("per-server overrides did not reach the client")
	}
}

func TestSameProcessAndSameCatalogue(t *testing.T) {
	t.Parallel()
	base := Spec{
		Name: "github", Transport: TransportHTTP,
		URL: "https://api.example/mcp", Headers: map[string]string{"Authorization": "Bearer a"},
		ToolPrefix: "gh_", ExcludeTools: []string{"noisy"},
		AnnotationOverrides: map[string]Annotations{"gh_create_pr": {ReadOnly: No}},
	}

	same := base
	same.Headers = map[string]string{"Authorization": "Bearer a"}
	same.ExcludeTools = []string{"noisy"}
	same.AnnotationOverrides = map[string]Annotations{"gh_create_pr": {ReadOnly: No}}
	if !base.SameProcess(same) || !base.SameCatalogue(same) {
		t.Fatal("an identically-valued copy compared as different")
	}

	// The HTTP incident: the Python diff compared only the stdio fields, so a
	// rotated remote token matched as "unchanged" and the stale connection
	// went on serving with the credential the operator had just revoked.
	rotated := same
	rotated.Headers = map[string]string{"Authorization": "Bearer b"}
	if base.SameProcess(rotated) {
		t.Fatal("a changed Authorization header compared as the same process")
	}
	movedURL := same
	movedURL.URL = "https://elsewhere.example/mcp"
	if base.SameProcess(movedURL) {
		t.Fatal("a changed url compared as the same process")
	}

	// The annotations incident: these change nothing about the child and
	// everything about whether a sub-agent may call a tool. Same process,
	// different catalogue.
	reannotated := same
	reannotated.AnnotationOverrides = map[string]Annotations{"gh_create_pr": {ReadOnly: Yes}}
	if !base.SameProcess(reannotated) {
		t.Fatal("an annotation override is not a property of the process")
	}
	if base.SameCatalogue(reannotated) {
		t.Fatal("a changed annotation override compared as the same catalogue: the edit would silently do nothing")
	}

	reprefixed := same
	reprefixed.ToolPrefix = "github_"
	if !reprefixed.SameProcess(base) || base.SameCatalogue(reprefixed) {
		t.Fatal("a prefix renames every tool without touching the child")
	}
	reexcluded := same
	reexcluded.ExcludeTools = nil
	if !reexcluded.SameProcess(base) || base.SameCatalogue(reexcluded) {
		t.Fatal("an exclusion list changes the catalogue, not the child")
	}
}

func TestSpecServerStripsTheRoleSuffix(t *testing.T) {
	t.Parallel()
	s := Spec{Name: InstanceName("atlassian", "Product Manager"), Command: "uvx"}
	if got := s.Server(); got != "atlassian" {
		t.Fatalf("Server() = %q, want atlassian", got)
	}
}

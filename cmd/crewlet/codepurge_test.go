package main

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A PURGE FROM A SHELL IS THE TOOL'S REQUEST, over the act transport: the
// repository as the tool's one argument, the operation key in the header the
// transport requires, the credential from the environment — and the answer
// told as the node gave it: the request, the node that serves it and what
// still holds the objects.
//
// Mutation: post to another tool, or drop the repository from the arguments
// → red.
func TestCodePurgeActsThroughTheActTransport(t *testing.T) {
	t.Parallel()
	node := newFakeActNode(t)
	node.answer = map[string]any{"outcome": "applied", "repo": "site",
		"request": "019a0c2e-0000-7000-8000-0000000000e0", "position": "CREWLET_CODE_LOG@1:42",
		"maintainer": "git-2", "holds": map[string]any{"git_nodes": []string{"git-2", "git-1"},
			"backups_before": "every backup taken before the purge commits", "store_until": "a day after"}}
	stdout, stderr, err := cli(t, "code", "purge-unreachable", "site", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("code purge-unreachable: %v\n%s", err, stderr)
	}
	if node.tool != "code_purge_unreachable" || len(node.body.Args) != 1 || node.body.Args["repo"] != "site" {
		t.Errorf("posted %v to %q, want the repository alone to code_purge_unreachable", node.body.Args,
			node.tool)
	}
	if node.authorization != "Bearer "+cliFixtureToken || statelog.CheckCallerOpID(node.key) != nil {
		t.Errorf("sent Authorization %q and key %q, want the token from %s and an operation id",
			node.authorization, node.key, apiTokenEnv)
	}
	for _, want := range []string{"019a0c2e-0000-7000-8000-0000000000e0", "served by git-2", "git-2, git-1",
		"every backup taken before the purge commits"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not say %q:\n%s", want, stdout)
		}
	}
}

// AN UNKNOWN OUTCOME NAMES THE OPERATION TO RETRY WITH, and the retry sends
// it: the same request, which the committer answers as the first.
func TestCodePurgeRetriesUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	node := newFakeActNode(t)
	node.answer = map[string]any{"outcome": "unknown"}
	stdout, _, err := cli(t, "code", "purge-unreachable", "site", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("code purge-unreachable: %v", err)
	}
	first := node.key
	if !strings.Contains(stdout, "-op-id "+first) {
		t.Fatalf("an unknown outcome did not print the id to retry with:\n%s", stdout)
	}
	if _, _, err := cli(t, "code", "purge-unreachable", "site", "-op-id", first,
		bootstrapForURL(t, node.server.URL)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if node.key != first {
		t.Errorf("the retry sent %q, want the first attempt's %q", node.key, first)
	}
	if _, _, err := cli(t, "code", "purge-unreachable", "-url", node.server.URL); err == nil ||
		!strings.Contains(err.Error(), "name the repository") {
		t.Errorf("a purge naming no repository = %v, want it refused for want of one", err)
	}
}

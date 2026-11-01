package main

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE IMPORT COMMAND RUNS ON THE NODE: without -local, `crewlet code import`
// is the tool's create over the act transport — the source, the name, the
// owner, the credential's NAME and the mirror as the tool's arguments, the
// operation key in the header the transport requires, the credential of the
// request from the environment — and runs no git on this machine; the
// answer is told as the node gave it, the repository importing. An unknown
// outcome names the operation, and -op-id sends it again. A flag of the
// other import is refused before anything is sent: -git or -dry-run without
// -local, and -owner, -credential, -mirror or -op-id beside it.
//
// Mutation: dispatch the import to the client-side push whatever -local
// says, or drop the owner or the mirror from the arguments → red.
func TestTheImportCommandRunsOnTheNode(t *testing.T) {
	t.Parallel()
	const source = "https://git.example.com/acme/web.git"
	node := newFakeActNode(t)
	node.answer = map[string]any{"outcome": "applied", "repo": "web", "position": "CREWLET_CODE_LOG@1:42",
		"repository": map[string]any{"name": "web", "status": "importing", "default_branch": "main",
			"owner": map[string]any{"kind": "unit", "ref": "eng"}}}
	stdout, stderr, err := cli(t, "code", "import", source, "web", bootstrapForURL(t, node.server.URL),
		"-owner", "unit:eng", "-credential", "GITHUB_TOKEN", "-mirror")
	if err != nil {
		t.Fatalf("code import: %v\n%s", err, stderr)
	}
	a := node.body.Args
	owner, _ := a["owner"].(map[string]any)
	if node.tool != "code_import_repo" || a["name"] != "web" || a["url"] != source ||
		a["credential"] != "GITHUB_TOKEN" || a["mirror"] != true || owner["kind"] != "unit" ||
		owner["ref"] != "eng" || len(a) != 5 {
		t.Errorf("posted %v to %q, want web's mirror of the source, owned by unit eng, to code_import_repo", a,
			node.tool)
	}
	if node.authorization != "Bearer "+cliFixtureToken || statelog.CheckCallerOpID(node.key) != nil {
		t.Errorf("sent Authorization %q and key %q, want the token from %s and an operation id",
			node.authorization, node.key, apiTokenEnv)
	}
	for _, want := range []string{"mirroring " + source + " into web — importing, owned by unit:eng",
		"CREWLET_CODE_LOG@1:42", "crewlet code status -repo web"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not say %q:\n%s", want, stdout)
		}
	}

	node.answer, node.body.Args = map[string]any{"outcome": "unknown"}, nil
	stdout, _, err = cli(t, "code", "import", source, "web", "-url", node.server.URL, "-owner", "unit:eng")
	if err != nil {
		t.Fatalf("code import: %v", err)
	}
	first := node.key
	if _, given := node.body.Args["credential"]; given || !strings.Contains(stdout, "-op-id "+first) {
		t.Fatalf("an anonymous import sent %v and its unknown outcome said:\n%s", node.body.Args, stdout)
	}
	if _, _, err := cli(t, "code", "import", source, "web", "-url", node.server.URL, "-owner", "unit:eng",
		"-op-id", first); err != nil || node.key != first {
		t.Errorf("the retry = %v, sent %q, want the first attempt's %q", err, node.key, first)
	}

	sent := node.key
	for _, c := range []struct {
		flags []string
		says  []string // what the refusal must name
	}{
		{[]string{"-owner", "unit:eng", "-git", "/usr/bin/git"}, []string{"-git", "add -local"}},
		{[]string{"-owner", "unit:eng", "-dry-run"}, []string{"-dry-run", "add -local"}},
		{[]string{"-local", "-owner", "unit:eng"}, []string{"-owner", "or -local"}},
		{[]string{"-local", "-mirror"}, []string{"-mirror", "or -local"}},
		{[]string{"-owner", "eng"}, []string{"-owner", "unit:<key>"}},
	} {
		args := append([]string{"code", "import", source, "web", "-url", node.server.URL}, c.flags...)
		_, _, err := cli(t, args...)
		for _, want := range c.says {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("code import %v = %v, want it refused naming %q", c.flags, err, want)
			}
		}
	}
	if node.key != sent {
		t.Errorf("a refused command line still reached the node (key %q)", node.key)
	}
}

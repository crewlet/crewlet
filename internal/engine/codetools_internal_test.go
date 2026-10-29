package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// THE CODE TOOLS ARE OFFERED WHERE THE CODE HOST IS SERVED, through the running
// wiring: a company whose node's runtime serves its repositories holds the
// nine in every seat's registry — equipped at the apply, after the runtime it
// starts — and the operator's halves read the same deps; a company that hosts
// no code is given none, whose tools would answer every call that no
// repository is served. The terms are [OperatorCode]'s, the code host's REST
// reads' own, and a node with no runtime serves none.
//
// Mutation: CodeToolDeps ignoring OperatorCode's answer (deps over the bare
// router) → the company hosting none holds the tools, red; equip dropping
// deps.Code, or CodeToolDeps leaving out who a turn is acting for or the chart
// that decides them (the requester ceiling) → the company hosting code holds
// none, red.
func TestTheCodeToolsAreOfferedWhereTheCodeHostIsServed(t *testing.T) {
	t.Parallel()
	t.Run("hosting code", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, codeHostCompany))
		if deps := CodeToolDeps(e); deps.Code == nil || deps.CloneURL == nil || deps.Requesters == nil ||
			deps.Chart == nil {
			t.Fatalf("a node serving the code host offers no code tools, or none decided again for whoever "+
				"woke a turn: %+v", deps)
		}
		for _, name := range builtin.CodeToolNames() {
			if _, ok := e.Company().Tools.Lookup(name); !ok {
				t.Errorf("a seat of a company hosting code holds no %s", name)
			}
		}
	})
	t.Run("hosting none", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, parseCompany(t, nativeCleanupCompany))
		if deps := CodeToolDeps(e); deps.Code != nil {
			t.Error("a node of a company hosting no code offers the code tools")
		}
		for _, name := range builtin.CodeToolNames() {
			if _, ok := e.Company().Tools.Lookup(name); ok {
				t.Errorf("a seat of a company hosting no code holds %s", name)
			}
		}
	})
	if deps := CodeToolDeps(nil); deps.Code != nil {
		t.Error("no engine offered the code tools")
	}
	if deps := CodeToolDeps(&Engine{}); deps.Code != nil {
		t.Error("an engine running no runtime offered the code tools")
	}
}

// A PERSON'S CLONE URL IS THE ONE THE CLI CONFIGURES GIT WITH: the deployment's
// `api.external_url` and the one path a repository is served at — and nothing
// where the deployment names no address, which no answer then invents.
func TestAClonesURLIsTheDeploymentsGitAddress(t *testing.T) {
	t.Parallel()
	e := &Engine{externalBase: "https://crewlet.example.com"}
	if got, want := e.cloneURL("site"), "https://crewlet.example.com/git/site.git"; got != want {
		t.Errorf("clone URL = %q, want %q", got, want)
	}
	if got := (&Engine{}).cloneURL("site"); got != "" {
		t.Errorf("a deployment with no external url handed out the clone URL %q", got)
	}
}

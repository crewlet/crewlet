package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// sessionsOffCompany is [nativeCleanupCompany] with its executor sessions
// switched off.
const sessionsOffCompany = nativeCleanupCompany + `turn_engine:
  conversation_session:
    enabled: false
`

// THE SESSION TOOLS ARE OMITTED WHERE NO SESSION IS KEPT, through the running
// wiring: an epoch that keeps sessions, on a node with the estate router and
// an object store, offers every seat both tools; an epoch whose company turned
// the switch off offers neither — no model reads a session, the ones kept
// before it went off included — and neither does a node with no router, nor
// one with no store a sealed session opens from. And the tools an epoch
// offered ask the switch of the revision the node runs at each call, so a
// launch that pinned the epoch is refused once the node applies one that
// turns it off.
//
// Mutation: register the session tools whatever the switch, the router or the
// store says, or ask the switch of the epoch the tools were offered by, and a
// check goes red.
func TestTheSessionToolsAreOmittedWhereNoSessionIsKept(t *testing.T) {
	t.Parallel()
	names := []string{builtin.ListSessionsTool, builtin.ReadSessionTool}
	keeping := newSandboxNode(t, parseCompany(t, nativeCleanupCompany))
	for _, name := range names {
		if _, ok := keeping.Company().Tools.Lookup(name); !ok {
			t.Errorf("an epoch keeping sessions, on a node with the router and a store, offers no %s", name)
		}
	}
	off := newSandboxNode(t, parseCompany(t, sessionsOffCompany))
	for _, name := range names {
		if _, ok := off.Company().Tools.Lookup(name); ok {
			t.Errorf("an epoch whose company keeps no session offers %s", name)
		}
	}
	for what, e := range map[string]*Engine{
		"no estate router":   {},
		"no object store":    {router: keeping.router},
		"the switch flipped": {router: keeping.router, objects: keeping.objects, boot: keeping.boot},
	} {
		company := keeping.Company()
		if what == "the switch flipped" {
			company = off.Company()
		}
		if deps := e.sessionDeps(company); deps.Rows != nil || deps.Objects != nil {
			t.Errorf("a node with %s is handed the session tools' deps", what)
		}
	}
	whole := &Engine{router: keeping.router, objects: keeping.objects, boot: keeping.boot}
	whole.epoch.current.Store(keeping.Company())
	deps := whole.sessionDeps(keeping.Company())
	if deps.Rows == nil || deps.Objects == nil || deps.Keeps == nil || !deps.Keeps() {
		t.Fatal("a node with the router and a store, on an epoch keeping sessions, is handed no deps")
	}
	whole.epoch.current.Store(off.Company())
	if deps.Keeps() {
		t.Error("the tools an epoch keeping sessions offered still read once the node runs a revision " +
			"that keeps none")
	}
}

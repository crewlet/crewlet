package tools_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// deliverStub is a tool with a name and nothing else. What it does is
// irrelevant here: the question is entirely about how it was REGISTERED.
type deliverStub struct{ name string }

func (t deliverStub) Name() string               { return t.name }
func (t deliverStub) Description() string        { return "a stub" }
func (t deliverStub) Parameters() map[string]any { return map[string]any{} }
func (t deliverStub) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "ok"}, nil
}

// The delivery gate asks whether a call could REACH somebody, and this is the
// whole of that question: two kinds of tool are in the answer and everything
// else is out.
//
// The rule this replaced keyed on origin — server-backed and not a known read
// — which was right for as long as every shared surface belonged to a vendor.
// A native tracker breaks it: commenting on a work item reaches the person who
// asked, and it is a builtin, so a gate keyed on origin judged that turn to
// have answered nobody and looped it to failure. Registering the write with
// [tools.Delivers] is what says otherwise, one tool at a time — never an
// annotation, which cannot tell a diary from a comment.
func TestDeliverablesIsDeclaredNotDerived(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()

	// First-party, declared: the native tracker's write.
	if err := r.RegisterWith(deliverStub{"comment_on_work_item"}, tools.OriginBuiltin,
		tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes},
		tools.Delivers()); err != nil {
		t.Fatal(err)
	}
	// First-party, NOT declared, and annotated exactly like a write. This is
	// reflect_and_persist's shape, and the case a derived rule got wrong: a
	// turn must not satisfy "did this reach anybody" by writing its own diary.
	if err := r.RegisterWith(deliverStub{"reflect_and_persist"}, tools.OriginBuiltin,
		tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}); err != nil {
		t.Fatal(err)
	}
	// MCP, unannotated: counts, because the fail-closed direction is what
	// stops a server that forgot to annotate from exempting all its tools.
	if err := r.Register(deliverStub{"vendor_post"}, tools.Origin("vendor")); err != nil {
		t.Fatal(err)
	}
	// MCP, POSITIVELY a read: does not count.
	if err := r.RegisterWith(deliverStub{"vendor_get"}, tools.Origin("vendor"),
		tools.Annotations{ReadOnly: mcp.Yes}); err != nil {
		t.Fatal(err)
	}

	want := []string{"comment_on_work_item", "vendor_post"}
	if got := r.Deliverables(); !slices.Equal(got, want) {
		t.Errorf("Deliverables() = %v, want %v", got, want)
	}

	// A snapshot answers the same question the registry does. They are read
	// by different frames — the registry by the tool room, a snapshot by the
	// running phase — and a tool that delivered through one and not the other
	// would make the gate depend on which frame asked.
	snap := r.Snapshot()
	if got := snap.Deliverables(); !slices.Equal(got, want) {
		t.Errorf("Snapshot.Deliverables() = %v, want %v", got, want)
	}
}

// The flag is not an annotation, and this is the assertion that keeps it from
// quietly becoming one: a first-party tool annotated as a shared write still
// does not deliver unless it was declared. Remove the Delivers() call in the
// registration above and the previous test goes red; remove this one and
// nothing stops a future contributor from deriving the set again.
func TestAFirstPartyWriteDoesNotDeliverByAnnotationAlone(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	if err := r.RegisterWith(deliverStub{"run_sandbox"}, tools.OriginBuiltin,
		// The real run_sandbox annotations: a shared write by the
		// classifier's reading, and still not an answer anybody is
		// waiting for — a coding run reports back to its own executor.
		tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.Yes}); err != nil {
		t.Fatal(err)
	}
	if got := r.Deliverables(); len(got) != 0 {
		t.Errorf("Deliverables() = %v, want none: annotations must not confer delivery", got)
	}
}

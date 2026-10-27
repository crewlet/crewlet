package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// A WRITE TOOL THAT KEEPS REFUSING ON ITS POLICY IS STOPPED IN PLAIN WORDS: the
// refusal that reaches the limit, and every one after it, tells the model to
// stop retrying and raise a blocker on the work item, while the refusal
// itself is answered whole and the call still runs (never a silent cap). The
// GoPhrase brand turn made fourteen attempts with nothing to stop it.
//
// Mutation: drop refusalLoop from execute and no answer says STOP.
func TestAWriteToolThatKeepsRefusingIsStoppedInPlainWords(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	write := &fakeTool{name: "create_thing", out: "refused: fix the index", failed: true,
		cause: tools.RefusalInvalid}
	read := &fakeTool{name: "read_thing", out: "refused: no such kind", failed: true,
		cause: tools.RefusalInvalid}
	stale := &fakeTool{name: "publish_thing", out: "stale", failed: true, cause: tools.RefusalStaleVersion}
	if err := r.RegisterWith(write, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWith(read, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.Yes}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterWith(stale, tools.OriginBuiltin, tools.Annotations{ReadOnly: mcp.No}); err != nil {
		t.Fatal(err)
	}
	s := tools.NewSurface("execute", r.Snapshot(), []string{"create_thing", "read_thing", "publish_thing"})
	for i := 1; i <= tools.PolicyRefusalLimit+1; i++ {
		for _, name := range []string{"create_thing", "read_thing", "publish_thing"} {
			res, err := s.Execute(context.Background(), llm.ToolCall{Name: name})
			if err != nil {
				t.Fatal(err)
			}
			stopped := strings.Contains(res.Output, "STOP RETRYING "+name)
			want := name == "create_thing" && i >= tools.PolicyRefusalLimit
			if stopped != want {
				t.Errorf("%s, attempt %d: told to stop %v, want %v: %q", name, i, stopped, want, res.Output)
			}
			if !res.Failed || (!strings.HasPrefix(res.Output, "refused") && name != "publish_thing") {
				t.Errorf("%s, attempt %d: the refusal was not answered whole: %q", name, i, res.Output)
			}
			if want && !strings.Contains(res.Output, "Raise a blocker on the work item") {
				t.Errorf("the stop does not say to raise a blocker: %q", res.Output)
			}
		}
	}
	if got := len(write.seen); got != tools.PolicyRefusalLimit+1 {
		t.Errorf("the write ran %d times, want every call run (no silent cap)", got)
	}
	// THE LEDGER RECORDS WHAT THE MODEL WAS TOLD.
	calls := s.Calls()
	if last := calls[len(calls)-3]; !strings.Contains(last.Output, "STOP RETRYING") {
		t.Errorf("the ledger's last write is %q, want the answer the model read", last.Output)
	}
}

package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

type fakeTool struct {
	name   string
	desc   string
	params map[string]any
	out    string
	failed bool
	err    error
	seen   []map[string]any
}

func (f *fakeTool) Name() string               { return f.name }
func (f *fakeTool) Description() string        { return f.desc }
func (f *fakeTool) Parameters() map[string]any { return f.params }
func (f *fakeTool) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	f.seen = append(f.seen, args)
	if f.err != nil {
		return tools.Result{}, f.err
	}
	return tools.Result{Output: f.out, Failed: f.failed}, nil
}

func tool(name string) *fakeTool { return &fakeTool{name: name, out: "ok"} }

func mustRegister(t *testing.T, r *tools.Registry, tl tools.Callable, origin string) {
	t.Helper()
	if err := r.Register(tl, origin); err != nil {
		t.Fatalf("Register(%s): %v", tl.Name(), err)
	}
}

func TestOriginIsRecordedBecauseItCannotBeRecovered(t *testing.T) {
	t.Parallel()
	// A tool an MCP server serves is structurally identical to one the
	// engine ships. With nothing recorded, a tool missing because its
	// server failed to start reads as a missing builtin — which sends
	// someone to debug the wrong subsystem.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("reflect"), tools.OriginBuiltin)
	mustRegister(t, r, tool("deploy"), tools.Origin("ops"))
	mustRegister(t, r, tool("slack_post"), tools.Origin("slack"))

	for name, want := range map[string]string{
		"reflect":    "builtin",
		"deploy":     "mcp:ops",
		"slack_post": "mcp:slack",
	} {
		e, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if e.Origin != want {
			t.Errorf("%s origin = %q, want %q", name, e.Origin, want)
		}
	}
}

func TestADuplicateNameIsRefusedRatherThanOverwritten(t *testing.T) {
	t.Parallel()
	// Overwriting means the second registrant silently wins and the first's
	// tool vanishes from every prompt — and with the two structurally
	// identical, nothing downstream can report which one the agent now
	// calls.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("post"), tools.OriginBuiltin)
	err := r.Register(tool("post"), tools.Origin("ops"))
	if err == nil {
		t.Fatal("a duplicate name registered cleanly")
	}
	var dup *tools.ErrDuplicate
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want *ErrDuplicate", err)
	}
	// Both origins named, or the operator cannot tell which two things
	// collided.
	if !strings.Contains(err.Error(), "builtin") || !strings.Contains(err.Error(), "mcp:ops") {
		t.Errorf("the error names neither side: %v", err)
	}
	if e, _ := r.Lookup("post"); e.Origin != tools.OriginBuiltin {
		t.Errorf("the incumbent was replaced: %s", e.Origin)
	}
}

func TestRegistrationIsRefusedWithoutTheFactsItCannotRecover(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	for name, call := range map[string]func() error{
		"nil tool":  func() error { return r.Register(nil, tools.OriginBuiltin) },
		"no name":   func() error { return r.Register(tool(""), tools.OriginBuiltin) },
		"no origin": func() error { return r.Register(tool("x"), "") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: registered cleanly", name)
		}
	}
}

func TestOrderIsRegistrationOrderSoAPromptDoesNotChurn(t *testing.T) {
	t.Parallel()
	// A map range would reorder the catalogue every turn and invalidate the
	// provider's prefix cache on a prompt that did not change.
	r := tools.NewRegistry()
	want := []string{"zulu", "alpha", "mike", "bravo"}
	for _, n := range want {
		mustRegister(t, r, tool(n), tools.OriginBuiltin)
	}
	for range 30 {
		if got := r.Names(); !slices.Equal(got, want) {
			t.Fatalf("names = %v, want registration order %v", got, want)
		}
		// List has its own loop and its own chance to range the map.
		if got := entryNames(r.List()); !slices.Equal(got, want) {
			t.Fatalf("list = %v, want registration order %v", got, want)
		}
		if got := r.Snapshot().Names(); !slices.Equal(got, want) {
			t.Fatalf("snapshot = %v, want registration order %v", got, want)
		}
	}

	// The returned slice must not be the registry's own: a caller sorting
	// it for display would silently reorder every prompt after it.
	got := r.Names()
	slices.Sort(got)
	if after := r.Names(); !slices.Equal(after, want) {
		t.Errorf("Names aliases internal state: after a caller sorted it, %v", after)
	}
}

func TestUnregisteringAnOriginLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	// The listing and the lookup index are two structures and both have to
	// forget. A name gone from the order but still in the index is a tool
	// no prompt offers and every call still reaches — invisible to every
	// listing, and live.
	//
	// Found by mutation: deleting only the first of a doomed set passed,
	// because nothing looked the survivors up by name.
	r := tools.NewRegistry()
	for _, n := range []string{"gh_a", "gh_b", "gh_c"} {
		mustRegister(t, r, tool(n), tools.Origin("github"))
	}
	r.UnregisterOrigin(tools.Origin("github"))
	for _, n := range []string{"gh_a", "gh_b", "gh_c"} {
		if _, ok := r.Lookup(n); ok {
			t.Errorf("%s is gone from the listing but still resolves", n)
		}
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("list = %v, want empty", entryNames(got))
	}
}

func TestASnapshotIsNotAliveToLaterRegistrations(t *testing.T) {
	t.Parallel()
	// Sharing the registry's own index would make a snapshot resolve tools
	// registered after it was taken — which is the moving target it exists
	// to stop.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("a"), tools.OriginBuiltin)
	snap := r.Snapshot()
	mustRegister(t, r, tool("b"), tools.OriginBuiltin)

	if _, ok := snap.Lookup("b"); ok {
		t.Error("the snapshot resolved a tool registered after it was taken")
	}
	if got := snap.Names(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("snapshot names = %v", got)
	}
}

func TestUnregisteringAnOriginTakesExactlyItsTools(t *testing.T) {
	t.Parallel()
	// A server restart unregisters then re-registers. Computing the doomed
	// set while mutating the map means its second half is decided by a map
	// already changing underneath.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("keep"), tools.OriginBuiltin)
	for _, n := range []string{"gh_a", "gh_b", "gh_c"} {
		mustRegister(t, r, tool(n), tools.Origin("github"))
	}
	mustRegister(t, r, tool("slack_post"), tools.Origin("slack"))

	got := r.UnregisterOrigin(tools.Origin("github"))
	if !slices.Equal(got, []string{"gh_a", "gh_b", "gh_c"}) {
		t.Errorf("removed %v, want all three github tools", got)
	}
	if !slices.Equal(r.Names(), []string{"keep", "slack_post"}) {
		t.Errorf("survivors = %v", r.Names())
	}
	// And the name is free again, which is what makes a restart work.
	if err := r.Register(tool("gh_a"), tools.Origin("github")); err != nil {
		t.Errorf("re-registering after a restart: %v", err)
	}
}

func TestUnregisterReportsWhetherItDidAnything(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	mustRegister(t, r, tool("x"), tools.OriginBuiltin)
	if !r.Unregister("x") {
		t.Error("removing a registered tool reported nothing removed")
	}
	if r.Unregister("x") {
		t.Error("removing an absent tool reported a removal")
	}
}

func TestAnOriginViewCannotGetItsOwnOriginWrong(t *testing.T) {
	t.Parallel()
	// A registrant handed the bare registry could register under "builtin"
	// by omission, and the whole grammar exists to stop that reading.
	r := tools.NewRegistry()
	view := r.ForOrigin(tools.Origin("ops"))
	if err := view.Register(tool("deploy")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if e, _ := r.Lookup("deploy"); e.Origin != "mcp:ops" {
		t.Errorf("origin = %q", e.Origin)
	}
	if got := view.Unregister(); !slices.Equal(got, []string{"deploy"}) {
		t.Errorf("the view removed %v", got)
	}
}

func TestMCPToolsAndKnownReadsAreReportedSeparately(t *testing.T) {
	t.Parallel()
	// The delivery gate reads both, and they answer different questions: a
	// delivery only ever comes from an MCP server, and an explicit read
	// through one is still recon.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("reflect"), tools.OriginBuiltin)
	if err := r.RegisterWith(tool("slack_post"), tools.Origin("slack"), tools.Annotations{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.RegisterWith(tool("slack_history"), tools.Origin("slack"),
		tools.Annotations{ReadOnly: mcp.Yes}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if got := r.MCPNames(); !slices.Equal(got, []string{"slack_post", "slack_history"}) {
		t.Errorf("MCP names = %v", got)
	}
	if got := r.KnownReads(); !slices.Equal(got, []string{"slack_history"}) {
		t.Errorf("known reads = %v", got)
	}
}

func TestOnlyAPositiveReadHintCounts(t *testing.T) {
	t.Parallel()
	// The trap: an UNANNOTATED tool is not a known read. Treating unknown
	// as read exempts most of a fresh MCP server from the delivery fence.
	r := tools.NewRegistry()
	for name, hint := range map[string]mcp.Hint{
		"unannotated": mcp.Unknown,
		"denied":      mcp.No,
		"asserted":    mcp.Yes,
	} {
		if err := r.RegisterWith(tool(name), tools.Origin("s"),
			tools.Annotations{ReadOnly: hint}); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	if got := r.KnownReads(); !slices.Equal(got, []string{"asserted"}) {
		t.Errorf("known reads = %v, want only the positively annotated one", got)
	}
}

func TestTheCatalogueNamesServersRatherThanExpandingThem(t *testing.T) {
	t.Parallel()
	// A real server publishes dozens of tools and a planner shown all of
	// them plans against a wall of text. Naming the server keeps the prompt
	// prefix stable while its catalogue changes underneath.
	r := tools.NewRegistry()
	first := tool("reflect")
	first.desc = "Record a lesson.\nMore detail on a second line."
	mustRegister(t, r, first, tools.OriginBuiltin)
	for _, n := range []string{"gh_a", "gh_b"} {
		mustRegister(t, r, tool(n), tools.Origin("github"))
	}

	got := r.Catalogue()
	if !strings.Contains(got, "- reflect: Record a lesson.") {
		t.Errorf("the builtin is missing its one-line description:\n%s", got)
	}
	// A MULTI-LINE DESCRIPTION SURVIVES WHOLE, indented under its bullet.
	// Keeping only the first line preserved the one-entry-per-bullet shape
	// by throwing away the content that shape was carrying — a tool's
	// argument rules and preconditions live below its opening sentence.
	if !strings.Contains(got, "More detail on a second line.") {
		t.Errorf("a description was cut at its first line:\n%s", got)
	}
	if !strings.Contains(got, "\n  More detail") {
		t.Errorf("a continuation line was not indented under its bullet:\n%s", got)
	}
	if strings.Contains(got, "gh_a") {
		t.Errorf("an MCP server's tools were expanded:\n%s", got)
	}
	if !strings.Contains(got, "`github`") {
		t.Errorf("the MCP server is not named:\n%s", got)
	}
	// One line per server, however many tools it serves.
	if n := strings.Count(got, "github"); n != 1 {
		t.Errorf("the server is named %d times:\n%s", n, got)
	}
}

func TestAnEmptyCatalogueSaysSo(t *testing.T) {
	t.Parallel()
	// An empty string in a prompt reads as a section the engine forgot to
	// fill in.
	if got := tools.NewRegistry().Catalogue(); got != "(no tools available)" {
		t.Errorf("catalogue = %q", got)
	}
}

func TestASnapshotDoesNotMoveUnderAPhase(t *testing.T) {
	t.Parallel()
	// A server restarting mid-turn would otherwise change what a phase is
	// judged against between the call and the delivery gate.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("slack_post"), tools.Origin("slack"))
	snap := r.Snapshot()

	r.UnregisterOrigin(tools.Origin("slack"))
	mustRegister(t, r, tool("jira_create"), tools.Origin("jira"))

	if !slices.Equal(snap.Names(), []string{"slack_post"}) {
		t.Errorf("the snapshot moved: %v", snap.Names())
	}
	if _, ok := snap.Lookup("jira_create"); ok {
		t.Error("the snapshot picked up a later registration")
	}
}

func TestASurfaceOffersOnlyWhatWasActivated(t *testing.T) {
	t.Parallel()
	// Everything in the snapshot is REACHABLE but not offered — that is
	// what discovery is for, and why a planner is not handed the whole of a
	// large server's catalogue in its prompt.
	r := tools.NewRegistry()
	for _, n := range []string{"a", "b", "c"} {
		mustRegister(t, r, tool(n), tools.OriginBuiltin)
	}
	s := tools.NewSurface("plan", r.Snapshot(), []string{"a"})
	if got := defNames(s.ToolDefs()); !slices.Equal(got, []string{"a"}) {
		t.Errorf("offered %v, want just the activated one", got)
	}
	if !s.Activate("b") {
		t.Error("activating a registered tool reported a miss")
	}
	if got := defNames(s.ToolDefs()); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("offered %v after activation", got)
	}
	if s.Activate("nope") {
		t.Error("activating an unregistered tool reported success")
	}
}

func TestActivatingTwiceOffersTheToolOnce(t *testing.T) {
	t.Parallel()
	// A duplicate in the offered list is a duplicate in the request, which
	// the vendor rejects — so one model repeating itself would fail the
	// whole round.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("a"), tools.OriginBuiltin)
	s := tools.NewSurface("plan", r.Snapshot(), []string{"a"})
	s.Activate("a")
	if got := defNames(s.ToolDefs()); !slices.Equal(got, []string{"a"}) {
		t.Errorf("offered %v", got)
	}
}

func TestAToolWithNoArgumentsGetsAValidSchema(t *testing.T) {
	t.Parallel()
	// A nil parameters map marshals to `null`, which vendors reject as an
	// invalid tool schema — so one zero-argument tool fails the whole
	// request rather than just itself.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("noargs"), tools.OriginBuiltin)
	defs := tools.NewSurface("plan", r.Snapshot(), []string{"noargs"}).ToolDefs()
	if len(defs) != 1 {
		t.Fatalf("defs = %v", defs)
	}
	blob, err := json.Marshal(defs[0].Parameters)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(blob) == "null" {
		t.Error("the schema marshalled to null")
	}
	if !strings.Contains(string(blob), `"type":"object"`) {
		t.Errorf("schema = %s, want an object", blob)
	}
}

func TestAnUnofferedToolAndAnUnknownOneReadDifferently(t *testing.T) {
	t.Parallel()
	// The model can act on the difference: a name that exists but was not
	// offered is something to activate; a name that does not exist is
	// something to stop trying.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("offered"), tools.OriginBuiltin)
	mustRegister(t, r, tool("registered"), tools.OriginBuiltin)
	s := tools.NewSurface("execute", r.Snapshot(), []string{"offered"})

	res, err := s.Execute(context.Background(), llm.ToolCall{Name: "registered"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "activate") {
		t.Errorf("an unoffered tool reported %q", res.Output)
	}
	res, err = s.Execute(context.Background(), llm.ToolCall{Name: "ghost"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "Unknown tool") {
		t.Errorf("an unknown tool reported %q", res.Output)
	}
}

func TestAHallucinatedToolIsAFailedResultNotAnError(t *testing.T) {
	t.Parallel()
	// A Go error here tears down the turn. The model asked for something it
	// cannot have, which is a thing to tell it about.
	s := tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil)
	if _, err := s.Execute(context.Background(), llm.ToolCall{Name: "ghost"}); err != nil {
		t.Errorf("a hallucinated name produced a Go error: %v", err)
	}
}

func TestATornDownTurnIsAnErrorAndIsNotRecorded(t *testing.T) {
	t.Parallel()
	// The caller's own context ended. Nothing is reported to the model and
	// nothing goes in the ledger: as far as the record is concerned this
	// call did not happen.
	r := tools.NewRegistry()
	broken := tool("x")
	broken.err = context.Canceled
	mustRegister(t, r, broken, tools.OriginBuiltin)
	s := tools.NewSurface("execute", r.Snapshot(), []string{"x"})

	if _, err := s.Execute(context.Background(), llm.ToolCall{Name: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context error", err)
	}
	if got := s.Calls(); len(got) != 0 {
		t.Errorf("a torn-down call was recorded: %v", got)
	}
}

func TestTheSurfaceRecordsWhatItRanInOrder(t *testing.T) {
	t.Parallel()
	// The ledger and the delivery gate both read this, and a call that ran
	// but was not recorded is a delivery the gate cannot see.
	r := tools.NewRegistry()
	ok := tool("post")
	bad := tool("fail")
	bad.failed, bad.out = true, "channel_not_found"
	mustRegister(t, r, ok, tools.OriginBuiltin)
	mustRegister(t, r, bad, tools.OriginBuiltin)
	s := tools.NewSurface("execute", r.Snapshot(), []string{"post", "fail"})

	ctx := context.Background()
	if _, err := s.Execute(ctx, llm.ToolCall{Name: "post", Arguments: map[string]any{"channel": "C1"}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := s.Execute(ctx, llm.ToolCall{Name: "fail"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := s.Calls()
	if len(got) != 2 {
		t.Fatalf("recorded %d calls, want 2", len(got))
	}
	if got[0].Name != "post" || got[0].Args["channel"] != "C1" || got[0].Failed {
		t.Errorf("first call = %+v", got[0])
	}
	if got[1].Name != "fail" || !got[1].Failed || got[1].Output != "channel_not_found" {
		t.Errorf("second call = %+v", got[1])
	}
	if !slices.Equal(s.CalledNames(), []string{"post", "fail"}) {
		t.Errorf("called names = %v", s.CalledNames())
	}
}

func TestNilArgumentsReachATheToolAsAnEmptyMap(t *testing.T) {
	t.Parallel()
	// A tool indexing a nil map is fine in Go, but one RANGING to build a
	// request and one checking len() are not the same, and a nil is the
	// shape most likely to be handled differently by accident.
	r := tools.NewRegistry()
	f := tool("noargs")
	mustRegister(t, r, f, tools.OriginBuiltin)
	s := tools.NewSurface("execute", r.Snapshot(), []string{"noargs"})
	if _, err := s.Execute(context.Background(), llm.ToolCall{Name: "noargs"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(f.seen) != 1 || f.seen[0] == nil {
		t.Errorf("the tool saw %v, want an empty map", f.seen)
	}
}

func TestAnUnresolvableNameIsNeverActivated(t *testing.T) {
	t.Parallel()
	// This is the invariant ToolDefs relies on: Activate is the only writer
	// of the offered list and it admits nothing the frozen snapshot cannot
	// resolve. Break it and ToolDefs dereferences a nil tool.
	r := tools.NewRegistry()
	mustRegister(t, r, tool("a"), tools.OriginBuiltin)
	s := tools.NewSurface("execute", r.Snapshot(), []string{"a", "gone"})
	if got := s.Active(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("active = %v, want only what resolves", got)
	}
	if got := defNames(s.ToolDefs()); !slices.Equal(got, []string{"a"}) {
		t.Errorf("offered %v, want only what resolves", got)
	}
}

func defNames(defs []llm.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func entryNames(es []tools.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

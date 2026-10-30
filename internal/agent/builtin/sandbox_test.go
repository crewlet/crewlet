package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// launchSpy records what the tool asked for.
type launchSpy struct {
	turns  []string
	briefs []string
	repos  [][]builtin.RepoRequest
	err    error
}

func (s *launchSpy) Launch(_ context.Context, turn *turnctx.Turn, req builtin.SandboxRequest) (sandbox.LaunchResult, error) {
	if s.err != nil {
		return sandbox.LaunchResult{}, s.err
	}
	s.turns = append(s.turns, turn.RunID)
	s.briefs = append(s.briefs, req.Brief)
	s.repos = append(s.repos, req.Repos)
	return sandbox.LaunchResult{SandboxID: "box-1", CommandID: "cmd-1", CodingAgent: "claude-code"}, nil
}

// sandboxSurface builds an Execute surface offering run_sandbox, bound to a
// turn — which is how the runner builds one.
func sandboxSurface(t *testing.T, launcher builtin.SandboxLauncher) *tools.Surface {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: launcher})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{builtin.RunSandboxTool})
	return surface.ForTurn(turnFor(t, "agent-ceo"))
}

func TestRunSandboxIsOmittedWithoutALauncher(t *testing.T) {
	reg := tools.NewRegistry()
	names, err := builtin.Register(reg, gated(builtin.Deps{}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range names {
		if name == builtin.RunSandboxTool {
			t.Fatal("run_sandbox was registered with no launcher — a seat would plan around a box it cannot start")
		}
	}
}

func TestRunSandboxIsRegisteredWithALauncher(t *testing.T) {
	reg := tools.NewRegistry()
	names, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: &launchSpy{}}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !slices.Contains(names, builtin.RunSandboxTool) {
		t.Fatalf("run_sandbox was not registered; got %v", names)
	}
}

// A coding run pushes branches and opens pull requests, so a sub-agent acting
// under its parent's identity must not reach it.
func TestRunSandboxIsClassifiedAsWritingToASharedSurface(t *testing.T) {
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: &launchSpy{}})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, ok := reg.Snapshot().Lookup(builtin.RunSandboxTool)
	if !ok {
		t.Fatal("run_sandbox is not in the registry")
	}
	if !mcp.WritesToSharedSurface(entry.Annotations) {
		t.Fatalf("run_sandbox reads as private: %+v", entry.Annotations)
	}
}

func TestALaunchedRunSuspendsTheLoop(t *testing.T) {
	spy := &launchSpy{}
	surface := sandboxSurface(t, spy)

	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{
		"brief": "Clone example.com/acme/api and fix the failing test",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Suspend {
		t.Fatal("a launched coding job did not suspend the loop — the turn would end believing the work was done")
	}
	if res.Failed {
		t.Fatalf("a successful launch reported failure: %q", res.Output)
	}
	if res.SuspendPayload["sandbox_id"] != "box-1" {
		t.Fatalf("payload = %v, want the box it started", res.SuspendPayload)
	}
	if len(spy.briefs) != 1 || !strings.Contains(spy.briefs[0], "acme/api") {
		t.Fatalf("briefs = %v", spy.briefs)
	}
	// THE TURN COMES FROM THE SURFACE, never from the arguments: a model
	// that spelled a different id could otherwise start a box against
	// somebody else's suspended conversation.
	//
	// And it is the RUN, because the pending row is keyed on it: two runs
	// of one redelivered trigger would otherwise share one row and one box.
	// See ADR-0017.
	if len(spy.turns) != 1 || spy.turns[0] != "run-1" {
		t.Fatalf("turns = %v, want the calling turn", spy.turns)
	}
}

// A launch the engine could not do is a tool failure the model can react to,
// not an engine error that loses the turn.
func TestAFailedLaunchIsAToolFailureAndDoesNotSuspend(t *testing.T) {
	surface := sandboxSurface(t, &launchSpy{err: errors.New("no sandbox capacity")})

	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{
		"brief": "fix the flake",
	}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Suspend {
		t.Fatal("a failed launch suspended the loop — nothing would ever resume it")
	}
	if !res.Failed || !strings.Contains(res.Output, "no sandbox capacity") {
		t.Fatalf("result = %+v, want a failure naming the reason", res)
	}
}

func TestAnEmptyBriefIsRefusedWithoutLaunching(t *testing.T) {
	spy := &launchSpy{}
	surface := sandboxSurface(t, spy)

	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: map[string]any{"brief": "   "}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Suspend || !res.Failed {
		t.Fatalf("result = %+v, want a refusal", res)
	}
	if len(spy.briefs) != 0 {
		t.Fatal("an empty brief still started a box")
	}
}

// reposSurface is an Execute surface offering run_sandbox where the company's
// code host is served, so `repos` is offered beside the brief.
func reposSurface(t *testing.T, launcher builtin.SandboxLauncher) (*tools.Surface, tools.Entry) {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: launcher, Code: codeDeps(newCodeHost())})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, ok := reg.Snapshot().Lookup(builtin.RunSandboxTool)
	if !ok {
		t.Fatal("run_sandbox is not in the registry")
	}
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{builtin.RunSandboxTool})
	return surface.ForTurn(turnFor(t, "agent-ceo")), entry
}

// RUN_SANDBOX TAKES REPOS where the company's code host is served: the schema
// offers `repos` — at most eight, each `repo` required, `access` read or
// write, a cone of at most 64 directories — and a valid call reaches the
// launcher as one request naming them in order; every malformed item is
// refused before the launcher is asked, naming the item and the field, and
// so are a ninth repository and one repository named twice. Where the code
// host is not served, the schema offers no `repos` and a call naming some is
// refused saying why.
//
// STRICT, because what a repository is given is a grant: a key misspelt and
// dropped would hand the run a default it never asked for.
//
// Mutations: read `repos` with the tolerant readers (an unknown key dropped)
// → the unknown-key row reaches the launcher, red; drop the bound → the nine
// row reaches it, red.
func TestRunSandboxTakesRepos(t *testing.T) {
	t.Parallel()
	spy := &launchSpy{}
	surface, entry := reposSurface(t, spy)
	schema, _ := entry.Tool.Parameters()["properties"].(map[string]any)
	repos, _ := schema["repos"].(map[string]any)
	if repos == nil || repos["maxItems"] != sandbox.MaxRunRepos {
		t.Fatalf("the schema's repos is %v, want a list of at most %d", schema["repos"], sandbox.MaxRunRepos)
	}
	if !strings.Contains(entry.Tool.Description(), "Name this company's repositories in `repos`") {
		t.Errorf("the description does not say to name repositories in `repos`: %q", entry.Tool.Description())
	}

	call := func(args map[string]any) tools.Result {
		t.Helper()
		res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool, Arguments: args})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return tools.Result{Output: res.Output, Failed: res.Failed}
	}
	valid := call(map[string]any{"brief": "fix it", "repos": []any{
		map[string]any{"repo": "site", "branch": "fix/a", "access": "write", "paths": []any{"src", "docs"}},
		// NULL IS UNSET, as an omitted field is.
		map[string]any{"repo": "api", "base": "v1", "access": nil},
	}})
	if valid.Failed {
		t.Fatalf("a valid call was refused: %q", valid.Output)
	}
	want := []builtin.RepoRequest{
		{Repo: "site", Branch: "fix/a", Access: "write", Paths: []string{"src", "docs"}},
		{Repo: "api", Base: "v1"},
	}
	if len(spy.repos) != 1 || !slices.EqualFunc(spy.repos[0], want, func(a, b builtin.RepoRequest) bool {
		return a.Repo == b.Repo && a.Base == b.Base && a.Branch == b.Branch && a.Access == b.Access &&
			slices.Equal(a.Paths, b.Paths)
	}) {
		t.Fatalf("the launcher was asked %+v, want %+v", spy.repos, want)
	}

	nine := make([]any, sandbox.MaxRunRepos+1)
	for i := range nine {
		nine[i] = map[string]any{"repo": "r" + strings.Repeat("x", i+1)}
	}
	cone := make([]any, sandbox.MaxConePaths+1)
	for i := range cone {
		cone[i] = "d"
	}
	for _, c := range []struct {
		name  string
		repos any
		says  string
	}{
		{"not a list", "site", "must be a list of objects"},
		{"an item that is not an object", []any{"site"}, "repos[0] is not an object"},
		{"an unknown key", []any{map[string]any{"repo": "site", "acess": "write"}}, `repos[0] holds "acess"`},
		{"no repo", []any{map[string]any{"branch": "x"}}, "repos[0] needs `repo`"},
		{"an access outside its two", []any{map[string]any{"repo": "site", "access": "admin"}},
			`repos[0].access is "admin"`},
		// A FIELD OF THE WRONG TYPE IS A REQUEST, NOT NONE: read as no
		// access asked, a list would give the run write wherever the seat
		// may push, and an object branch would put it on the seat's own.
		{"an access that is a list", []any{map[string]any{"repo": "site", "access": []any{"read"}}},
			"repos[0].access is not a string"},
		{"a branch that is an object", []any{map[string]any{"repo": "site",
			"branch": map[string]any{"name": "fix/a"}}}, "repos[0].branch is not a string"},
		{"a base that is a number", []any{map[string]any{"repo": "site", "base": float64(1234567)}},
			"repos[0].base is not a string"},
		{"a repo that is a bool", []any{map[string]any{"repo": true}}, "repos[0].repo is not a string"},
		{"paths that are not a list", []any{map[string]any{"repo": "site", "paths": "src"}},
			"repos[0].paths must be a list"},
		{"a cone past its bound", []any{map[string]any{"repo": "site", "paths": cone}},
			"a cone names at most 64"},
		{"nine repositories", nine, "one run is given at most 8"},
		{"one repository twice", []any{map[string]any{"repo": "site"}, map[string]any{"repo": "site"}},
			"site is named twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := call(map[string]any{"brief": "fix it", "repos": c.repos})
			if !got.Failed || !strings.Contains(got.Output, c.says) {
				t.Errorf("answered %q, want a refusal saying %q", got.Output, c.says)
			}
		})
	}
	if len(spy.repos) != 1 {
		t.Errorf("the launcher was asked %d times, want once: a malformed call reached it", len(spy.repos))
	}

	// NO CODE HOST, NO `repos`.
	plain := &launchSpy{}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: plain})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	bare, _ := reg.Snapshot().Lookup(builtin.RunSandboxTool)
	if props, _ := bare.Tool.Parameters()["properties"].(map[string]any); props["repos"] != nil {
		t.Errorf("a company whose code host is not served is offered repos: %v", props["repos"])
	}
	res, err := sandboxSurface(t, plain).Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool,
		Arguments: map[string]any{"brief": "fix it", "repos": []any{map[string]any{"repo": "site"}}}})
	if err != nil || !res.Failed || !strings.Contains(res.Output, `takes no argument "repos"`) {
		t.Errorf("a call naming repos where none is offered answered %v %q", err, res.Output)
	}
	if len(plain.briefs) != 0 {
		t.Errorf("a call naming repos where none is offered was launched: %v", plain.briefs)
	}
}

// A LAUNCH REFUSED WITH A COMPOSED RESULT IS ANSWERED AS IT IS: a repository
// the seat may not be given is refused in the code host's own words and
// class, never as a sandbox that could not be started.
func TestARefusedLaunchIsAnsweredAsComposed(t *testing.T) {
	t.Parallel()
	composed := tools.Result{Output: "run_sandbox: there is no repository \"gone\"", Failed: true,
		Cause: tools.RefusalNotFound}
	surface, _ := reposSurface(t, &launchSpy{err: fmt.Errorf("engine: %w",
		&builtin.RefusedLaunch{Result: composed})})
	res, err := surface.Execute(t.Context(), llm.ToolCall{ID: "c1", Name: builtin.RunSandboxTool,
		Arguments: map[string]any{"brief": "fix it", "repos": []any{map[string]any{"repo": "gone"}}}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Output != composed.Output || res.Suspend {
		t.Errorf("answered %q (suspend %v), want the composed refusal %q", res.Output, res.Suspend,
			composed.Output)
	}
}

// A surface that ran this tool without the detached seam would have the turn
// end believing the work was done while the job was still running.
func TestTheNonDetachedPathRefuses(t *testing.T) {
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Sandbox: &launchSpy{}})); err != nil {
		t.Fatalf("Register: %v", err)
	}
	entry, _ := reg.Snapshot().Lookup(builtin.RunSandboxTool)
	res, err := entry.Tool.Call(everyGrant(), map[string]any{"brief": "fix it"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.Failed {
		t.Fatal("the non-detached path ran the job and answered normally")
	}
	if !strings.Contains(res.Output, "suspend") {
		t.Fatalf("the refusal does not say why: %q", res.Output)
	}
}

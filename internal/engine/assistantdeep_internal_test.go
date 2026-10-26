package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE ENGINE'S HALF OF DEEP WORK (ADR-0054), over the fake sandbox backend and
// the real bridge: a run's box, its brief, its inputs and outbox, its one way
// into Crewlet, its caps and its box's life.

// deepCompany is a company whose deep work runs on an agent-mode Claude Code
// entry, in the e2b cell it configures, signed in by an API key.
func deepCompany(t *testing.T) *Company {
	t.Helper()
	deep, err := cliagent.New(cliagent.Config{
		Key: "deep", Agent: "claude-code", Model: "sonnet", AgentMode: true,
		StateDir: t.TempDir(), Timeout: time.Minute,
		Auth: cliagent.Auth{Mode: cliagent.AuthAPIKey, APIKey: "sk-ant-not-real"},
	})
	if err != nil {
		t.Fatalf("cliagent.New: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "deep", Provider: deep}})
	if err != nil {
		t.Fatal(err)
	}
	on := true
	return &Company{Models: models, Org: &org.Organization{Name: "Acme"},
		Config: &config.Company{
			Name: "Acme",
			Providers: config.Providers{
				LLM: map[string]config.LLMProvider{"deep": {
					Agent: config.CLIClaudeCode, Model: "sonnet", Mode: config.CLIModeAgent,
					Auth: config.CLIAgentAuth{Mode: config.AuthAPIKey, APIKey: "${ANTHROPIC_API_KEY}"},
				}},
				Sandbox: &config.SandboxProvider{E2B: &config.E2BSandbox{}},
			},
			Assistant: config.Assistant{Enabled: &on,
				DeepWork: &config.AssistantDeepWork{LLM: "deep"}},
		}}
}

// deepRig is the deep-work backend of an engine running deepCompany, its jobs
// and its boxes fakes, its bridge real and served.
type deepRig struct {
	deep   *assistantDeep
	jobs   *sandbox.FakeRunner
	boxes  *sandbox.FakeProvider
	bridge *mcpbridge.Bridge
}

func newDeepRig(t *testing.T) deepRig {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"),
		BaseURL: server.URL})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	server.Config.Handler = mux
	e, jobs := launchRig(t, deepCompany(t), bridge)
	boxes, err := e.sandboxManager().Provider(sandbox.E2B)
	if err != nil {
		t.Fatal(err)
	}
	deep := AssistantDeepWork(e, nil, DeepHost{}).(*assistantDeep)
	deep.poll = 10 * time.Millisecond
	return deepRig{deep: deep, jobs: jobs, boxes: boxes.(*sandbox.FakeProvider), bridge: bridge}
}

// whoSurface is a run's one Crewlet tool, answering who its call was decided
// as, and counting its calls.
type whoSurface struct{ calls atomic.Int32 }

func (*whoSurface) Revision() uint64 { return 1 }
func (*whoSurface) ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{{Name: "get_task", Description: "read a task",
		Parameters: map[string]any{"type": "object"}}}
}
func (*whoSurface) AnnotationsOf(string) crewletmcp.Annotations { return crewletmcp.Annotations{} }
func (w *whoSurface) Execute(ctx context.Context, _ llm.ToolCall) (toolloop.ToolResult, error) {
	w.calls.Add(1)
	_, how := iam.From(ctx)
	return toolloop.ToolResult{Output: string(how)}, nil
}

// started waits for the run's job and answers its request.
func (r deepRig) started(t *testing.T) sandbox.RunRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := r.jobs.Started(); len(got) > 0 {
			return got[len(got)-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the run's job never started")
	return sandbox.RunRequest{}
}

// A RUN IS A CODING RUN OF THE DEEP ENTRY, REACHING CREWLET ONLY THROUGH THE
// TURN'S SURFACE: its job is the entry's CLI on the entry's model with the
// brief and standing instructions it was handed, the bridge its one MCP
// server; a call over that bridge reaches the surface decided as NOBODY, so
// the request's identity reaches none of it; the attachments are in its
// inputs and its outbox comes back; its token is useless once it ends. No
// engine credential is in its environment. Mutation: decide bridged calls as
// the request and the identity reads resolved.
func TestADeepRunReachesCrewletOnlyThroughItsSurface(t *testing.T) {
	t.Parallel()
	rig := newDeepRig(t)
	surface := &whoSurface{}
	type ran struct {
		out assistant.DeepOutcome
		err error
	}
	done := make(chan ran, 1)
	go func() {
		out, err := rig.deep.Run(t.Context(), assistant.DeepRun{ID: "assistant-run-1",
			System: "standing", Brief: "Redesign the brand", Surface: surface,
			Inputs:  []assistant.DeepFile{{Name: "shot.png", Content: []byte("png")}},
			Timeout: time.Minute, Steps: 30, Tokens: 1000})
		done <- ran{out, err}
	}()
	req := rig.started(t)
	if req.Brief != "Redesign the brand" || req.System != "standing" || req.Model != "sonnet" ||
		req.Limits.MaxTurns != 30 {
		t.Fatalf("the job was started as %+v", req)
	}
	if len(req.MCPServers) != 1 {
		t.Fatalf("the job was handed %d MCP servers, want the bridge alone", len(req.MCPServers))
	}
	for name, value := range req.Env {
		if strings.HasPrefix(name, "CREWLET_") || strings.Contains(value, "test-key") {
			t.Errorf("the run's environment carries %s", name)
		}
	}
	endpoint := req.MCPServers[config.BridgeServerName].URL
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpxtest.Pool(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_task"})
	if err != nil {
		t.Fatal(err)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; text != string(iam.Unknown) {
		t.Fatalf("a bridged call was decided as %q, want nobody", text)
	}
	_ = session.Close()
	box := rig.boxes.Box("box-1")
	if got, err := box.ReadFile(t.Context(), "workspace/inputs/shot.png"); err != nil || string(got) != "png" {
		t.Fatalf("the attachment is not in the run's inputs: %q, %v", got, err)
	}
	box.Put("workspace/outbox/logo.svg", "<svg/>")
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if strings.HasPrefix(cmd, "find outbox") {
			return sandbox.ExecResult{Stdout: "outbox/logo.svg\x00"}, nil
		}
		return sandbox.ExecResult{}, nil
	}
	rig.jobs.Say(sandbox.SourceTranscript, "drew the logo\n")
	rig.jobs.Finish(sandbox.Result{Text: "Drew logo.svg.", Success: true, InputTokens: 300,
		OutputTokens: 40, SessionID: "session-9"})
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	out := got.out
	if out.End != assistant.DeepEndFinished || out.Report != "Drew logo.svg." ||
		out.Box.Sandbox != "box-1" || out.Box.Session != "session-9" || out.InputTokens != 300 {
		t.Fatalf("the run ended as %+v", out)
	}
	if len(out.Files) != 1 || out.Files[0].Name != "logo.svg" || string(out.Files[0].Content) != "<svg/>" {
		t.Fatalf("the outbox came back as %+v", out.Files)
	}
	if rig.bridge.Live() != 0 {
		t.Fatal("the run's bridge session outlived it, so its token still works")
	}
	if n := surface.calls.Load(); n != 1 {
		t.Fatalf("%d calls reached the surface, want 1", n)
	}
}

// A RUN THAT SPENDS PAST ITS TOKEN CAP IS STOPPED while it works, and says so.
// Mutation: drop the live check and the run waits for its job to end.
func TestADeepRunIsStoppedAtItsTokenCap(t *testing.T) {
	t.Parallel()
	rig := newDeepRig(t)
	done := make(chan assistant.DeepOutcome, 1)
	go func() {
		out, _ := rig.deep.Run(t.Context(), assistant.DeepRun{ID: "assistant-run-2",
			Surface: &whoSurface{}, Timeout: time.Minute, Steps: 10, Tokens: 1000})
		done <- out
	}()
	rig.started(t)
	rig.jobs.Spend(5000)
	select {
	case out := <-done:
		if out.End != assistant.DeepEndTokens {
			t.Fatalf("the run ended %s, want stopped at its token cap", out.End)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a run past its token cap went on working")
	}
}

// A STOPPED RUN ENDS AS STOPPED, and a run past its wall clock as timed out:
// the turn tells a cancellation from a limit by it.
func TestADeepRunEndsWithItsContext(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		ctx  func(context.Context) (context.Context, context.CancelFunc)
		want assistant.DeepEnd
	}{
		"stopped": {func(ctx context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(ctx)
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
			return ctx, cancel
		}, assistant.DeepEndStopped},
		"timed out": {func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, 50*time.Millisecond)
		}, assistant.DeepEndTimeout},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newDeepRig(t)
			ctx, cancel := c.ctx(t.Context())
			defer cancel()
			out, err := rig.deep.Run(ctx, assistant.DeepRun{ID: "assistant-run-" + name,
				Surface: &whoSurface{}, Timeout: time.Minute, Steps: 10, Tokens: 1000})
			if err != nil || out.End != c.want {
				t.Fatalf("the run ended %s, %v; want %s", out.End, err, c.want)
			}
		})
	}
}

// A PARKED RUN'S BOX IS KEPT FOR ITS CONTINUATION, and a continuation runs in
// it with the session it left; a discarded box is killed.
func TestAParkedRunsBoxIsKeptAndContinued(t *testing.T) {
	t.Parallel()
	rig := newDeepRig(t)
	go func() {
		rig.started(t)
		rig.jobs.Finish(sandbox.Result{Text: "Proposed.", Success: true, SessionID: "session-1"})
	}()
	out, err := rig.deep.Run(t.Context(), assistant.DeepRun{ID: "assistant-run-k",
		Surface: &whoSurface{}, Timeout: time.Minute, Steps: 10, Tokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.deep.Keep(t.Context(), out.Box); err != nil {
		t.Fatal(err)
	}
	if rig.boxes.Box(out.Box.Sandbox).Keepalives() == 0 {
		t.Fatal("the kept box's deadline was not moved to cover the approval")
	}
	cont, err := rig.deep.Run(t.Context(), assistant.DeepRun{ID: "assistant-run-k2",
		Surface: &whoSurface{}, Continue: &out.Box, Timeout: time.Minute, Steps: 10, Tokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if cont.Box.Sandbox != out.Box.Sandbox {
		t.Fatalf("the continuation ran in %s, not the kept %s", cont.Box.Sandbox, out.Box.Sandbox)
	}
	if last := rig.jobs.Started(); last[len(last)-1].Resume != "session-1" {
		t.Fatalf("the continuation did not continue the session: %+v", last[len(last)-1])
	}
	rig.deep.Discard(t.Context(), cont.Box)
	if !slices.Contains(rig.boxes.KilledIDs(), out.Box.Sandbox) {
		t.Fatal("a discarded box was not killed")
	}
}

// A HOST THAT CANNOT CONFINE A RUN RUNS NONE, and says why: deep work placed
// on the engine host is refused at Ready, never started open. Mutation:
// ignore the probe and the host cell is built unconfined.
func TestAnUnconfinableHostRefusesDeepWork(t *testing.T) {
	t.Parallel()
	rig := newDeepRig(t)
	c := deepCompany(t)
	c.Config.Providers.Sandbox = nil
	rig.deep.e.epoch.current.Store(c)
	rig.deep.probe = func() (*sandbox.HostConfinement, error) {
		return nil, &sandbox.ConfinementUnavailableError{Reason: "no mechanism here"}
	}
	err := rig.deep.Ready(t.Context())
	if !errors.Is(err, sandbox.ErrConfinementUnavailable) || !strings.Contains(err.Error(), "no mechanism here") {
		t.Fatalf("Ready = %v, want the host's confinement refusal", err)
	}
}

// THE HOST'S CONFINEMENT DENIES EVERY PATH THE ENGINE KEEPS: its config files,
// its store and estate, its stream store, its log, every CLI login, every
// seat's boxes and the operator's home.
func TestTheHostConfinementDeniesTheEnginesPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	boot := &config.Bootstrap{}
	boot.Store.Path = filepath.Join(root, "data", "node.db")
	boot.Store.ReplicatedPath = filepath.Join(root, "estate", "replicated.db")
	boot.Stream.StoreDir = filepath.Join(root, "stream")
	boot.Logging.File.Path = filepath.Join(root, "logs", "crewlet.log")
	c := deepCompany(t).Config
	login := filepath.Join(root, "logins", "deep")
	entry := c.Providers.LLM["deep"]
	entry.StateDir = login
	c.Providers.LLM["deep"] = entry
	d := &assistantDeep{boot: boot, host: DeepHost{Config: []string{filepath.Join(root, "crewlet.yaml")}}}
	deny, _ := d.confinement(c)
	home, _ := os.UserHomeDir()
	for _, want := range []string{filepath.Join(root, "crewlet.yaml"), filepath.Join(root, "data"),
		filepath.Join(root, "estate"), filepath.Join(root, "stream"), boot.Logging.File.Path,
		login, home, sandbox.DefaultBoxRoot()} {
		if !slices.Contains(deny, want) {
			t.Errorf("the confinement does not deny %s: %v", want, deny)
		}
	}
	if !strings.HasPrefix(d.boxRoot(), filepath.Join(root, "data")) {
		t.Errorf("confined boxes live at %s, outside the node's data", d.boxRoot())
	}
}

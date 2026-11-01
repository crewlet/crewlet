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

// deepCompany is a company whose deep work runs on a Claude Code entry, in the e2b cell it configures, signed in by an API key.
func deepCompany(t *testing.T) *Company {
	t.Helper()
	deep, err := cliagent.New(cliagent.Config{
		Key: "deep", Agent: "claude-code", Model: "sonnet",
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
					Harness: config.HarnessClaudeCode, Model: "sonnet",
					Auth: config.HarnessAuth{Mode: config.AuthAPIKey, APIKey: "${ANTHROPIC_API_KEY}"},
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
func (w *whoSurface) Admit(string, map[string]any) toolloop.Admitted {
	return func(ctx context.Context, _ llm.ToolCall) (toolloop.ToolResult, error) {
		w.calls.Add(1)
		_, how := iam.From(ctx)
		return toolloop.ToolResult{Output: string(how)}, nil
	}
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

// A RUN IS THE DEEP ENTRY'S CLI'S OWN RUN, REACHING CREWLET ONLY THROUGH THE
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
	// ITS COMMITS CREDIT CREWLET AND NAME NO RUN: deep work has no page a
	// `Crewlet-Run:` could lead to, and the company left the credit on.
	if req.Attribution == nil || req.Attribution.Run != "" {
		t.Errorf("the job was started asking for attribution %+v, want the co-author alone",
			req.Attribution)
	}
	for name, value := range req.Env {
		if strings.HasPrefix(name, "CREWLET_") || strings.Contains(value, "test-key") {
			t.Errorf("the run's environment carries %s", name)
		}
	}
	endpoint := req.MCPServers[config.ToolServerName].URL
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
// seat's boxes and every deep-work box — under the company's box root now and
// under one an earlier revision named — and the operator's home. Mutation:
// leave the roots this process built cells under out of the deny list, and
// the earlier one is readable.
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
	boxes := filepath.Join(root, "sandboxes")
	c.Providers.Sandbox.Local = &config.LocalSandbox{StateDir: boxes}
	earlier := filepath.Join(root, "sandboxes-before")
	d := &assistantDeep{boot: boot, host: DeepHost{Config: []string{filepath.Join(root, "crewlet.yaml")}},
		roots: []string{earlier}}
	deny, _ := d.confinement(c)
	home, _ := os.UserHomeDir()
	for _, want := range []string{filepath.Join(root, "crewlet.yaml"), filepath.Join(root, "data"),
		filepath.Join(root, "estate"), filepath.Join(root, "stream"), boot.Logging.File.Path,
		login, home, sandbox.DefaultBoxRoot(), boxes, earlier} {
		if !slices.Contains(deny, want) {
			t.Errorf("the confinement does not deny %s: %v", want, deny)
		}
	}
}

// directDeepCompany is deepCompany with its deep work on the engine host: a
// local catalogue with no image and no e2b, whose box root is stateDir ("" for
// the default).
func directDeepCompany(t *testing.T, stateDir string) *Company {
	t.Helper()
	c := deepCompany(t)
	c.Config.Providers.Sandbox = &config.SandboxProvider{Local: &config.LocalSandbox{StateDir: stateDir}}
	return c
}

// confinable is a host whose mechanism is known: nothing in these cases runs a
// command under it, so the mechanism is never asked to enforce anything.
func confinable() (*sandbox.HostConfinement, error) {
	return &sandbox.HostConfinement{Mechanism: sandbox.MechanismLandlock}, nil
}

// A DEEP-WORK BOX ON THE ENGINE HOST IS MADE UNDER THE COMPANY'S BOX ROOT, in
// a directory of its own there: providers.sandbox.local.state_dir where the
// company names one, the default box root where it does not, and never the
// store's directory, which is the volume holding the store and the stream. A
// seat's backend over the same root cannot reach it. Not parallel: the
// default case sets the default box root's variable. Mutations: put the box
// beside the store, or in the seats' own boxes directory, and a case fails.
func TestADeepWorkBoxIsMadeUnderTheCompanysBoxRoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stateDir func(root string) string
		boxRoot  func(root string) string
	}{
		{name: "state_dir",
			stateDir: func(root string) string { return filepath.Join(root, "sandboxes") },
			boxRoot:  func(root string) string { return filepath.Join(root, "sandboxes") }},
		{name: "the default",
			stateDir: func(string) string { return "" },
			boxRoot:  func(root string) string { return filepath.Join(root, "default-boxes") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(sandbox.BoxRootEnv, filepath.Join(root, "default-boxes"))
			rig := newDeepRig(t)
			rig.deep.probe = confinable
			rig.deep.boot = &config.Bootstrap{}
			rig.deep.boot.Store.Path = filepath.Join(root, "data", "node.db")
			rig.deep.e.epoch.current.Store(directDeepCompany(t, tc.stateDir(root)))

			target, err := rig.deep.target()
			if err != nil {
				t.Fatalf("target: %v", err)
			}
			if target.placement != sandbox.Direct {
				t.Fatalf("deep work is placed in %s, want the engine host", target.placement)
			}
			provider, err := target.manager.Provider(sandbox.Direct)
			if err != nil {
				t.Fatal(err)
			}
			box, err := provider.Create(t.Context(), sandbox.Spec{Placement: sandbox.Direct, TimeoutSec: 60})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			t.Cleanup(func() { _ = provider.Kill(context.WithoutCancel(t.Context()), box.ID()) })

			// THE LAYOUT IS DOCUMENTED (<box root>/assistant/boxes/<id>), so it is
			// spelled out rather than read off the constant.
			want := filepath.Join(tc.boxRoot(root), "assistant", "boxes", box.ID())
			if info, err := os.Stat(want); err != nil || !info.IsDir() {
				t.Fatalf("the deep-work box is not at %s (%v)", want, err)
			}
			if _, err := os.Stat(filepath.Join(root, "data")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a deep-work box wrote into the store's directory (%v)", err)
			}
			seats, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct,
				StateDir: tc.stateDir(root)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := seats.Attach(t.Context(), box.ID()); !errors.Is(err, sandbox.ErrBoxGone) {
				t.Fatalf("a seat's backend over the same box root reached the deep-work box: %v", err)
			}
		})
	}
}

// A BOX A PARKED RUN LEFT IS REACHED AFTER A REVISION MOVES THE BOX ROOT, and
// after one moves deep work off the engine host: kept and discarded through
// the confined cell under the root it was made in, never through the
// catalogue's own direct cell (a seat's, unconfined), and that root stays
// denied to every confined run. Mutations: ask only the cell under the root
// the company names now, or the catalogue for a direct box, and the keep is
// refused and the discard leaves the box; leave earlier roots out of the deny
// list, or stop the discard at the first cell that answers, and a check fails.
func TestADeepWorkBoxIsReachedAfterItsBoxRootMoves(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rig := newDeepRig(t)
	rig.deep.probe = confinable
	catalogue := rig.deep.e.Company().catalogue
	before, after := filepath.Join(root, "before"), filepath.Join(root, "after")
	rig.deep.e.epoch.current.Store(directDeepCompany(t, before))

	target, err := rig.deep.target()
	if err != nil {
		t.Fatalf("target: %v", err)
	}
	provider, err := target.manager.Provider(sandbox.Direct)
	if err != nil {
		t.Fatal(err)
	}
	made, err := provider.Create(t.Context(), sandbox.Spec{Placement: sandbox.Direct, TimeoutSec: 60})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = provider.Kill(context.WithoutCancel(t.Context()), made.ID()) })
	dir := filepath.Join(before, deepBoxDir, "boxes", made.ID())
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the box is not where it was made: %v", err)
	}

	// THE NEXT REVISION moves the box root and places deep work in e2b.
	moved := deepCompany(t)
	moved.Config.Providers.Sandbox.Local = &config.LocalSandbox{StateDir: after}
	moved.catalogue = catalogue
	rig.deep.e.epoch.current.Store(moved)
	kept := assistant.DeepBox{Placement: string(sandbox.Direct), Sandbox: made.ID(), Agent: "claude-code"}
	if err := rig.deep.Keep(t.Context(), kept); err != nil {
		t.Fatalf("a box made under the earlier box root was not kept: %v", err)
	}
	deny, _ := rig.deep.confinement(moved.Config)
	for _, want := range []string{before, after} {
		if !slices.Contains(deny, want) {
			t.Errorf("the confinement does not deny the box root %s: %v", want, deny)
		}
	}
	rig.deep.Discard(t.Context(), kept)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the discarded box is still at %s (%v)", dir, err)
	}
}

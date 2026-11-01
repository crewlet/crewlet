package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE ASSISTANT'S DEEP WORK, wired (ADR-0054): internal/assistant's DeepWork
// seam over the sandbox layer and the MCP bridge, which that package may not
// import.
//
// # Every moving part already existed
//
// A run is the deep_work entry's harness's own run in a box: the same runner, box,
// credentials and live reading a seat's executor gets (agentrun.go), with the turn's own surface on the bridge in place of a
// seat's. What it does NOT take is the sandbox coordinator: that machinery is
// a seat's (its lease, its inbox, its fence, a durable run row its node's
// successor resumes), and the assistant is no seat (ADR-0050). The turn that
// starts a run watches it to its end, and a node that dies with it ends the
// turn as interrupted, as any turn on that node; the box's own deadline reaps
// it.
//
// # Where it runs
//
// In the company's e2b or image-backed container cell when it configures one
// ([config.Company.DeepWorkPlacement]), through the catalogue's own backend.
// Otherwise on the engine host, in a box this file builds and CONFINES
// (internal/sandbox's confinement): its job is kept from the engine's data, its
// config, its keyring, every other box and the operator's home. A host that
// cannot confine one runs none, and says why.
//
// # Where its box lives on the engine host
//
// Under the company's local box root ([sandbox.BoxRoot]:
// providers.sandbox.local.state_dir, else the default), in a directory of its
// own ([deepBoxDir]) — the root a seat's direct boxes are made under, and so
// the volume an operator sized for boxes. It was the store's directory once,
// which put a run's checkout, its installs and its builds on the volume holding
// the store and the stream: one large run could fill it and stop the company.
// The directory of its own is what keeps the two kinds of box apart: a seat's
// backend reattaches, lists and reaps under <root>/boxes alone, so it never
// reaches a deep-work box (unconfined), and the confined cell never a seat's.
//
// The root is the company's, so a revision can move it. A box a parked run
// left under an earlier one is still reached through that root
// ([assistantDeep.hostManagers]) for its continuation, its keep and its
// discard, and every root this process has built the confined cell under
// stays denied to every confined run, so a move never hands one person's kept
// box, or a seat's box left there, to another person's run.

// DeepHost is what an engine host keeps from a confined deep-work run beyond
// what the engine knows of itself: the config files it was started from.
type DeepHost struct {
	Config []string
}

// AssistantDeepWork is the assistant's deep-work backend on this node.
func AssistantDeepWork(e *Engine, boot *config.Bootstrap, host DeepHost) assistant.DeepWork {
	return &assistantDeep{e: e, boot: boot, host: host, poll: deepPoll,
		probe: sync.OnceValues(sandbox.ProbeConfinement)}
}

type assistantDeep struct {
	e     *Engine
	boot  *config.Bootstrap
	host  DeepHost
	probe func() (*sandbox.HostConfinement, error)
	// poll is how often a run is read: [deepPoll].
	poll time.Duration

	mu sync.Mutex
	// roots are the company box roots ([sandbox.BoxRoot], absolute) this
	// process has built the confined host cell under, in the order first
	// built: the one the company names now, and every one an earlier
	// revision named. Held for the life of the process, as a seat's retired
	// backends are, and bounded by the roots a company has ever named.
	roots []string
	// cells are the confined host cells by company box root, built once the
	// host's mechanism is known, and all rebuilt when what they are kept from
	// changes (cellsKey).
	cells    map[string]*sandbox.Manager
	cellsKey string
}

// deepBoxDir is the directory of its own a deep-work run's confined box is
// made under, beneath the company's box root: never "boxes", the directory a
// seat's backend makes, lists and reaps its own boxes in under the same root.
const deepBoxDir = "assistant"

var _ assistant.DeepWork = (*assistantDeep)(nil)

// deepPoll is how often a run is read: twice a second's worth of a person
// watching, at the cost of one file read of the box.
const deepPoll = 2 * time.Second

// deepGrace is how long a box outlives its run's deadline: the collection of
// its result and its outbox.
const deepGrace = 2 * time.Minute

// deepKeepGrace is how long past a proposal's lifetime a kept box lives: the
// person's approval continues it at most a proposal's lifetime after it
// parked, and the box's own deadline reaps it when nobody does.
const deepKeepGrace = 5 * time.Minute

// assistantRunAs is the caller every deep-work bridge session decides its
// calls as: NOBODY. The run's surface carries the turn's own grant
// (internal/assistant), so the request's identity must reach none of it.
type assistantRunAs struct{}

var errAssistantBridge = errors.New("engine: a deep-work call is decided by the turn's own " +
	"grant, never by the request that carried it")

func (assistantRunAs) Acting(ctx context.Context) context.Context {
	return iam.WithUnresolved(ctx, errAssistantBridge)
}

func (assistantRunAs) Bound() bool { return true }

// target is where one run goes: its cell, the manager over it and its harness.
type deepTarget struct {
	key       string
	agent     *harness.Provider
	placement sandbox.Placement
	manager   *sandbox.Manager
	company   *Company
}

// Ready is why a run cannot start on this node now.
func (d *assistantDeep) Ready(context.Context) error {
	_, err := d.target()
	return err
}

// target resolves the run's entry, cell and manager off the company now.
func (d *assistantDeep) target() (deepTarget, error) {
	c := d.e.Company()
	if c == nil || c.Config == nil || c.Models == nil {
		return deepTarget{}, errors.New("this node runs no company yet")
	}
	key, _, err := c.Config.AssistantDeepWork()
	if err != nil {
		return deepTarget{}, err
	}
	provider, ok := c.Models.Provider(key)
	if !ok {
		return deepTarget{}, fmt.Errorf("assistant.deep_work.llm names %q, which this node "+
			"does not build", key)
	}
	agent, isHarness := provider.(*harness.Provider)
	if !isHarness || agent.Harness() != executor.ClaudeCodeName {
		return deepTarget{}, fmt.Errorf("providers.llm.%s is not a Claude Code entry this "+
			"node can run deep work on", key)
	}
	// THE TWO FIXES APART, as a seat's executor launch names them: an unset
	// base URL is one variable, and a node no box's call can reach is
	// another ([mcpbridge.Bridge.Reachable]). THE FLEET'S ADDRESS, never
	// this node's own listener a seat's direct box may dial instead: a
	// deep-work run is held to the address the variable gives, on the
	// engine host's loopback alone where that address is loopback
	// ([mcpbridge.Bridge.LoopbackBase]).
	switch {
	case d.e.bridge == nil || d.e.bridge.Reaches(false) == "":
		return deepTarget{}, fmt.Errorf("deep work reaches Crewlet over the tool bridge, and "+
			"this node has no address for it: set %s to the fleet's address, one its runs "+
			"can reach", mcpbridge.BaseURLVar)
	case !d.e.bridge.Reachable():
		return deepTarget{}, errors.New("deep work reaches Crewlet over the tool bridge, and " +
			"nothing reaches this node's: it serves the API with api.port 0, so it binds no " +
			"listener for the bridge's route; set api.port")
	}
	cell, _ := c.Config.DeepWorkPlacement()
	t := deepTarget{key: key, agent: agent, placement: sandbox.Placement(cell), company: c}
	if cell == config.PlacementDirect {
		cells, hostErr := d.hostManagers(c.Config)
		if hostErr != nil {
			return deepTarget{}, hostErr
		}
		t.manager = cells[0]
		return t, nil
	}
	// THE CATALOGUE OF THE EPOCH THE ENTRY WAS RESOLVED IN ([Company.catalogue]),
	// not the node's live manager: an apply landing between the two reads
	// would place the run by one revision's entry in another's catalogue.
	t.manager = c.catalogue
	if t.manager == nil {
		return deepTarget{}, fmt.Errorf("deep work runs in %s, and this node has no sandbox "+
			"runtime: check providers.sandbox", cell)
	}
	if _, err := t.manager.Provider(t.placement); err != nil {
		return deepTarget{}, err
	}
	return t, nil
}

// hostManagers are the confined engine-host cell's managers: the one under the
// box root the company names now first, which every new run is made in, then
// one under each root an earlier revision named, where a parked run's box may
// still be. Each is kept from the same paths: what the company names now, and
// every root in [assistantDeep.roots].
func (d *assistantDeep) hostManagers(c *config.Company) ([]*sandbox.Manager, error) {
	host, err := d.probe()
	if err != nil {
		return nil, err
	}
	// ABSOLUTE, as the backend makes its root ([sandbox.NewLocal]), so a
	// relative state_dir and the same one written out name one cell.
	now, err := filepath.Abs(boxRootOf(c))
	if err != nil {
		return nil, fmt.Errorf("providers.sandbox.local.state_dir %q is not a usable path: %w",
			boxRootOf(c), err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Contains(d.roots, now) {
		d.roots = append(d.roots, now)
	}
	deny, read := d.confinement(c)
	key := strings.Join(deny, "\x00") + "\x01" + strings.Join(read, "\x00")
	if d.cellsKey != key {
		d.cells, d.cellsKey = map[string]*sandbox.Manager{}, key
	}
	order := append([]string{now}, slices.DeleteFunc(slices.Clone(d.roots),
		func(root string) bool { return root == now })...)
	managers := make([]*sandbox.Manager, 0, len(order))
	for _, root := range order {
		manager := d.cells[root]
		if manager == nil {
			if manager, err = hostCell(root, deny, read, host); err != nil {
				return nil, err
			}
			d.cells[root] = manager
		}
		managers = append(managers, manager)
	}
	return managers, nil
}

// hostCell is the confined direct cell whose boxes live under the company box
// root root, in their own directory there ([deepBoxDir]).
func hostCell(root string, deny, read []string, host *sandbox.HostConfinement) (*sandbox.Manager, error) {
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct,
		StateDir: filepath.Join(root, deepBoxDir),
		Confine:  &sandbox.Confinement{Deny: deny, Read: read}, Host: host})
	if err != nil {
		return nil, err
	}
	return sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: local},
		Runners:   sandboxRunners(),
	})
}

// boxRootOf is the company's box root: where its seats' local boxes are made,
// and beneath which a deep-work run's confined box is ([sandbox.BoxRoot]).
func boxRootOf(c *config.Company) string {
	if c != nil && c.Providers.Sandbox != nil && c.Providers.Sandbox.Local != nil {
		return sandbox.BoxRoot(c.Providers.Sandbox.Local.StateDir)
	}
	return sandbox.BoxRoot("")
}

// boxManagers are the managers a box a run left in placement may be reached
// through, in the order to ask them.
//
// A BOX ON THE ENGINE HOST ([sandbox.Direct]) IS THE CONFINED CELL'S, whatever
// cell deep work is placed in now: the catalogue's own direct cell is a seat's,
// unconfined, and a deep-work box reattached through it would run its
// continuation with nothing kept from it. Any other box is the catalogue's.
func (d *assistantDeep) boxManagers(t deepTarget, placement sandbox.Placement) ([]*sandbox.Manager, error) {
	if placement == sandbox.Direct {
		return d.hostManagers(t.company.Config)
	}
	if t.company.catalogue == nil {
		return nil, fmt.Errorf("the box is in %s, and this node has no sandbox runtime: "+
			"check providers.sandbox", placement)
	}
	return []*sandbox.Manager{t.company.catalogue}, nil
}

// confinement is what a confined run is kept from and what it may read beneath
// it. Every path is the node's own: its store and estate, its stream store, its
// log file, its config files, every harness login a providers.llm entry keeps,
// every seat's box and every deep-work box (the company's box root now and
// every one this process built the confined cell under), and the operator's
// home; the harness's install is given back, read-only.
//
// Called with d.mu held, or on a value no other goroutine has.
func (d *assistantDeep) confinement(c *config.Company) (deny, read []string) {
	add := func(paths ...string) {
		for _, p := range paths {
			if strings.TrimSpace(p) != "" && !slices.Contains(deny, p) {
				deny = append(deny, p)
			}
		}
	}
	add(d.host.Config...)
	if b := d.boot; b != nil {
		if b.Store.Path != "" {
			add(filepath.Dir(b.Store.Path))
		}
		if b.Store.ReplicatedPath != "" {
			add(filepath.Dir(b.Store.ReplicatedPath))
		}
		add(b.Stream.StoreDir, b.Logging.File.Path)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home)
	}
	// EVERY BOX ROOT: the default, the company's now, and each one this
	// process built the confined cell under. A confined box lives beneath
	// one of them, and is given back its own home alone.
	add(sandbox.DefaultBoxRoot(), boxRootOf(c))
	add(d.roots...)
	if c != nil {
		// EVERY ENTRY'S LOGIN, the deep entry's own included: a box is
		// seeded with a copy of what it needs, and never reads the store.
		for key, entry := range c.Providers.LLM {
			if dir, err := entry.ResolvedStateDir(key); err == nil {
				add(dir)
			}
		}
	}
	add(os.Getenv(config.HarnessHomeEnv))
	slices.Sort(deny)
	// THE HARNESS ITSELF, wherever it is installed: a workstation keeps it in
	// the operator's home, which is denied above.
	if found, err := exec.LookPath("claude"); err == nil {
		read = append(read, filepath.Dir(found))
		if real, err := filepath.EvalSymlinks(found); err == nil {
			read = append(read, filepath.Dir(real))
		}
	}
	return deny, read
}

// Run launches one run, watches it and collects it.
func (d *assistantDeep) Run(ctx context.Context, r assistant.DeepRun) (assistant.DeepOutcome, error) {
	t, err := d.target()
	if err != nil {
		return assistant.DeepOutcome{}, err
	}
	run := newCodingRun(t.agent, nil)
	placement := t.placement
	if r.Continue != nil && r.Continue.Placement != "" {
		placement = sandbox.Placement(r.Continue.Placement)
	}
	if !placement.OnEngineHost() && !entrySignsIn(t.agent, run.env) {
		return assistant.DeepOutcome{}, fmt.Errorf("deep work runs in %s, and nothing signs "+
			"providers.llm.%s in there: mint a token that travels with `crewlet llm login %s "+
			"-capture-token`, or set its run_in to direct to run on the engine host", placement,
			t.key, t.key)
	}
	// A JOB OF ITS OWN PER RUN: the token names it beside the run, so a
	// continued run (a new launch after an approval) is a new job whose
	// token the earlier box's no longer reaches.
	endpoint := d.e.bridge.Open(ctx, &mcpbridge.Session{
		RunID: r.ID, Launch: uuid.NewString(), Handle: "assistant",
		Role: "Ask Crewlet deep work", Surface: r.Surface, Caller: assistantRunAs{},
		TTL: r.Timeout + deepGrace, Loopback: deepHeldToLoopback(placement, d.e.bridge),
	})
	if endpoint == "" {
		return assistant.DeepOutcome{}, fmt.Errorf("the tool bridge opened no session for "+
			"this run: set %s to a URL its runs can reach", mcpbridge.BaseURLVar)
	}
	defer d.e.bridge.Close(r.ID)

	maxTurns := r.Steps
	spec := t.manager.BuildSpec(sandbox.SpecInput{Placement: placement,
		CodingAgent: executor.ClaudeCodeName, Timeout: r.Timeout + deepGrace,
		MaxTurns: &maxTurns, Env: run.env, CredentialFiles: run.files})
	box, runner, session, err := d.acquire(ctx, t, spec, r)
	if err != nil {
		return assistant.DeepOutcome{}, err
	}
	out := assistant.DeepOutcome{Box: assistant.DeepBox{Placement: string(placement),
		Sandbox: box.ID(), Agent: executor.ClaudeCodeName, Session: session}}
	if placeErr := placeInputs(ctx, box, r.Inputs); placeErr != nil {
		d.discard(ctx, t, placement, box.ID())
		return assistant.DeepOutcome{}, placeErr
	}
	started := d.e.now()
	// THE CREDIT ON ITS COMMITS NAMES NO RUN: deep work has no page a
	// `Crewlet-Run:` could lead a reader of the history to, so a commit it
	// makes carries the co-author line alone.
	handle, err := runner.Start(ctx, box, sandbox.RunRequest{System: r.System, Brief: r.Brief,
		Env: run.env, Limits: sandbox.Limits{MaxTurns: r.Steps}, Model: run.model,
		Resume: session, MCPServers: bridgeServer(endpoint),
		Attribution: t.manager.CommitAttribution("")})
	if err != nil {
		d.discard(ctx, t, placement, box.ID())
		return assistant.DeepOutcome{}, fmt.Errorf("the deep-work run could not start: %w", err)
	}
	out.End = d.watch(ctx, box, runner, handle, r)
	collectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deepGrace)
	defer cancel()
	result, err := runner.Collect(collectCtx, box, handle)
	if err == nil {
		out.Report, out.InputTokens, out.OutputTokens = result.Text, result.InputTokens, result.OutputTokens
		if result.SessionID != "" {
			out.Box.Session = result.SessionID
		}
		if out.End == assistant.DeepEndFinished && !result.Success {
			out.End, out.Reason = assistant.DeepEndFailed, redact.Secrets(result.Error)
		}
	} else if out.End == assistant.DeepEndFinished {
		out.End, out.Reason = assistant.DeepEndFailed, "its result could not be read"
	}
	out.Files = collectOutbox(collectCtx, box)
	d.record(ctx, t, r, started, out)
	return out, nil
}

// acquire is the run's box: the parked run's own, reconnected, or a new one,
// and the harness session it continues.
func (d *assistantDeep) acquire(ctx context.Context, t deepTarget, spec sandbox.Spec,
	r assistant.DeepRun) (sandbox.Sandbox, sandbox.Runner, string, error) {
	if c := r.Continue; c != nil && c.Sandbox != "" {
		placement := sandbox.Placement(c.Placement)
		managers, err := d.boxManagers(t, placement)
		if err != nil {
			return nil, nil, "", fmt.Errorf("the box the run left could not be reached: %w", err)
		}
		var runner sandbox.Runner
		box, err := reach(managers, func(m *sandbox.Manager) (sandbox.Sandbox, error) {
			reconnected, held, reconnectErr := m.Reconnect(ctx, placement, c.Sandbox,
				executor.ClaudeCodeName)
			runner = held
			return reconnected, reconnectErr
		})
		if err == nil {
			if timeoutErr := box.SetTimeout(ctx, spec.TimeoutSec); timeoutErr != nil {
				return nil, nil, "", timeoutErr
			}
			return box, runner, c.Session, nil
		}
		return nil, nil, "", fmt.Errorf("the box the run left could not be reached: %w", err)
	}
	var setup []sandbox.SetupStep
	if !spec.Placement.OnEngineHost() || spec.Placement == sandbox.Container {
		setup = t.manager.DefaultSetup()
	}
	box, runner, err := t.manager.Acquire(ctx, spec, setup)
	if err != nil {
		return nil, nil, "", fmt.Errorf("the deep-work box could not be prepared: %w", err)
	}
	return box, runner, "", nil
}

// placeInputs writes the person's attachments where the run's brief names them,
// and makes the outbox the run delivers into.
func placeInputs(ctx context.Context, box sandbox.Sandbox, inputs []assistant.DeepFile) error {
	if _, err := box.Exec(ctx, "mkdir -p inputs outbox", sandbox.ExecOptions{}); err != nil {
		return fmt.Errorf("the run's folders could not be made: %w", err)
	}
	for _, in := range inputs {
		dst := path.Join(sandbox.WorkspaceSubdir, "inputs", path.Base(in.Name))
		if err := box.WriteFile(ctx, dst, in.Content); err != nil {
			return fmt.Errorf("the attachment %s could not be placed: %w", in.Name, err)
		}
	}
	return nil
}

// watch follows a run to its end, telling the turn what it says, and stops it
// at its token cap or when ctx ends.
func (d *assistantDeep) watch(ctx context.Context, box sandbox.Sandbox, runner sandbox.Runner,
	handle sandbox.RunHandle, r assistant.DeepRun) assistant.DeepEnd {
	reading := runner.Follow(handle)
	ticker := time.NewTicker(d.poll)
	defer ticker.Stop()
	for {
		if read, err := reading.Read(ctx, box); err == nil {
			if r.Progress != nil && (read.Text != "" || read.Spent > 0) {
				r.Progress(assistant.DeepProgress{Lines: lines(read.Text), Spent: read.Spent})
			}
			if r.Tokens > 0 && read.Spent > r.Tokens {
				stopJob(ctx, box)
				return assistant.DeepEndTokens
			}
		}
		if state, err := runner.Poll(ctx, box, handle); err == nil && state.Over() {
			return assistant.DeepEndFinished
		}
		select {
		case <-ctx.Done():
			stopJob(ctx, box)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return assistant.DeepEndTimeout
			}
			return assistant.DeepEndStopped
		case <-ticker.C:
		}
	}
}

// stopJob ends a run's job and keeps nothing running in its box: the box
// itself is the caller's to keep or discard.
//
// ON A CONTEXT OF ITS OWN, keeping the caller's values: the stop is often the
// caller's own cancellation, which a stop that inherited it would skip.
func stopJob(ctx context.Context, box sandbox.Sandbox) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deepGrace)
	defer cancel()
	if err := box.Pause(ctx); err != nil {
		log.Warn("assistant_deep_job_unstopped", "sandbox_id", box.ID(), "error", err.Error())
	}
}

// lines splits a reading's settled text into its lines.
func lines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// collectOutbox reads what the run left in its outbox: regular files only, at
// most one more than a run may deliver so the assistant can say some were
// left out, each within the size a file may be.
func collectOutbox(ctx context.Context, box sandbox.Sandbox) []assistant.DeepFile {
	res, err := box.Exec(ctx, "find outbox -maxdepth 1 -type f -print0", sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	var out []assistant.DeepFile
	for _, name := range strings.Split(res.Stdout, "\x00") {
		name = path.Base(strings.TrimSpace(name))
		if name == "" || name == "." || len(out) > wire.DeliveredFilesPerRun {
			continue
		}
		content, err := box.ReadFile(ctx, path.Join(sandbox.WorkspaceSubdir, "outbox", name))
		if err != nil || len(content) > wire.FileMaxBytes {
			out = append(out, assistant.DeepFile{Name: name})
			continue
		}
		out = append(out, assistant.DeepFile{Name: name, Content: content})
	}
	return out
}

// record files the run's spend on the spend history, as the person's on the
// operator stage. The turn's meter charges it.
func (d *assistantDeep) record(ctx context.Context, t deepTarget, r assistant.DeepRun,
	started time.Time, out assistant.DeepOutcome) {
	if d.e.auxSpend == nil {
		return
	}
	ended := d.e.now()
	seat := auxspend.Seat{Person: r.Seat}
	if t.company.Org != nil {
		if role := t.company.Org.SeatByHandle(r.Seat); role != nil {
			seat.PersonRole = role.Name
		}
	}
	d.e.auxSpend.Add(auxspend.Call{
		Seat: seat, Use: auxspend.Use{Stage: types.AuxStageOperator,
			Purpose: types.AuxAssistantDeepWork, TurnID: r.Turn.String()},
		Model: t.agent.Model(), ProviderKey: t.key,
		Day:     period.At(period.Day, ended, basisOf(t.company, nil).zone).Label,
		Started: started, Ended: ended, Failed: out.End != assistant.DeepEndFinished,
		Spent: auxspend.Spent{Calls: 1, Input: out.InputTokens, Output: out.OutputTokens},
		Trace: tracing.TraceOf(ctx),
	})
}

// Keep holds a parked run's box for its continuation: alive until a proposal's
// lifetime and a margin from now, when its own deadline reaps it if no
// approval has continued it.
func (d *assistantDeep) Keep(ctx context.Context, b assistant.DeepBox) error {
	t, err := d.target()
	if err != nil {
		return err
	}
	placement := sandbox.Placement(b.Placement)
	managers, err := d.boxManagers(t, placement)
	if err != nil {
		return err
	}
	box, err := reach(managers, func(m *sandbox.Manager) (sandbox.Sandbox, error) {
		provider, providerErr := m.Provider(placement)
		if providerErr != nil {
			return nil, providerErr
		}
		return provider.Attach(ctx, b.Sandbox)
	})
	if err != nil {
		return err
	}
	keep := t.company.Config.Assistant.ProposalTTL() + deepKeepGrace
	return box.SetTimeout(ctx, keep.Seconds())
}

// reach is fn's answer from the first manager that holds the box, asked in
// order: a cell the box is not under answers [sandbox.ErrBoxGone] and the next
// is asked, and any other answer is the box's. Gone from every one, it is gone.
func reach[T any](managers []*sandbox.Manager, fn func(*sandbox.Manager) (T, error)) (T, error) {
	var zero T
	err := sandbox.ErrBoxGone
	for _, m := range managers {
		var got T
		if got, err = fn(m); !errors.Is(err, sandbox.ErrBoxGone) {
			return got, err
		}
	}
	return zero, err
}

// Discard ends a box no run will continue.
func (d *assistantDeep) Discard(ctx context.Context, b assistant.DeepBox) {
	if b.Sandbox == "" {
		return
	}
	t, err := d.target()
	if err != nil {
		log.Warn("assistant_deep_box_undiscarded", "sandbox_id", b.Sandbox, "error", err.Error(),
			"detail", "its own deadline reaps it")
		return
	}
	d.discard(ctx, t, sandbox.Placement(b.Placement), b.Sandbox)
}

// discard kills a box through every manager it may be under ([boxManagers]).
//
// EVERY ONE, never the first that answers: a local backend's Kill of a box
// that is not under its root reclaims nothing and says nothing, so the first
// answer would be the cell the box is not in whenever a revision moved the
// company's box root after the box was made.
func (d *assistantDeep) discard(ctx context.Context, t deepTarget, placement sandbox.Placement, id string) {
	managers, err := d.boxManagers(t, placement)
	if err == nil {
		killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deepGrace)
		defer cancel()
		var errs []error
		for _, m := range managers {
			provider, providerErr := m.Provider(placement)
			if providerErr == nil {
				providerErr = provider.Kill(killCtx, id)
			}
			if providerErr != nil && !errors.Is(providerErr, sandbox.ErrBoxGone) {
				errs = append(errs, providerErr)
			}
		}
		err = errors.Join(errs...)
	}
	if err != nil {
		log.Warn("assistant_deep_box_undiscarded", "sandbox_id", id, "error", err.Error(),
			"detail", "its own deadline reaps it")
	}
}

// deepHeldToLoopback reports whether a deep-work run's bridge session admits
// calls from this host's loopback alone ([mcpbridge.Session.Loopback]).
//
// EVERY BOX ON THE ENGINE HOST IS HELD, a container's as much as a direct
// one's, whenever the bridge's base is loopback. A container box is handed
// the runtime's name for the engine host in place of that base
// ([sandbox.EngineAddress]), and the hold is what keeps that from widening
// anything: its calls are admitted only where they arrive on loopback, as a
// direct box's are, so a token it leaks is worth nothing from any other
// address, and nothing off this host reaches a session the hold covers. The
// cost is said in docs/guides/assistant.md: a runtime that delivers the
// box's calls from its bridge network instead (Docker Engine on Linux) is
// refused, and deep work there runs `direct` or remote.
func deepHeldToLoopback(placement sandbox.Placement, bridge *mcpbridge.Bridge) bool {
	return placement.OnEngineHost() && bridge.LoopbackBase()
}

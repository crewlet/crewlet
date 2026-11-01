// Package llmtest is a coding CLI for tests: a provider that, handed a request
// with tools, connects to the tool host's session exactly as a CLI's MCP client
// does and plays a scripted model against it — each response's prose, then the
// calls it makes, then the next response — until the script ends, a response
// calls nothing, or the host ends the run.
//
// THROUGH THE REAL SESSION, never around it. A double that called the host's
// dispatcher directly would test a path no CLI takes: the socket, the MCP
// server, the tool list, the withheld answers and the release are all on the
// way of every real call, and they are on the way of this one.
package llmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Response is one model response: its prose, then the calls it makes.
type Response struct {
	Text  string
	Calls []Call

	// Parallel issues the response's calls at once, as several CLIs do.
	Parallel bool

	// Usage is what the response cost; zero takes [DefaultUsage].
	Usage llm.Usage

	// Model names the model that answered; empty takes the script's.
	Model string

	// Streamed is the part of Text the CLI already streamed before the
	// response was played (a case that sends its own fragments while the
	// model writes): only the rest of Text is streamed with it.
	Streamed string

	// Trailing is prose the CLI streams for this response AFTER its record
	// has shown it, as a CLI whose record opens a request before its text
	// is written does — streamed with the request's id.
	Trailing string

	// Writing, when set, is called as the model begins this response —
	// before any of it is streamed, reported or called. A case blocks in it
	// to look at the host while the request is out, or sleeps in it to give
	// the model's half of the round a duration.
	Writing func()
}

// Call is one call a response makes.
type Call struct {
	// ID is the CLI's id for the call; empty mints one.
	ID   string
	Name string

	// Args are the arguments as the model wrote them — JSON, so a case can
	// write some that do not parse. Empty is `{}`.
	Args string

	// Handed is what the CLI hands the host's session for the arguments in
	// place of Args, where it does not hand Args: a CLI that turns what its
	// model wrote into something else (Claude Code sends an array's elements
	// as an object). The record still shows Args.
	Handed string

	// Native makes the call one of the CLI's OWN tools — its web search,
	// its fetch — which its record shows and which it runs itself: it never
	// reaches the host's session.
	Native bool

	// Sent is the name the CLI calls the host's session with, where it is
	// not Name: a CLI that rewrites a name its list does not hold to one it
	// does, and runs that with the model's arguments.
	Sent string

	// GiveUp is how long the CLI waits for this call before it gives up on
	// it — its own tool timeout — and cancels its request; its model is
	// told the call failed and the script goes on. Zero waits for the
	// answer.
	GiveUp time.Duration

	// Exit makes the CLI die once GiveUp has passed instead: it closes its
	// end of the session — EOF, to the host — and its run fails.
	Exit bool

	// Wait, when set, holds the call until it is closed (after
	// [Script.Before]), so a case can make one call of a Parallel response
	// reach the host only once another has begun — an arrival order the
	// scheduler would otherwise choose.
	Wait <-chan struct{}
}

// Run is what one Complete plays.
type Run struct {
	Responses []Response

	// Err fails the run: after the responses, or at once when there are
	// none.
	Err error

	// Stop is the stop reason the run ends with; empty is [llm.StopEnd].
	// [llm.StopCap] is a CLI whose own turn cap ended it.
	Stop llm.StopReason
}

// relist re-reads the tool list when the server says it changed, as a CLI that
// re-lists does — and as the host waits for before it answers the call that
// changed it.
func relist(ctx context.Context, req *mcp.ToolListChangedRequest) {
	_, _ = req.Session.ListTools(ctx, nil)
}

// CallIDKey is the `_meta` key the script sends a call's id under, as Codex
// does.
const CallIDKey = "callId"

func (s *Script) callIDMeta() string {
	switch {
	case s.NoCallIDs:
		return ""
	case s.CallIDMeta != "":
		return s.CallIDMeta
	}
	return CallIDKey
}

// KillLatency is how long the script takes to be stopped once the host ends
// its run: far longer than an answer takes to cross the session, as a real
// CLI's signal, grace and exit are.
var KillLatency = 100 * time.Millisecond

// DefaultUsage is what a response costs when its case does not say.
var DefaultUsage = llm.Usage{Input: 100, Output: 20}

// Script is the provider.
type Script struct {
	// Runs are played in order, one per Complete. Past the end, a run
	// answers "done" and calls nothing.
	Runs []Run

	// ModelName is the model every response names by default.
	ModelName string

	// Unobserved makes the CLI one whose record shows no requests: the host
	// is told nothing per response, and the whole run's cost at the end.
	Unobserved bool

	// NoRelist makes the CLI one that never re-lists its tools mid-run.
	NoRelist bool

	// ToolTimeout is the CLI's own per-call timeout, told to the host.
	ToolTimeout time.Duration

	// Concurrency is which calls the CLI says it makes side by side, told
	// to the host. What the script actually does is each response's
	// Parallel; a case sets both to say one thing.
	Concurrency llm.CallConcurrency

	// Before, when set, is called before each call with its name — a call
	// to one of the CLI's own tools included, which then takes as long as
	// Before does; a case uses it to act between a response and its calls.
	Before func(name string)

	// LateRecord makes the CLI one whose record shows a response — its prose,
	// its calls, its cost — only once the response's calls have run, as
	// Gemini CLI's and OpenCode's do, and says so to the host.
	LateRecord bool

	// NumberedRequests makes the CLI one whose requests are numbered from one
	// in every run — as a backend numbers the requests of a CLI whose record
	// carries no id — rather than given ids no other run shares.
	NumberedRequests bool

	// OnCall, when set, is told what the host tells a backend's
	// [llm.ServeOptions.OnCall]: when the first call the CLI is waiting on
	// starts and the last one ends, for the run of index run.
	OnCall func(run int, busy bool)

	// CallIDMeta is the `_meta` key the script tells the host it sends call
	// ids under; empty takes [CallIDKey], the key it always sends them
	// under. A case names another to play a CLI the host must not read.
	CallIDMeta string

	// NoCallIDs makes the CLI one that sends no id of its own with a call,
	// as most do: the host joins each call to the response that asked for it
	// by its name and arguments.
	NoCallIDs bool

	mu   sync.Mutex
	next int
	seen []llm.Request
	got  []Result
	late int
}

// Result is what one call was answered.
type Result struct {
	Name   string
	Output string
	Failed bool
	// Err is a call the session never answered — the run was ended under it.
	Err error
}

var _ llm.Provider = (*Script)(nil)

// Model is the configured identity.
func (s *Script) Model() string {
	if s.ModelName == "" {
		return "scripted"
	}
	return s.ModelName
}

// Seen is every request handed to the script, in order.
func (s *Script) Seen() []llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Request(nil), s.seen...)
}

// Late is how many model requests the script made after the host had decided
// the run must end — each one a request a real CLI would have been billed
// for, made after the phase was decided.
func (s *Script) Late() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.late
}

// Results is what every call was answered, in order.
func (s *Script) Results() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Result(nil), s.got...)
}

// Complete plays the next run.
func (s *Script) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	s.mu.Lock()
	s.seen = append(s.seen, req)
	run := Run{Responses: []Response{{Text: "done"}}}
	if s.next < len(s.Runs) {
		run = s.Runs[s.next]
	}
	n := s.next
	s.next++
	s.mu.Unlock()

	if req.Tools == nil {
		return s.textOnly(run)
	}
	return s.play(ctx, req, run, n)
}

// textOnly answers a call that offered no tools: the run's first response's
// prose, with nothing called.
func (s *Script) textOnly(run Run) (*llm.Completion, error) {
	if len(run.Responses) == 0 {
		return nil, orDefault(run.Err)
	}
	r := run.Responses[0]
	u := usageOf(r)
	return &llm.Completion{
		Model: s.modelOf(r), Content: r.Text, StopReason: llm.StopEnd,
		InputTokens: u.Input, OutputTokens: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
	}, run.Err
}

func orDefault(err error) error {
	if err != nil {
		return err
	}
	return errors.New("llmtest: the script failed the run")
}

func usageOf(r Response) llm.Usage {
	if r.Usage == (llm.Usage{}) {
		return DefaultUsage
	}
	return r.Usage
}

func (s *Script) modelOf(r Response) string {
	if r.Model != "" {
		return r.Model
	}
	return s.Model()
}

// play runs one scripted CLI run against the request's host.
func (s *Script) play(ctx context.Context, req llm.Request, run Run, n int) (*llm.Completion, error) {
	host := req.Tools
	served, err := host.Serve(ctx, llm.ServeOptions{
		Relists: !s.NoRelist, ToolTimeout: s.ToolTimeout, ObservesRequests: !s.Unobserved,
		Concurrency: s.Concurrency, CallIDMeta: s.callIDMeta(), RecordsCallsLate: s.LateRecord,
		OnCall: func(busy bool) {
			if s.OnCall != nil {
				s.OnCall(n, busy)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	// THE CLI IS STOPPED WHEN THE HOST SAYS SO — killed, and then released,
	// which is what a backend does once its CLI has exited and what lets a
	// call whose answer the host withheld return. Until it is killed it
	// behaves as a CLI does: it reads each answer and asks its model again,
	// whatever the host has decided — it is not told.
	var once sync.Once
	release := func() { once.Do(func() { served.Release() }) }
	defer release()
	var killed sync.Once
	dead := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		select {
		case <-host.Done():
			// A KILL TAKES TIME: a signal, a grace, a process exiting. An
			// answer the host did not withhold reaches the CLI first, and
			// the CLI asks its model again — which is what the host's
			// withholding exists to prevent, and what [Script.Late]
			// counts.
			time.Sleep(KillLatency)
			killed.Do(func() { close(dead) })
			release()
		case <-stopped:
		}
	}()
	defer close(stopped)

	if len(served.Args) < 2 {
		return nil, fmt.Errorf("llmtest: the host served %s %v, want the relay and a socket",
			served.Command, served.Args)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", served.Args[len(served.Args)-1])
	if err != nil {
		return nil, fmt.Errorf("llmtest: reach the session: %w", err)
	}
	var copts *mcp.ClientOptions
	if !s.NoRelist {
		copts = &mcp.ClientOptions{ToolListChangedHandler: relist}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "llmtest", Version: "1"}, copts)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		return nil, fmt.Errorf("llmtest: connect: %w", err)
	}
	defer func() { _ = cs.Close() }()
	if _, err := cs.ListTools(ctx, nil); err != nil {
		return nil, fmt.Errorf("llmtest: list the tools: %w", err)
	}

	var total llm.Usage
	var last Response
	stop := run.Stop
	for i, r := range run.Responses {
		select {
		case <-dead:
		default:
			if ended(host) {
				// A REQUEST AFTER THE DECISION: the host ended the run
				// and this CLI was not stopped before asking again.
				s.mu.Lock()
				s.late++
				s.mu.Unlock()
			}
		}
		if isDead(dead) {
			break
		}
		if r.Writing != nil {
			r.Writing()
		}
		last = r
		u := usageOf(r)
		total.Input += u.Input
		total.Output += u.Output
		total.CacheRead += u.CacheRead
		total.CacheWrite += u.CacheWrite
		req.Send(llm.Delta{Content: unstreamed(r)})
		calls := make([]llm.TurnCall, 0, len(r.Calls))
		for j, c := range r.Calls {
			if r.Calls[j].ID == "" {
				r.Calls[j].ID = fmt.Sprintf("call-%d-%d-%d", n, i, j)
			}
			args, argsErr := argsOf(c.Args)
			calls = append(calls, llm.TurnCall{
				ID: r.Calls[j].ID, Name: c.Name, Arguments: args, ArgumentsError: argsErr,
				Host: !c.Native,
			})
		}
		id := fmt.Sprintf("req-%d-%d", n, i)
		if s.NumberedRequests {
			id = fmt.Sprintf("request-%d", i+1)
		}
		observe := func() {
			if s.Unobserved {
				return
			}
			uu := u
			host.Observe(llm.Turn{
				ID: id, Text: r.Text, Calls: calls,
				Model: s.modelOf(r), Usage: &uu,
			})
			host.Spent(u)
		}
		late := s.LateRecord && len(r.Calls) > 0
		if !late {
			observe()
		}
		if r.Trailing != "" {
			req.Send(llm.Delta{Content: r.Trailing, Request: id})
		}
		if len(r.Calls) == 0 {
			break
		}
		exited := s.call(ctx, cs, conn, r)
		if late {
			observe()
		}
		if exited != nil {
			if s.Unobserved {
				host.Spent(total)
			}
			return nil, exited
		}
	}
	if s.Unobserved {
		host.Spent(total)
	}
	if run.Err != nil {
		return nil, run.Err
	}
	if stop == "" {
		stop = llm.StopEnd
	}
	return &llm.Completion{
		Model: s.modelOf(last), Content: last.Text, StopReason: stop,
		InputTokens: total.Input, OutputTokens: total.Output,
		CacheRead: total.CacheRead, CacheWrite: total.CacheWrite,
	}, nil
}

func isDead(dead <-chan struct{}) bool {
	select {
	case <-dead:
		return true
	default:
		return false
	}
}

func ended(host llm.ToolHost) bool {
	select {
	case <-host.Done():
		return true
	default:
		return false
	}
}

// call makes one response's calls, one after another or at once, and answers
// the error the run fails with when the CLI died during one ([Call.Exit]).
func (s *Script) call(ctx context.Context, cs *mcp.ClientSession, conn net.Conn, r Response) error {
	var exitMu sync.Mutex
	var exited error
	one := func(c Call) {
		if s.Before != nil {
			s.Before(c.Name)
		}
		if c.Wait != nil {
			select {
			case <-c.Wait:
			case <-ctx.Done():
			}
		}
		if c.Native {
			// The CLI's own tool: it runs it itself — for as long as
			// Before takes — and the host never hears of it beyond the
			// record.
			return
		}
		raw := c.Args
		if c.Handed != "" {
			raw = c.Handed
		}
		if raw == "" {
			raw = "{}"
		}
		name := c.Name
		if c.Sent != "" {
			name = c.Sent
		}
		params := &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(raw)}
		if !s.NoCallIDs {
			params.Meta = mcp.Meta{CallIDKey: c.ID}
		}
		res, err := s.callOne(ctx, cs, conn, c, params)
		if c.Exit && c.GiveUp > 0 {
			exitMu.Lock()
			exited = fmt.Errorf("llmtest: the CLI exited during %s: %w", name, err)
			exitMu.Unlock()
		}
		got := Result{Name: name, Err: err}
		if res != nil {
			got.Output, got.Failed = text(res), res.IsError
		}
		s.mu.Lock()
		s.got = append(s.got, got)
		s.mu.Unlock()
	}
	if !r.Parallel {
		for _, c := range r.Calls {
			one(c)
			if exited != nil {
				return exited
			}
		}
		return nil
	}
	var wg sync.WaitGroup
	for _, c := range r.Calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			one(c)
		}()
	}
	wg.Wait()
	return exited
}

// callOne makes one call, giving up on it — or dying during it — as the call
// says.
func (s *Script) callOne(ctx context.Context, cs *mcp.ClientSession, conn net.Conn, c Call,
	params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if c.GiveUp <= 0 {
		return cs.CallTool(ctx, params)
	}
	if !c.Exit {
		cctx, cancel := context.WithTimeout(ctx, c.GiveUp)
		defer cancel()
		return cs.CallTool(cctx, params)
	}
	// THE CLI DIES MID-CALL: its end of the session closes under the call,
	// which the host reads as EOF.
	type answer struct {
		res *mcp.CallToolResult
		err error
	}
	got := make(chan answer, 1)
	go func() {
		res, err := cs.CallTool(ctx, params)
		got <- answer{res, err}
	}()
	select {
	case a := <-got:
		return a.res, a.err
	case <-time.After(c.GiveUp):
	}
	_ = conn.Close()
	a := <-got
	if a.err == nil {
		a.err = errors.New("the session closed")
	}
	return nil, a.err
}

// argsOf decodes a call's arguments for the record a CLI reports, which
// carries them decoded; arguments that do not parse are reported as none.
func argsOf(raw string) (map[string]any, string) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, ""
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil || out == nil {
		return map[string]any{}, "they are not one JSON object"
	}
	return out, ""
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// unstreamed is the part of a response's prose not yet streamed: all of it, or
// what follows [Response.Streamed] when the text goes on from it.
func unstreamed(r Response) string {
	if rest, ok := strings.CutPrefix(r.Text, r.Streamed); ok {
		return rest
	}
	return r.Text
}

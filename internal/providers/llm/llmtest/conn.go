package llmtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Conn is one CLI run in progress against a host's session, driven response
// by response — the half of [Script] a test drives itself when what the model
// says next is not a fixed list of runs: a script chosen by the tools the run
// was offered, or a reply worked out in Go from what the earlier calls were
// answered.
//
// THE SAME CLI [Script] is: it lists the tools, reports each response to the
// host as the CLI's record shows it, spends what the response cost, and makes
// the response's calls over MCP. Two things it does as the measured CLIs do and
// [Script] does not:
//
//   - A name it was not offered is refused LOCALLY, without reaching the host —
//     every measured CLI but two answers an unlisted name itself, so the host
//     sees the model ask for it only in the record.
//   - It is stopped the moment the host ends its run, unless
//     [ConnOptions.KillLatency] says otherwise: no response is played after
//     that, and no further call of the response in flight is made.
type Conn struct {
	host   llm.ToolHost
	req    llm.Request
	opts   ConnOptions
	served llm.Served
	cs     *mcp.ClientSession
	sock   net.Conn
	seq    int64

	// dead is closed once the CLI has been stopped; stopped, once the run
	// is over and the watcher has nothing left to do.
	dead    chan struct{}
	stopped chan struct{}
	kill    sync.Once
	release sync.Once
	closed  sync.Once

	mu        sync.Mutex
	offered   []llm.ToolDef
	listed    map[string]bool
	played    []Response
	results   []Result
	total     llm.Usage
	late      int
	connected bool
}

// ConnOptions is the CLI a [Conn] is.
type ConnOptions struct {
	// ModelName is the model every response names when it names none;
	// empty is "scripted".
	ModelName string

	// Unobserved makes the CLI one whose record shows no requests: the host
	// is told nothing per response, and the whole run's cost at the end.
	Unobserved bool

	// NoRelist makes the CLI one that never re-lists its tools mid-run, so
	// a tool activated during the run is one it never offers its model.
	NoRelist bool

	// ToolTimeout is the CLI's own per-call timeout, told to the host.
	ToolTimeout time.Duration

	// KillLatency is how long the CLI takes to be stopped once the host has
	// ended its run. Zero stops it at once.
	KillLatency time.Duration

	// Before, when set, is called before each call with its name.
	Before func(name string)
}

// connSeq numbers every Conn of the process, so the ids a run mints are its
// own: a host keys the requests and joins the calls of EVERY run of a phase by
// them, and two runs that both called their first request "req-0" would read as
// one request reported twice.
var connSeq atomic.Int64

// Connect serves req's host to a new CLI run and connects to it as the CLI's
// MCP client does, listing the tools.
func Connect(ctx context.Context, req llm.Request, opts ConnOptions) (*Conn, error) {
	if req.Tools == nil {
		return nil, fmt.Errorf("llmtest: Connect needs a request that offers tools")
	}
	c := &Conn{
		host: req.Tools, req: req, opts: opts, seq: connSeq.Add(1),
		dead: make(chan struct{}), stopped: make(chan struct{}),
	}
	served, err := c.host.Serve(ctx, llm.ServeOptions{
		Relists: !opts.NoRelist, ToolTimeout: opts.ToolTimeout, ObservesRequests: !opts.Unobserved,
		CallIDMeta: CallIDKey,
	})
	if err != nil {
		return nil, err
	}
	c.served = served
	go c.watch()
	if len(served.Args) < 2 {
		c.Close()
		return nil, fmt.Errorf("llmtest: the host served %s %v, want the relay and a socket",
			served.Command, served.Args)
	}
	var dialer net.Dialer
	sock, err := dialer.DialContext(ctx, "unix", served.Args[len(served.Args)-1])
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("llmtest: reach the session: %w", err)
	}
	c.sock = sock
	var copts *mcp.ClientOptions
	if !opts.NoRelist {
		copts = &mcp.ClientOptions{ToolListChangedHandler: func(ctx context.Context, req *mcp.ToolListChangedRequest) {
			_ = c.listOn(ctx, req.Session)
		}}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "llmtest", Version: "1"}, copts)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: sock, Writer: sock}, nil)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("llmtest: connect: %w", err)
	}
	c.cs = cs
	if err := c.list(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// watch stops the CLI when the host ends its run — after KillLatency, as a
// signal, a grace and an exit take — and then releases the run, which is
// what a backend does once its CLI has exited and what lets a call whose
// answer the host withheld return.
func (c *Conn) watch() {
	select {
	case <-c.host.Done():
		if c.opts.KillLatency > 0 {
			select {
			case <-time.After(c.opts.KillLatency):
			case <-c.stopped:
			}
		}
		c.kill.Do(func() { close(c.dead) })
		c.releaseRun()
	case <-c.stopped:
	}
}

func (c *Conn) releaseRun() {
	c.release.Do(func() {
		connected := c.served.Release()
		c.mu.Lock()
		c.connected = connected
		c.mu.Unlock()
	})
}

// list reads the tool list, as the CLI offers it to its model.
func (c *Conn) list(ctx context.Context) error {
	return c.listOn(ctx, c.cs)
}

// listOn reads the tool list over cs — the connection's own session, handed
// to a list-changed notice by the SDK, so the notice reads no field the
// connection is still being built into.
func (c *Conn) listOn(ctx context.Context, cs *mcp.ClientSession) error {
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("llmtest: list the tools: %w", err)
	}
	offered := make([]llm.ToolDef, 0, len(res.Tools))
	listed := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		params, _ := tool.InputSchema.(map[string]any)
		offered = append(offered, llm.ToolDef{
			Name: tool.Name, Description: tool.Description, Parameters: params,
		})
		listed[tool.Name] = true
	}
	c.mu.Lock()
	c.offered, c.listed = offered, listed
	c.mu.Unlock()
	return nil
}

// Offered is the tool list as the CLI last read it — at connect, and again
// whenever a relisting CLI went looking for a name it did not have.
func (c *Conn) Offered() []llm.ToolDef {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.offered)
}

// Relist reads the tool list again and answers it, as a CLI that re-lists does
// when the server says the list changed — so a case can say what the model was
// offered at each request of a run, an activation's effect included.
func (c *Conn) Relist(ctx context.Context) ([]llm.ToolDef, error) {
	if err := c.list(ctx); err != nil {
		return nil, err
	}
	return c.Offered(), nil
}

// OfferedNames is [Conn.Offered]'s names, in its order.
func (c *Conn) OfferedNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.offered))
	for _, def := range c.offered {
		out = append(out, def.Name)
	}
	return out
}

// Stopped reports whether the CLI has been stopped: the host ended its run —
// at that very moment where KillLatency is zero, which is what makes a case
// that ends a run read the same on every schedule.
func (c *Conn) Stopped() bool {
	return isDead(c.dead) || (c.opts.KillLatency <= 0 && ended(c.host))
}

// Played is every response the run played, in order.
func (c *Conn) Played() []Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.played)
}

// Results is what every call of the run was answered, in the order made.
func (c *Conn) Results() []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.results)
}

// Late is how many responses were played after the host had ended the run —
// each one a request a real CLI would have been billed for, made after the
// phase was decided. Zero unless KillLatency gave the CLI time to make one.
func (c *Conn) Late() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.late
}

// Respond plays one model response: its prose streamed, the response reported
// to the host and its cost spent, then its calls made. It answers what each
// call was answered, in the response's order, and nothing once the CLI has
// been stopped.
func (c *Conn) Respond(ctx context.Context, r Response) []Result {
	if c.Stopped() {
		return nil
	}
	c.mu.Lock()
	if ended(c.host) {
		c.late++
	}
	i := len(c.played)
	c.played = append(c.played, r)
	u := usageOf(r)
	c.total.Input += u.Input
	c.total.Output += u.Output
	c.total.CacheRead += u.CacheRead
	c.total.CacheWrite += u.CacheWrite
	c.mu.Unlock()

	c.req.Send(llm.Delta{Content: unstreamed(r)})
	calls := make([]Call, len(r.Calls))
	seen := make([]llm.TurnCall, len(r.Calls))
	for j, call := range r.Calls {
		if call.ID == "" {
			call.ID = fmt.Sprintf("conn%d-call-%d-%d", c.seq, i, j)
		}
		calls[j] = call
		args, argsErr := argsOf(call.Args)
		seen[j] = llm.TurnCall{ID: call.ID, Name: call.Name, Arguments: args, ArgumentsError: argsErr, Host: true}
	}
	if !c.opts.Unobserved {
		uu := u
		c.host.Observe(llm.Turn{
			ID: fmt.Sprintf("conn%d-req-%d", c.seq, i), Text: r.Text, Calls: seen,
			Model: c.modelOf(r), Usage: &uu,
		})
		c.host.Spent(u)
	}
	if len(calls) == 0 {
		return nil
	}
	out := make([]Result, len(calls))
	if !r.Parallel {
		for j, call := range calls {
			if c.Stopped() {
				// A STOPPED CLI MAKES NO FURTHER CALL of the response it
				// was in the middle of.
				out = out[:j]
				break
			}
			out[j] = c.call(ctx, call)
		}
	} else {
		var wg sync.WaitGroup
		for j, call := range calls {
			wg.Go(func() { out[j] = c.call(ctx, call) })
		}
		wg.Wait()
	}
	c.mu.Lock()
	c.results = append(c.results, out...)
	c.mu.Unlock()
	return out
}

// call makes one call, or refuses it locally when the name was never listed.
func (c *Conn) call(ctx context.Context, call Call) Result {
	if c.opts.Before != nil {
		c.opts.Before(call.Name)
	}
	if !c.lists(ctx, call.Name) {
		// THE CLI'S OWN REFUSAL, in the words Claude Code uses: the host
		// never sees a name it did not offer.
		return Result{Name: call.Name, Output: "No such tool available: " + call.Name, Failed: true}
	}
	raw := call.Args
	if call.Handed != "" {
		raw = call.Handed
	}
	if raw == "" {
		raw = "{}"
	}
	res, err := c.cs.CallTool(ctx, &mcp.CallToolParams{
		Name: call.Name, Arguments: json.RawMessage(raw), Meta: mcp.Meta{CallIDKey: call.ID},
	})
	got := Result{Name: call.Name, Err: err}
	if res != nil {
		got.Output, got.Failed = text(res), res.IsError
	}
	return got
}

// lists reports whether the CLI offers name — re-reading the list first, on a
// CLI that re-lists, when it is not in the copy the CLI holds.
func (c *Conn) lists(ctx context.Context, name string) bool {
	c.mu.Lock()
	have := c.listed[name]
	c.mu.Unlock()
	if have || c.opts.NoRelist {
		return have
	}
	if err := c.list(ctx); err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listed[name]
}

func (c *Conn) modelOf(r Response) string {
	if r.Model != "" {
		return r.Model
	}
	if c.opts.ModelName != "" {
		return c.opts.ModelName
	}
	return "scripted"
}

// Close ends the run: a CLI whose record shows no requests reports what the
// whole run cost, the connection is closed and the run released. It answers
// whether the CLI ever reached the tools. Safe to call more than once.
func (c *Conn) Close() (connected bool) {
	c.closed.Do(func() {
		if c.opts.Unobserved {
			c.mu.Lock()
			total := c.total
			c.mu.Unlock()
			if total.Total() > 0 {
				c.host.Spent(total)
			}
		}
		close(c.stopped)
		if c.cs != nil {
			_ = c.cs.Close()
		}
		if c.sock != nil {
			_ = c.sock.Close()
		}
		c.releaseRun()
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Finish closes the run and answers its completion: the last response's prose,
// the run's whole cost, and stop as its reason — [llm.StopEnd] when empty.
func (c *Conn) Finish(stop llm.StopReason) *llm.Completion {
	c.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	var last Response
	if n := len(c.played); n > 0 {
		last = c.played[n-1]
	}
	if stop == "" {
		stop = llm.StopEnd
	}
	return &llm.Completion{
		Model: c.modelOf(last), Content: last.Text, StopReason: stop,
		InputTokens: c.total.Input, OutputTokens: c.total.Output,
		CacheRead: c.total.CacheRead, CacheWrite: c.total.CacheWrite,
	}
}

// Drive plays one whole CLI run over req's host: next is asked for each
// response in turn, handed the run so far, and the run ends when a response
// calls nothing — the model's answer — or the host ends it. An error from next
// fails the run, as a CLI that dies mid-run does, after everything it already
// did has reached the host.
func Drive(ctx context.Context, req llm.Request, opts ConnOptions,
	next func(c *Conn) (Response, error),
) (*llm.Completion, error) {
	c, err := Connect(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	// A next that panics — a case modelling a backend that falls over —
	// still leaves its run released, as a CLI that died is.
	defer c.Close()
	for !c.Stopped() {
		r, err := next(c)
		if err != nil {
			c.Close()
			return nil, err
		}
		c.Respond(ctx, r)
		if len(r.Calls) == 0 {
			break
		}
	}
	return c.Finish(""), nil
}

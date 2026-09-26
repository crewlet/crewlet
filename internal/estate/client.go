package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Asker is what a client needs from the queue.
type Asker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// Roster is every live node that holds data, as this node currently sees the
// fleet. The client never asks itself: a stateless node holds nothing.
type Roster func(ctx context.Context) ([]string, error)

// ClientOptions are a client's dependencies.
type ClientOptions struct {
	Queue  Asker
	Roster Roster

	// Self is this node, which asks — named on every request so a serving
	// node's log says who it answered.
	Self string

	// Now is the clock the suspect cooldown reads, injected for tests.
	Now func() time.Time
}

// Client asks data nodes for the estate a stateless node does not hold.
//
// ONE PER NODE, shared by every seat on it: the session floor is node-wide,
// which is conservative rather than wrong — a read may wait for another
// seat's write on this node as well as its own, and never for less than its
// own.
type Client struct {
	queue  Asker
	roster Roster
	self   string
	now    func() time.Time

	// readBudget and writeBudget are [readAttempt] and [writeAttempt],
	// held so a test can shorten them rather than wait out a dead node.
	readBudget, writeBudget time.Duration

	mu sync.Mutex
	// highWater is the furthest position this node has been told landed,
	// per stream — the floor every request carries.
	highWater map[string]statelog.Position
	// sticky is the node that answered last, asked first next time: its
	// applier is the one most likely to have this node's own writes.
	sticky string
	// suspect is when each node that went unanswered may be asked first
	// again.
	suspect map[string]time.Time
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Queue == nil || opts.Roster == nil {
		return nil, errors.New("estate: a client needs a queue and a roster")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		queue: opts.Queue, roster: opts.Roster, self: opts.Self, now: now,
		readBudget: readAttempt, writeBudget: writeAttempt,
		highWater: map[string]statelog.Position{}, suspect: map[string]time.Time{},
	}, nil
}

// readAttempt bounds one read's wait on one node.
//
// TEN SECONDS: a serving node may wait [statelog.ReadBudget] for the floor and
// again for the level a read asked for, and then runs the query — so a node
// that has not answered in several times that is one that is gone rather than
// slow, and the next one answers sooner than it would. It is a ceiling under
// the caller's own deadline, never over it.
const readAttempt = 10 * time.Second

// writeAttempt bounds one write's wait on one node, for a caller with no
// deadline of its own.
//
// SIXTY SECONDS: a single append resolves within [statelog.DefaultResolveBudget]
// (five), and the longest gesture a seat's tools make — a merge or a move —
// appends once per object it walks and waits between them. A caller with a
// deadline is held to that instead.
const writeAttempt = 60 * time.Second

// suspectFor is how long a node that went unanswered is asked last.
//
// THIRTY SECONDS, which is the order of a presence lease's life: a node that
// died leaves the roster when its lease lapses, and until then every request
// that asked it first would wait out a whole attempt. Asked last instead, it
// costs one attempt per client rather than one per request.
const suspectFor = 30 * time.Second

// Observe raises the session floor to a position this node has been told
// landed — every write's own, and anything a caller waits for.
//
// It IS the stateless node's read-your-writes wait: a data node applies the
// write itself, so there is nothing here to wait for, and what the next read
// needs is that whichever node answers it has applied this far.
func (c *Client) Observe(at statelog.Position) {
	if c == nil || at.Stream == "" || at.Seq == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.Packed() > c.highWater[at.Stream].Packed() {
		c.highWater[at.Stream] = at
	}
}

// Await is [Client.Observe] in the shape the tool seams wait in.
func (c *Client) Await(_ context.Context, at statelog.Position) error {
	c.Observe(at)
	return nil
}

// floors is the session, as a request carries it.
func (c *Client) floors(stream string) []statelog.Position {
	if stream == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if at, ok := c.highWater[stream]; ok {
		return []statelog.Position{at}
	}
	return nil
}

// forget drops floors a serving node reported no node will ever reach.
func (c *Client) forget(streams []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range streams {
		delete(c.highWater, s)
	}
}

// candidates is the order the data nodes are asked in: the node that
// answered last, then a rendezvous order that spreads stateless nodes across
// the members, with every suspect node last.
func (c *Client) candidates(ctx context.Context) ([]string, error) {
	nodes, err := c.roster(ctx)
	if err != nil {
		return nil, err
	}
	nodes = slices.DeleteFunc(slices.Clone(nodes), func(n string) bool { return n == c.self || n == "" })
	c.mu.Lock()
	sticky := c.sticky
	now := c.now()
	suspect := map[string]bool{}
	for n, until := range c.suspect {
		if now.Before(until) {
			suspect[n] = true
		} else {
			delete(c.suspect, n)
		}
	}
	c.mu.Unlock()
	slices.SortStableFunc(nodes, func(a, b string) int {
		// SUSPECT LAST, then STICKY FIRST, then rendezvous.
		if suspect[a] != suspect[b] {
			if suspect[a] {
				return 1
			}
			return -1
		}
		if (a == sticky) != (b == sticky) {
			if a == sticky {
				return -1
			}
			return 1
		}
		wa, wb := weight(c.self, a), weight(c.self, b)
		switch {
		case wa > wb:
			return -1
		case wa < wb:
			return 1
		}
		return 0
	})
	return nodes, nil
}

// weight is a rendezvous score for one asker and one serving node.
func weight(asker, node string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(asker))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(node))
	return h.Sum64()
}

// call runs one operation on the first data node that will.
func call[A, R any](ctx context.Context, c *Client, o op[A, R], actor *Actor, args A) (R, error) {
	var zero R
	spec := o.spec
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return zero, fmt.Errorf("estate: %s: encode the arguments: %w", spec.name, err)
	}
	nodes, err := c.candidates(ctx)
	if err != nil {
		return zero, fmt.Errorf("estate: %s: read the fleet's data nodes: %w", spec.name, err)
	}
	if len(nodes) == 0 {
		return zero, fmt.Errorf("%w: no live node holds data, so %s has nothing to "+
			"ask — give a node the `data` role", ErrNoDataNode, spec.name)
	}
	// A READ IS CAPPED under the caller's deadline; a WRITE takes the
	// caller's deadline where there is one, because a gesture the caller
	// gave five minutes must not be abandoned after one.
	budget := c.readBudget
	if spec.class != opRead {
		budget = c.writeBudget
		if _, bounded := ctx.Deadline(); bounded {
			budget = 0
		}
	}
	var reasons []string
	for _, node := range nodes {
		attempt, cancel := attemptContext(ctx, budget)
		req := request{
			Op: spec.name, Args: encodedArgs, Actor: actor,
			Floors: c.floors(spec.stream), Deadline: deadlineOf(attempt), From: c.self,
		}
		raw, err := json.Marshal(req)
		if err != nil {
			cancel()
			return zero, fmt.Errorf("estate: %s: encode the request: %w", spec.name, err)
		}
		replies, err := c.queue.Ask(attempt, Subject(node), raw, 1)
		cancel()
		if err != nil {
			// THE ASK COULD NOT BE MADE AT ALL — a queue that is not
			// live, a request over the size limit. No node would fare
			// better.
			return zero, fmt.Errorf("estate: %s: %w", spec.name, err)
		}
		if len(replies) == 0 {
			c.markSuspect(node)
			if spec.class == opOnceWrite {
				return zero, fmt.Errorf("%w (%s, asked of %s)", ErrOutcomeUnknown, spec.name, node)
			}
			if ctx.Err() != nil {
				return zero, fmt.Errorf("estate: %s: %w", spec.name, ctx.Err())
			}
			reasons = append(reasons, node+": no answer")
			continue
		}
		var rep reply
		if err := json.Unmarshal(replies[0], &rep); err != nil {
			return zero, fmt.Errorf("estate: %s: %s answered something this build "+
				"cannot decode: %w", spec.name, node, err)
		}
		c.forget(rep.Obsolete)
		if rep.Unserved != "" {
			reasons = append(reasons, fmt.Sprintf("%s: %s", node, rep.Detail))
			continue
		}
		c.markAnswered(node)
		if rep.Err != nil {
			return zero, decodeError(rep.Err)
		}
		var out R
		if len(rep.Result) > 0 {
			if err := json.Unmarshal(rep.Result, &out); err != nil {
				return zero, fmt.Errorf("estate: %s: %s answered with a result this "+
					"build cannot decode: %w", spec.name, node, err)
			}
		}
		return out, nil
	}
	return zero, fmt.Errorf("%w for %s: %v", ErrNoDataNode, spec.name, reasons)
}

// attemptContext is one attempt's context: the caller's, capped by budget.
//
// A ZERO budget is the caller's own deadline, unchanged.
func attemptContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); budget == 0 || (ok && time.Until(deadline) < budget) {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}

func (c *Client) markSuspect(node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suspect[node] = c.now().Add(suspectFor)
	if c.sticky == node {
		c.sticky = ""
	}
}

func (c *Client) markAnswered(node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sticky = node
	delete(c.suspect, node)
}

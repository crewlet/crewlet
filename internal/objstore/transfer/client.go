package transfer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// Asker is what a client needs from the queue.
type Asker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

var (
	// ErrNoMap is a fleet with no placement map yet: no data node holds
	// objects, so there is nowhere to put one.
	ErrNoMap = errors.New("objstore/transfer: no placement map — no data node holds objects yet")

	// ErrUnderReplicated is a write fewer members stored than a quorum.
	ErrUnderReplicated = errors.New("objstore/transfer: too few members stored the chunk")

	// ErrNotFound is a chunk no member could supply.
	ErrNotFound = errors.New("objstore/transfer: no member holds the chunk")

	// ErrNoAnswer is a member that did not answer in time.
	ErrNoAnswer = errors.New("objstore/transfer: no answer")
)

// ClientOptions are a client's dependencies.
type ClientOptions struct {
	Queue Asker

	// Self is this node. A write or read that lands here goes to Local
	// directly rather than through the broker.
	Self string

	// Local is this node's own chunk store, nil on a node that holds none.
	Local Chunks

	// Maps is the placement map this node places by.
	Maps Maps

	// Refresh re-reads the map. When set, a request that finds no map
	// re-reads it once before refusing [ErrNoMap], so the first file a
	// node stores after its fleet's first map was written does not wait
	// out a refresh interval for it. Nil is a client whose map is fixed.
	Refresh func(context.Context) error

	// Now is the clock the suspect cooldown reads, injected for tests.
	Now func() time.Time
}

// Client writes and reads chunks across the fleet. ONE PER NODE: the suspect
// list is what spares every later request a dead member's timeout, so a
// client per request would learn nothing.
type Client struct {
	queue   Asker
	self    string
	local   Chunks
	maps    Maps
	refresh func(context.Context) error
	now     func() time.Time

	// attempt is [attemptBudget], held so a test can shorten it rather
	// than wait out a dead member.
	attempt time.Duration

	mu      sync.Mutex
	suspect map[string]time.Time
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Queue == nil || opts.Maps == nil {
		return nil, errors.New("objstore/transfer: a client needs a queue and a map")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		queue: opts.Queue, self: opts.Self, local: opts.Local, maps: opts.Maps,
		refresh: opts.Refresh, now: now,
		attempt: attemptBudget, suspect: map[string]time.Time{},
	}, nil
}

// attemptBudget bounds one request to one member.
//
// TEN SECONDS: the slowest thing a member does for a request is write and sync
// one mebibyte, which is milliseconds on a healthy disk and a second or two on
// a struggling one — so a member that has not answered in several times that
// is gone rather than slow, and the next member answers sooner than it would.
// It is a ceiling under the caller's own deadline, never over it.
const attemptBudget = 10 * time.Second

// suspectFor is how long a member that did not answer is asked last.
//
// THIRTY SECONDS, the order of a presence lease's life, for the reason the
// estate client's cooldown is: a member that died is taken out of the map
// only after a grace, and until then every chunk of every file placed on it
// would wait out a whole attempt first.
const suspectFor = 30 * time.Second

// placing is the map a request places by, re-read once where none is held.
//
// ONLY ON A MISS: a held map is placed by as it is, however old, because an
// older map is correct until a newer one is read (see [CacheInterval]). A
// missing one is different — every request refuses until it arrives — and a
// fleet's first map is written moments after its nodes boot, so a node that
// read the store just before it would otherwise refuse every upload until its
// next refresh.
func (c *Client) placing(ctx context.Context) (placement.Map, error) {
	m, ok := c.maps()
	if (!ok || len(m.Members) == 0) && c.refresh != nil {
		if err := c.refresh(ctx); err != nil {
			return placement.Map{}, fmt.Errorf("%w (and re-reading it failed: %w)", ErrNoMap, err)
		}
		m, ok = c.maps()
	}
	if !ok || len(m.Members) == 0 {
		return placement.Map{}, ErrNoMap
	}
	return m, nil
}

// Put stores a chunk on its members and answers how many hold it.
//
// Its UP SET is asked in parallel; a member that refuses or does not answer is
// replaced by the next one in the ranking, until the map's replica count is
// met or the ranking runs out. Fewer than a quorum is [ErrUnderReplicated].
func (c *Client) Put(ctx context.Context, h objstore.Hash, data []byte) (int, error) {
	if !h.Valid() {
		return 0, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	if objstore.HashOf(data) != h {
		// THE CALLER'S BUG, caught before it reaches a peer that would
		// only refuse it.
		return 0, fmt.Errorf("objstore/transfer: the bytes offered as %s do not hash to it", h)
	}
	m, err := c.placing(ctx)
	if err != nil {
		return 0, err
	}
	ranked := c.order(m.Ranked(h.PG()))
	need, quorum := m.Size(), m.Quorum()

	type result struct {
		node string
		err  error
	}
	results := make(chan result)
	next, inflight, stored := 0, 0, 0
	launch := func() {
		node := ranked[next]
		next++
		inflight++
		go func() { results <- result{node: node, err: c.putOne(ctx, node, h, data)} }()
	}
	for inflight < need && next < len(ranked) {
		launch()
	}
	var failed []string
	for inflight > 0 {
		r := <-results
		inflight--
		if r.err == nil {
			stored++
			continue
		}
		failed = append(failed, r.node+": "+r.err.Error())
		if stored+inflight < need && next < len(ranked) && ctx.Err() == nil {
			launch()
		}
	}
	if stored >= quorum {
		return stored, nil
	}
	if err := ctx.Err(); err != nil {
		return stored, fmt.Errorf("objstore/transfer: store %s: %w", h, err)
	}
	return stored, fmt.Errorf("%w: %s is on %d of the %d a write needs (%s)",
		ErrUnderReplicated, h, stored, quorum, strings.Join(failed, "; "))
}

// putOne stores a chunk on one member.
func (c *Client) putOne(ctx context.Context, node string, h objstore.Hash, data []byte) error {
	if node == c.self && c.local != nil {
		return c.local.Put(h, data)
	}
	rep, _, err := c.ask(ctx, node, request{Op: opPut, Hash: h}, data)
	if err != nil {
		return err
	}
	if rep.Status != statusOK {
		return fmt.Errorf("%s", rep.Detail)
	}
	return nil
}

// Get reads a chunk: from this node's own disk when it holds it, else from the
// first member down the ranking that does.
func (c *Client) Get(ctx context.Context, h objstore.Hash) ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	if c.local != nil {
		if data, err := c.local.Get(h); err == nil {
			return data, nil
		}
	}
	return c.Fetch(ctx, h)
}

// Fetch reads a chunk from a PEER, never this node's own disk — the repair's
// read, which is looking for a copy precisely because this node has none.
func (c *Client) Fetch(ctx context.Context, h objstore.Hash) ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	m, err := c.placing(ctx)
	if err != nil {
		return nil, err
	}
	var reasons []string
	for _, node := range c.order(m.Ranked(h.PG())) {
		if node == c.self {
			continue
		}
		rep, body, err := c.ask(ctx, node, request{Op: opGet, Hash: h}, nil)
		switch {
		case err != nil:
			reasons = append(reasons, node+": "+err.Error())
		case rep.Status != statusOK:
			reasons = append(reasons, node+": "+rep.Detail)
		case objstore.HashOf(body) != h:
			// A COPY THAT ARRIVED WRONG is no copy: the member checked it
			// before sending, so this is the transport's, and the next
			// member's is as good.
			reasons = append(reasons, node+": sent bytes that do not match the hash")
		default:
			return body, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("objstore/transfer: read %s: %w", h, err)
		}
	}
	return nil, fmt.Errorf("%w: %s (%s)", ErrNotFound, h, strings.Join(reasons, "; "))
}

// Holding is one member's answer about a batch of chunks.
type Holding struct {
	Node string

	// Held and Placed are per hash, in the order asked: whether the member
	// holds each chunk, and whether its own map places the chunk there.
	Held, Placed []bool

	// Epoch is the map epoch the member judged Placed at.
	Epoch uint64
}

// Has asks one member which of up to [MaxHas] chunks it holds.
func (c *Client) Has(ctx context.Context, node string, hashes []objstore.Hash) (Holding, error) {
	if len(hashes) > MaxHas {
		return Holding{}, fmt.Errorf("objstore/transfer: %d hashes in one request, over %d",
			len(hashes), MaxHas)
	}
	rep, _, err := c.ask(ctx, node, request{Op: opHas, Hashes: hashes}, nil)
	if err != nil {
		return Holding{}, err
	}
	if rep.Status != statusOK {
		return Holding{}, fmt.Errorf("objstore/transfer: %s: %s", node, rep.Detail)
	}
	if len(rep.Held) != len(hashes) || len(rep.Placed) != len(hashes) {
		return Holding{}, fmt.Errorf("objstore/transfer: %s answered %d of %d hashes",
			node, len(rep.Held), len(hashes))
	}
	return Holding{Node: node, Held: rep.Held, Placed: rep.Placed, Epoch: rep.Epoch}, nil
}

// ask sends one request to one member and reads its reply.
func (c *Client) ask(ctx context.Context, node string, req request, body []byte) (reply, []byte, error) {
	req.From = c.self
	raw, err := frame(req, body)
	if err != nil {
		return reply{}, nil, err
	}
	attempt, cancel := attemptContext(ctx, c.attempt)
	defer cancel()
	replies, err := c.queue.Ask(attempt, Subject(node), raw, 1)
	if err != nil {
		return reply{}, nil, fmt.Errorf("objstore/transfer: ask %s: %w", node, err)
	}
	if len(replies) == 0 {
		if ctx.Err() == nil {
			c.markSuspect(node)
		}
		return reply{}, nil, ErrNoAnswer
	}
	c.markAnswered(node)
	var rep reply
	payload, err := unframe(replies[0], &rep)
	if err != nil {
		return reply{}, nil, fmt.Errorf("objstore/transfer: %s answered: %w", node, err)
	}
	return rep, payload, nil
}

// order is a ranking with every suspect member moved to the end, the rest in
// the ranking's own order.
func (c *Client) order(ranked []string) []string {
	c.mu.Lock()
	now := c.now()
	suspect := map[string]bool{}
	for node, until := range c.suspect {
		if now.Before(until) {
			suspect[node] = true
		} else {
			delete(c.suspect, node)
		}
	}
	c.mu.Unlock()
	out := slices.Clone(ranked)
	slices.SortStableFunc(out, func(a, b string) int {
		switch {
		case suspect[a] == suspect[b]:
			return 0
		case suspect[a]:
			return 1
		}
		return -1
	})
	return out
}

func (c *Client) markSuspect(node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suspect[node] = c.now().Add(suspectFor)
}

func (c *Client) markAnswered(node string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.suspect, node)
}

// attemptContext is one attempt's context: the caller's, capped by budget.
func attemptContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < budget {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}

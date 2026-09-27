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
	"github.com/crewlet/crewlet/internal/objstore/disk"
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

	// ErrNotFound is a chunk every member of the map ANSWERED it does not
	// hold: a definite absence, and the only failure that may be counted
	// as a chunk the fleet has lost.
	ErrNotFound = errors.New("objstore/transfer: no member holds the chunk")

	// ErrUnreachable is a chunk no member supplied where at least one
	// could not say whether it holds it — it did not answer, refused,
	// failed to read its copy or sent bytes that did not match. The chunk
	// may be intact on that member, so this is never counted as lost.
	ErrUnreachable = errors.New("objstore/transfer: no member supplied the chunk, " +
		"and some could not be asked")

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

	// Layouts is the layout of the placement map this node places by.
	Layouts Layouts

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
	layouts Layouts
	refresh func(context.Context) error
	now     func() time.Time

	mu      sync.Mutex
	suspect map[string]time.Time
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Queue == nil || opts.Layouts == nil {
		return nil, errors.New("objstore/transfer: a client needs a queue and a map")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		queue: opts.Queue, self: opts.Self, local: opts.Local, layouts: opts.Layouts,
		refresh: opts.Refresh, now: now,
		suspect: map[string]time.Time{},
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

// placing is the layout a request places by, the map re-read once where none
// is held.
//
// ONLY ON A MISS: a held map is placed by as it is, however old, because an
// older map is correct until a newer one is read (see [CacheInterval]). A
// missing one is different — every request refuses until it arrives — and a
// fleet's first map is written moments after its nodes boot, so a node that
// read the store just before it would otherwise refuse every upload until its
// next refresh.
func (c *Client) placing(ctx context.Context) (*placement.Layout, error) {
	l, ok := c.layouts()
	if (!ok || len(l.Map().Members) == 0) && c.refresh != nil {
		if err := c.refresh(ctx); err != nil {
			return nil, fmt.Errorf("%w (and re-reading it failed: %w)", ErrNoMap, err)
		}
		l, ok = c.layouts()
	}
	if !ok || len(l.Map().Members) == 0 {
		return nil, ErrNoMap
	}
	return l, nil
}

// Put stores a chunk on its members and answers how many hold it.
//
// Its UP SET is asked in parallel; a member that refuses or does not answer is
// replaced by the next one in the ranking, until the map's replica count is
// met or the ranking runs out. A member that is OUT is never written: it is
// being emptied, and a copy put there is one more the fleet has to move off
// it. Fewer than a quorum is [ErrUnderReplicated].
func (c *Client) Put(ctx context.Context, h objstore.Hash, data []byte) (int, error) {
	if !h.Valid() {
		return 0, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	if objstore.HashOf(data) != h {
		// THE CALLER'S BUG, caught before it reaches a peer that would
		// only refuse it.
		return 0, fmt.Errorf("objstore/transfer: the bytes offered as %s do not hash to it", h)
	}
	l, err := c.placing(ctx)
	if err != nil {
		return 0, err
	}
	m := l.Map()
	order := c.holders(l, l.Group(h.Slot()), true)
	need, quorum := m.Size(), m.Quorum()

	type result struct {
		node string
		err  error
	}
	results := make(chan result)
	next, inflight, stored := 0, 0, 0
	launch := func() bool {
		node, ok := order.at(next)
		if !ok {
			return false
		}
		next++
		inflight++
		go func() { results <- result{node: node, err: c.putOne(ctx, node, h, data)} }()
		return true
	}
	for inflight < need {
		if !launch() {
			break
		}
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
		if stored+inflight < need && ctx.Err() == nil {
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
	switch {
	case rep.Status == statusOK:
		return nil
	case rep.Unhealthy != "":
		return fmt.Errorf("refused, its object store is %s: %s", rep.Unhealthy, rep.Detail)
	}
	return fmt.Errorf("refused: %s", rep.Detail)
}

// Get reads a chunk: from this node's own disk when it holds it, else from the
// first member down the ranking that does.
func (c *Client) Get(ctx context.Context, h objstore.Hash) ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	var unread error
	if c.local != nil {
		data, err := c.local.Get(h)
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, disk.ErrNotFound) {
			unread = err
		}
	}
	data, err := c.Fetch(ctx, h)
	if unread != nil && errors.Is(err, ErrNotFound) {
		// THIS NODE COULD NOT SAY: its own copy may be the one the others
		// are missing, so the absence is not definite.
		return nil, fmt.Errorf("%w: %s — this node could not read its own copy (%v), "+
			"and every other member answered that it holds none", ErrUnreachable, h, unread)
	}
	return data, err
}

// Fetch reads a chunk from a PEER, never this node's own disk — the repair's
// read, which is looking for a copy precisely because this node has none.
//
// It walks the whole ranking before it gives up, out members included, since
// they may still hold what they are being emptied of — so the failure it
// answers says which of two things is true: [ErrNotFound] when every member
// answered that it does not hold the chunk, [ErrUnreachable] when any could
// not say.
func (c *Client) Fetch(ctx context.Context, h objstore.Hash) ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: %q", objstore.ErrBadHash, h)
	}
	l, err := c.placing(ctx)
	if err != nil {
		return nil, err
	}
	order := c.holders(l, l.Group(h.Slot()), false)
	var reasons []string
	definite := true
	for i := 0; ; i++ {
		node, ok := order.at(i)
		if !ok {
			break
		}
		if node == c.self {
			continue
		}
		rep, body, err := c.ask(ctx, node, request{Op: opGet, Hash: h}, nil)
		switch {
		case err != nil:
			reasons = append(reasons, node+": "+err.Error())
			definite = false
		case rep.Status == statusMissing:
			reasons = append(reasons, node+": not held")
		case rep.Status != statusOK:
			reasons = append(reasons, node+": "+rep.Detail)
			definite = false
		case objstore.HashOf(body) != h:
			// A COPY THAT ARRIVED WRONG is no copy: the member checked it
			// before sending, so this is the transport's, and the next
			// member's is as good.
			reasons = append(reasons, node+": sent bytes that do not match the hash")
			definite = false
		default:
			return body, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("objstore/transfer: read %s: %w", h, err)
		}
	}
	if definite {
		return nil, fmt.Errorf("%w: %s (%s)", ErrNotFound, h, strings.Join(reasons, "; "))
	}
	return nil, fmt.Errorf("%w: %s (%s)", ErrUnreachable, h, strings.Join(reasons, "; "))
}

// holders is one group's members in the order a request tries them: the up
// set from the layout, and the rest of the map's ranking only once the up set
// has run out — so the request that finds every holder answering never pays
// for ranking every member.
type holders struct {
	c     *Client
	m     placement.Map
	pg    int
	write bool

	list  []string
	whole bool // list holds the whole ranking
}

// holders orders a group's members for a request. An up set with a member
// that did not answer lately is ranked whole at once, so that member goes
// after every other rather than being tried first and waited out.
func (c *Client) holders(l *placement.Layout, pg int, write bool) *holders {
	h := &holders{c: c, m: l.Map(), pg: pg, write: write}
	up := l.Up(pg)
	if c.anySuspect(up) {
		h.extend()
	} else {
		h.list = slices.Clone(up)
	}
	return h
}

// at is the i-th member to try, and false past the last.
func (h *holders) at(i int) (string, bool) {
	if i >= len(h.list) && !h.whole {
		h.extend()
	}
	if i >= len(h.list) {
		return "", false
	}
	return h.list[i], true
}

// extend appends every member of the ranking not yet listed, members that did
// not answer lately last — and for a write, no member the map does not place
// on ([placement.Member.Placeable]): one that is out, or on probation. What is
// already listed keeps its place: a request has already tried it.
func (h *holders) extend() {
	h.whole = true
	var rest []string
	for _, node := range h.m.Ranked(h.pg) {
		if slices.Contains(h.list, node) {
			continue
		}
		if member, _ := h.m.Member(node); h.write && !member.Placeable() {
			continue
		}
		rest = append(rest, node)
	}
	h.list = append(h.list, h.c.order(rest)...)
}

// Holding is one member's answer about a batch of chunks.
type Holding struct {
	Node string

	// Held and Placed are per hash, in the order asked: whether the member
	// holds each chunk — an intact copy, read and checked, when the
	// question asked for that — and whether its own map places the chunk
	// there.
	Held, Placed []bool

	// Epoch is the map epoch the member judged Placed at.
	Epoch uint64
}

// Has asks one member which of a batch of chunks it holds: up to [MaxHas] when
// it only looks, up to [MaxVerify] when verify has it read and check every
// copy it holds before counting it.
func (c *Client) Has(ctx context.Context, node string, hashes []objstore.Hash, verify bool) (Holding, error) {
	limit := MaxHas
	if verify {
		limit = MaxVerify
	}
	if len(hashes) > limit {
		return Holding{}, fmt.Errorf("objstore/transfer: %d hashes in one request, over %d",
			len(hashes), limit)
	}
	rep, _, err := c.ask(ctx, node, request{Op: opHas, Hashes: hashes, Verify: verify}, nil)
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
	attempt, cancel := attemptContext(ctx)
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

// suspects is every member currently asked last, forgetting those whose
// cooldown has passed.
func (c *Client) suspects() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	out := map[string]bool{}
	for node, until := range c.suspect {
		if now.Before(until) {
			out[node] = true
		} else {
			delete(c.suspect, node)
		}
	}
	return out
}

// anySuspect reports whether any of nodes is currently asked last.
func (c *Client) anySuspect(nodes []string) bool {
	suspect := c.suspects()
	return slices.ContainsFunc(nodes, func(node string) bool { return suspect[node] })
}

// order is nodes with every suspect member moved to the end, the rest in
// their own order.
func (c *Client) order(nodes []string) []string {
	suspect := c.suspects()
	out := slices.Clone(nodes)
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

// attemptContext is one attempt's context: the caller's, capped by
// [attemptBudget].
func attemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < attemptBudget {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, attemptBudget)
}

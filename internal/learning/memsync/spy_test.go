package memsync

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// spyCopies is the changelog's own copies on a real broker ([changelogCopies]),
// watched: every pull a replay asks and every row it delivers is recorded, by
// the table's filter, and a case acts where its hooks say.
type spyCopies struct {
	inner copies

	// beforeAsk, when set, is called once per filter, before its replay
	// asks its first pull, with that replay's pulls: where a case races a
	// publish or a hydration against the hydration.
	beforeAsk func(filter string, pulls replayPulls)
	// beforeNext, when set, is called before each word a replay reads off
	// its inbox.
	beforeNext func()
	// heard, when set, is called with each word a replay is handed off its
	// inbox, as it is handed over.
	heard func(filter string, ev jsapi.Event)
	// behindThrough, when above 0, is where the FIRST consumer of each
	// table's replay stands as a copy behind: it hands the replay nothing
	// past this stream sequence, as a copy that has applied the changelog
	// only that far, and never catches up.
	behindThrough uint64

	mu      sync.Mutex
	pulls   map[string][]pulled
	asked   map[string]bool
	opened  map[string]int
	configs map[string][]jetstream.ConsumerConfig
}

// pulled is what one replay pull delivered of the table's rows: for each, in
// the order the pull delivered them, the bytes the broker counts it at against
// the pull's byte bound, and the bound the pull was asked with. The barrier a
// pull may deliver beside them is no row, and is not counted.
type pulled struct {
	sizes    []int
	maxBytes int
	n        int
	// headed says a row carried headers, which the broker counts too and
	// sizes does not.
	headed bool
}

// bytes is what the pull held.
func (p pulled) bytes() int {
	total := 0
	for _, size := range p.sizes {
		total += size
	}
	return total
}

func newSpyCopies(s *Syncer) *spyCopies {
	spy := &spyCopies{inner: changelogCopies{s: s}, pulls: map[string][]pulled{},
		asked: map[string]bool{}, opened: map[string]int{}, configs: map[string][]jetstream.ConsumerConfig{}}
	s.copies = spy
	return spy
}

// filterOf is the table's filter of a replay's, beside the seat's barrier.
func filterOf(cfg jetstream.ConsumerConfig) string {
	for _, subject := range cfg.FilterSubjects {
		if subject != barrierSubject(seat.AgentID) {
			return subject
		}
	}
	return cfg.FilterSubject
}

func (c *spyCopies) open(ctx context.Context, cfg jetstream.ConsumerConfig) (replayPulls, error) {
	filter := filterOf(cfg)
	c.mu.Lock()
	placement := c.opened[filter]
	c.opened[filter]++
	c.configs[filter] = append(c.configs[filter], cfg)
	c.mu.Unlock()
	pulls, err := c.inner.open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	sp := &spyPulls{replayPulls: pulls, c: c, filter: filter}
	if placement == 0 {
		sp.through = c.behindThrough
	}
	return sp, nil
}

func (c *spyCopies) drop(ctx context.Context, name string) { c.inner.drop(ctx, name) }

func (c *spyCopies) maxPayload() int64 { return c.inner.maxPayload() }

// of is the pulls of the replay over filter, in the order they were asked.
func (c *spyCopies) of(filter string) []pulled {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.pulls[filter])
}

// configsOf is every consumer configuration a replay over filter was made
// with, in order.
func (c *spyCopies) configsOf(filter string) []jetstream.ConsumerConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.configs[filter])
}

// spyPulls is one consumer's pulls, watched.
type spyPulls struct {
	replayPulls
	c      *spyCopies
	filter string
	// through, when above 0, is the last stream sequence the consumer's copy
	// hands over ([spyCopies.behindThrough]).
	through uint64
}

func (p *spyPulls) Ask(n, maxBytes int, expires, heartbeat time.Duration) error {
	c := p.c
	c.mu.Lock()
	first := !c.asked[p.filter]
	c.asked[p.filter] = true
	c.mu.Unlock()
	if first && c.beforeAsk != nil {
		c.beforeAsk(p.filter, p.replayPulls)
	}
	c.mu.Lock()
	c.pulls[p.filter] = append(c.pulls[p.filter], pulled{maxBytes: maxBytes, n: n})
	c.mu.Unlock()
	return p.replayPulls.Ask(n, maxBytes, expires, heartbeat)
}

func (p *spyPulls) Next(ctx context.Context) (jsapi.Event, error) {
	for {
		if p.c.beforeNext != nil {
			p.c.beforeNext()
		}
		ev, err := p.replayPulls.Next(ctx)
		if ev, shown := p.watch(ev, err); shown {
			p.told(ev, err)
			return ev, err
		}
	}
}

func (p *spyPulls) Ready() (jsapi.Event, bool, error) {
	for {
		ev, ok, err := p.replayPulls.Ready()
		if !ok {
			return ev, ok, err
		}
		if ev, shown := p.watch(ev, err); shown {
			p.told(ev, err)
			return ev, ok, err
		}
	}
}

// told hands the case's heard hook a word the replay is handed.
func (p *spyPulls) told(ev jsapi.Event, err error) {
	if err == nil && p.c.heard != nil {
		p.c.heard(p.filter, ev)
	}
}

// watch records a delivery the replay is handed, and says whether the replay
// sees ev: a delivery a copy behind does not hold is not seen, and one that
// ended its pull is seen as the pull's expiry.
func (p *spyPulls) watch(ev jsapi.Event, err error) (jsapi.Event, bool) {
	if err != nil || ev.Kind != jsapi.Delivered {
		return ev, true
	}
	if p.through > 0 && ev.Meta.Sequence.Stream > p.through {
		// A COPY BEHIND holds nothing past where it stands: what the
		// broker delivered past it is not the replay's to see, and a
		// pull it ended ends as one the copy had nothing for.
		if ev.Ended {
			return jsapi.Event{Kind: jsapi.Expired, Code: "408", Ended: true}, true
		}
		return ev, false
	}
	if ev.Msg.Subject != barrierSubject(seat.AgentID) {
		p.c.mu.Lock()
		pulls := p.c.pulls[p.filter]
		if len(pulls) > 0 {
			last := &pulls[len(pulls)-1]
			// WHAT THE BROKER COUNTS against a pull's bytes: the row's
			// subject, the acknowledgement subject it is delivered
			// with, its headers and its payload.
			last.sizes = append(last.sizes, len(ev.Msg.Subject)+len(ev.Msg.Reply)+len(ev.Msg.Data))
			last.headed = last.headed || len(ev.Msg.Header) > 0
		}
		p.c.mu.Unlock()
	}
	return ev, true
}

package memsync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A CHANGELOG AND ITS COPIES, PLAYED ON A BUBBLE'S CLOCK.
//
// The cases about how long a hydration waits, and on what, run in a synctest
// bubble, where a minute of a copy that stopped answering passes at once and a
// check that something did NOT happen moves the bubble's clock past every
// bound in play before it reads again. A real broker cannot run there: its
// read loop is outside the bubble, and a reply it hands a channel made inside
// one panics. So the changelog here is a model of one, and each consumer a
// hydration makes is a model of the COPY it is placed on:
//
//   - The changelog is the leader's log of every publish, in sequence order
//     ([fakeChangelog]); a copy holds the log as far as it has applied it, one
//     message per subject, the last at or below that point, which is how a
//     copy behind still holds a row the leader has since replaced, or an
//     earlier hydration's barrier.
//   - A consumer delivers its copy's store in sequence order from where it
//     was made to begin, reading the store at each delivery as the server's
//     getNextMsg does, so a row replaced before the consumer reached it is
//     delivered as its replacement, past where it stood.
//   - Each delivery takes as long as its copy's script says to cross, an idle
//     heartbeat is sent every heartbeat a pull is held with nothing to
//     deliver, and a pull ends at its expiry, its count or its bytes, as the
//     server ends one.
//
// What a copy does is its [copyScript], per table and per placement.

// fakeChangelog is the leader's log of every publish to the memory changelog,
// in the order it acknowledged them: the op at index i was acknowledged at
// stream sequence i+1.
type fakeChangelog struct {
	mu  sync.Mutex
	ops []fakeOp
	// moved is closed, and replaced, by every publish, so a consumer waiting
	// for its copy to hold more wakes.
	moved chan struct{}
	// lags is the sequences the copies' running count has not caught up
	// with yet: a delivery's count leaves them out ([fakeConsumer.pending]).
	lags map[uint64]bool
}

// fakeOp is one acknowledged publish.
type fakeOp struct {
	subject string
	data    []byte
}

// fakeMsg is a message a copy holds, at its stream sequence.
type fakeMsg struct {
	seq     uint64
	subject string
	data    []byte
}

func newFakeChangelog() *fakeChangelog {
	return &fakeChangelog{moved: make(chan struct{}), lags: map[uint64]bool{}}
}

// publish acknowledges one message and answers its sequence.
func (c *fakeChangelog) publish(subject string, data []byte) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, fakeOp{subject: subject, data: slices.Clone(data)})
	close(c.moved)
	c.moved = make(chan struct{})
	return uint64(len(c.ops))
}

// last is the newest sequence the changelog acknowledged.
func (c *fakeChangelog) last() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(len(c.ops))
}

// lagged marks seq as one the copies' running count has not caught up with.
func (c *fakeChangelog) lagged(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lags[seq] = true
}

// view is what a copy that has applied the log through applied holds of the
// subjects filter matches: the last message on each subject at or below
// applied, in sequence order.
func (c *fakeChangelog) view(applied uint64, filters []string) []fakeMsg {
	c.mu.Lock()
	defer c.mu.Unlock()
	newest := map[string]uint64{}
	for i := uint64(0); i < min(applied, uint64(len(c.ops))); i++ {
		newest[c.ops[i].subject] = i + 1
	}
	var held []fakeMsg
	for subject, seq := range newest {
		if !slices.ContainsFunc(filters, func(f string) bool { return topics.Match(f, subject) }) {
			continue
		}
		held = append(held, fakeMsg{seq: seq, subject: subject, data: c.ops[seq-1].data})
	}
	slices.SortFunc(held, func(a, b fakeMsg) int { return int(a.seq) - int(b.seq) })
	return held
}

// lagging reports whether the count leaves seq out.
func (c *fakeChangelog) lagging(seq uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lags[seq]
}

// waiting is the channel the next publish closes.
func (c *fakeChangelog) waiting() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moved
}

// copyScript is how the copy one consumer is placed on behaves.
type copyScript struct {
	// behind says the copy has applied the changelog only through holdsTo:
	// it holds nothing the changelog acknowledged past it, until it catches
	// up ([copyScript.catchUpAfter]). Otherwise the copy has applied
	// everything the changelog acknowledged, as it acknowledges it.
	behind  bool
	holdsTo uint64
	// catchUpAfter is how long after the consumer was placed a copy behind
	// applies the rest, and 0 for one that never does.
	catchUpAfter time.Duration
	// every is how long each delivery takes to reach the replay.
	every time.Duration
	// silentAfter, when above 0, is the deliveries after which the copy
	// says nothing more: no delivery, no heartbeat, no status, no answer
	// to its consumer's info.
	silentAfter int
	// dropFirstPull is a consumer whose first pull never reached it, as a
	// cluster drops a new consumer's first pull.
	dropFirstPull bool
	// overcounts is how many messages more than the copy holds every
	// delivery's count says follow it.
	overcounts uint64
	// placed, when set, is called once the consumer is placed, before the
	// reply that made it is counted: a publish racing the replay, landing
	// before it has read anything.
	placed func()
	// loses, when above 0, is the delivery, counted from the consumer's
	// first, that the link loses.
	loses uint64
}

// fakeCopies is every consumer one hydration makes on the model changelog,
// and what each was asked: the scripts say how each table's placements
// behave, in order, the last repeating; a table with none is served by a
// copy that holds everything at once.
type fakeCopies struct {
	log     *fakeChangelog
	scripts map[string][]copyScript
	// payload is the largest message the node's connection carries.
	payload int64

	mu        sync.Mutex
	opened    []openedCopy
	asked     []*askedPull
	dropped   []string
	consumers map[string]*fakeConsumer
	// inFlight is the most pulls ever open at once, across consumers.
	inFlight, mostInFlight int
}

// openedCopy is one consumer a hydration made: for which table, how, and
// whether the context it was made on carried a deadline of its own.
type openedCopy struct {
	table    string
	cfg      jetstream.ConsumerConfig
	at       time.Time
	deadline bool
}

// askedPull is one pull a hydration asked, and what it delivered.
type askedPull struct {
	table     string
	placement int
	n         int
	maxBytes  int
	delivered int
	bytes     int
	at        time.Time
}

func newFakeCopies(log *fakeChangelog) *fakeCopies {
	return &fakeCopies{log: log, scripts: map[string][]copyScript{}, payload: 8 << 20,
		consumers: map[string]*fakeConsumer{}}
}

// tableOf is the table a replay's filters read.
func tableOf(filters []string) string {
	for _, spec := range tables {
		if slices.Contains(filters, tableFilter(seat.AgentID, spec.name)) {
			return spec.name
		}
	}
	return ""
}

// place makes a consumer on a copy of the changelog for cfg.
func (c *fakeCopies) place(ctx context.Context, cfg jetstream.ConsumerConfig) *fakeConsumer {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := tableOf(cfg.FilterSubjects)
	placement := 0
	for _, o := range c.opened {
		if o.table == name {
			placement++
		}
	}
	_, deadline := ctx.Deadline()
	c.opened = append(c.opened, openedCopy{table: name, cfg: cfg, at: time.Now(), deadline: deadline})
	script := copyScript{}
	if scripts := c.scripts[name]; len(scripts) > 0 {
		script = scripts[min(placement, len(scripts)-1)]
	}
	cons := &fakeConsumer{copies: c, table: name, placement: placement,
		name:   fmt.Sprintf("replay-%s-%d", name, placement),
		filter: cfg.FilterSubjects, script: script}
	if script.behind && script.catchUpAfter > 0 {
		caught := make(chan struct{})
		cons.caught = caught
		time.AfterFunc(script.catchUpAfter, func() { close(caught) }) //fixedwait:simulated a copy behind applying the rest of the changelog
	}
	if cfg.DeliverPolicy == jetstream.DeliverByStartSequencePolicy && cfg.OptStartSeq > 0 {
		cons.pos = cfg.OptStartSeq - 1
	}
	c.consumers[cons.name] = cons
	return cons
}

// placing is [fakeCopies.place], with the copy's placed hook run once the
// consumer stands, outside every lock.
func (c *fakeCopies) placing(ctx context.Context, cfg jetstream.ConsumerConfig) *fakeConsumer {
	cons := c.place(ctx, cfg)
	if cons.script.placed != nil {
		cons.script.placed()
	}
	return cons
}

// openedFor is every consumer made for table, in order.
func (c *fakeCopies) openedFor(table string) []openedCopy {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []openedCopy
	for _, o := range c.opened {
		if o.table == table {
			out = append(out, o)
		}
	}
	return out
}

// pullsOf is every pull asked of table's consumers, in order.
func (c *fakeCopies) pullsOf(table string) []askedPull {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []askedPull
	for _, p := range c.asked {
		if p.table == table {
			out = append(out, *p)
		}
	}
	return out
}

// fakeConsumer is one consumer, on the copy its script describes.
type fakeConsumer struct {
	copies    *fakeCopies
	table     string
	placement int
	name      string
	filter    []string
	script    copyScript
	// caught is closed once a copy behind has caught up, and nil for one
	// that never does.
	caught chan struct{}

	// pos is the stream sequence of the last message the consumer
	// delivered, or the one before where it begins.
	pos uint64
	// sent is the consumer's count of its deliveries, and arrived the count
	// of the last one that reached the replay.
	sent, arrived uint64
	pull          *fakePull
	pullsAsked    int
	// inflight is a delivery on its way, arriving when arrival fires, or at
	// once where it is nil.
	inflight *fakeMsg
	arrival  *time.Timer
	// completed is a pull whose count ran out with bytes to spare: the next
	// word is its Batch Completed.
	completed bool
	stopped   bool
}

// fakePull is the pull a consumer holds.
type fakePull struct {
	n         int
	bytesLeft int
	bounded   bool
	// expiry fires at the pull's expiry, and beat at each of its idle
	// heartbeats (nil where it asked for none).
	expiry *time.Timer
	beat   *time.Ticker
	// dropped is a pull the consumer never received.
	dropped bool
	rec     *askedPull
}

// applied is how far the consumer's copy has applied the changelog now.
func (f *fakeConsumer) applied() uint64 {
	if f.script.behind && f.catching() != nil {
		return f.script.holdsTo
	}
	return f.copies.log.last()
}

// catching is what a copy still behind waits on to catch up: never, for one
// that never does, and nil for one that is not behind or has caught up.
func (f *fakeConsumer) catching() <-chan struct{} {
	if !f.script.behind {
		return nil
	}
	if f.caught == nil {
		return make(chan struct{})
	}
	select {
	case <-f.caught:
		return nil
	default:
		return f.caught
	}
}

// held is what the consumer's copy holds past where it stands.
func (f *fakeConsumer) held() []fakeMsg {
	return slices.DeleteFunc(f.copies.log.view(f.applied(), f.filter),
		func(m fakeMsg) bool { return m.seq <= f.pos })
}

// pending is the copy's running count of what follows the consumer's
// position: what it holds past it, less what the count has not caught up
// with, and more by what the script says it overcounts.
func (f *fakeConsumer) pending() uint64 {
	n := uint64(0)
	for _, m := range f.held() {
		if !f.copies.log.lagging(m.seq) {
			n++
		}
	}
	return n + f.script.overcounts
}

// silent reports a copy that says nothing more.
func (f *fakeConsumer) silent() bool {
	return f.script.silentAfter > 0 && f.sent >= uint64(f.script.silentAfter)
}

// fakeReply is the acknowledgement subject a delivery comes with, which the
// broker counts against a pull's bytes beside the subject and the payload.
const fakeReply = "$JS.ACK.KV_x.memory.replay.1.1.1.1700000000000000000.0"

// deliver takes the next message the copy holds, moving the consumer past it,
// and answers it, or nil where the copy holds none; lost says the link lost
// it.
func (f *fakeConsumer) deliver() (msg *fakeMsg, lost bool) {
	held := f.held()
	if len(held) == 0 {
		return nil, false
	}
	next := held[0]
	f.pos = next.seq
	f.sent++
	return &next, f.script.loses > 0 && f.sent == f.script.loses
}

// natsMsg is a delivery as it reaches the replay, its metadata counting what
// the copy holds past it.
func (f *fakeConsumer) natsMsg(m fakeMsg) (*nats.Msg, *nats.MsgMetadata) {
	meta := &nats.MsgMetadata{
		Sequence:   nats.SequencePair{Stream: m.seq, Consumer: f.sent},
		NumPending: f.pending(),
		Stream:     topics.MemoryStream,
		Consumer:   f.name,
	}
	return &nats.Msg{Subject: m.subject, Reply: fakeReply, Data: m.data}, meta
}

// fakeSize is a message as the broker counts it against a pull.
func fakeSize(m fakeMsg) int {
	return len(m.subject) + len(fakeReply) + len(m.data)
}

// fakeJS is the changelog's JetStream for a hydration on a bubble's clock:
// its identity read, its barriers' publishes and its count of the retired
// tables, all answered by the model changelog at once. A replay's consumers
// are the copies' ([fakeCopies]).
//
// Embedded rather than stubbed: anything else called on it is a nil
// interface's panic, which is a case reaching further than it means to.
type fakeJS struct {
	jetstream.JetStream
	log     *fakeChangelog
	created time.Time
	conn    *nats.Conn
	// replay, when set, serves the CreateConsumer of a table's replay.
	replay func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error)
}

func (j *fakeJS) Options() jetstream.JetStreamOptions {
	return jetstream.JetStreamOptions{DefaultTimeout: 5 * time.Second}
}

func (j *fakeJS) Conn() *nats.Conn { return j.conn }

func (j *fakeJS) Stream(context.Context, string) (jetstream.Stream, error) {
	return fakeStream{info: &jetstream.StreamInfo{Config: jetstream.StreamConfig{
		Name:     topics.MemoryStream,
		Metadata: map[string]string{statelog.CreatedAtKey: j.created.Format(time.RFC3339Nano)},
	}, Created: j.created}}, nil
}

func (j *fakeJS) Publish(_ context.Context, subject string, data []byte,
	_ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	return &jetstream.PubAck{Stream: topics.MemoryStream, Sequence: j.log.publish(subject, data)}, nil
}

func (j *fakeJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	if len(cfg.FilterSubjects) > 0 && j.replay != nil {
		return j.replay(ctx, cfg)
	}
	// A RETIRED TABLE'S COUNT, which the model changelog holds none of.
	return countedConsumer{info: &jetstream.ConsumerInfo{Name: "count-" + cfg.FilterSubject}}, nil
}

func (j *fakeJS) DeleteConsumer(context.Context, string, string) error { return nil }

type fakeStream struct {
	jetstream.Stream
	info *jetstream.StreamInfo
}

func (s fakeStream) CachedInfo() *jetstream.StreamInfo { return s.info }

// countedConsumer is a consumer whose creation counted nothing.
type countedConsumer struct {
	jetstream.Consumer
	info *jetstream.ConsumerInfo
}

func (c countedConsumer) CachedInfo() *jetstream.ConsumerInfo { return c.info }

// capturingJS takes every row a publish of a seat sends, in order, so a case
// can put a seat's memory as a real publish carries it onto the model
// changelog.
type capturingJS struct {
	jetstream.JetStream
	ops []fakeOp
}

func (j *capturingJS) Publish(_ context.Context, subject string, data []byte,
	_ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	j.ops = append(j.ops, fakeOp{subject: subject, data: slices.Clone(data)})
	return &jetstream.PubAck{}, nil
}

// publishedBy is every row a publish of the seat db holds sends, in the order
// it sends them.
func publishedBy(t *testing.T, db *store.DB) []fakeOp {
	t.Helper()
	capture := &capturingJS{}
	if _, err := syncerOver(db, capture).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish the seat's memory: %v", err)
	}
	return capture.ops
}

// onto puts ops on the model changelog and answers the sequence of each.
func onto(log *fakeChangelog, ops []fakeOp) []uint64 {
	seqs := make([]uint64, 0, len(ops))
	for _, op := range ops {
		seqs = append(seqs, log.publish(op.subject, op.data))
	}
	return seqs
}

// notesOnto puts n diary notes of size bytes each on the model changelog,
// ids prefix and a number, and answers their ids.
func notesOnto(t *testing.T, log *fakeChangelog, prefix string, n, size int) []string {
	t.Helper()
	spec := registered(t, "agent_diary")
	ids := make([]string, 0, n)
	for i := range n {
		note := noteOfSize(t, fmt.Sprintf("%s%05d", prefix, i), size)
		body, err := encode(note)
		if err != nil {
			t.Fatal(err)
		}
		log.publish(spec.subject(seat.AgentID, note.Values), body)
		ids = append(ids, note.Values["id"].(string))
	}
	return ids
}

// hydration is one Hydrate run on its own goroutine, read once it returns.
type hydration struct {
	done    chan struct{}
	carried int
	err     error
}

// hydrating starts s's hydration of the seat.
func hydrating(ctx context.Context, s *Syncer) *hydration {
	h := &hydration{done: make(chan struct{})}
	go func() {
		defer close(h.done)
		h.carried, h.err = s.Hydrate(ctx, seat.Handle)
	}()
	return h
}

// returned reports whether the hydration has returned, without waiting.
func (h *hydration) returned() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// says reports whether err says every one of words.
func says(err error, words ...string) bool {
	if err == nil {
		return false
	}
	for _, w := range words {
		if !strings.Contains(err.Error(), w) {
			return false
		}
	}
	return true
}

// THE CONSUMER'S PULLS, as a replay asks them on one inbox (jsapi.Pulls).

// Ask asks the consumer for up to n deliveries, no more than maxBytes in all
// as the broker counts them, held for expires and heartbeated every heartbeat
// while it has nothing to deliver.
func (f *fakeConsumer) Ask(n, maxBytes int, expires, heartbeat time.Duration) error {
	c := f.copies
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.pull == nil {
		c.inFlight++
		c.mostInFlight = max(c.mostInFlight, c.inFlight)
	}
	f.pullsAsked++
	rec := &askedPull{table: f.table, placement: f.placement, n: n, maxBytes: maxBytes, at: time.Now()}
	c.asked = append(c.asked, rec)
	if f.pull != nil {
		f.pull.stop()
	}
	f.pull = &fakePull{n: n, bytesLeft: maxBytes, bounded: maxBytes > 0,
		expiry:  time.NewTimer(expires), //fixedwait:simulated the server holding a pull to its expiry
		dropped: f.script.dropFirstPull && f.pullsAsked == 1, rec: rec}
	if heartbeat > 0 {
		f.pull.beat = time.NewTicker(heartbeat) //fixedwait:simulated the server's idle heartbeats
	}
	return nil
}

// stop stops the pull's timers.
func (p *fakePull) stop() {
	p.expiry.Stop()
	if p.beat != nil {
		p.beat.Stop()
	}
}

// ended closes the consumer's open pull in the copies' books.
func (f *fakeConsumer) ended() {
	if f.pull == nil {
		return
	}
	f.copies.mu.Lock()
	f.copies.inFlight--
	f.copies.mu.Unlock()
	f.pull.stop()
	f.pull = nil
}

// Next is the next word on the replay's inbox before ctx ends.
func (f *fakeConsumer) Next(ctx context.Context) (jsapi.Event, error) {
	for {
		switch {
		case f.stopped:
			return jsapi.Event{}, errors.New("the replay's pulls were stopped")
		case f.inflight != nil:
			if f.arrival != nil {
				select {
				case <-ctx.Done():
					return jsapi.Event{}, ctx.Err()
				case <-f.arrival.C:
				}
			}
			return f.arrive()
		case f.silent():
			<-ctx.Done()
			return jsapi.Event{}, ctx.Err()
		case f.completed:
			f.completed = false
			f.ended()
			return jsapi.Event{Kind: jsapi.BatchCompleted, Code: "409",
				Description: "Batch Completed", Ended: true}, nil
		case f.pull == nil || f.pull.dropped:
			// NO PULL THE CONSUMER HOLDS: nothing ever comes.
			<-ctx.Done()
			return jsapi.Event{}, ctx.Err()
		}
		if held := f.held(); len(held) > 0 && f.pull.bounded && fakeSize(held[0]) > f.pull.bytesLeft {
			f.ended()
			return jsapi.Event{Kind: jsapi.PastMaxBytes, Code: "409",
				Description: "Message Size Exceeds MaxBytes", Ended: true}, nil
		}
		if msg, lost := f.deliver(); msg != nil {
			if lost {
				continue
			}
			f.inflight, f.arrival = msg, nil
			if f.script.every > 0 {
				f.arrival = time.NewTimer(f.script.every) //fixedwait:simulated a delivery crossing the link
			}
			continue
		}
		// NOTHING TO DELIVER: the pull is held until its next heartbeat, its
		// expiry, a publish, or the copy catching up.
		var beat <-chan time.Time
		if f.pull.beat != nil {
			beat = f.pull.beat.C
		}
		select {
		case <-ctx.Done():
			return jsapi.Event{}, ctx.Err()
		case <-f.copies.log.waiting():
		case <-f.catching():
		case <-f.pull.expiry.C:
			f.ended()
			return jsapi.Event{Kind: jsapi.Expired, Code: "408", Description: "Request Timeout",
				Ended: true}, nil
		case <-beat:
			return jsapi.Event{Kind: jsapi.Heartbeat, Code: "100", Description: "Idle Heartbeat"}, nil
		}
	}
}

// arrive hands the delivery in flight over, counted against the open pull.
func (f *fakeConsumer) arrive() (jsapi.Event, error) {
	m := *f.inflight
	f.inflight = nil
	msg, meta := f.natsMsg(m)
	if meta.Sequence.Consumer != f.arrived+1 {
		return jsapi.Event{}, fmt.Errorf("%w: the consumer counted delivery %d after %d",
			jsapi.ErrDeliveryLost, meta.Sequence.Consumer, f.arrived)
	}
	f.arrived = meta.Sequence.Consumer
	ev := jsapi.Event{Kind: jsapi.Delivered, Msg: msg, Meta: meta}
	if p := f.pull; p != nil {
		p.rec.delivered++
		p.rec.bytes += fakeSize(m)
		p.n--
		p.bytesLeft -= fakeSize(m)
		switch {
		case p.bounded && p.bytesLeft == 0:
			ev.Ended = true
			f.ended()
		case p.n == 0:
			f.completed = true
		}
	}
	return ev, nil
}

// Quiet answers what the consumer says of itself to a replay that heard
// nothing for a while, as jsapi.Pulls.Quiet reads its info; a copy that says
// nothing more answers nothing until ctx ends.
func (f *fakeConsumer) Quiet(ctx context.Context, _ time.Duration) (jsapi.QuietSays,
	*jetstream.ConsumerInfo, error) {

	if f.silent() {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	waiting := 0
	if f.pull != nil && !f.pull.dropped {
		waiting = 1
	}
	info := &jetstream.ConsumerInfo{Name: f.name, NumPending: f.pending(), NumWaiting: waiting,
		Delivered: jetstream.SequenceInfo{Consumer: f.sent}}
	switch {
	case f.inflight != nil, f.sent > f.arrived:
		return jsapi.StillCrossing, info, nil
	case info.NumPending == 0:
		return jsapi.Drained, info, nil
	case waiting > 0:
		return jsapi.StillCrossing, info, nil
	}
	return jsapi.Unpulled, info, nil
}

// Stop ends the replay's pulls of the consumer.
func (f *fakeConsumer) Stop() {
	f.ended()
	f.stopped = true
}

// Name is the consumer's name, which the replay deletes it by.
func (f *fakeConsumer) Name() string { return f.name }

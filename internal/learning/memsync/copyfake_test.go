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

// A CHANGELOG AND ITS COPIES, PLAYED ON A BUBBLE'S CLOCK: the timing cases
// run in a synctest bubble, where a real broker cannot (its read loop is
// outside the bubble, and its reply to a channel made inside one panics). So
// the changelog is a model ([fakeChangelog]), and each consumer a model of the
// copy it is placed on, behaving as its [copyScript] says:
//   - a copy holds the last message per subject at or below how far it has
//     applied the log, so a copy behind still holds a replaced row or an
//     earlier hydration's barrier;
//   - a consumer reads its copy at each delivery, as the server's getNextMsg
//     does, so a row replaced before it was reached comes as its replacement;
//   - each delivery takes its script's crossing time, and a pull ends at its
//     expiry, count or bytes, with idle heartbeats while it has nothing.

// fakeChangelog is the leader's log of every publish to the memory changelog,
// in the order it acknowledged them: the op at index i was acknowledged at
// stream sequence i+1.
type fakeChangelog struct {
	mu  sync.Mutex
	ops []fakeOp
	// moved is closed and replaced by every publish, waking a consumer that
	// waits for its copy to hold more.
	moved chan struct{}
	// lags is the sequences the copies' running count leaves out
	// ([fakeConsumer.pending]).
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
// subjects filters match: each subject's last message at or below applied, in
// sequence order.
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
	// behind says the copy has applied the changelog only through holdsTo
	// until it catches up ([copyScript.catchUpAfter]); otherwise it applies
	// each publish as it is acknowledged.
	behind  bool
	holdsTo uint64
	// catchUpAfter is how long after placement a copy behind applies the
	// rest, and 0 for one that never does.
	catchUpAfter time.Duration
	// every is how long each delivery takes to reach the replay.
	every time.Duration
	// silentAfter, when above 0, is the deliveries after which the copy says
	// nothing more: no delivery, heartbeat, status or info answer.
	silentAfter int
	// mute is a copy silent from its placement on.
	mute bool
	// dropFirstPull is a consumer whose first pull never reaches it, as a
	// cluster drops a new consumer's first pull.
	dropFirstPull bool
	// overcounts is how many more messages than the copy holds every
	// delivery's count says follow it.
	overcounts uint64
	// placed, when set, runs once the consumer is placed, before the reply
	// that made it is counted: a publish racing the replay before it reads.
	placed func()
	// loses, when above 0, is the delivery, counted from the consumer's
	// first, that the link loses.
	loses uint64
	// refuses, when set, is the description of the `409` every pull is
	// refused with: a consumer deleted under its pulls, or a broker limit.
	refuses string
	// maxBatch, when above 0, is the most messages the consumer takes in one
	// pull; a pull asking more is refused (`409 Exceeded MaxRequestBatch of
	// N`), as the broker refuses one.
	maxBatch int
	// firstPullAfter, when above 0, is how long the first pull takes to
	// reach the consumer across a slow link; it knows nothing of it until then.
	firstPullAfter time.Duration
	// probeUnanswered is a consumer whose info nobody answers, while its
	// deliveries arrive.
	probeUnanswered bool
}

// fakeCopies is every consumer one hydration makes on the model changelog,
// and what each was asked. scripts gives each table's placements in order, the
// last repeating; a table with none gets a copy that holds everything at once.
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

// openedCopy is one consumer a hydration made: its table, its config, and
// whether the context it was made on carried a deadline.
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

// placing is [fakeCopies.place], then the copy's placed hook, outside every
// lock.
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

	// pos is the stream sequence the consumer last delivered, or the one
	// before where it begins.
	pos uint64
	// sent counts the consumer's deliveries; arrived is the count of the
	// last one that reached the replay.
	sent, arrived uint64
	pull          *fakePull
	pullsAsked    int
	// inflight is a delivery on its way, arriving when arrival fires, or at
	// once where arrival is nil.
	inflight *fakeMsg
	arrival  *time.Timer
	// completed is a pull whose count ran out with bytes to spare: its Batch
	// Completed is the next word.
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
	// dropped is a pull the consumer has not received: never where reaches
	// is nil, otherwise once reaches fires.
	dropped bool
	reaches <-chan time.Time
	// refusal, when set, is the status the consumer refuses the pull with.
	refusal *jsapi.Event
	rec     *askedPull
}

// applied is how far the consumer's copy has applied the changelog now.
func (f *fakeConsumer) applied() uint64 {
	if f.script.behind && f.catching() != nil {
		return f.script.holdsTo
	}
	return f.copies.log.last()
}

// catching is what a copy still behind waits on to catch up: a channel that
// never closes for one that never does, nil once caught up or never behind.
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

// pending is the copy's running count of what follows the consumer: what it
// holds past it, less the lagged sequences, plus the script's overcount.
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
	return f.script.mute || f.script.silentAfter > 0 && f.sent >= uint64(f.script.silentAfter)
}

// fakeReply is a delivery's acknowledgement subject, which the broker counts
// against a pull's bytes beside the subject and the payload.
const fakeReply = "$JS.ACK.KV_x.memory.replay.1.1.1.1700000000000000000.0"

// deliver moves the consumer past the next message its copy holds and
// answers it, or nil where it holds none; lost says the link lost it.
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
// the identity read, barrier publishes and retired tables' counts, answered
// by the model changelog at once unless a case says otherwise; a replay's
// consumers are the copies' ([fakeCopies]). Embedded rather than stubbed: any
// other call panics on the nil interface, a case reaching further than meant.
type fakeJS struct {
	jetstream.JetStream
	log     *fakeChangelog
	created time.Time
	conn    *nats.Conn
	// replay, when set, serves the CreateConsumer of a table's replay.
	replay func(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, error)

	// identityUnanswered is a changelog whose info nobody answers.
	identityUnanswered bool
	// identityAt, when set, is when the changelog's info is first answered,
	// as a reply held behind a link's other traffic.
	identityAt time.Time
	// ackAfter, when set, is how long the nth barrier publish's
	// acknowledgement takes (from 1, each request counted), the publish
	// stored as it comes; a negative one never comes.
	ackAfter func(n int) time.Duration
	// countUnanswered is a retired table's count whose create nobody
	// answers.
	countUnanswered bool

	mu sync.Mutex
	// publishes is the barrier publishes asked so far.
	publishes int
	// countAsked is when a retired table's count was first asked, and
	// deleteBounds the bound each consumer's deletion was given.
	countAsked   time.Time
	deleteBounds []time.Duration
}

func (j *fakeJS) Options() jetstream.JetStreamOptions {
	return jetstream.JetStreamOptions{DefaultTimeout: 5 * time.Second}
}

func (j *fakeJS) Conn() *nats.Conn { return j.conn }

func (j *fakeJS) Stream(ctx context.Context, _ string) (jetstream.Stream, error) {
	if j.identityUnanswered {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if wait := time.Until(j.identityAt); !j.identityAt.IsZero() && wait > 0 { //fixedwait:measure how long the bubble's clock has to run to the instant the case scripted
		answered := time.NewTimer(wait) //fixedwait:simulated an identity reply held behind a link's other traffic
		defer answered.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-answered.C:
		}
	}
	return fakeStream{info: &jetstream.StreamInfo{Config: jetstream.StreamConfig{
		Name:     topics.MemoryStream,
		Metadata: map[string]string{statelog.CreatedAtKey: j.created.Format(time.RFC3339Nano)},
	}, Created: j.created}}, nil
}

func (j *fakeJS) Publish(ctx context.Context, subject string, data []byte,
	_ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	j.mu.Lock()
	j.publishes++
	n := j.publishes
	j.mu.Unlock()
	if j.ackAfter != nil {
		switch after := j.ackAfter(n); {
		case after < 0:
			<-ctx.Done()
			return nil, ctx.Err()
		case after > 0:
			acked := time.NewTimer(after) //fixedwait:simulated a barrier's acknowledgement on its way
			defer acked.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-acked.C:
			}
		}
	}
	return &jetstream.PubAck{Stream: topics.MemoryStream, Sequence: j.log.publish(subject, data)}, nil
}

func (j *fakeJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	if len(cfg.FilterSubjects) > 0 && j.replay != nil {
		return j.replay(ctx, cfg)
	}
	// A RETIRED TABLE'S COUNT, which the model changelog holds none of.
	j.mu.Lock()
	if j.countAsked.IsZero() {
		j.countAsked = time.Now()
	}
	j.mu.Unlock()
	if j.countUnanswered {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return countedConsumer{info: &jetstream.ConsumerInfo{Name: "count-" + cfg.FilterSubject}}, nil
}

func (j *fakeJS) DeleteConsumer(ctx context.Context, _, _ string) error {
	deadline, ok := ctx.Deadline()
	j.mu.Lock()
	defer j.mu.Unlock()
	bound := time.Duration(-1)
	if ok {
		bound = time.Until(deadline) //fixedwait:measure the bound the deletion was handed, read on a bubble's clock that stands still meanwhile
	}
	j.deleteBounds = append(j.deleteBounds, bound)
	return nil
}

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

// capturingJS records every row a seat's publish sends, in order, so a case
// can put a seat's memory onto the model changelog as a real publish does.
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

// notesOnto puts n diary notes of size bytes each, ids prefix plus a number,
// on the model changelog and answers their ids.
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
	if f.script.firstPullAfter > 0 && f.pullsAsked == 1 {
		f.pull.dropped = true
		f.pull.reaches = time.After(f.script.firstPullAfter) //fixedwait:simulated a pull crossing a slow link
	}
	switch {
	case f.script.refuses != "":
		f.pull.refusal = &jsapi.Event{Kind: jsapi.Refused, Code: "409", Description: f.script.refuses,
			Ended: true}
	case f.script.maxBatch > 0 && n > f.script.maxBatch:
		f.pull.refusal = &jsapi.Event{Kind: jsapi.Refused, Code: "409",
			Description: fmt.Sprintf("Exceeded MaxRequestBatch of %d", f.script.maxBatch), Ended: true}
	}
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

// Next is the next word on the replay's inbox before ctx ends. A ready word
// wins over ctx's ending, so [fakeConsumer.Ready] can read the inbox on a
// context already over.
func (f *fakeConsumer) Next(ctx context.Context) (jsapi.Event, error) {
	for {
		switch {
		case f.stopped:
			return jsapi.Event{}, errors.New("the replay's pulls were stopped")
		case f.inflight != nil:
			if f.arrival != nil {
				select {
				case <-f.arrival.C:
				default:
					select {
					case <-ctx.Done():
						return jsapi.Event{}, ctx.Err()
					case <-f.arrival.C:
					}
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
		case f.pull == nil:
			// NO PULL THE CONSUMER HOLDS: nothing ever comes.
			<-ctx.Done()
			return jsapi.Event{}, ctx.Err()
		case f.pull.dropped:
			// A PULL THE CONSUMER HAS NOT RECEIVED: nothing comes until it
			// does.
			select {
			case <-f.pull.reaches:
			default:
				select {
				case <-ctx.Done():
					return jsapi.Event{}, ctx.Err()
				case <-f.pull.reaches:
				}
			}
			f.pull.dropped, f.pull.reaches = false, nil
			continue
		case f.pull.refusal != nil:
			ev := *f.pull.refusal
			f.ended()
			return ev, nil
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
		moved, caught := f.copies.log.waiting(), f.catching()
		select {
		case <-moved:
			continue
		case <-caught:
			continue
		case <-f.pull.expiry.C:
			return f.expired(), nil
		case <-beat:
			return fakeHeartbeat, nil
		default:
		}
		select {
		case <-ctx.Done():
			return jsapi.Event{}, ctx.Err()
		case <-moved:
		case <-caught:
		case <-f.pull.expiry.C:
			return f.expired(), nil
		case <-beat:
			return fakeHeartbeat, nil
		}
	}
}

// fakeHeartbeat is an idle heartbeat.
var fakeHeartbeat = jsapi.Event{Kind: jsapi.Heartbeat, Code: "100", Description: "Idle Heartbeat"}

// expired ends the consumer's pull at its expiry.
func (f *fakeConsumer) expired() jsapi.Event {
	f.ended()
	return jsapi.Event{Kind: jsapi.Expired, Code: "408", Description: "Request Timeout", Ended: true}
}

// Ready is the word the replay's inbox already holds, without waiting.
func (f *fakeConsumer) Ready() (jsapi.Event, bool, error) {
	over, cancel := context.WithCancel(context.Background())
	cancel()
	ev, err := f.Next(over)
	switch {
	case errors.Is(err, context.Canceled):
		return jsapi.Event{}, false, nil
	case err != nil:
		return jsapi.Event{}, false, err
	}
	return ev, true, nil
}

// RequestBatch is the most messages the consumer takes in one pull.
func (f *fakeConsumer) RequestBatch() int { return f.script.maxBatch }

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

// Quiet is what the consumer says of itself to a replay that heard nothing
// for a while, as jsapi.Pulls.Quiet reads its info; a silent copy answers
// nothing until ctx ends.
func (f *fakeConsumer) Quiet(ctx context.Context, _ time.Duration) (jsapi.QuietSays,
	*jetstream.ConsumerInfo, error) {

	if f.silent() || f.script.probeUnanswered {
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

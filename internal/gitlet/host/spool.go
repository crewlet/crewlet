package host

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"
)

// THE SPOOL ALLOWANCE: the node-wide bytes every fetch request's spool
// (body.go) draws on, and the one queue a request waits in for its room
// (package doc, "A fetch request is spooled, and bounded by the repository it
// names"; the sizes in budget.go). Never the cache's budget: a spool has no
// name the cache counts (cache.Spool), and counted there one large request
// would evict warm repositories.
//
// A request that finds no room WAITS ITS TURN (grantLocked) and is refused
// only when its wait ends unserved (await) or it finds no place to wait
// (placeLocked): the share stops one credential HOLDING the node's spool, and
// refusing past it would fail lazy fetches git never retries.

// spoolGrain is the most room a body arriving takes at a time in production
// (spoolSizes.grain): a mebibyte, so the allowance's lock is taken once a
// mebibyte rather than once a piece (spoolPiece), and a body holds at most a
// mebibyte it has not written, given back at its end. Room is taken as bytes
// arrive, never reserved up front: reserved by its repository's bound (fifty
// bytes an object), a few-megabyte fetch to a repository of millions of
// objects would hold a whole share while its body arrived.
const spoolGrain = 1 << 20

// How a request's turn at the spool allowance ended, as
// crewlet.code.host.spool.waits counts it by `outcome`. A request granted its
// room on arrival never waited, and is not counted.
const (
	// spoolGranted: it waited, and was granted its room.
	spoolGranted = "granted"
	// spoolExpired: its wait reached its bound unserved (503).
	spoolExpired = "expired"
	// spoolAbandoned: its request ended while it waited — its client gone,
	// as the transport reports it, or the host stopping.
	spoolAbandoned = "abandoned"
	// spoolRefused: it had to wait and found no place to: the node's waiting
	// room full, or its credential's quarter of it (503).
	spoolRefused = "refused"
)

// spoolOutcomes is every outcome a turn ends in, in the order the metric's
// reference lists them.
var spoolOutcomes = []string{spoolGranted, spoolExpired, spoolAbandoned, spoolRefused}

// spoolWait is how a request's turn went: its outcome, "" for a request
// granted its room on arrival, and how long it waited.
type spoolWait struct {
	outcome string
	took    time.Duration
}

// spoolUnserved is a request the spool allowance did not serve — errSpoolFull,
// with why: its wait expired, or it was refused a place to wait.
type spoolUnserved struct {
	// reason is spoolExpired or spoolRefused.
	reason string
	// detail is what held it back, in words a person reads.
	detail string
}

func (e *spoolUnserved) Error() string { return errSpoolFull.Error() + ": " + e.detail }
func (e *spoolUnserved) Unwrap() error { return errSpoolFull }

// errPastClaim is a body asking for room past its claim (spoolBody.claim).
// Answered as a body that could not be read (400), and never granted: room
// past a claim is room the bodies behind it were told was theirs.
var errPastClaim = errors.New("host: the request's body is longer than its client declared")

// tokenPerRunner is what a request held back by its own credential, a
// person's or a token's, is told to do ([ownRemedy]).
const tokenPerRunner = "give each CI runner a token of its own"

// boxesShareOne is what a CODING BOX'S request held back by its credential is
// told instead: every box on a node is the one spool credential [boxSpool], so
// no token of its own would help.
const boxesShareOne = "every coding box on this node shares one credential's room: retry once the boxes' " +
	"fetches ahead of this one end, or fetch fewer objects at once"

// ownRemedy is what a request held back by its own credential's requests is
// told to do: [boxesShareOne] for the boxes' credential, [tokenPerRunner] for
// any other.
func ownRemedy(credential string) string {
	if credential == boxSpool {
		return boxesShareOne
	}
	return tokenPerRunner
}

// spoolSizes are a spool allowance's bounds (SpoolSizesFor in production).
type spoolSizes struct {
	// limit and share are the bytes every spool, and each credential's, may
	// hold at once; request is the most one request spools (SpoolRequestBytes,
	// held below share), the claim of every body of undeclared length.
	limit, share, request int64
	// grain is the most room a body arriving asks for at a time
	// (spoolGrain).
	grain int64
	// places and placesPerCredential are how many requests may wait to
	// start at once, on the node and of one credential's.
	places, placesPerCredential int
}

// spoolSizesFor is the allowance of a node whose transfer lane has transfers
// slots.
func spoolSizesFor(transfers int) spoolSizes {
	return spoolSizes{limit: SpoolAllowance(transfers), share: SpoolCredentialBytes(transfers),
		request: SpoolRequestBytes(transfers), grain: spoolGrain, places: SpoolWaiters(transfers),
		placesPerCredential: SpoolWaitersPerCredential(transfers)}
}

// check reports why z cannot be an allowance: request past the node's limit
// or a credential's share, where even the oldest body could wait on nothing
// but itself, or sizes under which nothing could ever arrive.
func (z spoolSizes) check() error {
	switch {
	case z.limit < 0 || z.share < 0 || z.request < 0 || z.places < 0 || z.placesPerCredential < 0:
		return fmt.Errorf("host: spool sizes %+v: none may be negative", z)
	case z.request < 1 || z.grain < 1:
		return fmt.Errorf("host: spool sizes %+v: a body must be able to arrive, a byte at a time at least", z)
	case z.request > z.limit:
		return fmt.Errorf("host: spool sizes %+v: a body of %d bytes is past the node's %d", z, z.request, z.limit)
	case z.request > z.share:
		return fmt.Errorf("host: spool sizes %+v: a body of %d bytes is past a credential's %d", z, z.request,
			z.share)
	}
	return nil
}

// pastCeiling is a request past what this node spools for one request: it can
// never be taken, so it is refused (413) at once, never made to wait.
func (z spoolSizes) pastCeiling() error {
	return fmt.Errorf("%w: past the %d bytes this node spools for one request", errSpoolPastCeiling, z.request)
}

// unbounded is a bound no grant meets: a body with nothing older than it.
const unbounded = math.MaxInt64

// spools is the node's spool allowance and the queue of requests waiting for
// room in it.
type spools struct {
	spoolSizes

	mu    sync.Mutex
	used  int64
	bytes map[string]int64
	// bodies are the bodies arriving, in the order they started: the order
	// every grant keeps able to finish. seq numbers them as they start.
	bodies []*spoolBody
	seq    uint64
	// open is the spools started and not yet given back, arriving or ended.
	open int
	// placed is each credential's requests holding a place, inPlaces the
	// node's — granted ones included until their heads are in their spools.
	placed   map[string]int
	inPlaces int
	// queue is every request waiting: the bodies arriving that wait for room
	// (rooms of them), oldest first, then the requests yet to start, in
	// arrival order (grantLocked).
	queue []*spoolWaiter
	rooms int
	// runs is boundsLocked's per-credential working, kept to be cleared
	// rather than made again on every pass.
	runs map[string]credentialRun

	// armed, when set, is told the bound of every wait a request begins
	// ([spools.await]), which no answer names (a 503 says how long it waited).
	// Set only by a case (export_test.go's RecordSpoolWaits), and read without
	// a lock, so a case sets it before the requests it sends.
	armed func(time.Duration)
}

// spoolBody is one spool the allowance has granted, from its start until it is
// given back: while arriving, in spools.bodies with its claim; once ended,
// only its bytes. A body holds back ROOM, never a turn: a client at the pace
// floor (objstore.MiBPace) holds only what it has sent, and a body behind it
// waits for it only once the bodies arriving hold all but its claim.
type spoolBody struct {
	credential string
	seq        uint64
	// claim is the most it can grow to: request, or the length its client
	// declared for an unencoded body, where net/http ends it (git declares none
	// for a lazy fetch the host spools). Never its repository's bound, which a
	// stale row may understate: a claim may never grow, since a body waiting
	// for more would wait on the younger bodies, which wait on its room. held
	// is the room it has taken, written or not; what it spooled, once ended.
	claim, held int64
	arriving    bool
	// roomNode and roomCredential are the most room a grant to it may take
	// without leaving the claim of a body older than it short — the node's
	// and its credential's — as boundsLocked last worked them out.
	roomNode, roomCredential int64
}

// credentialRun is a credential's figures in one boundsLocked pass: what its
// bodies arriving hold in all, what those the pass has passed hold, and the
// least room any of those leaves the bodies after it.
type credentialRun struct {
	total, passed, least int64
}

// newSpools is an allowance of sizes z, which must pass check: sizes that do
// not are a body that could wait on nothing but itself, so they panic.
func newSpools(z spoolSizes) *spools {
	if err := z.check(); err != nil {
		panic(err)
	}
	return &spools{spoolSizes: z, bytes: map[string]int64{}, placed: map[string]int{},
		runs: map[string]credentialRun{}}
}

// spoolWaiter is one request waiting: for room for need bytes of its
// credential's share and of the node's allowance, and as much as want — and,
// for a body yet to start (body nil), a claim of claim bytes.
type spoolWaiter struct {
	credential string
	need, want int64
	claim      int64
	// body is the body arriving it asks room for; nil for one yet to start.
	body *spoolBody
	// ready is closed, and got set, when its room is taken for it — and, for
	// a body yet to start, started set to the body it began.
	ready   chan struct{}
	got     int64
	started *spoolBody
	// placed: it holds a place in the waiting room.
	placed bool
}

// spoolTurn is what a request was granted: a body, got bytes of room, and
// whether it holds a place still to be given back (unplace) once its head is in
// its spool.
type spoolTurn struct {
	body   *spoolBody
	got    int64
	placed bool
}

// start begins a body for credential that claims claim bytes, with room for
// need bytes and up to want: at once when grantLocked allows it, otherwise in
// its turn, holding a place until it is granted, wait passes (a *spoolUnserved,
// spoolExpired), or ctx ends (ctx's error). One that must wait and finds no
// place is refused (a *spoolUnserved, spoolRefused). need past claim is
// errPastClaim, and claim past request pastCeiling.
//
// waiting hands the request's head to its place: called once, on the caller's
// goroutine, under a.mu in the step that places it, so no reader counts a
// waiter still charged to the request memory. It may take only a leaf lock
// (heads') and never call into a.
func (a *spools) start(ctx context.Context, credential string, claim, need, want int64, wait time.Duration,
	waiting func()) (spoolTurn, spoolWait, error) {

	switch {
	case claim > a.request:
		return spoolTurn{}, spoolWait{}, a.pastCeiling()
	case need > claim:
		return spoolTurn{}, spoolWait{}, errPastClaim
	}
	w := &spoolWaiter{credential: credential, need: need, want: min(max(want, need), claim), claim: claim,
		ready: make(chan struct{})}
	a.mu.Lock()
	// THE QUEUE IS ASKED FIRST, before a place: a request that can start now
	// starts now, and is never refused for others' waiters.
	a.queue = append(a.queue, w)
	a.grantLocked()
	if w.got > 0 {
		a.mu.Unlock()
		return spoolTurn{body: w.started, got: w.got}, spoolWait{}, nil
	}
	if err := a.placeLocked(w); err != nil {
		a.removeLocked(w)
		a.mu.Unlock()
		return spoolTurn{}, spoolWait{outcome: spoolRefused}, err
	}
	if waiting != nil {
		waiting()
	}
	a.mu.Unlock()
	got, sw, err := a.await(ctx, w, wait)
	if err != nil {
		return spoolTurn{}, sw, err
	}
	return spoolTurn{body: w.started, got: got, placed: true}, sw, nil
}

// more takes room for need more bytes, and up to want, for body b, arriving:
// at once, or in its turn behind the older bodies waiting and ahead of the
// younger and of every request yet to start, holding no place. need past what
// b's claim leaves is errPastClaim.
func (a *spools) more(ctx context.Context, b *spoolBody, need, want int64, wait time.Duration) (int64,
	spoolWait, error) {

	a.mu.Lock()
	left := b.claim - b.held
	if need > left {
		a.mu.Unlock()
		return 0, spoolWait{}, errPastClaim
	}
	w := &spoolWaiter{credential: b.credential, need: need, want: min(max(want, need), left), body: b,
		ready: make(chan struct{})}
	at := 0
	for at < a.rooms && a.queue[at].body.seq < b.seq {
		at++
	}
	a.queue = slices.Insert(a.queue, at, w)
	a.rooms++
	a.grantLocked()
	if w.got > 0 {
		a.mu.Unlock()
		return w.got, spoolWait{}, nil
	}
	a.mu.Unlock()
	return a.await(ctx, w, wait)
}

// await waits for w, queued, to be granted, until ctx ends or wait passes
// (Timing.SpoolWait: a balancer's idle timeout for a quiet git request less a
// margin, so the 503 says why before the balancer cuts with no word). The
// bound holds whatever holds the room, even a credential's ENDED spools in a
// cold sync (SpoolCredentialShare).
//
// Over HTTP/1.1 ctx does not end when a client hangs up: net/http learns it
// only by reading, and a waiter reads nothing (its body is not to be held
// anywhere until granted). So a gone client costs its head and its place,
// never room, until its turn, where the read fails at once, or its bound.
func (a *spools) await(ctx context.Context, w *spoolWaiter, wait time.Duration) (int64, spoolWait, error) {
	if a.armed != nil {
		a.armed(wait)
	}
	start := time.Now()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	outcome := spoolExpired
	select {
	case <-w.ready:
		return w.got, spoolWait{outcome: spoolGranted, took: time.Since(start)}, nil
	case <-timer.C:
	case <-ctx.Done():
		outcome = spoolAbandoned
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	took := time.Since(start)
	if w.got > 0 {
		// GRANTED AS ITS WAIT ENDED: the room is the caller's, and goes
		// back with its body however the request goes on.
		return w.got, spoolWait{outcome: spoolGranted, took: took}, nil
	}
	behind := a.behindLocked(w)
	a.removeLocked(w)
	if w.placed {
		a.unplaceLocked(w.credential)
	}
	// A request that leaves may be the one the others waited behind.
	a.grantLocked()
	if outcome == spoolAbandoned {
		return 0, spoolWait{outcome: outcome, took: took}, ctx.Err()
	}
	return 0, spoolWait{outcome: outcome, took: took}, &spoolUnserved{reason: spoolExpired,
		detail: "this fetch waited " + waited(took) + " " + behind}
}

// placeLocked gives w a place in the waiting room, or refuses it one: the room
// full (SpoolWaiters), or its credential's quarter of it. A place holds the
// request's head, never the shared request memory (RequestMemoryAllowance's
// reason).
func (a *spools) placeLocked(w *spoolWaiter) error {
	c := w.credential
	switch {
	case a.placed[c] >= a.placesPerCredential:
		return &spoolUnserved{reason: spoolRefused, detail: fmt.Sprintf("this credential already has %d "+
			"fetches waiting to spool, as many as one credential's may; %s", a.placed[c], ownRemedy(c))}
	case a.inPlaces >= a.places:
		return &spoolUnserved{reason: spoolRefused, detail: fmt.Sprintf("%d fetches already wait to spool, "+
			"as many as this node keeps waiting", a.inPlaces)}
	}
	w.placed = true
	a.placed[c]++
	a.inPlaces++
	return nil
}

// unplace gives back one of credential's places, its request's head now in
// its spool or the request gone. A place gates nobody's grant, so nothing is
// granted for it.
func (a *spools) unplace(credential string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unplaceLocked(credential)
}

func (a *spools) unplaceLocked(credential string) {
	if a.placed[credential]--; a.placed[credential] <= 0 {
		delete(a.placed, credential)
	}
	a.inPlaces--
}

// boundsLocked works out the room a grant may take while every body arriving
// stays able to finish, oldest first: a banker's condition, without which two
// bodies could each hold part of the room and wait on each other. Body k
// leaves the bodies after it limit less its claim and what every younger body
// holds (free, plus what older bodies hold, less k's claim not yet taken), and
// the same of its credential's bodies against share; no grant takes either
// below zero. It sets each body's roomNode and roomCredential (the least any
// older body leaves) and each credential's start room (startRoomLocked), and
// returns the node's room for a body yet to start (the least any body leaves).
func (a *spools) boundsLocked() int64 {
	clear(a.runs)
	var arriving int64
	for _, b := range a.bodies {
		arriving += b.held
		r, ok := a.runs[b.credential]
		if !ok {
			r.least = unbounded
		}
		r.total += b.held
		a.runs[b.credential] = r
	}
	free := a.limit - arriving
	var passed int64
	least := int64(unbounded)
	for _, b := range a.bodies {
		r := a.runs[b.credential]
		b.roomNode, b.roomCredential = least, r.least
		left := b.claim - b.held
		least = min(least, free+passed-left)
		r.least = min(r.least, a.share-r.total+r.passed-left)
		passed += b.held
		r.passed += b.held
		a.runs[b.credential] = r
	}
	return least
}

// startRoomLocked is the room a body of credential's yet to start may take
// from its credential, as the last boundsLocked pass worked it out: the least
// any of its bodies arriving leaves, and no bound for a credential with none.
func (a *spools) startRoomLocked(credential string) int64 {
	if r, ok := a.runs[credential]; ok {
		return r.least
	}
	return unbounded
}

// olderLocked is how many of the bodies arriving older than w — every one, for
// a request yet to start — are credential's (any credential's for ""), and the
// room their claims may yet take.
func (a *spools) olderLocked(w *spoolWaiter, credential string) (int, int64) {
	n, left := 0, int64(0)
	for _, b := range a.bodies {
		if w.body != nil && b.seq >= w.body.seq {
			break
		}
		if credential == "" || b.credential == credential {
			n++
			left += b.claim - b.held
		}
	}
	return n, left
}

// behindLocked says what a waiting request is held back by: its credential's
// own spools or bodies, or the node's.
func (a *spools) behindLocked(w *spoolWaiter) string {
	c := w.credential
	nodeRoom := a.boundsLocked()
	credentialRoom := a.startRoomLocked(c)
	if w.body != nil {
		nodeRoom, credentialRoom = w.body.roomNode, w.body.roomCredential
	}
	switch {
	case a.bytes[c]+w.need > a.share:
		return fmt.Sprintf("behind this credential's own spooled requests (they hold %d of the %d bytes "+
			"one credential's may); %s", a.bytes[c], a.share, ownRemedy(c))
	case credentialRoom < w.need:
		n, left := a.olderLocked(w, c)
		return fmt.Sprintf("behind this credential's own requests still arriving (%d of its fetches, which "+
			"may yet need %d of the %d bytes one credential's may); %s", n, left, a.share, ownRemedy(c))
	}
	for _, o := range a.queue {
		if o == w {
			break
		}
		if o.credential == c {
			return "behind this credential's own requests waiting before it; " + ownRemedy(c)
		}
	}
	if nodeRoom < w.need {
		n, left := a.olderLocked(w, "")
		return fmt.Sprintf("behind the node's other requests still arriving (%d fetches, which may yet need "+
			"%d of its %d bytes)", n, left, a.limit)
	}
	return fmt.Sprintf("behind the node's other requests (%d of its %d bytes held)", a.used, a.limit)
}

// grantLocked grants every waiting request whose turn has come, in the queue's
// order: bodies arriving first, oldest first, then requests yet to start in
// arrival order. One its own credential holds back is passed over and holds
// back that credential's later requests, and nobody else's; one the node holds
// back holds back every later request, so none is starved by a stream of
// others. A body waits only for an ended spool's room or an older body's end,
// which go whatever waits behind it. Each grant keeps every body arriving able
// to finish (boundsLocked), so the bounds are worked out again after it.
func (a *spools) grantLocked() {
	if len(a.queue) == 0 {
		return
	}
	startNode := a.boundsLocked()
	var blocked map[string]bool
	block := func(c string) {
		if blocked == nil {
			blocked = map[string]bool{}
		}
		blocked[c] = true
	}
	roomOpen := true
	kept, rooms := a.queue[:0], 0
	for _, w := range a.queue {
		c := w.credential
		credit, room := a.share-a.bytes[c], a.limit-a.used
		nodeRoom, credentialRoom := startNode, a.startRoomLocked(c)
		if w.body != nil {
			nodeRoom, credentialRoom = w.body.roomNode, w.body.roomCredential
		}
		switch {
		case blocked[c]:
		case credit < w.need || credentialRoom < w.need:
			block(c)
		case !roomOpen || room < w.need || nodeRoom < w.need:
			roomOpen = false
		default:
			a.takeLocked(w, min(w.want, credit, room, nodeRoom, credentialRoom))
			close(w.ready)
			startNode = a.boundsLocked()
			continue
		}
		kept = append(kept, w)
		if w.body != nil {
			rooms++
		}
	}
	clear(a.queue[len(kept):])
	a.queue, a.rooms = kept, rooms
}

// takeLocked gives w n bytes of room — to its body, or to the body it starts,
// the youngest arriving.
func (a *spools) takeLocked(w *spoolWaiter, n int64) {
	a.used += n
	a.bytes[w.credential] += n
	b := w.body
	if b == nil {
		a.seq++
		b = &spoolBody{credential: w.credential, seq: a.seq, claim: w.claim, arriving: true}
		a.bodies = append(a.bodies, b)
		a.open++
		w.started = b
	}
	b.held += n
	w.got = n
}

// removeLocked takes w out of the queue unserved.
func (a *spools) removeLocked(w *spoolWaiter) {
	if i := slices.Index(a.queue, w); i >= 0 {
		a.queue = slices.Delete(a.queue, i, i+1)
		if w.body != nil {
			a.rooms--
		}
	}
}

// ended is body b arriving at its end, size bytes spooled: it gives back the
// room it took and did not write and leaves the bodies arriving, keeping size
// bytes until release, however long its sync and answer take.
func (a *spools) ended(b *spoolBody, size int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.giveLocked(b.credential, b.held-size)
	b.held = size
	a.leaveLocked(b)
	a.grantLocked()
}

// release gives back spool b: the bytes it holds, and its place among the
// bodies arriving when it was still arriving.
func (a *spools) release(b *spoolBody) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.giveLocked(b.credential, b.held)
	b.held = 0
	if b.arriving {
		a.leaveLocked(b)
	}
	a.open--
	a.grantLocked()
}

func (a *spools) giveLocked(credential string, n int64) {
	a.used -= n
	if a.bytes[credential] -= n; a.bytes[credential] <= 0 {
		delete(a.bytes, credential)
	}
}

// leaveLocked takes b, arriving, out of the bodies arriving.
func (a *spools) leaveLocked(b *spoolBody) {
	b.arriving = false
	if i := slices.Index(a.bodies, b); i >= 0 {
		a.bodies = slices.Delete(a.bodies, i, i+1)
	}
}

// inUse is the bytes held right now and the spools open.
func (a *spools) inUse() (int64, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.used, a.open
}

// arrivingNow is how many bodies are arriving right now.
func (a *spools) arrivingNow() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.bodies)
}

// placesHeld is how many places the waiting room has given out right now.
func (a *spools) placesHeld() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inPlaces
}

// waiters is how many requests wait right now.
func (a *spools) waiters() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queue)
}

// ask is the room a body that holds held bytes and claims claim asks for when
// it needs need more: up to a grain past what it holds, never past its claim,
// and never less than it needs.
func (z spoolSizes) ask(held, need, claim int64) int64 {
	return max(min(held+z.grain, claim)-held, need)
}

// waited is how long a request waited, as a person reads it: whole seconds
// past one.
func waited(d time.Duration) string {
	if d >= time.Second {
		return seconds(d)
	}
	return d.Round(time.Millisecond).String()
}

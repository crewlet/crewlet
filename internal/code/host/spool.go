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

// THE SPOOL ALLOWANCE, and the turn a request waits for its room in it.
//
// A fetch request past its in-memory part is spooled (body.go) — the whole of
// it, the part first read into memory included — and every spool draws on ONE
// node-wide allowance of bytes. Never the cache's budget: a spool has no name
// the cache counts (cache.Spool), and counted there one reader's large request
// would make the budget evict warm repositories.
//
//   - BYTES ARE TAKEN AS THEY ARRIVE, a grain at a time (spoolSizes.grain), and
//     what a body took and did not write goes back the moment it ends: the
//     bytes every spool holds at once, against limit (SpoolAllowance), and each
//     credential's, against share (SpoolCredentialBytes). A spool holds what
//     its client sent, never what its request might have sent: reserved by its
//     repository's bound — fifty bytes an object, where a checkout's lazy fetch
//     names fifty bytes a file it lacks — a fetch of a few megabytes to a
//     repository of millions of objects held a whole share while its body
//     arrived, so one credential's requests to it started one at a time and the
//     node's four at a time, behind any client sending slowly.
//   - A BODY ARRIVING HAS A CLAIM: the most it can grow to — the most one
//     request spools (request, SpoolRequestBytes), or the length its client
//     declared when it sent the body unencoded, at which net/http ends it
//     exactly. Never its repository's bound: a bound read from a stale row is
//     read again, fresher, when a body passes it, and a claim may never grow
//     while its body arrives (below). git declares no length for a lazy
//     fetch the host spools — one past its http.postBuffer goes chunked, a
//     smaller one compressed — so request is what such a fetch claims.
//
// THE BODIES ARRIVING ARE KEPT ABLE TO FINISH, OLDEST FIRST. Bytes taken as
// they arrive let two bodies each hold part of the room and wait for the
// other's, and neither gives any back, having not ended. So every grant is held
// to ONE condition, a banker's: in the order the bodies arriving STARTED, each
// body's claim fits in the room it would find were every spool that ended
// given back and every body older than it finished and given back too — for
// each body k, its claim and everything every body YOUNGER than k holds are no
// more than limit, and the same of each credential's bodies against share. A
// grant that keeps it is made; one that would break it waits. So:
//
//   - The OLDEST body's claim is room the rest may never take. It reaches its
//     end once the spools that ended are given back — and those wait for
//     nothing here (a sync, a transfer slot, upload-pack's read of the spool),
//     so they go whatever the waiters do — and once it has ended the next is
//     the oldest. A body waits, then, for an ended spool to go or for a body
//     OLDER than itself to end, and never for one that waits on it.
//   - A grant to a body touches only the conditions of the bodies older than it
//     (what is younger than them grows) and a new body only those of every body
//     arriving; a body that ends only loosens them. So each grant is checked in
//     one pass over the bodies arriving (boundsLocked), and a claim never grows:
//     a body waiting for a larger claim would wait for the bodies younger than
//     it to end, which wait for its room.
//   - What a body holds back is ROOM, never a turn. A client sending at the pace
//     floor (objstore.MiBPace, half a minute a mebibyte, a request of the
//     largest size half an hour) holds what it has sent and keeps its claim
//     free of the bodies behind it — and a body behind it waits for it only once
//     the bodies arriving hold all but that claim of the allowance, or of their
//     credential's share: 960 MiB of a four-core node's gibibyte and 192 MiB of
//     a credential's 256, a request's 64 MiB short of each. Counted as a whole
//     request instead, as many bodies as the lane is wide — sixteen slow
//     uploads on a four-core node, from any four tokens — held every turn the
//     node had, and every other large fetch on it waited out its bound. What
//     stays is the cost of what the uploads have SENT: bytes on the cache's
//     volume, as long as they take to arrive.
//
// What the claim costs is the largest request this node spools, request:
// SpoolPerTransfer, some 1.3 million wants — a blobless checkout of a tree that
// size, past the largest public monorepos' working trees — on every node of
// four transfer slots or more, and a quarter of the allowance on a smaller one.
// Past it a request is refused 413 at once (pastCeiling): the node can never
// take it, and waiting would not change that. It is held below the share for
// the reason the claim is the condition's: a body claiming a whole share
// leaves its credential no room for a second body while it arrives.
//
// A REQUEST THAT FINDS NO ROOM WAITS ITS TURN, and is refused only when its
// wait ends unserved. The share stops one credential HOLDING the node's spool,
// which is right; refusing the request that arrives past it turned a fairness
// bound into a failure, because git does not retry a lazy fetch: the fifth
// blobless checkout at once behind one CI token — the ordinary way to give CI
// access, on a four-core node — failed its checkout under no abuse at all. The
// turns are kept in ONE queue:
//
//   - A BODY ARRIVING IS SERVED BEFORE ANY REQUEST YET TO START, and the bodies
//     waiting in the order they started, oldest first: what they wait for is
//     an ended spool's room or an older body's end.
//   - A REQUEST THAT CAN START NOW STARTS NOW: the queue is asked first, and only
//     a request it cannot serve takes a place to wait in — so a request is never
//     turned away for what other requests' waiters hold back on themselves.
//   - FIRST COME FIRST SERVED WITHIN A CREDENTIAL: a credential's requests never
//     overtake each other, so a request that arrives while its credential has
//     requests waiting waits behind them, room or no room.
//   - THE NODE'S ROOM IN ARRIVAL ORDER ACROSS CREDENTIALS: once a request waits
//     for the NODE's, nothing behind it is granted room first, so a request is
//     never starved by a stream of others.
//   - A CREDENTIAL AT ITS SHARE HOLDS BACK ITS OWN REQUESTS AND NOBODY ELSE'S: a
//     request its own credential's spools or bodies keep waiting is passed over,
//     and a request of another credential behind it that fits is granted.
//
// A REQUEST WAITING TO START HOLDS ITS HEAD AND NO MORE: what it read into
// memory — up to RequestMemoryBytes — in a PLACE of the node's waiting
// room (SpoolWaiters), never in the request memory every request shares
// (RequestMemoryAllowance): charged there, a queue behind one token spent it,
// and every other fetch on the node — an ls-refs of a few hundred bytes — found
// it spent and went to the spool and its queue too. Once started, the head is
// written into the spool beside the rest and its memory let go, so a request
// waiting through its sync holds disk and no memory at all. A body arriving
// that waits for room holds the room it took and a piece of its body, and no
// place.
//
// A WAIT IS BOUNDED (Timing.SpoolWait): by the request's own context, and by
// what keeps its connection alive while nothing has been written to it — the
// idle timeout a balancer gives a quiet git request — less a margin, so its 503
// reaches the client before the balancer's own cut, which git reports with no
// word of why. It bounds every wait, whatever holds the room: a request behind
// its own credential's spools that ENDED waits as long as they take to be
// given back — their syncs, their transfer slots, upload-pack's read — and a
// cold sync of a minute or two outlasts it, so one token whose spooled
// requests fill its share has every request behind them answered 503 when
// they are slower than that — N lazy fetches of B bytes once N × B passes the
// share, the fourteenth of 20 MB on a four-core node's 256 MiB
// (TestACredentialsEndedSpoolsOutlastingTheWait). A token per runner moves
// that to the node's whole room; the wait cannot outlast the balancer's cut.
// And THE WAITING ROOM IS BOUNDED, by the memory its places hold: SpoolWaiters
// requests waiting to start on the node, and SpoolWaitersPerCredential of one
// credential's, a quarter of the room as the share is a quarter of the bytes.
// Past either a request that must wait is answered 503 at once — so a token
// with more large fetches waiting at once than a lane's width has the excess
// refused, and a token per CI runner ends that.
//
// A WAITER WHOSE CLIENT HAS GONE gives up its place as soon as the request's
// context ends: at once over HTTP/2, whose client resets the stream, and when
// the host stops. Over HTTP/1.1 — what the engine's API listener speaks —
// net/http learns that a client hung up only by reading its connection, and a
// waiter reads nothing: the rest of its body is the client's to send once it
// is granted, and read now it would have to be held somewhere. So such a
// waiter keeps its place until its turn, when the read fails at once and the
// room goes straight back, or until its bound — its head and its place, and
// never the room, are what a client that left costs.

// spoolGrain is the most room a body arriving takes at a time in production
// (spoolSizes.grain): a mebibyte, so the allowance's lock is taken once a
// mebibyte of body rather than once a piece (spoolPiece), and a body holds at
// most a mebibyte it has not written — given back at its end.
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

// errPastClaim is a body asking for room past its claim — the length its
// client declared, which net/http ends an unencoded body at, or the most one
// request spools, which readRequest refuses past first. Answered as a body
// that could not be read (400), and never granted: room past a claim is room
// the bodies behind it were told was theirs.
var errPastClaim = errors.New("host: the request's body is longer than its client declared")

// tokenPerRunner is what a request its own credential held back is told to
// do about it — a person's or a token's ([ownRemedy]).
const tokenPerRunner = "give each CI runner a token of its own"

// boxesShareOne is what a CODING BOX'S request its credential held back is
// told instead: every box on a node is the one spool credential [boxSpool],
// so no token of its own would help, and what is left to a box is to wait
// for the boxes' fetches ahead of it, or ask for less at once.
const boxesShareOne = "every coding box on this node shares one credential's room: retry once the boxes' " +
	"fetches ahead of this one end, or fetch fewer objects at once"

// ownRemedy is what a request credential held back by its own credential's
// requests is told to do about it: [boxesShareOne] for the boxes' one
// credential, [tokenPerRunner] for any other.
func ownRemedy(credential string) string {
	if credential == boxSpool {
		return boxesShareOne
	}
	return tokenPerRunner
}

// spoolSizes are a spool allowance's bounds (SpoolSizesFor in production).
type spoolSizes struct {
	// limit and share are the bytes every spool, and each credential's,
	// may hold at once; request is the most one request spools — the claim
	// of every body whose client did not declare its length.
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

// check reports why z cannot be an allowance: a body of the largest claim could
// never be sure of its room — request past the node's limit or a credential's
// share, where even the oldest body could wait on nothing but itself — or
// nothing could ever arrive.
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

// pastCeiling is a request past what this node spools for one request, which
// it can never take however idle — refused at once, never made to wait.
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
	// queue is every request waiting: first the bodies arriving that wait
	// for room (rooms of them), oldest first, then the requests waiting to
	// start, in arrival order.
	queue []*spoolWaiter
	rooms int
	// runs is boundsLocked's per-credential working, kept to be cleared
	// rather than made again on every pass.
	runs map[string]credentialRun
}

// spoolBody is one spool the allowance has granted, from its start until it is
// given back: while arriving, in spools.bodies with its claim; once ended,
// only its bytes.
type spoolBody struct {
	credential string
	seq        uint64
	// claim is the most it can grow to; held the room it has taken, written
	// or not — what it spooled, once it has ended.
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
// need bytes and up to want — at once when that keeps every body arriving able
// to finish and no request waits that it may not overtake, and otherwise in
// its turn: it takes a place to wait in (waiting is called, on the caller's
// goroutine, once it has one), and waits until it is granted, its wait reaches
// wait (a *spoolUnserved, spoolExpired), or ctx ends (ctx's error). A request
// that must wait and finds no place is refused one (a *spoolUnserved,
// spoolRefused). need past claim is errPastClaim, and claim past request
// pastCeiling.
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
	// THE QUEUE IS ASKED FIRST: a request that can start now starts now,
	// whatever waits ahead of it held back by its own credential.
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
	a.mu.Unlock()
	if waiting != nil {
		waiting()
	}
	got, sw, err := a.await(ctx, w, wait)
	if err != nil {
		return spoolTurn{}, sw, err
	}
	return spoolTurn{body: w.started, got: got, placed: true}, sw, nil
}

// more takes room for need more bytes, and up to want, for body b, arriving:
// at once, or waiting its turn — behind the older bodies already waiting and
// ahead of the younger ones and of every request yet to start — as start
// waits, holding no place. need past what b's claim leaves is errPastClaim.
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

// await waits for w, queued, to be granted.
func (a *spools) await(ctx context.Context, w *spoolWaiter, wait time.Duration) (int64, spoolWait, error) {
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

// placeLocked gives w a place in the waiting room, or refuses it one: the
// room full, or its credential's quarter of it.
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

// boundsLocked works out the room a grant may take without leaving the claim
// of any body arriving short: for each body, the room a grant to IT may take —
// the least any body older than it leaves (spoolBody.roomNode, roomCredential)
// — and the room a body yet to start may take, the least any body arriving
// leaves: from the node, returned, and from each credential (startRoomLocked,
// read before the next pass).
//
// What body k leaves the bodies after it is the room it would find were every
// spool that ended given back and every body older than it finished and given
// back, less what k may yet take: limit, less its claim and what every body
// younger than it holds — free (the allowance less what every body arriving
// holds), plus what the bodies older than k hold, less k's claim not yet taken.
// And the same of its credential's bodies against share. None of it is ever
// below zero: that is the condition every grant keeps.
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

// grantLocked grants every waiting request whose turn has come, in the
// queue's order: a request its own credential holds back is passed over, and
// holds back every later request of that credential; a request the node holds
// back holds back every later request. A grant takes what keeps every body
// arriving able to finish (boundsLocked), so the pass works the bounds out
// again after each one.
//
// The bodies waiting for room come first, oldest first, and a body the node
// holds back holds back every request after it: what it waits for is an ended
// spool's room or an older body's end, which go whatever waits behind it.
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
// room it took and did not write, and leaves the bodies arriving — keeping the
// size bytes until the spool is given back (release), however long its sync
// and its answer take.
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

package jetstream

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sockcount"
)

// meterPeriod is how often a [LinkMeter] reads its link's sockets: a quarter
// of [jsprovision.ReadTerm], the same quarter a pull's heartbeat is of the
// quiet wait it serves. The shortest wait the meter answers for is a request's
// term ([jsapi.RequestTerm], five seconds), so a reading at most this stale
// misplaces its window by five per cent of it at most; one read of each
// socket's TCP_INFO four times a second costs nothing a node can measure.
// Never a knob: there is nothing for an operator to trade.
const meterPeriod = jsprovision.ReadTerm / 4

// meterKeeps is how many readings a [LinkMeter] keeps: [objstore.Stall] of
// them, the longest silence any wait across a link is held to, so every such
// wait can ask about its whole span — 240 at a reading every 250 ms.
const meterKeeps = int(objstore.Stall / meterPeriod)

// movingShare is the share of the floor a link must carry, in at least one
// direction, to be MOVING ([LinkMeter.Moving]): seven tenths, in the bytes
// the socket counts — what crosses the wire, compression and protocol
// framing included.
//
// MIDWAY BETWEEN WHAT A QUIET LINK CARRIES AND WHAT A BUSY ONE DOES. A link
// with no bulk on it still carries its background: the presence reads of a
// fleet, which the documented fleets budget at up to 40% of the floor
// (docs/guides/satellite-nodes.md), event deliveries, watches, pings and
// heartbeats. A link draining bulk carries at least the floor beside that
// background, which is what the floor promises. 70% is the midpoint of 40%
// and 100%: a link carrying its background alone never reads as moving, so
// a wait that hears nothing on it ends for silence as it would on a dead one,
// and a link draining bulk at the floor reads as moving with as much margin
// below it as the background has above, which a reading's 250 ms staleness
// and TCP's acknowledgements arriving in bursts do not cross.
const movingShare = 0.7

// LinkMeter reads what a node's data link carries each way, at the link's own
// sockets: the bytes the node read from them, and the bytes the far end
// acknowledged of what the node wrote. See the package doc's "What a link
// carries is read at its socket".
//
// One meter a link, nil on a member, which crosses none ([Queue.LinkMeter]).
// On a leaf it meters the tunnel's upstream socket ([tunnel]); on an external
// broker every connection the queue dials, reconnects included ([dial]). Its
// readings are kept for [objstore.Stall] ([meterKeeps]), taken every
// [meterPeriod] from the moment it is made until [LinkMeter.close].
type LinkMeter struct {
	// floor is what the slowest link the engine allows carries in a second:
	// a mebibyte in [objstore.MiBPace]. A field so this package's own tests
	// can play the floor at a test link's rate.
	floor float64

	// read is every byte read from the link's sockets, counted as it is
	// read.
	read atomic.Uint64

	mu sync.Mutex
	// conns is every socket the link holds open.
	conns map[*meteredConn]struct{}
	// retired is what the far end acknowledged of the sockets that have
	// closed, at their last reading.
	retired uint64
	// opened is how many sockets the link has opened: a link that dialled
	// again shows here.
	opened int
	// readings is a ring of the last [meterKeeps] readings, next the slot
	// the next one takes, and taken how many it has taken in all.
	readings []reading
	next     int
	taken    uint64
	// sampled is closed at the next reading, and replaced by a fresh one:
	// how this package's tests wait for a reading by its count, never by a
	// clock.
	sampled chan struct{}

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// reading is one of a meter's readings: when it was taken, and the link's
// two counts then.
type reading struct {
	at          time.Time
	acked, read uint64
}

// newLinkMeter starts a meter of a link at the floor, reading its sockets
// every [meterPeriod] until it is closed.
func newLinkMeter() *LinkMeter {
	m := &LinkMeter{
		floor:    float64(1<<20) / objstore.MiBPace.Seconds(),
		conns:    map[*meteredConn]struct{}{},
		readings: make([]reading, meterKeeps),
		sampled:  make(chan struct{}),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	m.sample()
	go m.run()
	return m
}

// run takes a reading every [meterPeriod] until the meter is closed.
func (m *LinkMeter) run() {
	defer close(m.done)
	tick := time.NewTicker(meterPeriod)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
			m.sample()
		}
	}
}

// close stops the meter's readings and waits for its reader to end. From then
// on it answers [LinkMeter.Carried] with what it read last and the bytes read
// since, and [LinkMeter.Moving] that the link is not moving: a closed meter
// takes no readings, and a ring frozen while the link moved would otherwise
// hold every wait that reads it to its cap.
func (m *LinkMeter) close() {
	if m == nil {
		return
	}
	m.once.Do(func() { close(m.stop) })
	<-m.done
}

// sample takes one reading of the link.
func (m *LinkMeter) sample() {
	acked, read := m.Carried()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pushLocked(reading{at: time.Now(), acked: acked, read: read})
}

// pushLocked keeps r as the newest reading, and wakes whoever waits for one;
// m.mu held.
func (m *LinkMeter) pushLocked(r reading) {
	m.readings[m.next] = r
	m.next = (m.next + 1) % len(m.readings)
	m.taken++
	close(m.sampled)
	m.sampled = make(chan struct{})
}

// Carried is how many bytes the far end has acknowledged of what the node
// wrote to the link, and how many the node has read from it, since the meter
// was made: every socket the link has held, those closed since included.
//
// Read now, at each socket, rather than at the last reading.
func (m *LinkMeter) Carried() (acked, read uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acked = m.retired
	for c := range m.conns {
		acked += c.acked()
	}
	return acked, m.read.Load()
}

// Moving reports whether the link carried, in at least one direction, at
// least [movingShare] of what the floor carries over the last d — read off
// the meter's readings, the newest at most a [meterPeriod] old.
//
// A meter younger than d answers for the span it has read, and one that has
// read no span at all — made a moment ago — answers that it is not moving, as
// does a closed one ([LinkMeter.close]).
// A d past what the meter keeps ([objstore.Stall]) is answered over what it
// keeps.
func (m *LinkMeter) Moving(d time.Duration) bool {
	select {
	case <-m.done:
		return false
	default:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return movingOver(m.inOrderLocked(), d, m.floor)
}

// inOrderLocked is the meter's readings, oldest first; m.mu held.
func (m *LinkMeter) inOrderLocked() []reading {
	n := len(m.readings)
	if m.taken < uint64(n) {
		return m.readings[:m.taken]
	}
	return append(m.readings[m.next:n:n], m.readings[:m.next]...)
}

// movingOver is [LinkMeter.Moving] over readings, oldest first, of a link
// whose floor carries floor bytes a second: over the span from the newest
// reading back to the newest one at least d before it — or the oldest, where
// none is — whether either count grew by [movingShare] of what the floor
// carries in that span.
func movingOver(readings []reading, d time.Duration, floor float64) bool {
	if len(readings) < 2 {
		return false
	}
	newest := readings[len(readings)-1]
	from := readings[0]
	for i := len(readings) - 2; i >= 0; i-- {
		if newest.at.Sub(readings[i].at) >= d {
			from = readings[i]
			break
		}
	}
	span := newest.at.Sub(from.at)
	if span <= 0 {
		return false
	}
	line := movingShare * floor * span.Seconds()
	return float64(newest.acked-from.acked) >= line || float64(newest.read-from.read) >= line
}

// nextReading is closed at the meter's next reading after this call: how this
// package's tests wait for readings by their count.
func (m *LinkMeter) nextReading() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sampled
}

// wrap is c, a socket the link has just opened, counted by the meter from now
// until it closes.
func (m *LinkMeter) wrap(c net.Conn) net.Conn {
	if m == nil {
		return c
	}
	mc := &meteredConn{Conn: c, m: m}
	// THE BASELINE, before anything is written: the kernel's count of a
	// socket this end opened holds its SYN ([sockcount.Acked]).
	mc.base, _ = sockcount.Acked(c, 0)
	m.mu.Lock()
	m.conns[mc] = struct{}{}
	m.opened++
	m.mu.Unlock()
	return mc
}

// meteredConn is one socket of a metered link: every byte read from it is
// counted into its meter, and every byte written to it is counted here, for
// a platform whose kernel keeps no count of what the far end acknowledged
// ([sockcount.Acked]).
type meteredConn struct {
	net.Conn
	m       *LinkMeter
	written atomic.Uint64
	// base is the socket's count of acknowledged bytes when it was
	// wrapped, before anything was written to it ([sockcount.Acked]).
	base uint64
	// last is the largest count of acknowledged bytes read off the socket,
	// so the count never steps back, nor falls to nothing once the socket
	// is closed and cannot be read. Under m.m.mu.
	last uint64
	// closed is whether Close has retired the socket from its meter. Under
	// m.m.mu.
	closed bool
}

// Read counts what it read into the meter.
func (c *meteredConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.m.read.Add(uint64(n))
	}
	return n, err
}

// meterWriteChunk is the most of one Write a [meteredConn] hands its socket
// at a time: 64 KiB, the size of a socket's send buffer before the kernel
// autotunes it.
//
// WHY A WRITE IS SPLIT: where the kernel keeps no count of what the far end
// acknowledged (darwin, [sockcount.Acked]), the count is what was written
// less what the send buffer still holds, and the send buffer holds a Write's
// bytes while the Write is still in the socket. nats.go flushes a large
// message as ONE Write ([externalWriteDeadline]), so counted only once it
// returned, a message of 8 MiB crossing a link at the floor — four minutes —
// would read as nothing acknowledged for all of them, the very case the meter
// is for. Split, the count lags by one chunk at most. Each chunk is written
// under the deadline the whole Write was given, since a socket's deadline is
// an instant, not a span.
const meterWriteChunk = 64 << 10

// Write counts what it wrote, a chunk ([meterWriteChunk]) at a time.
func (c *meteredConn) Write(p []byte) (int, error) {
	var total int
	for len(p) > 0 {
		chunk := p[:min(len(p), meterWriteChunk)]
		n, err := c.Conn.Write(chunk)
		if n > 0 {
			c.written.Add(uint64(n))
			total += n
		}
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// NetConn is the socket under the count, for a reader of its kernel's counts
// ([sockcount]).
func (c *meteredConn) NetConn() net.Conn { return c.Conn }

// acked is what the far end has acknowledged of what was written to the
// socket, the largest reading yet: the last one where the socket cannot be
// read (closed under the reading). c.m.mu held.
func (c *meteredConn) acked() uint64 {
	if n, err := sockcount.Acked(c.Conn, c.written.Load()); err == nil && n > c.base && n-c.base > c.last {
		c.last = n - c.base
	}
	return c.last
}

// Close takes the socket's last count into its meter's, then closes it.
func (c *meteredConn) Close() error {
	c.m.mu.Lock()
	if !c.closed {
		c.closed = true
		c.m.retired += c.acked()
		delete(c.m.conns, c)
	}
	c.m.mu.Unlock()
	return c.Conn.Close()
}

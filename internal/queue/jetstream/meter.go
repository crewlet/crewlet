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
// of [jsprovision.ReadTerm], as a pull's heartbeat is of its quiet wait. The
// shortest wait the meter answers for is [jsapi.RequestTerm] (five seconds),
// so a reading this stale misplaces it by five per cent at most, and four
// TCP_INFO reads a second cost nothing measurable. Never a knob: there is
// nothing for an operator to trade.
const meterPeriod = jsprovision.ReadTerm / 4

// meterKeeps is how many readings a [LinkMeter] keeps: [objstore.Stall] of
// them, the longest silence any wait across a link is held to, so every such
// wait can ask about its whole span (240 at 250 ms).
const meterKeeps = int(objstore.Stall / meterPeriod)

// movingShare is the share of the floor a link must carry, in at least one
// direction, to be MOVING ([LinkMeter.Moving]), in the bytes the socket counts
// (compression and framing included).
//
// SEVEN TENTHS, midway between a quiet link's background (presence reads,
// budgeted at up to 40% of the floor in docs/guides/satellite-nodes.md, plus
// deliveries, watches, pings) and a link draining bulk at the floor. So the
// background alone never reads as moving, and a wait hearing nothing on it
// ends for silence as on a dead link, while bulk at the floor clears the line
// by a margin a reading's 250 ms staleness and bursty ACKs do not cross.
const movingShare = 0.7

// LinkMeter reads what a node's data link carries each way, at the link's own
// sockets: the bytes the node read, and the bytes the far end acknowledged of
// what it wrote (package doc, "What a link carries is read at its socket").
//
// One meter a link, nil on a member ([Queue.LinkMeter]): on a leaf the
// tunnel's upstream socket ([tunnel]), on an external broker every connection
// the queue dials ([dial]). It reads every [meterPeriod] until
// [LinkMeter.close], keeping [meterKeeps] readings.
type LinkMeter struct {
	// floor is bytes a second on the slowest link the engine allows (a MiB
	// per [objstore.MiBPace]); a field so tests can set a test link's rate.
	floor float64

	// read is every byte read from the link's sockets.
	read atomic.Uint64

	mu sync.Mutex
	// conns is every socket the link holds open.
	conns map[*meteredConn]struct{}
	// retired is what the far end acknowledged of closed sockets.
	retired uint64
	// opened counts sockets opened, so a redial shows.
	opened int
	// readings is a ring of the last [meterKeeps] readings, next the slot
	// the next one takes, and taken how many it has taken in all.
	readings []reading
	next     int
	taken    uint64
	// sampled is closed at the next reading and replaced: tests wait for a
	// reading by its count, never by a clock.
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
// on [LinkMeter.Moving] answers not moving: a ring frozen while the link moved
// would otherwise hold every wait that reads it to its cap.
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
// wrote to the link, and how many the node has read from it, over every
// socket the link has held since the meter was made. Read now, at each
// socket, not at the last reading.
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
// least [movingShare] of what the floor carries over the last d, read off
// readings the newest of which is at most a [meterPeriod] old. A meter younger
// than d, or a d past what it keeps, answers over what it has; one with no
// span yet, or closed ([LinkMeter.close]), is not moving.
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

// movingOver is [LinkMeter.Moving] over readings, oldest first, at floor bytes
// a second: the span runs from the newest reading back to the newest one at
// least d before it, or the oldest where none is.
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

// nextReading is closed at the meter's next reading after this call, for
// tests that wait for readings by their count.
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
	// The baseline, before any write: a dialled socket's kernel count holds
	// its SYN ([sockcount.Acked]).
	mc.base, _ = sockcount.Acked(c, 0)
	m.mu.Lock()
	m.conns[mc] = struct{}{}
	m.opened++
	m.mu.Unlock()
	return mc
}

// meteredConn is one socket of a metered link: bytes read are counted into its
// meter, and bytes written here, for a kernel that keeps no acknowledged count
// ([sockcount.Acked]).
type meteredConn struct {
	net.Conn
	m       *LinkMeter
	written atomic.Uint64
	// base is the socket's acknowledged count when wrapped.
	base uint64
	// last is the largest acknowledged count read, so the count never steps
	// back nor falls to nothing once the socket is closed. Under m.m.mu.
	last uint64
	// closed is whether Close has retired the socket. Under m.m.mu.
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
// at a time: 64 KiB, a socket's send buffer before the kernel autotunes it.
//
// SPLIT because where the kernel keeps no acknowledged count (darwin,
// [sockcount.Acked]) the count is written less what the send buffer holds, and
// a Write counts only once it returns: nats.go flushes a large message as ONE
// Write ([externalWriteDeadline]), so 8 MiB crossing at the floor would read
// as nothing acknowledged for four minutes. Split, the count lags one chunk at
// most. Each chunk keeps the whole Write's deadline, an instant, not a span.
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

// acked is what the far end has acknowledged of what was written, the largest
// reading yet (the last, once the socket cannot be read). c.m.mu held.
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

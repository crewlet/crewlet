package boxhelpertest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
)

// relayWithin bounds one wait on what a box's supervisor writes on
// relay.sock: a frame of a watched page comes within its slot, half a
// second, and a state as a change lands, so a minute is a hang, never a slow
// machine.
const relayWithin = time.Minute

// Record is one record a box's supervisor wrote on relay.sock: its type, its
// header as it came, and its payload, a frame's JPEG.
type Record struct {
	T       string
	Header  json.RawMessage
	Payload []byte
}

// Decode decodes the record's header into v, failing t on a header v does
// not hold.
func (r Record) Decode(t testing.TB, v any) {
	t.Helper()
	if err := json.Unmarshal(r.Header, v); err != nil {
		t.Fatalf("the supervisor's %s record %s: %v", r.T, r.Header, err)
	}
}

// RelaySock is a connection to a box's relay.sock as a job's relay holds
// one: a case writes the relay's records ([RelaySock.Send]) and reads the
// supervisor's, every one kept in the order it came, each type in its own
// order ([RelaySock.Next]).
type RelaySock struct {
	t    testing.TB
	conn net.Conn
	// wmu keeps each record's bytes together, as the reader acknowledges
	// frames beside a case's own writes.
	wmu     sync.Mutex
	ackEach atomic.Bool

	mu      sync.Mutex
	records []Record
	read    map[string]int
	arrived chan struct{}
	ended   chan struct{}
	err     error
}

// DialRelay connects to the box's relay.sock, as a job's relay does; t's end
// closes it.
func (b *Box) DialRelay(t testing.TB) *RelaySock {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", filepath.Join(b.Folder, "relay.sock"))
	if err != nil {
		t.Fatalf("dialling the box's relay.sock: %v", err)
	}
	return serveConn(t, conn)
}

// serveConn reads conn's records as [RelaySock] keeps them, either end of
// relay.sock; t's end closes it.
func serveConn(t testing.TB, conn net.Conn) *RelaySock {
	r := &RelaySock{t: t, conn: conn, read: map[string]int{}, arrived: make(chan struct{}, 1), ended: make(chan struct{})}
	t.Cleanup(func() {
		_ = conn.Close()
		<-r.ended
	})
	go r.readAll()
	return r
}

// lengthOf is a record's header length, as relay.sock frames it.
func lengthOf(header []byte) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(len(header)))
}

// readAll reads the supervisor's records until the connection ends, holding
// each to relay.sock's bounds: a header of at most live.MaxHeaderBytes, and
// a payload on a frame alone, of at most browser.MaxFrameBytes.
func (r *RelaySock) readAll() {
	defer close(r.ended)
	for {
		rec, err := readRecord(r.conn)
		if err != nil {
			r.mu.Lock()
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				r.err = err
			}
			r.mu.Unlock()
			r.signal()
			return
		}
		r.mu.Lock()
		r.records = append(r.records, rec)
		r.mu.Unlock()
		if rec.T == "frame" && r.ackEach.Load() {
			var f struct {
				Boot string `json:"boot"`
				Seq  int64  `json:"seq"`
			}
			if json.Unmarshal(rec.Header, &f) == nil {
				_ = r.write(map[string]any{"t": "frame_ack", "v": 1, "boot": f.Boot, "seq": f.Seq})
			}
		}
		r.signal()
	}
}

// AckEach acknowledges each frame as it arrives, as a relay whose every
// post is answered at once does.
func (r *RelaySock) AckEach() { r.ackEach.Store(true) }

// errFraming is a record the supervisor wrote past relay.sock's bounds.
var errFraming = errors.New("a record past relay.sock's bounds")

// readRecord reads one record from c.
func readRecord(c io.Reader) (Record, error) {
	var length [4]byte
	if _, err := io.ReadFull(c, length[:]); err != nil {
		return Record{}, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n < 2 || n > live.MaxHeaderBytes {
		return Record{}, errFraming
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(c, header); err != nil {
		return Record{}, err
	}
	var head struct {
		T     string `json:"t"`
		Bytes int    `json:"bytes"`
	}
	if err := json.Unmarshal(header, &head); err != nil {
		return Record{}, errFraming
	}
	rec := Record{T: head.T, Header: header}
	if head.T == "frame" {
		if head.Bytes < 1 || head.Bytes > browser.MaxFrameBytes {
			return Record{}, errFraming
		}
		rec.Payload = make([]byte, head.Bytes)
		if _, err := io.ReadFull(c, rec.Payload); err != nil {
			return Record{}, err
		}
	}
	return rec, nil
}

func (r *RelaySock) signal() {
	select {
	case r.arrived <- struct{}{}:
	default:
	}
}

// Send writes header as one record of the relay's.
func (r *RelaySock) Send(header map[string]any) {
	r.t.Helper()
	if err := r.write(header); err != nil {
		r.t.Fatalf("writing to relay.sock: %v", err)
	}
}

// SendRaw writes b to relay.sock as it is, for a case whose subject is a
// record the framing refuses.
func (r *RelaySock) SendRaw(b []byte) {
	r.t.Helper()
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if _, err := r.conn.Write(b); err != nil {
		r.t.Fatalf("writing to relay.sock: %v", err)
	}
}

// write writes header as one record, in one write.
func (r *RelaySock) write(header map[string]any) error {
	body, err := json.Marshal(header)
	if err != nil {
		return err
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	_, err = r.conn.Write(append(lengthOf(body), body...))
	return err
}

// Hello says the relay's hello for job, as a relay of helper says it, and
// answers the supervisor's answer, the first record it writes: its hello, or
// its refusal.
func (r *RelaySock) Hello(helper, job string) Record {
	r.t.Helper()
	r.Send(map[string]any{"t": "hello", "v": 1, "helper": helper, "job": job})
	rec := r.next("the answer to the hello", "", func(Record) bool { return true })
	if rec.T != "hello" && rec.T != "refused" {
		r.t.Fatalf("the supervisor answered the hello with %s", rec.Header)
	}
	return rec
}

// Watch sends the relay's watch.
func (r *RelaySock) Watch(on bool, fps int) {
	r.t.Helper()
	r.Send(map[string]any{"t": "watch", "v": 1, "on": on, "fps": fps})
}

// Ack sends the relay's frame_ack for a frame.
func (r *RelaySock) Ack(boot string, seq int64) {
	r.t.Helper()
	r.Send(map[string]any{"t": "frame_ack", "v": 1, "boot": boot, "seq": seq})
}

// Bye sends the relay's bye.
func (r *RelaySock) Bye() {
	r.t.Helper()
	r.Send(map[string]any{"t": "bye", "v": 1})
}

// Next is the first record of type kind after the last one of it Next
// answered that match holds, waiting for it within relayWithin, so records
// of other types between them are kept for their own: the case fails on
// none, on a connection that ends first, or on a record past relay.sock's
// bounds.
func (r *RelaySock) Next(kind, what string, match func(Record) bool) Record {
	r.t.Helper()
	return r.next(what, kind, func(rec Record) bool { return rec.T == kind && match(rec) })
}

// next is Next over the records under cursor's key, "" for every record.
func (r *RelaySock) next(what, cursor string, match func(Record) bool) Record {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), relayWithin)
	defer cancel()
	for {
		r.mu.Lock()
		for r.read[cursor] < len(r.records) {
			rec := r.records[r.read[cursor]]
			r.read[cursor]++
			if match(rec) {
				r.mu.Unlock()
				return rec
			}
		}
		err := r.err
		r.mu.Unlock()
		if err != nil {
			r.t.Fatalf("awaiting %s on relay.sock: %v", what, err)
		}
		select {
		case <-r.arrived:
		case <-r.ended:
			r.mu.Lock()
			more := r.read[cursor] < len(r.records)
			r.mu.Unlock()
			if !more {
				r.t.Fatalf("relay.sock closed before %s", what)
			}
		case <-ctx.Done():
			r.t.Fatalf("relay.sock carried no %s within %s", what, relayWithin)
		}
	}
}

// Records is every record the supervisor has written so far.
func (r *RelaySock) Records() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.records...)
}

// AwaitClosed waits for the supervisor to close the connection, failing the
// case on one still open after relayWithin.
func (r *RelaySock) AwaitClosed(what string) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(r.t.Context(), relayWithin)
	defer cancel()
	select {
	case <-r.ended:
	case <-ctx.Done():
		r.t.Fatalf("relay.sock is still open %s on: %s", relayWithin, what)
	}
}

// Close closes the relay's side of the connection.
func (r *RelaySock) Close() {
	_ = r.conn.Close()
}

// Hangup ends the relay's side of the connection and reads the
// supervisor's to its end, so every record it wrote before it closed is
// kept.
func (r *RelaySock) Hangup() {
	r.t.Helper()
	if err := r.conn.(*net.UnixConn).CloseWrite(); err != nil {
		r.t.Fatalf("ending the relay's side of relay.sock: %v", err)
	}
	r.AwaitClosed("the supervisor's side, once the relay's ended")
}

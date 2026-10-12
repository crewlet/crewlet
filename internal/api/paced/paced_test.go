package paced

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// stepClock is a pace clock that moves only when a case says time passed —
// a client's wait inside a read or a write, or the server's own work between
// two.
type stepClock struct{ at time.Time }

func newStepClock() *stepClock {
	return &stepClock{at: time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)}
}

func (c *stepClock) now() time.Time { return c.at }

func (c *stepClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// trickle is a body that hands over one byte a Read, each after a wait — a
// client sending as slowly as it likes.
type trickle struct {
	clock *stepClock
	wait  time.Duration
}

func (t trickle) Read(p []byte) (int, error) {
	t.clock.advance(t.wait)
	p[0] = 'x'
	return 1, nil
}

// prompt is a body that answers at once.
type prompt struct{}

func (prompt) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// A TRICKLE IS REFUSED ONCE ITS MEBIBYTE'S BUDGET IS SPENT INSIDE READS, and a
// prompt client whose server is slow between reads never is. A deadline
// re-armed whole at every Read would admit a byte every twenty-nine seconds
// for ever; one charged by the wall clock would refuse the second client.
// Shortened to a pace of a second, and timed on a clock the case moves itself.
func TestABodyChargesOnlyTheTimeInsideRead(t *testing.T) {
	t.Parallel()
	rc := http.NewResponseController(httptest.NewRecorder())
	clock := newStepClock()

	slow := &body{r: trickle{clock: clock, wait: 300 * time.Millisecond}, rc: rc,
		pace: time.Second, now: clock.now}
	buf := make([]byte, 64)
	var err error
	reads := 0
	for ; reads < 10 && err == nil; reads++ {
		_, err = slow.Read(buf)
	}
	if !errors.Is(err, ErrBody) || !strings.Contains(err.Error(), "took more than") {
		t.Fatalf("a trickle read for 10 rounds ended with %v, want it refused as the client's", err)
	}
	// Four waits of 300 ms spend the second; the fifth read finds it gone.
	if reads != 5 {
		t.Errorf("the trickle was refused on read %d, want the fifth: the first after "+
			"its budget ran out", reads)
	}

	waited := &body{r: prompt{}, rc: rc, pace: time.Second, now: clock.now}
	for range 4 {
		// THE SERVER'S OWN WORK between reads: more than the pace in
		// all, none of it the client's.
		clock.advance(400 * time.Millisecond)
		if _, err := waited.Read(buf); err != nil {
			t.Fatalf("a prompt client was refused for the server's own time: %v", err)
		}
	}

	// A MEBIBYTE THAT ARRIVES OPENS THE NEXT ONE'S WHOLE BUDGET: what was
	// left over is not carried, and what was spent is not either.
	whole := &body{r: io.LimitReader(prompt{}, 3<<20), rc: rc, pace: time.Hour, now: clock.now}
	whole.budget, whole.left = time.Millisecond, 10
	if _, err := whole.Read(make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	if whole.budget != time.Hour || whole.left != 1<<20-10 {
		t.Fatalf("after crossing into a new mebibyte: budget %v, %d left; want the "+
			"whole pace and the mebibyte less what crossed", whole.budget, whole.left)
	}
}

// client is a response writer standing for a client's connection: each write
// takes the client the next of takes on the case's clock, and each flush the
// next of flushTakes (none, once they run out); either one that would run
// past the deadline last set is cut AT it, with the error a net.Conn answers
// — so a deadline set here is one that bites.
//
// The flush has a time of its own because net/http's Write of a few bytes —
// a keepalive, a progress line — lands in a buffer, and it is the FLUSH that
// waits on the client's connection.
type client struct {
	clock      *stepClock
	takes      []time.Duration
	flushTakes []time.Duration

	deadline time.Time
	// out is how far past the moment it was set each deadline was, and
	// writes how many bytes each write that landed carried.
	out     []time.Duration
	writes  []int
	flushes int
}

func (c *client) Header() http.Header { return http.Header{} }

func (c *client) WriteHeader(int) {}

func (c *client) Write(p []byte) (int, error) {
	take := c.takes[0]
	c.takes = c.takes[1:]
	if err := c.take(take); err != nil {
		return 0, err
	}
	c.writes = append(c.writes, len(p))
	return len(p), nil
}

// FlushError is what http.ResponseController.Flush calls, in preference to
// Flush, on a writer that has it — as net/http's own response does.
func (c *client) FlushError() error {
	c.flushes++
	var take time.Duration
	if len(c.flushTakes) > 0 {
		take = c.flushTakes[0]
		c.flushTakes = c.flushTakes[1:]
	}
	return c.take(take)
}

// take spends d of the client's time on the case's clock, cut at the
// deadline last set.
func (c *client) take(d time.Duration) error {
	if !c.deadline.IsZero() && c.clock.at.Add(d).After(c.deadline) {
		c.clock.at = c.deadline
		return fmt.Errorf("write tcp 127.0.0.1:443: %w", os.ErrDeadlineExceeded)
	}
	c.clock.advance(d)
	return nil
}

func (c *client) SetWriteDeadline(t time.Time) error {
	c.deadline = t
	c.out = append(c.out, t.Sub(c.clock.at))
	return nil
}

// EVERY WRITE REACHES THE CLIENT WHEN IT IS WRITTEN. A git process's
// keepalive is a handful of bytes every five seconds; held in a buffer until
// a mebibyte had gathered, none would reach the client, and a balancer's idle
// timeout would cut a transfer that was merely working. An empty write
// flushes too, which is how a caller pushes the headers it wrote.
func TestAWriterFlushesEveryWrite(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := NewWriter(rec, time.Now)
	if rec.Flushed {
		t.Fatal("the recorder was flushed before anything was written")
	}
	if n, err := w.Write([]byte("0008NAK\n\n")); err != nil || n != 9 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if !rec.Flushed || rec.Body.String() != "0008NAK\n\n" {
		t.Fatalf("after a nine-byte write: flushed %v, body %q; want the write on its way",
			rec.Flushed, rec.Body.String())
	}

	headers := httptest.NewRecorder()
	headers.WriteHeader(http.StatusAccepted)
	if _, err := NewWriter(headers, time.Now).Write(nil); err != nil {
		t.Fatal(err)
	}
	if !headers.Flushed {
		t.Fatal("an empty write did not flush the headers written before it")
	}
}

// A WRITE IS CHARGED ONLY THE TIME THE CLIENT TAKES OVER IT, against its own
// mebibyte. A mebibyte the client takes in 29 seconds is within the pace; the
// server then works for ten minutes, none of it the client's; and the next
// mebibyte's 31 seconds is cut at the deadline the pace set, naming the pace.
// Within one mebibyte, a trickle of small writes spends one budget between
// them, and the write after it is spent is refused without a byte written.
func TestAWriterChargesOnlyTheTimeInsideWrite(t *testing.T) {
	t.Parallel()
	clock := newStepClock()
	conn := &client{clock: clock, takes: []time.Duration{29 * time.Second, 31 * time.Second}}
	w := NewWriter(conn, clock.now)

	if _, err := w.Write(make([]byte, objstore.MiB)); err != nil {
		t.Fatalf("a mebibyte taken in 29s was refused: %v", err)
	}
	clock.advance(10 * time.Minute)
	n, err := w.Write(make([]byte, objstore.MiB))
	if n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), "30s") {
		t.Fatalf("a mebibyte taking 31s wrote %d with %v; want it cut at its deadline, naming the 30s pace", n, err)
	}
	if want := []time.Duration{30 * time.Second, 30 * time.Second}; !slices.Equal(conn.out, want) {
		t.Errorf("deadlines set %v out, want %v: each mebibyte's whole pace, the "+
			"server's ten minutes charged to nobody", conn.out, want)
	}

	// A TRICKLE: ten writes of a kibibyte, three seconds each, with a
	// minute of the server's own work between every two.
	clock = newStepClock()
	slow := &client{clock: clock}
	for range 11 {
		slow.takes = append(slow.takes, 3*time.Second)
	}
	w = NewWriter(slow, clock.now)
	for i := range 10 {
		if _, err := w.Write(make([]byte, 1<<10)); err != nil {
			t.Fatalf("write %d of a trickle inside its budget: %v", i, err)
		}
		clock.advance(time.Minute)
	}
	n, err = w.Write(make([]byte, 1<<10))
	if n != 0 || err == nil || !strings.Contains(err.Error(), "30s") {
		t.Fatalf("the write after the budget was spent wrote %d with %v; want "+
			"it refused, nothing written, naming the pace", n, err)
	}
	if len(slow.writes) != 10 || len(slow.out) != 10 {
		t.Errorf("%d writes landed under %d deadlines; want ten of each — the "+
			"refused write sets none and writes nothing", len(slow.writes), len(slow.out))
	}
	for i, d := range slow.out {
		if want := 30*time.Second - time.Duration(i)*3*time.Second; d != want {
			t.Errorf("deadline %d was set %v out, want %v: what the writes before "+
				"it left of the budget, and nothing of the server's minutes", i, d, want)
		}
	}
}

// A FLUSH IS CHARGED AS THE WRITE IS. A git keepalive or a progress line is a
// few bytes, which net/http's Write puts in a buffer at once; what waits on a
// slow client is the flush that follows. Charged only for the write, every
// such write would re-arm nearly the whole pace, and a client taking 29 s to
// drain each keepalive would never be cut — the re-armed-whole failure the
// pace exists to prevent, on exactly the path the Writer was made for. And a
// flush the connection cut at its deadline is the client's pace spent, so it
// is answered naming the pace.
func TestAWriterChargesTheTimeInsideFlush(t *testing.T) {
	t.Parallel()
	clock := newStepClock()
	slow := &client{clock: clock}
	for range 11 {
		slow.takes = append(slow.takes, 0)
		slow.flushTakes = append(slow.flushTakes, 3*time.Second)
	}
	w := NewWriter(slow, clock.now)
	for i := range 10 {
		if _, err := w.Write([]byte("0005\x02.\n")); err != nil {
			t.Fatalf("keepalive %d inside its budget: %v", i, err)
		}
		// The server's own work between two keepalives — git counting
		// objects — charged to nobody.
		clock.advance(5 * time.Second)
	}
	n, err := w.Write([]byte("0005\x02.\n"))
	if n != 0 || err == nil || !strings.Contains(err.Error(), "30s") {
		t.Fatalf("the keepalive after ten 3 s flushes wrote %d with %v; want it "+
			"refused, nothing written, naming the pace", n, err)
	}
	if len(slow.writes) != 10 || slow.flushes != 10 {
		t.Errorf("%d writes and %d flushes, want ten of each: the refused write "+
			"neither writes nor flushes", len(slow.writes), slow.flushes)
	}
	for i, d := range slow.out {
		if want := 30*time.Second - time.Duration(i)*3*time.Second; d != want {
			t.Errorf("deadline %d was set %v out, want %v: the budget less every "+
				"flush before it", i, d, want)
		}
	}

	clock = newStepClock()
	cut := &client{clock: clock, takes: []time.Duration{0}, flushTakes: []time.Duration{31 * time.Second}}
	_, err = NewWriter(cut, clock.now).Write([]byte("0005\x02.\n"))
	if !errors.Is(err, os.ErrDeadlineExceeded) || !strings.Contains(err.Error(), "30s") {
		t.Fatalf("a flush taking 31s answered %v; want it cut at its deadline, "+
			"naming the 30s pace", err)
	}
}

// A WRITER'S END IS MEASURED FROM ITS END, never from its last write. What a
// handler does after its last byte — reaping git, publishing the audit
// record — is the server's own time, and net/http writes the end of a chunked
// response after the handler returns, under the deadline in force: so End
// gives that deadline what is left of the mebibyte's budget from the moment
// it is called, a whole pace when the last write completed a mebibyte, and
// none at all once the budget is spent.
func TestAWriterEndsWithTheBudgetLeftFromItsEnd(t *testing.T) {
	t.Parallel()
	clock := newStepClock()
	conn := &client{clock: clock, takes: []time.Duration{27 * time.Second}}
	w := NewWriter(conn, clock.now)
	if _, err := w.Write(make([]byte, 1<<10)); err != nil {
		t.Fatal(err)
	}
	clock.advance(10 * time.Minute)
	w.End()
	if want := clock.at.Add(3 * time.Second); !conn.deadline.Equal(want) {
		t.Errorf("after ten minutes of the server's own work, End left the deadline "+
			"%v past the last write; want 3s past End itself", conn.deadline.Sub(clock.at.Add(-10*time.Minute)))
	}

	clock = newStepClock()
	whole := &client{clock: clock, takes: []time.Duration{29 * time.Second}}
	w = NewWriter(whole, clock.now)
	if _, err := w.Write(make([]byte, objstore.MiB)); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	w.End()
	if want := clock.at.Add(30 * time.Second); !whole.deadline.Equal(want) {
		t.Errorf("End after a whole mebibyte set the deadline %v out, want the next "+
			"mebibyte's whole 30s", whole.deadline.Sub(clock.at))
	}

	clock = newStepClock()
	spent := &client{clock: clock, takes: []time.Duration{30 * time.Second}}
	w = NewWriter(spent, clock.now)
	if _, err := w.Write(make([]byte, 1<<10)); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	w.End()
	if !spent.deadline.Equal(clock.at) {
		t.Errorf("End with the budget spent set the deadline %v out, want now: the "+
			"client has had its pace, and net/http's own write is refused at once",
			spent.deadline.Sub(clock.at))
	}
}

// NET/HTTP ENDS A RESPONSE UNDER THE DEADLINE IN FORCE WHEN THE HANDLER
// RETURNS — the fact End exists for, on a real connection. A handler writes
// a chunked response through a Writer and then does more than a pace of its
// own work: ended, the client reads the whole response; not ended, the
// terminator is written under the deadline the last write set, which has
// passed, and the client's read of a response the server finished fails. The
// pace is shortened to a second so the work can outlast it.
func TestNetHTTPEndsAResponseUnderTheWritersEnd(t *testing.T) {
	t.Parallel()
	const pace = time.Second
	for _, end := range []bool{true, false} {
		t.Run(fmt.Sprintf("end=%v", end), func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
				w := NewWriter(rw, time.Now)
				w.pace = pace
				if end {
					defer w.End()
				}
				if _, err := w.Write([]byte("0008NAK\n")); err != nil {
					t.Error(err)
					return
				}
				time.Sleep(pace * 3 / 2)
			}))
			defer srv.Close()
			resp, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if !slices.Equal(resp.TransferEncoding, []string{"chunked"}) {
				t.Fatalf("transfer encoding %v, want a chunked response, whose end net/http "+
					"writes itself", resp.TransferEncoding)
			}
			body, err := io.ReadAll(resp.Body)
			switch {
			case end && (err != nil || string(body) != "0008NAK\n"):
				t.Errorf("an ended Writer's response read %q with %v; want it whole", body, err)
			case !end && err == nil:
				t.Errorf("a response whose Writer was not ended read whole (%q): the "+
					"terminator was not written under the last write's deadline, and this "+
					"case no longer shows what End is for", body)
			}
		})
	}
}

// A WRITE CROSSING A MEBIBYTE'S END IS TWO PIECES, each written and flushed
// under its own mebibyte's budget. A Write returns only once all of it is
// taken, so a crossing write held to what is left of the old budget would cut
// a client keeping the pace exactly; and a first piece left unflushed would
// put the bytes ending one mebibyte on the wire under the next one's.
func TestAWriterGivesTheNextMebibyteItsOwnBudget(t *testing.T) {
	t.Parallel()
	clock := newStepClock()
	conn := &client{clock: clock, takes: []time.Duration{20 * time.Second, 0, 0}}
	w := NewWriter(conn, clock.now)

	if _, err := w.Write(make([]byte, objstore.MiB-10)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	if want := []int{objstore.MiB - 10, 10, 10}; !slices.Equal(conn.writes, want) {
		t.Errorf("writes %v, want %v: the crossing write split at the mebibyte's end", conn.writes, want)
	}
	if want := []time.Duration{30 * time.Second, 10 * time.Second, 30 * time.Second}; !slices.Equal(conn.out, want) {
		t.Errorf("deadlines %v out, want %v: the first piece under what its mebibyte "+
			"left, the second under the next one's whole pace", conn.out, want)
	}
	if conn.flushes != 3 {
		t.Errorf("%d flushes for three pieces, want one each: the crossing write's "+
			"first piece flushed under its own mebibyte's deadline", conn.flushes)
	}
	if w.left != objstore.MiB-10 || w.budget != 30*time.Second {
		t.Errorf("after the crossing write: %d left of %v; want the next mebibyte "+
			"less the ten bytes it took, its budget whole", w.left, w.budget)
	}
}

// short is a response writer that takes half of every write and says nothing
// of the rest — a writer breaking io.Writer's contract.
type short struct{ *httptest.ResponseRecorder }

func (s *short) Write(p []byte) (int, error) { return s.ResponseRecorder.Write(p[:len(p)/2]) }

// A WRITE THE CONNECTION TOOK ONLY PART OF IS A FAILURE, even with no error
// beside it: answered as success, the caller would go on as if the client had
// the bytes it never got, and end a response missing its middle.
func TestAShortWriteIsAFailure(t *testing.T) {
	t.Parallel()
	n, err := NewWriter(&short{httptest.NewRecorder()}, time.Now).Write([]byte("0008NAK\n"))
	if n != 4 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("a writer that took half: Write = %d, %v; want 4 and io.ErrShortWrite", n, err)
	}
}

// failing is a source that hands over what it holds and then fails with err.
type failing struct {
	r   io.Reader
	err error
}

func (f *failing) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, f.err
	}
	return n, err
}

// A SOURCE ENDS ONLY AT ITS OWN io.EOF. A download whose store stopped
// answering part way — io.ErrUnexpectedEOF among the ways — is a failure the
// caller turns into a cut response, never one ended as if it were whole; and
// one that does end arrives whole, a mebibyte a write.
func TestSendEndsOnlyAtTheSourcesOwnEnd(t *testing.T) {
	t.Parallel()
	data := bytes.Repeat([]byte("crewlet "), (2*objstore.MiB+104)/8)
	rec := httptest.NewRecorder()
	if err := Send(rec, bytes.NewReader(data), time.Now); err != nil {
		t.Fatalf("a source that ended: %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) || !rec.Flushed {
		t.Fatalf("sent %d of %d bytes, flushed %v; want all of it, on its way",
			rec.Body.Len(), len(data), rec.Flushed)
	}

	for _, cut := range []error{io.ErrUnexpectedEOF, fmt.Errorf("store: %w", io.EOF)} {
		err := Send(httptest.NewRecorder(), &failing{r: bytes.NewReader(data), err: cut}, time.Now)
		if !errors.Is(err, cut) {
			t.Errorf("a source cut with %v: Send answered %v, want the source's own failure", cut, err)
		}
	}
}

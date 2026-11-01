package httprelay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

// --- fixture ---------------------------------------------------------------

var ring = statelog.OneKey("k", "relay-test-key")

// node is one process of a two-node fleet: its relay, served on the fleet's
// broker, and the entry route a client calls it on.
type node struct {
	relay *Relay
	entry *httptest.Server
	// verdict is what this node's Admit answers.
	verdict atomic.Value
	// mute, once set, makes every Ask this node sends go unanswered: the
	// node is still running, and nothing it says reaches anybody.
	mute atomic.Bool
	// admitting, once set, is called by this node's Admit before it answers,
	// so a case can hold an OPEN while it is being admitted.
	admitting atomic.Pointer[func()]
	// answered counts the requests this node has answered, OPEN and CLOSE
	// on its node subject and frames on an exchange's.
	answered atomic.Int64
}

// muteable is a node's broker, which its node can silence, and which counts
// what the node answers.
type muteable struct {
	Fabric
	mute     *atomic.Bool
	answered *atomic.Int64
}

func (m muteable) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return m.Fabric.Serve(ctx, subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		defer m.answered.Add(1)
		return h(ctx, raw)
	})
}

func (m muteable) Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error) {
	if m.mute.Load() {
		<-ctx.Done()
		return nil, nil
	}
	return m.Fabric.Ask(ctx, subject, request, want)
}

// fleet starts two nodes over one broker. The holder answers with h; the
// entry forwards every request it gets to the holder's incarnation.
func fleet(t *testing.T, h http.Handler, tune func(*clocks)) (entry, holder *node) {
	t.Helper()
	broker := memory.NewBroker()
	start := func(id string, handler http.Handler) *node {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		n := &node{}
		n.verdict.Store(Serve)
		rl, err := New(Options{
			Fabric: muteable{Fabric: q, mute: &n.mute, answered: &n.answered}, Name: "testrelay", Node: id, Owner: id + ":incarnation", Keyring: ring,
			Admit: func(*http.Request) Verdict {
				if hold := n.admitting.Load(); hold != nil {
					(*hold)()
				}
				return n.verdict.Load().(Verdict)
			},
			Handler: handler,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if tune != nil {
			tune(&rl.clock)
		}
		stop, err := rl.Serve(t.Context())
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
		n.relay = rl
		return n
	}
	holder = start("holder", h)
	entry = start("entry", http.NotFoundHandler())
	entry.entry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := Read(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		outcome, why := entry.relay.Forward(r.Context(), w, req, holder.relay.Owner())
		if outcome != Served {
			w.Header().Set("X-Outcome", string(outcome))
			http.Error(w, why, http.StatusBadGateway)
		}
	}))
	t.Cleanup(entry.entry.Close)
	return entry, holder
}

func fast(c *clocks) {
	c.heartbeat, c.idle, c.openWait, c.pushWait = 50*time.Millisecond, 300*time.Millisecond,
		200*time.Millisecond, 200*time.Millisecond
}

// --- what a client sees ----------------------------------------------------

// A RELAYED REQUEST IS THE HOLDER'S REQUEST: what its handler reads is what
// the client sent, and what the client reads is what the handler wrote — the
// status, the header and the body — so a handler cannot tell a relayed request
// from one handed to it, nor a client a relayed answer from a direct one.
func TestARelayedExchangeIsTheHoldersOwn(t *testing.T) {
	t.Parallel()
	entry, _ := fleet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Mcp-Session-Id", "s-1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "%s %s host=%s accept=%s hop=%s body=%s", r.Method, r.URL.RequestURI(), r.Host,
			r.Header.Get("Accept"), r.Header.Get("X-Hop"), body)
	}), nil)

	req, _ := http.NewRequest(http.MethodPost, entry.entry.URL+"/mcp/tok?x=1", strings.NewReader(`{"a":1}`))
	req.Header.Set("Accept", "application/json, text/event-stream")
	// A HEADER THE CONNECTION NAMES describes the client's connection to
	// the entry, and is not the holder's to read.
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "this hop only")
	res, err := httpxtest.Pool(t).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202", res.StatusCode, got)
	}
	if res.Header.Get("Mcp-Session-Id") != "s-1" || res.Header.Get("Content-Type") != "application/json" {
		t.Errorf("header = %v, want the handler's", res.Header)
	}
	want := "POST /mcp/tok?x=1 host=" + strings.TrimPrefix(entry.entry.URL, "http://") +
		` accept=application/json, text/event-stream hop= body={"a":1}`
	if string(got) != want {
		t.Errorf("body = %q\nwant  %q", got, want)
	}
}

// AN EVENT STREAM IS RELAYED AS IT IS WRITTEN, not when it ends: a box's tool
// list changing reaches it on a stream that never ends by itself, so a relay
// that waited for the end would deliver nothing at all.
func TestAStreamReachesTheClientAsItIsWritten(t *testing.T) {
	t.Parallel()
	second := make(chan struct{})
	entry, _ := fleet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: one\n\n")
		http.NewResponseController(w).Flush()
		select {
		case <-second:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: two\n\n")
	}), nil)

	res, err := httpxtest.Pool(t).Get(entry.entry.URL + "/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	lines := bufio.NewReader(res.Body)
	if line, err := lines.ReadString('\n'); err != nil || line != "data: one\n" {
		t.Fatalf("first event = %q, %v — before the handler wrote the second", line, err)
	}
	close(second)
	rest, _ := io.ReadAll(lines)
	if string(rest) != "\ndata: two\n\n" {
		t.Errorf("rest = %q", rest)
	}
}

// A BODY LARGER THAN A FRAME arrives whole and in order: it is several frames,
// each pushed once the last was taken.
func TestABodyLargerThanAFrameArrivesWhole(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("0123456789abcdef"), (2*MaxBody+MaxBody/2)/16)
	entry, _ := fleet(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}), nil)
	res, err := httpxtest.Pool(t).Get(entry.entry.URL + "/big")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)
	if !bytes.Equal(got, big) {
		t.Fatalf("got %d bytes, want the %d the handler wrote", len(got), len(big))
	}
}

// A REQUEST BODY OVER [MaxBody] IS REFUSED BEFORE ANYTHING IS SENT, because a
// message larger than the broker's ceiling would be refused by the broker after
// the asker had committed to it.
func TestARequestOverTheBodyCeilingIsRefusedUnsent(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(make([]byte, MaxBody+1)))
	if _, err := Read(r); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Read = %v, want ErrTooLarge", err)
	}
	r = httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(make([]byte, MaxBody)))
	if _, err := Read(r); err != nil {
		t.Fatalf("Read at the ceiling = %v, want it carried", err)
	}
}

// --- what the holder decides -----------------------------------------------

// THE HOLDER'S VERDICT IS THE OUTCOME, and nothing is written for any but
// [Serve]: the entry decides what a client is told when the holder says the
// request is not its own, or nobody's.
func TestAHolderThatDoesNotServeWritesNothing(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	entry, holder := fleet(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		ran.Store(true)
	}), nil)
	for verdict, want := range map[Verdict]Outcome{Elsewhere: Rerouted, Refuse: Refused} {
		holder.verdict.Store(verdict)
		res, err := httpxtest.Pool(t).Get(entry.entry.URL + "/x")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		_ = res.Body.Close()
		if got := Outcome(res.Header.Get("X-Outcome")); got != want {
			t.Errorf("verdict %s: outcome %q, want %q", verdict, got, want)
		}
	}
	if ran.Load() {
		t.Error("a request the holder did not serve reached its handler")
	}
}

// A HOLDER THAT RESTARTED IS ANOTHER PROCESS, and answers for nothing its
// predecessor was asked: an OPEN names the incarnation the entry's routing
// found, and any other is told to route again.
func TestAnOpenForAnotherIncarnationIsRerouted(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	entry, _ := fleet(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran.Store(true) }), nil)
	req, _ := Read(httptest.NewRequest(http.MethodGet, "/x", nil))
	w := httptest.NewRecorder()
	outcome, _ := entry.relay.Forward(t.Context(), w, req, "holder:an-earlier-incarnation")
	if outcome != Rerouted {
		t.Errorf("outcome = %q, want rerouted", outcome)
	}
	if ran.Load() {
		t.Error("a predecessor's request ran on its successor")
	}
}

// AN OPEN REPEATED AFTER A LOST REPLY RUNS NOTHING TWICE: the entry cannot
// tell an exchange that started from one that did not, so it opens again under
// the same id, and the holder starts each id once.
func TestARepeatedOpenRunsTheRequestOnce(t *testing.T) {
	t.Parallel()
	var runs atomic.Int32
	release := make(chan struct{})
	_, holder := fleet(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		runs.Add(1)
		<-release
	}), nil)
	defer close(release)
	rl := holder.relay
	open := func() openReply {
		id := signedask.NewID()
		sealed, err := rl.opens.SealRequest(openRequest{
			ID: id, Deadline: time.Now().Add(time.Minute), Kind: kindOpen,
			Owner: rl.owner, Exchange: "EXCHANGE", Request: &Request{Method: http.MethodPost, URI: "/x"},
		})
		if err != nil {
			t.Fatalf("SealRequest: %v", err)
		}
		raw, err := rl.answer(t.Context(), sealed)
		if err != nil {
			t.Fatalf("answer: %v", err)
		}
		rep, _, err := rl.opens.OpenReply(raw, id)
		if err != nil {
			t.Fatalf("OpenReply: %v", err)
		}
		return rep
	}
	for range 3 {
		if rep := open(); rep.Verdict != Serve {
			t.Fatalf("verdict = %s, want serve", rep.Verdict)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Errorf("the handler ran %d times for one exchange, want 1", n)
	}
}

// AN UNSIGNED OPEN RUNS NOTHING: the broker authenticates nobody, and an open
// is an instruction to run a request as whoever its credential names.
func TestAnOpenTheFleetDidNotSignRunsNothing(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	_, holder := fleet(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran.Store(true) }), nil)
	other, err := signedask.New[openRequest, openReply]("testrelay.open", statelog.OneKey("k", "another-fleet"))
	if err != nil {
		t.Fatalf("signedask.New: %v", err)
	}
	sealed, err := other.SealRequest(openRequest{
		ID: signedask.NewID(), Deadline: time.Now().Add(time.Minute), Kind: kindOpen,
		Owner: holder.relay.owner, Exchange: "X", Request: &Request{Method: http.MethodGet, URI: "/x"},
	})
	if err != nil {
		t.Fatalf("SealRequest: %v", err)
	}
	if _, err := holder.relay.answer(t.Context(), sealed); err == nil {
		t.Error("an open another keyring signed was answered")
	}
	time.Sleep(20 * time.Millisecond)
	if ran.Load() {
		t.Error("an open another keyring signed ran")
	}
}

// A HANDLER THAT PANICS BEFORE IT ANSWERED is answered 500, as a server would
// answer it, and the holder stays up.
func TestAHandlerThatPanicsIsAnswered500(t *testing.T) {
	t.Parallel()
	entry, _ := fleet(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }), nil)
	res, err := httpxtest.Pool(t).Get(entry.entry.URL + "/x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", res.StatusCode)
	}
}

// --- when one side goes away -----------------------------------------------

// A CLIENT THAT HANGS UP CANCELS THE HOLDER'S REQUEST, at once rather than at
// the holder's idle bound: an event stream a box closed must not stay open on
// the holder for the rest of the run.
func TestAClientThatHangsUpCancelsTheHoldersRequest(t *testing.T) {
	t.Parallel()
	cancelled := make(chan struct{})
	started := make(chan struct{})
	entry, _ := fleet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}), nil)
	ctx, hangUp := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, entry.entry.URL+"/stream", nil)
	go func() {
		res, err := httpxtest.Pool(t).Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
		}
	}()
	<-started
	hangUp()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder's request outlived its client")
	}
}

// A CLIENT THAT HANGS UP BEFORE ITS OPEN IS ANSWERED never has its request
// run: the entry gives the exchange up while the holder is still admitting
// it, and the CLOSE it sends all the same keeps the holder from starting it,
// rather than leaving a request nobody reads to run until the idle bound.
func TestAClientThatHangsUpBeforeItsOpenIsAnsweredRunsNothing(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	entry, holder := fleet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran.Store(true)
		<-r.Context().Done()
	}), nil)
	admitting, forwarded := make(chan struct{}), make(chan struct{})
	hold := func() {
		close(admitting)
		<-forwarded
	}
	holder.admitting.Store(&hold)
	client := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(forwarded)
		req, err := Read(r)
		if err != nil {
			t.Errorf("Read: %v", err)
			return
		}
		if outcome, _ := entry.relay.Forward(r.Context(), w, req, holder.relay.Owner()); outcome != Unanswered {
			t.Errorf("an exchange whose client left during its open was %s; want %s", outcome, Unanswered)
		}
	}))
	t.Cleanup(client.Close)
	ctx, hangUp := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, client.URL+"/stream", nil)
	go func() {
		if res, err := httpxtest.Pool(t).Do(req); err == nil {
			_ = res.Body.Close()
		}
	}()
	<-admitting
	hangUp()
	<-forwarded
	// The holder answers the entry's CLOSE and then, let go, the OPEN.
	deadline := time.Now().Add(5 * time.Second)
	for holder.answered.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the holder answered %d requests; want the CLOSE and the OPEN", holder.answered.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !holder.relay.answering(0) {
		t.Error("the holder is answering an exchange its entry gave up before it started")
	}
	if ran.Load() {
		t.Error("the holder ran a request whose client had gone before it started")
	}
}

// AN IDLE STREAM IS KEPT ALIVE BY ITS HEARTBEATS: a stream with nothing to say
// for several idle bounds is still open, because the holder's empty frames are
// what the entry counts as the holder being there.
func TestAnIdleStreamOutlivesItsIdleBound(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entry, _ := fleet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		select {
		case <-release:
			fmt.Fprint(w, "late")
		case <-r.Context().Done():
		}
	}), fast)
	res, err := httpxtest.Pool(t).Get(entry.entry.URL + "/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	time.Sleep(4 * entry.relay.clock.idle)
	close(release)
	got, err := io.ReadAll(res.Body)
	if err != nil || string(got) != "late" {
		t.Errorf("after %s idle the stream read %q, %v; want it still open", 4*entry.relay.clock.idle, got, err)
	}
}

// A HOLDER THAT STOPS ANSWERING CUTS THE RESPONSE, and the client sees a cut
// one rather than a stream that ended cleanly: its node went down, and nothing
// it had begun to say is complete.
func TestAHolderThatStopsAnsweringCutsTheResponse(t *testing.T) {
	t.Parallel()
	entry, holder := fleet(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "partial")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	}), fast)
	res, err := httpxtest.Pool(t).Get(entry.entry.URL + "/stream")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer res.Body.Close()
	buf := make([]byte, len("partial"))
	if _, err := io.ReadFull(res.Body, buf); err != nil {
		t.Fatalf("read the first frame: %v", err)
	}
	// THE HOLDER GOES SILENT: its pushes stop reaching the entry.
	holder.mute.Store(true)
	if _, err := io.ReadAll(res.Body); err == nil {
		t.Error("a response whose holder went silent ended as though it were complete")
	}
}

// A HOLDER NOTHING ANSWERS FOR IS UNANSWERED, after its open is repeated, and
// nothing is written — the entry is free to find the seat's holder again.
func TestAnUnservedHolderIsUnanswered(t *testing.T) {
	t.Parallel()
	entry, _ := fleet(t, http.NotFoundHandler(), fast)
	req, _ := Read(httptest.NewRequest(http.MethodGet, "/x", nil))
	w := httptest.NewRecorder()
	outcome, why := entry.relay.Forward(t.Context(), w, req, "nobody:incarnation")
	if outcome != Unanswered || why == "" {
		t.Errorf("outcome = %q (%q), want unanswered with a reason", outcome, why)
	}
	if w.Code != http.StatusOK || w.Body.Len() != 0 || len(w.Header()) != 0 {
		t.Errorf("an unanswered relay wrote %d %v %q", w.Code, w.Header(), w.Body.String())
	}
}

// A FRAME PUSHED AGAIN IS WRITTEN ONCE: its first copy's acknowledgement was
// lost or late, and a second copy of the same bytes in the client's stream
// would be a corrupt event, not a repeated one.
func TestAFramePushedAgainIsWrittenOnce(t *testing.T) {
	t.Parallel()
	entry, _ := fleet(t, http.NotFoundHandler(), nil)
	rl := entry.relay
	in := &inbound{rl: rl, id: "EX", frames: make(chan delivery), stopped: make(chan struct{}),
		guard: signedask.NewGuard()}
	var written bytes.Buffer
	go func() {
		for d := range in.frames {
			written.Write(d.f.Body)
			d.done <- true
		}
	}()
	defer close(in.frames)
	push := func(seq int, body string) frameReply {
		id := signedask.NewID()
		sealed, err := rl.frames.SealRequest(frameRequest{ID: id, Deadline: time.Now().Add(time.Minute),
			Exchange: "EX", Seq: seq, Body: []byte(body)})
		if err != nil {
			t.Fatalf("SealRequest: %v", err)
		}
		raw, err := in.answer(t.Context(), sealed)
		if err != nil {
			t.Fatalf("answer: %v", err)
		}
		ack, _, err := rl.frames.OpenReply(raw, id)
		if err != nil {
			t.Fatalf("OpenReply: %v", err)
		}
		return ack
	}
	for _, f := range []struct {
		seq  int
		body string
	}{{1, "one "}, {1, "one "}, {2, "two"}, {2, "two"}, {1, "one "}} {
		if ack := push(f.seq, f.body); ack.Seq != f.seq || ack.Closed {
			t.Fatalf("frame %d acknowledged as %+v", f.seq, ack)
		}
	}
	if got := written.String(); got != "one two" {
		t.Errorf("the client read %q, want each frame once", got)
	}
}

// THE HOLDER'S HANDLER WAITS once a frame's worth of body is unsent, so a
// large answer leaves the holder only as fast as its client takes it.
func TestAHandlerWaitsOnceAFramesWorthIsUnsent(t *testing.T) {
	t.Parallel()
	w := newWriter(t.Context())
	wrote := make(chan int, 1)
	go func() {
		n, _ := w.Write(make([]byte, MaxBody+1))
		wrote <- n
	}()
	select {
	case n := <-wrote:
		t.Fatalf("a write past a frame's worth returned (%d bytes) with nothing sent", n)
	case <-time.After(100 * time.Millisecond):
	}
	f, ok := w.next(t.Context(), time.Minute, true)
	if !ok || len(f.Body) != MaxBody || f.Status != http.StatusOK {
		t.Fatalf("next = %d bytes, status %d, %v; want a full frame", len(f.Body), f.Status, ok)
	}
	w.sent(len(f.Body))
	select {
	case n := <-wrote:
		if n != MaxBody+1 {
			t.Errorf("the write returned %d, want %d", n, MaxBody+1)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write still waits once its frame was sent")
	}
}

// answering reports whether the relay is answering exactly n exchanges and
// has ended at least one, which a case reads once an OPEN it held has been
// let go.
func (rl *Relay) answering(n int) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.served) == n && len(rl.ended) > 0
}

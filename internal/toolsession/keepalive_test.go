package toolsession_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/toolsession"
)

// A running call's answer, kept going (keepalive.go): what a client is sent
// while a call's tool runs, and when it is sent nothing.

// cadence is the beat the bubbled cases keep a call's answer going at: the
// bridge's own, though any would do, since a bubble's clock moves only when
// the case moves it.
const cadence = 20 * time.Second

// heldDispatcher offers `slow`, whose call it starts running
// ([toolsession.Call.Running]) and then holds until it is freed, and `quick`,
// which it answers at once without running it, as a dispatcher answers a
// call it refuses.
type heldDispatcher struct {
	entered chan struct{}
	release chan struct{}
	freed   sync.Once

	mu sync.Mutex
	// unstarted counts the held calls the server handed no way to start
	// their answer, a server that keeps nothing going.
	unstarted int
}

func newHeldDispatcher() *heldDispatcher {
	return &heldDispatcher{entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (d *heldDispatcher) Offered() []toolsession.Tool {
	return []toolsession.Tool{tool("slow"), tool("quick")}
}

func (d *heldDispatcher) Revision() uint64 { return 0 }

func (d *heldDispatcher) Call(call toolsession.Call) toolsession.Result {
	if call.Name != "slow" {
		return toolsession.Result{Output: call.Name + " refused", Failed: true}
	}
	if call.Running != nil {
		call.Running()
	} else {
		d.mu.Lock()
		d.unstarted++
		d.mu.Unlock()
	}
	d.entered <- struct{}{}
	<-d.release
	return toolsession.Result{Output: "slow done"}
}

// free lets every held call return, once.
func (d *heldDispatcher) free() { d.freed.Do(func() { close(d.release) }) }

func (d *heldDispatcher) unstartedCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.unstarted
}

// heard is every progress a client was sent, in the order its handler ran.
type heard struct {
	mu    sync.Mutex
	got   []*mcp.ProgressNotificationParams
	first chan struct{}
	once  sync.Once
}

func newHeard() *heard { return &heard{first: make(chan struct{})} }

func (h *heard) handler(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	h.mu.Lock()
	h.got = append(h.got, req.Params)
	h.mu.Unlock()
	h.once.Do(func() { close(h.first) })
}

func (h *heard) all() []*mcp.ProgressNotificationParams {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*mcp.ProgressNotificationParams(nil), h.got...)
}

func (h *heard) count() int { return len(h.all()) }

// overPipe connects a client that hears progress to s over an in-process pipe:
// a transport a bubble's goroutines wait on durably, as none waits on a
// socket. Everything it starts ends with the case, d's held calls first.
func overPipe(t *testing.T, s *toolsession.Server, d *heldDispatcher, h *heard) *mcp.ClientSession {
	t.Helper()
	serverEnd, clientEnd := mcp.NewInMemoryTransports()
	return pipeTo(t, s, d, h, serverEnd, clientEnd)
}

// overBlindPipe is [overPipe] with the server's end writing every message
// whatever context it is sent under ([blindTransport]), as the streamable HTTP
// transport the bridge serves a box over does.
func overBlindPipe(t *testing.T, s *toolsession.Server, d *heldDispatcher, h *heard) *mcp.ClientSession {
	t.Helper()
	serverEnd, clientEnd := mcp.NewInMemoryTransports()
	return pipeTo(t, s, d, h, blindTransport{serverEnd}, clientEnd)
}

// blindTransport is a server's end of a pipe that writes a message whatever
// context it is sent under. The in-memory pipe refuses a write made under a
// context that has ended, and so would keep a progress sent under a cancelled
// request's context off the wire on its own; the streamable HTTP transport
// does not (go-sdk v1.8.0: its write routes a message by the request id the
// context carries, and never asks whether the context ended), so on the
// bridge only the beat itself keeps one from following a cancel.
type blindTransport struct{ mcp.Transport }

func (b blindTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := b.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return blindConn{conn}, nil
}

// blindConn writes under a context no cancel reaches ([blindTransport]).
type blindConn struct{ mcp.Connection }

func (c blindConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	return c.Connection.Write(context.WithoutCancel(ctx), msg)
}

// pipeTo serves s on serverEnd and connects a client that hears progress to it
// on clientEnd; everything it starts ends with the case, d's held calls first.
func pipeTo(t *testing.T, s *toolsession.Server, d *heldDispatcher, h *heard,
	serverEnd, clientEnd mcp.Transport) *mcp.ClientSession {

	t.Helper()
	ss, err := s.MCP().Connect(context.WithoutCancel(t.Context()), serverEnd, nil)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "fake-cli", Version: "1"},
		&mcp.ClientOptions{ProgressNotificationHandler: h.handler})
	cs, err := c.Connect(context.WithoutCancel(t.Context()), clientEnd, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		d.free()
		_ = cs.Close()
		_ = ss.Wait()
	})
	return cs
}

// overHTTP serves s over the streamable HTTP transport, as the bridge serves a
// box's harness, and connects a client that hears progress to it.
func overHTTP(t *testing.T, s *toolsession.Server, h *heard) (*httptest.Server, *mcp.ClientSession) {
	t.Helper()
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.MCP() }, nil))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "fake-cli", Version: "1"},
		&mcp.ClientOptions{ProgressNotificationHandler: h.handler})
	cs, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL, HTTPClient: srv.Client()}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return srv, cs
}

// callInBackground makes a call to name, on progress token token where it is
// not empty, and hands back its answer once the call returns.
func callInBackground(ctx context.Context, cs *mcp.ClientSession, name, token string) <-chan *mcp.CallToolResult {
	params := &mcp.CallToolParams{Name: name, Arguments: map[string]any{}}
	if token != "" {
		params.SetProgressToken(token)
	}
	answered := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, err := cs.CallTool(ctx, params)
		if err != nil {
			res = &mcp.CallToolResult{IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "the call failed: " + err.Error()}}}
		}
		answered <- res
	}()
	return answered
}

// A RUNNING CALL'S FIRST PROGRESS REACHES AN HTTP CLIENT WHILE ITS TOOL IS
// STILL HELD. Over streamable HTTP the SDK writes nothing on a call's response
// until its first event, so a call that sends nothing has a silent response
// until its answer, and Claude Code gives a response's first byte a minute
// before it answers its model that the call timed out, while the call runs on
// (2.1.310, measured). So the first progress is written the moment the call
// starts running, before its tool returns, on the call's own token.
//
// Mutation: skip the first progress (`false &&` before the beat's first send),
// and nothing reaches the client while the tool is held.
func TestARunningCallsFirstProgressReachesAnHTTPClientWhileItsToolIsHeld(t *testing.T) {
	t.Parallel()
	d := newHeldDispatcher()
	s := toolsession.NewServer("crewlet", "", "", d)
	// AN HOUR'S CADENCE: the first progress is the one this case is about,
	// and none after it can come while the case runs.
	s.KeepAnswersGoing(time.Hour)
	h := newHeard()
	_, cs := overHTTP(t, s, h)
	t.Cleanup(d.free)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	answered := callInBackground(ctx, cs, "slow", "held-1")
	select {
	case <-d.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached the dispatcher")
	}
	select {
	case <-h.first:
	case <-ctx.Done():
		t.Fatal("no progress reached the client while its call's tool was held")
	}
	select {
	case res := <-answered:
		t.Fatalf("the call was answered %q while its tool was still held", text(res))
	default:
	}
	if got := h.all(); len(got) != 1 || got[0].ProgressToken != "held-1" || got[0].Progress != 0 {
		t.Errorf("the client heard %+v, want one progress, at 0, on the call's own token", got)
	}

	d.free()
	if res := <-answered; res.IsError || text(res) != "slow done" {
		t.Errorf("the call was answered %v %q, want its tool's answer", res.IsError, text(res))
	}
}

// A RUNNING CALL'S PROGRESS KEEPS COMING EVERY CADENCE WHILE ITS TOOL RUNS, AND
// STOPS ONCE IT HAS RETURNED. A harness gives up a call whose response has
// been silent for too long, so the answer is kept going at every cadence: a
// progress at once, then one each time the cadence passes, counted up. And
// none once the tool has returned: the answer comes next, and nothing may
// follow it. Decided in a bubble, on the instants the case names.
//
// Mutation: have a tick send nothing (`false &&` before its send), and the
// count stays at the first; never close the beat's quit, and the call is not
// answered while its progress goes on.
func TestARunningCallsProgressKeepsComingUntilItsToolReturns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newHeldDispatcher()
		s := toolsession.NewServer("crewlet", "", "", d)
		s.KeepAnswersGoing(cadence)
		h := newHeard()
		cs := overPipe(t, s, d, h)

		answered := callInBackground(t.Context(), cs, "slow", "beat-1")
		<-d.entered
		synctest.Wait()
		if got := h.count(); got != 1 {
			t.Fatalf("%d progress as the call started running, want the first one", got)
		}
		// HALF WAY TO THE FIRST BEAT, nothing more.
		time.Sleep(cadence / 2)
		synctest.Wait()
		if got := h.count(); got != 1 {
			t.Fatalf("%d progress half a cadence in, want only the first", got)
		}
		for beat := 1; beat <= 3; beat++ {
			time.Sleep(cadence)
			synctest.Wait()
			if got := h.count(); got != 1+beat {
				t.Fatalf("%d progress %d cadences in, want %d", got, beat, 1+beat)
			}
		}

		d.free()
		synctest.Wait()
		select {
		case res := <-answered:
			if res.IsError || text(res) != "slow done" {
				t.Errorf("the call was answered %v %q, want its tool's answer", res.IsError, text(res))
			}
		default:
			t.Error("the call was not answered once its tool returned")
		}
		// PAST SEVERAL MORE CADENCES, nothing more.
		time.Sleep(3 * cadence)
		synctest.Wait()
		got := h.all()
		if len(got) != 4 {
			t.Fatalf("%d progress in all, want the four sent while the tool ran", len(got))
		}
		for i, p := range got {
			if p.ProgressToken != "beat-1" || p.Progress != float64(i) || p.Total != 0 || p.Message != "" {
				t.Errorf("progress %d is %+v, want the call's own token counted up from 0, "+
					"with no total and no message", i, p)
			}
		}
	})
}

// A RUNNING CALL'S ANSWER WAITS FOR A PROGRESS ALREADY ON ITS WAY. The beat
// writes from a goroutine of its own, so the moment a tool returns can find a
// progress half-way out; the answer is handed back only once that write has
// ended, so no progress races the answer on its response or follows it — a
// harness handed one for a token it has retired logs it as an error (OpenCode
// 2.1.4: "Received a progress notification for an unknown token"). Held in a
// bubble: the beat's second progress is held in the server's sending
// middleware, which every progress passes through, the tool is let go, and the
// answer must not come until the held progress has been written.
//
// Mutation: hand the answer back without stopping the beat (`_ = going` for
// `going.stop()`), or stop it without waiting for its write to end (`false &&`
// before the wait in stop), and the answer comes while the progress is held.
func TestARunningCallsAnswerWaitsForAProgressAlreadyOnItsWay(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newHeldDispatcher()
		s := toolsession.NewServer("crewlet", "", "", d)
		s.KeepAnswersGoing(cadence)
		held, letGo := make(chan struct{}), make(chan struct{})
		var holding, letting sync.Once
		s.MCP().AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if p, ok := req.GetParams().(*mcp.ProgressNotificationParams); ok && p.Progress == 1 {
					holding.Do(func() { close(held) })
					<-letGo
				}
				return next(ctx, method, req)
			}
		})
		h := newHeard()
		cs := overPipe(t, s, d, h)
		// LET GO BEFORE THE PIPE IS CLOSED, which waits for every write
		// still on its way.
		release := func() { letting.Do(func() { close(letGo) }) }
		t.Cleanup(release)

		answered := callInBackground(t.Context(), cs, "slow", "held-beat-1")
		<-d.entered
		time.Sleep(cadence)
		synctest.Wait()
		select {
		case <-held:
		default:
			t.Fatal("the beat wrote no second progress a cadence into the call")
		}

		d.free()
		synctest.Wait()
		select {
		case res := <-answered:
			t.Fatalf("the call was answered %q while a progress was still on its way", text(res))
		default:
		}

		release()
		synctest.Wait()
		select {
		case res := <-answered:
			if res.IsError || text(res) != "slow done" {
				t.Errorf("the call was answered %v %q, want its tool's answer", res.IsError, text(res))
			}
		default:
			t.Fatal("the call was not answered once the progress on its way had been written")
		}
		if got := h.all(); len(got) != 2 || got[1].Progress != 1 {
			t.Errorf("the client heard %+v, want the progress at once and the one held, before the answer", got)
		}
	})
}

// A CALL ANSWERED WITHOUT RUNNING GETS NO PROGRESS. The dispatcher answers a
// call it refuses at once, before its harness could give up on it, so there is
// nothing to keep going: the answer starts where the dispatcher says the call
// has started running ([toolsession.Call.Running]), and only there. A call it
// runs, on the same session, does get one.
//
// Mutation: start the beat as the request arrives rather than where the
// dispatcher says, and the refused call is sent a progress before its answer.
func TestACallAnsweredWithoutRunningGetsNoProgress(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newHeldDispatcher()
		s := toolsession.NewServer("crewlet", "", "", d)
		s.KeepAnswersGoing(cadence)
		h := newHeard()
		cs := overPipe(t, s, d, h)

		res := <-callInBackground(t.Context(), cs, "quick", "refused-1")
		if !res.IsError || text(res) != "quick refused" {
			t.Fatalf("the refused call was answered %v %q", res.IsError, text(res))
		}
		time.Sleep(3 * cadence)
		synctest.Wait()
		if got := h.all(); len(got) != 0 {
			t.Fatalf("a call answered without running was sent %+v", got)
		}

		callInBackground(t.Context(), cs, "slow", "ran-1")
		<-d.entered
		synctest.Wait()
		if got := h.all(); len(got) != 1 || got[0].ProgressToken != "ran-1" {
			t.Errorf("a call that ran was sent %+v, want its first progress", got)
		}
	})
}

// A REQUEST WITH NO PROGRESS TOKEN GETS ONLY ITS ANSWER. The protocol ties a
// progress notification to a token its requester gave, and a client that gave
// none has said it reads none: however long the call runs, nothing is sent
// before its answer, and the answer still comes.
//
// Mutation: drop the token check (`false &&` before it), and the call is sent
// a progress on no token at all.
func TestARequestWithNoProgressTokenGetsOnlyItsAnswer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newHeldDispatcher()
		s := toolsession.NewServer("crewlet", "", "", d)
		s.KeepAnswersGoing(cadence)
		h := newHeard()
		cs := overPipe(t, s, d, h)

		answered := callInBackground(t.Context(), cs, "slow", "")
		<-d.entered
		time.Sleep(3 * cadence)
		synctest.Wait()
		if got := h.all(); len(got) != 0 {
			t.Fatalf("a call that carried no progress token was sent %+v", got)
		}
		d.free()
		if res := <-answered; res.IsError || text(res) != "slow done" {
			t.Errorf("the call was answered %v %q, want its tool's answer", res.IsError, text(res))
		}
	})
}

// A SERVER THAT KEEPS NOTHING GOING SENDS NO PROGRESS, and hands its dispatcher
// no way to start a call's answer: the relay's sessions are such servers
// (Host.Open keeps nothing going), since no harness on stdio times a
// response's first byte and the tool loop answers a call still running itself.
//
// Mutation: keep every server's answers going from NewServer, and the held
// call is handed a start and sent progress.
func TestAServerThatKeepsNothingGoingSendsNoProgress(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newHeldDispatcher()
		s := toolsession.NewServer("crewlet", "", "", d)
		h := newHeard()
		cs := overPipe(t, s, d, h)

		callInBackground(t.Context(), cs, "slow", "quiet-1")
		<-d.entered
		time.Sleep(3 * cadence)
		synctest.Wait()
		if n := d.unstartedCalls(); n != 1 {
			t.Errorf("the dispatcher was handed a way to start the call's answer (%d calls without one)", n)
		}
		if got := h.all(); len(got) != 0 {
			t.Errorf("a server that keeps nothing going sent %+v", got)
		}
	})
}

// A CANCELLED CALL IS SENT NO MORE PROGRESS. A harness that cancels a call has
// retired its progress token, so the beat stops at the cancel, however long the
// call's tool runs on after it: a progress for a token the harness no longer
// holds is one more error for it to log (OpenCode 2.1.4: "Received a progress
// notification for an unknown token"). Over a pipe that writes whatever
// context a message is sent under, as the streamable HTTP transport does, so
// nothing but the beat keeps the progress off the wire; decided in a bubble,
// the call still held.
//
// Mutation: let the beat ignore the request's end — drop its stop on it and the
// check before each write — and progress goes on after the cancel.
func TestACancelledCallIsSentNoMoreProgress(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newHeldDispatcher()
		s := toolsession.NewServer("crewlet", "", "", d)
		s.KeepAnswersGoing(cadence)
		h := newHeard()
		cs := overBlindPipe(t, s, d, h)

		call, harnessGivesUp := context.WithCancel(t.Context())
		defer harnessGivesUp()
		returned := callInBackground(call, cs, "slow", "cancelled-1")
		<-d.entered
		time.Sleep(cadence)
		synctest.Wait()
		if got := h.count(); got != 2 {
			t.Fatalf("%d progress a cadence into the call, want the first and one more", got)
		}

		harnessGivesUp()
		if res := <-returned; !res.IsError {
			t.Fatalf("the harness was answered %q for a call it had cancelled", text(res))
		}
		// PAST SEVERAL MORE CADENCES, the call still held: nothing more.
		time.Sleep(3 * cadence)
		synctest.Wait()
		if got := h.count(); got != 2 {
			t.Errorf("the client was sent %d progress after it cancelled the call, want none", got-2)
		}
	})
}

// unreadDispatcher starts its one call running and holds it until nobody can
// read its answer, or until the case gives up waiting for that, and says
// which.
type unreadDispatcher struct {
	entered chan struct{}
	gone    chan bool
	giveUp  chan struct{}
}

func (d *unreadDispatcher) Offered() []toolsession.Tool { return []toolsession.Tool{tool("slow")} }

func (d *unreadDispatcher) Revision() uint64 { return 0 }

func (d *unreadDispatcher) Call(call toolsession.Call) toolsession.Result {
	if call.Running != nil {
		call.Running()
	}
	close(d.entered)
	select {
	case <-call.Abandoned:
		d.gone <- true
	case <-d.giveUp:
		d.gone <- false
	}
	return toolsession.Result{Output: "slow done"}
}

// firstEvent reads one server-sent event off r, its data lines joined.
func firstEvent(r *bufio.Reader) (string, error) {
	var data []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return strings.Join(data, "\n"), err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, strings.TrimSpace(value))
		}
	}
}

// A CALL WHOSE RESPONSE IS GONE REACHES THE DISPATCHER AS ABANDONED. A harness
// can stop waiting for a call by hanging up on its POST, sending no cancel, and
// the streamable HTTP transport runs the call on and keeps no stream the
// harness could come back to: nobody can read the answer. The SDK refuses a
// progress on a response whose POST has ended, so the next one after the
// hang-up is where that is seen, and the dispatcher is told as it is of a
// cancel.
//
// Mutation: leave the request's end as it was on a failed write (drop the
// beat's lose there), and the dispatcher is never told.
func TestACallWhoseResponseIsGoneReachesTheDispatcherAsAbandoned(t *testing.T) {
	t.Parallel()
	d := &unreadDispatcher{entered: make(chan struct{}), gone: make(chan bool, 1),
		giveUp: make(chan struct{})}
	s := toolsession.NewServer("crewlet", "", "", d)
	// A SHORT CADENCE, for the case's sake alone: the hang-up is seen at the
	// first progress after it.
	s.KeepAnswersGoing(20 * time.Millisecond)
	srv, cs := overHTTP(t, s, newHeard())
	var gaveUp sync.Once
	giveUp := func() { gaveUp.Do(func() { close(d.giveUp) }) }
	t.Cleanup(giveUp)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// THE CALL, POSTED BY HAND: the SDK's own client cancels a call it stops
	// waiting for, and a harness that hangs up on its POST sends nothing.
	post, hangUp := context.WithCancel(ctx)
	defer hangUp()
	req, err := http.NewRequestWithContext(post, http.MethodPost, srv.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":9001,"method":"tools/call","params":{"name":"slow",`+
			`"arguments":{},"_meta":{"progressToken":"gone-1"}}}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", cs.ID())
	req.Header.Set("Mcp-Protocol-Version", cs.InitializeResult().ProtocolVersion)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the call's POST was answered %s", res.Status)
	}
	// ITS FIRST PROGRESS, AT ONCE, before the harness hangs up.
	if first, err := firstEvent(bufio.NewReader(res.Body)); err != nil ||
		!strings.Contains(first, "notifications/progress") || !strings.Contains(first, "gone-1") {
		t.Fatalf("the call's response began %q (%v), want its first progress", first, err)
	}
	select {
	case <-d.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached the dispatcher")
	}

	hangUp()
	select {
	case gone := <-d.gone:
		if !gone {
			t.Error("a call whose POST its harness hung up on reached the dispatcher as one still waited for")
		}
	case <-ctx.Done():
		giveUp()
		t.Fatal("the dispatcher was never told that nobody could read the call's answer")
	}
}

// A CALL ITS HARNESS CANCELS REACHES THE DISPATCHER AS ABANDONED WHERE ITS
// ANSWER IS KEPT GOING. A server that keeps a running call's answer going hands
// its dispatcher the request's end widened to a progress that found the
// response gone, and a cancel must still close it, as it does where nothing is
// kept going: Claude Code 2.1.310 and OpenCode 2.1.4 cancel a call at their
// server's timeout in the box's settings, which is how the bridge marks a call
// that ran past its own bound. Over the streamable HTTP transport the bridge
// serves a box over, the call held: closed before its tool returns.
//
// Mutation: derive what the dispatcher is handed from a context the request's
// cancel does not reach (context.WithoutCancel of it), and a cancelled call
// reaches the dispatcher as one its harness still waits for.
func TestACallItsHarnessCancelsReachesTheDispatcherAsAbandonedWhileItsAnswerIsKeptGoing(t *testing.T) {
	t.Parallel()
	d := &unreadDispatcher{entered: make(chan struct{}), gone: make(chan bool, 1),
		giveUp: make(chan struct{})}
	s := toolsession.NewServer("crewlet", "", "", d)
	// AN HOUR'S CADENCE: past the progress the call is sent at once, nothing is
	// written on its response while the case runs, so only the cancel can say
	// nobody reads the answer.
	s.KeepAnswersGoing(time.Hour)
	h := newHeard()
	_, cs := overHTTP(t, s, h)
	var gaveUp sync.Once
	giveUp := func() { gaveUp.Do(func() { close(d.giveUp) }) }
	t.Cleanup(giveUp)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	call, harnessGivesUp := context.WithCancel(ctx)
	defer harnessGivesUp()
	returned := callInBackground(call, cs, "slow", "cancelled-2")
	select {
	case <-d.entered:
	case <-ctx.Done():
		t.Fatal("the call never reached the dispatcher")
	}
	select {
	case <-h.first:
	case <-ctx.Done():
		t.Fatal("the call's answer was not started as it began running")
	}

	// THE HARNESS GIVES UP ON THE CALL, as at its server's timeout: it cancels
	// the request.
	harnessGivesUp()
	select {
	case gone := <-d.gone:
		if !gone {
			t.Error("a call its harness cancelled reached the dispatcher as one it still waited for")
		}
	case <-ctx.Done():
		giveUp()
		t.Fatal("the dispatcher was never told that the harness had cancelled the call")
	}
	if res := <-returned; !res.IsError {
		t.Errorf("the harness was answered %q for a call it had cancelled", text(res))
	}
}

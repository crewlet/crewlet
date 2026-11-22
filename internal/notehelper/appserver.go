package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

// THE CODEX APP-SERVER ADAPTER (-adapter codex-app-server). The run's Codex
// app-server listens on a unix socket (`--listen unix://…`), and `codex exec
// --remote` runs the turn through it; this adapter joins the run's thread as a
// second client and hands each note over with `turn/steer`, which Codex puts in
// front of its model at its next request as a user message (measured on
// 0.171.0, #1818's CODEX-MEASURE.md sections 5, 12.3 and 13.2): JSON-RPC in
// WebSocket text messages, over the socket.

// openAppServer waits for the run's thread file, dials the app-server's socket
// and joins the thread: initialize, initialized, thread/resume, then
// thread/turns/list for the turn in flight.
func openAppServer(ctx context.Context, f flags, p pacing) (harness, error) {
	switch {
	case f.socket == "":
		return nil, usageError("-socket is required for -adapter codex-app-server: the app-server's unix socket")
	case f.threadFile == "":
		return nil, usageError("-thread-file is required for -adapter codex-app-server: the file holding the run's thread id")
	}
	thread, err := waitThread(ctx, f.threadFile, os.ReadFile, p)
	if err != nil {
		return nil, err
	}
	conn, err := dialSocket(ctx, f.socket, p)
	if err != nil {
		return nil, err
	}
	a, err := joinAppServer(ctx, conn, thread, p)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return a, nil
}

// waitThread is the thread id in path, looked for until the file holds one:
// the run's script writes it (by a rename) from `codex exec --json`'s
// thread.started, after the helper has started.
func waitThread(ctx context.Context, path string, read func(string) ([]byte, error), p pacing) (string, error) {
	var wait time.Duration
	for {
		data, err := read(path)
		switch {
		case err == nil && len(bytes.TrimSpace(data)) > 0:
			return string(bytes.TrimSpace(data)), nil
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return "", fmt.Errorf("reading the thread file -thread-file %s: %w", path, err)
		}
		wait = next(wait, p.waitFirst, p.waitMost)
		if !sleep(ctx, wait) {
			return "", ctx.Err()
		}
	}
}

// dialSocket dials the app-server's unix socket, again while it does not
// answer, within the pacing's dial budget.
func dialSocket(ctx context.Context, path string, p pacing) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, p.dialBudget)
	defer cancel()
	var dialer net.Dialer
	var wait time.Duration
	for {
		conn, err := dialer.DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		wait = next(wait, p.waitFirst, p.waitMost)
		if !sleep(ctx, wait) {
			return nil, fmt.Errorf("dialling the app-server's socket -socket %s within %s: %w", path, p.dialBudget, err)
		}
	}
}

// appServer is one joined thread of a Codex app-server.
type appServer struct {
	ws     *websocket.Conn
	thread string
	bound  time.Duration
	ids    atomic.Int64
	done   chan struct{}

	mu      sync.Mutex
	pending map[int64]chan rpcAnswer
	// active is the turn in flight on the thread, "" for none; finished holds
	// every turn the thread completed while the helper watched.
	active   string
	finished map[string]bool
	// changed is closed, and replaced, whenever active changes.
	changed chan struct{}
}

// rpcError is a JSON-RPC error.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return strconv.Itoa(e.Code) + " " + e.Message }

// rpcAnswer is one answer to a request.
type rpcAnswer struct {
	result json.RawMessage
	err    *rpcError
}

// wire is one message off the socket: a request, a notification or an answer.
type wire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// joinAppServer runs the WebSocket handshake over conn and joins thread.
func joinAppServer(ctx context.Context, conn net.Conn, thread string, p pacing) (*appServer, error) {
	// THE HANDSHAKE IS BOUNDED as a call is: x/net's NewClient takes no context.
	_ = conn.SetDeadline(time.Now().Add(p.callBound))
	cfg, err := websocket.NewConfig("ws://localhost/", "http://localhost/")
	if err != nil {
		return nil, err
	}
	ws, err := websocket.NewClient(cfg, conn)
	if err != nil {
		return nil, fmt.Errorf("opening a WebSocket on the app-server's socket: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	a := &appServer{
		ws: ws, thread: thread, bound: p.callBound, done: make(chan struct{}),
		pending: map[int64]chan rpcAnswer{}, finished: map[string]bool{}, changed: make(chan struct{}),
	}
	go a.read()
	if _, err = a.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "crewlet-notehelper", "version": "1"},
	}); err != nil {
		a.close()
		return nil, fmt.Errorf("initializing with the app-server: %w", err)
	}
	if err = a.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		a.close()
		return nil, fmt.Errorf("initializing with the app-server: %w", err)
	}
	// thread/resume REJOINS a thread another client is running. Without its
	// turns: the full history it would answer is deprecated, and only the
	// newest turn matters, which thread/turns/list answers.
	if _, err = a.call(ctx, "thread/resume", map[string]any{"threadId": thread, "excludeTurns": true}); err != nil {
		a.close()
		return nil, fmt.Errorf("joining the run's thread %s: %w", thread, err)
	}
	raw, err := a.call(ctx, "thread/turns/list", map[string]any{"threadId": thread})
	if err == nil {
		err = a.listed(raw)
	}
	if err != nil {
		a.close()
		return nil, fmt.Errorf("listing the turns of the run's thread %s: %w", thread, err)
	}
	return a, nil
}

// read reads every message off the socket until it closes, then ends the
// connection. A request of the server's is left unanswered: the client
// running the turn answers it, and an answer from here could decide for it.
func (a *appServer) read() {
	dec := json.NewDecoder(a.ws)
	for {
		var m wire
		if err := dec.Decode(&m); err != nil {
			a.end()
			return
		}
		switch {
		case m.Method != "" && len(m.ID) == 0:
			a.notice(m.Method, m.Params)
		case m.Method == "" && len(m.ID) > 0:
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			a.mu.Lock()
			ch := a.pending[id]
			delete(a.pending, id)
			a.mu.Unlock()
			if ch != nil {
				ch <- rpcAnswer{result: m.Result, err: m.Error}
			}
		}
	}
}

// THE TURN IN FLIGHT is learnt here and nowhere else: from the thread's
// newest turn as the helper joins ([appServer.listed]), the thread's
// notifications ([appServer.notice]), and a steer's refusal naming the turn
// Codex has ([appServer.deliver]); each through [appServer.start] and
// [appServer.stop]. Measured on Codex 0.171.0.

// listed takes the turn in flight from thread/turns/list's answer, newest
// first: its first turn, while that one is in progress.
func (a *appServer) listed(raw json.RawMessage) error {
	var answer struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return fmt.Errorf("its answer is not a list of turns: %w", err)
	}
	if len(answer.Data) > 0 && answer.Data[0].Status == "inProgress" {
		a.begin(answer.Data[0].ID)
	}
	return nil
}

// notice follows the thread's turns from the server's notifications, a joined
// client receiving them for every turn started after it joined: a turn starts
// with turn/started and ends with turn/completed, each naming it as turn.id.
// Another thread's (a subagent's) are not the run's.
func (a *appServer) notice(method string, params json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(params, &p) != nil || p.ThreadID != a.thread {
		return
	}
	switch method {
	case "turn/started":
		a.begin(p.Turn.ID)
	case "turn/completed":
		a.stop(p.Turn.ID)
	}
}

// stop marks turn over: never the turn in flight again.
func (a *appServer) stop(turn string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finished[turn] = true
	if a.active == turn {
		a.set("")
	}
}

// begin makes turn the one in flight, unless the thread already completed it.
func (a *appServer) begin(turn string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.start(turn)
}

// start is begin with a.mu held.
func (a *appServer) start(turn string) {
	if turn != "" && !a.finished[turn] && a.active != turn {
		a.set(turn)
	}
}

// set changes the turn in flight; a.mu is held.
func (a *appServer) set(turn string) {
	a.active = turn
	close(a.changed)
	a.changed = make(chan struct{})
}

// end marks the connection ended, once.
func (a *appServer) end() {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}

func (a *appServer) ready(ctx context.Context) error {
	for {
		a.mu.Lock()
		active, changed := a.active, a.changed
		a.mu.Unlock()
		if active != "" {
			return nil
		}
		select {
		case <-changed:
		case <-a.done:
			return errGone
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *appServer) deliver(ctx context.Context, text string) error {
	a.mu.Lock()
	turn := a.active
	a.mu.Unlock()
	if turn == "" {
		return errNotNow
	}
	err := a.steer(ctx, turn, text)
	var other wrongTurn
	if errors.As(err, &other) {
		// THE TURN MOVED ON WITHOUT A NOTICE reaching the helper yet: Codex
		// names the one it has, and the note is steered there, once.
		a.stop(turn)
		a.begin(string(other))
		err = a.steer(ctx, string(other), text)
		turn = string(other)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errNoTurn):
		// The turn ended: the note waits for the next turn/started.
		a.stop(turn)
		return errNotNow
	}
	a.mu.Lock()
	over := a.finished[turn]
	a.mu.Unlock()
	if over {
		return errNotNow
	}
	return fmt.Errorf("turn/steer: %w", err)
}

// steer sends text to turn: nil once Codex took it; [errNoTurn] where no turn
// is in flight; a [wrongTurn] naming the turn in flight where it is not turn.
func (a *appServer) steer(ctx context.Context, turn, text string) error {
	_, err := a.call(ctx, "turn/steer", map[string]any{
		"threadId":       a.thread,
		"expectedTurnId": turn,
		"input":          []map[string]string{{"type": "text", "text": text}},
	})
	var refused *rpcError
	if !errors.As(err, &refused) || refused.Code != codeInvalidRequest {
		return err
	}
	if refused.Message == "no active turn to steer" {
		return errNoTurn
	}
	if named, found, ok := foundTurn(refused.Message); ok && named == turn && found != "" && found != turn {
		return wrongTurn(found)
	}
	return err
}

// codeInvalidRequest is JSON-RPC's invalid request, which turn/steer answers a
// steer of no turn or of the wrong one with.
const codeInvalidRequest = -32600

// errNoTurn is turn/steer's "no active turn to steer".
var errNoTurn = errors.New("no turn is in flight")

// foundTurn reads turn/steer's refusal of a steer naming another turn than the
// one in flight, "expected active turn id `X` but found `Y`": the turn named
// (X) and the turn in flight (Y). Read by hand, as regexp would add more to
// the box's binary than the helper's own code.
func foundTurn(msg string) (named, found string, ok bool) {
	rest, ok := strings.CutPrefix(msg, "expected active turn id `")
	if !ok {
		return "", "", false
	}
	named, rest, ok = strings.Cut(rest, "` but found `")
	if !ok {
		return "", "", false
	}
	found, ok = strings.CutSuffix(rest, "`")
	if !ok || strings.Contains(named+found, "`") {
		return "", "", false
	}
	return named, found, true
}

// wrongTurn is a steer refused for naming another turn: the turn in flight.
type wrongTurn string

func (w wrongTurn) Error() string { return "the turn in flight is " + string(w) }

func (a *appServer) gone() <-chan struct{} { return a.done }

func (a *appServer) close() {
	_ = a.ws.Close()
	<-a.done
}

// call sends method with params and waits, within the call bound, for its
// answer: the result, or the server's error.
func (a *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, a.bound)
	defer cancel()
	id := a.ids.Add(1)
	ch := make(chan rpcAnswer, 1)
	a.mu.Lock()
	a.pending[id] = ch
	a.mu.Unlock()
	forget := func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}
	if err := a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		forget()
		return nil, err
	}
	select {
	case ans := <-ch:
		if ans.err != nil {
			return nil, ans.err
		}
		return ans.result, nil
	case <-a.done:
		forget()
		return nil, errGone
	case <-ctx.Done():
		forget()
		return nil, fmt.Errorf("no answer to %s within %s: %w", method, a.bound, ctx.Err())
	}
}

// send writes one message as a WebSocket text message.
func (a *appServer) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err = websocket.Message.Send(a.ws, string(data)); err != nil {
		return fmt.Errorf("writing to the app-server: %w", err)
	}
	return nil
}

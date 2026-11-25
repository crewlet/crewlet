package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
// thread/turns/list for the turn in flight. -interrupt-once looks for the
// thread file once: the helper it stands in for has already ended.
func openAppServer(ctx context.Context, f flags, p pacing, ev events) (harness, error) {
	switch {
	case f.socket == "":
		return nil, usageError("-socket is required for -adapter codex-app-server: the app-server's unix socket")
	case f.threadFile == "":
		return nil, usageError("-thread-file is required for -adapter codex-app-server: the file holding the run's thread id")
	}
	var thread string
	var err error
	if f.interruptOnce {
		thread, err = readThread(f.threadFile)
	} else {
		thread, err = waitThread(ctx, f.threadFile, os.ReadFile, p)
	}
	if err != nil {
		return nil, err
	}
	conn, err := dialSocket(ctx, f.socket, p)
	if err != nil {
		return nil, err
	}
	a, err := joinAppServer(ctx, conn, thread, p, ev, f.skills)
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

// readThread is the thread id in path, which must already hold one.
func readThread(path string) (string, error) {
	data, err := os.ReadFile(path)
	switch {
	case err != nil:
		return "", fmt.Errorf("reading the thread file -thread-file %s: %w", path, err)
	case len(bytes.TrimSpace(data)) == 0:
		return "", fmt.Errorf("the thread file -thread-file %s holds no thread id", path)
	}
	return string(bytes.TrimSpace(data)), nil
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
	ev     events
	// skills is the run's skills as the runner installed them
	// ([appServer.completed]).
	skills skillSet
	// tokens is the thread's total tokens as last reported, and compacting
	// whether a context compaction runs, until its item completes or the
	// turn's work ends without it ([appServer.notice]): the reader's alone
	// ([appServer.counted]).
	tokens     int64
	compacting bool

	mu      sync.Mutex
	pending map[int64]waiter
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

// rpcAnswer is one answer to a request, and what taking it in the reader's
// order said ([appServer.callApplied]).
type rpcAnswer struct {
	result  json.RawMessage
	err     *rpcError
	applied error
}

// waiter is a request waiting for its answer: where the answer goes, and what
// the reader does with it first, nil for nothing.
type waiter struct {
	ch    chan rpcAnswer
	apply func(json.RawMessage) error
}

// wire is one message off the socket: a request, a notification or an answer.
type wire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// joinAppServer runs the WebSocket handshake over conn and joins thread,
// reporting ev of it.
func joinAppServer(ctx context.Context, conn net.Conn, thread string, p pacing, ev events, skills skillSet) (*appServer, error) {
	a, err := openAppSession(ctx, conn, thread, p, ev, skills)
	if err != nil {
		return nil, err
	}
	if err = a.rejoin(ctx, p); err != nil {
		a.close()
		return nil, fmt.Errorf("joining the run's thread %s: %w", thread, err)
	}
	// THE LIST IS TAKEN IN THE READER'S ORDER ([appServer.callApplied]): a
	// notice Codex sends after its answer (a response of the listed turn) is
	// read after the turn it names is in flight, never dropped before it is.
	if _, err = a.callApplied(ctx, "thread/turns/list", map[string]any{"threadId": thread}, a.listed); err != nil {
		a.close()
		return nil, fmt.Errorf("listing the turns of the run's thread %s: %w", thread, err)
	}
	return a, nil
}

// openAppSession runs the WebSocket handshake over conn and initializes a
// session with the app-server, following thread ("" for none).
func openAppSession(ctx context.Context, conn net.Conn, thread string, p pacing, ev events, skills skillSet) (*appServer, error) {
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
		ws: ws, thread: thread, bound: p.callBound, done: make(chan struct{}), ev: ev, skills: skills,
		pending: map[int64]waiter{}, finished: map[string]bool{}, changed: make(chan struct{}),
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
	return a, nil
}

// rejoin REJOINS the thread another client is running, with thread/resume:
// without its turns, whose full history is deprecated, since only the newest
// turn matters and thread/turns/list answers it. AGAIN WHILE CODEX REFUSES IT,
// within the pacing's dial budget: a thread just started refuses a joining
// client until its first turn is recorded ("list_turns is not supported yet",
// for up to 137 ms after thread.started on Codex 0.171.0, measured), and the
// helper reads the thread file within that. Only an answered refusal is tried
// again; a call that fails otherwise fails the join.
func (a *appServer) rejoin(ctx context.Context, p pacing) error {
	until := time.Now().Add(p.dialBudget)
	var wait time.Duration
	for {
		_, err := a.call(ctx, "thread/resume", map[string]any{"threadId": a.thread, "excludeTurns": true})
		var refused *rpcError
		if err == nil || !errors.As(err, &refused) {
			return err
		}
		wait = next(wait, p.waitFirst, p.waitMost)
		if time.Now().Add(wait).After(until) {
			return fmt.Errorf("refused for %s: %w", p.dialBudget, err)
		}
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
	}
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
			w, ok := a.pending[id]
			delete(a.pending, id)
			a.mu.Unlock()
			if !ok {
				continue
			}
			ans := rpcAnswer{result: m.Result, err: m.Error}
			if w.apply != nil && m.Error == nil {
				ans.applied = w.apply(m.Result)
			}
			w.ch <- ans
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
	if a.thread == "" || json.Unmarshal(params, &p) != nil || p.ThreadID != a.thread {
		return
	}
	switch method {
	case "turn/started":
		a.begin(p.Turn.ID)
	case "turn/completed":
		// A COMPACTION ENDS WITH ITS TURN, whether or not its item completed:
		// one left open would keep every later response from the count.
		a.compacting = false
		a.stop(p.Turn.ID)
	case "error":
		a.failed(params)
	case "thread/tokenUsage/updated":
		a.counted(params)
	case "item/started":
		a.compaction(params, true)
	case "item/completed":
		a.compaction(params, false)
		a.completed(params)
	}
}

// compaction follows a context compaction's window: its contextCompaction
// item's item/started to its item/completed ([appServer.counted]).
func (a *appServer) compaction(params json.RawMessage, started bool) {
	var p struct {
		Item struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &p) == nil && p.Item.Type == "contextCompaction" {
		a.compacting = started
	}
}

// failed ends a compaction's window on an error Codex does not retry: the
// app-server's `error` notification (rust-v0.171.0's
// codex-rs/app-server-protocol/src/protocol/common.rs, `Error => "error"`)
// carries v2::ErrorNotification {error, willRetry, threadId, turnId}
// (protocol/v2/notification.rs), whose willRetry false interrupts the turn.
// Its work, the compaction among it, stopped there, and a compaction whose
// item never completes would otherwise stop the count for the rest of the
// run. One Codex retries leaves the window open: the retry is the compaction's.
func (a *appServer) failed(params json.RawMessage) {
	var p struct {
		WillRetry bool `json:"willRetry"`
	}
	if json.Unmarshal(params, &p) == nil && !p.WillRetry {
		a.compacting = false
	}
}

// counted reports a model response of the turn in flight to the step cap,
// the one place the step rule lives. Codex updates the thread's token usage
// after each response, and repeats an update unchanged, so a response is an
// update whose total rose; a compaction's request rises too, inside its
// contextCompaction item's window, and is no response, as Claude Code's
// --max-turns counts none (CODEX-MEASURE §18). A turn joined in flight counts
// from the join, as excludeTurns replays no update.
func (a *appServer) counted(params json.RawMessage) {
	var p struct {
		TurnID     string `json:"turnId"`
		TokenUsage struct {
			Total struct {
				TotalTokens int64 `json:"totalTokens"`
			} `json:"total"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &p) != nil || p.TokenUsage.Total.TotalTokens <= a.tokens {
		return
	}
	a.tokens = p.TokenUsage.Total.TotalTokens
	if a.compacting {
		return
	}
	a.mu.Lock()
	inFlight := p.TurnID != "" && p.TurnID == a.active
	a.mu.Unlock()
	if inFlight && a.ev.step != nil {
		a.ev.step(p.TurnID)
	}
}

// completed reports a load of one of the run's skills: a command that read
// <skills>/<name>/SKILL.md whole, as the runner installed it. Codex records
// each command it ran as a commandExecution item with its parsed actions and
// its output, so a load is a command that succeeded, whose one action is a
// read of that file (named, resolved against the command's directory,
// exactly), and whose output's SHA-256 is the one the runner recorded for that
// skill (-skill-digest). Never the file as it stands now: a model that
// rewrote it and read it back read no skill of the run's. Anything else (a
// listing, a test, an echo, a failed or partial read, an output Codex cut
// short, another skill's file, a script) is no load, and the bridge's guard
// fails closed on it.
func (a *appServer) completed(params json.RawMessage) {
	if a.ev.skill == nil || a.skills.dir == "" {
		return
	}
	var p struct {
		Item struct {
			Type     string `json:"type"`
			Status   string `json:"status"`
			Cwd      string `json:"cwd"`
			ExitCode *int   `json:"exitCode"`
			Output   string `json:"aggregatedOutput"`
			Actions  []struct {
				Type string `json:"type"`
				Path string `json:"path"`
			} `json:"commandActions"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	it := p.Item
	if it.Type != "commandExecution" || it.Status != "completed" || it.ExitCode == nil || *it.ExitCode != 0 ||
		len(it.Actions) != 1 || it.Actions[0].Type != "read" || it.Actions[0].Path == "" {
		return
	}
	path := it.Actions[0].Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(it.Cwd, path)
	}
	name, ok := skillOf(a.skills.dir, filepath.Clean(path))
	if !ok {
		return
	}
	if want, ok := a.skills.digests[name]; !ok || sha256.Sum256([]byte(it.Output)) != want {
		return
	}
	a.ev.skill(name)
}

// skillOf is the skill whose SKILL.md path is, where path is one: exactly
// <dir>/<name>/SKILL.md, name one plain path element.
func skillOf(dir, path string) (string, bool) {
	rel, err := filepath.Rel(filepath.Clean(dir), path)
	if err != nil || !filepath.IsLocal(rel) {
		// Rel answers a clean path, so a local one has no "", "." or ".."
		// element: the name below is one plain element, checked nowhere else.
		return "", false
	}
	name, file, ok := strings.Cut(filepath.ToSlash(rel), "/")
	if !ok || file != "SKILL.md" {
		return "", false
	}
	return name, true
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

// start is begin with a.mu held. A turn put in flight is reported to the cap
// ([events]), which ends one the run's spent cap allows none of.
func (a *appServer) start(turn string) {
	if turn != "" && !a.finished[turn] && a.active != turn {
		a.set(turn)
		if a.ev.begin != nil {
			a.ev.begin(turn)
		}
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
	return classify(err, turn, "no active turn to steer")
}

// classify reads Codex's refusal of a call naming turn: [errNoTurn] for its
// "no active turn" answer (noTurn), a [wrongTurn] where it names another turn
// in flight, and err as it is otherwise.
func classify(err error, turn, noTurn string) error {
	var refused *rpcError
	if !errors.As(err, &refused) || refused.Code != codeInvalidRequest {
		return err
	}
	if refused.Message == noTurn {
		return errNoTurn
	}
	if named, found, ok := foundTurn(refused.Message); ok && named == turn && found != turn {
		return wrongTurn(found)
	}
	return err
}

// codeInvalidRequest is JSON-RPC's invalid request, which turn/steer answers a
// steer of no turn or of the wrong one with.
const codeInvalidRequest = -32600

// errNoTurn is turn/steer's "no active turn to steer".
var errNoTurn = errors.New("no turn is in flight")

// foundTurn reads Codex's refusal of a call naming another turn than the one
// in flight: the turn named (X) and the turn in flight (Y), worded
// "expected active turn id `X` but found `Y`" by turn/steer and the same
// without the backticks by turn/interrupt (measured on 0.171.0). Read by
// hand, as regexp would add more to the box's binary than the helper's own
// code.
func foundTurn(msg string) (named, found string, ok bool) {
	rest, ok := strings.CutPrefix(msg, "expected active turn id ")
	if !ok {
		return "", "", false
	}
	quote := strings.HasPrefix(rest, "`")
	sep := " but found "
	if quote {
		rest, sep = rest[1:], "` but found `"
		if rest, ok = strings.CutSuffix(rest, "`"); !ok {
			return "", "", false
		}
	}
	named, found, ok = strings.Cut(rest, sep)
	if !ok || named == "" || found == "" || strings.ContainsAny(named+found, "` ") {
		return "", "", false
	}
	return named, found, true
}

// wrongTurn is a call refused for naming another turn: the turn in flight.
type wrongTurn string

func (w wrongTurn) Error() string { return "the turn in flight is " + string(w) }

// interrupt sends turn/interrupt, which both of its params require
// (CODEX-MEASURE §17.1, §18): {} once Codex ends the turn, -32600 "no active
// turn to interrupt" where none runs, and a refusal naming the turn in flight
// where turn is not it, which interrupts nothing: that turn is then the one
// interrupted, once.
func (a *appServer) interrupt(ctx context.Context, turn string) error {
	if turn == "" {
		a.mu.Lock()
		turn = a.active
		a.mu.Unlock()
	}
	if turn == "" {
		return errNotNow
	}
	err := a.interruptTurn(ctx, turn)
	var other wrongTurn
	if errors.As(err, &other) {
		// Codex names the turn in flight and interrupted nothing: that one,
		// once more.
		a.stop(turn)
		a.begin(string(other))
		turn = string(other)
		err = a.interruptTurn(ctx, turn)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errNoTurn):
		return errNotNow
	}
	return fmt.Errorf("turn/interrupt of turn %s: %w", turn, err)
}

// interruptTurn sends turn/interrupt of turn, classified ([classify]).
func (a *appServer) interruptTurn(ctx context.Context, turn string) error {
	_, err := a.call(ctx, "turn/interrupt", map[string]any{"threadId": a.thread, "turnId": turn})
	return classify(err, turn, "no active turn to interrupt")
}

func (a *appServer) gone() <-chan struct{} { return a.done }

func (a *appServer) close() {
	_ = a.ws.Close()
	<-a.done
}

// call sends method with params and waits, within the call bound, for its
// answer: the result, or the server's error.
func (a *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return a.callApplied(ctx, method, params, nil)
}

// callApplied is call whose successful answer the reader hands apply before
// it reads the next message, so what apply learns holds for every message
// after the answer; apply's error is the call's.
func (a *appServer) callApplied(ctx context.Context, method string, params any,
	apply func(json.RawMessage) error) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, a.bound)
	defer cancel()
	id := a.ids.Add(1)
	ch := make(chan rpcAnswer, 1)
	a.mu.Lock()
	a.pending[id] = waiter{ch: ch, apply: apply}
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
		switch {
		case ans.err != nil:
			return nil, ans.err
		case ans.applied != nil:
			return nil, ans.applied
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

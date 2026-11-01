package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// envd: the agent inside the box.
//
// # Two protocols on one host, because envd serves two
//
// FILES are plain HTTP — a GET and a multipart POST on /files. Nothing exotic
// and nothing to explain.
//
// PROCESSES are Connect RPC, and the reason matters: a command's output
// arrives as it is produced, so the call is SERVER-STREAMING. Connect's
// streaming framing is five bytes of envelope — one flag byte, then a
// big-endian uint32 length — in front of each JSON message, and this file
// implements exactly that rather than pulling in a Connect runtime and its
// protobuf toolchain for one call shape. The framing is small, stable and
// specified; the dependency is neither.
//
// # Why the streaming call is drained rather than followed
//
// The engine's own model of a launch is DETACHED: a job is started, the
// turn suspends, and something later reads a result file. So nothing here
// needs to watch output arrive — a foreground command is run to completion
// and its frames are folded into one [ExecResult], and a background command
// is started and abandoned deliberately, with the PID taken from the first
// frame envd sends.

// e2bEnvdUser is the account every E2B template runs as, and the owner every
// envd call is made on behalf of.
//
// envd takes the user as a QUERY PARAMETER rather than inferring it, and it
// has no default: a call that omits it fails with a permission error that
// names no user, which reads as a broken template.
const e2bEnvdUser = "user"

// e2bStreamIdleTimeout bounds how long one exec may go with NO output before
// the call is abandoned.
//
// NOT A RUN DEADLINE — the engine imposes none, and this is measured between
// FRAMES rather than over the call. The distinction is the whole point: a
// coding agent's foreground command legitimately runs for many minutes, so an
// overall timeout would kill working jobs, while a stream that has gone
// completely silent is indistinguishable from a wedged envd holding the
// request open for ever.
//
// Ten minutes is longer than any gap a real command leaves — a container
// pull, a dependency install and a test suite all emit as they go — and short
// enough that a hung box is noticed the same hour.
const e2bStreamIdleTimeout = 10 * time.Minute

// idleReader abandons a stream that stops arriving.
//
// It cancels the REQUEST rather than just failing the read, because a read
// that returned an error while the connection stayed open would leak the
// round-trip: the transport keeps the connection, and envd keeps the process.
type idleReader struct {
	inner io.Reader
	timer idleTimer

	// idle is HELD, because the reset on progress needs the same value the
	// first arming used. Reading the package constant there instead made
	// the parameter a lie: a caller passing anything else got its budget
	// once and ten minutes on every read after it.
	idle   time.Duration
	cancel context.CancelFunc

	// fired is whether the countdown ran out — what tells an exchange the
	// bound abandoned from one the caller's own context ended, which both
	// surface as the same "context canceled".
	fired atomic.Bool
}

// idleTimer is the countdown a reader arms and re-arms. *time.Timer satisfies
// it; the interface exists so a test can supply one it drives itself.
type idleTimer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

func newIdleReader(body io.Reader, cancel context.CancelFunc, idle time.Duration) *idleReader {
	return newIdleReaderOn(body, cancel, idle,
		func(d time.Duration, fire func()) idleTimer { return time.AfterFunc(d, fire) })
}

// newIdleReaderOn is newIdleReader with the countdown supplied.
//
// The seam carries ONE assertion that a real clock cannot make honestly.
// "A delivery re-arms the budget" is shown by a stream that keeps delivering
// NOT being abandoned, and an abandonment that has not happened has no signal
// to wait on — so against wall time the case can only sleep, deliver, and
// assert the timer stayed quiet, which asserts that the TEST's own goroutine
// was scheduled within the budget. That is a claim about the machine rather
// than about this reader, and it is false often enough to matter: the case
// slept 10ms against a 40ms budget and went red on a loaded CI box, on code
// that was doing exactly what it should.
//
// Only the negative needs this. That the armed countdown eventually fires is
// asserted against a real timer, because waiting for something to HAPPEN is
// the direction a slow machine helps.
func newIdleReaderOn(
	body io.Reader,
	cancel context.CancelFunc,
	idle time.Duration,
	arm func(time.Duration, func()) idleTimer,
) *idleReader {
	r := &idleReader{inner: body, idle: idle, cancel: cancel}
	r.timer = arm(idle, func() {
		r.fired.Store(true)
		cancel()
	})
	return r
}

// silent names an error this reader's bound caused as exactly that, and leaves
// every other error as it was: a bare "context canceled" reads as the caller's
// own cancellation, which sends whoever reads it looking in the wrong place.
func (r *idleReader) silent(err error) error {
	if err != nil && r.fired.Load() {
		return fmt.Errorf("envd moved no byte for %s, so the exchange was abandoned: %w", r.idle, err)
	}
	return err
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		// RESET ON PROGRESS, not on entry: a read that blocked for nine
		// minutes and then delivered a byte is a live command, and one
		// that never delivers anything is what this exists to stop.
		r.timer.Reset(r.idle)
	}
	return n, err
}

// stop releases the idle timer. The BODY is closed by whoever opened the
// response — a reader that closed somebody else's body would be a second
// owner of it, and the one thing worse than a leaked body is two closes.
func (r *idleReader) stop() {
	r.timer.Stop()
	// Cancelling releases the request context even on the ordinary path,
	// where the stream ended by itself: a context that is never cancelled
	// leaks its goroutine and its timer for the life of the process.
	r.cancel()
}

// envdClient talks to one box's envd.
//
// ITS HTTP CLIENT CARRIES NO OVERALL TIMEOUT, and that is the one thing about
// it that has to be got right. http.Client.Timeout covers the whole exchange
// INCLUDING reading the body, so the control plane's bound — which is correct
// for minting a VM — would kill every command that ran longer than it. What
// bounds a command here is [e2bStreamIdleTimeout], measured between frames.
type envdClient struct {
	host string
	http *http.Client

	// token is the box's envd access token ([e2bBox.EnvdAccessToken]),
	// sent on every request.
	token string

	// fileIdle is [e2bFileIdleTimeout], held on the value so a test can
	// watch a silent transfer abandoned without waiting a minute for it.
	fileIdle time.Duration
}

// newEnvdClient derives envd's client from the caller's, keeping its
// transport and dropping its deadline.
//
// The transport falls back to [httpx.Transport] rather than to nil, which
// would be http.DefaultTransport and its two idle connections per host —
// the one thing every other client here was moved off.
func newEnvdClient(host, token string, from *http.Client) *envdClient {
	client := &http.Client{Transport: httpx.Transport()}
	if from != nil {
		if from.Transport != nil {
			client.Transport = from.Transport
		}
		client.CheckRedirect = from.CheckRedirect
		client.Jar = from.Jar
	}
	return &envdClient{host: host, http: client, token: token, fileIdle: e2bFileIdleTimeout}
}

// connectEnvelope is the five-byte prefix Connect puts before each streamed
// message: a flag byte, then a big-endian uint32 length.
const connectEnvelope = 5

// connectEndFlag marks the final frame of a Connect stream, whose payload is
// the trailer rather than a message.
const connectEndFlag = 0x02

// e2bProcessResult is one command's outcome, folded from the frames: what
// envd SAID about how it ended, each fact apart, so the caller reads a status
// off them rather than off a sentinel standing for every way of not having
// one.
type e2bProcessResult struct {
	PID int
	// Ended is whether envd sent the command's `end` event. A stream that
	// closes without one carries no account of the process at all.
	Ended bool
	// ExitCode is the status the process exited with, and -1 when it
	// reported none: no `end` event, or a process a signal ended.
	ExitCode int
	// Signal is the number of the signal that ended the process, read off
	// the end event's status, and 0 when none did. SignalName is how the box
	// names it.
	Signal     int
	SignalName string
	// Status is envd's own account of how the process ended, kept for an
	// end event that carries neither an exit code nor a signal this reads.
	Status string
	// Trailer is the error envd closed the stream with, nil when it closed
	// it cleanly. A command that outlived its timeout_seconds ends here, as
	// a `deadline_exceeded` with no end event before it.
	Trailer *connectError
	Stdout  string
	Stderr  string
}

// connectError is the error a Connect stream's last frame carries — its code
// is one of Connect's own (`deadline_exceeded`, `unavailable`, …).
type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// connectDeadline is the code envd's stream closes with when the command's
// timeout runs out: the handler's own context carries the request's
// Connect-Timeout-Ms, and that context ending is what stops the stream —
// usually before the end event of the process it then kills, and at best
// beside one that reports the kill.
const connectDeadline = "deadline_exceeded"

// connectEnd is the payload of a Connect stream's last frame.
type connectEnd struct {
	Error *connectError `json:"error,omitempty"`
}

// startRequest is envd's Process/Start body.
//
// The command is run THROUGH A SHELL, deliberately: every caller here hands
// over a shell line — pipes, redirects, `&&` — because that is what a setup
// step and a coding-agent invocation are. envd's own API takes an argv, so
// the shell is named explicitly rather than left to a runtime that has none.
type startRequest struct {
	Process struct {
		Cmd  string            `json:"cmd"`
		Args []string          `json:"args"`
		Envs map[string]string `json:"envs,omitempty"`
		Cwd  string            `json:"cwd,omitempty"`
	} `json:"process"`
}

func newStartRequest(cmd string, opts ExecOptions) startRequest {
	var req startRequest
	req.Process.Cmd = "/bin/bash"
	req.Process.Args = []string{"-l", "-c", cmd}
	req.Process.Envs = opts.Env
	req.Process.Cwd = opts.Cwd
	return req
}

// startResponse is one frame of the Process/Start stream.
//
// envd sends a `start` event first, carrying the pid, then any number of
// `data` events, then an `end` event with the exit code. Every field is
// optional because every frame carries exactly one of them.
type startResponse struct {
	Event struct {
		Start *struct {
			PID int `json:"pid"`
		} `json:"start,omitempty"`
		Data *struct {
			Stdout []byte `json:"stdout,omitempty"`
			Stderr []byte `json:"stderr,omitempty"`
		} `json:"data,omitempty"`
		End *struct {
			ExitCode int    `json:"exitCode"`
			Status   string `json:"status,omitempty"`
			Error    string `json:"error,omitempty"`
		} `json:"end,omitempty"`
	} `json:"event"`
}

// start runs a command and folds the stream into one result.
//
// `background` abandons the stream once the pid is known, which is the whole
// of "start and walk away": the process keeps running inside the box because
// envd owns it, not because anything here is still listening.
func (c *envdClient) start(ctx context.Context, cmd string, opts ExecOptions, background bool) (e2bProcessResult, error) {
	body, err := json.Marshal(newStartRequest(cmd, opts))
	if err != nil {
		return e2bProcessResult{}, fmt.Errorf("e2b: encode command: %w", err)
	}
	framed := make([]byte, connectEnvelope+len(body))
	binary.BigEndian.PutUint32(framed[1:connectEnvelope], uint32(len(body)))
	copy(framed[connectEnvelope:], body)

	// CANCELLABLE INDEPENDENTLY OF THE CALLER, so a silent stream can be
	// abandoned without disturbing the turn's own context.
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodPost,
		c.host+"/process.Process/Start", bytes.NewReader(framed))
	if err != nil {
		cancel()
		return e2bProcessResult{}, fmt.Errorf("e2b: start: %w", err)
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if opts.TimeoutSec > 0 {
		// envd's own per-call cap, so a runaway command is stopped
		// INSIDE the box rather than merely abandoned by this client.
		req.Header.Set("Connect-Timeout-Ms",
			strconv.FormatInt(int64(opts.TimeoutSec*1000), 10))
	}
	c.authorize(req)

	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return e2bProcessResult{}, unanswered(ctx, fmt.Errorf("e2b: start: %w", err))
	}
	defer resp.Body.Close()
	stream := newIdleReader(resp.Body, cancel, e2bStreamIdleTimeout)
	// The reader's own close only stops the idle timer; the body is closed
	// above, where a linter can see it belongs to this response.
	defer stream.stop()
	if resp.StatusCode >= 400 {
		return e2bProcessResult{}, envdRefused(resp.StatusCode, fmt.Errorf("e2b: start: %d: %s",
			resp.StatusCode, httpx.ReadRefusalFrom(resp.Header.Get("Content-Type"), stream)))
	}
	return foldStream(ctx, stream, background)
}

// foldStream reads Connect frames and folds them into one result.
//
// A STREAM THAT ENDS WITHOUT AN `end` EVENT IS NOT AN ERROR HERE. envd closes
// the connection when a box is paused or reclaimed mid-command, and the
// engine's own completion model already handles a job whose box went away —
// raising instead would turn an ordinary pause into a failed run. What the
// stream did say — an end event, the trailer's error — is kept apart in the
// result, and the caller decides what the command's status is.
func foldStream(ctx context.Context, r io.Reader, background bool) (e2bProcessResult, error) {
	var (
		out    e2bProcessResult
		stdout strings.Builder
		stderr strings.Builder
		header = make([]byte, connectEnvelope)
	)
	out.ExitCode = -1
	for {
		if _, err := io.ReadFull(r, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return out, unanswered(ctx, fmt.Errorf("e2b: read stream: %w", err))
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length > maxEnvdFrame {
			// A length this large is a framing desync rather than a
			// real message, and allocating it is how a desync becomes
			// an out-of-memory instead of an error.
			return out, fmt.Errorf(
				"e2b: stream frame claims %d bytes, which is past the %d-byte "+
					"cap — the connection is out of frame", length, maxEnvdFrame)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return out, unanswered(ctx, fmt.Errorf("e2b: read stream: %w", err))
		}
		if header[0]&connectEndFlag != 0 {
			// The trailer, whose error is how a stream envd ended on a
			// deadline says so. A stream that ends here without an
			// `end` event leaves Ended false, which the caller reads as
			// "no verdict" rather than as success.
			var end connectEnd
			if err := json.Unmarshal(payload, &end); err != nil {
				return out, fmt.Errorf("e2b: decode stream trailer: %w", err)
			}
			out.Trailer = end.Error
			break
		}

		var frame startResponse
		if err := json.Unmarshal(payload, &frame); err != nil {
			return out, fmt.Errorf("e2b: decode stream frame: %w", err)
		}
		switch {
		case frame.Event.Start != nil:
			out.PID = frame.Event.Start.PID
			if background {
				// The pid is the whole answer for a detached start.
				// Reading on would block this turn for the length of
				// the job.
				return out, nil
			}
		case frame.Event.Data != nil:
			stdout.Write(frame.Event.Data.Stdout)
			stderr.Write(frame.Event.Data.Stderr)
		case frame.Event.End != nil:
			end := frame.Event.End
			out.Ended = true
			out.ExitCode = end.ExitCode
			if end.ExitCode >= 0 {
				if end.Error != "" && stderr.Len() == 0 {
					stderr.WriteString(end.Error)
				}
				continue
			}
			// NO EXIT CODE: the process the box waited on — the shell
			// a command runs as — was ended by a signal, which envd
			// reports as os.ProcessState does: -1 beside a status naming
			// the signal. Its Error is that same status, so it is not
			// copied in; the caller names the signal.
			out.Signal, out.SignalName = boxSignal(end.Status)
			out.Status = end.Status
		}
	}
	out.Stdout, out.Stderr = stdout.String(), stderr.String()
	return out, nil
}

// boxSignal is the signal a box's end event names in its status — the number
// and the box's own name for it — and 0 for a status that names none.
//
// envd describes a process a signal ended as os.ProcessState.String does:
// "signal: " and the signal's description, "(core dumped)" after it where one
// was, or "signal: signal 34" for a number Go has no description for.
func boxSignal(status string) (int, string) {
	name, ok := strings.CutPrefix(status, "signal: ")
	if !ok {
		return 0, ""
	}
	name = strings.TrimSuffix(name, " (core dumped)")
	if n, known := boxSignals[name]; known {
		return n, name
	}
	if digits, numbered := strings.CutPrefix(name, "signal "); numbered {
		if n, err := strconv.Atoi(digits); err == nil && n > 0 {
			return n, name
		}
	}
	return 0, ""
}

// boxSignals is each Linux signal's description, as Go — and so envd — names
// it, by the description: the only form an end event carries.
//
// The BOX's table rather than this process's syscall.Signal: a box is Linux
// whatever the engine host runs, and the two disagree off Linux — on darwin
// signal 10 is "bus error", in the box "user defined signal 1". A test built
// only on Linux holds this to Go's own table there, so the two cannot drift.
var boxSignals = map[string]int{
	"hangup": 1, "interrupt": 2, "quit": 3, "illegal instruction": 4,
	"trace/breakpoint trap": 5, "aborted": 6, "bus error": 7,
	"floating point exception": 8, "killed": 9, "user defined signal 1": 10,
	"segmentation fault": 11, "user defined signal 2": 12, "broken pipe": 13,
	"alarm clock": 14, "terminated": 15, "stack fault": 16, "child exited": 17,
	"continued": 18, "stopped (signal)": 19, "stopped": 20,
	"stopped (tty input)": 21, "stopped (tty output)": 22,
	"urgent I/O condition": 23, "CPU time limit exceeded": 24,
	"file size limit exceeded": 25, "virtual timer expired": 26,
	"profiling timer expired": 27, "window changed": 28, "I/O possible": 29,
	"power failure": 30, "bad system call": 31,
}

// maxEnvdFrame caps one streamed message.
//
// A coding agent's output is the largest thing that comes through here and it
// is line-shaped; 32 MiB is far past any real frame and small enough that a
// desynced length cannot exhaust the process.
const maxEnvdFrame = 32 << 20

// e2bFileIdleTimeout bounds how long one file transfer may go with NO byte
// moving in either direction before it is abandoned.
//
// A file transfer had no bound at all: the in-box client carries no overall
// deadline (see [envdClient]), and a /files call was the one envd exchange
// with no idle reader either, so a box whose envd accepted the connection and
// then said nothing held the caller for as long as the caller's context lived
// — which for a collection or a live reading is as long as its delivery or
// its viewer lasts. (The completion poll bounds each box's poll as a whole
// besides, and polls no box behind another — see [MaxConcurrentPolls].)
//
// MEASURED BETWEEN BYTES, not over the call, for the reason
// [e2bStreamIdleTimeout] gives: a large read on a slow link legitimately takes
// longer than any fixed bound. But a tenth of that bound, because unlike a
// command, a file transfer has no legitimate reason to fall silent — envd
// answers from a file that already exists. Sixty seconds is the control
// plane's whole-request budget ([E2BClientTimeout]), which is the engine's
// standing answer to "how long may E2B take to start answering".
const e2bFileIdleTimeout = E2BClientTimeout

// fileExchange makes one /files request whose every byte, sent or received,
// re-arms [e2bFileIdleTimeout] — the wait for the response's headers
// included, which is silence like any other. A request with no body passes
// nil.
//
// The caller owns both returns: it closes the response's body and stops the
// reader, which also releases the request's context.
func (c *envdClient) fileExchange(ctx context.Context, method, path string,
	body []byte, header http.Header,
) (*http.Response, *idleReader, error) {
	return c.exchange(ctx, method, c.host+"/files?"+filesQuery(path), body, header)
}

// exchange is one request to envd about the box's files — a /files transfer
// ([envdClient.fileExchange]) or a call of its filesystem service
// ([envdClient.filesystem]) — under the one bound every such request has:
// [e2bFileIdleTimeout] between bytes, re-armed by each one sent or received.
// The caller owns both returns, as [envdClient.fileExchange] says.
func (c *envdClient) exchange(ctx context.Context, method, target string,
	body []byte, header http.Header,
) (*http.Response, *idleReader, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	// ARMED NOW, before the request is sent, so a connection that is never
	// answered is abandoned exactly as one that stops mid-body is.
	idle := newIdleReader(nil, cancel, c.fileIdle)
	var reader io.Reader
	if body != nil {
		reader = sendProgress{inner: bytes.NewReader(body), idle: idle}
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target, reader)
	if err != nil {
		idle.stop()
		return nil, nil, err
	}
	if body != nil {
		// BOTH STATED, because the progress wrapper hides the buffer the
		// request would have taken them from. Without the length a body
		// goes out chunked — a different wire shape for a write that has
		// always carried its length — and without GetBody the transport
		// cannot send the body again: an HTTP/2 connection the server
		// closes with GOAWAY before reading the request is retried on a
		// fresh one only when the body can be had twice.
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(sendProgress{inner: bytes.NewReader(body), idle: idle}), nil
		}
	}
	for key, values := range header {
		req.Header[key] = values
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		idle.stop()
		return nil, nil, unanswered(ctx, idle.silent(err))
	}
	idle.inner = resp.Body
	return resp, idle, nil
}

// sendProgress re-arms an exchange's idle bound for every byte of the request
// body the transport takes, so a large upload is not abandoned while it is
// still moving.
type sendProgress struct {
	inner io.Reader
	idle  *idleReader
}

func (s sendProgress) Read(p []byte) (int, error) {
	n, err := s.inner.Read(p)
	if n > 0 {
		s.idle.timer.Reset(s.idle.idle)
	}
	return n, err
}

// readFile fetches a file from the box, WHOLE — see [Sandbox.ReadFile].
//
// EMPTY ON MISSING, not an error: the detached runner polls for a done marker
// and a result file that do not exist until the job finishes, so "not there
// yet" is the ordinary answer on most calls.
func (c *envdClient) readFile(ctx context.Context, path string) ([]byte, error) {
	resp, idle, err := c.fileExchange(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s: %w", path, err)
	}
	defer resp.Body.Close()
	defer idle.stop()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, envdRefused(resp.StatusCode, fmt.Errorf("e2b: read %s: %d: %s", path,
			resp.StatusCode, httpx.ReadRefusal(resp)))
	}
	raw, err := readCapped(idle, path, MaxFileBytes)
	if err != nil {
		return nil, unanswered(ctx, fmt.Errorf("e2b: read %s: %w", path, idle.silent(err)))
	}
	return raw, nil
}

// openFile streams a file out of the box — see [Sandbox.OpenFile]. The body
// IS the stream: envd serves the file as it is, and nothing here holds more
// of it than the caller's own read asks for.
func (c *envdClient) openFile(ctx context.Context, path string) (io.ReadCloser, error) {
	resp, idle, err := c.fileExchange(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("e2b: open %s: %w", path, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		idle.stop()
		_ = resp.Body.Close()
		return io.NopCloser(strings.NewReader("")), nil
	case resp.StatusCode >= 400:
		defer idle.stop()
		defer resp.Body.Close()
		return nil, envdRefused(resp.StatusCode, fmt.Errorf("e2b: open %s: %d: %s", path,
			resp.StatusCode, httpx.ReadRefusal(resp)))
	}
	return &envdStream{body: resp.Body, idle: idle, path: path,
		cancelled: func() bool { return cancelledBy(ctx) }}, nil
}

// envdStream is an open file's body, read through its idle bound.
type envdStream struct {
	body io.ReadCloser
	idle *idleReader
	path string
	// cancelled reports whether the caller that opened the file cancelled
	// it: an io.Reader's Read takes no context, and a read the caller
	// cancelled is no class at all ([unanswered]).
	cancelled func() bool
}

func (s *envdStream) Read(p []byte) (int, error) {
	n, err := s.idle.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = fmt.Errorf("e2b: read %s: %w", s.path, s.idle.silent(err))
		if !s.cancelled() {
			err = Because(WhyProviderUnavailable, err)
		}
	}
	return n, err
}

func (s *envdStream) Close() error {
	s.idle.stop()
	return s.body.Close()
}

// readTail reads the end of a file — see [Sandbox.ReadTail] — as an HTTP
// SUFFIX RANGE (`Range: bytes=-n`).
//
// envd serves /files through Go's http.ServeContent, which answers a range
// with 206 and the file's whole size in Content-Range; with Range set it also
// serves identity rather than gzip, so the bytes are the file's own
// (packages/envd/internal/api/download.go in e2b-dev/infra; ServeContent has
// been its writer since file handling landed, and the gzip support added
// beside it in February 2026 steps aside for any Range or conditional
// request). A server that ignores the range and answers 200 with the whole
// file is still answered correctly — its body is read through and only the
// last n bytes are kept — so the read is bounded in memory whatever the
// box's envd does, and only its cost differs.
func (c *envdClient) readTail(ctx context.Context, path string, n int) (FileTail, error) {
	want := max(n, 0)
	// A suffix range of zero is unsatisfiable, so the smallest real one is
	// asked for and dropped: what such a caller wants is the size.
	header := http.Header{"Range": {fmt.Sprintf("bytes=-%d", max(want, 1))}}
	resp, idle, err := c.fileExchange(ctx, http.MethodGet, path, nil, header)
	if err != nil {
		return FileTail{}, fmt.Errorf("e2b: read the end of %s: %w", path, err)
	}
	defer resp.Body.Close()
	defer idle.stop()

	var tail FileTail
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return FileTail{}, nil
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// Unsatisfiable only because the file is empty: Content-Range then
		// reads `bytes */0`.
		_, total, _ := parseContentRange(resp.Header.Get("Content-Range"))
		return FileTail{Size: max(total, 0)}, nil
	case resp.StatusCode == http.StatusPartialContent:
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		data, readErr := io.ReadAll(io.LimitReader(idle, int64(max(want, 1))))
		if readErr != nil {
			return FileTail{}, unanswered(ctx, fmt.Errorf(
				"e2b: read the end of %s: %w", path, idle.silent(readErr)))
		}
		if !ok || total < 0 {
			total = start + int64(len(data))
		}
		tail = FileTail{Data: data, Size: max(total, start+int64(len(data)))}
	case resp.StatusCode >= 400:
		return FileTail{}, envdRefused(resp.StatusCode, fmt.Errorf(
			"e2b: read the end of %s: %d: %s", path, resp.StatusCode, httpx.ReadRefusal(resp)))
	default:
		data, total, readErr := keepLast(idle, max(want, 1))
		if readErr != nil {
			return FileTail{}, unanswered(ctx, fmt.Errorf(
				"e2b: read the end of %s: %w", path, idle.silent(readErr)))
		}
		tail = FileTail{Data: data, Size: total}
	}
	if len(tail.Data) > want {
		tail.Data = tail.Data[len(tail.Data)-want:]
	}
	return tail, nil
}

// parseContentRange reads `bytes <start>-<end>/<total>` (or `bytes */<total>`):
// where the served range starts and the file's whole size, with ok false for
// a header that is absent or says neither. A total of `*` is unknown, answered
// as -1.
func parseContentRange(header string) (start, total int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(header), "bytes ")
	if !found {
		return 0, -1, false
	}
	served, size, found := strings.Cut(spec, "/")
	if !found {
		return 0, -1, false
	}
	total = -1
	if size != "*" {
		parsed, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			return 0, -1, false
		}
		total = parsed
	}
	if served == "*" {
		return 0, total, true
	}
	from, _, found := strings.Cut(served, "-")
	if !found {
		return 0, total, false
	}
	start, err := strconv.ParseInt(from, 10, 64)
	if err != nil {
		return 0, total, false
	}
	return start, total, true
}

// keepLast reads r to its end keeping only its last n bytes, and how many it
// read in all — the end of a stream in memory bounded by n, whatever its
// length.
func keepLast(r io.Reader, n int) ([]byte, int64, error) {
	buf := make([]byte, 0, 2*n)
	chunk := make([]byte, 32<<10)
	var total int64
	for {
		got, err := r.Read(chunk)
		total += int64(got)
		buf = append(buf, chunk[:got]...)
		if len(buf) > 2*n {
			// Slid down only once the buffer has doubled, so the copy is
			// paid once per n bytes read rather than once per chunk.
			buf = append(buf[:0], buf[len(buf)-n:]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, total, err
		}
	}
	if len(buf) > n {
		buf = buf[len(buf)-n:]
	}
	return buf, total, nil
}

// writeFile puts a file into the box WHOLE ([Sandbox.WriteFile]): uploaded
// under a fresh name beside the path, then moved over it.
//
// ENVD WRITES AN UPLOAD IN PLACE. Its /files handler opens the destination
// with O_TRUNC and copies the request's body into it as the body arrives
// (processFile in packages/envd/internal/api/upload.go, e2b-dev/infra), so a
// file uploaded over one a run is using — a seat's GitHub App token file, which
// the completion poll rewrites under the running job and its git credential
// helper reads at every call — was empty, and then part written, for as long
// as the body took to cross the network, and a push that read it then
// authenticated with nothing. Uploaded under another name, the file the box
// reads changes in one step: envd's own Move, which is a rename
// (packages/envd/internal/services/filesystem/move.go), so a reader opens the
// old file or the new one.
//
// THE STAGED FILE LIES BESIDE THE PATH, in the directory envd resolves the
// path in, so the rename never crosses a filesystem ([stagedBeside]). NONE
// OUTLIVES A WRITE THAT FAILED: an upload or a move that failed removes it
// ([envdClient.discard]).
//
// What is at the path is REPLACED, a link included: the box is the VM, so
// nothing written there reaches the engine host, and the rename writes through
// no link. The new file has the mode and owner envd gives every upload.
func (c *envdClient) writeFile(ctx context.Context, path string, content []byte) error {
	path = envdPath(path)
	staged := stagedBeside(path)
	if err := c.upload(ctx, staged, content); err != nil {
		return c.discard(ctx, staged, fmt.Errorf("e2b: write %s: %w", path, err))
	}
	if err := c.filesystem(ctx, "Move", envdMove{Source: staged, Destination: path}); err != nil {
		return c.discard(ctx, staged, fmt.Errorf("e2b: write %s: %w", path, err))
	}
	return nil
}

// upload puts content at path through envd's /files, which writes it there in
// place ([envdClient.writeFile] says why nothing the box reads is uploaded over).
func (c *envdClient) upload(ctx context.Context, path string, content []byte) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	// THE PART NAME IS THE PATH'S BASENAME and envd reads the destination
	// from the query rather than from the part — a form that named the
	// full path in the part would write to the wrong place with no error.
	part, err := form.CreateFormFile("file", baseName(path))
	if err != nil {
		return err
	}
	if _, writeErr := part.Write(content); writeErr != nil {
		return writeErr
	}
	if closeErr := form.Close(); closeErr != nil {
		return closeErr
	}

	header := http.Header{"Content-Type": {form.FormDataContentType()}}
	resp, idle, err := c.fileExchange(ctx, http.MethodPost, path, body.Bytes(), header)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	defer resp.Body.Close()
	defer idle.stop()
	if resp.StatusCode >= 400 {
		return envdRefused(resp.StatusCode, fmt.Errorf("upload: %d: %s", resp.StatusCode, httpx.ReadRefusal(resp)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(idle, 1<<20))
	return nil
}

// discard removes the file a write staged and could not move over its path,
// and returns failed — the write's own error — saying so where the removal
// failed too, since the staged file then holds the content in the box under a
// name nothing reads.
//
// A ROLLBACK, so it takes [context.WithoutCancel] of the write's context: the
// write often failed because that context ended, and a removal inheriting it
// would remove nothing. It is bounded as every envd file exchange is
// ([e2bFileIdleTimeout]). envd removes a path that is not there without
// complaint, so a staged file the failed upload never created costs one
// request.
func (c *envdClient) discard(ctx context.Context, staged string, failed error) error {
	err := c.filesystem(context.WithoutCancel(ctx), "Remove", envdRemove{Path: staged})
	if err != nil {
		return fmt.Errorf("%w (and the file staged for it, %s, could not be removed: %w)", failed, staged, err)
	}
	return failed
}

// stagedBeside is the name a write to path stages its content under: a fresh
// one in path's own directory, as envd resolves it.
//
// THE PATH'S LAST ELEMENT IS REPLACED AS WRITTEN, never cleaned: envd joins a
// relative or `~/` path onto the account's home and cleans only then, so the
// directory a lexical clean names here is not always the one envd writes the
// path in ([envdPath] keeps `a/../~/x` as written for that reason, and its
// directory is `a/../~/`, which envd resolves to <home>/~). A trailing slash
// is dropped first, as envd's join drops it from the path itself.
//
// UNGUESSABLE, as the local cells' staged files are ([hostbox.ReplaceFileIn]):
// the box's own processes write the directory, and a name one of them could
// make first would fail the upload, or be moved over the file in place of the
// write's own content.
func stagedBeside(path string) string {
	dir := ""
	if i := strings.LastIndex(strings.TrimRight(path, "/"), "/"); i >= 0 {
		dir = path[:i+1]
	}
	return dir + stagedPrefix + rand.Text()
}

// stagedPrefix begins the name a box file's new content is staged under before
// it replaces the file — on E2B as on the engine host ([hostbox.ReplaceFileIn]).
const stagedPrefix = ".crewlet-tmp-"

// envdMove and envdRemove are the requests of envd's filesystem Move and
// Remove, in the JSON Connect reads a protobuf message from
// (packages/envd/spec/filesystem/filesystem.proto in e2b-dev/infra).
type (
	envdMove struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	envdRemove struct {
		Path string `json:"path"`
	}
)

// filesystem makes one call of envd's filesystem service: a Connect UNARY
// call, in JSON, answered whole — unlike Process/Start, whose answer is a
// stream ([envdClient.start]). Only whether it succeeded is read.
//
// AS THE ACCOUNT EVERY /files CALL NAMES ([e2bEnvdUser]), by HTTP Basic: envd
// reads a Connect call's user from that header alone (AuthenticateUsername in
// packages/envd/internal/permissions/authenticate.go), and without it acts as
// the template's default user, in whose home a relative or `~/` path would
// then be resolved — another directory than the one the upload wrote, for a
// template whose default user is not that account.
func (c *envdClient) filesystem(ctx context.Context, method string, request any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("%s: encode: %w", strings.ToLower(method), err)
	}
	header := http.Header{
		"Content-Type":             {"application/json"},
		"Connect-Protocol-Version": {"1"},
		"Authorization":            {envdBasicUser},
	}
	resp, idle, err := c.exchange(ctx, http.MethodPost, c.host+"/filesystem.Filesystem/"+method, body, header)
	if err != nil {
		return fmt.Errorf("%s: %w", strings.ToLower(method), err)
	}
	defer resp.Body.Close()
	defer idle.stop()
	if resp.StatusCode >= 400 {
		return envdRefused(resp.StatusCode, fmt.Errorf("%s: %d: %s", strings.ToLower(method),
			resp.StatusCode, httpx.ReadRefusal(resp)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(idle, 1<<20))
	return nil
}

// envdBasicUser is the Authorization a call of envd's filesystem service
// carries: [e2bEnvdUser], with no password, which is how envd's Basic scheme
// names an account (the token that admits the call is X-Access-Token's).
var envdBasicUser = "Basic " + base64.StdEncoding.EncodeToString([]byte(e2bEnvdUser+":"))

// envdRefused is an answer of 400 or more from a box's agent, marked by what
// its status means where it means something ([statusWhy]): the box failing on
// its side, or the account at a limit. A refused access token is the box's own
// credential, handed to the engine at its create, and no person's to fix, so
// it is left for the caller to read as the fault it is.
func envdRefused(status int, err error) error {
	if why, ok := statusWhy(status, false); ok {
		return Because(why, err)
	}
	return err
}

// filesQuery is the path-and-user pair every /files call carries.
func filesQuery(path string) string {
	return url.Values{"path": {path}, "username": {e2bEnvdUser}}.Encode()
}

// authorize names the account a call acts as and presents the box's access
// token — the two headers EVERY envd request carries, which is why each one
// is built through here.
func (c *envdClient) authorize(req *http.Request) {
	// Sent as a header AND as the query parameter above, because envd has
	// read it from both across versions and a mismatch between the box's
	// envd and this build shows up as a permission error naming no user.
	req.Header.Set("X-User", e2bEnvdUser)
	// envd's own header for the token (packages/envd/internal/api/auth.go
	// in e2b-dev/infra). A secured box refuses a process call without it
	// and a file call without it or a signature; a header is the form both
	// accept, and the one E2B's SDKs send.
	req.Header.Set("X-Access-Token", c.token)
}

// envdPath is a path to write as every other cell writes it: an absolute one
// lexically clean, and every other form as envd itself reads it.
//
// ONE MEANING IN EVERY CELL. envd joins a relative or `~/` path onto the
// account's home, which cleans it, but takes an absolute path VERBATIM: it
// creates the path's parent and then writes the path, so `/home/user/.npmrc/`
// made `/home/user/.npmrc` a directory and was refused as one ("path is a
// directory"), while both local cells clean the same key through
// hostbox.InHome and write the file. The setup-file rule in internal/config
// cleans before it judges, so it admitted a key every e2b box then failed on.
// Cleaned here, an absolute key lands where the other forms do, and the
// upload's part is named for the file rather than for the empty element after
// a trailing slash.
//
// NEVER CLEANED ACROSS THE TILDE. To a lexical clean `~` is an ordinary
// element a following `..` removes: `~/../user/.npmrc` cleans to
// `user/.npmrc`, which envd joins onto the home as `/home/user/user/.npmrc`,
// where envd's own expansion — and the config rule, which resolves `~/` the
// same way — put it at `/home/user/.npmrc`. So a `~/` path keeps its prefix and
// only what follows it is cleaned, and any other form (a relative path, `~`,
// `~name`) goes to envd as written: its join cleans a relative path itself,
// and cleaning one here could even MAKE a tilde form, `a/../~/x` becoming the
// `~/x` envd would read as the home.
func envdPath(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return "~/" + path.Clean(rest)
	}
	return p
}

// baseName is the last path element, without importing path/filepath for a
// remote POSIX path — filepath is the HOST's separator, and on Windows it
// would split a Linux box's path on the wrong character.
func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

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
// Files are plain HTTP: a GET and a multipart POST on /files. Processes are
// Connect RPC, server-streaming because output arrives as it is produced; its
// envelope (a flag byte, then a big-endian uint32 length, before each JSON
// message) is implemented here rather than pulling in a Connect runtime and
// its protobuf toolchain for one small, stable, specified call shape.
//
// The stream is drained, not followed: a launch is detached and its result
// read later from a file, so a foreground command's frames fold into one
// [ExecResult], and a background command is abandoned once its first frame
// gives the PID.

// e2bEnvdUser is the account every E2B template runs as and every envd call
// acts for. envd takes it as a query parameter with no default: a call
// omitting it fails with a permission error naming no user, which reads as a
// broken template.
const e2bEnvdUser = "user"

// e2bStreamIdleTimeout bounds how long one exec may go with no output. It is
// measured between frames, never over the call: the engine imposes no run
// deadline and a foreground command may run for many minutes, while a stream
// silent this long is indistinguishable from a wedged envd. Ten minutes is
// past any gap a real command leaves (pulls, installs and test suites emit as
// they go) and short enough to notice a hung box the same hour.
const e2bStreamIdleTimeout = 10 * time.Minute

// idleReader abandons a stream that stops arriving by cancelling the request,
// not just failing the read: otherwise the transport keeps the connection and
// envd the process.
type idleReader struct {
	inner io.Reader
	timer idleTimer

	// idle is held so every reset re-arms the caller's budget, not the
	// package constant.
	idle   time.Duration
	cancel context.CancelFunc

	// fired tells an exchange this bound abandoned from one the caller's
	// context ended; both surface as "context canceled".
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

// newIdleReaderOn is newIdleReader with the countdown supplied, for the one
// assertion a real clock cannot make honestly: that a delivery re-arms the
// budget. An abandonment that has not happened has no signal to wait on, so
// against wall time the case could only sleep and check the timer stayed
// quiet, asserting the test goroutine's scheduling rather than this reader.
// That the countdown fires is asserted against a real timer: a slow machine
// only helps that direction.
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

// silent names an error this reader's bound caused as such: a bare "context
// canceled" reads as the caller's own cancellation.
func (r *idleReader) silent(err error) error {
	if err != nil && r.fired.Load() {
		return fmt.Errorf("envd moved no byte for %s, so the exchange was abandoned: %w", r.idle, err)
	}
	return err
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		// Reset on progress, not on entry: a read that blocked nine minutes
		// then delivered a byte is a live command.
		r.timer.Reset(r.idle)
	}
	return n, err
}

// stop releases the idle timer. The body is closed by whoever opened the
// response; a second owner would close it twice.
func (r *idleReader) stop() {
	r.timer.Stop()
	// Cancelled on the ordinary path too: a context never cancelled leaks
	// its resources for the life of the process.
	r.cancel()
}

// envdClient talks to one box's envd. Its HTTP client has no overall timeout:
// http.Client.Timeout covers reading the body too, so the control plane's
// bound would kill every longer command. [e2bStreamIdleTimeout] bounds a
// command, between frames.
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
// transport and dropping its deadline. It falls back to [httpx.Transport],
// not nil: nil is http.DefaultTransport, with two idle connections per host.
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

// e2bProcessResult is one command's outcome folded from the frames: each fact
// envd said about its end, kept apart so the caller reads a status off them
// rather than off one sentinel for every way of having none.
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

// connectDeadline is the code envd's stream closes with when the request's
// Connect-Timeout-Ms runs out: usually before the end event of the process it
// then kills, at best beside one reporting the kill.
const connectDeadline = "deadline_exceeded"

// connectEnd is the payload of a Connect stream's last frame.
type connectEnd struct {
	Error *connectError `json:"error,omitempty"`
}

// startRequest is envd's Process/Start body. envd takes an argv, and every
// caller hands over a shell line (pipes, redirects, `&&`), so the shell is
// named explicitly.
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

// startResponse is one frame of the Process/Start stream: a `start` event
// with the pid, any number of `data` events, then an `end` event. Every field
// is optional because each frame carries exactly one.
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

// start runs a command and folds the stream into one result. background
// abandons the stream once the pid is known; the process keeps running
// because envd owns it.
func (c *envdClient) start(ctx context.Context, cmd string, opts ExecOptions, background bool) (e2bProcessResult, error) {
	body, err := json.Marshal(newStartRequest(cmd, opts))
	if err != nil {
		return e2bProcessResult{}, fmt.Errorf("e2b: encode command: %w", err)
	}
	framed := make([]byte, connectEnvelope+len(body))
	binary.BigEndian.PutUint32(framed[1:connectEnvelope], uint32(len(body)))
	copy(framed[connectEnvelope:], body)

	// Its own cancel, so a silent stream is abandoned without ending the
	// turn's context.
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
		// envd's per-call cap, so a runaway command is stopped inside the
		// box, not merely abandoned here.
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
	// stop only stops the timer; the body is closed above, where a linter
	// sees it belongs to this response.
	defer stream.stop()
	if resp.StatusCode >= 400 {
		return e2bProcessResult{}, envdRefused(resp.StatusCode, fmt.Errorf("e2b: start: %d: %s",
			resp.StatusCode, httpx.ReadRefusalFrom(resp.Header.Get("Content-Type"), stream)))
	}
	return foldStream(ctx, stream, background)
}

// foldStream reads Connect frames and folds them into one result. A stream
// ending without an `end` event is not an error: envd closes it when a box is
// paused or reclaimed mid-command, which the completion model handles, so
// raising would fail a run on an ordinary pause. What the stream did say is
// kept apart, and the caller decides the command's status.
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
			// A framing desync; allocating it would make the desync an
			// out-of-memory.
			return out, fmt.Errorf(
				"e2b: stream frame claims %d bytes, which is past the %d-byte "+
					"cap — the connection is out of frame", length, maxEnvdFrame)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return out, unanswered(ctx, fmt.Errorf("e2b: read stream: %w", err))
		}
		if header[0]&connectEndFlag != 0 {
			// The trailer: its error is how envd reports a deadline.
			// Without an `end` event Ended stays false, which the caller
			// reads as no verdict, not success.
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
				// The pid is a detached start's whole answer; reading on
				// would block the turn for the job's length.
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
			// A signal ended the shell, which envd reports as
			// os.ProcessState does: -1 beside a status naming it. Error
			// repeats that status, so it is not copied; the caller names
			// the signal.
			out.Signal, out.SignalName = boxSignal(end.Status)
			out.Status = end.Status
		}
	}
	out.Stdout, out.Stderr = stdout.String(), stderr.String()
	return out, nil
}

// boxSignal is the signal number, and the box's name for it, that an end
// event's status names, and 0 for none. envd formats it as
// os.ProcessState.String: "signal: <description>", " (core dumped)" after it
// where one was, or "signal: signal 34" for a number Go cannot describe.
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

// boxSignals maps each Linux signal's Go description, the only form an end
// event carries, to its number. The box's table, not this host's
// syscall.Signal: a box is Linux whatever the engine runs on, and darwin's
// signal 10 is "bus error". A Linux-only test holds it to Go's table there.
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

// maxEnvdFrame caps one streamed message: 32 MiB is far past any frame of
// line-shaped harness output, and small enough that a desynced length cannot
// exhaust the process.
const maxEnvdFrame = 32 << 20

// e2bFileIdleTimeout bounds how long one file transfer may go with no byte
// moving either way. The in-box client has no overall deadline
// ([envdClient]), so without it an envd that accepted and fell silent would
// hold a collection or a live reading for its caller's whole life. (The
// completion poll also bounds each box's poll whole; see [MaxConcurrentPolls].)
//
// Between bytes, as [e2bStreamIdleTimeout], since a large read on a slow link
// outlasts any fixed bound; but a transfer has no reason to fall silent (envd
// answers from a file that exists), so it is the control plane's
// whole-request budget, [E2BClientTimeout]: how long E2B may take to answer.
const e2bFileIdleTimeout = E2BClientTimeout

// fileExchange makes one /files request under [e2bFileIdleTimeout], re-armed
// by every byte sent or received; the wait for headers counts as silence too.
// body is nil for none. The caller closes the response body and stops the
// reader, which also releases the request's context.
func (c *envdClient) fileExchange(ctx context.Context, method, path string,
	body []byte, header http.Header,
) (*http.Response, *idleReader, error) {
	return c.exchange(ctx, method, c.host+"/files?"+filesQuery(path), body, header)
}

// exchange is one request about the box's files, a /files transfer
// ([envdClient.fileExchange]) or a filesystem-service call
// ([envdClient.filesystem]), under [e2bFileIdleTimeout] between bytes. The
// caller owns both returns, as [envdClient.fileExchange] says.
func (c *envdClient) exchange(ctx context.Context, method, target string,
	body []byte, header http.Header,
) (*http.Response, *idleReader, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	// Armed before sending, so a connection never answered is abandoned
	// like one that stops mid-body.
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
		// Both set, since the progress wrapper hides the buffer they would
		// come from: without the length the body goes out chunked, and
		// without GetBody an HTTP/2 GOAWAY before the server read the
		// request cannot be retried on a fresh connection.
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(sendProgress{inner: bytes.NewReader(body), idle: idle}), nil
		}
	}
	return c.send(ctx, req, idle, header)
}

// streamExchange is [envdClient.exchange] for a body read once as it is sent
// ([envdClient.uploadStream]), under the same bound, re-armed by each byte the
// transport takes. Its length is unknown until read, so it goes out chunked;
// and it cannot be read twice, so a GOAWAY before the server read it fails
// the write rather than retrying. Making it again is the caller's: a carried
// session's restore is one launch step, which a failed launch repeats whole.
func (c *envdClient) streamExchange(ctx context.Context, method, target string,
	body io.Reader, header http.Header,
) (*http.Response, *idleReader, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	idle := newIdleReader(nil, cancel, c.fileIdle)
	req, err := http.NewRequestWithContext(reqCtx, method, target, sendProgress{inner: body, idle: idle})
	if err != nil {
		idle.stop()
		return nil, nil, err
	}
	return c.send(ctx, req, idle, header)
}

// send makes one request built by [envdClient.exchange] or
// [envdClient.streamExchange], its idle bound already armed, and hands the
// response body to that bound.
func (c *envdClient) send(ctx context.Context, req *http.Request, idle *idleReader,
	header http.Header,
) (*http.Response, *idleReader, error) {
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

// sendProgress re-arms an exchange's idle bound for every request-body byte
// the transport takes, so an upload still moving is not abandoned.
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

// readFile fetches a file from the box whole ([Sandbox.ReadFile]). Missing is
// empty, not an error: the detached runner polls for a done marker and a
// result file that exist only once the job finishes.
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

// openFile streams a file out of the box ([Sandbox.OpenFile]); the body is the
// stream, so nothing here holds more than the caller's read asks for.
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
	// cancelled reports whether the opener's context was cancelled, since
	// Read takes none; a cancelled read is no class ([unanswered]).
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

// openTail streams the end of a file ([Sandbox.OpenTail]) as an HTTP suffix
// range (`Range: bytes=-n`), so only the n bytes kept cross the WAN; envd
// answers 206 with the whole size in Content-Range ([envdClient.readTail]).
// A server that ignores the range and answers 200 is still answered right:
// with a length, the bytes before the last n are read and dropped; without
// one, the last n are kept in memory as they pass ([keepLast]). That envd
// costs the whole transfer either way.
func (c *envdClient) openTail(ctx context.Context, path string, n int64) (io.ReadCloser, int64, error) {
	if n <= 0 {
		// A zero suffix range is unsatisfiable; such a caller wants only
		// the size.
		tail, err := c.readTail(ctx, path, 0)
		if err != nil {
			return nil, 0, err
		}
		return io.NopCloser(strings.NewReader("")), tail.Size, nil
	}
	header := http.Header{"Range": {fmt.Sprintf("bytes=-%d", n)}}
	resp, idle, err := c.fileExchange(ctx, http.MethodGet, path, nil, header)
	if err != nil {
		return nil, 0, fmt.Errorf("e2b: open the end of %s: %w", path, err)
	}
	done := func() {
		idle.stop()
		_ = resp.Body.Close()
	}
	empty := io.NopCloser(strings.NewReader(""))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		done()
		return empty, 0, nil
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// Unsatisfiable only because the file is empty: Content-Range then
		// reads `bytes */0`.
		_, total, _ := parseContentRange(resp.Header.Get("Content-Range"))
		done()
		return empty, max(total, 0), nil
	case resp.StatusCode == http.StatusPartialContent:
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || total < 0 {
			done()
			return nil, 0, fmt.Errorf("e2b: open the end of %s: a range answered with Content-Range %q, "+
				"which names no whole size", path, resp.Header.Get("Content-Range"))
		}
		stream := &envdStream{body: resp.Body, idle: idle, path: path}
		return limitedStream{Reader: io.LimitReader(stream, min(n, total-start)), Closer: stream}, total, nil
	case resp.StatusCode >= 400:
		defer done()
		return nil, 0, fmt.Errorf("e2b: open the end of %s: %d: %s", path, resp.StatusCode,
			httpx.ReadRefusal(resp))
	case resp.ContentLength >= 0:
		stream := &envdStream{body: resp.Body, idle: idle, path: path}
		if skip := resp.ContentLength - n; skip > 0 {
			if _, err = io.CopyN(io.Discard, stream, skip); err != nil {
				_ = stream.Close()
				return nil, 0, err
			}
		}
		return limitedStream{Reader: io.LimitReader(stream, n), Closer: stream}, resp.ContentLength, nil
	}
	defer done()
	data, total, err := keepLast(idle, int(n))
	if err != nil {
		return nil, 0, fmt.Errorf("e2b: read the end of %s: %w", path, idle.silent(err))
	}
	return io.NopCloser(bytes.NewReader(data)), total, nil
}

// limitedStream is a stream read no further than its limit, closed whole.
type limitedStream struct {
	io.Reader
	io.Closer
}

// readTail reads the end of a file ([Sandbox.ReadTail]) as an HTTP suffix
// range (`Range: bytes=-n`). envd serves /files through http.ServeContent,
// which answers a range with 206 and the whole size in Content-Range, and
// with Range set serves identity rather than gzip, so the bytes are the
// file's own (packages/envd/internal/api/download.go in e2b-dev/infra). A
// server answering 200 is read through keeping only the last n bytes, so
// memory stays bounded whatever envd does; only the cost differs.
func (c *envdClient) readTail(ctx context.Context, path string, n int) (FileTail, error) {
	want := max(n, 0)
	// A zero suffix range is unsatisfiable, so the smallest real one is
	// asked for and dropped: such a caller wants the size.
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
// read in all, in memory bounded by n whatever the stream's length.
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

// writeFile puts a file into the box whole ([Sandbox.WriteFile]): uploaded
// under a fresh name beside the path, then moved over it. envd writes an
// upload in place, truncating first (processFile in
// packages/envd/internal/api/upload.go, e2b-dev/infra), so a file a run reads,
// such as a seat's GitHub App token file, would be empty or partial while the
// body crossed; envd's Move is a rename
// (packages/envd/internal/services/filesystem/move.go), so a reader sees old
// or new. Staged beside the path, the rename never crosses a filesystem
// ([stagedBeside]); a failed upload or move removes it ([envdClient.discard]).
// A link at the path is replaced, not written through, and nothing reaches the
// engine host: the box is the VM. The file gets envd's upload mode and owner.
func (c *envdClient) writeFile(ctx context.Context, path string, content []byte) error {
	return c.replace(ctx, path, func(staged string) error { return c.upload(ctx, staged, content) })
}

// writeStream is [envdClient.writeFile] for content read as it is uploaded
// ([Sandbox.WriteStream]): the same staged name, move and removal of what a
// failure staged, the upload's body streamed ([envdClient.uploadStream]).
func (c *envdClient) writeStream(ctx context.Context, path string, content io.Reader) error {
	return c.replace(ctx, path, func(staged string) error { return c.uploadStream(ctx, staged, content) })
}

// replace is a write's two steps: upload puts the content at a fresh name
// beside path, and envd's Move puts that over path — or, either failing, the
// staged file is removed ([envdClient.discard]).
func (c *envdClient) replace(ctx context.Context, path string, upload func(staged string) error) error {
	path = envdPath(path)
	staged := stagedBeside(path)
	if err := upload(staged); err != nil {
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
	// The part takes the basename: envd reads the destination from the
	// query, and a full path in the part would write elsewhere silently.
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
	return uploaded(resp, idle)
}

// uploadStream is [envdClient.upload] with the form written through a pipe
// into the request body, so nothing here holds more than one piece of the
// content, never the whole archive.
//
// The form is written on a goroutine the call stops and waits for before
// returning: closing the pipe's reading end makes a writer the transport
// stopped reading (envd answered early, or the exchange went silent) fail its
// next write, so content is never read after the call returns. A source that
// fails part way closes the pipe with its error, the request ends short of the
// form's last boundary, and the staged file is removed. A source already
// blocked in a read is its own to end; one read under the call's context
// ends with it.
func (c *envdClient) uploadStream(ctx context.Context, path string, content io.Reader) error {
	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)
	header := http.Header{"Content-Type": {form.FormDataContentType()}}
	written := make(chan struct{})
	go func() {
		defer close(written)
		pw.CloseWithError(writeForm(form, baseName(path), readUnder(ctx, content)))
	}()
	resp, idle, err := c.streamExchange(ctx, http.MethodPost, c.host+"/files?"+filesQuery(path), pr, header)
	pr.CloseWithError(errUploadEnded)
	<-written
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return uploaded(resp, idle)
}

// errUploadEnded is what a streamed upload's form writer meets once the
// exchange it was writing for has ended ([envdClient.uploadStream]).
var errUploadEnded = errors.New("the upload's exchange has ended")

// writeForm writes content as an upload's one file part, named name — the
// destination is the query's, never the part's — and closes the form.
func writeForm(form *multipart.Writer, name string, content io.Reader) error {
	part, err := form.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, copyErr := io.Copy(part, content); copyErr != nil {
		return copyErr
	}
	return form.Close()
}

// uploaded reads an upload's answer: refused at 400 or more, its body drained
// up to a bound otherwise, and released either way.
func uploaded(resp *http.Response, idle *idleReader) error {
	defer resp.Body.Close()
	defer idle.stop()
	if resp.StatusCode >= 400 {
		return envdRefused(resp.StatusCode, fmt.Errorf("upload: %d: %s", resp.StatusCode, httpx.ReadRefusal(resp)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(idle, 1<<20))
	return nil
}

// discard removes the file a write staged and could not move over its path,
// and returns failed, the write's own error, noting a failed removal too: the
// staged file then holds the content under a name nothing reads.
//
// A rollback, so under [context.WithoutCancel]: the write often failed because
// its context ended. It is bounded like every envd file exchange
// ([e2bFileIdleTimeout]), and envd removes a missing path without complaint.
func (c *envdClient) discard(ctx context.Context, staged string, failed error) error {
	err := c.filesystem(context.WithoutCancel(ctx), "Remove", envdRemove{Path: staged})
	if err != nil {
		return fmt.Errorf("%w (and the file staged for it, %s, could not be removed: %w)", failed, staged, err)
	}
	return failed
}

// stagedBeside is the name a write to path stages its content under: a fresh
// one in path's own directory, as envd resolves it. The last element is
// replaced as written, never cleaned: envd joins a relative or `~/` path onto
// the home and cleans only then, so a lexical clean here could name another
// directory ([envdPath] keeps `a/../~/x` as written, whose directory envd
// resolves to <home>/~). A trailing slash is dropped first, as envd's join
// drops it.
//
// Unguessable, as [hostbox.ReplaceFileIn]'s staged files are: the box's own
// processes write the directory, and a name one of them made first would fail
// the upload or be moved over the file in its place.
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

// filesystem makes one unary Connect call of envd's filesystem service, in
// JSON, answered whole (unlike [envdClient.start]'s stream); only success is
// read. It acts as [e2bEnvdUser] by HTTP Basic, the only header envd reads a
// Connect call's user from (AuthenticateUsername in
// packages/envd/internal/permissions/authenticate.go): otherwise envd acts as
// the template's default user and resolves a relative or `~/` path in that
// user's home, not the one the upload wrote to.
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

// envdRefused classes an answer of 400 or more from a box's agent by its
// status where that means something ([statusWhy]): the box failing, or the
// account at a limit. A refused access token is the box's own credential, no
// person's to fix, so it is left for the caller to read as the fault it is.
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

// authorize sets the two headers every envd request carries: the account it
// acts as and the box's access token.
func (c *envdClient) authorize(req *http.Request) {
	// Also sent as the query parameter ([filesQuery]): envd has read it from
	// each across versions, and a miss shows as a permission error naming
	// no user.
	req.Header.Set("X-User", e2bEnvdUser)
	// envd's token header (packages/envd/internal/api/auth.go in
	// e2b-dev/infra): a secured box refuses a process call without it and
	// a file call without it or a signature, so the header serves both.
	req.Header.Set("X-Access-Token", c.token)
}

// envdPath is a path to write as every other cell writes it: an absolute one
// lexically clean, every other form as envd itself reads it.
//
// envd writes an absolute path verbatim, so `/home/user/.npmrc/` would be
// refused as a directory where the local cells (hostbox.InHome) and
// internal/config's setup-file rule clean it to the file.
//
// Never cleaned across the tilde: `~/../user/.npmrc` would clean to
// `user/.npmrc`, which envd puts at /home/user/user/.npmrc. So `~/` keeps its
// prefix and only the rest is cleaned; any other form goes as written, since
// envd's join cleans a relative path and a clean here could make a tilde form
// (`a/../~/x` to `~/x`).
func envdPath(p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return "~/" + path.Clean(rest)
	}
	return p
}

// baseName is the last element of a box's POSIX path: path/filepath splits on
// the host's separator, the wrong character on Windows.
func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

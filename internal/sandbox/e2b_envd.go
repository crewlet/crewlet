package sandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
// The engine's own model of a coding run is DETACHED: a job is started, the
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
	timer *time.Timer

	// idle is HELD, because the reset on progress needs the same value the
	// first arming used. Reading the package constant there instead made
	// the parameter a lie: a caller passing anything else got its budget
	// once and ten minutes on every read after it.
	idle   time.Duration
	cancel context.CancelFunc
}

func newIdleReader(body io.Reader, cancel context.CancelFunc, idle time.Duration) *idleReader {
	r := &idleReader{inner: body, idle: idle, cancel: cancel}
	r.timer = time.AfterFunc(idle, cancel)
	return r
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
}

// newEnvdClient derives envd's client from the caller's, keeping its
// transport and dropping its deadline.
//
// The transport falls back to [httpx.Transport] rather than to nil, which
// would be http.DefaultTransport and its two idle connections per host —
// the one thing every other client here was moved off.
func newEnvdClient(host string, from *http.Client) *envdClient {
	client := &http.Client{Transport: httpx.Transport()}
	if from != nil {
		if from.Transport != nil {
			client.Transport = from.Transport
		}
		client.CheckRedirect = from.CheckRedirect
		client.Jar = from.Jar
	}
	return &envdClient{host: host, http: client}
}

// connectEnvelope is the five-byte prefix Connect puts before each streamed
// message: a flag byte, then a big-endian uint32 length.
const connectEnvelope = 5

// connectEndFlag marks the final frame of a Connect stream, whose payload is
// the trailer rather than a message.
const connectEndFlag = 0x02

// e2bProcessResult is one command's outcome, folded from the frames.
type e2bProcessResult struct {
	PID      int
	ExitCode int
	Stdout   string
	Stderr   string
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
	setEnvdUser(req)

	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return e2bProcessResult{}, fmt.Errorf("e2b: start: %w", err)
	}
	defer resp.Body.Close()
	stream := newIdleReader(resp.Body, cancel, e2bStreamIdleTimeout)
	// The reader's own close only stops the idle timer; the body is closed
	// above, where a linter can see it belongs to this response.
	defer stream.stop()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(stream, 2048))
		return e2bProcessResult{}, fmt.Errorf("e2b: start: %d: %s",
			resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return foldStream(stream, background)
}

// foldStream reads Connect frames and folds them into one result.
//
// A STREAM THAT ENDS WITHOUT AN `end` EVENT IS NOT AN ERROR HERE. envd closes
// the connection when a box is paused or reclaimed mid-command, and the
// engine's own completion model already handles a job whose box went away —
// raising instead would turn an ordinary pause into a failed run.
func foldStream(r io.Reader, background bool) (e2bProcessResult, error) {
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
			return out, fmt.Errorf("e2b: read stream: %w", err)
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
			return out, fmt.Errorf("e2b: read stream: %w", err)
		}
		if header[0]&connectEndFlag != 0 {
			// The trailer. A stream that ends here without an `end`
			// event leaves ExitCode at -1, which the caller reads as
			// "no verdict" rather than as success.
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
				// the coding run.
				return out, nil
			}
		case frame.Event.Data != nil:
			stdout.Write(frame.Event.Data.Stdout)
			stderr.Write(frame.Event.Data.Stderr)
		case frame.Event.End != nil:
			out.ExitCode = frame.Event.End.ExitCode
			if e := frame.Event.End.Error; e != "" && stderr.Len() == 0 {
				stderr.WriteString(e)
			}
		}
	}
	out.Stdout, out.Stderr = stdout.String(), stderr.String()
	return out, nil
}

// maxEnvdFrame caps one streamed message.
//
// A coding agent's output is the largest thing that comes through here and it
// is line-shaped; 32 MiB is far past any real frame and small enough that a
// desynced length cannot exhaust the process.
const maxEnvdFrame = 32 << 20

// maxEnvdFile caps one file read back out of a box, and REFUSES past it.
//
// Separate from maxEnvdFrame, which guards a desynced stream length: this
// bounds a whole file read into engine memory, and the two answer different
// questions even at the same number.
//
// The refusal is the point. io.LimitReader stops at its cap and reports a
// clean EOF, so a file of exactly the cap cannot be told from one that was
// clipped there — and the files this reads are a run's report and its stderr,
// which is precisely the content nothing downstream can sanity-check. A
// silently halved report reads as a finished one.
const maxEnvdFile = 32 << 20

// readFile fetches a file from the box.
//
// EMPTY ON MISSING, not an error: the detached runner polls for a done marker
// and a result file that do not exist until the job finishes, so "not there
// yet" is the ordinary answer on most calls.
func (c *envdClient) readFile(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.host+"/files?"+filesQuery(path), nil)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s: %w", path, err)
	}
	setEnvdUser(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("e2b: read %s: %d: %s", path,
			resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	// +1 so an overrun is visible; see maxEnvdFile.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEnvdFile+1))
	if err != nil {
		return nil, fmt.Errorf("e2b: read %s: %w", path, err)
	}
	if len(raw) > maxEnvdFile {
		return nil, fmt.Errorf(
			"e2b: %s is larger than the %d-byte cap this engine reads back from "+
				"a box, so it was not read — the coding agent wrote more than a "+
				"report, and a clipped one would be indistinguishable from a "+
				"finished one", path, maxEnvdFile)
	}
	return raw, nil
}

// writeFile puts a file into the box.
func (c *envdClient) writeFile(ctx context.Context, path string, content []byte) error {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	// THE PART NAME IS THE PATH'S BASENAME and envd reads the destination
	// from the query rather than from the part — a form that named the
	// full path in the part would write to the wrong place with no error.
	part, err := form.CreateFormFile("file", baseName(path))
	if err != nil {
		return fmt.Errorf("e2b: write %s: %w", path, err)
	}
	if _, writeErr := part.Write(content); writeErr != nil {
		return fmt.Errorf("e2b: write %s: %w", path, writeErr)
	}
	if closeErr := form.Close(); closeErr != nil {
		return fmt.Errorf("e2b: write %s: %w", path, closeErr)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.host+"/files?"+filesQuery(path), &body)
	if err != nil {
		return fmt.Errorf("e2b: write %s: %w", path, err)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	setEnvdUser(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("e2b: write %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("e2b: write %s: %d: %s", path,
			resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// filesQuery is the path-and-user pair every /files call carries.
func filesQuery(path string) string {
	return url.Values{"path": {path}, "username": {e2bEnvdUser}}.Encode()
}

// setEnvdUser names the account a call acts as.
func setEnvdUser(req *http.Request) {
	// Sent as a header AND as the query parameter above, because envd has
	// read it from both across versions and a mismatch between the box's
	// envd and this build shows up as a permission error naming no user.
	req.Header.Set("X-User", e2bEnvdUser)
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

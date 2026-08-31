package cliagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// termGrace is how long a terminated CLI has to exit before it is killed.
//
// The timeout has already fired, so the call is lost either way; this decides
// only whether the tree gets to flush. Five seconds is enough for a Node
// runtime to run its exit handlers and close its sockets, and short enough
// that a wedged child does not hold the seat's concurrency slot for a
// meaningful fraction of the next call. Same figure the sandbox uses for the
// same reason.
const termGrace = 5 * time.Second

// stderrTail is how many lines of a failed child's stderr are reported.
//
// A CLI that crashes on startup prints a stack trace; the first line names
// the failure and the rest names the vendor's own internals. Fifty lines
// carries a real Node or Rust trace intact while keeping one bad provider out
// of the log budget for every other seat. Same figure the MCP supervisor uses.
const stderrTail = 50

// maxOutput bounds what is read from a child's stdout and stderr.
//
// A CLI told to stream JSONL can emit tens of megabytes for a long run, and
// all of it is held in memory to be parsed. 32 MiB is far above any real
// completion — the largest observed is under 2 MiB — and far below what would
// put the engine under memory pressure with max_concurrent processes running.
const maxOutput = 32 << 20

// invocation is one CLI call, fully resolved.
type invocation struct {
	binary  string
	args    []string
	stdin   string
	dir     string
	env     []string
	timeout time.Duration

	// onLine, when set, receives each COMPLETE line of stdout as the child
	// writes it, before the call returns. Nil takes the ordinary path where
	// stdout is only read after the process exits.
	//
	// Called from the goroutine os/exec runs the output copier on — one
	// goroutine, in order, and cmd.Wait does not return until it has
	// finished, so it never runs concurrently with the caller's own use of
	// the result.
	onLine func(string)
}

// lineTee forwards complete lines to a callback while passing every byte
// through to the real sink.
//
// A CLI's stdout arrives in whatever chunks the pipe hands over, which split
// mid-line, so a consumer that wants JSONL events has to reassemble them. The
// tail after the last newline is deliberately NOT delivered: a partial JSON
// object is not parseable, and the buffered copy is what the extractor reads
// once the process exits, so nothing is lost by waiting.
type lineTee struct {
	sink io.Writer
	on   func(string)
	buf  []byte
}

func (t *lineTee) Write(b []byte) (int, error) {
	n, err := t.sink.Write(b)
	if n > 0 {
		t.buf = append(t.buf, b[:n]...)
		for {
			i := bytes.IndexByte(t.buf, '\n')
			if i < 0 {
				break
			}
			line := string(t.buf[:i])
			t.buf = t.buf[i+1:]
			t.on(line)
		}
	}
	return n, err
}

// rawResult is what a child produced.
type rawResult struct {
	stdout   string
	stderr   string
	exitCode int
	// timedOut reports that the wall-clock cap fired, which is a different
	// fact from the context the caller passed being cancelled.
	timedOut bool
}

// run executes one invocation, terminating the whole process TREE on timeout.
//
// The tree, not the process: a coding CLI is a launcher over a Node or Bun
// runtime that forks helpers, and signalling only the process Go started
// leaves those holding the seat's memory and its network sockets.
func run(ctx context.Context, in invocation) (*rawResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, in.timeout)
	defer cancel()

	cmd := exec.CommandContext(callCtx, in.binary, in.args...) //nolint:gosec // binary and args come from a validated profile
	cmd.Dir = in.dir
	cmd.Env = in.env
	if in.stdin != "" {
		cmd.Stdin = strings.NewReader(in.stdin)
	}
	var stdout, stderr cappedBuffer
	stdout.limit = maxOutput
	stderr.limit = maxOutput
	cmd.Stdout = io.Writer(&stdout)
	if in.onLine != nil {
		cmd.Stdout = &lineTee{sink: &stdout, on: in.onLine}
	}
	cmd.Stderr = &stderr
	procgroup.Set(cmd)

	// Cancel signals the GROUP rather than letting os/exec signal the one
	// process it knows about, and WaitDelay bounds how long a child that
	// ignored SIGTERM can hold the call open.
	cmd.Cancel = func() error { return procgroup.Terminate(cmd.Process.Pid) }
	cmd.WaitDelay = termGrace

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cli-agent: starting %q: %w", in.binary, err)
	}
	pgid := cmd.Process.Pid
	waitErr := cmd.Wait()

	res := &rawResult{
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		timedOut: errors.Is(callCtx.Err(), context.DeadlineExceeded),
	}
	if dropped := stdout.Truncated() + stderr.Truncated(); dropped > 0 {
		// Said out loud rather than swallowed: a truncated stream is
		// how a usage figure or a closing fence goes missing, and an
		// operator debugging a mangled answer needs to know the output
		// was cut rather than malformed.
		log.WarnContext(ctx, "cli_agent_output_truncated", "binary", in.binary,
			"dropped_bytes", dropped, "limit_bytes", maxOutput)
	}
	if res.timedOut {
		// The group was signalled by Cancel and given WaitDelay to go;
		// anything still alive after Wait returned is a survivor that
		// ignored SIGTERM, and it holds a concurrency slot until killed.
		if err := procgroup.Kill(pgid); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			log.WarnContext(ctx, "cli_agent_group_kill_failed", "pgid", pgid, "error", err)
		}
	}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		return res, nil
	case errors.As(waitErr, &exitErr):
		res.exitCode = exitErr.ExitCode()
		return res, nil
	default:
		return res, fmt.Errorf("cli-agent: running %q: %w", in.binary, waitErr)
	}
}

// cappedBuffer accumulates output up to a limit and then drops the rest.
//
// Dropping rather than failing the call: a CLI whose output overran the cap
// has almost certainly already emitted its answer, and turning that into an
// error would lose a completion the operator paid for. The overrun is
// reported by [cappedBuffer.Truncated] so the caller can say so.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.dropped += len(p) - room
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// Truncated is how many bytes were dropped past the cap.
func (c *cappedBuffer) Truncated() int { return c.dropped }

// tail returns the last [stderrTail] lines of s, with a marker when lines
// were dropped.
//
// The depth is a constant rather than a parameter because there is one honest
// answer to "how much of a crashing CLI's stderr does an operator need", and
// every caller here is reporting exactly that; a per-call number would be a
// knob nobody has a reason to set differently.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= stderrTail {
		return strings.Join(lines, "\n")
	}
	kept := lines[len(lines)-stderrTail:]
	return fmt.Sprintf("… %d earlier lines omitted …\n%s",
		len(lines)-stderrTail, strings.Join(kept, "\n"))
}

// extract reads a CLI's stdout into the answer and its token counts.
type extracted struct {
	text       string
	input      int
	output     int
	cacheRead  int
	cacheWrite int
	// reported is false when no usage path resolved, so the counts are
	// estimates. `crewlet llm doctor` prints which of the two a provider
	// gets, because a budget built on estimates is a different promise.
	reported bool
	// failed is the CLI's own is_error flag, for a vendor that reports a
	// failure inside a successful exit.
	failed bool
}

// extract pulls the answer and the usage out of one CLI's stdout.
func extract(p Profile, stdout string) extracted {
	switch p.output() {
	case OutputText:
		return extracted{text: strings.TrimSpace(stdout)}
	case OutputJSONL:
		return extractStream(p, stdout)
	default:
		return extractObject(p, stdout)
	}
}

// extractObject reads one JSON document.
//
// A CLI that printed a banner before its JSON is common enough to handle
// here: the outermost braces are tried when the whole of stdout does not
// parse, which costs nothing and saves an operator an override.
func extractObject(p Profile, stdout string) extracted {
	doc, ok := decodeObject(strings.TrimSpace(stdout))
	if !ok {
		if bare, found := outermostObject(stdout); found {
			doc, ok = decodeObject(bare)
		}
	}
	if !ok {
		// Not JSON at all: the CLI printed prose, which is still an
		// answer. Reporting an unparseable-output error here would fail
		// a turn over a vendor's banner.
		return extracted{text: strings.TrimSpace(stdout)}
	}
	out := extracted{text: firstString(doc, p.TextPaths)}
	if out.text == "" {
		out.text = strings.TrimSpace(stdout)
	}
	out.failed = firstBool(doc, p.ErrorPaths)
	out.input, out.reported = firstInt(doc, p.Usage.Input)
	if got, ok := firstInt(doc, p.Usage.Output); ok {
		out.output, out.reported = got, true
	}
	out.cacheRead, _ = firstInt(doc, p.Usage.CacheRead)
	out.cacheWrite, _ = firstInt(doc, p.Usage.CacheWrite)
	return out
}

// extractStream reads a JSONL event stream.
//
// Text CONCATENATES in stream order — an event stream spells one answer
// across several events — while usage figures take the LAST value found,
// because a stream reports a running total and the final one is the total.
func extractStream(p Profile, stdout string) extracted {
	var out extracted
	var text strings.Builder
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		doc, ok := decodeObject(line)
		if !ok {
			continue
		}
		if chunk := firstString(doc, p.TextPaths); chunk != "" {
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(chunk)
		}
		if firstBool(doc, p.ErrorPaths) {
			out.failed = true
		}
		if got, ok := firstInt(doc, p.Usage.Input); ok {
			out.input, out.reported = got, true
		}
		if got, ok := firstInt(doc, p.Usage.Output); ok {
			out.output, out.reported = got, true
		}
		if got, ok := firstInt(doc, p.Usage.CacheRead); ok {
			out.cacheRead = got
		}
		if got, ok := firstInt(doc, p.Usage.CacheWrite); ok {
			out.cacheWrite = got
		}
	}
	out.text = strings.TrimSpace(text.String())
	if out.text == "" {
		// A stream whose text events this profile does not recognise is
		// still better reported as its raw output than as an empty
		// answer — the operator can see the shape and write an override.
		out.text = strings.TrimSpace(stdout)
	}
	return out
}

func decodeObject(s string) (map[string]any, bool) {
	if s == "" || s[0] != '{' {
		return nil, false
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, false
	}
	return doc, true
}

// lookup walks one path into a decoded document.
func lookup(doc map[string]any, path Path) (any, bool) {
	var current any = doc
	for _, step := range path {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[step]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			idx, err := strconv.Atoi(step)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			current = node[idx]
		default:
			return nil, false
		}
	}
	return current, true
}

func firstString(doc map[string]any, paths []Path) string {
	for _, path := range paths {
		v, ok := lookup(doc, path)
		if !ok {
			continue
		}
		if s, isString := v.(string); isString && s != "" {
			return s
		}
	}
	return ""
}

func firstBool(doc map[string]any, paths []Path) bool {
	for _, path := range paths {
		v, ok := lookup(doc, path)
		if !ok {
			continue
		}
		if b, isBool := v.(bool); isBool {
			return b
		}
	}
	return false
}

// firstInt reads a token count, accepting the float64 every JSON number
// decodes to as well as a string, which some CLIs emit for large counts.
func firstInt(doc map[string]any, paths []Path) (int, bool) {
	for _, path := range paths {
		v, ok := lookup(doc, path)
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

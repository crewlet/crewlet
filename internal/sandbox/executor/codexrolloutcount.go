package executor

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT CODEX COUNTS A SESSION AT BEFORE ITS NEXT REQUEST, read off the rollout
// a capture packed ([codexCounting.line]). The next request of a carried
// session always opens a turn (`exec resume` is one), and before a turn Codex
// compares the newest token_count's last_token_usage.total_tokens with its
// limit, adding neither the new input nor the items since (CODEX-MEASURE §16.4,
// 0.171.0). The estimate of items since is mid-turn alone, which no count read
// meets: a copy cut off mid-turn is not counted ([Codex.threadTraits]).
// A rollout with no such figure, or a compaction after the newest, holds what
// no rollout states (the system prompt and tools), so the count is marked.

// codexChargeBytes is the fewest bytes Codex's own estimate counts as a
// token: it estimates a text at its UTF-8 length over four, rounded up
// (CODEX-MEASURE §16.4).
const codexChargeBytes sandbox.Charge = 4

// codexCountRead reports whether release is one whose count rule this build
// read: a SET, never a range, as each harness's is ([openCodeCountRead]).
func codexCountRead(release string) bool { return release == codexCarryRelease }

// codexCounting is one walk's account of a rollout for the count, the meter's
// figure and the window ([codexCounting.line]).
type codexCounting struct {
	// anchored says a token_count stated Codex's figure, base, and compacted
	// that a compaction ended after it.
	anchored, compacted bool
	base                int64
	// figure is the input of the newest request that stated one; window the
	// least raw window Codex reported ([codexRawWindow]).
	figure, window int64
	// restart names the newest compaction, "" for none.
	restart string
}

// codexRawWindow is the model's own window that a rollout's
// model_context_window states: Codex reports 95% of it, and compacts at 90%
// of it unless a limit is configured (CODEX-MEASURE §16.4). Rounded down, so
// a window read is never above the model's; zero for none.
func codexRawWindow(reported int64) int64 {
	if reported <= 0 {
		return 0
	}
	return reported * 20 / 19
}

// codexCountLine is what the count reads of one rollout line on the notes walk
// ([codexCountLine.read]), each member by its whole-line decode's field
// ([fieldKind]): its type and ordinal, and its payload's type, last usage,
// window and window id. A line one member refuses is read as none. A window
// id past [transcriptNameMaxBytes] reads as none, so its compaction is named
// by its ordinal, as one with no id is.
type codexCountLine struct {
	typ          string
	ordinal      int64
	took         bool
	payloadType  string
	info, last   bool
	input, total int64
	window       int64
	windowID     string
}

// read reads the line's members, its opening brace current.
func (l *codexCountLine) read(w *lineWalk) error {
	*l = codexCountLine{took: true}
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.typ, ok, err = w.wordField(k)
		case "ordinal":
			l.ordinal, ok, err = w.intField(k)
		case "payload":
			l.payloadType, l.info, l.last, l.input, l.total, l.window, l.windowID = "", false, false, 0, 0, 0, ""
			ok, err = w.objectField(k, l.payloadMember(w))
		default:
			return w.scan.Skip()
		}
		l.took = l.took && ok
		return err
	})
}

// payloadMember reads one member of the line's payload.
func (l *codexCountLine) payloadMember(w *lineWalk) func(name string, k jsonscan.Kind) error {
	return func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.payloadType, ok, err = w.wordField(k)
		case "window_id":
			l.windowID, ok, err = w.wordField(k)
		case "info":
			l.info, l.last, l.input, l.total, l.window = k == jsonscan.ObjectStart, false, 0, 0, 0
			ok, err = w.objectField(k, l.infoMember(w))
		default:
			return w.scan.Skip()
		}
		l.took = l.took && ok
		return err
	}
}

// infoMember reads one member of a token count's info.
func (l *codexCountLine) infoMember(w *lineWalk) func(name string, k jsonscan.Kind) error {
	return func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "model_context_window":
			l.window, ok, err = w.intField(k)
		case "last_token_usage":
			l.last, l.input, l.total = k == jsonscan.ObjectStart, 0, 0
			ok, err = w.objectField(k, l.usageMember(w))
		default:
			return w.scan.Skip()
		}
		l.took = l.took && ok
		return err
	}
}

// usageMember reads one member of a token count's last usage.
func (l *codexCountLine) usageMember(w *lineWalk) func(name string, k jsonscan.Kind) error {
	return func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "input_tokens":
			l.input, ok, err = w.intField(k)
		case "total_tokens":
			l.total, ok, err = w.intField(k)
		default:
			return w.scan.Skip()
		}
		l.took = l.took && ok
		return err
	}
}

// line takes one rollout line: THE ONE RULE of Codex's count, as measured
// (CODEX-MEASURE §16.4): the newest token_count's last total, per request and
// never the cumulative one, rewritten by Codex as its estimate of the history
// a compaction replaced.
func (c *codexCounting) line(l *codexCountLine) {
	switch {
	case l.typ == "compacted":
		c.compacted = true
		c.restart = l.windowID
		if c.restart == "" {
			c.restart = "ordinal-" + strconv.FormatInt(l.ordinal, 10)
		}
	case l.typ == "event_msg" && l.payloadType == "token_count" && l.info && l.last:
		c.anchored, c.compacted, c.base = true, false, max(l.total, 0)
		if l.input > 0 {
			c.figure = l.input
		}
		if w := codexRawWindow(l.window); w > 0 && (c.window == 0 || w < c.window) {
			c.window = w
		}
	}
}

// count is the count c read, under marks the read set before the walk.
func (c *codexCounting) count(marks countMarks) *sandbox.ResumedCount {
	switch {
	case !c.anchored:
		marks = marks.with(sandbox.CountUnanchored)
	case c.compacted:
		marks = marks.with(sandbox.CountCompacted)
	}
	restart := c.restart
	out := &sandbox.ResumedCount{Window: c.window, Restart: &restart}
	switch mark := marks.first(); mark {
	case "":
		out.Tokens = c.base
	case sandbox.CountUnread, sandbox.CountRelease:
		out.Mark, out.Restart = mark, nil
	default:
		out.Mark = mark
	}
	return out
}

// errCodexArchive is an archive that holds no one Codex rollout.
var errCodexArchive = errors.New("executor: the archive holds no one Codex rollout")

// codexMeter reads a Codex archive, as kept, once within ctx: the figure,
// Codex's count of the session by the rules of release, and the estimate of
// its next request, the count's bound. An archive that does not read to its
// end, or holds no one rollout, has no figure and an unread count.
func codexMeter(ctx context.Context, r io.Reader, release string) (meterReading, error) {
	var marks countMarks
	if !codexCountRead(release) {
		marks = marks.with(sandbox.CountRelease)
	}
	var c codexCounting
	err := codexArchiveRollout(r, func(rollout io.Reader) error { return c.read(ctx, rollout) })
	if err != nil {
		return meterReading{count: (&codexCounting{}).count(marks.with(sandbox.CountUnread))}, err
	}
	read := meterReading{tokens: c.figure, count: c.count(marks)}
	if n, ok := read.count.Bound(); ok {
		read.next = sandbox.NextRequest{Tokens: n}
	}
	return read, nil
}

// read reads a rollout's lines into c on the notes walk ([lineWalk.setupNotes]),
// holding no line: a torn last line is never read, and a line past its bound
// fails the read ([errCodexRolloutLine]).
func (c *codexCounting) read(ctx context.Context, rollout io.Reader) error {
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(rollout, lineChunkBytes), codexWalkBound)
	var l codexCountLine
	for n := 1; ; n++ {
		line, err := w.noteLine(ctx, func() error { return l.read(&w) })
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			return fmt.Errorf("executor: reading codex rollout line %d: %w", n, err)
		case line.none, line.torn:
			return nil
		case line.past:
			return fmt.Errorf("%w: line %d", errCodexRolloutLine, n)
		}
		if line.object && l.took {
			c.line(&l)
		}
		if line.ended {
			return nil
		}
	}
}

// codexArchiveRollout hands each the one rollout a gzipped Codex archive
// holds ([codexIsMember]); errCodexArchive where it holds none or more.
func codexArchiveRollout(r io.Reader, each func(io.Reader) error) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("%w: %w", errNotGzip, err)
	}
	tr := tar.NewReader(zr)
	read := false
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF) && read:
			return nil
		case errors.Is(err, io.EOF):
			return errCodexArchive
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if !codexIsMember(h.Name) {
			continue
		}
		if read || h.Typeflag != tar.TypeReg {
			return errCodexArchive
		}
		read = true
		if err := each(tr); err != nil {
			return err
		}
	}
}

// meter reads a Codex archive for the take's meter ([carrier.meter],
// [codexMeter]).
func (Codex) meter(ctx context.Context, r io.Reader, _, release string, _ *passBudget) (meterReading, error) {
	return codexMeter(ctx, r, release)
}

var _ mainThreadHarness = Codex{}

// readMainThread is Codex's read of ask's session in box: the archive the
// job's script packed as the job ended ([codexCapture]), walked once for the
// count and the estimate ([codexMeter]), cheapest check first: a release
// whose rule was not read is [sandbox.CountRelease] with no read; an archive
// the box holds none of is [sandbox.CountUnread].
func (Codex) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if !ask.Count && !ask.Next {
		return sandbox.MainThreadReading{}
	}
	marked := func(mark sandbox.CountMark) sandbox.MainThreadReading {
		if !ask.Count {
			return sandbox.MainThreadReading{}
		}
		return sandbox.MainThreadReading{Count: &sandbox.ResumedCount{Mark: mark}}
	}
	if !codexCountRead(ask.Release) {
		return marked(sandbox.CountRelease)
	}
	body, err := box.OpenFile(ctx, paths.SessionCapture())
	if err != nil {
		return marked(sandbox.CountUnread)
	}
	defer func() { _ = body.Close() }()
	read, _ := codexMeter(ctx, body, ask.Release)
	var reading sandbox.MainThreadReading
	if ask.Count {
		reading.Count = read.count
	}
	if ask.Next {
		reading.Next = read.next
	}
	return reading
}

// threadTraits is what Codex's main-thread read settles: the archive its
// script packs as it ends, which a cut-off copy may lack what it holds, so
// such a copy is not counted. Receipts come of its confirm off the rollout
// ([recordConfirmer]), which [Runner.ThreadTraits] adds.
func (Codex) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{} }

// sessionTraits is how a relaunch judges a Codex session: by the run's window,
// whose limit the runner pins and Codex compacts at; its calls keyed by their
// own call_id, which a resume replays unchanged; a brief at
// [codexChargeBytes] (each measured, CODEX-MEASURE §16.4).
func (Codex) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestWindow, CallKeying: sandbox.CallKeyingByCallID,
		Charge: codexChargeBytes}
}

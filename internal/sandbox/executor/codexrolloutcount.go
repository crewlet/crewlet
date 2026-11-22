package executor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT CODEX COUNTS A SESSION AT BEFORE ITS NEXT REQUEST, read off the rollout
// a capture packed ([codexCounting.line]): Codex's own figure for its context,
// the newest token_count's last_token_usage.total_tokens (which it rewrites as
// an estimate of the replaced history once a compaction ends, measured on
// 0.171.0), plus every response item written after it at [codexChargeBytes].
// A rollout with no such figure, or a compaction after the newest, holds what
// no rollout states (the system prompt and tools), so the count is marked.

// codexChargeBytes is the fewest bytes Codex's own estimate counts as a
// token: it estimates a text at its UTF-8 length over four, rounded up.
const codexChargeBytes sandbox.Charge = 4

// codexCountRead reports whether release is one whose count rule this build
// read: a SET, never a range, as each harness's is ([openCodeCountRead]).
func codexCountRead(release string) bool { return release == codexCarryFloor }

// codexCounting is one walk's account of a rollout for the count, the meter's
// figure and the window ([codexCounting.line]).
type codexCounting struct {
	// anchored says a token_count stated Codex's figure, base; after is the
	// bytes of the response items written since, and compacted says a
	// compaction ended after it.
	anchored, compacted bool
	base, after         int64
	// figure is the input of the newest request that stated one; window the
	// least context window Codex reported.
	figure, window int64
	// restart names the newest compaction, "" for none.
	restart string
}

// codexCountWire is the part of a token_count or compacted payload the count
// reads.
type codexCountWire struct {
	Type string `json:"type"`
	Info *struct {
		Last *struct {
			Input int64 `json:"input_tokens"`
			Total int64 `json:"total_tokens"`
		} `json:"last_token_usage"`
		Window int64 `json:"model_context_window"`
	} `json:"info"`
	WindowID string `json:"window_id"`
}

// line reads one rollout line of size bytes: THE ONE RULE of Codex's count.
func (c *codexCounting) line(raw []byte, size int) {
	var env struct {
		Type    string         `json:"type"`
		Ordinal int64          `json:"ordinal"`
		Payload codexCountWire `json:"payload"`
	}
	if wireDecode(raw, &env) != nil {
		return
	}
	switch p := env.Payload; {
	case env.Type == "response_item":
		c.after += int64(size)
	case env.Type == "compacted":
		c.compacted, c.after = true, 0
		c.restart = p.WindowID
		if c.restart == "" {
			c.restart = "ordinal-" + strconv.FormatInt(env.Ordinal, 10)
		}
	case env.Type == "event_msg" && p.Type == "token_count" && p.Info != nil && p.Info.Last != nil:
		c.anchored, c.compacted, c.base, c.after = true, false, max(p.Info.Last.Total, 0), 0
		if p.Info.Last.Input > 0 {
			c.figure = p.Info.Last.Input
		}
		if w := p.Info.Window; w > 0 && (c.window == 0 || w < c.window) {
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
		out.Tokens = c.base + codexChargeBytes.Tokens(c.after)
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
	err := codexArchiveRollout(r, func(rollout io.Reader) error {
		_, err := codexLines(rollout, func(_ int, body []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			c.line(body, len(body))
			return nil
		}, func(n int) error {
			return fmt.Errorf("%w: line %d", errCodexRolloutLine, n)
		})
		return err
	})
	if err != nil {
		return meterReading{count: (&codexCounting{}).count(marks.with(sandbox.CountUnread))}, err
	}
	read := meterReading{tokens: c.figure, count: c.count(marks)}
	if n, ok := read.count.Bound(); ok {
		read.next = sandbox.NextRequest{Tokens: n}
	}
	return read, nil
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
// which Codex compacts at; its calls keyed by their own call_id, which a
// resume carries over unchanged; a brief at [codexChargeBytes].
func (Codex) sessionTraits() sandbox.SessionTraits {
	return sandbox.SessionTraits{FirstRequest: sandbox.FirstRequestWindow, CallKeying: sandbox.CallKeyingByCallID,
		Charge: codexChargeBytes}
}

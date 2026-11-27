package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A collection opens every piece it reads before it reads any, weighs them by
// the sizes those opens give, holds that weight of the node's answer budget
// ([queue.AnswerBudget]), and only then reads, from the same opens, each no
// further than the size it was weighed at (internal/queue's package doc, "The
// node's answer budget"). A collection waiting for the budget holds its opens:
// at most ten, across the node's turns and its peers' reads (executor.NodeWork's
// Turns and PeerReads).

// collectHeldMultiple is what a collection holds at once over the bytes its
// pieces held: BenchmarkACollectionAtItsBounds' x-input column, 3.2 at worst
// (OpenCode's plain answer), held at five for the margin a measure on one
// machine leaves.
const collectHeldMultiple = 5

// collectionWeight is what a collection of pieces holding sizes bytes holds of
// the node's answer budget: [collectHeldMultiple] times them, at least the
// fixed per-read buffers and summary ([baseTierReadBytes]) and at most what one
// collection can hold ([sandbox.BoxReadHeldBytes]).
func collectionWeight(sizes int64) int64 {
	return min(max(collectHeldMultiple*sizes, baseTierReadBytes), sandbox.BoxReadHeldBytes)
}

// piece is one file a collection reads, opened: what it is, the open and the
// size it gave, or why it is not read ([refusal]: refused for its kind or its
// size, described as a piece the run did not report).
type piece struct {
	path, what string
	body       io.ReadCloser
	size       int64
	refusal    string
	// kind and reason say what the piece is, and why it was refused where
	// it was, to count ([sandbox.Withholding]); meter is what counts it.
	kind   sandbox.WithheldPiece
	reason sandbox.WithheldReason
	meter  sandbox.CollectMeter
}

// withhold counts the piece withheld for reason, and answers refusal.
func (p *piece) withhold(reason sandbox.WithheldReason, refusal string) string {
	sandbox.Withholding{Piece: p.kind, Reason: reason}.To(p.meter)
	return refusal
}

// weight is what the piece counts toward its collection's weight: its size,
// or nothing for one not read.
func (p *piece) weight() int64 {
	if p == nil || p.body == nil {
		return 0
	}
	return p.size
}

// close closes the open, once. Safe on nil.
func (p *piece) close() {
	if p != nil && p.body != nil {
		_ = p.body.Close()
		p.body = nil
	}
}

// whole reads the piece whole, at most the size it was opened and weighed at,
// and only as one version of its file ([sandbox.ChangeOf]): one byte more, or
// a file grown since the open, is refused as a piece past its bound, since a
// cut report reads as a finished one; a read short of the size, or a file cut
// or written over since, as a piece that changed while it was read, since what
// was read may join two versions of it.
func (p *piece) whole() ([]byte, string, error) {
	if p.refusal != "" || p.body == nil {
		return nil, p.refusal, nil
	}
	defer p.close()
	raw, err := io.ReadAll(io.LimitReader(p.body, p.size+1))
	if err != nil {
		return nil, "", fmt.Errorf("executor: reading %s (%s): %w", p.what, p.path, err)
	}
	change, err := sandbox.ChangeOf(p.body)
	if err != nil {
		return nil, "", fmt.Errorf("executor: telling whether %s (%s) changed while it was read: %w",
			p.what, p.path, err)
	}
	switch {
	case int64(len(raw)) > p.size, change == sandbox.FileAppended:
		return nil, p.withhold(sandbox.WithheldGrown, grownPiece(p.path, p.what, p.size)), nil
	case int64(len(raw)) < p.size, change != sandbox.FileUnchanged:
		return nil, p.withhold(sandbox.WithheldChanged, changedPiece(p.path, p.what, int64(len(raw)), p.size)), nil
	}
	return raw, "", nil
}

// text is [piece.whole] as text.
func (p *piece) text() (string, string, error) {
	raw, refusal, err := p.whole()
	return string(raw), refusal, err
}

// grownPiece describes a piece that held more than the size it was opened at:
// written after the open, which the collection never reads past.
func grownPiece(path, what string, size int64) string {
	return fmt.Sprintf("%s (%s) grew past the %s it held when the collection opened it, "+
		"so it was not read", what, path, humanSize(size))
}

// unchangedRecord is err, or, where a walk of the session record body named by
// what ended without one, its refusal as a record that changed while it was
// read ([sandbox.ChangeOf]): what was read may join two versions of it, so its
// notes and its summary are read off neither.
func unchangedRecord(body io.Reader, what string, err error) error {
	if err != nil {
		return err
	}
	change, err := sandbox.ChangeOf(body)
	if err != nil {
		return fmt.Errorf("executor: telling whether %s changed while it was read: %w", what, err)
	}
	if change != sandbox.FileUnchanged {
		return fmt.Errorf("executor: %s was %s while it was read, so what was read may join two versions of it: %w",
			what, change, errRecordChanged)
	}
	return nil
}

// errRecordChanged is a session record written to while a walk read it
// ([unchangedRecord]).
var errRecordChanged = errors.New("executor: a session record changed while it was read")

// countRecordChanged counts, to the meter ctx carries ([sandbox.MeterOf]), a
// walk of a session record err says changed while it was read
// ([errRecordChanged]).
func countRecordChanged(ctx context.Context, err error) {
	if errors.Is(err, errRecordChanged) {
		sandbox.Withholding{Piece: sandbox.WithheldSessionRecord, Reason: sandbox.WithheldChanged}.
			To(sandbox.MeterOf(ctx))
	}
}

// counting has every piece of c counted by the meter ctx carries
// ([sandbox.MeterOf]), those refused at their open counted now.
func (c *collectionPieces) counting(ctx context.Context) {
	meter := sandbox.MeterOf(ctx)
	for _, p := range c.all() {
		if p == nil {
			continue
		}
		p.meter = meter
		if p.reason != "" {
			sandbox.Withholding{Piece: p.kind, Reason: p.reason}.To(meter)
		}
	}
}

// changedPiece describes a piece cut or written over while the collection read
// it: read bytes of the size it held at the open.
func changedPiece(path, what string, read, size int64) string {
	return fmt.Sprintf("%s (%s) changed while the collection read it (%s of the %s it held when opened "+
		"were read, and it was cut or written over meanwhile), so it was not read", what, path,
		humanSize(read), humanSize(size))
}

// openWhole opens a piece read whole ([sandbox.Sandbox.OpenWhole]): refused,
// not failed, for a path that is no regular file or a file past the box's
// whole-read cap, as [readWholeBytes] refuses it.
func openWhole(ctx context.Context, box sandbox.Sandbox, path, what string) (*piece, error) {
	body, size, err := box.OpenWhole(ctx, path)
	const kind = sandbox.WithheldWholePiece
	if refusal := notRegularPiece(err, path, what); refusal != "" {
		return &piece{path: path, what: what, refusal: refusal, kind: kind, reason: sandbox.WithheldNotRegular}, nil
	}
	var tooLarge *sandbox.FileTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		return &piece{path: path, what: what, refusal: fmt.Sprintf("%s (%s) is %s, past the %s the engine "+
			"reads back from a box whole, so it was not read", what, path, humanSize(tooLarge.Size),
			humanSize(int64(tooLarge.Limit))), kind: kind, reason: sandbox.WithheldTooLarge}, nil
	case err != nil:
		return nil, fmt.Errorf("executor: opening %s (%s): %w", what, path, err)
	}
	return &piece{path: path, what: what, body: body, size: size, kind: kind}, nil
}

// openStream opens an event stream, decoded a line at a time and never read
// whole: as it stood when opened, its size from that open.
func openStream(ctx context.Context, box sandbox.Sandbox, path string) (*piece, error) {
	const what, kind = "the event stream", sandbox.WithheldEventStream
	body, size, err := box.OpenRange(ctx, path, 0, math.MaxInt64)
	if refusal := notRegularPiece(err, path, what); refusal != "" {
		return &piece{path: path, what: what, refusal: refusal, kind: kind, reason: sandbox.WithheldNotRegular}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("executor: opening the event stream: %w", err)
	}
	return &piece{path: path, what: what, body: body, size: size, kind: kind}, nil
}

// collectionPieces is every piece one collection reads, opened before its
// weight is taken: at most ten opens a collection waiting for the budget holds.
type collectionPieces struct {
	stream, result, record, base, stderr, exitCode, ended, findings *piece
	memory                                                          [2]*piece
	// keep is how much of the error stream's redacted end its read keeps
	// ([Runner.failureBound]).
	keep int
}

// all is every piece, absent ones nil.
func (c *collectionPieces) all() []*piece {
	return []*piece{c.stream, c.result, c.record, c.base, c.stderr, c.exitCode, c.ended, c.findings,
		c.memory[0], c.memory[1]}
}

// close closes every open still held.
func (c *collectionPieces) close() {
	for _, p := range c.all() {
		p.close()
	}
}

// weight is what the collection holds of the node's answer budget: its pieces
// at [collectionWeight], and beside them the error stream's read
// ([stderrHeld]), which holds no piece whole.
func (c *collectionPieces) weight() int64 {
	var sizes int64
	for _, p := range c.all() {
		if p != c.stderr {
			sizes += p.weight()
		}
	}
	return min(collectionWeight(sizes)+stderrHeld(c.stderr.weight(), c.keep), sandbox.BoxReadHeldBytes)
}

// stderrHeld is what the read of an error stream of size bytes holds
// ([Runner.readFailureTail]): one redaction stream at its stages' windows
// ([streamHeldBytes]), the 2*keep+1 bytes of its end the ring keeps and the
// keep+1 of its start held raw while it is no longer, or, for a stream short
// enough to be redacted whole, [collectHeldMultiple] times it.
func stderrHeld(size int64, keep int) int64 {
	return min(collectHeldMultiple*size, streamHeldBytes+3*int64(keep)+2)
}

// openCollection opens every piece a collection of the run in box reads, in
// the order it reads them; on an error, those already open are closed.
func (r *Runner) openCollection(ctx context.Context, box sandbox.Sandbox, paths Paths,
	out Output) (_ *collectionPieces, err error) {

	c := &collectionPieces{keep: r.failureBound}
	defer func() {
		if err != nil {
			c.close()
		}
	}()
	if out.Events {
		if c.stream, err = openStream(ctx, box, out.Stdout); err != nil {
			return nil, err
		}
	}
	if out.Result != "" {
		if c.result, err = openWhole(ctx, box, out.Result, "the result its harness printed"); err != nil {
			return nil, err
		}
	}
	whole := []struct {
		into       **piece
		path, what string
	}{
		{&c.record, paths.SessionRecord(), "the record of the session its harness ran in"},
		{&c.exitCode, paths.ExitCode(), "its exit status"},
		{&c.memory[0], paths.MemoryBefore(), "the record of its memory"},
		{&c.memory[1], paths.MemoryAfter(), "the record of its memory"},
		{&c.ended, paths.JobEnded(), "the record of how its job was ended"},
		{&c.findings, paths.Findings(), "the report the harness wrote"},
	}
	if _, totals := r.harness.(sessionTotals); totals {
		whole = append(whole, struct {
			into       **piece
			path, what string
		}{&c.base, paths.BaseResult(), "what the continued session had spent"})
	}
	for _, w := range whole {
		if *w.into, err = openWhole(ctx, box, w.path, w.what); err != nil {
			return nil, err
		}
	}
	// FROM ITS FIRST BYTE, as it stood at the open: a failure shows the end of
	// the whole stream's redaction ([Runner.readFailureTail]).
	body, size, err := box.OpenRange(ctx, paths.Err(), 0, math.MaxInt64)
	const kind = sandbox.WithheldErrorStream
	if refusal := notRegularPiece(err, paths.Err(), "the error stream"); refusal != "" {
		c.stderr = &piece{path: paths.Err(), what: "the error stream", refusal: refusal, kind: kind,
			reason: sandbox.WithheldNotRegular}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("executor: reading the error stream: %w", err)
	}
	c.stderr = &piece{path: paths.Err(), what: "the error stream", body: body, size: size, kind: kind}
	return c, nil
}

// holdCollection takes the weight of c from answers for a collection: in a
// peer's box read until its reply is written, else until the release.
func holdCollection(ctx context.Context, answers *queue.AnswerBudget, c *collectionPieces) (func(), error) {
	release, err := answers.Acquire(ctx, queue.AnswerBoxRead, c.weight())
	if err != nil {
		return nil, fmt.Errorf("executor: holding the collection's weight of the node's answer budget: %w", err)
	}
	return release, nil
}

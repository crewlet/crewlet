package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact"
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

// whole reads the piece whole, at most the size it was opened and weighed at:
// one byte more is a file that grew past it after the open, which is refused
// as a piece past its bound, since a cut report reads as a finished one.
func (p *piece) whole() ([]byte, string, error) {
	if p.refusal != "" || p.body == nil {
		return nil, p.refusal, nil
	}
	defer p.close()
	raw, err := io.ReadAll(io.LimitReader(p.body, p.size+1))
	if err != nil {
		return nil, "", fmt.Errorf("executor: reading %s (%s): %w", p.what, p.path, err)
	}
	if int64(len(raw)) > p.size {
		return nil, grownPiece(p.path, p.what, p.size), nil
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

// openWhole opens a piece read whole ([sandbox.Sandbox.OpenWhole]): refused,
// not failed, for a path that is no regular file or a file past the box's
// whole-read cap, as [readWholeBytes] refuses it.
func openWhole(ctx context.Context, box sandbox.Sandbox, path, what string) (*piece, error) {
	body, size, err := box.OpenWhole(ctx, path)
	if refusal := notRegularPiece(err, path, what); refusal != "" {
		return &piece{path: path, what: what, refusal: refusal}, nil
	}
	var tooLarge *sandbox.FileTooLargeError
	switch {
	case errors.As(err, &tooLarge):
		return &piece{path: path, what: what, refusal: fmt.Sprintf("%s (%s) is %s, past the %s the engine "+
			"reads back from a box whole, so it was not read", what, path, humanSize(tooLarge.Size),
			humanSize(int64(tooLarge.Limit)))}, nil
	case err != nil:
		return nil, fmt.Errorf("executor: opening %s (%s): %w", what, path, err)
	}
	return &piece{path: path, what: what, body: body, size: size}, nil
}

// openStream opens an event stream, decoded a line at a time and never read
// whole: as it stood when opened, its size from that open.
func openStream(ctx context.Context, box sandbox.Sandbox, path string) (*piece, error) {
	const what = "the event stream"
	body, size, err := box.OpenRange(ctx, path, 0, math.MaxInt64)
	if refusal := notRegularPiece(err, path, what); refusal != "" {
		return &piece{path: path, what: what, refusal: refusal}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("executor: opening the event stream: %w", err)
	}
	return &piece{path: path, what: what, body: body, size: size}, nil
}

// collectionPieces is every piece one collection reads, opened before its
// weight is taken: at most ten opens a collection waiting for the budget holds.
type collectionPieces struct {
	stream, result, record, base, errEnd, exitCode, ended, findings *piece
	memory                                                          [2]*piece
	// errWant is the error stream's end asked for, of which errEnd holds
	// at most that much.
	errWant int64
}

// all is every piece, absent ones nil.
func (c *collectionPieces) all() []*piece {
	return []*piece{c.stream, c.result, c.record, c.base, c.errEnd, c.exitCode, c.ended, c.findings,
		c.memory[0], c.memory[1]}
}

// close closes every open still held.
func (c *collectionPieces) close() {
	for _, p := range c.all() {
		p.close()
	}
}

// weight is what the collection holds of the node's answer budget
// ([collectionWeight]) for what its opens hold: the error stream's end only.
func (c *collectionPieces) weight() int64 {
	var sizes int64
	for _, p := range c.all() {
		sizes += p.weight()
	}
	if c.errEnd != nil {
		sizes -= c.errEnd.weight() - min(c.errEnd.weight(), c.errWant)
	}
	return collectionWeight(sizes)
}

// openCollection opens every piece a collection of the run in box reads, in
// the order it reads them; on an error, those already open are closed.
func (r *Runner) openCollection(ctx context.Context, box sandbox.Sandbox, paths Paths,
	out Output) (_ *collectionPieces, err error) {

	c := &collectionPieces{errWant: int64(r.failureBound + redact.LookBack)}
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
	body, size, err := box.OpenTail(ctx, paths.Err(), c.errWant)
	if refusal := notRegularPiece(err, paths.Err(), "the error stream"); refusal != "" {
		c.errEnd = &piece{path: paths.Err(), what: "the error stream", refusal: refusal}
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("executor: reading the error stream: %w", err)
	}
	c.errEnd = &piece{path: paths.Err(), what: "the error stream", body: body, size: size}
	return c, nil
}

// errTail reads the error stream's end the collection opened: at most what it
// asked for, with the stream's whole size at the open, or the refusal.
func (c *collectionPieces) errTail() (sandbox.FileTail, string, error) {
	p := c.errEnd
	if p.refusal != "" || p.body == nil {
		return sandbox.FileTail{}, p.refusal, nil
	}
	defer p.close()
	data, err := io.ReadAll(io.LimitReader(p.body, c.errWant))
	if err != nil {
		return sandbox.FileTail{}, "", fmt.Errorf("executor: reading the error stream: %w", err)
	}
	return sandbox.FileTail{Data: data, Size: max(p.size, int64(len(data)))}, "", nil
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

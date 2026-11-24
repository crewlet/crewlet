package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
)

// A CHAT FILE'S HOLDINGS READING (Qwen Code's, Gemini CLI's) reads the one
// member its kept archive holds ([chatHoldingsMember], [chatHoldingsEnd]) a
// line at a time ([chatLines]), holding no line, and the harness's walk keeps
// what it needs of each; [onceEach] holds a file to each id once. A line the
// harness's own reader would split, or a member it would not load, is no
// reading: what the reading cannot see as the harness does, it tells.

// chatHoldingsMember is the chat file plain, a kept session's tar as
// unpacked, holds: its first entry, which must be a plain file member names,
// read from the returned reader. A tar error passes through, wrapped
// ([errNotTar]).
func chatHoldingsMember(plain io.Reader, member func(name string) bool) (*tar.Reader, error) {
	tr := tar.NewReader(plain)
	h, err := tr.Next()
	switch {
	case errors.Is(err, io.EOF):
		return nil, errChatHoldingsNone
	case err != nil:
		return nil, fmt.Errorf("%w: %w", errNotTar, err)
	case h.Typeflag != tar.TypeReg || !member(h.Name):
		return nil, errChatHoldingsNone
	}
	return tr, nil
}

// chatHoldingsEnd holds tr, its chat file read, to its end: no entry follows.
func chatHoldingsEnd(tr *tar.Reader) error {
	_, err := tr.Next()
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return fmt.Errorf("%w: %w", errNotTar, err)
	}
	return errChatHoldingsMore
}

// The words of a chat file's reading for an archive or a line it does not
// read.
var (
	errChatHoldingsNone = &holdingsError{why: "the session's archive does not open with its one chat file, " +
		"a plain file"}
	errChatHoldingsMore     = &holdingsError{why: "the session's archive holds more than its chat file"}
	errChatHoldingsCarriage = &holdingsError{why: "a line of the session's chat file holds a carriage return, " +
		"where its harness's reader ends a line"}
	errChatHoldingsUnread = &holdingsError{why: "a line of the session's chat file was not read to its end"}
)

// chatReadFailed is why a chat file's walk read no file whole: the reading's
// own words, the caller gone, the plain meter's bound, or the archive read as
// no chat file, its cause the reader's words, which [holdingsError] cuts.
func chatReadFailed(ctx context.Context, err error) error {
	var own *holdingsError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &own):
		return own
	case errors.Is(err, errScrubBound):
		return &holdingsError{why: "the session's archive is past the bound a kept session unpacks to", cause: err}
	}
	return &holdingsError{why: "the session's archive does not read as one chat file", cause: err}
}

// errChatTorn is what a line's reader answers where the file ends inside it:
// the torn line [tornCut] drops before a resume, never handed on.
var errChatTorn = errors.New("executor: the chat file ends inside its last line")

// chatLines hands line, in order, each line of r, a chat file, as body: its
// bytes to its `\n`, which body answers io.EOF at, holding none of them. n is
// the line's number from 1. It answers the number of whole lines.
//
// A line applies its facts only once body answered io.EOF, and line answers
// nil only then. A torn last line, which [tornCut] drops before a resume, is
// drained unread and whatever line answered for it dropped; a `\r` in a whole
// line is no reading, since the harness's reader may end a line there. ctx is
// asked before each line.
func chatLines(ctx context.Context, r io.Reader, line func(n int, body io.Reader) error) (int, error) {
	l := &chatLine{br: bufio.NewReaderSize(r, lineChunkBytes)}
	for n := 1; ; n++ {
		if err := ctx.Err(); err != nil {
			return n - 1, err
		}
		if _, err := l.br.Peek(1); errors.Is(err, io.EOF) {
			return n - 1, nil
		} else if err != nil {
			return n - 1, err
		}
		l.state = lineOpen
		err := line(n, l)
		switch {
		case err == nil && l.state == lineBroken:
			continue
		case err == nil:
			// LINE KEPT FACTS OF A LINE IT DID NOT READ WHOLE: torn or not,
			// they are no file's.
			return n - 1, errChatHoldingsUnread
		case ctx.Err() != nil:
			return n - 1, ctx.Err()
		}
		carriage := l.state == lineCarriage
		switch l.drain() {
		case lineTorn:
			return n - 1, nil
		case lineFailed:
			return n - 1, l.err
		}
		if carriage {
			return n - 1, errChatHoldingsCarriage
		}
		return n - 1, err
	}
}

// chatLineState is where a line's reader stands in its line.
type chatLineState uint8

const (
	lineOpen     chatLineState = iota // inside the line
	lineBroken                        // past its `\n`: io.EOF
	lineCarriage                      // at a `\r` inside it
	lineTorn                          // the file ended inside it
	lineFailed                        // r failed: err
)

// chatLine is [chatLines]' reader of the current line.
type chatLine struct {
	br    *bufio.Reader
	state chatLineState
	err   error
}

// Read reads the line's next bytes, up to its `\n`.
func (l *chatLine) Read(p []byte) (int, error) {
	switch l.state {
	case lineBroken:
		return 0, io.EOF
	case lineCarriage:
		return 0, errChatHoldingsCarriage
	case lineTorn:
		return 0, errChatTorn
	case lineFailed:
		return 0, l.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if _, err := l.br.Peek(1); err != nil {
		l.end(err)
		return l.Read(p)
	}
	chunk, _ := l.br.Peek(min(len(p), l.br.Buffered()))
	i := bytes.IndexAny(chunk, "\r\n")
	if i < 0 {
		i = len(chunk)
	}
	n := copy(p, chunk[:i])
	_, _ = l.br.Discard(n)
	switch {
	case i == len(chunk):
	case chunk[i] == '\n':
		_, _ = l.br.Discard(1)
		l.state = lineBroken
	default:
		l.state = lineCarriage
	}
	if n == 0 {
		return l.Read(p)
	}
	return n, nil
}

// end ends the line where r answered err inside it.
func (l *chatLine) end(err error) {
	if errors.Is(err, io.EOF) {
		l.state = lineTorn
		return
	}
	l.state, l.err = lineFailed, err
}

// drain passes the rest of the line, a `\r` in it too, and answers where it
// ended: past its `\n`, torn, or failed.
func (l *chatLine) drain() chatLineState {
	for l.state == lineOpen || l.state == lineCarriage {
		_, err := l.br.ReadSlice('\n')
		switch {
		case err == nil:
			l.state = lineBroken
		case !errors.Is(err, bufio.ErrBufferFull):
			l.end(err)
		}
	}
	return l.state
}

// onceEachBlock is how many keys a block of [onceEach] holds: 64 KiB, so a
// file's millions of ids grow it a block at a time, never by copying it whole.
const onceEachBlock = 8 << 10

// onceEach is whether a file names each of its ids once: 8 bytes of each id's
// own, its key, 8 bytes an id. Two ids of one key read as one id twice, which
// only tells. The key is the id's, never a hash of it: ordering hashes is
// rendezvous's alone (ADR-0008), and an id the harness draws at random is
// already as spread as one.
type onceEach struct {
	blocks [][]uint64
}

// add keeps an id by its key.
func (o *onceEach) add(key uint64) {
	if len(o.blocks) == 0 || len(o.blocks[len(o.blocks)-1]) == onceEachBlock {
		o.blocks = append(o.blocks, make([]uint64, 0, onceEachBlock))
	}
	last := len(o.blocks) - 1
	o.blocks[last] = append(o.blocks[last], key)
}

// twice reports whether any id was kept twice: each block sorted, then the
// blocks merged, each key compared with the one before it.
func (o *onceEach) twice() bool {
	heads := make(onceHeads, 0, len(o.blocks))
	for _, b := range o.blocks {
		slices.Sort(b)
		heads = append(heads, b)
	}
	heap.Init(&heads)
	var last uint64
	for seen := false; len(heads) > 0; seen = true {
		h := heads[0][0]
		if seen && h == last {
			return true
		}
		last = h
		if heads[0] = heads[0][1:]; len(heads[0]) == 0 {
			heap.Pop(&heads)
		} else {
			heap.Fix(&heads, 0)
		}
	}
	return false
}

// onceHeads is [onceEach.twice]'s heap of its sorted blocks' unmerged rests,
// each non-empty, least first.
type onceHeads [][]uint64

func (h onceHeads) Len() int           { return len(h) }
func (h onceHeads) Less(i, j int) bool { return h[i][0] < h[j][0] }
func (h onceHeads) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *onceHeads) Push(x any)        { *h = append(*h, x.([]uint64)) }
func (h *onceHeads) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

package cdp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
)

// Options is how [NewPipe] runs its connection.
type Options struct {
	// Logger takes the connection's DEBUG lines; nil is logging.Get("cdp").
	Logger *slog.Logger
}

// NewPipe starts a connection over a browser's DevTools pipes: r is what
// Chromium writes (its fd 4 under --remote-debugging-pipe, or a container's
// stdout), w what it reads (its fd 3, or the container's stdin). Each message
// is one JSON object ended by one NUL byte, each way; a message read past
// [ReadLimit] is read through to its NUL and ends only what it is for. Closing
// the connection closes w, which makes Chromium exit, and r, which ends the
// connection's reader whatever Chromium does — so r is a ReadCloser, as every
// pipe a caller holds (an *os.File, a command's StdoutPipe) is.
func NewPipe(r io.ReadCloser, w io.WriteCloser, o Options) *Conn {
	return newConn(context.Background(), &pipe{frames: newFrames(r), r: r, w: w}, o.Logger)
}

// pipe is the NUL-delimited transport.
type pipe struct {
	frames *frames
	r      io.ReadCloser
	w      io.WriteCloser
	once   sync.Once
}

// read ignores ctx: a pipe's read ends only with the pipe, which close ends.
func (p *pipe) read(context.Context) (message, error) { return p.frames.next() }

// write sends one message and its NUL in one Write: the connection's one
// writer goroutine is the only caller, so messages never interleave. A Write
// a browser that stopped reading holds is freed by close, which the
// connection's writer runs at CommandTimeout.
func (p *pipe) write(_ context.Context, msg []byte) error {
	framed := make([]byte, 0, len(msg)+1)
	framed = append(append(framed, msg...), 0)
	_, err := p.w.Write(framed)
	return err
}

func (p *pipe) close() error {
	var err error
	p.once.Do(func() {
		err = errors.Join(p.w.Close(), p.r.Close())
	})
	return err
}

// framesBuffer is the reader's own buffer, which a message larger than it
// crosses a slice at a time: 64 KiB, a Linux pipe's default capacity and the
// most a macOS pipe grows to, so one read takes everything a full pipe holds.
// The socket's reader reads in slices of the same size: nothing about a
// websocket's messages asks for another.
const framesBuffer = 64 << 10

// frames splits what a pipe reads into NUL-terminated messages. Every message
// is a slice of its own, allocated to its size: a buffer that grew to the
// largest message and stayed (bufio.Scanner's) would keep a screenshot's
// 32 MiB for as long as the browser lived, and its slices are overwritten by
// the next message while another goroutine still holds them. A message past
// ReadLimit is read through to its NUL a buffer at a time, and kept only as
// its start, its end and its size (gather).
type frames struct {
	br *bufio.Reader
}

func newFrames(r io.Reader) *frames { return &frames{br: bufio.NewReaderSize(r, framesBuffer)} }

// next is the next message without its NUL, or the reader's error, a message
// cut short by it dropped.
func (f *frames) next() (message, error) {
	var g gather
	for {
		chunk, err := f.br.ReadSlice(0)
		switch {
		case err == nil:
			g.add(chunk[:len(chunk)-1])
			return g.message(), nil
		case errors.Is(err, bufio.ErrBufferFull):
			g.add(chunk)
		default:
			return message{}, err
		}
	}
}

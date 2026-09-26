package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/crewlet/crewlet/internal/objstore"
)

// readAhead is how many chunks a reader fetches ahead of the one it is
// handing out.
//
// FOUR: a chunk from a peer is a round trip plus a mebibyte on the wire, and
// with one in flight a reader streams at one chunk per round trip; with four
// the round trips overlap and the wire is what limits it. It is also what a
// reader holds in memory — four mebibytes per open object — which a small
// stateless node serving a handful of downloads at once can afford.
const readAhead = 4

// Open streams an object back in order.
//
// Every chunk is checked against its own name as it arrives, and the whole
// against the manifest's hash at the end: a manifest whose chunks do not make
// up the object it names is an error at the last read rather than a quiet
// end of file.
func (c *Client) Open(ctx context.Context, m objstore.Manifest) (io.ReadCloser, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &reader{
		manifest: m, cancel: cancel, whole: sha256.New(),
		futures: make(chan chan fetched, readAhead), done: make(chan struct{}),
	}
	go r.fetch(ctx, c)
	return r, nil
}

type fetched struct {
	data []byte
	err  error
}

type reader struct {
	manifest objstore.Manifest
	cancel   context.CancelFunc
	whole    hash.Hash

	// futures carries one pending fetch per chunk, in order; its capacity
	// is how far ahead the fetches run.
	futures chan chan fetched
	done    chan struct{}

	current []byte
	err     error
}

// fetch starts one fetch per chunk, in order, never more than [readAhead]
// ahead of the consumer.
func (r *reader) fetch(ctx context.Context, c *Client) {
	defer close(r.done)
	defer close(r.futures)
	for _, chunk := range r.manifest.Chunks {
		future := make(chan fetched, 1)
		select {
		case r.futures <- future:
		case <-ctx.Done():
			return
		}
		go func() {
			data, err := c.Get(ctx, chunk.Hash)
			if err == nil && int64(len(data)) != chunk.Size {
				err = fmt.Errorf("objstore/transfer: chunk %s is %d bytes and the manifest says %d",
					chunk.Hash, len(data), chunk.Size)
			}
			future <- fetched{data: data, err: err}
		}()
	}
}

func (r *reader) Read(p []byte) (int, error) {
	for len(r.current) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		future, more := <-r.futures
		if !more {
			if got := hex.EncodeToString(r.whole.Sum(nil)); got != string(r.manifest.Hash) {
				r.err = fmt.Errorf("objstore/transfer: the chunks make up %s and the manifest names %s",
					got, r.manifest.Hash)
				return 0, r.err
			}
			r.err = io.EOF
			return 0, io.EOF
		}
		f := <-future
		if f.err != nil {
			r.err = f.err
			return 0, r.err
		}
		_, _ = r.whole.Write(f.data)
		r.current = f.data
	}
	n := copy(p, r.current)
	r.current = r.current[n:]
	return n, nil
}

// Close stops the fetches ahead and waits for the loop starting them, so no
// fetch is started after the object was closed.
func (r *reader) Close() error {
	r.cancel()
	<-r.done
	return nil
}

// ReadAt answers up to n bytes of an object starting at off, fetching only
// the chunks that range touches — a page of a large file costs the chunks on
// that page, not the file. Fewer than n bytes is the end of the object.
func (c *Client) ReadAt(ctx context.Context, m objstore.Manifest, off, n int64) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if off < 0 || n < 0 {
		return nil, fmt.Errorf("objstore/transfer: a read at %d for %d bytes", off, n)
	}
	end := min(off+n, m.Size)
	if off >= end {
		return []byte{}, nil
	}
	out := make([]byte, 0, end-off)
	var start int64
	for _, chunk := range m.Chunks {
		stop := start + chunk.Size
		if stop > off && start < end {
			data, err := c.Get(ctx, chunk.Hash)
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != chunk.Size {
				return nil, fmt.Errorf("objstore/transfer: chunk %s is %d bytes and the manifest says %d",
					chunk.Hash, len(data), chunk.Size)
			}
			from, to := max(off, start)-start, min(end, stop)-start
			out = append(out, data[from:to]...)
		}
		if stop >= end {
			break
		}
		start = stop
	}
	return out, nil
}

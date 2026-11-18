package gitlet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
)

// GIT'S HASHFILE TRAILER, HELD BY ONE IMPLEMENTATION (ADR-0008). Every file git
// writes through csum-file.c (a pack, .idx, .rev, multi-pack-index, its bitmap,
// each commit-graph layer) ends in a trailer hashing every byte before it,
// which git never checks on open. The cache (check, scrub, Verify), the layer
// check ([CheckLayer], [LayLayer]) and `gitlet export`'s bundle all judge it by
// [HashTrailer] over [HashPieces]: two copies would judge one file two ways,
// and one taking no context or slot could not be stopped.
//
// A PIECE AT A TIME, STOPPED WITH ITS CALLER: a layer's root reaches 16 GiB
// ([MaxLayerParts], over 2 min cold), so ctx is checked before every read and a
// read-lane slot (gitproc.Pool.HoldRead; the cache's CheckPieceBytes) is held
// per piece, never a whole file, stalling the lane.

// hashBuffer is the buffer a hash allocates when its caller hands none
// ([Pieces.Buf]), never more than the file holds: a quarter mebibyte, the
// piece the cache's download copies through (copyBuffer), so system calls
// stay a small fraction of the hash.
const hashBuffer = 256 << 10

// Pieces is how a hash reads a file ([HashPieces], [HashTrailer]). The zero
// value reads it in one pass, holding nothing, through a buffer of its own.
type Pieces struct {
	// Buf is the buffer reads go through, each at most its length; a caller
	// hashing many files shares one. Empty, the hash allocates hashBuffer
	// bytes, never more than it reads.
	Buf []byte
	// Hold, when not nil, takes a slot before each piece and answers its
	// release, called once the piece is read (gitproc.Pool.HoldRead). Nil
	// when the caller holds what it needs across the hash (a reserved
	// maintenance slot) or needs nothing.
	Hold func(context.Context) (release func(), err error)
	// Size is the most one piece reads under one hold, so the most a read
	// queued behind the hash waits. Read only with Hold, and then at least 1.
	Size int64
}

// HashPieces adds the n bytes r holds from where it stands to w, a piece at a
// time as p says, checking ctx before every read, and answers how many it
// added: fewer than n with no error means r ended sooner, for the caller to
// judge (a file rewritten mid-read, or not the file its row describes). A
// failed hold (gitproc.ErrBusy for a lane with no slot in time) or read is
// answered as it came, an ended ctx as its cause.
func HashPieces(ctx context.Context, w io.Writer, r io.Reader, n int64, p Pieces) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, context.Cause(ctx)
	}
	buf := p.Buf
	if len(buf) == 0 {
		buf = make([]byte, max(1, min(n, hashBuffer)))
	}
	if p.Hold == nil {
		return copyChecked(ctx, w, io.LimitReader(r, n), buf)
	}
	if p.Size < 1 {
		return 0, fmt.Errorf("gitlet: hash in pieces: a hold over pieces of %d bytes would read nothing", p.Size)
	}
	var added int64
	for added < n {
		release, err := p.Hold(ctx)
		if err != nil {
			return added, err
		}
		piece := min(p.Size, n-added)
		copied, err := copyChecked(ctx, w, io.LimitReader(r, piece), buf)
		release()
		added += copied
		switch {
		case err != nil:
			return added, err
		case copied < piece:
			// r ended inside this piece.
			return added, nil
		}
	}
	return added, nil
}

// copyChecked copies r to w through buf, checking ctx before every read, and
// answers what it copied before r ended or a read, a write or ctx failed.
func copyChecked(ctx context.Context, w io.Writer, r io.Reader, buf []byte) (int64, error) {
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return copied, context.Cause(ctx)
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return copied, werr
			}
			copied += int64(n)
		}
		switch {
		case errors.Is(err, io.EOF):
			return copied, nil
		case err != nil:
			return copied, err
		}
	}
}

// Trailer is what a hash of a hashfile read ([HashTrailer]).
type Trailer struct {
	// Held is the trailer the file ends with, and Sum the hash of every
	// byte before it.
	Held, Sum []byte
	// Read is how many of the file's bytes the hash read, its trailer's
	// among them.
	Read int64
}

// Sound reports the file whole by git's rule: its trailer the hash of every
// byte before it. A Trailer of no trailer read — the zero value, or what a
// failed hash answers — is never sound.
func (t Trailer) Sound() bool { return len(t.Held) > 0 && bytes.Equal(t.Held, t.Sum) }

// HashTrailer holds the size-byte hashfile r holds to git's trailer rule: its
// last sum.Size() bytes are sum (fresh) over every byte before them — SHA-1
// for git's files in Gitlet's SHA-1 repositories, SHA-256 for a provenance
// run the cache writes the same way. It hashes the body as p says
// ([HashPieces]), reads the trailer and answers both; [Trailer.Sound] is the
// verdict, a definite answer about the bytes.
//
// An r that ends before size answers io.ErrUnexpectedEOF for the caller to
// judge (rewritten mid-read, or shorter than its row's section). A failed
// read, hold or ctx is answered as it came with what was read: it says
// nothing of the bytes.
func HashTrailer(ctx context.Context, r io.ReaderAt, size int64, sum hash.Hash, p Pieces) (Trailer, error) {
	trailer := int64(sum.Size())
	if size < trailer {
		return Trailer{}, fmt.Errorf("gitlet: hash a hashfile of %d bytes: that is shorter than its %d-byte trailer",
			size, trailer)
	}
	body := size - trailer
	read, err := HashPieces(ctx, sum, io.NewSectionReader(r, 0, body), body, p)
	t := Trailer{Read: read}
	switch {
	case err != nil:
		return t, err
	case read < body:
		return t, fmt.Errorf("gitlet: the hashfile ends at byte %d, inside the %d before its trailer: %w", read, body,
			io.ErrUnexpectedEOF)
	}
	held := make([]byte, trailer)
	n, err := r.ReadAt(held, body)
	t.Read += int64(n)
	switch {
	case n == len(held):
		// io.ReaderAt may answer io.EOF beside the last of a file's bytes.
	case errors.Is(err, io.EOF):
		return t, fmt.Errorf("gitlet: the hashfile ends at byte %d, inside its %d-byte trailer at %d: %w",
			body+int64(n), trailer, body, io.ErrUnexpectedEOF)
	case err == nil:
		return t, fmt.Errorf("gitlet: read the hashfile's trailer at byte %d: %d bytes of %d and no error, which no "+
			"reader may answer", body, n, trailer)
	default:
		return t, fmt.Errorf("gitlet: read the hashfile's trailer at byte %d: %w", body, err)
	}
	t.Held, t.Sum = held, sum.Sum(nil)
	return t, nil
}

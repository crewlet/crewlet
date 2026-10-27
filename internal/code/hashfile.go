package code

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
)

// GIT'S HASHFILE TRAILER, HELD BY ONE IMPLEMENTATION (ADR-0008). Every file
// git writes through its hashfile (csum-file.c) — a pack, its .idx and its
// .rev, a multi-pack-index and its bitmap, a commit-graph and each layer of
// one — ends in a TRAILER, the hash of every byte before it
// (finalize_hashfile, CSUM_HASH_IN_STREAM), and git reads none of them when it
// opens the file. Two packages of a git node hold such files to it: its cache
// (internal/code/cache: its round's check, its scrub, Verify) and a published
// layer's check here ([CheckLayer], [LayLayer]). The same files reach both —
// the installer holds a layer it fetched to its trailers through CheckLayer,
// the cache's check and scrub hold the layer it keeps — so the rule, and how a
// hash of the largest file a node holds reads it, is this one function,
// [HashTrailer], over the one piece loop, [HashPieces]: two copies would judge
// one file two ways, and the one that took no context or slot would be the
// one a node could not stop.
//
// A PIECE AT A TIME, STOPPED WITH ITS CALLER. A layer's root alone is some
// 2.6 GB for the largest repository the defaults admit and 16 GiB at
// [MaxLayerParts] — four to five seconds of one core from the page cache,
// about 21 s and 2 min 17 s read cold at a gp3 volume's 125 MB/s — and a
// pack is up to [MaxPackBytes]. So ctx is checked before every read, and a
// hold is taken for one piece and given back once it is read: a git node's
// read lane (gitproc.Pool.HoldRead, internal/code/cache's CheckPieceBytes),
// whose slot a hash of a large file holding whole would keep from every read
// queued behind it for the whole read.

// hashBuffer is the buffer a hash reads a file through when its caller hands
// none ([Pieces.Buf]): a quarter mebibyte, the piece a git node's download
// copies a stored object through (internal/code/cache's copyBuffer), so a
// hash reads a file in the pieces its download wrote it — 4 096 reads a
// gibibyte, whose system calls are a small fraction of the hash itself.
// Never more than the file holds.
const hashBuffer = 256 << 10

// Pieces is how a hash reads a file ([HashPieces], [HashTrailer]). The zero
// value reads the file in one pass, holding nothing, through a buffer of its
// own.
type Pieces struct {
	// Buf is the buffer the reads are made through, each read at most its
	// length: a caller hashing many files hands one buffer to all. Empty, a
	// hash allocates one of hashBuffer bytes, never more than it reads.
	Buf []byte
	// Hold, when not nil, takes a slot before each piece is read and
	// answers the function that gives it back, which the hash calls once
	// the piece is read: a git node's read lane (gitproc.Pool.HoldRead).
	// Nil for a caller that holds what it needs across the whole hash — a
	// maintenance slot it reserved — or needs nothing.
	Hold func(context.Context) (release func(), err error)
	// Size is the most of a file one piece reads, in one hold: what a read
	// the lane queues behind the hash waits at most. Read only beside a
	// Hold, which it must then be at least one byte for.
	Size int64
}

// HashPieces adds the n bytes r holds from where it stands to w, a piece at a
// time as p says — ctx checked before every read, each piece in a slot p.Hold
// takes and gives back once the piece is read — and answers how many it
// added: n, or fewer and no error where r ended sooner, which the caller
// judges (a file shorter than it was is a file rewritten while it was read,
// or not the file a row describes). A hold that failed — a lane with no slot
// in time is its own refusal, gitproc.ErrBusy — is answered as it came, as
// is a failed read, and an ended ctx as its cause.
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
		return 0, fmt.Errorf("code: hash in pieces: a hold over pieces of %d bytes would read nothing", p.Size)
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
// answers what it copied: r's every byte, or what it had copied when a read,
// a write or ctx failed.
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

// HashTrailer holds the hashfile r holds, size bytes, to git's trailer rule:
// its last sum.Size() bytes are sum over every byte before them — a SHA-1 for
// git's files in a SHA-1 repository, the code host's object format, and the
// SHA-256 of a provenance run, which the cache writes the same way. It hashes
// those bytes from the file's start a piece at a time as p says
// ([HashPieces]), into sum, which is fresh; then reads the trailer, and answers
// both, the verdict [Trailer.Sound]'s — a definite answer about the bytes.
//
// An r that ends before its size answers io.ErrUnexpectedEOF: it is not the
// file it was handed as, which its caller judges — a file rewritten while it
// was read, or one shorter than its row's section. A read, a hold or a ctx
// that failed is answered as it came, with what had been read: a disk that
// failed a read says nothing of the bytes, nor does a lane with no slot.
func HashTrailer(ctx context.Context, r io.ReaderAt, size int64, sum hash.Hash, p Pieces) (Trailer, error) {
	trailer := int64(sum.Size())
	if size < trailer {
		return Trailer{}, fmt.Errorf("code: hash a hashfile of %d bytes: that is shorter than its %d-byte trailer",
			size, trailer)
	}
	body := size - trailer
	read, err := HashPieces(ctx, sum, io.NewSectionReader(r, 0, body), body, p)
	t := Trailer{Read: read}
	switch {
	case err != nil:
		return t, err
	case read < body:
		return t, fmt.Errorf("code: the hashfile ends at byte %d, inside the %d before its trailer: %w", read, body,
			io.ErrUnexpectedEOF)
	}
	held := make([]byte, trailer)
	n, err := r.ReadAt(held, body)
	t.Read += int64(n)
	switch {
	case n == len(held):
		// io.ReaderAt may answer io.EOF beside the last of a file's bytes.
	case errors.Is(err, io.EOF):
		return t, fmt.Errorf("code: the hashfile ends at byte %d, inside its %d-byte trailer at %d: %w",
			body+int64(n), trailer, body, io.ErrUnexpectedEOF)
	case err == nil:
		return t, fmt.Errorf("code: read the hashfile's trailer at byte %d: %d bytes of %d and no error, which no "+
			"reader may answer", body, n, trailer)
	default:
		return t, fmt.Errorf("code: read the hashfile's trailer at byte %d: %w", body, err)
	}
	t.Held, t.Sum = held, sum.Sum(nil)
	return t, nil
}

package gitlet

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A BUNDLE LAID OUT AND STORED: a git's .pack, .idx (v2) and .rev (v1) laid
// out as the one object a stored pack is (bundle.go) and uploaded, sealed on
// its way. Unlike bundle.go it does I/O, but still runs no git.
//
// ONE COPY for every writer of a stored pack (ADR-0008): a push's landing and
// every rewrite store through these functions, never a copy of their own. The
// seal is what every installing cache holds a bundle to, so a writer laying
// out or sealing by its own reading would make a row no Gitlet node can
// install. [DeriveOutputPackID] is one derivation for the same reason.

// bundleSections are the files a bundle is laid out from, in its order.
var bundleSections = [3]string{".pack", ".idx", ".rev"}

// Putter stores one object; *objstore.Store satisfies it. Declared by its two
// callers, StoreBundle and StoreLayer (layer.go): a writer holds its store as
// a Putter only to hand it to one of them, where the put and the seal (or a
// layer's cut) are made together.
type Putter interface {
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)
}

// OpenIdx opens a version-2 pack index and reads its header, answering its
// object count (the fan-out's last entry) and the file positioned at the first
// object id, which the caller closes. A version-1 index, a bad magic, a short
// header or a count above 1<<31 is refused naming the file. The ONE reading of
// an index's header (ADR-0008): LayBundle counts through it, and the host's
// stream of a push's ids reads on from where it leaves the file.
//
// Version 2 only: git's default since 1.5.2, the one the receive configuration
// pins, and the one CheckBundle holds a bundle's index to.
func OpenIdx(path string) (count int64, f *os.File, err error) {
	f, err = os.Open(path)
	if err != nil {
		return 0, nil, fmt.Errorf("gitlet: open the pack index %s: %w", path, err)
	}
	var head [indexHeaderBytes + fanoutBytes]byte
	if _, err = io.ReadFull(f, head[:]); err != nil {
		_ = f.Close()
		return 0, nil, fmt.Errorf("gitlet: read the pack index %s's header: %w", path, err)
	}
	if !bytes.Equal(head[:len(indexMagic)], indexMagic) ||
		binary.BigEndian.Uint32(head[len(indexMagic):indexHeaderBytes]) != 2 {
		_ = f.Close()
		return 0, nil, fmt.Errorf("gitlet: %s is not a version-2 pack index", path)
	}
	n := binary.BigEndian.Uint32(head[len(head)-4:])
	if n > 1<<31 {
		_ = f.Close()
		return 0, nil, fmt.Errorf("gitlet: the pack index %s counts %d objects, past 1<<31", path, n)
	}
	return int64(n), f, nil
}

// LayBundle is the PackRef, named packID, of the bundle the three files beside
// packPath make: section sizes from the files, the object count from OpenIdx,
// and checksum (the pack's trailer) as its writer named it; Object and Hash
// stay unset until StoreBundle fills them. A missing file fails it, naming
// which: a writer writes all three before it lays them out.
func LayBundle(packID, packPath, checksum string) (*PackRef, error) {
	base := strings.TrimSuffix(packPath, ".pack")
	var sizes [len(bundleSections)]int64
	for i, ext := range bundleSections {
		info, err := os.Stat(base + ext)
		if err != nil {
			return nil, fmt.Errorf("gitlet: the bundle's %s: %w", ext, err)
		}
		sizes[i] = info.Size()
	}
	count, idx, err := OpenIdx(base + ".idx")
	if err != nil {
		return nil, err
	}
	_ = idx.Close()
	return &PackRef{PackID: packID, Size: sizes[0] + sizes[1] + sizes[2], IndexBytes: sizes[1],
		ReverseBytes: sizes[2], GitChecksum: checksum, Objects: count}, nil
}

// DeriveOutputPackID is the pack id of output n (from 0, in the record's order)
// of the rewrite whose operation id is rewriteID ([Compaction.CompactionID],
// [ExpungeBegin.ExpungeID], [ExpungeCommit.CommitID]): an UNNAMED
// [statelog.DeriveOpID] over that id and n at its [statelog.OpMintedAt].
//
// ONE FUNCTION (ADR-0008): the node building a rewrite names outputs by it and
// a proposal is held to it before any committer decides; one byte apart and
// every rewrite is refused. DERIVED, so no output can take another pack's id
// and a re-proposal names the same outputs. UNNAMED, since a stored pack's id
// is a bare UUIDv7 as a push's is. At the rewrite's instant, never the call's
// clock, so every node derives the same id; an id carrying no instant derives
// at the zero instant, still the same everywhere.
func DeriveOutputPackID(rewriteID string, n int) string {
	at, _ := statelog.OpMintedAt(rewriteID)
	return statelog.DeriveOpID(at, "", rewriteID, strconv.Itoa(n))
}

// UploadDeadline bounds a bundle put: objstore.ReadBase (the lookup, the
// request and a retry) plus the pace floor over its bytes (objstore.PaceFor).
// A BOUND on a trickle, never a schedule: objstore.Stall ends a stopped upload
// long before it.
func UploadDeadline(size int64) time.Duration { return objstore.ReadBase + objstore.PaceFor(size) }

// BadBundle is a bundle StoreBundle STORED and its seal then refused: the bytes
// sit under Object, which no row names, until the collector's grace removes
// them. Typed, not a bare ErrBadBundle, because its caller answers it apart
// from a failed store and names the object left behind.
type BadBundle struct {
	// Object is the key the refused bytes were stored under.
	Object objstore.Key
	// Err is the seal's refusal: ErrBadBundle, naming what disagrees.
	Err error
}

// Error is the seal's refusal, which names what disagrees.
func (b *BadBundle) Error() string { return b.Err.Error() }

// Unwrap is the seal's refusal, so errors.Is meets ErrBadBundle.
func (b *BadBundle) Unwrap() error { return b.Err }

// StoreBundle uploads the bundle ref lays out (the three files beside
// packPath, limited at ref.Size) through put under UploadDeadline, gathering
// the seal on the way (SealWriter, so nothing is read twice) and holding it to
// ref (CheckBundle), the rule every installing cache applies.
//
// hit, when not nil, runs before each read the store makes, with the bytes
// handed over so far; its error fails the read. It is gitletfault's point,
// which this package never imports.
//
// On success ref names the stored object (Object, Hash). A refused seal is a
// *BadBundle, ref unchanged. Any other failure is the store's or the files' as
// it came; an *objstore.Full is answered for its room, not retried as a blip.
func StoreBundle(ctx context.Context, put Putter, packPath string, ref *PackRef,
	hit func(ctx context.Context, at int64) error) error {

	base := strings.TrimSuffix(packPath, ".pack")
	files := make([]io.Reader, 0, len(bundleSections))
	for _, ext := range bundleSections {
		f, err := os.Open(base + ext)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		files = append(files, f)
	}
	ctx, cancel := context.WithTimeout(ctx, UploadDeadline(ref.Size))
	defer cancel()
	sw := NewSealWriter(*ref)
	in := io.MultiReader(files...)
	if hit != nil {
		in = &hitReader{ctx: ctx, hit: hit, r: in}
	}
	obj, err := put.Put(ctx, io.TeeReader(in, sw), ref.Size, objstore.PutMeta{ContentType: BundleContentType})
	if err != nil {
		return err
	}
	stored := *ref
	stored.Object, stored.Hash = obj.Key, obj.Hash
	seal, err := sw.Seal()
	if err == nil {
		err = CheckBundle(stored, seal)
	}
	if err != nil {
		return &BadBundle{Object: obj.Key, Err: err}
	}
	*ref = stored
	return nil
}

// hitReader calls hit before each read of r at the offset handed over so far,
// counted from at (a layer part's place in the layer, or 0 for a bundle). It
// holds the upload's context, which Read lacks, so the hit runs under the put's
// deadline.
type hitReader struct {
	ctx context.Context
	hit func(ctx context.Context, at int64) error
	r   io.Reader
	at  int64
}

func (h *hitReader) Read(p []byte) (int, error) {
	if err := h.hit(h.ctx, h.at); err != nil {
		return 0, err
	}
	n, err := h.r.Read(p)
	h.at += int64(n)
	return n, err
}

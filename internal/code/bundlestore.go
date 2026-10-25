package code

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

// A BUNDLE LAID OUT AND STORED: the files a git wrote for one pack — the
// pack, its .idx (version 2) and its .rev (version 1) — laid out as the one
// object a stored pack is (bundle.go), and that object uploaded, sealed on
// its way to the store.
//
// ONE COPY for every writer of a stored pack (ADR-0008). The host's landing
// stores a push's pack through these functions, and whatever rewrites stored
// packs stores its outputs through them too, never through a copy of its own:
// the layout is what the seal is held to, and the seal is what every
// installing cache holds a bundle to, so a second writer that laid a bundle
// out or sealed it by its own reading would be judged by a rule it did not
// write against — and the first difference would be a row no git node can
// install, fleet-wide. The id a compaction's output is laid out under is one
// derivation for the same reason ([DeriveCompactionPackID]): the build that
// names the output and the check of the proposal that carries it must agree.
//
// Unlike bundle.go, this half does I/O: it reads the three files and streams
// them to the store it is handed. It still runs no git — the files are a
// git's, written before either function is called.

// bundleSections are the files a bundle is laid out from, in its order.
var bundleSections = [3]string{".pack", ".idx", ".rev"}

// Putter stores one object: the *objstore.Store satisfies it. Declared here,
// by its one caller, StoreBundle — a writer of a stored pack holds its store
// as a Putter only to hand it to StoreBundle, which is where the bundle's put
// and its seal are made together.
type Putter interface {
	Put(ctx context.Context, r io.Reader, limit int64, m objstore.PutMeta) (objstore.Object, error)
}

// OpenIdx opens a version-2 pack index and reads its header — the magic, the
// version and the fan-out — answering its object count (the fan-out's last
// entry) and the file positioned at the first object id, which the caller
// closes. A version-1 index, a bad magic, a header cut short or a count above
// 1<<31 is refused naming the file. The ONE reading of an index's header
// (ADR-0008): LayBundle counts a bundle's objects through it, and the host's
// stream of a push's ids reads on from where it leaves the file.
//
// Version 2 only, because it is the only version git writes by default since
// 1.5.2, the one the receive configuration pins and the one a bundle's seal
// holds its index to (CheckBundle).
func OpenIdx(path string) (count int64, f *os.File, err error) {
	f, err = os.Open(path)
	if err != nil {
		return 0, nil, fmt.Errorf("code: open the pack index %s: %w", path, err)
	}
	var head [indexHeaderBytes + fanoutBytes]byte
	if _, err = io.ReadFull(f, head[:]); err != nil {
		_ = f.Close()
		return 0, nil, fmt.Errorf("code: read the pack index %s's header: %w", path, err)
	}
	if !bytes.Equal(head[:len(indexMagic)], indexMagic) ||
		binary.BigEndian.Uint32(head[len(indexMagic):indexHeaderBytes]) != 2 {
		_ = f.Close()
		return 0, nil, fmt.Errorf("code: %s is not a version-2 pack index", path)
	}
	n := binary.BigEndian.Uint32(head[len(head)-4:])
	if n > 1<<31 {
		_ = f.Close()
		return 0, nil, fmt.Errorf("code: the pack index %s counts %d objects, past 1<<31", path, n)
	}
	return int64(n), f, nil
}

// LayBundle is the PackRef of the bundle the three files beside packPath make
// — the pack, its .idx (version 2) and its .rev (version 1), back to back —
// named packID: each section's size from its file, the object count from the
// index's fan-out (OpenIdx), and checksum, the pack's trailer, as its writer
// named it, with Object and Hash unset until StoreBundle fills them. A missing
// file fails it, naming which: a writer of a stored pack writes all three
// before it lays them out.
func LayBundle(packID, packPath, checksum string) (*PackRef, error) {
	base := strings.TrimSuffix(packPath, ".pack")
	var sizes [len(bundleSections)]int64
	for i, ext := range bundleSections {
		info, err := os.Stat(base + ext)
		if err != nil {
			return nil, fmt.Errorf("code: the bundle's %s: %w", ext, err)
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

// DeriveCompactionPackID is the pack id of output n of the compaction
// compactionID — the id [LayBundle] lays that output out under, and its
// row's — n counting the outputs from 0 in the order the build wrote them:
// an UNNAMED derivation ([statelog.DeriveOpID]) over the compaction's id and
// n, at the instant that id was minted ([statelog.OpMintedAt]).
//
// ONE FUNCTION, because two packages must agree on every byte of it without
// asking each other (ADR-0008): the git node that builds a compaction names
// each output by it before the output is stored, and a proposal of the
// compaction is held to it output by output before any committer decides it.
// A copy that disagreed in one byte would have every compaction's outputs
// refused, and a repository nothing compacts keeps every pack each push
// stored.
//
// DERIVED rather than minted, as a push's pack carries the push's own id: an
// output's id is its compaction's, so a proposal's outputs are held to the
// compaction that wrote them — none can take another pack's id — with nothing
// to look up, and the same compaction proposed again names the same outputs.
// UNNAMED, because a stored pack's id is a bare UUIDv7 in its canonical
// spelling, as a push's is: a name would follow it after a dot. AT THE
// COMPACTION'S INSTANT, the one its id carries, never the clock of the call,
// so whoever derives it, wherever and whenever, derives the same id.
//
// An id that carries no instant derives at the zero instant — statelog's
// reading of such an id, which the layout writes as the Unix epoch — and is
// still the same id on every node, which is all the two sides need of it. A
// compaction's own id always carries one: it is a bare UUIDv7.
func DeriveCompactionPackID(compactionID string, n int) string {
	at, _ := statelog.OpMintedAt(compactionID)
	return statelog.DeriveOpID(at, "", compactionID, strconv.Itoa(n))
}

// UploadDeadline is a bundle put's own bound: objstore.ReadBase — the store's
// lookup and the request around it, and a retry of either — and the pace floor
// over its bytes (objstore.PaceFor), the slowest a node's upload is allowed to
// move. A BOUND on an upload that trickles, never a schedule: the store ends
// one that stops moving (objstore.Stall) long before it.
func UploadDeadline(size int64) time.Duration { return objstore.ReadBase + objstore.PaceFor(size) }

// BadBundle is a bundle StoreBundle STORED and its own seal then refused
// (CheckBundle): its bytes are in the store under Object, which no row names,
// and the collector removes them after its grace as it does every object no
// row names. Err is the seal's refusal, wrapping ErrBadBundle and naming the
// first disagreement.
//
// Typed rather than a bare ErrBadBundle because its caller answers it apart
// from a store that failed — the bytes moved, and are not the pack its row
// would describe — and names the object it left behind.
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

// StoreBundle uploads the bundle ref lays out — the three files beside
// packPath, back to back, exactly ref.Size bytes and limited at that size —
// through put, sealing it on the way (SealWriter) and holding the seal to ref
// (CheckBundle), the one rule every installing cache holds a bundle to, all
// under UploadDeadline(ref.Size). Nothing is read twice: the seal is gathered
// from the bytes on their way to the store.
//
// hit, when not nil, is called before each read the store makes of the
// bundle, with the bytes handed over so far, and an error from it ends the
// upload as a failed read: a fault injector's point (internal/code/codefault),
// which this package never imports — the caller's closure hits it.
//
// On success ref names the stored object (Object, Hash). A bundle its seal
// refuses is a *BadBundle, ref unchanged. Any other failure is the store's or
// the files', as it came — an *objstore.Full among them, which a caller
// answers for the room it names rather than as a blip to retry.
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

// hitReader calls hit before each read of r, at the offset of the bytes r has
// handed over so far. It holds the upload's context because io.Reader's Read
// takes none: the hit is made under the deadline the put runs under.
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

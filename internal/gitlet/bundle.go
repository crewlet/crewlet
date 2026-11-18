package gitlet

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"github.com/crewlet/crewlet/internal/gitref"
)

// THE PACK BUNDLE: the ONE object a push stores — its pack, then the .idx (v2)
// and .rev (v1) the receiving receive-pack wrote, back to back under one key —
// and the ONE rule every node holds it to. Why one object, no re-indexing and
// no pack past 2 GiB: the package doc's "The bundle is the permanent format".
//
// PURE OVER BYTES IT IS HANDED: no I/O and no git. bundlestore.go tees the
// upload through a [SealWriter] and checks the seal before the receiver may
// propose the push ([StoreBundle]); the cache tees its download and checks
// before it installs. SHA-256 says the bytes are the ones stored; this says
// they are the ones the ROW describes, a disagreement no retry fixes.

// BundleContentType is the content type a pack bundle is stored under: not a
// packfile — a console that hands the object back hands back three sections.
const BundleContentType = "application/vnd.crewlet.git-pack-bundle"

// The sections' fixed parts, as git writes them (Documentation/gitformat-pack).
const (
	// packHeaderBytes is "PACK", a version and an object count.
	packHeaderBytes = 12
	// hashBytes is a SHA-1: a pack's trailer, and each index's two
	// checksums.
	hashBytes = 20
	// minPackBytes is a header and a trailer.
	minPackBytes = packHeaderBytes + hashBytes
	// indexHeaderBytes is "\377tOc" and version 2.
	indexHeaderBytes = 8
	// fanoutBytes is the index's 256 cumulative uint32 counts; the last is
	// the object count.
	fanoutBytes = 256 * 4
	// reverseHeaderBytes is "RIDX", version 1 and a hash id.
	reverseHeaderBytes = 12
)

// IndexBytesFor is the exact size of a version-2 .idx for n objects: header,
// fan-out, per object a 20-byte id, a CRC-32 and a 4-byte offset, then two
// checksums. No 64-bit offset table: [MaxPackBytes] keeps every stored pack
// from needing one. 1 156 bytes for three objects, as git 2.43 writes it.
func IndexBytesFor(n int64) int64 {
	return indexHeaderBytes + fanoutBytes + n*(hashBytes+4+4) + 2*hashBytes
}

// ReverseBytesFor is the exact size of a version-1 .rev for n objects: header,
// a uint32 per object, then two checksums. 64 bytes for three objects.
func ReverseBytesFor(n int64) int64 {
	return reverseHeaderBytes + n*4 + 2*hashBytes
}

// BundleSeal is what a bundle says about itself, read from the heads and the
// tails of its three sections.
type BundleSeal struct {
	// PackHead is "PACK", the version (2 or 3) and the object count.
	PackHead [packHeaderBytes]byte
	// PackTrailer is the pack's last 20 bytes: its SHA-1.
	PackTrailer [hashBytes]byte
	// IndexHead is "\377tOc" and version 2.
	IndexHead [indexHeaderBytes]byte
	// IndexCount is the fan-out's last entry: the objects the index lists.
	IndexCount [4]byte
	// IndexPack is the index's pack-checksum field.
	IndexPack [hashBytes]byte
	// ReverseHead is "RIDX", version 1 and hash id 1 (SHA-1).
	ReverseHead [reverseHeaderBytes]byte
	// ReversePack is the reverse index's pack-checksum field.
	ReversePack [hashBytes]byte
}

// SealWriter is an io.Writer a bundle's bytes are teed through IN ORDER — the
// receiver's upload and the cache's download alike — gathering the
// [BundleSeal] of a bundle laid out as its [PackRef] says. It keeps a few
// dozen bytes, never the bundle.
type SealWriter struct {
	ref     PackRef
	n       int64
	seal    BundleSeal
	windows []sealWindow
	layout  error
}

// sealWindow is one span of the bundle a seal field is copied from.
type sealWindow struct {
	start int64
	dst   []byte
}

// NewSealWriter gathers the seal of a bundle laid out as ref says.
func NewSealWriter(ref PackRef) *SealWriter {
	w := &SealWriter{ref: ref}
	pack, index, reverse := ref.PackBytes(), ref.IndexBytes, ref.ReverseBytes
	if pack < minPackBytes || index < IndexBytesFor(0) || reverse < ReverseBytesFor(0) {
		// A LAYOUT NO BUNDLE CAN HAVE gathers nothing: its sections would
		// overlap, and a seal read from them would describe no section.
		w.layout = fmt.Errorf("%w: the row lays the bundle out as a %d-byte pack, a "+
			"%d-byte index and a %d-byte reverse index, and no section can be that small",
			ErrBadBundle, pack, index, reverse)
		return w
	}
	s := &w.seal
	w.windows = []sealWindow{
		{0, s.PackHead[:]},
		{pack - hashBytes, s.PackTrailer[:]},
		{pack, s.IndexHead[:]},
		{pack + indexHeaderBytes + fanoutBytes - 4, s.IndexCount[:]},
		{pack + index - 2*hashBytes, s.IndexPack[:]},
		{pack + index, s.ReverseHead[:]},
		{pack + index + reverse - 2*hashBytes, s.ReversePack[:]},
	}
	return w
}

// Write gathers whatever seal fields p overlaps and never fails.
func (w *SealWriter) Write(p []byte) (int, error) {
	from, to := w.n, w.n+int64(len(p))
	for _, win := range w.windows {
		end := win.start + int64(len(win.dst))
		lo, hi := max(from, win.start), min(to, end)
		if lo < hi {
			copy(win.dst[lo-win.start:hi-win.start], p[lo-from:hi-from])
		}
	}
	w.n = to
	return len(p), nil
}

// Seal is the seal of exactly ref.Size bytes, or an error naming how many
// passed.
func (w *SealWriter) Seal() (BundleSeal, error) {
	switch {
	case w.layout != nil:
		return BundleSeal{}, w.layout
	case w.n != w.ref.Size:
		return BundleSeal{}, fmt.Errorf("%w: %d bytes passed and the row says the "+
			"bundle is %d", ErrBadBundle, w.n, w.ref.Size)
	}
	return w.seal, nil
}

// The magic numbers a seal is held to.
var (
	packMagic    = []byte("PACK")
	indexMagic   = []byte("\377tOc")
	reverseMagic = []byte("RIDX")
)

// Check holds a [PackRef] to a bundle its row can describe, from the row alone
// before any byte (the row-level half of [CheckBundle]): a nameable object, at
// least one object, both indexes exactly their formats' size for that count, a
// pack at least a header and trailer long, the whole within [MaxPackBytes],
// and a git checksum that can be a pack's trailer.
//
// ONE RULE FOR EVERY HOLDER OF A ROW (ADR-0008): the receiver's proposal, the
// cache before it downloads, and [CheckBundle]. A row the domain admitted and a
// cache refused would leave a repository no Gitlet node can sync, for good.
//
// The answer names the row's JSON field first ("size: …"); each holder wraps
// it with its own sentinel.
func (p PackRef) Check() error {
	if err := p.Content().Validate(); err != nil {
		return fmt.Errorf("object: %w", err)
	}
	switch {
	case p.Objects < 1:
		return fmt.Errorf("objects: the row counts %d objects, and a stored pack holds at least one", p.Objects)
	case p.IndexBytes != IndexBytesFor(p.Objects):
		return fmt.Errorf("index_bytes: the index is %d bytes and a version-2 index of %d objects is %d",
			p.IndexBytes, p.Objects, IndexBytesFor(p.Objects))
	case p.ReverseBytes != ReverseBytesFor(p.Objects):
		return fmt.Errorf("reverse_bytes: the reverse index is %d bytes and a version-1 one of %d objects is %d",
			p.ReverseBytes, p.Objects, ReverseBytesFor(p.Objects))
	case p.PackBytes() < minPackBytes:
		return fmt.Errorf("size: the pack section is %d bytes, under a pack's own %d-byte header and trailer",
			p.PackBytes(), minPackBytes)
	case p.Size > MaxPackBytes:
		return fmt.Errorf("size: the bundle is %d bytes, past the %d one push may store", p.Size, MaxPackBytes)
	case !gitref.ValidOID(p.GitChecksum):
		return fmt.Errorf("git_checksum: %q is not 40 hex digits", p.GitChecksum)
	case p.GitChecksum == ZeroOID:
		// git's null id: what it writes for "no object", and no pack's
		// trailer — and the name of the files a cache would install.
		return fmt.Errorf("git_checksum: the zero id is no pack's trailer")
	}
	return nil
}

// CheckBundle holds a bundle's seal to the [PackRef] that names it (ADR-0008):
// the receiver before it proposes a push, every Gitlet node before it installs
// one. The first disagreement is [ErrBadBundle] with both values: the row's
// own fields first ([PackRef.Check]), then the seal against them.
func CheckBundle(ref PackRef, s BundleSeal) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrBadBundle}, args...)...)
	}
	if err := ref.Check(); err != nil {
		return fmt.Errorf("%w: %w", ErrBadBundle, err)
	}
	checksum, err := hex.DecodeString(ref.GitChecksum)
	if err != nil {
		return bad("the row's git checksum %q: %v", ref.GitChecksum, err)
	}

	head := s.PackHead[:]
	version := binary.BigEndian.Uint32(head[4:8])
	switch count := int64(binary.BigEndian.Uint32(head[8:12])); {
	case !bytes.Equal(head[:4], packMagic):
		return bad("the pack section begins %q, not %q", head[:4], packMagic)
	case version != 2 && version != 3:
		return bad("the pack is version %d, not 2 or 3", version)
	case count != ref.Objects:
		return bad("the pack's header counts %d objects and the row %d", count, ref.Objects)
	}
	switch index := s.IndexHead[:]; {
	case !bytes.Equal(index[:4], indexMagic):
		return bad("the index section begins %q, not %q", index[:4], indexMagic)
	case binary.BigEndian.Uint32(index[4:8]) != 2:
		return bad("the index is version %d, not 2", binary.BigEndian.Uint32(index[4:8]))
	}
	if count := int64(binary.BigEndian.Uint32(s.IndexCount[:])); count != ref.Objects {
		return bad("the index's fan-out counts %d objects and the row %d", count, ref.Objects)
	}
	switch reverse := s.ReverseHead[:]; {
	case !bytes.Equal(reverse[:4], reverseMagic):
		return bad("the reverse index section begins %q, not %q", reverse[:4], reverseMagic)
	case binary.BigEndian.Uint32(reverse[4:8]) != 1:
		return bad("the reverse index is version %d, not 1", binary.BigEndian.Uint32(reverse[4:8]))
	case binary.BigEndian.Uint32(reverse[8:12]) != 1:
		return bad("the reverse index is for hash %d, not 1 (SHA-1)", binary.BigEndian.Uint32(reverse[8:12]))
	}
	for _, c := range []struct {
		name string
		sum  []byte
	}{
		{"the pack's trailer", s.PackTrailer[:]},
		{"the index's pack checksum", s.IndexPack[:]},
		{"the reverse index's pack checksum", s.ReversePack[:]},
	} {
		if !bytes.Equal(c.sum, checksum) {
			return bad("%s is %x and the row's git checksum %s", c.name, c.sum, ref.GitChecksum)
		}
	}
	return nil
}

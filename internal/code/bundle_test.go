package code_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
)

// realBundle is a pack the client's git wrote — three commits' objects — read
// back as the one object a push stores, with the row that names it.
func realBundle(t *testing.T) (gittest.Bundled, code.PackRef) {
	t.Helper()
	c := gittest.NewClient(t)
	work := c.Init(t)
	var head string
	for i, body := range []string{"one\n", "two\n", "three\n"} {
		head = c.Commit(t, work, map[string]string{"file.txt": body}, "commit "+string(rune('a'+i)))
	}
	bare := gittest.Empty(t)
	b := gittest.Bundle(t, bare, gittest.Pack(t, c, bare, work, head))
	ref := code.PackRef{
		PackID: "p1", Object: keyOf("p1"), Hash: objstore.HashOf(b.Bytes),
		Size: int64(len(b.Bytes)), IndexBytes: b.IndexBytes, ReverseBytes: b.ReverseBytes,
		GitChecksum: b.Checksum, Objects: b.Objects,
	}
	return b, ref
}

// seal tees bytes through a seal writer in chunks of n, the way an upload or a
// download arrives.
func seal(t *testing.T, ref code.PackRef, bytes []byte, n int) (code.BundleSeal, error) {
	t.Helper()
	w := code.NewSealWriter(ref)
	for len(bytes) > 0 {
		k := min(n, len(bytes))
		if _, err := w.Write(bytes[:k]); err != nil {
			t.Fatalf("a seal writer failed a write: %v", err)
		}
		bytes = bytes[k:]
	}
	return w.Seal()
}

// THE INDEXES' SIZES ARE PURE FUNCTIONS OF THE OBJECT COUNT, as git writes
// them — measured on a pack git made, not restated from the format document —
// which is the premise every size comparison the check makes rests on.
func TestTheIndexSizesAreWhatGitWrites(t *testing.T) {
	t.Parallel()
	b, _ := realBundle(t)
	if b.IndexBytes != code.IndexBytesFor(b.Objects) || b.ReverseBytes != code.ReverseBytesFor(b.Objects) {
		t.Fatalf("git wrote a %d-byte index and a %d-byte reverse index for %d objects; the "+
			"functions say %d and %d", b.IndexBytes, b.ReverseBytes, b.Objects,
			code.IndexBytesFor(b.Objects), code.ReverseBytesFor(b.Objects))
	}
	if code.IndexBytesFor(3) != 1156 || code.ReverseBytesFor(3) != 64 {
		t.Fatalf("the documented three-object sizes are %d and %d", code.IndexBytesFor(3), code.ReverseBytesFor(3))
	}
}

// A BUNDLE IS CHECKED AGAINST THE ROW THAT NAMES IT, however its bytes arrive:
// a sealed real pack passes in every chunking, and every disagreement — a
// byte of a section's magic, version or hash id, a checksum, a count, a
// length, a size past MaxPackBytes or a pack section too short to be one — is
// ErrBadBundle, refused by the clause written for it.
func TestABundleIsTheOneItsRowDescribes(t *testing.T) {
	t.Parallel()
	b, ref := realBundle(t)
	var sealed code.BundleSeal
	for _, n := range []int{1, 7, 4096, len(b.Bytes)} {
		s, err := seal(t, ref, b.Bytes, n)
		if err != nil {
			t.Fatalf("seal in chunks of %d: %v", n, err)
		}
		if err := code.CheckBundle(ref, s); err != nil {
			t.Fatalf("a bundle git wrote, sealed in chunks of %d, was refused: %v", n, err)
		}
		sealed = s
	}

	pack, index, reverse := b.PackBytes, b.IndexBytes, b.ReverseBytes
	flip := func(at int64) []byte {
		out := append([]byte(nil), b.Bytes...)
		out[at] ^= 0xff
		return out
	}
	// grown is the bundle laid out for ONE MORE object than its pack holds:
	// each index gains its per-object bytes before its two checksums, so
	// every section's head and tail sit where a row counting one more object
	// says they do and only the counts disagree.
	grown := func() []byte {
		var out []byte
		out = append(out, b.Bytes[:pack+index-40]...)
		out = append(out, make([]byte, 28)...)
		out = append(out, b.Bytes[pack+index-40:pack+index+reverse-40]...)
		out = append(out, make([]byte, 4)...)
		return append(out, b.Bytes[pack+index+reverse-40:]...)
	}()
	// Each case names the clause it must be refused BY (a substring of that
	// clause's message): a case refused by an earlier clause would pass while
	// the clause it was written for is gone.
	//
	// A case marked rowOnly checks the real bundle's OWN seal against the
	// changed row rather than sealing bytes under it: its row describes a
	// layout no bytes a test can afford are sealed under — a bundle past
	// MaxPackBytes, or a pack section too short for a header — and a seal of
	// anything else (the zero seal included) is refused by its pack's magic
	// first. On the real seal every other clause passes, so the one the case
	// names is the only thing that can refuse it.
	for name, tc := range map[string]struct {
		bytes   []byte
		ref     func(code.PackRef) code.PackRef
		rowOnly bool
		by      string
	}{
		"the pack's magic":                  {bytes: flip(0), by: "the pack section begins"},
		"the pack's version":                {bytes: flip(7), by: "the pack is version"},
		"the pack's object count":           {bytes: flip(11), by: "the pack's header counts"},
		"the pack's trailer":                {bytes: flip(pack - 1), by: "the pack's trailer"},
		"the index's magic":                 {bytes: flip(pack), by: "the index section begins"},
		"the index's version":               {bytes: flip(pack + 7), by: "the index is version"},
		"the index's fan-out total":         {bytes: flip(pack + 8 + 1024 - 1), by: "the index's fan-out counts"},
		"the index's pack checksum":         {bytes: flip(pack + index - 40), by: "the index's pack checksum"},
		"the reverse index's magic":         {bytes: flip(pack + index), by: "the reverse index section begins"},
		"the reverse index's version":       {bytes: flip(pack + index + 7), by: "the reverse index is version"},
		"the reverse index's hash":          {bytes: flip(pack + index + 11), by: "the reverse index is for hash"},
		"the reverse index's pack checksum": {bytes: flip(pack + index + reverse - 40), by: "the reverse index's pack checksum"},
		"a row naming another checksum": {
			ref: func(r code.PackRef) code.PackRef { r.GitChecksum = oidOf("x"); return r },
			by:  "the pack's trailer",
		},
		"a row naming no checksum": {
			ref: func(r code.PackRef) code.PackRef { r.GitChecksum = "zz"; return r },
			by:  "is not 40 hex digits",
		},
		"a row counting another number": {bytes: grown, ref: func(r code.PackRef) code.PackRef {
			r.Objects++
			r.IndexBytes, r.ReverseBytes = code.IndexBytesFor(r.Objects), code.ReverseBytesFor(r.Objects)
			r.Size += 28 + 4
			return r
		}, by: "the pack's header counts"},
		"a row counting no objects": {
			ref: func(r code.PackRef) code.PackRef { r.Objects = 0; return r },
			by:  "a stored pack holds at least one",
		},
		"a row with the wrong index length": {
			ref: func(r code.PackRef) code.PackRef { r.IndexBytes += 8; return r },
			by:  "a version-2 index of",
		},
		"a row with the wrong reverse index length": {
			ref: func(r code.PackRef) code.PackRef { r.ReverseBytes += 4; return r },
			by:  "a version-1 one of",
		},
		// THE ONE PLACE MaxPackBytes IS ENFORCED — what the receiver and every
		// cache refuse a bundle past, and what keeps every stored index free
		// of the 64-bit offset table the exact-size clauses assume is absent.
		"a bundle past MaxPackBytes": {
			ref:     func(r code.PackRef) code.PackRef { r.Size = code.MaxPackBytes + 1; return r },
			rowOnly: true, by: "past the",
		},
		"a pack section under a pack's header and trailer": {
			ref: func(r code.PackRef) code.PackRef {
				r.Size = r.IndexBytes + r.ReverseBytes + 10
				return r
			},
			rowOnly: true, by: "under a pack's own",
		},
	} {
		t.Run(name, func(t *testing.T) {
			bytes, r := b.Bytes, ref
			if tc.bytes != nil {
				bytes = tc.bytes
			}
			if tc.ref != nil {
				r = tc.ref(r)
			}
			s := sealed
			if !tc.rowOnly {
				var err error
				if s, err = seal(t, r, bytes, 4096); err != nil {
					t.Fatalf("the bundle did not seal, so no clause of the check was reached: %v", err)
				}
			}
			err := code.CheckBundle(r, s)
			if !errors.Is(err, code.ErrBadBundle) {
				t.Fatalf("CheckBundle = %v, want ErrBadBundle", err)
			}
			if !strings.Contains(err.Error(), tc.by) {
				t.Fatalf("CheckBundle = %v, want the clause naming %q", err, tc.by)
			}
		})
	}

	if _, err := seal(t, ref, b.Bytes[:len(b.Bytes)-1], 4096); !errors.Is(err, code.ErrBadBundle) {
		t.Fatalf("a bundle one byte short sealed: %v", err)
	}
	tiny := ref
	tiny.Size, tiny.IndexBytes, tiny.ReverseBytes = 10, 4, 4
	if _, err := seal(t, tiny, make([]byte, 10), 4); !errors.Is(err, code.ErrBadBundle) {
		t.Fatalf("a layout no bundle can have sealed: %v", err)
	}
}

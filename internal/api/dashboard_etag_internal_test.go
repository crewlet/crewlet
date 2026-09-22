package api

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"testing/fstest"
)

// Internal for the same reason the traversal cases are: the width is a
// property of how the validator is DERIVED, and an external test can only see
// a quoted string whose length it would have to restate as a literal — which
// is the bare number [etagDigestBytes] exists to remove.

// The invariant: an ETag is the first [etagDigestBytes] of the SHA-256 OF THE
// BODY, hex, quoted — so the validator changes whenever the asset does.
//
// It fails in both directions that matter. Derive the value from anything but
// the content (the path, a build stamp, a counter) and the comparison against
// a recomputed digest goes red; take a different number of bytes than the
// named constant and it goes red too.
func TestAnETagIsTheNamedWidthOfTheBodysDigest(t *testing.T) {
	t.Parallel()
	body := []byte("<!doctype html><title>shell</title>")
	a := newAssets(fstest.MapFS{"dashboard/index.html": {Data: body}})

	sum := sha256.Sum256(body)
	want := `"` + hex.EncodeToString(sum[:etagDigestBytes]) + `"`
	if got := a.etagFor("dashboard/index.html", body); got != want {
		t.Errorf("ETag = %s, want %s — a validator that is not etagDigestBytes "+
			"of the BODY's digest either leaves a changed module revalidating "+
			"as unchanged, or carries a width the constant no longer names",
			got, want)
	}
}

// The invariant: the width never drops below the arithmetic stated at
// [etagDigestBytes].
//
// That comment defends 80 bits as a PAIRWISE probability — one cached
// validator against this build's, 2^-80 — and a shrunk constant would leave
// the reasoning in place while the number no longer supports it. A collision
// here is not a failure anybody sees: the browser keeps a stale module and
// runs half the old app against half the new one.
//
// A floor rather than an equality, because lengthening needs no review.
func TestTheETagDigestIsNeverNarrowedBelowItsStatedArithmetic(t *testing.T) {
	t.Parallel()
	const floor = 10 // 80 bits, the width the comment's 2^-80 is computed at.
	if etagDigestBytes < floor {
		t.Errorf("etagDigestBytes = %d bytes (%d bits), below the %d the "+
			"doc comment's arithmetic rests on", etagDigestBytes,
			etagDigestBytes*8, floor)
	}
}

// The counterfactual for the first case: two assets must not share a
// validator, because a shared one would serve one file's body for the other's
// 304. Here rather than only in the external suite because that one compares
// two real dashboard paths, so it would keep passing if the digest stopped
// covering the body and started covering the name.
func TestTwoBodiesGetTwoETags(t *testing.T) {
	t.Parallel()
	a := newAssets(fstest.MapFS{})
	if first, second := a.etagFor("a.js", []byte("one")), a.etagFor("b.js", []byte("two")); first == second {
		t.Errorf("two different bodies share the ETag %s", first)
	}
}

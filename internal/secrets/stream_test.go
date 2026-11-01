package secrets

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

// streamRing is a keyring of the given ids, the first active, each key
// distinct.
func streamRing(t *testing.T, ids ...string) Keyring {
	t.Helper()
	ring := Keyring{ActiveID: ids[0], Keys: map[string][]byte{}}
	for i, id := range ids {
		ring.Keys[id] = bytes.Repeat([]byte{byte(i + 1)}, keyBytes)
	}
	return ring
}

// smallSealer is a sealer over ring whose chunks are chunk bytes, so a case
// can walk every chunk boundary of a stream a few dozen bytes long.
func smallSealer(t *testing.T, ring Keyring, chunk int) *StreamSealer {
	t.Helper()
	s, err := NewStreamSealer(ring)
	if err != nil {
		t.Fatalf("NewStreamSealer: %v", err)
	}
	s.chunk = chunk
	return s
}

// sealAll seals plain under s, bound to aad, and answers the stream.
func sealAll(t *testing.T, s *StreamSealer, plain, aad []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, _, err := s.Seal(&out, aad)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	// IN UNEVEN PIECES, so a chunk boundary falls inside a Write as often
	// as between two.
	for rest := plain; len(rest) > 0; {
		n := min(len(rest), 7)
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		rest = rest[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

// openAll opens a stream under s and reads it to its end.
func openAll(s *StreamSealer, stream, aad []byte) ([]byte, error) {
	r, err := s.Open(bytes.NewReader(stream), aad)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// random is n random bytes.
func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// A SEALED STREAM OPENS TO WHAT WAS SEALED, at every length a chunk boundary
// can fall at: empty, short of one chunk, exactly one, one and a byte, several
// exactly, and several with a tail; and its length is what SealedStreamSize
// promises, which is what an object's size cap is stated in.
//
// Mutation: seal a full last chunk as not-final (flush it on Close only when it
// is short) and every exact multiple fails to open.
func TestASealedStreamOpensToWhatWasSealed(t *testing.T) {
	t.Parallel()
	const chunk = 16
	s := smallSealer(t, streamRing(t, "k1"), chunk)
	aad := []byte("executor/seat/turn/object")
	for _, n := range []int{0, 1, chunk - 1, chunk, chunk + 1, 3 * chunk, 3*chunk + 5} {
		plain := random(t, n)
		stream := sealAll(t, s, plain, aad)
		if want := sealedSize(int64(n), "k1", chunk); int64(len(stream)) != want {
			t.Errorf("%d bytes sealed to %d, and the size rule says %d", n, len(stream), want)
		}
		got, err := openAll(s, stream, aad)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%d bytes: opened (%d bytes, %v), want the plaintext back", n, len(got), err)
		}
	}
}

// THE SHIPPED CHUNK SIZE ROUND-TRIPS A STREAM SEVERAL CHUNKS LONG, so the
// production format (not only the cases' small chunks) is the one proved.
//
// Mutation: read chunks of another size than the writer seals and this fails.
func TestTheShippedChunkSizeRoundTrips(t *testing.T) {
	t.Parallel()
	s, err := NewStreamSealer(streamRing(t, "k1"))
	if err != nil {
		t.Fatal(err)
	}
	plain := random(t, 3*StreamChunkBytes+123)
	stream := sealAll(t, s, plain, []byte("aad"))
	got, err := openAll(s, stream, []byte("aad"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("opened (%d bytes, %v), want %d bytes back", len(got), err, len(plain))
	}
	if want := SealedStreamSize(int64(len(plain)), "k1"); int64(len(stream)) != want {
		t.Fatalf("sealed to %d bytes, and SealedStreamSize says %d", len(stream), want)
	}
}

// EVERY BYTE IS AUTHENTICATED: a stream with any one byte flipped (in its
// header, a chunk or a tag) does not open, and says ErrDecrypt rather than
// handing back a plaintext.
//
// Mutation: leave the salt out of the key derivation, or the version check out
// of Open, and a flipped salt or version byte opens. (A flipped key-id byte
// names an id this ring does not hold and is refused at the lookup whatever the
// derivation does; leaving the id out of the derivation is caught by
// TestAHeaderRenamedToAnotherKeyOfTheSameMaterialDoesNotOpen.)
func TestEveryByteOfASealedStreamIsAuthenticated(t *testing.T) {
	t.Parallel()
	s := smallSealer(t, streamRing(t, "k1", "k2"), 16)
	aad := []byte("aad")
	stream := sealAll(t, s, random(t, 40), aad)
	for i := range stream {
		tampered := bytes.Clone(stream)
		tampered[i] ^= 0x01
		if got, err := openAll(s, tampered, aad); !errors.Is(err, ErrDecrypt) {
			t.Errorf("byte %d flipped: opened (%d bytes, %v), want ErrDecrypt", i, len(got), err)
		}
	}
}

// A STREAM CUT SHORT DOES NOT READ AS A SHORTER ONE: cut at every chunk
// boundary (where a naive reader sees a clean end) and inside every chunk, it
// fails with ErrDecrypt rather than ending at io.EOF.
//
// Mutation: drop the final flag from streamAAD and a stream cut at a chunk
// boundary opens its last whole chunk as the end. (A reader that guesses
// final from a short read alone and never peeks fails a cut here too, on the
// empty read after it; what catches that reader is the exact-multiple round
// trip in TestASealedStreamOpensToWhatWasSealed.)
func TestASealedStreamCutShortIsRefused(t *testing.T) {
	t.Parallel()
	const chunk = 16
	s := smallSealer(t, streamRing(t, "k1"), chunk)
	aad := []byte("aad")
	stream := sealAll(t, s, random(t, 3*chunk+5), aad)
	header := headerSize("k1")
	for cut := header; cut < len(stream); cut++ {
		if _, err := openAll(s, stream[:cut], aad); !errors.Is(err, ErrDecrypt) {
			t.Errorf("cut at byte %d of %d: %v, want ErrDecrypt", cut, len(stream), err)
		}
	}
	for cut := range header {
		if _, err := openAll(s, stream[:cut], aad); !errors.Is(err, ErrDecrypt) {
			t.Errorf("cut inside the header at %d: %v, want ErrDecrypt", cut, err)
		}
	}
}

// A STREAM WITH CHUNKS REORDERED, OR ONE APPENDED, DOES NOT OPEN.
//
// Mutation: drop the index from the nonce and the associated data, and two
// swapped chunks open. (A stream extended by a copy of its own last chunk is
// refused twice over — the original last chunk now reads as not final, and the
// copy is opened at an index it was not sealed at — so no one mutation shows
// here; what the final flag alone catches is a cut, in
// TestASealedStreamCutShortIsRefused.)
func TestASealedStreamReorderedOrExtendedIsRefused(t *testing.T) {
	t.Parallel()
	const chunk = 16
	s := smallSealer(t, streamRing(t, "k1"), chunk)
	aad := []byte("aad")
	stream := sealAll(t, s, random(t, 3*chunk), aad)
	header, sealedChunk := headerSize("k1"), chunk+streamTagBytes
	first := stream[header : header+sealedChunk]
	second := stream[header+sealedChunk : header+2*sealedChunk]

	swapped := bytes.Clone(stream)
	copy(swapped[header:], second)
	copy(swapped[header+sealedChunk:], first)
	if _, err := openAll(s, swapped, aad); !errors.Is(err, ErrDecrypt) {
		t.Errorf("two chunks swapped: %v, want ErrDecrypt", err)
	}
	extended := append(bytes.Clone(stream), stream[len(stream)-sealedChunk:]...)
	if _, err := openAll(s, extended, aad); !errors.Is(err, ErrDecrypt) {
		t.Errorf("the last chunk appended again: %v, want ErrDecrypt", err)
	}
	if _, err := openAll(s, append(bytes.Clone(stream), 0), aad); !errors.Is(err, ErrDecrypt) {
		t.Errorf("one byte appended: %v, want ErrDecrypt", err)
	}
}

// A HEADER RENAMED TO ANOTHER KEY OF THE SAME MATERIAL DOES NOT OPEN: the key
// id is in the derivation, so a stream whose header is rewritten to name
// another id of the ring derives another key even where the two ids hold the
// same bytes — as two ids for one key do after a re-import under a new name.
//
// Mutation: leave the key id out of the info streamAEAD derives under and the
// renamed stream opens.
func TestAHeaderRenamedToAnotherKeyOfTheSameMaterialDoesNotOpen(t *testing.T) {
	t.Parallel()
	material := bytes.Repeat([]byte{5}, keyBytes)
	s := smallSealer(t, Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": material, "k2": material}}, 16)
	plain := random(t, 40)
	stream := sealAll(t, s, plain, []byte("aad"))
	if got, err := openAll(s, stream, []byte("aad")); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("the premise: the stream opened as (%d bytes, %v)", len(got), err)
	}
	renamed := bytes.Clone(stream)
	// THE ID'S LAST BYTE, right after the magic, the version and the id's
	// length: "k1" becomes "k2", an id of the same length the ring holds.
	at := len(streamMagic) + 2 + 1
	if renamed[at] != '1' {
		t.Fatalf("the premise: byte %d of the header is %q, want the id's last byte", at, renamed[at])
	}
	renamed[at] = '2'
	if got, err := openAll(s, renamed, []byte("aad")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("the header renamed to k2: opened (%d bytes, %v), want ErrDecrypt", len(got), err)
	}
}

// A STREAM IS BOUND TO WHERE IT BELONGS: opened under another associated data
// (the same object named by another record) it does not open.
//
// Mutation: leave the caller's associated data out of streamAAD and this opens.
func TestASealedStreamOpensOnlyUnderItsOwnAssociatedData(t *testing.T) {
	t.Parallel()
	s := smallSealer(t, streamRing(t, "k1"), 16)
	stream := sealAll(t, s, random(t, 40), []byte("executor/a/turn-1/object"))
	for _, other := range [][]byte{
		[]byte("executor/a/turn-2/object"), []byte("executor/b/turn-1/object"), nil,
	} {
		if _, err := openAll(s, stream, other); !errors.Is(err, ErrDecrypt) {
			t.Errorf("opened under %q: %v, want ErrDecrypt", other, err)
		}
	}
}

// A ROTATION READS WHAT ITS RETIRED KEY SEALED, while the keyring still holds
// it, and nothing once it is dropped; a key id the keyring never held is
// ErrDecrypt, not a different error an attacker could probe for.
//
// Mutation: open under the active key rather than the one the header names,
// and the rotated read fails.
func TestARotationReadsWhatItsRetiredKeySealed(t *testing.T) {
	t.Parallel()
	retired, current := bytes.Repeat([]byte{7}, keyBytes), bytes.Repeat([]byte{9}, keyBytes)
	before := smallSealer(t, Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": retired}}, 16)
	plain := random(t, 50)
	stream := sealAll(t, before, plain, []byte("aad"))

	after := smallSealer(t, Keyring{ActiveID: "k2",
		Keys: map[string][]byte{"k1": retired, "k2": current}}, 16)
	if after.ActiveKeyID() != "k2" {
		t.Fatalf("the premise: k2 seals after the rotation, and %q does", after.ActiveKeyID())
	}
	got, err := openAll(after, stream, []byte("aad"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("a stream sealed under the retired k1 opened (%d bytes, %v), want it back", len(got), err)
	}
	dropped := smallSealer(t, Keyring{ActiveID: "k2", Keys: map[string][]byte{"k2": current}}, 16)
	if _, err := openAll(dropped, stream, []byte("aad")); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("a stream under a dropped key: %v, want ErrDecrypt", err)
	}
}

// WHAT IS NOT A SEALED STREAM IS REFUSED AT ITS HEADER: an envelope, a
// plaintext, an empty object.
func TestWhatIsNotASealedStreamIsRefused(t *testing.T) {
	t.Parallel()
	s := smallSealer(t, streamRing(t, "k1"), 16)
	for name, body := range map[string][]byte{
		"empty":     nil,
		"plaintext": []byte("a plaintext archive, never sealed"),
		"envelope":  []byte(EnvelopePrefix + "k1:AAAA"),
	} {
		if _, err := openAll(s, body, nil); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: %v, want ErrDecrypt", name, err)
		}
	}
}

// A SOURCE THAT FAILS IS NOT A TAMPERED STREAM: its error comes back as it was,
// so a caller can tell a network that went away (retry) from a stream that is
// not what was sealed (give up).
//
// Mutation: map every read error to ErrDecrypt and this fails.
func TestASourceFailureIsNotADecryptFailure(t *testing.T) {
	t.Parallel()
	s := smallSealer(t, streamRing(t, "k1"), 16)
	stream := sealAll(t, s, random(t, 40), []byte("aad"))
	broken := errors.New("the connection went away")
	r, err := s.Open(io.MultiReader(bytes.NewReader(stream[:len(stream)/2]), failing{broken}),
		[]byte("aad"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := io.ReadAll(r); !errors.Is(err, broken) || errors.Is(err, ErrDecrypt) {
		t.Fatalf("read through a failing source: %v, want the source's own error", err)
	}
}

// failing is a reader that fails with err.
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// A KEYRING WHOSE KEY ID A HEADER CANNOT NAME IS REFUSED where the sealer is
// built, not at the first stream nobody can open.
func TestAKeyIDAHeaderCannotNameIsRefused(t *testing.T) {
	t.Parallel()
	long := string(bytes.Repeat([]byte{'k'}, MaxStreamKeyIDBytes+1))
	if _, err := NewStreamSealer(streamRing(t, long)); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a %d-byte key id: %v, want ErrNoKey", len(long), err)
	}
}

package canon

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"strconv"
)

// DigestSize is a digest's length in bytes.
const DigestSize = sha256.Size

// Digest is a store's state digest: a 256-bit unsigned integer, big-endian,
// the sum modulo two to the 256th of its terms. The zero Digest is the digest
// of an empty store.
type Digest [DigestSize]byte

// Term is one SHA-256 the digest sums.
type Term [DigestSize]byte

// Add is d plus t, modulo two to the 256th.
func (d Digest) Add(t Term) Digest {
	var out Digest
	var carry uint64
	for i := 3; i >= 0; i-- {
		lo := i * 8
		sum, c := bits.Add64(binary.BigEndian.Uint64(d[lo:]), binary.BigEndian.Uint64(t[lo:]), carry)
		binary.BigEndian.PutUint64(out[lo:], sum)
		carry = c
	}
	return out
}

// Sub is d minus t, modulo two to the 256th: what removing a term the sum
// once added leaves.
func (d Digest) Sub(t Term) Digest {
	var out Digest
	var borrow uint64
	for i := 3; i >= 0; i-- {
		lo := i * 8
		diff, b := bits.Sub64(binary.BigEndian.Uint64(d[lo:]), binary.BigEndian.Uint64(t[lo:]), borrow)
		binary.BigEndian.PutUint64(out[lo:], diff)
		borrow = b
	}
	return out
}

// Bytes is the digest's 32 bytes, as a row stores them.
func (d Digest) Bytes() []byte { return append([]byte(nil), d[:]...) }

// String is the digest in lowercase hex, as an operator reads it.
func (d Digest) String() string { return hex.EncodeToString(d[:]) }

// IsZero reports the digest of an empty store.
func (d Digest) IsZero() bool { return d == Digest{} }

// DigestFrom reads a digest a row stored. An empty column is the empty store's
// digest; any other length is a row this encoder did not write.
func DigestFrom(b []byte) (Digest, error) {
	var d Digest
	switch len(b) {
	case 0:
		return d, nil
	case DigestSize:
		copy(d[:], b)
		return d, nil
	}
	return d, fmt.Errorf("canon: a stored digest is %d bytes and a digest is %d",
		len(b), DigestSize)
}

// DocumentTerm is one live document's term in its store's digest: the
// SHA-256 of the canonical line naming its collection, its id, its canonical
// body and its version.
//
// THE LINE IS AN OBJECT THIS PACKAGE ENCODES, never a concatenation, so no
// collection name, id or body can be chosen to collide with another split of
// the same bytes.
func DocumentTerm(collection, id string, body []byte, version int64) Term {
	line := []byte(`{"body":`)
	line = append(line, body...)
	line = append(line, `,"c":`...)
	line = AppendString(line, collection)
	line = append(line, `,"id":`...)
	line = AppendString(line, id)
	line = append(line, `,"v":`...)
	line = strconv.AppendInt(line, version, 10)
	line = append(line, '}')
	return Term(sha256.Sum256(line))
}

// CollectionTerm is one collection's term in its store's digest: the SHA-256
// of the canonical line naming it and its canonical specification, so a
// schema change moves the digest exactly as a document change does.
func CollectionTerm(name string, spec []byte) Term {
	line := []byte(`{"collection":`)
	line = AppendString(line, name)
	line = append(line, `,"spec":`...)
	line = append(line, spec...)
	line = append(line, '}')
	return Term(sha256.Sum256(line))
}

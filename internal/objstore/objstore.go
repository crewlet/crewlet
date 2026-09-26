// Package objstore is the fleet's object store: bytes too large and too many
// to hold on every node, placed across the data nodes and addressed by their
// content.
//
// # What is divided, and what is replicated
//
// ADR-0019. Everything the company has to agree on — that a file exists, what
// it is called, which project it is in, which chunks it is made of — stays in
// the replicated estate, held whole on every data node and derived from a
// state log like any other row. What is DIVIDED is the bytes: each chunk is
// held by the members of its placement group ([placement]), and nobody else.
// A company whose attachments outgrow one disk grows by adding data nodes,
// where holding them on every node would have grown every disk.
//
// # Content-addressed and immutable
//
// A chunk's name is the SHA-256 of its bytes, so a chunk is never rewritten —
// a changed file is new chunks and a new manifest — and a copy is verified by
// the name it is stored under. That is what lets every node decide on its own
// what to hold, fetch and delete: two copies of one name are the same bytes,
// and there is no version to agree on.
//
// # The inventory is the estate
//
// Which chunks SHOULD exist is not a list this package keeps. Every data node
// holds the whole replicated estate, and every row that refers to an object
// names its chunks — so every data node can compute, from its own rows, every
// chunk the company references and which of them its placement says it
// should hold. Repair fetches what is missing; garbage collection deletes
// what nothing references and nobody has just written.
package objstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// ChunkSize is the most bytes one chunk holds.
//
// ONE MEBIBYTE: well under the eight a broker message carries, with room for
// the envelope; small enough that a node streaming a file holds one chunk in
// memory at a time, and large enough that a hundred-megabyte file is a
// hundred requests rather than thousands. It is part of a chunk's identity
// only through the bytes it cuts — two builds cutting at different sizes store
// different chunks for one file, both correct — so it may change, and nothing
// written before has to.
const ChunkSize = 1 << 20

// Hash is a content address: the lowercase hex SHA-256 of some bytes.
type Hash string

// ErrBadHash is a string that is not a content address.
var ErrBadHash = errors.New("objstore: not a content hash")

// HashOf is the content address of b.
func HashOf(b []byte) Hash {
	sum := sha256.Sum256(b)
	return Hash(hex.EncodeToString(sum[:]))
}

// ParseHash accepts a content address, refusing anything else by name.
func ParseHash(s string) (Hash, error) {
	h := Hash(s)
	if !h.Valid() {
		return "", fmt.Errorf("%w: %q", ErrBadHash, s)
	}
	return h, nil
}

// Valid reports whether h is sixty-four lowercase hex digits.
func (h Hash) Valid() bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	for _, c := range []byte(h) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// PG is the placement group h falls into. An invalid hash is group 0, which
// nothing reaches: every entry point refuses one first.
func (h Hash) PG() int {
	raw, err := hex.DecodeString(string(h))
	if err != nil {
		return 0
	}
	return placement.PG(raw)
}

// Chunk is one piece of an object.
type Chunk struct {
	Hash Hash  `json:"hash"`
	Size int64 `json:"size"`
}

// Manifest is one object: its whole-content address, its size, and the chunks
// it is made of in order.
//
// It is what a consumer stores in its row — the chunk list IS the reference
// the collector and the repair read — and what a reader needs to put the bytes
// back together.
type Manifest struct {
	Hash   Hash    `json:"hash"`
	Size   int64   `json:"size"`
	Chunks []Chunk `json:"chunks"`
}

// Validate refuses a manifest whose parts do not add up.
func (m Manifest) Validate() error {
	if !m.Hash.Valid() {
		return fmt.Errorf("%w: the object's own hash %q", ErrBadHash, m.Hash)
	}
	var total int64
	for i, c := range m.Chunks {
		if !c.Hash.Valid() {
			return fmt.Errorf("%w: chunk %d is %q", ErrBadHash, i, c.Hash)
		}
		if c.Size <= 0 || c.Size > ChunkSize {
			return fmt.Errorf("objstore: chunk %d is %d bytes, want 1..%d", i, c.Size, ChunkSize)
		}
		total += c.Size
	}
	if total != m.Size {
		return fmt.Errorf("objstore: the chunks add up to %d bytes and the object says %d",
			total, m.Size)
	}
	return nil
}

// ErrTooLarge is an object past the limit its caller set.
var ErrTooLarge = errors.New("objstore: the object is over its size limit")

// Split cuts r into chunks, handing each to put as it is cut, and answers the
// manifest. At most limit bytes are read; one more is [ErrTooLarge], so a
// caller streaming an upload refuses it without having buffered it.
//
// ONE CHUNK IN MEMORY AT A TIME, which is the whole reason this streams: a
// node that buffered a file to hash it first would need the file's size in
// memory, and a small stateless node would be the one to fail.
func Split(ctx context.Context, r io.Reader, limit int64,
	put func(ctx context.Context, c Chunk, data []byte) error) (Manifest, error) {

	whole := sha256.New()
	var m Manifest
	buf := make([]byte, ChunkSize)
	for {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if m.Size+int64(n) > limit {
				return Manifest{}, fmt.Errorf("%w (%d bytes)", ErrTooLarge, limit)
			}
			data := buf[:n]
			_, _ = whole.Write(data)
			c := Chunk{Hash: HashOf(data), Size: int64(n)}
			if perr := put(ctx, c, data); perr != nil {
				return Manifest{}, perr
			}
			m.Chunks = append(m.Chunks, c)
			m.Size += int64(n)
		}
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			m.Hash = Hash(hex.EncodeToString(whole.Sum(nil)))
			return m, nil
		case err != nil:
			return Manifest{}, fmt.Errorf("objstore: read the object: %w", err)
		}
	}
}

// ReferenceTable is a replicated table whose rows keep chunks alive: every
// value in its Column is a chunk the company refers to.
//
// DECLARED BY THE CONSUMER that owns the table, and collected in one list
// (internal/objstore/references) that everything asking which chunks exist
// reads — the collector, the repair and the backup, each building its query
// from the declaration rather than from a statement of its own. A table that
// names chunks and is missing from that list is the one mistake here that
// destroys data: its chunks read as unreferenced and are collected a day
// after they were written. So the list is held against the schema by a test
// rather than by a reader's memory.
type ReferenceTable struct {
	// Domain is the state log whose applier writes the table. A pass must
	// be current on THAT log before it may call a chunk unnamed, so the
	// declaration says which one — and a declared table whose domain this
	// node reads no estate of is refused rather than read as empty.
	Domain string

	Table  string
	Column string

	// Group is the column holding each chunk's placement group, so a pass
	// over one group reads one range of an index rather than the table.
	Group string
}

// identifier is what a declared table or column may be spelled as, since each
// is written into a statement.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Validate refuses a declaration that cannot be read safely: no domain, or a
// table or column spelled as anything but a plain identifier.
func (t ReferenceTable) Validate() error {
	if strings.TrimSpace(t.Domain) == "" {
		return fmt.Errorf("objstore: %s names chunks and no state log that writes it", t.Table)
	}
	for _, name := range []string{t.Table, t.Column, t.Group} {
		if !identifier.MatchString(name) {
			return fmt.Errorf("objstore: %q (declared by %s) is not an identifier "+
				"this package will write into a statement", name, t.Table)
		}
	}
	return nil
}

// Chunks is the statement answering every chunk the table names.
func (t ReferenceTable) Chunks() (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	return `SELECT DISTINCT ` + t.Column + ` FROM ` + t.Table, nil
}

// ChunksIn is the statement answering every chunk the table names in one
// placement group, which is its one argument.
func (t ReferenceTable) ChunksIn() (string, error) {
	all, err := t.Chunks()
	if err != nil {
		return "", err
	}
	return all + ` WHERE ` + t.Group + ` = ?`, nil
}

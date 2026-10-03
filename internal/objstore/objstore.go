// Package objstore is the fleet's object store: bytes too large to carry in a
// replicated row, kept in ONE store every node reaches and addressed by their
// content.
//
// # What is replicated, and where the bytes go
//
// ADR-0026. Everything the company has to agree on — that a file exists, what
// it is called, which project it is in, which chunks it is made of — stays in
// the replicated estate, held whole on every data node and derived from a
// state log like any other row. The BYTES go to a [Backend]: the fleet's own
// NATS JetStream object store by default (natsobj), replicated across the
// broker's members exactly as every stream is, or an S3-compatible bucket
// (s3obj) where the deployment has one. Either is one store the whole fleet
// shares, so no node decides on its own what to hold, fetch or repair — the
// backend does the copies.
//
// # Content-addressed and immutable
//
// A chunk's name is the SHA-256 of its bytes, so a chunk is never rewritten —
// a changed file is new chunks and a new manifest — and every read is checked
// against the name it was stored under ([Store]). Two files sharing a chunk
// share one copy of it.
//
// # The inventory is the estate
//
// Which chunks SHOULD exist is not a list this package keeps: every row that
// refers to an object names its chunks, and the declared tables
// (internal/objstore/references) are the whole inventory. The collector
// (internal/objstore/collect) deletes what nothing references once it is past
// a grace, and the [Locks] a writer and the collector share are what stop a
// chunk being deleted while a new file is re-using it.
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

// Chunk is one piece of an object.
type Chunk struct {
	Hash Hash  `json:"hash"`
	Size int64 `json:"size"`
}

// Manifest is one object: its whole-content address, its size, and the chunks
// it is made of in order.
//
// It is what a consumer stores in its row — the chunk list IS the reference
// the collector reads — and what a reader needs to put the bytes back
// together.
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
// reads — the collector and the backup, each building its query from the
// declaration rather than from a statement of its own. A table that names
// chunks and is missing from that list is the one mistake here that destroys
// data: its chunks read as unreferenced and are collected a day after they
// were written. So the list is held against the schema by a test rather than
// by a reader's memory.
type ReferenceTable struct {
	// Domain is the state log whose applier writes the table. A pass must
	// be current on THAT log before it may call a chunk unnamed, so the
	// declaration says which one — and a declared table whose domain this
	// node reads no estate of is refused rather than read as empty.
	Domain string

	Table  string
	Column string
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
	for _, name := range []string{t.Table, t.Column} {
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

// ChunksAmong is the statement answering which of n given chunks the table
// names, its n arguments the chunks asked about. The collector asks it of a
// BATCH of the chunks a backend listed, so a pass holds one batch in memory
// rather than the company's whole inventory, and each question is n seeks of
// the column's index rather than a scan.
func (t ReferenceTable) ChunksAmong(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("objstore: a question about %d chunks", n)
	}
	all, err := t.Chunks()
	if err != nil {
		return "", err
	}
	return all + ` WHERE ` + t.Column + ` IN (?` + strings.Repeat(`, ?`, n-1) + `)`, nil
}

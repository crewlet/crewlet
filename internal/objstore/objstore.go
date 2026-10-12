// Package objstore is the fleet's object store: bytes too large to carry in a
// replicated row, kept in ONE store every node reaches, one object per upload.
//
// # What is replicated, and where the bytes go
//
// ADR-0026. Everything the company has to agree on — that a file exists, what
// it is called, which project it is in, which object holds its bytes and what
// those bytes hash to — stays in the replicated estate, held whole on every
// data node and derived from a state log like any other row. The BYTES go to
// a [Backend]: the fleet's own NATS JetStream object store by default
// (natsobj), replicated across the broker's members exactly as every stream
// is, or an S3-compatible bucket (s3obj) where the deployment has one. Either
// is one store the whole fleet shares, so no node decides on its own what to
// hold, fetch or repair — the backend does the copies.
//
// # One object per upload, under a key minted for it
//
// An upload is streamed into ONE object under a [Key] minted for that upload
// alone — a UUIDv7, so it carries the instant it was minted and is never
// minted twice — and the row that records the file names the [Object]: the
// key, the SHA-256 of the whole content and its size. A key is never derived
// from the content: content addressing was what made two files share a
// stored copy, and a shared copy is one a writer can be re-using at the moment
// the collector deletes it. A key named by one write and no other has no such
// moment, which is what lets a deletion take no lock (ADR-0027) — and it is
// why the rule is written as "a key is named only by the write that uploaded
// it": a gesture that named an existing key from a second write would bring
// the race back.
//
// What it costs is that nothing is shared: a file written twice with the same
// bytes is two objects, and a moved file is uploaded again. The seat's own
// write skips content the file already holds, and the collector deletes the
// object a write replaced at its next pass once that object is past the
// grace — which is measured from the object's upload, never from its
// replacement, so a replaced object older than a day goes within the hour.
//
// # Every read is checked against the row
//
// [Store.Open] hashes and counts every byte it hands out against the row's
// [Object] and HOLDS BACK THE BYTE THAT COMPLETES THE FILE until the whole of
// it is verified, so no reader — a download, a backup — is ever handed the
// end of a file whose content is not the one recorded: a download of a
// damaged object ends short of its Content-Length rather than whole.
//
// # The inventory is the estate
//
// Which objects SHOULD exist is not a list this package keeps: every row that
// refers to an object names it, and the declared tables
// (internal/objstore/references) are the whole inventory. The collector
// (internal/objstore/collect) deletes what nothing names once it is past a
// grace measured both from when the backend stored it and from when its key
// was minted — and [RecordWithin] is the bound on the write that names a key
// that makes the second of those safe. It also abandons, past the same grace,
// the uploads a backend began and never finished ([Backend.Pending]), which no
// listing of the objects shows.
package objstore

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Hash is a SHA-256 digest of some bytes, as lowercase hex: the whole content
// of a file, as its row records it and every read is checked against.
//
// NOT A NAME. An object is stored under its [Key]; a digest says what the
// bytes are, never where they are.
type Hash string

// ErrBadHash is a string that is not a SHA-256 digest.
var ErrBadHash = errors.New("objstore: not a SHA-256 digest")

// HashOf is the digest of b.
func HashOf(b []byte) Hash {
	sum := sha256.Sum256(b)
	return Hash(hex.EncodeToString(sum[:]))
}

// ParseHash accepts a digest, refusing anything else by name.
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

// Key names one object: a UUIDv7 minted for the upload that stored it.
//
// # Why a UUIDv7
//
// RANDOM, so no two uploads are ever stored under one name — which is the
// whole of what lets the collector delete without a lock. TIME-ORDERED and
// carrying its own minting instant ([Key.Minted]), so the write that names a
// key can be refused once the key is too old to be named safely
// ([RecordWithin]) with no field beside it, and a keyset page of a table's
// keys walks them in roughly the order they were written.
//
// ONE SPELLING: [ParseKey] accepts the canonical lowercase form of a version
// 7, RFC 4122 UUID and nothing else — an upper-case spelling, a `urn:uuid:`
// or braced one, thirty-two bare hex digits, another version. A second
// spelling of one key would be a second name in the store, and a name that is
// not the canonical spelling of a key is not one the collector may judge.
type Key uuid.UUID

// ErrBadKey is a string that is not an object key.
var ErrBadKey = errors.New("objstore: not an object key")

// KeyAt mints a key at t: its first forty-eight bits are t in milliseconds,
// and the rest random but for the version and variant bits.
//
// THE INSTANT IS AN ARGUMENT because the store mints on its own clock
// ([NewStoreAt]) — the key's minting instant is what a write and the
// collector judge its age by, so a test of either has to be able to mint one
// at the instant its clock says. The library's own v7 constructor reads the
// wall clock and nothing else.
func KeyAt(t time.Time) Key {
	var k Key
	ms := t.UnixMilli()
	if ms < 0 {
		ms = 0
	}
	var stamp [8]byte
	binary.BigEndian.PutUint64(stamp[:], uint64(ms))
	copy(k[:6], stamp[2:])
	// crypto/rand.Read never fails: it crashes the program rather than
	// answer fewer random bytes than asked.
	_, _ = rand.Read(k[6:])
	k[6] = k[6]&0x0f | 0x70 // version 7
	k[8] = k[8]&0x3f | 0x80 // the RFC 4122 variant
	return k
}

// ParseKey accepts the canonical spelling of an object key, refusing every
// other by name.
func ParseKey(s string) (Key, error) {
	u, err := uuid.Parse(s)
	switch {
	case err != nil:
		return Key{}, fmt.Errorf("%w: %q: %w", ErrBadKey, s, err)
	case u.String() != s:
		return Key{}, fmt.Errorf("%w: %q is not the canonical lowercase spelling of one", ErrBadKey, s)
	case u.Version() != 7 || u.Variant() != uuid.RFC4122:
		return Key{}, fmt.Errorf("%w: %q is not a version 7 UUID", ErrBadKey, s)
	}
	return Key(u), nil
}

// String is the key's canonical spelling.
func (k Key) String() string { return uuid.UUID(k).String() }

// IsZero reports whether k is no key — what a row that names no object
// carries.
func (k Key) IsZero() bool { return k == Key{} }

// Minted is the instant the key was minted at, to the millisecond, in UTC.
func (k Key) Minted() time.Time {
	var stamp [8]byte
	copy(stamp[2:], k[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(stamp[:]))).UTC()
}

// MarshalText is the key's canonical spelling; the zero key has none.
func (k Key) MarshalText() ([]byte, error) {
	if k.IsZero() {
		return nil, fmt.Errorf("%w: the zero key names no object", ErrBadKey)
	}
	return []byte(k.String()), nil
}

// UnmarshalText reads a key strictly ([ParseKey]).
func (k *Key) UnmarshalText(text []byte) error {
	parsed, err := ParseKey(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// namespace is what every object's name begins with in a backend: the engine's
// own corner of the store.
//
// A NAMESPACE RATHER THAN THE BARE KEY, so the collector claims only names the
// engine wrote: an S3 bucket shared with another application under an empty
// prefix may hold objects named by UUIDs of its own, and a bare key would make
// every one of them look like the engine's. The backup's copy of an S3 store
// keeps the same layout, so it restores with one sync.
const namespace = "files/"

// Name is the name k's object is stored under in a backend.
func (k Key) Name() string { return namespace + k.String() }

// KeyOfName reads a backend name back as a key, refusing a name that is not
// one the store would have written — which is what the collector judges by.
func KeyOfName(name string) (Key, error) {
	rest, ok := strings.CutPrefix(name, namespace)
	if !ok {
		return Key{}, fmt.Errorf("%w: %q is not under %q", ErrBadKey, name, namespace)
	}
	return ParseKey(rest)
}

// Object is one stored object as a row names it: its key, and the digest and
// size of the bytes the upload streamed into it.
//
// THE ROW CARRIES THE DIGEST, never the backend: a bucket cannot hold a
// whole-object SHA-256 for an upload sent in parts, and a reader trusting a
// backend's own account of its bytes would be trusting the thing it is
// checking. [Store.Open] checks every byte against this.
//
// THE KEY IS OMITTED WHEN ZERO rather than refused at encoding: a zero Object
// is a value an operation's body may carry and must cross a wire, and what
// refuses it is [Object.Validate] wherever an object is named — a key that is
// present, though, is only ever its canonical spelling ([Key.MarshalText]).
type Object struct {
	Key  Key   `json:"key,omitzero"`
	Hash Hash  `json:"hash"`
	Size int64 `json:"size"`
}

// Validate refuses an object a row could not name: no key, a digest that is
// not one, or a negative size.
func (o Object) Validate() error {
	switch {
	case o.Key.IsZero():
		return fmt.Errorf("%w: an object names no key", ErrBadKey)
	case !o.Hash.Valid():
		return fmt.Errorf("%w: the object's digest %q", ErrBadHash, o.Hash)
	case o.Size < 0:
		return fmt.Errorf("objstore: an object of %d bytes", o.Size)
	}
	return nil
}

// RecordWithin is how long after its key was minted a write may still name an
// object: the write is refused once the key is older than this by the clock
// of the node deciding it (tracker's PutFile).
//
// # Why it exists
//
// The collector deletes an object no row names once it is past a grace
// (collect.PendingGrace), measured both from the instant the backend stored
// it and from the instant its key was minted. Unbounded, a write naming an
// old key could land AFTER a collection pinned the estate and read it as
// unnamed — and the row would name bytes the collector had just deleted. With
// this bound, a key the collector judges is one no write can name any more:
// the grace exceeds this by more than any two nodes' clocks disagree.
//
// TWELVE HOURS: half the collector's day, and comfortably more than the
// slowest upload the engine accepts — a file of tracker.MaxFileBytes at the
// [MiBPace] floor is eight and a half hours from the key's minting to the
// last byte stored — plus the write that follows it. The two inequalities are
// pinned by one test where every constant they name can be imported
// (internal/objstore/references).
const RecordWithin = 12 * time.Hour

// identifier is what a declared table or column may be spelled as, since each
// is written into a statement.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ReferenceTable is a replicated table whose rows keep objects alive: every
// non-NULL value in its Key column names an object the company refers to,
// and the row beside it says what that object's bytes must be.
//
// DECLARED BY THE CONSUMER that owns the table, and collected in one list
// (internal/objstore/references) that everything asking which objects exist
// reads — the collector, its audit and the backup, each building its query
// from the declaration rather than from a statement of its own. A table that
// names objects and is missing from that list is the one mistake here that
// destroys data: its objects read as unreferenced and are collected a day
// after they were written. So the list is held against the schema by a test
// rather than by a reader's memory.
type ReferenceTable struct {
	// Domain is the state log whose applier writes the table. A pass must
	// be current on THAT log before it may call an object unnamed, so the
	// declaration says which one — and a declared table whose domain this
	// node reads no estate of is refused rather than read as empty.
	Domain string

	Table string

	// Key is the column naming the object — NULL on a row that names
	// none, which every statement here leaves out; Hash and Size are the
	// columns holding what its bytes must hash to and how many there are,
	// which the backup verifies its copy against.
	Key  string
	Hash string
	Size string

	// Owner is the columns that say which row names an object, in the
	// order a person reads them — a project and a path — so a lost object
	// is reported as the file it belonged to rather than as a key nobody
	// can look up.
	Owner []string
}

// Validate refuses a declaration that cannot be read safely: no domain, no
// owner, or a table or column spelled as anything but a plain identifier.
func (t ReferenceTable) Validate() error {
	if strings.TrimSpace(t.Domain) == "" {
		return fmt.Errorf("objstore: %s names objects and no state log that writes it", t.Table)
	}
	if len(t.Owner) == 0 {
		return fmt.Errorf("objstore: %s names objects and no column saying whose", t.Table)
	}
	for _, name := range append([]string{t.Table, t.Key, t.Hash, t.Size}, t.Owner...) {
		if !identifier.MatchString(name) {
			return fmt.Errorf("objstore: %q (declared by %s) is not an identifier "+
				"this package will write into a statement", name, t.Table)
		}
	}
	return nil
}

// ObjectsAmong is the statement answering which of n given keys the table
// names, its n arguments the keys asked about. The collector asks it of a
// BATCH of the keys a backend listed, so a pass holds one batch in memory
// rather than the company's whole inventory, and each question is n seeks of
// the key column's index rather than a scan. A NULL key matches no IN list,
// so a row naming no object is never an answer.
func (t ReferenceTable) ObjectsAmong(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("objstore: a question about %d objects", n)
	}
	if err := t.Validate(); err != nil {
		return "", err
	}
	return `SELECT DISTINCT ` + t.Key + ` FROM ` + t.Table + ` WHERE ` + t.Key +
		` IN (?` + strings.Repeat(`, ?`, n-1) + `)`, nil
}

// ReferencesAfter is the statement answering the next page of at most n rows
// naming an object, in key order after its one argument — the last key of
// the page before, or "" for the first. Each row is the key, the digest, the
// size and then the owner columns.
//
// A KEYSET PAGE ON THE KEY COLUMN'S INDEX, so a walk of every reference — the
// audit's and the backup's — holds a page in memory and seeks to each next
// one, and NULL is excluded by name: a row naming no object (a removed file)
// is no reference.
func (t ReferenceTable) ReferencesAfter(n int) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("objstore: a page of %d references", n)
	}
	if err := t.Validate(); err != nil {
		return "", err
	}
	return `SELECT ` + t.Key + `, ` + t.Hash + `, ` + t.Size + `, ` +
		strings.Join(t.Owner, `, `) + ` FROM ` + t.Table +
		` WHERE ` + t.Key + ` IS NOT NULL AND ` + t.Key + ` > ?` +
		` ORDER BY ` + t.Key + ` LIMIT ` + fmt.Sprint(n), nil
}

// Rows is the part of a statement's result the decoders below read — what
// *sql.Rows offers, so a decoder is exercised without a database.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// ScanObjectsAmong hands visit every key rows answers to [ReferenceTable.ObjectsAmong]'s
// statement over t. The caller closes rows.
//
// THE DECODER LIVES BESIDE THE STATEMENT because the columns are this
// package's: the collector and the backup each read these rows, and a
// private decoder in each was two copies of one column layout that nothing
// held to the statement or to each other — a column added here would be
// fixed in one and mis-scanned by the other.
func (t ReferenceTable) ScanObjectsAmong(rows Rows, visit func(Key) error) error {
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		k, err := ParseKey(raw)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Table, t.Key, err)
		}
		if err := visit(k); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ScanReferences hands visit every row rows answers to [ReferenceTable.ReferencesAfter]'s
// statement over t: the [Object] the row names, and namedBy — the row's owner
// columns joined by "/", the file a person would look the object up as. The
// caller closes rows; see [ReferenceTable.ScanObjectsAmong] for why the decoder is here.
func (t ReferenceTable) ScanReferences(rows Rows, visit func(obj Object, namedBy string) error) error {
	var (
		key, hash string
		size      int64
		owner     = make([]sql.NullString, len(t.Owner))
	)
	dest := []any{&key, &hash, &size}
	for i := range owner {
		dest = append(dest, &owner[i])
	}
	parts := make([]string, len(owner))
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		k, err := ParseKey(key)
		if err != nil {
			return fmt.Errorf("%s.%s: %w", t.Table, t.Key, err)
		}
		h, err := ParseHash(hash)
		if err != nil {
			return fmt.Errorf("%s.%s of %s: %w", t.Table, t.Hash, k, err)
		}
		for i, o := range owner {
			parts[i] = o.String
		}
		if err := visit(Object{Key: k, Hash: h, Size: size}, strings.Join(parts, "/")); err != nil {
			return err
		}
	}
	return rows.Err()
}

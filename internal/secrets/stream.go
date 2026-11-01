package secrets

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// THE SEALED STREAM: a value too large to hold whole, sealed and opened while
// it streams, under the same keyring every envelope above is sealed under.
//
// # Why a second format beside the envelope
//
// The envelope ([Cipher.Encrypt]) seals a whole value in one AES-GCM call and
// base64s it into a string, which is right for a field: a name, a token, a
// document. It is wrong for a value of tens of mebibytes. Sealing one holds the
// plaintext, its string copy, the GCM output and two base64 strings at once,
// about five times the value, and opening one holds as much again; and a value
// that only exists to be streamed between a box and an object store (a coding
// CLI's captured session, internal/executordomain) would be read whole into an
// engine process whose memory is sized for the CLIs it runs. So this format is
// ONLINE: a writer seals while it streams and a reader opens while it streams,
// each holding ONE CHUNK at a time.
//
// # The construction, and what each part is for
//
// It is the STREAM construction (Hoang, Reyhanitabar, Rogaway and Vizár,
// "Online authenticated-encryption and its nonce-reuse misuse-resistance",
// CRYPTO 2015), the one age and Tink's streaming AEAD are built on:
//
//	header   magic "CRWS" | version 1 | key id length (1 byte) | key id | 32-byte salt
//	chunk i  AES-256-GCM(key, nonce_i, plaintext_i, aad_i), the last one flagged final
//
//   - A PER-OBJECT KEY, HKDF-SHA256 over the keyring key the header names, the
//     random salt, and an info string naming this format, its version and the
//     key id. Every object is sealed under a key no other object shares, so a
//     COUNTER is a safe nonce (it never repeats under one key) and nothing
//     random is drawn per chunk. A header byte changed in transit (the key id,
//     the salt, the version) derives another key, and the first chunk fails.
//   - THE NONCE IS THE CHUNK'S INDEX, so a chunk moved to another place opens
//     under the wrong nonce and fails: REORDERING IS DETECTED.
//   - THE ASSOCIATED DATA IS THE CALLER'S, THEN THE INDEX, THEN THE FINAL FLAG
//     ([streamAAD]). The caller's binds the stream to where it belongs (an
//     object moved under another record does not open there); the index binds
//     each chunk to its place a second time; and the FINAL FLAG is what makes
//     the end authenticated: a stream cut at a chunk boundary ends on a chunk
//     sealed as not-final, which the reader can only open as final and so
//     cannot open at all — TRUNCATION IS DETECTED — and a chunk appended after
//     the final one makes the final one read as not-final, which fails the
//     same way — EXTENSION IS DETECTED. The caller's associated data is
//     prefixed by its length, so no two (caller, index) pairs spell one byte
//     string.
//
// # Every failure is ErrDecrypt
//
// A wrong key, an unknown key id, a tampered byte, a reordered, truncated or
// extended stream and a mismatched associated data are all [ErrDecrypt], for
// that error's own reason: a caller that could tell them apart holds an oracle.
// What is NOT ErrDecrypt is the source failing to read — a network that went
// away mid-object — which is an I/O error the caller may retry and is returned
// as it came.

// StreamChunkBytes is how much plaintext one chunk of a sealed stream carries.
//
// SIXTY-FOUR KIBIBYTES, age's figure, chosen from the two things a chunk costs:
//   - MEMORY, which bounds it from above. A reader and a writer each hold one
//     chunk and its tag, so a node streaming a hundred sessions at once (a
//     fleet of seats collecting together) holds about 6.4 MiB of buffers
//     between them, where a mebibyte chunk would hold a hundred.
//   - OVERHEAD, which bounds it from below. Each chunk adds a 16-byte tag and
//     one AEAD call, so at 64 KiB the tags are 0.024% of the stream and the
//     call is amortised over enough bytes that sealing runs at AES-GCM's
//     hardware rate; at 4 KiB the tags would be 0.4% and the calls sixteen
//     times as many.
//
// PART OF THE FORMAT, not a tuning knob: a reader of version 1 reads chunks of
// exactly this size, so changing it is a new [streamVersion].
const StreamChunkBytes = 64 << 10

// MaxStreamKeyIDBytes is the longest keyring key id a sealed stream's header
// can name: its length is one byte.
const MaxStreamKeyIDBytes = 255

const (
	// streamMagic opens every sealed stream, so a reader handed anything else
	// (an envelope, a plaintext archive, an empty object) refuses it at the
	// first four bytes rather than deriving a key for it.
	streamMagic = "CRWS"

	// streamVersion is the format: the header layout, the chunk size, the
	// key derivation and the associated data. A reader of one version opens
	// that version alone.
	streamVersion byte = 1

	// streamSaltBytes is the random salt each stream's key is derived with:
	// 32 bytes, so two streams sealed under one keyring key derive one key
	// with probability 2^-256, which is what lets the nonce be a counter.
	streamSaltBytes = 32

	// streamTagBytes is AES-GCM's tag.
	streamTagBytes = 16

	// streamInfo names this format in the key derivation, so a key derived
	// for it is never one any other purpose derives from the same keyring
	// key ([Keyring.Derive]'s info strings are another space).
	streamInfo = "crewlet/sealed-stream/v1/"
)

// StreamSealer seals and opens sealed streams under a keyring: the active key
// seals, and any key the keyring still holds opens what it sealed, so a
// rotation reads every stream sealed under a key it has not yet retired.
type StreamSealer struct {
	ring Keyring

	// chunk is [StreamChunkBytes] in production. A field so this package's
	// own tests can walk every chunk boundary of a stream a few bytes long,
	// where the shipped size would make them walk megabytes; nothing outside
	// the package can set it, because a reader and a writer that disagreed
	// on it would never open a stream.
	chunk int
}

// NewStreamSealer builds a sealer over a keyring, refusing one that cannot
// seal ([Keyring.Validate]) and one whose key ids do not fit a header
// ([MaxStreamKeyIDBytes]): a key that sealed a stream it could never be named
// in would be a stream nobody opens.
func NewStreamSealer(ring Keyring) (*StreamSealer, error) {
	if err := ring.Validate(); err != nil {
		return nil, err
	}
	keys := make(map[string][]byte, len(ring.Keys))
	for id, key := range ring.Keys {
		if len(id) > MaxStreamKeyIDBytes {
			return nil, fmt.Errorf("%w: key id %q is %d bytes and a sealed stream "+
				"names its key in at most %d", ErrNoKey, id, len(id), MaxStreamKeyIDBytes)
		}
		keys[id] = append([]byte(nil), key...)
	}
	return &StreamSealer{ring: Keyring{ActiveID: ring.ActiveID, Keys: keys},
		chunk: StreamChunkBytes}, nil
}

// ActiveKeyID is the keyring key a stream sealed now is sealed under: what a
// row records beside the object, so a rotation counts what is left under a
// retired key without opening anything.
func (s *StreamSealer) ActiveKeyID() string { return s.ring.ActiveID }

// SealedStreamSize is how many bytes a stream of plain bytes takes once sealed
// under a key named keyID: the header, the plaintext, and a tag per chunk —
// at least one chunk, since even an empty stream ends on a final chunk.
func SealedStreamSize(plain int64, keyID string) int64 {
	return sealedSize(plain, keyID, StreamChunkBytes)
}

// sealedSize is [SealedStreamSize] at a chunk size.
func sealedSize(plain int64, keyID string, chunk int) int64 {
	chunks := max((plain+int64(chunk)-1)/int64(chunk), 1)
	return int64(headerSize(keyID)) + plain + chunks*streamTagBytes
}

// headerSize is the header's length for a key id.
func headerSize(keyID string) int {
	return len(streamMagic) + 1 + 1 + len(keyID) + streamSaltBytes
}

// Seal returns a writer whose every byte is sealed onto dst under the active
// key, bound to aad; Close writes the final chunk and must be called, or the
// stream is one no reader opens. It answers the key id it seals under.
//
// The header is written before Seal returns, so a dst that refuses its first
// write fails here rather than on the caller's first Write.
func (s *StreamSealer) Seal(dst io.Writer, aad []byte) (io.WriteCloser, string, error) {
	id := s.ring.ActiveID
	salt := make([]byte, streamSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, "", fmt.Errorf("secrets: draw a sealed stream's salt: %w", err)
	}
	aead, err := streamAEAD(s.ring.Keys[id], salt, id)
	if err != nil {
		return nil, "", fmt.Errorf("%w: active key unusable", ErrNoKey)
	}
	header := make([]byte, 0, headerSize(id))
	header = append(header, streamMagic...)
	header = append(header, streamVersion, byte(len(id)))
	header = append(header, id...)
	header = append(header, salt...)
	if _, err := dst.Write(header); err != nil {
		return nil, "", err
	}
	return &streamWriter{dst: dst, aead: aead, aad: aad, buf: make([]byte, 0, s.chunk),
		sealed: make([]byte, 0, s.chunk+streamTagBytes)}, id, nil
}

// Open reads a sealed stream's header off src and returns a reader of its
// plaintext, opened chunk by chunk under the key the header names and bound to
// aad. A header this keyring cannot open is [ErrDecrypt] here; anything wrong
// further in is ErrDecrypt from the Read that reaches it, and the reader
// answers it for good.
//
// NO BYTE IS RETURNED BEFORE THE CHUNK HOLDING IT HAS AUTHENTICATED, and none
// at all from a stream whose end does not: a truncated stream reads as every
// chunk up to the cut and then ErrDecrypt, never as a clean EOF, so a reader
// that stops at the first error has trusted nothing it was not given.
func (s *StreamSealer) Open(src io.Reader, aad []byte) (io.Reader, error) {
	in := bufio.NewReaderSize(src, s.chunk+streamTagBytes+1)
	fixed := make([]byte, len(streamMagic)+2)
	if _, err := io.ReadFull(in, fixed); err != nil {
		return nil, ioOrDecrypt(err)
	}
	if string(fixed[:len(streamMagic)]) != streamMagic || fixed[len(streamMagic)] != streamVersion {
		return nil, ErrDecrypt
	}
	rest := make([]byte, int(fixed[len(streamMagic)+1])+streamSaltBytes)
	if _, err := io.ReadFull(in, rest); err != nil {
		return nil, ioOrDecrypt(err)
	}
	id, salt := string(rest[:len(rest)-streamSaltBytes]), rest[len(rest)-streamSaltBytes:]
	key, ok := s.ring.Keys[id]
	if !ok {
		return nil, ErrDecrypt
	}
	aead, err := streamAEAD(key, salt, id)
	if err != nil {
		return nil, ErrDecrypt
	}
	return &streamReader{in: in, aead: aead, aad: aad,
		sealed: make([]byte, s.chunk+streamTagBytes)}, nil
}

// streamAEAD is the per-object AEAD: AES-256-GCM under the key HKDF derives
// from the keyring key, the stream's salt and the format's info, which names
// the key id so a header naming another id derives another key.
func streamAEAD(key, salt []byte, id string) (cipher.AEAD, error) {
	if len(key) != keyBytes {
		return nil, ErrDecrypt
	}
	derived, err := hkdf.Key(sha256.New, key, salt, streamInfo+id, keyBytes)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// streamNonce is chunk index's nonce: four zero bytes and the index, big
// endian. A counter is a safe nonce because the key is the stream's own.
func streamNonce(index uint64) []byte {
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], index)
	return nonce
}

// streamAAD is chunk index's associated data: the caller's, prefixed by its
// length, then the index and the final flag.
func streamAAD(aad []byte, index uint64, final bool) []byte {
	out := make([]byte, 0, 4+len(aad)+8+1)
	out = binary.BigEndian.AppendUint32(out, uint32(len(aad)))
	out = append(out, aad...)
	out = binary.BigEndian.AppendUint64(out, index)
	if final {
		return append(out, 1)
	}
	return append(out, 0)
}

// ioOrDecrypt is what a short read of the stream means: a stream that ends
// before its header or a chunk does is a truncated one, which is ErrDecrypt;
// any other failure is the source's own, returned for the caller to retry.
func ioOrDecrypt(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrDecrypt
	}
	return err
}

// streamWriter is [StreamSealer.Seal]'s writer.
type streamWriter struct {
	dst  io.Writer
	aead cipher.AEAD
	aad  []byte

	// buf is the chunk being filled. A FULL chunk is sealed only once the
	// next byte arrives, because until then it may be the last one, which
	// is sealed as final by Close.
	buf    []byte
	sealed []byte
	index  uint64
	closed bool
	err    error
}

// Write seals p onto the stream, a chunk at a time.
func (w *streamWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("secrets: write to a closed sealed stream")
	}
	written := 0
	for len(p) > 0 {
		if len(w.buf) == cap(w.buf) {
			if err := w.flush(false); err != nil {
				return written, err
			}
		}
		n := copy(w.buf[len(w.buf):cap(w.buf)], p)
		w.buf = w.buf[:len(w.buf)+n]
		p, written = p[n:], written+n
	}
	return written, nil
}

// Close seals what is buffered as the final chunk, empty or full.
func (w *streamWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return nil
	}
	w.closed = true
	return w.flush(true)
}

// flush seals the buffered chunk and writes it.
func (w *streamWriter) flush(final bool) error {
	w.sealed = w.aead.Seal(w.sealed[:0], streamNonce(w.index), w.buf,
		streamAAD(w.aad, w.index, final))
	if _, err := w.dst.Write(w.sealed); err != nil {
		w.err = err
		return err
	}
	w.index++
	w.buf = w.buf[:0]
	return nil
}

// streamReader is [StreamSealer.Open]'s reader.
type streamReader struct {
	in   *bufio.Reader
	aead cipher.AEAD
	aad  []byte

	sealed []byte
	// plain is the opened chunk not yet handed out.
	plain []byte
	index uint64
	done  bool
	err   error
}

// Read hands out the plaintext, opening the next chunk when the last is spent.
func (r *streamReader) Read(p []byte) (int, error) {
	for len(r.plain) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		if r.done {
			return 0, io.EOF
		}
		if err := r.next(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.plain)
	r.plain = r.plain[n:]
	return n, nil
}

// next reads and opens one chunk. A chunk shorter than a whole one is the last
// one by construction; a whole one is the last one exactly when nothing
// follows it, which a one-byte peek answers — and either way the chunk opens
// only under the final flag it was sealed with.
func (r *streamReader) next() error {
	n, err := io.ReadFull(r.in, r.sealed)
	final := false
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		if n < streamTagBytes {
			return ErrDecrypt
		}
		final = true
	case err != nil:
		return err
	default:
		if _, peekErr := r.in.Peek(1); peekErr != nil {
			if !errors.Is(peekErr, io.EOF) {
				return peekErr
			}
			final = true
		}
	}
	// IN PLACE, the one chunk's buffer reused for its own plaintext: the
	// plaintext is handed out whole before [streamReader.next] reads again,
	// so nothing still refers to the bytes the next chunk overwrites.
	plain, openErr := r.aead.Open(r.sealed[:0], streamNonce(r.index),
		r.sealed[:n], streamAAD(r.aad, r.index, final))
	if openErr != nil {
		return ErrDecrypt
	}
	r.index++
	r.plain, r.done = plain, final
	return nil
}

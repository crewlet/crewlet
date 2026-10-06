package objstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// A SPLIT CUTS AT THE CHUNK SIZE, HANDS OVER EVERY PIECE IN ORDER, AND ITS
// MANIFEST ADDS UP — the object's own hash is the hash of the whole, not of
// any piece.
func TestASplitCutsAtTheChunkSizeAndItsManifestAddsUp(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, ChunkSize - 1, ChunkSize, ChunkSize + 1, 3*ChunkSize + 17} {
		body := bytes.Repeat([]byte{0x5a}, size)
		for i := range body {
			body[i] = byte(i * 31)
		}
		var got bytes.Buffer
		var pieces []Chunk
		m, err := Split(context.Background(), bytes.NewReader(body), int64(size),
			func(_ context.Context, c Chunk, data []byte) error {
				if HashOf(data) != c.Hash || int64(len(data)) != c.Size {
					t.Fatalf("size %d: a piece was handed over under the wrong name", size)
				}
				pieces = append(pieces, c)
				got.Write(data)
				return nil
			})
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(got.Bytes(), body) {
			t.Fatalf("size %d: the pieces do not reassemble the object", size)
		}
		if m.Hash != HashOf(body) || m.Size != int64(size) {
			t.Fatalf("size %d: manifest %s/%d, want %s/%d", size, m.Hash, m.Size,
				HashOf(body), size)
		}
		wantChunks := (size + ChunkSize - 1) / ChunkSize
		if len(m.Chunks) != wantChunks || len(pieces) != wantChunks {
			t.Fatalf("size %d: %d chunks, want %d", size, len(m.Chunks), wantChunks)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("size %d: its own manifest does not validate: %v", size, err)
		}
	}
}

// AN OBJECT OVER ITS LIMIT IS REFUSED BY NAME, having read one byte past it
// rather than the whole thing.
func TestAnObjectOverItsLimitIsRefused(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("x"), 100)
	_, err := Split(context.Background(), bytes.NewReader(body), 99,
		func(context.Context, Chunk, []byte) error { return nil })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Split over the limit = %v, want ErrTooLarge", err)
	}
	if _, err := Split(context.Background(), bytes.NewReader(body), 100,
		func(context.Context, Chunk, []byte) error { return nil }); err != nil {
		t.Fatalf("Split at exactly the limit = %v", err)
	}
}

// A PIECE THE CALLER COULD NOT STORE FAILS THE SPLIT, rather than yielding a
// manifest naming bytes that are nowhere.
func TestAPieceThatCouldNotBeStoredFailsTheSplit(t *testing.T) {
	t.Parallel()
	refused := errors.New("no holder answered")
	_, err := Split(context.Background(), strings.NewReader("abc"), 10,
		func(context.Context, Chunk, []byte) error { return refused })
	if !errors.Is(err, refused) {
		t.Fatalf("Split = %v, want the put's own error", err)
	}
}

// endsWith yields its bytes and then answers err in place of the end, as a
// request body whose client went away does.
type endsWith struct {
	r   io.Reader
	err error
}

func (e *endsWith) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, e.err
	}
	return n, err
}

// stuck answers nothing and no error, for ever.
type stuck struct{}

func (stuck) Read([]byte) (int, error) { return 0, nil }

// failingWith answers its bytes together with an error, in one read.
type failingWith struct {
	data []byte
	err  error
}

func (f *failingWith) Read(p []byte) (int, error) {
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, f.err
}

// THE END OF A STREAM IS io.EOF ITSELF, and every other answer is the failure
// it is. io.ReadFull, which the object store read through, answers its own
// short read as io.ErrUnexpectedEOF — the value net/http's request body
// answers for a client that went away mid-body — so a reader that took the
// one for the end stored a cut upload as a whole one.
func TestAFillEndsOnlyAtIoEOFItself(t *testing.T) {
	t.Parallel()
	data := []byte("seven b")
	cases := []struct {
		name    string
		r       io.Reader
		size    int
		wantN   int
		wantEnd bool
		wantErr error
	}{
		{"a full buffer from a trickle", iotest.OneByteReader(bytes.NewReader(data)), 4, 4, false, nil},
		{"a short end", iotest.OneByteReader(bytes.NewReader(data)), 10, 7, true, nil},
		{"an end that fills the buffer exactly", iotest.DataErrReader(bytes.NewReader(data)), 7, 7, true, nil},
		{"an end and nothing before it", bytes.NewReader(nil), 4, 0, true, nil},
		{"a body cut short", &endsWith{bytes.NewReader(data), io.ErrUnexpectedEOF}, 10, 7, false, io.ErrUnexpectedEOF},
		{"a cut body, wrapped", &endsWith{bytes.NewReader(data),
			fmt.Errorf("the body: %w", io.ErrUnexpectedEOF)}, 10, 7, false, io.ErrUnexpectedEOF},
		{"a wrapped end", &endsWith{bytes.NewReader(data), fmt.Errorf("the body: %w", io.EOF)}, 10, 7, false, io.EOF},
		{"a failure arriving with the last bytes", &failingWith{data, io.ErrClosedPipe}, 7, 7, false, io.ErrClosedPipe},
		{"a reader that never moves", stuck{}, 4, 0, false, io.ErrNoProgress},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := make([]byte, c.size)
			n, end, err := Fill(c.r, buf)
			if n != c.wantN || end != c.wantEnd || !errors.Is(err, c.wantErr) || (c.wantErr == nil) != (err == nil) {
				t.Fatalf("Fill = %d, end %v, %v; want %d, end %v, %v", n, end, err, c.wantN, c.wantEnd, c.wantErr)
			}
			if !bytes.Equal(buf[:n], data[:n]) {
				t.Fatalf("Fill read %q, want %q", buf[:n], data[:n])
			}
		})
	}
}

// A SPLIT ENDS ONLY WHERE ITS READER ENDS: a body cut short is a failed split
// with no manifest, never a manifest of the half that arrived — which is what
// an upload whose client went away mid-body was recorded as.
func TestACutBodyFailsTheSplit(t *testing.T) {
	t.Parallel()
	for _, cut := range []error{io.ErrUnexpectedEOF,
		fmt.Errorf("the file's body could not be read: %w", io.ErrUnexpectedEOF),
		fmt.Errorf("the file's body: %w", io.EOF)} {
		for _, size := range []int{10, ChunkSize, ChunkSize + 10} {
			stored := 0
			m, err := Split(context.Background(), &endsWith{bytes.NewReader(make([]byte, size)), cut},
				int64(2*ChunkSize), func(_ context.Context, _ Chunk, data []byte) error {
					stored += len(data)
					return nil
				})
			if !errors.Is(err, cut) {
				t.Fatalf("a body of %d bytes cut with %q split into %+v, %v; want the cut", size, cut, m, err)
			}
			if want := size / ChunkSize * ChunkSize; stored != want {
				t.Errorf("a body of %d bytes cut with %q handed over %d bytes, want the %d "+
					"of its whole chunks", size, cut, stored, want)
			}
		}
	}
}

// A MANIFEST WHOSE PARTS DO NOT ADD UP IS REFUSED — it is what a reader
// reassembles by, and what the collector keeps alive.
func TestAManifestWhosePartsDoNotAddUpIsRefused(t *testing.T) {
	t.Parallel()
	good := HashOf([]byte("a"))
	for name, m := range map[string]Manifest{
		"no own hash":     {Size: 1, Chunks: []Chunk{{Hash: good, Size: 1}}},
		"a bad chunk":     {Hash: good, Size: 1, Chunks: []Chunk{{Hash: "zz", Size: 1}}},
		"an empty chunk":  {Hash: good, Size: 0, Chunks: []Chunk{{Hash: good, Size: 0}}},
		"an oversize one": {Hash: good, Size: ChunkSize + 1, Chunks: []Chunk{{Hash: good, Size: ChunkSize + 1}}},
		"a wrong total":   {Hash: good, Size: 2, Chunks: []Chunk{{Hash: good, Size: 1}}},
	} {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

// ONLY SIXTY-FOUR LOWERCASE HEX DIGITS ARE A HASH: an uppercase spelling of
// the same digest is a second name for one chunk, which would be a second
// file on disk.
func TestOnlyLowercaseHexIsAHash(t *testing.T) {
	t.Parallel()
	good := string(HashOf([]byte("a")))
	if _, err := ParseHash(good); err != nil {
		t.Fatalf("ParseHash(%q) = %v", good, err)
	}
	for _, bad := range []string{"", good[:63], good + "0", strings.ToUpper(good),
		"../" + good[3:], strings.Repeat("g", 64)} {
		if _, err := ParseHash(bad); !errors.Is(err, ErrBadHash) {
			t.Errorf("ParseHash(%q) = %v, want ErrBadHash", bad, err)
		}
	}
}

// A DECLARATION IS WRITTEN INTO A STATEMENT, so it is refused unless every
// name in it is a plain identifier — and unless it names the log that writes
// the table, which is what a pass must be current on before it may call a
// chunk unnamed.
func TestADeclarationIsReadOnlyWhenItIsSafeToWrite(t *testing.T) {
	t.Parallel()
	good := ReferenceTable{Domain: "tracker", Table: "tracker_file_chunks", Column: "chunk"}
	query, err := good.ChunksAmong(3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT DISTINCT chunk FROM tracker_file_chunks WHERE chunk IN (?, ?, ?)"; query != want {
		t.Errorf("statement = %q, want %q", query, want)
	}
	if _, err := good.ChunksAmong(0); err == nil {
		t.Error("a question about no chunks was built")
	}
	for name, bad := range map[string]ReferenceTable{
		"no domain":        {Table: "t", Column: "chunk"},
		"a table injected": {Domain: "tracker", Table: "t; DROP TABLE t", Column: "chunk"},
		"a quoted column":  {Domain: "tracker", Table: "t", Column: `"chunk"`},
		"no column":        {Domain: "tracker", Table: "t"},
		"upper case":       {Domain: "tracker", Table: "T", Column: "chunk"},
	} {
		if _, err := bad.Chunks(); err == nil {
			t.Errorf("%s: a statement was built", name)
		}
	}
}

// A TRANSFER IS GIVEN A PACE FOR EVERY MEBIBYTE IT BEGINS, so a request
// carrying one byte gets as long as one carrying a mebibyte and one byte more
// gets a second pace — never a bound rounded down to less than its body
// needs at the floor.
func TestATransferIsPacedByTheMebibytesItBegins(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]time.Duration{
		0: 0, 1: MiBPace, MiB: MiBPace, MiB + 1: 2 * MiBPace, 8 * MiB: 8 * MiBPace,
	} {
		if got := PaceFor(n); got != want {
			t.Errorf("PaceFor(%d) = %v, want %v", n, got, want)
		}
	}
}

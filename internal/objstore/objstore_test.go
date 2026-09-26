package objstore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
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
	good := ReferenceTable{Domain: "tracker", Table: "tracker_file_chunks", Column: "chunk", Group: "pg"}
	query, err := good.ChunksIn()
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT DISTINCT chunk FROM tracker_file_chunks WHERE pg = ?"; query != want {
		t.Errorf("statement = %q, want %q", query, want)
	}
	for name, bad := range map[string]ReferenceTable{
		"no domain":        {Table: "t", Column: "chunk", Group: "pg"},
		"a table injected": {Domain: "tracker", Table: "t; DROP TABLE t", Column: "chunk", Group: "pg"},
		"a quoted column":  {Domain: "tracker", Table: "t", Column: `"chunk"`, Group: "pg"},
		"no group":         {Domain: "tracker", Table: "t", Column: "chunk"},
		"upper case":       {Domain: "tracker", Table: "T", Column: "chunk", Group: "pg"},
	} {
		if _, err := bad.Chunks(); err == nil {
			t.Errorf("%s: a statement was built", name)
		}
	}
}

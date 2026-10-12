package objstore

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"
)

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

// ONLY SIXTY-FOUR LOWERCASE HEX DIGITS ARE A DIGEST: an uppercase spelling
// would compare unequal to the one a read computes.
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
// the table, which is what a pass must be current on before it may call an
// object unnamed, and the columns saying whose a reference is.
//
// AND NEITHER STATEMENT CAN ANSWER A NULL KEY: a removed file names no
// object, and a walk handing its NULL to the key parser would stop every
// audit and every backup for good.
func TestADeclarationIsReadOnlyWhenItIsSafeToWrite(t *testing.T) {
	t.Parallel()
	good := ReferenceTable{Domain: "tracker", Table: "tracker_files", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"project_key", "path"}}
	among, err := good.ObjectsAmong(3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT DISTINCT object FROM tracker_files WHERE object IN (?, ?, ?)"; among != want {
		t.Errorf("ObjectsAmong = %q, want %q", among, want)
	}
	page, err := good.ReferencesAfter(500)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT object, hash, size, project_key, path FROM tracker_files " +
		"WHERE object IS NOT NULL AND object > ? ORDER BY object LIMIT 500"; page != want {
		t.Errorf("ReferencesAfter = %q, want %q", page, want)
	}
	if _, err := good.ObjectsAmong(0); err == nil {
		t.Error("a question about no objects was built")
	}
	if _, err := good.ReferencesAfter(0); err == nil {
		t.Error("a page of no references was built")
	}
	for name, bad := range map[string]ReferenceTable{
		"no domain":        {Table: "t", Key: "object", Hash: "hash", Size: "size", Owner: []string{"path"}},
		"a table injected": {Domain: "tracker", Table: "t; DROP TABLE t", Key: "object", Hash: "hash", Size: "size", Owner: []string{"path"}},
		"a quoted key":     {Domain: "tracker", Table: "t", Key: `"object"`, Hash: "hash", Size: "size", Owner: []string{"path"}},
		"no key":           {Domain: "tracker", Table: "t", Hash: "hash", Size: "size", Owner: []string{"path"}},
		"no hash":          {Domain: "tracker", Table: "t", Key: "object", Size: "size", Owner: []string{"path"}},
		"no size":          {Domain: "tracker", Table: "t", Key: "object", Hash: "hash", Owner: []string{"path"}},
		"no owner":         {Domain: "tracker", Table: "t", Key: "object", Hash: "hash", Size: "size"},
		"an owner injected": {Domain: "tracker", Table: "t", Key: "object", Hash: "hash", Size: "size",
			Owner: []string{"path, (SELECT 1)"}},
		"upper case": {Domain: "tracker", Table: "T", Key: "object", Hash: "hash", Size: "size", Owner: []string{"path"}},
	} {
		if _, err := bad.ObjectsAmong(1); err == nil {
			t.Errorf("%s: a question was built", name)
		}
		if _, err := bad.ReferencesAfter(1); err == nil {
			t.Errorf("%s: a page was built", name)
		}
	}
}

// ONLY THE CANONICAL SPELLING OF A VERSION 7 UUID IS A KEY. Every other
// spelling the UUID library would take is a second name for one object — a
// second object in the store, or one the collector judges as somebody
// else's — and a digest is not a key at all.
func TestOnlyTheCanonicalSpellingIsAKey(t *testing.T) {
	t.Parallel()
	k := KeyAt(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	good := k.String()
	if got, err := ParseKey(good); err != nil || got != k {
		t.Fatalf("ParseKey(%q) = %v, %v; want the key back", good, got, err)
	}
	v4 := uuid.New().String()
	for name, bad := range map[string]string{
		"empty":         "",
		"upper case":    strings.ToUpper(good),
		"a urn":         "urn:uuid:" + good,
		"braced":        "{" + good + "}",
		"bare hex":      strings.ReplaceAll(good, "-", ""),
		"a version 4":   v4,
		"a digest":      string(HashOf([]byte("a digest"))),
		"with a prefix": "files/" + good,
		"the nil uuid":  uuid.Nil.String(),
	} {
		if _, err := ParseKey(bad); !errors.Is(err, ErrBadKey) {
			t.Errorf("%s: ParseKey(%q) = %v, want ErrBadKey", name, bad, err)
		}
		var into Key
		if err := into.UnmarshalText([]byte(bad)); !errors.Is(err, ErrBadKey) {
			t.Errorf("%s: UnmarshalText(%q) = %v, want ErrBadKey", name, bad, err)
		}
	}
	if _, err := (Key{}).MarshalText(); !errors.Is(err, ErrBadKey) {
		t.Errorf("the zero key marshalled: %v", err)
	}
}

// A KEY CARRIES THE INSTANT IT WAS MINTED, to the millisecond — what a write
// is refused by once the key is too old and what the collector's grace is
// measured from — and two keys minted at one instant are still two keys.
func TestAKeyCarriesItsMintingInstant(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 16, 9, 30, 15, 123_456_789, time.UTC)
	a, b := KeyAt(at), KeyAt(at)
	if want := at.Truncate(time.Millisecond); !a.Minted().Equal(want) || a.Minted().Location() != time.UTC {
		t.Fatalf("a key minted at %v says %v", want, a.Minted())
	}
	if a == b {
		t.Fatal("two keys minted at one instant are the same key")
	}
	if u := uuid.UUID(a); u.Version() != 7 || u.Variant() != uuid.RFC4122 {
		t.Fatalf("a minted key is version %d, variant %v", u.Version(), u.Variant())
	}
	if later := KeyAt(at.Add(time.Millisecond)); later.String() <= a.String() {
		t.Errorf("a key minted later sorts before an earlier one: %s <= %s", later, a)
	}
	got, err := KeyOfName(a.Name())
	if err != nil || got != a {
		t.Fatalf("KeyOfName(%q) = %v, %v", a.Name(), got, err)
	}
	if !strings.HasPrefix(a.Name(), "files/") {
		t.Errorf("a key is stored as %q, outside the engine's namespace", a.Name())
	}
	for _, foreign := range []string{a.String(), "other/" + a.String(), string(HashOf([]byte("x")))} {
		if _, err := KeyOfName(foreign); !errors.Is(err, ErrBadKey) {
			t.Errorf("KeyOfName(%q) = %v, want ErrBadKey", foreign, err)
		}
	}
}

// AN OBJECT A ROW COULD NOT NAME IS REFUSED: no key, a digest that is not
// one, or a negative size.
func TestAnObjectARowCannotNameIsRefused(t *testing.T) {
	t.Parallel()
	good := Object{Key: KeyAt(time.Now()), Hash: HashOf(nil), Size: 0}
	if err := good.Validate(); err != nil {
		t.Fatalf("an empty object was refused: %v", err)
	}
	for name, bad := range map[string]Object{
		"no key":        {Hash: good.Hash},
		"no digest":     {Key: good.Key},
		"a bad digest":  {Key: good.Key, Hash: "zz"},
		"negative size": {Key: good.Key, Hash: good.Hash, Size: -1},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: validated", name)
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

// A READ IS BUDGETED A PACE PER MEBIBYTE ON EACH OF ITS TWO LEGS, beside its
// base — and the stall it is ended at is the time a mebibyte is given, so
// neither restates the floor the upload is held to.
func TestAReadIsBudgetedBothLegsAtTheFloor(t *testing.T) {
	t.Parallel()
	for size, want := range map[int64]time.Duration{
		0: ReadBase, 1: ReadBase + 2*MiBPace, 1 << 30: ReadBase + 2*1024*MiBPace,
	} {
		if got := ReadBudget(size); got != want {
			t.Errorf("ReadBudget(%d) = %v, want %v", size, got, want)
		}
	}
	if ReadStall != 2*MiBPace {
		t.Errorf("ReadStall = %v, want two paces", ReadStall)
	}
}

// fakeRows answers a statement's rows from values, so the decoders are read
// without a database: each row is assigned into its destinations in order,
// as database/sql's own conversion would for these column types.
type fakeRows struct {
	rows [][]any
	at   int
}

func (r *fakeRows) Next() bool { r.at++; return r.at <= len(r.rows) }
func (r *fakeRows) Err() error { return nil }
func (r *fakeRows) Scan(dest ...any) error {
	row := r.rows[r.at-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan of %d columns into %d destinations", len(row), len(dest))
	}
	for i, d := range dest {
		switch d := d.(type) {
		case *string:
			*d = row[i].(string)
		case *int64:
			*d = row[i].(int64)
		case *sql.NullString:
			if row[i] == nil {
				*d = sql.NullString{}
			} else {
				*d = sql.NullString{String: row[i].(string), Valid: true}
			}
		default:
			return fmt.Errorf("column %d: destination %T", i, d)
		}
	}
	return nil
}

// THE DECODERS READ THE COLUMNS THE STATEMENTS NAME, IN THEIR ORDER — key,
// digest, size, then every owner column — and refuse a value that is not a
// key or a digest naming the column it came from, so a corrupt row stops the
// walk rather than being handed on as an object nobody stored. They are the
// ONE decoder of each statement: the collector and the backup both read
// through them.
func TestTheDecodersReadTheStatementsColumns(t *testing.T) {
	t.Parallel()
	table := ReferenceTable{Domain: "tracker", Table: "tracker_files", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"project_key", "path"}}
	k1 := KeyAt(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	k2 := KeyAt(time.Date(2026, 10, 6, 13, 0, 0, 0, time.UTC))
	h := HashOf([]byte("a"))

	type ref struct {
		obj     Object
		namedBy string
	}
	var got []ref
	err := table.ScanReferences(&fakeRows{rows: [][]any{
		{k1.String(), string(h), int64(1), "ENG", "docs/a.md"},
		{k2.String(), string(h), int64(1), nil, "b.md"},
	}}, func(obj Object, namedBy string) error {
		got = append(got, ref{obj, namedBy})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []ref{{Object{Key: k1, Hash: h, Size: 1}, "ENG/docs/a.md"},
		{Object{Key: k2, Hash: h, Size: 1}, "/b.md"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ScanReferences = %+v, want %+v", got, want)
	}

	for name, row := range map[string][]any{
		"tracker_files.object": {"not-a-key", string(h), int64(1), "ENG", "a"},
		"tracker_files.hash":   {k1.String(), "nope", int64(1), "ENG", "a"},
	} {
		err := table.ScanReferences(&fakeRows{rows: [][]any{row}},
			func(Object, string) error { t.Errorf("%s: a bad row was visited", name); return nil })
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("a bad %s: %v, want an error naming the column", name, err)
		}
	}
	stop := errors.New("stop")
	if err := table.ScanReferences(&fakeRows{rows: [][]any{
		{k1.String(), string(h), int64(1), "ENG", "a"},
	}}, func(Object, string) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("a visitor's error came back as %v", err)
	}

	var keys []Key
	if err := table.ScanObjectsAmong(&fakeRows{rows: [][]any{{k1.String()}, {k2.String()}}},
		func(k Key) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != k1 || keys[1] != k2 {
		t.Errorf("ScanObjectsAmong = %v, want [%s %s]", keys, k1, k2)
	}
	if err := table.ScanObjectsAmong(&fakeRows{rows: [][]any{{string(h)}}},
		func(Key) error { t.Error("a digest was visited as a key"); return nil }); err == nil ||
		!strings.Contains(err.Error(), "tracker_files.object") {
		t.Errorf("a digest in the key column: %v, want an error naming the column", err)
	}
}

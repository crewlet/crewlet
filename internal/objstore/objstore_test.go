package objstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
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
		Hash: "hash", Size: "size", Owner: []string{"project_key", "path"}, Standing: Required}
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
		// A STANDING ON EVERY ONE, so each is refused for its own fault
		// rather than for the standing none of them states — without it
		// this loop passes with every other check deleted.
		bad.Standing = Required
		if _, err := bad.ObjectsAmong(1); err == nil {
			t.Errorf("%s: a question was built", name)
		}
		if _, err := bad.ReferencesAfter(1); err == nil {
			t.Errorf("%s: a page was built", name)
		}
	}
}

// A DECLARATION STATES ITS STANDING, AND NONE IS A DEFAULT (ADR-0074): a live
// table read as retired or derived is audited by nobody and accounted for by
// no backup, and a retired or derived one read as live pages somebody about
// bytes nobody needs. So the zero value — a declaration that forgot the field —
// is refused by name, the refusal names all three remedies, and no statement
// is built from it; and so is every spelling that is not one of the three.
func TestADeclarationStatesItsStanding(t *testing.T) {
	t.Parallel()
	declared := func(s Standing) ReferenceTable {
		return ReferenceTable{Domain: "tracker", Table: "t", Key: "object", Hash: "hash",
			Size: "size", Owner: []string{"path"}, Standing: s}
	}
	for _, s := range []Standing{"", "other", "live", "Required", "required ", "RETIRED", "Derived", " derived"} {
		t.Run("refused "+strconv.Quote(string(s)), func(t *testing.T) {
			t.Parallel()
			ref := declared(s)
			if s.Valid() {
				t.Errorf("Standing(%q).Valid() = true", s)
			}
			err := ref.Validate()
			if err == nil {
				t.Fatalf("a declaration with standing %q validated", s)
			}
			for _, remedy := range []string{"objstore.Required", "objstore.Retired", "objstore.Derived"} {
				if !strings.Contains(err.Error(), remedy) {
					t.Errorf("the refusal %q does not name %s", err, remedy)
				}
			}
			if _, err := ref.ObjectsAmong(1); err == nil {
				t.Error("a question was built from it")
			}
			if _, err := ref.ReferencesAfter(1); err == nil {
				t.Error("a page was built from it")
			}
		})
	}
	for _, s := range []Standing{Required, Retired, Derived} {
		ref := declared(s)
		if !s.Valid() {
			t.Errorf("Standing(%q).Valid() = false", s)
		}
		if err := ref.Validate(); err != nil {
			t.Errorf("a declaration standing %s: %v", s, err)
		}
		if _, err := ref.ObjectsAmong(1); err != nil {
			t.Errorf("a declaration standing %s cannot be asked about its objects: %v", s, err)
		}
	}
}

// A WALK READS ONLY THE STANDINGS ITS PASS IS OWED (ADR-0074). A retired or
// derived row keeps its object from the collector exactly as a live one does
// — so the collector's batch question is the same statement whatever the
// standing — and names nothing the company is owed: a retired one a replaced
// object, a derived one an object the engine builds again. So the walk the
// audit takes and the one a backup takes are REFUSED for either, and so is the
// question about given keys a backup asks past its copy; a store move's walk
// reads a derived table besides — its objects are ones the moved fleet would
// otherwise build again on every node — and still refuses a retired one, whose
// rows no pass reads at all. Each refusal comes on the walk's first page,
// before anything is read, so a pass that forgot to leave such a table out
// fails on its first pass rather than paging somebody about an object nobody
// has to repair, copying it into every backup, or carrying a replaced one into
// the store the fleet moves to.
func TestAWalkReadsOnlyTheStandingsItsPassIsOwed(t *testing.T) {
	t.Parallel()
	live := ReferenceTable{Domain: "tracker", Table: "replaced_packs", Key: "object", Hash: "hash",
		Size: "size", Owner: []string{"path"}, Standing: Required}

	walked, err := live.ReferencesAfter(500)
	if err != nil {
		t.Fatalf("a required table cannot be walked: %v", err)
	}
	if _, err := live.ReferencesAmong(0); err == nil {
		t.Error("a question about no keys was asked")
	}
	among, err := live.ReferencesAmong(3)
	if err != nil {
		t.Fatalf("a required table cannot be asked which references it holds: %v", err)
	}
	columns := func(q string) string { return q[:strings.Index(q, " WHERE ")] }
	if columns(among) != columns(walked) || !strings.HasSuffix(among, ` IN (?, ?, ?)`) {
		t.Errorf("ReferencesAmong = %q; want the walk's columns, %q, over three keys", among, columns(walked))
	}
	keeps, err := live.ObjectsAmong(3)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		standing Standing
		// walkedBy is every pass whose walk reads a table of the standing,
		// and carried whether a backup asks which references one holds.
		walkedBy []Pass
		carried  bool
	}{
		{Required, []Pass{PassAudit, PassBackup, PassMove}, true},
		{Retired, nil, false},
		{Derived, []Pass{PassMove}, false},
	} {
		t.Run(string(c.standing), func(t *testing.T) {
			t.Parallel()
			table := live
			table.Standing = c.standing

			// THE STATEMENT is built for a standing some pass walks, and
			// refused, naming the table and its standing, for one none does.
			page, err := table.ReferencesAfter(500)
			switch {
			case len(c.walkedBy) > 0 && err != nil:
				t.Fatalf("a %s table, which a pass walks, has no walk statement: %v", c.standing, err)
			case len(c.walkedBy) > 0 && page != walked:
				t.Errorf("a %s table's walk statement is %q, a required one's %q", c.standing, page, walked)
			case len(c.walkedBy) == 0 && err == nil:
				t.Fatalf("a %s table, which no pass walks, has a walk statement: %q", c.standing, page)
			case len(c.walkedBy) == 0:
				for _, named := range []string{"replaced_packs", string(c.standing)} {
					if !strings.Contains(err.Error(), named) {
						t.Errorf("the refusal %q does not name %q", err, named)
					}
				}
			}

			// EACH PASS'S WALK reads the table where the pass is owed its
			// objects, and anywhere else refuses it from its first page,
			// before anything is read, naming the table, its standing and
			// the pass.
			for _, p := range []Pass{PassAudit, PassBackup, PassMove} {
				owed := slices.Contains(c.walkedBy, p)
				if p.Reads(c.standing) != owed {
					t.Errorf("the %s pass reads a %s table: %v, want %v", p, c.standing, !owed, owed)
				}
				read := func(context.Context, func(*sql.Tx) error) (bool, error) {
					if !owed {
						t.Errorf("the %s pass's walk of a %s table read the estate", p, c.standing)
					}
					return true, nil
				}
				refs, _, werr := table.WalkOf(read, p).Next(t.Context())
				switch {
				case owed && werr != nil:
					t.Errorf("the %s pass cannot walk a %s table: %v", p, c.standing, werr)
				case !owed && werr == nil:
					t.Errorf("the %s pass walked a %s table: %d references", p, c.standing, len(refs))
				case !owed:
					for _, named := range []string{"replaced_packs", string(c.standing), string(p)} {
						if !strings.Contains(werr.Error(), named) {
							t.Errorf("the refusal %q does not name %q", werr, named)
						}
					}
				}
			}

			// THE QUESTION ABOUT GIVEN KEYS is a backup's alone, and answers a
			// walk's own columns so one decoder reads both.
			if among, err := table.ReferencesAmong(3); (err == nil) != c.carried {
				t.Errorf("a %s table asked which references it holds = %q, %v; want it built: %v",
					c.standing, among, err, c.carried)
			}

			shields, err := table.ObjectsAmong(3)
			if err != nil {
				t.Fatalf("a %s table cannot be asked which objects it names: %v", c.standing, err)
			}
			if shields != keeps {
				t.Errorf("a %s table is asked %q and a required one %q — a row keeps its "+
					"object from collection the same way whatever its standing", c.standing, shields, keeps)
			}
		})
	}

	// A WALK FOR NO PASS reads nothing either: a pass is never a default.
	for _, p := range []Pass{"", "Audit", "migrate"} {
		if p.Valid() {
			t.Errorf("Pass(%q).Valid() = true", p)
		}
		read := func(context.Context, func(*sql.Tx) error) (bool, error) {
			t.Errorf("a walk for the pass %q read the estate", p)
			return true, nil
		}
		if _, _, err := live.WalkOf(read, p).Next(t.Context()); err == nil {
			t.Errorf("a walk for the pass %q was read", p)
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
	if Stall != 2*MiBPace {
		t.Errorf("Stall = %v, want two paces", Stall)
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
		Hash: "hash", Size: "size", Owner: []string{"project_key", "path"}, Standing: Required}
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

// A REQUIRED TABLE SAYS HOW ITS LOG NAMES OBJECTS (ADR-0075): the stream, the
// table its domain defers records into, and the domain's reader of one record
// — each refused by name when missing, since a backup cannot carry what a
// log it was not told about names. A retired or derived table, which no backup
// carries, needs none of them, and its deferred records are never read; and
// the validation every reader asks comes first.
func TestARequiredTableSaysHowItsLogNamesObjects(t *testing.T) {
	t.Parallel()
	whole := ReferenceTable{Domain: "tracker", Table: "tracker_files", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"project_key", "path"}, Standing: Required,
		Log: "CREWLET_TRACKER_LOG", Deferred: "tracker_log_deferred",
		Records: func([]byte) ([]RecordReference, error) { return nil, nil }}
	if err := whole.ValidateLog(); err != nil {
		t.Fatalf("a whole declaration was refused: %v", err)
	}
	for field, broken := range map[string]func(*ReferenceTable){
		"Log":      func(r *ReferenceTable) { r.Log = " " },
		"Deferred": func(r *ReferenceTable) { r.Deferred = "" },
		"Records":  func(r *ReferenceTable) { r.Records = nil },
		"Standing": func(r *ReferenceTable) { r.Standing = "" },
	} {
		ref := whole
		broken(&ref)
		err := ref.ValidateLog()
		if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "tracker_files") {
			t.Errorf("a declaration with no %s = %v; want it refused naming the table and the field", field, err)
		}
	}
	unquoted := whole
	unquoted.Deferred = "tracker_log_deferred; DROP TABLE tracker_files"
	if err := unquoted.ValidateLog(); err == nil {
		t.Error("a deferred table that is not an identifier was accepted into a statement")
	}
	for _, s := range []Standing{Retired, Derived} {
		unowed := ReferenceTable{Domain: "tracker", Table: "replaced", Key: "object", Hash: "hash",
			Size: "size", Owner: []string{"path"}, Standing: s}
		if err := unowed.ValidateLog(); err != nil {
			t.Errorf("a %s declaration naming no log was refused: %v", s, err)
		}
		if _, err := unowed.DeferredRecordsAfter(256); err == nil {
			t.Errorf("a %s table's deferred records were read", s)
		}
	}
	if _, err := whole.DeferredRecordsAfter(0); err == nil {
		t.Error("a page of no deferred records was asked for")
	}
	query, err := whole.DeferredRecordsAfter(256)
	if err != nil || query != `SELECT position, payload FROM tracker_log_deferred `+
		`WHERE position > ? ORDER BY position LIMIT 256` {
		t.Errorf("DeferredRecordsAfter = %q, %v", query, err)
	}
}

// AN OWNER IS JOINED ONE WAY. The walk of a table spells a row's owner values
// with JoinOwner and a domain's reader of a record spells the row it writes
// with it too, so the backup can tell which row a record rewrote — spelled two
// ways, a file replaced during a backup would read as two files.
func TestAnOwnerIsJoinedOneWay(t *testing.T) {
	t.Parallel()
	if got := JoinOwner("ENG", "a/b.md"); got != "ENG/a/b.md" {
		t.Errorf(`JoinOwner("ENG", "a/b.md") = %q`, got)
	}
	table := ReferenceTable{Domain: "tracker", Table: "tracker_files", Key: "object",
		Hash: "hash", Size: "size", Owner: []string{"project_key", "path"}, Standing: Required}
	k := KeyAt(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	var named string
	if err := table.ScanReferences(&fakeRows{rows: [][]any{
		{k.String(), string(HashOf(nil)), int64(0), "ENG", "a/b.md"},
	}}, func(_ Object, namedBy string) error { named = namedBy; return nil }); err != nil {
		t.Fatal(err)
	}
	if named != JoinOwner("ENG", "a/b.md") {
		t.Errorf("the walk spells the row %q and JoinOwner %q", named, JoinOwner("ENG", "a/b.md"))
	}
}

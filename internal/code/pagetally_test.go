package code

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE PAGES A TABLE HOLDS, read off the estate's file.
//
// What a push leaves every data node is rows of two lifetimes: its
// code_pushes row, which the push-history horizon keeps 181 days, and its
// pack's code_packs row, which lives as long as the pack. Only the TABLES
// tell the two apart. The push row names its pack too (its pack_id and
// pack_bytes), so the file's growth over pushes carrying a pack, less its
// growth over pushes carrying none, counts those columns — kept 181 days —
// as the pack row's, kept for good; and that difference moves with how many
// rows of each shape fit a page, so one pack row weighed differently beside a
// person's push and beside a seat's. dbstat, SQLite's count of a
// table's pages, is no table Turso has, so the count is read from the file in
// SQLite's format, which Turso writes: each b-tree walked from the root page
// sqlite_master names, every overflow chain followed, the free list counted
// from the header — and held to the file, every page of which is one
// b-tree's or free, or the walk is wrong.

// pageTally is the estate's file page by page: what each table's b-trees hold
// — its own and its indexes', overflow pages included — and what is free.
type pageTally struct {
	pageBytes int64            // the file's page size
	pages     int64            // the pages the file holds
	free      int64            // the pages on its free list
	tables    map[string]int64 // by table, its indexes' b-trees counted with its own
}

// tally is the estate's file page by page once a truncating checkpoint has
// folded every committed page into it — the database itself, never a
// sidecar's frames — failing t where a page is held by two b-trees, or by
// none and is not free.
func (e *estate) tally(t testing.TB) pageTally {
	t.Helper()
	db := storetest.ReplicatedDB(t, e.db)
	ctx := t.Context()
	var busy, frames, folded sql.NullInt64
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &frames, &folded); err != nil {
		t.Fatalf("checkpoint the estate: %v", err)
	}
	if busy.Int64 != 0 {
		t.Fatalf("the estate's checkpoint was busy: %d of %d frames folded", folded.Int64, frames.Int64)
	}
	// THE SCHEMA'S OWN B-TREE is rooted at page 1, which no row of
	// sqlite_master names.
	roots := map[int64]string{1: "sqlite_master"}
	rows, err := db.SQL().QueryContext(ctx, `SELECT tbl_name, rootpage FROM sqlite_master WHERE rootpage > 0`)
	if err != nil {
		t.Fatalf("list the estate's b-trees: %v", err)
	}
	for rows.Next() {
		var table string
		var root int64
		if scanErr := rows.Scan(&table, &root); scanErr != nil {
			t.Fatalf("read the estate's b-trees: %v", scanErr)
		}
		roots[root] = table
	}
	if listErr := errors.Join(rows.Err(), rows.Close()); listErr != nil {
		t.Fatalf("list the estate's b-trees: %v", listErr)
	}
	raw, err := os.ReadFile(db.Path())
	if err != nil {
		t.Fatalf("read the estate's file: %v", err)
	}
	f, err := readSQLiteFile(raw)
	if err != nil {
		t.Fatalf("read the estate's file %s: %v", db.Path(), err)
	}
	tally := pageTally{pageBytes: f.pageBytes, pages: f.pages, free: f.free, tables: map[string]int64{}}
	held := make(map[int64]bool, f.pages)
	for root, table := range roots {
		n, walkErr := f.btreePages(root, held)
		if walkErr != nil {
			t.Fatalf("walk %s's b-tree from page %d: %v", table, root, walkErr)
		}
		tally.tables[table] += n
	}
	if accounted := int64(len(held)) + f.free + f.lockPages(); accounted != f.pages {
		t.Fatalf("the estate's file holds %d pages, and its b-trees hold %d, its free list %d and the lock "+
			"byte's page %d: a tally that leaves a page out counts no table's bytes right", f.pages,
			len(held), f.free, f.lockPages())
	}
	return tally
}

// sqliteFile is a database file in SQLite's format, laid out as its header
// says.
type sqliteFile struct {
	raw       []byte
	pageBytes int64 // a page
	usable    int64 // a page less the bytes each reserves at its end
	pages     int64 // the database's pages
	free      int64 // on its free list
}

// The b-tree page types.
const (
	indexInterior = 0x02
	tableInterior = 0x05
	indexLeaf     = 0x0a
	tableLeaf     = 0x0d
)

// readSQLiteFile reads raw's header, refusing a file that is no SQLite
// database, one whose header's page count is not the file's, and one laid
// out with auto-vacuum's pointer-map pages, which are no b-tree's and which
// the tally does not count.
func readSQLiteFile(raw []byte) (sqliteFile, error) {
	if len(raw) < 100 || string(raw[:16]) != "SQLite format 3\x00" {
		return sqliteFile{}, errors.New("not a SQLite database: no header")
	}
	size := int64(binary.BigEndian.Uint16(raw[16:]))
	if size == 1 {
		size = 1 << 16
	}
	f := sqliteFile{raw: raw, pageBytes: size, usable: size - int64(raw[20]),
		pages: int64(binary.BigEndian.Uint32(raw[28:])), free: int64(binary.BigEndian.Uint32(raw[36:]))}
	switch {
	case size < 512 || int64(len(raw))%size != 0:
		return sqliteFile{}, fmt.Errorf("a page of %d bytes does not divide the file's %d", size, len(raw))
	case f.pages != int64(len(raw))/size:
		return sqliteFile{}, fmt.Errorf("the header counts %d pages and the file holds %d", f.pages,
			int64(len(raw))/size)
	case binary.BigEndian.Uint32(raw[52:]) != 0:
		return sqliteFile{}, errors.New("an auto-vacuum database, whose pointer-map pages the tally does not count")
	}
	return f, nil
}

// page is page p's bytes; pages count from 1.
func (f sqliteFile) page(p int64) []byte { return f.raw[(p-1)*f.pageBytes : p*f.pageBytes] }

// lockPages is the page holding the lock byte at the file's first gibibyte —
// one no b-tree and no free list ever holds — where the file reaches it.
func (f sqliteFile) lockPages() int64 {
	if f.pages > (1<<30)/f.pageBytes {
		return 1
	}
	return 0
}

// btreePages is how many pages the b-tree rooted at root holds — its interior
// and leaf pages and every overflow page a payload spills into — each marked
// in held, so a page reached twice, or one past the file, fails the walk.
func (f sqliteFile) btreePages(root int64, held map[int64]bool) (int64, error) {
	var n int64
	take := func(p int64) error {
		if p < 1 || p > f.pages || held[p] {
			return fmt.Errorf("page %d is past the file's %d pages or held twice", p, f.pages)
		}
		held[p] = true
		n++
		return nil
	}
	todo := []int64{root}
	for len(todo) > 0 {
		p := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if err := take(p); err != nil {
			return 0, err
		}
		page := f.page(p)
		at := 0
		if p == 1 {
			at = 100 // page 1 opens with the file's header
		}
		kind, cells, pointers := page[at], int(binary.BigEndian.Uint16(page[at+3:])), at+8
		switch kind {
		case indexInterior, tableInterior:
			todo = append(todo, int64(binary.BigEndian.Uint32(page[at+8:])))
			pointers = at + 12
		case indexLeaf, tableLeaf:
		default:
			return 0, fmt.Errorf("page %d is no b-tree page: its type is %#x", p, kind)
		}
		if pointers+2*cells > len(page) {
			return 0, fmt.Errorf("page %d's %d cells overrun it", p, cells)
		}
		for i := range cells {
			c := int(binary.BigEndian.Uint16(page[pointers+2*i:]))
			if kind == indexInterior || kind == tableInterior {
				if c+4 > len(page) {
					return 0, fmt.Errorf("page %d's cell %d starts past it", p, i)
				}
				todo = append(todo, int64(binary.BigEndian.Uint32(page[c:])))
				c += 4
			}
			if kind == tableInterior {
				continue // a key, which carries no payload
			}
			payload, w := sqliteVarint(page[c:])
			c += w
			if kind == tableLeaf {
				_, w = sqliteVarint(page[c:]) // the rowid
				c += w
			}
			local := f.local(kind, payload)
			if local == payload {
				continue
			}
			if c+int(local)+4 > len(page) || w == 0 {
				return 0, fmt.Errorf("page %d's cell %d of %d payload bytes overruns it", p, i, payload)
			}
			// THE OVERFLOW CHAIN: its first page named after the part the
			// page holds, each page naming the next and holding all but four
			// of its usable bytes.
			want := (payload - local + f.usable - 5) / (f.usable - 4)
			var chain int64
			for next := int64(binary.BigEndian.Uint32(page[c+int(local):])); next != 0; chain++ {
				if err := take(next); err != nil {
					return 0, fmt.Errorf("the overflow of page %d's cell %d: %w", p, i, err)
				}
				next = int64(binary.BigEndian.Uint32(f.page(next)))
			}
			if chain != want {
				return 0, fmt.Errorf("page %d's cell %d spills %d payload bytes into %d overflow pages, "+
					"where %d hold them", p, i, payload-local, chain, want)
			}
		}
	}
	return n, nil
}

// local is how many of a cell's payload bytes its own page holds, the rest
// spilling into overflow pages — SQLite's rule, whose bound is a table leaf's
// or an index's.
func (f sqliteFile) local(kind byte, payload int64) int64 {
	most := f.usable - 35
	if kind != tableLeaf {
		most = (f.usable-12)*64/255 - 23
	}
	if payload <= most {
		return payload
	}
	least := (f.usable-12)*32/255 - 23
	if k := least + (payload-least)%(f.usable-4); k <= most {
		return k
	}
	return least
}

// sqliteVarint reads the varint b opens with — SQLite's: big-endian, seven
// bits a byte while its high bit is set, all eight of a ninth — and how many
// bytes it took; 0 bytes where b ends first.
func sqliteVarint(b []byte) (int64, int) {
	var v uint64
	for i, c := range b {
		if i == 8 {
			return int64(v<<8 | uint64(c)), 9
		}
		v = v<<7 | uint64(c&0x7f)
		if c < 0x80 {
			return int64(v), i + 1
		}
	}
	return 0, 0
}

// EVERY PAGE OF THE ESTATE IS ONE TABLE'S OR FREE, as the tally counts it:
// BenchmarkPushHistoryBytes prints what each of a push's two rows costs by
// the pages its table grew by, so a tally that left a page out, or counted
// one twice, would print a lifetime's bytes wrong. Pushes of every shape that
// benchmark measures — one commit with its pack and with none, and a row
// past a page, which spills into overflow pages — then a dropped table, whose
// pages go to the free list, each tallied, the tally failing itself where the
// pages do not add up.
//
// Mutation: count no overflow page, or no free page, or walk no interior
// page's rightmost child, and the pages no longer add up — red.
func TestATallyCountsEveryPageOfTheEstateOnce(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	const repo = "repo-tally"
	e.apply(create(t, repo, "tally"))
	start := e.tally(t)
	tip := ZeroOID
	for r := range 300 {
		next := oid(fmt.Sprint("tally-", r))
		entry := pushEntry(fmt.Sprint("tally-", r), move("refs/heads/main", tip, next))
		if r%3 == 0 {
			entry.Pack = nil
		}
		entry.Introduced, entry.IntroducedCount = []string{next}, 1
		e.apply(pushOf(t, repo, entry))
		tip = next
	}
	// A ROW PAST A PAGE: a push of refs whose names fill MaxRefNameBytes,
	// enough of them that its commands spill out of its leaf.
	wide := make([]Command, 64)
	for c := range wide {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		wide[c] = move(ref+strings.Repeat("w", MaxRefNameBytes-len(ref)), ZeroOID, oid(fmt.Sprint("wide-", c)))
	}
	e.apply(pushOf(t, repo, pushEntry("tally-wide", wide...)))
	pushed := e.tally(t)
	for _, table := range []string{"code_pushes", "code_packs"} {
		if pushed.tables[table] <= start.tables[table] {
			t.Errorf("%s holds %d pages after 301 pushes, as many as the %d before them: the tally reads "+
				"no table's growth", table, pushed.tables[table], start.tables[table])
		}
	}
	// THE FREE LIST: a table dropped gives its pages back to the file.
	db := storetest.ReplicatedDB(t, e.db).SQL()
	if _, err := db.ExecContext(t.Context(),
		`CREATE TABLE tally_scratch (k INTEGER PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for k := range 200 {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO tally_scratch (k, v) VALUES (?, ?)`, k,
			strings.Repeat("v", 600)); err != nil {
			t.Fatal(err)
		}
	}
	if scratch := e.tally(t).tables["tally_scratch"]; scratch < 2 {
		t.Fatalf("a table of 200 rows of 600 bytes holds %d pages", scratch)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TABLE tally_scratch`); err != nil {
		t.Fatal(err)
	}
	if dropped := e.tally(t); dropped.free <= pushed.free {
		t.Errorf("a dropped table of 200 rows left %d pages free, as many as the %d before it: the tally "+
			"is never held to a free list", dropped.free, pushed.free)
	}
}

package artifactdata

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reader answers questions about a store from this node's own applied rows,
// every one through [statelog.Reader], so a level is a property of the answer:
// the refusal ladder, the coverage probe over the store and the freshness
// target come before the rows, in one transaction.
//
// # A row hidden from a reader is hidden from every read path
//
// `own_rows`, `editors` and `private` apply to the listing, a query, a single
// document, the history and the export alike (03 section 4.5): a history row
// that showed "Dana changed row 12" for a row the reader cannot read leaks the
// row's existence and Dana's vote. One function, [Readable], answers every
// path.
type Reader struct {
	db  store.ReplicatedReader
	log *statelog.Reader
}

// ReaderOptions configure a reader.
type ReaderOptions struct {
	DB store.ReplicatedReader

	// Log is this domain's read authority. REQUIRED.
	Log *statelog.Reader
}

// NewReader builds the datastore's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB.IsZero() {
		return nil, errors.New("artifactdata: a reader needs the replicated estate")
	}
	if opts.Log == nil {
		return nil, errors.New("artifactdata: a reader needs its domain's read authority: " +
			"without it every read level is a label rather than a guarantee")
	}
	return &Reader{db: opts.DB, log: opts.Log}, nil
}

// Reading is who is reading, and how fresh the answer must be. The record and
// the role are decided by the caller (the role from the artifact's shares,
// through the authority table); the reader applies the collection's read rule
// to them on every path.
type Reading struct {
	// Record is the reader's iam.RecordOwner: what a row's creator is
	// compared with.
	Record string

	// Role is the reader's effective role on the artifact.
	Role artifacts.Role

	// Fresh is the level the answer is read at; empty is `stale`, whatever
	// this node holds, with its coverage on the answer.
	Fresh statelog.Freshness
}

// fresh is the level a reading is served at.
func (r Reading) fresh() statelog.Freshness {
	f := r.Fresh
	if f.Level == "" {
		f.Level = statelog.ReadStale
	}
	return f
}

// Readable reports whether a reader may see a row of a collection whose read
// rule is rule and whose creator is createdBy: THE ONE ANSWER every read path
// asks. A rule this build does not know admits nobody.
func Readable(rule artifacts.ReadRule, r Reading, createdBy string) bool {
	if !r.Role.Includes(artifacts.RoleViewer) {
		return false
	}
	switch rule {
	case artifacts.ReadAll:
		return true
	case artifacts.ReadOwnRows:
		return r.Role.Includes(artifacts.RoleEditor) || (r.Record != "" && createdBy == r.Record)
	case artifacts.ReadEditors:
		return r.Role.Includes(artifacts.RoleEditor)
	case artifacts.ReadPrivate:
		return r.Record != "" && createdBy == r.Record
	}
	return false
}

// restriction is how a read rule narrows a collection's rows for a reader: to
// every row, to the reader's own, or to none.
func restriction(rule artifacts.ReadRule, r Reading) (createdBy string, none bool) {
	switch {
	case !r.Role.Includes(artifacts.RoleViewer):
		return "", true
	case rule == artifacts.ReadAll:
		return "", false
	case rule == artifacts.ReadOwnRows && r.Role.Includes(artifacts.RoleEditor):
		return "", false
	case rule == artifacts.ReadEditors:
		return "", !r.Role.Includes(artifacts.RoleEditor)
	case rule == artifacts.ReadOwnRows || rule == artifacts.ReadPrivate:
		return r.Record, r.Record == ""
	}
	return "", true
}

// Doc is one document as a reader sees it.
type Doc struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`

	// Fields are the document's fields, canonical JSON, with a column's
	// default filled where the field is absent. Empty on a tombstone.
	Fields  json.RawMessage `json:"fields,omitempty"`
	Deleted bool            `json:"deleted,omitempty"`

	// Version is the document's version: the if_version a guarded write
	// names.
	Version int64 `json:"version"`

	CreatedBy iam.Actor `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedBy iam.Actor `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`

	// Defaulted names the fields read from a column's default rather than
	// stored.
	Defaulted []string `json:"defaulted,omitempty"`

	// Invalid names the stored fields the collection's schema no longer
	// admits (a value written before a column was retyped or removed),
	// which a reader shows as a warning rather than drops.
	Invalid []string `json:"invalid,omitempty"`
}

// DocPage is one page of a collection.
type DocPage struct {
	Docs []Doc `json:"docs"`

	// After is the cursor of the next page, absent on the last.
	After string `json:"after,omitempty"`

	// Resync says the `since` asked is below what the store can still
	// account for (a delete older than it has been forgotten): read the
	// collection whole.
	Resync bool `json:"resync,omitempty"`

	// Through is the store's version when it was read: the `since` the
	// next catch-up names.
	Through int64 `json:"through"`

	Position statelog.Position `json:"position"`
	Complete bool              `json:"complete"`
}

// pageLimit is a page size: the default where none is asked, the query bound
// at most.
func pageLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return query.DefaultLimit, nil
	case limit < 0 || limit > artifacts.MaxQueryLimit:
		return 0, fmt.Errorf("%w: limit %d is outside 1..%d", ErrInvalid, limit, artifacts.MaxQueryLimit)
	}
	return limit, nil
}

// read runs fn over one store's rows at the reading's level.
func (r *Reader) read(ctx context.Context, id artifacts.ID, rd Reading,
	fn func(tx *sql.Tx, s storeRow, present bool) error) (statelog.Answer, error) {

	return r.log.Read(ctx, rd.fresh().Query(StoreScope(id.String()), true), func(tx *sql.Tx) error {
		if _, marked, err := deletionMarker(ctx, tx, id.String()); err != nil {
			return err
		} else if marked {
			return fmt.Errorf("%w: %s", ErrGone, id)
		}
		s, present, err := readStore(ctx, tx, id.String())
		if err != nil {
			return err
		}
		return fn(tx, s, present)
	})
}

// visibleCollection is the collection a reader asked for, or ErrNotFound where
// the store holds none or the reader may never read a row of it.
func visibleCollection(ctx context.Context, tx *sql.Tx, s storeRow, present bool, name string,
	rd Reading) (*collState, error) {

	v := &txView{ctx: ctx, tx: tx, store: s, present: present}
	c, err := v.collection(name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("%w: collection %q", ErrNotFound, name)
	}
	if _, none := restriction(c.spec.Read, rd); none {
		return nil, fmt.Errorf("%w: collection %q", ErrNotFound, name)
	}
	return c, nil
}

// Docs answers one page of a collection: every live document in id order, or,
// with since above zero, every document (tombstones included) whose version
// is above since, in version order, for a client catching up.
func (r *Reader) Docs(ctx context.Context, id artifacts.ID, rd Reading, collection string,
	since int64, cursor string, limit int) (DocPage, error) {

	limit, err := pageLimit(limit)
	if err != nil {
		return DocPage{}, err
	}
	var page DocPage
	served, err := r.read(ctx, id, rd, func(tx *sql.Tx, s storeRow, present bool) error {
		c, err := visibleCollection(ctx, tx, s, present, collection, rd)
		if err != nil {
			return err
		}
		page = DocPage{Docs: []Doc{}, Through: s.version}
		if !c.stored {
			return nil
		}
		if since > 0 && since < s.tombstoneFloor {
			page.Resync = true
			return nil
		}
		createdBy, _ := restriction(c.spec.Read, rd)
		q := `SELECT doc_id, body, created_by, created_by_kind, created_at, updated_by,
			updated_by_kind, updated_at, version FROM artifact_data_docs
			WHERE store_no = ? AND collection_no = ?`
		args := []any{s.no, c.no}
		if createdBy != "" {
			q += ` AND created_by = ?`
			args = append(args, createdBy)
		}
		if since > 0 {
			q += ` AND version > ?`
			args = append(args, since)
			if cursor != "" {
				after, err := decodeDocCursor(cursor)
				if err != nil {
					return err
				}
				q += ` AND (version > ? OR (version = ? AND doc_id > ?))`
				args = append(args, after.Version, after.Version, after.Doc)
			}
			q += ` ORDER BY version, doc_id LIMIT ?`
		} else {
			q += ` AND body IS NOT NULL`
			if cursor != "" {
				after, err := decodeDocCursor(cursor)
				if err != nil {
					return err
				}
				q += ` AND doc_id > ?`
				args = append(args, after.Doc)
			}
			q += ` ORDER BY doc_id LIMIT ?`
		}
		args = append(args, limit+1)
		docs, err := scanDocs(ctx, tx, c, q, args...)
		if err != nil {
			return err
		}
		if len(docs) > limit {
			docs = docs[:limit]
			last := docs[limit-1]
			page.After = encodeDocCursor(docCursor{Version: last.Version, Doc: last.ID})
		}
		page.Docs = docs
		return nil
	})
	if err != nil {
		return DocPage{}, err
	}
	page.Position, page.Complete = served.Position, served.Complete
	return page, nil
}

// Doc answers one document, or ErrNotFound where it does not exist, is a
// tombstone, or the reader may not see it.
func (r *Reader) Doc(ctx context.Context, id artifacts.ID, rd Reading, collection, doc string) (Doc, error) {
	var out Doc
	_, err := r.read(ctx, id, rd, func(tx *sql.Tx, s storeRow, present bool) error {
		c, err := visibleCollection(ctx, tx, s, present, collection, rd)
		if err != nil {
			return err
		}
		if !c.stored {
			return fmt.Errorf("%w: %s/%s", ErrNotFound, collection, doc)
		}
		docs, err := scanDocs(ctx, tx, c, `
			SELECT doc_id, body, created_by, created_by_kind, created_at, updated_by,
			updated_by_kind, updated_at, version FROM artifact_data_docs
			WHERE store_no = ? AND collection_no = ? AND doc_id = ? AND body IS NOT NULL`,
			s.no, c.no, doc)
		if err != nil {
			return err
		}
		if len(docs) == 0 || !Readable(c.spec.Read, rd, docs[0].CreatedBy.Name) {
			return fmt.Errorf("%w: %s/%s", ErrNotFound, collection, doc)
		}
		out = docs[0]
		return nil
	})
	return out, err
}

// Query answers one page of a query over a collection's field index, under the
// collection's read rule.
func (r *Reader) Query(ctx context.Context, id artifacts.ID, rd Reading, collection string,
	q query.Query) (DocPage, error) {

	var page DocPage
	served, err := r.read(ctx, id, rd, func(tx *sql.Tx, s storeRow, present bool) error {
		c, err := visibleCollection(ctx, tx, s, present, collection, rd)
		if err != nil {
			return err
		}
		page = DocPage{Docs: []Doc{}, Through: s.version}
		createdBy, _ := restriction(c.spec.Read, rd)
		idx := &storeIndex{ctx: ctx, tx: tx, store: s.no, coll: c}
		compiled, err := query.Compile(q, query.Scope{StoreNo: s.no, CollectionNo: c.no,
			CreatedBy: createdBy}, idx)
		if err != nil {
			return err
		}
		if idx.err != nil {
			return idx.err
		}
		if !c.stored {
			return nil
		}
		rows, err := tx.QueryContext(ctx, compiled.SQL, compiled.Args...)
		if err != nil {
			return fmt.Errorf("artifactdata: run a query on %s: %w", collection, err)
		}
		defer func() { _ = rows.Close() }()
		fetched := 0
		var last struct {
			kind, txt, doc string
			num            float64
		}
		for rows.Next() {
			var (
				d       docRow
				sk, st  string
				sn      float64
				bodyStr sql.NullString
			)
			if err := rows.Scan(&d.id, &bodyStr, &d.createdBy, &d.createdKind, &d.createdAt,
				&d.updatedBy, &d.updatedKind, &d.updatedAt, &d.version, &d.size, &sk, &sn, &st); err != nil {
				return fmt.Errorf("artifactdata: read a query's row: %w", err)
			}
			fetched++
			if fetched > compiled.Limit-1 {
				break
			}
			last.kind, last.num, last.txt, last.doc = sk, sn, st, d.id
			d.body = bodyStr
			keep, err := compiled.Keep([]byte(bodyStr.String))
			if err != nil || !keep {
				if err != nil {
					return err
				}
				continue
			}
			page.Docs = append(page.Docs, renderDoc(c, d))
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("artifactdata: run a query on %s: %w", collection, err)
		}
		if fetched >= compiled.Limit {
			page.After = compiled.Cursor(last.kind, last.num, last.txt, last.doc)
		}
		return nil
	})
	if err != nil {
		return DocPage{}, err
	}
	page.Position, page.Complete = served.Position, served.Complete
	return page, nil
}

// storeIndex is one collection's field index as the query compiler asks it.
type storeIndex struct {
	ctx   context.Context
	tx    *sql.Tx
	store int64
	coll  *collState
	err   error
}

func (i *storeIndex) Indexed(path string) bool {
	if !strings.Contains(path, ".") {
		return true
	}
	return slices.Contains(i.coll.spec.Indexed, path)
}

func (i *storeIndex) PathNo(path string) (int64, bool) {
	var no int64
	err := i.tx.QueryRowContext(i.ctx, `
		SELECT path_no FROM artifact_data_paths WHERE store_no = ? AND path = ?`,
		i.store, path).Scan(&no)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false
	case err != nil:
		i.err = fmt.Errorf("artifactdata: read path %s: %w", path, err)
		return 0, false
	}
	return no, true
}

// docRow is one document row as it is scanned.
type docRow struct {
	id                                string
	body                              sql.NullString
	size                              int64
	createdBy, createdKind, updatedBy string
	updatedKind                       string
	createdAt, updatedAt, version     int64
}

// scanDocs runs a document query selecting doc_id, body, created_by,
// created_by_kind, created_at, updated_by, updated_by_kind, updated_at and
// version, and renders each row.
func scanDocs(ctx context.Context, tx *sql.Tx, c *collState, q string, args ...any) ([]Doc, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: read %s: %w", c.spec.Name, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Doc
	for rows.Next() {
		var d docRow
		if err := rows.Scan(&d.id, &d.body, &d.createdBy, &d.createdKind, &d.createdAt,
			&d.updatedBy, &d.updatedKind, &d.updatedAt, &d.version); err != nil {
			return nil, fmt.Errorf("artifactdata: read %s: %w", c.spec.Name, err)
		}
		out = append(out, renderDoc(c, d))
	}
	return out, rows.Err()
}

// renderDoc is a row as a reader sees it: defaults filled, invalid fields
// named. READS ARE LENIENT: a value the schema no longer admits is returned
// and marked, never dropped.
func renderDoc(c *collState, d docRow) Doc {
	out := Doc{
		Collection: c.spec.Name, ID: d.id, Version: d.version,
		CreatedBy: iam.Actor{Name: d.createdBy, Kind: iam.ActorKind(d.createdKind)},
		CreatedAt: store.DecodeTime(d.createdAt),
		UpdatedBy: iam.Actor{Name: d.updatedBy, Kind: iam.ActorKind(d.updatedKind)},
		UpdatedAt: store.DecodeTime(d.updatedAt),
	}
	if !d.body.Valid {
		out.Deleted = true
		return out
	}
	parsed, err := canon.Parse([]byte(d.body.String), canon.MaxDepth)
	if err != nil || parsed.Kind != canon.KindObject {
		// UNREACHABLE for a body this encoder wrote; returned as it is
		// rather than hidden, with every field marked.
		out.Fields = json.RawMessage(d.body.String)
		return out
	}
	fields := map[string]canon.Value{}
	for _, m := range parsed.Members {
		fields[m.Key] = m.Value
	}
	if !c.schemaless() {
		for _, name := range sortedKeys(fields) {
			col, declared := c.columns[name]
			if !declared || checkValue(col, fields[name]) != nil {
				out.Invalid = append(out.Invalid, name)
			}
		}
		for _, col := range c.spec.Columns {
			if _, held := fields[col.Key]; held || len(col.Default) == 0 {
				continue
			}
			v, err := canon.Parse(col.Default, MaxValueDepth)
			if err != nil || v.Kind == canon.KindNull {
				continue
			}
			fields[col.Key] = v
			out.Defaulted = append(out.Defaulted, col.Key)
		}
	}
	out.Fields = encodeFields(fields)
	return out
}

// docCursor is where a document page stopped.
type docCursor struct {
	Version int64  `json:"v,omitempty"`
	Doc     string `json:"d"`
}

func encodeDocCursor(c docCursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeDocCursor(s string) (docCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return docCursor{}, ErrBadCursor
	}
	var c docCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Doc == "" || c.Version < 0 {
		return docCursor{}, ErrBadCursor
	}
	return c, nil
}

// HistoryFilter narrows a store's history: to one collection, one document of
// it, one field (a cell's history), or one actor's writes.
type HistoryFilter struct {
	Collection string
	Doc        string
	Field      string
	Actor      string
}

// HistoryEntry is one record a store applied, as a reader may see it.
type HistoryEntry struct {
	Seq      int64                 `json:"seq"`
	OpID     string                `json:"op_id"`
	Op       OpKind                `json:"op"`
	Verdict  artifacts.DataVerdict `json:"verdict"`
	Reason   string                `json:"reason,omitempty"`
	Actor    iam.Actor             `json:"actor"`
	Origin   string                `json:"origin,omitempty"`
	WorkItem string                `json:"work_item,omitempty"`
	Elevated bool                  `json:"elevated,omitempty"`
	ClientID string                `json:"client_id,omitempty"`
	At       time.Time             `json:"at"`

	// Changes are the documents the record changed that the reader may see.
	Changes []Change `json:"changes"`

	// Undoable says the record can be undone as a whole: an applied write
	// or undo whose every value is held whole.
	Undoable bool  `json:"undoable,omitempty"`
	Undoes   int64 `json:"undoes,omitempty"`

	// Digest is the store's state digest after the record, in hex.
	Digest string `json:"digest"`
}

// HistoryPage is one page of a store's history, newest first.
type HistoryPage struct {
	Entries []HistoryEntry `json:"entries"`
	After   string         `json:"after,omitempty"`

	// Floor is the oldest position the hot history still holds.
	Floor int64 `json:"floor"`

	Position statelog.Position `json:"position"`
	Complete bool              `json:"complete"`
}

// History answers a page of a store's history, newest first, holding every row
// to the read rules of the documents it names.
func (r *Reader) History(ctx context.Context, id artifacts.ID, rd Reading, filter HistoryFilter,
	cursor string, limit int) (HistoryPage, error) {

	limit, err := pageLimit(limit)
	if err != nil {
		return HistoryPage{}, err
	}
	var before int64
	if cursor != "" {
		if before, err = strconv.ParseInt(cursor, 10, 64); err != nil || before <= 0 {
			return HistoryPage{}, ErrBadCursor
		}
	}
	if (filter.Doc != "" || filter.Field != "") && filter.Collection == "" {
		return HistoryPage{}, fmt.Errorf("%w: a document's or a cell's history names its collection",
			ErrInvalid)
	}
	var page HistoryPage
	served, err := r.read(ctx, id, rd, func(tx *sql.Tx, s storeRow, present bool) error {
		page = HistoryPage{Entries: []HistoryEntry{}, Floor: s.historyFloor}
		if !present {
			return nil
		}
		v := &txView{ctx: ctx, tx: tx, store: s, present: present}
		colls, err := readCollections(ctx, tx, s.no)
		if err != nil {
			return err
		}
		byName := map[string]*collState{}
		for _, c := range colls {
			byName[c.spec.Name] = c
		}
		q := `SELECT w.seq, w.op_id, w.op, w.verdict, w.reason, w.actor, w.actor_kind,
			w.operator_id, w.origin, w.work_item, w.elevated, w.client_id, w.at, w.changes,
			w.undoable, w.undoes, w.digest_after FROM artifact_data_writes w`
		args := []any{}
		where := []string{"w.store_no = ?"}
		if filter.Doc != "" {
			c, err := visibleCollection(ctx, tx, s, present, filter.Collection, rd)
			if err != nil {
				return err
			}
			q += ` JOIN artifact_data_touches t ON t.store_no = w.store_no AND t.seq = w.seq`
			where = append(where, "t.collection_no = ?", "t.doc_id = ?")
			args = append(args, s.no, c.no, filter.Doc)
		} else {
			args = append(args, s.no)
		}
		if filter.Actor != "" {
			where = append(where, "w.actor = ?")
			args = append(args, filter.Actor)
		}
		if before > 0 {
			where = append(where, "w.seq < ?")
			args = append(args, before)
		}
		// A SCAN BOUNDED BY THE HOT HISTORY: a page reads past rows the
		// reader may not see, at most the store's whole hot window.
		q += ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY w.seq DESC`
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("artifactdata: read %s's history: %w", id, err)
		}
		type raw struct {
			e       HistoryEntry
			changes string
			kind    string
			digest  []byte
			elev    int
			undo    int
			at      int64
		}
		var scanned []raw
		for rows.Next() {
			var x raw
			if err := rows.Scan(&x.e.Seq, &x.e.OpID, &x.e.Op, &x.e.Verdict, &x.e.Reason,
				&x.e.Actor.Name, &x.kind, &x.e.Actor.OperatorID, &x.e.Origin, &x.e.WorkItem,
				&x.elev, &x.e.ClientID, &x.at, &x.changes, &x.undo, &x.e.Undoes, &x.digest); err != nil {
				_ = rows.Close()
				return fmt.Errorf("artifactdata: read %s's history: %w", id, err)
			}
			scanned = append(scanned, x)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("artifactdata: read %s's history: %w", id, err)
		}
		creators := map[docRef]string{}
		creatorOf := func(c *collState, doc string) (string, error) {
			ref := docRef{c.spec.Name, doc}
			if who, ok := creators[ref]; ok {
				return who, nil
			}
			d, err := v.doc(c, doc)
			if err != nil {
				return "", err
			}
			creators[ref] = d.createdBy
			return d.createdBy, nil
		}
		for _, x := range scanned {
			var changes []Change
			if err := json.Unmarshal([]byte(x.changes), &changes); err != nil {
				return fmt.Errorf("artifactdata: decode a history row of %s: %w", id, err)
			}
			visible := []Change{}
			for _, ch := range changes {
				if filter.Collection != "" && ch.Collection != filter.Collection {
					continue
				}
				if filter.Doc != "" && ch.Doc != filter.Doc {
					continue
				}
				c := byName[ch.Collection]
				if ch.Doc == "" {
					// A SCHEMA CHANGE is shown to whoever may read the
					// collection's rows at all; one of a collection since
					// dropped, to editors.
					if c == nil && !rd.Role.Includes(artifacts.RoleEditor) {
						continue
					}
					if c != nil {
						if _, none := restriction(c.spec.Read, rd); none {
							continue
						}
					}
				}
				if ch.Doc != "" {
					// A ROW OF A COLLECTION SINCE DROPPED, or one the
					// reader may not see, is not shown.
					if c == nil {
						continue
					}
					who := ""
					if c.spec.Read == artifacts.ReadOwnRows || c.spec.Read == artifacts.ReadPrivate {
						if who, err = creatorOf(c, ch.Doc); err != nil {
							return err
						}
					}
					if !Readable(c.spec.Read, rd, who) {
						continue
					}
				}
				if filter.Field != "" {
					ch.Fields = slices.DeleteFunc(slices.Clone(ch.Fields), func(f FieldChange) bool {
						return f.Field != filter.Field
					})
					if len(ch.Fields) == 0 {
						continue
					}
				}
				visible = append(visible, ch)
			}
			refusedRow := x.e.Verdict != artifacts.DataApplied
			switch {
			case len(visible) > 0:
			case refusedRow && filter.Doc == "" && filter.Field == "" &&
				(x.e.Actor.Name == rd.Record || rd.Role.Includes(artifacts.RoleEditor)):
				// A REFUSED WRITE is its author's and the editors' to
				// see: it names what was attempted, not what changed.
				visible = []Change{}
			default:
				continue
			}
			x.e.Changes = visible
			x.e.Actor.Kind = iam.ActorKind(x.kind)
			x.e.Elevated, x.e.Undoable = x.elev != 0, x.undo != 0
			x.e.At = store.DecodeTime(x.at)
			if d, err := canon.DigestFrom(x.digest); err == nil {
				x.e.Digest = d.String()
			}
			page.Entries = append(page.Entries, x.e)
			if len(page.Entries) > limit {
				page.Entries = page.Entries[:limit]
				page.After = strconv.FormatInt(page.Entries[limit-1].Seq, 10)
				break
			}
		}
		return nil
	})
	if err != nil {
		return HistoryPage{}, err
	}
	page.Position, page.Complete = served.Position, served.Complete
	return page, nil
}

// Schema answers a store's collections, declared and implicit, in collection
// order. The engine's per-viewer state collection is not one of them.
func (r *Reader) Schema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error) {
	out := []artifacts.Collection{}
	_, err := r.read(ctx, id, Reading{Role: artifacts.RoleViewer}, func(tx *sql.Tx, s storeRow, present bool) error {
		if !present {
			return nil
		}
		colls, err := readCollections(ctx, tx, s.no)
		if err != nil {
			return err
		}
		for _, c := range colls {
			if c.spec.Name != StateCollection {
				out = append(out, c.spec)
			}
		}
		return nil
	})
	return out, err
}

// StoreInfo is one store's catalogue row as an operator and the dashboard's
// data tab read it.
type StoreInfo struct {
	Artifact  artifacts.ID `json:"artifact"`
	Docs      int64        `json:"docs"`
	Footprint int64        `json:"footprint"`

	// DocBytes and HistoryBytes are what the footprint sums, each in the
	// bytes its rows cost on disk: every document row with its field-index
	// entries, and the hot history with its touches. BodyBytes and
	// FieldEntries are the canonical bodies and the entry count inside
	// DocBytes.
	DocBytes     int64 `json:"doc_bytes"`
	HistoryBytes int64 `json:"history_bytes"`
	BodyBytes    int64 `json:"body_bytes"`
	FieldEntries int64 `json:"field_entries"`

	Writes int64 `json:"writes"`

	// Digest is the store's state digest, in hex: equal on two nodes, the
	// stores are equal.
	Digest string `json:"digest"`

	HistoryFloor    int64 `json:"history_floor"`
	TombstonesFloor int64 `json:"tombstones_floor"`

	// Version is the position of the store's last record.
	Version int64 `json:"version"`
}

// Store answers one store's catalogue row; an artifact whose datastore was
// never written answers an empty one.
func (r *Reader) Store(ctx context.Context, id artifacts.ID) (StoreInfo, error) {
	out := StoreInfo{Artifact: id, Digest: canon.Digest{}.String()}
	_, err := r.read(ctx, id, Reading{Role: artifacts.RoleViewer}, func(tx *sql.Tx, s storeRow, present bool) error {
		if !present {
			return nil
		}
		out = StoreInfo{
			Artifact: id, Docs: s.docs, Footprint: s.footprint, DocBytes: s.docBytes,
			BodyBytes: s.bodyBytes, FieldEntries: s.fieldEntries, HistoryBytes: s.historyBytes,
			Writes: s.writes,
			Digest: s.digest.String(), HistoryFloor: s.historyFloor,
			TombstonesFloor: s.tombstoneFloor, Version: s.version,
		}
		return nil
	})
	return out, err
}

// Usage is the company's datastore counters.
type Usage struct {
	Stores    int64 `json:"stores"`
	Docs      int64 `json:"docs"`
	Footprint int64 `json:"footprint"`

	Position statelog.Position `json:"position"`
	Complete bool              `json:"complete"`
}

// Usage answers the company's datastore counters.
func (r *Reader) Usage(ctx context.Context) (Usage, error) {
	var out Usage
	served, err := r.log.Read(ctx, statelog.Freshness{Level: statelog.ReadStale}.Query(DomainScope(), true),
		func(tx *sql.Tx) error {
			err := tx.QueryRowContext(ctx, `
				SELECT stores, docs, footprint FROM artifact_data_usage WHERE scope = ?`,
				usageScope).Scan(&out.Stores, &out.Docs, &out.Footprint)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		})
	if err != nil {
		return Usage{}, fmt.Errorf("artifactdata: read the company's datastore usage: %w", err)
	}
	out.Position, out.Complete = served.Position, served.Complete
	return out, nil
}

// StateEntry is one per-viewer state key and its value.
type StateEntry struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Version int64           `json:"version"`
}

// State answers every per-viewer state key one reader keeps in a store.
func (r *Reader) State(ctx context.Context, id artifacts.ID, rd Reading) ([]StateEntry, error) {
	out := []StateEntry{}
	if rd.Record == "" {
		return out, nil
	}
	_, err := r.read(ctx, id, rd, func(tx *sql.Tx, s storeRow, present bool) error {
		c, err := (&txView{ctx: ctx, tx: tx, store: s, present: present}).collection(StateCollection)
		if err != nil || !c.stored {
			return err
		}
		docs, err := scanDocs(ctx, tx, c, `
			SELECT doc_id, body, created_by, created_by_kind, created_at, updated_by,
			updated_by_kind, updated_at, version FROM artifact_data_docs
			WHERE store_no = ? AND collection_no = ? AND created_by = ? AND body IS NOT NULL
			ORDER BY doc_id`, s.no, c.no, rd.Record)
		if err != nil {
			return err
		}
		token := StateToken(rd.Record) + "_"
		for _, d := range docs {
			if !strings.HasPrefix(d.ID, token) {
				continue
			}
			value, err := stateValue(d.Fields)
			if err != nil {
				return err
			}
			out = append(out, StateEntry{Key: StateKey(d.ID), Value: value, Version: d.Version})
		}
		return nil
	})
	return out, err
}

// StateField is the one field a per-viewer state document keeps its value in.
const StateField = "value"

// stateValue is the value a state document's fields hold.
func stateValue(fields json.RawMessage) (json.RawMessage, error) {
	parsed, err := canon.Parse(fields, canon.MaxDepth)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: decode a state document: %w", err)
	}
	for _, m := range parsed.Members {
		if m.Key == StateField {
			return canon.Encode(m.Value), nil
		}
	}
	return json.RawMessage("null"), nil
}

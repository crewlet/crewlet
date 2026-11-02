package artifactdata

// THE FOOTPRINT METER (D42, ADR-0039): what a store's rows cost on disk, in
// the bytes `artifacts.limits.data_store_bytes` and `data_bytes` bound.
//
// Every datastore byte is held on every data node and rides every snapshot
// and backup, so a quota that counted less than the file holds would let a
// company store several times what its operator sized disks for. The first
// meter counted canonical bodies, 48 bytes per field-index entry and history
// at its payload, and spike DS-2 measured the file at 2.06 times that: a
// field-index entry is a row keyed by five columns and two more index entries
// (152 bytes on the spike's sheet), a document row costs more than its body,
// and history carries its row and index overhead.
//
// THE MODEL IS THE STORE'S OWN LAYOUT, not a ratio. A table row costs the
// share of a 4 KiB B-tree page its cell takes, with the page's whole slack
// charged to the rows that fit in it (one row of 2.1 KiB fills a page, which
// a byte count would read as half), or its overflow pages past the largest
// payload a page holds in place; an index entry costs its key and the row id,
// at the fill an index's random inserts leave (nine eighths). Each cost is a
// pure function of what the applier writes, so every data node meters every
// record identically, and the constants are the column overhead of the
// pinned driver's record format, calibrated against the DS-2 snapshot
// (TestTheFootprintMeterMatchesTheFile holds the meter within 10 percent of
// the file across row shapes).
//
// NOT METERED: a store's catalogue row and its collection and path rows, a
// few KiB that the collection and column bounds hold; and the file's own
// free pages, which a vacuum reclaims and a snapshot never carries.

// pageBytes is the store's page size: the driver's default, which nothing in
// the engine changes.
const pageBytes = 4096

// The B-tree's page arithmetic, as the file format defines it.
const (
	// leafHeader is a table leaf page's header.
	leafHeader = 8

	// maxLocal is the largest payload a table leaf cell holds in place;
	// past it the payload spills to overflow pages.
	maxLocal = pageBytes - 35

	// minLocal is what a spilled payload keeps in place at least.
	minLocal = (pageBytes-12)*32/255 - 23

	// overflowPayload is what one overflow page carries.
	overflowPayload = pageBytes - 4

	// cellOverhead is a table cell's bytes beyond its payload: the payload's
	// length and the row id as varints, and its two-byte pointer in the
	// page's cell array.
	cellOverhead = 7
)

// The fixed parts of each table's record: its header and its integer
// columns, as the record format encodes them for this engine's values
// (packed log positions and broker instants take eight bytes each).
const (
	// docRecordFixed is a document row's header and integers.
	docRecordFixed = 51

	// fieldEntryFixed is a field-index entry's row, its primary-key entry
	// and its numeric and text index entries, beyond the document id each
	// carries and the text two of them carry.
	fieldEntryFixed = 83

	// touchRecordFixed is a touch row's header and integers.
	touchRecordFixed = 21

	// historyRecordFixed is a history row's header, integers and the
	// thirty-two bytes of its digest.
	historyRecordFixed = 89
)

// tableRowBytes is what one table row of payload bytes costs: its share of
// the leaf page it sits in, slack included, and its overflow pages.
func tableRowBytes(payload int) int64 {
	if payload <= maxLocal {
		return leafSlot(payload + cellOverhead)
	}
	local := minLocal + (payload-minLocal)%overflowPayload
	if local > maxLocal {
		local = minLocal
	}
	overflow := (payload - local + overflowPayload - 1) / overflowPayload
	// THE CELL IN PLACE carries the first overflow page's number too.
	return int64(overflow)*pageBytes + leafSlot(local+4+cellOverhead)
}

// leafSlot is one cell's share of a leaf page: the page divided by the cells
// of that size it holds.
func leafSlot(cell int) int64 {
	per := (pageBytes - leafHeader) / cell
	if per < 1 {
		per = 1
	}
	return (pageBytes + int64(per) - 1) / int64(per)
}

// indexEntryBytes is one index entry of a key of key bytes: the key, the row
// id it points at and the cell's own overhead, at an index page's fill.
func indexEntryBytes(key int) int64 {
	return (int64(key+cellOverhead)*9 + 7) / 8
}

// docBytes is what one document row costs, live (body non-nil) or a
// tombstone, with its primary-key, catch-up and creator index entries; its
// field-index entries are [fieldEntryBytes] each.
func docBytes(id string, body []byte, createdBy, createdKind, updatedBy, updatedKind string) int64 {
	payload := docRecordFixed + len(id) + len(body) + len(createdBy) + len(createdKind) +
		len(updatedBy) + len(updatedKind)
	return tableRowBytes(payload) +
		indexEntryBytes(10+len(id)) + // (store_no, collection_no, doc_id)
		indexEntryBytes(17) + // (store_no, version)
		indexEntryBytes(10+len(createdBy)) // (store_no, collection_no, created_by)
}

// fieldEntryBytes is what one field-index entry of the document id costs: its
// row and its three index entries, the id in all four and a string's indexed
// prefix in the row and the text index.
func fieldEntryBytes(id string, e fieldEntry) int64 {
	txt := 0
	if e.kind == "s" {
		txt = len(e.txt)
	}
	return (int64(fieldEntryFixed+4*len(id)+2*txt)*9 + 7) / 8
}

// meteredDoc is what a document costs as written: its row and, live, its
// field-index entries.
func meteredDoc(d *docState, updatedBy, updatedKind string) int64 {
	var body []byte
	if d.live {
		body = d.body
	}
	n := docBytes(d.id, body, d.createdBy, d.createdByKind, updatedBy, updatedKind)
	if d.live {
		for _, e := range d.entries {
			n += fieldEntryBytes(d.id, e)
		}
	}
	return n
}

// touchBytes is what one touch row of the document id costs, with its
// primary-key and history-order index entries.
func touchBytes(id string) int64 {
	return tableRowBytes(touchRecordFixed+len(id)) +
		indexEntryBytes(18+len(id)) + // (store_no, collection_no, doc_id, seq)
		indexEntryBytes(16) // (store_no, seq)
}

// historyRowBytes is what one history row costs: its row, of the record's
// strings and its changes, and its four index entries (its key, its
// operation id, its prune order and its actor).
func historyRowBytes(strs int, opID, actor string) int64 {
	return tableRowBytes(historyRecordFixed+strs) +
		indexEntryBytes(16) + // (store_no, seq)
		indexEntryBytes(len(opID)) + // (op_id)
		indexEntryBytes(24) + // (store_no, at, seq)
		indexEntryBytes(16+len(actor)) // (store_no, actor, seq)
}

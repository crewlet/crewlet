package statelog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// VersionedField is one record field that did not exist in a domain's base
// record format, and the record version a build must read before it may apply
// a record carrying it. A record is stamped with the highest Since among the
// fields it carries, never the build's version (package doc, "A record is
// stamped with the lowest version that reads it").
//
// A field is carried when its key is present with any value but null, not
// when it is non-zero: a written zero is a value an older build would drop
// like any other. A field whose zero should not stamp declares `omitempty`
// (or `omitzero`), so the struct decides presence.
type VersionedField struct {
	// Name is how the field is named to a person — "TurnSpend.Workers" — for
	// a failure message and nothing else.
	Name string

	// Since is the record version that introduced the field. At least 2:
	// version 1 is the base format and needs no row.
	Since int

	// Op narrows the rule to records of one wire op; EMPTY means every op.
	// It tells apart two payloads sharing a key (a task patch and a turn's
	// spend both carrying `workers`) without a path knowing the payload type.
	Op string

	// Path is the JSON keys from the record's root to the field. An array
	// met on the way is searched element by element.
	Path []string

	// Equals narrows the rule to one VALUE at Path, compared as a JSON
	// string; EMPTY means any value but null. It is how a new KIND or OP is
	// declared (`subject.kind` equal to "file"): the key alone has been
	// carried since the base format and would stamp every record, while
	// key and value make an older build retain the records it has no
	// applier for rather than fault at the end of its dispatch.
	Equals string

	// Gate marks a field a GATE may carry: a change to what a gate's APPLY
	// does. Every other field is refused on a gate, because an older build
	// HALTS at a gate it cannot read (deferring it would license every record
	// above it). A gate whose apply changed can neither be applied the old
	// way without diverging rows nor deferred, so halting older nodes is the
	// one honest answer. Chosen per field in the table, never by a writer.
	Gate bool
}

// RecordFields is a domain's table of every field its records have gained
// since the base format: one per domain, consulted by its encoder and
// certified by [internal/statelog/statelogtest]. A field missing here is a
// silent divergence; a row no build reads is a version nothing can apply.
type RecordFields []VersionedField

// Minimum is the lowest record version a record may be stamped at, given its
// op and its encoded JSON: 1 when it carries no versioned field. A record
// that is not a JSON object is refused: the caller encoded the wrong value.
func (f RecordFields) Minimum(op string, record []byte) (int, error) {
	minimum := 1
	if len(f) == 0 {
		return minimum, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(record, &root); err != nil {
		return 0, fmt.Errorf("statelog: read a record for its versioned fields: %w", err)
	}
	for _, field := range f {
		if field.Since <= minimum || (field.Op != "" && field.Op != op) {
			continue
		}
		if field.carriedBy(root) {
			minimum = field.Since
		}
	}
	return minimum, nil
}

// Ungated names the versioned fields a record carries that put a GATE above
// what its own apply needs: every carried field not marked
// [VersionedField.Gate] whose version is above the highest version among the
// gate fields the record carries (1 where it carries none).
//
// The gate fields decide the halt: a field at or below them halts no node the
// gate does not already halt. Only a field ABOVE them takes an older node out
// of service for a field rather than a rule, which the encoder refuses. A
// barrier and a generation carry no gate field, so every field they carry is
// named.
func (f RecordFields) Ungated(op string, record []byte) []string {
	var root map[string]json.RawMessage
	if len(f) == 0 || json.Unmarshal(record, &root) != nil {
		return nil
	}
	gate := 1
	for _, field := range f {
		if field.Gate && (field.Op == "" || field.Op == op) && field.carriedBy(root) {
			gate = max(gate, field.Since)
		}
	}
	var out []string
	for _, field := range f {
		if !field.Gate && field.Since > gate && (field.Op == "" || field.Op == op) &&
			field.carriedBy(root) {
			out = append(out, fmt.Sprintf("%s (version %d)", field.Name, field.Since))
		}
	}
	return out
}

// Carried names the versioned fields a record carries, for a refusal that has
// to say which field put a record above the version it was stamped at.
func (f RecordFields) Carried(op string, record []byte) []string {
	var root map[string]json.RawMessage
	if len(f) == 0 || json.Unmarshal(record, &root) != nil {
		return nil
	}
	var out []string
	for _, field := range f {
		if (field.Op == "" || field.Op == op) && field.carriedBy(root) {
			out = append(out, fmt.Sprintf("%s (version %d)", field.Name, field.Since))
		}
	}
	return out
}

// Check refuses a table a build reading version reads cannot honour:
//
//   - Every field's version is one this build READS, or it retains its own
//     writes.
//   - The build reads EXACTLY the table's highest version, or 1 with no
//     table. Claiming a version no field introduced, it would apply a newer
//     peer's record at that version without the field it was minted for.
func (f RecordFields) Check(reads int) error {
	highest := 1
	names := map[string]bool{}
	for _, field := range f {
		switch {
		case field.Name == "":
			return fmt.Errorf("statelog: a versioned field names nothing — the " +
				"name is what a refused record reports")
		case names[field.Name]:
			return fmt.Errorf("statelog: the versioned field %s is listed twice", field.Name)
		case field.Since < 2:
			return fmt.Errorf("statelog: the versioned field %s is at version %d — "+
				"version 1 is the base format, so a field there needs no row and "+
				"one below it is not a version", field.Name, field.Since)
		case len(field.Path) == 0 || slices.Contains(field.Path, ""):
			return fmt.Errorf("statelog: the versioned field %s has no path to "+
				"the key it is carried under", field.Name)
		case field.Since > reads:
			return fmt.Errorf("statelog: the versioned field %s is at version %d "+
				"and this build reads %d — its own records would be retained by "+
				"the build that wrote them", field.Name, field.Since, reads)
		}
		names[field.Name] = true
		highest = max(highest, field.Since)
	}
	if reads != highest {
		return fmt.Errorf("statelog: this build reads record version %d and the "+
			"highest version any of its fields needs is %d — a build that claims "+
			"a version no field introduced applies a newer peer's record at that "+
			"version without the field it was minted for", reads, highest)
	}
	return nil
}

// carriedBy reports whether a record's decoded root carries the field: a
// non-null value at its path — any element's, where the path crosses an
// array — equal to [VersionedField.Equals] where it names one.
func (field VersionedField) carriedBy(root map[string]json.RawMessage) bool {
	return anyAt(root, field.Path, func(value json.RawMessage) bool {
		if field.Equals == "" {
			return true
		}
		var s string
		return json.Unmarshal(value, &s) == nil && s == field.Equals
	})
}

// anyAt walks path from node and reports whether a non-null value at its end
// satisfies match. An array met on the way is searched element by element.
func anyAt(node map[string]json.RawMessage, path []string, match func(json.RawMessage) bool) bool {
	value, ok := node[path[0]]
	if !ok || isNull(value) {
		return false
	}
	if len(path) == 1 {
		return match(value)
	}
	return anyIn(value, path[1:], match)
}

// anyIn descends one value, searching an array element by element.
func anyIn(value json.RawMessage, rest []string, match func(json.RawMessage) bool) bool {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '{':
		var child map[string]json.RawMessage
		if json.Unmarshal(trimmed, &child) != nil {
			return false
		}
		return anyAt(child, rest, match)
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil {
			return false
		}
		for _, item := range items {
			if anyIn(item, rest, match) {
				return true
			}
		}
	}
	return false
}

func isNull(value json.RawMessage) bool {
	return strings.TrimSpace(string(value)) == "null"
}

// ReadableBy reports whether a build reading record version reads may decode
// and apply this record: the ONE statement of the retain rule's comparison, so
// the apply loop and the conformance suite cannot disagree about the boundary.
func (e Envelope) ReadableBy(reads int) bool { return e.V <= reads }

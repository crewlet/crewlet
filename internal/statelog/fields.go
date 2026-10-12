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
// a record carrying it.
//
// # Why a record is stamped with the MINIMUM version, never the build's
//
// A record above a build's [Domain.RecordVersion] is RETAINED rather than
// applied, and so is everything whose scope meets it — which is the whole
// safety of a rolling upgrade: an older node holds a record it would apply
// lossily instead of applying it. That makes the stamp a statement about the
// RECORD, and there are two ways to get it wrong:
//
//   - Stamped BELOW what its fields need, an older node reads the version, finds
//     it readable, decodes the record, drops the field it has no home for and
//     applies what is left. Its rows then differ from every upgraded node's for
//     that object, permanently, on tables a fleet compares byte for byte —
//     and nothing reports it, because every node applied "the same" record.
//   - Stamped at the BUILD's version, every record the new build writes is
//     unreadable to an older node the moment the build's version moves — the
//     ones that carry nothing new included. The older half of the fleet then
//     retains every write in the company rather than the few that need a
//     newer reader, and every scope any of them touches refuses reads for the
//     length of the upgrade.
//
// So the writer computes the lowest version that is still honest: the highest
// [VersionedField.Since] among the fields the record actually carries, or 1
// when it carries none. A record that does not use a new field stays readable
// by every build there is, and a record that does is held back only on the
// nodes that would have lost it.
//
// # Presence is what a field is carried BY
//
// A field counts as carried when its key is present in the record's JSON with
// any value but null. Not "non-zero": a new field whose zero value is written
// out is a zero the writer meant, and an older build would drop it exactly as
// it drops any other value. A field that should not stamp its zero declares
// `omitempty` (or `omitzero`), which is the Go struct deciding presence rather
// than a second rule here.
type VersionedField struct {
	// Name is how the field is named to a person — the Go type and field,
	// "TurnSpend.Workers" — for a failure message and nothing else.
	Name string

	// Since is the record version that introduced the field: the lowest
	// version a build must read to apply a record carrying it. At least 2,
	// because version 1 is the base format and needs no row.
	Since int

	// Op narrows the rule to records of one operation, spelled as the
	// domain's own wire op. EMPTY means every op. It is what lets two
	// payload shapes that happen to share a key — a task patch and a turn's
	// spend both carrying some `workers` — be told apart without a path
	// that knows which payload type sits under `mutation`.
	Op string

	// Path is the JSON keys from the record's root to the field. An array
	// met on the way is searched element by element, so a field inside a
	// list of objects is carried when any element carries it.
	Path []string

	// Equals narrows the rule to one VALUE at Path, compared as the JSON
	// string it is written as. EMPTY means any value but null.
	//
	// It is what a new KIND or a new OP is: a value of a field every record
	// has carried since the base format, so a row naming only the key would
	// stamp every record the domain writes. Its row names the key and the
	// value instead — a file kind is `subject.kind` equal to "file" — and
	// an older build retains the records of the kind it has no applier for
	// rather than faulting at the end of its dispatch.
	Equals string

	// Gate marks a field a GATE may carry: a change to what a gate's APPLY
	// does, which no older build can honour.
	//
	// Every other field is refused on a gate (each domain's encoder says
	// why), because the one thing an older build does with a gate it cannot
	// read is HALT at it — a deferred gate would license every record above
	// it — and a halt takes that node out of service until it upgrades. A
	// field marked here is the deliberate exception: a gate whose apply
	// changed cannot be applied the old way without leaving that node's rows
	// different from every newer node's for good, and cannot be deferred, so
	// halting an older node is the one honest answer left. It is chosen per
	// field, in the table a reviewer reads, never by a writer.
	Gate bool
}

// RecordFields is a domain's table of every field its records have gained
// since the base format.
//
// ONE TABLE PER DOMAIN, consulted by that domain's encoder and certified by
// [internal/statelog/statelogtest]: a field added to a record without a row
// here is the silent divergence the type exists to prevent, and a row here
// that no build reads is a version nothing can apply.
type RecordFields []VersionedField

// Minimum is the lowest record version a record may be stamped at, given its
// op and its encoded JSON.
//
// One when it carries no versioned field. It refuses a record that is not a
// JSON object, because every domain's record is one and a caller that handed
// something else has encoded the wrong value.
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
// THE GATE FIELDS DECIDE THE HALT, and anything at or below them halts nobody
// they do not already halt: a build that cannot read such a field cannot read
// the gate either, and stops there whether the field is carried or not. Only a
// field ABOVE them takes an older node out of service that the gate's own rule
// did not — for a field rather than for a rule it cannot honour — which is
// what the encoder refuses. A barrier and a generation carry no gate field, so
// every versioned field they carry is named here.
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

// Check refuses a table a build reading version reads cannot honour.
//
// Two rules close the two ways the table and the build's version drift apart:
//
//   - Every field's version is one this build READS. A build that stamps a
//     record at a version above its own retains its own writes.
//   - The build reads EXACTLY the highest version its table knows, or 1 with
//     no table. A build claiming to read a version no field introduced would
//     accept a newer peer's record at that version and apply it without the
//     field that version was minted for — the lossy apply the stamp exists to
//     prevent, arriving from the other side.
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
// and apply this record. The ONE statement of the retain rule's comparison, so
// the apply loop and a conformance case asking "would an older build hold
// this back?" cannot disagree about which side of the boundary a version sits.
func (e Envelope) ReadableBy(reads int) bool { return e.V <= reads }

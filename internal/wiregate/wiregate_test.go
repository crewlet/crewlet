package wiregate

import (
	"fmt"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Marker kinds, one per kind of field: each claims that leaving the field out
// and sending its zero say the same thing to every reader.
const (
	absentIsFalse = "absent-is-false"
	absentIsZero  = "absent-is-zero"
)

// markerKind is the marker a field of kind may carry.
var markerKind = map[string]string{"flag": absentIsFalse, "number": absentIsZero}

// EVERY FLAG AND NUMBER A CLIENT RECEIVES IS SENT, false and 0 included,
// unless its field says why leaving it out means the same thing: encoding/json
// drops a zero under `omitempty` or `omitzero`, and a reader that is not Go
// (the dashboard, a script reading the CLI) cannot tell a dropped zero from a
// key nobody sent, so it picks a default, and two readers pick two. What the
// tree held when the gate arrived is omittedInventory, which only shrinks.
// Mutation: put `omitempty` back on worklet.Blocker's `open` and this fails.
func TestEveryFlagAndNumberAClientReadsIsSent(t *testing.T) {
	t.Parallel()
	w, err := loaded()
	if err != nil {
		t.Fatalf("reading the wire: %v", err)
	}
	found, refused := w.judge(newMarkers())
	for _, problem := range refused {
		t.Error(problem)
	}
	for _, f := range found {
		if !slices.Contains(omittedInventory[f.pkg], f.entry) {
			t.Error(f.message)
		}
	}
	for _, pkg := range slices.Sorted(maps.Keys(omittedInventory)) {
		for _, entry := range omittedInventory[pkg] {
			if !slices.ContainsFunc(found, func(f finding) bool { return f.pkg == pkg && f.entry == entry }) {
				t.Errorf("omittedInventory lists %s %s, which is sent always or carries a marker "+
					"now (or left the wire): remove it from inventory_test.go", pkg, entry)
			}
		}
	}
	// A WALK THAT READ NOTHING PASSES: a root that stopped resolving, or
	// a surface pattern that matches no package, judges nothing at all.
	judged := 0
	for _, f := range w.fields {
		if f.kind() != "" {
			judged++
		}
	}
	t.Logf("judged %d flags and numbers on %d fields reached from %d roots; %d omitted", judged,
		len(w.fields), len(w.roots), len(found))
	if len(w.roots) < 300 || judged < 1500 {
		t.Fatalf("judged %d flags and numbers from %d roots: the walk is not reading the surfaces",
			judged, len(w.roots))
	}
}

// finding is one omitted flag or number no marker excuses: its package
// within the module, its entry as omittedInventory spells it (struct and
// key), and what the gate says of it.
type finding struct {
	pkg, entry, message string
}

// judge returns every omitted flag and number on the wire that no marker
// excuses, and, apart, every marker on one that is malformed.
func (w *wire) judge(m *markers) ([]finding, []string) {
	var found []finding
	var refused []string
	for _, f := range w.fields {
		kind := f.kind()
		if kind == "" || !f.omitted() || w.document(f) != "" {
			continue
		}
		ms, err := m.of(w.fset.Position(f.field.Pos()), f.field.Name())
		if err != nil {
			refused = append(refused, err.Error())
			continue
		}
		want := markerKind[kind]
		switch {
		case len(ms) == 0:
			pkg, typ, _ := strings.Cut(f.owner, ".")
			found = append(found, finding{pkg: pkg, entry: typ + "." + f.name, message: fmt.Sprintf(
				"%s: %s.%s is a %s sent with `%s`, so a client never sees its zero and reads a "+
					"missing key however it likes (reached from %s). Send it always (drop the "+
					"option, and make the dashboard's member required), or, if a missing key and "+
					"its zero mean the same to every reader, say why after it: `%s%s <reason>`",
				w.at(f), f.owner, f.name, kind, f.options, f.via, markerPrefix, want)})
		case len(ms) > 1:
			refused = append(refused, fmt.Sprintf("%s: %s.%s carries %d wiregate markers; one says it",
				w.at(f), f.owner, f.name, len(ms)))
		case ms[0].kind != want:
			refused = append(refused, fmt.Sprintf("%s: %s.%s is a %s, and its marker says %s: a %s's is %s%s",
				w.at(f), f.owner, f.name, kind, ms[0].kind, kind, markerPrefix, want))
		case ms[0].reason == "":
			refused = append(refused, fmt.Sprintf("%s: %s.%s's marker gives no reason: say why a "+
				"missing key and its zero mean the same to every reader", w.at(f), f.owner, f.name))
		}
	}
	return found, refused
}

// EVERY MARKER EXCUSES A FIELD THE GATE JUDGES: one on a field a client is
// always sent, or one no surface sends, or one in no field at all, is a claim
// nobody checks, and the next reader trusts it.
func TestEveryMarkerIsOnAnOmittedFieldOnTheWire(t *testing.T) {
	t.Parallel()
	w, err := loaded()
	if err != nil {
		t.Fatalf("reading the wire: %v", err)
	}
	for _, problem := range w.strayMarkers(w.root) {
		t.Error(problem)
	}
}

// strayMarkers is every marker under dir that excuses no omitted field on
// the wire, or names no kind there is.
func (w *wire) strayMarkers(dir string) []string {
	found, names, err := everyMarker(dir)
	if err != nil {
		return []string{fmt.Sprintf("reading the markers: %v", err)}
	}
	judged := map[token.Position]bool{}
	for _, f := range w.fields {
		if f.kind() == "" || !f.omitted() || w.document(f) != "" {
			continue
		}
		at := w.fset.Position(f.field.Pos())
		at.Column, at.Offset = 0, 0
		judged[at] = true
	}
	var out []string
	for _, at := range sortedPositions(found) {
		where := relative(at, w.root)
		switch {
		case names[at] == "" && !judged[at]:
			out = append(out, fmt.Sprintf("%s: a wiregate marker on no field; it goes on the "+
				"field it excuses", where))
		case !judged[at]:
			out = append(out, fmt.Sprintf("%s: %s carries a wiregate marker, and it is not a "+
				"flag or number a client is sent with `omitempty` or `omitzero` (or its struct "+
				"is a document): remove the marker", where, names[at]))
		}
		for _, mk := range found[at] {
			if !slices.Contains(slices.Collect(maps.Values(markerKind)), mk.kind) {
				out = append(out, fmt.Sprintf("%s: a wiregate marker of kind %q; the kinds are %s "+
					"and %s", where, mk.kind, absentIsFalse, absentIsZero))
			}
		}
	}
	return out
}

// EVERY OPAQUE FIELD ON THE WIRE SAYS WHAT IT CARRIES: an interface or raw
// JSON hides its struct from the walk, so each one names the types whose
// values it holds (walked as roots) or says why it holds none of this
// module's, and a row for a field the wire no longer has is removed.
func TestEveryOpaqueFieldSaysWhatItCarries(t *testing.T) {
	t.Parallel()
	w, err := loaded()
	if err != nil {
		t.Fatalf("reading the wire: %v", err)
	}
	reached := map[string]bool{}
	for _, f := range w.opaque {
		key := f.key()
		reached[key] = true
		if _, ok := opaqueFields[key]; !ok {
			t.Errorf("%s: %s is %s, so the walk cannot see what a client receives in it: "+
				"add its row to opaqueFields, naming the types it carries or why none of "+
				"this module's", w.at(f), key, f.field.Type())
		}
	}
	for _, key := range slices.Sorted(maps.Keys(opaqueFields)) {
		row := opaqueFields[key]
		switch {
		case !reached[key]:
			t.Errorf("opaqueFields has a row for %s, which no client receives: remove it", key)
		case len(row.types) == 0 && strings.TrimSpace(row.reason) == "":
			t.Errorf("opaqueFields' row for %s names no type and gives no reason", key)
		case len(row.types) > 0 && row.reason != "":
			t.Errorf("opaqueFields' row for %s names types and a reason; it is one or the other", key)
		}
	}
}

// document is the document f's struct is part of, or "" for an answer.
func (w *wire) document(f wireField) string { return w.documents[f.owner] }

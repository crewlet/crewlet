// Package kept is what a record keeps of a payload too large to keep whole:
// the bytes a value weighs as the record holds it, and the one rule for
// leaving part of it out so that what is left reads as left out.
//
// # Bytes as the record holds them, never as the payload was sent
//
// A record that is ONE JSON VALUE in a store held to a byte ceiling — a
// detached run's row or its call log in the coordination store, each one broker
// message of at most queue.MaxPayloadBytes — is bounded in the bytes its
// encoder writes, and the encoder escapes: `<`, `>`, `&`, U+2028 and U+2029, an
// invalid byte and a control byte cost six bytes each, a quote or a backslash
// two. A bound measured on what was sent is therefore off by up to six-fold,
// and a tool's arguments, which a record holds as JSON TEXT inside a JSON
// string, are escaped twice. [Len] is the escape encoding/json writes (HTML
// escaping on, as json.Marshal leaves it), so a cut here is measured rather
// than estimated, and a test holds it to the encoder byte for byte.
//
// # A cut that reads as one
//
// What is not kept is SAID, in the text itself, for a reader that reads only
// the text — a model handed a call's arguments, a person reading them on a
// screen — and beside it in a size the caller records, for a reader that
// decides by it. Two shapes, by what the payload is:
//
//   - TEXT (a tool's output, a title) keeps its opening, cut on a rune
//     boundary, and ends with how much more of how much was not kept ([Text]).
//     Its opening is what a person reads it by, and the marker says it is an
//     opening. A reader that compares another copy of the text with the
//     record reads the opening back off it exactly ([Record.Opening]), so it
//     compares all the record kept, and nothing the marker added.
//   - A TOOL'S ARGUMENTS keep their shape: a VALUE past what the record holds
//     is replaced by a marker naming its size and four bytes of its digest
//     ([Args]), the way internal/compact's Omitted names a payload it could not
//     carry, so two left-out values are still told apart. LEAVES go first,
//     largest first: an identifier is short and stays whole beside the body
//     that is left out, which is what a reader of the call needs to know where
//     it went.
//
// Never a fragment that reads as the whole, which is the cut internal/compact
// and internal/textcut were written to remove: a value is left out whole or
// kept whole, and an opening is always followed by its marker.
//
// A reader that COMPARES two calls' arguments, rather than reading them,
// recognises the marker ([IsLeftOut], [IsNotKept]) and reads a value left out
// as one that may have been anything: compared as the text it is, the marker
// would read two calls apart whose left-out values may have been one.
//
// # A leaf
//
// The writer (internal/sandbox, a run's row and its log) and the condensation
// of a parked turn (internal/agent/execstate) sit at heights neither can import
// the other from, and both state the same rule; so it lives here, importing the
// standard library and internal/plainjson, which writes a call's arguments as
// the engine records them. A test holds that.
package kept

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// Record names the record a marker says a payload is not kept on: "the run's
// record", "the turn's record". The caller's, because only it knows which
// record a reader will look in.
type Record string

// Len is the bytes s takes inside a JSON string as encoding/json writes it,
// HTML escaping on, quotes not counted: exact for valid UTF-8, never less for
// invalid bytes. THE ENCODER'S OWN RULE, mirrored rune for rune: `"` and `\`
// and the five control bytes with a short escape (`\b`, `\f`, `\n`, `\r`,
// `\t`) are two bytes; every other control byte, `<`, `>`, `&`, U+2028 and
// U+2029 are six (`\u003c`); every other rune is its UTF-8 length.
//
// AN INVALID BYTE IS COUNTED SIX, the most the encoder writes for one (six
// bytes for some, U+FFFD's three for others, by context): a bound needs a rule
// never short, so such a payload is measured a few bytes long. A test holds
// both halves to json.Marshal.
func Len(s string) int {
	n := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			n += asciiLen(b)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			n += 6
		case r == '\u2028' || r == '\u2029':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}

// asciiLen is one ASCII byte's escaped length — see [Len].
func asciiLen(b byte) int {
	switch {
	case b == '"' || b == '\\':
		return 2
	case b == '\b' || b == '\f' || b == '\n' || b == '\r' || b == '\t':
		return 2
	case b < 0x20 || b == '<' || b == '>' || b == '&':
		return 6
	}
	return 1
}

// Quoted is what s weighs as a JSON string value: [Len] and its two quotes.
func Quoted(s string) int { return Len(s) + 2 }

// Size is n bytes as a marker states it: bytes under a KiB, whole KiB under a
// MiB, and MiB to one place.
func Size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// SizeUp is n bytes in [Size]'s units, ROUNDED UP: the figure a refusal states
// for what is past a bound, beside the bound in [Size]'s.
//
// UP, because [Size] rounds whole KiB down, and a value one byte past a bound
// of whole KiB then reads as the bound itself — "16 KiB, and one may be at
// most 16 KiB" — which tells its reader nothing was wrong. Up, a value past a
// bound always reads past it, by at most a unit, and a value within one never
// reads past it. Bytes under a KiB are exact, as [Size] states them; a figure
// that rounds up to a whole MiB is stated in MiB, so no figure reads 1024 KiB.
func SizeUp(n int) string {
	switch {
	case n > 1<<20-1<<10:
		return fmt.Sprintf("%.1f MiB", float64((n*10+1<<20-1)>>20)/10)
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", (n+1<<10-1)>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// LeftOut is the marker a value of a call's arguments is replaced by when the
// record does not keep it: its size as written (raw, its JSON text) and four
// bytes of that text's SHA-256, so two left-out values are told apart.
func (r Record) LeftOut(raw []byte) string {
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("(%s not kept on %s%s%s)", Size(len(raw)), r, markerDigestAt,
		hex.EncodeToString(sum[:markerDigestBytes]))
}

// markerDigestBytes is how much of a left-out value's SHA-256 its marker
// names ([Record.LeftOut]): four bytes, eight hex digits, enough to tell apart
// the few values one call's arguments leave out.
const markerDigestBytes = 4

// markerDigestAt is what a marker writes before its digest ([Record.LeftOut]).
const markerDigestAt = ", digest "

// IsLeftOut reports whether v is the marker a record leaves in place of a
// value it did not keep ([Record.LeftOut]), whatever the record: a string that
// is exactly "(", a size as [Size] states it, " not kept on ", a record's
// name, ", digest ", eight lowercase hex digits and ")". A reader comparing
// two calls' arguments reads it as a value that may have been anything; a
// model-written value in that shape reads so too, the safe side.
//
// ANY RECORD'S NAME, never a list: a name is its caller's, and a marker under
// a missed name would be compared as text, reading apart two calls whose
// left-out values may have been one. Size and digest are held to what [Size]
// and LeftOut write, so a model's parenthesis is not taken for a marker.
func IsLeftOut(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	inner, ok := strings.CutPrefix(s, "(")
	if !ok {
		return false
	}
	inner, ok = strings.CutSuffix(inner, ")")
	if !ok {
		return false
	}
	// THE LAST SEPARATOR, since a record's name may hold one.
	at := strings.LastIndex(inner, markerDigestAt)
	if at < 0 || !isMarkerDigest(inner[at+len(markerDigestAt):]) {
		return false
	}
	size, _, ok := strings.Cut(inner[:at], " not kept on ")
	return ok && isSize(size)
}

// IsNotKept reports whether args are what [Record.Args] makes of arguments it
// kept nothing of but what they were: one key, [NotKeptKey], holding a string
// (the whole's marker, or the one notKept spells by hand). A reader comparing
// two calls' arguments reads such a call's as ones that may have been
// anything, as it reads a value [IsLeftOut].
func IsNotKept(args map[string]any) bool {
	if len(args) != 1 {
		return false
	}
	_, ok := args[NotKeptKey].(string)
	return ok
}

// isMarkerDigest reports whether s is the digest a marker names: eight
// lowercase hex digits, as hex.EncodeToString writes [markerDigestBytes].
func isMarkerDigest(s string) bool {
	if len(s) != 2*markerDigestBytes {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isSize reports whether s is a size as [Size] states one: a whole number of
// bytes under a KiB, a whole number of KiB from one to under a MiB, or MiB to
// one place from one up, each figure with no leading zero.
func isSize(s string) bool {
	figure, unit, ok := strings.Cut(s, " ")
	if !ok {
		return false
	}
	switch unit {
	case "bytes":
		n, ok := smallWhole(figure)
		return ok && n < 1<<10
	case "KiB":
		n, ok := smallWhole(figure)
		return ok && n >= 1 && n < 1<<10
	case "MiB":
		whole, tenth, ok := strings.Cut(figure, ".")
		return ok && whole != "0" && isWhole(whole) && len(tenth) == 1 && isWhole(tenth)
	}
	return false
}

// smallWhole is figure as a whole number where it is one of at most four
// digits with no leading zero: every figure [Size] writes in bytes or KiB.
func smallWhole(figure string) (int, bool) {
	if len(figure) > 4 || !isWhole(figure) {
		return 0, false
	}
	n, err := strconv.Atoi(figure)
	return n, err == nil
}

// isWhole reports whether figure is a whole number as %d writes one: digits,
// at least one, with no leading zero unless it is "0".
func isWhole(figure string) bool {
	if figure == "" || (len(figure) > 1 && figure[0] == '0') {
		return false
	}
	for i := range len(figure) {
		if figure[i] < '0' || figure[i] > '9' {
			return false
		}
	}
	return true
}

// NotKeptKey is the one key of what a call's arguments become when not even
// their shape fits what the record keeps ([Record.Args]): an object holding
// the marker of the whole.
const NotKeptKey = "not_kept"

// sizeWidth is the widest [Size] spells a figure a record can hold: "1023
// bytes", "1023.9 MiB" — ten bytes, which a marker's room is measured with
// before the figure it will state is known.
const sizeWidth = 10

// Text is s within budget bytes as a JSON string value (its quotes counted):
// s where it fits, and otherwise its opening, cut on a rune boundary, ended by
// a marker saying how much more of how much is not kept on r. whole is the
// size of s where it was cut and zero where s is kept whole — the size a
// record states beside the text, so a reader that decides by it never has to
// parse the marker.
//
// A budget too small for the marker keeps the marker alone, as
// internal/textcut.Within keeps its own: something was cut, and an empty
// value would read as one that was empty.
func (r Record) Text(s string, budget int) (kept string, whole int) {
	if Quoted(s) <= budget {
		return s, 0
	}
	whole = len(s)
	// THE MARKER'S ROOM AT ITS WIDEST, since the figure it states (what is
	// left out) is known only once the opening is: the opening is measured
	// against this, and the marker then written is never longer.
	room := budget - 2 - Len(textMarker(r, strings.Repeat("0", sizeWidth), Size(whole)))
	cut := 0
	used := 0
	for cut < len(s) {
		step, cost := runeCost(s, cut)
		if used+cost > room {
			break
		}
		used += cost
		cut += step
	}
	opening := s[:cut]
	return opening + textMarker(r, Size(whole-cut), Size(whole)), whole
}

// Opening is the opening [Record.Text] kept of a text of whole bytes that it
// cut, read back off kept, the value it wrote; ok is false where kept is not
// such a cut, a text Text kept whole (whole zero) included. Exact: the
// marker's last opening `…\n(` is the cut, since the figure after it holds no
// parenthesis, and the cut is checked by writing the marker again for it.
func (r Record) Opening(kept string, whole int) (opening string, ok bool) {
	rest, ok := strings.CutSuffix(kept, " of "+Size(whole)+" not kept on "+string(r)+")")
	if whole <= 0 || !ok {
		return "", false
	}
	at := strings.LastIndex(rest, textMarkerOpens)
	if at < 0 || at >= whole || rest[at+len(textMarkerOpens):] != Size(whole-at) {
		return "", false
	}
	return rest[:at], true
}

// textMarkerOpens is how the marker that ends a cut text opens ([textMarker]).
const textMarkerOpens = "…\n("

// textMarker is what ends a cut text: how much more of how much is not kept.
func textMarker(r Record, left, whole string) string {
	return textMarkerOpens + left + " of " + whole + " not kept on " + string(r) + ")"
}

// runeCost is the bytes of the rune at s[i] and what it costs escaped.
func runeCost(s string, i int) (step, cost int) {
	if b := s[i]; b < utf8.RuneSelf {
		return 1, asciiLen(b)
	}
	r, size := utf8.DecodeRuneInString(s[i:])
	switch {
	case r == utf8.RuneError && size == 1:
		return 1, 6
	case r == '\u2028' || r == '\u2029':
		return size, 6
	}
	return size, size
}

// Args is a call's arguments (the JSON text internal/tools.RecordArgs writes:
// one object, keys sorted, `<` `>` `&` as themselves) within budget bytes as a
// JSON string value: text where it fits, else the same object with the values
// the record cannot keep left out ([Record.LeftOut]), and whole the size of
// text. Zero whole is the arguments kept whole, as given or as the engine
// spells them where only that fits. LEAVES FIRST, LARGEST FIRST, which keeps
// every short value (ids, keys, paths, names). Only once every leaf larger
// than its marker is out and the text still does not fit are containers left
// out, largest first; past that the whole object becomes {"not_kept": marker},
// as does text that is not one JSON object. A value no larger than its marker
// is never left out. Deterministic: ties break in the text's order.
func (r Record) Args(text string, budget int) (kept string, whole int) {
	if Quoted(text) <= budget {
		return text, 0
	}
	root, ok := decodeObject(text)
	if !ok {
		return r.notKept(text), len(text)
	}
	// MEASURED AS THE ENGINE SPELLS THEM: the values below are weighed by the
	// encoder that writes them back (internal/plainjson), so the total they are
	// taken out of is that encoder's too. A text spelt otherwise (`<` written
	// `\u003c`, as json.Marshal writes it) that fits once spelt so is the same
	// arguments, kept whole.
	canonical, err := plainjson.Marshal(root, "")
	if err != nil {
		return r.notKept(text), len(text)
	}
	total := Quoted(string(canonical))
	if total <= budget {
		return string(canonical), 0
	}
	whole = len(text)
	for _, containersPass := range []bool{false, true} {
		if total <= budget {
			break
		}
		total = r.leaveOut(root, containersPass, total, budget)
	}
	out, err := plainjson.Marshal(root, "")
	if err != nil || Quoted(string(out)) > budget {
		return r.notKept(text), whole
	}
	return string(out), whole
}

// leaveOut is one pass of [Record.Args] over the tree under root — its leaves,
// or its containers — leaving out the largest values first until total, what
// the text weighs on the record, is within budget; it answers the new total.
//
// A CONTAINER LEFT OUT TAKES EVERYTHING UNDER IT WITH IT, so a value listed
// beneath one already left out is passed over: replacing it would change a
// subtree nothing holds any more, and count its saving against a text it is no
// longer in.
func (r Record) leaveOut(root map[string]any, containersPass bool, total, budget int) int {
	var slots []slot
	walk(root, nil, func(s slot) {
		if s.container == containersPass {
			s.cost = encodedCost(s.get())
			slots = append(slots, s)
		}
	})
	// LARGEST FIRST, the text's own order breaking a tie: a stable sort over
	// slots listed in that order.
	slices.SortStableFunc(slots, func(a, b slot) int { return cmp.Compare(b.cost, a.cost) })
	var gone [][]string
	for _, s := range slots {
		if total <= budget {
			break
		}
		if slices.ContainsFunc(gone, func(prefix []string) bool { return under(s.path, prefix) }) {
			continue
		}
		value := s.get()
		raw, err := plainjson.Marshal(value, "")
		if err != nil {
			continue
		}
		marker := r.LeftOut(raw)
		saved := Len(string(raw)) - encodedCost(marker)
		if saved <= 0 {
			continue
		}
		s.set(marker)
		total -= saved
		if s.container {
			gone = append(gone, s.path)
		}
	}
	return total
}

// under reports whether path is prefix or a place below it.
func under(path, prefix []string) bool {
	return len(path) >= len(prefix) && slices.Equal(path[:len(prefix)], prefix)
}

// notKept is arguments that keep nothing but what they were: one object
// holding the marker of the whole text.
func (r Record) notKept(text string) string {
	out, err := plainjson.Marshal(map[string]any{NotKeptKey: r.LeftOut([]byte(text))}, "")
	if err != nil {
		// A map of one string always encodes; the fallback is spelled by
		// hand so a record is never left holding the whole after all.
		return `{"` + NotKeptKey + `":"(not kept)"}`
	}
	return string(out)
}

// encodedCost is what v weighs inside a recorded argument text as the record
// holds it: its JSON text, escaped once more ([Len]). Escaping is per
// character, so a value's cost is the same wherever in the text it stands,
// and replacing it changes the text's cost by exactly the difference.
func encodedCost(v any) int {
	raw, err := plainjson.Marshal(v, "")
	if err != nil {
		return 0
	}
	return Len(string(raw))
}

// decodeObject reads a recorded argument text back as the tree it encodes:
// one JSON object, every number kept as the digits it was written in, and
// nothing after it.
func decodeObject(text string) (map[string]any, bool) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return nil, false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return root, true
}

// slot is one value of an argument tree, the place it stands in (its path
// from the root, a key or an index per level) so it can be replaced where it
// is, and whether it is an object or an array.
type slot struct {
	get       func() any
	set       func(any)
	path      []string
	container bool
	cost      int
}

// walk visits every value below v, depth first, children in the text's order
// (an object's keys sorted, as the engine writes them).
func walk(v any, path []string, visit func(slot)) {
	switch node := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			here := append(slices.Clip(path), key)
			child := node[key]
			visit(slot{get: func() any { return node[key] }, set: func(x any) { node[key] = x },
				path: here, container: isContainer(child)})
			walk(child, here, visit)
		}
	case []any:
		for i := range node {
			here := append(slices.Clip(path), fmt.Sprint(i))
			child := node[i]
			visit(slot{get: func() any { return node[i] }, set: func(x any) { node[i] = x },
				path: here, container: isContainer(child)})
			walk(child, here, visit)
		}
	}
}

// isContainer reports whether v is an object or an array.
func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

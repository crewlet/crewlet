package redact

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"
)

// jsonReach is how far around a password's key [decodedReading] looks for the
// logged string the key stands in, and for an escape after the key: a bound on
// the work a key costs, not on what is redacted, since the text's own reading
// is kept whatever this one finds ([readValue]).
const jsonReach = 1 << 10

// jsonString is a logged JSON string's contents from its opening quote to
// where the read of it stopped, decoded once, and where each escape of it
// stands in the text it was read from: view start, view end, raw start, raw
// end.
type jsonString struct {
	text  string
	from  int
	edits [][4]int
	// end is where the read stopped in the raw text: the string's closing
	// quote, or the bound or the text's end where it did not close before;
	// closed is whether it closed there.
	end    int
	closed bool
}

// raw is where view offset i stands in the raw text: inside an escape, at the
// escape's start, or at its end where end is set, so a range mapped back holds
// every escape it touches whole.
func (j jsonString) raw(i int, end bool) int {
	shift := j.from
	for _, e := range j.edits {
		if i <= e[0] {
			return i + shift
		}
		if i < e[1] {
			if end {
				return e[3]
			}
			return e[2]
		}
		shift = e[3] - e[1]
	}
	return i + shift
}

// view is where raw offset r stands in the view: inside an escape, at the
// escape's start.
func (j jsonString) view(r int) int {
	shift := -j.from
	for _, e := range j.edits {
		if r <= e[2] {
			return r + shift
		}
		if r < e[3] {
			return e[0]
		}
		shift = e[1] - e[3]
	}
	return r + shift
}

// loggedString is the logged JSON string the byte at start stands in, decoded
// once, read from its opening quote to its close or past at+jsonReach at most,
// and whether there is one: the nearest unescaped quote before start opens it
// in value position, in a JSON value that opens at a `{` or a `[` after any
// prefix on the line (a timestamp, a level); every `\` in it begins a JSON
// escape; its first unescaped quote closes it, on its line; and it holds a
// layer of escaping, a `\\`, a `\"` or a `\uXXXX`. A string that is not all of
// that, its lines joined into it raw among them, is read raw ([readValue]).
func loggedString[T textual](whole T, start, at int) (jsonString, bool) {
	floor := max(0, start-jsonReach)
	open := start - 1
	for open >= floor && whole[open] != '\n' && (whole[open] != '"' || backslashesBefore(whole, open, floor)%2 == 1) {
		open--
	}
	if open < floor || whole[open] != '"' {
		return jsonString{}, false
	}
	k := open - 1
	for k >= floor && (whole[k] == ' ' || whole[k] == '\t') {
		k--
	}
	if k < floor || tIndexByte(":[,{", whole[k]) < 0 || !jsonPartBefore(whole, k+1, floor) {
		return jsonString{}, false
	}
	bound := min(len(whole), at+jsonReach)
	j := jsonString{from: open + 1}
	var b strings.Builder
	layered := false
	i := open + 1
read:
	for ; i < bound; i++ {
		switch c := whole[i]; {
		case c == '\n':
			return jsonString{}, false
		case c == '"':
			if !closesString(whole, i, bound) {
				return jsonString{}, false
			}
			j.closed = true
			break read
		case c != '\\':
			b.WriteByte(c)
		default:
			r, n, ok := jsonEscapeAt(whole, i)
			if !ok {
				return jsonString{}, false
			}
			layered = layered || tIndexByte(`"\u`, whole[i+1]) >= 0
			v := b.Len()
			b.WriteRune(r)
			j.edits = append(j.edits, [4]int{v, b.Len(), i, i + n})
			i += n - 1
		}
	}
	j.end = i
	if !layered || j.closed && start >= j.end {
		return jsonString{}, false
	}
	j.text = b.String()
	return j, true
}

// jsonPartBefore reports whether the line holding open has a `{` or a `[`
// between its start, or floor, and open: a JSON value opens there, after any
// prefix the line has.
func jsonPartBefore[T textual](whole T, open, floor int) bool {
	for i := open - 1; i >= floor && whole[i] != '\n'; i-- {
		if whole[i] == '{' || whole[i] == '[' {
			return true
		}
	}
	return false
}

// closesString reports whether the unescaped quote at i is where a JSON
// string may close: what follows it, past spaces, is what may follow a string
// in a JSON value (a `,`, a `:`, a closing bracket) or the line's end, or a
// marker, which stands for what a pass took there, so a second pass closes it
// where the first did. A quote followed by words is one a line joined into
// the string raw held.
func closesString[T textual](whole T, i, bound int) bool {
	k := i + 1
	for k < bound && (whole[k] == ' ' || whole[k] == '\t') {
		k++
	}
	return k >= len(whole) || whole[k] == '\n' || whole[k] == '\r' || tIndexByte(",:}]", whole[k]) >= 0 ||
		tHasPrefix(whole[k:], Marker)
}

// backslashesBefore is how many backslashes stand straight before i, back to
// floor at most.
func backslashesBefore[T textual](whole T, i, floor int) int {
	n := 0
	for k := i - 1; k >= floor && whole[k] == '\\'; k-- {
		n++
	}
	return n
}

// jsonEscapeAt is the rune the JSON escape at whole[i:] stands for, its length,
// and whether there is one: `\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`, or
// `\u` and four hex digits, a surrogate pair read as one rune and a lone one as
// U+FFFD, which is no letter.
func jsonEscapeAt[T textual](whole T, i int) (rune, int, bool) {
	if i+1 >= len(whole) {
		return 0, 0, false
	}
	switch c := whole[i+1]; c {
	case '"', '\\', '/':
		return rune(c), 2, true
	case 'b':
		return '\b', 2, true
	case 'f':
		return '\f', 2, true
	case 'n':
		return '\n', 2, true
	case 'r':
		return '\r', 2, true
	case 't':
		return '\t', 2, true
	case 'u':
	default:
		return 0, 0, false
	}
	r, ok := hex4(whole, i+2)
	if !ok {
		return 0, 0, false
	}
	if 0xD800 <= r && r < 0xDC00 && i+12 <= len(whole) && whole[i+6] == '\\' && whole[i+7] == 'u' {
		if lo, ok := hex4(whole, i+8); ok && 0xDC00 <= lo && lo < 0xE000 {
			return 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00), 12, true
		}
	}
	if 0xD800 <= r && r < 0xE000 {
		return utf8.RuneError, 6, true
	}
	return r, 6, true
}

// hex4 is the four hex digits at whole[i:] read as a number, and whether they
// are four.
func hex4[T textual](whole T, i int) (rune, bool) {
	if i+4 > len(whole) {
		return 0, false
	}
	var r rune
	for k := i; k < i+4; k++ {
		switch c := whole[k]; {
		case '0' <= c && c <= '9':
			r = r<<4 | rune(c-'0')
		case 'a' <= c && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case 'A' <= c && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

// decodedReading is the value a password's key from start to end names, read
// in the logged JSON string it stands in decoded once ([loggedString]), mapped
// back to the text, and whether there is such a string: a value a logger's
// escaping hid from the text's own reading (a shell line's `\n` written `\\n`,
// its quotes `\"`, a `>` written `\u003e`). Where no escape follows the key
// on its line, or the string is no such one, there is none.
func decodedReading[T textual](whole T, start, end int, prior [][2]int) (aValue, bool) {
	near := whole[end:min(len(whole), end+jsonReach/4)]
	if i := tIndexByte(near, '\n'); i >= 0 {
		// The key's own line alone: what follows it decides nothing here,
		// so a text cut after the line reads as the whole text does.
		near = near[:i]
	}
	if tIndexByte(near, '\\') < 0 {
		return aValue{}, false
	}
	j, found := loggedString(whole, start, end)
	if !found {
		return aValue{}, false
	}
	var vprior [][2]int
	first, _ := slices.BinarySearchFunc(prior, j.from, func(p [2]int, at int) int {
		if p[1] <= at {
			return -1
		}
		return 1
	})
	for _, p := range prior[first:] {
		if p[0] >= j.end {
			break
		}
		if a, b := j.view(max(p[0], j.from)), j.view(min(p[1], j.end)); a < b {
			vprior = append(vprior, [2]int{a, b})
		}
	}
	vs, ve := j.view(start), j.view(end)
	if ve <= vs || ve > len(j.text) || !strings.ContainsAny(j.text[vs:ve], ":=") {
		// The key's separator is an escape, or the key runs past the
		// string: the decoded string names no key here.
		return aValue{}, false
	}
	o := readValuePlain(j.text, vs, ve, vprior)
	v := aValue{unread: o.unread, decided: j.end}
	if o.to > o.from {
		v.from, v.to = j.raw(o.from, false), j.raw(o.to, true)
	}
	if o.also[1] > o.also[0] {
		v.also = [2]int{j.raw(o.also[0], false), j.raw(o.also[1], true)}
	}
	if o.decided < len(j.text) {
		v.decided = j.raw(o.decided, false)
	}
	// The seams go back with the value: a claim without its gutter has a
	// key's block written across it ([claims.apart]).
	v.gutter, v.ending, v.opening = j.seam(o.gutter), j.seam(o.ending), j.seam(o.opening)
	for _, r := range o.runs {
		v.runs = append(v.runs, [2]int{j.raw(r[0], false), j.raw(r[1], true)})
	}
	return v, true
}

// seam is where seam i of a reading of the view stands in the raw text, or 0
// where there is none ([aValue]).
func (j jsonString) seam(i int) int {
	if i == 0 {
		return 0
	}
	return j.raw(i, false)
}

// unionReading is v, the text's own reading, with o, the decoded one's, added:
// every range either reads is hidden, the two that lie earliest kept as the
// value and its second range and any more joined to the one before ([aValue]),
// the value is decided where both are, and each range keeps the seams of the
// reading that named it, so the claims cut a key's block by each.
func unionReading(v, o aValue) aValue {
	v.decided, v.unread = max(v.decided, o.decided), v.unread && o.unread
	v.runs = mergeSpans(append(v.runs, o.runs...))
	type read struct {
		r     [2]int
		seams [3]int
		own   bool
	}
	var rs []read
	for _, r := range []read{
		{[2]int{v.from, v.to}, [3]int{v.gutter, v.ending, v.opening}, true}, {v.also, v.alsoSeams, true},
		{[2]int{o.from, o.to}, [3]int{o.gutter, o.ending, o.opening}, false}, {o.also, o.alsoSeams, false},
	} {
		if r.r[1] > r.r[0] {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return v
	}
	slices.SortStableFunc(rs, func(a, b read) int { return a.r[0] - b.r[0] })
	merged := rs[:1]
	for _, r := range rs[1:] {
		last := &merged[len(merged)-1]
		if r.r[0] <= last.r[1] || len(merged) == 2 {
			// One range both readings name keeps the text's own reading's
			// seams, as its claims had them, and the other's where it has none.
			first, second := last.seams, r.seams
			if r.own && !last.own {
				first, second = second, first
			}
			last.r[1], last.own = max(last.r[1], r.r[1]), last.own || r.own
			for i := range last.seams {
				last.seams[i] = cmp.Or(first[i], second[i])
			}
			continue
		}
		merged = append(merged, r)
	}
	if merged[0].r != [2]int{v.from, v.to} {
		v.ran = false
	}
	v.from, v.to, v.also, v.alsoSeams = merged[0].r[0], merged[0].r[1], [2]int{}, [3]int{}
	v.gutter, v.ending, v.opening = merged[0].seams[0], merged[0].seams[1], merged[0].seams[2]
	if len(merged) == 2 {
		v.also, v.alsoSeams = merged[1].r, merged[1].seams
	}
	return v
}

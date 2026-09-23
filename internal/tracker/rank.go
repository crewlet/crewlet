package tracker

import (
	"errors"
	"fmt"
	"strings"
)

// The manual order, as a value type.
//
// A project's board has an order nobody stores as a number: dragging one card
// between two others must not rewrite every row beneath it, and two people
// dragging at once must not have to take turns. So the order is a KEY —
// byte-comparable, dense, and mintable strictly between any two neighbours —
// and this file is the arithmetic, with no database, no broker and no applier
// anywhere in it. That is deliberate: the algebra is the part that is hard to
// get right and impossible to exercise through a live fleet, and the two bugs
// it has had were both reachable with two integers and a loop.
//
// # The key has two parts, and the split is what bounds its growth
//
// An INTEGER part — a magnitude head over 52 symbols declaring how many
// base-62 digits follow — and an optional FRACTIONAL part over 62. The head
// is what makes the lattice unbounded in both directions without a sign
// character: A..Z count downward and a..z upward, so "Zz" < "a0" < "azz" <
// "b100" all hold as plain byte comparisons.
//
// The alternative — one flat digit string, decremented at the front for a
// head placement — is what this replaces, and it had a hard capacity of
// THIRTY head placements per project, ever. The integer part borrows and
// carries instead, so a head placement grows the key by about one character
// per sixty-two insertions rather than exhausting an alphabet.
//
// # Two minting rules, and both are about the fractional part
//
// The fraction never ends in '0', and nothing else. There is deliberately NO
// rule about the first character: it is a magnitude head, so it is never '0'
// by construction, and a rule stated about the whole key outlaws "a0" — the
// very first key an empty project mints. There is no rule against an all-'z'
// key either.

// RankDigits is the fractional alphabet, in byte order.
//
// SIXTY-TWO SYMBOLS in ASCII order, so a plain byte comparison of two keys IS
// their order. Any alphabet whose symbols are not in byte order would need the
// database to sort with a collation, and a collation is a setting a restore
// can lose.
const RankDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RankOrigin is the first key an empty project mints.
//
// It is also the BOUNDARY the two mint sets are separated by: every create's
// key is a pure integer at or above it, and every head placement is a pure
// integer strictly below it.
const RankOrigin = "a0"

// RankRenormaliseAt is the key length past which a project's order is handed
// to the re-spread walk.
//
// The applier flags a project whenever one of its live keys is longer than
// this, and [PlanRespread] rewrites the project's whole order to short keys. A
// drag is not refused for its key's length below [RankRefuseAt]: it lands long
// and the walk tidies after it.
//
// SIXTY-FOUR, because it sits far above the length a healthy project reaches
// and well below the schema's own ceiling: 10 000 tasks re-spread to keys of
// five characters, a key grows past this only after three to four hundred
// drags land in one gap (both measured, the second from a gap between two
// adjacent integers), and [RankRefuseAt] is nearly four times it.
const RankRenormaliseAt = 64

// RankRefuseAt is the longest key the schema's CHECK on `tracker_tasks.rank`
// accepts, and a key past it is REFUSED with [ErrRankTooLong] rather than cut:
// a shortened key is a different position.
//
// It is not reached in correct operation: a key past [RankRenormaliseAt] flags
// its project for the re-spread walk, and carrying one key from there to here
// takes another nine hundred and fifty to eleven hundred and forty drags into
// the same gap (measured, as above) before the walk has run. That is reachable
// — nothing but the walk's own schedule stands between the two thresholds — so
// the refusal a drag gets names the key's length, this limit and the remedy:
// the walk gives every task in the project a short key again, so a drop
// between the same two cards, read after it has run, mints a short one.
const RankRefuseAt = 254

// ErrRankTooLong is a key longer than [RankRefuseAt].
//
// A SENTINEL rather than a message alone, because a drag into a gap
// subdivided past the limit reaches it with a request that is not malformed:
// whoever dropped the card is to be told to wait for the walk, which a surface
// can say only if it can tell this refusal from a bad key.
var ErrRankTooLong = errors.New("tracker: a rank key is longer than the schema accepts")

// Rank is one position in a project's manual order.
type Rank string

// Valid reports a well-formed key.
func (r Rank) Valid() bool {
	if len(r) == 0 || len(r) > RankRefuseAt {
		return false
	}
	i, err := integerPart(string(r))
	if err != nil {
		return false
	}
	frac := string(r)[len(i):]
	if strings.HasSuffix(frac, "0") {
		return false
	}
	for j := range len(frac) {
		if strings.IndexByte(RankDigits, frac[j]) < 0 {
			return false
		}
	}
	return true
}

// The three predicates, ON THE VALUE ALONE.
//
// This is what makes the two minting sets provably disjoint without mentioning
// a counter, a high-water mark, staleness or an in-flight create — so the
// proof survives out-of-order create landings and head placements, which is
// exactly where the bound this replaces failed with no concurrency at all.

// FromCreate reports a key only a create can have minted: a pure integer at or
// above the origin.
func (r Rank) FromCreate() bool {
	return r.Fraction() == "" && string(r) >= RankOrigin && r.Valid()
}

// HeadPlacement reports a key only a move can have minted at the head: a pure
// integer strictly below the origin.
func (r Rank) HeadPlacement() bool {
	return r.Fraction() == "" && string(r) < RankOrigin && r.Valid()
}

// Fraction is the part after the integer part, empty when there is none.
func (r Rank) Fraction() string {
	i, err := integerPart(string(r))
	if err != nil {
		return ""
	}
	return string(r)[len(i):]
}

// integerLength is how many characters an integer part with this head has,
// including the head.
func integerLength(head byte) (int, error) {
	switch {
	case head >= 'a' && head <= 'z':
		return int(head-'a') + 2, nil
	case head >= 'A' && head <= 'Z':
		return int('Z'-head) + 2, nil
	}
	return 0, fmt.Errorf("tracker: %q is not a rank magnitude head — the head "+
		"declares how many digits follow, so a key without one has no length "+
		"to read", string(head))
}

// integerPart splits the integer part off a key.
func integerPart(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("tracker: an empty rank key has no integer part")
	}
	n, err := integerLength(key[0])
	if err != nil {
		return "", err
	}
	if len(key) < n {
		return "", fmt.Errorf("tracker: rank key %q declares %d characters of "+
			"integer part and carries %d", key, n, len(key))
	}
	return key[:n], nil
}

// incrementInteger is the next integer key, and it CARRIES through the head.
func incrementInteger(x string) (string, error) {
	if _, err := integerPart(x); err != nil {
		return "", err
	}
	head, digits := x[0], []byte(x[1:])
	carry := true
	for i := len(digits) - 1; carry && i >= 0; i-- {
		d := strings.IndexByte(RankDigits, digits[i]) + 1
		if d == len(RankDigits) {
			digits[i] = RankDigits[0]
		} else {
			digits[i] = RankDigits[d]
			carry = false
		}
	}
	if !carry {
		return string(head) + string(digits), nil
	}
	switch head {
	case 'Z':
		// The last negative magnitude rolls over into the origin, which
		// is the one place the two halves of the lattice meet.
		return RankOrigin, nil
	case 'z':
		return "", fmt.Errorf("tracker: the rank lattice is exhausted upward at "+
			"%q — this is 62^26 positions above the origin and bounds a "+
			"project's lifetime task count, nothing else", x)
	}
	// The mirror of the rule in [decrementInteger]: stepping UP from a
	// positive head lengthens the digit run and stepping up from a
	// negative one shortens it.
	h := head + 1
	if h >= 'a' {
		digits = append(digits, RankDigits[0])
	} else {
		digits = digits[:len(digits)-1]
	}
	return string(h) + string(digits), nil
}

// decrementInteger is the previous integer key, and it BORROWS through the
// head — which is what makes a head placement cost one character per
// sixty-two insertions instead of exhausting a thirty-symbol alphabet.
func decrementInteger(x string) (string, error) {
	if _, err := integerPart(x); err != nil {
		return "", err
	}
	head, digits := x[0], []byte(x[1:])
	borrow := true
	for i := len(digits) - 1; borrow && i >= 0; i-- {
		d := strings.IndexByte(RankDigits, digits[i]) - 1
		if d < 0 {
			digits[i] = RankDigits[len(RankDigits)-1]
		} else {
			digits[i] = RankDigits[d]
			borrow = false
		}
	}
	if !borrow {
		return string(head) + string(digits), nil
	}
	switch head {
	case 'a':
		return "Z" + string(RankDigits[len(RankDigits)-1]), nil
	case 'A':
		return "", fmt.Errorf("tracker: the rank lattice is exhausted downward "+
			"at %q", x)
	}
	// THE DIRECTION THE DIGIT RUN MOVES IS OPPOSITE ON THE TWO SIDES, and
	// getting it backwards is the bug that makes a key length-invalid
	// rather than merely mis-ordered — which nothing downstream would
	// catch, because a length-invalid key still compares. Heads count
	// AWAY from the origin in both directions: 'b' declares one more
	// digit than 'a', and 'Y' declares one more than 'Z'. So stepping
	// DOWN from a positive head shortens the run, and stepping down from
	// a negative head lengthens it.
	h := head - 1
	if h <= 'Z' {
		digits = append(digits, RankDigits[len(RankDigits)-1])
	} else {
		digits = digits[:len(digits)-1]
	}
	return string(h) + string(digits), nil
}

// IntegerAt is the n-th key of the CREATE lattice, one-based.
//
// A create mints this from the counter value its own conditional append just
// obtained. The counter value is unique and increasing under that row's own
// arbitration, so no two creates can collide and every create's key is
// strictly the new maximum — which is what makes new tasks land at the tail
// in creation order with no extra arbitration at all.
//
// COMPUTED DIRECTLY rather than by incrementing n times: the n-th key is a
// base-62 encoding once the head is known, and a loop would make a create on a
// mature project O(n) in the project's own age.
func IntegerAt(n uint64) (Rank, error) {
	if n == 0 {
		return "", fmt.Errorf("tracker: the create lattice is one-based — key " +
			"zero would sit below the origin, which is where head placements " +
			"live and where a create must never land")
	}
	offset := n - 1
	span := uint64(len(RankDigits))
	for head := byte('a'); head <= 'z'; head++ {
		if offset < span {
			digits := int(head-'a') + 1
			out := make([]byte, digits)
			for i := digits - 1; i >= 0; i-- {
				out[i] = RankDigits[offset%uint64(len(RankDigits))]
				offset /= uint64(len(RankDigits))
			}
			return Rank(string(head) + string(out)), nil
		}
		offset -= span
		next := span * uint64(len(RankDigits))
		if next/uint64(len(RankDigits)) != span {
			// THE SPAN OUTGREW A uint64 BEFORE THE OFFSET COULD, and
			// saturating is exact rather than approximate: from this
			// head on, one head's digit run holds more keys than the
			// argument type can express, so the remaining offset is
			// always inside it and the base-62 encoding at that head's
			// declared digit count is the right answer.
			span = ^uint64(0)
			continue
		}
		span = next
	}
	return "", fmt.Errorf("tracker: %d is past the create lattice", n)
}

// KeyBetween mints a key strictly between a and b.
//
// An empty a means "before everything" and an empty b means "after
// everything", which is what makes the head and tail placements the same call
// rather than three.
func KeyBetween(a, b Rank) (Rank, error) {
	if a != "" && b != "" && string(a) >= string(b) {
		return "", fmt.Errorf("tracker: %q is not below %q, so there is nothing "+
			"between them", a, b)
	}
	switch {
	case a == "" && b == "":
		return RankOrigin, nil
	case a == "":
		ib, err := integerPart(string(b))
		if err != nil {
			return "", err
		}
		if ib < string(b) {
			// b carries a fraction, so its own integer part is free.
			return Rank(ib), nil
		}
		prev, err := decrementInteger(ib)
		if err != nil {
			// The lattice is exhausted downward. There is deliberately
			// no fallback into b's own fraction here: b HAS no
			// fraction on this branch, so subdividing it would mint a
			// key ABOVE b under the name of one below it.
			return "", err
		}
		return Rank(prev), nil
	case b == "":
		ia, err := integerPart(string(a))
		if err != nil {
			return "", err
		}
		fa := string(a)[len(ia):]
		next, err := incrementInteger(ia)
		if err != nil {
			mid, mErr := midpoint(fa, "")
			if mErr != nil {
				return "", mErr
			}
			return Rank(ia + mid), nil
		}
		return Rank(next), nil
	}
	ia, err := integerPart(string(a))
	if err != nil {
		return "", err
	}
	ib, err := integerPart(string(b))
	if err != nil {
		return "", err
	}
	fa, fb := string(a)[len(ia):], string(b)[len(ib):]
	if ia == ib {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		mid, err := midpoint(fa, fb)
		if err != nil {
			return "", err
		}
		return Rank(ia + mid), nil
	}
	next, err := incrementInteger(ia)
	if err != nil {
		return "", err
	}
	if next < string(b) {
		return Rank(next), nil
	}
	mid, err := midpoint(fa, "")
	if err != nil {
		return "", err
	}
	return Rank(ia + mid), nil
}

// midpoint is a fractional string strictly between a and b, where b empty
// means "no upper bound".
//
// It never returns a value ending in '0', which is the whole of the second
// minting rule: a trailing zero is a key with a shorter equal, and two
// spellings of one position is a unique index that accepts both.
func midpoint(a, b string) (string, error) {
	if b != "" && a >= b {
		return "", fmt.Errorf("tracker: fractional %q is not below %q", a, b)
	}
	if strings.HasSuffix(a, "0") || strings.HasSuffix(b, "0") {
		return "", fmt.Errorf("tracker: a fractional part ending in '0' is a key " +
			"with a shorter equal, so it can never have been minted")
	}
	if b != "" {
		n := 0
		for n < len(b) {
			var ac byte = '0'
			if n < len(a) {
				ac = a[n]
			}
			if ac != b[n] {
				break
			}
			n++
		}
		if n > 0 {
			rest, err := midpoint(cut(a, n), b[n:])
			if err != nil {
				return "", err
			}
			return b[:n] + rest, nil
		}
	}
	digitA := 0
	if a != "" {
		digitA = strings.IndexByte(RankDigits, a[0])
		if digitA < 0 {
			return "", fmt.Errorf("tracker: %q is not a rank digit", string(a[0]))
		}
	}
	digitB := len(RankDigits)
	if b != "" {
		digitB = strings.IndexByte(RankDigits, b[0])
		if digitB < 0 {
			return "", fmt.Errorf("tracker: %q is not a rank digit", string(b[0]))
		}
	}
	if digitB-digitA > 1 {
		return string(RankDigits[(digitA+digitB+1)/2]), nil
	}
	if len(b) > 1 {
		return b[:1], nil
	}
	rest, err := midpoint(cut(a, 1), "")
	if err != nil {
		return "", err
	}
	return string(RankDigits[digitA]) + rest, nil
}

// cut drops the first n bytes, answering empty rather than panicking on a
// short string — which is the case where a is a prefix of b.
func cut(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

// RespreadKeys is n keys, evenly spaced, all sharing one integer head.
//
// # Why the re-spread does not bisect
//
// Minting n keys by repeated bisection of an interval — [KeyBetween] applied
// recursively — is exactly right for "put this card between those two" and
// exactly wrong here: half the keys crowd against the upper bound and inherit
// its length, so re-spreading a project whose keys are 69 characters long
// produces keys that are 70. The walk that exists to SHORTEN keys would
// lengthen them.
//
// What a re-spread wants instead is a fresh integer position and n evenly
// spaced fractions inside it, each as short as the count allows: at 62 symbols
// a three-digit fraction holds 238 328 positions, so every key a real project
// can produce is five characters. The spacing is uniform rather than bisected
// because there is no reason to prefer one end — the whole order is being
// rewritten, so every gap should be the same size.
//
// The keys are all strictly greater than head alone and strictly less than the
// next integer up, which is what makes the caller's "below the project's
// minimum" reserve enough to place the whole walk.
func RespreadKeys(head Rank, n int) ([]Rank, error) {
	if n <= 0 {
		return nil, nil
	}
	integer, err := integerPart(string(head))
	if err != nil {
		return nil, err
	}

	// THE NARROWEST FRACTION THAT LEAVES A GAP OF AT LEAST TWO between
	// consecutive keys. Two rather than one because the last digit is
	// nudged off '0' below, and a gap of one would let that nudge
	// overtake the next key.
	span := uint64(1)
	width := 0
	for span < uint64(2*(n+1)) {
		span *= uint64(len(RankDigits))
		width++
		if width > 10 {
			return nil, fmt.Errorf("tracker: %d keys need a fraction wider "+
				"than ten symbols, which is more positions than a project can "+
				"hold — the order needs rebuilding rather than re-spreading", n)
		}
	}

	keys := make([]Rank, 0, n)
	for i := range n {
		// EVENLY SPACED, and the arithmetic is over the whole span so
		// rounding cannot make two keys equal: consecutive values differ
		// by at least two because the span was chosen for it.
		value := span * uint64(i+1) / uint64(n+1)
		fraction := renderFraction(value, width)
		keys = append(keys, Rank(integer+fraction))
	}
	return keys, nil
}

// renderFraction writes a value in the rank alphabet, fixed width, never
// ending in '0'.
//
// The trailing '0' is forbidden because a key ending in one has a shorter
// equivalent that sorts identically, and two spellings of one position is a
// second thing for every comparison to agree about.
func renderFraction(value uint64, width int) string {
	base := uint64(len(RankDigits))
	digits := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		digits[i] = RankDigits[value%base]
		value /= base
	}
	if digits[width-1] == '0' {
		digits[width-1] = RankDigits[1]
	}
	return string(digits)
}

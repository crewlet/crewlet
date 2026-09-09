package tracker

import (
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

// RankRenormaliseAt is both the length at which a mint is replaced by a
// re-spread and the initial window that re-spread takes.
//
// ONE CONSTANT RATHER THAN TWO, because the window is DERIVED from it by
// doubling until the keys it would produce fit under it.
//
// SIXTY-FOUR RATHER THAN JIRA'S 128, and the number is a property of the
// REPAIR'S COST rather than a citation: Jira's 128 is calibrated against a
// rebalance that reindexes every issue in the system, where this repair is at
// most 256 rows in one commit — about 150× cheaper on a 10 000-task project
// — so the threshold balancing repair cost against key length sits far lower.
// It is 16× the length a healthy project reaches (10 000 tasks re-spread to
// four characters, measured), so it fires only on a genuine same-gap nest of
// 318–380 consecutive inserts into one gap, and it halves the worst-case
// entry in the three rank-bearing indexes.
const RankRenormaliseAt = 64

// RankRespreadInline is where the inline repair stops and the project is
// marked for the duty's paced walk instead.
//
// Both halves of it are an order of magnitude inside their budgets: 256
// (uuid, key) pairs are ≈ 15.4 KB — 1.2 % of the maximal commit — and 256
// apply rows are 6.4 % of one apply transaction's row budget. So an inline
// re-spread can never be what makes a transaction long. A DRAG IS NEVER
// REFUSED FOR LENGTH: past this many neighbours it still succeeds and the
// walk finishes the tidying.
const RankRespreadInline = 256

// RankRefuseAt is a schema CHECK and therefore an ASSERTION, not a policy.
//
// It is unreachable in correct operation, because any mint that would exceed
// [RankRenormaliseAt] is replaced by a re-spread. Its message says the
// generator is broken.
const RankRefuseAt = 254

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

// KeysBetween mints n keys in ascending order strictly between a and b.
//
// The re-spread's own primitive. It splits at the midpoint and recurses rather
// than chaining, so the keys it produces are as short as the gap allows: a
// chain would make the last key of a 256-row re-spread 256 characters long,
// which is the length the re-spread exists to remove.
func KeysBetween(a, b Rank, n int) ([]Rank, error) {
	switch {
	case n < 0:
		return nil, fmt.Errorf("tracker: cannot mint %d keys", n)
	case n == 0:
		return nil, nil
	}
	if a != "" && b != "" && string(a) >= string(b) {
		return nil, fmt.Errorf("tracker: %q is not below %q, so there is nothing "+
			"between them", a, b)
	}
	mid := n / 2
	key, err := KeyBetween(a, b)
	if err != nil {
		return nil, err
	}
	left, err := KeysBetween(a, key, mid)
	if err != nil {
		return nil, err
	}
	right, err := KeysBetween(key, b, n-mid-1)
	if err != nil {
		return nil, err
	}
	out := make([]Rank, 0, n)
	out = append(out, left...)
	out = append(out, key)
	out = append(out, right...)
	return out, nil
}

// RespreadWindow is how many neighbours a re-spread takes, DERIVED from
// [RankRenormaliseAt] rather than declared beside it.
//
// Start at the threshold and double until the keys the window would produce
// fall under it. One constant, one derivation: a second constant for the
// window is a second thing to keep in step with the first, and the two have
// no independent reason to differ.
//
// It stops at [RankRespreadInline]; past that the drag still succeeds and the
// project is marked for the duty's paced walk.
func RespreadWindow(a, b Rank) (int, error) {
	window := RankRenormaliseAt
	for {
		keys, err := KeysBetween(a, b, window)
		if err != nil {
			return 0, err
		}
		longest := 0
		for _, k := range keys {
			if len(k) > longest {
				longest = len(k)
			}
		}
		if longest < RankRenormaliseAt || window >= RankRespreadInline {
			return window, nil
		}
		window *= 2
		if window > RankRespreadInline {
			return RankRespreadInline, nil
		}
	}
}

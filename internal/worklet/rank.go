package worklet

import (
	"fmt"
	"strconv"
	"strings"
)

// The manual order, as a value type: a KEY that is byte-comparable, dense and
// mintable strictly between any two neighbours, so a drag rewrites no row
// beneath it and two concurrent drags need not take turns. This file is the
// arithmetic alone, with no database, broker or applier.
//
// A key is an INTEGER part (a magnitude head, A..Z counting down and a..z up,
// declaring how many base-62 digits follow, so "Zz" < "a0" < "azz" < "b100" as
// bytes) and an optional base-62 FRACTION. The integer part borrows and
// carries, so head placements grow a key by about one character per 62. The
// one minting rule: the FRACTION never ends in '0' (of the whole key, it would
// outlaw "a0", the first key). No rule on the first character, a head and so
// never '0', or on all-'z' keys.

// RankDigits is the fractional alphabet, in byte order, so a plain byte
// comparison of two keys IS their order; any other order would need a
// collation, a setting a restore can lose.
const RankDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RankOrigin is the first key an empty project mints, and the boundary between
// the two mint sets: every create's key is a pure integer at or above it,
// every head placement a pure integer strictly below it.
const RankOrigin = "a0"

// RankRenormaliseAt is both the length at which a mint is replaced by a
// re-spread and that re-spread's initial window (doubled until its keys fit).
//
// 64, not Jira's 128: this repair is at most 256 rows in one commit, far
// cheaper than a whole-system reindex, so the threshold sits lower. It is 16×
// the length a healthy 10 000-task project reaches (four characters), so it
// fires only on 318–380 consecutive inserts into one gap.
//
// It is also the WHERE of `tracker_tasks_respread_idx` (replicated 0002):
// moving it takes a migration replacing that index, or every statement
// [longRank] is spelled into fails with `no query solution`
// (TestEveryIndexServesARegisteredQuery fails first).
const RankRenormaliseAt = 64

// longRank is the term selecting a project's keys past [RankRenormaliseAt]
// (the rows a re-spread rewrites) over the rank column `column`, for a read of
// `tracker_tasks_respread_idx`.
//
// The threshold is a LITERAL, never a bound value: the planner uses a partial
// index only from a term it can compare with the index's `length(rank) > 64`.
// Every reader also names the index (`INDEXED BY`): unprompted, the planner
// takes `tracker_tasks_board_idx` and walks every live task of the project, on
// the apply path of every node. Named, a threshold the index no longer implies
// or a dropped index fails loudly rather than as a slower board.
func longRank(column string) string {
	return "length(" + column + ") > " + strconv.Itoa(RankRenormaliseAt)
}

// RankRespreadInline is where the inline repair stops and the project is
// marked for the duty's paced walk instead.
//
// 256 (uuid, key) pairs are ≈ 15.4 KB, 1.2 % of the maximal commit, and 6.4 %
// of an apply transaction's row budget, so an inline re-spread never makes a
// transaction long. A drag is never refused for length: past this many
// neighbours it still lands and the walk finishes the tidying.
const RankRespreadInline = 256

// RankRefuseAt is a schema CHECK, an assertion rather than a policy:
// unreachable in correct operation, since any mint past [RankRenormaliseAt] is
// replaced by a re-spread. Its message says the generator is broken.
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

// The predicates read the value alone, so the two minting sets are provably
// disjoint without a counter, high-water mark or in-flight create, and the
// proof survives out-of-order create landings and head placements.

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
	return 0, fmt.Errorf("worklet: %q is not a rank magnitude head; the head "+
		"declares how many digits follow, so a key without one has no length "+
		"to read", string(head))
}

// integerPart splits the integer part off a key.
func integerPart(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("worklet: an empty rank key has no integer part")
	}
	n, err := integerLength(key[0])
	if err != nil {
		return "", err
	}
	if len(key) < n {
		return "", fmt.Errorf("worklet: rank key %q declares %d characters of "+
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
		// The last negative magnitude rolls over into the origin.
		return RankOrigin, nil
	case 'z':
		return "", fmt.Errorf("worklet: the rank lattice is exhausted upward at "+
			"%q — this is 62^26 positions above the origin and bounds a "+
			"project's lifetime task count, nothing else", x)
	}
	// The mirror of [decrementInteger]: up from a positive head lengthens
	// the digit run, up from a negative one shortens it.
	h := head + 1
	if h >= 'a' {
		digits = append(digits, RankDigits[0])
	} else {
		digits = digits[:len(digits)-1]
	}
	return string(h) + string(digits), nil
}

// decrementInteger is the previous integer key, and it BORROWS through the
// head, so a head placement costs one character per 62 insertions.
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
		return "", fmt.Errorf("worklet: the rank lattice is exhausted downward "+
			"at %q", x)
	}
	// Heads count AWAY from the origin both ways ('b' declares one more
	// digit than 'a', 'Y' one more than 'Z'), so down from a positive head
	// shortens the run and down from a negative one lengthens it. Backwards,
	// the key is length-invalid, which nothing downstream catches because
	// it still compares.
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
// A create mints it from the counter value its conditional append obtained,
// unique and increasing under that row's arbitration, so creates never collide
// and land at the tail in creation order with no extra arbitration.
// Computed directly, not by n increments, which would make a create O(n) in
// the project's age.
func IntegerAt(n uint64) (Rank, error) {
	if n == 0 {
		return "", fmt.Errorf("worklet: the create lattice is one-based; key " +
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
			// The span outgrew a uint64: saturating is exact, since every
			// later head holds more keys than a uint64 can name, so the
			// remaining offset always falls inside it.
			span = ^uint64(0)
			continue
		}
		span = next
	}
	return "", fmt.Errorf("worklet: %d is past the create lattice", n)
}

// KeyBetween mints a key strictly between a and b.
//
// An empty a means "before everything" and an empty b "after everything", so
// head and tail placements are the same call.
func KeyBetween(a, b Rank) (Rank, error) {
	if a != "" && b != "" && string(a) >= string(b) {
		return "", fmt.Errorf("worklet: %q is not below %q, so there is nothing "+
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
			// Exhausted downward. No fallback into b's fraction: b has
			// none here, so subdividing would mint a key ABOVE b.
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
// It never returns a value ending in '0', the minting rule: a trailing zero is
// a key with a shorter equal, and a unique index accepts both spellings.
func midpoint(a, b string) (string, error) {
	if b != "" && a >= b {
		return "", fmt.Errorf("worklet: fractional %q is not below %q", a, b)
	}
	if strings.HasSuffix(a, "0") || strings.HasSuffix(b, "0") {
		return "", fmt.Errorf("worklet: a fractional part ending in '0' is a key " +
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
			return "", fmt.Errorf("worklet: %q is not a rank digit", string(a[0]))
		}
	}
	digitB := len(RankDigits)
	if b != "" {
		digitB = strings.IndexByte(RankDigits, b[0])
		if digitB < 0 {
			return "", fmt.Errorf("worklet: %q is not a rank digit", string(b[0]))
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

// cut drops the first n bytes, answering empty on a short string (a is a
// prefix of b) rather than panicking.
func cut(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

// KeysBetween mints n keys in ascending order strictly between a and b.
//
// It splits at the midpoint and recurses rather than chaining, so its keys are
// as short as the gap allows: a chain would make the last of 256 keys 256
// characters long.
func KeysBetween(a, b Rank, n int) ([]Rank, error) {
	switch {
	case n < 0:
		return nil, fmt.Errorf("worklet: cannot mint %d keys", n)
	case n == 0:
		return nil, nil
	}
	if a != "" && b != "" && string(a) >= string(b) {
		return nil, fmt.Errorf("worklet: %q is not below %q, so there is nothing "+
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

// MoveKeys mints n keys in ascending order strictly between a and b for a
// MOVE — and none of them is a key a create could mint.
//
// [KeysBetween] alone would take the cheapest key at a project's ends, a pure
// integer in the create lattice: after the last task, exactly the key the next
// create mints, so the two would share a rank. [Writer.CreateTask] rests on
// every move's key carrying a fraction or sitting below the origin.
//
// So the upper bound is TIGHTENED, never the lower: after a key, to the next
// integer above its integer part; before everything, to the origin. Each keeps
// a non-empty open interval and keys as short as the untightened mint.
func MoveKeys(a, b Rank, n int) ([]Rank, error) {
	ceiling, err := moveCeiling(a, b)
	if err != nil {
		return nil, err
	}
	return KeysBetween(a, ceiling, n)
}

// moveCeiling is the tightest upper bound a move may mint under — see
// [MoveKeys].
func moveCeiling(a, b Rank) (Rank, error) {
	if a == "" {
		if b == "" || string(b) > RankOrigin {
			return RankOrigin, nil
		}
		return b, nil
	}
	integer, err := integerPart(string(a))
	if err != nil {
		return "", err
	}
	next, err := incrementInteger(integer)
	if err != nil {
		// Exhausted upward: no create can mint above this key either, so
		// the untightened bound is already safe.
		return b, nil //nolint:nilerr // an exhausted lattice is the answer, not a failure
	}
	if b == "" || string(b) > next {
		return Rank(next), nil
	}
	return b, nil
}

// PlaceInGap decides where one moved task lands, and which neighbours a long
// key makes it re-spread in the same record; nil when it already sits in the
// gap. Pure, over rows [Writer.PlaceTask] reads in its snapshot. below and
// above are the tasks strictly beneath and over the gap, NEAREST FIRST,
// without the mover; each needs at most [RankRespreadInline]/2 + 1 rows.
//
// A single key too long, or neighbours sharing a key, WIDENS the window from
// [RankRenormaliseAt] rows, doubling up to [RankRespreadInline]; its bounds
// are the first rows outside it, so it escapes a same-gap nest and converges;
// a shared key inside it is re-minted apart. Past the cap the drag still lands with its long key for the duty's walk;
// the one refusal is a gap with no key even at the cap, which only a repair
// clears.
func PlaceInGap(mover Placement, below, above []Placement) ([]Placement, error) {
	var lower, upper Rank
	if len(below) > 0 {
		lower = below[0].Rank
	}
	if len(above) > 0 {
		upper = above[0].Rank
	}
	if (lower == "" || string(mover.Rank) > string(lower)) &&
		(upper == "" || string(mover.Rank) < string(upper)) {
		return nil, nil
	}
	single, singleErr := MoveKeys(lower, upper, 1)
	if singleErr == nil && len(single[0]) <= RankRenormaliseAt {
		return []Placement{{Task: mover.Task, Rank: single[0]}}, nil
	}

	half := RankRespreadInline / 2
	for k := RankRenormaliseAt / 2; ; k *= 2 {
		k = min(k, half)
		lo, outerLo := windowSide(below, k, half)
		hi, outerHi := windowSide(above, k, half)
		keys, err := MoveKeys(outerLo, outerHi, len(lo)+1+len(hi))
		if err == nil && longest(keys) <= RankRenormaliseAt {
			placements := make([]Placement, 0, len(keys))
			for i := len(lo) - 1; i >= 0; i-- {
				placements = append(placements, Placement{Task: lo[i].Task})
			}
			placements = append(placements, Placement{Task: mover.Task})
			for _, p := range hi {
				placements = append(placements, Placement{Task: p.Task})
			}
			for i := range placements {
				placements[i].Rank = keys[i]
			}
			return placements, nil
		}
		if k == half {
			break
		}
	}
	if singleErr != nil {
		return nil, fmt.Errorf("worklet: there is no key between %q and %q "+
			"and the %d rows around them share keys too: %w",
			lower, upper, RankRespreadInline, singleErr)
	}
	return []Placement{{Task: mover.Task, Rank: single[0]}}, nil
}

// windowSide is the k nearest rows of one side, capped at half, and the key
// of the first row outside them — empty when the side ran out, which is the
// project's own end.
func windowSide(side []Placement, k, half int) ([]Placement, Rank) {
	n := min(k, half, len(side))
	if n < len(side) {
		return side[:n], side[n].Rank
	}
	return side, ""
}

// longest is the length of the longest key.
func longest(keys []Rank) int {
	n := 0
	for _, k := range keys {
		n = max(n, len(k))
	}
	return n
}

// RespreadKeys is n keys, evenly spaced, all sharing one integer head.
//
// Not [KeysBetween]: its bisection crowds half the keys against the upper
// bound, inheriting its length, so the walk meant to SHORTEN keys would
// lengthen them. Instead, n uniformly spaced fractions under one fresh integer,
// each as short as the count allows (a three-digit fraction holds 238 328).
//
// Every key is strictly above head alone and below the next integer up, which
// is what makes the caller's "below the project's minimum" reserve enough.
func RespreadKeys(head Rank, n int) ([]Rank, error) {
	if n <= 0 {
		return nil, nil
	}
	integer, err := integerPart(string(head))
	if err != nil {
		return nil, err
	}

	// The narrowest fraction leaving a gap of at least two between keys: the
	// last digit is nudged off '0', and a gap of one would let the nudge
	// overtake the next key.
	span := uint64(1)
	width := 0
	for span < uint64(2*(n+1)) {
		span *= uint64(len(RankDigits))
		width++
		if width > 10 {
			return nil, fmt.Errorf("worklet: %d keys need a fraction wider "+
				"than ten symbols, which is more positions than a project can "+
				"hold — the order needs rebuilding rather than re-spreading", n)
		}
	}

	keys := make([]Rank, 0, n)
	for i := range n {
		value := span * uint64(i+1) / uint64(n+1)
		fraction := renderFraction(value, width)
		keys = append(keys, Rank(integer+fraction))
	}
	return keys, nil
}

// renderFraction writes a value in the rank alphabet, fixed width, never
// ending in '0' (the minting rule, see [midpoint]).
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

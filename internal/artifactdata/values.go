package artifactdata

import (
	"fmt"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE VALUE RULES, one per column type, and the bounds each is held to. Writes
// are strict and reads are lenient: a stored value that no longer matches its
// column (a value written before the column was retyped) is returned as it is
// and marked, never dropped.
//
// These bounds are the datastore's own, beside the leaf's limits table, because
// they describe one column type's values rather than what a store, a batch or a
// record may hold. Each is a judgement with its reason at its definition.
const (
	// DefaultTextLength and MaxTextLength bound a single-line text value, in
	// characters: a title or a label is under a thousand, and four thousand
	// is a paragraph somebody should have put in a long-text column.
	DefaultTextLength = 1000
	MaxTextLength     = 4000

	// DefaultLongTextLength and MaxLongTextLength bound a long-text value,
	// in characters: twenty thousand is a few pages of notes, and sixty
	// thousand keeps one long-text field inside the 64 KiB document cap
	// with room for the row's other fields.
	DefaultLongTextLength = 20_000
	MaxLongTextLength     = 60_000

	// DefaultMaxItems and MaxItemsCeiling bound a multiselect's chips: fifty
	// labels on one row is already unreadable.
	DefaultMaxItems = 50
	MaxItemsCeiling = 200

	// MaxURLBytes bounds a url value, the length every browser accepts.
	MaxURLBytes = 2048

	// DefaultRatingMax and MaxRatingMax bound a rating column's scale.
	DefaultRatingMax = 5
	MaxRatingMax     = 10

	// MaxNumberText bounds a number's decimal text: a float carries
	// seventeen significant digits, and sixty-four characters is every
	// honest literal with room to spare, while a ten-thousand-digit one is
	// refused rather than parsed.
	MaxNumberText = 64

	// MaxValueDepth is how deeply a schemaless value nests: six levels is
	// any record a page keeps, and the bound keeps the field index and the
	// encoder's walk small.
	MaxValueDepth = 6

	// MaxOptions bounds a select, multiselect or status column's options.
	MaxOptions = 200

	// MaxLabelBytes bounds a column's or an option's label, and a
	// collection's title.
	MaxLabelBytes = 120
)

// StatusCategories are the categories every status option names one of.
var StatusCategories = []string{"todo", "in_progress", "done", "blocked"}

// optionIDPattern is an option's stable id; colorPattern a design-token hue
// name, never a literal colour.
var (
	// Compiled at first use, not at init: the code host's push hook is this
	// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
	optionIDPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	})
	// Compiled at first use, not at init: the code host's push hook is this
	// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
	colorPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	})
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// valueError is a value its column refuses, naming the column and what it
// accepts.
func valueError(col artifacts.Column, format string, args ...any) error {
	return fmt.Errorf("%w: %s (%s): %s", ErrInvalid, col.Key, col.Type, fmt.Sprintf(format, args...))
}

// checkValue holds one value to its column's type and bounds. v is never null:
// a null in a write removes the field before anything is checked.
func checkValue(col artifacts.Column, v canon.Value) error {
	switch col.Type {
	case artifacts.ColumnText:
		s, err := stringOf(col, v)
		if err != nil {
			return err
		}
		if strings.ContainsAny(s, "\r\n") {
			return valueError(col, "a text value is one line; use a long-text column for more")
		}
		return checkLength(col, s, DefaultTextLength, MaxTextLength)
	case artifacts.ColumnLongText:
		s, err := stringOf(col, v)
		if err != nil {
			return err
		}
		return checkLength(col, s, DefaultLongTextLength, MaxLongTextLength)
	case artifacts.ColumnNumber:
		f, err := numberOf(col, v)
		if err != nil {
			return err
		}
		if col.Min != nil && f < *col.Min {
			return valueError(col, "%s is below the minimum %v", v.Number, *col.Min)
		}
		if col.Max != nil && f > *col.Max {
			return valueError(col, "%s is above the maximum %v", v.Number, *col.Max)
		}
		return nil
	case artifacts.ColumnCheckbox:
		if v.Kind != canon.KindBool {
			return valueError(col, "a checkbox is true or false")
		}
		return nil
	case artifacts.ColumnSelect, artifacts.ColumnStatus:
		s, err := stringOf(col, v)
		if err != nil {
			return err
		}
		return liveOption(col, s)
	case artifacts.ColumnMultiSelect:
		if v.Kind != canon.KindArray {
			return valueError(col, "a multiselect is a list of option ids")
		}
		if len(v.Array) > maxItems(col) {
			return valueError(col, "%d options are chosen and the maximum is %d",
				len(v.Array), maxItems(col))
		}
		seen := map[string]bool{}
		for _, e := range v.Array {
			s, err := stringOf(col, e)
			if err != nil {
				return err
			}
			if seen[s] {
				return valueError(col, "option %q is chosen twice", s)
			}
			seen[s] = true
			if err := liveOption(col, s); err != nil {
				return err
			}
		}
		return nil
	case artifacts.ColumnPerson:
		s, err := stringOf(col, v)
		if err != nil {
			return err
		}
		if !iam.ValidSeatHandle(s) {
			return valueError(col, "%q is not a seat handle", s)
		}
		return nil
	case artifacts.ColumnDate:
		s, err := stringOf(col, v)
		if err != nil {
			return err
		}
		return checkDate(col, s)
	case artifacts.ColumnURL:
		s, err := stringOf(col, v)
		if err != nil {
			return err
		}
		return checkURL(col, s)
	case artifacts.ColumnRating:
		if v.Kind != canon.KindNumber {
			return valueError(col, "a rating is a whole number")
		}
		n, err := strconv.Atoi(v.Number)
		if err != nil {
			return valueError(col, "a rating is a whole number, and %s is not", v.Number)
		}
		top := ratingMax(col)
		if n < 1 || n > top {
			return valueError(col, "a rating is 1..%d, and %d is not", top, n)
		}
		return nil
	}
	return valueError(col, "this build writes no %q column", col.Type)
}

// checkLength holds a string to its column's length in characters.
func checkLength(col artifacts.Column, s string, def, ceiling int) error {
	limit := def
	if col.MaxLength > 0 {
		limit = min(col.MaxLength, ceiling)
	}
	if n := utf8.RuneCountInString(s); n > limit {
		return valueError(col, "the value is %d characters and the maximum is %d", n, limit)
	}
	return nil
}

// stringOf is a string value, refusing any other kind and the NUL character.
func stringOf(col artifacts.Column, v canon.Value) (string, error) {
	if v.Kind != canon.KindString {
		return "", valueError(col, "the value is a string")
	}
	if strings.ContainsRune(v.String, 0) {
		return "", valueError(col, "the value carries a NUL character")
	}
	return v.String, nil
}

// numberOf is a finite number value.
func numberOf(col artifacts.Column, v canon.Value) (float64, error) {
	if v.Kind != canon.KindNumber {
		return 0, valueError(col, "the value is a number")
	}
	f, err := finite(v.Number)
	if err != nil {
		return 0, valueError(col, "%v", err)
	}
	return f, nil
}

// finite parses a number's decimal text, refusing one that is too long to be
// honest or outside a float's finite range.
func finite(text string) (float64, error) {
	if len(text) > MaxNumberText {
		return 0, fmt.Errorf("a number is at most %d characters of decimal text", MaxNumberText)
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("%s is not a finite number", text)
	}
	return f, nil
}

// liveOption refuses an option id the column does not offer, or has archived.
func liveOption(col artifacts.Column, id string) error {
	i := slices.IndexFunc(col.Options, func(o artifacts.Option) bool { return o.ID == id })
	if i < 0 {
		return valueError(col, "%q is not one of its options", id)
	}
	if col.Options[i].Archived {
		return valueError(col, "option %q is archived; choose a live one", id)
	}
	return nil
}

// maxItems is a multiselect column's bound on chosen options.
func maxItems(col artifacts.Column) int {
	if col.MaxItems > 0 {
		return min(col.MaxItems, MaxItemsCeiling)
	}
	return DefaultMaxItems
}

// ratingMax is a rating column's top of scale.
func ratingMax(col artifacts.Column) int {
	if col.Max != nil {
		return int(*col.Max)
	}
	return DefaultRatingMax
}

// checkDate admits `YYYY-MM-DD` and RFC 3339 in UTC (ending `Z`), from the
// year 1900 to 2200.
func checkDate(col artifacts.Column, s string) error {
	var t time.Time
	var err error
	if datePattern.MatchString(s) {
		t, err = time.Parse(time.DateOnly, s)
	} else {
		if !strings.HasSuffix(s, "Z") {
			return valueError(col, "a date is YYYY-MM-DD or an RFC 3339 instant in UTC")
		}
		t, err = time.Parse(time.RFC3339Nano, s)
	}
	if err != nil {
		return valueError(col, "%q is not a date", s)
	}
	if t.Year() < 1900 || t.Year() > 2200 {
		return valueError(col, "a date is between the years 1900 and 2200")
	}
	return nil
}

// checkURL admits an absolute https or http URL with a host, at most
// [MaxURLBytes], carrying no whitespace or control character: never
// `javascript:` or `data:`.
func checkURL(col artifacts.Column, s string) error {
	if len(s) > MaxURLBytes {
		return valueError(col, "the URL is %d bytes and the maximum is %d", len(s), MaxURLBytes)
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return valueError(col, "a URL carries no whitespace or control character")
		}
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return valueError(col, "a URL is absolute, https or http, with a host")
	}
	return nil
}

// checkLoose holds a schemaless value to the shape every document keeps: at
// most [MaxValueDepth] levels deep, and no string carrying a NUL.
func checkLoose(field string, v canon.Value, depth int) error {
	if depth > MaxValueDepth {
		return fmt.Errorf("%w: %s nests more than %d levels", ErrInvalid, field, MaxValueDepth)
	}
	switch v.Kind {
	case canon.KindString:
		if strings.ContainsRune(v.String, 0) {
			return fmt.Errorf("%w: %s carries a NUL character", ErrInvalid, field)
		}
	case canon.KindNumber:
		if _, err := finite(v.Number); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrInvalid, field, err)
		}
	case canon.KindArray:
		for _, e := range v.Array {
			if err := checkLoose(field, e, depth+1); err != nil {
				return err
			}
		}
	case canon.KindObject:
		for _, m := range v.Members {
			if strings.ContainsRune(m.Key, 0) {
				return fmt.Errorf("%w: %s holds a key carrying a NUL character", ErrInvalid, field)
			}
			if err := checkLoose(field, m.Value, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// addDecimal is a + b, computed EXACTLY on their decimal text and written back
// as decimal text: an increment is applied on every data node, and a float sum
// would carry a rounding error (0.1 + 0.2) into a counter for ever.
func addDecimal(a, b string) (string, error) {
	x, ok := new(big.Rat).SetString(a)
	if !ok {
		return "", fmt.Errorf("%s is not a number", a)
	}
	y, ok := new(big.Rat).SetString(b)
	if !ok {
		return "", fmt.Errorf("%s is not a number", b)
	}
	sum := new(big.Rat).Add(x, y)
	if sum.IsInt() {
		out := sum.Num().String()
		if _, err := finite(out); err != nil {
			return "", err
		}
		return out, nil
	}
	// A SUM OF TWO DECIMALS IS A DECIMAL: its denominator divides a power of
	// ten, so it is exact at the larger of the two inputs' fractional
	// digits, which is how many it is written with.
	places := max(fractionDigits(a), fractionDigits(b))
	out := strings.TrimRight(strings.TrimRight(sum.FloatString(places), "0"), ".")
	if _, err := finite(out); err != nil {
		return "", err
	}
	return out, nil
}

// fractionDigits is how many digits after the point a decimal literal's value
// needs, its exponent applied: `2.50` needs two, `1e-3` three, `5e2` none.
func fractionDigits(s string) int {
	mantissa, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]
		e, err := strconv.Atoi(s[i+1:])
		if err == nil {
			exp = e
		}
	}
	frac := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		frac = len(mantissa) - i - 1
	}
	return max(frac-exp, 0)
}

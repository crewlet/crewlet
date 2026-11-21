package growth

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// MicrosPerUnit is one unit of a currency in micros: money inside the engine
// is int64 micros of an account's currency, never a float.
const MicrosPerUnit = 1_000_000

// amountPattern is a non-negative decimal amount with at most six decimals
// and twelve whole digits: 10^12 units in micros stays inside int64, and no
// budget or cap is a trillion of anything.
var amountPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,6})?$`)
})

// ErrAmount is a text that is not an amount.
var ErrAmount = errors.New("growth: not an amount")

// ParseAmount reads a decimal amount such as "50.00" into micros, exactly.
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !amountPattern().MatchString(s) {
		return 0, fmt.Errorf("%w: %q is a number of whole units with at most six decimals, such as 50.00",
			ErrAmount, s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %w", ErrAmount, s, err)
	}
	frac += strings.Repeat("0", 6-len(frac))
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %w", ErrAmount, s, err)
	}
	return w*MicrosPerUnit + f, nil
}

// pow10 is 10^n for the digits a currency may have, 0 to 6.
func pow10(n int) int64 {
	p := int64(1)
	for range n {
		p *= 10
	}
	return p
}

// ErrDigits is a currency exponent outside 0 to 6.
var ErrDigits = errors.New("growth: a currency's minor digits are 0 to 6")

// FormatAmount is micros as a decimal with exactly digits decimals, rounded
// toward zero: what an answer says in the account's currency, whose minor
// digits its vendor's own table gives (package doc, "Money").
func FormatAmount(micros int64, digits int) (string, error) {
	if digits < 0 || digits > 6 {
		return "", ErrDigits
	}
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	whole, frac := micros/MicrosPerUnit, micros%MicrosPerUnit
	if digits == 0 {
		return sign + strconv.FormatInt(whole, 10), nil
	}
	f := frac / pow10(6-digits)
	return fmt.Sprintf("%s%d.%0*d", sign, whole, digits, f), nil
}

// FromMinor is an amount in a currency's minor units (cents, yen) in micros,
// for a currency with digits minor digits. Overflow is an error.
func FromMinor(minor int64, digits int) (int64, error) {
	if digits < 0 || digits > 6 {
		return 0, ErrDigits
	}
	scale := pow10(6 - digits)
	if minor > (1<<63-1)/scale || minor < -(1<<63-1)/scale {
		return 0, fmt.Errorf("%w: %d minor units overflow", ErrAmount, minor)
	}
	return minor * scale, nil
}

// ToMinor is micros in a currency's minor units, rounded down: the amount a
// vendor that counts in minor units is sent, never more than was allowed.
func ToMinor(micros int64, digits int) (int64, error) {
	if digits < 0 || digits > 6 {
		return 0, ErrDigits
	}
	return floorDiv(micros, pow10(6-digits)), nil
}

// RoundDown is micros rounded down to a multiple of unit micros (a vendor's
// least billable unit), so the person is asked about the amount that is sent.
func RoundDown(micros, unit int64) int64 {
	if unit <= 1 {
		return micros
	}
	return floorDiv(micros, unit) * unit
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// currencyCode is an ISO 4217 alphabetic code's shape.
var currencyCode = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z]{3}$`) })

// ValidCurrency reports whether code has an ISO 4217 code's shape: three
// capital letters. Which currencies exist is each vendor's table's to say.
func ValidCurrency(code string) bool { return currencyCode().MatchString(code) }

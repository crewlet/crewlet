package runner

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/kept"
)

// readAlike reports whether a model reading the lines a brief and its session
// show of a and b, two calls rendered under opts, could take one for the other:
// the same tool, and arguments their lines do not tell apart. Symmetric; results
// are never compared, since a successful call's line shows none.
//
// PER VALUE, AS THE LINES SHOW IT, never by digest alone: a value past the
// ledger's budget ([ledger.ShowsWhole]) is not shown whole, and either call may
// appear only as a line, so a value not shown whole, or left out by its record
// ([kept.IsLeftOut]), may equal anything. Calls read apart only on a key in one
// and not the other (null reads as absent) or a key both show whole with values
// not alike. Calls whose arguments are not comparable ([argsComparable]) read
// alike: reading apart two a model takes for one loses the told call.
func readAlike(a, b ledger.Call, opts ledger.FormatOptions) bool {
	switch {
	case a.Name != b.Name:
		return false
	case !argsComparable(a) || !argsComparable(b) || a.Digest == b.Digest:
		return true
	}
	for key, bv := range b.Args {
		av := a.Args[key]
		if (av == nil) != (bv == nil) {
			return false
		}
		if bv != nil && ledger.ShowsWhole(av, opts) && ledger.ShowsWhole(bv, opts) && !valuesAlike(av, bv) {
			return false
		}
	}
	for key, av := range a.Args {
		if av != nil && b.Args[key] == nil {
			return false
		}
	}
	return true
}

// argsComparable reports whether call's arguments are ones a line shows value
// by value: not withheld, unfinished or unread, digested, non-nil, and not left
// out whole by their record ([kept.IsNotKept]).
//
// Withheld and unread are tested on their own flags, not on what the record
// cleared (sandbox's keptCall): their lines show no arguments whatever the call
// carries ([ledger.Call.Withheld], [ledger.Call.ArgsUnread]), so comparing any
// would read apart calls whose lines read alike. Nil arguments (a call made with
// none) read alike every call of the tool, which costs a line, never a call.
func argsComparable(call ledger.Call) bool {
	return !call.Withheld && !call.Unfinished && !call.ArgsUnread && call.Digest != "" && call.Args != nil &&
		!kept.IsNotKept(call.Args)
}

// valuesAlike reports whether a reader could take h and t, two argument values
// as the record decoded them ([tools.ReadArgs], numbers as literal text), for
// one: equal as JSON values, not spellings (7 and 7.0, 0 and -0), null keys read
// as absent, and a string alike the number or boolean it spells (`"7"` and 7),
// the safe reading for a model comparing characters.
//
// A value left out by its record ([kept.IsLeftOut]) reads alike any, at any
// depth: compared as its marker text, it would read apart values that may have
// been one. [BridgedCall.ArgsBytes] decides nothing: the markers say what a
// record left out, and the values it kept compare as any.
func valuesAlike(h, t any) bool {
	if kept.IsLeftOut(h) || kept.IsLeftOut(t) {
		return true
	}
	switch tv := t.(type) {
	case nil:
		return h == nil
	case map[string]any:
		hv, ok := h.(map[string]any)
		if !ok {
			return false
		}
		for key, v := range tv {
			if !valuesAlike(hv[key], v) {
				return false
			}
		}
		for key, v := range hv {
			if v != nil && tv[key] == nil {
				return false
			}
		}
		return true
	case []any:
		hv, ok := h.([]any)
		if !ok || len(hv) != len(tv) {
			return false
		}
		for i := range tv {
			if !valuesAlike(hv[i], tv[i]) {
				return false
			}
		}
		return true
	}
	if hs, ok := h.(string); ok {
		if ts, ok := t.(string); ok {
			return hs == ts
		}
	}
	// A number or boolean, either perhaps a string spelling one.
	if hn, ok := numberOf(h); ok {
		tn, ok := numberOf(t)
		return ok && hn == tn
	}
	if hb, ok := boolOf(h); ok {
		tb, ok := boolOf(t)
		return ok && hb == tb
	}
	return reflect.DeepEqual(h, t)
}

// numberOf is v as a number in canonical text ([canonicalNumber]): a decoded
// number (json.Number or a Go number), or a string that is a JSON number literal.
func numberOf(v any) (string, bool) {
	var text string
	switch n := v.(type) {
	case json.Number:
		text = string(n)
	case string:
		text = n
	case float64:
		text = strconv.FormatFloat(n, 'g', -1, 64)
	case float32:
		text = strconv.FormatFloat(float64(n), 'g', -1, 32)
	case int:
		text = strconv.FormatInt(int64(n), 10)
	case int64:
		text = strconv.FormatInt(n, 10)
	case int32:
		text = strconv.FormatInt(int64(n), 10)
	case uint:
		text = strconv.FormatUint(uint64(n), 10)
	case uint64:
		text = strconv.FormatUint(n, 10)
	case uint32:
		text = strconv.FormatUint(uint64(n), 10)
	default:
		return "", false
	}
	return canonicalNumber(text)
}

// canonicalNumber is text, a JSON number literal, as sign, significant digits
// and decimal exponent, shared by every literal of its value ("7", "7.0" and
// "0.7E1" all "7e0"); false where text is no JSON literal ("+7", "Inf", "0x10").
//
// Exact and linear in the text, never a float or big.Rat: a float rounds two
// 19-digit ids to one, and big.Rat.SetString computes the power, so 1e999999999
// would cost a billion-digit number. The exponent is a big.Int, which only adds.
func canonicalNumber(text string) (string, bool) {
	rest, negative := strings.CutPrefix(text, "-")
	whole, rest := leadingDigits(rest)
	if whole == "" || (len(whole) > 1 && whole[0] == '0') {
		return "", false
	}
	var fraction string
	if after, ok := strings.CutPrefix(rest, "."); ok {
		if fraction, rest = leadingDigits(after); fraction == "" {
			return "", false
		}
	}
	exponent := new(big.Int)
	if rest != "" {
		if rest[0] != 'e' && rest[0] != 'E' {
			return "", false
		}
		signed := rest[1:]
		unsigned := strings.TrimLeft(signed, "+-")
		if len(signed)-len(unsigned) > 1 {
			return "", false
		}
		digits, tail := leadingDigits(unsigned)
		if digits == "" || tail != "" {
			return "", false
		}
		exponent.SetString(signed, 10)
	}
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return "0", true
	}
	exponent.Sub(exponent, big.NewInt(int64(len(fraction))))
	trimmed := strings.TrimRight(digits, "0")
	exponent.Add(exponent, big.NewInt(int64(len(digits)-len(trimmed))))
	sign := ""
	if negative {
		sign = "-"
	}
	return sign + trimmed + "e" + exponent.String(), true
}

// leadingDigits splits s after its leading ASCII digits.
func leadingDigits(s string) (digits, rest string) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	return s[:end], s[end:]
}

// boolOf is v as a boolean: a decoded one, or a string "true" or "false".
func boolOf(v any) (value, ok bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		switch b {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

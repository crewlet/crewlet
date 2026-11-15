package runner

import (
	"encoding/json"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
)

// readAlike reports whether a model reading the line a brief tells t by,
// rendered under opts, could take it for h, a call the session it continues
// shows whole: the same tool, and arguments that read alike in t's line. Its
// result is never compared: a successful call's line carries none, and two
// calls of one tool whose arguments read alike read alike whatever they
// returned.
//
// PER ARGUMENT, AS THE LINE SHOWS IT, never by the calls' digests alone. A
// line shows a value as itself only where it fits the ledger's budget
// ([ledger.ShowsWhole]); a longer one is a rewrite or omitted by its size, so
// two writes to one path whose contents differ past the budget read alike in
// their lines, digests apart. So a value t's line does not show whole may
// equal anything, and h's and t's arguments read apart only where some key is
// in one and not the other (a key the line shows is seen, whatever its value)
// or t's line shows a value whole that h's does not read alike ([valuesAlike]).
// A key whose value is null is read as absent, on both sides: `"thread_ts":null`
// and no thread_ts at all say the same to a reader.
//
// ALIKE WHEREVER EITHER CALL'S ARGUMENTS ARE NOT KNOWN WHOLE: withheld
// ([ledger.Call.Withheld]), in flight ([ledger.Call.Unfinished]), kept less
// than whole by the run's record ([BridgedCall.ArgsBytes]), with no digest,
// nothing having recorded them, or recorded in text the engine could not read
// back (nil arguments, [tools.ReadArgs]), which its line draws as no arguments
// at all. Nothing says they read apart, and the one direction that costs
// anything is reading two calls apart that a model takes for one: listing a
// held call again that no told call could be taken for costs a line, while
// leaving one out loses the told call.
func readAlike(h, t BridgedCall, opts ledger.FormatOptions) bool {
	switch {
	case h.Name != t.Name:
		return false
	case !argsKnownWhole(h) || !argsKnownWhole(t) || h.Digest == t.Digest:
		return true
	}
	for key, tv := range t.Args {
		hv := h.Args[key]
		if (hv == nil) != (tv == nil) || (tv != nil && ledger.ShowsWhole(tv, opts) && !valuesAlike(hv, tv)) {
			return false
		}
	}
	for key, hv := range h.Args {
		if hv != nil && t.Args[key] == nil {
			return false
		}
	}
	return true
}

// argsKnownWhole reports whether call's arguments are known whole: neither
// withheld nor in flight, kept whole by the run's record and read back from
// it, and digested where they were admitted ([readAlike]).
//
// WITHHELD IS ITS OWN TEST, never left to the arguments and digest a withheld
// call's record clears (sandbox's keptCall): its line draws its arguments as
// sealed whatever the call carries ([ledger.Call.Withheld]), so a reader
// compares none of them, and were a record ever to keep some, comparing them
// would read apart two calls whose lines read alike, the one direction that
// loses a call.
func argsKnownWhole(call BridgedCall) bool {
	return !call.Withheld && !call.Unfinished && call.ArgsBytes == 0 && call.Digest != "" && call.Args != nil
}

// valuesAlike reports whether a reader could take h and t, two argument values
// as the run's record decoded them ([tools.ReadArgs], numbers kept as their
// literal's text), for one value: equal as JSON values, never as their
// spellings. So a number reads alike another of the same value however it is
// written (7 and 7.0, 100 and 1e2, 0 and -0); an object reads alike one with
// the same keys, a key whose value is null read as absent, each value alike;
// an array one of its length, each element alike; and a string reads alike a
// number or a boolean it spells (`"7"` and 7, `"true"` and true), the safe
// reading of a value a model compares by its characters. Two strings read
// alike only where they are the same text.
func valuesAlike(h, t any) bool {
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
	// A NUMBER OR A BOOLEAN, either of them perhaps a string spelling one,
	// read as the scalar is.
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

// numberOf is v's value as a number, in a canonical text two numbers share
// exactly where they are equal ([canonicalNumber]): a decoded number
// (json.Number, or a Go number a caller built the arguments with), or a string
// whose whole text is a JSON number literal.
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

// canonicalNumber is text, a JSON number literal, as a sign, its significant
// digits and a decimal exponent, the one text every literal of its value
// shares ("7", "7.0", "70e-1" and "0.7E1" all "7e0"), and false where text is
// no JSON number literal (strconv's "+7", "Inf" or "0x10" among them).
//
// EXACT AND LINEAR IN THE TEXT, never a big.Rat or a float: a float rounds two
// 19-digit ids to one value, which reads two calls alike that differ only
// there (the safe direction, but no rule), and big.Rat.SetString computes the
// power an exponent names, so a model's 1e999999999 would cost a
// billion-digit number to compare. The exponent is a big.Int, which only adds.
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

// boolOf is v as a boolean: a decoded boolean, or a string whose whole text is
// a JSON boolean literal.
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

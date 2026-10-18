package canon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"unicode/utf8"
)

// MaxDepth is the deepest nesting this encoder walks: arrays and objects inside
// one another at most this many levels.
//
// A BOUND ON THE ENCODER, NOT A DATASTORE RULE. The datastore holds documents
// to a much shallower depth of its own; this one exists so that a hostile value
// handed to any caller of the encoder costs a bounded stack and a bounded
// error, never a goroutine's whole stack.
const MaxDepth = 64

// Errors a caller tells apart.
var (
	// ErrInvalid is input that is not exactly one JSON value.
	ErrInvalid = errors.New("canon: not one well-formed JSON value")

	// ErrDuplicateKey is an object naming one key twice.
	ErrDuplicateKey = errors.New("canon: an object names a key twice")

	// ErrTooDeep is a value nested past the depth asked for.
	ErrTooDeep = errors.New("canon: the value is nested too deeply")
)

// Kind is what a decoded value is.
type Kind uint8

const (
	KindNull Kind = iota + 1
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

// Value is one decoded JSON value, in the shape the canonical encoder writes
// back. Numbers keep their decimal text; object members keep their keys
// sorted bytewise.
type Value struct {
	Kind    Kind
	Bool    bool
	Number  string // the literal decimal text, for KindNumber
	String  string
	Array   []Value
	Members []Member // sorted by Key, for KindObject
}

// Member is one key of an object and its value.
type Member struct {
	Key   string
	Value Value
}

// Parse decodes raw into a [Value], refusing anything that is not exactly one
// well-formed JSON value, an object naming a key twice, and nesting deeper than
// maxDepth (capped at [MaxDepth]).
func Parse(raw []byte, maxDepth int) (Value, error) {
	if maxDepth <= 0 || maxDepth > MaxDepth {
		maxDepth = MaxDepth
	}
	if !utf8.Valid(raw) {
		return Value{}, fmt.Errorf("%w: the bytes are not valid UTF-8", ErrInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parse(dec, 0, maxDepth)
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Value{}, fmt.Errorf("%w: more than one value", ErrInvalid)
	}
	return v, nil
}

// parse reads one value from dec at depth.
func parse(dec *json.Decoder, depth, maxDepth int) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return Value{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	switch t := tok.(type) {
	case nil:
		return Value{Kind: KindNull}, nil
	case bool:
		return Value{Kind: KindBool, Bool: t}, nil
	case json.Number:
		return Value{Kind: KindNumber, Number: t.String()}, nil
	case string:
		return Value{Kind: KindString, String: t}, nil
	case json.Delim:
		if depth >= maxDepth {
			return Value{}, fmt.Errorf("%w: past %d levels", ErrTooDeep, maxDepth)
		}
		switch t {
		case '[':
			out := Value{Kind: KindArray, Array: []Value{}}
			for dec.More() {
				elem, err := parse(dec, depth+1, maxDepth)
				if err != nil {
					return Value{}, err
				}
				out.Array = append(out.Array, elem)
			}
			if _, err := dec.Token(); err != nil {
				return Value{}, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			return out, nil
		case '{':
			out := Value{Kind: KindObject, Members: []Member{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return Value{}, fmt.Errorf("%w: %w", ErrInvalid, err)
				}
				key, ok := keyTok.(string)
				if !ok {
					return Value{}, fmt.Errorf("%w: an object key is not a string", ErrInvalid)
				}
				elem, err := parse(dec, depth+1, maxDepth)
				if err != nil {
					return Value{}, err
				}
				out.Members = append(out.Members, Member{Key: key, Value: elem})
			}
			if _, err := dec.Token(); err != nil {
				return Value{}, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			slices.SortStableFunc(out.Members, func(a, b Member) int {
				return compareKeys(a.Key, b.Key)
			})
			for i := 1; i < len(out.Members); i++ {
				if out.Members[i].Key == out.Members[i-1].Key {
					return Value{}, fmt.Errorf("%w: %q", ErrDuplicateKey, out.Members[i].Key)
				}
			}
			return out, nil
		}
	}
	return Value{}, fmt.Errorf("%w: unexpected token %v", ErrInvalid, tok)
}

// compareKeys orders object keys bytewise, the order every node sorts by.
func compareKeys(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Encode writes v in canonical form.
func Encode(v Value) []byte {
	var b []byte
	return appendValue(b, v)
}

// Canonical is [Parse] then [Encode]: raw's canonical bytes.
func Canonical(raw []byte, maxDepth int) ([]byte, error) {
	v, err := Parse(raw, maxDepth)
	if err != nil {
		return nil, err
	}
	return Encode(v), nil
}

// Object encodes members as one canonical object, sorting them first. Two
// members naming one key are refused.
func Object(members []Member) ([]byte, error) {
	sorted := slices.Clone(members)
	slices.SortStableFunc(sorted, func(a, b Member) int { return compareKeys(a.Key, b.Key) })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Key == sorted[i-1].Key {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateKey, sorted[i].Key)
		}
	}
	return Encode(Value{Kind: KindObject, Members: sorted}), nil
}

// appendValue appends v's canonical bytes to b.
func appendValue(b []byte, v Value) []byte {
	switch v.Kind {
	case KindNull:
		return append(b, "null"...)
	case KindBool:
		return strconv.AppendBool(b, v.Bool)
	case KindNumber:
		return append(b, v.Number...)
	case KindString:
		return AppendString(b, v.String)
	case KindArray:
		b = append(b, '[')
		for i, e := range v.Array {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendValue(b, e)
		}
		return append(b, ']')
	case KindObject:
		b = append(b, '{')
		for i, m := range v.Members {
			if i > 0 {
				b = append(b, ',')
			}
			b = AppendString(b, m.Key)
			b = append(b, ':')
			b = appendValue(b, m.Value)
		}
		return append(b, '}')
	}
	// THE ZERO KIND is no value at all, which no parse produces; written as
	// null so the encoder is total rather than a panic on a caller's
	// mistake.
	return append(b, "null"...)
}

// AppendString appends s as a canonical JSON string.
//
// ONE ESCAPING RULE, written here rather than borrowed: a quote and a backslash
// are escaped, the five control characters with short forms use them, every
// other control character and U+2028 and U+2029 (which end a line in
// JavaScript) are \u escapes in lowercase hex, and everything else is written
// as the UTF-8 it is. Invalid UTF-8 cannot reach here: [Parse] refuses it, and
// a string built in Go is written rune by rune, an invalid byte as U+FFFD.
func AppendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for _, r := range s {
		switch {
		case r == '"':
			b = append(b, '\\', '"')
		case r == '\\':
			b = append(b, '\\', '\\')
		case r == '\b':
			b = append(b, '\\', 'b')
		case r == '\f':
			b = append(b, '\\', 'f')
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r == '\t':
			b = append(b, '\\', 't')
		case r < 0x20 || r == 0x2028 || r == 0x2029:
			b = append(b, '\\', 'u', hex[r>>12&0xf], hex[r>>8&0xf], hex[r>>4&0xf], hex[r&0xf])
		default:
			b = utf8.AppendRune(b, r)
		}
	}
	return append(b, '"')
}

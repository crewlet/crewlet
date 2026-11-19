package plainjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ErrTrailing is a document that is one JSON value followed by anything else —
// a second value, or the stray brace, comma or text a hand or a model leaves
// after one. A json.Decoder reads one value and stops, so a reader that wants
// exactly one asks again and answers this for whatever it finds; [Unreadable]
// says it, in words true of each.
var ErrTrailing = errors.New("something follows the JSON value")

// Unreadable is why a JSON document did not decode, as the person or the
// model that sent it reads it: which field, and what it holds where it takes
// something else. Never encoding/json's own words, which name Go's types and
// struct fields ("json: cannot unmarshal string into Go struct field
// patchBody.grants of type []iam.Grant"), nor the time package's layout.
//
// what is a SINGULAR noun phrase ("the body"); the sentence is lowercase with
// no full stop, so it composes like any Go error.
//
// ONE PLACE FOR EVERY SURFACE THAT DECODES A CALLER'S JSON: each answering
// err.Error() spoke a different dialect of Go's. A failure not recognised
// (a type's own decoder refusing a value) is said without its words.
func Unreadable(what string, err error) string {
	var (
		syntax    *json.SyntaxError
		wrongType *json.UnmarshalTypeError
		badTime   *time.ParseError
	)
	switch {
	case errors.Is(err, ErrTrailing):
		return what + " has more after its JSON value, which is all it may hold"
	case errors.Is(err, io.EOF):
		return what + " is empty"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return what + " isn't valid JSON: it ends too soon"
	case errors.As(err, &syntax) && syntax.Error() == unexpectedEnd && syntax.Offset == 0:
		// json.Unmarshal SAYS A DECODER'S io.EOF AND io.ErrUnexpectedEOF AS
		// ONE SyntaxError, so a surface that unmarshals told an empty body
		// "goes wrong at byte 0" where one that decodes said it was empty.
		return what + " is empty"
	case errors.As(err, &syntax) && syntax.Error() == unexpectedEnd:
		return what + " isn't valid JSON: it ends too soon"
	case errors.As(err, &syntax):
		return fmt.Sprintf("%s isn't valid JSON: it goes wrong at byte %d", what,
			syntax.Offset)
	case errors.As(err, &wrongType):
		holds := valueWords(wrongType.Value)
		if outOfRange(wrongType) {
			digits := strings.TrimPrefix(wrongType.Value, "number ")
			holds = digits + ", which is too large"
			if strings.HasPrefix(digits, "-") {
				// BELOW THE FIELD'S RANGE, a negative past an int's or
				// any negative into an unsigned one: "too large" said
				// the opposite.
				holds = digits + ", which is too small"
			}
			if wrongType.Field == "" {
				return what + " is " + holds
			}
			return fmt.Sprintf("in %s, %q is %s", what, wrongType.Field, holds)
		}
		takes := typeWords(wrongType.Type)
		if wrongType.Field == "" {
			return fmt.Sprintf("%s is %s where %s belongs", what, holds, takes)
		}
		return fmt.Sprintf("in %s, %q holds %s where %s belongs", what,
			wrongType.Field, holds, takes)
	case errors.As(err, &badTime):
		return fmt.Sprintf("in %s, the time %q isn't an RFC 3339 time like "+
			"2026-04-16T09:00:00Z", what, badTime.Value)
	}
	if field, ok := unknownField(err); ok {
		return fmt.Sprintf("in %s, %q isn't a field this takes", what, field)
	}
	return what + " holds a value this doesn't take"
}

// unknownField is the field a decoder refusing unknown fields named.
//
// READ OFF THE MESSAGE because encoding/json has no type for it: the
// decoder's DisallowUnknownFields answers `json: unknown field "owner"` as a
// bare error.
func unknownField(err error) (string, bool) {
	quoted, ok := strings.CutPrefix(err.Error(), `json: unknown field `)
	if !ok {
		return "", false
	}
	field, unquoteErr := strconv.Unquote(quoted)
	return field, unquoteErr == nil
}

// valueWords is what the document held, from encoding/json's description of
// it ("string", "number", "number 1.5", "bool", "array", "object").
func valueWords(value string) string {
	switch {
	case value == "string":
		return "text"
	case value == "number":
		return "a number"
	case strings.HasPrefix(value, "number "):
		return strings.TrimPrefix(value, "number ")
	case value == "bool":
		return "true or false"
	case value == "array":
		return "a list"
	case value == "object":
		return "an object"
	}
	return "a value"
}

// unexpectedEnd is json.Unmarshal's message for a document that ended before
// its value did, which it answers as a [json.SyntaxError] with no type of its
// own.
const unexpectedEnd = "unexpected end of JSON input"

// outOfRange reports a whole number outside what its field's type holds: the
// value is integer-shaped and the field takes a whole number, so "holds 1e30
// where a whole number belongs" would describe a value that IS one.
func outOfRange(e *json.UnmarshalTypeError) bool {
	digits, ok := strings.CutPrefix(e.Value, "number ")
	if !ok || strings.ContainsAny(digits, ".eE") {
		return false
	}
	switch kindOf(e.Type) {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

// typeWords is what a field takes, from its Go type.
//
// A TIME IS ITS FORM, never "an object": its type is a struct, and it is
// written as text.
func typeWords(t reflect.Type) string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[time.Time]() {
		return "an RFC 3339 time like 2026-04-16T09:00:00Z"
	}
	switch kindOf(t) {
	case reflect.String:
		return "text"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "a list"
	case reflect.Map, reflect.Struct:
		return "an object"
	}
	return "another value"
}

// kindOf is t's kind under every pointer, or Invalid for no type.
func kindOf(t reflect.Type) reflect.Kind {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return reflect.Invalid
	}
	return t.Kind()
}

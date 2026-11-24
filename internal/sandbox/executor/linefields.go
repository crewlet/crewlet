package executor

import (
	"strconv"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A RECORD IS READ BY THE FIELDS ITS WHOLE-LINE DECODE READ, and refused where
// that decode refused it: encoding/json fails a value of one kind read into a
// Go field of another, so a bounded walk's reader checks each member that
// decode named by its field's kind, whether or not it reads the value
// ([fieldKind]). A KEY MATCHES ITS NAME EXACTLY, AND ONE WRITTEN TWICE HOLDS ITS
// LAST VALUE, as the harness's own JSON.parse reads it, where encoding/json
// matched a name in any case and merged an object written twice: no harness
// writes either.

// fieldKind is the kind of Go field a whole-line decode read a member into:
// null goes into any, and so does every value into a json.RawMessage
// (fieldAny); a string into a string, true or false into a bool, and a number
// written as a whole int64 into an int or an int64.
type fieldKind uint8

const (
	fieldAny fieldKind = iota
	fieldString
	fieldBool
	fieldInt
)

// intLiteralBytes is the longest number written as a whole int64,
// "-9223372036854775808": encoding/json reads an int field's literal with
// strconv.ParseInt, and JSON writes no leading zero.
const intLiteralBytes = 20

// wordField is the word the string value k begins, "" for null, and whether a
// string field takes the value, passing over one it does not.
func (w *lineWalk) wordField(k jsonscan.Kind) (string, bool, error) {
	switch k {
	case jsonscan.String:
		word, err := w.word()
		return word, true, err
	case jsonscan.Null:
		return "", true, nil
	}
	return "", false, w.scan.Skip()
}

// flagField is the bool the value k is, and whether a bool field takes it.
func (w *lineWalk) flagField(k jsonscan.Kind) (bool, bool, error) {
	switch k {
	case jsonscan.True:
		return true, true, nil
	case jsonscan.False, jsonscan.Null:
		return false, true, nil
	}
	return false, false, w.scan.Skip()
}

// intField is the whole number the value k begins, and whether an int64
// field takes it.
func (w *lineWalk) intField(k jsonscan.Kind) (int64, bool, error) {
	switch k {
	case jsonscan.Null:
		return 0, true, nil
	case jsonscan.Number:
		raw, whole, err := w.scan.Capture(intLiteralBytes)
		if err != nil || !whole {
			return 0, false, err
		}
		n, parseErr := strconv.ParseInt(string(raw), 10, 64)
		return n, parseErr == nil, nil
	}
	return 0, false, w.scan.Skip()
}

// typedField reports whether a field of kind takes the value k begins,
// passing it over.
func (w *lineWalk) typedField(k jsonscan.Kind, kind fieldKind) (bool, error) {
	switch {
	case kind == fieldInt && k == jsonscan.Number:
		_, ok, err := w.intField(k)
		return ok, err
	case kind == fieldBool:
		_, ok, err := w.flagField(k)
		return ok, err
	}
	took := k == jsonscan.Null || kind == fieldAny || kind == fieldString && k == jsonscan.String
	return took, w.scan.Skip()
}

// typedObject reports whether a struct whose fields read its members of
// fields' names takes the value k begins, passing it over.
func (w *lineWalk) typedObject(k jsonscan.Kind, fields map[string]fieldKind) (bool, error) {
	switch k {
	case jsonscan.Null:
		return true, nil
	case jsonscan.ObjectStart:
	default:
		return false, w.scan.Skip()
	}
	took := true
	err := w.members(func(name string, k jsonscan.Kind) error {
		kind, read := fields[name]
		if !read {
			return w.scan.Skip()
		}
		ok, err := w.typedField(k, kind)
		took = took && ok
		return err
	})
	return took, err
}

// objectField reports whether a struct field takes the value k begins, an
// object read with each or null, passing over a value of another kind.
func (w *lineWalk) objectField(k jsonscan.Kind, each func(name string, k jsonscan.Kind) error) (bool, error) {
	switch k {
	case jsonscan.Null:
		return true, nil
	case jsonscan.ObjectStart:
		return true, w.members(each)
	}
	return false, w.scan.Skip()
}

// listField reports whether a slice field takes the value k begins, a list
// whose elements each reads or null, passing over a value of another kind.
func (w *lineWalk) listField(k jsonscan.Kind, each func(k jsonscan.Kind) error) (bool, error) {
	switch k {
	case jsonscan.Null:
		return true, nil
	case jsonscan.ArrayStart:
		return true, w.elements(func(k jsonscan.Kind) (bool, error) { return true, each(k) })
	}
	return false, w.scan.Skip()
}

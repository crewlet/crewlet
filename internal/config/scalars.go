package config

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// What each scalar of a document is decoded INTO, found before the decoder runs.
//
// # Why two readings need the Go type ahead of the decoder
//
// The decoder answers what a scalar became and never what it was about to
// become, and two readings have to be decided before it runs:
//
//   - a Tier A `${VAR}` is substituted before anything is decoded (see
//     [ParseBootstrap]), and what the substituted text MEANS depends on where
//     it lands. Into a text field it is text, whatever it looks like; into a
//     number or a switch it is read exactly as the same characters written
//     there would be, so `port: ${API_PORT}` is `port: 8080` and not the
//     string "8080" a number field refuses;
//   - a fractional number bound for an integer field is TRUNCATED by the
//     decoder without a word — `api.port: 8080.9` ran as 8080, and
//     `max_iterations: 3.7` as 3 — so it has to be refused before the
//     decoder gets the chance to drop what the author wrote.
//
// Both walk the node beside the type the decoder will read it into, with the
// decoder's own rules for which key lands in which field.

// unmarshalerType is the interface of a type whose YAML reading is its own.
var unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()

// durationType is the one integer kind the decoder reads from TEXT ("30s").
var durationType = reflect.TypeFor[time.Duration]()

// decodedAs is the type a value of t is decoded as — pointers stripped, as
// the decoder allocates through them — or nil where that reading is not the
// type's own fields: a custom unmarshaler decides the shape of everything
// below it (and re-enters [decodeKnown], which walks its own node), and an
// interface takes whatever it is given.
func decodedAs(t reflect.Type) reflect.Type {
	for t != nil {
		if t.Implements(unmarshalerType) || (t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(unmarshalerType)) {
			return nil
		}
		switch t.Kind() {
		case reflect.Pointer:
			t = t.Elem()
		case reflect.Interface:
			return nil
		default:
			return t
		}
	}
	return nil
}

// fieldFor is the type a struct decodes one key into, by yaml.v3's own rule:
// the tag's name, or the field's name lowercased, and a `,inline` struct's
// keys as the parent's. Nil for a key the struct does not define, which the
// decoder refuses itself.
func fieldFor(t reflect.Type, key string) reflect.Type {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if strings.Contains(","+opts+",", ",inline,") {
			switch inner := decodedAs(f.Type); {
			case inner == nil:
			case inner.Kind() == reflect.Struct:
				if got := fieldFor(inner, key); got != nil {
					return got
				}
			case inner.Kind() == reflect.Map:
				return inner.Elem()
			}
			continue
		}
		if f.PkgPath != "" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		if name == key {
			return f.Type
		}
	}
	return nil
}

// eachScalar walks node as the decoder will read it into t, handing visit
// every scalar with the type it lands in — nil where that is not known (see
// [decodedAs]) — and its authored path.
//
// An ALIAS is not followed: its anchor is visited where it is written, once,
// which is what a caller that rewrites a scalar in place needs — the same
// node reached twice would be rewritten twice.
func eachScalar(node *yaml.Node, t reflect.Type, path Path, visit func(n *yaml.Node, into reflect.Type, path Path)) {
	if node == nil {
		return
	}
	t = decodedAs(t)
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			eachScalar(child, t, path, visit)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			var into reflect.Type
			switch {
			case t == nil:
			case t.Kind() == reflect.Struct:
				into = fieldFor(t, key.Value)
			case t.Kind() == reflect.Map:
				into = t.Elem()
			}
			eachScalar(value, into, entry(path, key.Value), visit)
		}
	case yaml.SequenceNode:
		var into reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			into = t.Elem()
		}
		for i, child := range node.Content {
			eachScalar(child, into, idx(path, i), visit)
		}
	case yaml.ScalarNode:
		visit(node, t, path)
	}
}

// literalKind reports whether a value of t is read from a scalar as a NUMBER
// or a SWITCH — a Go kind whose text the decoder parses — rather than taken
// as text. A named type keeps its kind, so a `…Seconds int` is a number.
// time.Duration is an int64 the decoder reads from text ("30s"), so it is
// deliberately NOT one: its value is the text, and it stays text.
func literalKind(t reflect.Type) bool {
	if t == nil || t == durationType {
		return false
	}
	switch t.Kind() {
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// integerKind reports whether t holds whole numbers only.
func integerKind(t reflect.Type) bool {
	if t == nil {
		return false
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return t != durationType
	}
	return false
}

// refuseFractions is every fractional number node holds where t takes a whole
// one, as faults on the nodes that carry them — in the coordinates of node,
// which is what [decodeNode] hands back.
//
// The number is read by the decoder itself, so `1e3` is the whole number it
// is and `8080.9` is not, in every spelling YAML has for one. A scalar that
// is not a number at all is left to the decoder, which refuses it on its own.
func refuseFractions(node *yaml.Node, t reflect.Type) problems {
	var out problems
	eachScalar(node, t, nil, func(n *yaml.Node, into reflect.Type, _ Path) {
		if !integerKind(into) || n.ShortTag() != "!!float" {
			return
		}
		var f float64
		if n.Decode(&f) != nil || math.IsInf(f, 0) || math.IsNaN(f) || f == math.Trunc(f) {
			return
		}
		out = append(out, &Fault{Kind: ErrShape, pos: positionOf(n, false), Detail: fmt.Sprintf(
			"%s is not a whole number, and this setting counts whole units: the "+
				"fraction would be dropped rather than read, running it as %.0f. "+
				"Write the whole number you mean", n.Value, math.Trunc(f))})
	})
	return out
}

package cdp_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/crewlet/crewlet/internal/browser/cdp"
)

// protocol is a browser's own description of the protocol it speaks, as its
// /json/protocol answers it: every domain's types, commands and events.
type protocol struct {
	Domains []protocolDomain `json:"domains"`
}

type protocolDomain struct {
	Domain   string            `json:"domain"`
	Types    []protocolType    `json:"types"`
	Commands []protocolMessage `json:"commands"`
	Events   []protocolMessage `json:"events"`
}

// protocolMessage is a command (its parameters and returns) or an event (its
// parameters).
type protocolMessage struct {
	Name       string             `json:"name"`
	Parameters []protocolProperty `json:"parameters"`
	Returns    []protocolProperty `json:"returns"`
	Deprecated bool               `json:"deprecated"`
}

// protocolType is a named type of a domain: an object with properties, an
// array, or a primitive (a string with an enum, an integer).
type protocolType struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Items      *protocolProperty  `json:"items"`
	Properties []protocolProperty `json:"properties"`
	Deprecated bool               `json:"deprecated"`
}

// protocolProperty is a parameter, a return or a property: a primitive type,
// an array of items, or a reference to a named type; and whether a sender may
// leave it out.
type protocolProperty struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Ref        string            `json:"$ref"`
	Items      *protocolProperty `json:"items"`
	Optional   bool              `json:"optional"`
	Deprecated bool              `json:"deprecated"`
}

// describes holds every declared command and event to desc: each exists in
// its domain, and every field of its Go parameters and result — by its JSON
// name, recursively through named types — exists there with a JSON type the
// Go type reads and writes; and what a command sends has, at every depth, a
// field for each property the protocol requires, one always sent and never as
// null. With
// newest, nothing declared may be deprecated. It returns every disagreement,
// each naming the field's path.
func describes(desc protocol, newest bool) []string {
	c := &checker{desc: desc, newest: newest, domains: map[string]protocolDomain{}, seen: map[seenType]bool{}}
	for _, d := range desc.Domains {
		c.domains[d.Domain] = d
	}
	for _, d := range cdp.Inventory() {
		domain, name, _ := strings.Cut(d.Name, ".")
		dom, ok := c.domains[domain]
		if !ok {
			c.fail(d.Name, "its domain is not in this browser's protocol")
			continue
		}
		messages := dom.Commands
		if d.Event {
			messages = dom.Events
		}
		i := indexOf(messages, func(m protocolMessage) bool { return m.Name == name })
		if i < 0 {
			c.fail(d.Name, "is not in this browser's protocol")
			continue
		}
		m := messages[i]
		if m.Deprecated && newest {
			c.fail(d.Name, "is deprecated")
		}
		// A command's parameters are sent; an event's, like a result, are
		// read.
		c.object(d.Name+" params", d.Params, m.Parameters, domain, !d.Event)
		if !d.Event {
			c.object(d.Name+" result", d.Result, m.Returns, domain, false)
		}
	}
	return c.problems
}

type checker struct {
	desc     protocol
	newest   bool
	domains  map[string]protocolDomain
	problems []string
	// seen is every Go type already held to a named protocol type, so a
	// type that holds itself (a frame tree's children) is read once.
	seen map[seenType]bool
}

// seenType is a Go type held to a named protocol type, as the client sends it
// or as it reads it, which are held to different rules.
type seenType struct {
	goType  reflect.Type
	ref     string
	sending bool
}

func (c *checker) fail(path, format string, args ...any) {
	c.problems = append(c.problems, path+" "+fmt.Sprintf(format, args...))
}

// object holds the Go struct t to an object of props, every field by its JSON
// name. Where the client sends t — a command's parameters, and every object
// they hold — every property the protocol does not mark optional must also be
// a field of t, one encoding/json always writes, and never as null: a command
// lacking a parameter its browser requires is refused on every send; a
// required field tagged omitempty leaves the wire whenever it holds its zero
// value — a click at x 0, autoAttach false, a cookie's empty value — and is
// refused the same way, only on the day a caller sends that zero; and a
// required field whose zero value encoding/json writes as null — a nil slice,
// map, pointer or json.RawMessage — is refused as an invalid parameter on that
// day too (both pins refuse DOMSnapshot.captureSnapshot with computedStyles
// null, and Storage.setCookies with cookies null), which a List, written as []
// when nil, never is. What the client only reads may leave out whatever it
// does not read.
func (c *checker) object(path string, t reflect.Type, props []protocolProperty, domain string, sending bool) {
	if t.Kind() != reflect.Struct {
		c.fail(path, "is a Go %s, and the protocol says an object", t.Kind())
		return
	}
	declared := map[string]bool{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, options, ok := jsonField(f)
		if !ok {
			continue
		}
		declared[name] = true
		at := path + "." + name
		j := indexOf(props, func(p protocolProperty) bool { return p.Name == name })
		if j < 0 {
			c.fail(at, "is not in this browser's protocol")
			continue
		}
		if props[j].Deprecated && c.newest {
			c.fail(at, "is deprecated")
		}
		if sending && !props[j].Optional {
			switch {
			case dropsZero(f, options):
				c.fail(at, "is required by this browser's protocol, and its field is omitempty: its zero value is never sent")
			case writesNull(f.Type):
				c.fail(at, "is required by this browser's protocol, and its field's zero value is written as null, "+
					"which is no value of it")
			}
		}
		c.value(at, f.Type, props[j], domain, sending)
	}
	if !sending {
		return
	}
	for _, p := range props {
		if !p.Optional && !declared[p.Name] {
			c.fail(path+"."+p.Name, "is required by this browser's protocol, and the Go type sent has no such field")
		}
	}
}

// jsonField is f's name on the wire and its tag's options, and false for a
// field encoding/json never writes.
func jsonField(f reflect.StructField) (name, options string, ok bool) {
	if !f.IsExported() {
		return "", "", false
	}
	name, options, _ = strings.Cut(f.Tag.Get("json"), ",")
	switch name {
	case "-":
		return "", "", false
	case "":
		name = f.Name
	}
	return name, options, true
}

// dropsZero reports whether encoding/json leaves field f off the wire when it
// holds its zero value: under omitzero always, and under omitempty for every
// kind but a struct, which omitempty never finds empty, and an array of a
// length, which it finds empty only at length zero.
func dropsZero(f reflect.StructField, options string) bool {
	omitempty, omitzero := false, false
	for option := range strings.SplitSeq(options, ",") {
		omitempty = omitempty || option == "omitempty"
		omitzero = omitzero || option == "omitzero"
	}
	switch {
	case omitzero:
		return true
	case !omitempty:
		return false
	case f.Type.Kind() == reflect.Struct:
		return false
	case f.Type.Kind() == reflect.Array:
		return f.Type.Len() == 0
	}
	return true
}

// writesNull reports whether encoding/json writes t's zero value as null: a
// nil slice, map, pointer, interface or json.RawMessage does, unless t's own
// MarshalJSON writes something else, as a List's does. It asks encoding/json
// itself rather than restate its rules.
func writesNull(t reflect.Type) bool {
	b, err := json.Marshal(reflect.Zero(t).Interface())
	return err == nil && string(b) == "null"
}

var rawMessage = reflect.TypeFor[json.RawMessage]()

// value holds the Go type t to the protocol's p, as sent or as read.
func (c *checker) value(path string, t reflect.Type, p protocolProperty, domain string, sending bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if p.Ref != "" {
		refDomain, id := domain, p.Ref
		if d, name, ok := strings.Cut(p.Ref, "."); ok {
			refDomain, id = d, name
		}
		dom, ok := c.domains[refDomain]
		k := -1
		if ok {
			k = indexOf(dom.Types, func(ty protocolType) bool { return ty.ID == id })
		}
		if k < 0 {
			c.fail(path, "names the type %s, which this browser's protocol does not declare", p.Ref)
			return
		}
		ty := dom.Types[k]
		key := seenType{goType: t, ref: refDomain + "." + id, sending: sending}
		if c.seen[key] {
			return
		}
		c.seen[key] = true
		if ty.Deprecated && c.newest {
			c.fail(path, "is of the deprecated type %s", p.Ref)
		}
		switch {
		case ty.Type == "object" && len(ty.Properties) > 0:
			c.object(path, t, ty.Properties, refDomain, sending)
		case ty.Type == "array" && ty.Items != nil:
			c.value(path, t, protocolProperty{Type: "array", Items: ty.Items}, refDomain, sending)
		default:
			c.value(path, t, protocolProperty{Type: ty.Type}, refDomain, sending)
		}
		return
	}
	ok := false
	switch p.Type {
	case "any":
		ok = t == rawMessage || t.Kind() == reflect.Interface
	case "object":
		// An object the protocol declares no properties for is opaque: read
		// raw, or as a map of its values.
		ok = t == rawMessage || t.Kind() == reflect.Map && t.Key().Kind() == reflect.String
	case "string":
		ok = t.Kind() == reflect.String
	case "binary":
		ok = t != rawMessage && t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
	case "integer":
		switch t.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			ok = true
		}
	case "number":
		ok = t.Kind() == reflect.Float64 || t.Kind() == reflect.Float32
	case "boolean":
		ok = t.Kind() == reflect.Bool
	case "array":
		if t != rawMessage && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && p.Items != nil {
			c.value(path+"[]", t.Elem(), *p.Items, domain, sending)
			return
		}
	}
	if !ok {
		c.fail(path, "is a Go %s, and the protocol says %s", t, p.Type)
	}
}

func indexOf[T any](s []T, match func(T) bool) int {
	for i, v := range s {
		if match(v) {
			return i
		}
	}
	return -1
}

package config

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
)

// Carrying what this build cannot represent across a write.
//
// # Why a write has to carry fields it does not know
//
// A rolling upgrade puts two builds on one coordination store, and the
// activation pointer carries the payload, so an older node holds, byte for
// byte, a document a NEWER peer wrote: a seat, an MCP server or a whole block
// with a setting this build has no field for. Every write decodes that
// document into this build's [Company] somewhere, and a struct cannot hold
// what it has no field for. A write that stored the struct's encoding deleted
// the newer peer's setting and published the loss as the fleet's
// configuration, and every newer node reconciled onto it.
//
// Merging the struct's encoding back over the stored bytes keeps a field only
// where the merge reaches it, and a merge replaces a list wholesale. Most of a
// company is lists (its seats, its units, its MCP servers), so a merge-back
// lost the unknown keys of every member of every list it wrote.
//
// # What is carried
//
// Only a key THIS BUILD CANNOT DECODE, and only where the written document
// does not name it. Which keys those are is read off this build's own types:
// a key a struct has no JSON field for. A caller of this build can never name
// such a key (the strict reader refuses it), so nobody writing through this
// build can mean to remove one; a key this build knows and the write left out
// was left out on purpose, and stays out.
//
// # Where from
//
// The written document is walked by this build's types, every value it holds,
// beside the stored value it corresponds to where there is one: a struct field
// by its JSON name, a map entry by its key. A LIST MEMBER IS MATCHED BY
// IDENTITY, by the same rules as [Company.RestoreRedacted], because
// position is not identity: a seat by its handle and a unit by its name,
// anywhere in the document, so one that moved keeps its keys; an MCP server or
// a setup step by its name within its own list. An identity that is empty or
// held twice in the stored document matches nothing, since guessing which of
// two members a key belonged to is how one seat's setting reaches another. A
// list whose members have no identity carries nothing.
//
// Nothing below a value with a JSON encoding of its own is followed: its keys
// are its encoder's business rather than a struct's fields, and a walk that
// read them as fields would carry a key its decoder refuses. No type in this
// build encodes itself as an object today (a toggle is a boolean, a provider
// chain a string or a list), so the rule guards the next one that does.

// CarryUnknown writes into written every value of stored that this build
// cannot represent and written does not name. Both are company documents
// decoded from JSON as trees (objects, lists and scalars); written is changed in
// place, and stored is only read.
//
// The values carried are stored's own, not copies: the caller encodes written
// and discards both.
func CarryUnknown(stored, written map[string]any) {
	company := reflect.TypeFor[Company]()
	c := carrier{documentWide: indexStoredDocument(stored, company)}
	c.carry(stored, written, company)
}

// carrier walks a stored document beside a written one.
type carrier struct {
	// documentWide is, per [documentIdentified] member type, every identity
	// the stored document holds exactly once, mapped to that member.
	documentWide map[reflect.Type]map[string]any
}

// carry carries what stored holds and t cannot represent into written, and
// follows every value written holds.
//
// EVERY VALUE WRITTEN HOLDS, including one stored has no counterpart for,
// because a seat is matched by identity rather than by where it sits: a new
// unit can hold seats that are not new at all, and a seat moved into a unit
// that had no seats before is found only by walking the unit it is in now.
// stored is nil where there is no counterpart.
func (c *carrier) carry(stored, written any, t reflect.Type) {
	t = indirect(t)
	if encodesItself(t) {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		into, ok := written.(map[string]any)
		if !ok {
			return
		}
		from, _ := stored.(map[string]any)
		fields := jsonFields(t)
		for key, value := range from {
			if _, known := fields[key]; known {
				continue
			}
			if _, named := into[key]; !named {
				into[key] = value
			}
		}
		for key, value := range into {
			if field, known := fields[key]; known {
				c.carry(from[key], value, field)
			}
		}
	case reflect.Map:
		into, ok := written.(map[string]any)
		if !ok {
			return
		}
		from, _ := stored.(map[string]any)
		for key, value := range into {
			c.carry(from[key], value, t.Elem())
		}
	case reflect.Slice, reflect.Array:
		into, _ := written.([]any)
		from, _ := stored.([]any)
		member := indirect(t.Elem())
		match := c.matcher(member, from)
		for _, element := range into {
			prior, _ := match(element)
			c.carry(prior, element, member)
		}
	}
}

// matcher returns how a written member of a list of member finds the stored
// member it is. A member of a list whose members have no identity finds
// nothing.
func (c *carrier) matcher(member reflect.Type, stored []any) func(any) (any, bool) {
	switch {
	case member.Implements(documentIdentifiedType):
		index := c.documentWide[member]
		return func(element any) (any, bool) {
			key, ok := identityOfTree(element, member)
			if !ok {
				return nil, false
			}
			prior, found := index[key]
			return prior, found
		}
	case member.Implements(identifiedType):
		index := uniqueTreeMembers(stored, member)
		return func(element any) (any, bool) {
			key, ok := identityOfTree(element, member)
			if !ok {
				return nil, false
			}
			prior, found := index[key]
			return prior, found
		}
	}
	return func(any) (any, bool) { return nil, false }
}

// indexStoredDocument collects every [documentIdentified] member of a stored
// document tree, at any depth, by identity, leaving out an identity that is
// empty or not unique, for the reason [indexDocumentWide] gives.
func indexStoredDocument(root map[string]any, company reflect.Type) map[reflect.Type]map[string]any {
	seen := map[reflect.Type]map[string]any{}
	ambiguous := map[reflect.Type]map[string]bool{}
	var walk func(value any, t reflect.Type)
	walk = func(value any, t reflect.Type) {
		t = indirect(t)
		if encodesItself(t) {
			return
		}
		switch t.Kind() {
		case reflect.Struct:
			object, _ := value.(map[string]any)
			fields := jsonFields(t)
			for key, child := range object {
				if field, known := fields[key]; known {
					walk(child, field)
				}
			}
		case reflect.Map:
			object, _ := value.(map[string]any)
			for _, child := range object {
				walk(child, t.Elem())
			}
		case reflect.Slice, reflect.Array:
			list, _ := value.([]any)
			member := indirect(t.Elem())
			documentWide := member.Implements(documentIdentifiedType)
			if documentWide && seen[member] == nil {
				seen[member], ambiguous[member] = map[string]any{}, map[string]bool{}
			}
			for _, element := range list {
				if documentWide {
					key, ok := identityOfTree(element, member)
					if _, twice := seen[member][key]; twice || !ok || key == "" {
						ambiguous[member][key] = true
					}
					seen[member][key] = element
				}
				walk(element, member)
			}
		}
	}
	walk(root, company)
	for member, keys := range ambiguous {
		for key := range keys {
			delete(seen[member], key)
		}
	}
	return seen
}

// uniqueTreeMembers indexes one stored list of [identified] members by
// identity, leaving out an identity that is empty or held more than once.
func uniqueTreeMembers(list []any, member reflect.Type) map[string]any {
	index := make(map[string]any, len(list))
	ambiguous := map[string]bool{}
	for _, element := range list {
		key, ok := identityOfTree(element, member)
		if _, twice := index[key]; twice || !ok || key == "" {
			ambiguous[key] = true
		}
		index[key] = element
	}
	for key := range ambiguous {
		delete(index, key)
	}
	return index
}

// identityOfTree is a list member's identity, as its type derives it: the
// member decoded into member and asked. False for a member this build cannot
// read as one, which matches nothing.
func identityOfTree(element any, member reflect.Type) (string, bool) {
	raw, err := json.Marshal(element)
	if err != nil {
		return "", false
	}
	decoded := reflect.New(member)
	if json.Unmarshal(raw, decoded.Interface()) != nil {
		return "", false
	}
	named, ok := decoded.Elem().Interface().(identified)
	if !ok {
		return "", false
	}
	return named.IdentityKey(), true
}

// indirect is t with every pointer removed.
func indirect(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

var (
	jsonMarshalerType   = reflect.TypeFor[json.Marshaler]()
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// encodesItself reports a type whose JSON form is its own encoder's rather
// than its fields': a toggle, a provider chain, a timestamp. Nothing inside
// one is a field this walk can reason about, and an interface holds no type
// to reason with at all.
func encodesItself(t reflect.Type) bool {
	pointer := reflect.PointerTo(t)
	return t.Kind() == reflect.Interface ||
		t.Implements(jsonMarshalerType) || pointer.Implements(jsonMarshalerType) ||
		pointer.Implements(jsonUnmarshalerType) || pointer.Implements(textUnmarshalerType)
}

// jsonFieldsCache holds each struct type's JSON field names, which never
// change for the life of the process.
var jsonFieldsCache sync.Map

// jsonFields is every JSON key a struct type decodes, mapped to the type of
// the field that holds it, by encoding/json's own naming: the tag's name, the
// Go name when the tag gives none, no field for `-`, and the fields of an
// untagged embedded struct as the struct's own.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	if cached, ok := jsonFieldsCache.Load(t); ok {
		return cached.(map[string]reflect.Type)
	}
	fields := map[string]reflect.Type{}
	for i := range t.NumField() {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" && indirect(field.Type).Kind() == reflect.Struct {
			for key, embedded := range jsonFields(indirect(field.Type)) {
				if _, shadowed := fields[key]; !shadowed {
					fields[key] = embedded
				}
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	jsonFieldsCache.Store(t, fields)
	return fields
}

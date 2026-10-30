package toolargs_test

import (
	"maps"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// pageSchema is a save's: what it changes is named by the arguments it sends,
// and `status` is a closed set.
var pageSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"page":   map[string]any{"type": "string"},
		"body":   map[string]any{"type": "string"},
		"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"status": map[string]any{"type": "string", "enum": []any{"draft", "published"}},
	},
}

// A NULL IS LEFT OUT AT THE ENTRY (ADR-0059). The check reads a null as a
// missing value, and a tool that names what a call changes by the arguments
// it sends read one as a value: a null `body` emptied a page. So every
// argument sent as null whose schema does not declare null is left out, one
// the schema does not declare at all included, alone or beside a closed set's
// member made canonical in the same pass, and the caller's map keeps what it
// sent either way.
//
// Mutation: hand the canonical map on with its nulls, and `body`, `labels`
// and `bogus` reach the tool; leave them out of the caller's own map, and the
// call alone changes what the caller holds.
func TestANullArgumentIsLeftOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ sent, want map[string]any }{
		"alone": {
			sent: map[string]any{"page": "p1", "body": nil, "labels": nil, "bogus": nil},
			want: map[string]any{"page": "p1"},
		},
		"beside a closed set's member": {
			sent: map[string]any{"page": "p1", "status": " Draft", "body": nil},
			want: map[string]any{"page": "p1", "status": "draft"},
		},
	} {
		raw := maps.Clone(tc.sent)
		if got := toolargs.Of(pageSchema, raw).Map(); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Of handed on %v, want %v", name, got, tc.want)
		}
		if !reflect.DeepEqual(raw, tc.sent) {
			t.Errorf("%s: Of changed the caller's arguments to %v", name, raw)
		}
	}
}

// A NULL ITS SCHEMA DECLARES IS KEPT, because null is then the argument's
// value: an update's `due` and `points` are cleared by one. Declared either way
// a schema spells a list of types (a Go literal's []string, a decoded
// document's []any), or by the schema every undeclared member is held to; the
// string beside them, whose schema does not declare null, is the control.
//
// Mutation: leave out every null whatever its schema says, and `due`, `points`
// and `extra` are lost.
func TestANullItsSchemaDeclaresIsKept(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"due":    map[string]any{"type": []string{"string", "null"}},
			"points": map[string]any{"type": []any{"number", "null"}},
			"title":  map[string]any{"type": "string"},
		},
		"additionalProperties": map[string]any{"type": []any{"string", "null"}},
	}
	got := toolargs.Of(schema, map[string]any{
		"due": nil, "points": nil, "title": nil, "extra": nil,
	}).Map()
	if want := map[string]any{"due": nil, "points": nil, "extra": nil}; !reflect.DeepEqual(got, want) {
		t.Errorf("Of handed on %v, want %v", got, want)
	}
}

// A TOOL WITH NO SCHEMA HERE IS HANDED EVERY NULL AS SENT: a third-party MCP
// server holds its own schema, and what a null means is its to read.
//
// Mutation: leave the nil schema's nulls out with the rest, and `body` is lost.
func TestANilSchemaHandsEveryNullOnAsSent(t *testing.T) {
	t.Parallel()
	got := toolargs.Of(nil, map[string]any{"page": "p1", "body": nil}).Map()
	if want := map[string]any{"page": "p1", "body": nil}; !reflect.DeepEqual(got, want) {
		t.Errorf("Of handed on %v, want %v", got, want)
	}
}

// A NESTED NULL IS LEFT AS SENT: a custom field's value in `fields` is cleared
// by one, and an object argument's reader leaves out its own null members
// ([argval.Object]), so the entry touches the top level alone.
//
// Mutation: leave out nulls at every depth, and the field's clear is lost.
func TestANestedNullIsLeftAsSent(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"fields": map[string]any{"type": "object"},
			"waiting_on": map[string]any{"type": "object", "properties": map[string]any{
				"set": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			}},
			"labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	sent := func() map[string]any {
		return map[string]any{
			"fields":     map[string]any{"severity": nil},
			"waiting_on": map[string]any{"set": nil},
			"labels":     []any{"ops", nil},
		}
	}
	if got := toolargs.Of(schema, sent()).Map(); !reflect.DeepEqual(got, sent()) {
		t.Errorf("Of handed on %v, want %v", got, sent())
	}
}

// Package seatnames is how a record names a seat: by the handle the seat was
// CREATED under, which is its identity, and never by the handle it answers to
// now, which is only its address (ADR-0019).
//
// # Why a package of its own
//
// Two state-log domains store people — the work tracker and the knowledge base
// — and each wrote the handle a person answered to AT THE WRITE into every
// column that names somebody, and asked for the handle they answered to AT THE
// READ. A rename moves the address and keeps the seat, so a renamed seat's
// queue, inbox, watches and pins went empty, and its own remarks stopped being
// its own to edit. Both need the same two directions — a question and a write
// by identity, an answer shown by the current handle — over their own record
// and answer types, and the rule for WHICH values those are is where a second
// copy drifts: one domain learns that a list naming one seat twice is one
// member, or that a people value may be a list, and the other does not.
//
// # The tag is the classification
//
// A field that names somebody says so on its declaration, with the struct tag
// `person:"seat"` ([Tag]), and a [Walker] rewrites exactly those: a string, a
// pointer to one, a list, or a pointer to a list — a list deduplicated, since a
// set naming one seat under two spellings is one member. The tag sits in the
// one place a new field is written, rather than in a list beside the types
// that the next field would not reach; each domain's tests hold the names that
// say "somebody" to it. A shape a tag cannot state — a change record's text,
// a value whose meaning depends on a declaration — is an [Arm] the domain
// hands its walker.
//
// # Where it is applied, and where never
//
// AT A WRITER, before its decide reads anything, because a decide compares a
// caller's value with the rows — is this person already watching, is this
// their own remark — and a comparison across two spellings of one seat fails.
// And AT A READER, on the question on the way in and the answer on the way
// out. NEVER AT AN APPLIER: which seat a handle names is the chart's to say,
// and an applier that read a chart would write different rows on two nodes
// briefly on different epochs — which is why a RECORD carries the identity,
// resolved once, by its writer.
//
// # One reading per call
//
// A [Chart] is ONE reading of the org chart, and a domain holds one for the
// whole of one call. An answer names people in more than one pass — a board's
// column keys and the cards beneath them, a question's filter and the rows it
// matched — and a seam that asked the live chart afresh for every name let a
// rename landing mid-answer draw a card under a column spelled the other way.
// So a domain's seam hands out a reading ([Chart]) rather than answering
// names itself, and each call takes exactly one.
//
// A LEAF over reflect and sync, importing nothing of the engine's, so
// every domain that stores a person can import it.
package seatnames

import (
	"reflect"
	"slices"
	"sync"
)

// Tag is the struct tag a field naming somebody carries, as `person:"seat"`.
const Tag = "person"

// Chart is ONE reading of the org chart, answering both directions of a seat's
// name — see the package doc for why it is a reading and not the live chart.
//
// Declared here rather than by each domain because a domain's seam returns
// one: two consumers each declaring `Pin() <their own interface>` could not
// both be satisfied by the one implementation the engine hands them.
type Chart interface {
	// Identity is the handle the seat answering to handle was created
	// under, for any handle it answers to — its current one, the one it
	// was created under, or one a rename retired — and handle itself when
	// no seat answers to it.
	Identity(handle string) string

	// Current is the handle the seat created under identity answers to
	// now, and identity itself when no seat was.
	Current(identity string) string
}

// IdentityOf is one handle's identity, through a chart that may be nil — a
// build holding no chart, which records every name as it was given.
func IdentityOf(c Chart, handle string) string {
	if c == nil || handle == "" {
		return handle
	}
	return c.Identity(handle)
}

// CurrentOf is one identity's current handle, through a chart that may be nil.
func CurrentOf(c Chart, identity string) string {
	if c == nil || identity == "" {
		return identity
	}
	return c.Current(identity)
}

// Identified is v with every name w reaches rewritten to the seat's identity,
// through a chart that may be nil.
func Identified[T any](w *Walker, c Chart, v T) T {
	if c == nil {
		return v
	}
	return Rewrite(w, v, c.Identity)
}

// Shown is v with every name w reaches rewritten to the handle the seat
// answers to now, through a chart that may be nil.
func Shown[T any](w *Walker, c Chart, v T) T {
	if c == nil {
		return v
	}
	return Rewrite(w, v, c.Current)
}

// Arm is one type a [Walker] rewrites by a rule of the domain's own rather than
// by the tag: its Rewrite is handed a value of exactly Type and a name mapping
// that passes "nobody" through, and returns the value rewritten — a copy,
// never the value it was handed.
type Arm struct {
	Type    reflect.Type
	Rewrite func(v reflect.Value, name func(string) string) reflect.Value
}

// Walker rewrites every name a value holds under the tag, and every value of
// one of its arms by that arm's rule.
//
// ONE PER DOMAIN, built once, because which types can hold a name depends on
// the arms as well as on the tags, and the answer is remembered per walker.
type Walker struct {
	arms map[reflect.Type]func(reflect.Value, func(string) string) reflect.Value
	// reachable is reflect.Type → bool: whether a value of that type can
	// hold anything this walker rewrites, so everything that cannot is
	// handed back as it is, with no copy.
	reachable sync.Map
}

// NewWalker is a walker with these arms.
func NewWalker(arms ...Arm) *Walker {
	w := &Walker{arms: make(map[reflect.Type]func(reflect.Value,
		func(string) string) reflect.Value, len(arms))}
	for _, arm := range arms {
		w.arms[arm.Type] = arm.Rewrite
	}
	return w
}

// Rewrite is v with f applied to every name w reaches in it.
//
// IT NEVER WRITES THROUGH v. Every slice, map and pointer on the way to a
// rewritten value is COPIED, because the value a writer is handed is also the
// one a retried decide reads again, and one a caller may go on using after the
// call. A nil walker hands v back.
func Rewrite[T any](w *Walker, v T, f func(string) string) T {
	if w == nil {
		return v
	}
	named := Name(f)
	in := reflect.ValueOf(&v).Elem()
	if in.Kind() == reflect.Interface {
		// A VALUE HANDED IN AS AN INTERFACE is walked as whatever it holds,
		// and handed back as the same interface.
		if in.IsNil() || !w.Reaches(in.Elem().Type()) {
			return v
		}
		boxed := reflect.New(in.Type()).Elem()
		boxed.Set(w.walk(in.Elem(), named))
		out, ok := boxed.Interface().(T)
		if !ok {
			return v
		}
		return out
	}
	if !w.Reaches(in.Type()) {
		return v
	}
	out, ok := w.walk(in, named).Interface().(T)
	if !ok {
		return v
	}
	return out
}

// Name is f with the empty value passed through, so "nobody" stays nobody
// whatever the chart does with an empty string.
func Name(f func(string) string) func(string) string {
	return func(s string) string {
		if s == "" {
			return s
		}
		return f(s)
	}
}

// Names rewrites a set of people, keeping the first of any two that turn out
// to be one seat.
func Names(names []string, f func(string) string) []string {
	out := make([]string, 0, len(names))
	for _, each := range names {
		if mapped := f(each); mapped == "" || !slices.Contains(out, mapped) {
			out = append(out, mapped)
		}
	}
	return out
}

// HoldsNames reports a type a [Tag] may stand on: a string, a list of them, or
// a pointer to either — what [Walker] rewrites under the tag, and so what a
// domain's tag test holds every tagged field to.
func HoldsNames(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t.Kind() == reflect.String
}

// Reaches reports whether a value of type t can hold anything w rewrites.
func (w *Walker) Reaches(t reflect.Type) bool {
	return w.reachesFrom(t, map[reflect.Type]bool{})
}

func (w *Walker) reachesFrom(t reflect.Type, visiting map[reflect.Type]bool) bool {
	if held, ok := w.reachable.Load(t); ok {
		return held.(bool)
	}
	if visiting[t] {
		// A RECURSIVE TYPE answers for itself once its other fields have
		// been seen; the cycle adds nothing.
		return false
	}
	visiting[t] = true
	defer delete(visiting, t)
	var got bool
	if _, armed := w.arms[t]; armed {
		got = true
	} else {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			got = w.reachesFrom(t.Elem(), visiting)
		case reflect.Struct:
			for i := range t.NumField() {
				field := t.Field(i)
				if !field.IsExported() {
					continue
				}
				if _, tagged := field.Tag.Lookup(Tag); tagged ||
					w.reachesFrom(field.Type, visiting) {

					got = true
					break
				}
			}
		}
	}
	if len(visiting) == 1 || got {
		// ONLY A SETTLED ANSWER IS KEPT. A false one reached while a
		// cycle was open may be false only because the cycle was cut.
		w.reachable.Store(t, got)
	}
	return got
}

// walk returns v with f applied to every name it can reach, copying what it
// changes and sharing everything else.
func (w *Walker) walk(v reflect.Value, f func(string) string) reflect.Value {
	t := v.Type()
	if !w.Reaches(t) {
		return v
	}
	if arm, armed := w.arms[t]; armed {
		return arm(v, f)
	}
	switch t.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t.Elem())
		out.Elem().Set(w.walk(v.Elem(), f))
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(w.walk(v.Index(i), f))
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := range v.Len() {
			out.Index(i).Set(w.walk(v.Index(i), f))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), w.walk(it.Value(), f))
		}
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			if _, tagged := field.Tag.Lookup(Tag); tagged {
				out.Field(i).Set(rewriteTagged(v.Field(i), f))
				continue
			}
			out.Field(i).Set(w.walk(v.Field(i), f))
		}
		return out
	}
	return v
}

// rewriteTagged rewrites one tagged field: a string, a list of them, or a
// pointer to either. Anything else under the tag is a declaration mistake, and
// it is left alone rather than guessed at — each domain's tag test is what
// refuses it.
func rewriteTagged(v reflect.Value, f func(string) string) reflect.Value {
	t := v.Type()
	switch t.Kind() {
	case reflect.String:
		return reflect.ValueOf(f(v.String())).Convert(t)
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t.Elem())
		out.Elem().Set(rewriteTagged(v.Elem(), f))
		return out
	case reflect.Slice:
		if v.IsNil() || t.Elem().Kind() != reflect.String {
			return v
		}
		names := make([]string, 0, v.Len())
		for i := range v.Len() {
			names = append(names, v.Index(i).String())
		}
		mapped := Names(names, f)
		out := reflect.MakeSlice(t, len(mapped), len(mapped))
		for i, each := range mapped {
			out.Index(i).Set(reflect.ValueOf(each).Convert(t.Elem()))
		}
		return out
	}
	return v
}

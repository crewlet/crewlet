// Package jsoncarry keeps the members of a JSON object that a struct has no
// field for, and writes them back, so that a build decoding, changing and
// encoding an object it only half knows leaves the other half where the build
// that knows it put it.
//
// # Why an object carries what this build cannot read
//
// A record or a document shared between builds is decoded into this build's
// struct, changed and encoded again by whichever node handles it — a rolling
// upgrade makes that ordinary traffic. A member a newer build added and this
// one has no field for would be dropped by the decode and missing from the
// encode, so the node that wrote the object back would hold it without the
// member while every peer that knows it holds it with: one log, two sets of
// rows, and a peer that acts as though the member was never written. Evolution
// here is additive precisely so that an older build may pass over what it does
// not know, and this is what makes passing over it lossless.
//
// # How a type carries
//
// It holds what it does not know in a field the encoder skips,
//
//	Extra map[string]json.RawMessage `json:"-"`
//
// and routes its own MarshalJSON and UnmarshalJSON through [Marshal] and
// [Unmarshal], handing each the type REDECLARED WITHOUT ITS METHODS, so that
// the encoding/json call inside does not call back into the method it was
// called from:
//
//	func (t Task) MarshalJSON() ([]byte, error) {
//		type fields Task
//		return jsoncarry.Marshal(fields(t), t.Extra)
//	}
//
//	func (t *Task) UnmarshalJSON(b []byte) error {
//		type fields Task
//		return jsoncarry.Unmarshal(b, (*fields)(t), &t.Extra)
//	}
//
// # The contract
//
// A MEMBER IS KNOWN WHEN THE STRUCT DECODES IT, decided the way encoding/json
// decides it ([MembersOf]): the member names after Go's embedding rules, then a
// match on the exact name and failing that on the name case-folded as
// encoding/json folds it. A comparison that is stricter than the decoder's —
// exact where it folds, or folding differently — files a member the struct
// decoded in the carry as well, and the object is written with it twice.
//
// `null` CHANGES NOTHING, in the fields or in the carry, as encoding/json
// leaves a struct alone for it. Any other value that is not an object is an
// error, reported against the struct as encoding/json reports it.
//
// DECODING INTO A VALUE THAT ALREADY HOLDS MEMBERS MERGES, as encoding/json
// does: a member the bytes carry replaces what the value held for it, and one
// they do not carry is left as it was — in the fields and in the carry alike.
//
// A MEMBER THE STRUCT KNOWS, IN A SHAPE IT CANNOT DECODE, IS AN ERROR, never
// carried and never passed over. The name is the struct's, so a copy in the
// carry would be written over by the field's own value on the way out, and a
// field left zero would be written back as though it were the value — either
// way the object leaves this node without what a newer build wrote.
//
// A MEMBER THE STRUCT KNOWS IS NEVER WRITTEN FROM THE CARRY, even one the
// struct's own value leaves out: a field this build cleared is written absent,
// and a copy carried beside it would undo the clear.
//
// AN OBJECT CARRYING NOTHING ENCODES BYTE FOR BYTE AS ITS STRUCT DOES, so every
// object a build writes itself is the bytes it would be without the carry. An
// object carrying members encodes as its struct does, followed by the carried
// members in key order. The order a newer build writes them in is its own
// struct's, which a build without those fields cannot reproduce; what this
// build writes is the same bytes every time it writes the same object.
//
// THE CARRY IS PER OBJECT. An object nested in a carrying one is decoded
// through its own type, and keeps what it does not know only if that type
// carries too. So every object on a record shared between builds carries, at
// every depth — [github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest.Uncarried]
// is the walk that finds one that does not.
package jsoncarry

import (
	"cmp"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Unmarshal decodes raw into fields exactly as encoding/json would, and adds
// to *extra every member of raw that fields' type does not decode.
//
// T is the carrying type redeclared without its methods — see the package
// doc. A nested object on it is still decoded through its own methods.
func Unmarshal[T any](raw []byte, fields *T, extra *map[string]json.RawMessage) error {
	members := MembersOf(reflect.TypeFor[T]())
	// THE MEMBERS FIRST, because this is the decode that tells an object
	// from everything else: an object and `null` both decode into a map, and
	// nothing else does.
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		// Reported against T, which is what the caller decoded into, so
		// the carry is invisible in the error as well: it reads exactly
		// as decoding into T without a carry would.
		var mismatch *json.UnmarshalTypeError
		if errors.As(err, &mismatch) {
			mismatch.Type = reflect.TypeFor[T]()
		}
		return err
	}
	if err := json.Unmarshal(raw, fields); err != nil {
		return err
	}
	var carried map[string]json.RawMessage
	for name, value := range all {
		if members.Decodes(name) {
			continue
		}
		if carried == nil {
			// A COPY, so a value sharing its carry with another — a
			// struct copied before the decode — does not change the
			// other's.
			carried = maps.Clone(*extra)
			if carried == nil {
				carried = make(map[string]json.RawMessage)
			}
		}
		carried[name] = value
	}
	if carried != nil {
		*extra = carried
	}
	return nil
}

// Marshal encodes fields exactly as encoding/json would, followed by every
// member of extra that fields' type does not decode, in key order. With no
// such member the result is fields' own encoding, byte for byte.
//
// T is the carrying type redeclared without its methods — see the package
// doc.
func Marshal[T any](fields T, extra map[string]json.RawMessage) ([]byte, error) {
	members := MembersOf(reflect.TypeFor[T]())
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var carried map[string]json.RawMessage
	for name, value := range extra {
		if members.Decodes(name) {
			continue
		}
		if carried == nil {
			carried = make(map[string]json.RawMessage)
		}
		carried[name] = value
	}
	if carried == nil {
		return body, nil
	}
	// The carried members as encoding/json writes a map: in key order,
	// each value compacted and escaped as the struct's own are. A value that
	// is not JSON fails here, naming it — only a caller can have put one
	// there, since a decode keeps nothing but what it read.
	tail, err := json.Marshal(carried)
	if err != nil {
		return nil, fmt.Errorf("jsoncarry: encode the members a %v carries: %w",
			reflect.TypeFor[T](), err)
	}
	if len(body) == len("{}") {
		return tail, nil
	}
	out := make([]byte, 0, len(body)+len(tail))
	out = append(out, body[:len(body)-1]...)
	out = append(out, ',')
	return append(out, tail[1:]...), nil
}

// Members is the set of member names a struct type decodes.
type Members struct {
	exact  map[string]bool
	folded map[string]bool
}

// Decodes reports whether encoding/json decodes a member called name into a
// field of the type: its name exactly, or failing that its name case-folded
// as encoding/json folds it.
func (m *Members) Decodes(name string) bool {
	return m.exact[name] || m.folded[fold(name)]
}

// memberSets caches [MembersOf]: a set is read off a type once, and a type
// never changes.
var memberSets sync.Map // reflect.Type → *Members

// MembersOf is the set of member names encoding/json decodes into t's fields.
//
// READ OFF THE TYPE, as encoding/json reads it, rather than listed beside it:
// a list is right only until the next field is added, and a field missing from
// it would be decoded into its field and carried as well.
//
// It panics — at the first use, naming the type — on a type the carry cannot
// answer for:
//
//   - one that is not a struct;
//   - one where t or *t has an encoding of its own — a MarshalJSON, an
//     UnmarshalJSON or their text forms, declared or promoted from an
//     embedded field. Then the method and not the fields decides what the
//     object holds, and Marshal and Unmarshal handed such a type would call
//     straight back into the method they were called from;
//   - one with a tag the two implementations of encoding/json read
//     differently (see [memberNames]).
func MembersOf(t reflect.Type) *Members {
	if cached, ok := memberSets.Load(t); ok {
		return cached.(*Members)
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("jsoncarry: %v is not a struct, so it has no members to carry beside", t))
	}
	for _, own := range ownEncodings {
		if t.Implements(own) || reflect.PointerTo(t).Implements(own) {
			panic(fmt.Sprintf("jsoncarry: %v has an encoding of its own (%v) — hand "+
				"jsoncarry the type redeclared without its methods, `type fields %s`",
				t, own, t.Name()))
		}
	}
	names, err := memberNames(t)
	if err != nil {
		panic(fmt.Sprintf("jsoncarry: %v: %v", t, err))
	}
	members := &Members{exact: map[string]bool{}, folded: map[string]bool{}}
	for _, name := range names {
		members.exact[name] = true
		members.folded[fold(name)] = true
	}
	cached, _ := memberSets.LoadOrStore(t, members)
	return cached.(*Members)
}

var ownEncodings = []reflect.Type{
	reflect.TypeFor[json.Marshaler](),
	reflect.TypeFor[json.Unmarshaler](),
	reflect.TypeFor[encoding.TextMarshaler](),
	reflect.TypeFor[encoding.TextUnmarshaler](),
}

// memberNames is encoding/json's own resolution of a struct's member names:
// its fields and those of its embedded structs, breadth first, with Go's
// rules for a name that more than one of them claims.
//
// Every rule below is one the decoder applies, and each one a simpler walk
// gets wrong in the direction that loses data — a name the decoder drops,
// counted as known, is a member neither decoded nor carried:
//
//   - an unexported field is skipped, but an embedded struct is walked
//     whether its type is exported or not, since its fields may be;
//   - a field tagged exactly `json:"-"` is skipped, and `json:"-,"` is a
//     member named "-";
//   - an embedded struct with a tag name is a member of its own rather than
//     promoted;
//   - where fields claim one name, the shallowest wins, a tagged one beats
//     an untagged one at the same depth, and two at the same depth that
//     neither rule separates are BOTH dropped — as is every field of a
//     struct embedded twice at one depth.
//
// TWO IMPLEMENTATIONS OF encoding/json read these tags: the one built on
// encoding/json/v2, which the toolchain builds by default, and the original,
// which GOEXPERIMENT=nojsonv2 selects. They agree on every rule above for a
// tag whose name the original accepts and whose options are omitempty,
// omitzero and string. Outside that they part: a name with a character the
// original rejects is the field's Go name to it and not to the other, and an
// option such as `case:strict` changes which spellings the second matches.
// Such a tag is an error rather than an answer for one of them, since the
// carry must agree with whichever one is decoding.
func memberNames(t reflect.Type) ([]string, error) {
	type claim struct {
		depth  int
		tagged bool
	}
	claims := map[string][]claim{}
	var current, next []reflect.Type
	next = []reflect.Type{t}
	var count, nextCount map[reflect.Type]int
	visited := map[reflect.Type]bool{}
	for depth := 0; len(next) > 0; depth++ {
		current, next = next, nil
		count, nextCount = nextCount, map[reflect.Type]int{}
		for _, typ := range current {
			if visited[typ] {
				continue
			}
			visited[typ] = true
			for i := range typ.NumField() {
				field := typ.Field(i)
				if field.Anonymous {
					embedded := field.Type
					if embedded.Kind() == reflect.Pointer {
						embedded = embedded.Elem()
					}
					if !field.IsExported() && embedded.Kind() != reflect.Struct {
						continue
					}
				} else if !field.IsExported() {
					continue
				}
				tag := field.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, options, _ := strings.Cut(tag, ",")
				if name != "" && !validName(name) {
					return nil, fmt.Errorf("field %s is tagged with the name %q, which the two "+
						"implementations of encoding/json read differently", field.Name, name)
				}
				for option := range strings.SplitSeq(options, ",") {
					if !sharedOptions[option] {
						return nil, fmt.Errorf("field %s is tagged with the option %q, which the "+
							"two implementations of encoding/json read differently", field.Name, option)
					}
				}
				ft := field.Type
				if ft.Name() == "" && ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if name != "" || !field.Anonymous || ft.Kind() != reflect.Struct {
					tagged := name != ""
					if name == "" {
						name = field.Name
					}
					claims[name] = append(claims[name], claim{depth: depth, tagged: tagged})
					if count[typ] > 1 {
						// Embedded more than once at this depth: a second
						// claim, so the rule below drops the name.
						claims[name] = append(claims[name], claim{depth: depth, tagged: tagged})
					}
					continue
				}
				nextCount[ft]++
				if nextCount[ft] == 1 {
					next = append(next, ft)
				}
			}
		}
	}
	names := make([]string, 0, len(claims))
	for name, held := range claims {
		if len(held) > 1 {
			slices.SortFunc(held, func(a, b claim) int {
				if c := cmp.Compare(a.depth, b.depth); c != 0 {
					return c
				}
				switch {
				case a.tagged == b.tagged:
					return 0
				case a.tagged:
					return -1
				}
				return 1
			})
			if held[0].depth == held[1].depth && held[0].tagged == held[1].tagged {
				continue
			}
		}
		names = append(names, name)
	}
	return names, nil
}

// sharedOptions are the tag options both implementations of encoding/json
// read alike — none of which changes a member's name or how it is matched.
// The empty one is a tag with no options at all.
var sharedOptions = map[string]bool{"": true, "omitempty": true, "omitzero": true, "string": true}

// validName is whether the original encoding/json takes a tag's name as the
// member name rather than falling back to the field's.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// fold is a member name case-folded as encoding/json folds it when it matches
// a member to a field: an ASCII letter upper-cased, and every other rune
// replaced by the smallest rune of its Unicode simple-fold orbit. Two names
// fold alike exactly when [strings.EqualFold] holds for them, which neither
// case mapping gives: the long s (ſ) folds with "s" and lower-cases to
// itself, and the Kelvin sign (K) folds with "k" and upper-cases to itself.
func fold(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); {
		if c := name[i]; c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(name[i:])
		out = utf8.AppendRune(out, foldRune(r))
		i += n
	}
	return string(out)
}

// foldRune is the smallest rune in r's simple-fold orbit.
func foldRune(r rune) rune {
	for {
		next := unicode.SimpleFold(r)
		if next <= r {
			return next
		}
		r = next
	}
}

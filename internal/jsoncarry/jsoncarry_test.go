package jsoncarry_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/jsoncarry"
)

// ---- which members a struct decodes ------------------------------------ //

// The gallery: one struct per rule encoding/json applies to member names, each
// shaped so that a walk getting the rule wrong answers differently from the
// decoder.

type tagRules struct {
	Tagged     string `json:"tagged"`
	Options    int    `json:"options,omitempty,string"`
	GoName     string
	Skipped    string `json:"-"`
	Status     string `json:"status"`
	Kind       string `json:"kind"`
	unexported string
}

type Promoted struct {
	Shallow string `json:"shallow"`
	Deep    string
}

type hiddenType struct {
	Hidden string `json:"hidden"`
}

type Pointed struct {
	Pointer string `json:"pointer"`
}

type TaggedEmbed struct {
	Inside string `json:"inside"`
}

type Label string

type lowerLabel string

type embedRules struct {
	Promoted
	hiddenType
	*Pointed
	TaggedEmbed `json:"tagged_embed"`
	Label
	lowerLabel
}

type Left struct {
	Shared string `json:"shared"`
	Plain  string
	Won    string `json:"Winner"`
}

type Right struct {
	Shared string `json:"shared"`
	Plain  string
	Winner string
}

type Deeper struct {
	Left
}

type depthRules struct {
	Plain string `json:"shared"`
	Deeper
}

type Twice struct {
	Repeated string `json:"repeated"`
}

type FirstPath struct{ Twice }

type SecondPath struct{ Twice }

// embedding is a struct type embedding each of types, built at run time:
// the two shapes that need it — one name tagged the same way at one depth,
// and one struct embedded twice at one depth — are exactly the ones go vet
// refuses in a declaration, and the decoder's answer for them is the rule
// under test.
func embedding(own reflect.StructField, types ...reflect.Type) reflect.Type {
	fields := []reflect.StructField{own}
	for _, typ := range types {
		fields = append(fields, reflect.StructField{Name: typ.Name(), Type: typ, Anonymous: true})
	}
	return reflect.StructOf(fields)
}

var gallery = []reflect.Type{
	reflect.TypeFor[tagRules](),
	// `json:"-,"` is a member named "-", which a linter reads as a typo for
	// `json:"-"` — so it is built here rather than declared.
	reflect.StructOf([]reflect.StructField{
		{Name: "Dash", Type: reflect.TypeFor[string](), Tag: `json:"-,"`},
		{Name: "Skipped", Type: reflect.TypeFor[string](), Tag: `json:"-"`},
	}),
	reflect.TypeFor[embedRules](),
	reflect.TypeFor[depthRules](),
	embedding(reflect.StructField{Name: "Shallow", Type: reflect.TypeFor[string](), Tag: `json:"inside_left"`},
		reflect.TypeFor[Left](), reflect.TypeFor[Right]()),
	embedding(reflect.StructField{Name: "Own", Type: reflect.TypeFor[string](), Tag: `json:"own"`},
		reflect.TypeFor[FirstPath](), reflect.TypeFor[SecondPath]()),
}

// A MEMBER IS KNOWN EXACTLY WHEN ENCODING/JSON DECODES IT.
//
// The carry files everything else in Extra, so the two must agree for every
// name there is: a name the decoder takes that the carry calls unknown is
// decoded AND carried, and written twice; a name the decoder drops that the
// carry calls known is neither decoded nor carried, and gone. Every candidate
// name each gallery struct could plausibly answer to — each tag and Go name,
// in every case, with the long s and the Kelvin sign standing in for their
// ASCII fold-mates — is put to both, and the decoder itself is the judge.
func TestAMemberIsKnownExactlyWhenEncodingJSONDecodesIt(t *testing.T) {
	t.Parallel()
	for _, typ := range gallery {
		members := jsoncarry.MembersOf(typ)
		known := 0
		for _, name := range candidates(typ) {
			want := decodedByEncodingJSON(t, typ, name)
			if want {
				known++
			}
			if got := members.Decodes(name); got != want {
				t.Errorf("%v: Decodes(%q) = %v, and encoding/json %s it", typ, name, got,
					map[bool]string{true: "decodes", false: "does not decode"}[want])
			}
		}
		if known == 0 {
			t.Errorf("%v: no candidate is one the decoder takes — the gallery tests nothing", typ)
		}
	}
}

// candidates is every name a struct type could be addressed by: each tag name
// and Go name at any depth, re-cased and re-folded, plus names nothing has.
func candidates(typ reflect.Type) []string {
	var names []string
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for i := range typ.NumField() {
			field := typ.Field(i)
			tagName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			names = append(names, field.Name, tagName)
			inner := field.Type
			if inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			if field.Anonymous && inner.Kind() == reflect.Struct {
				walk(inner)
			}
		}
	}
	walk(typ)
	var out []string
	for _, name := range names {
		out = append(out, name, strings.ToUpper(name), strings.ToLower(name),
			strings.ReplaceAll(strings.ToLower(name), "s", "ſ"),
			strings.ReplaceAll(strings.ToLower(name), "k", "K"),
			name+"_newer")
		if name != "" {
			r := []rune(name)
			r[0] = unicode.ToUpper(r[0])
			out = append(out, string(r))
		}
	}
	out = append(out, "a_member_from_a_newer_build", "-", "")
	slices.Sort(out)
	return slices.Compact(out)
}

// decodedByEncodingJSON is the oracle: whether encoding/json, told to refuse
// a member it has no field for, takes this one.
func decodedByEncodingJSON(t *testing.T, typ reflect.Type, name string) bool {
	t.Helper()
	body, err := json.Marshal(map[string]any{name: nil})
	if err != nil {
		t.Fatalf("encode the probe for %q: %v", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(reflect.New(typ).Interface())
	switch {
	case err == nil:
		return true
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return false
	}
	t.Fatalf("%v refused %q for a reason other than not knowing it: %v", typ, name, err)
	return false
}

// AN UNEXPORTED FIELD IS NO MEMBER, on either side: encoding/json never
// writes one, and the carry does not claim one it could be handed — whether a
// field or an embedded non-struct type.
func TestAnUnexportedFieldIsNoMember(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]any{
		"field":    tagRules{Tagged: "t", unexported: "never written"},
		"embedded": embedRules{lowerLabel: "never written"},
	} {
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(string(out), "never written") {
			t.Errorf("%s: an unexported field was written: %s", name, out)
		}
		members := jsoncarry.MembersOf(reflect.TypeOf(v))
		for _, spelling := range []string{"unexported", "lowerLabel"} {
			if members.Decodes(spelling) {
				t.Errorf("%s: %q is claimed as a member", name, spelling)
			}
		}
	}
}

// A TYPE THE CARRY CANNOT ANSWER FOR IS REFUSED, at the first use and naming
// the type.
//
// One that encodes through a method of its own: the method, not the fields,
// decides what it holds, and a carry handed it would call back into the method
// it was called from — a method promoted from an embedded field counts, since
// encoding/json calls it all the same. And one with a tag the two
// implementations of encoding/json read differently, since the carry has to
// agree with whichever one is decoding.
func TestATypeTheCarryCannotAnswerForIsRefused(t *testing.T) {
	t.Parallel()
	for name, typ := range map[string]reflect.Type{
		"MarshalJSON":     reflect.TypeFor[ownsMarshal](),
		"UnmarshalJSON":   reflect.TypeFor[ownsUnmarshal](),
		"promoted":        reflect.TypeFor[embedsATime](),
		"not a struct":    reflect.TypeFor[map[string]string](),
		"UnmarshalText":   reflect.TypeFor[ownsText](),
		"pointer methods": reflect.TypeFor[ownsPointerMarshal](),
		// Its field decodes "Name" to the original encoding/json, which
		// falls back to the Go name, and "x" to the one built on
		// encoding/json/v2, which reads a name out of the tag. Built rather
		// than declared, since a linter refuses the tag it is about.
		"a name": reflect.StructOf([]reflect.StructField{
			{Name: "Name", Type: reflect.TypeFor[string](), Tag: `json:"x\\y"`},
		}),
		"an option":       reflect.TypeFor[unsharedOption](),
		"an embedded one": reflect.TypeFor[embedsUnshared](),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("MembersOf(%v) answered, and a carry over it would recurse", typ)
				}
			}()
			jsoncarry.MembersOf(typ)
		})
	}
}

type ownsMarshal struct{ A string }

func (ownsMarshal) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }

type ownsUnmarshal struct{ A string }

func (*ownsUnmarshal) UnmarshalJSON([]byte) error { return nil }

type ownsText struct{ A string }

func (*ownsText) UnmarshalText([]byte) error { return nil }

type ownsPointerMarshal struct{ A string }

func (*ownsPointerMarshal) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }

// unsharedOption's field decodes "A" to the original encoding/json and only
// "a" to the one built on encoding/json/v2.
type unsharedOption struct {
	A string `json:"a,case:strict"`
}

type embedsUnshared struct {
	unsharedOption
}

type embedsATime struct {
	time.Time
	A string
}

// ---- the round trip ---------------------------------------------------- //

// carrier is a type that carries, as every carrying type in the engine does.
type carrier struct {
	Name   string         `json:"name"`
	Count  int            `json:"count,omitempty"`
	Status string         `json:"status,omitempty"`
	Kind   string         `json:"kind,omitempty"`
	Inner  *innerCarrier  `json:"inner,omitempty"`
	Plain  *innerPlain    `json:"plain,omitempty"`
	Many   []innerCarrier `json:"many,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (c carrier) MarshalJSON() ([]byte, error) {
	type fields carrier
	return jsoncarry.Marshal(fields(c), c.Extra)
}

func (c *carrier) UnmarshalJSON(b []byte) error {
	type fields carrier
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

type innerCarrier struct {
	Depth int `json:"depth"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (c innerCarrier) MarshalJSON() ([]byte, error) {
	type fields innerCarrier
	return jsoncarry.Marshal(fields(c), c.Extra)
}

func (c *innerCarrier) UnmarshalJSON(b []byte) error {
	type fields innerCarrier
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// innerPlain is an object that does not carry.
type innerPlain struct {
	Depth int `json:"depth"`
}

func roundTrip(t *testing.T, raw string) (carrier, string) {
	t.Helper()
	var c carrier
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return c, string(out)
}

// A MEMBER A NEWER BUILD WROTE SURVIVES THIS BUILD'S READ-MODIFY-WRITE, BYTE
// FOR BYTE — at the top of the object and inside every object that carries.
//
// Byte for byte means the VALUE: an integer past 2^53 and an object's own key
// order survive, which a value that passed through `any` on the way would not.
// And the carry is per object — the object that does not carry drops its
// newer member, which is why every object on a shared record has to carry.
func TestAMemberANewerBuildWroteSurvivesAtEveryDepth(t *testing.T) {
	t.Parallel()
	const raw = `{"name":"n","later":{"b":9007199254740993,"a":[1,{"z":1,"y":2}]},` +
		`"inner":{"depth":1,"later":"in"},"many":[{"depth":2,"later":"el"}],` +
		`"plain":{"depth":3,"later":"lost"}}`
	c, out := roundTrip(t, raw)
	c.Name = "changed"
	changed, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("encode the changed object: %v", err)
	}
	const want = `{"name":"changed","inner":{"depth":1,"later":"in"},"plain":{"depth":3},` +
		`"many":[{"depth":2,"later":"el"}],"later":{"b":9007199254740993,"a":[1,{"z":1,"y":2}]}}`
	if string(changed) != want {
		t.Fatalf("the object changed and written back is\n  %s\nwant\n  %s", changed, want)
	}
	if !strings.Contains(out, `"later":{"b":9007199254740993,"a":[1,{"z":1,"y":2}]}`) {
		t.Errorf("the unchanged round trip lost the newer member's bytes: %s", out)
	}
}

// A MEMBER THE STRUCT DECODES IS NEVER CARRIED, however it is spelled.
//
// encoding/json matches a member to a field ignoring case, and folds as
// Unicode folds rather than as a case mapping does — the long s decodes into
// "status" and the Kelvin sign into "kind". A carry that compared more
// strictly would keep such a member as well as decode it, and the object
// would go out with it twice.
func TestAMemberTheStructDecodesIsNeverCarried(t *testing.T) {
	t.Parallel()
	for _, spelling := range []string{"NAME", "Name", "ſtatus", "STATUS", "\u212aind"} {
		c, out := roundTrip(t, `{"`+spelling+`":"v"}`)
		if len(c.Extra) != 0 {
			t.Errorf("%q was decoded and carried as well: %v", spelling, c.Extra)
		}
		if strings.Count(strings.ToLower(out), `"v"`) != 1 {
			t.Errorf("%q went out as %s — written more than once, or not at all", spelling, out)
		}
	}
}

// NULL CHANGES NOTHING, in the fields or in the carry — which is what
// encoding/json does to a struct for it.
func TestNullChangesNothing(t *testing.T) {
	t.Parallel()
	held := carrier{Name: "kept", Extra: map[string]json.RawMessage{"later": json.RawMessage(`1`)}}
	if err := json.Unmarshal([]byte(`null`), &held); err != nil {
		t.Fatalf("decode null: %v", err)
	}
	if held.Name != "kept" || string(held.Extra["later"]) != "1" {
		t.Errorf("null changed the value to %+v", held)
	}
	var outer struct {
		Inner innerCarrier `json:"inner"`
	}
	outer.Inner = innerCarrier{Depth: 4, Extra: map[string]json.RawMessage{"later": json.RawMessage(`2`)}}
	if err := json.Unmarshal([]byte(`{"inner":null}`), &outer); err != nil {
		t.Fatalf("decode a null member: %v", err)
	}
	if outer.Inner.Depth != 4 || string(outer.Inner.Extra["later"]) != "2" {
		t.Errorf("a null member changed the object under it to %+v", outer.Inner)
	}
}

// ANYTHING ELSE THAT IS NOT AN OBJECT IS REFUSED, as encoding/json refuses it
// for the struct — the carry is invisible in the error too.
func TestAValueThatIsNotAnObjectIsRefusedAgainstTheStruct(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`"text"`, `[1]`, `7`, `true`} {
		c := carrier{Name: "kept"}
		err := json.Unmarshal([]byte(raw), &c)
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			t.Errorf("%s decoded as a carrier (%v)", raw, err)
			continue
		}
		if mismatch.Type.Kind() != reflect.Struct || mismatch.Type.Name() != "fields" {
			t.Errorf("%s was refused against %v, not the struct it was decoded into", raw, mismatch.Type)
		}
		if c.Name != "kept" || c.Extra != nil {
			t.Errorf("a refused %s changed the value to %+v", raw, c)
		}
	}
	if err := json.Unmarshal([]byte(`{"name":`), new(carrier)); err == nil {
		t.Error("a truncated object decoded")
	}
}

// A MEMBER THE STRUCT KNOWS, IN A SHAPE IT CANNOT DECODE, IS AN ERROR — never
// carried, since the field's own value would be written over it, and never
// passed over, since the field's zero would then be written back as the value.
func TestAKnownMemberInAShapeThisBuildCannotReadIsAnError(t *testing.T) {
	t.Parallel()
	var c carrier
	err := json.Unmarshal([]byte(`{"name":{"first":"a","last":"b"},"later":1}`), &c)
	if err == nil {
		t.Fatalf("a name that is an object decoded, as %+v", c)
	}
	if _, carried := c.Extra["name"]; carried {
		t.Error("the member the struct owns was carried")
	}
}

// AN OBJECT CARRYING NOTHING IS ITS STRUCT'S OWN BYTES — including one whose
// carry holds only names the struct decodes, which are never written from it.
func TestAnObjectCarryingNothingEncodesAsItsStruct(t *testing.T) {
	t.Parallel()
	type fields carrier
	bare := carrier{Name: "n", Count: 2, Inner: &innerCarrier{Depth: 1}}
	want, err := json.Marshal(fields(bare))
	if err != nil {
		t.Fatalf("encode the struct: %v", err)
	}
	for name, extra := range map[string]map[string]json.RawMessage{
		"nothing":                nil,
		"an empty carry":         {},
		"only names it decodes":  {"NAME": json.RawMessage(`"stale"`), "count": json.RawMessage(`9`)},
		"a field it cleared too": {"status": json.RawMessage(`"stale"`)},
	} {
		c := bare
		c.Extra = extra
		got, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: encodes as\n  %s\nand its struct as\n  %s", name, got, want)
		}
	}
}

// AN OBJECT CARRYING MEMBERS IS ITS STRUCT'S BYTES, THEN THE CARRIED MEMBERS
// IN KEY ORDER — the same bytes every time this build writes the object.
func TestCarriedMembersFollowTheStructsOwnInKeyOrder(t *testing.T) {
	t.Parallel()
	c := carrier{Name: "n", Extra: map[string]json.RawMessage{
		"zeta": json.RawMessage(`{ "spaced" : "<b>" }`), "alpha": json.RawMessage(`1`),
	}}
	got, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"name":"n","alpha":1,"zeta":{"spaced":"\u003cb\u003e"}}`
	if string(got) != want {
		t.Errorf("encodes as\n  %s\nwant\n  %s", got, want)
	}
	empty, err := jsoncarry.Marshal(struct{}{}, map[string]json.RawMessage{"only": json.RawMessage(`true`)})
	if err != nil || string(empty) != `{"only":true}` {
		t.Errorf("a struct with no members of its own carrying one encodes as %s (%v)", empty, err)
	}
	c.Extra["broken"] = json.RawMessage(`{`)
	if out, err := json.Marshal(c); err == nil {
		t.Errorf("a carried member that is not JSON was written: %s", out)
	}
}

// DECODING INTO A VALUE THAT ALREADY HOLDS MEMBERS MERGES, as encoding/json
// does — and never writes into a carry another value shares.
func TestDecodingIntoAHeldValueMerges(t *testing.T) {
	t.Parallel()
	shared := map[string]json.RawMessage{"old": json.RawMessage(`1`)}
	original := carrier{Name: "kept", Count: 3, Extra: shared}
	c := original
	if err := json.Unmarshal([]byte(`{"count":4,"new":2}`), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Name != "kept" || c.Count != 4 {
		t.Errorf("the fields merged to %+v", c)
	}
	if string(c.Extra["old"]) != "1" || string(c.Extra["new"]) != "2" {
		t.Errorf("the carry merged to %v", c.Extra)
	}
	if _, leaked := original.Extra["new"]; leaked || len(shared) != 1 {
		t.Errorf("the decode wrote into a carry another value holds: %v", shared)
	}
}

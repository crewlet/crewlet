package jsoncarrytest_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/jsoncarry"
	"github.com/crewlet/crewlet/internal/jsoncarry/jsoncarrytest"
)

type walkRoot struct {
	Carried  *walkCarrier           `json:"carried"`
	Lists    []walkPlainInList      `json:"lists"`
	Values   map[string]walkInMap   `json:"values"`
	At       time.Time              `json:"at"`
	Address  walkAddress            `json:"address"`
	Skipped  walkSkipped            `json:"-"`
	Named    walkPlainEmbeddedNamed `json:"named"`
	Half     walkHalfCarrier        `json:"half"`
	Scope    walkEncodedScope       `json:"scope"`
	unwalked walkUnexported
	walkPromoted

	Extra map[string]json.RawMessage `json:"-"`
}

func (r walkRoot) MarshalJSON() ([]byte, error) {
	type fields walkRoot
	return jsoncarry.Marshal(fields(r), r.Extra)
}

func (r *walkRoot) UnmarshalJSON(b []byte) error {
	type fields walkRoot
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

type walkCarrier struct {
	Inside walkPlainInsideCarrier `json:"inside"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (c walkCarrier) MarshalJSON() ([]byte, error) {
	type fields walkCarrier
	return jsoncarry.Marshal(fields(c), c.Extra)
}

func (c *walkCarrier) UnmarshalJSON(b []byte) error {
	type fields walkCarrier
	return jsoncarry.Unmarshal(b, (*fields)(c), &c.Extra)
}

// walkHalfCarrier keeps nothing: it has the field and only one of the two
// methods.
type walkHalfCarrier struct {
	Extra map[string]json.RawMessage `json:"-"`
}

func (c walkHalfCarrier) MarshalJSON() ([]byte, error) {
	type fields walkHalfCarrier
	return jsoncarry.Marshal(fields(c), c.Extra)
}

// walkEncodedScope is written by a method of its own — as its terms, a bare
// list — and so is no object itself; the terms it writes are.
type walkEncodedScope struct {
	Terms []walkTermBehindMethod
}

func (s walkEncodedScope) MarshalJSON() ([]byte, error) { return json.Marshal(s.Terms) }

type (
	walkTermBehindMethod   struct{ K string }
	walkPlainInList        struct{ A string }
	walkInMap              struct{ B string }
	walkAddress            struct{ Kind, ID string }
	walkSkipped            struct{ C string }
	walkPlainEmbeddedNamed struct{ D string }
	walkUnexported         struct{ E string }
	walkPlainInsideCarrier struct{ F string }
	walkPromoted           struct {
		Promoted walkUnderPromoted `json:"promoted"`
	}
	walkUnderPromoted struct{ G string }
	walkNeverReached  struct{ H string }
)

// THE WALK FINDS EVERY OBJECT THAT DOES NOT CARRY, AND ONLY THOSE.
//
// It reaches objects in lists, in map values, inside an object that carries,
// under a struct embedded in the root (whose own fields are the root's, so it
// is not an object in its own right), behind a struct written by a method of
// its own, and one with half a carry. It passes over a time and the scope
// itself (values their methods write), a member the encoder skips, an
// unexported field, and a type the caller exempts; and it names an exempt type
// it never reached, so the exemption cannot outlive what it names.
func TestTheWalkFindsEveryObjectThatDoesNotCarry(t *testing.T) {
	t.Parallel()
	got := jsoncarrytest.Uncarried(map[reflect.Type]string{
		reflect.TypeFor[walkAddress]():      "an address",
		reflect.TypeFor[walkNeverReached](): "a declaration that went stale",
	}, reflect.TypeFor[walkRoot]())
	want := []string{
		"walkPlainEmbeddedNamed", "walkPlainInList", "walkInMap", "walkPlainInsideCarrier",
		"walkUnderPromoted", "walkNeverReached is exempt", "walkHalfCarrier", "walkTermBehindMethod",
	}
	for _, name := range want {
		if !slices.ContainsFunc(got, func(line string) bool { return strings.Contains(line, name) }) {
			t.Errorf("the walk did not report %s: %q", name, got)
		}
	}
	for _, name := range []string{
		"walkRoot ", "walkCarrier ", "Time", "walkSkipped", "walkUnexported", "walkPromoted ", "walkAddress",
		"walkEncodedScope ",
	} {
		if slices.ContainsFunc(got, func(line string) bool { return strings.Contains(line, name) }) {
			t.Errorf("the walk reported %s, which is not an object missing its carry: %q", name, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the walk reported %d, want %d: %q", len(got), len(want), got)
	}
	// What the walk passes over is what encoding/json never writes.
	out, err := json.Marshal(walkRoot{unwalked: walkUnexported{E: "never written"}})
	if err != nil || strings.Contains(string(out), "never written") {
		t.Errorf("an unexported field was written (%v): %s", err, out)
	}
}

type everyShape struct {
	S   string                     `json:"s,omitempty"`
	I   int64                      `json:"i,omitempty"`
	U   uint32                     `json:"u,omitempty"`
	F   float64                    `json:"f,omitempty"`
	B   bool                       `json:"b,omitempty"`
	P   *string                    `json:"p,omitempty"`
	L   []int                      `json:"l,omitempty"`
	M   map[string]int             `json:"m,omitempty"`
	R   json.RawMessage            `json:"r,omitempty"`
	X   map[string]json.RawMessage `json:"x,omitempty"`
	T   time.Time                  `json:"t,omitzero"`
	N   everyShapeNested           `json:"n,omitzero"`
	Rec *everyShape                `json:"rec,omitempty"`
	Out string                     `json:"-"`
}

type everyShapeNested struct {
	Inner string `json:"inner,omitempty"`
}

// A FILLED VALUE WRITES EVERY MEMBER ITS TYPE HAS, which is what makes bytes
// pinned from one a pin on every member — an omitted one would leave a member
// free to change encoding with no pinned byte moving. A member the encoder
// skips stays unset, and a type that contains itself ends.
func TestAFilledValueWritesEveryMember(t *testing.T) {
	t.Parallel()
	filled := jsoncarrytest.Filled[everyShape]()
	body, err := json.Marshal(filled)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatalf("decode: %v", err)
	}
	typ := reflect.TypeFor[everyShape]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if _, written := members[name]; !written && name != "-" {
			t.Errorf("the filled value leaves %q out: %s", name, body)
		}
	}
	if filled.Out != "" {
		t.Errorf("a member the encoder skips was set to %q", filled.Out)
	}
	if filled.Rec == nil || filled.Rec.Rec != nil || filled.Rec.S != "" {
		t.Errorf("the type that contains itself was filled as %+v", filled.Rec)
	}
	if !filled.T.Equal(jsoncarrytest.FilledAt) {
		t.Errorf("the time was filled as %v", filled.T)
	}
	// AND A TYPE KNOWN ONLY AT RUN TIME fills to the same value.
	byType := jsoncarrytest.FilledOf(reflect.TypeFor[everyShape]()).Interface().(*everyShape)
	again, err := json.Marshal(byType)
	if err != nil || string(again) != string(body) {
		t.Errorf("filled by type it writes %s (%v), want %s", again, err, body)
	}
}

// plantRoot carries, and holds carrying objects behind a pointer, in a list
// and in a map.
type plantRoot struct {
	Name   string                 `json:"name"`
	One    *walkCarrier           `json:"one"`
	List   []walkCarrier          `json:"list"`
	ByName map[string]walkCarrier `json:"by_name"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (r plantRoot) MarshalJSON() ([]byte, error) {
	type fields plantRoot
	return jsoncarry.Marshal(fields(r), r.Extra)
}

func (r *plantRoot) UnmarshalJSON(b []byte) error {
	type fields plantRoot
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

// lossyRoot is plantRoot as a build whose list elements do not carry reads it.
type lossyRoot struct {
	Name   string                 `json:"name"`
	One    *walkCarrier           `json:"one"`
	List   []walkPlainInList      `json:"list"`
	ByName map[string]walkCarrier `json:"by_name"`

	Extra map[string]json.RawMessage `json:"-"`
}

func (r lossyRoot) MarshalJSON() ([]byte, error) {
	type fields lossyRoot
	return jsoncarry.Marshal(fields(r), r.Extra)
}

func (r *lossyRoot) UnmarshalJSON(b []byte) error {
	type fields lossyRoot
	return jsoncarry.Unmarshal(b, (*fields)(r), &r.Extra)
}

const plantedFrom = `{"name":"n","one":{"inside":{"F":"f"}},"list":[{"inside":{}},{"inside":{}}],"by_name":{"k.1]":{"inside":{}}}}`

func roundTripAs[T any](raw []byte) ([]byte, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// recorder is a testing.TB that keeps what a check reported, so a case can
// assert the check fails where it should.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	panic(r)
}

func (r *recorder) run(check func(testing.TB)) {
	defer func() {
		if recovered := recover(); recovered != nil && recovered != r {
			panic(recovered)
		}
	}()
	check(r)
}

// A MEMBER PLANTED IN EVERY CARRYING OBJECT IS FOUND AGAIN — through a
// pointer, in each element of a list and under a map key that is not a
// plain word — and the check names the object that lost it, the top one
// included.
func TestSurvivesFindsEveryPlantedMemberAndNamesTheOneLost(t *testing.T) {
	t.Parallel()
	whole := &recorder{TB: t}
	whole.run(func(tb testing.TB) {
		jsoncarrytest.Survives(tb, reflect.TypeFor[plantRoot](), []byte(plantedFrom), roundTripAs[plantRoot])
	})
	if len(whole.failures) != 0 {
		t.Errorf("a round trip that carries everything failed the check: %q", whole.failures)
	}

	lossy := &recorder{TB: t}
	lossy.run(func(tb testing.TB) {
		jsoncarrytest.Survives(tb, reflect.TypeFor[plantRoot](), []byte(plantedFrom), roundTripAs[lossyRoot])
	})
	if len(lossy.failures) != 2 ||
		!strings.Contains(lossy.failures[0], ".list[0]") || !strings.Contains(lossy.failures[1], ".list[1]") {
		t.Errorf("a round trip dropping the list's members was reported as %q, want each element named", lossy.failures)
	}

	// THE TOP OBJECT IS PLANTED IN WHATEVER ITS TYPE: the round trip is what
	// carries it, so a round trip that does not is named even for a type
	// with no carry of its own.
	plain := &recorder{TB: t}
	plain.run(func(tb testing.TB) {
		jsoncarrytest.Survives(tb, reflect.TypeFor[walkPlainInList](), []byte(`{"A":"a"}`), roundTripAs[walkPlainInList])
	})
	if len(plain.failures) != 1 || !strings.Contains(plain.failures[0], "walkPlainInList came back") {
		t.Errorf("a round trip that drops the top object's member was reported as %q", plain.failures)
	}
}

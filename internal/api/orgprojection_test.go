package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// The org projection is ANONYMOUSLY READABLE, so what it carries is a security
// decision made one field at a time. These tests are what keep it one.

// exposure is the decision recorded for one authored field.
type exposure int

const (
	// exposurePublic: founder prose and structure, carried by /org.
	exposurePublic exposure = iota + 1
	// exposureGuarded: read only through the guarded `config` query.
	exposureGuarded
)

// classified is one field's decision and the reason for it. The reason is
// there for the next reader: a table of bare verdicts invites flipping one
// without knowing what it was protecting.
type classified struct {
	exposure exposure
	why      string
}

// A tracker project and a knowledge space are the decision here that is not
// about credentials: `project` and `space` hold neither a secret nor a ${VAR},
// and both are guarded anyway. A project key or a space key names a
// THIRD-PARTY CONTAINER, so handing one to an anonymous reader says where this
// company files its work and writes its pages, and what to go looking at. It
// is the reasoning internal/api/setupapi's package doc records for guarding
// even its reads: what a company has wired up, and to what, is a map of what
// to attack. They are read with the rest of the integration picture instead,
// through the operator-only `config` and `integrations` answers.

// roleFields classifies every field of config.Role.
var roleFields = map[string]classified{
	"Name":                 {exposurePublic, "the seat's identity in the chart"},
	"Kind":                 {exposurePublic, "agent or human, which the chart draws differently"},
	"Contact":              {exposureGuarded, "a person's account ids on external services"},
	"Availability":         {exposurePublic, "free text written to be read by colleagues"},
	"Handle":               {exposurePublic, "the declared slug every other surface addresses the seat by"},
	"Email":                {exposureGuarded, "a personal identifier, and it may hold a ${VAR} name"},
	"Unit":                 {exposureGuarded, "a placement reference; where a seat sits is derived, not authored prose"},
	"Goal":                 {exposurePublic, "founder prose"},
	"Backstory":            {exposurePublic, "founder prose"},
	"Responsibilities":     {exposurePublic, "founder prose"},
	"Manages":              {exposurePublic, "the reporting structure the chart draws"},
	"BehavioralGuidelines": {exposurePublic, "founder prose"},
	"Workers":              {exposureGuarded, "operational configuration of the delegate surface"},
	"TokenBudget":          {exposureGuarded, "operational configuration; the enforced cap has its own surface, /budgets"},
	"LLM":                  {exposureGuarded, "provider keys, which name the company's model accounts"},
	"LLMReview":            {exposureGuarded, "provider keys"},
	"LLMSubagent":          {exposureGuarded, "provider keys"},
	"LLMAuxiliary":         {exposureGuarded, "provider keys"},
	"LLMJudge":             {exposureGuarded, "provider keys"},
	"LLMSandbox":           {exposureGuarded, "provider keys"},
	"LearningEnabled":      {exposureGuarded, "an operational setting"},
	"MCPEnv":               {exposureGuarded, "tool credentials and their ${VAR} names"},
	"Sandbox":              {exposureGuarded, "setup commands and env, the usual home of a registry credential"},
	"Placement":            {exposureGuarded, "node ids and labels describing the deployment"},
	"Integrations":         {exposureGuarded, "vendor identities, bot tokens and signing secrets"},
	"Project":              {exposureGuarded, "a tracker project key: the container this seat files work in"},
	"Space":                {exposureGuarded, "a knowledge container key: where this seat writes pages"},
	"Schedules":            {exposureGuarded, "configured work, not structure; /schedules is the surface that describes it"},
}

// unitFields classifies every field of config.Unit.
var unitFields = map[string]classified{
	"Name":      {exposurePublic, "the unit's identity in the chart"},
	"ID":        {exposurePublic, "the unit's KEY: a `manages:` entry and a seat's `unit:` resolve it, so a client reading this projection needs it to resolve a reference the projection itself carries. The chart still draws Name"},
	"Type":      {exposurePublic, "an informational label"},
	"Purpose":   {exposurePublic, "founder prose"},
	"Lead":      {exposurePublic, "the structure the chart draws"},
	"Goals":     {exposurePublic, "founder prose"},
	"Channel":   {exposurePublic, "where the unit talks, which a colleague needs to know"},
	"Knowledge": {exposurePublic, "free-text references, not a read scope"},
	"MCPEnv":    {exposureGuarded, "tool credentials inherited by members"},
	"Project":   {exposureGuarded, "a tracker project key: the container this unit's work is filed in"},
	"Space":     {exposureGuarded, "a knowledge container key: where this unit's pages are written"},
	"Roles":     {exposurePublic, "the tree itself"},
	"Children":  {exposurePublic, "the tree itself"},
	"Schedules": {exposureGuarded, "configured work, not structure; /schedules is the surface that describes it"},
}

// EVERY AUTHORED FIELD HAS A DECISION, and the public ones are exactly what
// the projection carries.
//
// A field added to config.Role fails the first half until somebody decides
// whether an anonymous reader may see it. Marking it public without adding it
// to the projection (or the reverse) fails the second half, so the table and
// the type cannot drift apart.
func TestEveryOrgFieldIsClassified(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		authored   reflect.Type
		table      map[string]classified
		projection reflect.Type
	}{
		{reflect.TypeFor[config.Role](), roleFields, reflect.TypeFor[api.OrgSeat]()},
		{reflect.TypeFor[config.Unit](), unitFields, reflect.TypeFor[api.OrgUnit]()},
	} {
		t.Run(tc.authored.Name(), func(t *testing.T) {
			t.Parallel()
			var publicNames []string
			seen := map[string]bool{}
			for field := range fieldsOf(tc.authored) {
				seen[field.Name] = true
				decision, ok := tc.table[field.Name]
				if !ok {
					t.Errorf("config.%s.%s is not classified. Decide whether an anonymous "+
						"reader of /org may see it: add it to this test's table as public "+
						"(and to api.%s) or guarded (and read it through the config query)",
						tc.authored.Name(), field.Name, tc.projection.Name())
					continue
				}
				if decision.why == "" {
					t.Errorf("config.%s.%s is classified with no reason", tc.authored.Name(), field.Name)
				}
				if decision.exposure == exposurePublic {
					publicNames = append(publicNames, jsonName(field))
				}
			}
			for name := range tc.table {
				if !seen[name] {
					t.Errorf("the table classifies config.%s.%s, which no longer exists; "+
						"remove the entry", tc.authored.Name(), name)
				}
			}
			var projected []string
			for field := range fieldsOf(tc.projection) {
				projected = append(projected, jsonName(field))
			}
			slices.Sort(publicNames)
			slices.Sort(projected)
			if !slices.Equal(publicNames, projected) {
				t.Errorf("the public fields of config.%s are %v but api.%s carries %v; "+
					"the two must name the same wire fields",
					tc.authored.Name(), publicNames, tc.projection.Name(), projected)
			}
		})
	}
}

// NOTHING GUARDED REACHES AN ANONYMOUS READER, whatever it holds.
//
// Every guarded field of every seat and unit (and every field of the company
// outside its charter) is filled, all the way down through pointers, slices
// and maps, keys included: once with a distinct literal, once with a distinct
// whole ${VAR} reference. The projection is read the three ways a browser
// reaches it, and none may contain a fill value, a `${`, or any key outside
// the public shape. The public fields are filled with distinct prose that
// MUST appear, which is what stops this passing on an empty answer.
func TestTheOrgProjectionCarriesNothingGuarded(t *testing.T) {
	t.Parallel()
	for _, mode := range []fillMode{fillLiteral, fillReference} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			f := &filler{mode: mode}
			company := f.company()
			a := newApp(t, api.Options{
				Sources: queries.Sources{Company: companySource(t, company)},
			})

			res := fetch(t, a, "/org", nil)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET /org = %d, want an anonymous 200", res.StatusCode)
			}
			rest, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read /org: %v", err)
			}
			snapshot, err := json.Marshal(a.Stream().Snapshot()["org"])
			if err != nil {
				t.Fatalf("marshal the snapshot's org: %v", err)
			}
			push, err := json.Marshal(a.Stream().Org())
			if err != nil {
				t.Fatalf("marshal the org push: %v", err)
			}

			for surface, body := range map[string][]byte{
				"GET /org": rest, "snapshot": snapshot, "org push": push,
			} {
				text := string(body)
				if strings.Contains(text, "${") {
					t.Errorf("%s carries a ${VAR} reference:\n%s", surface, text)
				}
				for _, secret := range f.guardedValues {
					if strings.Contains(text, secret) {
						t.Errorf("%s carries the guarded value %q:\n%s", surface, secret, text)
					}
				}
				for _, prose := range f.publicValues {
					if !strings.Contains(text, prose) {
						t.Errorf("%s is missing the public value %q, so a public field "+
							"was dropped:\n%s", surface, prose, text)
					}
				}
				var decoded any
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Fatalf("%s is not JSON: %v", surface, err)
				}
				for key := range objectKeys(decoded) {
					if !publicKeys[key] {
						t.Errorf("%s carries the key %q, which is not in the public shape",
							surface, key)
					}
				}
			}
		})
	}
}

// publicKeys is every JSON key the public shape may contain.
var publicKeys = func() map[string]bool {
	out := map[string]bool{}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[api.OrgProjection](),
		reflect.TypeFor[api.OrgSeat](),
		reflect.TypeFor[api.OrgUnit](),
		// The derived hierarchy is the values above, resolved: handles,
		// names, unit names and the effective lead and channel. Its own
		// keys belong to the public shape for that reason.
		reflect.TypeFor[config.Derived](),
		reflect.TypeFor[config.DerivedSeat](),
		reflect.TypeFor[config.DerivedUnit](),
	} {
		for field := range fieldsOf(typ) {
			out[jsonName(field)] = true
		}
	}
	return out
}()

// fieldsOf yields a struct type's exported fields.
func fieldsOf(typ reflect.Type) func(func(reflect.StructField) bool) {
	return func(yield func(reflect.StructField) bool) {
		for i := range typ.NumField() {
			if field := typ.Field(i); field.IsExported() && !yield(field) {
				return
			}
		}
	}
}

func jsonName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if name == "" {
		return field.Name
	}
	return name
}

// objectKeys yields every object key anywhere in a decoded JSON value.
func objectKeys(v any) func(func(string) bool) {
	return func(yield func(string) bool) {
		var walk func(any) bool
		walk = func(v any) bool {
			switch v := v.(type) {
			case map[string]any:
				for key, child := range v {
					if !yield(key) || !walk(child) {
						return false
					}
				}
			case []any:
				for _, child := range v {
					if !walk(child) {
						return false
					}
				}
			}
			return true
		}
		walk(v)
	}
}

type fillMode string

const (
	fillLiteral   fillMode = "literal"
	fillReference fillMode = "reference"
)

// filler builds a company whose every value is recognisable.
type filler struct {
	mode fillMode
	n    int

	guardedValues []string
	publicValues  []string
}

// fillDepth bounds the walk into guarded values. The authored types are not
// recursive below a seat, so the bound is never reached by them; it exists so a
// future recursive type makes this test incomplete rather than hang.
const fillDepth = 12

func (f *filler) next() int { f.n++; return f.n }

func (f *filler) secret() string {
	var value string
	if f.mode == fillReference {
		value = fmt.Sprintf("${GUARDED_REFERENCE_%04d}", f.next())
	} else {
		value = fmt.Sprintf("guardedliteral%04d", f.next())
	}
	f.guardedValues = append(f.guardedValues, value)
	return value
}

func (f *filler) prose() string {
	value := fmt.Sprintf("publicprose%04d", f.next())
	f.publicValues = append(f.publicValues, value)
	return value
}

// company fills the charter with prose and everything else with secrets.
func (f *filler) company() *config.Company {
	c := &config.Company{}
	v := reflect.ValueOf(c).Elem()
	for i := range v.NumField() {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		switch field.Name {
		case "Name", "Mission", "Vision", "Policies":
			f.public(v.Field(i))
		case "Roles", "Units":
			// Filled below, through the classification tables.
		default:
			f.guarded(v.Field(i), 0)
		}
	}
	c.Roles = f.seats()
	c.Units = []config.Unit{f.unit(2)}
	return c
}

// agentOnlyRoleFields are refused on a HUMAN seat, and humanOnlyRoleFields on
// an agent one.
//
// TWO SEATS, NOT ONE, is what these force, and it is the fixture telling the
// truth rather than a concession: a company cannot have a seat carrying both
// sets, so a single filled seat could never have built an org at all. It did
// not have to while the projection was copied out of the document; it does
// now that the projection is DERIVED from the company the node runs.
var agentOnlyRoleFields = map[string]bool{
	"LLM": true, "LLMReview": true, "LLMSubagent": true, "LLMAuxiliary": true,
	"LLMJudge": true, "LLMSandbox": true, "Sandbox": true, "TokenBudget": true,
	"Workers": true, "LearningEnabled": true, "Schedules": true,
	"MCPEnv": true, "BehavioralGuidelines": true,
	// The per-seat vendor blocks and the two keys that ARE a vendor
	// identity — a person acts at a third-party app as themselves, so an
	// app or a project created for one would be a second identity for
	// somebody who already has one.
	"Integrations": true, "Project": true, "Space": true,
}

var humanOnlyRoleFields = map[string]bool{"Contact": true, "Availability": true}

// role fills one seat of the given kind, leaving out the fields the other
// kind owns.
func (f *filler) role(kind org.RoleKind) config.Role {
	var r config.Role
	v := reflect.ValueOf(&r).Elem()
	for field := range fieldsOf(v.Type()) {
		target := v.FieldByIndex(field.Index)
		switch {
		case field.Name == "Kind":
			// A CLOSED SET, not prose. `kind:` is public and it is an
			// enum, so the value that has to reach the projection is a
			// value the org model ACCEPTS — filled with prose, the org
			// refused to build and every seat vanished from /org with
			// nothing in the response to say why.
			target.SetString(string(kind))
		case kind == org.KindHuman && agentOnlyRoleFields[field.Name],
			kind == org.KindAgent && humanOnlyRoleFields[field.Name]:
			// Not this seat's to carry; the other kind's seat has it.
		case roleFields[field.Name].exposure == exposurePublic:
			f.public(target)
		default:
			f.guarded(target, 0)
		}
	}
	// AND THE GUARDED FIELDS THE ORG MODEL PARSES. A schedule's cron is a
	// five-field expression, its timezone an IANA name and its target a
	// closed set — none of them free text — so prose in any of them
	// refuses the whole company, and the projection is derived from the
	// company now. The schedule's own NAME and TASK stay guarded prose,
	// which is what this case is about; these three are the values it
	// stops asserting are hidden, because a valid cron has to be a valid
	// cron.
	for i := range r.Schedules {
		r.Schedules[i].Cron = "0 9 * * 1"
		r.Schedules[i].Timezone = "UTC"
		r.Schedules[i].Target = org.TargetEach
	}
	return r
}

// seats is one seat of each kind, so every public field of either reaches the
// projection and is asserted there.
func (f *filler) seats() []config.Role {
	f.publicValues = append(f.publicValues,
		string(org.KindAgent), string(org.KindHuman))
	return []config.Role{f.role(org.KindAgent), f.role(org.KindHuman)}
}

// unit fills one unit, with a seat and, above depth zero, a child unit.
func (f *filler) unit(depth int) config.Unit {
	var u config.Unit
	v := reflect.ValueOf(&u).Elem()
	for field := range fieldsOf(v.Type()) {
		target := v.FieldByIndex(field.Index)
		switch {
		case field.Name == "Roles" || field.Name == "Children":
			// Structure, filled below.
		case unitFields[field.Name].exposure == exposurePublic:
			f.public(target)
		default:
			f.guarded(target, 0)
		}
	}
	// THE PARSED FIELDS, on [filler.role]'s own reasoning: a unit carries
	// schedules too, and prose in a cron, a timezone or a target refuses
	// the whole company.
	for i := range u.Schedules {
		u.Schedules[i].Cron = "0 9 * * 1"
		u.Schedules[i].Timezone = "UTC"
		u.Schedules[i].Target = org.TargetEach
	}
	u.Roles = f.seats()
	if depth > 0 {
		u.Children = []config.Unit{f.unit(depth - 1)}
	}
	return u
}

// public fills a public field, which is a string or a list of strings.
func (f *filler) public(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(f.prose())
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		s.Index(0).SetString(f.prose())
		v.Set(s)
	default:
		panic(fmt.Sprintf("a public field of kind %s; teach the filler before classifying it", v.Kind()))
	}
}

// guarded fills every reachable value, keys included.
func (f *filler) guarded(v reflect.Value, depth int) {
	if depth > fillDepth || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(f.secret())
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		f.guarded(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				f.guarded(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		f.guarded(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		f.guarded(key, depth+1)
		value := reflect.New(v.Type().Elem()).Elem()
		f.guarded(value, depth+1)
		m.SetMapIndex(key, value)
		v.Set(m)
	}
}

// THE PROJECTION SHARES NO MEMORY WITH THE APPLIED COMPANY.
//
// The company is shared by every reader of the epoch, and the projection is
// handed to the socket hub and to REST callers. A projection that aliased the
// company's slices would let any of those holders rewrite the applied org for
// everyone else, so every list is copied on the way out.
func TestTheOrgProjectionDoesNotAliasTheCompany(t *testing.T) {
	t.Parallel()
	fresh := func() *config.Company {
		seat := func(name string) config.Role {
			return config.Role{
				Name:                 name,
				Responsibilities:     []string{name + " responsibility"},
				BehavioralGuidelines: []string{name + " guideline"},
				Manages:              []string{name + " report"},
			}
		}
		return &config.Company{
			Name:     "Acme",
			Policies: []string{"policy"},
			Roles:    []config.Role{seat("CEO")},
			Units: []config.Unit{{
				Name: "Engineering", Goals: []string{"goal"}, Knowledge: []string{"reference"},
				Roles: []config.Role{seat("CTO")},
				Children: []config.Unit{{
					Name: "Platform", Goals: []string{"child goal"}, Knowledge: []string{"child reference"},
					Roles: []config.Role{seat("Engineer")},
				}},
			}},
		}
	}
	company := fresh()
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: companySource(t, company)},
	})

	projection, ok := a.Stream().Org().(api.OrgProjection)
	if !ok {
		t.Fatalf("the org surface answers %T, want api.OrgProjection", a.Stream().Org())
	}
	scribble := func(lists ...[]string) {
		for _, list := range lists {
			for i := range list {
				list[i] = "rewritten through the projection"
			}
		}
	}
	scribbleSeats := func(seats []api.OrgSeat) {
		for i := range seats {
			scribble(seats[i].Responsibilities, seats[i].BehavioralGuidelines, seats[i].Manages)
		}
	}
	scribble(projection.Policies)
	scribbleSeats(projection.Roles)
	for _, unit := range projection.Units {
		scribble(unit.Goals, unit.Knowledge)
		scribbleSeats(unit.Roles)
		for _, child := range unit.Children {
			scribble(child.Goals, child.Knowledge)
			scribbleSeats(child.Roles)
		}
	}

	if !reflect.DeepEqual(company, fresh()) {
		t.Errorf("writing into the projection changed the applied company:\n%+v", company)
	}
}

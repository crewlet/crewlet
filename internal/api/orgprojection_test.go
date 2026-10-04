package api_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// The org projection is read by every `state:read` holder, an audience far
// wider than the configuration's, so what it carries is a security decision
// made one field at a time. These tests are what keep it one.

// exposure is the decision recorded for one authored field.
type exposure int

const (
	// exposurePublic: founder prose and structure, carried by /org to
	// every `state:read` reader.
	exposurePublic exposure = iota + 1
	// exposureGuarded: read only through the `config:read` answers.
	exposureGuarded
	// exposureResolved: read, with others, into the RESOLVED seat field
	// named by `into`, which carries the labels the value decides (a
	// provider key, a server name) and never the value's own contents —
	// and which only a `config:read` audience is handed, because the labels
	// are read off a seat's runtime settings, which `GET /config` serves to
	// nobody else.
	exposureResolved
)

// classified is one field's decision and the reason for it. The reason is
// there for the next reader: a table of bare verdicts invites flipping one
// without knowing what it was protecting.
type classified struct {
	exposure exposure
	why      string
	// into is the api.OrgSeat wire field a resolved value reaches.
	into string
}

// companyFields classifies every field of config.Company.
var companyFields = map[string]classified{
	"Name":           {exposure: exposurePublic, why: "the company's identity"},
	"Mission":        {exposure: exposurePublic, why: "founder prose"},
	"Vision":         {exposure: exposurePublic, why: "founder prose"},
	"Policies":       {exposure: exposurePublic, why: "founder prose"},
	"Timezone":       {exposure: exposurePublic, why: "every date a reader is shown is cut on it (ADR-0018)"},
	"TokenBudget":    {exposure: exposurePublic, why: "a statement of what the company may spend, beside the /budgets meter spending against it"},
	"Integrations":   {exposure: exposureGuarded, why: "vendor identities, bot tokens and signing secrets"},
	"Tracker":        {exposure: exposureGuarded, why: "which tracker backend and project keys: a map of what is wired to what"},
	"Knowledge":      {exposure: exposureGuarded, why: "which knowledge backend and container keys"},
	"SkillVariables": {exposure: exposureGuarded, why: "facts substituted into tool-skill text, routinely hostnames and account names"},
	"Providers": {exposure: exposureResolved, into: "llm",
		why: "only the KEYS reach a config:read reader, as each seat's resolved chain; every entry's model, endpoint and credentials stay guarded"},
	"TurnEngine": {exposure: exposureGuarded, why: "an operational setting"},
	"Learning":   {exposure: exposureGuarded, why: "an operational setting"},
	"Scheduling": {exposure: exposureGuarded, why: "an operational setting"},
	"MCPServers": {exposure: exposureResolved, into: "tool_sources",
		why: "only a granted server's NAME reaches a config:read reader, the label its tools carry in every prompt; its command, URL, env and headers stay guarded"},
	"NotificationRateLimit":             {exposure: exposureGuarded, why: "an operational setting"},
	"NotificationCoalesceWindowSeconds": {exposure: exposureGuarded, why: "an operational setting"},
	"NotificationCoalesceMaxBatch":      {exposure: exposureGuarded, why: "an operational setting"},
	"Workers":                           {exposure: exposureGuarded, why: "operational configuration of the delegate surface"},
	"Roles":                             {exposure: exposurePublic, why: "the tree itself"},
	"Units":                             {exposure: exposurePublic, why: "the tree itself"},
}

// projectionOnly is every OrgProjection field no authored company field
// accounts for, and why it may exist anyway.
var projectionOnly = map[string]string{
	"derived": "the hierarchy the fields above resolve to, with no document paths",
}

// A tracker project and a knowledge space are the decision here that is not
// about credentials: `project` and `space` hold neither a secret nor a ${VAR},
// and both are guarded anyway. A project key or a space key names a
// THIRD-PARTY CONTAINER, so handing one to every reader says where this
// company files its work and writes its pages, and what to go looking at. It
// is the reasoning internal/api/setupapi's package doc records for guarding
// even its reads: what a company has wired up, and to what, is a map of what
// to attack. They are read with the rest of the integration picture instead,
// through the `config:read` answers, `config` and `integrations`.

// roleFields classifies every field of config.Role.
var roleFields = map[string]classified{
	"Name":                 {exposurePublic, "the seat's identity in the chart", ""},
	"Kind":                 {exposurePublic, "agent or human, which the chart draws differently", ""},
	"Contact":              {exposureGuarded, "a person's account ids on external services", ""},
	"Availability":         {exposurePublic, "free text written to be read by colleagues", ""},
	"Handle":               {exposurePublic, "the declared slug every other surface addresses the seat by", ""},
	"Email":                {exposureGuarded, "a personal identifier, and it may hold a ${VAR} name", ""},
	"Unit":                 {exposureGuarded, "a placement reference; where a seat sits is derived, not authored prose", ""},
	"Goal":                 {exposurePublic, "founder prose", ""},
	"Backstory":            {exposurePublic, "founder prose", ""},
	"Responsibilities":     {exposurePublic, "founder prose", ""},
	"Manages":              {exposurePublic, "the reporting structure the chart draws", ""},
	"BehavioralGuidelines": {exposurePublic, "founder prose", ""},
	"Workers":              {exposureGuarded, "operational configuration of the delegate surface", ""},
	"TokenBudget":          {exposurePublic, "the seat's own ceilings, beside the /budgets meter spending against them", ""},
	"LLM":                  {exposureResolved, "provider keys, labels for the company's model entries, resolved per phase as a turn resolves them", "llm"},
	"LLMReview":            {exposureResolved, "provider keys, resolved into the reviewer's chain", "llm"},
	"LLMSubagent":          {exposureResolved, "provider keys, resolved into the worker chain", "llm"},
	"LLMAuxiliary":         {exposureResolved, "provider keys, resolved into the auxiliary chain", "llm"},
	"LLMJudge":             {exposureResolved, "provider keys, resolved into the judge's chain", "llm"},
	"LLMSandbox":           {exposureResolved, "provider keys, resolved into the coding agent's chain", "llm"},
	"LearningEnabled":      {exposureGuarded, "an operational setting", ""},
	"MCPEnv":               {exposureGuarded, "tool credentials and their ${VAR} names; which servers they grant is resolved from mcp_servers", ""},
	"Sandbox":              {exposureGuarded, "setup commands and env, the usual home of a registry credential", ""},
	"Placement":            {exposureGuarded, "node ids and labels describing the deployment", ""},
	"Integrations":         {exposureGuarded, "vendor identities, bot tokens and signing secrets", ""},
	"Project":              {exposureGuarded, "a tracker project key: the container this seat files work in", ""},
	"Space":                {exposureGuarded, "a knowledge container key: where this seat writes pages", ""},
	"Schedules":            {exposureGuarded, "configured work, not structure; /schedules is the surface that describes it", ""},
}

// unitFields classifies every field of config.Unit.
var unitFields = map[string]classified{
	"Name":      {exposurePublic, "the unit's identity in the chart", ""},
	"ID":        {exposurePublic, "the unit's KEY: a `manages:` entry and a seat's `unit:` resolve it, so a client reading this projection needs it to resolve a reference the projection itself carries. The chart still draws Name", ""},
	"Type":      {exposurePublic, "an informational label", ""},
	"Purpose":   {exposurePublic, "founder prose", ""},
	"Lead":      {exposurePublic, "the structure the chart draws", ""},
	"Goals":     {exposurePublic, "founder prose", ""},
	"Channel":   {exposurePublic, "where the unit talks, which a colleague needs to know", ""},
	"Knowledge": {exposurePublic, "free-text references, not a read scope", ""},
	"MCPEnv":    {exposureGuarded, "tool credentials inherited by members", ""},
	"Project":   {exposureGuarded, "a tracker project key: the container this unit's work is filed in", ""},
	"Space":     {exposureGuarded, "a knowledge container key: where this unit's pages are written", ""},
	"Roles":     {exposurePublic, "the tree itself", ""},
	"Children":  {exposurePublic, "the tree itself", ""},
	"Schedules": {exposureGuarded, "configured work, not structure; /schedules is the surface that describes it", ""},
}

// EVERY AUTHORED FIELD HAS A DECISION, and the public and resolved ones are
// exactly what the projection carries.
//
// A field added to config.Company, config.Role or config.Unit fails the first
// half until somebody decides whether every `state:read` holder may see it.
// Marking it public without adding it to the projection (or the reverse) fails
// the second half, so the table and the type cannot drift apart. A RESOLVED
// field must name the api.OrgSeat field it reaches, and every resolved seat
// field must be reached by something classified, so a resolved field is never
// a way to publish a value nobody decided on.
func TestEveryOrgFieldIsClassified(t *testing.T) {
	t.Parallel()
	seatWire := map[string]bool{}
	for field := range fieldsOf(reflect.TypeFor[api.OrgSeat]()) {
		seatWire[jsonName(field)] = true
	}
	// resolvedInto is every seat field a resolved value reaches, from any
	// table: a seat's chain is resolved from its own llm fields AND the
	// company's provider keys.
	resolvedInto := map[string]string{}
	for _, table := range []map[string]classified{companyFields, roleFields, unitFields} {
		for name, decision := range table {
			switch {
			case decision.exposure == exposureResolved && !seatWire[decision.into]:
				t.Errorf("%s is resolved into %q, which api.OrgSeat does not carry", name, decision.into)
			case decision.exposure != exposureResolved && decision.into != "":
				t.Errorf("%s names a field it is resolved into but is not classified resolved", name)
			case decision.exposure == exposureResolved:
				resolvedInto[decision.into] = "resolved from " + name
			}
		}
	}
	for _, tc := range []struct {
		authored   reflect.Type
		table      map[string]classified
		projection reflect.Type
		// extra is the projected fields no public authored field accounts
		// for, each with the reason it may exist.
		extra map[string]string
	}{
		{reflect.TypeFor[config.Company](), companyFields, reflect.TypeFor[api.OrgProjection](), projectionOnly},
		{reflect.TypeFor[config.Role](), roleFields, reflect.TypeFor[api.OrgSeat](), resolvedInto},
		{reflect.TypeFor[config.Unit](), unitFields, reflect.TypeFor[api.OrgUnit](), nil},
	} {
		t.Run(tc.authored.Name(), func(t *testing.T) {
			t.Parallel()
			var publicNames []string
			seen := map[string]bool{}
			for field := range fieldsOf(tc.authored) {
				seen[field.Name] = true
				decision, ok := tc.table[field.Name]
				if !ok {
					t.Errorf("config.%s.%s is not classified. Decide whether every "+
						"state:read reader of /org may see it: add it to this test's table as "+
						"public (and to api.%s), resolved (naming the api.OrgSeat field it "+
						"reaches, which only config:read is handed) or guarded (and read it "+
						"through the config query)",
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
			for name := range tc.extra {
				publicNames = append(publicNames, name)
			}
			var projected []string
			for field := range fieldsOf(tc.projection) {
				projected = append(projected, jsonName(field))
			}
			slices.Sort(publicNames)
			slices.Sort(projected)
			if !slices.Equal(publicNames, projected) {
				t.Errorf("the public and resolved fields of config.%s are %v but api.%s carries %v; "+
					"the two must name the same wire fields",
					tc.authored.Name(), publicNames, tc.projection.Name(), projected)
			}
		})
	}
}

// The two credentials an org reader presents in these cases: one holding every
// grant, `config:read` among them, and one holding `state:read` alone — the
// widest audience the projection reaches.
const (
	orgOpsToken    = "org-ops-token-long-enough-to-pass"
	orgReaderToken = "org-reader-token-long-enough-to-pass"
)

// orgReaders is an app's org surface with both credentials authorised.
func orgReaders(t *testing.T, company *config.Company) *api.App {
	t.Helper()
	b := config.DefaultBootstrap()
	authorize(&b,
		config.APIToken{ID: "ops", Token: orgOpsToken},
		config.APIToken{ID: "reader", Token: orgReaderToken, Grants: []iam.Grant{iam.GrantStateRead}},
	)
	return newApp(t, api.Options{
		Bootstrap: &b,
		Sources:   queries.Sources{Company: companySource(t, company)},
	})
}

// orgBody is GET /org as the holder of token reads it.
func orgBody(t *testing.T, a *api.App, token string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/org", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /org = %d, want 200", res.StatusCode)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read /org: %v", err)
	}
	return raw
}

// NOTHING GUARDED REACHES ANY READER, and nothing resolved reaches a reader
// without `config:read` — whatever the company holds.
//
// Every guarded field of every seat and unit (and every field of the company
// outside its charter) is filled, all the way down through pointers, slices
// and maps, keys included: once with a distinct literal, once with a distinct
// whole ${VAR} reference. The projection is read the three ways a browser
// reaches it, by a `state:read` reader and by a `config:read` one, and none may
// contain a fill value, a `${`, or any key outside the public shape. The
// public fields are filled with distinct prose that MUST appear to both, which
// is what stops this passing on an empty answer; the labels a seat's chain and
// tool sources resolve to MUST appear to the `config:read` reader and must not
// to the other — a label read off a seat's runtime settings is theirs to
// guard.
//
// Mutation: hand every audience the whole projection, and the reader's
// surfaces carry the resolved labels.
func TestTheOrgProjectionCarriesNothingGuarded(t *testing.T) {
	t.Parallel()
	for _, mode := range []fillMode{fillLiteral, fillReference} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			f := &filler{mode: mode}
			a := orgReaders(t, f.company())
			if len(f.resolvedValues) == 0 {
				t.Fatal("the fixture resolves no label, so the audience split is unasserted")
			}

			for _, reader := range []struct {
				name     string
				token    string
				audience stream.Audience
				resolved bool
			}{
				{"state:read", orgReaderToken, stream.AudienceOf([]iam.Grant{iam.GrantStateRead}), false},
				{"config:read", orgOpsToken, stream.AudienceOf(iam.AllGrants), true},
			} {
				snapshot, err := json.Marshal(a.Stream().Snapshot(reader.audience)["org"])
				if err != nil {
					t.Fatalf("marshal the snapshot's org: %v", err)
				}
				push, err := json.Marshal(a.Stream().Org(reader.audience))
				if err != nil {
					t.Fatalf("marshal the org push: %v", err)
				}
				for surface, body := range map[string][]byte{
					"GET /org": orgBody(t, a, reader.token), "snapshot": snapshot, "org push": push,
				} {
					surface = reader.name + " " + surface
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
					for _, label := range f.resolvedValues {
						if got := strings.Contains(text, label); got != reader.resolved {
							t.Errorf("%s carries the resolved label %q = %v, want %v:\n%s",
								surface, label, got, reader.resolved, text)
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
		reflect.TypeFor[api.OrgTokenBudget](),
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
	// A seat's resolved chain is keyed by phase.
	for _, ph := range phase.All {
		out[ph.String()] = true
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
	// resolvedValues are the labels a seat's resolved fields carry — each
	// provider key a seat names and the shared server's name — which reach
	// a `config:read` reader and nobody else.
	resolvedValues []string
	// providerKeys is every provider key a seat was given.
	providerKeys []string
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

func (f *filler) label() string {
	value := fmt.Sprintf("resolvedlabel%04d", f.next())
	f.resolvedValues = append(f.resolvedValues, value)
	return value
}

// company fills every field by its classification.
func (f *filler) company() *config.Company {
	c := &config.Company{}
	v := reflect.ValueOf(c).Elem()
	for field := range fieldsOf(v.Type()) {
		target := v.FieldByIndex(field.Index)
		switch {
		case field.Name == "Timezone":
			// PUBLIC, and filled with a zone that LOADS rather than with
			// prose: the projection carries the clock the engine resolved,
			// so a value that does not load would surface as UTC and this
			// could not tell a dropped field from a defaulted one.
			target.SetString("Pacific/Chatham")
			f.publicValues = append(f.publicValues, "Pacific/Chatham")
		case field.Name == "Roles" || field.Name == "Units":
			// Filled below, through the classification tables.
		case companyFields[field.Name].exposure == exposurePublic:
			f.public(target)
		default:
			// A resolved field is filled like a guarded one, whole, and
			// then given the labels it resolves from below: whatever the
			// label does not carry must still never appear.
			f.guarded(target, 0)
		}
	}
	// One SHARED server, so every agent seat is granted it and its name
	// must reach every agent seat's tool_sources while its command, URL,
	// env and headers — filled with secrets above — reach nothing.
	c.MCPServers[0].Name = f.label()
	c.MCPServers[0].Shared = org.On()
	c.Roles = f.seats()
	c.Units = []config.Unit{f.unit(2)}
	// Every provider key a seat names is configured, each entry filled with
	// secrets, so a key reaching the answer is the resolved chain and the
	// entry behind it reaching it is a leak.
	var entry config.LLMProvider
	f.guarded(reflect.ValueOf(&entry).Elem(), 0)
	c.Providers.LLM = map[string]config.LLMProvider{}
	for _, key := range f.providerKeys {
		c.Providers.LLM[key] = entry
	}
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
		case roleFields[field.Name].exposure == exposureResolved:
			f.resolvedLLM(target)
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

// resolvedLLM fills a seat's model field with a provider key the company
// configures, which must then reach the seat's resolved chain.
//
// The mapping form gets its default only: a flat llm_<phase> field wins over
// the same phase inside the mapping, so a key written there would be one no
// chain resolves to and nothing could require it to appear.
func (f *filler) resolvedLLM(v reflect.Value) {
	key := f.label()
	f.providerKeys = append(f.providerKeys, key)
	switch field := v.Addr().Interface().(type) {
	case *config.ProviderKeys:
		*field = config.ProviderKeys{key}
	case *config.PhaseLLM:
		*field = config.PhaseLLM{Default: config.ProviderKeys{key}}
	default:
		panic(fmt.Sprintf("a resolved model field of type %s; teach the filler before classifying it", v.Type()))
	}
}

// unit fills one unit, with a seat of each kind and, above depth zero, a child
// unit.
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

// public fills a public field: a string, a list of strings, or a budget.
func (f *filler) public(v reflect.Value) {
	if budget, ok := v.Addr().Interface().(*config.TokenBudget); ok {
		// Distinct ceilings in every window, each of which must appear.
		ceiling := func() *int {
			n := 900000 + f.next()
			f.publicValues = append(f.publicValues, strconv.Itoa(n))
			return &n
		}
		*budget = config.TokenBudget{Day: ceiling(), Week: ceiling(), Month: ceiling()}
		return
	}
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

// configReader is an audience holding every grant, `config:read` among them —
// the one a seat's resolved fields reach.
var configReader = stream.AudienceOf(iam.AllGrants)

// projectionOf is the org surface as audience reads it, typed.
func projectionOf(t *testing.T, a *api.App, audience stream.Audience) api.OrgProjection {
	t.Helper()
	got := a.Stream().Org(audience)
	projection, ok := got.(api.OrgProjection)
	if !ok {
		t.Fatalf("the org surface answers %T, want api.OrgProjection", got)
	}
	return projection
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

	projection := projectionOf(t, a, configReader)
	scribble := func(lists ...[]string) {
		for _, list := range lists {
			for i := range list {
				list[i] = "rewritten through the projection"
			}
		}
	}
	scribbleSeats := func(seats []api.OrgSeat) {
		for i := range seats {
			scribble(seats[i].Responsibilities, seats[i].BehavioralGuidelines,
				seats[i].Manages, seats[i].ToolSources)
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

// THE COMPANY'S CLOCK IS ON THE `state:read` PROJECTION, AS THE ENGINE RESOLVED
// IT (ADR-0018).
//
// Every date a dashboard reader is shown is cut on it, so a screen without it
// cuts "today" on the browser's own zone and disagrees with the engine's own
// due bands beside it. And an unwritten clock arrives as `UTC` rather than
// empty, because a client defaulting an empty string defaults it to ITS OWN
// zone — the very disagreement the field exists to end. A node with no company
// still answers `{}`.
func TestTheOrgProjectionCarriesTheCompanysClock(t *testing.T) {
	t.Parallel()
	reader := stream.AudienceOf([]iam.Grant{iam.GrantStateRead})
	for _, tc := range []struct {
		name    string
		company *config.Company
		want    string
	}{
		{"a written clock", &config.Company{Name: "Acme", Timezone: "America/Los_Angeles"}, "America/Los_Angeles"},
		{"no clock written", &config.Company{Name: "Acme"}, "UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := newApp(t, api.Options{
				Sources: queries.Sources{Company: companySource(t, tc.company)},
			})
			if got := projectionOf(t, a, reader).Timezone; got != tc.want {
				t.Errorf("the projection's clock is %q, want %q", got, tc.want)
			}
		})
	}

	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: companySource(t, nil)},
	})
	body, err := json.Marshal(a.Stream().Org(reader))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{}" {
		t.Errorf("a node with no company answers %s, want {} — the shape the "+
			"dashboard reads as nothing loaded", body)
	}
}

// WHAT A SEAT RUNS ON AND WHAT IT CAN REACH ARE PUBLISHED AS THE ENGINE
// RESOLVES THEM — to a `config:read` reader, and to nobody else.
//
// The model chain is every phase's, the way a turn resolves it: a flat
// llm_<phase> field wins, a phase naming nothing falls to the seat's llm, and a
// seat naming nothing lands on the company's `default` provider. The tool
// sources are the builtins, every shared server, and a per-seat template only
// where the seat — or its unit — declares credentials for it, which is the
// grant the engine starts a seat's children by. Both are resolved from the
// seat AS THE CHART HOLDS IT, the composed org. A human seat runs neither. The
// budgets are the ceilings in force, company and seat alike, a window nobody
// caps absent rather than zero — and those a `state:read` reader is handed
// too, where the resolved labels it is not.
func TestTheOrgProjectionPublishesWhatASeatRunsOn(t *testing.T) {
	t.Parallel()
	ceiling := func(n int) *int { return &n }
	company := &config.Company{
		Name:        "Acme",
		TokenBudget: config.TokenBudget{Month: ceiling(40_000_000)},
		Providers: config.Providers{
			LLM: map[string]config.LLMProvider{
				"fast": {}, "big": {}, "default": {},
			},
			LLMOrder: []string{"fast", "big", "default"},
		},
		MCPServers: []config.MCPServer{
			{Name: "github", Shared: org.Off()},
			{Name: "search"},
			{Name: "jira", Shared: org.Off()},
		},
		Roles: []config.Role{
			{
				Name: "CTO", LLM: config.PhaseLLM{Default: config.ProviderKeys{"fast"}, Review: config.ProviderKeys{"fast"}},
				LLMReview:   config.ProviderKeys{"big", "fast"},
				TokenBudget: config.TokenBudget{Day: ceiling(2_000_000)},
				MCPEnv:      org.MCPEnv{"jira": {"JIRA_TOKEN": "${CTO_JIRA}"}},
			},
			{Name: "Founder", Kind: org.KindHuman, Contact: &org.HumanContact{SlackUserID: "U0FOUNDER"}},
		},
		Units: []config.Unit{{
			Name:   "Engineering",
			MCPEnv: org.MCPEnv{"github": {"GITHUB_TOKEN": "${ENG_GITHUB}"}},
			Roles:  []config.Role{{Name: "SRE"}},
		}},
	}
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: companySource(t, company)},
	})
	// THE FIXTURE CREDENTIAL HOLDS EVERY GRANT, so GET /org is the
	// config:read shape.
	got := orgOf(t, a)

	if got.TokenBudget == nil || got.TokenBudget.Month == nil || *got.TokenBudget.Month != 40_000_000 ||
		got.TokenBudget.Day != nil || got.TokenBudget.Week != nil {
		t.Errorf("the company's budget = %+v, want only a month of 40000000", got.TokenBudget)
	}

	cto, founder, sre := got.Roles[0], got.Roles[1], got.Units[0].Roles[0]
	if cto.TokenBudget == nil || cto.TokenBudget.Day == nil || *cto.TokenBudget.Day != 2_000_000 ||
		cto.TokenBudget.Week != nil || cto.TokenBudget.Month != nil {
		t.Errorf("the CTO's budget = %+v, want only a day of 2000000", cto.TokenBudget)
	}
	if sre.TokenBudget != nil {
		t.Errorf("a seat capping nothing carries a budget %+v", sre.TokenBudget)
	}

	for _, tc := range []struct {
		seat  string
		llm   map[string][]string
		tools []string
	}{
		{
			seat: "CTO",
			llm: map[string][]string{
				// The flat field wins over the mapping's own review.
				"review":  {"big", "fast"},
				"execute": {"fast"}, "subagent": {"fast"}, "auxiliary": {"fast"},
				"judge": {"fast"}, "sandbox": {"fast"}, "onboarding": {"fast"},
			},
			// Its own jira credentials grant jira; github is its unit's
			// and it sits in none. Declaration order, builtins first.
			tools: []string{"builtin", "mcp:search", "mcp:jira"},
		},
		{
			seat: "SRE",
			llm: map[string][]string{
				"execute": {"default"}, "review": {"default"}, "subagent": {"default"},
				"auxiliary": {"default"}, "judge": {"default"}, "sandbox": {"default"},
				"onboarding": {"default"},
			},
			// Inherited from the unit's mcp_env.
			tools: []string{"builtin", "mcp:github", "mcp:search"},
		},
	} {
		seat := cto
		if tc.seat == "SRE" {
			seat = sre
		}
		if !reflect.DeepEqual(seat.LLM, tc.llm) {
			t.Errorf("%s's chain = %v, want %v", tc.seat, seat.LLM, tc.llm)
		}
		if !slices.Equal(seat.ToolSources, tc.tools) {
			t.Errorf("%s's tool sources = %v, want %v", tc.seat, seat.ToolSources, tc.tools)
		}
	}
	if founder.LLM != nil || founder.ToolSources != nil {
		t.Errorf("a human seat carries a chain %v and tool sources %v; it runs neither",
			founder.LLM, founder.ToolSources)
	}
	for phaseName := range cto.LLM {
		if !slices.ContainsFunc(phase.All, func(p phase.Phase) bool { return p.String() == phaseName }) {
			t.Errorf("the chain names %q, which is not a phase", phaseName)
		}
	}
	if len(cto.LLM) != len(phase.All) {
		t.Errorf("the CTO's chain covers %d phases, want every one of %d", len(cto.LLM), len(phase.All))
	}

	// A STATE:READ READER IS HANDED THE SAME TREE WITHOUT THEM, at every
	// depth — and the budgets all the same.
	reader := projectionOf(t, a, stream.AudienceOf([]iam.Grant{iam.GrantStateRead}))
	for name, seat := range map[string]api.OrgSeat{
		"CTO": reader.Roles[0], "SRE": reader.Units[0].Roles[0],
	} {
		if seat.LLM != nil || seat.ToolSources != nil {
			t.Errorf("a state:read reader is handed %s's chain %v and tool sources %v",
				name, seat.LLM, seat.ToolSources)
		}
	}
	if b := reader.Roles[0].TokenBudget; b == nil || b.Day == nil || *b.Day != 2_000_000 {
		t.Errorf("a state:read reader is handed the CTO's budget %+v, want its day of 2000000", b)
	}

	// A company with no provider has no chain to show, and says nothing
	// rather than an empty one.
	bare := &config.Company{Name: "Acme", Roles: []config.Role{{Name: "CTO"}}}
	b := newApp(t, api.Options{
		Sources: queries.Sources{Company: companySource(t, bare)},
	})
	if seat := orgOf(t, b).Roles[0]; seat.LLM != nil || !slices.Equal(seat.ToolSources, []string{"builtin"}) {
		t.Errorf("with no provider and no server, the seat carries %v and %v; want no chain and only the builtins",
			seat.LLM, seat.ToolSources)
	}
}

package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The org projection is READABLE WITHOUT A KEY, and served to three reaches, so
// what it carries — and to whom — is a security decision made one field at a
// time. These tests are what keep it one.

// exposure is the decision recorded for one authored field.
type exposure int

const (
	// exposureCarried: carried by the projection, to the reach named.
	exposureCarried exposure = iota + 1
	// exposureGuarded: read only through the guarded `config` query.
	exposureGuarded
	// exposureResolved: read, with others, into the RESOLVED seat field
	// named by `into`, which carries the labels the value decides (a
	// provider key, a server name) and never the value's own contents.
	exposureResolved
	// exposureMember: not in the anonymous projection, and served to a
	// MEMBER key (ADR-0031) by the question named by `into`, which resolves
	// it against the chart beside the thing it is about.
	exposureMember
)

// classified is one field's decision and the reason for it. The reason is
// there for the next reader: a table of bare verdicts invites flipping one
// without knowing what it was protecting.
type classified struct {
	exposure exposure
	// reach is the lowest reach a carried or resolved field is served to
	// (ADR-0031): public for the chart, member for what the company
	// published for its people, admin for how it is run.
	reach auth.Reach
	why   string
	// into is where a value the projection does not carry does reach a
	// reader: the api.OrgSeat wire field a resolved value reaches, or the
	// member question that serves a member value.
	into string
}

// companyFields classifies every field of config.Company.
var companyFields = map[string]classified{
	"Name":           {exposure: exposureCarried, reach: auth.ReachPublic, why: "the company's identity"},
	"Mission":        {exposure: exposureCarried, reach: auth.ReachPublic, why: "founder prose the company shows the world"},
	"Vision":         {exposure: exposureCarried, reach: auth.ReachPublic, why: "founder prose the company shows the world"},
	"Policies":       {exposure: exposureCarried, reach: auth.ReachMember, why: "the rules the company's seats work by, written for the people in it"},
	"Timezone":       {exposure: exposureCarried, reach: auth.ReachPublic, why: "every date a reader is shown is cut on it (ADR-0018)"},
	"TokenBudget":    {exposure: exposureCarried, reach: auth.ReachAdmin, why: "what the company may spend is how it is run, beside the /budgets meter spending against it"},
	"Integrations":   {exposure: exposureGuarded, why: "vendor identities, bot tokens and signing secrets"},
	"Tracker":        {exposure: exposureGuarded, why: "which tracker backend and project keys: a map of what is wired to what"},
	"Knowledge":      {exposure: exposureGuarded, why: "which knowledge backend and container keys"},
	"SkillVariables": {exposure: exposureGuarded, why: "facts substituted into tool-skill text, routinely hostnames and account names"},
	"Providers": {exposure: exposureResolved, reach: auth.ReachAdmin, into: "llm",
		why: "only the KEYS reach a reader, as each seat's resolved chain; every entry's model, endpoint and credentials stay guarded"},
	"TurnEngine": {exposure: exposureGuarded, why: "an operational setting"},
	"Learning":   {exposure: exposureGuarded, why: "an operational setting"},
	"Scheduling": {exposure: exposureGuarded, why: "an operational setting"},
	"MCPServers": {exposure: exposureResolved, reach: auth.ReachAdmin, into: "tool_sources",
		why: "only a granted server's NAME reaches a reader, the label its tools carry in every prompt; its command, URL, env and headers stay guarded"},
	"NotificationRateLimit":             {exposure: exposureGuarded, why: "an operational setting"},
	"NotificationCoalesceWindowSeconds": {exposure: exposureGuarded, why: "an operational setting"},
	"NotificationCoalesceMaxBatch":      {exposure: exposureGuarded, why: "an operational setting"},
	"Workers":                           {exposure: exposureGuarded, why: "operational configuration of the delegate surface"},
	"Roles":                             {exposure: exposureCarried, reach: auth.ReachPublic, why: "the tree itself"},
	"Units":                             {exposure: exposureCarried, reach: auth.ReachPublic, why: "the tree itself"},
}

// projectionOnly is every OrgProjection field no authored company field
// accounts for, and why it may exist anyway.
var projectionOnly = map[string]classified{
	"derived": {exposure: exposureCarried, reach: auth.ReachPublic,
		why: "the hierarchy the fields above resolve to, with no document paths; each of its own fields is classified below"},
}

// A tracker project and a knowledge space are the decision here that is not
// about credentials: `project` and `space` hold neither a secret nor a ${VAR},
// and neither reaches an anonymous reader anyway. A project key or a space key
// names a THIRD-PARTY CONTAINER, so handing one to a stranger says where this
// company files its work and writes its pages, and what to go looking at — the
// reasoning internal/api/setupapi's package doc records for guarding even its
// reads. A UNIT's are a member's (ADR-0031), because a teammate cannot read the
// company's work without knowing which team a project or a space is:
// `work_projects` serves each project's unit, and `containers` each space's
// units and their projects, both resolved against the running chart. A SEAT's
// stay guarded: where one person files is not structure a colleague needs.

func servedAt(reach auth.Reach, why string) classified {
	return classified{exposure: exposureCarried, reach: reach, why: why}
}

func withheld(why string) classified { return classified{exposure: exposureGuarded, why: why} }

// servedBy is a field the projection does not carry and a MEMBER question does,
// resolved against the chart beside the thing it is about.
func servedBy(question, why string) classified {
	return classified{exposure: exposureMember, reach: auth.ReachMember, why: why, into: question}
}

func resolvedInto(into, why string) classified {
	return classified{exposure: exposureResolved, reach: auth.ReachAdmin, why: why, into: into}
}

// roleFields classifies every field of config.Role.
var roleFields = map[string]classified{
	"Name":                 servedAt(auth.ReachPublic, "the seat's identity in the chart"),
	"Kind":                 servedAt(auth.ReachPublic, "agent or human, which the chart draws differently"),
	"Contact":              withheld("a person's account ids on external services"),
	"Availability":         servedAt(auth.ReachMember, "free text written to be read by colleagues"),
	"Handle":               servedAt(auth.ReachPublic, "the declared slug every other surface addresses the seat by"),
	"Email":                withheld("a personal identifier, and it may hold a ${VAR} name"),
	"Unit":                 withheld("a placement reference; where a seat sits is derived, not authored prose"),
	"Goal":                 servedAt(auth.ReachMember, "founder prose, written for the seat's colleagues"),
	"Backstory":            servedAt(auth.ReachMember, "founder prose, written for the seat's colleagues"),
	"Responsibilities":     servedAt(auth.ReachMember, "founder prose, written for the seat's colleagues"),
	"Manages":              servedAt(auth.ReachPublic, "the reporting structure the chart draws"),
	"BehavioralGuidelines": servedAt(auth.ReachMember, "founder prose, written for the seat's colleagues"),
	"Workers":              withheld("operational configuration of the delegate surface"),
	"TokenBudget":          servedAt(auth.ReachAdmin, "the seat's own ceilings: how it is run, beside the /budgets meter spending against them"),
	"LLM":                  resolvedInto("llm", "provider keys, labels for the company's model entries, resolved per phase as a turn resolves them"),
	"LLMReview":            resolvedInto("llm", "provider keys, resolved into the reviewer's chain"),
	"LLMSubagent":          resolvedInto("llm", "provider keys, resolved into the worker chain"),
	"LLMAuxiliary":         resolvedInto("llm", "provider keys, resolved into the auxiliary chain"),
	"LLMJudge":             resolvedInto("llm", "provider keys, resolved into the judge's chain"),
	"LLMSandbox":           resolvedInto("llm", "provider keys, resolved into the coding agent's chain"),
	"LearningEnabled":      withheld("an operational setting"),
	"MCPEnv":               withheld("tool credentials and their ${VAR} names; which servers they grant is resolved from mcp_servers"),
	"Sandbox":              withheld("setup commands and env, the usual home of a registry credential"),
	"Placement":            withheld("node ids and labels describing the deployment"),
	"Integrations":         withheld("vendor identities, bot tokens and signing secrets"),
	"Project":              withheld("a tracker project key: the container this seat files work in"),
	"Space":                withheld("a knowledge container key: where this seat writes pages"),
	"Schedules":            withheld("configured work, not structure; /schedules is the surface that describes it"),
}

// unitFields classifies every field of config.Unit.
var unitFields = map[string]classified{
	"Name":      servedAt(auth.ReachPublic, "the unit's identity in the chart"),
	"ID":        withheld("a durable key rather than a name: everything filed against the unit keys on it (org.Unit.Key) and nobody reads it, while the chart draws Name"),
	"Type":      servedAt(auth.ReachPublic, "an informational label the chart draws"),
	"Purpose":   servedAt(auth.ReachMember, "founder prose, written for the unit's people"),
	"Lead":      servedAt(auth.ReachPublic, "the structure the chart draws"),
	"Goals":     servedAt(auth.ReachMember, "founder prose, written for the unit's people"),
	"Channel":   servedAt(auth.ReachMember, "where the unit talks, which a colleague needs and a stranger does not"),
	"Knowledge": servedAt(auth.ReachMember, "free-text references for the unit's people, not a read scope"),
	"MCPEnv":    withheld("tool credentials inherited by members"),
	"Project":   servedBy("work_projects", "a tracker project key: the container this unit's work is filed in"),
	"Space":     servedBy("containers", "a knowledge container key: where this unit's pages are written"),
	"Roles":     servedAt(auth.ReachPublic, "the tree itself"),
	"Children":  servedAt(auth.ReachPublic, "the tree itself"),
	"Schedules": withheld("configured work, not structure; /schedules is the surface that describes it"),
}

// tokenBudgetFields classifies every field of config.TokenBudget, which a
// carried budget is copied into api.OrgTokenBudget from.
var tokenBudgetFields = map[string]classified{
	"Day":   servedAt(auth.ReachAdmin, "a ceiling, carried where the budget is"),
	"Week":  servedAt(auth.ReachAdmin, "a ceiling, carried where the budget is"),
	"Month": servedAt(auth.ReachAdmin, "a ceiling, carried where the budget is"),
}

// derivedFields, derivedSeatFields and derivedUnitFields classify the engine's
// derived hierarchy, which the chart carries resolved: each field at the reach
// of what it is resolved from, and never the document paths.
var derivedFields = map[string]classified{
	"Seats": servedAt(auth.ReachPublic, "every seat, resolved"),
	"Units": servedAt(auth.ReachPublic, "every unit, resolved"),
}

var derivedSeatFields = map[string]classified{
	"Path":            withheld("a path into a document the chart's reader is not given"),
	"UnitPath":        withheld("a path into a document the chart's reader is not given"),
	"Handle":          servedAt(auth.ReachPublic, "the seat's identity"),
	"Name":            servedAt(auth.ReachPublic, "the seat's name"),
	"Kind":            servedAt(auth.ReachPublic, "agent or human"),
	"PlacedByRef":     servedAt(auth.ReachPublic, "where the chart draws the seat"),
	"Manager":         servedAt(auth.ReachPublic, "the reporting structure, resolved"),
	"Managers":        servedAt(auth.ReachPublic, "the reporting structure, resolved"),
	"Reports":         servedAt(auth.ReachPublic, "the reporting structure, resolved"),
	"AutoReports":     servedAt(auth.ReachPublic, "the reporting structure, resolved"),
	"OnboardingChain": servedAt(auth.ReachPublic, "the names of the units above the seat, which the chart already shows"),
}

var derivedUnitFields = map[string]classified{
	"Path":             withheld("a path into a document the chart's reader is not given"),
	"Name":             servedAt(auth.ReachPublic, "the unit's name"),
	"Type":             servedAt(auth.ReachPublic, "the effective type the chart draws"),
	"Lead":             servedAt(auth.ReachPublic, "the effective lead, the structure the chart draws"),
	"LeadInherited":    servedAt(auth.ReachPublic, "whether the lead was inherited"),
	"Channel":          servedAt(auth.ReachMember, "the effective channel, a member's as the authored one is"),
	"ChannelInherited": servedAt(auth.ReachMember, "whether the channel was inherited, a member's with it"),
	"Seats":            servedAt(auth.ReachPublic, "the unit's members"),
}

// projectionPairs is every authored type the projection copies from, its
// classification, the projection type it is copied into, and the projected
// fields nothing authored accounts for.
type projectionPair struct {
	authored   reflect.Type
	table      map[string]classified
	projection reflect.Type
	extra      map[string]classified
}

// noPages is a knowledge base that holds nothing: what registers the page
// questions, for a test that reads their reach and asks none of them.
type noPages struct{}

func (noPages) List(context.Context, pages.Filter, statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (noPages) Get(context.Context, string, statelog.Freshness) (pages.Detail, error) {
	return pages.Detail{}, nil
}

func (noPages) Containers(context.Context, statelog.Freshness) ([]pages.ContainerListing, error) {
	return nil, nil
}

func (noPages) Activity(context.Context, pages.PageActivityQuery) (pages.PageActivity, error) {
	return pages.PageActivity{}, nil
}

func (noPages) Revision(context.Context, string, int, statelog.Freshness) (pages.Revision, bool, error) {
	return pages.Revision{}, false, nil
}

func (noPages) TurnWrites(context.Context, []string, statelog.Freshness) (map[string][]pages.TurnWrite, error) {
	return nil, nil
}

// EVERY AUTHORED FIELD HAS A DECISION, the carried ones are exactly what the
// projection carries, and every projected field is served at the reach its
// decision names.
//
// A field added to config.Company, config.Role, config.Unit, a budget or the
// derived hierarchy fails the first half until somebody decides whether — and
// to which reach — it may be shown. Marking it carried without adding it to
// the projection (or the reverse) fails the second half, so the table and the
// type cannot drift apart. A projected field whose `reach` tag is not the
// decision's fails the third, and one with no valid tag at all is withheld
// from everybody (orgprojection.go) and fails here first. A RESOLVED field
// must name the api.OrgSeat field it reaches, and every resolved seat field
// must be reached by something classified, so a resolved field is never a way
// to publish a value nobody decided on.
//
// Mutation: tag OrgSeat.Goal `reach:"public"`, and the third half fails.
func TestEveryOrgFieldIsClassified(t *testing.T) {
	t.Parallel()
	seatWire := map[string]bool{}
	for field := range fieldsOf(reflect.TypeFor[api.OrgSeat]()) {
		seatWire[jsonName(field)] = true
	}
	// resolvedInto is every seat field a resolved value reaches, from any
	// table: a seat's chain is resolved from its own llm fields AND the
	// company's provider keys.
	resolvedInto := map[string]classified{}
	// THE QUESTIONS AS REGISTERED, so a member field names a question a
	// member key is actually served — not a name somebody typed.
	questions := queries.NewRegistry()
	queries.Register(questions, queries.Sources{Work: stubWorkReader{}, Pages: noPages{}})
	for _, table := range []map[string]classified{companyFields, roleFields, unitFields} {
		for name, decision := range table {
			switch {
			case decision.exposure == exposureResolved && !seatWire[decision.into]:
				t.Errorf("%s is resolved into %q, which api.OrgSeat does not carry", name, decision.into)
			case decision.exposure == exposureMember &&
				questions.ReachOf(decision.into) != auth.ReachMember:
				t.Errorf("%s is said to be served to members by %q, which is registered at %q",
					name, decision.into, questions.ReachOf(decision.into))
			case decision.exposure != exposureResolved && decision.exposure != exposureMember &&
				decision.into != "":
				t.Errorf("%s names where it is served but is classified %d", name, decision.exposure)
			case decision.exposure == exposureResolved:
				if prior, ok := resolvedInto[decision.into]; ok && prior.reach != decision.reach {
					t.Errorf("%q is resolved from fields served at %s and %s; one field has one reach",
						decision.into, prior.reach, decision.reach)
				}
				resolvedInto[decision.into] = classified{
					exposure: exposureCarried, reach: decision.reach, why: "resolved from " + name,
				}
			}
		}
	}
	for _, tc := range []projectionPair{
		{reflect.TypeFor[config.Company](), companyFields, reflect.TypeFor[api.OrgProjection](), projectionOnly},
		{reflect.TypeFor[config.Role](), roleFields, reflect.TypeFor[api.OrgSeat](), resolvedInto},
		{reflect.TypeFor[config.Unit](), unitFields, reflect.TypeFor[api.OrgUnit](), nil},
		{reflect.TypeFor[config.TokenBudget](), tokenBudgetFields, reflect.TypeFor[api.OrgTokenBudget](), nil},
		{reflect.TypeFor[config.Derived](), derivedFields, reflect.TypeFor[api.OrgDerived](), nil},
		{reflect.TypeFor[config.DerivedSeat](), derivedSeatFields, reflect.TypeFor[api.OrgDerivedSeat](), nil},
		{reflect.TypeFor[config.DerivedUnit](), derivedUnitFields, reflect.TypeFor[api.OrgDerivedUnit](), nil},
	} {
		t.Run(tc.authored.Name(), func(t *testing.T) {
			t.Parallel()
			want := map[string]auth.Reach{}
			seen := map[string]bool{}
			for field := range fieldsOf(tc.authored) {
				seen[field.Name] = true
				decision, ok := tc.table[field.Name]
				if !ok {
					t.Errorf("config.%s.%s is not classified. Decide whether a reader of /org "+
						"may see it, and at which reach: add it to this test's table as carried "+
						"(and to api.%s, tagged with that reach), resolved (naming the api.OrgSeat "+
						"field it reaches) or guarded (and read it through the config query)",
						tc.authored.Name(), field.Name, tc.projection.Name())
					continue
				}
				if decision.why == "" {
					t.Errorf("config.%s.%s is classified with no reason", tc.authored.Name(), field.Name)
				}
				if decision.exposure == exposureCarried {
					want[jsonName(field)] = decision.reach
				}
			}
			for name := range tc.table {
				if !seen[name] {
					t.Errorf("the table classifies config.%s.%s, which no longer exists; "+
						"remove the entry", tc.authored.Name(), name)
				}
			}
			for name, decision := range tc.extra {
				want[name] = decision.reach
			}
			got := map[string]auth.Reach{}
			for field := range fieldsOf(tc.projection) {
				reach := auth.Reach(field.Tag.Get("reach"))
				if !reach.Valid() || reach == auth.ReachOpen {
					t.Errorf("api.%s.%s is tagged reach %q; every field of the chart names public, "+
						"member or admin", tc.projection.Name(), field.Name, reach)
				}
				got[jsonName(field)] = reach
			}
			if !maps.Equal(got, want) {
				t.Errorf("the carried and resolved fields of config.%s, with their reaches, are %v "+
					"but api.%s carries %v; the two must name the same wire fields at the same reaches",
					tc.authored.Name(), want, tc.projection.Name(), got)
			}
		})
	}
}

// chartReaders is each reach the chart is served to, with the key that
// presents it on [chartPosture].
var chartReaders = []struct {
	name  string
	key   string
	reach auth.Reach
}{
	{"anonymous", "", auth.ReachPublic},
	{"member", "member-secret", auth.ReachMember},
	{"admin", "secret", auth.ReachAdmin},
}

// NOTHING GUARDED REACHES ANY READER, AND NOTHING REACHES A READER ITS REACH
// DOES NOT COVER.
//
// Every guarded field of every seat and unit (and every field of the company
// outside its charter) is filled, all the way down through pointers, slices
// and maps, keys included: once with a distinct literal, once with a distinct
// whole ${VAR} reference. Every carried field is filled with distinct text
// recorded under the reach it is served at. The projection is read the three
// ways a browser reaches it, by each of the three readers, and none may contain
// a guarded value, a `${`, a value carried above the reader's reach, or a key
// outside the shape that reach is served — while every value AT or below the
// reader's reach MUST appear, which is what stops this passing on an empty
// answer.
//
// Mutation: skip the withholding in orgProjection, and the anonymous and member
// readers fail; skip descending into slices, and a nested seat's goal reaches
// the anonymous one.
func TestTheOrgProjectionCarriesNothingGuarded(t *testing.T) {
	t.Parallel()
	for _, mode := range []fillMode{fillLiteral, fillReference} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			f := &filler{mode: mode, carried: map[auth.Reach][]string{}}
			company := f.company()
			a := chartApp(t, company)

			for _, reader := range chartReaders {
				headers := map[string]string{}
				if reader.key != "" {
					headers["Authorization"] = "Bearer " + reader.key
				}
				res := fetch(t, a, "/org", headers)
				if res.StatusCode != http.StatusOK {
					t.Fatalf("GET /org as %s = %d, want 200", reader.name, res.StatusCode)
				}
				rest, err := io.ReadAll(res.Body)
				if err != nil {
					t.Fatalf("read /org: %v", err)
				}
				snapshot, err := json.Marshal(a.Stream().Snapshot(reader.reach)["org"])
				if err != nil {
					t.Fatalf("marshal the snapshot's org: %v", err)
				}
				push, err := json.Marshal(a.Stream().Org(reader.reach))
				if err != nil {
					t.Fatalf("marshal the org push: %v", err)
				}
				allowed := keysAt(reader.reach)

				for surface, body := range map[string][]byte{
					"GET /org": rest, "snapshot": snapshot, "org push": push,
				} {
					surface = reader.name + "'s " + surface
					text := string(body)
					if strings.Contains(text, "${") {
						t.Errorf("%s carries a ${VAR} reference:\n%s", surface, text)
					}
					for _, secret := range f.guardedValues {
						if strings.Contains(text, secret) {
							t.Errorf("%s carries the guarded value %q:\n%s", surface, secret, text)
						}
					}
					for reach, values := range f.carried {
						for _, value := range values {
							switch has := strings.Contains(text, value); {
							case reader.reach.Covers(reach) && !has:
								t.Errorf("%s is missing the %s value %q, so a field it is served "+
									"was dropped:\n%s", surface, reach, value, text)
							case !reader.reach.Covers(reach) && has:
								t.Errorf("%s carries the %s value %q, which its reach does not "+
									"cover:\n%s", surface, reach, value, text)
							}
						}
					}
					var decoded any
					if err := json.Unmarshal(body, &decoded); err != nil {
						t.Fatalf("%s is not JSON: %v", surface, err)
					}
					for key := range objectKeys(decoded) {
						if !allowed[key] {
							t.Errorf("%s carries the key %q, which is not in the shape its reach "+
								"is served", surface, key)
						}
					}
				}
			}
		})
	}
}

// keysAt is every JSON key the chart may contain for a reader holding reach.
func keysAt(reach auth.Reach) map[string]bool {
	out := map[string]bool{}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[api.OrgProjection](),
		reflect.TypeFor[api.OrgSeat](),
		reflect.TypeFor[api.OrgUnit](),
		reflect.TypeFor[api.OrgTokenBudget](),
		reflect.TypeFor[api.OrgDerived](),
		reflect.TypeFor[api.OrgDerivedSeat](),
		reflect.TypeFor[api.OrgDerivedUnit](),
	} {
		for field := range fieldsOf(typ) {
			if reach.Covers(auth.Reach(field.Tag.Get("reach"))) {
				out[jsonName(field)] = true
			}
		}
	}
	// A seat's resolved chain is keyed by phase, and served where the chain
	// is.
	if reach.Covers(auth.ReachAdmin) {
		for _, ph := range phase.All {
			out[ph.String()] = true
		}
	}
	return out
}

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
	// carried is every value filled into a carried field, by the reach the
	// field is served at.
	carried map[auth.Reach][]string
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

// prose is distinct text for a field served at reach.
func (f *filler) prose(reach auth.Reach) string {
	value := fmt.Sprintf("%sprose%04d", reach, f.next())
	f.carried[reach] = append(f.carried[reach], value)
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
			f.carried[auth.ReachPublic] = append(f.carried[auth.ReachPublic], "Pacific/Chatham")
		case field.Name == "Roles" || field.Name == "Units":
			// Filled below, through the classification tables.
		case companyFields[field.Name].exposure == exposureCarried:
			f.public(target, companyFields[field.Name].reach)
		default:
			// A resolved field is filled like a guarded one, whole, and
			// then given the labels it resolves from below: whatever the
			// label does not carry must still never appear.
			f.guarded(target, 0)
		}
	}
	// One SHARED server, so every agent seat is granted it and its name
	// must reach every seat's tool_sources while its command, URL, env and
	// headers — filled with secrets above — reach nothing.
	c.MCPServers[0].Name = f.prose(companyFields["MCPServers"].reach)
	c.Roles = []config.Role{f.role()}
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

func (f *filler) role() config.Role {
	var r config.Role
	v := reflect.ValueOf(&r).Elem()
	for field := range fieldsOf(v.Type()) {
		target := v.FieldByIndex(field.Index)
		switch decision := roleFields[field.Name]; decision.exposure {
		case exposureCarried:
			f.public(target, decision.reach)
		case exposureResolved:
			f.resolvedLLM(target)
		default:
			f.guarded(target, 0)
		}
	}
	return r
}

// resolvedLLM fills a seat's model field with a provider key the company
// configures, which must then reach the seat's resolved chain.
//
// The mapping form gets its default only: a flat llm_<phase> field wins over
// the same phase inside the mapping, so a key written there would be one no
// chain resolves to and nothing could require it to appear.
func (f *filler) resolvedLLM(v reflect.Value) {
	key := f.prose(roleFields["LLM"].reach)
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

// unit fills one unit, with a seat and, above depth zero, a child unit.
func (f *filler) unit(depth int) config.Unit {
	var u config.Unit
	v := reflect.ValueOf(&u).Elem()
	for field := range fieldsOf(v.Type()) {
		target := v.FieldByIndex(field.Index)
		switch {
		case field.Name == "Roles" || field.Name == "Children":
			// Structure, filled below.
		case unitFields[field.Name].exposure == exposureCarried:
			f.public(target, unitFields[field.Name].reach)
		default:
			f.guarded(target, 0)
		}
	}
	u.Roles = []config.Role{f.role()}
	if depth > 0 {
		u.Children = []config.Unit{f.unit(depth - 1)}
	}
	return u
}

// public fills a carried field served at reach: a string, a list of strings,
// or a budget.
func (f *filler) public(v reflect.Value, reach auth.Reach) {
	if budget, ok := v.Addr().Interface().(*config.TokenBudget); ok {
		// Distinct ceilings in every window, each of which must appear
		// where the budget is served.
		ceiling := func() *int {
			n := 900000 + f.next()
			f.carried[reach] = append(f.carried[reach], strconv.Itoa(n))
			return &n
		}
		*budget = config.TokenBudget{Day: ceiling(), Week: ceiling(), Month: ceiling()}
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(f.prose(reach))
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		s.Index(0).SetString(f.prose(reach))
		v.Set(s)
	default:
		panic(fmt.Sprintf("a carried field of kind %s; teach the filler before classifying it", v.Kind()))
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
		Sources: queries.Sources{Company: func() *config.Company { return company }},
	})

	projection, ok := a.Stream().Org(auth.ReachAdmin).(api.OrgProjection)
	if !ok {
		t.Fatalf("the org surface answers %T, want api.OrgProjection", a.Stream().Org(auth.ReachAdmin))
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

// THE COMPANY'S CLOCK IS ON THE ANONYMOUS PROJECTION, AS THE ENGINE RESOLVED IT
// (ADR-0018).
//
// Every date a dashboard reader is shown is cut on it, so a screen without it
// cuts "today" on the browser's own zone and disagrees with the engine's own
// due bands beside it. And an unwritten clock arrives as `UTC` rather than
// empty, because a client defaulting an empty string defaults it to ITS OWN
// zone — the very disagreement the field exists to end. A node with no company
// still answers `{}`.
func TestTheOrgProjectionCarriesTheCompanysClock(t *testing.T) {
	t.Parallel()
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
				Sources: queries.Sources{Company: func() *config.Company { return tc.company }},
			})
			projection, ok := a.Stream().Org(auth.ReachPublic).(api.OrgProjection)
			if !ok {
				t.Fatalf("the org surface answers %T, want api.OrgProjection", a.Stream().Org(auth.ReachPublic))
			}
			if projection.Timezone != tc.want {
				t.Errorf("the projection's clock is %q, want %q", projection.Timezone, tc.want)
			}
		})
	}

	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: func() *config.Company { return nil }},
	})
	body, err := json.Marshal(a.Stream().Org(auth.ReachAdmin))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "{}" {
		t.Errorf("a node with no company answers %s, want {} — the shape the "+
			"dashboard reads as nothing loaded", body)
	}
}

// WHAT A SEAT RUNS ON AND WHAT IT CAN REACH ARE PUBLISHED AS THE ENGINE
// RESOLVES THEM.
//
// The model chain is every phase's, the way a turn resolves it: a flat
// llm_<phase> field wins, a phase naming nothing falls to the seat's llm, and a
// seat naming nothing lands on the company's `default` provider. The tool
// sources are the builtins, every shared server, and a per-seat template only
// where the seat — or its unit — declares credentials for it, which is the
// grant the engine starts a seat's children by. A human seat runs neither. The
// budgets are the ceilings as written, company and seat alike, and a window
// nobody caps is absent rather than zero.
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
	a := chartApp(t, company)
	got := orgOf(t, a, "secret")

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

	// A company with no provider has no chain to show, and says nothing
	// rather than an empty one.
	bare := &config.Company{Name: "Acme", Roles: []config.Role{{Name: "CTO"}}}
	b := chartApp(t, bare)
	if seat := orgOf(t, b, "secret").Roles[0]; seat.LLM != nil || !slices.Equal(seat.ToolSources, []string{"builtin"}) {
		t.Errorf("with no provider and no server, the seat carries %v and %v; want no chain and only the builtins",
			seat.LLM, seat.ToolSources)
	}
}

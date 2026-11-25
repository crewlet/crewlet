package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
)

// THE GENERIC API TOOLS: the person's whole API, through their assistant
// (ADR-0052's parity), and how Ask Crewlet reads every domain:
// [DescribeAPITool] lists the routes the person may use, by domain or by words,
// and answers one's documentation from the API reference; [ReadAPITool] reads
// (GET), narrowed to the fields a call selects, then shaped and fenced by
// [Shape]; [ProposeAPIChangeTool] writes (POST, PUT, PATCH, DELETE), always a
// proposal previewed from the resource as it is, sent only from the person's
// approval (ADR-0051).
//
// A call names a route as [RouteTable] holds it ("PATCH /config"), each
// wildcard's value apart, every value one escaped segment ([at]), a dot
// segment refused, so it is only a request to a mounted route. See [API.Bind].

// GenericBodyBytes is the most a generic change's request body may hold:
// sixty-four kibibytes. What the person approves is what the body moves, drawn
// as rows every approver reviews ([diffRows] of them, the rest counted), and
// the body itself, redacted, is an administrator's technical details, within
// the card's own bound of 256 KiB, which the before and after rows share. What
// a route takes beyond that (a file's bytes, an artifact's bundle) has its own
// screen and its own upload path.
const GenericBodyBytes = 64 << 10

// GenericPage is the page a generic read asks a list route for when the model
// names none: twenty-five rows, under a third of what one step reads at the
// widest row a list route answers today (a turn, about a kilobyte), so the
// read is answered whole with the route's cursor to read on.
const GenericPage = 25

// genericReadQuery is what a generic read asks a route for where the model
// sets none of it, for a route whose own default answer is past what one step
// reads: a seat's read answers its call history (whole transcripts) unless
// asked not to. THE ROUTE'S DEFAULT STAYS ITS OWN: the dashboard and every
// other caller read it as they always have, and only the assistant's read is
// bounded, here, where it is sent (ADR-0061). A parameter the model names is
// sent as it named it.
var genericReadQuery = map[string]url.Values{
	"GET /agents/{id}": seatAlone,
	// A ROOM ANSWERS WHOLE, so fifteen of the largest fit one step where
	// [GenericPage] of them do not.
	"GET /chatlet/channels": {"limit": {"15"}},
}

// The generic tools, by name.
const (
	DescribeAPITool      = "describe_api"
	ReadAPITool          = "read_api"
	ProposeAPIChangeTool = "propose_api_change"
)

// Route is one route of the API's table as the generic tools reach it.
type Route struct {
	// Pattern is "METHOD /path" exactly as the API mounts it.
	Pattern string
	// Action is the verb the router decides it by, or empty for a route that
	// decides inside its handler: a question (its registered posture), an
	// overview read (the push table) or the act route (each tool's own verb).
	Action authz.Action
	// Absent is what a PUT here does at an address that holds nothing, as
	// the route was mounted with it ([authz.Absent]); nil for every other
	// route, and for a PUT that declares nothing, which then creates
	// nothing a preview would propose.
	Absent *authz.Absent
	// Effect is what a POST here does to what its path names, as the route
	// was mounted with it ([authz.Effect]): what its change is titled by and
	// whether it is destructive. The act route calls a tool
	// ([authz.EffectCalls]). Empty for every other method.
	Effect authz.Effect
	// DryRun is a write that answers `?dry_run=true` as a check
	// ([authz.Policy.Checked]): the preview runs it before any card is drawn.
	DryRun bool
	// Summary is a write that requires a revision summary
	// ([authz.Policy.Summarised]): the change carries one as `_summary` in
	// its body, which the digest binds.
	Summary bool
	// Replaces is a PUT that stores its body as the whole object its GET
	// answers ([authz.Policy.Replacing]): a field the body leaves out is
	// removed, so the preview refuses one the call did not declare in
	// `removes`.
	Replaces bool
	// Apps are the apps a read deciding inside its handler draws on, as the
	// question behind it is registered; nil where it needs none.
	Apps []iam.Grant
	// QuestionApps are, for [QuestionRoute], the apps each question it asks
	// draws on, by name.
	QuestionApps map[string][]iam.Grant
}

// QuestionRoute is the route any registered question is asked on by name.
const QuestionRoute = "GET /query/{what}"

// VerbApps are the apps a read decided by verb a draws on: its row's app, the
// apps it admits on any one of, or admin for an administrator's verb.
func VerbApps(a authz.Action) []iam.Grant {
	if app, _ := authz.AppOf(a); app != "" {
		return []iam.Grant{app}
	}
	if apps, _ := authz.AnyOf(a); len(apps) > 0 {
		return slices.Clone(apps)
	}
	if class, _ := authz.ClassOf(a); class == authz.ClassAdmin {
		return []iam.Grant{iam.GrantAdmin}
	}
	return nil
}

// createsWhenAbsent reports a route that creates what an empty address names.
func (r Route) createsWhenAbsent() bool { return r.Absent != nil && r.Absent.Creates }

// Method is the route's method.
func (r Route) Method() string {
	method, _, _ := strings.Cut(r.Pattern, " ")
	return method
}

// Path is the route's path pattern.
func (r Route) Path() string {
	_, path, _ := strings.Cut(r.Pattern, " ")
	return path
}

// reads reports a route a read sends: a GET.
func (r Route) reads() bool { return r.Method() == http.MethodGet }

// writeMethods are the methods a generic change may send.
var writeMethods = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

// RouteTable is the API's route table as the generic tools read it: every
// route the assistant could reach, sorted by pattern.
type RouteTable struct {
	routes    []Route
	byPattern map[string]Route
	// kept are the routes left out for a verb on the human-only list, so a
	// call naming one is told why in the person's own words.
	kept map[string]authz.Action
}

// NewRouteTable is the table of routes, refusing a route that is not "METHOD
// /path", a method no generic tool sends, a pattern given twice, and a route
// mounted on a verb the authority table does not know. A route whose verb the
// assistant never takes is left out here, so nothing offered can name it.
func NewRouteTable(routes []Route) (*RouteTable, error) {
	t := &RouteTable{byPattern: map[string]Route{}, kept: map[string]authz.Action{}}
	for _, r := range routes {
		method, path, ok := strings.Cut(r.Pattern, " ")
		switch {
		case !ok || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " ?#"):
			return nil, fmt.Errorf("catalog: route %q is not METHOD /path", r.Pattern)
		case method != http.MethodGet && !slices.Contains(writeMethods, method):
			return nil, fmt.Errorf("catalog: route %q sends a method no generic tool sends",
				r.Pattern)
		case t.byPattern[r.Pattern].Pattern != "":
			return nil, fmt.Errorf("catalog: route %q is given twice", r.Pattern)
		}
		if r.Pattern == ActRoute {
			// THE ACT ROUTE CALLS THE TOOL ITS PATH NAMES, in every table: it is
			// mounted outside a router, so no policy says so.
			r.Effect = authz.EffectCalls
		}
		if r.Action != "" {
			class, known := authz.AssistOf(r.Action)
			if !known {
				return nil, fmt.Errorf("catalog: route %q is mounted on %q, which the "+
					"authority table has no row for", r.Pattern, r.Action)
			}
			if class == authz.AssistNever {
				t.kept[r.Pattern] = r.Action
				continue
			}
		}
		t.byPattern[r.Pattern] = r
		t.routes = append(t.routes, r)
	}
	slices.SortFunc(t.routes, func(a, b Route) int { return strings.Compare(a.Pattern, b.Pattern) })
	return t, nil
}

// Routes are the table's routes, sorted by pattern.
func (t *RouteTable) Routes() []Route { return slices.Clone(t.routes) }

// Lookup is the route a pattern names.
func (t *RouteTable) Lookup(pattern string) (Route, bool) {
	r, ok := t.byPattern[strings.TrimSpace(pattern)]
	return r, ok
}

// patterns are the patterns of the routes keep admits.
func (t *RouteTable) patterns(keep func(Route) bool) []string {
	var out []string
	for _, r := range t.routes {
		if keep(r) {
			out = append(out, r.Pattern)
		}
	}
	return out
}

// readerOf is the read route serving path itself, where the table has one: the
// representation a change to path is previewed against.
func (t *RouteTable) readerOf(path string) (Route, bool) {
	for _, r := range t.routes {
		if r.Method() == http.MethodGet && matches(r.Pattern, http.MethodGet, path) {
			return r, true
		}
	}
	return Route{}, false
}

// API is the curated catalogue and the generic API tools over the route table
// the API binds once it is built: the [Catalogue] a node's assistant offers.
// Until [API.Bind] has run it offers the curated tools alone, so a node whose
// table is not bound offers nothing it could not check.
type API struct {
	table atomic.Pointer[RouteTable]
}

// Bind hands the API's route table to the catalogue, once, as the guarded
// routes with the verb each is mounted with, the named and overview reads, the
// question route and the act route. THE TABLE IS A FILTER, NEVER AUTHORITY: a
// route whose verb the authority table keeps from the assistant is left out of
// what the model is offered, and every call is still decided by the API's own
// guard, as the person, through the turn's grant (ADR-0050): the guard refuses
// a write to a read grant, the authority table's assistant column the
// human-only verbs, and the request binding holds an exec grant to the one
// request that was approved.
func (a *API) Bind(routes []Route) error {
	table, err := NewRouteTable(routes)
	if err != nil {
		return err
	}
	if !a.table.CompareAndSwap(nil, table) {
		return errors.New("catalog: the API's route table is already bound")
	}
	return nil
}

// Table is the bound route table, nil before [API.Bind].
func (a *API) Table() *RouteTable { return a.table.Load() }

// Offered are [Offered]'s curated tools, then the generic API tools over the
// bound table.
func (a *API) Offered(p iam.Principal, acts []string) []Tool {
	out := Offered(p, acts)
	if table := a.table.Load(); table != nil {
		out = append(out, GenericTools(table, p, acts)...)
	}
	return out
}

// GenericTools are the generic API tools over table, for p, whose act route
// writes are the act tools acts names: the routes p may use, the read where
// the table holds a read, and the change where it holds a change. Built for
// one turn, because what they may name is the person's.
func GenericTools(table *RouteTable, p iam.Principal, acts []string) []Tool {
	g := generic{table: table, person: p, acts: slices.Clone(acts)}
	// A TOOL ONLY OVER ROUTES OF ITS KIND: a table with no change route
	// offers no change.
	tools := []Tool{g.describe()}
	if len(table.patterns(Route.reads)) > 0 {
		tools = append(tools, g.read())
	}
	if len(table.patterns(func(r Route) bool { return !r.reads() })) > 0 {
		tools = append(tools, g.change())
	}
	for i := range tools {
		tools[i].Parameters = tools[i].schema.JSON()
		if err := tools[i].validate(); err != nil {
			// A BUILD DEFECT, as a curated tool that does not validate
			// is ([Tools]): the descriptors are this file's own.
			panic(err)
		}
	}
	return tools
}

// generic is one turn's generic API tools: the table, the person and the act
// tools they may send.
type generic struct {
	table  *RouteTable
	person iam.Principal
	acts   []string
}

// The arguments every generic call names a request by.
var (
	routeArgument = text("The route exactly as describe_api lists it, its method and its "+
		"path pattern: \"GET /worklet/{id}\", \"PATCH /config\".", 160)
	pathArgument = &schema{kind: "object", payload: true, description: "Each `{name}` in " +
		"the route's path, by name, as its value: {\"id\": \"ENG-42\"}. A `{name...}` " +
		"value may hold slashes."}
	queryArgument = &schema{kind: "object", payload: true, description: "The query " +
		"parameters, each a string or a list of strings: {\"limit\": \"20\"}."}
	bodyArgument = &schema{kind: "object", payload: true, description: "The JSON request " +
		"body the route takes, exactly as its documentation states it."}
	summaryArgument = text("One line for the configuration's history saying what the "+
		"change does, in plain words: \"Add a QA Engineer seat to Product and "+
		"Engineering\". A route that keeps a history gets it as its revision summary "+
		"(you never send X-Summary or _summary yourself), and the person sees it on the "+
		"card.", lineBytes)
)

// SummaryKey is the body key a [Route.Summary] write reads its revision summary
// from (internal/api/configapi), the one channel the request digest binds.
const SummaryKey = "_summary"

func (g generic) describe() Tool {
	return Tool{
		Name: DescribeAPITool, Kind: KindDocs, Tier: TierRead,
		Phrase: "look up the API",
		Description: "Find the routes this person can use, or read one route's " +
			"documentation. With a domain, it lists that domain's routes; with query, the " +
			"routes whose path or documentation holds those words; each with what it is " +
			"for, its query parameters and what it answers. With a route, it answers that " +
			"route's documentation from the engine's API reference. Look a route up here " +
			"before read_api or propose_api_change rather than guessing it. The domains " +
			"this person can read, with the routes each starts with:\n" + g.domainMap(),
		schema: object(nil, map[string]*schema{
			"domain": text("One domain from the list, as it is written there: \"worklet\", "+
				"\"agents\".", nameBytes),
			"query": text("Words to find routes by, matched against each route's path and "+
				"documentation.", lineBytes),
			"route": text("One route to read the documentation of, \"METHOD /path\" "+
				"as the list names it.", 160),
		}),
		// IN THE PERSON'S WORDS: what is being looked up is the engine's
		// own reference, which a step never names by its route.
		Label: func(a map[string]any) string {
			if route := Args(a).String("route"); route != "" && len(route) <= 160 {
				return "Looking up how to work with " + lowerFirst(RequestTitle(route, nil))
			}
			return "Looking up what can be done"
		},
		Docs: g.document,
	}
}

func (g generic) read() Tool {
	return Tool{
		Name: ReadAPITool, Kind: KindRead, Tier: TierRead, Phrase: "read from the API",
		// THE ENGINE'S OWN RECORDS, by the one rule for a surface the engine
		// hosts; TrustOf reads the route a call named.
		Source: provenance.SourceAPIAnswer, Trust: provenance.TrustForHosted("", nil, false),
		Description: "Read anything this person can read in the engine: a GET on any route " +
			"describe_api lists, with its path values and query parameters. The answer is the " +
			"route's own JSON, cut to what one step reads, so narrow it with the route's own " +
			"filters and name the fields you need in select. A list route that pages by limit " +
			"is read 25 rows at a time unless you name a limit; read on with the cursor it " +
			"answers.",
		schema: object([]string{"route"}, map[string]*schema{
			"route": routeArgument, "path": pathArgument, "query": queryArgument,
			SelectArgument: selectArgument,
			"cursor": text("The cursor a list route answered, to read its next page.",
				cursorBytes),
		}),
		Routes: g.table.patterns(Route.reads),
		Build: func(a map[string]any) (exec.Request, error) {
			req, _, err := g.request(Args(a), true)
			return req, err
		},
		Label:   genericLabel("Reading"),
		Fence:   true,
		Infer:   true,
		TrustOf: g.trustOf,
		AppsOf:  g.appsOf,
	}
}

// appsOf are the apps a read's answer drew on, of those the person holds: its
// route's verb's, or the registered question's for a route that decides
// inside. A card and an answer are tagged with them, so a person who loses an
// app is no longer shown what was read through it.
func (g generic) appsOf(args Args) []iam.Grant {
	route, ok := g.table.Lookup(args.String("route"))
	if !ok {
		return nil
	}
	apps := route.Apps
	switch {
	case route.Action != "":
		apps = VerbApps(route.Action)
	case route.Pattern == QuestionRoute:
		path, _ := pathValues(args["path"])
		apps = route.QuestionApps[path["what"]]
	}
	var held []iam.Grant
	for _, app := range apps {
		if g.person.Can(app) {
			held = append(held, app)
		}
	}
	return held
}

func (g generic) change() Tool {
	return Tool{
		Name: ProposeAPIChangeTool, Kind: KindWrite, Routed: true,
		Tier:   TierConsequential,
		Phrase: "change something through the API",
		Description: "Propose any change this person can make in the engine's API: a " +
			"POST, PUT, PATCH or DELETE on any route describe_api lists, with its path " +
			"values, query parameters and JSON body exactly as the route's documentation " +
			"states them. Nothing is sent until the person approves: they are shown, in " +
			"plain words, what the change does to the resource as it is now. " +
			"Use a dedicated write tool whenever one makes the change (filing or updating a " +
			"task, a page, a share, the principles, a chat message or room), since its " +
			"preview is shaped for it: a call on the act route one of them makes is refused " +
			"here. Use this for everything else. Read the resource first, and send its whole " +
			"new value where the route replaces it (a PUT): every field it holds now that " +
			"should stay. A field the body leaves out is removed, so a removal the person did " +
			"not ask for is refused unless `removes` names it. The body is at most 64 KiB.",
		schema: object([]string{"route"}, map[string]*schema{
			"route": routeArgument, "path": pathArgument, "query": queryArgument,
			"body": bodyArgument, "summary": summaryArgument, RemovesArgument: removesArgument,
		}),
		Routes: g.table.patterns(func(r Route) bool { return !r.reads() }),
		Build: func(a map[string]any) (exec.Request, error) {
			req, _, err := g.request(Args(a), false)
			return req, err
		},
		Label:   genericLabel("Preparing"),
		Preview: g.preview,
	}
}

// genericLabel is a generic call's step sentence: what it does, and what it
// does it to, titled by the one rule its card is ([RequestTitle]): "Reading
// role · engineer", never "Reading GET /config/roles/{id}". The request
// itself is on the step beside it, in mono.
func genericLabel(doing string) func(map[string]any) string {
	return func(a map[string]any) string {
		route := Args(a).String("route")
		if route == "" || len(route) > 160 {
			return doing + " a record"
		}
		return doing + " " + lowerFirst(callTitle(Args(a)))
	}
}

// request is the one request a generic call names, and the route it names it
// on: a route of the table, of the tool's own kind, its wildcards filled from
// path and its query from query, and for a change the body.
func (g generic) request(args Args, reading bool) (exec.Request, Route, error) {
	pattern := args.String("route")
	route, ok := g.table.Lookup(pattern)
	if tool, owned := webRoutes[pattern]; owned {
		return exec.Request{}, Route{}, fmt.Errorf("%s is read with %s, which holds the "+
			"web's rules", pattern, tool)
	}
	if verb, kept := g.table.kept[pattern]; kept {
		return exec.Request{}, Route{}, fmt.Errorf("%s stays with the person: %s", pattern,
			authz.AssistNeverSentence(verb))
	}
	if !ok {
		return exec.Request{}, Route{}, fmt.Errorf("%q is not a route you can use; "+
			"describe_api lists them", pattern)
	}
	switch {
	case reading && !route.reads():
		return exec.Request{}, Route{}, fmt.Errorf("%s changes something; propose it with "+
			"%s", pattern, ProposeAPIChangeTool)
	case !reading && route.reads():
		return exec.Request{}, Route{}, fmt.Errorf("%s only reads; send it with %s",
			pattern, ReadAPITool)
	}
	path, err := pathValues(args["path"])
	if err != nil {
		return exec.Request{}, Route{}, err
	}
	query, err := queryValues(args["query"])
	if err != nil {
		return exec.Request{}, Route{}, err
	}
	if cursor := args.String("cursor"); cursor != "" {
		if !reading {
			return exec.Request{}, Route{}, errors.New("a change reads no cursor")
		}
		query.Set("cursor", cursor)
	}
	if _, given := args[SelectArgument]; given && !reading {
		return exec.Request{}, Route{}, errors.New("a change selects no fields")
	}
	if _, selectErr := Selection(args); selectErr != nil {
		return exec.Request{}, Route{}, selectErr
	}
	if reading {
		for name, values := range genericReadQuery[pattern] {
			if _, named := query[name]; !named {
				query[name] = slices.Clone(values)
			}
		}
	}
	if reading && query.Get("limit") == "" && pagedByLimit(pattern) {
		// A LIST READ IS ONE PAGE UNLESS THE MODEL ASKED FOR ONE: a route that
		// pages by `limit` answers its own default page (a hundred events,
		// fifty turns) when none is named, which is close to or past what one
		// step reads, and a cut read gives the model nothing. A first page
		// it can answer from, with the route's own cursor to read on, is the
		// bounded read ([GenericPage]).
		query.Set("limit", strconv.Itoa(GenericPage))
	}
	if _, asked := query[authz.DryRunQuery]; asked && !reading {
		// A CHANGE IS THE WRITE: the preview asks the route's own check
		// before the person sees it, so a proposal carrying the flag would
		// be a write that stores nothing, approved as though it did.
		return exec.Request{}, Route{}, fmt.Errorf("leave %s out: a change is checked "+
			"for you before the person sees it, and what they approve is the write",
			authz.DryRunQuery)
	}
	req, err := fill(pattern, path, query)
	if err != nil {
		return exec.Request{}, Route{}, err
	}
	if route.Action == "" && route.Pattern == ActRoute {
		// THE ACT ROUTE CARRIES A TOOL OF THE PERSON'S OWN: the viewer's
		// acts list it, which is what the authority table could admit them
		// to, and the act route decides the call again on the tool's own
		// verb.
		tool := path["tool"]
		if !slices.Contains(g.acts, tool) {
			return exec.Request{}, Route{}, fmt.Errorf("the act tool %q is not one you "+
				"can use", tool)
		}
		if class, _ := authz.AssistOf(authz.Action(tool)); class == authz.AssistNever {
			return exec.Request{}, Route{}, fmt.Errorf("%s stays with the person: %s", tool,
				authz.AssistNeverSentence(authz.Action(tool)))
		}
	}
	if body, given := args["body"]; given {
		if reading {
			return exec.Request{}, Route{}, errors.New("a read sends no body")
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return exec.Request{}, Route{}, fmt.Errorf("the body does not encode: %w", err)
		}
		if len(encoded) > GenericBodyBytes {
			return exec.Request{}, Route{}, fmt.Errorf("the body is %d bytes and the most "+
				"a change through the API carries is %d; send a narrower change", len(encoded),
				GenericBodyBytes)
		}
		req.Body = encoded
	}
	if !reading && route.Pattern == ActRoute {
		// A CALL IS ITS TOOL'S ARGUMENTS, in the one shape the act route
		// reads, and a call a curated write makes is that write's: its
		// preview reads what the tool changes and binds the revision it
		// read, where this card can show only the call ([curatedCall]).
		sent, err := callArgs(req.Body)
		if err != nil {
			return exec.Request{}, Route{}, err
		}
		if curated, ok := curatedCall(path["tool"], sent); ok {
			return exec.Request{}, Route{}, fmt.Errorf("%s has a change of its own: propose "+
				"it with %s, whose card shows what it changes", path["tool"], curated)
		}
	}
	if !reading && route.Summary {
		body, summaryErr := summarised(req.Body, args)
		if summaryErr != nil {
			return exec.Request{}, Route{}, summaryErr
		}
		req.Body = body
	}
	return req, route, nil
}

// actBody is the one body a call on the act route sends
// (internal/api/operator's act transport, which refuses any other).
const actBody = `a call on the act route sends one JSON object, {"args": {...}}, ` +
	`its tool's arguments in args and nothing beside them`

// callArgs are the arguments a call on the act route hands its tool: its body's
// `args`. A body of any other shape is refused here, before a card is drawn,
// rather than by the route once the person approved it.
func callArgs(body []byte) (map[string]any, error) {
	var call map[string]json.RawMessage
	if json.Unmarshal(body, &call) != nil || call == nil {
		return nil, errors.New(actBody)
	}
	for field := range call {
		if field != "args" {
			return nil, fmt.Errorf("%s, and the body names %q", actBody, field)
		}
	}
	args := map[string]any{}
	if raw, given := call["args"]; given && string(raw) != "null" {
		if json.Unmarshal(raw, &args) != nil {
			return nil, errors.New(actBody)
		}
	}
	return args, nil
}

// curatedCall is the curated write that makes the act route's call of tool
// with args, and whether one does: a write executing as tool on the act route
// whose schema declares every argument the call sends.
//
// THE GENERIC CHANGE DEFERS TO IT. A tool on the act route is a call, so the
// generic card can show what the call sends and never what it does to what is
// there; a curated write's preview reads that, and binds the revision it read
// (ADR-0051). A call sending an argument no curated write takes is one only
// the generic change can make, and stays its, so nothing the person may do is
// lost to the assistant.
func curatedCall(tool string, args map[string]any) (string, bool) {
	for _, t := range Tools() {
		if t.Kind != KindWrite || t.Act != tool || !slices.Equal(t.Routes, []string{ActRoute}) {
			continue
		}
		takes := true
		for arg := range args {
			if _, declared := t.schema.properties[arg]; !declared {
				takes = false
			}
		}
		if takes {
			return t.Name, true
		}
	}
	return "", false
}

// summarised is a [Route.Summary] write's body with its revision summary in
// it: the one the body already states, else the call's `summary`, else one
// naming the change from its route. IN THE BODY, never a header, because the
// body is what the request digest binds: the history line the route records
// is the one the person approved on the card (ADR-0055). A body that is not
// one JSON object is left as it is, for the route's own check to refuse.
func summarised(body []byte, args Args) ([]byte, error) {
	var doc map[string]any
	if len(body) == 0 {
		doc = map[string]any{}
	} else if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return body, nil //nolint:nilerr // The route's check refuses it in its own words.
	}
	if stated, _ := doc[SummaryKey].(string); strings.TrimSpace(stated) != "" {
		return body, nil
	}
	summary := strings.Join(strings.Fields(args.String("summary")), " ")
	if summary == "" {
		summary = "Change " + lowerFirst(callTitle(args)) + " through Ask Crewlet"
	}
	doc[SummaryKey] = summary
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("the body does not encode: %w", err)
	}
	if len(encoded) > GenericBodyBytes {
		return nil, fmt.Errorf("the body and its summary are %d bytes and the most a "+
			"change through the API carries is %d; send a narrower change", len(encoded),
			GenericBodyBytes)
	}
	return encoded, nil
}

// fill is the request on pattern with its wildcards filled from path, each
// value escaped segment by segment, and its query. A wildcard with no value, a
// value given for no wildcard, and a dot segment are refused.
func fill(pattern string, path map[string]string, query url.Values) (exec.Request, error) {
	method, template, _ := strings.Cut(pattern, " ")
	segments := strings.Split(template, "/")
	used := map[string]bool{}
	for i, segment := range segments {
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
		rest := strings.HasSuffix(name, "...")
		name = strings.TrimSuffix(name, "...")
		if name == "$" {
			segments[i] = ""
			continue
		}
		value, given := path[name]
		if !given || strings.TrimSpace(value) == "" {
			return exec.Request{}, fmt.Errorf("%s needs a value for {%s}", pattern, name)
		}
		used[name] = true
		parts := []string{value}
		if rest {
			parts = strings.Split(value, "/")
		}
		for j, part := range parts {
			if part == "" || part == "." || part == ".." {
				return exec.Request{}, fmt.Errorf("{%s} holds the segment %q, which names "+
					"no resource", name, part)
			}
			parts[j] = url.PathEscape(part)
		}
		segments[i] = strings.Join(parts, "/")
	}
	for _, name := range slices.Sorted(maps.Keys(path)) {
		if !used[name] {
			return exec.Request{}, fmt.Errorf("%s has no {%s}", pattern, name)
		}
	}
	req := exec.Request{Method: method, Path: strings.Join(segments, "/")}
	if len(query) > 0 {
		req.Query = query
	}
	return req, nil
}

// pathValues reads a call's `path`: every value a string.
func pathValues(v any) (map[string]string, error) {
	out := map[string]string{}
	if v == nil {
		return out, nil
	}
	values, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("path is an object of each wildcard's value")
	}
	for name, value := range values {
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("path.%s is a string", name)
		}
		out[name] = s
	}
	return out, nil
}

// queryValues reads a call's `query`: every value a string or a list of them.
func queryValues(v any) (url.Values, error) {
	out := url.Values{}
	if v == nil {
		return out, nil
	}
	values, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("query is an object of each parameter's value")
	}
	for name, value := range values {
		switch t := value.(type) {
		case string:
			out.Add(name, t)
		case []any:
			for _, e := range t {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("query.%s is a string or a list of strings", name)
				}
				out.Add(name, s)
			}
		default:
			return nil, fmt.Errorf("query.%s is a string or a list of strings", name)
		}
	}
	return out, nil
}

// outsideRoutes are the read routes whose answers carry text from outside the
// company, by path prefix: the events, turns and executors that quote what was
// said on chat surfaces this engine does not own; a seat's executor sessions
// (a harness's whole record of a run, every file it read and every command's
// output, and the listing beside it, whose asks and endings quote them); and
// the integrations and their agents' access, which carry a vendor's refusals
// and its names for levels, accounts and scope entries; and the atlas, whose
// rows an integration's users and anybody with a push name, one entry read as
// its sources say ([atlasEntryTrust]). An answer of one is fenced as external; every
// other route's as a colleague's, unless it carries an executor's question
// (the decisions waiting on a person, and the ones they answered:
// [decisionRows]) or names a connected knowledge base as its backend.
var outsideRoutes = []string{
	"/agents/{id}/sessions", "/agents/{id}/sessions/{turn_id}",
	"/events", "/turns", "/executors", "/query/{what}",
	"/integrations", "/setup/integrations/{kind}/access", "/atlas",
}

// decisionRows is where a decisions answer keeps an executor's question: on
// a row's `run`, among the decisions waiting on a person (/worklet/decisions)
// and the ones they answered (/worklet/decisions/answered) alike.
const decisionRows, runAskField = "items", "run.question"

// webRoutes are the routes the web tools own, by their tool: a page is read
// only at an address the turn saw, which the generic read does not check.
var webRoutes = map[string]string{
	"GET /research/page":   FetchWebPageToolName,
	"GET /research/search": SearchWebToolName,
}

// OwnedByWebTool reports a route only a web tool reads ([webRoutes]).
func OwnedByWebTool(pattern string) bool {
	_, owned := webRoutes[pattern]
	return owned
}

// knowledgeQuestion is the question a knowledge search is asked as.
const knowledgeQuestion = "knowledge"

// trustOf is a generic read's trust, by the route it was sent to and by what
// its answer holds: an executor's question is read by the one rule for an
// executor's words (provenance.Executor) whichever route carries it, as the
// curated reads of the same decisions read it.
func (g generic) trustOf(args Args, answer any) Trusted {
	route, _ := g.table.Lookup(args.String("route"))
	path, _ := pathValues(args["path"])
	switch {
	case route.Pattern == atlasEntryRoute:
		return atlasEntryTrust(answer)
	case route.Pattern == QuestionRoute && path["what"] == knowledgeQuestion:
		// A KNOWLEDGE SEARCH is read as each backend labels its hits.
		return knowledgeTrust(answer)
	case route.Path() == "/executors" && carriesRunAsk(answer, "runs", "question"):
		// AN EXECUTOR'S QUESTION is its words whichever route carries it.
		l := provenance.Executor("")
		return Trusted{Trust: l.Trust, Source: l.Source}
	}
	for _, prefix := range outsideRoutes {
		if strings.HasPrefix(route.Path(), prefix) {
			return Trusted{Trust: provenance.TrustExternal}
		}
	}
	if carriesRunAsk(answer, decisionRows, runAskField) {
		l := provenance.Executor("")
		return Trusted{Trust: l.Trust, Source: l.Source}
	}
	return knowledgeTrust(answer)
}

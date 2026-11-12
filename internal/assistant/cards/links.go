package cards

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// screen is one dashboard screen an open in app link may name: its address
// with each parameter a placeholder, and the app a person needs to open it at
// all (empty for the overview every person reads).
type screen struct {
	address string
	// query is the address's query, each pair's value a placeholder with the
	// literal around it ("peek=atlas:{id}&trace={direction}"), "" for a
	// screen its path alone opens. Kept apart from address, which the
	// printed-address suite resolves as a path.
	query string
	app   iam.Grant
	// label is the link's text when nothing names it.
	label string
	// noun is what one record of a record screen is, for the link's text
	// when nothing names it ("Open seat"): a seat's handle or a turn's id
	// is how the address spells it, and "pm" or a UUID on a button names
	// nothing a person knows.
	noun string
	// id, where set, is the grammar this screen's `{id}` is held to in place
	// of a uuid: a task is named by its key as well as its id.
	id func() *regexp.Regexp
}

// links is the link table: every screen the assistant may link to, by the
// dashboard's own screen name (app/routes.ts), at the address its router
// resolves.
//
// HELD AGAINST THE ROUTER, twice: every address below is a Go string the
// dashboard's printed-address suite (app/links.test.ts) resolves with its
// placeholders filled, and the golden file this package's fixture test writes
// (testdata/links.json) is replayed by the dashboard's assistant link suite,
// which asks the router which screen each rendered link opens. A screen
// renamed or moved there fails here's build on the next run of either.
//
// The placeholders are the router fixture's own (`{KEY}`, `{id}`, `{handle}`,
// `{CONTAINER}`, `{turn_id}`), so one table fills them on both sides.
var links = map[string]screen{
	"home":             {address: "#/home", label: "Home"},
	"inbox":            {address: "#/inbox", app: iam.GrantWorklet, label: "Inbox"},
	"me":               {address: "#/me", app: iam.GrantWorklet, label: "My work"},
	"worklet-projects": {address: "#/worklet", app: iam.GrantWorklet, label: "Projects"},
	"worklet-tasks":    {address: "#/worklet/tasks", app: iam.GrantWorklet, label: "Tasks"},
	"worklet-search":   {address: "#/worklet/search", app: iam.GrantWorklet, label: "Search"},
	"project":          {address: "#/worklet/{KEY}", app: iam.GrantWorklet},
	"item":             {address: "#/worklet/{id}", app: iam.GrantWorklet, id: taskName},
	"structure":        {address: "#/company/org", label: "Org chart"},
	"structure-table":  {address: "#/company/org/table", label: "Table"},
	"principles":       {address: "#/company/principles", label: "Principles"},
	"seat":             {address: "#/company/org/seats/{handle}", noun: "seat"},
	"budgets":          {address: "#/company/budgets", label: "Budgets"},
	"schedules":        {address: "#/company/schedules", label: "Schedules"},
	"integrations":     {address: "#/company/integrations", app: iam.GrantAdmin, label: "Integrations"},
	"live":             {address: "#/live", label: "Now running"},
	"turns":            {address: "#/live/turns", label: "Turns"},
	"turn":             {address: "#/live/turns/{id}", noun: "turn"},
	"executors":        {address: "#/live/executors/{turn_id}", noun: "executor"},
	"doclet":           {address: "#/doclet", app: iam.GrantDoclet, label: "Doclet"},
	"container":        {address: "#/doclet/{CONTAINER}", app: iam.GrantDoclet},
	"page":             {address: "#/doclet/pages/{id}", app: iam.GrantDoclet, noun: "page"},
	"chatlet":          {address: "#/chatlet/{id}", app: iam.GrantChatlet, noun: "room"},
	"chatlet-activity": {address: "#/chatlet/activity", app: iam.GrantChatlet, label: "Activity"},
	"chatlet-search":   {address: "#/chatlet/search", app: iam.GrantChatlet, label: "Search chat"},
	"artifacts":        {address: "#/artifacts", label: "Artifacts"},
	"artifact":         {address: "#/artifacts/{id}", noun: "artifact"},
	"spend":            {address: "#/settings/spend", label: "Token spend"},
	"spend-tasks":      {address: "#/settings/spend/tasks", app: iam.GrantWorklet, label: "Expensive tasks"},
	"nodes":            {address: "#/settings/nodes", app: iam.GrantAdmin, label: "Nodes"},
	"backups":          {address: "#/settings/backups", app: iam.GrantAdmin, label: "Backups"},
	"config":           {address: "#/settings/config/revisions", app: iam.GrantAdmin, label: "Configuration"},
	// THE ATLAS, by the keys its screens read on arrival: an entry's page by
	// its id, and the map with the frame's peek open on an entry (the
	// frame's `peek=` key, `app/frame/peekAddress.ts`), lit along its trace
	// where the answer was one (the map's `trace=` key). Both map links read
	// "Show on the map", which is what they do.
	"atlas":       {address: "#/company/atlas", app: iam.GrantAtlas, label: "Atlas"},
	"atlas-entry": {address: "#/company/atlas/{id}", app: iam.GrantAtlas, noun: "entry"},
	"atlas-entry-on-map": {address: "#/company/atlas", query: "peek=atlas:{id}",
		app: iam.GrantAtlas, label: "Show on the map"},
	"atlas-trace-on-map": {address: "#/company/atlas", query: "peek=atlas:{id}&trace={direction}",
		app: iam.GrantAtlas, label: "Show on the map"},
}

// placeholder is a link parameter's placeholder and the grammar its value is
// held to. The grammar is what keeps a value from naming another screen: a task
// key passed where a project key goes would resolve to the task.
type placeholder struct {
	param string
	// value compiles its grammar at first use ([lazyGrammar]).
	value func() *regexp.Regexp
}

// placeholders are every placeholder the link table writes.
var placeholders = map[string]placeholder{
	"{KEY}":       {"key", lazyGrammar(`^[A-Z][A-Z0-9]{0,15}$`)},
	"{CONTAINER}": {"key", lazyGrammar(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)},
	"{id}":        {"id", uuidName},
	"{turn_id}":   {"id", uuidName},
	"{handle}":    {"handle", lazyGrammar(`^[a-z0-9][a-z0-9_-]{0,63}$`)},
	// A TRACE'S DIRECTION, as the atlas walks one: up to what depends on an
	// entry, down to what it depends on.
	"{direction}": {"direction", lazyGrammar(`^(?:up|down)$`)},
}

// queryPair is one pair of a screen's query template: its key, and its value
// as the literal before and after one placeholder.
type queryPair struct {
	key, before, after string
	placeholder        string
}

// queryPairs are screen's query template, pair by pair, in its order; a
// template every pair of which holds one placeholder of the table. The
// table is this package's own literals, so one written wrong is a panic at
// first use rather than a link that silently drops a key.
func queryPairs(template string) []queryPair {
	if template == "" {
		return nil
	}
	var out []queryPair
	for _, pair := range strings.Split(template, "&") {
		key, value, _ := strings.Cut(pair, "=")
		open, close := strings.Index(value, "{"), strings.Index(value, "}")
		if key == "" || open < 0 || close < open {
			panic(fmt.Sprintf("cards: the query template %q holds no placeholder in %q", template, pair))
		}
		placeholder := value[open : close+1]
		if _, ok := placeholders[placeholder]; !ok {
			panic(fmt.Sprintf("cards: the query template %q names %q, which the table does not hold",
				template, placeholder))
		}
		out = append(out, queryPair{key: key, before: value[:open], after: value[close+1:],
			placeholder: placeholder})
	}
	return out
}

// lazyGrammar is pattern compiled at its first use, not at init: a bounded
// repeat compiles to a program holding every count, and Gitlet's push
// hook is this binary, which pays every package's init on every push
// (cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit).
func lazyGrammar(pattern string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(pattern) })
}

// The two id grammars: a record's uuid, and a task's key or its uuid (a key a
// collision gave to another task is linked by the id instead).
var (
	uuidName = lazyGrammar(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	taskName = lazyGrammar(`^(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-` +
		`[0-9a-f]{12}|[A-Z][A-Z0-9]{0,15}-[0-9]{1,9})$`)
)

// Screens are every screen name the link table holds, sorted: the closed set
// open_in_app's `screen` argument takes.
func Screens() []string { return slices.Sorted(maps.Keys(links)) }

// ScreenApp is the app a person needs to open screen at all, "" for one every
// person reads, and false for a screen the table does not hold.
func ScreenApp(name string) (iam.Grant, bool) {
	s, ok := links[name]
	return s.app, ok
}

// ScreenParams are the parameters screen's address takes, in order: its
// path's, then its query's.
func ScreenParams(name string) []string {
	var out []string
	for _, segment := range strings.Split(links[name].address, "/") {
		if p, ok := placeholders[segment]; ok {
			out = append(out, p.param)
		}
	}
	for _, pair := range queryPairs(links[name].query) {
		out = append(out, placeholders[pair.placeholder].param)
	}
	return out
}

// ErrNoLink is a link the table cannot build: a screen it does not hold, a
// parameter missing or not of its grammar, or one the screen does not take.
var ErrNoLink = errors.New("cards: no such link")

// Link is the link to screen with params, at the address the dashboard's
// router resolves, labelled label (the screen's own name, or the first
// parameter's value, where empty).
func Link(name string, params map[string]string, label string) (wire.Link, error) {
	s, ok := links[name]
	if !ok {
		return wire.Link{}, fmt.Errorf("%w: the screen %q is not one of %s", ErrNoLink, name,
			words.Or(Screens()))
	}
	used := map[string]bool{}
	segments := strings.Split(s.address, "/")
	first := ""
	for i, segment := range segments {
		p, ok := placeholders[segment]
		if !ok {
			continue
		}
		v, grammar := params[p.param], p.value()
		if segment == "{id}" && s.id != nil {
			grammar = s.id()
		}
		if !grammar.MatchString(v) {
			return wire.Link{}, fmt.Errorf("%w: %s takes %s as %s, and %q is not one",
				ErrNoLink, name, p.param, grammar, v)
		}
		segments[i] = url.PathEscape(v)
		used[p.param] = true
		if first == "" {
			first = v
		}
	}
	query := url.Values{}
	for _, pair := range queryPairs(s.query) {
		p := placeholders[pair.placeholder]
		v := params[p.param]
		if !p.value().MatchString(v) {
			return wire.Link{}, fmt.Errorf("%w: %s takes %s as %s, and %q is not one",
				ErrNoLink, name, p.param, p.value(), v)
		}
		query.Set(pair.key, pair.before+v+pair.after)
		used[p.param] = true
	}
	for param := range params {
		if !used[param] {
			return wire.Link{}, fmt.Errorf("%w: %s takes no %s", ErrNoLink, name, param)
		}
	}
	switch {
	case label != "":
	case s.noun != "":
		label = "Open " + s.noun
	case s.label != "":
		// A SCREEN NAMED FOR WHAT IT DOES ("Show on the map") is named so
		// whatever its query carries: an id names nothing a person knows.
		label = s.label
	default:
		// A KEY A PERSON KNOWS ("FAIR-10", "ENG", "HOME") names its record.
		label = first
	}
	href := strings.Join(segments, "/")
	if len(query) > 0 {
		href += "?" + query.Encode()
	}
	return wire.Link{Label: label, Screen: name, Href: href}, nil
}

// ScreenOf is the record screen a dashboard address opens and the parameters
// it names: the link table read backwards, so "#/worklet/ENG-12" is the item
// screen with id ENG-12. False for an address no screen of the table resolves,
// and for one that only a screen naming no record does: what reads an address
// back asks about the record it names.
//
// THE SAME TABLE AND THE SAME GRAMMARS [Link] writes with, so an address read
// back is one this table would build, and a value outside a placeholder's
// grammar (a project key where a task's goes) names no screen.
func ScreenOf(address string) (string, map[string]string, bool) {
	path, rawQuery, _ := strings.Cut(address, "?")
	segments := strings.Split(path, "/")
	for _, name := range Screens() {
		template := strings.Split(links[name].address, "/")
		if len(template) != len(segments) {
			continue
		}
		params, matched := queryParams(links[name].query, rawQuery)
		if !matched {
			continue
		}
		for i, part := range template {
			p, placeholder := placeholders[part]
			if !placeholder {
				if part != segments[i] {
					matched = false
					break
				}
				continue
			}
			value, err := url.PathUnescape(segments[i])
			grammar := p.value()
			if part == "{id}" && links[name].id != nil {
				grammar = links[name].id()
			}
			if err != nil || !grammar.MatchString(value) {
				matched = false
				break
			}
			params[p.param] = value
		}
		if matched && len(params) > 0 {
			return name, params, true
		}
	}
	return "", nil, false
}

// queryParams are the parameters template's pairs read from rawQuery, and
// whether it is the query the template writes: exactly its keys, each value
// the literal around a value of the placeholder's grammar, and none at all
// for a screen the path alone opens, so an address read back is one the
// table would build again, query included.
func queryParams(template, rawQuery string) (map[string]string, bool) {
	params := map[string]string{}
	pairs := queryPairs(template)
	if len(pairs) == 0 {
		return params, rawQuery == ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil || len(values) != len(pairs) {
		return nil, false
	}
	for _, pair := range pairs {
		v, ok := values[pair.key]
		if !ok || len(v) != 1 {
			return nil, false
		}
		inner, cut := strings.CutPrefix(v[0], pair.before)
		if !cut {
			return nil, false
		}
		inner, cut = strings.CutSuffix(inner, pair.after)
		p := placeholders[pair.placeholder]
		if !cut || !p.value().MatchString(inner) {
			return nil, false
		}
		params[p.param] = inner
	}
	return params, true
}

// checkOpen reports a spec's link naming a screen the table does not hold, or
// parameters that screen does not take.
func checkOpen(o *Open) error {
	if _, ok := links[o.Screen]; !ok {
		return fmt.Errorf("%w: the screen %q is not one of %s", ErrNoLink, o.Screen,
			words.Or(Screens()))
	}
	want := ScreenParams(o.Screen)
	got := slices.Sorted(maps.Keys(o.Params))
	slices.Sort(want)
	if !slices.Equal(want, got) {
		return fmt.Errorf("%w: %s takes %s, and the spec names %s", ErrNoLink, o.Screen,
			words.And(want), words.And(got))
	}
	return nil
}

// linkFrom is the link o reads from v, a row or a record of an answer.
func linkFrom(o *Open, v any) (wire.Link, error) {
	params := map[string]string{}
	for param, path := range o.Params {
		params[param] = text(lookup(v, path))
	}
	label := o.Label
	if label == "" && o.LabelFrom != "" {
		label = text(lookup(v, o.LabelFrom))
	}
	return Link(o.Screen, params, label)
}

// OpenFor is o's link with its parameters read from a read's own arguments,
// for a read with no answer to read them from (one past what a step reads):
// the screen that shows all of it. Nil where o is nil or the arguments do not
// fill its parameters.
func OpenFor(o *Open, args map[string]any) *wire.Link {
	if o == nil {
		return nil
	}
	link, err := linkFrom(o, args)
	if err != nil {
		return nil
	}
	return &link
}

// OpenInApp is the open in app card for link, decided for the person: a link
// they may open, or one they may not, never told apart from one that does not
// exist.
func OpenInApp(link wire.Link, permitted bool) wire.Card {
	c := &wire.OpenInAppCard{Link: link, Permitted: permitted}
	if !permitted {
		c.Reason = wire.OpenNotPermitted
	}
	return wire.Card{Type: wire.CardOpenInApp, OpenInApp: c}
}

// MayOpen reports whether the person may open screen as their grants stand,
// before any record is asked about: the screen's app, held through
// [iam.Expand] so an administrator opens every app's screens.
func MayOpen(name string, p iam.Principal) bool {
	s, ok := links[name]
	if !ok {
		return false
	}
	return s.app == "" || p.Can(s.app)
}

// Permitted reads an answer to the record's own read, sent as the person, as
// whether they may open it: an answer is permitted, and a refusal and an
// absence are both not, because the read does not tell the person which of the
// two it was.
func Permitted(status int) bool { return status >= 200 && status < 300 }

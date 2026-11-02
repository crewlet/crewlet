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
	app     iam.Grant
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
// `{unit}`, `{CONTAINER}`, `{turn_id}`), so one table fills them on both sides.
var links = map[string]screen{
	"home":             {address: "#/home", label: "Home"},
	"inbox":            {address: "#/inbox", app: iam.GrantWorklet, label: "Inbox"},
	"me":               {address: "#/me", app: iam.GrantWorklet, label: "My work"},
	"worklet-projects": {address: "#/worklet", app: iam.GrantWorklet, label: "Projects"},
	"worklet-tasks":    {address: "#/worklet/tasks", app: iam.GrantWorklet, label: "Tasks"},
	"worklet-search":   {address: "#/worklet/search", app: iam.GrantWorklet, label: "Search"},
	"project":          {address: "#/worklet/{KEY}", app: iam.GrantWorklet},
	"item":             {address: "#/worklet/{id}", app: iam.GrantWorklet, id: taskName},
	"org-chart":        {address: "#/company/org", label: "Org chart"},
	"principles":       {address: "#/company/principles", label: "Principles"},
	"roster":           {address: "#/company/org/roster", label: "Roster"},
	"teams":            {address: "#/company/org/teams/{unit}", noun: "team"},
	"seat":             {address: "#/company/org/seats/{handle}", noun: "seat"},
	"budgets":          {address: "#/company/budgets", label: "Budgets"},
	"schedules":        {address: "#/company/schedules", label: "Schedules"},
	"integrations":     {address: "#/company/integrations", app: iam.GrantAdmin, label: "Integrations"},
	"live":             {address: "#/live", label: "Now running"},
	"turns":            {address: "#/live/turns", label: "Turns"},
	"turn":             {address: "#/live/turns/{id}", noun: "turn"},
	"runs":             {address: "#/live/runs/{turn_id}", noun: "coding run"},
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
	"{unit}":      {"unit", lazyGrammar(`^[a-z0-9][a-z0-9_-]{0,63}$`)},
}

// lazyGrammar is pattern compiled at its first use, not at init: a bounded
// repeat compiles to a program holding every count, and the code host's push
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

// ScreenParams are the parameters screen's address takes, in order.
func ScreenParams(name string) []string {
	var out []string
	for _, segment := range strings.Split(links[name].address, "/") {
		if p, ok := placeholders[segment]; ok {
			out = append(out, p.param)
		}
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
	for param := range params {
		if !used[param] {
			return wire.Link{}, fmt.Errorf("%w: %s takes no %s", ErrNoLink, name, param)
		}
	}
	switch {
	case label != "":
	case s.noun != "":
		label = "Open " + s.noun
	case first != "":
		// A KEY A PERSON KNOWS ("FAIR-10", "ENG", "HOME") names its record.
		label = first
	default:
		label = s.label
	}
	return wire.Link{Label: label, Screen: name, Href: strings.Join(segments, "/")}, nil
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
	segments := strings.Split(address, "/")
	for _, name := range Screens() {
		template := strings.Split(links[name].address, "/")
		if len(template) != len(segments) {
			continue
		}
		params := map[string]string{}
		matched := true
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

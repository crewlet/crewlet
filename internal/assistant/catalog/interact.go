package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// PageAction is a change a screen applies to itself when the person approves
// it: a filter, a range, a lens, or a draft in an editor the person then saves.
// Nothing is written to the company by the action itself (D11).
//
// THE ENGINE'S HALF OF ONE LIST. The screen that mounts an action declares it
// (the dashboard's ASSISTANT_PAGE_ACTIONS, held to this table by the client
// gate) and the page context names the ones mounted now; this table is what
// the engine accepts a proposal for.
type PageAction struct {
	ID string
	// Scope is the proposal's scope line: what it changes, and that nothing
	// is saved.
	Scope string
	// Confirm is approve for an action into a draft, and none for a pure
	// view change, applied at once with an Undo.
	Confirm wire.ActionConfirm
}

// view and draft are the two kinds of page action.
func view(id, scope string) PageAction {
	return PageAction{ID: id, Confirm: wire.ConfirmNone, Scope: scope + " Nothing is saved."}
}

func draft(id, scope string) PageAction {
	return PageAction{ID: id, Confirm: wire.ConfirmApprove,
		Scope: scope + " Nothing is saved until you save it."}
}

// PageActions are every page action a proposal may name, in v1.
var PageActions = []PageAction{
	view("home.range.set", "Changes the window Home shows."),
	view("home.feed.set", "Changes which activity Home's feed shows."),
	view("work.filter.apply", "Changes the filters on this list."),
	view("work.filters.clear", "Clears the filters on this list."),
	view("work.arrange.set", "Changes how this list is grouped and sorted."),
	view("project.lens.set", "Switches this project's lens."),
	view("item.activity.set", "Changes which activity this task shows."),
	view("spend.range.set", "Changes the window this spend covers."),
	view("spend.group.set", "Changes how this spend is grouped."),
	view("live.filter.apply", "Changes what Now running shows."),
	view("turns.filter.apply", "Changes the filters on this list of turns."),
	view("knowledge.search.set", "Searches the knowledge base."),
	// THE ONE ACTION INTO AN OPEN DRAFT: every screen with a draft declares
	// its fields in the page context (`draft`), and this sets the editable
	// ones by path through the screen's own setters. A structural change
	// the fields cannot express keeps an action of its own (the org
	// builder's operations; the ceiling editor, which opens a draft).
	draft(DraftSet, "Changes the draft open on this screen."),
	draft("org.draft.apply_ops", "Changes the org builder's draft."),
	draft("budgets.ceiling.prefill", "Opens the ceiling editor with a new value filled in."),
}

// DraftSet is the page action that sets fields of the draft a screen has
// open: `{"values": {"<path>": "<value>"}}`, each path one the page context's
// draft lists as editable, a list's value an array of strings or one item per
// line, and a file's the file's id, which the engine resolves into the whole
// reference ([SetDraftFile]).
const DraftSet = "draft.set"

// PageActionOf is the page action named id.
func PageActionOf(id string) (PageAction, bool) {
	i := slices.IndexFunc(PageActions, func(a PageAction) bool { return a.ID == id })
	if i < 0 {
		return PageAction{}, false
	}
	return PageActions[i], true
}

// ErrNotAccepted is a page action the turn's screen did not mount.
var ErrNotAccepted = errors.New("catalog: the screen does not accept that page action")

// PageActionFor is the page action a propose_page_change call names, held to
// the turn's page context BEFORE anything is previewed or recorded: one this
// build knows, one the screen mounted now (its `accepts`), and arguments the
// screen takes (its `action_args`), so a proposal the screen could only
// refuse is the model's refused step and never a card in front of the person.
// A turn whose page chip was removed accepts none.
//
// Every action's arguments are the schema the screen declares, and an action
// it declares none for is refused rather than taken on trust; draft.set alone
// may go without, because the open draft's editable fields already declare
// what it takes, and [DraftValues] holds a call to them either way.
func PageActionFor(args Args, page *wire.PageContext) (PageAction, error) {
	id := args.String("action")
	action, known := PageActionOf(id)
	var accepts []string
	if page != nil {
		accepts = page.Accepts
	}
	switch {
	case !known:
		return PageAction{}, fmt.Errorf("%w: %q is not a page action this build knows",
			ErrInvalidArguments, id)
	case !slices.Contains(accepts, id):
		return PageAction{}, fmt.Errorf("%w: %q. A page action changes only the screen the "+
			"person has open. Make a saved change with its write tool or propose_api_change "+
			"from here, or offer the screen that edits it with open_in_app and say what to "+
			"do there", ErrNotAccepted, id)
	}
	declared := page.ActionArgs[id]
	switch {
	case declared == nil && id == DraftSet:
		return action, nil
	case declared == nil:
		return PageAction{}, fmt.Errorf("%w: the screen does not say what %q takes, so it "+
			"cannot be proposed", ErrInvalidArguments, id)
	}
	if err := declared.Check(args["args"]); err != nil {
		return PageAction{}, fmt.Errorf("%w: %s takes what the page context's action_args "+
			"declares for it: %w", ErrInvalidArguments, id, err)
	}
	return action, nil
}

// PageActionPrefix opens a page action's request path. A page action sends
// nothing to the engine; its request is the action as the digest binds it
// (`PAGE /page-actions/<id>` with the payload as its body), which the page
// applies once the person approves.
const PageActionPrefix = "/page-actions/"

// PageMethod is a page action's request method, one no route serves.
const PageMethod = "PAGE"

// payloadBytes bounds a page action's payload: a page context's own bound,
// since an action names what the screen shows and never a record's content.
const payloadBytes = wire.PageContextMaxBytes

func interactionTools() []Tool {
	pageAction := Tool{
		Name: "propose_page_change", Kind: KindPageAction, Tier: TierPageLocal,
		Phrase: "change this screen",
		Description: "Propose a change to what the person's current screen shows: a filter, " +
			"a range, a grouping, a tab, or the draft open in an editor. Only an action the page " +
			"context lists in accepts is valid, and its args are exactly what the page " +
			"context's action_args declares for it: its members, its values, and its examples " +
			"to copy the shape from. Never guess a key. For the open draft, use draft.set with args " +
			"{\"values\": {\"<path>\": \"<value>\"}}, each path an editable field of the page " +
			"context's draft and each value in its hint's form. A file field takes the file id " +
			"of a file delivered or attached in this conversation, as a string; the engine " +
			"fills in the rest of the reference. Nothing is saved: the person approves, and " +
			"saves the draft themselves.",
		// THE PAYLOAD'S SHAPE IS THE SCREEN'S, declared in the page
		// context's `action_args` and held there ([PageActionFor]) before
		// this schema's own check, which only bounds it to one object
		// naming nobody.
		schema: object([]string{"action"}, map[string]*schema{
			"action": text("One of the page context's accepts.", 64),
			"args":   {kind: "object", description: "The action's arguments.", freeform: true},
		}),
		Build: func(a map[string]any) (exec.Request, error) {
			id := Args(a).String("action")
			if _, ok := PageActionOf(id); !ok || !wire.ValidActionID(id) {
				return exec.Request{}, fmt.Errorf("%q is not a page action this build knows", id)
			}
			body, err := json.Marshal(a["args"])
			if err != nil || len(body) > payloadBytes {
				return exec.Request{}, fmt.Errorf("the action's arguments are not a payload of at "+
					"most %d bytes", payloadBytes)
			}
			return exec.Request{Method: PageMethod, Path: PageActionPrefix + id, Body: body}, nil
		},
	}

	params := map[string]*schema{
		"screen": oneOf("The screen to open.", cards.Screens()...),
		"label":  text("The link's text, a few words.", 80),
	}
	for _, name := range []string{"key", "id"} {
		params[name] = text("The screen's "+name+", where it takes one.", nameBytes)
	}
	params["handle"] = text("The seat's handle or name, for a seat's screen.", nameBytes)
	params["unit"] = text("The unit's key or name, for a unit's screen.", nameBytes)
	open := Tool{
		Name: "open_in_app", Kind: KindNavigate, Tier: TierRead, Phrase: "open this in the app",
		Description: "Offer the person a link to a screen or a record, which the engine builds " +
			"and checks they may open. Never write a link yourself.",
		schema: object([]string{"screen"}, params),
		Names:  map[string]Directory{"handle": DirSeat, "unit": DirUnit},
		Routes: ProbeRoutes(),
		Build: func(a map[string]any) (exec.Request, error) {
			_, probe, err := Open(Args(a))
			if err != nil {
				return exec.Request{}, err
			}
			if probe == nil {
				return exec.Request{}, ErrNoProbe
			}
			return *probe, nil
		},
	}
	return []Tool{pageAction, open}
}

// probes are the record reads open_in_app sends to decide whether the person
// may open a record's screen: the screen's own read, sent as them.
//
// Each names its route, the route's one wildcard, and the link parameter that
// fills it.
var probes = map[string]struct {
	route, wildcard, param string
}{
	"item":     {"GET /worklet/{id}", "id", "id"},
	"project":  {"GET /worklet/projects/{key}", "key", "key"},
	"page":     {"GET /doclet/pages/{id}", "id", "id"},
	"artifact": {"GET /artifacts/{id}", "id", "id"},
	"chatlet":  {"GET /chatlet/channels/{channel_id}", "channel_id", "id"},
	"seat":     {"GET /agents/{id}", "id", "handle"},
}

// ProbeRoutes are every route open_in_app may send.
func ProbeRoutes() []string {
	var out []string
	for _, p := range probes {
		if !slices.Contains(out, p.route) {
			out = append(out, p.route)
		}
	}
	slices.Sort(out)
	return out
}

// ErrNoProbe is a screen whose permission is decided by the person's grants
// alone ([cards.MayOpen]), with no record to ask about.
var ErrNoProbe = fmt.Errorf("catalog: the screen names no record to ask about")

// Open is open_in_app's link and, for a screen that names a record, the read
// that decides whether the person may open it. A screen with no record is
// decided by [cards.MayOpen] on the person's grants.
func Open(a Args) (wire.Link, *exec.Request, error) {
	screen := a.String("screen")
	params := map[string]string{}
	for _, name := range cards.ScreenParams(screen) {
		params[name] = a.String(name)
	}
	link, err := cards.Link(screen, params, a.String("label"))
	if err != nil {
		return wire.Link{}, nil, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	p, ok := probes[screen]
	if !ok {
		return link, nil, nil
	}
	req, err := get(p.route, map[string]string{p.wildcard: params[p.param]}, nil)
	if err != nil {
		return wire.Link{}, nil, err
	}
	return link, &req, nil
}

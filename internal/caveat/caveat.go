// Package caveat is what a write that LANDED tells its caller beside the
// outcome: something it did other than the obvious, or something the caller
// should know and was not refused for. A task filed to triage because the
// project's default assignee left the chart, a timestamp stored as the day it
// falls on, a comment that wakes somebody without asking them anything.
//
// # Two readers, two phrasings, one value (ADR-0060)
//
// The builtin tools are shared: a seat's model calls them over its MCP
// session, and a person calls the very same tools through the dashboard
// (`POST /operator/act`). A model is helped by advice in its own vocabulary,
// naming the tool and the argument it would type next ("Use ask: ana"). A
// person is not: they see labels and controls, never argument names, and a
// sentence telling them to pass `ask:` is noise they cannot act on. So a
// caveat carries BOTH phrasings, written together in this package where one
// constructor per [Code] states them, and each surface reads its own:
// [Caveat.Agent] rides the tool's answer (`warnings`) to a model, and
// [Caveat.Person] is what the act route answers a person with.
//
// # Why both are written here, and not by each surface
//
// The obvious alternative is a code plus parameters, rendered by every surface
// in its own words. It puts every caveat's sentence in two languages (Go for
// the model, TypeScript for the dashboard) where nothing holds the two to the
// same set of codes, and a code the dashboard has not learned yet would render
// as nothing at all. Here a caveat is one value with every reader's words, a
// test walks every constructor and holds the person's words to what a person
// can see, and the dashboard renders text it never had to write.
//
// # Whether the person must act is the code's, never the call site's
//
// [Action] says whether a caveat asks the person to DO something
// ([ActionRequired]: a setting that no longer works, which stays broken until
// somebody changes it) or only to KNOW something ([ActionNone]: a value stored
// in its canonical form, a link list that was capped). The dashboard turns
// the first into a toast that stays until it is dismissed and the second into
// one that goes by itself, so the decision is a property of the code, stated
// once in [actions] and never guessed by a renderer from the sentence.
//
// # Constructed here, or rebuilt from a peer at one boundary
//
// A caveat's fields are unexported and it does not decode from JSON, so the
// only ways to make one are a constructor in this package's catalog and
// [RebuildFromPeer], which rebuilds what a peer's catalog wrote and which only
// the estate router calls (a gate in the suite holds both). That is what
// makes the walk in its suite complete: every caveat any surface can answer
// was written by a catalog constructor, this build's or a peer's, and this
// build's have their person phrasing checked.
package caveat

import (
	"encoding/json"
	"slices"
)

// Code names one kind of caveat, stably: the act route answers it, and a
// client may branch on it.
type Code string

const (
	// CodeUnaddressedComment is a comment that wakes the item's assignee
	// without asking them anything or naming them, so a reply is not owed.
	CodeUnaddressedComment Code = "unaddressed_comment"

	// CodeDefaultAssigneeGone is a create that named nobody, in a project
	// whose default assignee is no longer a seat on the chart: the item
	// was filed to triage instead.
	CodeDefaultAssigneeGone Code = "default_assignee_gone"

	// CodeBodyLinksCapped is a body naming more item keys than are linked.
	CodeBodyLinksCapped Code = "body_links_capped"

	// CodeDateTruncated is a timestamp written to a value that holds a day,
	// stored as the day it falls on in the company's calendar.
	CodeDateTruncated Code = "date_truncated"

	// CodeTagNearDuplicate is a tag declared within a typo of one the
	// project already keeps.
	CodeTagNearDuplicate Code = "tag_near_duplicate"

	// CodeFiledEarlierUnread is a create an earlier attempt of the same
	// operation already filed, whose key this node could not read back.
	CodeFiledEarlierUnread Code = "filed_earlier_unread"

	// CodeFiledEarlierRemoved is a create an earlier attempt of the same
	// operation already filed, and that has since been purged.
	CodeFiledEarlierRemoved Code = "filed_earlier_removed"

	// CodeLaneChangedNotPlaced is a card dropped on a board whose lane
	// change landed and whose place in the lane did not.
	CodeLaneChangedNotPlaced Code = "lane_changed_not_placed"

	// CodeDependenciesNotRecorded is a create or an update that landed and
	// whose dependency change did not.
	CodeDependenciesNotRecorded Code = "dependencies_not_recorded"

	// CodeMoveUnfinished is a move whose item landed in its new project and
	// whose walk over the items under it stopped part of the way.
	CodeMoveUnfinished Code = "move_unfinished"

	// CodeMoveWaiting is [CodeMoveUnfinished] for a walk that waits on an
	// item in the trash under it, which somebody has to restore or delete.
	CodeMoveWaiting Code = "move_waiting"

	// CodeSubtreeUnfinished is a removal or a restore whose item landed and
	// whose walk over the items that go with it stopped part of the way.
	CodeSubtreeUnfinished Code = "subtree_unfinished"

	// CodeDatastorePending is an artifact datastore write on the log whose
	// verdict this node has not applied yet.
	CodeDatastorePending Code = "datastore_pending"

	// CodeFileFinding is a finding about a file the caller authored and
	// published: legal, and almost certainly not what they meant (an
	// artifact page with no viewport, a resource the origin will not load).
	CodeFileFinding Code = "file_finding"
)

// Action is what a caveat asks of the person who reads it.
type Action string

const (
	// ActionNone is a caveat to know about: nothing is left broken, and
	// the person need do nothing. Shown and gone.
	ActionNone Action = "none"

	// ActionRequired is a caveat somebody has to act on: what it names
	// stays as it is until a person changes it. Shown until dismissed.
	ActionRequired Action = "required"
)

// Valid reports whether a is an action this build knows.
func (a Action) Valid() bool { return a == ActionNone || a == ActionRequired }

// Actions is every action, in the order the wire documents them.
func Actions() []Action { return []Action{ActionNone, ActionRequired} }

// actions is THE one statement of each code's action, and of which codes
// exist: [Code.Valid] and [Codes] read it.
var actions = map[Code]Action{
	// The commenter may well want no reply; it is advice, not a fault.
	CodeUnaddressedComment: ActionNone,
	// The project's setting names somebody who is gone, and every create
	// that names nobody lands in triage until somebody sets another.
	CodeDefaultAssigneeGone: ActionRequired,
	CodeBodyLinksCapped:     ActionNone,
	CodeDateTruncated:       ActionNone,
	// Usually deliberate (the writer just declared it); merging is
	// available when it was not.
	CodeTagNearDuplicate:    ActionNone,
	CodeFiledEarlierUnread:  ActionNone,
	CodeFiledEarlierRemoved: ActionNone,
	// The item stands without what it should wait on until somebody sets
	// it again.
	CodeDependenciesNotRecorded: ActionRequired,
	// The Worklet duty finishes the walk on its own.
	CodeMoveUnfinished: ActionNone,
	// The walk waits on an item in the trash until somebody restores or
	// deletes it.
	CodeMoveWaiting: ActionRequired,
	// The items that go with it are split until the gesture is made again.
	CodeSubtreeUnfinished: ActionRequired,
	// Its verdict is not known and can still be a refusal, which nothing
	// shows a person who asked through the act route (an assistant's
	// approved write): they look at the artifact to see it.
	CodeDatastorePending: ActionRequired,
	// The card is in the lane it was dropped into; only its order there
	// differs, which the next drag sets.
	CodeLaneChangedNotPlaced: ActionNone,
	// The published file is live as it is, and stays wrong in the way the
	// finding says until its author publishes a corrected version.
	CodeFileFinding: ActionRequired,
}

// Valid reports whether c is a code this build knows.
func (c Code) Valid() bool {
	_, ok := actions[c]
	return ok
}

// Action is what a caveat of code c asks of a person. An unknown code asks
// nothing: it is one a newer peer wrote, and a sticky toast this build cannot
// explain is worse than one that goes by itself.
func (c Code) Action() Action {
	if a, ok := actions[c]; ok {
		return a
	}
	return ActionNone
}

// Codes is every code this build knows, sorted.
func Codes() []Code {
	out := make([]Code, 0, len(actions))
	for c := range actions {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// Caveat is one caveat, in the words of each reader. The zero value is no
// caveat, and only this package's catalog (or the estate router, rebuilding a
// peer's) makes another.
type Caveat struct {
	code   Code
	agent  string
	person string
}

// Code is the caveat's kind.
func (c Caveat) Code() Code { return c.code }

// Agent is the caveat as a model reads it: it may name the tool and the
// argument that would change the outcome, because that is what a model can do
// next.
func (c Caveat) Agent() string { return c.agent }

// Person is the caveat as a person reads it on a screen: what happened and,
// where something is theirs to do, what, in the words of the controls they
// see. It names no tool, no argument and no internal id.
func (c Caveat) Person() string { return c.person }

// Action is what the caveat asks of the person who reads it ([Code.Action]).
func (c Caveat) Action() Action { return c.code.Action() }

// IsZero reports whether c is no caveat at all.
func (c Caveat) IsZero() bool { return c == Caveat{} }

// wire is how a caveat travels between nodes: an estate write routed to a
// data node answers its caveats to the node the caller is on. The action is
// not on it, because it is the code's and every node derives it.
type wire struct {
	Code   Code   `json:"code"`
	Agent  string `json:"agent"`
	Person string `json:"person"`
}

// MarshalJSON writes the caveat as it travels between nodes.
func (c Caveat) MarshalJSON() ([]byte, error) {
	return json.Marshal(wire{Code: c.code, Agent: c.agent, Person: c.person})
}

// NO UnmarshalJSON, deliberately: decoding JSON into a Caveat leaves it the
// zero value, which no surface answers. A caveat a peer wrote is rebuilt only
// by the estate router, through [RebuildFromPeer] (peer.go), so text from
// anywhere else (a tool's output, a request body) can never become words a
// person is shown.

// Agents is the model's words for each caveat, in order: what a tool's answer
// carries as `warnings`. Nil for none.
func Agents(cs []Caveat) []string {
	var out []string
	for _, c := range cs {
		if c.IsZero() {
			continue
		}
		out = append(out, c.agent)
	}
	return out
}

// Shown is one caveat as a person's surface answers it: the person's words and
// what they ask of the reader. The act route's `caveats`.
type Shown struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Action  Action `json:"action"`
}

// ForPeople is every caveat as a person's surface answers it, in order. Never
// nil, so the answer's `caveats` is always a list.
func ForPeople(cs []Caveat) []Shown {
	out := make([]Shown, 0, len(cs))
	for _, c := range cs {
		if c.IsZero() {
			continue
		}
		out = append(out, Shown{Code: c.code, Message: c.person, Action: c.Action()})
	}
	return out
}

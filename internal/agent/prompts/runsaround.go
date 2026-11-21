package prompts

import (
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/toolname"
)

// RunAround is one run in flight around an executor's run, as `list_runs`
// answers it and a launch's brief lists it ([RunsAround], ADR-0176). Whom it
// works for decides what is shown: a run for the same requester shows its
// title; another requester's shows its stage and item alone, since its task's
// words carry its requester's data (ADR-0109).
type RunAround struct {
	// Turn is its turn id, which `message_run` and `wait_for_run` name.
	Turn string

	// Seat is its seat's handle, and OwnSeat whether that is the caller's.
	Seat    string
	OwnSeat bool

	// Stage is where it stands (internal/sandbox's RunStage), and Started
	// when its run was first launched.
	Stage   string
	Started time.Time

	// Item is the work item it is charged to, "" for none.
	Item string

	// Relation is how a run of another seat bears on the caller's work
	// ([RunRelation]); "" for a run of the caller's own seat.
	Relation RunRelation

	// SameRequester says it works for whom the caller works for. Only then
	// are its conversation, branch and title shown.
	SameRequester bool

	// Conversation is the kind of its conversation (a chat surface, a
	// tracker, a schedule), never its key, and SameConversation whether it
	// is the caller's own.
	Conversation     string
	SameConversation bool

	// Branch is the branch it declared it pushes to, "" for none.
	Branch string

	// Title is the first line of what its turn was asked, in the words of
	// whom it works for, read under Label; "" for another requester's run.
	Title string `untrusted:"fence"`
	Label provenance.Label

	// TakesMessages says its harness reads a message between its steps now,
	// so `message_run` reaches it.
	TakesMessages bool
}

// RunRelation is how another seat's run bears on a run's work: the same work
// item, the same conversation, or a work item the tracker links to the
// caller's.
type RunRelation string

const (
	// RelationSameItem is a run charged to the caller's work item.
	RelationSameItem RunRelation = "same_item"
	// RelationSameConversation is a run in the caller's conversation.
	RelationSameConversation RunRelation = "same_conversation"
	// RelationLinkedItem is a run on an item the tracker links to the caller's.
	RelationLinkedItem RunRelation = "linked_item"
)

// RunRelations is the closed set.
var RunRelations = []RunRelation{RelationSameItem, RelationSameConversation, RelationLinkedItem}

// Valid reports whether r is one of [RunRelations].
func (r RunRelation) Valid() bool { return slices.Contains(RunRelations, r) }

// KeyRunsAround keys the section [RunsAround] adds to a launch's fresh brief.
const KeyRunsAround = "runs_around"

// runsAroundHeader opens [RunsAround]'s section.
const runsAroundHeader = "## Other runs in flight around this one"

// RunsAround is the section of a launch's brief listing the runs in flight
// around it, one line each in `list_runs`'s form, under b, with one line on
// when to use the tools: "" for none, so a run alone is told nothing.
func RunsAround(runs []RunAround, b provenance.Boundary) string {
	if len(runs) == 0 {
		return ""
	}
	var w strings.Builder
	w.WriteString(runsAroundHeader + "\n")
	w.WriteString("Other runs are working while this one does: your seat's, and other seats' on the same work. " +
		"Before you start, check whether one works on the " +
		"same work item, branch or conversation as you: then call `" + toolname.ListRuns + "` to see where it " +
		"stands, `" + toolname.MessageRun + "` to tell it what you are doing or found (it reads your message " +
		"at its next step; only a run working for the same requester as you takes one), or `" +
		toolname.WaitForRun + "` to park until it finishes rather than duplicate or collide with its work. " +
		"A run on unrelated work is unrelated: leave it be.\n")
	for _, r := range runs {
		w.WriteString("\n- " + RunLine(r, b))
	}
	return w.String()
}

// RunLine is one run as [RunsAround] lists it, under b: its turn and seat,
// stage and item, and for a run working for the same requester its
// conversation, branch and title, the title inside its fence.
func RunLine(r RunAround, b provenance.Boundary) string {
	seat := "this seat"
	if !r.OwnSeat {
		seat = "seat " + wakeName(b, r.Seat, "unnamed")
	}
	parts := []string{"turn `" + wakeName(b, r.Turn, "unnamed") + "` (" + seat + ")"}
	if !r.SameRequester {
		parts = append(parts, "a run for another requester")
	}
	parts = append(parts, strings.ReplaceAll(r.Stage, "_", " "))
	if said, ok := relationSaid[r.Relation]; ok {
		parts = append(parts, said)
	}
	if r.Item != "" {
		parts = append(parts, "on work item "+wakeName(b, r.Item, "unnamed"))
	}
	if !r.SameRequester {
		return strings.Join(parts, ", ")
	}
	if !r.Started.IsZero() {
		parts = append(parts, "started "+r.Started.UTC().Format(time.RFC3339))
	}
	switch {
	case r.SameConversation:
		parts = append(parts, "in this conversation")
	case r.Conversation != "":
		parts = append(parts, "in a "+wakeName(b, r.Conversation, "")+" conversation")
	}
	if r.Branch != "" {
		parts = append(parts, "branch `"+wakeName(b, r.Branch, "")+"`")
	}
	if !r.TakesMessages {
		parts = append(parts, "takes no message now")
	}
	line := strings.Join(parts, ", ")
	if r.Title != "" {
		line += ":\n" + r.Label.Fence(b, r.Title)
	}
	return line
}

// relationSaid is each relation as a run's line says it.
var relationSaid = map[RunRelation]string{
	RelationSameItem:         "on the same work item as yours",
	RelationSameConversation: "in the same conversation as yours",
	RelationLinkedItem:       "on a work item linked to yours",
}

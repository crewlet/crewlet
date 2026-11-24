package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE PREVIEWS: what each write would change, read by the server from the
// object as it is now, through the turn's grant, and never from what the model
// said about it (ADR-0051). Each answers the exact request its tool builds, so
// the card the person approves and the request the approval sends are one
// value.

// ErrStale is a write prepared against a version of its object the object is
// no longer at: the model read it, somebody changed it, and the change must be
// prepared again from what is there now.
var ErrStale = errors.New("catalog: the object changed since it was read")

// ErrPreviewRead is a preview whose read of the object was refused or failed;
// the refusal is the route's own.
var ErrPreviewRead = errors.New("catalog: the preview could not read the object")

// PreviewRefusal is a preview whose change the route itself rules out: its read
// refused (a 403, a 404 on an object the change edits or deletes), in the
// route's own code and words. It is [ErrPreviewRead], and it is what the turn
// shows on the step and hands the model as the step's refusal, so the model
// can read why and prepare a change that can apply. No proposal is drawn for
// it: a card the person approves only to watch fail is a change the server
// already knew it could not make.
type PreviewRefusal struct {
	// Status is the read's answer.
	Status int
	// Refusal is the route's own, as [exec.RefusalOf] reads it.
	Refusal wire.Refusal
}

func (e *PreviewRefusal) Error() string {
	return fmt.Sprintf("%v: %s (%s)", ErrPreviewRead, e.Refusal.ForModel(), e.Refusal.Code)
}

// Is makes a refusal [ErrPreviewRead].
func (e *PreviewRefusal) Is(target error) bool { return target == ErrPreviewRead }

// notCreatableCode is the API's own code for a create a route does not take,
// which a preview answers before the person is asked rather than after.
const notCreatableCode = "not_creatable"

// notCreatable is the refusal of a PUT to path, which holds nothing, on a
// route that does not create by address: in the route's own words about where
// one is added ([authz.Absent]), or, for a route that declares nothing, that
// it creates nothing a preview could propose.
func notCreatable(route Route, path string) *PreviewRefusal {
	hint := route.Pattern + " does not say it creates what it names, so nothing is " +
		"proposed. Add it through the route that creates it."
	if route.Absent != nil && route.Absent.Elsewhere != "" {
		hint = route.Absent.Elsewhere
	}
	return &PreviewRefusal{Status: http.StatusNotFound, Refusal: wire.Refusal{
		Code:    notCreatableCode,
		Message: "Nothing is at " + path + ", and " + route.Pattern + " does not add one by its address.",
		Hint:    hint,
	}}
}

// refusedRead is a read the route refused, as a [PreviewRefusal].
func refusedRead(resp exec.Response) error {
	return &PreviewRefusal{Status: resp.Status, Refusal: exec.RefusalOf(resp)}
}

// previews are each write tool's and the page action's preview, by tool.
var previews = map[string]func(t Tool) proposal.Previewer{
	"create_task":               createTaskPreview,
	"update_task":               updateTaskPreview,
	"comment_on_task":           commentPreview,
	"answer_decision":           answerPreview,
	"set_my_priorities":         prioritiesPreview,
	markInboxTool:               inboxPreview,
	"pause_agent":               seatPreview(true),
	"resume_agent":              seatPreview(false),
	steerTurnTool:               steerPreview,
	AnswerExecutorTool:          runAnswerPreview,
	"create_page":               createPagePreview,
	"edit_page":                 editPagePreview,
	shareArtifactTool:           sharePreview(true),
	"update_company_principles": principlesPreview,
	unshareArtifactTool:         sharePreview(false),
	"propose_page_change":       pageActionPreview,
	ReadScreenTool:              readScreenPreview,
	"post_chat_message":         chatPostPreview,
	"reply_in_chat_thread":      chatReplyPreview,
	"send_direct_message":       chatDirectPreview,
	"edit_chat_message":         chatEditPreview,
	"delete_chat_message":       chatDeletePreview,
	"update_chat_room":          chatRoomPreview,
	"set_chat_room_members":     chatMembersPreview,
	"leave_chat_room":           chatLeavePreview,
	addAtlasEntryTool:           addAtlasPreview,
	correctAtlasFactTool:        correctAtlasPreview,
	confirmAtlasEntryTool:       confirmAtlasPreview,
	relateAtlasEntriesTool:      relateAtlasPreview(false),
	unrelateAtlasEntriesTool:    relateAtlasPreview(true),
	setAtlasOwnerTool:           ownerAtlasPreview,
	retireAtlasEntryTool:        retireAtlasPreview(true),
	restoreAtlasEntryTool:       retireAtlasPreview(false),
	mergeAtlasEntriesTool:       pairAtlasPreview(true),
	splitAtlasEntriesTool:       pairAtlasPreview(false),
	purgeAtlasEntryTool:         purgeAtlasPreview,
}

// withHome is args with arg named: as the call names it, else the person's own
// home the write would default to, read from `GET /viewer` (field), which
// answers it through the one derivation the write applies
// (internal/engine.ProjectOfSeat, SpaceOfSeat) (ADR-0055).
//
// THE RESOLVED VALUE IS PINNED INTO THE REQUEST, so the card names the place
// the approved write lands, never a placeholder, and a chart that moves before
// the approval does not move the write. A person with no home is refused here,
// in the write's own words (refusal), rather than shown a card the write would
// refuse once approved.
func withHome(ctx context.Context, read proposal.Read, args map[string]any, arg, field,
	refusal string) (map[string]any, error) {

	if strings.TrimSpace(Args(args).String(arg)) != "" {
		return args, nil
	}
	viewer, err := readGet(ctx, read, "GET /viewer", nil, nil)
	if err != nil {
		return nil, err
	}
	home := strings.TrimSpace(value(viewer, field))
	if home == "" {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArguments, refusal)
	}
	out := maps.Clone(args)
	out[arg] = home
	return out, nil
}

// readGet builds a read on route and sends it through [readOne].
func readGet(ctx context.Context, read proposal.Read, route string, path map[string]string,
	q url.Values) (any, error) {
	req, err := get(route, path, q)
	if err != nil {
		return nil, err
	}
	return readOne(ctx, read, req)
}

// readOne sends one read through the grant and decodes its answer, a refusal
// being [ErrPreviewRead] with the route's own words.
func readOne(ctx context.Context, read proposal.Read, req exec.Request) (any, error) {
	resp, err := read(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPreviewRead, err)
	}
	if !cards.Permitted(resp.Status) {
		return nil, refusedRead(resp)
	}
	if resp.Truncated {
		return nil, fmt.Errorf("%w: the answer is larger than a change's preview reads", ErrPreviewRead)
	}
	return cards.Decode(resp.Body)
}

// value is a field of a decoded answer as text.
func value(doc any, path string) string { return cards.Text(cards.Lookup(doc, path)) }

// changes collects a preview's rows, keeping only the ones that change
// something, in D10's order.
type changes []wire.Change

func (c *changes) fact(field, label, v string) { c.factOf(field, label, wire.Plain(v)) }

// factOf is a fact whose value names records.
func (c *changes) factOf(field, label string, v wire.Phrase) {
	if len(v) > 0 {
		*c = append(*c, proposal.Fact(field, label, v))
	}
}

func (c *changes) move(field, label, from, to string) {
	if row, moved := proposal.Value(field, label, from, to); moved {
		*c = append(*c, row)
	}
}

// moveOf is a value that names records moving from from to to.
func (c *changes) moveOf(field, label string, from, to wire.Phrase) {
	if row, moved := proposal.ValueOf(field, label, from, to); moved {
		*c = append(*c, row)
	}
}

func (c *changes) add(field, label, to string) { c.addOf(field, label, wire.Plain(to)) }

// addOf is a value that names records, set where there was none.
func (c *changes) addOf(field, label string, to wire.Phrase) {
	if len(to) > 0 {
		*c = append(*c, proposal.Added(field, label, to, false))
	}
}

func (c changes) rows() []wire.Change { return proposal.Rows(c) }

// built is the preview's request, the one the tool builds for args.
func built(t Tool, args map[string]any) (exec.Request, error) { return t.Request(args) }

func createTaskPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, now time.Time, args map[string]any) (proposal.Preview, error) {
		chart, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		args, due, err := chart.resolveDue(args, now)
		if err != nil {
			return proposal.Preview{}, err
		}
		if args, err = withHome(ctx, read, args, "project", "project",
			org.NoHomeProject(t.Act)); err != nil {
			return proposal.Preview{}, err
		}
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		var c changes
		c.factOf("project", "Project", naming(wire.ResourceProject, a.String("project"),
			a.String("project")))
		c.add("title", "Title", a.String("title"))
		c.addOf("assignee", "Assignee", chart.seat(a.String("assignee")))
		c.addOf("ask", "Asks", chart.seat(a.String("ask")))
		c.addOf("parent", "Parent", taskRef(a.String("parent")))
		for _, f := range []struct{ arg, label string }{{"type", "Type"},
			{"status", "Status"}, {"priority", "Priority"}} {
			c.add(f.arg, f.label, a.String(f.arg))
		}
		c.add("due", "Due", due)
		c.add("labels", "Tags", strings.Join(a.Strings("labels"), ", "))
		c.move("body", "Description", "", a.String("body"))
		var note wire.Phrase
		switch {
		case a.String("ask") != "":
			note = chart.seat(a.String("ask")).Plain(" is asked the question and owes an answer.")
		case a.String("assignee") != "":
			note = chart.handover(ctx, read, a.String("assignee"), "the new task")
		}
		return proposal.Preview{Title: "Create task", Target: a.String("title"), Note: note,
			ApproveLabel: "Approve and create", Changes: c.rows(), Request: req}, nil
	}
}

func updateTaskPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, now time.Time, args map[string]any) (proposal.Preview, error) {
		chart, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		args, due, err := chart.resolveDue(args, now)
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /worklet/{id}",
			map[string]string{"id": a.String("item")}, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		key, version := value(doc, "task.key"), value(doc, "task.version")
		if want, _ := a.Int("if_match"); strconv.Itoa(want) != version {
			return proposal.Preview{}, fmt.Errorf("%w: %s is at version %s and the change "+
				"was prepared against version %d; read it again", ErrStale, key, version, want)
		}
		var c changes
		c.factOf("task", "Task", taskLine(key, value(doc, "task.title")))
		for _, f := range []struct{ arg, path, label string }{{"title", "task.title", "Title"},
			{"status", "task.status", "Status"}, {"priority", "task.priority", "Priority"},
			{"body", "task.body", "Description"}} {
			if _, given := args[f.arg]; given {
				c.move(f.arg, f.label, value(doc, f.path), a.String(f.arg))
			}
		}
		if _, given := args["assignee"]; given {
			c.moveOf("assignee", "Assignee", chart.seat(value(doc, "task.assignee")),
				chart.seat(a.String("assignee")))
		}
		if _, given := args["due"]; given {
			c.move("due", "Due", chart.dueOf(value(doc, "task.due_at"),
				value(doc, "task.due_all_day") == "true"), due)
		}
		if _, given := args["labels"]; given {
			c.move("labels", "Tags", value(doc, "task.tags"), strings.Join(a.Strings("labels"), ", "))
		}
		if len(c) < 2 {
			return proposal.Preview{}, fmt.Errorf("%w: the call changes nothing on %s",
				ErrInvalidArguments, key)
		}
		c.fact("reason", "Why", a.String("reason"))
		var note wire.Phrase
		if to, given := args["assignee"]; given && to != value(doc, "task.assignee") {
			note = wire.Plain("Nobody is assigned to it any more.")
			if a.String("assignee") != "" {
				note = chart.handover(ctx, read, a.String("assignee"), "the work")
			}
		}
		return proposal.Preview{Title: "Update task", Target: key, Note: note,
			ApproveLabel: "Approve and update", Changes: c.rows(), Request: req,
			SourceRevision: proposal.Revision{Kind: proposal.RevisionTask, ID: key,
				Version: version}.String()}, nil
	}
}

// company is what a task preview reads of the company through the grant: its
// clock, which a due date is resolved on, and its seats, which a handle is
// shown by. Read from the one chart every reader is shown (`GET /org`).
type company struct {
	zone  *time.Location
	seats map[string]seatShown
	// units are the chart's teams' names, by id.
	units map[string]string
}

// seatShown is how a preview names one seat.
type seatShown struct{ name, kind string }

// companyOf reads the company's clock and seats through the grant.
func companyOf(ctx context.Context, read proposal.Read) (company, error) {
	doc, err := readGet(ctx, read, "GET /org", nil, nil)
	if err != nil {
		return company{}, err
	}
	zone, err := period.LoadZone(orDefault(value(doc, "timezone"), "UTC"))
	if err != nil {
		return company{}, fmt.Errorf("%w: the company's clock: %w", ErrPreviewRead, err)
	}
	out := company{zone: zone, seats: map[string]seatShown{}, units: map[string]string{}}
	walkUnits(doc, func(node any) {
		if id := value(node, "id"); id != "" {
			out.units[id] = value(node, "name")
		}
		roles, _ := cards.Lookup(node, "roles").([]any)
		for _, r := range roles {
			if handle := value(r, "handle"); handle != "" {
				out.seats[handle] = seatShown{name: value(r, "name"), kind: value(r, "kind")}
			}
		}
	})
	return out, nil
}

// seatName is how a preview shows handle: the seat's name, with the handle
// beside it where the two differ, and the handle alone for one the chart does
// not hold.
func (c company) seatName(handle string) string {
	seat, held := c.seats[handle]
	switch {
	case handle == "":
		return ""
	case !held || seat.name == "":
		return handle
	case strings.EqualFold(seat.name, handle):
		return seat.name
	}
	return seat.name + " (" + handle + ")"
}

// entries are the chart's seats as a refusal lists them ([choices]), in handle
// order.
func (c company) entries() []Entry {
	out := make([]Entry, 0, len(c.seats))
	for _, handle := range slices.Sorted(maps.Keys(c.seats)) {
		out = append(out, Entry{Key: handle, Name: c.seats[handle].name})
	}
	return out
}

// handover is the note a task handed to handle carries: what happens to the
// work once the person approves, and nothing the engine cannot stand behind.
// A person is told about it. An agent is said to pick it up only when its
// live state, read through the grant as the seat's own screen reads it, says
// it takes work now; a paused seat, one out of budget, one no node runs or one
// whose model is down is said to wait, and one whose last turn failed is
// flagged. A seat whose state could not be read is only said to be assigned.
//
// THE PROMISE IS READ, NOT ASSUMED: "starts on the work once this is
// approved" was once printed for a seat that could not onboard at all.
func (c company) handover(ctx context.Context, read proposal.Read, handle, what string) wire.Phrase {
	name := c.seat(handle)
	seat, held := c.seats[handle]
	assigned := name.Plain(" is assigned " + what + " once this is approved.")
	switch {
	case !held:
		return assigned
	case seat.kind == "human":
		return name.Plain(" is notified of " + what + " once this is approved.")
	}
	doc, err := readGet(ctx, read, "GET /agents/{id}", map[string]string{"id": handle}, seatAlone)
	if err != nil {
		return assigned
	}
	switch value(doc, "live.activity") {
	case "stopped":
		if wait, known := stoppedWaits[value(doc, "live.stopped_reason")]; known {
			return assigned.Plain(" ").Then(name).Plain(wait)
		}
		return assigned.Plain(" ").Then(name).Plain(" cannot take work right now, so it " +
			"does not start until it can.")
	case "working", "needs", "idle":
		if value(doc, "live.last_turn.outcome") == "failed" {
			return assigned.Plain(" Its last turn failed, so check it before relying on it " +
				"to start.")
		}
		return assigned.Plain(" It picks the work up on its next turn.")
	}
	return assigned
}

// seatAlone asks a seat's read for its state without its call history, whose
// payloads are whole transcripts past what one read holds; a preview reads
// none of it.
var seatAlone = url.Values{"history": {"false"}}

// stoppedWaits are what a stopped seat waits for, by the reason its live state
// gives (internal/api/livestate's StoppedReason), each completing a sentence
// that starts with the seat's name.
var stoppedWaits = map[string]string{
	"paused": " is paused, so it does not start until somebody resumes it.",
	"budget": " has used its token budget, so it does not start until the budget " +
		"resets or is raised.",
	"unplaced": " is not running on any node right now, so it does not start until it is.",
	"provider": "'s model is unavailable right now, so it does not start until the model " +
		"is back.",
}

// resolveDue resolves a call's due date on the company's clock as of now,
// through the function the act tool resolves it with ([period.Resolve]), and
// answers the call with the date written out in place of a relative token, so
// the write carries the very date the card shows however long the approval
// waits; and that date as the card shows it. A call with no due date is
// answered as it is.
func (c company) resolveDue(args map[string]any, now time.Time) (map[string]any, string, error) {
	token := strings.TrimSpace(Args(args).String("due"))
	if token == "" {
		return args, "", nil
	}
	anchor, err := period.Resolve(token, now, c.zone)
	if err != nil {
		return nil, "", fmt.Errorf("%w: due: %w", ErrInvalidArguments, err)
	}
	written := anchor.At.UTC().Format(time.RFC3339)
	if anchor.AllDay {
		written = anchor.At.In(c.zone).Format(time.DateOnly)
	}
	out := maps.Clone(args)
	out["due"] = written
	return out, c.dueText(anchor.At, anchor.AllDay), nil
}

// valueShown is a page action's value as a person reads it: a seat's handle
// as the seat, and any other value as it is.
func (c company) valueShown(v string) wire.Phrase {
	if _, held := c.seats[v]; held {
		return c.seat(v)
	}
	return wire.Plain(v)
}

// dueOf shows a task's stored due date, an RFC 3339 instant, or "" for none.
func (c company) dueOf(stored string, allDay bool) string {
	at, err := time.Parse(time.RFC3339, stored)
	if err != nil {
		return stored
	}
	return c.dueText(at, allDay)
}

// dueText is a due date as a person reads it, on the company's clock.
func (c company) dueText(at time.Time, allDay bool) string {
	local := at.In(c.zone)
	if allDay {
		return local.Format("Monday 2 January 2006")
	}
	return local.Format("Monday 2 January 2006, 15:04 MST")
}

func commentPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /worklet/{id}",
			map[string]string{"id": a.String("item")}, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.factOf("task", "Task", taskLine(value(doc, "task.key"), value(doc, "task.title")))
		if id := a.String("reply_to"); id != "" {
			replied, err := repliedTo(ctx, read, a.String("item"), id)
			if err != nil {
				return proposal.Preview{}, err
			}
			c.factOf("reply_to", "Replying to", replied)
		}
		c.move("body", "Comment", "", a.String("body"))
		return proposal.Preview{Title: "Comment on task", Target: value(doc, "task.key"),
			Note:         wire.Plain("Everyone following the task is told about the comment."),
			ApproveLabel: "Approve and post", Changes: c.rows(), Request: req}, nil
	}
}

// repliedTo is the comment id on item as a reply's card names it: its author
// and the first words it says; its id where the thread no longer holds it.
func repliedTo(ctx context.Context, read proposal.Read, item, id string) (wire.Phrase, error) {
	var found any
	err := walkThread(ctx, read, item, url.Values{"thread": {id}, "with_root": {"true"}},
		func(_ question, comments []any) bool {
			for _, c := range comments {
				if value(c, "id") == id {
					found = c
					return true
				}
			}
			return false
		})
	if err != nil || found == nil {
		return wire.Plain(id), err
	}
	chart, err := companyOf(ctx, read)
	if err != nil {
		return nil, err
	}
	words := strings.Join(strings.Fields(value(found, "body")), " ")
	return chart.seat(value(found, "author")).
		Plain(": " + textcut.Within(words, cardLineBytes)), nil
}

// answerPreview previews an answer to an open question put to the person, bound to
// the question as shown (proposal.RevisionAsk) and sent to it by its ids.
func answerPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		if _, err := built(t, args); err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		q, err := questionAnswered(ctx, read, a.String("item"), a.String("answers"))
		if err != nil {
			return proposal.Preview{}, err
		}
		pinned := maps.Clone(args)
		pinned["item"], pinned["answers"] = q.task, q.id()
		req, err := built(t, pinned)
		if err != nil {
			return proposal.Preview{}, err
		}
		options := q.options()
		// An agent's grant request is answered in the engine's words (ADR-0118).
		if g, ok := grantChoiceOf(options, a.String("choice")); ok {
			if _, offered := options[a.String("choice")]; !offered {
				return proposal.Preview{}, fmt.Errorf("%w: the question offers no option %q",
					ErrInvalidArguments, a.String("choice"))
			}
			p, err := grantAnswerPreview(ctx, read, req,
				map[string]any{"key": q.key, "title": q.title}, a, g)
			p.SourceRevision = q.bound()
			return p, err
		}
		var c changes
		c.factOf("task", "Task", taskLine(q.key, q.title))
		c.fact("question", "Question", orDefault(value(q.comment, "decision.question"),
			value(q.comment, "body")))
		if choice := a.String("choice"); choice != "" {
			label, ok := options[choice]
			if !ok {
				return proposal.Preview{}, fmt.Errorf("%w: the question offers no option %q",
					ErrInvalidArguments, choice)
			}
			c.add("choice", "Answer", label)
		}
		c.move("body", "Comment", "", a.String("body"))
		return proposal.Preview{Title: "Answer question", Target: q.key,
			Note:         wire.Plain("Whoever asked is woken with the answer and goes on from it."),
			ApproveLabel: "Approve and answer", Changes: c.rows(),
			SourceRevision: q.bound(), Request: req}, nil
	}
}

// threadPages is the most thread pages a preview walks (ten of fifty comments);
// past it the question is named by its comment id and read narrowed.
const threadPages = 10

// question is a question asked on a task, as the task's thread answered it.
type question struct {
	// task, key and title are the task's id, key and title.
	task, key, title string
	// comment is the comment that asks it.
	comment any
}

// id is the question's comment id.
func (q question) id() string { return value(q.comment, "id") }

// options are the question's options, label by id.
func (q question) options() map[string]string {
	out := map[string]string{}
	list, _ := cards.Lookup(q.comment, "decision.options").([]any)
	for _, o := range list {
		out[value(o, "id")] = value(o, "label")
	}
	return out
}

// bound is a digest of the question as shown: whom it asks, its words, decision
// and whether it is open, which an answer, an edit or a removal moves.
func (q question) bound() string {
	shown := struct {
		Ask       string `json:"ask"`
		Body      string `json:"body"`
		Decision  any    `json:"decision"`
		Resolved  string `json:"resolved"`
		Removed   string `json:"removed"`
		UpdatedAt string `json:"updated_at"`
	}{Ask: value(q.comment, "ask"), Body: value(q.comment, "body"),
		Decision: cards.Lookup(q.comment, "decision"), Resolved: value(q.comment, "resolved"),
		Removed: value(q.comment, "removed"), UpdatedAt: value(q.comment, "updated_at")}
	// A decoded answer's values, which always encode, in one key order.
	encoded, _ := json.Marshal(shown)
	sum := sha256.Sum256(encoded)
	return proposal.Revision{Kind: proposal.RevisionAsk,
		ID:      url.PathEscape(q.task) + "/" + url.PathEscape(q.id()),
		Version: hex.EncodeToString(sum[:16])}.String()
}

// questionAnswered is the question named by id, or the one open question on item
// asked of the person; only an open question put to them is answered.
func questionAnswered(ctx context.Context, read proposal.Read, item, id string) (question, error) {
	// THE PERSON'S RECORD, the name a question put to them is asked of, as
	// the decisions read answers it for its caller (a page of one, since
	// only its name is wanted).
	mine, err := readGet(ctx, read, "GET /worklet/decisions", nil, url.Values{"limit": {"1"}})
	if err != nil {
		return question{}, err
	}
	person := value(mine, "handle")
	var q question
	if id != "" {
		q, err = readQuestion(ctx, read, item, id)
	} else {
		q, err = openQuestionOf(ctx, read, item, person)
	}
	if err != nil {
		return question{}, err
	}
	asked := value(q.comment, "ask")
	switch {
	case asked == "":
		return question{}, fmt.Errorf("%w: comment %s on %s asks no question; answers names "+
			"the comment that asks it", ErrInvalidArguments, q.id(), q.key)
	case asked != person:
		return question{}, fmt.Errorf("%w: the question %s on %s is asked of %s, not of the "+
			"person; Ask Crewlet answers only questions put to them", ErrInvalidArguments,
			q.id(), q.key, asked)
	case value(q.comment, "removed") == "true":
		return question{}, fmt.Errorf("%w: the question %s on %s was removed", ErrInvalidArguments,
			q.id(), q.key)
	case value(q.comment, "resolved") == "true":
		return question{}, fmt.Errorf("%w: the question %s on %s is already answered or closed",
			ErrInvalidArguments, q.id(), q.key)
	}
	return q, nil
}

// readQuestion reads the comment id on item from its own conversation (the
// thread narrowed to it, with it), walking back page by page to it: it is the
// conversation's first comment, so on its oldest page.
func readQuestion(ctx context.Context, read proposal.Read, item, id string) (question, error) {
	var found question
	err := walkThread(ctx, read, item, url.Values{"thread": {id}, "with_root": {"true"}},
		func(task question, comments []any) bool {
			for _, c := range comments {
				if value(c, "id") == id {
					task.comment = c
					found = task
					return true
				}
			}
			return false
		})
	switch {
	case err != nil:
		return question{}, err
	case found.comment == nil:
		return question{}, fmt.Errorf("%w: %s holds no comment %s; answers names the comment "+
			"that asks the question, as GET /worklet/decisions or the task's thread gives it",
			ErrInvalidArguments, item, id)
	}
	return found, nil
}

// openQuestionOf is the one open question on item asked of person, read from
// the task's whole thread: the question the act infers an answer naming none
// answers. None, or several, is refused naming what to do.
func openQuestionOf(ctx context.Context, read proposal.Read, item, person string) (question, error) {
	var open []question
	err := walkThread(ctx, read, item, nil, func(task question, comments []any) bool {
		for _, c := range comments {
			if value(c, "ask") == person && value(c, "resolved") != "true" &&
				value(c, "removed") != "true" {
				task.comment = c
				open = append(open, task)
			}
		}
		return false
	})
	if err != nil {
		return question{}, err
	}
	switch len(open) {
	case 0:
		return question{}, fmt.Errorf("%w: no open question on %s is asked of the person",
			ErrInvalidArguments, item)
	case 1:
		return open[0], nil
	}
	ids := make([]string, 0, len(open))
	for _, q := range open {
		ids = append(ids, q.id())
	}
	return question{}, fmt.Errorf("%w: %s holds %d open questions asked of the person (%s); "+
		"name the one this answers with answers", ErrInvalidArguments, open[0].key, len(open),
		strings.Join(ids, ", "))
}

// walkThread hands page each page of item's thread, newest first, until it answers
// true or the thread ends; past threadPages it is refused.
func walkThread(ctx context.Context, read proposal.Read, item string, narrow url.Values,
	page func(task question, comments []any) bool) error {
	cursor := ""
	for range threadPages {
		q := url.Values{"item": {item}, "limit": {"50"}}
		maps.Copy(q, narrow)
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		doc, err := readGet(ctx, read, "GET /worklet/comments", nil, q)
		if err != nil {
			return err
		}
		comments, _ := cards.Lookup(doc, "comments").([]any)
		task := question{task: value(doc, "item"), key: value(doc, "key"),
			title: value(doc, "title")}
		if page(task, comments) {
			return nil
		}
		if cursor = value(doc, "next_cursor"); cursor == "" {
			return nil
		}
	}
	return fmt.Errorf("%w: %s's thread is longer than a preview reads; name the question "+
		"with answers, its comment id as GET /worklet/decisions gives it", ErrInvalidArguments, item)
}

// askRevision re-reads the question r names, as an answer's card showed it,
// through the turn's grant: [proposal.ErrGone] where the task or the comment
// is no longer there to read.
func askRevision(ctx context.Context, read proposal.Read, r proposal.Revision) (string, error) {
	taskPart, commentPart, _ := strings.Cut(r.ID, "/")
	task, taskErr := url.PathUnescape(taskPart)
	comment, commentErr := url.PathUnescape(commentPart)
	if taskErr != nil || commentErr != nil || task == "" || comment == "" {
		return "", fmt.Errorf("%w: revision %s names no question", proposal.ErrInvalid,
			r.String())
	}
	q, err := readQuestion(ctx, read, task, comment)
	var refused *PreviewRefusal
	switch {
	case errors.As(err, &refused) && refused.Status == http.StatusNotFound,
		errors.Is(err, ErrInvalidArguments):
		return "", proposal.ErrGone
	case err != nil:
		return "", err
	}
	return q.bound(), nil
}

func prioritiesPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		handle := a.String("handle")
		if handle == "" {
			mine, mineErr := readGet(ctx, read, "GET /worklet/my-work", nil, nil)
			if mineErr != nil {
				return proposal.Preview{}, mineErr
			}
			handle = value(mine, "handle")
		}
		person, err := readGet(ctx, read, personRoute, map[string]string{"handle": handle}, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		chart, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.factOf("whose", "Whose list", chart.seat(handle))
		var held []wire.Phrase
		for _, key := range strings.Split(value(person, "priorities"), ", ") {
			if key != "" {
				held = append(held, taskRef(key))
			}
		}
		var next []wire.Phrase
		for _, key := range a.Strings("items") {
			next = append(next, taskRef(key))
		}
		if row, moved := proposal.ListOf("priorities", "Order", held, next); moved {
			c = append(c, row)
		}
		if len(c) < 2 {
			return proposal.Preview{}, fmt.Errorf("%w: the order is the one %s already has",
				ErrInvalidArguments, handle)
		}
		return proposal.Preview{Title: "Reorder priorities", Target: handle,
			ApproveLabel: "Approve and reorder", Changes: c.rows(), Request: req,
			SourceRevision: proposal.Revision{Kind: proposal.RevisionPerson, ID: handle,
				Version: value(person, "version")}.String()}, nil
	}
}

// personRoute is the read of a person's tracker record, which a priority
// change is bound to.
const personRoute = "GET /worklet/people/{handle}"

// inboxPreview names the notices a mark_inbox call marks, read through the
// grant from the person's own inbox: a card that said "Mark read: 12 notices"
// could carry a notice the person never meant to clear (a security notice a
// prompt in some task talked the model into marking) without showing it.
func inboxPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		marked, unmarked := a.Strings("read"), a.Strings("unread")
		if len(marked)+len(unmarked) == 0 {
			return proposal.Preview{}, fmt.Errorf("%w: the call marks nothing", ErrInvalidArguments)
		}
		names, err := noticeNames(ctx, read, slices.Concat(marked, unmarked))
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.notices("read", "Mark read", marked, names)
		c.notices("unread", "Mark unread", unmarked, names)
		return proposal.Preview{Title: "Mark inbox", Target: "Your inbox",
			ApproveLabel: "Approve and mark", Changes: c.rows(), Request: req}, nil
	}
}

const (
	// listNamed is how many members of one list a card names one by one
	// before it says how many more (the notices a mark_inbox card clears,
	// the people who read a chat room): enough to see what a short list
	// holds, short enough that a card stays readable.
	listNamed = 10
	// inboxPage and inboxPages bound the reads a mark_inbox preview makes
	// to find the notices it names: the inbox route's own largest page, at
	// most five times, which covers the two hundred ids a list may carry.
	inboxPage  = 200
	inboxPages = 5
	// cardLineBytes bounds one line a card quotes from what somebody wrote:
	// a notice's name, the chat message a reply answers.
	cardLineBytes = 160
)

// noticeNames reads the person's inbox through the grant until every id in
// ids is found or the inbox ends, answering each found notice's name by its
// record id.
func noticeNames(ctx context.Context, read proposal.Read, ids []string) (map[string]wire.Phrase, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := map[string]wire.Phrase{}
	cursor := ""
	for range inboxPages {
		q := url.Values{"limit": {strconv.Itoa(inboxPage)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		doc, err := readGet(ctx, read, "GET /worklet/inbox", nil, q)
		if err != nil {
			return nil, err
		}
		notices, _ := cards.Lookup(doc, "notices").([]any)
		for _, n := range notices {
			if id := value(n, "record_id"); want[id] {
				out[id] = noticeName(n)
			}
		}
		if cursor = value(doc, "next_cursor"); cursor == "" || len(out) == len(want) {
			break
		}
	}
	return out, nil
}

// noticeName is how a card names one notice: the task it is about, where it
// names one, and what it says, as the inbox shows it.
func noticeName(n any) wire.Phrase {
	key, task := value(n, "subject_key"), value(n, "task")
	if value(n, "subject_key_collision") == "true" {
		key = ""
	}
	about := wire.Plain(key)
	if task != "" {
		about = naming(wire.ResourceTask, task, key)
	}
	says := textcut.Within(orDefault(value(n, "excerpt"), value(n, "reason")), cardLineBytes)
	switch {
	case len(about) == 0:
		return wire.Plain(orDefault(says, "A notice"))
	case says == "":
		return about
	}
	return about.Plain(": " + says)
}

// notices adds one row per notice of ids, named where the inbox read found it
// and said to be missing where it did not, then how many more past
// [listNamed].
func (c *changes) notices(field, label string, ids []string, names map[string]wire.Phrase) {
	for i, id := range ids {
		if i == listNamed {
			c.fact(field+".more", label, fmt.Sprintf("And %d more", len(ids)-listNamed))
			return
		}
		name, found := names[id]
		if !found {
			name = wire.Plain("A notice not in your inbox (" + id + ")")
		}
		c.factOf(fmt.Sprintf("%s.%d", field, i+1), label, name)
	}
}

func seatPreview(pause bool) func(t Tool) proposal.Previewer {
	return func(t Tool) proposal.Previewer {
		return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
			req, err := built(t, args)
			if err != nil {
				return proposal.Preview{}, err
			}
			a := Args(args)
			doc, err := readGet(ctx, read, "GET /agents/{id}",
				map[string]string{"id": a.String("handle")}, seatAlone)
			if err != nil {
				return proposal.Preview{}, err
			}
			var c changes
			c.factOf("seat", "Seat", naming(wire.ResourceSeat, a.String("handle"),
				value(doc, "role")))
			now := orDefault(value(doc, "live.activity"), "unknown")
			title, label, to, note := "Resume agent", "Approve and resume", "resumed",
				"The seat takes its queued work up again."
			if pause {
				title, label, to = "Pause agent", "Approve and pause", "paused"
				note = "The seat takes no new work until somebody resumes it."
				if a.Bool("stop_running") {
					note = "The turn it is running now is stopped, and it takes no new work " +
						"until somebody resumes it."
				}
			}
			c = append(c, proposal.Changed("state", "State", wire.Plain(now), wire.Plain(to)))
			c.fact("reason", "Why", a.String("reason"))
			return proposal.Preview{Title: title, Target: a.String("handle"), Note: wire.Plain(note),
				ApproveLabel: label, Changes: c.rows(), Request: req}, nil
		}
	}
}

func steerPreview(t Tool) proposal.Previewer {
	return func(_ context.Context, _ proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		var c changes
		c.factOf("turn", "Turn", naming(wire.ResourceTurn, a.String("turn_id"), a.String("turn_id")))
		c.move("note", "Note", "", a.String("note"))
		return proposal.Preview{Title: "Steer turn", Target: a.String("turn_id"),
			Note: wire.Plain("The turn reads the note at its next step (a round of the engine's " +
				"own loop, or a step of its executor in its box) and keeps to it for the rest of " +
				"the turn; an executor waiting on a person's answer reads it once that is answered."),
			ApproveLabel: "Approve and send", Changes: c.rows(), Request: req}, nil
	}
}

func runAnswerPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /executors", nil, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		runs, _ := cards.Lookup(doc, "runs").([]any)
		var run any
		for _, r := range runs {
			if value(r, "turn_id") == a.String("turn_id") {
				run = r
			}
		}
		if run == nil {
			return proposal.Preview{}, fmt.Errorf("%w: no executor of turn %s is parked on a question",
				ErrInvalidArguments, a.String("turn_id"))
		}
		// THE QUESTION THE PERSON IS SHOWN IS THE ONE ANSWERED: an executor
		// that moved on to another since the proposal was drafted is refused here,
		// before anybody approves an answer to a question it no longer asks.
		if value(run, "launch_id") != a.String("launch_id") {
			return proposal.Preview{}, fmt.Errorf("%w: the executor of turn %s has moved on to "+
				"another question since; read it again and answer that one", ErrInvalidArguments,
				a.String("turn_id"))
		}
		chart, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		work := wire.Plain("no task")
		if key := value(run, "work_key"); key != "" {
			work = taskRef(key)
		}
		var c changes
		c.factOf("run", "Executor", chart.seat(value(run, "agent_handle")).Plain(" on ").Then(work))
		c.fact("question", "Question", value(run, "question"))
		if choice := a.String("choice"); choice != "" {
			label := ""
			options, _ := cards.Lookup(run, "decision.options").([]any)
			for _, option := range options {
				if value(option, "id") == choice {
					label = value(option, "label")
				}
			}
			if label == "" {
				return proposal.Preview{}, fmt.Errorf("%w: %q is not an option of the question "+
					"turn %s's executor asked", ErrInvalidArguments, choice, a.String("turn_id"))
			}
			c.move("choice", "Choice", "", label)
		}
		c.move("answer", "Answer", "", a.String("answer"))
		return proposal.Preview{Title: "Answer executor", Target: a.String("turn_id"),
			Note:         wire.Plain("The executor carries on with the answer."),
			ApproveLabel: "Approve and answer", Changes: c.rows(), Request: req}, nil
	}
}

func createPagePreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		args, err := withHome(ctx, read, args, "container", "space", org.NoHomeSpace(t.Act))
		if err != nil {
			return proposal.Preview{}, err
		}
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		var c changes
		c.factOf("container", "Space", naming(wire.ResourceContainer, a.String("container"),
			a.String("container")))
		c.add("title", "Title", a.String("title"))
		c.add("status", "Status", a.String("status"))
		if parent := a.String("parent"); parent != "" {
			doc, err := readGet(ctx, read, "GET /doclet/pages/{id}",
				map[string]string{"id": parent}, nil)
			if err != nil {
				return proposal.Preview{}, err
			}
			c.addOf("parent", "Under", naming(wire.ResourcePage, value(doc, "page.id"),
				value(doc, "page.title")))
		}
		c.add("labels", "Labels", strings.Join(a.Strings("labels"), ", "))
		c.move("body", "Page", "", a.String("body"))
		return proposal.Preview{Title: "Create page", Target: a.String("title"),
			ApproveLabel: "Approve and create", Changes: c.rows(), Request: req}, nil
	}
}

func editPagePreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /doclet/pages/{id}",
			map[string]string{"id": a.String("page")}, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		id, version := value(doc, "page.id"), value(doc, "page.version")
		if base, _ := a.Int("base_version"); strconv.Itoa(base) != version {
			return proposal.Preview{}, fmt.Errorf("%w: the page is at version %s and the edit "+
				"was prepared against version %d; read it again", ErrStale, version, base)
		}
		var c changes
		c.factOf("page", "Page", naming(wire.ResourcePage, id, value(doc, "page.title")))
		if _, given := args["title"]; given {
			c.move("title", "Title", value(doc, "page.title"), a.String("title"))
		}
		c.move("body", "Page", value(doc, "page.body"), a.String("body"))
		if len(c) < 2 {
			return proposal.Preview{}, fmt.Errorf("%w: the edit changes nothing", ErrInvalidArguments)
		}
		c.fact("message", "Why", a.String("message"))
		return proposal.Preview{Title: "Edit page", Target: value(doc, "page.title"),
			ApproveLabel: "Approve and save", Changes: c.rows(), Request: req,
			SourceRevision: proposal.Revision{Kind: proposal.RevisionPage, ID: id,
				Version: version}.String()}, nil
	}
}

func sharePreview(share bool) func(t Tool) proposal.Previewer {
	return func(t Tool) proposal.Previewer {
		return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
			req, err := built(t, args)
			if err != nil {
				return proposal.Preview{}, err
			}
			a := Args(args)
			doc, err := readGet(ctx, read, "GET /artifacts/{id}",
				map[string]string{"id": a.String("artifact")}, nil)
			if err != nil {
				return proposal.Preview{}, err
			}
			chart, err := companyOf(ctx, read)
			if err != nil {
				return proposal.Preview{}, err
			}
			var c changes
			c.factOf("artifact", "Artifact", artifactRef(doc))
			p := proposal.Preview{Target: value(doc, "title"), Request: req,
				SourceRevision: proposal.Revision{Kind: proposal.RevisionArtifact,
					ID: value(doc, "id"), Version: value(doc, "head_number")}.String()}
			if share {
				who := chart.audience(a.String("to"))
				c.addOf("audience", "Shared with", who.Plain(" as "+a.String("role")))
				p.Title, p.ApproveLabel = "Share artifact", "Approve and share"
				p.Audience, p.Note = []wire.Phrase{who}, who.Plain(" can open it.")
				p.StepUp = t.StepUp != nil && t.StepUp(a)
			} else {
				who := chart.audience(a.String("from"))
				c = append(c, proposal.Removed("audience", "Shared with", who))
				p.Title, p.ApproveLabel = "Withdraw share", "Approve and withdraw"
				p.Audience = []wire.Phrase{who}
				p.Note = who.Plain(" can no longer open it, unless another share reaches them.")
			}
			p.Changes = c.rows()
			return p, nil
		}
	}
}

// audience is a share's audience as the card names it: a seat or a team by
// its name on the chart, or the whole company.
func (c company) audience(s string) wire.Phrase {
	kind, id, _ := strings.Cut(s, ":")
	switch {
	case s == string(artifacts.AudienceCompany):
		return wire.Plain("The whole company")
	case kind == string(artifacts.AudienceUnit):
		return wire.Plain("The ").Then(c.unit(id)).Plain(" team and everybody under it")
	case kind == string(artifacts.AudienceSeat):
		return c.seat(id)
	}
	return c.seat(s)
}

// readScreenPreview is [ReadScreenTool]'s record: one fact, since it changes
// nothing and no card draws it.
func readScreenPreview(t Tool) proposal.Previewer {
	return func(_ context.Context, _ proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		action, _ := PageActionOf(ReadScreen)
		return proposal.Preview{Title: "Read this screen", Target: ReadScreen, ApproveLabel: "Read",
			Changes: []wire.Change{proposal.Fact("scope", "This screen", wire.Plain(action.Scope))},
			Request: req}, nil
	}
}

func pageActionPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		action, ok := PageActionOf(Args(args).String("action"))
		if !ok {
			return proposal.Preview{}, fmt.Errorf("%w: no such page action", ErrInvalidArguments)
		}
		chart, err := companyOf(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("scope", "This screen", action.Scope)
		payload, _ := args["args"].(map[string]any)
		if values, ok := payload["values"].(map[string]any); ok && action.ID == DraftSet {
			// A DRAFT'S FIELDS ONE ROW EACH, by the path the page context's
			// draft named; the screen draws its own rows over these, with
			// each field's label and its value now.
			payload = values
		}
		for _, k := range sortedKeys(payload) {
			// NEVER A RAW PATH: a dotted key reads as its words, and a
			// draft's own labels replace these where the turn's page
			// declared them (assistant's draftRows).
			row := proposal.Fact(k, cards.FieldLabel(k), chart.valueShown(draftValueShown(payload[k])))
			if items, ok := plainItems(payload[k]); ok {
				row.Items = make([]wire.Phrase, len(items))
				for i, item := range items {
					row.Items[i] = chart.valueShown(item)
				}
			}
			if len(row.To) > 0 {
				c = append(c, row)
			}
		}
		label := "Apply"
		if action.Confirm == wire.ConfirmApprove {
			label = "Apply to the draft"
		}
		// THE PAGE CONTEXT'S REVISION IS THE TURN'S, which this preview does
		// not see: the proposal takes its source revision from the turn.
		return proposal.Preview{Title: "Change this screen", Target: action.ID,
			ApproveLabel: label, Changes: c.rows(), Request: req}, nil
	}
}

// draftValueShown is a draft value as a card row reads it: a file reference
// by its file's name, which the engine resolved, and any other value as text.
func draftValueShown(v any) string {
	if ref, ok := v.(map[string]any); ok && ref["kind"] == string(wire.DraftFieldFile) {
		if name, ok := ref["name"].(string); ok && name != "" {
			return name
		}
	}
	return cards.Text(v)
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Revision re-reads, through the turn's grant, the revision a preview recorded
// for the object it read: a task's version, a page's, an artifact's head, a
// person's tracker record's, a chat room's settings and members, a chat
// message's position, a question on a task as its answer showed it and the
// principles' entity tag. An approval compares what
// it answers with the recorded revision, and an answer that differs is a stale
// proposal.
var Revision proposal.Revisions = func(ctx context.Context, read proposal.Read, source string) (string, error) {
	r, err := proposal.ParseRevision(source)
	if err != nil {
		return "", err
	}
	switch r.Kind {
	case proposal.RevisionCharter:
		// THE TAG IS A HEADER, not a field of the answer's body.
		return principlesRevision(ctx, read)
	case proposal.RevisionEntity, proposal.RevisionResource, proposal.RevisionCreate:
		// A GENERIC CHANGE'S RESOURCE, re-read at the path it was read at.
		return apiRevision(ctx, read, r)
	case proposal.RevisionRoom, proposal.RevisionMessage:
		// A CHAT ROOM OR MESSAGE, re-read through the route its preview read.
		return chatRevision(ctx, read, r)
	case proposal.RevisionAtlasEntry:
		// AN ATLAS ENTRY, re-read as the card showed it.
		return atlasEntryRevision(ctx, read, r)
	case proposal.RevisionAsk:
		// A QUESTION ON A TASK, re-read from its own conversation.
		return askRevision(ctx, read, r)
	}
	var req exec.Request
	path, versionAt, idAt := "", "", ""
	switch r.Kind {
	case proposal.RevisionTask:
		req, err = get("GET /worklet/{id}", map[string]string{"id": r.ID}, nil)
		path, idAt, versionAt = "task", "key", "version"
	case proposal.RevisionPage:
		req, err = get("GET /doclet/pages/{id}", map[string]string{"id": r.ID}, nil)
		path, idAt, versionAt = "page", "id", "version"
	case proposal.RevisionArtifact:
		req, err = get("GET /artifacts/{id}", map[string]string{"id": r.ID}, nil)
		idAt, versionAt = "id", "head_number"
	case proposal.RevisionPerson:
		req, err = get(personRoute, map[string]string{"handle": r.ID}, nil)
		idAt, versionAt = "handle", "version"
	}
	if err != nil {
		return "", err
	}
	doc, err := readOne(ctx, read, req)
	if err != nil {
		return "", err
	}
	record := cards.Lookup(doc, path)
	return proposal.Revision{Kind: r.Kind, ID: value(record, idAt),
		Version: value(record, versionAt)}.String(), nil
}

// sortedKeys are m's keys in order, so a preview's rows are the same for the
// same payload.
func sortedKeys(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

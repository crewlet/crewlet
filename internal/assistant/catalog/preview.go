package catalog

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/textcut"
)

// THE PREVIEWS: what each write would change, read by the server from the
// object as it is now, through the turn's grant, and never from what the model
// said about it (ADR-0050). Each answers the exact request its tool builds, so
// the card the person approves and the request the approval sends are one
// value.

// ErrStale is a write prepared against a version of its object the object is
// no longer at: the model read it, somebody changed it, and the change must be
// prepared again from what is there now.
var ErrStale = errors.New("catalog: the object changed since it was read")

// ErrPreviewRead is a preview whose read of the object was refused or failed;
// the refusal is the route's own.
var ErrPreviewRead = errors.New("catalog: the preview could not read the object")

// previews are each write tool's and the page action's preview, by tool.
var previews = map[string]func(t Tool) proposal.Previewer{
	"create_task":         createTaskPreview,
	"update_task":         updateTaskPreview,
	"comment_on_task":     commentPreview,
	"answer_decision":     answerPreview,
	"set_my_priorities":   prioritiesPreview,
	"mark_inbox":          inboxPreview,
	"pause_agent":         seatPreview(true),
	"resume_agent":        seatPreview(false),
	"steer_turn":          steerPreview,
	"answer_coding_run":   runAnswerPreview,
	"create_page":         createPagePreview,
	"edit_page":           editPagePreview,
	"share_artifact":      sharePreview(true),
	"unshare_artifact":    sharePreview(false),
	"propose_page_change": pageActionPreview,
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
		r := exec.RefusalOf(resp)
		return nil, fmt.Errorf("%w: %s (%s)", ErrPreviewRead, r.Message, r.Code)
	}
	if resp.Truncated {
		return nil, fmt.Errorf("%w: the answer is larger than one step reads", ErrPreviewRead)
	}
	return cards.Decode(resp.Body)
}

// value is a field of a decoded answer as text.
func value(doc any, path string) string { return cards.Text(cards.Lookup(doc, path)) }

// changes collects a preview's rows, keeping only the ones that change
// something, in D10's order.
type changes []wire.Change

func (c *changes) fact(field, label, v string) {
	if v != "" {
		*c = append(*c, proposal.Fact(field, label, v))
	}
}

func (c *changes) move(field, label, from, to string) {
	if row, moved := proposal.Value(field, label, from, to); moved {
		*c = append(*c, row)
	}
}

func (c *changes) add(field, label, to string) {
	if to != "" {
		*c = append(*c, proposal.Added(field, label, to, false))
	}
}

func (c changes) rows() []wire.Change { return proposal.Rows(c) }

// built is the preview's request, the one the tool builds for args.
func built(t Tool, args map[string]any) (exec.Request, error) { return t.Request(args) }

func createTaskPreview(t Tool) proposal.Previewer {
	return func(_ context.Context, _ proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		var c changes
		c.fact("project", "Project", orDefault(a.String("project"), "Your team's project"))
		for _, f := range []struct{ arg, label string }{{"title", "Title"},
			{"assignee", "Assignee"}, {"ask", "Asks"}, {"parent", "Parent"}, {"type", "Type"},
			{"status", "Status"}, {"priority", "Priority"}, {"due", "Due"}} {
			c.add(f.arg, f.label, a.String(f.arg))
		}
		c.add("labels", "Tags", strings.Join(a.Strings("labels"), ", "))
		c.move("body", "Description", "", a.String("body"))
		note := ""
		switch {
		case a.String("ask") != "":
			note = a.String("ask") + " is asked a question they owe an answer to."
		case a.String("assignee") != "":
			note = a.String("assignee") + " is woken with the new task."
		}
		return proposal.Preview{Title: "Create task", Target: a.String("title"), Note: note,
			ApproveLabel: "Approve and create", Changes: c.rows(), Request: req}, nil
	}
}

func updateTaskPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /work/{id}",
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
		c.fact("task", "Task", key+" "+value(doc, "task.title"))
		for _, f := range []struct{ arg, path, label string }{{"title", "task.title", "Title"},
			{"status", "task.status", "Status"}, {"priority", "task.priority", "Priority"},
			{"assignee", "task.assignee", "Assignee"}, {"due", "task.due_at", "Due"},
			{"body", "task.body", "Description"}} {
			if _, given := args[f.arg]; given {
				c.move(f.arg, f.label, value(doc, f.path), a.String(f.arg))
			}
		}
		if _, given := args["labels"]; given {
			c.move("labels", "Tags", value(doc, "task.tags"), strings.Join(a.Strings("labels"), ", "))
		}
		if len(c) < 2 {
			return proposal.Preview{}, fmt.Errorf("%w: the call changes nothing on %s",
				ErrInvalidArguments, key)
		}
		c.fact("reason", "Why", a.String("reason"))
		note := ""
		if to, given := args["assignee"]; given && to != value(doc, "task.assignee") {
			note = "Hands the work to " + orDefault(a.String("assignee"), "nobody") +
				", who is woken with it."
		}
		return proposal.Preview{Title: "Update task", Target: key, Note: note,
			ApproveLabel: "Approve and update", Changes: c.rows(), Request: req,
			SourceRevision: proposal.Revision{Kind: proposal.RevisionTask, ID: key,
				Version: version}.String()}, nil
	}
}

func commentPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /work/{id}",
			map[string]string{"id": a.String("item")}, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("task", "Task", value(doc, "task.key")+" "+value(doc, "task.title"))
		c.fact("reply_to", "Replying to", a.String("reply_to"))
		c.move("body", "Comment", "", a.String("body"))
		return proposal.Preview{Title: "Comment on task", Target: value(doc, "task.key"),
			Note:         "Everyone following the task is told about the comment.",
			ApproveLabel: "Approve and post", Changes: c.rows(), Request: req}, nil
	}
}

func answerPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /work/comments", nil,
			query(a).str("item", "item").fixed("limit", "50").values())
		if err != nil {
			return proposal.Preview{}, err
		}
		question, options := askOf(doc, a.String("answers"))
		if question == nil {
			return proposal.Preview{}, fmt.Errorf("%w: %s holds no open question to answer; name "+
				"it with answers", ErrInvalidArguments, value(doc, "key"))
		}
		var c changes
		c.fact("task", "Task", value(doc, "key")+" "+value(doc, "title"))
		c.fact("question", "Question", orDefault(value(question, "decision.question"),
			value(question, "body")))
		if choice := a.String("choice"); choice != "" {
			label, ok := options[choice]
			if !ok {
				return proposal.Preview{}, fmt.Errorf("%w: the question offers no option %q",
					ErrInvalidArguments, choice)
			}
			c.add("choice", "Answer", label)
		}
		c.move("body", "Comment", "", a.String("body"))
		return proposal.Preview{Title: "Answer question", Target: value(doc, "key"),
			Note:         "Whoever asked is woken with the answer and goes on from it.",
			ApproveLabel: "Approve and answer", Changes: c.rows(), Request: req}, nil
	}
}

// askOf is the question an answer answers, by its comment id or, unnamed, the
// one open question on the thread, and its options by id.
func askOf(thread any, id string) (any, map[string]string) {
	comments, _ := cards.Lookup(thread, "comments").([]any)
	var found any
	for _, cm := range comments {
		open := value(cm, "ask") != "" && value(cm, "resolved") != "true"
		if (id != "" && value(cm, "id") == id) || (id == "" && open) {
			if found != nil && id == "" {
				return nil, nil
			}
			found = cm
		}
	}
	options := map[string]string{}
	list, _ := cards.Lookup(found, "decision.options").([]any)
	for _, o := range list {
		options[value(o, "id")] = value(o, "label")
	}
	return found, options
}

func prioritiesPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		handle := a.String("handle")
		if handle == "" {
			mine, err := readGet(ctx, read, "GET /work/my-work", nil, nil)
			if err != nil {
				return proposal.Preview{}, err
			}
			handle = value(mine, "handle")
		}
		person, err := readGet(ctx, read, personRoute, map[string]string{"handle": handle}, nil)
		if err != nil {
			return proposal.Preview{}, err
		}
		var c changes
		c.fact("whose", "Whose list", handle)
		c.move("priorities", "Order", strings.ReplaceAll(value(person, "priorities"), ", ", "\n"),
			strings.Join(a.Strings("items"), "\n"))
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
const personRoute = "GET /work/people/{handle}"

// inboxPreview names the notices a mark_inbox call marks, read through the
// grant from the person's own inbox: a card that said "Mark read: 12 notices"
// could carry a notice the person never meant to clear (a security notice a
// prompt in some task talked the model into marking) without showing it.
func inboxPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
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
	// inboxNamed is how many notices of one list a mark_inbox card names
	// one by one before it says how many more: enough to see what a short
	// list clears, short enough that a card stays readable.
	inboxNamed = 10
	// inboxPage and inboxPages bound the reads a mark_inbox preview makes
	// to find the notices it names: the inbox route's own largest page, at
	// most five times, which covers the two hundred ids a list may carry.
	inboxPage  = 200
	inboxPages = 5
	// noticeNameBytes bounds one notice's name on the card.
	noticeNameBytes = 160
)

// noticeNames reads the person's inbox through the grant until every id in
// ids is found or the inbox ends, answering each found notice's name by its
// record id.
func noticeNames(ctx context.Context, read proposal.Read, ids []string) (map[string]string, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := map[string]string{}
	cursor := ""
	for range inboxPages {
		q := url.Values{"limit": {strconv.Itoa(inboxPage)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		doc, err := readGet(ctx, read, "GET /work/inbox", nil, q)
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

// noticeName is how a card names one notice: what it is about and what it
// says, as the inbox shows it.
func noticeName(n any) string {
	about := orDefault(value(n, "subject_key"), value(n, "task"))
	says := orDefault(value(n, "excerpt"), value(n, "reason"))
	name := strings.TrimSpace(about + ": " + says)
	switch {
	case about == "":
		name = says
	case says == "":
		name = about
	}
	return textcut.Within(orDefault(name, "A notice"), noticeNameBytes)
}

// notices adds one row per notice of ids, named where the inbox read found it
// and said to be missing where it did not, then how many more past
// [inboxNamed].
func (c *changes) notices(field, label string, ids []string, names map[string]string) {
	for i, id := range ids {
		if i == inboxNamed {
			c.fact(field+".more", label, fmt.Sprintf("And %d more", len(ids)-inboxNamed))
			return
		}
		name, found := names[id]
		if !found {
			name = "A notice not in your inbox (" + id + ")"
		}
		c.fact(fmt.Sprintf("%s.%d", field, i+1), label, name)
	}
}

func seatPreview(pause bool) func(t Tool) proposal.Previewer {
	return func(t Tool) proposal.Previewer {
		return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
			req, err := built(t, args)
			if err != nil {
				return proposal.Preview{}, err
			}
			a := Args(args)
			doc, err := readGet(ctx, read, "GET /agents/{id}",
				map[string]string{"id": a.String("handle")}, nil)
			if err != nil {
				return proposal.Preview{}, err
			}
			var c changes
			c.fact("seat", "Seat", value(doc, "role")+" ("+a.String("handle")+")")
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
			c = append(c, proposal.Changed("state", "State", now, to))
			c.fact("reason", "Why", a.String("reason"))
			return proposal.Preview{Title: title, Target: a.String("handle"), Note: note,
				ApproveLabel: label, Changes: c.rows(), Request: req}, nil
		}
	}
}

func steerPreview(t Tool) proposal.Previewer {
	return func(_ context.Context, _ proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		var c changes
		c.fact("turn", "Turn", a.String("turn_id"))
		c.move("note", "Note", "", a.String("note"))
		return proposal.Preview{Title: "Steer turn", Target: a.String("turn_id"),
			Note:         "The note becomes the running agent's input at its next round.",
			ApproveLabel: "Approve and send", Changes: c.rows(), Request: req}, nil
	}
}

func runAnswerPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /sandbox-runs", nil, nil)
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
			return proposal.Preview{}, fmt.Errorf("%w: no coding run %s is parked on a question",
				ErrInvalidArguments, a.String("turn_id"))
		}
		var c changes
		c.fact("run", "Coding run", value(run, "agent_handle")+" on "+
			orDefault(value(run, "work_key"), "no task"))
		c.fact("question", "Question", value(run, "question"))
		c.move("answer", "Answer", "", a.String("answer"))
		return proposal.Preview{Title: "Answer coding run", Target: a.String("turn_id"),
			Note:         "The run resumes on the answer.",
			ApproveLabel: "Approve and answer", Changes: c.rows(), Request: req}, nil
	}
}

func createPagePreview(t Tool) proposal.Previewer {
	return func(_ context.Context, _ proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		var c changes
		c.fact("container", "Space", orDefault(a.String("container"), "Your team's space"))
		c.add("title", "Title", a.String("title"))
		c.add("status", "Status", a.String("status"))
		c.add("parent", "Under", a.String("parent"))
		c.add("labels", "Labels", strings.Join(a.Strings("labels"), ", "))
		c.move("body", "Page", "", a.String("body"))
		return proposal.Preview{Title: "Create page", Target: a.String("title"),
			ApproveLabel: "Approve and create", Changes: c.rows(), Request: req}, nil
	}
}

func editPagePreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		a := Args(args)
		doc, err := readGet(ctx, read, "GET /pages/{id}",
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
		c.fact("page", "Page", value(doc, "page.title"))
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
		return func(ctx context.Context, read proposal.Read, args map[string]any) (proposal.Preview, error) {
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
			var c changes
			c.fact("artifact", "Artifact", value(doc, "title"))
			p := proposal.Preview{Target: value(doc, "title"), Request: req,
				SourceRevision: proposal.Revision{Kind: proposal.RevisionArtifact,
					ID: value(doc, "id"), Version: value(doc, "head_number")}.String()}
			if share {
				who := audienceLabel(a.String("to"))
				c.add("audience", "Shared with", who+" as "+a.String("role"))
				p.Title, p.ApproveLabel, p.Audience = "Share artifact", "Approve and share", []string{who}
				p.Note = who + " can open it."
				p.StepUp = t.StepUp != nil && t.StepUp(a)
			} else {
				who := audienceLabel(a.String("from"))
				c = append(c, proposal.Removed("audience", "Shared with", who))
				p.Title, p.ApproveLabel, p.Audience = "Withdraw share", "Approve and withdraw", []string{who}
				p.Note = who + " can no longer open it, unless another share reaches them."
			}
			p.Changes = c.rows()
			return p, nil
		}
	}
}

// audienceLabel names an audience as the card shows it.
func audienceLabel(s string) string {
	switch {
	case s == "company":
		return "The whole company"
	case strings.HasPrefix(s, "unit:"):
		return "The " + strings.TrimPrefix(s, "unit:") + " team and everybody under it"
	}
	return strings.TrimPrefix(s, "seat:")
}

func pageActionPreview(t Tool) proposal.Previewer {
	return func(_ context.Context, _ proposal.Read, args map[string]any) (proposal.Preview, error) {
		req, err := built(t, args)
		if err != nil {
			return proposal.Preview{}, err
		}
		action, ok := PageActionOf(Args(args).String("action"))
		if !ok {
			return proposal.Preview{}, fmt.Errorf("%w: no such page action", ErrInvalidArguments)
		}
		var c changes
		c.fact("scope", "This screen", action.Scope)
		payload, _ := args["args"].(map[string]any)
		for _, k := range sortedKeys(payload) {
			c.fact(k, k, cards.Text(payload[k]))
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

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// Revision re-reads, through the turn's grant, the revision a preview recorded
// for the object it read: a task's version, a page's, an artifact's head and a
// person's tracker record's. An approval compares what it answers with the
// recorded revision, and an answer that differs is a stale proposal.
var Revision proposal.Revisions = func(ctx context.Context, read proposal.Read, source string) (string, error) {
	r, err := proposal.ParseRevision(source)
	if err != nil {
		return "", err
	}
	var req exec.Request
	path, versionAt, idAt := "", "", ""
	switch r.Kind {
	case proposal.RevisionTask:
		req, err = get("GET /work/{id}", map[string]string{"id": r.ID}, nil)
		path, idAt, versionAt = "task", "key", "version"
	case proposal.RevisionPage:
		req, err = get("GET /pages/{id}", map[string]string{"id": r.ID}, nil)
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

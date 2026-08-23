package gitlab

import (
	"context"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// Routing a code host.
//
// # Two layers, and the difference between them is what a seat is being
// asked for
//
// A DIRECTED event names its recipient in the payload: an assignee was
// added, a reviewer was requested, somebody wrote your name. Those route
// from the payload alone, need no reads, and survive a lapsed credential.
//
// THREAD ACTIVITY — a comment, a close, a merge, an approval — concerns
// everyone taking part, which is exactly the set GitLab itself would notify
// and exactly the set that is NOT in the payload. It takes one REST lookup
// per event, and without one it degrades to the payload's assignees. That
// degradation is deliberate and bounded: it can only ever cost reach on the
// watching layer, never on the directed one.
//
// Mentions stay TEXT-EXTRACTED rather than inferred from participation, for
// two reasons that both matter. A mention is a directed ask and gets its own
// prompt, where participation cannot tell "this note pings you" from "you
// commented here in March". And GitLab materialises a new mention into the
// participants list through a background job, so the lookup races the
// webhook that announced it — the text does not.

// Routing reasons, doubling as the event types the prompt dispatches on.
//
// They read as "<object>.<what happened>" because that is what the recipient
// needs to know first, and they are the vendor's own event vocabulary
// wherever GitLab has one.
const (
	IssueAssigned  = "issue.assigned"
	IssueMention   = "issue.mention"
	IssueClosed    = "issue.close"
	MRAssigned     = "merge_request.assigned"
	MRReview       = "merge_request.review_requested"
	MRMention      = "merge_request.mention"
	NoteMention    = "note.mention"
	NoteComment    = "note.comment"
	PipelineFailed = "pipeline.failed"
)

// mrStateChange builds the reason for a merge request's own state moving.
//
// Derived rather than tabulated, because the set is GitLab's and it grows:
// an approval rule added in a later release produces "merge_request.<action>"
// with no code change here, and the prompt's default branch already reads
// any of them as thread activity.
func mrStateChange(action string) string { return "merge_request." + action }

// mrStateActions are the merge request actions that concern the whole
// thread.
//
// Everything else an update hook can carry — a label, a milestone, a draft
// flag — is bookkeeping: it changes the item without asking anyone for
// anything, and routing it produces turns triaging "somebody added a label".
var mrStateActions = map[string]bool{
	"approval": true, "approved": true,
	"unapproval": true, "unapproved": true,
	"merge": true, "close": true,
}

// Participants lists everyone taking part in an issue or merge request:
// author, assignees, reviewers, commenters, and anyone previously mentioned.
//
// A seam because it needs the API and a credential, and the routing rules
// are worth testing without either.
type Participants interface {
	// Of returns lowercased usernames. Kind is "issue" or
	// "merge_request".
	Of(ctx context.Context, projectID int, kind string, iid int) ([]string, error)
}

// ParserOptions configure a [Parser].
type ParserOptions struct {
	// Participants is the thread fan-out. Nil degrades every watching
	// event to the payload's assignees, which is what a company with no
	// integrations.gitlab.token gets.
	Participants Participants
}

// Parser turns one GitLab webhook into the notifications it implies.
type Parser struct{ participants Participants }

// NewParser builds the parser.
//
// No required options, and no error: unlike the tracker, nothing here needs
// an instance URL — GitLab puts a full, absolute url on every object it
// sends, so a link is read rather than built.
func NewParser(opts ParserOptions) *Parser {
	return &Parser{participants: opts.Participants}
}

// Source implements [notify.Parser].
func (p *Parser) Source() string { return Backend }

// target is one recipient and why.
type target struct{ username, reason string }

// fanout names a thread whose participants should hear this event.
type fanout struct {
	kind   string
	iid    int
	reason string
}

// Parse implements [notify.Parser].
func (p *Parser) Parse(ctx context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	h, err := decode(w)
	if err != nil {
		return nil, err
	}

	var (
		targets []target
		fan     fanout
	)
	switch h.Kind {
	case "issue":
		targets, fan = routeIssue(h)
	case "merge_request":
		targets, fan = routeMergeRequest(h)
	case "note":
		targets, fan = routeNote(h)
	case "pipeline":
		targets = routePipeline(h)
	default:
		// Push, tag, wiki, deployment, release, emoji — none of them
		// names a party to notify. An emoji award is the near miss: it
		// carries the awarder as the actor and the awardable without a
		// username, so there is nobody to tell even though something
		// clearly happened.
		return nil, nil
	}
	if len(targets) == 0 && fan.reason == "" {
		return nil, nil
	}

	targets = append(targets, p.thread(ctx, h, fan)...)
	return build(h, targets, reg), nil
}

// routeIssue handles an issue's own lifecycle.
func routeIssue(h hook) ([]target, fanout) {
	var out []target
	switch h.Attrs.Action {
	case "update":
		out = append(out, targetsOf(added(h.Changes.Assignees), IssueAssigned)...)
		out = append(out, targetsOf(freshMentions(h), IssueMention)...)

	case "open", "reopen":
		out = append(out, targetsOf(usernames(h.Assignees), IssueAssigned)...)
		out = append(out, targetsOf(freshMentions(h), IssueMention)...)
		// NO FAN-OUT ON OPEN. A fresh issue's participants are its
		// author, its assignees and its description mentions — the
		// author is the actor, and the other two are already above. A
		// lookup here would spend a request to learn what the payload
		// just said.

	case "close":
		// A person closing an issue an agent is working on has to reach
		// that agent, and the message is "stop". This is the one issue
		// event where the watching layer earns its request.
		out = append(out, targetsOf(usernames(h.Assignees), IssueClosed)...)
		return out, fanout{kind: "issue", iid: h.Attrs.IID, reason: IssueClosed}
	}
	return out, fanout{}
}

// routeMergeRequest handles a merge request's own lifecycle.
func routeMergeRequest(h hook) ([]target, fanout) {
	action := h.Attrs.Action
	var out []target
	switch {
	case action == "update":
		out = append(out, targetsOf(added(h.Changes.Reviewers), MRReview)...)
		out = append(out, targetsOf(added(h.Changes.Assignees), MRAssigned)...)
		out = append(out, targetsOf(freshMentions(h), MRMention)...)

	case mrStateActions[action]:
		// The author reports back to whoever asked for the change; the
		// reviewer's follow-up closes it out. The payload's assignees
		// are the degraded fallback and a reliable one here: in the
		// agent model the agent that opens a merge request assigns
		// itself, and the author's USERNAME is not in this hook at all —
		// only author_id.
		reason := mrStateChange(action)
		out = append(out, targetsOf(usernames(h.Assignees), reason)...)
		return out, fanout{kind: "merge_request", iid: h.Attrs.IID, reason: reason}

	case action == "open" || action == "reopen":
		out = append(out, targetsOf(usernames(h.Reviewers), MRReview)...)
		out = append(out, targetsOf(usernames(h.Assignees), MRAssigned)...)
		out = append(out, targetsOf(freshMentions(h), MRMention)...)
		if action == "reopen" {
			// Reopening wakes the THREAD, not just the named
			// parties: everyone who took part before the merge
			// request was closed has a stake in it being live again.
			return out, fanout{kind: "merge_request",
				iid: h.Attrs.IID, reason: mrStateChange("reopen")}
		}
	}
	// An unrecognised action — an auto-merge, a draft toggle a later
	// release names differently — reaches nobody rather than everybody.
	return out, fanout{}
}

// routeNote handles a comment.
func routeNote(h hook) ([]target, fanout) {
	text := h.Attrs.Note
	out := targetsOf(Mentions(text), NoteMention)

	var (
		item noteable
		kind string
	)
	switch strings.ToLower(h.Attrs.NoteableType) {
	case "mergerequest":
		item, kind = h.MergeRequest, "merge_request"
	case "issue":
		item, kind = h.Issue, "issue"
	default:
		// A comment on a snippet or a commit. The mentions above still
		// route — being named is being named — but there is no thread
		// to fan out to.
		return out, fanout{}
	}
	out = append(out, targetsOf(usernames(item.Assignees), NoteComment)...)
	return out, fanout{kind: kind, iid: item.IID, reason: NoteComment}
}

// routePipeline handles a build.
//
// FAILURES ONLY, and only the actor. A green pipeline is not news, and the
// person whose push turned it red is the one who has to fix it — which is
// why this is the one event in the integration that deliberately reaches its
// own actor. The override lives on the prompt (see [Prompt.WakesActor]), so
// the exception is stated once where the spine reads it rather than
// implemented here as a flag the guard has to be told about.
func routePipeline(h hook) []target {
	if h.Attrs.Status != "failed" || h.actor() == "" {
		return nil
	}
	return targetsOf([]string{h.actor()}, PipelineFailed)
}

// thread is the participants fan-out.
//
// PURELY ADDITIVE over the directed targets: the dedupe below keeps the
// first, higher-signal reason per person, so a mentioned participant is
// still woken as a mention. Best effort in every direction — no lookup, a
// failed lookup, an unknown project — because losing reach on the watching
// layer is a smaller harm than a delivery that raises.
func (p *Parser) thread(ctx context.Context, h hook, fan fanout) []target {
	if p.participants == nil || fan.kind == "" || fan.iid == 0 || fan.reason == "" {
		return nil
	}
	projectID := h.projectID()
	if projectID == 0 {
		log.Warn("gitlab_thread_project_unknown", "kind", fan.kind, "iid", fan.iid)
		return nil
	}
	people, err := p.participants.Of(ctx, projectID, fan.kind, fan.iid)
	if err != nil {
		log.Warn("gitlab_participants_unavailable", "project", projectID,
			"kind", fan.kind, "iid", fan.iid, "error", err.Error())
		return nil
	}
	return targetsOf(people, fan.reason)
}

// freshMentions are the description mentions this event newly introduces.
//
// On open and reopen every mention in the description is fresh. On UPDATE
// only the ones the edit ADDED count, which is GitLab's own semantics:
// re-saving a description does not re-notify the people it already named,
// and a parser that ignored the diff would ping every named person on every
// typo fix.
func freshMentions(h hook) []string {
	if h.Attrs.Action != "update" {
		return Mentions(h.Attrs.Description)
	}
	before := map[string]bool{}
	for _, name := range Mentions(h.Changes.Description.Previous) {
		before[name] = true
	}
	var fresh []string
	for _, name := range Mentions(h.Changes.Description.Current) {
		if !before[name] {
			fresh = append(fresh, name)
		}
	}
	return fresh
}

func targetsOf(names []string, reason string) []target {
	out := make([]target, 0, len(names))
	for _, name := range names {
		out = append(out, target{username: name, reason: reason})
	}
	return out
}

// build turns the target list into one notification per recipient.
//
// THE SINGLE CHOKE POINT where every fan-out meets the parties the engine
// can actually route to. A repository has contributors who are not seats
// here, and a comment naming three of them would otherwise produce three
// notifications the service cannot deliver — each one an undeliverable
// warning and a skip record, by the dozen on a busy project.
//
// ONE gate rather than one per layer. The routing rules above pre-filtered
// their own mentions and participants too, which was not defence in depth:
// it made the counter below under-report, and it meant two places could
// disagree about who is routable with only one of them being tested.
//
// FIRST REASON PER PERSON WINS, and the list arrives in priority order, so a
// mentioned assignee is woken once — as a mention, which is the stronger
// claim on their attention and the one the prompt renders differently.
//
// The ACTOR is not filtered here. It is stamped on the metadata under the
// one key every vendor uses and suppressed by the spine, which knows the
// exception ([Prompt.WakesActor]) and can resolve an actor across identity
// namespaces — neither of which a parser comparing usernames can do.
//
// A NIL REGISTRY LETS EVERYTHING THROUGH, which is right for the one caller
// that has none: a test exercising the routing rules themselves, where the
// question is which usernames a payload implies rather than which of them
// work here.
func build(h hook, targets []target, reg *notify.Registry) []notify.Routed {
	var (
		out     []notify.Routed
		seen    = map[string]bool{}
		dropped int
	)
	for _, t := range targets {
		if t.username == "" || seen[t.username] {
			continue
		}
		seen[t.username] = true
		if reg != nil {
			if _, ok := reg.ByExternalID(Backend, t.username); !ok {
				dropped++
				continue
			}
		}
		out = append(out, notify.Routed{
			Inbound: inbound(h, t.reason),
			To:      notify.Recipient{ExternalIDs: []string{t.username}},
		})
	}
	if dropped > 0 {
		log.Debug("gitlab_outsiders_dropped", "count", dropped, "kind", h.Kind)
	}
	return out
}

// inbound assembles one recipient's copy.
//
// Built per recipient rather than once and shared, because the metadata
// carries the ROUTING REASON and the copies differ in it — a shared map
// would make every copy claim whichever reason was written last.
func inbound(h hook, reason string) notify.Inbound {
	meta := map[string]string{
		notify.ActorField: h.actor(),
		"actor_name":      h.User.Username,
		"event_type":      reason,
		"project":         h.Project.Path,
	}
	subject, body := subjectAndBody(h)
	if key, value := itemRef(h); key != "" {
		meta[key] = value
	}
	if h.Attrs.URL != "" {
		meta["url"] = h.Attrs.URL
	}
	return notify.Inbound{
		Source: Backend, EventType: reason,
		Sender: h.User.Username, Subject: subject, Body: body, Metadata: meta,
	}
}

// subjectAndBody is what the event is about, in the shape each kind carries.
func subjectAndBody(h hook) (string, string) {
	switch h.Kind {
	case "note":
		// The NOTE is the body and the noteable's title is the subject:
		// a comment's own "title" does not exist, and rendering the
		// issue's description here would bury what was just said.
		if item := noteableOf(h); item.Title != "" {
			return item.Title, h.Attrs.Note
		}
		return "Comment", h.Attrs.Note
	case "pipeline":
		return "Pipeline " + h.Attrs.Status, ""
	default:
		return h.Attrs.Title, h.Attrs.Description
	}
}

// itemRef is the issue or merge request this event names, as a metadata key
// and value.
//
// The IID, never the global id: it is what a person sees, what the API takes
// on a project-scoped path, and what the conversation key is built from.
func itemRef(h hook) (string, string) {
	switch h.Kind {
	case "issue":
		return refOf("issue_iid", h.Attrs.IID)
	case "merge_request":
		return refOf("mr_iid", h.Attrs.IID)
	case "note":
		switch strings.ToLower(h.Attrs.NoteableType) {
		case "mergerequest":
			return refOf("mr_iid", h.MergeRequest.IID)
		case "issue":
			return refOf("issue_iid", h.Issue.IID)
		}
	case "pipeline":
		// A pipeline for a merge request names it; one for a branch
		// push names nothing, and that is the honest answer — there is
		// no item to go and read.
		return refOf("mr_iid", h.MergeRequest.IID)
	}
	return "", ""
}

func refOf(key string, iid int) (string, string) {
	if iid == 0 {
		return "", ""
	}
	return key, strconv.Itoa(iid)
}

func noteableOf(h hook) noteable {
	if strings.EqualFold(h.Attrs.NoteableType, "mergerequest") {
		return h.MergeRequest
	}
	return h.Issue
}

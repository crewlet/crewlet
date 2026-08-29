package github

import (
	"context"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// Routing a hosted code host.
//
// # Two layers, and the difference between them is what a seat is asked for
//
// A DIRECTED event names its recipient in the payload: an assignee was
// added, a review was requested, somebody wrote your name, somebody asked
// for changes on your pull request. Those route from the payload alone, need
// no reads, and survive a lapsed credential.
//
// THREAD ACTIVITY — a comment, a close, a merge — concerns everyone taking
// part, and GitHub does not put that set in the payload either. It costs one
// read per event, and without one it degrades to the author and assignees
// the payload does carry. That degradation is deliberate and bounded: it can
// only ever cost reach on the watching layer, never on the directed one.
//
// # Three places this deliberately differs from the self-hosted host
//
// GITHUB NAMES THE PARTY, so there is no diff. An assignment arrives as
// `action: assigned` with `assignee` set, and a review request as
// `action: review_requested` with `requested_reviewer` set — one event per
// party. The self-hosted host sends one `update` carrying the whole list and
// leaves the reader to diff it against the previous one, which is where its
// `added()` comes from. Reading a list here would ping every assignee each
// time anybody was added.
//
// THE AUTHOR IS IN THE PAYLOAD. `pull_request.user.login` and
// `issue.user.login` are logins rather than opaque ids, so the person who
// opened the thing can be told what happened to it without a lookup. The
// self-hosted host carries only `author_id` and cannot.
//
// A CLOSED PULL REQUEST IS TWO EVENTS WEARING ONE NAME. GitHub sends
// `action: closed` for a merge and for an abandonment, distinguished only by
// `pull_request.merged` — and to the author they are opposite outcomes. They
// are separated here, at the only point that can still tell them apart.

// Routing reasons, doubling as the event types the prompt dispatches on.
//
// They read as "<object>.<what happened>" because that is what the recipient
// needs to know first, and they are GitHub's own vocabulary wherever it has
// one.
const (
	IssueAssigned = "issue.assigned"
	IssueMention  = "issue.mention"
	IssueClosed   = "issue.close"

	PRAssigned         = "pull_request.assigned"
	PRReviewRequested  = "pull_request.review_requested"
	PRMention          = "pull_request.mention"
	PRMerged           = "pull_request.merged"
	PRClosed           = "pull_request.close"
	PRApproved         = "pull_request.approved"
	PRChangesRequested = "pull_request.changes_requested"
	PRReviewed         = "pull_request.reviewed"

	CommentMention = "comment.mention"
	CommentAdded   = "comment.created"

	WorkflowFailed = "workflow_run.failed"
)

// prStateChange builds the reason for a pull request's own state moving.
//
// Derived rather than tabulated for the actions with no special meaning, so
// an action a later API version adds produces "pull_request.<action>" with
// no code change here — and the prompt's default branch already reads any of
// them as thread activity.
func prStateChange(action string) string { return "pull_request." + action }

// prStateActions are the pull request actions that concern the whole thread.
//
// Everything else `pull_request` can carry — a label, a milestone, a new
// commit pushed (`synchronize`), an auto-merge toggle — is bookkeeping: it
// changes the item without asking anyone for anything, and routing it
// produces turns triaging "somebody added a label".
var prStateActions = map[string]bool{
	"closed": true, "reopened": true, "ready_for_review": true,
	"converted_to_draft": true,
}

// Participants lists everyone taking part in an issue or pull request beyond
// what the payload already names: the people who have COMMENTED and, on a
// pull request, the people who have REVIEWED.
//
// A seam because it needs the API and a credential, and the routing rules
// are worth testing without either.
type Participants interface {
	// Of returns lowercased logins. Kind is "issue" or "pull_request".
	Of(ctx context.Context, owner, repo, kind string, number int) ([]string, error)
}

// ParserOptions configure a [Parser].
type ParserOptions struct {
	// Participants is the thread fan-out. Nil degrades every watching
	// event to the payload's author and assignees, which is what a
	// company with no integrations.github.token gets.
	Participants Participants
}

// Parser turns one GitHub delivery into the notifications it implies.
type Parser struct{ participants Participants }

// NewParser builds the parser.
//
// No required options, and no error: GitHub puts an absolute html_url on
// every object it sends, so a link is read rather than built and the parser
// needs no instance address even on an Enterprise Server.
func NewParser(opts ParserOptions) *Parser {
	return &Parser{participants: opts.Participants}
}

// Source implements [notify.Parser].
func (p *Parser) Source() string { return Backend }

// target is one recipient and why.
type target struct{ login, reason string }

// fanout names a thread whose participants should hear this event.
type fanout struct {
	kind   string
	number int
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
	switch h.Event {
	case EventIssues:
		targets, fan = routeIssue(h)
	case EventPullRequest:
		targets, fan = routePullRequest(h)
	case EventIssueComment:
		targets, fan = routeComment(h)
	case EventReview:
		targets, fan = routeReview(h)
	case EventReviewNote:
		targets, fan = routeReviewComment(h)
	case EventWorkflowRun:
		targets = routeWorkflowRun(h)
	default:
		// push, create, delete, release, star, fork, check_run — none of
		// them names a party to notify. `check_run` is the near miss and
		// is deliberately left out: it reports the same failing Actions
		// run as workflow_run, once per job, so routing both would wake
		// the same seat for one red build as many times as the workflow
		// has jobs.
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
	switch h.Action {
	case "assigned":
		// THE PARTY IS NAMED, so this is one target rather than a diff.
		out = append(out, targetsOf([]string{login(h.Assignee)}, IssueAssigned)...)

	case "opened":
		out = append(out, targetsOf(logins(h.Issue.Assignees), IssueAssigned)...)
		out = append(out, targetsOf(Mentions(h.Issue.Body), IssueMention)...)
		// NO FAN-OUT ON OPEN. A fresh issue's participants are its
		// author, its assignees and its body mentions — the author is
		// the actor and the other two are above. A lookup here would
		// spend a request to learn what the payload just said.

	case "edited":
		out = append(out, targetsOf(freshMentions(h, h.Issue.Body), IssueMention)...)

	case "closed":
		// A person closing an issue an agent is working on has to reach
		// that agent, and the message is "stop". This is the one issue
		// event where the watching layer earns its request.
		out = append(out, targetsOf(logins(h.Issue.Assignees), IssueClosed)...)
		return out, fanout{kind: "issue", number: h.Issue.Number, reason: IssueClosed}

	case "reopened":
		reason := "issue.reopen"
		out = append(out, targetsOf(logins(h.Issue.Assignees), reason)...)
		return out, fanout{kind: "issue", number: h.Issue.Number, reason: reason}
	}
	return out, fanout{}
}

// routePullRequest handles a pull request's own lifecycle.
func routePullRequest(h hook) ([]target, fanout) {
	pr := h.PullRequest
	var out []target
	switch action := h.Action; {
	case action == "assigned":
		out = append(out, targetsOf([]string{login(h.Assignee)}, PRAssigned)...)

	case action == "review_requested":
		if h.RequestedReviewer.Login == "" && h.RequestedTeam.Slug != "" {
			// A TEAM REVIEW REQUEST NAMES NO PERSON, and expanding it
			// would mean a members lookup on the inbound path to
			// produce a fan-out GitHub itself treats as weaker than a
			// direct request. Logged rather than dropped silently:
			// "the review request reached nobody" is otherwise
			// indistinguishable from "no review was requested".
			log.Debug("github_team_review_request_not_routed",
				"team", h.RequestedTeam.Slug, "repo", h.Repository.FullName,
				"number", pr.Number)
			return nil, fanout{}
		}
		out = append(out, targetsOf([]string{login(h.RequestedReviewer)}, PRReviewRequested)...)

	case action == "opened":
		out = append(out, targetsOf(logins(pr.Reviewers), PRReviewRequested)...)
		out = append(out, targetsOf(logins(pr.Assignees), PRAssigned)...)
		out = append(out, targetsOf(Mentions(pr.Body), PRMention)...)

	case action == "edited":
		out = append(out, targetsOf(freshMentions(h, pr.Body), PRMention)...)

	case prStateActions[action]:
		// THE AUTHOR FIRST. A pull request's outcome is news to whoever
		// opened it before it is news to anyone else, and GitHub gives
		// the login rather than an opaque id — so this needs no lookup
		// and works with no credential at all.
		reason := stateReason(h)
		out = append(out, targetsOf([]string{login(pr.User)}, reason)...)
		out = append(out, targetsOf(logins(pr.Assignees), reason)...)
		return out, fanout{kind: "pull_request", number: pr.Number, reason: reason}
	}
	// An unrecognised action — a label, a synchronize, an auto-merge a
	// later API version names differently — reaches nobody rather than
	// everybody.
	return out, fanout{}
}

// stateReason separates a merge from an abandonment.
//
// GitHub sends `closed` for both and distinguishes them with a boolean, and
// to the author they are opposite outcomes: one means the work landed, the
// other means it did not and somebody decided so. Collapsing them would
// render "your change is in" and "your change was dropped" as one line.
func stateReason(h hook) string {
	if h.Action == "closed" {
		if h.PullRequest.Merged {
			return PRMerged
		}
		return PRClosed
	}
	return prStateChange(h.Action)
}

// routeComment handles a comment on an issue or on a pull request's
// conversation.
func routeComment(h hook) ([]target, fanout) {
	if h.Action != "created" && h.Action != "edited" {
		// A DELETED comment names nobody: whatever it said is gone, and
		// a notification pointing at it would send the recipient to a
		// 404.
		return nil, fanout{}
	}
	body := h.Comment.Body
	mentions := Mentions(body)
	if h.Action == "edited" {
		// An edit that ADDS a mention is a real ping — GitHub notifies
		// on it too — and one that fixes a typo is not. Only the new
		// names count, and nothing else about an edited comment reaches
		// anybody.
		return targetsOf(freshMentions(h, body), CommentMention), fanout{}
	}
	out := targetsOf(mentions, CommentMention)

	// The issue payload IS the pull request payload when the comment is on
	// a pull request's conversation — GitHub models one as the other — so
	// the kind is read from the item rather than from the event name.
	kind := "issue"
	if h.Issue.isPullRequest() {
		kind = "pull_request"
	}
	out = append(out, targetsOf([]string{login(h.Issue.User)}, CommentAdded)...)
	out = append(out, targetsOf(logins(h.Issue.Assignees), CommentAdded)...)
	return out, fanout{kind: kind, number: h.Issue.Number, reason: CommentAdded}
}

// routeReview handles a submitted review.
//
// THE STRONGEST DIRECTED SIGNAL A CODE HOST HAS, and the self-hosted one
// beside this has no equivalent: it carries approvals but not a review that
// asks for changes with a body explaining what. A changes-requested review
// is a direct ask at the author, and the prompt renders it as one.
func routeReview(h hook) ([]target, fanout) {
	if h.Action != "submitted" {
		return nil, fanout{}
	}
	pr := h.PullRequest
	reason := reviewReason(h.Review.State)
	out := targetsOf(Mentions(h.Review.Body), PRMention)
	out = append(out, targetsOf([]string{login(pr.User)}, reason)...)
	return out, fanout{kind: "pull_request", number: pr.Number, reason: reason}
}

// reviewReason maps a review's verdict onto what it asks of the author.
func reviewReason(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "approved":
		return PRApproved
	case "changes_requested":
		return PRChangesRequested
	default:
		// "commented", and anything a later API version adds. A review
		// with no verdict is thread activity with a body.
		return PRReviewed
	}
}

// routeReviewComment handles one comment on a diff line.
func routeReviewComment(h hook) ([]target, fanout) {
	if h.Action != "created" {
		return nil, fanout{}
	}
	pr := h.PullRequest
	out := targetsOf(Mentions(h.Comment.Body), CommentMention)
	out = append(out, targetsOf([]string{login(pr.User)}, CommentAdded)...)
	return out, fanout{kind: "pull_request", number: pr.Number, reason: CommentAdded}
}

// routeWorkflowRun handles a build.
//
// FAILURES ONLY, and only the actor. A green run is not news, and the person
// whose push turned it red is the one who has to fix it — which is why this
// is the one event in the integration that deliberately reaches its own
// actor. The override lives on the prompt (see [Prompt.WakesActor]), so the
// exception is stated once where the spine reads it rather than implemented
// here as a flag the guard has to be told about.
func routeWorkflowRun(h hook) []target {
	run := h.WorkflowRun
	if h.Action != "completed" || run.Conclusion != "failure" || h.actor() == "" {
		// CANCELLED AND TIMED_OUT ARE NOT FAILURES. A cancel is somebody
		// deciding the run was unnecessary, and waking the pusher to fix
		// a build somebody deliberately stopped is noise. A timeout
		// reaches nobody for a weaker reason and it is the honest one:
		// it is usually the runner rather than the diff, and GitHub
		// reports it identically whether the job hung or the fleet did.
		return nil
	}
	return targetsOf([]string{h.actor()}, WorkflowFailed)
}

// thread is the participants fan-out.
//
// PURELY ADDITIVE over the directed targets: the dedupe below keeps the
// first, higher-signal reason per person, so a mentioned participant is
// still woken as a mention. Best effort in every direction — no lookup, a
// failed lookup, an unknown repository — because losing reach on the
// watching layer is a smaller harm than a delivery that raises.
func (p *Parser) thread(ctx context.Context, h hook, fan fanout) []target {
	if p.participants == nil || fan.kind == "" || fan.number == 0 || fan.reason == "" {
		return nil
	}
	owner, repo := h.owner(), h.repo()
	if owner == "" || repo == "" {
		log.Warn("github_thread_repository_unknown",
			"kind", fan.kind, "number", fan.number)
		return nil
	}
	people, err := p.participants.Of(ctx, owner, repo, fan.kind, fan.number)
	if err != nil {
		log.Warn("github_participants_unavailable", "repo", h.Repository.FullName,
			"kind", fan.kind, "number", fan.number, "error", err.Error())
		return nil
	}
	return targetsOf(people, fan.reason)
}

// freshMentions are the mentions an edit newly introduces.
//
// GitHub's own semantics: re-saving a body does not re-notify the people it
// already named. A parser ignoring the diff would ping every named person on
// every typo fix — and on GitHub that is worse than on a tracker, because
// editing a comment to fix formatting is routine.
func freshMentions(h hook, current string) []string {
	before := map[string]bool{}
	for _, name := range Mentions(h.Changes.Body.From) {
		before[name] = true
	}
	var fresh []string
	for _, name := range Mentions(current) {
		if !before[name] {
			fresh = append(fresh, name)
		}
	}
	return fresh
}

func targetsOf(names []string, reason string) []target {
	out := make([]target, 0, len(names))
	for _, name := range names {
		out = append(out, target{login: name, reason: reason})
	}
	return out
}

// build turns the target list into one notification per recipient.
//
// THE SINGLE CHOKE POINT where every fan-out meets the parties the engine
// can actually route to. A repository has contributors who are not seats
// here, and a comment naming three of them would otherwise produce three
// notifications the service cannot deliver — each one an undeliverable
// warning and a skip record, by the dozen on a busy repository.
//
// FIRST REASON PER PERSON WINS, and the list arrives in priority order, so a
// mentioned author is woken once — as a mention, which is the stronger claim
// on their attention and the one the prompt renders differently.
//
// The ACTOR is not filtered here. It is stamped on the metadata under the
// one key every vendor uses and suppressed by the spine, which knows the
// exception ([Prompt.WakesActor]) and can resolve an actor across identity
// namespaces — neither of which a parser comparing logins can do.
//
// A NIL REGISTRY LETS EVERYTHING THROUGH, which is right for the one caller
// that has none: a test exercising the routing rules themselves, where the
// question is which logins a payload implies rather than which of them work
// here.
func build(h hook, targets []target, reg *notify.Registry) []notify.Routed {
	var (
		out     []notify.Routed
		seen    = map[string]bool{}
		dropped int
	)
	for _, t := range targets {
		if t.login == "" || seen[t.login] {
			continue
		}
		seen[t.login] = true
		if reg != nil {
			if _, ok := reg.ByExternalID(Backend, t.login); !ok {
				dropped++
				continue
			}
		}
		out = append(out, notify.Routed{
			Inbound: inbound(h, t.reason),
			To:      notify.Recipient{ExternalIDs: []string{t.login}},
		})
	}
	if dropped > 0 {
		log.Debug("github_outsiders_dropped", "count", dropped, "event", h.Event)
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
		"actor_name":      h.Sender.Login,
		"event_type":      reason,
		"repo":            h.Repository.FullName,
	}
	subject, body := subjectAndBody(h)
	if key, value := itemRef(h); key != "" {
		meta[key] = value
	}
	if url := eventURL(h); url != "" {
		meta["url"] = url
	}
	if h.Event == EventReviewNote && h.Comment.Path != "" {
		// The diff line a review comment hangs off. Without it the body
		// is a sentence about code with no way to tell which code.
		meta["path"] = h.Comment.Path
		if h.Comment.Line > 0 {
			meta["line"] = strconv.Itoa(h.Comment.Line)
		}
	}
	return notify.Inbound{
		Source: Backend, EventType: reason,
		Sender: h.Sender.Login, Subject: subject, Body: body, Metadata: meta,
	}
}

// subjectAndBody is what the event is about, in the shape each event carries.
func subjectAndBody(h hook) (string, string) {
	switch h.Event {
	case EventIssueComment, EventReviewNote:
		// The COMMENT is the body and the item's title is the subject: a
		// comment has no title of its own, and rendering the issue's
		// description here would bury what was just said.
		if title := itemOf(h).Title; title != "" {
			return title, h.Comment.Body
		}
		return "Comment", h.Comment.Body
	case EventReview:
		if title := h.PullRequest.Title; title != "" {
			return title, h.Review.Body
		}
		return "Review", h.Review.Body
	case EventWorkflowRun:
		run := h.WorkflowRun
		subject := run.Name
		if subject == "" {
			subject = "Workflow"
		}
		if run.HeadBranch != "" {
			subject += " on " + run.HeadBranch
		}
		return subject, ""
	case EventPullRequest:
		return h.PullRequest.Title, h.PullRequest.Body
	default:
		return h.Issue.Title, h.Issue.Body
	}
}

// eventURL is the link a recipient should follow, which is the most specific
// thing the event names.
//
// A COMMENT'S OWN URL, not its item's, wherever there is one: GitHub anchors
// a comment link to the comment, and sending a recipient to the top of a
// thread with two hundred messages to find the one they were named in is a
// link that technically works.
func eventURL(h hook) string {
	switch h.Event {
	case EventIssueComment, EventReviewNote:
		if h.Comment.HTMLURL != "" {
			return h.Comment.HTMLURL
		}
	case EventReview:
		if h.Review.HTMLURL != "" {
			return h.Review.HTMLURL
		}
	case EventWorkflowRun:
		return h.WorkflowRun.HTMLURL
	}
	if url := itemOf(h).HTMLURL; url != "" {
		return url
	}
	return h.Repository.HTMLURL
}

// itemRef is the issue or pull request this event names, as a metadata key
// and value.
//
// The NUMBER, never a node id: it is what a person sees, what every API path
// takes, and what the conversation key is built from.
func itemRef(h hook) (string, string) {
	switch h.Event {
	case EventIssues:
		return refOf("issue_number", h.Issue.Number)
	case EventPullRequest, EventReview, EventReviewNote:
		return refOf("pr_number", h.PullRequest.Number)
	case EventIssueComment:
		if h.Issue.isPullRequest() {
			return refOf("pr_number", h.Issue.Number)
		}
		return refOf("issue_number", h.Issue.Number)
	case EventWorkflowRun:
		// A run for a pull request names it; one for a branch push names
		// nothing, and that is the honest answer — there is no item to
		// go and read. A fork's pull request lands here too: GitHub
		// leaves the list empty for a run in the upstream repository.
		if prs := h.WorkflowRun.PullRequests; len(prs) > 0 {
			return refOf("pr_number", prs[0].Number)
		}
	}
	return "", ""
}

func refOf(key string, number int) (string, string) {
	if number == 0 {
		return "", ""
	}
	return key, strconv.Itoa(number)
}

// itemOf is whichever of the two item fields this event populated.
func itemOf(h hook) item {
	if h.PullRequest.Number != 0 {
		return h.PullRequest
	}
	return h.Issue
}

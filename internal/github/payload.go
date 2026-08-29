// Package github is the hosted code-host integration: github.com or an
// Enterprise Server, on the same terms the self-hosted one is served on.
package github

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("github")

// Backend is the transport name.
const Backend = "github"

// Reading a GitHub delivery.
//
// # The event name is in a HEADER, and that is the whole shape of this file
//
// Every other vendor here says what a delivery is inside the body —
// `object_kind` on GitLab, `webhookEvent` on Jira, `type` on Slack. GitHub
// says it in `X-GitHub-Event` and puts only the ACTION in the body, so
// `{"action": "created"}` is the entire discriminator a body-only reader
// gets: created WHAT is not in there. A parser that read the body alone
// would route an issue comment and a review comment identically, which is
// two different asks rendered as one.
//
// The header survives to here because the API edge captures headers onto the
// envelope for exactly this reason, and it is LOWERCASED there — Go's
// canonical form is per-header and matching it by hand is how a lookup comes
// back empty against a header that is present.
//
// # Typed, like the self-hosted host's
//
// GitHub's payloads are documented, versioned by an API-version header, and
// stable — so they decode into structs, and a field a branch reads is
// declared where the next reader can see it. A key GitHub adds is ignored
// rather than fatal, which is what makes an API-version bump a non-event.

// The delivery header names, lowercased as the envelope carries them.
const (
	eventHeader    = "x-github-event"
	deliveryHeader = "x-github-delivery"
)

// The event names this parser routes, as GitHub spells them.
const (
	EventIssues       = "issues"
	EventPullRequest  = "pull_request"
	EventIssueComment = "issue_comment"
	EventReview       = "pull_request_review"
	EventReviewNote   = "pull_request_review_comment"
	EventWorkflowRun  = "workflow_run"
)

// hook is one delivery, across every event this parser handles.
//
// ONE struct rather than one per event, for the reason the self-hosted
// host's is one: the events overlap almost entirely — every one carries a
// repository and a sender, and half of them carry an issue or a pull
// request — and a discriminated union would declare those fields six times
// with six chances to spell one differently.
type hook struct {
	// Event is the X-GitHub-Event header, carried in rather than decoded:
	// it is not in the body at all.
	Event string `json:"-"`

	Action string `json:"action"`

	Repository repository `json:"repository"`
	Sender     user       `json:"sender"`

	// Issue and PullRequest are the item an event is about. An
	// issue_comment on a PULL REQUEST arrives under Issue with
	// Issue.PullRequest set — GitHub models a pull request as an issue
	// with a diff, and a reader that took Issue at face value would file
	// every pull-request comment under the wrong kind.
	Issue       item `json:"issue"`
	PullRequest item `json:"pull_request"`

	Comment comment `json:"comment"`
	Review  review  `json:"review"`

	// Assignee and RequestedReviewer are the party this event is ABOUT,
	// which GitHub names directly rather than leaving to a diff of the
	// before and after lists. See [routeIssue].
	Assignee          user `json:"assignee"`
	RequestedReviewer user `json:"requested_reviewer"`
	RequestedTeam     team `json:"requested_team"`

	Changes     changes     `json:"changes"`
	WorkflowRun workflowRun `json:"workflow_run"`
}

type user struct {
	Login string `json:"login"`
	// Type is "Bot" for an app's own identity. Read so a bot's own
	// activity can be told from a person's where that matters.
	Type string `json:"type"`
}

type team struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type repository struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Owner    user   `json:"owner"`
	HTMLURL  string `json:"html_url"`
}

// item is an issue or a pull request, which share a shape.
type item struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	Merged    bool   `json:"merged"`
	User      user   `json:"user"`
	Assignees []user `json:"assignees"`
	Reviewers []user `json:"requested_reviewers"`

	// PullRequest is present, and non-nil, on an ISSUE that is really a
	// pull request. Its contents are links this integration never
	// follows; what matters is whether the key is there at all.
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

// isPullRequest reports an issue payload that is really a pull request.
func (i item) isPullRequest() bool { return i.PullRequest != nil }

type comment struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    user   `json:"user"`
	// Path and Line are set on a review comment: the diff line it hangs
	// off. Carried into the metadata because "on line 42 of client.go" is
	// most of what a review comment means.
	Path string `json:"path"`
	Line int    `json:"line"`
}

type review struct {
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    user   `json:"user"`
	// State is approved, changes_requested or commented, lowercased by
	// GitHub on the webhook (the REST API upper-cases it).
	State string `json:"state"`
}

// changes is the before value an edit carries.
//
// Only the body, because it is the only edited field that names a person: a
// retitled issue pings nobody, and GitHub sends the same `edited` action for
// both.
type changes struct {
	Body struct {
		From string `json:"from"`
	} `json:"body"`
}

type workflowRun struct {
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadBranch string `json:"head_branch"`
	Actor      user   `json:"actor"`
	// PullRequests is the pull requests this run belongs to, which is
	// empty for a run on a branch push. GitHub populates it only for runs
	// triggered by a pull_request event in the SAME repository, so a fork
	// PR's failing run legitimately names no item.
	PullRequests []struct {
		Number int `json:"number"`
	} `json:"pull_requests"`
}

// decode reads a delivery.
//
// PREFERS THE RAW BYTES, which are the ones the signature was checked
// against and the only faithful copy: a payload that round-tripped through a
// map has had every number turned into a float, which is how issue 42 comes
// back as "42" in one place and "42.000000" in another. The map is the
// fallback for a delivery published from inside the engine, where nothing
// arrived over a wire.
func decode(w types.RawWebhook) (hook, error) {
	var h hook
	raw := w.BodyRaw
	if len(raw) == 0 {
		encoded, err := json.Marshal(w.Body)
		if err != nil {
			return hook{}, fmt.Errorf("github: re-encode delivery: %w", err)
		}
		raw = encoded
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return hook{}, fmt.Errorf("github: decode delivery: %w", err)
	}
	h.Event = strings.ToLower(strings.TrimSpace(w.Headers[eventHeader]))
	if h.Event == "" {
		// NOT AN ERROR. A delivery with no event header is one this
		// integration cannot name, and the honest outcome is to route
		// nothing — the same as an event it knows and does not route.
		// Failing would nak the delivery and have GitHub redeliver a
		// payload that will be just as unnameable next time.
		log.Debug("github_delivery_has_no_event_header",
			"delivery", w.Headers[deliveryHeader])
	}
	return h, nil
}

// actor is who caused the event, lowercased.
//
// LOWERCASED AT EVERY BOUNDARY, because GitHub preserves the case a login
// was created with and echoes whatever case a mention was typed in — so
// "@Ana" in a comment and "ana" on an assignee list are one person, and a
// parser comparing them raw wakes her twice or not at all.
func (h hook) actor() string {
	if h.Event == EventWorkflowRun && h.WorkflowRun.Actor.Login != "" {
		// A workflow run names the person whose push triggered it, which
		// is not always the sender: a scheduled or re-run workflow is
		// sent by whoever pressed the button, and the run's own actor is
		// who it is about.
		return strings.ToLower(h.WorkflowRun.Actor.Login)
	}
	return strings.ToLower(h.Sender.Login)
}

// owner and repo are the two halves of the repository path, which every API
// call this integration makes is scoped by.
func (h hook) owner() string {
	if h.Repository.Owner.Login != "" {
		return h.Repository.Owner.Login
	}
	owner, _, _ := strings.Cut(h.Repository.FullName, "/")
	return owner
}

func (h hook) repo() string {
	if h.Repository.Name != "" {
		return h.Repository.Name
	}
	_, name, _ := strings.Cut(h.Repository.FullName, "/")
	return name
}

// logins reads a user array, lowercased, skipping the empty entries a
// removed collaborator leaves behind.
func logins(users []user) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if name := strings.ToLower(strings.TrimSpace(u.Login)); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// login is one user, lowercased, or empty.
func login(u user) string { return strings.ToLower(strings.TrimSpace(u.Login)) }

package queries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/iam"
)

// The previews of a message's links: a task, a page or an artifact of this
// dashboard's own, drawn as the record it names.
//
// # Each record is read by the question its own screen asks
//
// A link's record is asked through this same registry, under the caller's
// own context: `work_item` for a task, `page` for a page, `artifact` for an
// artifact. So the posture, the authority table's decision and the shape are
// those questions' own, and a record the caller may not open is not
// previewed, exactly as its screen would not open: a preview is never a way
// to read what a person could not read by following the link.
//
// # Only this dashboard's own addresses
//
// A link is taken for a record only when its origin is this deployment's own
// (`api.external_url`, [Sources.DashboardBase]) and its fragment is an
// address the link table builds ([cards.ScreenOf]). The same fragment under
// any other origin is somebody else's page, and a preview of the company's
// task drawn beside it would vouch for a link that does not go there.
// Everything else is drawn by the dashboard's link rules alone, so it is
// simply absent from the answer.

// PullRequestReader reads one pull request for a preview, through the
// company's own GitHub wiring, cached briefly by its implementation.
type PullRequestReader interface {
	PullRequestPreview(ctx context.Context, ref github.PullRef) (github.PullRequest, error)
}

// githubWeb is the one host a pull request's address is read on: github.com.
// An Enterprise Server's addresses are drawn by the link rules alone.
const githubWeb = "https://github.com"

// chatUnfurlKind is what a link points at.
type chatUnfurlKind string

const (
	unfurlTask     chatUnfurlKind = "task"
	unfurlPage     chatUnfurlKind = "page"
	unfurlArtifact chatUnfurlKind = "artifact"
	unfurlPull     chatUnfurlKind = "pull_request"
)

// chatUnfurl is one link resolved for its preview.
type chatUnfurl struct {
	URL  string         `json:"url" untrusted:"name"`
	Kind chatUnfurlKind `json:"kind"`

	// Href is where the preview opens, built by the link table from the
	// record, never echoed from the message.
	Href string `json:"href"`

	Code   string `json:"code,omitempty" untrusted:"name"`
	Title  string `json:"title" untrusted:"name"`
	Status string `json:"status,omitempty" untrusted:"name"`
	Tone   string `json:"tone,omitempty"`

	// Owner is a seat's handle: an assignee, an author, an owner.
	Owner string `json:"owner,omitempty"`

	Meta string `json:"meta,omitempty" untrusted:"name"`
}

// chatUnfurlsAnswer is the previews of one message's links.
type chatUnfurlsAnswer struct {
	Unfurls []chatUnfurl `json:"unfurls"`
}

// chatUnfurls answers the previews of the links a message carries (`urls`, at
// most [chatlet.MaxLinks]), each read as the caller.
func (s Sources) chatUnfurls(r *Registry) Answer {
	return func(ctx context.Context, p Params) (any, error) {
		if _, err := s.chatViewer(ctx, authz.ActionChatRead); err != nil {
			return nil, err
		}
		urls, err := unfurlURLs(p)
		if err != nil {
			return nil, err
		}
		base := ""
		if s.DashboardBase != nil {
			base = s.DashboardBase()
		}
		out := chatUnfurlsAnswer{Unfurls: []chatUnfurl{}}
		for _, raw := range urls {
			unfurl, ok, err := s.unfurl(ctx, r, base, raw)
			if err != nil {
				return nil, err
			}
			if ok {
				out.Unfurls = append(out.Unfurls, unfurl)
			}
		}
		return out, nil
	}
}

// unfurlURLs reads `urls`: a list of the message's own links, at most
// [chatlet.MaxLinks], since one read previews one message.
func unfurlURLs(p Params) ([]string, error) {
	list, ok := p.Values()["urls"].([]any)
	if !ok || len(list) == 0 {
		return nil, badParams("urls", "", nil)
	}
	if len(list) > chatlet.MaxLinks {
		return nil, fmt.Errorf("%w: `urls` names %d links and a message carries at "+
			"most %d; ask for one message's links", ErrBadParams, len(list),
			chatlet.MaxLinks)
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		link, isText := v.(string)
		if !isText || strings.TrimSpace(link) == "" {
			return nil, badParams("urls", fmt.Sprint(v), nil)
		}
		out = append(out, strings.TrimSpace(link))
	}
	return out, nil
}

// unfurl resolves one link, false when it is not one this read previews or
// its record is not one the caller may open. An error is the node's own: a
// record question that could not be answered is not a link without a record.
func (s Sources) unfurl(ctx context.Context, r *Registry, base, raw string) (
	chatUnfurl, bool, error) {

	if pull, ok := s.pullUnfurl(ctx, raw); ok {
		return pull, true, nil
	}
	screen, params, ok := ownAddress(base, raw)
	if !ok {
		return chatUnfurl{}, false, nil
	}
	record, previewed := unfurlRecords[screen]
	if !previewed {
		return chatUnfurl{}, false, nil
	}
	question, kind := record.question, record.kind
	answer, err := r.Answer(ctx, question, map[string]any{"id": params["id"]})
	switch {
	case err == nil:
	case unansweredRecord(err):
		return chatUnfurl{}, false, nil
	default:
		return chatUnfurl{}, false, err
	}
	body, err := json.Marshal(answer)
	if err != nil {
		return chatUnfurl{}, false, fmt.Errorf("queries: encode %s's answer for a "+
			"preview: %w", question, err)
	}
	unfurl, ok := previewOf(kind, body)
	if !ok {
		return chatUnfurl{}, false, nil
	}
	// THE LINK TABLE BUILDS WHAT IT PARSED: ownAddress read this screen and
	// these parameters off the same table, each by the grammar Link builds
	// from, so a refusal here is the table disagreeing with itself. That is
	// the node's own fault, said as one, never a link quietly left plain.
	link, err := cards.Link(screen, params, "")
	if err != nil {
		return chatUnfurl{}, false, fmt.Errorf("queries: build the %s address a "+
			"preview parsed: %w", screen, err)
	}
	unfurl.URL, unfurl.Kind, unfurl.Href = raw, kind, link.Href
	return unfurl, true, nil
}

// unfurlRecords is each record screen a link previews, by the question its
// own screen asks and the kind of preview it draws.
var unfurlRecords = map[string]struct {
	question string
	kind     chatUnfurlKind
}{
	"item":     {"work_item", unfurlTask},
	"page":     {"page", unfurlPage},
	"artifact": {"artifact", unfurlArtifact},
}

// ownAddress is the record screen a link names when it is one of this
// dashboard's own addresses: same scheme and host as base, and a fragment the
// link table builds.
func ownAddress(base, raw string) (string, map[string]string, bool) {
	if base == "" {
		return "", nil, false
	}
	own, err := url.Parse(base)
	if err != nil || own.Host == "" {
		return "", nil, false
	}
	link, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(link.Scheme, own.Scheme) ||
		!strings.EqualFold(link.Host, own.Host) || link.Fragment == "" {
		return "", nil, false
	}
	return cards.ScreenOf("#" + link.Fragment)
}

// unansweredRecord reports a record question refused or not found for this
// caller, or not asked on this node: a link drawn without a preview.
func unansweredRecord(err error) bool {
	for _, class := range []error{ErrNotFound, ErrUnauthorized, ErrUnknown, ErrBadParams} {
		if errors.Is(err, class) {
			return true
		}
	}
	return false
}

// previewOf reads a record question's answer, as the wire carries it, into a
// preview: the fields its own screen draws a card from.
func previewOf(kind chatUnfurlKind, raw []byte) (chatUnfurl, bool) {
	switch kind {
	case unfurlTask:
		var a struct {
			Task struct {
				Key         string `json:"key"`
				Title       string `json:"title"`
				Status      string `json:"status"`
				StatusGroup string `json:"status_group"`
				Assignee    string `json:"assignee"`
			} `json:"task"`
		}
		if json.Unmarshal(raw, &a) != nil || a.Task.Key == "" {
			return chatUnfurl{}, false
		}
		return chatUnfurl{Code: a.Task.Key, Title: a.Task.Title, Status: a.Task.Status,
			Tone: taskTone(a.Task.StatusGroup), Owner: a.Task.Assignee}, true
	case unfurlPage:
		var a struct {
			Page struct {
				Title  string `json:"title"`
				Status string `json:"status"`
				Author string `json:"author"`
			} `json:"page"`
		}
		if json.Unmarshal(raw, &a) != nil || a.Page.Title == "" {
			return chatUnfurl{}, false
		}
		return chatUnfurl{Title: a.Page.Title, Status: a.Page.Status,
			Owner: a.Page.Author}, true
	case unfurlArtifact:
		var a struct {
			Title     string `json:"title"`
			Kind      string `json:"kind"`
			Owner     string `json:"owner"`
			OwnerKind string `json:"owner_kind"`
		}
		if json.Unmarshal(raw, &a) != nil || a.Title == "" {
			return chatUnfurl{}, false
		}
		out := chatUnfurl{Title: a.Title, Meta: a.Kind}
		// A SEAT OWNS IT, agent or person: an operator's credential is
		// nobody the screen can name and draw.
		if a.OwnerKind == string(iam.ActorAgent) || a.OwnerKind == string(iam.ActorHuman) {
			out.Owner = a.Owner
		}
		return out, true
	}
	return chatUnfurl{}, false
}

// taskTone is a task's status group as the dot beside its status: done is
// success, under way is info, and anything else is neutral. Closed without
// being done is neutral too: it is over, and nothing went wrong to say so.
func taskTone(group string) string {
	switch group {
	case "done":
		return "success"
	case "active":
		return "info"
	}
	return "neutral"
}

// pullUnfurl previews a link to a pull request on github.com, under an owner
// the company's GitHub configuration names, read through its own wiring. False
// for any other link, and for a pull request that could not be read: a
// preview is never a reason to refuse the rest, and the link is drawn plain.
//
// ONLY WHAT THE LINK ALREADY POINTS AT, in the words anybody who opens it
// reads first: its repository and number, title, state, whether it is a
// draft, and its size. Never its body, its comments or its author's details,
// since the preview is read with the company's credential rather than the
// reader's and shown to everybody who can read the room.
func (s Sources) pullUnfurl(ctx context.Context, raw string) (chatUnfurl, bool) {
	if s.PullRequests == nil {
		return chatUnfurl{}, false
	}
	ref, ok := github.ParsePullURL(githubWeb, raw)
	if !ok {
		return chatUnfurl{}, false
	}
	web, owners, ok := s.companyGitHub()
	if !ok || browserBase(web) != githubWeb ||
		!slices.Contains(owners, strings.ToLower(ref.Owner)) {
		return chatUnfurl{}, false
	}
	pr, err := s.PullRequests.PullRequestPreview(ctx, ref)
	if err != nil {
		log.DebugContext(ctx, "chat_pull_preview_unread", "owner", ref.Owner,
			"repo", ref.Repo, "number", ref.Number, "error", err)
		return chatUnfurl{}, false
	}
	status, tone := pullState(pr)
	return chatUnfurl{
		URL: raw, Kind: unfurlPull, Href: ref.Canonical(githubWeb),
		Code:  fmt.Sprintf("%s/%s#%d", ref.Owner, ref.Repo, ref.Number),
		Title: pr.Title, Status: status, Tone: tone,
		Meta: fmt.Sprintf("+%d -%d", pr.Additions, pr.Deletions),
	}, true
}

// pullState is a pull request's state in the words its own page shows it in,
// and the tone of the dot beside it.
func pullState(pr github.PullRequest) (string, string) {
	switch {
	case pr.Merged:
		return "Merged", "info"
	case pr.State == "closed":
		return "Closed", "neutral"
	case pr.Draft:
		return "Draft", "neutral"
	}
	return "Open", "success"
}

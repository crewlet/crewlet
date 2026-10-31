package queries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
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

// chatUnfurlKind is what a link points at.
type chatUnfurlKind string

const (
	unfurlTask     chatUnfurlKind = "task"
	unfurlPage     chatUnfurlKind = "page"
	unfurlArtifact chatUnfurlKind = "artifact"
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

	screen, params, ok := ownAddress(base, raw)
	if !ok {
		return chatUnfurl{}, false, nil
	}
	question, kind := "", chatUnfurlKind("")
	switch screen {
	case "item":
		question, kind = "work_item", unfurlTask
	case "page":
		question, kind = "page", unfurlPage
	case "artifact":
		question, kind = "artifact", unfurlArtifact
	default:
		return chatUnfurl{}, false, nil
	}
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
	link, err := cards.Link(screen, params, "")
	if err != nil {
		return chatUnfurl{}, false, nil
	}
	unfurl.URL, unfurl.Kind, unfurl.Href = raw, kind, link.Href
	return unfurl, true, nil
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

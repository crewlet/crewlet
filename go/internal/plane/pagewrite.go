package plane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Publishing pages — the write half the import CLI is built on.
//
// # external_id is the idempotency contract
//
// Every page this tool publishes is stamped `external_source: "crewlet"`
// and an `external_id` derived from what it is: `skill:<key>` or
// `doc:<title>`. The fork enforces those two unique per project, so a
// re-import finds its own page by identity rather than by name — and
// retitling a page in Plane's UI never orphans it.
//
// The alternative, matching on the page name, breaks the first time
// somebody edits a heading: the run finds nothing, publishes a second page,
// and the project quietly accumulates one copy per edit.

// ExternalSource is the marker stamped on every page this tool publishes.
//
// The POSITIVE marker a prune keys on. Deleting by absence-from-the-tree
// alone would take an operator's own pages with it — a project holds notes
// beside the published ones, and nothing about a hand-written page says it
// is not managed except that this tool never claimed it.
const ExternalSource = "crewlet"

// The external-id prefixes, by what the page is.
const (
	SkillPrefix = "skill:"
	DocPrefix   = "doc:"
)

// SkillExternalID is the identity of a published tool-skill page.
func SkillExternalID(key string) string { return SkillPrefix + key }

// DocExternalID is the identity of a published knowledge doc.
func DocExternalID(title string) string { return DocPrefix + title }

// PageRef is a page as the import index needs it.
//
// NAME AND IDENTITY ONLY: the index answers "does this already exist and
// where", and fetching bodies for that would pull a project's entire
// content over the wire to decide a handful of upserts.
type PageRef struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ExternalID     string `json:"external_id"`
	ExternalSource string `json:"external_source"`
	ArchivedAt     string `json:"archived_at"`
}

// Managed reports a page this tool published.
func (p PageRef) Managed() bool { return p.ExternalSource == ExternalSource }

// indexFields is the projection the import enumeration asks for.
var indexFields = strings.Join(
	[]string{"id", "name", "external_id", "external_source", "archived_at"}, ",")

// PageIndex enumerates a project's pages by identity.
//
// ALL OR NOTHING, like every other enumeration a decision is made from: a
// truncated walk makes an existing page look absent, and the import then
// publishes a duplicate that the fork's uniqueness constraint refuses —
// leaving the operator with a failed import and no idea which page it meant.
func (c *Client) PageIndex(ctx context.Context, projectID string) ([]PageRef, error) {
	var (
		out    []PageRef
		cursor string
	)
	path := c.ws("/projects/" + url.PathEscape(projectID) + "/pages/")
	for {
		params := url.Values{
			"per_page": {strconv.Itoa(pageWalkSize)},
			"fields":   {indexFields},
		}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var payload json.RawMessage
		if err := c.get(ctx, path, params, &payload); err != nil {
			return nil, err
		}
		for _, row := range rows(payload) {
			out = append(out, PageRef{
				ID: str(row, "id"), Name: str(row, "name"),
				ExternalID:     str(row, "external_id"),
				ExternalSource: str(row, "external_source"),
				ArchivedAt:     str(row, "archived_at"),
			})
		}
		next := nextCursor(payload)
		if next == "" || next == cursor {
			return out, nil
		}
		if len(out) >= pageWalkCap {
			return nil, fmt.Errorf("plane: project %s has more than %d pages",
				projectID, pageWalkCap)
		}
		cursor = next
	}
}

// CreatePage publishes a new page.
func (c *Client) CreatePage(ctx context.Context, projectID, name, html, externalID string) (PageRef, error) {
	body := map[string]any{
		"name": name, "description_html": html,
		"external_id": externalID, "external_source": ExternalSource,
		// PUBLIC, because the whole point is that every seat can read it.
		// A page created private is one the knowledge search will never
		// return, silently, for every agent but its author.
		"access": 0,
	}
	var out PageRef
	path := c.ws("/projects/" + url.PathEscape(projectID) + "/pages/")
	if err := c.send(ctx, http.MethodPost, path, body, &out); err != nil {
		return PageRef{}, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return PageRef{}, fmt.Errorf("plane: publishing %q returned no page id", name)
	}
	return out, nil
}

// UpdatePage rewrites an existing page's name and body.
//
// THE EXTERNAL IDENTITY IS STAMPED HERE TOO, which is what adopts a page an
// operator created by hand under the same title: it becomes managed from
// this run on, and the next one finds it by id rather than by name.
func (c *Client) UpdatePage(ctx context.Context, projectID, pageID, name, html, externalID string) error {
	body := map[string]any{
		"name": name, "description_html": html,
		"external_id": externalID, "external_source": ExternalSource,
	}
	path := c.ws("/projects/" + url.PathEscape(projectID) +
		"/pages/" + url.PathEscape(pageID) + "/")
	return c.send(ctx, http.MethodPatch, path, body, nil)
}

// ArchivePage archives a page, which Plane requires before a delete.
func (c *Client) ArchivePage(ctx context.Context, projectID, pageID string) error {
	path := c.ws("/projects/" + url.PathEscape(projectID) +
		"/pages/" + url.PathEscape(pageID) + "/archive/")
	err := c.send(ctx, http.MethodPost, path, map[string]any{}, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// UnarchivePage restores a page.
//
// THE ROLLBACK for a prune whose delete was refused. Without it a failed
// prune leaves the page archived — invisible to every agent, and behind an
// external id that keeps 409ing every future import of the same skill. A
// failed prune has to be a no-op, not a half-removal.
func (c *Client) UnarchivePage(ctx context.Context, projectID, pageID string) error {
	path := c.ws("/projects/" + url.PathEscape(projectID) +
		"/pages/" + url.PathEscape(pageID) + "/unarchive/")
	err := c.send(ctx, http.MethodDelete, path, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// DeletePage removes an archived page.
func (c *Client) DeletePage(ctx context.Context, projectID, pageID string) error {
	path := c.ws("/projects/" + url.PathEscape(projectID) +
		"/pages/" + url.PathEscape(pageID) + "/")
	err := c.send(ctx, http.MethodDelete, path, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

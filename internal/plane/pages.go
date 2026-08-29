package plane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Walking a project's pages — the read the tool-skill sync is built on.
//
// # A cursor walk here, where the search deliberately does not
//
// The knowledge SEARCH takes one page of results because a consumer wants
// the top of a ranking. This is the opposite question: "what is in this
// container", asked once at boot, and an answer that stopped at a hundred
// would silently drop every skill past the hundredth — which nothing would
// report, because a short list and a complete short list look identical.

// pageWalkSize is how many pages one request asks for.
//
// The endpoint's own ceiling. Fewer requests for one bounded boot walk is
// strictly better here: nothing streams the result and nothing acts on a
// partial one.
const pageWalkSize = 100

// pageWalkCap bounds the whole walk.
//
// A container with more pages than this is not a tool-skills project — it is
// a project somebody pointed the sync at by mistake, and walking it would
// spend a hundred requests to build a catalogue no prompt could carry. The
// walk REPORTS hitting it rather than truncating quietly, because the caller
// must not seed a registry from a partial enumeration.
const pageWalkCap = 2000

// FullPage is a page with its body, as the skill sync needs it.
//
// Distinct from [Page], which is a SEARCH HIT: a hit carries the stripped
// text the ranking matched, while this carries the html the codec decodes a
// skill's frontmatter out of. Collapsing them would make a search result
// look loadable and a skill page look rankable, and neither is true.
type FullPage struct {
	ID          string
	Name        string
	ProjectID   string
	HTML        string
	Description string
}

// ListPages walks every page in a project.
//
// ALL OR NOTHING: an error means the caller does not know what is in the
// container, and a caller that seeded from a partial walk would silently
// delete every skill it did not reach.
func (c *Client) ListPages(ctx context.Context, projectID string) ([]FullPage, error) {
	if c == nil {
		return nil, fmt.Errorf("plane: no client")
	}
	var (
		out    []FullPage
		cursor string
	)
	path := "/workspaces/" + url.PathEscape(c.workspace) +
		"/projects/" + url.PathEscape(projectID) + "/pages/"
	for {
		params := url.Values{"per_page": {strconv.Itoa(pageWalkSize)}}
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var payload json.RawMessage
		if err := c.get(ctx, path, params, &payload); err != nil {
			return nil, err
		}
		for _, row := range rows(payload) {
			out = append(out, FullPage{
				ID: str(row, "id"), Name: str(row, "name"),
				ProjectID:   pageProjectID(row),
				HTML:        str(row, "description_html"),
				Description: str(row, "description_stripped"),
			})
		}
		next := nextCursor(payload)
		if next == "" || next == cursor {
			// No cursor, or one that did not advance. The second is a
			// server bug and a walker that trusted it would loop for
			// ever against a container it can never finish.
			return out, nil
		}
		if len(out) >= pageWalkCap {
			return nil, fmt.Errorf("plane: project %s has more than %d pages; "+
				"this is not a tool-skills project", projectID, pageWalkCap)
		}
		cursor = next
	}
}

// nextCursor reads the pagination cursor, tolerating the shapes the fork
// has used.
//
// Absent means DONE rather than an error: a single-page response has no
// cursor, and treating that as a failure would make every small container
// unwalkable.
func nextCursor(payload json.RawMessage) string {
	var envelope struct {
		NextCursor  string `json:"next_cursor"`
		NextPageRes string `json:"next_page_results"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	// next_page_results is a BOOLEAN on some builds and a string on
	// others; either way an explicit "false" ends the walk, and a cursor
	// with nothing after it would otherwise be followed once more for an
	// empty page.
	if strings.EqualFold(strings.TrimSpace(envelope.NextPageRes), "false") {
		return ""
	}
	return strings.TrimSpace(envelope.NextCursor)
}

// GetPage reads one page, for the webhook path.
//
// PROJECT-SCOPED, which is what makes it the admission check as well as the
// read: the fork's project-scoped page GET 404s for a page in another
// project, so a page that moved OUT of the skills container comes back
// missing and is evicted rather than continuing to serve its last body.
func (c *Client) GetPage(ctx context.Context, projectID, pageID string) (FullPage, error) {
	if c == nil {
		return FullPage{}, fmt.Errorf("plane: no client")
	}
	path := "/workspaces/" + url.PathEscape(c.workspace) +
		"/projects/" + url.PathEscape(projectID) + "/pages/" + url.PathEscape(pageID) + "/"
	var row map[string]any
	if err := c.get(ctx, path, nil, &row); err != nil {
		return FullPage{}, err
	}
	if archived := strings.TrimSpace(str(row, "archived_at")); archived != "" {
		// ARCHIVE IS THE OPERATOR'S REMOVE GESTURE — Plane requires it
		// before a delete — so an archived page must stop serving its
		// skill immediately rather than at whatever later moment somebody
		// completes the deletion.
		return FullPage{}, fmt.Errorf("plane: page %s is archived", pageID)
	}
	return FullPage{
		ID: str(row, "id"), Name: str(row, "name"),
		ProjectID:   pageProjectID(row),
		HTML:        str(row, "description_html"),
		Description: str(row, "description_stripped"),
	}, nil
}

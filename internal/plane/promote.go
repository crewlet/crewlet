package plane

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/knowledge"
)

// Plane's half of cross-agent skill promotion.
//
// # Dedup by EXTERNAL ID, not by title
//
// Plane pages are not unique by name within a project — two pages may share
// one — so a title lookup would find the wrong page as readily as the right
// one after a lead renames a draft while reviewing it. The external id is
// this engine's own identity for the row and survives a rename, which is
// exactly the property the check needs: the pass re-clusters the same skills
// every tick, and a draft a lead is halfway through editing must not be
// re-created beside itself.
//
// # No parent page
//
// Unlike Confluence, what hides a Plane draft from the knowledge search is
// the title PREFIX rather than an ancestor — see internal/plane/search.go and
// knowledge.AutoDraftTitlePrefix. So there is no parent to resolve and none
// to create, and the caller's prefix is load-bearing: a promotion drafted
// without it is reachable by every seat in the company, unreviewed.

// promotionExternalPrefix namespaces a promoted draft's external id.
//
// Its own prefix rather than the importer's, because the two write into the
// same project and an id collision would make an imported skill page and a
// promoted draft the same row — the second write silently adopting the first.
const promotionExternalPrefix = "draft:"

// PromotionWriter drafts promoted skills into a Plane project.
type PromotionWriter struct{ client *Client }

// NewPromotionWriter builds one over an org-token client.
func NewPromotionWriter(c *Client) *PromotionWriter {
	if c == nil {
		return nil
	}
	return &PromotionWriter{client: c}
}

// CreateDraft creates the draft, or returns the one already drafted for this
// name in this project.
func (w *PromotionWriter) CreateDraft(ctx context.Context, project, name, markdown string) (
	knowledge.DraftPage, bool, error,
) {
	project = strings.TrimSpace(project)
	if project == "" {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"plane: no project to draft %q into", name)
	}
	external := promotionExternalPrefix + name

	// THE INDEX FIRST, and it is an ALL-OR-NOTHING walk: a truncated one
	// makes an existing draft look absent, and the create that follows
	// either 409s or lands a second copy beside a page a lead is editing.
	index, err := w.client.PageIndex(ctx, project)
	if err != nil {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"plane: listing %s's pages before drafting %q: %w", project, name, err)
	}
	for _, page := range index {
		if page.ExternalID == external && page.Managed() {
			return knowledge.DraftPage{ID: page.ID, Title: page.Name}, false, nil
		}
	}

	html, err := knowledge.RenderMarkdown(markdown)
	if err != nil {
		return knowledge.DraftPage{}, false, fmt.Errorf("plane: %w", err)
	}
	page, err := w.client.CreatePage(ctx, project, name, html, external)
	if err != nil {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"plane: creating %q in %s: %w", name, project, err)
	}
	return knowledge.DraftPage{ID: page.ID, Title: name}, true, nil
}

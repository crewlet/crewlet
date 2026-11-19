package confluence

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/knowledge"
)

// Confluence's half of cross-agent skill promotion: create the draft page a
// unit lead reviews, or hand back the one already there.
//
// The pass re-clusters the same skills every tick, so without dedup one team
// yields a draft a day forever; a page title is unique within a space, so the
// check lives here, not in a ledger. The Auto-Drafted Skills parent hides a
// draft from every agent (the knowledge search excludes its subtree), so a
// missing parent is created and a promotion whose parent cannot be created is
// REFUSED, never filed at the space root where every seat could read it
// unreviewed.

// PromotionWriter drafts promoted skills into a Confluence space.
type PromotionWriter struct{ client *Client }

// NewPromotionWriter builds one over an org-token client.
func NewPromotionWriter(c *Client) *PromotionWriter {
	if c == nil {
		return nil
	}
	return &PromotionWriter{client: c}
}

// CreateDraft creates the draft under the space's auto-drafted parent, or
// returns the page already there under that title.
//
// The bool reports whether this call created it, so a pass that finds an
// existing draft stays quiet rather than re-announcing the same promotion
// every tick.
func (w *PromotionWriter) CreateDraft(ctx context.Context, space, name, markdown string) (
	knowledge.DraftPage, bool, error,
) {
	space = strings.ToUpper(strings.TrimSpace(space))
	if space == "" {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"confluence: no space to draft %q into", name)
	}

	// THE EXISTING PAGE FIRST. Creating and reading a 400 back would work
	// too, but only if every Confluence version answers a title clash the
	// same way — and it would burn a write attempt per tick per unit for
	// the whole life of a promotion that is already drafted.
	if page, found, err := w.client.PageByTitle(ctx, space, name); err != nil {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"confluence: looking for an existing %q in %s: %w", name, space, err)
	} else if found {
		return knowledge.DraftPage{ID: page.ID, Title: page.Title}, false, nil
	}

	parent, err := w.parent(ctx, space)
	if err != nil {
		return knowledge.DraftPage{}, false, err
	}
	storage, err := knowledge.RenderMarkdown(markdown)
	if err != nil {
		return knowledge.DraftPage{}, false, fmt.Errorf("confluence: %w", err)
	}
	page, err := w.client.CreatePage(ctx, space, name, storage, parent)
	if err != nil {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"confluence: creating %q in %s: %w", name, space, err)
	}
	return knowledge.DraftPage{ID: page.ID, Title: page.Title}, true, nil
}

// parent finds the space's Auto-Drafted Skills page, creating it if absent.
func (w *PromotionWriter) parent(ctx context.Context, space string) (string, error) {
	page, found, err := w.client.PageByTitle(ctx, space, knowledge.AutoDraftedParent)
	if err != nil {
		return "", fmt.Errorf("confluence: looking for %q in %s: %w",
			knowledge.AutoDraftedParent, space, err)
	}
	if found {
		return page.ID, nil
	}
	created, err := w.client.CreatePage(ctx, space, knowledge.AutoDraftedParent,
		autoDraftedParentBody, "")
	if err != nil {
		// REFUSED, not filed at the root. A draft outside this subtree is
		// one every agent's knowledge search can reach, unreviewed.
		return "", fmt.Errorf("confluence: creating %q in %s, which is what "+
			"keeps a draft hidden until a lead publishes it: %w",
			knowledge.AutoDraftedParent, space, err)
	}
	return created.ID, nil
}

// autoDraftedParentBody explains the parent to whoever finds it in the UI.
const autoDraftedParentBody = `<p>Pages under this one were drafted ` +
	`automatically from what several agents on this team independently ` +
	`learned. They are <strong>not reviewed</strong> and no agent can find ` +
	`them: the knowledge search excludes this subtree.</p>` +
	`<p>To adopt one, edit it and move it out of this parent. To reject one, ` +
	`delete it — it will be re-drafted only if the team converges on it again.</p>`

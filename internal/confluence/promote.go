package confluence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf16"

	"github.com/crewlet/crewlet/internal/knowledge"
)

// Confluence's half of cross-agent skill promotion: create the draft page a
// unit lead reviews, or hand back the one already there, and say whether a
// lead rejected it.
//
// # What dedups a draft, and what this writer adds
//
// The promotion pass keeps a fleet-wide record of every convergence it has
// drafted and reads it before asking a model, so a drafted procedure is not
// drafted again whatever the model would have called it (internal/learning's
// promote.go). What this writer adds is that a create is idempotent by TITLE,
// which a page title's uniqueness within a space gives it: a pass that made
// the page and could not record it makes it again from its record and finds
// it here, rather than making a second.
//
// # A lead rejects a draft by deleting it
//
// Deleting is what Confluence lets a person do to a page: it moves the page to
// the space's trash, and a lead who changes their mind restores it from there.
// The pass asks for the page by id, which Confluence serves at the status
// asked for, `current` unless told otherwise, so a page in the trash is not
// served and neither is one purged from it. A page not served in a space that
// is served has been deleted; a space that is not served proves nothing about
// the page in it, so that is an error rather than a rejection.
//
// DELETING THE DRAFTS PARENT REJECTS NOTHING. Confluence Cloud moves the
// children of a deleted page up to the nearest parent, and Data Center does
// the same unless "Also delete child pages" is chosen — which still moves up
// any child the deleting user cannot see. That takes every draft moved out of
// the subtree the knowledge search leaves out: each is then returned to every
// agent unreviewed, and the pass, finding each still served, reads it as
// published. So the parent's own page tells a lead not to, and to delete each
// draft instead.
//
// # What never clears
//
// [PromotionWriter.CheckDraft] refuses a title Confluence refuses by length
// before the pass records anything. After that, a create Confluence answers
// 400 or 404 to is asked about the SPACE: a space that is not served takes no
// page however often it is asked (RefusesContainer), and a 400 in a space
// that is served is a draft refused as sent (RefusesPage), or, for the drafts
// parent, the space refusing every draft (RefusesContainer) — unless a page
// now holds the title, which is somebody else's create landing between this
// writer's look and its own, handed back as found. Everything else is an
// outage, a permission somebody can grant, or a space that cannot be asked
// about, and the pass tries it again.
//
// # Why the parent is created rather than required
//
// The Auto-Drafted Skills parent is what hides a draft from every agent: the
// turn-start knowledge search excludes its subtree. A promotion that landed
// at the space root because the parent did not exist would be reachable by
// every seat in the company, unreviewed — which is the one outcome the whole
// review step exists to prevent. So a missing parent is created, and a
// promotion whose parent could not be created is REFUSED rather than filed
// somewhere visible.

// PromotionWriter drafts promoted skills into a Confluence space.
type PromotionWriter struct{ client *Client }

// Backend names this knowledge base as its searcher does.
func (w *PromotionWriter) Backend() string { return Backend }

// Rejection is how a lead rejects a draft in Confluence.
func (w *PromotionWriter) Rejection() string { return "delete it" }

// Rejected reports whether a lead deleted a draft, reading the page by id.
//
// Present anywhere — under the drafts parent or moved out of it — the draft
// stands. Absent from a space that still answers, it was deleted. Anything
// else is an error, never a rejection.
func (w *PromotionWriter) Rejected(ctx context.Context, space, pageID string) (string, error) {
	if strings.TrimSpace(pageID) == "" {
		return "", fmt.Errorf("confluence: no page id to look for a rejected draft by")
	}
	_, err := w.client.PageByID(ctx, pageID)
	if err == nil {
		return "", nil
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return "", fmt.Errorf("confluence: reading the draft %s: %w", pageID, err)
	}
	// NOT SERVED. Confluence answers a page that is not there and one this
	// credential may not view alike, and it answers so for every page of a
	// space that is gone or unreadable — which says nothing about what a
	// lead did. The space is asked before the page is called deleted.
	readable, err := w.client.SpaceExists(ctx, space)
	switch {
	case err != nil:
		return "", fmt.Errorf("confluence: the draft %s is not served, and whether "+
			"its space %s is could not be read: %w", pageID, space, err)
	case !readable:
		return "", fmt.Errorf("confluence: the draft %s is not served, and neither "+
			"is its space %s, so its absence says nothing about a lead's "+
			"decision", pageID, space)
	}
	return "it was deleted", nil
}

// maxTitleUnits is the longest page title Confluence takes: 255 characters,
// the limit its own refusal names ("Title cannot be longer than 255
// characters"). Counted here in UTF-16 code units, which are never fewer than
// the characters a server could count, so a title this accepts fits however
// the server counts it.
const maxTitleUnits = 255

// CheckDraft reports why Confluence would refuse a draft's title however
// often it was asked. The body is not checked: this writer knows no limit on
// one that a draft could reach.
func (w *PromotionWriter) CheckDraft(title, _ string) error {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return fmt.Errorf("confluence: a draft needs a title")
	}
	if units := len(utf16.Encode([]rune(trimmed))); units > maxTitleUnits {
		return fmt.Errorf("confluence: the title %q is %d characters, past the "+
			"%d a page title may have", trimmed, units, maxTitleUnits)
	}
	return nil
}

// refusedContainer is a create refused in a space Confluence does not serve
// to this credential: it does not exist, or the token cannot see it.
type refusedContainer struct {
	space string
	err   error
}

func (e *refusedContainer) Error() string {
	return fmt.Sprintf("confluence: the space %s is not served to the org "+
		"token — it does not exist, or the token cannot see it: %v", e.space, e.err)
}

func (e *refusedContainer) Unwrap() error { return e.err }

// RefusesContainer says the space will take no page until it is served.
func (*refusedContainer) RefusesContainer() bool { return true }

// refusedPage is a create Confluence answered 400 to in a space it serves:
// the page itself, as sent, is what it refused.
type refusedPage struct{ err error }

func (e *refusedPage) Error() string { return e.err.Error() }

func (e *refusedPage) Unwrap() error { return e.err }

// RefusesPage says the same page will be refused again.
func (*refusedPage) RefusesPage() bool { return true }

// refusal classifies a create Confluence refused in a space. See "What never
// clears" above.
//
// perDraft says whether the refused page was the draft itself. The drafts
// parent's title and body are this writer's own constants, the same for every
// draft in the space, so a parent refused as sent in a served space is the
// SPACE refusing every draft rather than this draft being wrong: retiring the
// record would pay the model for an answer refused in the same place.
func (w *PromotionWriter) refusal(ctx context.Context, space string, err error, perDraft bool) error {
	var apiErr *APIError
	if !errors.As(err, &apiErr) ||
		(apiErr.Status != http.StatusBadRequest && apiErr.Status != http.StatusNotFound) {
		return err
	}
	served, spaceErr := w.client.SpaceExists(ctx, space)
	switch {
	case spaceErr != nil:
		return err
	case !served:
		return &refusedContainer{space: space, err: err}
	case apiErr.Status == http.StatusBadRequest && perDraft:
		return &refusedPage{err: err}
	case apiErr.Status == http.StatusBadRequest:
		return &refusedContainer{space: space, err: err}
	}
	return err
}

// heldAfterRefusal is the page holding a title after Confluence answered 400
// to a create of it: somebody else's create landing between this writer's
// look and its own, which is a title taken rather than a page refused.
func (w *PromotionWriter) heldAfterRefusal(ctx context.Context, space, title string,
	err error) (Page, bool) {

	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		return Page{}, false
	}
	held, found, findErr := w.client.PageByTitle(ctx, space, title)
	return held, findErr == nil && found
}

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
		// A TITLE TAKEN BETWEEN THE LOOK AND THE CREATE is handed back as
		// found, which is the pass's to judge, rather than read as a page
		// refused for good.
		if held, found := w.heldAfterRefusal(ctx, space, name, err); found {
			return knowledge.DraftPage{ID: held.ID, Title: held.Title}, false, nil
		}
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"confluence: creating %q in %s: %w", name, space, w.refusal(ctx, space, err, true))
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
		if held, found := w.heldAfterRefusal(ctx, space, knowledge.AutoDraftedParent, err); found {
			return held.ID, nil
		}
		// REFUSED, not filed at the root. A draft outside this subtree is
		// one every agent's knowledge search can reach, unreviewed.
		return "", fmt.Errorf("confluence: creating %q in %s, which is what "+
			"keeps a draft hidden until a lead publishes it: %w",
			knowledge.AutoDraftedParent, space, w.refusal(ctx, space, err, false))
	}
	return created.ID, nil
}

// autoDraftedParentBody explains the parent to whoever finds it in the UI.
const autoDraftedParentBody = `<p>Pages under this one were drafted ` +
	`automatically from what several agents on this team independently ` +
	`learned. They are <strong>not reviewed</strong> and no agent can find ` +
	`them: the knowledge search excludes this subtree.</p>` +
	`<p>To adopt one, move it out of this parent. To reject one, delete it: the ` +
	`promotion pass records the rejection for the whole company. Either way, ` +
	`and if you leave it here, the pass does not draft that procedure for this ` +
	`team in this knowledge base again.</p>` +
	`<p><strong>Do not delete this page to reject what is under it.</strong> ` +
	`Unless they are deleted with it, Confluence moves the pages under a ` +
	`deleted page up a level, which takes each draft out of this subtree: ` +
	`every agent then finds it, unreviewed, and it is not recorded as ` +
	`rejected. Delete each draft instead.</p>`

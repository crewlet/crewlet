package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The engine's own knowledge base's half of cross-agent skill promotion:
// create the draft page a unit lead reviews, or hand back the one already
// there.
//
// # The same conventions as the Confluence writer
//
// A draft goes under the container's [knowledge.AutoDraftedParent] page and
// its title carries [knowledge.AutoDraftTitlePrefix], which the pass stamps.
// Both are what the knowledge search judges a hit by ([knowledge.Excludes]),
// through the one function both backends share, so a draft here is hidden
// from every seat's search for exactly the reason a Confluence draft is — its
// parent chain names the auto-drafted page — and published by the same
// gesture: moving it out from under that page.
//
// # Why the dedup is a read and then a create
//
// A title is an address in this knowledge base: unique per container, and
// what a create ARBITRATES ON at the broker. So a second page at a draft's
// address is impossible whatever this node has applied, and the read first
// only saves the round trip. The read is at `stale` — this node's own rows,
// no broker call — because a miss there is settled by the create itself: a
// title somebody else holds is refused as taken, and only then is the winner
// read, through the log's barrier.
//
// # Why the parent is created rather than required, and never skipped
//
// The parent is what hides a draft from every agent's search. A draft filed at
// the top of its container because the parent could not be made would be
// found by every seat in the company, unreviewed — the one outcome the review
// step exists to prevent — so a parent that cannot be created REFUSES the
// draft.

// nativeDrafts writes promotion drafts into the engine's own pages.
//
// Built by [Engine.startNative] over the node's own write path and read side,
// in the step that builds the searcher that hides what it writes.
type nativeDrafts struct {
	store  *pages.Store
	reader *pages.Reader
}

// draftAuthor is who a native draft and its parent are written as: the engine
// itself, under the name the engine's own writes to this knowledge base carry
// — [pages.Store.EnsureContainer] writes a container as it.
//
// NOT A SEAT, because no seat wrote the draft: several converged on it, and
// the page names them. This knowledge base's author kinds are agent, human and
// operator, and a write no seat made is recorded as `operator` there.
var draftAuthor = pages.Actor{Handle: "system", Kind: pages.AuthorOperator}

// CreateDraft creates the draft under the container's auto-drafted parent, or
// returns the page already there under that title.
//
// The bool reports whether this call created it, so a pass that finds an
// existing draft stays quiet rather than re-announcing the same promotion
// every tick.
func (w *nativeDrafts) CreateDraft(ctx context.Context, container, name, markdown string) (
	knowledge.DraftPage, bool, error,
) {
	container = strings.ToUpper(strings.TrimSpace(container))
	if container == "" {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"pages: no container to draft %q into", name)
	}

	if page, found, err := w.find(ctx, container, name, ownRows()); err != nil {
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"pages: looking for an existing %q in %s: %w", name, container, err)
	} else if found {
		return draftOf(page), false, nil
	}

	parent, err := w.parent(ctx, container)
	if err != nil {
		return knowledge.DraftPage{}, false, err
	}
	// NOT QUIET. A page created in a team's container with nobody named on
	// it reaches that container's lead ([pages.LeadWorthy]), which is who
	// reviews a draft; the author is the page's only watcher and is never
	// woken by its own write.
	written, err := w.store.Create(ctx, draftAuthor, pages.NewPage{
		Container: container, Title: name, Body: markdown, ParentID: parent,
		Message: "Drafted from a procedure several of this team's seats " +
			"arrived at independently.",
	})
	switch {
	case errors.Is(err, pages.ErrTitleTaken):
		// A WRITE THIS NODE HAD NOT APPLIED WHEN IT LOOKED HOLDS THE
		// ADDRESS: another node that ran the pass, or a page somebody
		// wrote under this title. Either way a page holds the draft's
		// address, and nobody here made it.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		page, err := w.winner(ctx, container, name)
		if err != nil {
			return knowledge.DraftPage{}, false, err
		}
		return draftOf(page), false, nil
	case err != nil:
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"pages: creating %q in %s: %w", name, container, err)
	}
	if written.Outcome.Outcome != statelog.OutcomeUnknown {
		// APPLIED OR PENDING, and both are durable at a position: every
		// node applies it.
		return knowledge.DraftPage{ID: written.Page.ID, Title: written.Page.Title},
			true, nil
	}
	// UNKNOWN: nothing can be said about the record from here, so the log
	// is asked. A page under this title with this write's id is the write;
	// one with another id is somebody else's; none is a write that did not
	// land, and the next pass makes it again.
	page, found, err := w.find(ctx, container, name, throughBarrier())
	switch {
	case err != nil:
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"pages: creating %q in %s went unanswered, and reading it back "+
				"failed: %w", name, container, err)
	case !found:
		return knowledge.DraftPage{}, false, fmt.Errorf(
			"pages: creating %q in %s went unanswered, and the log holds no "+
				"such page; the next pass drafts it again", name, container)
	}
	return draftOf(page), page.ID == written.Page.ID, nil
}

// parent finds the container's auto-drafted parent, creating it if absent,
// and answers with a page THIS NODE HOLDS: a create names its parent by a page
// its own applied rows must already have.
func (w *nativeDrafts) parent(ctx context.Context, container string) (string, error) {
	title := knowledge.AutoDraftedParent
	if page, found, err := w.find(ctx, container, title, ownRows()); err != nil {
		return "", fmt.Errorf("pages: looking for %q in %s: %w", title, container, err)
	} else if found {
		return page.ID, nil
	}
	// QUIET. It is scaffolding a lead has no decision to make about; the
	// draft filed under it is what reaches them.
	written, err := w.store.Create(ctx, draftAuthor, pages.NewPage{
		Container: container, Title: title, Body: draftParentBody,
		Message: "Holds the drafts skill promotion writes until a lead reviews them.",
		Quiet:   true,
	})
	switch {
	case errors.Is(err, pages.ErrTitleTaken):
		// THE WINNER IS READ THROUGH THE BARRIER, which is also what puts
		// it in this node's rows for the draft's create to name.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		page, err := w.winner(ctx, container, title)
		if err != nil {
			return "", err
		}
		return page.ID, nil
	case err != nil:
		// REFUSED, not filed at the top of the container. A draft outside
		// this page's subtree is one every agent's search can return.
		return "", fmt.Errorf("pages: creating %q in %s, which is what keeps "+
			"a draft hidden until a lead publishes it: %w", title, container, err)
	}
	page, found, err := w.find(ctx, container, title, afterWrite(written.Outcome))
	switch {
	case err != nil:
		return "", fmt.Errorf("pages: reading back %q in %s, which a draft "+
			"can only be filed under once this node holds it: %w",
			title, container, err)
	case !found:
		return "", fmt.Errorf("pages: %q in %s is not in this node's rows "+
			"after its create; the next pass looks for it again",
			title, container)
	}
	return page.ID, nil
}

// winner reads the page that holds an address this node's create lost, through
// the log's barrier: the broker has it and this node may not have applied it.
func (w *nativeDrafts) winner(ctx context.Context, container, title string) (pages.Page, error) {
	page, found, err := w.find(ctx, container, title, throughBarrier())
	switch {
	case err != nil:
		return pages.Page{}, fmt.Errorf("pages: %q in %s is taken, and reading "+
			"the page that holds it failed: %w", title, container, err)
	case !found:
		return pages.Page{}, fmt.Errorf("pages: %q in %s is refused as taken "+
			"and no page answers to it after the log's barrier",
			title, container)
	}
	return page, nil
}

// find reads the page at an address, and whether there is one.
func (w *nativeDrafts) find(ctx context.Context, container, title string,
	fresh statelog.Freshness) (pages.Page, bool, error) {

	detail, err := w.reader.Get(ctx, container+"/"+title, fresh)
	switch {
	case errors.Is(err, pages.ErrNotFound):
		return pages.Page{}, false, nil
	case err != nil:
		return pages.Page{}, false, err
	}
	return detail.Page, true, nil
}

// ownRows is a read of this node's own applied rows, with no broker call.
func ownRows() statelog.Freshness { return statelog.Freshness{Level: statelog.ReadStale} }

// throughBarrier is a read that includes everything the log had committed when
// it arrived.
func throughBarrier() statelog.Freshness {
	return statelog.Freshness{Level: statelog.ReadLinearizable}
}

// afterWrite is the read that includes a write this node just made: a session
// read at the write's own position when it answered with one, and through the
// barrier when it answered unknown and has none.
func afterWrite(result statelog.Result) statelog.Freshness {
	if result.Position.Seq == 0 {
		return throughBarrier()
	}
	return statelog.Freshness{Level: statelog.ReadSession, MinPosition: result.Position}
}

func draftOf(page pages.Page) knowledge.DraftPage {
	return knowledge.DraftPage{ID: page.ID, Title: page.Title}
}

// draftParentBody explains the parent to whoever opens it.
const draftParentBody = "Pages under this one were drafted automatically " +
	"from a procedure several agents on this team arrived at independently. " +
	"They are **not reviewed**, and no agent's knowledge search returns them: " +
	"the search leaves out every page beneath this one.\n\n" +
	"To adopt one, move it out from under this page — to the top of this " +
	"container, or under any other page — and it is an ordinary page every " +
	"agent's search can return. A draft left here stays hidden, and while it " +
	"holds its title the same draft is not written again."

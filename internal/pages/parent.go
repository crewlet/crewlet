package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// A PAGE'S PARENT, and the two places it is judged.
//
// A parent is a pointer to another page's row, and the knowledge base is a
// TREE only while every pointer names a page that is there, in the same
// container, above the page rather than beneath it. A pointer that breaks any
// of that hides the page: a tree walk from the container's top never reaches a
// page filed under a page that does not exist, and two pages filed under each
// other are reachable from nowhere at all — with nothing on either row to say
// so, and no duty that would ever look.
//
// # The write authority refuses, inside its own snapshot
//
// [checkParent] runs in the decide, against the rows the decision is made
// from, and refuses every one of the five with a remedy — so a caller that
// names a page it mistyped, one in another space, or one it is about to file
// its own ancestor under is told so and changes nothing.
//
// # The applier salvages, because two writers can still make one
//
// The decide sees one snapshot and the broker arbitrates each page on its own
// subject, so two moves that are each sound — A under B on one node, B under
// A on another — are both accepted, and so is a move under a page whose purge
// lands between the decision and the apply. The tracker's applier states the
// rule for a shape two concurrent writers can legally create: it APPLIES the
// record rather than refusing it, because a refusal inside the apply
// transaction stops that node's whole log, on every node, over one page.
//
// What applying MEANS here is what keeps the tree a tree: [parentFault] is
// evaluated again at the apply, against the rows at that position — which are
// identical on every node, so every node reaches the same answer — and a
// parent the page cannot hold is not written. A CREATE files the page at its
// container's top; a MOVE leaves the page under the parent it already had,
// which was sound when its own record applied. Everything else the record
// carries applies. The tracker raises an attention flag instead and lets a
// repair duty write the fix; the knowledge base has neither the flag nor the
// duty, and a page reachable from its container's top is one a person can see
// and move, which a page in a cycle is not.
//
// A TRASHED parent is the one fault the applier accepts: the trash is
// reversible, and a page under a trashed page is the state trashing a page
// with children already leaves. Only the decide refuses to file a page there,
// because a caller doing it deliberately is filing it somewhere nobody looks.

// parentFault is why a page cannot be filed under a parent.
type parentFault string

const (
	parentSound     parentFault = ""
	parentMissing   parentFault = "missing"
	parentPurged    parentFault = "purged"
	parentElsewhere parentFault = "another_container"
	parentTrashed   parentFault = "trashed"
	parentSelf      parentFault = "itself"
	parentBeneath   parentFault = "beneath"
)

// salvaged reports a fault the applier does not write through: every one but
// a trashed parent, which is a reversible state rather than a broken pointer.
func (f parentFault) salvaged() bool { return f != parentSound && f != parentTrashed }

// judgeParent is the one judgement both halves make, over the rows tx sees:
// whether page pageID, living in container, can be filed under parentID — and
// the parent's head when it exists, for a refusal to name.
//
// An empty parent is the container's top and always sound.
func judgeParent(ctx context.Context, tx *sql.Tx, pageID, container,
	parentID string) (parentFault, Page, error) {

	if parentID == "" {
		return parentSound, Page{}, nil
	}
	if parentID == pageID {
		return parentSelf, Page{}, nil
	}
	parent, held, err := readHead(ctx, tx, parentID)
	if err != nil {
		return "", Page{}, err
	}
	if !held {
		purged, err := pageIsPurged(ctx, tx, parentID)
		if err != nil {
			return "", Page{}, err
		}
		if purged {
			return parentPurged, Page{}, nil
		}
		return parentMissing, Page{}, nil
	}
	if parent.Container != container {
		return parentElsewhere, parent, nil
	}
	// THE WALK UP FROM THE PARENT, looking for the page itself. A visited
	// set rather than a depth bound, for the tracker's reason: a chain that
	// already loops somewhere above — the residue of an older build, since
	// nothing this one applies can leave one — terminates here instead of
	// walking the loop until a bound, and that loop is not this write's to
	// refuse.
	seen := map[string]bool{parentID: true}
	for at := parent.ParentID; at != "" && !seen[at]; {
		if at == pageID {
			return parentBeneath, parent, nil
		}
		seen[at] = true
		var next string
		err := tx.QueryRowContext(ctx,
			`SELECT parent_id FROM pages_heads WHERE id = ?`, at).Scan(&next)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			next = ""
		case err != nil:
			return "", Page{}, fmt.Errorf("pages: walk the parents of %s: %w", parentID, err)
		}
		at = next
	}
	if parent.Status == StatusTrashed {
		return parentTrashed, parent, nil
	}
	return parentSound, parent, nil
}

// checkParent is the write authority's refusal of a parent, with the remedy
// for each fault — see the file's doc for why the decide refuses what the
// applier salvages.
func checkParent(ctx context.Context, tx *sql.Tx, pageID, container,
	parentID string) error {

	fault, parent, err := judgeParent(ctx, tx, pageID, container, parentID)
	if err != nil {
		return err
	}
	switch fault {
	case parentSound:
		return nil
	case parentSelf:
		return badParent("a page cannot be its own parent — leave parent_id " +
			"empty to file it at its container's top")
	case parentMissing:
		return badParent("there is no page %s — name the id of a page in "+
			"container %s (list_pages finds one), or leave parent_id empty to "+
			"file this page at the container's top", parentID, container)
	case parentPurged:
		return badParent("page %s was purged and is gone for good — name "+
			"another page in container %s, or leave parent_id empty to file "+
			"this page at the container's top", parentID, container)
	case parentElsewhere:
		return badParent("page %s (%q) is in container %s, and a page's parent "+
			"must be in its own container, %s — name a page there, or leave "+
			"parent_id empty", parentID, parent.Title, parent.Container, container)
	case parentTrashed:
		return badParent("page %s (%q) is in the trash — restore it first, or "+
			"name another parent", parentID, parent.Title)
	case parentBeneath:
		return badParent("page %s (%q) is beneath this page, so filing this "+
			"page under it would make each the other's ancestor — move %q "+
			"somewhere else first, or choose another parent",
			parentID, parent.Title, parent.Title)
	}
	return badParent("page %s cannot hold this page (%s)", parentID, fault)
}

// salvageParent is the parent the applier writes for a record whose recorded
// parent is want and whose page currently sits under have: want when the page
// can be filed there at this position, have otherwise.
//
// have is the container's top for a create, and the page's own current parent
// for a move — see the file's doc.
func salvageParent(ctx context.Context, tx *sql.Tx, at applyContext, pageID,
	container, want, have string) (string, error) {

	fault, _, err := judgeParent(ctx, tx, pageID, container, want)
	if err != nil {
		return "", err
	}
	if !fault.salvaged() {
		return want, nil
	}
	log.WarnContext(ctx, "pages_parent_salvaged",
		slog.String("page", pageID), slog.String("recorded_parent", want),
		slog.String("fault", string(fault)), slog.String("kept_parent", have),
		slog.String("position", at.position.String()),
		slog.String("detail", "the record names a parent this page cannot be "+
			"filed under at this position — two moves that each made the other "+
			"an ancestor, or a parent purged after the move was decided — so "+
			"every node files it where it was, and the rest of the record "+
			"applies"))
	return have, nil
}

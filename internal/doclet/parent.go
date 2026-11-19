package doclet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// A PAGE'S PARENT, and the two places it is judged.
//
// The knowledge base is a TREE only while every parent pointer names a page
// that is there, in the same container, above the page rather than beneath it;
// a broken one hides the page from every tree walk, with nothing on the row to
// say so and no duty that would look. [checkParent] runs in the decide, against
// its snapshot, and refuses each fault with a remedy. The applier salvages
// instead (see [parentFault]).

// parentFault is why a page cannot be filed under a parent.
//
// The applier APPLIES a record whose parent faults rather than refusing it: two
// sound moves on two nodes (A under B, B under A) or a purge landing between
// decide and apply can both be accepted, and a refusal in the apply transaction
// stops that node's whole log. It re-evaluates this against the rows at its
// position (identical on every node): a CREATE files the page at its container's
// top, a MOVE leaves it under its old parent, the rest applies. No flag or
// repair duty exists; a page reachable from the top can be seen and moved, one in
// a cycle cannot. A TRASHED parent is accepted (trashing already leaves children
// under it); only the decide refuses to file a page there.
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
	// THE WALK UP FROM THE PARENT, looking for the page itself, to the top.
	// It ends because the tree is acyclic, by induction over the strict log:
	// every write of a parent keeps it so — a create and a move are judged
	// here at their own position, on rows identical on every data node, so the
	// second of two crossing moves finds the page above its new parent and
	// keeps the old one ([salvageParent]); a purge re-parents its children
	// onto its own parent, which in an acyclic tree is never beneath them;
	// and a patch writes back the parent its own transaction read.
	for at := parent.ParentID; at != ""; {
		if at == pageID {
			return parentBeneath, parent, nil
		}
		var next string
		err := tx.QueryRowContext(ctx, parentOfStatement, at).Scan(&next)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			next = ""
		case err != nil:
			return "", Page{}, fmt.Errorf("doclet: walk the parents of %s: %w", parentID, err)
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
		return badParent("a page cannot be its own parent. Leave parent_id " +
			"empty to file it at its container's top")
	case parentMissing:
		return badParent("there is no page %s. Name the id of a page in "+
			"container %s ("+ListPagesTool+" finds one), or leave parent_id empty to "+
			"file this page at the container's top", parentID, container)
	case parentPurged:
		return badParent("page %s was purged and is gone for good. Name "+
			"another page in container %s, or leave parent_id empty to file "+
			"this page at the container's top", parentID, container)
	case parentElsewhere:
		return badParent("page %s (%q) is in container %s, and a page's parent "+
			"must be in its own container, %s. Name a page there, or leave "+
			"parent_id empty", parentID, parent.Title, parent.Container, container)
	case parentTrashed:
		return badParent("page %s (%q) is in the trash. Restore it first, or "+
			"name another parent", parentID, parent.Title)
	case parentBeneath:
		return badParent("page %s (%q) is beneath this page, so filing this "+
			"page under it would make each the other's ancestor. Move %q "+
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

// parentOfStatement is one step of a parent walk: a page's parent, by its key,
// named for the plan gate.
const parentOfStatement = `SELECT parent_id FROM pages_heads WHERE id = ?`

package pages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// NewComment is a remark to add to a page.
type NewComment struct {
	Body     string
	Mentions []string
	ReplyTo  string

	// TurnKey makes a comment made from a turn idempotent, on [work]'s
	// terms: a re-run turn posts once.
	TurnKey string

	Quiet bool
}

// Comment adds a remark to a page.
//
// # The watcher asymmetry, and why it is kept
//
// An EDIT subscribes its author and a MENTION subscribes its target, but a
// COMMENT DOES NOT subscribe its commenter — which is the opposite of the
// tracker's participants rule, and deliberate rather than an oversight.
//
// A tracker item is a piece of work with an owner, and everyone who says
// anything about it has a stake in how it ends. A wiki page is a document:
// people comment on one to point out a typo or ask a question, and
// subscribing each of them means a page a hundred people have remarked on
// wakes a hundred seats every time somebody fixes a heading. The Confluence
// integration drew that line and it has held; the founder's participants
// decision was about the TRACKER, where the stake is real.
//
// Somebody who wants the page follows it explicitly, and a mention still
// reaches a muted person because it is directed.
func (s *Store) Comment(ctx context.Context, actor Actor, pageID string, in NewComment) (Comment, Written, error) {
	if err := actor.validate(); err != nil {
		return Comment{}, Written{}, err
	}
	body := strings.TrimSpace(in.Body)
	switch {
	case body == "":
		return Comment{}, Written{}, invalid("body", "a comment needs something in it")
	case len(body) > MaxComment:
		return Comment{}, Written{}, invalid("body",
			"%d bytes, past the %d-byte cap — a comment is refused rather than "+
				"cut, because half a remark reads as a different remark",
			len(body), MaxComment)
	}

	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, PageKey(pageID))
	if err != nil {
		return Comment{}, Written{}, fmt.Errorf("pages: read %s: %w", pageID, err)
	}
	if !found {
		return Comment{}, Written{}, fmt.Errorf("%w: page %s", ErrNotFound, pageID)
	}
	page, err := DecodePage(rec.Value)
	if err != nil {
		return Comment{}, Written{}, err
	}

	at := s.now()
	comment := Comment{
		V: DocumentVersion, ID: s.commentID(page.ID, in), PageID: page.ID,
		Author: actor.Name(), AuthorKind: actor.Kind, Body: body,
		Mentions: cleanList(in.Mentions), ReplyTo: in.ReplyTo,
		CreatedAt: at, UpdatedAt: at,
	}
	change := s.change(actor, page, ChangeComment, at)
	change.CommentID = comment.ID
	change.Excerpt = excerpt(body)
	change.Mentions = comment.Mentions
	change.Quiet = in.Quiet
	comment.LastChange = &change

	data, err := EncodeComment(comment)
	if err != nil {
		return Comment{}, Written{}, err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyPages,
		CommentKey(page.ID, comment.ID), data)
	if err != nil {
		return Comment{}, Written{}, fmt.Errorf("pages: comment on %q: %w", page.Title, err)
	}
	if !created {
		// The deterministic id collided: this exact remark from this exact
		// turn is already there. Returning the existing one is the point.
		existing, err := s.readComment(ctx, page.ID, comment.ID)
		if err != nil {
			return Comment{}, Written{}, err
		}
		return existing, Written{Page: page, Revision: rec.Version}, nil
	}

	// A MENTION SUBSCRIBES ITS TARGET even when muted: a mute says "stop
	// telling me about this page", and somebody typing a handle is telling
	// THAT PERSON specifically. Nothing else about a comment changes the
	// head, which is why a page with no mentions takes no head write at all
	// — a comment on a busy page must not contend with every other comment.
	if len(comment.Mentions) == 0 {
		change.HeadRevision = rec.Version
		if err := s.writeChange(ctx, change); err != nil {
			return comment, Written{}, err
		}
		return comment, Written{Page: page, Revision: rec.Version, ChangeID: change.ID}, nil
	}

	written, err := s.subscribeMentions(ctx, actor, page.ID, comment, change)
	if err != nil {
		return comment, Written{}, err
	}
	return comment, written, nil
}

// subscribeMentions folds a comment's mentions into the page's watchers.
func (s *Store) subscribeMentions(ctx context.Context, actor Actor, pageID string,
	comment Comment, change Change) (Written, error) {
	key := PageKey(pageID)
	for range casRounds {
		rec, found, err := s.docs.Document(ctx, coord.FamilyPages, key)
		if err != nil {
			return Written{}, fmt.Errorf("pages: read %s: %w", pageID, err)
		}
		if !found {
			return Written{}, fmt.Errorf("%w: page %s", ErrNotFound, pageID)
		}
		page, err := DecodePage(rec.Value)
		if err != nil {
			return Written{}, err
		}
		before := len(page.Watchers)
		for _, handle := range comment.Mentions {
			mention(&page, handle)
		}
		if len(page.Watchers) == before {
			// Everyone named already follows it. No head write, so a busy
			// page's comments do not contend.
			change.HeadRevision = rec.Version
			change.Snapshot = snapshotOf(page)
			if err := s.writeChange(ctx, change); err != nil {
				return Written{}, err
			}
			return Written{Page: page, Revision: rec.Version, ChangeID: change.ID}, nil
		}
		page.UpdatedAt = change.CreatedAt
		change.Snapshot = snapshotOf(page)
		page.LastChange = &change

		data, err := EncodePage(page)
		if err != nil {
			return Written{}, err
		}
		ok, err := s.docs.UpdateDocument(ctx, coord.FamilyPages, key, data, rec.Version)
		if err != nil {
			return Written{}, fmt.Errorf("pages: write %s: %w", pageID, err)
		}
		if !ok {
			continue
		}
		revision, err := s.revisionOf(ctx, key)
		if err != nil {
			return Written{}, err
		}
		change.HeadRevision = revision
		if err := s.writeChange(ctx, change); err != nil {
			return Written{}, err
		}
		return Written{Page: page, Revision: revision, ChangeID: change.ID}, nil
	}
	return Written{}, fmt.Errorf("%w: %s", ErrConflict, pageID)
}

// commentID mints a comment's id, deterministically for a turn.
func (s *Store) commentID(pageID string, in NewComment) string {
	if strings.TrimSpace(in.TurnKey) == "" {
		return s.newID()
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(in.Body)))
	name := pageID + "\x00" + in.TurnKey + "\x00" + hex.EncodeToString(sum[:])
	return uuid.NewSHA1(commentNamespace, []byte(name)).String()
}

// commentNamespace scopes the derived comment ids. Fixed for the life of the
// deployment: a new one would make every re-run turn post a duplicate.
var commentNamespace = uuid.MustParse("9c1d2e3f-4a5b-5c6d-8e7f-0a1b2c3d4e5f")

func (s *Store) readComment(ctx context.Context, pageID, commentID string) (Comment, error) {
	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, CommentKey(pageID, commentID))
	if err != nil {
		return Comment{}, fmt.Errorf("pages: read comment %s: %w", commentID, err)
	}
	if !found {
		return Comment{}, fmt.Errorf("%w: comment %s", ErrNotFound, commentID)
	}
	return DecodeComment(rec.Value)
}

// Remove deletes a page and everything under it.
//
// TRASHING IS THE ORDINARY GESTURE — a status change, recoverable, and what
// the sweep turns into this after thirty days. This is the permanent one, and
// it purges rather than deletes because these buckets are ageless: a delete's
// tombstone would outlive the deployment and a listing returning tombstones
// is a tree with ghosts in it.
//
// THE CHANGE KEY IS WRITTEN FIRST, before anything is purged: a wake saying
// "stop working on this" is worth more than a clean purge, and a crash
// between the two leaves a change a projector applies as a removal — the same
// end state.
func (s *Store) Remove(ctx context.Context, actor Actor, pageID string) error {
	if err := actor.validate(); err != nil {
		return err
	}
	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, PageKey(pageID))
	if err != nil {
		return fmt.Errorf("pages: read %s: %w", pageID, err)
	}
	if !found {
		return fmt.Errorf("%w: page %s", ErrNotFound, pageID)
	}
	page, err := DecodePage(rec.Value)
	if err != nil {
		return err
	}

	change := s.change(actor, page, ChangeRemoved, s.now())
	change.Excerpt = excerpt(page.Title)
	change.HeadRevision = rec.Version
	if err := s.writeChange(ctx, change); err != nil {
		return err
	}

	for _, prefix := range []string{CommentPrefix(pageID), RevisionPrefix(pageID)} {
		records, err := s.docs.Documents(ctx, coord.FamilyPages, prefix)
		if err != nil {
			return fmt.Errorf("pages: list %s under %q: %w", prefix, page.Title, err)
		}
		for _, r := range records {
			if _, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, r.Key, r.Version); err != nil {
				return fmt.Errorf("pages: remove a record under %q: %w", page.Title, err)
			}
		}
	}
	if _, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, PageKey(pageID), rec.Version); err != nil {
		return fmt.Errorf("pages: remove %q: %w", page.Title, err)
	}
	// The title is released LAST, so the name is never free while the page
	// still exists.
	s.releaseTitle(ctx, page.Container, page.Title, page.ID)

	// THE CHANGE KEYS STAY, as the tracker's do: they are what a
	// redelivered feed message is deduplicated against, and the yearly
	// sweep is what ends them.
	log.InfoContext(ctx, "pages_page_removed", "page", page.ID,
		"title", page.Title, "container", page.Container, "actor", actor.Name())
	return nil
}

// Page reads one head from coordination, for a caller that must not see a
// stale one. Ordinary reads go to the projection.
func (s *Store) Page(ctx context.Context, pageID string) (Page, uint64, error) {
	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, PageKey(pageID))
	if err != nil {
		return Page{}, 0, fmt.Errorf("pages: read %s: %w", pageID, err)
	}
	if !found {
		return Page{}, 0, fmt.Errorf("%w: page %s", ErrNotFound, pageID)
	}
	page, err := DecodePage(rec.Value)
	if err != nil {
		return Page{}, 0, err
	}
	return page, rec.Version, nil
}

// Revision reads one past body.
//
// FROM COORDINATION, not the projection, and that is the design: the
// projection keeps revision METADATA only, because a 512 KiB body times a
// hundred revisions times every page would be a local copy an order of
// magnitude larger than the record it copies, on every node, to answer a
// question a person asks about one page at a time.
func (s *Store) Revision(ctx context.Context, pageID string, version int) (Revision, error) {
	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, RevisionKey(pageID, version))
	if err != nil {
		return Revision{}, fmt.Errorf("pages: read revision %d of %s: %w", version, pageID, err)
	}
	if !found {
		return Revision{}, fmt.Errorf("%w: revision %d of page %s", ErrNotFound, version, pageID)
	}
	return DecodeRevision(rec.Value)
}

// EnsureContainer creates a container if it does not exist.
//
// IDEMPOTENT AND FIRST-WRITER-WINS: every node calls it on every apply for
// every unit's space, so a race between two nodes booting must be a no-op
// rather than an error either of them reports.
func (s *Store) EnsureContainer(ctx context.Context, key, name, purpose string) (Container, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return Container{}, invalid("container", "a container needs a key")
	}
	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, ContainerKey(key))
	if err != nil {
		return Container{}, fmt.Errorf("pages: read the container %s: %w", key, err)
	}
	if found {
		return DecodeContainer(rec.Value)
	}
	container := Container{
		V: DocumentVersion, Key: key, Name: strings.TrimSpace(name),
		Purpose: strings.TrimSpace(purpose), CreatedAt: s.now(),
	}
	data, err := EncodeContainer(container)
	if err != nil {
		return Container{}, err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyPages, ContainerKey(key), data)
	if err != nil {
		return Container{}, fmt.Errorf("pages: create the container %s: %w", key, err)
	}
	if !created {
		// A peer got there first, which is the ordinary case on a fleet
		// boot. Read theirs.
		rec, found, err := s.docs.Document(ctx, coord.FamilyPages, ContainerKey(key))
		if err != nil || !found {
			return Container{}, fmt.Errorf("pages: read the container %s back: %w", key, err)
		}
		return DecodeContainer(rec.Value)
	}
	return container, nil
}

// Thread reads a page's comments from coordination, oldest first.
func (s *Store) Thread(ctx context.Context, pageID string) ([]Comment, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyPages, CommentPrefix(pageID))
	if err != nil {
		return nil, fmt.Errorf("pages: read the thread on %s: %w", pageID, err)
	}
	out := make([]Comment, 0, len(records))
	for _, rec := range records {
		comment, err := DecodeComment(rec.Value)
		if err != nil {
			log.WarnContext(ctx, "pages_comment_unreadable", "page", pageID,
				"key", rec.Key, "error", err.Error())
			continue
		}
		out = append(out, comment)
	}
	slices.SortFunc(out, func(a, b Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

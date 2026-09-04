package work

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

// NewComment is a remark to add to an item.
type NewComment struct {
	Body string

	// Mentions are the handles this comment names, already resolved by the
	// caller against the party registry.
	//
	// RESOLVED AT WRITE TIME, not at read time. The roster moves — people
	// join, handles change — and a mention resolved later would wake
	// somebody the author never addressed.
	Mentions []string

	ReplyTo string

	// TurnKey makes a comment made from a turn IDEMPOTENT. A turn that is
	// re-run after its node died must not post its remark twice, and the
	// engine's own redelivery guarantees make a re-run ordinary rather than
	// exceptional. Empty mints a random id, which is right for a person:
	// somebody typing the same sentence twice meant to say it twice.
	TurnKey string

	Quiet bool
}

// Comment adds a remark and wakes whoever the rules say.
//
// THREE KEYS, IN ORDER: the comment is created first, then the head is
// compare-and-set (for the watcher set and the change sequence), then the
// change key. A crash after the first leaves a DURABLE COMMENT whose change
// key any projector repairs from the comment's own LastChange record — which
// is why a comment carries one at all. The other order would lose the remark
// and keep the notification about it.
func (s *Store) Comment(ctx context.Context, actor Actor, itemID string, in NewComment) (Comment, Written, error) {
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

	rec, found, err := s.docs.Document(ctx, coord.FamilyWork, ItemKey(itemID))
	if err != nil {
		return Comment{}, Written{}, fmt.Errorf("work: read %s: %w", itemID, err)
	}
	if !found {
		return Comment{}, Written{}, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	item, err := DecodeItem(rec.Value)
	if err != nil {
		return Comment{}, Written{}, err
	}

	at := s.now()
	comment := Comment{
		V: DocumentVersion, ID: s.commentID(item.ID, in), ItemID: item.ID,
		Author: actor.Name(), AuthorKind: actor.Kind, Body: body,
		Mentions: cleanList(in.Mentions), ReplyTo: in.ReplyTo,
		CreatedAt: at, UpdatedAt: at,
	}
	change := s.change(actor, item, ChangeComment, at)
	change.CommentID = comment.ID
	change.Excerpt = excerpt(body)
	change.Mentions = comment.Mentions
	change.Quiet = in.Quiet
	comment.LastChange = &change

	data, err := EncodeComment(comment)
	if err != nil {
		return Comment{}, Written{}, err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyWork,
		CommentKey(item.ID, comment.ID), data)
	if err != nil {
		return Comment{}, Written{}, fmt.Errorf("work: write comment on %s: %w", item.Key, err)
	}
	if !created {
		// THE DETERMINISTIC ID COLLIDED, which means this exact comment
		// from this exact turn is already there — a re-run. Returning the
		// existing one is the whole reason the id is derived: a re-posted
		// remark reads to everyone else as the agent saying it twice.
		existing, err := s.readComment(ctx, item.ID, comment.ID)
		if err != nil {
			return Comment{}, Written{}, err
		}
		return existing, Written{Item: item, Revision: rec.Version}, nil
	}

	// The head write carries the watcher rules. A LOST RACE HERE IS
	// RETRIED, never refused: the comment is already durable, and giving
	// the caller an error over a contended head would have them post it
	// again.
	var out Written
	err = s.mutate(ctx, item.ID, 0, func(head *Item) (Change, error) {
		s.applyCommentWatchers(actor, head, comment)
		next := s.change(actor, *head, ChangeComment, at)
		next.ID = change.ID
		next.CommentID = comment.ID
		next.Excerpt = change.Excerpt
		next.Mentions = comment.Mentions
		next.Quiet = in.Quiet
		return next, nil
	}, &out)
	if err != nil {
		return comment, Written{}, err
	}
	return comment, out, nil
}

// applyCommentWatchers is the participants rule, Jira's.
//
// A COMMENTER WATCHES. It is the rule people expect from a tracker and it is
// what makes a thread a conversation rather than a broadcast: somebody who
// says something in a thread hears the reply. The cost is that a lead who
// hands work over by comment keeps hearing about it — which is one click to
// undo in the item view, and the alternative (a lead who comments and then
// misses the answer) is the worse failure.
//
// A MENTION IS DIRECTED and goes through [mention] rather than [addWatcher],
// so it reaches a muted person: a mute says "stop telling me about this
// item", and somebody typing your handle is telling YOU specifically.
func (s *Store) applyCommentWatchers(actor Actor, item *Item, comment Comment) {
	addWatcher(item, actor.Handle)
	for _, handle := range comment.Mentions {
		mention(item, handle)
	}
	if len(item.Watchers) > MaxWatchers {
		// Past the cap the OLDEST are dropped, not the newest: the people
		// most recently involved are the ones a thread is about.
		item.Watchers = item.Watchers[len(item.Watchers)-MaxWatchers:]
	}
}

// commentID mints a comment's id, deterministically for a turn.
//
// uuid5 OVER (item, turn key, body hash), so a re-run turn producing the same
// remark collides with itself and the create is a no-op. The BODY IS IN THE
// HASH because a turn that is re-run and says something DIFFERENT is making a
// second remark, and collapsing those would silently drop it.
func (s *Store) commentID(itemID string, in NewComment) string {
	if strings.TrimSpace(in.TurnKey) == "" {
		return s.newID()
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(in.Body)))
	name := itemID + "\x00" + in.TurnKey + "\x00" + hex.EncodeToString(sum[:])
	return uuid.NewSHA1(commentNamespace, []byte(name)).String()
}

// commentNamespace scopes the derived comment ids.
//
// A FIXED, ARBITRARY uuid — it is a namespace, and its only requirement is
// that it never changes: a new one would make every re-run turn post a
// duplicate of every comment it had already made.
var commentNamespace = uuid.MustParse("6f5f6a1e-9d3f-5b2e-8a41-2c6f9d0b7e14")

// readComment reads one comment.
func (s *Store) readComment(ctx context.Context, itemID, commentID string) (Comment, error) {
	rec, found, err := s.docs.Document(ctx, coord.FamilyWork, CommentKey(itemID, commentID))
	if err != nil {
		return Comment{}, fmt.Errorf("work: read comment %s: %w", commentID, err)
	}
	if !found {
		return Comment{}, fmt.Errorf("%w: comment %s", ErrNotFound, commentID)
	}
	return DecodeComment(rec.Value)
}

// EditComment rewrites a comment's body.
//
// AN EDIT IS A COMPARE-AND-SET ON THE SAME KEY, and only newly mentioned
// parties are woken: re-waking everybody for a typo fix is how a tracker
// teaches people to ignore it. Only the author may edit — an operator or
// another seat rewriting somebody's words would put a sentence under a name
// that never said it.
func (s *Store) EditComment(ctx context.Context, actor Actor, itemID, commentID, body string) (Comment, error) {
	if err := actor.validate(); err != nil {
		return Comment{}, err
	}
	body = strings.TrimSpace(body)
	switch {
	case body == "":
		return Comment{}, invalid("body", "a comment needs something in it")
	case len(body) > MaxComment:
		return Comment{}, invalid("body", "%d bytes, past the %d-byte cap", len(body), MaxComment)
	}
	key := CommentKey(itemID, commentID)
	for range casRounds {
		rec, found, err := s.docs.Document(ctx, coord.FamilyWork, key)
		if err != nil {
			return Comment{}, fmt.Errorf("work: read comment %s: %w", commentID, err)
		}
		if !found {
			return Comment{}, fmt.Errorf("%w: comment %s", ErrNotFound, commentID)
		}
		comment, err := DecodeComment(rec.Value)
		if err != nil {
			return Comment{}, err
		}
		if comment.Author != actor.Name() {
			return Comment{}, invalid("author",
				"only %s can edit their own comment; this write is %s",
				comment.Author, actor.Name())
		}
		if comment.Body == body {
			return comment, nil
		}
		comment.Body = body
		comment.UpdatedAt = s.now()

		data, err := EncodeComment(comment)
		if err != nil {
			return Comment{}, err
		}
		ok, err := s.docs.UpdateDocument(ctx, coord.FamilyWork, key, data, rec.Version)
		if err != nil {
			return Comment{}, fmt.Errorf("work: write comment %s: %w", commentID, err)
		}
		if !ok {
			continue
		}
		return comment, nil
	}
	return Comment{}, fmt.Errorf("%w: comment %s", ErrConflict, commentID)
}

// Remove deletes an item and everything under it.
//
// PURGE, NEVER DELETE, on the buckets' own rule: these buckets have no age,
// so a delete leaves a tombstone that outlives the deployment and a listing
// that returns tombstones is a board with ghosts on it.
//
// THE CHANGE KEY IS WRITTEN FIRST, before anything is purged, and this is the
// one sequence that runs in that order: a wake saying "stop working on this"
// is worth more than a clean purge, and a crash between the two leaves a
// change key for an item that is gone — which a projector applies as a
// removal, reaching the same end state.
func (s *Store) Remove(ctx context.Context, actor Actor, itemID string) error {
	if err := actor.validate(); err != nil {
		return err
	}
	rec, found, err := s.docs.Document(ctx, coord.FamilyWork, ItemKey(itemID))
	if err != nil {
		return fmt.Errorf("work: read %s: %w", itemID, err)
	}
	if !found {
		return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	item, err := DecodeItem(rec.Value)
	if err != nil {
		return err
	}

	change := s.change(actor, item, ChangeRemoved, s.now())
	change.Excerpt = excerpt(item.Title)
	change.HeadRevision = rec.Version
	if err := s.writeChange(ctx, change); err != nil {
		return err
	}

	// The comments go before the head, so a crash never leaves a thread
	// hanging off nothing: an orphan comment key would be invisible to
	// every reader and swept only by the hourly pass.
	comments, err := s.docs.Documents(ctx, coord.FamilyWork, CommentPrefix(itemID))
	if err != nil {
		return fmt.Errorf("work: list the thread on %s: %w", item.Key, err)
	}
	for _, c := range comments {
		if _, err := s.docs.PurgeDocument(ctx, coord.FamilyWork, c.Key, c.Version); err != nil {
			return fmt.Errorf("work: remove a comment on %s: %w", item.Key, err)
		}
	}
	if _, err := s.docs.PurgeDocument(ctx, coord.FamilyWork, ItemKey(itemID), rec.Version); err != nil {
		return fmt.Errorf("work: remove %s: %w", item.Key, err)
	}

	// THE CHANGE KEYS STAY. They are the record of what happened, they are
	// what a redelivered feed message is deduplicated against, and the
	// 365-day sweep is what ends them — purging them here would let a
	// redelivery re-wake a seat about an item nobody can look at.
	log.InfoContext(ctx, "work_item_removed", "item", item.Key,
		"actor", actor.Name(), "comments", len(comments))
	return nil
}

// Item reads one head from coordination.
//
// THE READ-THROUGH PATH, for a caller that must not see a stale head: a turn
// woken by a change above the projection's cursor, and every read-decide-write
// above. Ordinary reads go to the projection, which is what a board and a
// tool's list call use.
func (s *Store) Item(ctx context.Context, itemID string) (Item, uint64, error) {
	rec, found, err := s.docs.Document(ctx, coord.FamilyWork, ItemKey(itemID))
	if err != nil {
		return Item{}, 0, fmt.Errorf("work: read %s: %w", itemID, err)
	}
	if !found {
		return Item{}, 0, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	item, err := DecodeItem(rec.Value)
	if err != nil {
		return Item{}, 0, err
	}
	return item, rec.Version, nil
}

// Thread reads an item's comments from coordination, oldest first.
func (s *Store) Thread(ctx context.Context, itemID string) ([]Comment, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyWork, CommentPrefix(itemID))
	if err != nil {
		return nil, fmt.Errorf("work: read the thread on %s: %w", itemID, err)
	}
	out := make([]Comment, 0, len(records))
	for _, rec := range records {
		comment, err := DecodeComment(rec.Value)
		if err != nil {
			// A COMMENT A NEWER BUILD WROTE IS SKIPPED, not fatal: a
			// rolling upgrade must not make a whole thread unreadable on
			// the older half.
			log.WarnContext(ctx, "work_comment_unreadable", "item", itemID,
				"key", rec.Key, "error", err.Error())
			continue
		}
		out = append(out, comment)
	}
	slices.SortFunc(out, func(a, b Comment) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// History reads an item's change record, oldest first.
func (s *Store) History(ctx context.Context, itemID string) ([]Change, error) {
	records, err := s.docs.Documents(ctx, coord.FamilyWork, ChangePrefix(itemID))
	if err != nil {
		return nil, fmt.Errorf("work: read the history of %s: %w", itemID, err)
	}
	out := make([]Change, 0, len(records))
	for _, rec := range records {
		change, err := DecodeChange(rec.Value)
		if err != nil {
			log.WarnContext(ctx, "work_change_unreadable", "item", itemID,
				"key", rec.Key, "error", err.Error())
			continue
		}
		out = append(out, change)
	}
	slices.SortFunc(out, func(a, b Change) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		// The ids are time-ordered, so they break a same-instant tie the
		// same way on every node — a history that listed differently per
		// node would make two people reading one item disagree.
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

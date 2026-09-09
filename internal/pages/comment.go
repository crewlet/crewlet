package pages

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// COMMENTS, AND THE ONE RULE THAT MAKES THEM DIFFERENT FROM THE TRACKER'S.
//
// A comment rides the PAGE's subject, so two people commenting on one page
// contend at the broker and one retries — the same trade the tracker makes for
// a task's comments, and for the same reason: a comment changes what the
// page's card shows and what its history says, so the two are one object's
// state and not two.
//
// What is NOT the same is subscription. A COMMENT DOES NOT SUBSCRIBE ITS
// COMMENTER here, which is the opposite of the tracker's participants rule and
// deliberate: a page a hundred people have remarked on would otherwise wake a
// hundred seats when somebody fixes a heading. Only a MENTION subscribes, and
// only the person mentioned.

// NewComment is a remark to add.
type NewComment struct {
	Body string

	// ReplyTo is the comment this one answers, for threading.
	ReplyTo string

	// Mentions are handles this comment named. Each is woken whether or
	// not they watch, and each is subscribed.
	Mentions []string

	// TurnKey makes a comment made from a turn idempotent: a re-run turn
	// posts once.
	//
	// IT DERIVES THE OPERATION ID rather than only the comment's own,
	// which is the upgrade the log brings: the operation ledger collapses
	// the whole record, so a retried turn does not even append — where the
	// bucket could only make the second write land on the same key.
	TurnKey string

	Quiet bool
}

// Comment adds one remark to a page.
func (s *Store) Comment(ctx context.Context, actor Actor, pageID string,
	in NewComment) (Comment, Written, error) {

	if err := actor.validate(); err != nil {
		return Comment{}, Written{}, err
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		return Comment{}, Written{}, invalid("body", "a comment needs a body")
	}
	if len(body) > MaxComment {
		return Comment{}, Written{}, invalid("body",
			"%d bytes, past the %d-byte cap", len(body), MaxComment)
	}
	mentions := cleanList(in.Mentions)

	at := s.now()
	opID := s.commentOpID(pageID, in)
	subject := PageSubject(pageID)
	comment := Comment{
		V: DocumentVersion, ID: opID, PageID: pageID,
		Author: actor.Name(), AuthorKind: actor.Kind, Body: body,
		Mentions: mentions, ReplyTo: strings.TrimSpace(in.ReplyTo),
		CreatedAt: at, UpdatedAt: at,
	}

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			head, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			patch := PagePatch{V: DocumentVersion, Comment: &CommentPatch{
				ID: comment.ID, Body: &body, Author: comment.Author,
				AuthorKind: actor.Kind, ReplyTo: comment.ReplyTo,
				Mentions: mentions,
			}}
			// A MENTION SUBSCRIBES, and nothing else does. It is
			// carried on the patch as the whole watcher set, because
			// a collection the write touches travels whole — a delta
			// could not rebuild the row on a replay from zero.
			if subscribed := subscribeMentions(&head, mentions); subscribed {
				patch.Watchers = head.Watchers
				patch.Muted = head.Muted
			}
			scope := ScopeSet{Subject: true, Container: head.Container}
			notify := s.notifyOf(in.Quiet, ChangeComment, head,
				excerpt(body), mentions)
			return s.decide(actor, subject, OpPatch, scope, opID, patch, notify, at)
		},
	})
	if err != nil {
		return Comment{}, Written{}, err
	}
	return comment, Written{
		Revision: result.Position.Seq, ChangeID: opID, Outcome: result,
	}, nil
}

// EditComment rewrites one remark's body.
func (s *Store) EditComment(ctx context.Context, actor Actor, pageID,
	commentID, body string) (Comment, Written, error) {

	if err := actor.validate(); err != nil {
		return Comment{}, Written{}, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return Comment{}, Written{}, invalid("body", "a comment needs a body")
	}
	if len(body) > MaxComment {
		return Comment{}, Written{}, invalid("body",
			"%d bytes, past the %d-byte cap", len(body), MaxComment)
	}

	at := s.now()
	opID := s.newSeqID()
	subject := PageSubject(pageID)
	var out Comment

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			head, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			held, err := readCommentTx(ctx, tx, pageID, commentID)
			if err != nil {
				return statelog.Decision{}, err
			}
			// ONLY THE AUTHOR, operator included. A comment is a remark
			// somebody made, and an edit anybody could make is a remark
			// attributed to a person who did not make it — on a record
			// that outlives the page's body and is quoted in a wake.
			if held.Author != actor.Name() {
				return statelog.Decision{}, invalid("comment",
					"comment %s was written by %s and only its author may edit "+
						"it — a remark somebody else can rewrite is a remark "+
						"attributed to a person who did not make it",
					commentID, held.Author)
			}
			if held.Body == body {
				out = held
				return statelog.Decision{}, nil
			}
			out = held
			out.Body, out.UpdatedAt = body, at
			scope := ScopeSet{Subject: true, Container: head.Container}
			notify := s.notifyOf(false, ChangeCommentEdited, head,
				excerpt(body), nil)
			return s.decide(actor, subject, OpPatch, scope, opID, PagePatch{
				V: DocumentVersion, Comment: &CommentPatch{
					ID: commentID, Body: &body, Author: held.Author,
					AuthorKind: held.AuthorKind, ReplyTo: held.ReplyTo,
					Mentions: held.Mentions,
				},
			}, notify, at)
		},
	})
	if err != nil {
		return Comment{}, Written{}, err
	}
	return out, Written{
		Revision: result.Position.Seq, ChangeID: opID, Outcome: result,
	}, nil
}

// RemoveComment takes one remark down.
func (s *Store) RemoveComment(ctx context.Context, actor Actor, pageID,
	commentID string) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	at := s.now()
	opID := s.newSeqID()
	subject := PageSubject(pageID)

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			head, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			scope := ScopeSet{Subject: true, Container: head.Container}
			notify := s.notifyOf(true, ChangeCommentEdited, head, "", nil)
			return s.decide(actor, subject, OpPatch, scope, opID, PagePatch{
				V:       DocumentVersion,
				Comment: &CommentPatch{ID: commentID, Removed: true},
			}, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return Written{
		Revision: result.Position.Seq, ChangeID: opID, Outcome: result,
	}, nil
}

// subscribeMentions adds the handles a comment named, reporting whether the
// set moved.
//
// ONLY A MENTION SUBSCRIBES, which is this package's rule and the opposite of
// the tracker's: a page a hundred people have remarked on would wake a hundred
// seats when somebody fixes a heading.
func subscribeMentions(page *Page, mentions []string) bool {
	moved := false
	for _, handle := range mentions {
		if handle == "" || contains(page.Watchers, handle) {
			continue
		}
		// A MUTED PERSON WHO IS MENTIONED IS NOT RE-SUBSCRIBED. They
		// said no once; a mention wakes them for this one comment,
		// which the notification's own Mentions field carries.
		if contains(page.Muted, handle) {
			continue
		}
		page.Watchers = append(page.Watchers, handle)
		moved = true
	}
	if moved {
		slicesSort(page.Watchers)
	}
	return moved
}

// readCommentTx reads one comment from inside a decision's own transaction.
func readCommentTx(ctx context.Context, tx *sql.Tx, pageID, commentID string) (
	Comment, error) {

	var document []byte
	err := tx.QueryRowContext(ctx,
		`SELECT document FROM pages_comments WHERE id = ? AND page_id = ?`,
		commentID, pageID).Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Comment{}, fmt.Errorf("%w: comment %s on page %s",
			ErrNotFound, commentID, pageID)
	case err != nil:
		return Comment{}, fmt.Errorf("pages: read comment %s: %w", commentID, err)
	}
	return DecodeComment(document)
}

// commentOpID is the operation this comment belongs to.
//
// DERIVED FROM THE TURN when one is named, so a re-run turn is one operation
// the ledger collapses rather than a second comment. The comment's own id is
// the same value: one comment is one operation here, and two identifiers for
// one thing is two places for a retry to disagree with itself.
func (s *Store) commentOpID(pageID string, in NewComment) string {
	if strings.TrimSpace(in.TurnKey) == "" {
		return s.newSeqID()
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(in.Body)))
	name := pageID + "\x00" + strings.TrimSpace(in.TurnKey) + "\x00" +
		hex.EncodeToString(sum[:])
	return uuid.NewSHA1(commentNamespace, []byte(name)).String()
}

// commentNamespace scopes the derived operation ids. FIXED for the life of the
// deployment: a new one would make every re-run turn post a duplicate.
var commentNamespace = uuid.MustParse("9c1d2e3f-4a5b-5c6d-8e7f-0a1b2c3d4e5f")

// Thread is one page's comments, oldest first.
//
// ON THE STORE rather than only on the reader, because a caller that just
// wrote a comment reads the thread back to render it — and routing that
// through the reader would mean two seams for one question.
func (s *Store) Thread(ctx context.Context, pageID string) ([]Comment, error) {
	var out []Comment
	err := s.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT document FROM pages_comments WHERE page_id = ?
			  ORDER BY created_at, id`, pageID)
		if err != nil {
			return fmt.Errorf("pages: read the thread on %s: %w", pageID, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var document []byte
			if err := rows.Scan(&document); err != nil {
				return fmt.Errorf("pages: scan a comment on %s: %w", pageID, err)
			}
			comment, err := DecodeComment(document)
			if err != nil {
				return err
			}
			out = append(out, comment)
		}
		return rows.Err()
	})
	return out, err
}

// Page is one page's head, read outside a decision.
func (s *Store) Page(ctx context.Context, pageID string) (Page, uint64, error) {
	head, err := s.head(ctx, pageID)
	if err != nil {
		return Page{}, 0, err
	}
	return head, 0, nil
}

// Revision is one immutable body.
func (s *Store) Revision(ctx context.Context, pageID string, version int) (
	Revision, error) {

	var out Revision
	err := s.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var author, message, title, body string
		var created int64
		err := tx.QueryRowContext(ctx, `
			SELECT title, body, message, author, created_at
			  FROM pages_revisions WHERE page_id = ? AND edit_version = ?`,
			pageID, version).Scan(&title, &body, &message, &author, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: revision %d of page %s", ErrNotFound,
				version, pageID)
		}
		if err != nil {
			return fmt.Errorf("pages: read revision %d of %s: %w",
				version, pageID, err)
		}
		out = Revision{
			V: DocumentVersion, PageID: pageID, Version: version,
			Title: title, Body: body, Message: message, Author: author,
			CreatedAt: store.DecodeTime(created),
		}
		return nil
	})
	return out, err
}

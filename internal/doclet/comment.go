package doclet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
	opID, opErr := s.commentOpID(actor, pageID, in)
	if opErr != nil {
		return Comment{}, Written{}, opErr
	}
	subject := PageSubject(pageID)
	comment := Comment{
		V: DocumentVersion, ID: opID, PageID: pageID,
		Author: actor.Name(), AuthorKind: actor.Kind, Body: body,
		Mentions: mentions, ReplyTo: strings.TrimSpace(in.ReplyTo),
		CreatedAt: at, UpdatedAt: at,
	}

	result, err := s.onPage(ctx, pageID, func(container string) statelog.Request {
		return statelog.Request{
			Subject: statelog.Subject{Kind: string(KindPage), ID: pageID},
			Scope:   ScopeSet{Subject: true, Container: container}.Resolve(subject),
			OpID:    opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, _, err := readHeadTx(ctx, tx, pageID)
				if err != nil {
					return statelog.Decision{}, err
				}
				if moved := inContainer(head, container); moved != nil {
					return statelog.Decision{}, moved
				}
				if err = s.refuseReserved(actor, head.Container, false); err != nil {
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
				return s.decide(stamp, actor, subject, OpPatch, scope, opID, patch, notify, at)
			},
		}
	})
	if err != nil {
		return Comment{}, Written{}, err
	}
	// A COMMENT ALWAYS LANDS A RECORD — the decision is unconditional — so
	// there is no read arm for [writtenRevision] to fall back on.
	written := Written{
		Revision: writtenRevision(result, 0), ChangeID: opID, Outcome: result,
	}
	if result.Collapsed {
		// THE REMARK AN EARLIER COPY POSTED, as the rows hold it: its
		// author and its instant are that copy's, and only the id, the
		// page, the body and the remark it answers are pinned by the
		// operation this call shares with it — see [Store.commentOpID].
		comment, err = s.landedComment(ctx, Comment{
			V: DocumentVersion, ID: opID, PageID: pageID, Body: body,
			ReplyTo: comment.ReplyTo,
		}, opID)
		return comment, written, err
	}
	return comment, written, nil
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
	opID, opErr := s.operation(actor, "comment-edit", commentID, body)
	if opErr != nil {
		return Comment{}, Written{}, opErr
	}
	subject := PageSubject(pageID)
	var out Comment
	// THE REVISION AN UNCHANGED EDIT ANSWERS WITH, taken in the decision's
	// own snapshot: re-editing a comment to the text it already has appends
	// nothing, so the row's own number is the only one there will be.
	var read uint64

	result, err := s.onPage(ctx, pageID, func(container string) statelog.Request {
		return statelog.Request{
			Subject: statelog.Subject{Kind: string(KindPage), ID: pageID},
			Scope:   ScopeSet{Subject: true, Container: container}.Resolve(subject),
			OpID:    opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, revision, err := readHeadTx(ctx, tx, pageID)
				if err != nil {
					return statelog.Decision{}, err
				}
				if moved := inContainer(head, container); moved != nil {
					return statelog.Decision{}, moved
				}
				if err = s.refuseReserved(actor, head.Container, false); err != nil {
					return statelog.Decision{}, err
				}
				read = revision
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
							"it, because a remark somebody else can rewrite is a remark "+
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
				return s.decide(stamp, actor, subject, OpPatch, scope, opID, PagePatch{
					V: DocumentVersion, Comment: &CommentPatch{
						ID: commentID, Body: &body, Author: held.Author,
						AuthorKind: held.AuthorKind, ReplyTo: held.ReplyTo,
						Mentions: held.Mentions,
					},
				}, notify, at)
			},
		}
	})
	if err != nil {
		return Comment{}, Written{}, err
	}
	written := Written{
		Revision: writtenRevision(result, read), ChangeID: opID, Outcome: result,
	}
	if result.Collapsed {
		out, err = s.landedComment(ctx, Comment{
			V: DocumentVersion, ID: commentID, PageID: pageID,
		}, opID)
		return out, written, err
	}
	return out, written, nil
}

// CommentAuthority is what a caller may do to somebody else's remark.
//
// A VALUE RATHER THAN A BOOL for [worklet.PersonAuthority]'s reason, and it
// states the DECISION: the caller asks [authz.ActionPageCommentRemove] and
// passes what came back, because this package holds no chart and no
// permissions.
type CommentAuthority struct {
	// Moderate reports a caller authorized to take down a remark they did
	// not write, which [authz.ClassAuthored] admits on ADMIN alone — not
	// on any relation to the page. A lead is not
	// automatically a moderator of what is said on their team's pages,
	// and making them one is a decision the table would have to state.
	//
	// THERE IS NO EDIT EQUIVALENT, deliberately. Removing a remark takes
	// it down and everyone can see that it is gone; an EDIT rewrites it,
	// so a remark somebody else can change is a remark attributed to a
	// person who did not make it — on a record that outlives the page's
	// body and is quoted in a wake. A lead moderating a page removes;
	// nobody puts words in somebody's mouth.
	Moderate bool
}

// RemoveComment takes one remark down.
//
// THE AUTHOR, OR A MODERATOR. It used to be neither: this verb had no check
// at all, so any caller that could reach it could take down any remark on any
// page — which [EditComment] three functions up refuses in its own decide, and
// the asymmetry was invisible because nothing called this yet.
func (s *Store) RemoveComment(ctx context.Context, actor Actor, pageID,
	commentID string, authority CommentAuthority) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	at := s.now()
	// NOTHING BUT THE COMMENT: a removal says nothing else, and the
	// authority it is taken under is who is asking rather than what.
	opID, opErr := s.operation(actor, "comment-remove", commentID, nil)
	if opErr != nil {
		return Written{}, opErr
	}
	subject := PageSubject(pageID)

	result, err := s.onPage(ctx, pageID, func(container string) statelog.Request {
		return statelog.Request{
			Subject: statelog.Subject{Kind: string(KindPage), ID: pageID},
			Scope:   ScopeSet{Subject: true, Container: container}.Resolve(subject),
			OpID:    opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, _, err := readHeadTx(ctx, tx, pageID)
				if err != nil {
					return statelog.Decision{}, err
				}
				if moved := inContainer(head, container); moved != nil {
					return statelog.Decision{}, moved
				}
				if err = s.refuseReserved(actor, head.Container, false); err != nil {
					return statelog.Decision{}, err
				}
				// THE AUTHOR IS READ IN THE DECIDE'S OWN SNAPSHOT, which
				// is the only place it can be: a removal names a comment
				// id and who wrote it is a row. A comment that is not
				// there is [ErrNotFound], for a moderator as for its
				// author — the refusal [Store.EditComment] gives the same
				// absence: a remark that is gone has no author to ask the
				// check about and nothing for a record to take down.
				held, err := readCommentTx(ctx, tx, pageID, commentID)
				if err != nil {
					return statelog.Decision{}, err
				}
				if !authority.Moderate && held.Author != actor.Name() {
					return statelog.Decision{}, invalid("comment",
						"comment %s was written by %s — a remark is taken down by "+
							"whoever made it, and by nobody else but an "+
							"admin", commentID,
						held.Author)
				}
				scope := ScopeSet{Subject: true, Container: head.Container}
				notify := s.notifyOf(true, ChangeCommentEdited, head, "", nil)
				return s.decide(stamp, actor, subject, OpPatch, scope, opID, PagePatch{
					V:       DocumentVersion,
					Comment: &CommentPatch{ID: commentID, Removed: true},
				}, notify, at)
			},
		}
	})
	if err != nil {
		return Written{}, err
	}
	// A REMOVAL THAT WAS DECIDED ALWAYS LANDS A RECORD, so there is no read
	// arm for [writtenRevision] to fall back on; and a retry answered from
	// the ledger ([statelog.Result.Collapsed]) carries nothing computed in a
	// decision, so it is answered as it stands.
	return Written{
		Revision: writtenRevision(result, 0), ChangeID: opID, Outcome: result,
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
	err := tx.QueryRowContext(ctx, commentTxStatement, commentID, pageID).
		Scan(&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Comment{}, fmt.Errorf("%w: comment %s on page %s",
			ErrNotFound, commentID, pageID)
	case err != nil:
		return Comment{}, fmt.Errorf("doclet: read comment %s: %w", commentID, err)
	}
	return DecodeComment(document)
}

// commentOpID is the operation this comment belongs to.
//
// DERIVED FROM THE CALLER'S KEY ([Actor.OpKey]) where it brought one — a
// turn's call through its own key, which a seat's tools derive from the turn,
// the tool, the call's arguments and its repeat count in the run; a person's
// request through its Idempotency-Key — so a retry or a re-run turn is one
// operation the ledger collapses rather than a second comment. The comment's
// own id is the same value: one comment is one operation here, and two
// identifiers for one thing is two places for a retry to disagree with itself.
//
// It is [statelog.KeyedObjectOpID], the derivation every keyed write takes,
// UNNAMED because this is the comment's own id as well and a reader addresses
// it: the page and the call it came from are on the row. That is also where
// carrying the key's instant rather than this call's is argued — the gesture
// a person is most likely to press twice is a comment, and a request retried
// after an `unknown` must post once.
//
// OVER WHAT THE COMMENT ASKS ([commentAsk]), not its words alone: bound to the
// body, the same key sent with the same words in another thread was the first
// comment's operation — answered from the ledger as posted, with nothing
// posted where it was asked to go.
func (s *Store) commentOpID(actor Actor, pageID string, in NewComment) (string, error) {
	key := strings.TrimSpace(actor.OpKey)
	if key == "" {
		return s.newSeqID(), nil
	}
	id, err := statelog.KeyedObjectOpID(key, "comment", pageID, commentAsk{
		Body: strings.TrimSpace(in.Body), ReplyTo: strings.TrimSpace(in.ReplyTo),
		Quiet: in.Quiet,
	})
	if err != nil {
		return "", fmt.Errorf("doclet: %w", err)
	}
	return id, nil
}

// commentAsk is what a comment SAYS, which its operation is bound to
// ([Store.commentOpID]): its words, the remark it answers and whether it is
// quiet — [renameAsk]'s terms for a remark.
//
// NOT ITS MENTIONS ([NewComment.Mentions]), which a seat's tool reads off the
// words against the roster: a roster that moved between a call and its retry
// would make the retry a second comment, and the words already decide them.
type commentAsk struct {
	Body    string
	ReplyTo string
	Quiet   bool
}

// Thread is one page's comments, oldest first.
//
// ON THE STORE rather than only on the reader, because a caller that just
// wrote a comment reads the thread back to render it — and routing that
// through the reader would mean two seams for one question.
func (s *Store) Thread(ctx context.Context, pageID string) ([]Comment, error) {
	var out []Comment
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, threadStatement, pageID)
		if err != nil {
			return fmt.Errorf("doclet: read the thread on %s: %w", pageID, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var document []byte
			if err := rows.Scan(&document); err != nil {
				return fmt.Errorf("doclet: scan a comment on %s: %w", pageID, err)
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

// Page is one page's head, read outside a decision, and the log revision it
// was read at.
//
// THE REVISION IS THE ROW'S OWN, never a constant. It was the coordination
// bucket's version before this domain moved onto its log, and the move left a
// literal zero in its place — which is the one value a caller cannot act on,
// being both "this node has applied nothing for this page" and "nobody
// answered". See [Store.headAt] for which number it is and why.
func (s *Store) Page(ctx context.Context, pageID string) (Page, uint64, error) {
	return s.headAt(ctx, pageID)
}

// Revision is one immutable body.
func (s *Store) Revision(ctx context.Context, pageID string, version int) (
	Revision, error) {

	var out Revision
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var author, message, title, body string
		var created int64
		err := tx.QueryRowContext(ctx, revisionStatement, pageID, version).
			Scan(&title, &body, &message, &author, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: revision %d of page %s", ErrNotFound,
				version, pageID)
		}
		if err != nil {
			return fmt.Errorf("doclet: read revision %d of %s: %w",
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

// commentTxStatement is [readCommentTx]'s read of one comment on one page, by
// its key, named for the plan gate.
const commentTxStatement = `SELECT document FROM pages_comments WHERE id = ? AND page_id = ?`

// revisionStatement reads one immutable body by its page and its number — the
// statement [Store.Revision] and [Reader.Revision] both read it with, named for
// the plan gate.
const revisionStatement = `
			SELECT title, body, message, author, created_at
			  FROM pages_revisions
			 WHERE page_id = ? AND edit_version = ?`

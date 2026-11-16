package artifactsdomain

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The writes on a comment thread's subject (ADR-0153). Each decides inside one
// snapshot of this node's rows: the artifact exists and is not in the trash,
// the thread and the remark exist, an edit is its author's, a resolve names a
// version between the one the thread was made on and the head. Who may comment
// at all is the API's decision (`comment_on_artifact`), as every role is.

// OpenThread opens a thread on an artifact with its first remark.
//
// THE THREAD'S ID AND ITS FIRST REMARK'S ARE THE OPERATION'S, each in its own
// namespace, so a retry names what its first attempt made; the thread's
// subject is its own beneath the artifact, create-only, as an asset's is.
func (w *Writer) OpenThread(ctx context.Context, c Call, in ThreadInput) (Commented, error) {
	if err := validCall(c); err != nil {
		return Commented{}, err
	}
	if in.Artifact.IsZero() {
		return Commented{}, fmt.Errorf("%w: a comment names its artifact", ErrInvalid)
	}
	if in.Version < 0 {
		return Commented{}, fmt.Errorf("%w: version %d is not a version", ErrInvalid, in.Version)
	}
	if err := checkRemark(in.Body); err != nil {
		return Commented{}, err
	}
	if err := checkQuote(in.Quote); err != nil {
		return Commented{}, err
	}
	mentions, err := cleanMentions(in.Mentions)
	if err != nil {
		return Commented{}, err
	}
	crop, cropType, err := checkCrop(in.Crop, in.CropType)
	if err != nil {
		return Commented{}, err
	}
	if err = checkNotify(in.Notify); err != nil {
		return Commented{}, err
	}
	anchor := in.Anchor
	if anchor.Kind == "" {
		anchor = commentanchor.Whole()
	}
	// THE ROUTING SNAPSHOT IS NOT WHAT THE WRITE SAYS: a retry recomputes it,
	// and a different snapshot must not make a second thread.
	said := in
	said.Notify = CommentNotify{}
	opID, err := w.operation(c, string(OpCommentOpen), in.Artifact.String(), said)
	if err != nil {
		return Commented{}, err
	}
	at := w.now()
	thread, remark := threadIDOf(opID), remarkIDOf(opID)
	id := in.Artifact.String()
	subject, scope := CommentSubject(in.Artifact, thread), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternCreate,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := liveHead(ctx, tx, id) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			version, err := madeOn(ctx, tx, head, in.Version)
			if err != nil {
				return statelog.Decision{}, err
			}
			if err = commentanchor.Check(head.Kind, anchor); err != nil {
				return statelog.Decision{}, fmt.Errorf("%w: the anchor: %w", ErrInvalid, err)
			}
			threads, err := countThreads(ctx, tx, id)
			if err != nil {
				return statelog.Decision{}, err
			}
			if threads >= artifacts.MaxThreadsPerArtifact {
				return statelog.Decision{}, fmt.Errorf("%w: this artifact keeps %d comment "+
					"threads, the most it may", ErrCommentLimit, threads)
			}
			return w.decision(stamp, c, subject, OpCommentOpen, scope, opID, CommentOpenPayload{
				V: baseRecordVersion, Remark: remark.String(), Body: in.Body, Version: version,
				Anchor: anchor, Quote: in.Quote, Crop: crop, CropType: cropType,
				Mentions: mentions, MaxThreads: artifacts.MaxThreadsPerArtifact,
				Notify: notifyOf(in.Notify),
			}, at)
		}))
	out := Commented{Thread: thread, Remark: remark, Result: res}
	if err != nil {
		return out, err
	}
	return out, w.commentVerdict(ctx, in.Artifact, thread, remark, OpCommentOpen, res,
		artifacts.MaxThreadsPerArtifact)
}

// Reply adds one remark to a thread, open or resolved: a reply leaves the
// thread's status as it is.
func (w *Writer) Reply(ctx context.Context, c Call, in ReplyInput) (Commented, error) {
	if err := validCall(c); err != nil {
		return Commented{}, err
	}
	if err := checkThreadRef(in.Artifact, in.Thread); err != nil {
		return Commented{}, err
	}
	if err := checkRemark(in.Body); err != nil {
		return Commented{}, err
	}
	mentions, err := cleanMentions(in.Mentions)
	if err != nil {
		return Commented{}, err
	}
	if err = checkNotify(in.Notify); err != nil {
		return Commented{}, err
	}
	subject, scope := CommentSubject(in.Artifact, in.Thread), ScopeSet{Subject: true}
	said := in
	said.Notify = CommentNotify{}
	opID, err := w.operation(c, string(OpCommentReply), subject.ID, said)
	if err != nil {
		return Commented{}, err
	}
	at := w.now()
	remark := remarkIDOf(opID)
	id, thread := in.Artifact.String(), in.Thread.String()
	var replyTo string
	if in.ReplyTo != uuid.Nil {
		replyTo = in.ReplyTo.String()
	}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := liveHead(ctx, tx, id); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			if _, err := mustThread(ctx, tx, id, thread); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			remarks, err := countRemarks(ctx, tx, id, thread) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if remarks >= artifacts.MaxRemarksPerThread {
				return statelog.Decision{}, fmt.Errorf("%w: this thread holds %d remarks, "+
					"the most one may; open another thread", ErrCommentLimit, remarks)
			}
			if replyTo != "" {
				if _, err = mustRemark(ctx, tx, id, thread, replyTo, true); err != nil {
					return statelog.Decision{}, err
				}
			}
			return w.decision(stamp, c, subject, OpCommentReply, scope, opID, CommentReplyPayload{
				V: baseRecordVersion, Remark: remark.String(), Body: in.Body, ReplyTo: replyTo,
				Mentions: mentions, MaxRemarks: artifacts.MaxRemarksPerThread,
				Notify: notifyOf(in.Notify),
			}, at)
		}))
	out := Commented{Thread: in.Thread, Remark: remark, Result: res}
	if err != nil {
		return out, err
	}
	return out, w.commentVerdict(ctx, in.Artifact, in.Thread, remark, OpCommentReply, res,
		artifacts.MaxRemarksPerThread)
}

// EditRemark replaces one remark's body and mentions. Only its author edits
// it, and an edit that changes nothing publishes nothing.
func (w *Writer) EditRemark(ctx context.Context, c Call, in RemarkEdit) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if err := checkRemarkRef(in.RemarkRef); err != nil {
		return Written{}, err
	}
	if err := checkRemark(in.Body); err != nil {
		return Written{}, err
	}
	mentions, err := cleanMentions(in.Mentions)
	if err != nil {
		return Written{}, err
	}
	subject, scope := CommentSubject(in.Artifact, in.Thread), ScopeSet{Subject: true}
	opID, err := w.operation(c, string(OpCommentEdit), subject.ID, in)
	if err != nil {
		return Written{}, err
	}
	at := w.now()
	id, thread, remark := in.Artifact.String(), in.Thread.String(), in.Remark.String()
	actor := c.Authority.Actor
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := liveHead(ctx, tx, id); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			r, err := mustRemark(ctx, tx, id, thread, remark, false) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if r.author != actor.Name || r.authorKind != string(actor.Kind) {
				return statelog.Decision{}, fmt.Errorf("%w: this remark is %s's", ErrNotTheAuthor, r.author)
			}
			if r.body == in.Body && slices.Equal(r.mentions, mentions) {
				return statelog.Decision{}, nil
			}
			return w.decision(stamp, c, subject, OpCommentEdit, scope, opID, CommentEditPayload{
				V: baseRecordVersion, Remark: remark, Body: in.Body, Mentions: mentions,
			}, at)
		}))
	if err != nil {
		return Written{Result: res}, err
	}
	return Written{Result: res}, w.commentVerdict(ctx, in.Artifact, in.Thread, in.Remark, OpCommentEdit, res, 0)
}

// RemoveRemark removes one remark: its row stays in its place, marked removed,
// its words cleared. Its author removes it, or anybody when the API marked the
// removal moderated; a remark already removed publishes nothing.
func (w *Writer) RemoveRemark(ctx context.Context, c Call, in RemarkRemoval) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if err := checkRemarkRef(in.RemarkRef); err != nil {
		return Written{}, err
	}
	subject, scope := CommentSubject(in.Artifact, in.Thread), ScopeSet{Subject: true}
	opID, err := w.operation(c, string(OpCommentRemove), subject.ID, in.Remark.String())
	if err != nil {
		return Written{}, err
	}
	at := w.now()
	id, thread, remark := in.Artifact.String(), in.Thread.String(), in.Remark.String()
	actor := c.Authority.Actor
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := liveHead(ctx, tx, id); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			r, err := mustRemark(ctx, tx, id, thread, remark, true) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if r.removed {
				return statelog.Decision{}, nil
			}
			if !in.Moderated && (r.author != actor.Name || r.authorKind != string(actor.Kind)) {
				return statelog.Decision{}, fmt.Errorf("%w: this remark is %s's", ErrNotTheAuthor, r.author)
			}
			return w.decision(stamp, c, subject, OpCommentRemove, scope, opID,
				CommentRemovePayload{V: baseRecordVersion, Remark: remark}, at)
		}))
	if err != nil {
		return Written{Result: res}, err
	}
	return Written{Result: res}, w.commentVerdict(ctx, in.Artifact, in.Thread, in.Remark, OpCommentRemove, res, 0)
}

// Resolve marks a thread addressed by a version, between the version it was
// made on and the head. Resolving a resolved thread publishes nothing.
func (w *Writer) Resolve(ctx context.Context, c Call, in ResolveInput) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if err := checkThreadRef(in.Artifact, in.Thread); err != nil {
		return Written{}, err
	}
	if in.Version < 0 {
		return Written{}, fmt.Errorf("%w: version %d is not a version", ErrInvalid, in.Version)
	}
	if err := checkNotify(in.Notify); err != nil {
		return Written{}, err
	}
	subject, scope := CommentSubject(in.Artifact, in.Thread), ScopeSet{Subject: true}
	opID, err := w.operation(c, string(OpCommentResolve), subject.ID, in.Version)
	if err != nil {
		return Written{}, err
	}
	at := w.now()
	id, thread := in.Artifact.String(), in.Thread.String()
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := liveHead(ctx, tx, id) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			t, err := mustThread(ctx, tx, id, thread)
			if err != nil {
				return statelog.Decision{}, err
			}
			if t.status == ThreadResolved {
				return statelog.Decision{}, nil
			}
			version := in.Version
			if version == 0 {
				version = head.HeadNumber
			}
			if version < t.version || version > head.HeadNumber {
				return statelog.Decision{}, fmt.Errorf("%w: the thread was made on version %d "+
					"and the head is %d, so the version that addressed it is one from %d to %d, "+
					"not %d", ErrInvalid, t.version, head.HeadNumber, t.version, head.HeadNumber, version)
			}
			return w.decision(stamp, c, subject, OpCommentResolve, scope, opID, CommentStatusPayload{
				V: baseRecordVersion, Version: version, Notify: notifyOf(in.Notify),
			}, at)
		}))
	if err != nil {
		return Written{Result: res}, err
	}
	return Written{Result: res}, w.commentVerdict(ctx, in.Artifact, in.Thread, uuid.Nil, OpCommentResolve, res, 0)
}

// Reopen opens a resolved thread again. Reopening an open thread publishes
// nothing.
func (w *Writer) Reopen(ctx context.Context, c Call, in ReopenInput) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	if err := checkThreadRef(in.Artifact, in.Thread); err != nil {
		return Written{}, err
	}
	if err := checkNotify(in.Notify); err != nil {
		return Written{}, err
	}
	subject, scope := CommentSubject(in.Artifact, in.Thread), ScopeSet{Subject: true}
	opID, err := w.operation(c, string(OpCommentReopen), subject.ID, nil)
	if err != nil {
		return Written{}, err
	}
	at := w.now()
	id, thread := in.Artifact.String(), in.Thread.String()
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := liveHead(ctx, tx, id); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			t, err := mustThread(ctx, tx, id, thread) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if t.status == ThreadOpen {
				return statelog.Decision{}, nil
			}
			return w.decision(stamp, c, subject, OpCommentReopen, scope, opID, CommentStatusPayload{
				V: baseRecordVersion, Notify: notifyOf(in.Notify),
			}, at)
		}))
	if err != nil {
		return Written{Result: res}, err
	}
	return Written{Result: res}, w.commentVerdict(ctx, in.Artifact, in.Thread, uuid.Nil, OpCommentReopen, res, 0)
}

// ---- the checks ------------------------------------------------------------ //

// checkRemark refuses a remark the domain never stores: empty, past its bound,
// not text, or carrying a credential. Markdown is kept verbatim.
func checkRemark(body string) error {
	switch {
	case strings.TrimSpace(body) == "":
		return fmt.Errorf("%w: a remark needs a body", ErrInvalid)
	case int64(len(body)) > artifacts.MaxCommentBytes:
		return fmt.Errorf("%w: the remark is %d bytes and the most is %d", ErrInvalid, len(body),
			artifacts.MaxCommentBytes)
	case !utf8.ValidString(body):
		return fmt.Errorf("%w: the remark is not UTF-8 text", ErrInvalid)
	}
	return refuseCredential("the remark", body)
}

// checkQuote refuses a quote past its bound, not text, or carrying a credential.
func checkQuote(quote string) error {
	switch {
	case int64(len(quote)) > artifacts.MaxCommentQuoteBytes:
		return fmt.Errorf("%w: the quote is %d bytes and the most is %d", ErrInvalid, len(quote),
			artifacts.MaxCommentQuoteBytes)
	case !utf8.ValidString(quote):
		return fmt.Errorf("%w: the quote is not UTF-8 text", ErrInvalid)
	}
	return refuseCredential("the quote", quote)
}

// refuseCredential refuses text holding a credential shape, never redacting it:
// everybody who reads the artifact reads its comments.
func refuseCredential(what, text string) error {
	if !redact.Contains(text) {
		return nil
	}
	return fmt.Errorf("%w: %w: %s carries a credential; remove it and send it again",
		ErrInvalid, ErrCommentCredential, what)
}

// cleanMentions is a remark's mentions sorted and unique, each a seat handle a
// record can carry, at most [artifacts.MaxCommentMentions].
func cleanMentions(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	for _, m := range in {
		if err := checkName("a mention", m); err != nil {
			return nil, err
		}
	}
	out := slices.Compact(slices.Sorted(slices.Values(in)))
	if len(out) > artifacts.MaxCommentMentions {
		return nil, fmt.Errorf("%w: the remark mentions %d seats and the most is %d", ErrInvalid,
			len(out), artifacts.MaxCommentMentions)
	}
	return out, nil
}

// checkCrop is a crop's stored form and media type: a still PNG, JPEG or WebP,
// told by its own bytes, at most [artifacts.MaxCommentCropBytes] and
// [artifacts.MaxCommentCropSide] pixels on its longer side.
func checkCrop(data []byte, typ string) ([]byte, string, error) {
	if len(data) == 0 {
		if typ != "" {
			return nil, "", fmt.Errorf("%w: crop_type %q names no crop", ErrInvalid, typ)
		}
		return nil, "", nil
	}
	if int64(len(data)) > artifacts.MaxCommentCropBytes {
		return nil, "", fmt.Errorf("%w: the crop is %d bytes and the most is %d", ErrInvalid,
			len(data), artifacts.MaxCommentCropBytes)
	}
	var width, height int
	switch typ {
	case "image/png", "image/webp":
		out, w, h, err := brandformat.Still(typ, data)
		if err != nil {
			return nil, "", fmt.Errorf("%w: the crop: %w", ErrInvalid, err)
		}
		data, width, height = out, w, h
	case "image/jpeg":
		if !bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}) {
			return nil, "", fmt.Errorf("%w: the crop is not a JPEG: it does not begin as one", ErrInvalid)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return nil, "", fmt.Errorf("%w: the crop's JPEG header cannot be read: %w", ErrInvalid, err)
		}
		width, height = cfg.Width, cfg.Height
	default:
		return nil, "", fmt.Errorf("%w: crop_type %q is not image/png, image/jpeg or image/webp",
			ErrInvalid, typ)
	}
	if width <= 0 || height <= 0 || max(width, height) > artifacts.MaxCommentCropSide {
		return nil, "", fmt.Errorf("%w: the crop is %dx%d pixels and its longer side may be at most %d",
			ErrInvalid, width, height, artifacts.MaxCommentCropSide)
	}
	return data, typ, nil
}

// checkThreadRef refuses a write that names no artifact or no thread.
func checkThreadRef(artifact artifacts.ID, thread uuid.UUID) error {
	switch {
	case artifact.IsZero():
		return fmt.Errorf("%w: a comment names its artifact", ErrInvalid)
	case thread == uuid.Nil:
		return fmt.Errorf("%w: a comment names its thread", ErrInvalid)
	}
	return nil
}

// checkRemarkRef refuses a write that names no remark.
func checkRemarkRef(ref RemarkRef) error {
	if err := checkThreadRef(ref.Artifact, ref.Thread); err != nil {
		return err
	}
	if ref.Remark == uuid.Nil {
		return fmt.Errorf("%w: a comment names its remark", ErrInvalid)
	}
	return nil
}

// ---- the snapshot's reads -------------------------------------------------- //

// liveHead is an artifact a comment may be written on: there, and not in the
// trash.
func liveHead(ctx context.Context, tx *sql.Tx, id string) (Head, error) {
	head, err := mustHead(ctx, tx, id)
	if err != nil {
		return Head{}, err
	}
	if head.Status == StatusTrashed {
		return Head{}, fmt.Errorf("%w: %s", ErrTrashed, id)
	}
	return head, nil
}

// madeOn is the version a thread is made on: the one asked, the head for
// zero, refused where the artifact does not keep it.
func madeOn(ctx context.Context, tx *sql.Tx, head Head, asked int) (int, error) {
	if asked == 0 {
		return head.HeadNumber, nil
	}
	if asked <= head.HeadNumber {
		kept, err := versionExists(ctx, tx, head.ID.String(), asked)
		if err != nil || kept {
			return asked, err
		}
	}
	return 0, fmt.Errorf("%w: %s: this artifact doesn't keep version %d", ErrNotFound, head.ID, asked)
}

// threadRow is one thread's row as a write decides from it.
type threadRow struct {
	version  int
	status   ThreadStatus
	position int64
}

// readThreadTx reads one thread's row, false when the artifact holds none.
func readThreadTx(ctx context.Context, tx *sql.Tx, artifact, thread string) (threadRow, bool, error) {
	var t threadRow
	var status string
	err := tx.QueryRowContext(ctx, `
		SELECT version, status, position FROM artifact_comment_threads
		WHERE artifact_id = ? AND id = ?`, artifact, thread).Scan(&t.version, &status, &t.position)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return threadRow{}, false, nil
	case err != nil:
		return threadRow{}, false, fmt.Errorf("artifactsdomain: read comment thread %s of %s: %w",
			thread, artifact, err)
	}
	t.status = ThreadStatus(status)
	return t, true, nil
}

// mustThread is one thread's row, or [ErrCommentNotFound].
func mustThread(ctx context.Context, tx *sql.Tx, artifact, thread string) (threadRow, error) {
	t, found, err := readThreadTx(ctx, tx, artifact, thread)
	if err != nil || found {
		return t, err
	}
	return threadRow{}, fmt.Errorf("%w: this artifact holds no comment thread %s", ErrCommentNotFound, thread)
}

// remarkRow is one remark's row as a write decides from it.
type remarkRow struct {
	author, authorKind, body string
	mentions                 []string
	removed                  bool
	position                 int64
}

// readRemarkTx reads one remark's row, false when the thread holds none.
func readRemarkTx(ctx context.Context, tx *sql.Tx, artifact, thread, remark string) (remarkRow, bool, error) {
	var (
		r        remarkRow
		mentions string
		removed  int
	)
	err := tx.QueryRowContext(ctx, `
		SELECT author, author_kind, body, mentions, removed, position FROM artifact_comments
		WHERE artifact_id = ? AND thread_id = ? AND id = ?`, artifact, thread, remark).Scan(
		&r.author, &r.authorKind, &r.body, &mentions, &removed, &r.position)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return remarkRow{}, false, nil
	case err != nil:
		return remarkRow{}, false, fmt.Errorf("artifactsdomain: read remark %s of %s: %w", remark, thread, err)
	}
	if r.mentions, err = decodeMentions(mentions); err != nil {
		return remarkRow{}, false, err
	}
	r.removed = removed != 0
	return r, true, nil
}

// mustRemark is one remark's row, or [ErrCommentNotFound]; a removed remark is
// found only where removed says it may be.
func mustRemark(ctx context.Context, tx *sql.Tx, artifact, thread, remark string, removed bool) (remarkRow, error) {
	r, found, err := readRemarkTx(ctx, tx, artifact, thread, remark)
	switch {
	case err != nil:
		return remarkRow{}, err
	case !found:
		return remarkRow{}, fmt.Errorf("%w: this thread holds no remark %s", ErrCommentNotFound, remark)
	case r.removed && !removed:
		return remarkRow{}, fmt.Errorf("%w: remark %s was removed", ErrCommentNotFound, remark)
	}
	return r, nil
}

// countThreads counts an artifact's threads.
func countThreads(ctx context.Context, tx *sql.Tx, artifact string) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_comment_threads WHERE artifact_id = ?`, artifact).Scan(&n); err != nil {
		return 0, fmt.Errorf("artifactsdomain: count %s's comment threads: %w", artifact, err)
	}
	return n, nil
}

// countRemarks counts one thread's remarks, removed ones included: a removed
// remark keeps its place.
func countRemarks(ctx context.Context, tx *sql.Tx, artifact, thread string) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM artifact_comments WHERE artifact_id = ? AND thread_id = ?`,
		artifact, thread).Scan(&n); err != nil {
		return 0, fmt.Errorf("artifactsdomain: count the remarks of %s: %w", thread, err)
	}
	return n, nil
}

// ---- the verdict ----------------------------------------------------------- //

// commentVerdict is what the apply of a comment record did, read back from the
// rows it writes: nil when the row moved to the record's position or past it,
// and the refusal the rows explain when it did not. remark is the remark the
// record writes, uuid.Nil for a resolve or a reopen, which write the thread;
// bound is the thread or remark bound an open or a reply was stamped with.
func (w *Writer) commentVerdict(ctx context.Context, id artifacts.ID, thread, remark uuid.UUID,
	op OpKind, res statelog.Result, bound int) error {

	if res.Outcome != statelog.OutcomeApplied || res.Position.IsZero() {
		return nil
	}
	packed := res.Position.Packed()
	artifact, threadID := id.String(), thread.String()
	var refused error
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		landed, err := commentLanded(ctx, tx, artifact, threadID, remark, op, packed)
		if err != nil || landed {
			return err
		}
		refused = commentRefusal(ctx, tx, artifact, threadID, op, bound)
		return nil
	})
	if err != nil {
		return fmt.Errorf("artifactsdomain: read what %s on %s did: %w", op, thread, err)
	}
	return refused
}

// commentLanded reports whether the row a comment record writes moved to its
// position or past it: an open's thread and a reply's remark exist once made,
// an edit's and a removal's remark and a resolve's and a reopen's thread carry
// the position, and a resolve's or a reopen's status says which.
func commentLanded(ctx context.Context, tx *sql.Tx, artifact, thread string, remark uuid.UUID,
	op OpKind, packed int64) (bool, error) {

	switch op {
	case OpCommentOpen:
		_, found, err := readThreadTx(ctx, tx, artifact, thread)
		return found, err
	case OpCommentReply, OpCommentEdit, OpCommentRemove:
		r, found, err := readRemarkTx(ctx, tx, artifact, thread, remark.String())
		if err != nil || !found {
			return false, err
		}
		return op == OpCommentReply || r.position >= packed, nil
	}
	t, found, err := readThreadTx(ctx, tx, artifact, thread)
	if err != nil || !found {
		return false, err
	}
	want := ThreadResolved
	if op == OpCommentReopen {
		want = ThreadOpen
	}
	return t.position >= packed && t.status == want, nil
}

// commentRefusal is why the apply refused a comment record, read from the rows
// it was decided by: the artifact gone or in the trash, the thread gone, or a
// bound the record was stamped with. A read that fails is answered as itself.
func commentRefusal(ctx context.Context, tx *sql.Tx, artifact, thread string, op OpKind, bound int) error {
	head, err := mustHead(ctx, tx, artifact)
	switch {
	case err != nil:
		return err
	case head.Status == StatusTrashed:
		return fmt.Errorf("%w: %s", ErrTrashed, artifact)
	}
	if op == OpCommentOpen {
		threads, err := countThreads(ctx, tx, artifact)
		switch {
		case err != nil:
			return err
		case threads >= bound:
			return fmt.Errorf("%w: this artifact keeps %d comment threads, the most it may",
				ErrCommentLimit, threads)
		}
		return fmt.Errorf("%w: the log refused this comment write as %s", ErrInvalid, op)
	}
	if _, err := mustThread(ctx, tx, artifact, thread); err != nil {
		return err
	}
	if op == OpCommentReply {
		remarks, err := countRemarks(ctx, tx, artifact, thread)
		switch {
		case err != nil:
			return err
		case remarks >= bound:
			return fmt.Errorf("%w: this thread holds %d remarks, the most one may", ErrCommentLimit,
				remarks)
		}
	}
	return fmt.Errorf("%w: the log refused this comment write as %s", ErrInvalid, op)
}

// decodeMentions reads a mentions column.
func decodeMentions(raw string) ([]string, error) {
	var out []string
	if raw == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("artifactsdomain: read a remark's mentions: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

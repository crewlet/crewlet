package artifactsapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/iam"
)

// Comment threads on an artifact (ADR-0153): six keyed writes routed by the
// agent tools' router, and the read placing each thread on a version through
// that version's anchor index, cached per version as a deck's read is.

// Comments are the artifacts' comment threads: the estate router's.
type Comments interface {
	Threads(ctx context.Context, id artifacts.ID) ([]artifactsdomain.CommentThread, error)
	Thread(ctx context.Context, id artifacts.ID, thread uuid.UUID) (artifactsdomain.CommentThread, error)
	OpenThread(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.ThreadInput) (
		artifactsdomain.Commented, error)
	Reply(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.ReplyInput) (
		artifactsdomain.Commented, error)
	EditRemark(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.RemarkEdit) (
		artifactsdomain.Written, error)
	RemoveRemark(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.RemarkRemoval) (
		artifactsdomain.Written, error)
	Resolve(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.ResolveInput) (
		artifactsdomain.Written, error)
	Reopen(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.ReopenInput) (
		artifactsdomain.Written, error)
}

// The comment writes' budget, per node: a principal's own bucket on one
// artifact, thirty writes refilled one per two seconds, and the artifact's,
// a hundred and twenty refilled two a second.
const (
	commentsPerPrincipalPerSecond = 0.5
	commentsPrincipalBurst        = 30
	commentsPerArtifactPerSecond  = 2
	commentsArtifactBurst         = 120
)

// newCommentLimiter builds the node's comment buckets on a clock.
func newCommentLimiter(now func() time.Time) *costlimit.Limiter {
	return costlimit.New(costlimit.Rates{
		PrincipalRate: commentsPerPrincipalPerSecond, PrincipalBurst: commentsPrincipalBurst,
		ScopeRate: commentsPerArtifactPerSecond, ScopeBurst: commentsArtifactBurst,
	}, now)
}

// maxCommentBodyBytes bounds a comment write's body: a remark and a quote as
// JSON strings, which escaping grows by up to six times, a crop's bytes in
// base64, and room for the anchor.
const maxCommentBodyBytes = 6*(artifacts.MaxCommentBytes+artifacts.MaxCommentQuoteBytes) +
	2*artifacts.MaxCommentCropBytes + 8<<10

// newIndexCache reads each version's anchor index by its artifact's kind.
func newIndexCache(objects Objects) *formatCache[*commentanchor.Index] {
	return newFormatCache(objects, "page", commentanchor.ErrInvalid,
		func(v artifactsdomain.Version) string { return v.Manifest.Entry },
		func(src manifestSource, v artifactsdomain.Version) (*commentanchor.Index, int64, error) {
			idx, err := commentanchor.Read(src.kind, src, v.Manifest)
			return idx, idx.Size() + 1, err
		})
}

// ---- the read ------------------------------------------------------------ //

// CommentsAnswer is every thread on an artifact placed on one version (the
// `artifact_comments` question, `GET /artifacts/{id}/comments`).
type CommentsAnswer struct {
	Artifact   artifacts.ID    `json:"artifact"`
	Version    int             `json:"version"`
	Head       int             `json:"head"`
	Counts     CommentCounts   `json:"counts"`
	CanComment bool            `json:"can_comment"`
	Role       artifacts.Role  `json:"role"`
	Threads    []CommentThread `json:"threads"`
}

// CommentCounts are the threads per filter: open and placed, open and gone
// from the version asked, and resolved.
type CommentCounts struct {
	Open     int `json:"open"`
	Resolved int `json:"resolved"`
	Outdated int `json:"outdated"`
}

// CommentThread is one thread as the question answers it.
type CommentThread struct {
	ID              uuid.UUID                    `json:"id"`
	Version         int                          `json:"version"`
	Anchor          commentanchor.Anchor         `json:"anchor"`
	Label           string                       `json:"label"`
	Placement       commentanchor.State          `json:"placement"`
	Place           *commentanchor.Anchor        `json:"place,omitempty"`
	Quote           string                       `json:"quote,omitempty"`
	Crop            string                       `json:"crop,omitempty"`
	Status          artifactsdomain.ThreadStatus `json:"status"`
	ResolvedBy      string                       `json:"resolved_by,omitempty"`
	ResolvedByKind  iam.ActorKind                `json:"resolved_by_kind,omitempty"`
	ResolvedAt      *time.Time                   `json:"resolved_at,omitempty"`
	ResolvedVersion int                          `json:"resolved_version,omitempty"`
	CreatedBy       string                       `json:"created_by"`
	CreatedByKind   iam.ActorKind                `json:"created_by_kind"`
	CreatedAt       time.Time                    `json:"created_at"`
	UpdatedAt       time.Time                    `json:"updated_at"`
	Remarks         []CommentRemark              `json:"remarks"`
}

// CommentRemark is one remark as the question answers it; a removed one keeps
// its place and author and says nothing.
type CommentRemark struct {
	ID         uuid.UUID     `json:"id"`
	Author     string        `json:"author"`
	AuthorKind iam.ActorKind `json:"author_kind"`
	Body       string        `json:"body"`
	Mentions   []string      `json:"mentions,omitempty"`
	ReplyTo    *uuid.UUID    `json:"reply_to,omitempty"`
	CreatedAt  time.Time     `json:"created_at"`
	EditedAt   *time.Time    `json:"edited_at,omitempty"`
	Removed    bool          `json:"removed,omitempty"`
}

// Comments answers every thread on artifact id placed on version (zero for the
// head), decided as a read of its content: a trashed artifact serves none.
func (s *Service) Comments(ctx context.Context, p iam.Principal, id artifacts.ID, version int) (
	CommentsAnswer, error) {

	a, err := s.gate.Content(ctx, p, authz.ActionArtifactRead, id, version)
	if err != nil {
		return CommentsAnswer{}, err
	}
	if version == 0 {
		version = a.Head.HeadNumber
	}
	idx, err := s.indexOf(ctx, a.Head, version)
	if err != nil {
		return CommentsAnswer{}, err
	}
	threads, err := s.comments.Threads(ctx, a.Head.ID)
	if err != nil {
		return CommentsAnswer{}, err
	}
	d := authz.Decide(ctx, p, authz.ActionArtifactComment, s.gate.object(a.Head), s.gate.Chart,
		s.gate.decidedAt(ctx))
	out := CommentsAnswer{Artifact: a.Head.ID, Version: version, Head: a.Head.HeadNumber,
		CanComment: d.Allowed, Role: a.Role(), Threads: make([]CommentThread, 0, len(threads))}
	order := idx.SlideOrder()
	for _, th := range threads {
		placed := commentanchor.Place(th.Anchor, th.Version, version, idx)
		switch {
		case th.Status == artifactsdomain.ThreadResolved:
			out.Counts.Resolved++
		case placed.State == commentanchor.StateOutdated:
			out.Counts.Outdated++
		default:
			out.Counts.Open++
		}
		out.Threads = append(out.Threads, threadAnswer(th, placed, order))
	}
	return out, nil
}

// threadAnswer is one thread placed: its label is read on the version asked.
func threadAnswer(th artifactsdomain.CommentThread, placed commentanchor.Placement,
	order []string) CommentThread {

	out := CommentThread{ID: th.ID, Version: th.Version, Anchor: th.Anchor,
		Label: commentanchor.Label(th.Anchor, order), Placement: placed.State, Place: placed.Anchor,
		Quote: th.Quote, Status: th.Status, CreatedBy: th.CreatedBy.Name, CreatedByKind: th.CreatedBy.Kind,
		CreatedAt: th.CreatedAt.UTC(), UpdatedAt: th.UpdatedAt.UTC(),
		Remarks: make([]CommentRemark, 0, len(th.Remarks))}
	if len(th.Crop) > 0 {
		out.Crop = "data:" + th.CropType + ";base64," + base64.StdEncoding.EncodeToString(th.Crop)
	}
	if th.Status == artifactsdomain.ThreadResolved {
		at := th.ResolvedAt.UTC()
		out.ResolvedBy, out.ResolvedByKind = th.ResolvedBy.Name, th.ResolvedBy.Kind
		out.ResolvedAt, out.ResolvedVersion = &at, th.ResolvedVersion
	}
	for _, rm := range th.Remarks {
		r := CommentRemark{ID: rm.ID, Author: rm.Author.Name, AuthorKind: rm.Author.Kind,
			Body: rm.Body, Mentions: rm.Mentions, CreatedAt: rm.CreatedAt.UTC(), Removed: rm.Removed}
		if rm.ReplyTo != uuid.Nil {
			replyTo := rm.ReplyTo
			r.ReplyTo = &replyTo
		}
		if !rm.EditedAt.IsZero() {
			edited := rm.EditedAt.UTC()
			r.EditedAt = &edited
		}
		out.Remarks = append(out.Remarks, r)
	}
	return out
}

// indexOf is version's anchor index for head's artifact, from this node's
// cache: nil, with no read, for a kind whose comments take the whole version.
func (s *Service) indexOf(ctx context.Context, head artifactsdomain.Head, version int) (
	*commentanchor.Index, error) {

	accepted, _ := commentanchor.Accepts(head.Kind)
	if !slices.ContainsFunc(accepted, func(k commentanchor.Kind) bool { return k != commentanchor.KindVersion }) {
		return nil, nil
	}
	v, err := s.reads.Version(ctx, head.ID, version)
	if err != nil {
		return nil, err
	}
	return s.indexes.get(ctx, head, v)
}

// slideOrder is the head's slide ids where the artifact is a deck, which a
// wake's label numbers a slide by. A head that could not be read labels its
// slide as removed rather than refusing the comment, and says so in the log.
func (s *Service) slideOrder(ctx context.Context, head artifactsdomain.Head) []string {
	if accepted, _ := commentanchor.Accepts(head.Kind); !slices.Contains(accepted, commentanchor.KindSlides) {
		return nil
	}
	idx, err := s.indexOf(ctx, head, head.HeadNumber)
	if err != nil {
		log.WarnContext(ctx, "artifact_comment_slides_unread", "artifact", head.ID.String(),
			"version", head.HeadNumber, "error", err.Error())
		return nil
	}
	return idx.SlideOrder()
}

// ---- the writes ---------------------------------------------------------- //

// commentWrite admits action on the path's artifact for a comment write: the
// gesture's admission and key, the artifact live, and the caller within the
// node's comment budget. It renders every refusal itself.
func (s *Service) commentWrite(w http.ResponseWriter, r *http.Request, action authz.Action) (
	Admission, artifactsdomain.Call, string, bool) {

	a, c, key, ok := s.gesture(w, r, action)
	if !ok {
		return Admission{}, artifactsdomain.Call{}, "", false
	}
	if a.Head.Status == artifactsdomain.StatusTrashed {
		trashed(w)
		return Admission{}, artifactsdomain.Call{}, "", false
	}
	if refused := s.commentBudget.Take(a.Head.ID.String(), iam.RecordOwner(a.Principal), 1); refused != nil {
		scope := "principal"
		if refused.Scope == costlimit.ScopeShared {
			scope = "artifact"
		}
		httpjson.ThrottledWith(w, httpjson.CodeRateLimited, refused.RetryAfter, httpjson.Detail{"scope": scope})
		return Admission{}, artifactsdomain.Call{}, "", false
	}
	return a, c, key, true
}

// pathUUID is the path's id under name, refusing one that is no id as a
// comment the artifact does not hold.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		gone(w, "this artifact holds no such comment")
		return uuid.Nil, false
	}
	return id, true
}

// routed is the routing snapshot of a write, or the refusal of one whose
// recipients could not be read, rendered: refused rather than told to nobody.
func (s *Service) routed(w http.ResponseWriter, r *http.Request, write builtin.CommentWrite) (
	artifactsdomain.CommentNotify, bool) {

	n, err := s.routing.Route(r.Context(), write)
	if err != nil {
		log.WarnContext(r.Context(), "artifact_comment_unrouted", "artifact", write.Head.ID.String(),
			"error", err.Error())
		unavailable(w, err, "this node could not read who the comment tells; nothing was written")
		return artifactsdomain.CommentNotify{}, false
	}
	return n, true
}

// openBody opens a thread.
type openBody struct {
	Body     string                `json:"body"`
	Version  int                   `json:"version,omitempty"`
	Anchor   *commentanchor.Anchor `json:"anchor,omitempty"`
	Quote    string                `json:"quote,omitempty"`
	Crop     string                `json:"crop,omitempty"`
	CropType string                `json:"crop_type,omitempty"`
}

// openComment opens a thread on the artifact (`POST /artifacts/{id}/comments`),
// decided by `comment_on_artifact`: a commenter's. The crop arrives as its
// bytes in base64, which the domain holds to its media type and size.
func (s *Service) openComment(w http.ResponseWriter, r *http.Request) {
	a, c, key, ok := s.commentWrite(w, r, authz.ActionArtifactComment)
	if !ok {
		return
	}
	var body openBody
	if !decodeWithin(w, r, &body, false, maxCommentBodyBytes) {
		return
	}
	crop, err := base64.StdEncoding.DecodeString(body.Crop)
	if err != nil {
		invalid(w, "crop is the picture's bytes in standard base64")
		return
	}
	anchor := commentanchor.Whole()
	if body.Anchor != nil {
		anchor = *body.Anchor
	}
	mentions := s.routing.Mention(body.Body)
	n, ok := s.routed(w, r, builtin.CommentWrite{Op: artifactsdomain.OpCommentOpen, Head: a.Head,
		Author: c.Authority.Actor, Mentions: mentions, Anchor: anchor, Version: body.Version,
		Quote: body.Quote, SlideOrder: s.slideOrder(r.Context(), a.Head)})
	if !ok {
		return
	}
	out, err := s.comments.OpenThread(r.Context(), c, artifactsdomain.ThreadInput{
		Artifact: a.Head.ID, Body: body.Body, Version: body.Version, Anchor: anchor, Quote: body.Quote,
		Crop: crop, CropType: body.CropType, Mentions: mentions, Notify: n,
	})
	if err != nil {
		writeFailed(w, r, key, "comment on the artifact", err)
		return
	}
	written(w, key, out.Result, httpjson.Detail{"artifact": a.Head.ID.String(),
		"thread": out.Thread.String(), "remark": out.Remark.String()})
}

// replyBody answers a thread.
type replyBody struct {
	Body    string     `json:"body"`
	ReplyTo *uuid.UUID `json:"reply_to,omitempty"`
}

// replyComment adds a remark to a thread (`POST
// /artifacts/{id}/comments/{thread}/replies`), decided by `comment_on_artifact`.
func (s *Service) replyComment(w http.ResponseWriter, r *http.Request) {
	thread, ok := pathUUID(w, r, "thread")
	if !ok {
		return
	}
	a, c, key, ok := s.commentWrite(w, r, authz.ActionArtifactComment)
	if !ok {
		return
	}
	var body replyBody
	if !decodeWithin(w, r, &body, false, maxCommentBodyBytes) {
		return
	}
	th, ok := s.thread(w, r, key, a.Head.ID, thread)
	if !ok {
		return
	}
	mentions := s.routing.Mention(body.Body)
	n, ok := s.routed(w, r, builtin.CommentWrite{Op: artifactsdomain.OpCommentReply, Head: a.Head,
		Author: c.Authority.Actor, Mentions: mentions, Thread: &th,
		SlideOrder: s.slideOrder(r.Context(), a.Head)})
	if !ok {
		return
	}
	in := artifactsdomain.ReplyInput{Artifact: a.Head.ID, Thread: thread, Body: body.Body,
		Mentions: mentions, Notify: n}
	if body.ReplyTo != nil {
		in.ReplyTo = *body.ReplyTo
	}
	out, err := s.comments.Reply(r.Context(), c, in)
	if err != nil {
		writeFailed(w, r, key, "reply in the thread", err)
		return
	}
	written(w, key, out.Result, httpjson.Detail{"artifact": a.Head.ID.String(),
		"thread": thread.String(), "remark": out.Remark.String()})
}

// thread reads the thread a write names, rendering a failure itself.
func (s *Service) thread(w http.ResponseWriter, r *http.Request, key string, id artifacts.ID,
	thread uuid.UUID) (artifactsdomain.CommentThread, bool) {

	th, err := s.comments.Thread(r.Context(), id, thread)
	if err != nil {
		writeFailed(w, r, key, "read the thread", err)
		return artifactsdomain.CommentThread{}, false
	}
	return th, true
}

// editBody is a remark's new words.
type editBody struct {
	Body string `json:"body"`
}

// editComment replaces one remark's words (`PATCH
// /artifacts/{id}/comments/{thread}/remarks/{remark}`), decided by
// `edit_artifact_comment`; the domain keeps an edit to the remark's author.
func (s *Service) editComment(w http.ResponseWriter, r *http.Request) {
	ref, ok := remarkRef(w, r)
	if !ok {
		return
	}
	a, c, key, ok := s.commentWrite(w, r, authz.ActionArtifactCommentEdit)
	if !ok {
		return
	}
	var body editBody
	if !decodeWithin(w, r, &body, false, maxCommentBodyBytes) {
		return
	}
	ref.Artifact = a.Head.ID
	out, err := s.comments.EditRemark(r.Context(), c, artifactsdomain.RemarkEdit{RemarkRef: ref,
		Body: body.Body, Mentions: s.routing.Mention(body.Body)})
	commentDone(w, r, key, "edit the remark", ref, out, err)
}

// removeComment takes one remark's words down (`DELETE
// /artifacts/{id}/comments/{thread}/remarks/{remark}`), decided by
// `remove_artifact_comment`: its author's, or an administrator's moderating.
func (s *Service) removeComment(w http.ResponseWriter, r *http.Request) {
	ref, ok := remarkRef(w, r)
	if !ok {
		return
	}
	a, c, key, ok := s.commentWrite(w, r, authz.ActionArtifactCommentRemove)
	if !ok {
		return
	}
	ref.Artifact = a.Head.ID
	out, err := s.comments.RemoveRemark(r.Context(), c, artifactsdomain.RemarkRemoval{RemarkRef: ref,
		Moderated: a.Principal.Can(iam.GrantAdmin)})
	commentDone(w, r, key, "remove the remark", ref, out, err)
}

// remarkRef is the thread and the remark the path names.
func remarkRef(w http.ResponseWriter, r *http.Request) (artifactsdomain.RemarkRef, bool) {
	thread, ok := pathUUID(w, r, "thread")
	if !ok {
		return artifactsdomain.RemarkRef{}, false
	}
	remark, ok := pathUUID(w, r, "remark")
	if !ok {
		return artifactsdomain.RemarkRef{}, false
	}
	return artifactsdomain.RemarkRef{Thread: thread, Remark: remark}, true
}

// commentDone answers a remark's or a thread's write.
func commentDone(w http.ResponseWriter, r *http.Request, key, verb string, ref artifactsdomain.RemarkRef,
	out artifactsdomain.Written, err error) {

	if err != nil {
		writeFailed(w, r, key, verb, err)
		return
	}
	detail := httpjson.Detail{"artifact": ref.Artifact.String(), "thread": ref.Thread.String()}
	if ref.Remark != uuid.Nil {
		detail["remark"] = ref.Remark.String()
	}
	written(w, key, out.Result, detail)
}

// resolveBody names the version that addressed a thread; zero is the head.
type resolveBody struct {
	Version int `json:"version,omitempty"`
}

// resolveComment resolves a thread (`POST
// /artifacts/{id}/comments/{thread}/resolve`), decided by
// `resolve_artifact_comment`: a commenter's.
func (s *Service) resolveComment(w http.ResponseWriter, r *http.Request) {
	s.threadStatus(w, r, false)
}

// reopenComment reopens a resolved thread (`POST
// /artifacts/{id}/comments/{thread}/reopen`), decided as a resolve is.
func (s *Service) reopenComment(w http.ResponseWriter, r *http.Request) {
	s.threadStatus(w, r, true)
}

// threadStatus resolves or reopens the path's thread.
func (s *Service) threadStatus(w http.ResponseWriter, r *http.Request, reopen bool) {
	thread, ok := pathUUID(w, r, "thread")
	if !ok {
		return
	}
	a, c, key, ok := s.commentWrite(w, r, authz.ActionArtifactCommentResolve)
	if !ok {
		return
	}
	var body resolveBody
	if !decodeWithin(w, r, &body, true, maxCommentBodyBytes) {
		return
	}
	if reopen && body.Version != 0 {
		invalid(w, "a reopen names no version")
		return
	}
	th, ok := s.thread(w, r, key, a.Head.ID, thread)
	if !ok {
		return
	}
	op := artifactsdomain.OpCommentResolve
	if reopen {
		op = artifactsdomain.OpCommentReopen
	}
	n, ok := s.routed(w, r, builtin.CommentWrite{Op: op, Head: a.Head, Author: c.Authority.Actor,
		Thread: &th, SlideOrder: s.slideOrder(r.Context(), a.Head)})
	if !ok {
		return
	}
	ref := artifactsdomain.RemarkRef{Artifact: a.Head.ID, Thread: thread}
	var (
		out artifactsdomain.Written
		err error
	)
	if reopen {
		out, err = s.comments.Reopen(r.Context(), c, artifactsdomain.ReopenInput{Artifact: a.Head.ID,
			Thread: thread, Notify: n})
		commentDone(w, r, key, "reopen the thread", ref, out, err)
		return
	}
	out, err = s.comments.Resolve(r.Context(), c, artifactsdomain.ResolveInput{Artifact: a.Head.ID,
		Thread: thread, Version: body.Version, Notify: n})
	commentDone(w, r, key, "resolve the thread", ref, out, err)
}

// errNoComments is a surface built with no comment threads to read.
var errNoComments = errors.New("artifactsapi: no comment threads: every comment is read and " +
	"written through them")

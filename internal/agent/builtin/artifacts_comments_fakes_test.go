package builtin_test

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/commentanchor"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// comment holds a thread on artifact id opened by author, on version 1 at
// anchor, quoting quote, its remarks bodies each by the author before it.
func (f *fakeArtifacts) comment(id artifacts.ID, anchor commentanchor.Anchor, quote string,
	remarks ...artifactsdomain.Remark) artifactsdomain.CommentThread {

	f.mu.Lock()
	defer f.mu.Unlock()
	th := artifactsdomain.CommentThread{Artifact: id,
		ID:      uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("thread-%s-%d", id, len(f.threads[id])))),
		Version: 1, Anchor: anchor, Quote: quote, Status: artifactsdomain.ThreadOpen,
		CreatedAt: fakeAt, UpdatedAt: fakeAt}
	for i, rm := range remarks {
		rm.ID = uuid.NewSHA1(th.ID, []byte(fmt.Sprint(i)))
		rm.CreatedAt = fakeAt
		th.Remarks = append(th.Remarks, rm)
	}
	if len(remarks) > 0 {
		th.CreatedBy = remarks[0].Author
	}
	f.threads[id] = append(f.threads[id], th)
	return th
}

func (f *fakeArtifacts) Threads(_ context.Context, id artifacts.ID) ([]artifactsdomain.CommentThread, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.heads[id]; !ok {
		return nil, fmt.Errorf("%w: %s", artifactsdomain.ErrNotFound, id)
	}
	return slices.Clone(f.threads[id]), nil
}

func (f *fakeArtifacts) Thread(_ context.Context, id artifacts.ID, thread uuid.UUID) (
	artifactsdomain.CommentThread, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, th := range f.threads[id] {
		if th.ID == thread {
			return th, nil
		}
	}
	return artifactsdomain.CommentThread{}, fmt.Errorf("%w: %s", artifactsdomain.ErrCommentNotFound, thread)
}

func (f *fakeArtifacts) OpenThread(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ThreadInput) (
	artifactsdomain.Commented, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCalled = append(f.commentCalled, c)
	f.opened = append(f.opened, in)
	if h := f.heads[in.Artifact]; h.Status == artifactsdomain.StatusTrashed {
		return artifactsdomain.Commented{}, fmt.Errorf("%w: %s", artifactsdomain.ErrTrashed, in.Artifact)
	}
	thread := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("opened-%d", len(f.opened))))
	remark := uuid.NewSHA1(thread, []byte("0"))
	f.threads[in.Artifact] = append(f.threads[in.Artifact], artifactsdomain.CommentThread{
		Artifact: in.Artifact, ID: thread, Version: max(in.Version, 1), Anchor: in.Anchor, Quote: in.Quote,
		Status: artifactsdomain.ThreadOpen, CreatedBy: c.Authority.Actor,
		Remarks: []artifactsdomain.Remark{{ID: remark, Author: c.Authority.Actor, Body: in.Body}}})
	return artifactsdomain.Commented{Thread: thread, Remark: remark, Result: f.result()}, nil
}

func (f *fakeArtifacts) Reply(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ReplyInput) (
	artifactsdomain.Commented, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCalled = append(f.commentCalled, c)
	f.replied = append(f.replied, in)
	remark := uuid.NewSHA1(in.Thread, []byte(fmt.Sprintf("reply-%d", len(f.replied))))
	return artifactsdomain.Commented{Thread: in.Thread, Remark: remark, Result: f.result()}, nil
}

func (f *fakeArtifacts) EditRemark(_ context.Context, c artifactsdomain.Call, in artifactsdomain.RemarkEdit) (
	artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCalled = append(f.commentCalled, c)
	f.edited = append(f.edited, in)
	return artifactsdomain.Written{Result: f.result()}, nil
}

func (f *fakeArtifacts) RemoveRemark(_ context.Context, c artifactsdomain.Call, in artifactsdomain.RemarkRemoval) (
	artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCalled = append(f.commentCalled, c)
	f.removed = append(f.removed, in)
	return artifactsdomain.Written{Result: f.result()}, nil
}

func (f *fakeArtifacts) Resolve(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ResolveInput) (
	artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCalled = append(f.commentCalled, c)
	f.resolved = append(f.resolved, in)
	return artifactsdomain.Written{Result: f.result()}, nil
}

func (f *fakeArtifacts) Reopen(_ context.Context, c artifactsdomain.Call, in artifactsdomain.ReopenInput) (
	artifactsdomain.Written, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.commentCalled = append(f.commentCalled, c)
	f.reopened = append(f.reopened, in)
	return artifactsdomain.Written{Result: f.result()}, nil
}

// remark is one remark by author saying body.
func remark(author string, kind iam.ActorKind, body string) artifactsdomain.Remark {
	return artifactsdomain.Remark{Author: iam.Actor{Name: author, Kind: kind}, Body: body}
}

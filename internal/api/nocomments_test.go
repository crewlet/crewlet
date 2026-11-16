package api_test

import (
	"context"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// noComments is an artifacts estate holding no comment thread and taking no
// comment write, for a surface whose case is about something else.
type noComments struct{}

func (noComments) Threads(context.Context, artifacts.ID) ([]artifactsdomain.CommentThread, error) {
	return []artifactsdomain.CommentThread{}, nil
}

func (noComments) Thread(context.Context, artifacts.ID, uuid.UUID) (artifactsdomain.CommentThread, error) {
	return artifactsdomain.CommentThread{}, artifactsdomain.ErrCommentNotFound
}

func (noComments) OpenThread(context.Context, artifactsdomain.Call, artifactsdomain.ThreadInput) (
	artifactsdomain.Commented, error) {
	return artifactsdomain.Commented{}, artifactsdomain.ErrInvalid
}

func (noComments) Reply(context.Context, artifactsdomain.Call, artifactsdomain.ReplyInput) (
	artifactsdomain.Commented, error) {
	return artifactsdomain.Commented{}, artifactsdomain.ErrCommentNotFound
}

func (noComments) EditRemark(context.Context, artifactsdomain.Call, artifactsdomain.RemarkEdit) (
	artifactsdomain.Written, error) {
	return artifactsdomain.Written{}, artifactsdomain.ErrCommentNotFound
}

func (noComments) RemoveRemark(context.Context, artifactsdomain.Call, artifactsdomain.RemarkRemoval) (
	artifactsdomain.Written, error) {
	return artifactsdomain.Written{}, artifactsdomain.ErrCommentNotFound
}

func (noComments) Resolve(context.Context, artifactsdomain.Call, artifactsdomain.ResolveInput) (
	artifactsdomain.Written, error) {
	return artifactsdomain.Written{}, artifactsdomain.ErrCommentNotFound
}

func (noComments) Reopen(context.Context, artifactsdomain.Call, artifactsdomain.ReopenInput) (
	artifactsdomain.Written, error) {
	return artifactsdomain.Written{}, artifactsdomain.ErrCommentNotFound
}

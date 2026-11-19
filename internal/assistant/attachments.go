package assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// Files is where a conversation's files are kept: the images a person
// attaches, sealed and owner-keyed like the rest of a conversation (ADR-0053),
// and purged with it. A draft change that names a file is held to it too
// ([turn.draftChange]).
type Files interface {
	// Store keeps one file, refusing one past [wire.FileMaxBytes]. A file a
	// condition of the node kept out is answered as [*Unkept].
	Store(ctx context.Context, w Write, f NewFile) (wire.FileRef, error)
	// Open is one file and its bytes, read as its owner: another person's,
	// or one of another conversation, is [ErrNotFound].
	Open(ctx context.Context, owner, conversation, file uuid.UUID) (StoredFile, []byte, error)
	// Stat is one file's record without its bytes, by the same rule as Open.
	Stat(ctx context.Context, owner, conversation, file uuid.UUID) (StoredFile, error)
}

// Unkept is a file [Files.Store] did not keep because of a condition of the
// node: the company's file storage full, a store that did not take the bytes,
// a log that refused the file's record. Said is that condition as a model is
// told it, a clause with nothing of the error's own text, and Cause is the
// failure itself, beneath for the log and for the surface that tells a person
// (the upload's route answers it through the cause).
//
// WORDED BY WHOEVER STORES THE FILE (internal/engine, through the condition
// table internal/agent/builtin keeps), because nothing under
// internal/assistant may reach that table.
type Unkept struct {
	Said  string
	Cause error
}

func (e *Unkept) Error() string { return fmt.Sprintf("assistant: file not kept: %v", e.Cause) }

// Unwrap makes [Unkept] errors.Is-comparable with its cause.
func (e *Unkept) Unwrap() error { return e.Cause }

// NewFile is one file to keep.
type NewFile struct {
	ID           uuid.UUID
	Conversation uuid.UUID
	Turn         uuid.UUID
	Origin       wire.FileOrigin
	Name         string
	// MediaType is the type [SniffFile] answered for Content.
	MediaType string
	Content   []byte
}

// StoredFile is a kept file's record.
type StoredFile struct {
	wire.FileRef
	Owner        uuid.UUID
	Conversation uuid.UUID
	Turn         uuid.UUID
	Origin       wire.FileOrigin
	CreatedAt    time.Time
}

// attachmentsOf are the files a question names, each one the person uploaded
// to this conversation as an attachment, or the refusal of the question.
func (s *Service) attachmentsOf(ctx context.Context, ts TurnStart) ([]StoredFile, error) {
	if len(ts.Attachments) == 0 {
		return nil, nil
	}
	if s.files == nil {
		return nil, invalid("attachments are not taken on this node: ask without them")
	}
	if len(ts.Attachments) > wire.AttachmentsPerTurn {
		return nil, invalid("a question carries at most %d attachments, and this one names %d",
			wire.AttachmentsPerTurn, len(ts.Attachments))
	}
	out := make([]StoredFile, 0, len(ts.Attachments))
	for _, id := range ts.Attachments {
		if slices.ContainsFunc(out, func(f StoredFile) bool { return f.FileID == id }) {
			continue
		}
		f, err := s.files.Stat(ctx, ts.Person.ID, ts.Conversation, id)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil, invalid("attachment %s is no file of this conversation: upload it again", id)
		case err != nil:
			return nil, fmt.Errorf("assistant: attachment %s: %w", id, err)
		case f.Origin != wire.FileAttached || !slices.Contains(wire.AttachmentMediaTypes, f.MediaType):
			return nil, invalid("attachment %s is not an image the person attached", id)
		}
		out = append(out, f)
	}
	return out, nil
}

// refsOf are files as a prompt and a message name them.
func refsOf(files []StoredFile) []wire.FileRef {
	out := make([]wire.FileRef, 0, len(files))
	for _, f := range files {
		out = append(out, f.FileRef)
	}
	return out
}

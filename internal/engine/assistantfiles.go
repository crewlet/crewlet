package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// THE ASSISTANT'S FILE STORE, wired: a conversation's files sealed into the
// fleet's object store and named by the assistant domain's rows
// (assistantdomain.FileStore), adapted to the seam internal/assistant declares.

// AssistantFileStore is the domain's file store over this node's assistant
// writer and reader and its object store, or nil on a node missing any of the
// three: a node with no object store keeps no file, so its assistant takes no
// attachment and offers no deep work.
func AssistantFileStore(e *Engine) *assistantdomain.FileStore {
	w, r, objects := e.AssistantWriter(), e.AssistantReader(), e.Objects()
	if w == nil || r == nil || objects == nil {
		return nil
	}
	store, err := assistantdomain.NewFileStore(w, r, objects)
	if err != nil {
		// UNREACHABLE while NewFileStore refuses only the three checked
		// above; logged rather than dropped, should it ever refuse more.
		log.Error("assistant_files_not_built", "error", err)
		return nil
	}
	return store
}

// AssistantFiles is the assistant's file store as the orchestrator calls it,
// or nil where [AssistantFileStore] is.
func AssistantFiles(e *Engine) assistant.Files {
	store := AssistantFileStore(e)
	if store == nil {
		return nil
	}
	return assistantFiles{store: store}
}

// assistantFiles adapts the domain's file store: the operation a file's record
// is published under is derived from the gesture's key, and the domain's
// refusals are told as the orchestrator's.
type assistantFiles struct{ store *assistantdomain.FileStore }

func (a assistantFiles) Store(ctx context.Context, w assistant.Write,
	f assistant.NewFile) (wire.FileRef, error) {
	c, err := assistantCall(w)
	if err != nil {
		return wire.FileRef{}, err
	}
	stored, res, err := a.store.Store(ctx, c, assistantdomain.NewFile{
		ID: f.ID, Conversation: f.Conversation, Turn: f.Turn, Origin: f.Origin,
		Name: f.Name, MediaType: f.MediaType, Content: f.Content,
	})
	if err = settled(res, err); err != nil {
		return wire.FileRef{}, unkept(fileRefusal(err))
	}
	return stored.Ref(), nil
}

func (a assistantFiles) Open(ctx context.Context, owner, conversation,
	file uuid.UUID) (assistant.StoredFile, []byte, error) {
	f, content, err := a.store.Open(ctx, owner, conversation, file)
	if err != nil {
		return assistant.StoredFile{}, nil, fileRefusal(err)
	}
	return assistantStoredFile(f), content, nil
}

func (a assistantFiles) Stat(ctx context.Context, owner, conversation,
	file uuid.UUID) (assistant.StoredFile, error) {
	f, err := a.store.Stat(ctx, owner, conversation, file)
	if err != nil {
		return assistant.StoredFile{}, fileRefusal(err)
	}
	return assistantStoredFile(f), nil
}

// assistantStoredFile is a kept file's record as the orchestrator reads it.
func assistantStoredFile(f assistantdomain.File) assistant.StoredFile {
	return assistant.StoredFile{FileRef: f.Ref(), Owner: f.Owner,
		Conversation: f.Conversation, Turn: f.Turn, Origin: f.Origin, CreatedAt: f.CreatedAt}
}

// unkept is a file the store did not keep, as the orchestrator is told it: a
// condition of the node (the company's file storage full, a store that did not
// take the bytes, a log that refused the file's record) carries the words a
// model is told it in ([assistant.Unkept], from [builtin.Condition], which
// nothing under internal/assistant may reach), its error kept beneath for the
// route that tells a person; anything else is returned as it is.
func unkept(err error) error {
	if said, ok := builtin.Condition(err); ok {
		return &assistant.Unkept{Said: said, Cause: err}
	}
	return err
}

// fileRefusal is a domain refusal told as the orchestrator's sentinel, the
// domain's kept beside it so a surface can tell a full conversation from a
// file refused as written.
func fileRefusal(err error) error {
	switch {
	case errors.Is(err, assistantdomain.ErrNotFound), errors.Is(err, assistantdomain.ErrPurged),
		errors.Is(err, assistantdomain.ErrErased):
		return fmt.Errorf("%w: %w", assistant.ErrNotFound, err)
	case errors.Is(err, assistantdomain.ErrTooManyFiles):
		return fmt.Errorf("%w: %w", assistant.ErrInvalid, err)
	case errors.Is(err, assistantdomain.ErrInvalid):
		return fmt.Errorf("%w: %s", assistant.ErrInvalid,
			strings.TrimPrefix(err.Error(), assistantdomain.ErrInvalid.Error()+": "))
	}
	return err
}

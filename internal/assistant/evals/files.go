package evals

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// Files is a conversation's file store in memory, keyed on the owner as the
// real one is: another person's file, or one of another conversation, is not
// found.
type Files struct {
	now func() time.Time

	mu    sync.Mutex
	files map[uuid.UUID]storedFile
	// refuse, when set, is what every later Store answers.
	refuse error
}

type storedFile struct {
	assistant.StoredFile
	content []byte
}

// NewFiles is an empty store.
func NewFiles(now func() time.Time) *Files {
	return &Files{now: now, files: map[uuid.UUID]storedFile{}}
}

var _ assistant.Files = (*Files)(nil)

// Store keeps one file under the writer's owner.
func (f *Files) Store(_ context.Context, w assistant.Write, n assistant.NewFile) (wire.FileRef, error) {
	if len(n.Content) > wire.FileMaxBytes {
		return wire.FileRef{}, fmt.Errorf("%w: %d bytes", assistant.ErrFileRefused, len(n.Content))
	}
	ref := wire.FileRef{FileID: n.ID, Name: n.Name, MediaType: n.MediaType,
		Size: int64(len(n.Content)), Download: wire.FilePath(n.Conversation, n.ID)}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return wire.FileRef{}, f.refuse
	}
	f.files[n.ID] = storedFile{StoredFile: assistant.StoredFile{FileRef: ref, Owner: w.Owner,
		Conversation: n.Conversation, Turn: n.Turn, Origin: n.Origin, CreatedAt: f.now()},
		content: slices.Clone(n.Content)}
	return ref, nil
}

// Open is one file and its bytes, as its owner.
func (f *Files) Open(ctx context.Context, owner, conversation, file uuid.UUID) (assistant.StoredFile, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.files[file]
	if !ok || held.Owner != owner || held.Conversation != conversation {
		return assistant.StoredFile{}, nil, assistant.ErrNotFound
	}
	return held.StoredFile, slices.Clone(held.content), nil
}

// Stat is one file's record, as its owner.
func (f *Files) Stat(ctx context.Context, owner, conversation, file uuid.UUID) (assistant.StoredFile, error) {
	held, _, err := f.Open(ctx, owner, conversation, file)
	return held, err
}

// RefuseStores makes every later Store fail with err, as a store that keeps
// nothing more does.
func (f *Files) RefuseStores(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse = err
}

// Attach keeps an image the person uploaded, as the upload route does.
func (f *Files) Attach(owner, conversation uuid.UUID, name string, content []byte) (wire.FileRef, error) {
	mediaType, kept, err := assistant.SniffFile(name, content, wire.AttachmentMediaTypes)
	if err != nil {
		return wire.FileRef{}, err
	}
	return f.Store(context.Background(), assistant.Write{Owner: owner}, assistant.NewFile{
		ID: ID("attachment/" + kept), Conversation: conversation, Origin: wire.FileAttached,
		Name: kept, MediaType: mediaType, Content: content})
}

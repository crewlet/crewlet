package assistant

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// Files is the store of the files a person's assistant runs make, as a draft
// change reads it: whether a file is the owner's, in one conversation.
type Files interface {
	Owns(ctx context.Context, owner, conversation, file uuid.UUID) (bool, error)
}

// errNoFiles is a draft change naming a file on an engine that keeps none.
var errNoFiles = fmt.Errorf("%w: this engine keeps no assistant files, so a file cannot be "+
	"proposed", catalog.ErrInvalidArguments)

// draftChange holds a draft.set call to the draft the turn's page declared
// ([catalog.DraftValues]) and every file it names to the person's own files
// in this conversation, before anything is previewed: the page parses again,
// and a file the engine did not check never reaches it.
func (t *turn) draftChange(ctx context.Context, args map[string]any) error {
	var draft *wire.PageDraft
	if t.page != nil {
		draft = t.page.Draft
	}
	refs, err := catalog.DraftValues(args, draft, t.conversation)
	if err != nil || len(refs) == 0 {
		return err
	}
	if t.svc.files == nil {
		return errNoFiles
	}
	for _, ref := range refs {
		owned, err := t.svc.files.Owns(ctx, t.person.ID, t.conversation, ref.FileID)
		switch {
		case err != nil:
			return fmt.Errorf("the file %s could not be checked: %w", ref.Name, err)
		case !owned:
			return fmt.Errorf("%w: %s is not one of your files in this conversation",
				catalog.ErrInvalidArguments, ref.Name)
		}
	}
	return nil
}

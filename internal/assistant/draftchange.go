package assistant

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// errNoFiles is a draft change naming a file on an engine that keeps none.
var errNoFiles = fmt.Errorf("%w: this engine keeps no Ask Crewlet files, so a file cannot be "+
	"proposed", catalog.ErrInvalidArguments)

// draftChange holds a draft.set call to the draft the turn's page declared
// ([catalog.DraftValues]) and resolves every file it names, by id alone,
// through the person's own files in this conversation, before anything is
// previewed: the engine fills in the reference (the conversation, the name,
// the media type) from its record and writes it into the call, so the page
// parses a reference the engine checked and the model never echoes an id the
// engine already knows.
func (t *turn) draftChange(ctx context.Context, args map[string]any) error {
	var draft *wire.PageDraft
	if t.page != nil {
		draft = t.page.Draft
	}
	named, err := catalog.DraftValues(args, draft, t.conversation)
	if err != nil || len(named) == 0 {
		return err
	}
	if t.svc.files == nil {
		return errNoFiles
	}
	for _, f := range named {
		stored, err := t.svc.files.Stat(ctx, t.person.ID, t.conversation, f.FileID)
		switch {
		case errors.Is(err, ErrNotFound):
			return fmt.Errorf("%w: file %s is not one of your files in this conversation",
				catalog.ErrInvalidArguments, f.FileID)
		case err != nil:
			return fmt.Errorf("the file %s could not be checked: %w", f.FileID, err)
		}
		ref := wire.DraftFileRef{Kind: wire.DraftFieldFile, FileID: f.FileID,
			ConversationID: t.conversation, Name: stored.Name, MediaType: stored.MediaType}
		if err := catalog.SetDraftFile(args, f.Path, ref); err != nil {
			return err
		}
	}
	return nil
}

// draftRows names a draft.set preview's rows as the screen names its fields:
// each row whose field is a path of the turn's open draft takes that field's
// label ("Dark ground colour", never "colors.dark_ground"), and a value for a
// list field is drawn as its items. The labels are stored with the card, so a
// card read after the screen has closed still says them.
func draftRows(changes []wire.Change, draft *wire.PageDraft) []wire.Change {
	if draft == nil {
		return changes
	}
	fields := make(map[string]wire.DraftField, len(draft.Fields))
	for _, f := range draft.Fields {
		fields[f.Path] = f
	}
	out := slices.Clone(changes)
	for i, row := range out {
		f, ok := fields[row.Field]
		if !ok {
			continue
		}
		if label := strings.TrimSpace(f.Label); label != "" {
			out[i].Label = label
		}
		// A LIST MAY ARRIVE AS ITS TEXT, one item per line, as the page
		// context shows it.
		if f.Kind == wire.DraftFieldList && len(row.Items) == 0 && row.To != "" {
			for _, item := range strings.Split(row.To, "\n") {
				if item = strings.TrimSpace(item); item != "" {
					out[i].Items = append(out[i].Items, item)
				}
			}
		}
	}
	return out
}

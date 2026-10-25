package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// DraftValues holds a draft.set call to the draft the turn's page declared,
// BEFORE anything is previewed or recorded: every path one of the draft's
// editable fields (an unknown path is refused, never ignored), every value in
// its field's shape (text, a list, or for a file field a [wire.DraftFileRef]
// to a file of this conversation), and nothing beside `values`. It answers
// the file references, which the caller checks are the person's.
//
// THE SCREEN PARSES AGAIN before it previews or applies (its own field set
// and rules), so this is the engine's half of one rule: a proposal the page
// could only refuse is never put in front of the person, and a file the
// engine did not check never reaches the page.
func DraftValues(args Args, draft *wire.PageDraft, conversation uuid.UUID) ([]wire.DraftFileRef, error) {
	if draft == nil {
		return nil, fmt.Errorf("%w: the page has no open draft to change", ErrInvalidArguments)
	}
	payload, ok := args["args"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: draft.set takes {\"values\": {\"<path>\": <value>}}",
			ErrInvalidArguments)
	}
	values, ok := payload["values"].(map[string]any)
	switch {
	case !ok || len(payload) != 1:
		return nil, fmt.Errorf("%w: draft.set takes {\"values\": {\"<path>\": <value>}} and "+
			"nothing beside it", ErrInvalidArguments)
	case len(values) == 0:
		return nil, fmt.Errorf("%w: draft.set names no field", ErrInvalidArguments)
	case len(values) > wire.PageDraftMaxFields:
		return nil, fmt.Errorf("%w: draft.set names %d fields, over %d", ErrInvalidArguments,
			len(values), wire.PageDraftMaxFields)
	}
	var refs []wire.DraftFileRef
	for _, path := range sortedKeys(values) {
		i := slices.IndexFunc(draft.Fields, func(f wire.DraftField) bool { return f.Path == path })
		if i < 0 {
			return nil, fmt.Errorf("%w: the open draft has no field %q", ErrInvalidArguments, path)
		}
		field := draft.Fields[i]
		if !field.Editable {
			return nil, fmt.Errorf("%w: %q is not a field the screen lets you set",
				ErrInvalidArguments, path)
		}
		ref, err := draftValue(field, values[path], conversation)
		if err != nil {
			return nil, err
		}
		if ref != nil {
			refs = append(refs, *ref)
		}
	}
	return refs, nil
}

// draftValue holds one value to its field's shape, answering the file
// reference a file field takes.
func draftValue(field wire.DraftField, value any, conversation uuid.UUID) (*wire.DraftFileRef, error) {
	switch field.Kind {
	case wire.DraftFieldText:
		if _, ok := value.(string); ok {
			return nil, nil
		}
	case wire.DraftFieldList:
		switch v := value.(type) {
		case string:
			return nil, nil
		case []any:
			if !slices.ContainsFunc(v, func(item any) bool { _, ok := item.(string); return !ok }) {
				return nil, nil
			}
		}
	case wire.DraftFieldFile:
		// A REFERENCE, DECODED STRICTLY: markup or a name where the reference
		// belongs is refused rather than read as one.
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%w: %q takes a file reference", ErrInvalidArguments, field.Path)
		}
		var ref wire.DraftFileRef
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ref); err != nil {
			return nil, fmt.Errorf("%w: %q takes a file reference {\"kind\": \"file\", "+
				"\"file_id\", \"conversation_id\", \"name\", \"media_type\"}", ErrInvalidArguments,
				field.Path)
		}
		if err := ref.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %q: %w", ErrInvalidArguments, field.Path, err)
		}
		if ref.ConversationID != conversation {
			return nil, fmt.Errorf("%w: %q names a file of another conversation",
				ErrInvalidArguments, field.Path)
		}
		return &ref, nil
	}
	return nil, fmt.Errorf("%w: %q takes a %s value", ErrInvalidArguments, field.Path, field.Kind)
}

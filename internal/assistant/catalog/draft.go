package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// DraftFile is a file field a draft.set call names: the field's path and the
// file, by id alone. The engine resolves the rest of the reference (its
// conversation, name and media type) from its own record of the file, so the
// model never has to echo an id the engine already knows.
type DraftFile struct {
	Path   string
	FileID uuid.UUID
}

// DraftValues holds a draft.set call to the draft the turn's page declared,
// BEFORE anything is previewed or recorded: every path one of the draft's
// editable fields (an unknown path is refused, never ignored), every value in
// its field's shape (text, a list, or for a file field the id of a file of
// this conversation), and nothing beside `values`. It answers the file
// fields, which the caller resolves through the person's own files and
// writes back with [SetDraftFile].
//
// THE SCREEN PARSES AGAIN before it previews or applies (its own field set
// and rules), so this is the engine's half of one rule: a proposal the page
// could only refuse is never put in front of the person, and a file the
// engine did not check never reaches the page.
func DraftValues(args Args, draft *wire.PageDraft, conversation uuid.UUID) ([]DraftFile, error) {
	if draft == nil {
		return nil, fmt.Errorf("%w: the page has no open draft to change", ErrInvalidArguments)
	}
	values, err := draftSetValues(args)
	if err != nil {
		return nil, err
	}
	var refs []DraftFile
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
		file, err := draftValue(field, values[path], conversation)
		if err != nil {
			return nil, err
		}
		if file != uuid.Nil {
			refs = append(refs, DraftFile{Path: path, FileID: file})
		}
	}
	return refs, nil
}

// draftSetValues is a draft.set call's `values`, held to its envelope.
func draftSetValues(args Args) (map[string]any, error) {
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
	return values, nil
}

// SetDraftFile writes the engine's resolved reference into a draft.set call
// already held by [DraftValues], in place of what the model named, so the
// preview, the digest and the page all see the one reference the engine
// checked.
func SetDraftFile(args Args, path string, ref wire.DraftFileRef) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInvalidArguments, path, err)
	}
	values, err := draftSetValues(args)
	if err != nil {
		return err
	}
	values[path] = map[string]any{"kind": string(ref.Kind), "file_id": ref.FileID.String(),
		"conversation_id": ref.ConversationID.String(), "name": ref.Name,
		"media_type": ref.MediaType}
	return nil
}

// draftFileArg is what a model may write for a file field: the file's id,
// and optionally the rest of a reference the engine already holds. Every
// field but the id is checked against the engine's record, never trusted.
type draftFileArg struct {
	Kind           wire.DraftFieldKind `json:"kind"`
	FileID         uuid.UUID           `json:"file_id"`
	ConversationID uuid.UUID           `json:"conversation_id"`
	Name           string              `json:"name"`
	MediaType      string              `json:"media_type"`
}

// draftValue holds one value to its field's shape, answering the file a file
// field names (uuid.Nil for any other field).
func draftValue(field wire.DraftField, value any, conversation uuid.UUID) (uuid.UUID, error) {
	switch field.Kind {
	case wire.DraftFieldText:
		if _, ok := value.(string); ok {
			return uuid.Nil, nil
		}
	case wire.DraftFieldList:
		switch v := value.(type) {
		case string:
			return uuid.Nil, nil
		case []any:
			if !slices.ContainsFunc(v, func(item any) bool { _, ok := item.(string); return !ok }) {
				return uuid.Nil, nil
			}
		}
	case wire.DraftFieldFile:
		return draftFileID(field.Path, value, conversation)
	}
	return uuid.Nil, fmt.Errorf("%w: %q takes a %s value", ErrInvalidArguments, field.Path, field.Kind)
}

// draftFileID is the file a file field's value names: its id as a string, or
// an object carrying `file_id`, DECODED STRICTLY so markup or a name where the
// reference belongs is refused rather than read as one. A conversation the
// value names must be this one; the rest is resolved by the engine.
func draftFileID(path string, value any, conversation uuid.UUID) (uuid.UUID, error) {
	usage := fmt.Errorf("%w: %q takes the file id of a file delivered or attached in this "+
		"conversation, as a string or {\"file_id\": \"<id>\"}", ErrInvalidArguments, path)
	if id, ok := value.(string); ok {
		file, err := uuid.Parse(id)
		if err != nil || file == uuid.Nil {
			return uuid.Nil, usage
		}
		return file, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return uuid.Nil, usage
	}
	var arg draftFileArg
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&arg) != nil || arg.FileID == uuid.Nil {
		return uuid.Nil, usage
	}
	switch {
	case arg.Kind != "" && arg.Kind != wire.DraftFieldFile:
		return uuid.Nil, usage
	case arg.ConversationID != uuid.Nil && arg.ConversationID != conversation:
		return uuid.Nil, fmt.Errorf("%w: %q names a file of another conversation",
			ErrInvalidArguments, path)
	}
	return arg.FileID, nil
}

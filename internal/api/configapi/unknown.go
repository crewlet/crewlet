package configapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/crewlet/crewlet/internal/config"
)

// Keeping what this build cannot represent, on every write.
//
// A document a newer peer wrote carries fields no type of this build has, and
// every write here decodes the stored document into this build's types before
// it writes. So each write stores bytes built from the stored document rather
// than from its struct, and [config.CarryUnknown] carries back what the struct
// could not hold: the rule for which keys, and how a seat or an MCP server is
// matched to the stored one it is, lives there beside the mask restore that
// matches members the same way.

// carryUnknown returns written with every field of the stored document this
// build cannot represent, and written does not name, carried over.
func carryUnknown(stored, written []byte) ([]byte, error) {
	storedTree, err := decodeTree(stored)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the active revision: %w", err)
	}
	writtenTree, err := decodeTree(written)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the written config: %w", err)
	}
	from, ok1 := storedTree.(map[string]any)
	into, ok2 := writtenTree.(map[string]any)
	if !ok1 || !ok2 {
		return written, nil
	}
	config.CarryUnknown(from, into)
	return json.Marshal(into)
}

// spliceStored writes an entity over its element in the stored document,
// leaving every other value of that document as it was stored, and carries
// what this build cannot represent into the entity from the element it
// replaces.
//
// THE STORED DOCUMENT, not this build's struct encoded again: the rest of the
// company may carry fields this build cannot represent, and a write about one
// seat must not remove them.
func spliceStored(stored []byte, access entityAccess, id string, entity any) ([]byte, error) {
	tree, err := decodeTree(stored)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the active revision: %w", err)
	}
	root, _ := tree.(map[string]any)
	element, ok := access.stored(root, id)
	if !ok {
		// Unreachable while stored and find agree: the struct this entity
		// was spliced into was decoded from these bytes.
		return nil, fmt.Errorf("configapi: the stored document holds no entity %q", id)
	}
	encoded, err := json.Marshal(entity)
	if err != nil {
		return nil, fmt.Errorf("configapi: encode the entity: %w", err)
	}
	decoded, err := decodeTree(encoded)
	if err != nil {
		return nil, err
	}
	replacement, _ := decoded.(map[string]any)
	// IN PLACE, so the element keeps its position in whatever list or map
	// holds it without this having to know which.
	clear(element)
	maps.Copy(element, replacement)
	final, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("configapi: encode the config: %w", err)
	}
	// The element replaced is matched to its stored self like every other
	// member: a seat by its handle, a unit by its name, a server by its name
	// in its list, a provider by its key.
	return carryUnknown(stored, final)
}

// decodeTree decodes JSON into maps, lists and scalars, keeping every number
// as the text it was written as.
//
// NUMBERS AS WRITTEN, because a tree is encoded again: decoded as float64, an
// integer above 2^53 (a token budget, an id somebody stored as a number) comes
// back as a different integer, and a write that changed nothing near it would
// store the corruption.
func decodeTree(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return nil, err
	}
	return tree, nil
}

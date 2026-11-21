package configapi

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// JSON Merge Patch (RFC 7396) over the company document: 7396, not the 7386 it
// obsoletes, whose mangled pseudo-code reads as an algorithm that stops after
// the first member.
//
// WHY A MERGE PATCH RATHER THAN A SMALLER PUT: `PUT /config` makes every edit
// company-wide, and the per-entity routes narrow that only for the four
// collections with identities. The turn engine, budgets, notification knobs,
// integration blocks and the rest had no narrower form. One patch route covers
// every section, because the shape sent IS the shape of the document, so
// nothing is added here when a section is.
//
// `null` deletes (also RFC 7396): without it no section can be REMOVED.

// mergePatch applies a patch to a target, both already decoded from JSON.
//
// A patch that is not an object REPLACES the target outright, which is what
// makes the recursion terminate and what makes `"mission": "new"` work.
//
// ARRAYS REPLACE WHOLE, by the RFC, which cannot address a list element. That
// is the edit the per-entity routes exist for, and a list syntax here would
// give two answers to one question. Neither RFC 6902 nor a merge key replaces
// them: a patch addresses by STRUCTURE, an entity by IDENTITY, which keeps
// reaching a server after one above it is removed.
func mergePatch(target, patch any) any {
	object, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	into, ok := target.(map[string]any)
	if !ok {
		// The target is a scalar, a list, or absent. RFC 7396 says to
		// treat it as an empty object and let the patch build one.
		into = map[string]any{}
	}
	for key, value := range object {
		if value == nil {
			delete(into, key)
			continue
		}
		into[key] = mergePatch(into[key], value)
	}
	return into
}

// applyMergePatch merges a patch document onto a stored config document.
//
// BOTH SIDES GO THROUGH JSON, and the patch is read with the YAML reader for
// the same reason the full-document write is: YAML is a superset of JSON, so
// one reader accepts the form an operator edits and the form a script sends.
func applyMergePatch(document, patch []byte) ([]byte, error) {
	// NUMBERS AS WRITTEN: see [decodeTree]. A patch to the mission used to
	// round every stored integer above 2^53 to a neighbour.
	target, err := decodeTree(document)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the active document: %w", err)
	}
	overlay, err := readPatch(patch)
	if err != nil {
		return nil, err
	}
	merged, err := json.Marshal(mergePatch(target, overlay))
	if err != nil {
		return nil, fmt.Errorf("configapi: encode the patched document: %w", err)
	}
	return merged, nil
}

// readPatch reads a merge patch as the object it has to be.
func readPatch(patch []byte) (map[string]any, error) {
	var overlay any
	if err := yaml.Unmarshal(patch, &overlay); err != nil {
		return nil, fmt.Errorf("the patch is not valid JSON or YAML: %w", err)
	}
	if overlay == nil {
		// An EMPTY patch is refused rather than treated as a no-op that
		// writes a revision. A caller that sent nothing did not mean to
		// mint an epoch every node reconciles onto.
		return nil, errEmptyPatch
	}
	object, ok := overlay.(map[string]any)
	if !ok {
		// A top-level scalar or list would REPLACE the whole company
		// under RFC 7396, which is never what a caller meant on this
		// route, and `PUT /config` is how you say that deliberately.
		return nil, fmt.Errorf(
			"a patch must be an object naming the sections to change, not %T",
			overlay)
	}
	return object, nil
}

// writeBackNamed writes restored, this build's encoding of a patched
// document, over merged at exactly the places patch names, and leaves every
// other value of merged as it was.
//
// The encoding is written back because the struct is where masks were restored;
// a mask can only be where the patch put one. Elsewhere it has something to
// lose: it holds only what this build represents and writes every list whole,
// so a mission patch would drop fields a newer build wrote ([config.CarryUnknown]
// restores only members matchable by identity).
//
// Where the patch and both sides hold an object, the walk goes inside; else the
// encoding replaces merged's value, and a null or omitted key is left to the merge.
func writeBackNamed(merged, restored []byte, patch map[string]any) ([]byte, error) {
	into, err := decodeTree(merged)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the patched document: %w", err)
	}
	from, err := decodeTree(restored)
	if err != nil {
		return nil, fmt.Errorf("configapi: decode the restored document: %w", err)
	}
	target, ok1 := into.(map[string]any)
	source, ok2 := from.(map[string]any)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("configapi: a patched document is not an object")
	}
	writeBack(target, source, patch)
	out, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("configapi: encode the patched document: %w", err)
	}
	return out, nil
}

// writeBack is [writeBackNamed] over decoded trees, in place.
func writeBack(into, from, named map[string]any) {
	for key, value := range named {
		encoded, present := from[key]
		nested, isObject := value.(map[string]any)
		inner, intoObject := into[key].(map[string]any)
		source, fromObject := encoded.(map[string]any)
		switch {
		case isObject && intoObject && fromObject:
			writeBack(inner, source, nested)
		case present && encoded != nil:
			into[key] = encoded
		}
	}
}

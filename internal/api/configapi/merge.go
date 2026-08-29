package configapi

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// JSON Merge Patch (RFC 7386) over the company document.
//
// # Why a merge patch rather than a smaller PUT
//
// `PUT /config` makes every edit a company-wide one: a founder changing one
// turn-engine setting sends back a document carrying every seat, every
// provider and every integration, and a concurrent edit anywhere in it is
// theirs to lose. The per-entity routes narrow that for the four collections
// whose members have identities. Everything else — the turn engine, learning,
// budgets, the notification knobs, the integration blocks, mission and vision
// — had no narrower form at all, so changing one of them meant re-sending the
// whole company.
//
// A merge patch is that narrower form, and one route covers every section
// rather than one route per section: the shape a caller sends IS the shape of
// the document, so nothing has to be added here when a section is added to
// the config.
//
// # Arrays replace, and that is the rule rather than an omission
//
// RFC 7386 has no way to address a list element, so `roles: [...]` in a patch
// replaces the whole roster. That is exactly the edit the per-entity routes
// exist for — `PUT /config/roles/{handle}` changes one seat — and inventing a
// list syntax here would give two answers to one question.
//
// # `null` deletes
//
// Also RFC 7386, and the reason this is a merge patch rather than an ad-hoc
// deep merge: without it there is no way to REMOVE a section, and a config
// surface that can only add is one an operator eventually edits by hand.

// mergePatch applies a patch to a target, both already decoded from JSON.
//
// A patch that is not an object REPLACES the target outright, which is what
// makes the recursion terminate and what makes `"mission": "new"` work.
func mergePatch(target, patch any) any {
	object, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	into, ok := target.(map[string]any)
	if !ok {
		// The target is a scalar, a list, or absent. RFC 7386 says to
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
	var target any
	if err := json.Unmarshal(document, &target); err != nil {
		return nil, fmt.Errorf("configapi: decode the active document: %w", err)
	}
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
	if _, ok := overlay.(map[string]any); !ok {
		// A top-level scalar or list would REPLACE the whole company
		// under RFC 7386, which is never what a caller meant on this
		// route — and `PUT /config` is how you say that deliberately.
		return nil, fmt.Errorf(
			"a patch must be an object naming the sections to change, not %T",
			overlay)
	}
	merged, err := json.Marshal(mergePatch(target, overlay))
	if err != nil {
		return nil, fmt.Errorf("configapi: encode the patched document: %w", err)
	}
	return merged, nil
}

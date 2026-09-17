package configapi

import (
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// JSON Merge Patch (RFC 7396) over the company document.
//
// 7396 rather than the 7386 this cited until now: same authors, same month,
// and 7386 is obsoleted by it. The correction is to 7386's pseudo-code, whose
// mangled indentation renders `return Target` as though it sat inside the
// loop — so the obsolete one reads as specifying an algorithm that stops
// after the first member.
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
// RFC 7396 has no way to address a list element, so `roles: [...]` in a patch
// replaces the whole roster. That is exactly the edit the per-entity routes
// exist for — `PUT /config/roles/{handle}` changes one seat — and inventing a
// list syntax here would give two answers to one question.
//
// That is an argument about THIS format, and the obvious reply is to reach for
// one that does address list members — RFC 6902, or a Kubernetes-style merge
// key. Neither replaces the entity routes, and the reason is not about
// formats: a patch addresses by STRUCTURE and a seat is addressed by
// IDENTITY, which is why the same handle reaches a seat whether it sits at
// the root or three units down.
//
// # `null` deletes
//
// Also RFC 7396, and the reason this is a merge patch rather than an ad-hoc
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
// # Why only there
//
// The encoding is written back because the struct is where the masks a
// redacted read handed back were restored: the stored bytes have to carry
// the credentials, not the markers. A mask can only be where the patch put
// one, since the rest of merged is the stored document and a stored document
// holds none. So nothing outside what the patch named has anything to take
// from the encoding.
//
// And it has something to lose. The encoding holds only what this build can
// represent, and it writes every list whole: written back everywhere, a
// patch to the mission replaced every list in the company with this build's
// copy of it, and every member of every list lost the fields a newer build
// had written. [config.CarryUnknown] brings them back for a member it can
// match by identity, and a schedule has none, so its fields were simply gone.
//
// Where the patch names an object and both sides hold one, the walk goes
// inside, so a patch naming one provider's model writes back that model and
// not the provider block. Anywhere else the encoding's value replaces
// merged's, and a key the encoding holds as null or omits is left to what the
// merge made of it, which is what a merge patch writing it back did too.
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

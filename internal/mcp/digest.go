package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// A TOOL'S DEFINITION DIGEST: one hash naming what a server said a tool is.
//
// It names the definition everywhere the definition travels without it. A
// node's listing names each tool's schemas by it rather than carrying them,
// so a schema forty seats' children all list is stored once
// (internal/coord's MCPToolDefinition), and a decision taken on a tool can be
// held to the definition it was taken on: a server that changes a tool
// changes its digest.
//
// THE SERVER'S OWN DEFINITION, and nothing of the company's: the name the
// server knows the tool by, its title, description, input and output schemas
// and the hints it declared. Not the prefix, and not an operator's annotation
// overrides. Both are configuration, and a digest that moved with them would
// read a company's own edit as the server changing its tool.
//
// SHA-256 over the definition as encoding/json writes it, which sorts every
// object's keys, so two nodes listing the same definition name it alike.

// definition is what a digest hashes.
type definition struct {
	Name         string         `json:"name"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
	Hints        hintWords      `json:"hints"`
}

// hintWords is a server's declared hints, the annotations' title among them,
// as the digest hashes them.
type hintWords struct {
	Title       string `json:"title"`
	ReadOnly    string `json:"read_only"`
	Destructive string `json:"destructive"`
	Idempotent  string `json:"idempotent"`
	OpenWorld   string `json:"open_world"`
}

// hashedOf is what a digest hashes of one listed definition.
func hashedOf(def toolDef) definition {
	return definition{
		Name: def.Name, Title: def.Title, Description: def.Description,
		InputSchema: def.InputSchema, OutputSchema: def.OutputSchema,
		Hints: hintWords{
			Title:       def.Annotations.Title,
			ReadOnly:    def.Annotations.ReadOnly.String(),
			Destructive: def.Annotations.Destructive.String(),
			Idempotent:  def.Annotations.Idempotent.String(),
			OpenWorld:   def.Annotations.OpenWorld.String(),
		},
	}
}

// digestOf is the digest of one listed definition: 64 lowercase hex digits.
func digestOf(def toolDef) string {
	return hashOf(hashedOf(def))
}

// hashOf is SHA-256 over v as encoding/json writes it, as hex.
func hashOf(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		// A schema decoded from JSON always encodes again, so this is a
		// definition no server's listing produces; it is named by its
		// encoding failure rather than by nothing, which every such
		// definition would then share.
		raw = []byte("unencodable:" + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// PartLength is how many hex digits name one part of a definition ([Parts]):
// 64 bits, enough that two different parts of one tool's definitions are told
// apart, which is all a part is read for. The definition's whole [digestOf] is
// what a decision is held to.
const PartLength = 16

// Parts names each part of a definition apart, so a reader comparing two
// definitions of one tool can say WHICH part the server changed without
// holding either whole: a listing bounds a description and may keep no
// schema, and a definition accepted months ago is no longer listed at all.
// Each is the first [PartLength] hex digits of SHA-256 over the part as the
// digest hashes it, so the four together change exactly when the digest does
// (the name, which the digest also covers, is the tool's identity and never
// differs between two definitions of one tool).
type Parts struct {
	// Title is the tool's own title and its annotations' title.
	Title string
	// Description is what the model is told it does.
	Description string
	// Schema is its input and output schemas.
	Schema string
	// Hints is its four declared hints.
	Hints string
}

// partsOf names each part of def ([Parts]).
func partsOf(def toolDef) Parts {
	d := hashedOf(def)
	cut := func(v any) string { return hashOf(v)[:PartLength] }
	return Parts{
		Title:       cut([]string{d.Title, d.Hints.Title}),
		Description: cut(d.Description),
		Schema:      cut([]map[string]any{d.InputSchema, d.OutputSchema}),
		Hints: cut([]string{d.Hints.ReadOnly, d.Hints.Destructive, d.Hints.Idempotent,
			d.Hints.OpenWorld}),
	}
}

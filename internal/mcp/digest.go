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
// (internal/coord's MCPToolSchema), and a decision taken on a tool can be
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

// digestOf is the digest of one listed definition: 64 lowercase hex digits.
func digestOf(def toolDef) string {
	raw, err := json.Marshal(definition{
		Name: def.Name, Title: def.Title, Description: def.Description,
		InputSchema: def.InputSchema, OutputSchema: def.OutputSchema,
		Hints: hintWords{
			Title:       def.Annotations.Title,
			ReadOnly:    def.Annotations.ReadOnly.String(),
			Destructive: def.Annotations.Destructive.String(),
			Idempotent:  def.Annotations.Idempotent.String(),
			OpenWorld:   def.Annotations.OpenWorld.String(),
		},
	})
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

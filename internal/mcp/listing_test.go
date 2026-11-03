package mcp

import (
	"maps"
	"testing"
)

// A TOOL'S DIGEST NAMES THE SERVER'S DEFINITION, AND NOTHING OF THE COMPANY'S:
// every field the server states moves it, and neither the prefix nor an
// operator's annotation override does, so a company editing its own
// configuration never reads as a server changing its tool.
//
// Mutation: hash the prefixed name or the merged annotations, or leave the
// title, the output schema or a hint out of the hash, and this is red.
func TestAToolsDigestNamesTheServersDefinitionAlone(t *testing.T) {
	t.Parallel()
	base := toolDef{
		Name: "create_issue", Title: "Create issue", Description: "Open an issue",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string"}}},
		OutputSchema: map[string]any{"type": "object"},
		Annotations:  Annotations{Title: "Create", Destructive: No, OpenWorld: Yes},
	}
	digest := digestOf(base)
	if !isHexDigest(digest) {
		t.Fatalf("digest %q is not 64 lowercase hex digits", digest)
	}
	moves := map[string]func(d *toolDef){
		"the name":             func(d *toolDef) { d.Name = "open_issue" },
		"the title":            func(d *toolDef) { d.Title = "Open issue" },
		"the description":      func(d *toolDef) { d.Description = "Open an issue, now" },
		"the input schema":     func(d *toolDef) { d.InputSchema = map[string]any{"type": "object"} },
		"the output schema":    func(d *toolDef) { d.OutputSchema = nil },
		"the annotation title": func(d *toolDef) { d.Annotations.Title = "" },
		"a hint":               func(d *toolDef) { d.Annotations.Destructive = Yes },
		"an unknown hint set":  func(d *toolDef) { d.Annotations.ReadOnly = No },
	}
	for what, move := range moves {
		changed := base
		changed.InputSchema, changed.OutputSchema = maps.Clone(base.InputSchema), maps.Clone(base.OutputSchema)
		move(&changed)
		if digestOf(changed) == digest {
			t.Errorf("changing %s left the digest as it was", what)
		}
	}
	// The SAME definition, rebuilt, names the same digest: a listing on
	// another node is compared by it.
	again := base
	again.InputSchema = map[string]any{"properties": map[string]any{"title": map[string]any{"type": "string"}}, "type": "object"}
	if digestOf(again) != digest {
		t.Error("the same definition, its schema keys written in another order, named another digest")
	}

	// NOTHING OF THE COMPANY'S: a prefix and an override shape the Tool and
	// leave its digest the server's.
	b := NewBridge(discardLogger(), Supervision{})
	s := b.newServer(Spec{Name: "github", ToolPrefix: "gh_",
		AnnotationOverrides: map[string]Annotations{"create_issue": {Destructive: Yes}}}, func() {})
	tool := newTool(s, base, s.spec)
	if tool.Name() != "gh_create_issue" || tool.Annotations().Destructive != Yes {
		t.Fatalf("the tool is %q with destructive %v, want the prefix and the override applied",
			tool.Name(), tool.Annotations().Destructive)
	}
	if tool.Digest() != digest {
		t.Errorf("the prefix or the override moved the digest: %s, want %s", tool.Digest(), digest)
	}
	if tool.Title() != "Create issue" {
		t.Errorf("Title() = %q, want the tool's own over its annotations'", tool.Title())
	}
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// THE ONE RISK READING: a tool's hints, read as the specification's defaults
// read an absent one, so a tool that declares nothing is destructive and says
// no hint decided it.
//
// Mutation: read an unknown destructive hint as additive, or an unknown open
// world hint as closed, and this is red.
func TestARiskReadsAnUndeclaredToolAsDestructive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ann  Annotations
		want RiskReading
	}{
		{"nothing declared", Annotations{},
			RiskReading{Risk: RiskDestructive, Declared: false, External: true}},
		{"read-only, closed world", Annotations{ReadOnly: Yes, OpenWorld: No},
			RiskReading{Risk: RiskReadOnly, Declared: true, External: false}},
		{"read-only wins over destructive", Annotations{ReadOnly: Yes, Destructive: Yes},
			RiskReading{Risk: RiskReadOnly, Declared: true, External: true}},
		{"not destructive", Annotations{ReadOnly: No, Destructive: No},
			RiskReading{Risk: RiskAdditive, Declared: true, External: true}},
		{"declared destructive", Annotations{Destructive: Yes, OpenWorld: No},
			RiskReading{Risk: RiskDestructive, Declared: true, External: false}},
		{"not read-only, nothing else", Annotations{ReadOnly: No},
			RiskReading{Risk: RiskDestructive, Declared: false, External: true}},
	}
	for _, c := range cases {
		if got := RiskOf(c.ann); got != c.want {
			t.Errorf("%s: RiskOf = %+v, want %+v", c.name, got, c.want)
		}
		if !RiskOf(c.ann).Risk.Valid() {
			t.Errorf("%s: an invalid risk", c.name)
		}
	}
}

// WHAT A SERVER SAID OF ITSELF AND OF EACH TOOL REACHES THE BRIDGE: its
// identity and instructions from the handshake, and each tool's own title and
// output schema from its listing, which the fleet's listing is built from.
//
// Mutation: drop the identity at connect, or the title or the output schema
// at the listing, and this is red.
func TestABridgeKeepsWhatAServerSaidOfItselfAndItsTools(t *testing.T) {
	t.Parallel()
	srv, h := newHTTPMCPServer(t, `[{"name":"get_issue","title":"Get issue","description":"Read one issue",`+
		`"inputSchema":{"type":"object"},"outputSchema":{"type":"object","properties":{"id":{"type":"integer"}}},`+
		`"annotations":{"readOnlyHint":true}}]`)
	h.identity = `"serverInfo":{"name":"github-mcp","title":"GitHub","version":"1.4.0"},` +
		`"instructions":"Search before you create."`
	b := NewBridge(discardLogger(), Supervision{})
	t.Cleanup(func() { _, _ = b.StopAll(t.Context()) })
	if _, err := b.Add(t.Context(), httpSpec("github", srv.URL)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	id, ok := b.Identity("github")
	if !ok || id.Name != "github-mcp" || id.Title != "GitHub" || id.Version != "1.4.0" ||
		id.Instructions != "Search before you create." || id.Protocol == "" {
		t.Errorf("Identity = %+v (%v), want the handshake's", id, ok)
	}
	if _, ok := b.Identity("nothing"); ok {
		t.Error("a server the bridge does not run has an identity")
	}
	tools := b.ServerTools("github")
	if len(tools) != 1 {
		t.Fatalf("ServerTools = %v", tools)
	}
	got := tools[0]
	if got.Title() != "Get issue" || got.OutputSchema()["type"] != "object" || !isHexDigest(got.Digest()) {
		t.Errorf("the listed tool has title %q, output %v, digest %q", got.Title(), got.OutputSchema(), got.Digest())
	}
}

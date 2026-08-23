package mcp

import (
	"encoding/json"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAnnotationsFromAcceptsBothSpellings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  map[string]any
		want Annotations
	}{
		{
			name: "empty object advertises nothing",
			raw:  map[string]any{},
			want: Annotations{},
		},
		{
			name: "mcp camelCase, as a server sends it",
			raw: map[string]any{
				"readOnlyHint": false, "destructiveHint": true,
				"openWorldHint": true, "idempotentHint": false, "title": "Open a PR",
			},
			want: Annotations{Title: "Open a PR", ReadOnly: No, Destructive: Yes, OpenWorld: Yes, Idempotent: No},
		},
		{
			name: "snake_case, as operator config writes it",
			raw:  map[string]any{"read_only": false, "open_world": true},
			want: Annotations{ReadOnly: No, OpenWorld: Yes},
		},
		{
			name: "an explicit null is not an assertion",
			raw:  map[string]any{"readOnlyHint": nil},
			want: Annotations{},
		},
		{
			name: "a non-bool is not an assertion either",
			raw:  map[string]any{"readOnlyHint": "true"},
			want: Annotations{},
		},
		{
			name: "a partial object leaves the rest unknown",
			raw:  map[string]any{"destructiveHint": true},
			want: Annotations{Destructive: Yes},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AnnotationsFrom(tc.raw); got != tc.want {
				t.Fatalf("AnnotationsFrom(%v) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMergeAppliesOnlyWhatTheOverrideAsserts(t *testing.T) {
	t.Parallel()
	server := Annotations{ReadOnly: Yes, OpenWorld: No, Title: "Search"}

	// The escape hatch: the operator knows the server is lying.
	got := server.Merge(Annotations{ReadOnly: No, OpenWorld: Yes})
	if got.ReadOnly != No || got.OpenWorld != Yes {
		t.Fatalf("override did not win: %+v", got)
	}
	if got.Title != "Search" {
		t.Fatalf("an override that says nothing about the title must not clear it: %+v", got)
	}

	// An override that asserts nothing changes nothing. Coercing Unknown to
	// No here would let an operator who annotated ONE hint silently deny the
	// other three.
	if got := server.Merge(Annotations{}); got != server {
		t.Fatalf("an all-unknown override changed the set: %+v", got)
	}
}

func TestWritesToSharedSurface(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ann  Annotations
		want bool
	}{
		{"a pure read is never a shared write", Annotations{ReadOnly: Yes}, false},
		{"read-only wins even over destructive", Annotations{ReadOnly: Yes, Destructive: Yes}, false},
		{"destructive is always a write", Annotations{Destructive: Yes}, true},
		{"not-read-only and open-world is a write", Annotations{ReadOnly: No, OpenWorld: Yes}, true},
		{"not-read-only with world unstated is a write", Annotations{ReadOnly: No}, true},
		{"not-read-only but closed-world is local", Annotations{ReadOnly: No, OpenWorld: No}, false},
		{"open-world alone says nothing about writing", Annotations{OpenWorld: Yes}, false},
		{"ALL UNKNOWN IS NOT A WRITE", Annotations{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WritesToSharedSurface(tc.ann); got != tc.want {
				t.Fatalf("WritesToSharedSurface(%+v) = %v, want %v", tc.ann, got, tc.want)
			}
		})
	}
}

func TestReadOnlyProvenIsNotTheNegationOfWrites(t *testing.T) {
	t.Parallel()
	// The trap, pinned. These two answer OPPOSITE questions and they disagree
	// for exactly the tools nobody annotated — which is most of them. A fence
	// built on !WritesToSharedSurface admits every one of these.
	disagree := []Annotations{
		{},                      // nothing advertised at all
		{Title: "Create issue"}, // a title and no hints
		{OpenWorld: Yes},        // touches the world, silent about writing
		{Idempotent: Yes},       // says something, but not the thing asked
	}
	for _, ann := range disagree {
		if WritesToSharedSurface(ann) {
			t.Fatalf("%+v: expected the permissive answer for an unclassifiable tool", ann)
		}
		if ReadOnlyProven(ann) {
			t.Fatalf("%+v: an unannotated tool must never be PROVEN read-only", ann)
		}
	}
	// And where the server did assert it, both agree.
	ro := Annotations{ReadOnly: Yes}
	if WritesToSharedSurface(ro) || !ReadOnlyProven(ro) {
		t.Fatalf("an asserted read-only tool should satisfy both: %+v", ro)
	}
}

// TestSDKAnnotationsCannotHoldTheTriState is the REASON probe for probe.go.
//
// If this ever fails, the SDK has started carrying absence for these two hints
// and the wire probe has lost its justification — which is a thing the next
// reader should be told loudly rather than left to discover by deleting it.
func TestSDKAnnotationsCannotHoldTheTriState(t *testing.T) {
	t.Parallel()
	decode := func(raw string) sdk.ToolAnnotations {
		var a sdk.ToolAnnotations
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			t.Fatalf("unmarshal %s: %v", raw, err)
		}
		return a
	}
	silent := decode(`{}`)
	explicit := decode(`{"readOnlyHint":false,"idempotentHint":false}`)

	if silent.ReadOnlyHint != explicit.ReadOnlyHint || silent.IdempotentHint != explicit.IdempotentHint {
		t.Fatal("the SDK now distinguishes an absent hint from an explicit false: " +
			"probe.go's reason has changed and its comment is stale")
	}
	// The pointers DO carry absence, which is why the fallback trusts them.
	if silent.DestructiveHint != nil || silent.OpenWorldHint != nil {
		t.Fatal("destructiveHint/openWorldHint stopped being pointers; annotationsFromSDK needs revisiting")
	}

	// And here is the consequence the probe exists to prevent: read the
	// SILENT set naively — believing the bools — and an unannotated tool
	// classifies as a shared-surface write.
	naive := Annotations{
		ReadOnly:   hintOf(silent.ReadOnlyHint),
		Idempotent: hintOf(silent.IdempotentHint),
	}
	if !WritesToSharedSurface(naive) {
		t.Fatal("the naive read no longer inverts the guard; re-derive the probe's justification")
	}
	// The degraded fallback refuses to invent that assertion.
	if got := annotationsFromSDK(&silent); got != (Annotations{}) {
		t.Fatalf("annotationsFromSDK(%+v) = %+v, want all-unknown", silent, got)
	}
	if WritesToSharedSurface(annotationsFromSDK(&silent)) {
		t.Fatal("the fallback classified an unannotated tool as a shared write")
	}
}

func TestAnnotationsFromSDKKeepsWhatThePointersCarry(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	got := annotationsFromSDK(&sdk.ToolAnnotations{
		Title:           "Delete branch",
		ReadOnlyHint:    true,
		DestructiveHint: &yes,
		OpenWorldHint:   &no,
	})
	want := Annotations{Title: "Delete branch", ReadOnly: Yes, Destructive: Yes, OpenWorld: No}
	if got != want {
		t.Fatalf("annotationsFromSDK = %+v, want %+v", got, want)
	}
	if got := annotationsFromSDK(nil); got != (Annotations{}) {
		t.Fatalf("nil annotations = %+v, want all-unknown", got)
	}
}

func TestAnnotationsFromJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "null"} {
		got, err := annotationsFromJSON(json.RawMessage(raw))
		if err != nil || got != (Annotations{}) {
			t.Fatalf("annotationsFromJSON(%q) = %+v, %v", raw, got, err)
		}
	}
	got, err := annotationsFromJSON(json.RawMessage(`{"readOnlyHint":true}`))
	if err != nil || got.ReadOnly != Yes {
		t.Fatalf("annotationsFromJSON = %+v, %v", got, err)
	}
	if _, err := annotationsFromJSON(json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed annotations must report an error, not decode to all-unknown")
	}
}

func TestHintString(t *testing.T) {
	t.Parallel()
	for h, want := range map[Hint]string{Unknown: "unknown", Yes: "yes", No: "no"} {
		if got := h.String(); got != want {
			t.Fatalf("Hint(%d).String() = %q, want %q", h, got, want)
		}
	}
}

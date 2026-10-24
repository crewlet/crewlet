package org

import "testing"

func TestSlugify(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"Sarah Chen", "sarah-chen"},
		{"QA/Test Lead", "qa-test-lead"},
		{"  VP  Engineering  ", "vp-engineering"},
		{"CEO", "ceo"},
		{"--weird--", "weird"},
		{"", ""},
	} {
		if got := Slugify(tc.in); got != tc.want {
			t.Errorf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDeriveAgentIDIsStable pins the derivation itself. The value is a
// UUIDv5 over "{org}:{handle}" in a frozen namespace; a change here silently
// orphans every seat's memory, so the constant is asserted rather than
// merely exercised.
func TestDeriveAgentIDIsStable(t *testing.T) {
	t.Parallel()
	id, ok := DeriveAgentID("Acme AI", "ceo")
	if !ok {
		t.Fatal("DeriveAgentID reported failure for valid inputs")
	}
	const want = "2ceeb519-da94-59de-aaf2-be0042636a9f"
	if id.String() != want {
		t.Errorf("DeriveAgentID = %s, want %s", id, want)
	}
}

// TestDeriveAgentIDNamespacesByOrg is why the org name is in the input: two
// companies sharing one store must be able to both have a "ceo".
func TestDeriveAgentIDNamespacesByOrg(t *testing.T) {
	t.Parallel()
	a, _ := DeriveAgentID("Acme AI", "ceo")
	b, _ := DeriveAgentID("Other Co", "ceo")
	if a == b {
		t.Error("same handle in different orgs derived the same id")
	}
}

// TestDeriveAgentIDRefusesEmptyInput guards against an unnameable seat
// quietly acquiring an id that another unnameable seat would also derive.
func TestDeriveAgentIDRefusesEmptyInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ org, handle string }{
		{"", "ceo"}, {"Acme", ""}, {"", ""},
	} {
		if _, ok := DeriveAgentID(tc.org, tc.handle); ok {
			t.Errorf("DeriveAgentID(%q, %q) reported success", tc.org, tc.handle)
		}
	}
}

// TestTheDocumentChangeOfAHandleRemovesAndCreates pins ADR-0013's rule that a
// handle is IMMUTABLE: there is no rename. A document that changes a seat's
// handle holds a different seat — a different agent id, so a different
// mailbox, lease and memory — and the handle it gave up names nobody, rather
// than resolving to the seat now standing under the new one. Correcting the
// seat's NAME, with the handle declared, keeps the seat.
func TestTheDocumentChangeOfAHandleRemovesAndCreates(t *testing.T) {
	t.Parallel()
	before := &Organization{Name: "Acme", Roles: []*Role{
		{Name: "Sarah Chen", DeclaredHandle: "sarah-chen"}}}
	wasID, ok := before.AgentIDFor(before.Roles[0])
	if !ok {
		t.Fatal("the seat has no agent id")
	}

	changed := &Organization{Name: "Acme", Roles: []*Role{
		{Name: "Sarah Chen", DeclaredHandle: "sarah-okonkwo"}}}
	isID, _ := changed.AgentIDFor(changed.Roles[0])
	if isID == wasID {
		t.Error("a changed handle kept the old seat's agent id: a handle is " +
			"the seat's identity, so a new one is a new seat")
	}
	if got := changed.Role("sarah-chen"); got != nil {
		t.Errorf("the handle the document gave up still resolves, to %q: a "+
			"handle no seat answers to names nobody", got.Handle())
	}

	// THE CONTROL: the same handle under a corrected name is the same seat.
	renamed := &Organization{Name: "Acme", Roles: []*Role{
		{Name: "Sarah Okonkwo", DeclaredHandle: "sarah-chen"}}}
	keptID, _ := renamed.AgentIDFor(renamed.Roles[0])
	if keptID != wasID {
		t.Errorf("correcting the display name moved the agent id from %s to "+
			"%s: only the handle is the identity", wasID, keptID)
	}
	if got := renamed.Role("sarah-chen"); got != renamed.Roles[0] {
		t.Errorf("the declared handle resolves to %v", got)
	}
}

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

package envref

import "testing"

// EVERY DERIVED NAME IS ONE A ${VAR} CAN REFERENCE, and a separator becomes an
// underscore rather than being dropped, so nova-1 and nova1 stay two seats.
func TestSealedNameIsReferenceableAndDistinct(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ vendor, field, seat, want string }{
		{"datadog", "APP_KEY", "sre", "DATADOG_APP_KEY_SRE"},
		{"github", "APP_KEY", "senior-engineer", "GITHUB_APP_KEY_SENIOR_ENGINEER"},
		{"slack", "BOT_TOKEN", "nova.1", "SLACK_BOT_TOKEN_NOVA_1"},
		{"gitlab", "TOKEN", "7th", "GITLAB_TOKEN_7TH"},
		{"datadog", "API_KEY", "", "DATADOG_API_KEY"},
	} {
		got := SealedName(tc.vendor, tc.field, tc.seat)
		if got != tc.want {
			t.Errorf("SealedName(%q, %q, %q) = %q, want %q", tc.vendor, tc.field, tc.seat, got, tc.want)
		}
		if name, ok := Whole("${" + got + "}"); !ok || name != got {
			t.Errorf("%q cannot be referenced as a whole ${VAR}", got)
		}
	}
	if SealedName("slack", "BOT_TOKEN", "nova-1") == SealedName("slack", "BOT_TOKEN", "nova1") {
		t.Error("nova-1 and nova1 derive one name: a dropped separator merges two seats")
	}
}

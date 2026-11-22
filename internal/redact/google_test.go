package redact_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// A GOOGLE REFRESH TOKEN AND AN OAUTH CLIENT SECRET ARE MASKED WHEREVER THEY
// STAND: a token endpoint's error that echoes the refresh token a consent
// grant holds, and a client secret printed beside it, reach no surface. The
// refresh token's slashes keep it out of the shared canary corpus, whose
// samples are planted in path segments, so it is held here.
func TestAGoogleRefreshTokenAndClientSecretAreMasked(t *testing.T) {
	t.Parallel()
	refresh := "1//0g" + strings.Repeat("Ab3dE6gH9jK2", 8)
	secret := "GOCSPX-" + strings.Repeat("mN5pQ8sT1v", 3)
	text := "invalid_grant: token " + refresh + " for client secret " + secret + "."
	got := redact.Secrets(text)
	if strings.Contains(got, refresh[4:]) || strings.Contains(got, secret[7:]) {
		t.Fatalf("redacted %q still carries a credential", got)
	}
	if !strings.Contains(got, "google-refresh-token") || !strings.Contains(got, "google-client-secret") {
		t.Errorf("redacted %q does not name what it took out", got)
	}
}

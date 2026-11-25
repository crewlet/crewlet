package accessmigrate

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// WHY AN INTEGRATION WAITS IS THE MIGRATION'S OWN SENTENCE: a connection whose
// values are unset is a setup not finished, a vendor that failed did not
// answer, and neither carries the cause's text (a config path, a package's
// name), which stays for the log and errors.Is.
//
// Mutation: say the wrapped error, or call an unset connection a vendor that
// did not answer, and this fails.
func TestWhyAnIntegrationWaitsIsTheMigrationsOwnSentence(t *testing.T) {
	t.Parallel()
	unset := fmt.Errorf("googleauth: integrations.google_cloud.provisioning.provider is not set: %w",
		integration.ErrNotConfigured)
	why := unread(integration.KindGoogleCloud, unset)
	if said := Said(why); said != "Google Cloud's own setup is not finished, so its agents' "+
		"accounts could not be read" || !errors.Is(why, integration.ErrNotConfigured) {
		t.Errorf("an unset connection waits on %q (%v)", said, why)
	}
	down := unread(integration.KindDatadog, errors.New("datadog: GET /api/v2/users: 503"))
	if said := Said(down); said != "Datadog did not answer when its agents' accounts were read" {
		t.Errorf("a vendor that failed waits on %q", said)
	}
	if !strings.Contains(down.Error(), "503") {
		t.Errorf("the log lost the cause: %v", down)
	}
}

package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A KEY IS SPELLED AS ITS VENDOR MATCHES IT: Jira's and a Confluence global
// space's upper-cased, a Confluence personal space's kept exactly.
func TestScopeKeysAreSpelledAsTheVendorMatchesThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rule     func(string) string
		in, want string
	}{
		{config.JiraProjectKey, " eng ", "ENG"},
		{config.ConfluenceSpaceKey, " eng ", "ENG"},
		{config.ConfluenceSpaceKey, " ~Bob ", "~Bob"},
		{config.ConfluenceSpaceKey, "~5b10ac8d82e05b22cc7d4ef5", "~5b10ac8d82e05b22cc7d4ef5"},
	} {
		if got := tc.rule(tc.in); got != tc.want {
			t.Errorf("%q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

package gitref_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitref"
)

// A REMOTE'S URL IS READ ONE WAY AND WRITTEN BACK THAT WAY, because two parsers
// that read one URL differently are how a check of one host becomes a dial of
// another. Anything another parser could read as a different host (a trailing
// dot, an IPv4 address in octal, hex or fewer than four parts, a name libcurl
// would turn into punycode, a zone), any other scheme, and what git would
// write into its children's arguments (a user, a password) is refused —
// quoting no password. The code domain and the configuration hold a recorded
// mirror or import to this one reading, and the git process wrapper dials only
// what it wrote back.
//
// Mutation: drop the user's case from ParseRemoteURL — the URLs naming a user
// are admitted, red.
func TestARemoteURLIsWrittenBackOneWay(t *testing.T) {
	t.Parallel()
	for given, want := range map[string]struct {
		url, host string
		port      int
		literal   bool
	}{
		"https://Git.Example.COM/team/r.git":       {"https://git.example.com/team/r.git", "git.example.com", 443, false},
		"https://git.example.com:443/r.git":        {"https://git.example.com/r.git", "git.example.com", 443, false},
		"https://git.example.com:/r.git":           {"https://git.example.com/r.git", "git.example.com", 443, false},
		"https://git.example.com:8443/r.git":       {"https://git.example.com:8443/r.git", "git.example.com", 8443, false},
		"https://git.example.com":                  {"https://git.example.com", "git.example.com", 443, false},
		"https://git.example.com/a%20b.git":        {"https://git.example.com/a%20b.git", "git.example.com", 443, false},
		"https://xn--gt-fia.example.com/r.git":     {"https://xn--gt-fia.example.com/r.git", "xn--gt-fia.example.com", 443, false},
		"https://104.16.0.1/r.git":                 {"https://104.16.0.1/r.git", "104.16.0.1", 443, true},
		"https://[2606:4700:0:0:0:0:0:1]/r.git":    {"https://[2606:4700::1]/r.git", "2606:4700::1", 443, true},
		"https://[2606:4700::1]:8443/r.git":        {"https://[2606:4700::1]:8443/r.git", "2606:4700::1", 8443, true},
		"https://[2606:4700::ABCD]:443/team/r.git": {"https://[2606:4700::abcd]/team/r.git", "2606:4700::abcd", 443, true},
	} {
		got, err := gitref.ParseRemoteURL(given)
		switch {
		case err != nil:
			t.Errorf("%s is refused: %v", given, err)
		case got.URL != want.url || got.Host != want.host || got.Port != want.port ||
			got.Addr.IsValid() != want.literal:
			t.Errorf("%s is read as %s (host %s, port %d, an address %t), want %s (host %s, port %d, an "+
				"address %t)", given, got.URL, got.Host, got.Port, got.Addr.IsValid(), want.url, want.host,
				want.port, want.literal)
		case got.Parsed().String() != got.URL:
			t.Errorf("%s's parsed value writes %s, and its URL is %s", given, got.Parsed(), got.URL)
		}
		// WRITTEN BACK ONCE IS WRITTEN BACK: the URL read again is itself.
		if again, err := gitref.ParseRemoteURL(got.URL); err != nil || again.URL != got.URL {
			t.Errorf("%s written back as %s reads again as %s, %v", given, got.URL, again.URL, err)
		}
	}
	const password = "hunter2-the-password"
	for _, given := range []string{
		"",
		"http://git.example.com/r.git",
		"ssh://git@git.example.com/r.git",
		"git://git.example.com/r.git",
		"file:///srv/r.git",
		"git.example.com:team/r.git",
		"https:git.example.com/r.git",
		"https:///r.git",
		"https://user@git.example.com/r.git",
		"https://@git.example.com/r.git",
		"https://user:" + password + "@git.example.com/r.git",
		"https://git.example.com\\@evil.example.com/r.git",
		"https://git.example.com/r.git?ref=main",
		"https://git.example.com/r.git?",
		"https://git.example.com/r.git#main",
		"https://git.example.com/r.git#",
		"https://git.example.com./r.git",
		"https://127.1/r.git",
		"https://0x7f.0.0.1/r.git",
		"https://010.0.0.1/r.git",
		"https://2130706433/r.git",
		"https://git.example.123/r.git",
		"https://gït.example.com/r.git",
		"https://git_lab.example.com/r.git",
		"https://-git.example.com/r.git",
		"https://git-.example.com/r.git",
		"https://git..example.com/r.git",
		"https://[10.0.0.5]/r.git",
		"https://[fe80::1%25eth0]/r.git",
		"https://git.example.com:0/r.git",
		"https://git.example.com:65536/r.git",
		"https://" + strings.Repeat("a", 64) + ".example.com/r.git",
	} {
		got, err := gitref.ParseRemoteURL(given)
		if err == nil {
			t.Errorf("%q is read as %s, want it refused", given, got.URL)
			continue
		}
		if strings.Contains(err.Error(), password) {
			t.Errorf("%q's refusal quotes its password: %v", given, err)
		}
	}
}

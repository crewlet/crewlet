package redact

import (
	"net/url"
	"strings"
)

// A URL'S QUERY IS WHERE A VENDOR PUTS A KEY when it asks for one in the
// address (`?api_key=`, `?token=`, a signed link's `sig=`), and an address is
// shown wherever its server is: a settings page, an API answer, a status row.
// [URL] is the one reading of which parameters hold a credential, so every
// surface that answers an address answers it the same way.

// redactedValue replaces a credential-like parameter's value: a word, so a
// reader sees that a value was there and that it was withheld.
const redactedValue = "redacted"

// credentialParams are the fragments of a query parameter's name that mark it
// as holding a credential, matched case-insensitively anywhere in the name:
// `api_key`, `apikey`, `x-api-key`, `access_token`, `client_secret`,
// `password`, `sig` and `signature`, `auth`, `session`, `credential`, `jwt`.
var credentialParams = []string{
	"key", "token", "secret", "sig", "pass", "pwd", "auth", "session", "credential", "jwt", "bearer",
}

// URL is raw with every credential in it withheld: the value of each query
// parameter whose name marks it as a credential ([credentialParams]), the
// value of any other parameter this package's text pass would redact, and the
// user name and password of a URL that carries them. An address that does not
// parse as a URL is passed through the text pass whole ([Secrets]), since it
// can still carry a key.
func URL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Secrets(raw)
	}
	if u.User != nil {
		u.User = url.User(redactedValue)
	}
	if u.RawQuery != "" {
		query := u.Query()
		for name, values := range query {
			for i, v := range values {
				if credentialName(name) || Contains(v) {
					values[i] = redactedValue
				}
			}
			query[name] = values
		}
		u.RawQuery = query.Encode()
	}
	return u.String()
}

// credentialName reports whether a query parameter's name marks its value as a
// credential.
func credentialName(name string) bool {
	lower := strings.ToLower(name)
	for _, fragment := range credentialParams {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

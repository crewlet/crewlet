package redact

import (
	"net/url"
	"strings"
)

// AN ADDRESS CAN CARRY A KEY IN THREE PLACES, and an address is shown wherever
// its server is: a settings page, an API answer, a status row. A vendor that
// asks for a key in the address puts it in the query (`?api_key=`, `?token=`,
// a signed link's `sig=`), in a path segment (`/mcp/<key>/sse`), or in the
// fragment (`#access_token=...`). [URL] is the one reading of an address with
// its credentials withheld, so every surface that answers one answers it the
// same way.
//
// THE SCHEME, HOST AND PORT STAY READABLE, since they are what a reader
// recognises the server by, and an address with nothing to withhold comes back
// byte for byte as it went in: nothing is re-encoded or reordered unless a part
// of it was withheld.

// redactedValue replaces a credential: a word, so a reader sees that a value
// was there and that it was withheld.
const redactedValue = "redacted"

// credentialParams are the fragments of a parameter's name that mark it as
// holding a credential, matched case-insensitively anywhere in the name:
// `api_key`, `apikey`, `x-api-key`, `access_token`, `client_secret`,
// `password`, `sig` and `signature`, `auth`, `session`, `credential`, `jwt`.
var credentialParams = []string{
	"key", "token", "secret", "sig", "pass", "pwd", "auth", "session", "credential", "jwt", "bearer",
}

// URL is raw with every credential in it withheld:
//
//   - in the query, the value of each parameter whose name marks it as a
//     credential ([credentialParams]), and of any other parameter this
//     package's text pass would redact ([Contains]);
//   - in the path, each segment the text pass would redact: a path has no
//     names to judge, so only a known credential's shape is withheld there;
//   - in the fragment, the same rule as the query where it is a list of
//     parameters (`#access_token=...&state=...`), and the text pass over the
//     whole of it otherwise;
//   - the user name and password of an address that carries them.
//
// An address that does not parse as a URL is passed through the text pass
// whole ([Secrets]), since it can still carry a key.
func URL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Secrets(raw)
	}
	changed := false
	if u.User != nil {
		u.User, changed = url.User(redactedValue), true
	}
	if path, withheld := redactPath(u.EscapedPath()); withheld {
		unescaped, err := url.PathUnescape(path)
		if err != nil {
			return Secrets(raw)
		}
		u.Path, u.RawPath, changed = unescaped, path, true
	}
	if query, withheld := redactParams(u.RawQuery); withheld {
		u.RawQuery, changed = query, true
	}
	if fragment, withheld := redactFragment(u.EscapedFragment()); withheld {
		unescaped, err := url.PathUnescape(fragment)
		if err != nil {
			return Secrets(raw)
		}
		u.Fragment, u.RawFragment, changed = unescaped, fragment, true
	}
	if !changed {
		return raw
	}
	return u.String()
}

// redactPath is an escaped path with each segment the text pass would redact
// replaced, and whether any was.
func redactPath(path string) (string, bool) {
	segments := strings.Split(path, "/")
	withheld := false
	for i, segment := range segments {
		if segment != "" && credentialText(segment) {
			segments[i], withheld = redactedValue, true
		}
	}
	return strings.Join(segments, "/"), withheld
}

// redactParams is a raw `name=value&...` list with each credential's value
// replaced, in the order and the spelling it was written, and whether any was.
func redactParams(raw string) (string, bool) {
	if raw == "" {
		return raw, false
	}
	pairs := strings.Split(raw, "&")
	withheld := false
	for i, pair := range pairs {
		rawName, rawValue, hasValue := strings.Cut(pair, "=")
		name, err := url.QueryUnescape(rawName)
		if err != nil {
			name = rawName
		}
		credential := (hasValue && credentialName(name)) || credentialText(rawValue) || credentialText(rawName)
		if !credential {
			continue
		}
		pairs[i], withheld = rawName+"="+redactedValue, true
	}
	return strings.Join(pairs, "&"), withheld
}

// redactFragment is an escaped fragment with its credentials withheld: as a
// parameter list where it is one, and whole where the text pass would redact
// it otherwise.
func redactFragment(fragment string) (string, bool) {
	if fragment == "" {
		return fragment, false
	}
	if strings.Contains(fragment, "=") {
		return redactParams(fragment)
	}
	if credentialText(fragment) {
		return redactedValue, true
	}
	return fragment, false
}

// credentialText reports whether a raw part of an address, decoded or as
// written, holds something the text pass would redact.
func credentialText(raw string) bool {
	if Contains(raw) {
		return true
	}
	decoded, err := url.QueryUnescape(raw)
	return err == nil && decoded != raw && Contains(decoded)
}

// credentialName reports whether a parameter's name marks its value as a
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

package jetstream

import (
	"errors"
	"net/url"
	"strings"

	"github.com/nats-io/nats.go"
)

// serverAddresses renders stream.url — one server URL or a comma-separated
// list of them, the shape the client takes — for a sentence an operator reads:
// each entry by its scheme, host and port and nothing else.
//
// # Why the whole userinfo, and not only a password
//
// A NATS URL carries a credential in either half of its userinfo:
// `nats://user:password@host` signs in as a user, and `nats://token@host` —
// a username with no password — is a bearer TOKEN, which is how the client
// reads it. [url.URL.Redacted], and the client's own ConnectedUrlRedacted
// built on it, mask a password and print a username, so a token written into
// stream.url went into every refusal and log line that named the server — a
// boot's stderr, a journal, a ticket somebody pastes it into. A host and a
// port are all a sentence needs to say which member it means, so nothing else
// of the URL is kept: not its userinfo, and not a path or a query either,
// which the client never reads and a sentence has no use for.
//
// # Why an entry that does not parse is not echoed
//
// Its text is where the credential is, and a parse that failed has not said
// where the credential ends. It is named as an address that does not parse,
// which is what its remedy turns on.
func serverAddresses(raw string) string {
	var named []string
	// Split and trimmed as the client splits it (nats.go's
	// processUrlString), so the list named is the list dialled.
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSuffix(strings.TrimSpace(entry), "/")
		if entry != "" {
			named = append(named, serverAddress(entry))
		}
	}
	return strings.Join(named, ",")
}

// serverAddress renders one server URL by its scheme, host and port — see
// [serverAddresses].
func serverAddress(entry string) string {
	// A bare host:port is the client's `nats://` one: without the scheme a
	// URL parse reads `token@host:4222` as a path with a colon in its first
	// segment and refuses it, and the entry would be named as one that does
	// not parse when the client dials it perfectly well.
	if !strings.Contains(entry, "://") {
		entry = "nats://" + entry
	}
	u, err := url.Parse(entry)
	if err != nil || u.Host == "" {
		return "(an address that does not parse)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

// connectedServer names the server nc is connected to, as
// [serverAddresses] names one — never through nc.ConnectedUrlRedacted, which
// prints a token.
func connectedServer(nc *nats.Conn) string {
	connected := nc.ConnectedUrl()
	if connected == "" {
		// Not connected any more: the member it was talking to is the one
		// a sentence would name, and the client no longer says which.
		return "(a server this connection has since left)"
	}
	return serverAddress(connected)
}

// unquoted is a dial's error without the text of a URL it quotes.
//
// The client hands a URL that does not parse back as the [url.Error] its
// parse returned, whose sentence quotes the WHOLE entry — the userinfo
// included — so a mistyped port on a URL carrying a password printed the
// password. What failed is what the operator needs, and it is the inner
// error: an invalid port, an invalid character in the host, invalid userinfo,
// none of which quotes the credential. The one that can is a malformed
// %-escape, which quotes the three characters it failed on wherever they
// were — a password's among them — so it is named without them.
func unquoted(err error) error {
	var parse *url.Error
	if !errors.As(err, &parse) {
		return err
	}
	var escape url.EscapeError
	if errors.As(parse.Err, &escape) {
		return errors.New("it carries a malformed %-escape")
	}
	return parse.Err
}

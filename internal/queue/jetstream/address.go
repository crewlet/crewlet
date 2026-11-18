package jetstream

import (
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/nats-io/nats.go"
)

// serverAddresses renders stream.url (one server URL or a comma-separated
// list) for a sentence an operator reads: each entry by its scheme, host and
// port and nothing else.
//
// The whole userinfo goes, not only a password: the client reads
// `nats://token@host` as a bearer token, and [url.URL.Redacted] (and the
// client's ConnectedUrlRedacted) print a username, so a token would reach every
// refusal and log line naming the server. A path or query goes too; the client
// never reads them. An entry that does not parse is not echoed: its text holds
// the credential, and a failed parse has not said where it ends.
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
	// A bare host:port is the client's `nats://` one; unschemed, a URL parse
	// reads `token@host:4222` as a path and refuses an entry the client dials.
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
// The client returns an unparseable URL as its [url.Error], whose sentence
// quotes the whole entry, userinfo included. The inner error (an invalid port,
// host character or userinfo) names what failed without the credential, except
// a malformed %-escape, which quotes the three characters it failed on, so that
// one is named without them.
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

// ServerHosts is the host of each server stream.url names, in the order it
// names them and each once: no scheme, no port and nothing of the userinfo,
// for the reasons [serverAddresses] gives. An entry that does not parse names
// no host and is left out, since its text is where a credential would be.
func ServerHosts(raw string) []string {
	var hosts []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSuffix(strings.TrimSpace(entry), "/")
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "://") {
			entry = "nats://" + entry
		}
		u, err := url.Parse(entry)
		if err != nil || u.Hostname() == "" || slices.Contains(hosts, u.Hostname()) {
			continue
		}
		hosts = append(hosts, u.Hostname())
	}
	return hosts
}

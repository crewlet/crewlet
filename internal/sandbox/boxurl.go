package sandbox

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// boxURLShape is how a refusal says what a box URL looks like. It names no
// host of anybody's on purpose: a refusal never repeats the value, and an
// example host would read as part of it.
const boxURLShape = "https://host[:port][/path]"

// ParseBoxURL is the value of variable — an engine address a box dials: the
// OTLP receiver's ([OtelReceiverURLVar]), the tool bridge's
// (CREWLET_MCP_BRIDGE_URL) and the code host's ([BoxGitURLVar]) — as a box
// will use it: an absolute http or https
// URL with a host a box's URL parser accepts (the WHATWG URL Standard's, as
// boxHostRefusal holds it), a port a client can dial where it names one (1 to
// 65535), and no user information, query or fragment, its trailing '/'
// trimmed, since each caller appends its own path. "" (after trimming space)
// is unset and returns "", nil: the caller's feature is off.
//
// WHAT IT RETURNS IS SPELLED BY net/url's OWN SERIALIZER, never put together
// from the parsed parts, because the parser hands back the host UNESCAPED: a
// host written "ex%C3%A9.example" comes back as raw UTF-8, which is an IRI
// rather than the URI RFC 3986 §3.2.2 spells, and an IPv6 zone written
// "[fe80::1%25eth0]" came back "[fe80::1%eth0]", an escape no parser reads
// (zones are refused below, for their clients' sake). The serializer escapes
// the host's non-ASCII bytes again, so every answer parses back to itself,
// here and in the box's client, which parses it with the engine's own path
// appended. It is spelled as RFC 3986 spells a URI but for one character: a
// '"' in a name, which net/url and the WHATWG URL Standard both read and RFC
// 3986's reg-name does not, is kept — the line this grammar holds is the box
// parser's, and that parser reads it. An empty port ("host:") is omitted, as
// RFC 3986 §3.2.3 asks a normalizer to.
//
// Any other value is an error that names the variable and the part that is
// wrong, and the caller refuses the boot on it: a value that is set and
// unusable is a deployment that asked for the feature and would get none. The
// alternative is the failure this replaced — every box handed an endpoint its
// client cannot use, so a run's telemetry went nowhere and an agent-mode run's
// every tool call failed, each silently, inside a box the engine never hears
// from again.
//
// THE VALUE IS NEVER ECHOED, nor any piece of it — not even the parser's own
// message, which quotes the port or the escape it stopped at. A URL's user
// information is a credential, and the refusal goes to a log and to whatever
// terminal a deploy script captured; the operator holds the value already.
//
// THE SCHEME IS NEVER JUDGED BEYOND HTTP OR HTTPS: a local cell dials the
// engine over the host's own network (172.17.0.1, host.docker.internal), where
// TLS buys nothing and a certificate for that address is one nobody holds;
// whether a URL's path leaves the box's host — always for a remote cell, and
// for a local one whose URL names another node or a balancer — is a fact about
// the network this function cannot see, so the docs, not the engine, say where
// https is needed (docs/concepts/code-sandbox.md, "Security model").
func ParseBoxURL(variable, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	// THE SCHEME FIRST, BY HAND. url.Parse reads "engine.example.com:80"
	// as the scheme "engine.example.com" and an opaque "80", and refuses
	// "127.0.0.1:8080" as a path segment with a colon — two answers to the
	// one mistake, neither of which says what it is.
	scheme, _, absolute := strings.Cut(value, "://")
	if !absolute {
		return "", fmt.Errorf("%s is not an absolute URL: it does not start with a scheme "+
			"and a host, and a box dials it as given; write it as %s, with http or https "+
			"as its scheme", variable, boxURLShape)
	}
	if !strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https") {
		return "", fmt.Errorf("%s names a scheme a box cannot dial: it must be http or "+
			"https, as in %s", variable, boxURLShape)
	}
	u, err := url.Parse(value)
	if err != nil {
		// Not wrapped: url.Error quotes the whole value, and its inner
		// error the port or escape it stopped at.
		return "", fmt.Errorf("%s does not parse as a URL: write it as %s, with no space or "+
			"control character in it and every %% starting a valid escape", variable, boxURLShape)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s carries user information (a name or password before an @): "+
			"a box authenticates with the token the engine mints for its run, and a "+
			"credential in this URL would be handed to every box; remove it", variable)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%s names no host for a box to dial; write it as %s",
			variable, boxURLShape)
	}
	if err := boxHostRefusal(variable, u); err != nil {
		return "", err
	}
	// THE PORT'S RANGE, because url.Parse checks only that a port is
	// digits: ":99999" parses here and no box's client reads it (Go's
	// dialer: "invalid port"; Node and Bun: "Invalid URL"), and ":0" is a
	// port nothing listens on, so every dial of it fails. The digits are not
	// repeated, for the reason nothing of the value is.
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return "", fmt.Errorf("%s names a port no client can dial: a port is a number "+
				"from 1 to 65535; write it as %s", variable, boxURLShape)
		}
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("%s carries a query: the engine appends its own path to the "+
			"value, which would then follow the query; remove the ? and what follows it",
			variable)
	}
	// BY THE CHARACTER, because url.Parse records "https://h#" as no
	// fragment at all, and a client handed "https://h#/otlp/…" sends none of
	// the path the engine appended.
	if strings.Contains(value, "#") {
		return "", fmt.Errorf("%s carries a fragment: the engine appends its own path to the "+
			"value, which a client would then never send; remove the # and what follows it",
			variable)
	}
	// The path as the parser ESCAPED it, trimmed of its trailing '/' in that
	// form, so "/a%2Fb/" stays the one segment "a/b" rather than being
	// re-escaped from a decoded "/a/b".
	origin := url.URL{Scheme: u.Scheme, Host: strings.TrimSuffix(u.Host, ":")}
	return origin.String() + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// boxHostRefusal holds the host url.Parse read out of a box URL to what the
// URL parser in the box accepts: the WHATWG URL Standard's host parser
// (https://url.spec.whatwg.org/#host-parsing), by which every coding agent
// this build launches parses a URL — claude-code on Node, opencode on Bun.
// net/url reads RFC 3986 and is laxer about a host in four ways, each of which
// booted a node whose every box failed on its first request; Node 22's and
// Bun's `new URL` answer "Invalid URL" for each, measured:
//
//   - An IPv6 zone ("[fe80::1%25eth0]", RFC 6874): the standard has no zone
//     syntax.
//   - A character the standard forbids in a domain — its forbidden domain
//     code points: every C0 control, space, DEL and # % / : < > ? @ [ \ ] ^ |.
//     net/url passes '%' (written %25), '<', '>' and ']', so an unfilled
//     placeholder ("http://<engine-host>:8000", the style these docs write an
//     address in) booted. The rest of the set is held here too though
//     url.Parse refuses it first today, so the rule is the standard's whatever
//     net/url passes — its GODEBUG urlstrictcolons=0 already lets a second ':'
//     into an http host.
//   - Escapes that decode to no UTF-8 ("%FF"), which the standard decodes to
//     U+FFFD, a code point no domain may hold.
//   - A name that ENDS IN A NUMBER and is no IPv4 address. The standard reads
//     a host whose last label is a number (decimal, or hex after 0x) as an
//     IPv4 address and fails one that is not, so a typo'd "10.0.0.300", a
//     fifth part or "engine.123" is refused here, while the shorthand it does
//     read ("127.1", "0x7f.1") boots: the box dials the address its parser
//     makes of it, and nothing in the engine dials this URL at all.
//
// WHAT IT DOES NOT REPRODUCE is the standard's UTS 46 mapping of a non-ASCII
// name (its case and width folding, and the code points it disallows). Those
// tables follow the Unicode version of the box's own runtime, which this
// process cannot know, so a copy here would refuse a name some boxes read or
// pass one they refuse; a non-ASCII name is held to its encoding and to the
// rules above as written, and the box's parser judges the rest.
//
// The refusal names the variable and what is wrong with the host, never the
// value, for the reason [ParseBoxURL] gives.
func boxHostRefusal(variable string, u *url.URL) error {
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		// An IP literal, which url.Parse has held to an IPv6 address
		// (netip.ParseAddr, refusing an IPv4 one): the standard's IPv6
		// parser reads the same addresses, but for a zone.
		if strings.Contains(host, "%") {
			return fmt.Errorf("%s names an IPv6 host with a zone, which the URL parser of a "+
				"box's coding agent refuses; name the host by an address with no zone, or by "+
				"a name", variable)
		}
		return nil
	}
	if !utf8.ValidString(host) {
		return fmt.Errorf("%s names a host that is not valid UTF-8 — a %% escape in it decodes "+
			"to bytes that spell no character — which the URL parser of a box's coding agent "+
			"refuses; write the host as a name or an address", variable)
	}
	if strings.ContainsFunc(host, forbiddenInADomain) {
		return fmt.Errorf("%s names a host holding a character no host may hold — a space, a "+
			"control character, or one of # %% / : < > ? @ [ \\ ] ^ | — which the URL parser "+
			"of a box's coding agent refuses, as it does an unfilled placeholder in angle "+
			"brackets or an escaped %%; write the host as a name or an address", variable)
	}
	if endsInANumber(host) && !isIPv4Address(host) {
		return fmt.Errorf("%s names a host whose last label is a number, which the URL parser "+
			"of a box's coding agent reads as an IPv4 address, and it is not one; write an "+
			"address as four numbers from 0 to 255 joined by dots, or name the host by a name "+
			"whose last label is not a number", variable)
	}
	return nil
}

// forbiddenInADomain is the WHATWG URL Standard's forbidden domain code
// point: a C0 control, space, DEL, or one of # % / : < > ? @ [ \ ] ^ |.
func forbiddenInADomain(r rune) bool {
	return r <= ' ' || r == 0x7f || strings.ContainsRune(`#%/:<>?@[\]^|`, r)
}

// endsInANumber is the standard's "ends in a number checker": whether a
// name's last label — one trailing dot ignored — is decimal digits, or any
// other part [ipv4Number] reads, which beyond digits can only be hex after
// "0x". The standard runs it on the name UTS 46 has mapped; this runs on the
// name as written, which differs only where a non-ASCII character maps to a
// digit or a dot.
func endsInANumber(name string) bool {
	labels := strings.Split(name, ".")
	if labels[len(labels)-1] == "" {
		if len(labels) == 1 {
			return false
		}
		labels = labels[:len(labels)-1]
	}
	last := labels[len(labels)-1]
	if last != "" && strings.Trim(last, "0123456789") == "" {
		return true
	}
	_, ok := ipv4Number(last)
	return ok
}

// isIPv4Address is whether the standard's IPv4 parser reads name as an
// address: at most four dot-separated parts (one trailing dot ignored), each
// a number [ipv4Number] reads, every one but the last at most 255 and the last
// below 256 to the power of the bytes left for it — so "127.1" is 127.0.0.1,
// "1.2.65535" is 1.2.255.255, and "1.2.65536" is no address.
func isIPv4Address(name string) bool {
	parts := strings.Split(name, ".")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > 4 {
		return false
	}
	for i, part := range parts {
		n, ok := ipv4Number(part)
		if !ok {
			return false
		}
		if i < len(parts)-1 && n > 255 {
			return false
		}
		if i == len(parts)-1 && n >= 1<<(8*(5-len(parts))) {
			return false
		}
	}
	return true
}

// ipv4Number is the standard's "IPv4 number parser": a part in hex after "0x"
// or "0X", in octal after a leading "0", and in decimal otherwise — "0x" alone
// is zero — with ok false for an empty part or a digit outside its radix. The
// value saturates at 2^32, past every bound an address puts on a part, so a
// part of any length is judged without overflowing.
func ipv4Number(part string) (n uint64, ok bool) {
	if part == "" {
		return 0, false
	}
	radix := uint64(10)
	switch {
	case len(part) >= 2 && (part[:2] == "0x" || part[:2] == "0X"):
		part, radix = part[2:], 16
	case len(part) >= 2 && part[0] == '0':
		part, radix = part[1:], 8
	}
	const saturated = 1 << 32
	for i := 0; i < len(part); i++ {
		var d uint64
		switch c := part[i]; {
		case '0' <= c && c <= '9':
			d = uint64(c - '0')
		case 'a' <= c && c <= 'f':
			d = uint64(c-'a') + 10
		case 'A' <= c && c <= 'F':
			d = uint64(c-'A') + 10
		default:
			return 0, false
		}
		if d >= radix {
			return 0, false
		}
		n = min(n*radix+d, saturated)
	}
	return n, true
}

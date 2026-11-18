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

// ParseBoxURL is the value of variable, an engine address a box dials (the
// OTLP receiver's [OtelReceiverURLVar], the tool bridge's
// CREWLET_MCP_BRIDGE_URL, Gitlet's [BoxGitURLVar]), as a box will use it: an
// absolute http or https URL with a host a box's URL parser accepts
// ([boxHostRefusal]), a port from 1 to 65535 if it names one, no user
// information, query or fragment, and no trailing '/', since each caller
// appends its own path. A blank value is unset: "", nil, the feature off.
//
// Any other value is an error naming the variable and the wrong part, and
// the caller refuses the boot: a box handed an endpoint it cannot use fails
// silently, where the engine never hears. The error NEVER ECHOES the value or
// any piece of it, not even the parser's: user information is a credential.
func ParseBoxURL(variable, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	// THE SCHEME FIRST, BY HAND: url.Parse reads "engine.example.com:80" as the
	// scheme "engine.example.com" and refuses "127.0.0.1:8080" as a path, neither
	// saying what is wrong. It is judged no further than http or https: a local
	// cell dials over the host's own network, where TLS buys nothing, and whether
	// the path leaves the host is a network fact this cannot see, so
	// docs/concepts/code-sandbox.md ("Security model") says where https is needed.
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
	// THE PORT'S RANGE: url.Parse checks only that a port is digits, so ":99999"
	// parses and no box's client reads it, and nothing listens on ":0". The
	// digits are not repeated, as nothing of the value is.
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
	// SPELLED BY net/url's SERIALIZER, never from the parsed parts: the parser
	// hands the host back unescaped (raw UTF-8, an IRI rather than a URI), and
	// the serializer escapes it again, so every answer parses back to itself,
	// here and in the box. Only a '"' in a name is kept against RFC 3986, since
	// the box's parser reads it; an empty port is dropped (RFC 3986 section 3.2.3).
	// The path is the parser's ESCAPED one, so "/a%2Fb/" stays the one segment.
	origin := url.URL{Scheme: u.Scheme, Host: strings.TrimSuffix(u.Host, ":")}
	return origin.String() + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// boxHostRefusal holds the host url.Parse read to what a box's URL parser
// accepts: the WHATWG URL Standard's host parser, by which every harness this
// build launches (claude-code on Node, opencode on Bun) parses a URL. net/url
// is laxer in four ways, each booting a node whose every box fails its first
// request: an IPv6 zone (RFC 6874); a forbidden domain code point
// ([forbiddenInADomain]), as in an unfilled "<engine-host>" placeholder, held
// in full whatever net/url passes (GODEBUG urlstrictcolons=0 loosens it);
// escapes that decode to no UTF-8; and a name ending in a number that is no
// IPv4 address ("10.0.0.300"), while shorthand ("127.1") boots, the box
// dialing what its parser makes of it. The standard's UTS 46 mapping is not
// copied: its tables follow the box runtime's Unicode version, unknown here.
// The refusal never names the value ([ParseBoxURL]).
func boxHostRefusal(variable string, u *url.URL) error {
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		// An IP literal, which url.Parse has held to an IPv6 address
		// (netip.ParseAddr, refusing an IPv4 one): the standard's IPv6
		// parser reads the same addresses, but for a zone.
		if strings.Contains(host, "%") {
			return fmt.Errorf("%s names an IPv6 host with a zone, which the URL parser of a "+
				"box's harness refuses; name the host by an address with no zone, or by "+
				"a name", variable)
		}
		return nil
	}
	if !utf8.ValidString(host) {
		return fmt.Errorf("%s names a host that is not valid UTF-8 — a %% escape in it decodes "+
			"to bytes that spell no character — which the URL parser of a box's harness "+
			"refuses; write the host as a name or an address", variable)
	}
	if strings.ContainsFunc(host, forbiddenInADomain) {
		return fmt.Errorf("%s names a host holding a character no host may hold — a space, a "+
			"control character, or one of # %% / : < > ? @ [ \\ ] ^ | — which the URL parser "+
			"of a box's harness refuses, as it does an unfilled placeholder in angle "+
			"brackets or an escaped %%; write the host as a name or an address", variable)
	}
	if endsInANumber(host) && !isIPv4Address(host) {
		return fmt.Errorf("%s names a host whose last label is a number, which the URL parser "+
			"of a box's harness reads as an IPv4 address, and it is not one; write an "+
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
// name's last label, one trailing dot ignored, is decimal digits or another
// part [ipv4Number] reads (hex after "0x"). The standard runs it on the UTS 46
// mapped name; this runs on the name as written, which differs only where a
// non-ASCII character maps to a digit or a dot.
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

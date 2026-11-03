package gitref

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// RemoteURL is a REMOTE'S URL as the engine reads it — a git repository at
// another host that an import fetches from or a mirror fetches from or pushes
// to — read one way and written back that way ([ParseRemoteURL]).
//
// HERE, BELOW EVERY READER, because three judge one URL: the git process
// wrapper (internal/code/gitproc) before it hands git a URL to dial, the code
// domain before it records a repository's mirror or import, and the
// configuration validating code.repos at `crewlet validate`. Each read it its
// own way once, and a URL the document admitted — a user before the host, a
// query, a host with a trailing dot — would have been refused by the dial
// alone, at an import's first attempt rather than where its author wrote it.
type RemoteURL struct {
	// URL is the URL written back from what was parsed — https, a lowercase
	// name or an address in its one written form, no user, query or
	// fragment — never the text it was given.
	URL string
	// Host and Port are the URL's as libcurl keys the name it resolves: the
	// lowercase name, or the address, and the port it connects to (443 where
	// the URL names none).
	Host string
	Port int
	// Addr is the host's address where the host is one — nothing resolves
	// it — and the zero Addr for a name.
	Addr netip.Addr
	// written is URL as a parsed value, for a reader that asks the process
	// which proxy reaches it.
	written url.URL
}

// Parsed is the URL as a parsed value: a copy, which the caller may keep.
func (r RemoteURL) Parsed() *url.URL {
	u := r.written
	return &u
}

// ParseRemoteURL reads raw as a remote's URL and writes it back.
//
// WRITTEN BACK, NEVER PASSED ON: two parsers that read one URL differently — a
// host after an @, a backslash, an IPv4 address in octal or in fewer than four
// parts, a trailing dot, a name libcurl would convert to punycode — are how a
// check of one host becomes a dial of another, or how a pin keyed by one
// spelling is missed by libcurl's lookup of another and answered by DNS. So
// git is handed the URL this wrote from what it parsed, in the one spelling
// every parser reads alike, and anything that has another is refused:
//
//   - any scheme but https: the engine dials another host over https alone;
//   - no host, or a host that is neither a DNS name of letters, digits and
//     hyphens whose last label begins with a letter ([dnsName]) nor an IP
//     address in its one written form (dotted decimal, or IPv6 in brackets
//     with no zone);
//   - a user, with or without a password: a credential is a header, never
//     part of the URL, which git writes into the arguments of every process
//     it starts, where any user of the machine can read it — and a document
//     carries a credential as a ${VAR} reference, never as text;
//   - a query, which git appends its own to, or a fragment, which names
//     nothing a git server serves;
//   - a port that is not one from 1 to 65535.
//
// THE REFUSAL NEVER QUOTES THE URL BEFORE IT PARSES, and only redacted after:
// a URL naming a user may carry a password.
func ParseRemoteURL(raw string) (RemoteURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		// A *url.Error's text quotes the URL whole; its cause does not.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return RemoteURL{}, fmt.Errorf("the remote is not a URL: %w", err)
	}
	switch {
	case u.Scheme != "https":
		return RemoteURL{}, fmt.Errorf("%s is not an https URL: a git process dials another host over "+
			"https alone", u.Redacted())
	case u.Opaque != "" || u.Host == "":
		return RemoteURL{}, fmt.Errorf("%s names no host: write it https://<host>/<path>", u.Redacted())
	case u.User != nil:
		return RemoteURL{}, fmt.Errorf("%s names a user: a credential is a header, never part of the URL, "+
			"which git writes into the arguments of every process it starts, where any user of the "+
			"machine can read it", u.Redacted())
	case u.RawQuery != "" || u.ForceQuery:
		return RemoteURL{}, fmt.Errorf("%s has a query: git appends its own to a remote's URL", u.Redacted())
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return RemoteURL{}, fmt.Errorf("%s has a fragment, which names nothing a git server serves",
			u.Redacted())
	}
	host, addr, err := remoteHost(u)
	if err != nil {
		return RemoteURL{}, err
	}
	port := 443
	if p := u.Port(); p != "" {
		// url.Parse has held it to digits.
		if port, err = strconv.Atoi(p); err != nil || port < 1 || port > 65535 {
			return RemoteURL{}, fmt.Errorf("%s's port %s is not one from 1 to 65535", u.Redacted(), p)
		}
	}
	hostport := host
	switch {
	case port != 443:
		hostport = net.JoinHostPort(host, strconv.Itoa(port))
	case addr.Is6():
		hostport = "[" + host + "]"
	}
	written := url.URL{Scheme: "https", Host: hostport, Path: u.Path, RawPath: u.RawPath}
	return RemoteURL{URL: written.String(), Host: host, Port: port, Addr: addr, written: written}, nil
}

// remoteHost is u's host as a remote's: an IPv6 address in brackets or an
// IPv4 address in dotted decimal, each in its one written form, and the
// address; or a DNS name, lowercased ([dnsName]), and no address.
//
// AN IPv6 ADDRESS WITH A ZONE IS REFUSED: a zone names a link of this host, so
// no host on the internet has one, and libcurl cannot pin one.
func remoteHost(u *url.URL) (string, netip.Addr, error) {
	h := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(h)
		switch {
		case err != nil || !addr.Is6():
			return "", netip.Addr{}, fmt.Errorf("%s's host %q in brackets is not an IPv6 address", u.Redacted(), h)
		case addr.Zone() != "":
			return "", netip.Addr{}, fmt.Errorf("%s's host %s carries a zone, which names a link of this host "+
				"and never a host on the internet", u.Redacted(), h)
		}
		return addr.String(), addr, nil
	}
	// Without brackets only IPv4 parses: url.Parse reads an IPv6 address's
	// colons as a port.
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.String(), addr, nil
	}
	name := strings.ToLower(h)
	if !dnsName(name) {
		return "", netip.Addr{}, fmt.Errorf("%s's host %q is not a DNS name of letters, digits and hyphens "+
			"whose last label begins with a letter, nor an IP address in its one written form (dotted decimal, "+
			"or IPv6 in brackets): write it so, without a trailing dot, an internationalised name as its "+
			"punycode", u.Redacted(), h)
	}
	return name, netip.Addr{}, nil
}

// dnsName reports whether s, lowercase, is a host name every parser reads
// alike: labels of letters, digits and hyphens, 1 to 63 bytes each and
// neither beginning nor ending with a hyphen, 253 bytes in all, with no empty
// label (so no trailing dot, which libcurl may strip before it looks the name
// up), and a LAST LABEL THAT BEGINS WITH A LETTER, as every top-level domain
// does — a host that ends in a number is an IPv4 address to libcurl and the C
// resolver, which read 127.1, 0x7f.1 and 010.0.0.1 as addresses (the last as
// octal) where netip refuses all three.
func dnsName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for i := range len(l) {
			if c := l[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	return 'a' <= last[0] && last[0] <= 'z'
}

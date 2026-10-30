package gitproc

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
)

// remoteCommands are the subcommands that dial a remote: each takes a
// [Cmd.Remote] and must, and no other subcommand takes one.
//
// A SET, as gitproc spells its other sets of names (receivePackOptions,
// crashFormats), never a []string: the cache's writers gate
// (TestNothingButSyncWritesACache, internal/code/cache) reads a []string
// literal that begins with a git writer, in any file that runs git, as an
// argument list built away from its Cmd, and so read this one as a fetch run
// outside the files allowed to. It runs nothing: a fetch or a push is judged
// at the call site that spells its Cmd.
var remoteCommands = map[string]bool{"fetch": true, "ls-remote": true, "push": true}

// The configuration a remote run is given after the base, each written by the
// pool from the run's [Cmd.Remote] (Remote.config) for the reason the package
// doc's "One outbound dial" gives it — so no call site may set one
// (checkConfig), as none may set a base key.
const (
	keyHTTPSAllow        = "protocol.https.allow"
	keyFollowRedirects   = "http.followRedirects"
	keyBundleURI         = "transfer.bundleURI"
	keyRecurse           = "fetch.recurseSubmodules"
	keyKeepAliveIdle     = "http.keepAliveIdle"
	keyKeepAliveInterval = "http.keepAliveInterval"
	keyKeepAliveCount    = "http.keepAliveCount"
	keyResolve           = "http.curloptResolve"
	keyProxy             = "http.proxy"
	keyCAInfo            = "http.sslCAInfo"
	keyCAPath            = "http.sslCAPath"
	keyExtraHeader       = "http.extraHeader"
)

// remoteKeys is every key a remote run's configuration may carry.
var remoteKeys = []string{
	keyHTTPSAllow, keyFollowRedirects, keyBundleURI, keyRecurse, keyKeepAliveIdle, keyKeepAliveInterval,
	keyKeepAliveCount, keyResolve, keyProxy, keyCAInfo, keyCAPath, keyExtraHeader,
}

// remoteKeepAliveProbes is how many of TCP's keepalive probes a remote run's
// connection may leave unanswered before the kernel ends it
// (http.keepAliveCount): THREE, ten seconds apart once the connection has been
// quiet for half of [TransferStall] (http.keepAliveIdle and
// http.keepAliveInterval, which [Remote.config] derives from the pool's
// stall). So a connection whose peer stopped answering — a host gone, a path
// dropped, a NAT that forgot it — is ended a minute after the last thing it
// heard, the bound every other transfer of the engine's is held to, while a
// probe or its answer lost on the way ends nothing: three in a row would have
// to be. A push whose peer vanishes with bytes still in flight sends no probe
// — TCP retransmits those instead, and gives up after about fifteen minutes on
// Linux's defaults (tcp_retries2), since neither git nor libcurl sets
// TCP_USER_TIMEOUT.
//
// THE PEER'S LIVENESS, NEVER ITS SPEED. A git server working on a large
// repository — counting and compressing what a fetch wants, indexing and
// checking what a push sent — may send nothing but a keepalive for minutes, or
// nothing at all where no progress was asked for and it keeps no keepalive,
// and no client tells that from a server that has wedged: so a remote that
// stays connected is waited on until [RemoteDeadline], whatever it sends.
//
// NOT LIBCURL'S LOW-SPEED CHECK (http.lowSpeedLimit, http.lowSpeedTime), which
// bounded this leg until it was measured. What that check reads is a SPEED:
// the bytes moved, both ways, over libcurl's last five seconds of
// once-a-second samples, in whole bytes a second rounded down
// (lib/progress.c), so at its least limit, a byte a second, a window reads as
// moving only with as many bytes as the seconds it spans — and git's keepalive
// is five bytes every five seconds (uploadpack.keepAlive, receive.keepAlive):
// the threshold itself, which side of it a window lands on turning on
// sub-millisecond timing on both machines. MEASURED with git 2.56.0 and
// libcurl 8.14.1 over TLS on loopback: with the check's time scaled to ten
// seconds, a remote sending git's keepalive every five seconds was ended in
// one fetch of twelve, paced as upload-pack paces it (each five seconds after
// the last) in one of two, and one every 5.003 s in both; at the minute,
// keepalives ten to thirty seconds apart were ended at 62-84 s in three
// fetches of five, and silence at 62 s. No limit or time mends it: the least
// speed the check can ask for is what git's own keepalive moves.
const remoteKeepAliveProbes = 3

// remoteMarker is what a run's lines read where one of its remote's secrets
// stood, in internal/redact's marker form, so a second redaction pass
// recognises its work and a reader tells a removal from git's own words.
const remoteMarker = redact.Marker + "credential]"

// minDerivedSecret is the shortest piece [secretsOf] takes out of a run's
// lines that the run was not handed whole — what a Basic credential encodes,
// its user and password and the password alone: eight bytes. No forge issues
// a credential shorter, so a shorter piece is a word, an id or a placeholder
// (a user named git, a password of x), and taking it out of every line would
// leave git's account of the failure unreadable. What the run WAS handed — the
// header's value and the credential after its scheme — is taken out whatever
// its length: a credential too short to be one is still the operator's to keep
// out of the logs.
const minDerivedSecret = 8

// Remote is the one host a git process may dial ([Cmd.Remote]), and how: an
// https URL; the addresses git connects to for its host, resolved in Go and
// held to the engine's public-address rule before git is told them — or, in
// their place, the proxy the process names for it; the certificate
// authorities git verifies it with; and one header, a credential, that git
// sends with every request. The package doc's "One outbound dial" says why
// each is so.
//
// [ResolveRemote] is the only way to a Remote that dials: its fields are its
// own, so no call site can hand git a host or an address the rule did not
// judge, and the zero Remote is refused by every run. [Remote.WithHeader]
// gives it its header.
//
// It PRINTS WITHOUT ITS HEADER and without a proxy's password
// ([Remote.String]): a Remote a log line names never names a credential.
type Remote struct {
	// url is the URL git is handed, as remoteURL wrote it back from what it
	// parsed — https, a lowercase name or an address, no user, query or
	// fragment — never the text it was given.
	url string
	// host and port are the URL's as libcurl keys the name it resolves: the
	// lowercase name, or the address, and the port it connects to (443 where
	// the URL names none).
	host string
	port int
	// literal is that host is an address: nothing resolves it, so nothing is
	// pinned.
	literal bool
	// addrs are what git connects to: the host's addresses, each judged
	// public unless the call allowed private ones, in the order the resolver
	// answered; the literal's one for an address; none through a proxy,
	// which resolves the host itself.
	addrs []netip.Addr
	// proxy is http.proxy: the proxy the process names for the URL, as
	// libcurl reads one ([remoteProxy]); "" for none.
	proxy string
	// caFile is http.sslCAInfo: the process's SSL_CERT_FILE; "" for
	// libcurl's own file of authorities.
	caFile string
	// caDir is http.sslCAPath: the one directory the process's SSL_CERT_DIR
	// names ([remoteCADir]); "" for libcurl's own.
	caDir string
	// header is http.extraHeader, "Name: value"; "" for none.
	header string
	// secrets are what a run's lines are redacted of ([secretsOf]), longest
	// first, each once.
	secrets []string
}

// remoteEnv is what a remote is resolved against: the resolver its host is
// resolved with (and the network's NAT64 prefixes asked of), the proxy the
// process names for a URL, and the process's certificate authorities — the
// file SSL_CERT_FILE names and the directories SSL_CERT_DIR does, as the
// process's environment holds them. The process's own in ResolveRemote; a
// table and values of the case's in the suite, which reaches no network.
type remoteEnv struct {
	resolver httpx.Resolver
	proxy    func(*url.URL) (*url.URL, error)
	caFile   string
	caDirs   string
}

// ResolveRemote is the remote at rawURL a git process may dial
// ([Cmd.Remote]). The URL is read one way and written back that way
// ([Remote.URL]): https, its host a DNS name of letters, digits and hyphens
// whose last label begins with a letter, or an address in its one written
// form, and no user, query or fragment — otherwise [ErrRemote], naming what
// to change.
//
// Its host's addresses are what git connects to, pinned so git never asks DNS
// for the name again, and every one is held to the engine's public-address
// rule on the network the node resolves on (httpx.ResolvePublic) — a name
// that resolves to any address that is not on the public internet is refused
// whole, httpx.ErrNotPublic wrapped with what lets it through. An IPv6
// address under a NAT64 prefix is judged by the IPv4 address it carries
// where the prefix is the well-known one, or one the network's own resolver
// names where httpx takes that resolver's word: in global unicast clear of
// the blocks the rule refuses there, or in RFC 8215's 64:ff9b:1::/48. A
// prefix named anywhere else could widen the rule into a network that is not
// on the internet, so httpx ignores it and the addresses under it stay
// refused: a DNS64 network that translates through a unique-local prefix
// reaches no IPv4-only host this way, and names a proxy for it instead
// (below). The prefixes are asked of the resolver the host is resolved with,
// and only for an address whose verdict they can change. allowPrivate (Tier A
// git.import_allow_private) lifts the rule: the host's addresses are pinned
// as the resolver answers them.
//
// THROUGH A PROXY the process names for the URL — HTTPS_PROXY and NO_PROXY,
// read as every other client of the engine reads them
// (http.ProxyFromEnvironment) — nothing is resolved here: the proxy resolves
// the host and dials it, so the wall is the proxy, the operator's own, as it is
// for the CDN fetch's dialer (internal/artifactupload/vendor), and allowPrivate
// changes nothing. The process's SSL_CERT_FILE and SSL_CERT_DIR, where they
// are set, are the certificate authorities git verifies the remote with, so git
// trusts what the engine's own clients trust ([remoteCADir] says what of
// SSL_CERT_DIR it can take).
//
// It resolves on every call and keeps nothing, as httpx keeps nothing of the
// network's prefixes past one resolve: a remote is dialed once a fetch or a
// push, and an answer kept past it would be a judgment of an address the name
// may no longer have. A lookup that fails, the host's or the one for the
// NAT64 prefixes an address's verdict needs, is the resolver's error, wrapped.
func ResolveRemote(ctx context.Context, rawURL string, allowPrivate bool) (Remote, error) {
	return resolveRemote(ctx, rawURL, allowPrivate, remoteEnv{
		resolver: net.DefaultResolver,
		proxy: func(u *url.URL) (*url.URL, error) {
			return http.ProxyFromEnvironment(&http.Request{URL: u})
		},
		caFile: os.Getenv("SSL_CERT_FILE"),
		caDirs: os.Getenv("SSL_CERT_DIR"),
	})
}

// resolveRemote is ResolveRemote against env.
func resolveRemote(ctx context.Context, rawURL string, allowPrivate bool, env remoteEnv) (Remote, error) {
	r, u, err := remoteURL(rawURL)
	if err != nil {
		return Remote{}, err
	}
	r.caFile = env.caFile
	if r.caDir, err = remoteCADir(env.caDirs); err != nil {
		return Remote{}, err
	}
	proxy, err := env.proxy(u)
	if err != nil {
		return Remote{}, fmt.Errorf("gitproc: reading the process's proxy for %s (HTTPS_PROXY, NO_PROXY): %w", r.url, err)
	}
	if proxy != nil {
		if r.proxy, err = remoteProxy(proxy); err != nil {
			return Remote{}, err
		}
		r.secrets = secretsOf(r.proxy, "")
		return r, nil
	}
	if r.addrs, err = remoteAddrs(ctx, env.resolver, r.host, allowPrivate); err != nil {
		return Remote{}, fmt.Errorf("gitproc: the remote %s: %w", r.url, err)
	}
	return r, nil
}

// remoteURL reads rawURL as a remote's URL and writes it back
// ([gitref.ParseRemoteURL]: THE ONE READING, which the code domain and the
// configuration hold a recorded mirror or import to as well), with the Remote
// it names (no addresses yet) and the URL as parsed — refused ErrRemote,
// naming what to change.
func remoteURL(rawURL string) (Remote, *url.URL, error) {
	parsed, err := gitref.ParseRemoteURL(rawURL)
	if err != nil {
		return Remote{}, nil, fmt.Errorf("%w: %w", ErrRemote, err)
	}
	return Remote{url: parsed.URL, host: parsed.Host, port: parsed.Port, literal: parsed.Addr.IsValid()},
		parsed.Parsed(), nil
}

// remoteProxy is the proxy git is told to dial for a remote — p, as the
// process names it — in the form libcurl reads as Go's transport reads p. A
// SOCKS5 proxy is written socks5h: Go's transport hands a SOCKS5 proxy the
// remote's NAME to resolve, and libcurl's socks5:// resolves it on this node
// instead — through DNS no pin covers, since a proxied remote has none —
// where its socks5h:// does what Go's transport does. A proxy neither reads
// is ErrRemote, naming the variable that set it.
func remoteProxy(p *url.URL) (string, error) {
	proxy := *p
	switch proxy.Scheme {
	case "http", "https", "socks5h":
	case "socks5":
		proxy.Scheme = "socks5h"
	default:
		return "", fmt.Errorf("%w: the process's proxy %s is not an http, https or socks5 proxy: it is HTTPS_PROXY's "+
			"(or https_proxy's) value", ErrRemote, proxy.Redacted())
	}
	if proxy.Host == "" {
		return "", fmt.Errorf("%w: the process's proxy %s names no host: it is HTTPS_PROXY's (or https_proxy's) value",
			ErrRemote, proxy.Redacted())
	}
	return proxy.String(), nil
}

// remoteCADir is git's http.sslCAPath for the process's SSL_CERT_DIR, dirs:
// "" where it names no directory, the directory where it names one, and
// ErrRemote, naming the variable, where it names several.
//
// ONE DIRECTORY, because libcurl reads one. The engine's own clients read
// SSL_CERT_DIR as crypto/x509 does — a colon-separated list, OpenSSL's reading
// — while the GnuTLS libcurl the image's git does its TLS through
// (scripts/git.sh) takes its CA path as a single directory, so a list handed to
// it would trust no directory in it, and a remote whose authority is in one
// would fail verification where the engine's own clients connect. Refused
// here, the operator is told so at the import or the mirror that needs it,
// rather than told the remote's certificate is untrusted. An empty entry of
// the list names nothing, for crypto/x509 too, and is passed over.
func remoteCADir(dirs string) (string, error) {
	var named []string
	for _, d := range strings.Split(dirs, ":") {
		if d != "" {
			named = append(named, d)
		}
	}
	if len(named) > 1 {
		return "", fmt.Errorf("%w: the process's SSL_CERT_DIR names %d directories of certificate authorities, and "+
			"git does its TLS through libcurl, which reads one: name one directory there, or gather the "+
			"authorities into the file SSL_CERT_FILE names", ErrRemote, len(named))
	}
	if len(named) == 0 {
		return "", nil
	}
	return named[0], nil
}

// remoteAddrs is what git connects to for host: every address res answers for
// it held to the public-address rule (httpx.ResolvePublic, which asks res
// itself for the network's NAT64 prefixes) — or, with allowPrivate, every
// address res answers — once each, unmapped, in the order answered.
func remoteAddrs(ctx context.Context, res httpx.Resolver, host string, allowPrivate bool) ([]netip.Addr, error) {
	var addrs []netip.Addr
	var err error
	if !allowPrivate {
		addrs, err = httpx.ResolvePublic(ctx, res, host)
		if errors.Is(err, httpx.ErrNotPublic) {
			return nil, fmt.Errorf("%w — a git process dials another host only at an address on the public "+
				"internet, unless git.import_allow_private is set on the node that runs it", err)
		}
	} else if literal, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{literal}
	} else {
		addrs, err = res.LookupNetIP(ctx, "ip", host)
	}
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		a = a.Unmap()
		if a.Zone() != "" {
			// From a hosts file: libcurl cannot pin a zone, and refuses the
			// whole entry rather than dial without it.
			return nil, fmt.Errorf("%w: %s resolves to %s, which carries a zone libcurl cannot be pinned to", ErrRemote, host, a)
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s resolves to no address", host)
	}
	return out, nil
}

// WithHeader is r sending header with every request git makes to the remote —
// ONE HTTP header line, "Name: value": the credential a remote is fetched from
// or pushed to with, an `Authorization: Basic …` or `Bearer …`. git is given
// it through the run's environment (http.extraHeader), never its arguments,
// and the header's value, the credential in it and a Basic credential's
// password are taken out of every line the run hands back ([secretsOf]). It
// replaces a header r had.
//
// The name must be an HTTP token and the value one line of visible characters
// — a carriage return or a line feed would write a second header nobody
// meant git to send — or the header is ErrRemote, whose text quotes nothing of
// the value.
func (r Remote) WithHeader(header string) (Remote, error) {
	if r.url == "" {
		return Remote{}, fmt.Errorf("%w: a header for a remote ResolveRemote did not build", ErrRemote)
	}
	name, value, ok := strings.Cut(header, ":")
	value = strings.Trim(value, " \t")
	switch {
	case !ok || !httpToken(name):
		// The name is not quoted: a line with no colon may be all value.
		return Remote{}, fmt.Errorf("%w: the header is not \"Name: value\" with a name of HTTP's token characters", ErrRemote)
	case value == "":
		return Remote{}, fmt.Errorf("%w: the header %s has no value", ErrRemote, name)
	case !headerValue(value):
		return Remote{}, fmt.Errorf("%w: the header %s's value holds a control character: a line break in it would "+
			"write a second header", ErrRemote, name)
	}
	r.header = name + ": " + value
	r.secrets = secretsOf(r.proxy, value)
	return r, nil
}

// httpToken reports whether s is an HTTP token (RFC 9110, section 5.6.2): a
// header name.
func httpToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') ||
			strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

// headerValue reports whether s holds no control character but a tab: a
// header value of one line (RFC 9110, section 5.5, its obs-text included).
func headerValue(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < ' ' && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// secretsOf is what a remote run's lines are redacted of, given its proxy and
// its header's value: the value whole; the credential after its scheme
// (`Basic`, `Bearer`, `token` …), which is what a remote echoing the
// credential it was sent most likely prints; for Basic, the user and password
// it encodes and the password alone, each at least [minDerivedSecret] long;
// and the proxy's password. Longest first, so a secret never leaves part of a
// longer one behind, each once.
//
// THE MATERIAL THE ENGINE KNOWS IS SECRET, rather than shapes it hopes to
// recognise (internal/redact's pass, which runs after this): git never prints
// the header, but the remote, which holds it, may echo it — in the `remote:`
// lines git relays, an `ERR` it dies with, the body of an HTTP error — and
// what it echoes is exactly what it was sent.
func secretsOf(proxy, value string) []string {
	var out []string
	if value != "" {
		out = append(out, value)
		if scheme, credential, ok := strings.Cut(value, " "); ok {
			if credential = strings.Trim(credential, " \t"); credential != "" {
				out = append(out, credential)
				out = append(out, basicSecrets(scheme, credential)...)
			}
		}
	}
	if p, err := url.Parse(proxy); proxy != "" && err == nil {
		if password, ok := p.User.Password(); ok && password != "" {
			out = append(out, password)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b)) })
	return slices.Compact(out)
}

// basicSecrets is what a Basic credential encodes that a remote may echo
// decoded — the user and password, and the password alone — each at least
// [minDerivedSecret] long; nothing for another scheme, or a credential that
// is not base64.
func basicSecrets(scheme, credential string) []string {
	if !strings.EqualFold(scheme, "Basic") {
		return nil
	}
	plain, err := base64.StdEncoding.DecodeString(credential)
	if err != nil {
		return nil
	}
	var out []string
	if len(plain) >= minDerivedSecret {
		out = append(out, string(plain))
	}
	if _, password, ok := strings.Cut(string(plain), ":"); ok && len(password) >= minDerivedSecret {
		out = append(out, password)
	}
	return out
}

// URL is the URL git is handed, as ResolveRemote wrote it: what a run's
// arguments must name where git reads the remote ([Cmd.Remote]).
func (r Remote) URL() string { return r.url }

// Addrs is what git connects to for the remote's host: its addresses, each
// judged, in the order the resolver answered — the address itself for a host
// that is one — and none through a proxy, which resolves the host itself.
func (r Remote) Addrs() []netip.Addr { return slices.Clone(r.addrs) }

// String is the remote as a log line may name it: its URL, and the addresses
// it is pinned to or the proxy it is dialed through, the proxy's password
// redacted — and never its header, whose presence alone it says.
func (r Remote) String() string {
	if r.url == "" {
		return "no remote"
	}
	var b strings.Builder
	b.WriteString(r.url)
	switch {
	case r.proxy != "":
		b.WriteString(" through ")
		if p, err := url.Parse(r.proxy); err == nil {
			b.WriteString(p.Redacted())
		}
	case len(r.addrs) > 0:
		b.WriteString(" at ")
		for i, a := range r.addrs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.String())
		}
	}
	if r.header != "" {
		b.WriteString(", with a header")
	}
	return b.String()
}

// GoString is [Remote.String], so %#v prints no credential either.
func (r Remote) GoString() string { return "gitproc.Remote(" + r.String() + ")" }

// config is the configuration r's run is given after the base, in this order
// (the package doc's "One outbound dial" gives each its reason). Its TCP
// keepalive is derived from stall, the pool's [TransferStall], in the whole
// seconds git reads: quiet for half of it before the first probe, and the
// rest shared among [remoteKeepAliveProbes] probes.
func (r *Remote) config(stall time.Duration) []KV {
	seconds := int(stall / time.Second)
	idle := max(1, seconds/2)
	interval := max(1, (seconds-idle)/remoteKeepAliveProbes)
	kv := []KV{
		{keyHTTPSAllow, "always"},
		{keyFollowRedirects, "false"},
		{keyBundleURI, "false"},
		{keyRecurse, "false"},
		{keyKeepAliveIdle, strconv.Itoa(idle)},
		{keyKeepAliveInterval, strconv.Itoa(interval)},
		{keyKeepAliveCount, strconv.Itoa(remoteKeepAliveProbes)},
	}
	switch {
	case r.proxy != "":
		kv = append(kv, KV{keyProxy, r.proxy})
	case !r.literal:
		kv = append(kv, KV{keyResolve, r.pin()})
	}
	if r.caFile != "" {
		kv = append(kv, KV{keyCAInfo, r.caFile})
	}
	if r.caDir != "" {
		kv = append(kv, KV{keyCAPath, r.caDir})
	}
	if r.header != "" {
		kv = append(kv, KV{keyExtraHeader, r.header})
	}
	return kv
}

// pin is the http.curloptResolve entry that is r's host's only answer to
// libcurl: "host:port:address[,address…]", an IPv6 address in brackets, in
// the order the resolver answered, so libcurl races the two families as
// net.Dialer would (RFC 8305) — and never with the '+' that would let the
// entry time out and the name be looked up again.
func (r *Remote) pin() string {
	addrs := make([]string, len(r.addrs))
	for i, a := range r.addrs {
		if addrs[i] = a.String(); a.Is6() {
			addrs[i] = "[" + addrs[i] + "]"
		}
	}
	return r.host + ":" + strconv.Itoa(r.port) + ":" + strings.Join(addrs, ",")
}

// known is the replacer that takes r's secrets out of a line, or nil for none.
func (r *Remote) known() *strings.Replacer {
	if len(r.secrets) == 0 {
		return nil
	}
	pairs := make([]string, 0, 2*len(r.secrets))
	for _, s := range r.secrets {
		pairs = append(pairs, s, remoteMarker)
	}
	return strings.NewReplacer(pairs...)
}

// lineRedactor hands w what is written to it a line at a time, a remote's
// secrets taken out of each (Remote.known): a remote run's stdout, whose
// remote has a secret. A line is held until it ends — a secret holds no line
// break, so a line holds it whole — or until it passes MaxLine, when what is
// held is handed on as a piece of one; the last is handed on by flush, once
// the stream has ended.
type lineRedactor struct {
	w       io.Writer
	known   *strings.Replacer
	pending []byte
}

// Write takes p, handing on every line it ends. It answers all of p taken
// unless the writer it hands on to fails.
func (l *lineRedactor) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.pending = append(l.pending, p...)
			if len(l.pending) > MaxLine {
				if err := l.flush(); err != nil {
					return 0, err
				}
			}
			break
		}
		l.pending = append(l.pending, p[:i+1]...)
		if err := l.flush(); err != nil {
			return 0, err
		}
		p = p[i+1:]
	}
	return n, nil
}

// flush hands on what is held, its secrets taken out.
func (l *lineRedactor) flush() error {
	if len(l.pending) == 0 {
		return nil
	}
	line := l.known.Replace(string(l.pending))
	l.pending = l.pending[:0]
	_, err := io.WriteString(l.w, line)
	return err
}

// redactDamage is damage with its message redacted as a remote run's lines
// are: known's secrets taken out (nil: none), then internal/redact's pass.
func redactDamage(damage error, known *strings.Replacer) error {
	d, ok := damage.(*Damage)
	if !ok {
		return damage
	}
	redacted := *d
	if known != nil {
		redacted.Message = known.Replace(redacted.Message)
	}
	redacted.Message = redact.Secrets(redacted.Message)
	return &redacted
}

// checkRemote holds a run's Remote to its subcommand — fetch, ls-remote and
// push take one and must, every other subcommand none — to its lane, and to
// what its arguments name.
//
// THE URL GIT DIALS IS THE REMOTE'S: git reads a remote from the first
// argument that is no option (or the first after "--"), so that argument
// must be the Remote's URL exactly as ResolveRemote wrote it, and NO OTHER
// ARGUMENT MAY NAME A URL — "://", or a remote helper's "<transport>::" —
// since a second https URL git dialed would be resolved by libcurl itself,
// where no pin and no rule reaches. An option whose value is a separate
// argument would be read as the remote here, so a remote run writes each
// option's value after an "=".
func checkRemote(c Cmd) error {
	dials := remoteCommands[c.Args[0]]
	switch {
	case c.Remote == nil && dials:
		return fmt.Errorf("%w: git %s dials a remote and carries no Cmd.Remote: name the remote it dials there "+
			"(ResolveRemote), since nothing else lets a git process reach another host", ErrInvalid, c.Args[0])
	case c.Remote == nil:
		return nil
	case !dials:
		return fmt.Errorf("%w: git %s carries a Cmd.Remote: only %s dial a remote", ErrInvalid, c.Args[0],
			strings.Join(slices.Sorted(maps.Keys(remoteCommands)), ", "))
	case c.Remote.url == "":
		return fmt.Errorf("%w: git %s's Cmd.Remote was not built by ResolveRemote, the one way to a remote a git "+
			"process may dial", ErrInvalid, c.Args[0])
	case c.Class != ClassTransfer:
		return fmt.Errorf("%w: git %s dials a remote in the %s lane: a remote paces its run as a client paces a "+
			"transfer, so it waits in the transfer lane", ErrInvalid, c.Args[0], c.Class)
	case c.Stall != 0:
		return fmt.Errorf("%w: git %s dials a remote and carries a Stall of %v: its network leg is libcurl's, past "+
			"the pool's pipes, and ends when its peer stops answering TCP's keepalive probes or at its Deadline, "+
			"while the pool's watchdog sees only what git writes on its pipes — nothing, from a fetch or a push "+
			"without --progress, until it ends — so it would end a transfer that is moving", ErrInvalid, c.Args[0], c.Stall)
	}
	at := -1
	for i := 1; i < len(c.Args); i++ {
		if c.Args[i] == "--" {
			if i+1 < len(c.Args) {
				at = i + 1
			}
			break
		}
		if !strings.HasPrefix(c.Args[i], "-") {
			at = i
			break
		}
	}
	if at < 0 || c.Args[at] != c.Remote.url {
		return fmt.Errorf("%w: git %s's remote — its first argument that is no option — must be its Cmd.Remote's URL, "+
			"%s, as Remote.URL writes it, with every option before it written --name=value", ErrInvalid, c.Args[0],
			c.Remote.url)
	}
	for i, a := range c.Args[1:] {
		if i+1 != at && (strings.Contains(a, "://") || strings.Contains(a, "::")) {
			return fmt.Errorf("%w: git %s names a URL beside its remote's (argument %d): a remote run dials its "+
				"Cmd.Remote and nothing else", ErrInvalid, c.Args[0], i+1)
		}
	}
	return nil
}

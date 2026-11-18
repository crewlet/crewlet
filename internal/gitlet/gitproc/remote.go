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
// A SET, never a []string: the cache's writers gate
// (TestNothingButSyncWritesACache, internal/gitlet/cache) reads a []string
// literal beginning with a git writer as an argument list built away from its
// Cmd, and would flag this one as a fetch outside the files allowed to run one.
var remoteCommands = map[string]bool{"fetch": true, "ls-remote": true, "push": true}

// The configuration the pool writes for a remote run after the base, from its
// [Cmd.Remote] (Remote.config; package doc, "One outbound dial"). No call site
// may set one (checkConfig), as none may set a base key.
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

// remoteKeepAliveProbes is how many TCP keepalive probes in a row a remote
// run's connection may leave unanswered before the kernel ends it
// (http.keepAliveCount): after quiet for half of [TransferStall], three ten
// seconds apart ([Remote.config]), so a vanished peer is ended a minute after
// the last thing it heard, and one lost probe ends nothing. A push with bytes
// in flight sends no probe: TCP retransmits until Linux's tcp_retries2 (~15
// minutes), since neither git nor libcurl sets TCP_USER_TIMEOUT.
//
// Liveness, never speed (package doc, "One outbound dial"). MEASURED (git
// 2.56.0, libcurl 8.14.1, TLS, loopback): libcurl's low-speed check at a byte
// a second ended remotes sending only git's keepalive, one fetch in twelve
// over ten seconds; three in five over a minute at one per 10-30 s.
const remoteKeepAliveProbes = 3

// remoteMarker is what a run's lines read where one of its remote's secrets
// stood, in internal/redact's marker form, so a second redaction pass
// recognises its work and a reader tells a removal from git's own words.
const remoteMarker = redact.Marker + "credential]"

// minDerivedSecret is the shortest piece [secretsOf] derives from a Basic
// credential (user:password, and the password) to take out of a run's lines:
// no forge issues a shorter credential, and taking a shorter word (a user named
// git, a password of x) out of every line would make git's account unreadable.
// What the run was handed whole is taken out whatever its length.
const minDerivedSecret = 8

// Remote is the one host a git process may dial ([Cmd.Remote]), and how: an
// https URL; the addresses git connects to, resolved in Go and held to the
// public-address rule, or instead the process's proxy; the certificate
// authorities git verifies it with; and one header, a credential, sent with
// every request ([Remote.WithHeader]). Package doc, "One outbound dial".
//
// [ResolveRemote] is the only way to a Remote that dials: its fields are
// unexported, so no call site hands git a host or an address the rule did not
// judge, and every run refuses the zero Remote. It PRINTS WITHOUT ITS HEADER
// or a proxy's password ([Remote.String]).
type Remote struct {
	// url is the URL git is handed, as remoteURL wrote it back from what it
	// parsed, never the text it was given.
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
	// public unless private ones were allowed, in the resolver's order; the
	// literal's one; none through a proxy, which resolves the host itself.
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

// remoteEnv is what a remote is resolved against: the resolver (also asked for
// the network's NAT64 prefixes), the process's proxy for a URL, and its
// SSL_CERT_FILE and SSL_CERT_DIR. ResolveRemote passes the process's own; the
// suite, which reaches no network, its own.
type remoteEnv struct {
	resolver httpx.Resolver
	proxy    func(*url.URL) (*url.URL, error)
	caFile   string
	caDirs   string
}

// ResolveRemote is the remote at rawURL a git process may dial ([Cmd.Remote]),
// its URL written back in one form ([Remote.URL]): https; a DNS name whose
// last label begins with a letter, or an address; no user, query or fragment;
// otherwise [ErrRemote], naming what to change. Package doc, "One outbound dial".
// Its host's addresses are pinned, and one that is not public (as
// httpx.ResolvePublic judges it, NAT64 included) refuses the name whole,
// httpx.ErrNotPublic, unless allowPrivate (Tier A gitlet.import_allow_private).
// Through a proxy the process names for the URL (HTTPS_PROXY, NO_PROXY) nothing
// is resolved here: the proxy, the operator's, is the wall, and allowPrivate
// changes nothing. It resolves on every call and keeps nothing: a kept answer
// would judge an address the name may no longer have. A failed lookup is the
// resolver's error, wrapped.
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

// remoteURL reads rawURL through [gitref.ParseRemoteURL], THE ONE READING (the
// code domain and the configuration hold a mirror or import to it too): the
// Remote it names, no addresses yet, and the URL as parsed; or ErrRemote.
func remoteURL(rawURL string) (Remote, *url.URL, error) {
	parsed, err := gitref.ParseRemoteURL(rawURL)
	if err != nil {
		return Remote{}, nil, fmt.Errorf("%w: %w", ErrRemote, err)
	}
	return Remote{url: parsed.URL, host: parsed.Host, port: parsed.Port, literal: parsed.Addr.IsValid()},
		parsed.Parsed(), nil
}

// remoteProxy is the process's proxy p written so libcurl reads it as Go's
// transport does: socks5 becomes socks5h, since Go hands a SOCKS5 proxy the
// remote's NAME while libcurl's socks5:// resolves it on this node, through
// DNS no pin covers. A proxy neither reads is ErrRemote, naming its variable.
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

// remoteCADir is git's http.sslCAPath for SSL_CERT_DIR's value dirs: "" when
// it names no directory, the one it names, or ErrRemote naming the variable
// when it names several. The engine's clients read a colon-separated list
// (crypto/x509), but the GnuTLS libcurl of the image's git (scripts/git.sh)
// takes one directory and would trust nothing in a list; refused here, the
// operator learns why at the import or mirror, not as an untrusted
// certificate. An empty entry names nothing and is skipped.
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

// remoteAddrs is what git connects to for host: every address res answers,
// held to the public-address rule (httpx.ResolvePublic) unless allowPrivate,
// once each, unmapped, in the order answered.
func remoteAddrs(ctx context.Context, res httpx.Resolver, host string, allowPrivate bool) ([]netip.Addr, error) {
	var addrs []netip.Addr
	var err error
	if !allowPrivate {
		addrs, err = httpx.ResolvePublic(ctx, res, host)
		if errors.Is(err, httpx.ErrNotPublic) {
			return nil, fmt.Errorf("%w — a git process dials another host only at an address on the public "+
				"internet, unless gitlet.import_allow_private is set on the node that runs it", err)
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

// WithHeader is r sending header, ONE line "Name: value" (the credential, an
// `Authorization: Basic …` or `Bearer …`), with every request git makes to the
// remote, replacing any header r had. git gets it through the run's
// environment (http.extraHeader), never its arguments, and its secrets are
// taken out of every line the run hands back ([secretsOf]). The name must be
// an HTTP token and the value one line of visible characters, since a line
// break would write a second header; otherwise ErrRemote, quoting nothing of
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
// its header's value: the value whole; the credential after its scheme, which
// a remote echoing it most likely prints; for Basic, [basicSecrets]; and the
// proxy's password. Longest first, so no secret leaves part of a longer one
// behind, each once. The material the engine KNOWS is secret, not shapes
// internal/redact (which runs after) hopes to recognise: git never prints the
// header, but the remote may echo exactly what it was sent.
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

// lineRedactor hands w a remote run's stdout a line at a time, Remote.known's
// secrets taken out of each. A line is held until it ends, since a secret holds
// no line break, or until it passes MaxLine, when it is handed on in pieces;
// flush hands on the last once the stream has ended.
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

// redactDamage is the *Damage in damage's chain, its message redacted as a
// remote run's lines are (known's secrets, nil for none, then internal/redact),
// answered alone, since a wrap around it would carry its text unredacted.
func redactDamage(damage error, known *strings.Replacer) error {
	var d *Damage
	if !errors.As(damage, &d) {
		return damage
	}
	redacted := *d
	if known != nil {
		redacted.Message = known.Replace(redacted.Message)
	}
	redacted.Message = redact.Secrets(redacted.Message)
	return &redacted
}

// checkRemote holds a run's Remote to its subcommand (only remoteCommands take
// one, and must), to the transfer lane, and to its arguments. git dials the
// first argument that is no option (or the first after "--"), so that must be
// the Remote's URL exactly, and NO OTHER ARGUMENT MAY NAME A URL ("://" or a
// remote helper's "<transport>::"): libcurl would resolve a second one itself,
// past every pin and rule. A remote run writes each option's value after an
// "=", or the value would be read here as the remote.
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

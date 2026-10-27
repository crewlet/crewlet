package gitproc_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/redact"
)

// remoteHost is the name every remote a case dials is written with. The
// certificate every httptest TLS server presents names example.com and
// *.example.com, and no DNS answers this name with a server of a case's, so
// a run that reaches one reached it at the address it was pinned to.
const remoteHost = "git.example.com"

// ipv4onlyArpa is the name RFC 7050 discovery asks AAAA records of, rooted as
// httpx asks it: the network's NAT64 prefixes are read from its answers.
const ipv4onlyArpa = "ipv4only.arpa."

// resolver is a DNS a case writes: the addresses each name has — every name
// it has no row for answering fallback — the error a name's lookup fails
// with, and every lookup asked of it. A name with no address of the family
// asked is not found, as a resolver says of a name with no record of the type.
type resolver struct {
	answers  map[string][]netip.Addr
	fallback []netip.Addr
	fails    map[string]error

	mu    sync.Mutex
	asked []string
}

func (r *resolver) LookupNetIP(_ context.Context, network, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.asked = append(r.asked, network+" "+host)
	r.mu.Unlock()
	if err := r.fails[host]; err != nil {
		return nil, err
	}
	answers, ok := r.answers[host]
	if !ok && host != ipv4onlyArpa {
		answers = r.fallback
	}
	var out []netip.Addr
	for _, a := range answers {
		if network == "ip" || (network == "ip6") == (a.Is6() && !a.Is4In6()) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return out, nil
}

// lookups is every lookup asked so far, "<network> <host>" each.
func (r *resolver) lookups() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

// addrs is each of s parsed.
func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// resolve is the remote at rawURL against env, failing t if it is refused.
func resolve(t *testing.T, env gitproc.RemoteEnv, rawURL string, allowPrivate bool) gitproc.Remote {
	t.Helper()
	r, err := gitproc.ResolveRemoteWith(t.Context(), env, rawURL, allowPrivate)
	if err != nil {
		t.Fatalf("resolving %s: %v", rawURL, err)
	}
	return r
}

// publicDNS answers every name with one public address.
func publicDNS() *resolver { return &resolver{fallback: addrs("104.16.0.1")} }

// secretHex is a credential no other case and no process of the machine
// holds: sixteen random bytes in hex behind prefix.
func secretHex(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

// remoteRun is a run of the stand-in git body over r: a listing of r's refs,
// in the transfer lane, carrying config.
func remoteRun(t *testing.T, body string, r gitproc.Remote, config ...gitproc.KV) (gitproc.Result, error) {
	t.Helper()
	p := fakePool(t, body, gitproc.Options{})
	return p.Run(t.Context(), gitproc.Cmd{
		Args: []string{"ls-remote", r.URL()}, Remote: &r, Config: config,
		Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: gitproc.RemoteDeadline(0, 0),
	})
}

// remoteConfig is the configuration a remote run over r is handed, as a
// stand-in git that prints its environment reads it — the base, the remote's
// keys, then config — and the run's arguments, which the stand-in prints
// first.
func remoteConfig(t *testing.T, r gitproc.Remote, config ...gitproc.KV) (got []gitproc.KV, args []string) {
	t.Helper()
	res, err := remoteRun(t, `for a in "$@"; do printf 'ARG=%s\n' "$a"; done; exec env`, r, config...)
	if err != nil {
		t.Fatalf("a remote run over %v: %v", r, err)
	}
	var env []string
	for line := range strings.Lines(string(res.Stdout)) {
		if arg, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "ARG="); ok {
			args = append(args, arg)
			continue
		}
		env = append(env, line)
	}
	return configOf(t, envOf([]byte(strings.Join(env, "")))), args
}

// baseConfig is the base every process is handed, in order: the pool's six
// keys, the hooks path pointing at no directory.
var baseConfig = []gitproc.KV{
	{Key: "protocol.allow", Value: "never"},
	{Key: "gc.auto", Value: "0"},
	{Key: "maintenance.auto", Value: "false"},
	{Key: "pack.threads", Value: "1"},
	{Key: "core.hooksPath", Value: "/dev/null/no-hooks"},
	{Key: "core.attributesFile", Value: "/dev/null/no-attributes"},
}

// remoteBase is what every remote run is handed after the base, in order,
// before where its host is reached and what it trusts: its TCP keepalive
// among it, quiet for half of TransferStall before the first probe and three
// probes over the rest — and no speed bound.
var remoteBase = []gitproc.KV{
	{Key: "protocol.https.allow", Value: "always"},
	{Key: "http.followRedirects", Value: "false"},
	{Key: "transfer.bundleURI", Value: "false"},
	{Key: "fetch.recurseSubmodules", Value: "false"},
	{Key: "http.keepAliveIdle", Value: "30"},
	{Key: "http.keepAliveInterval", Value: "10"},
	{Key: "http.keepAliveCount", Value: "3"},
}

// A REMOTE RESOLVING TO AN ADDRESS OFF THE PUBLIC INTERNET IS REFUSED WHOLE,
// before git is told anything: a private, loopback, link-local or
// unique-local address, written as the host or answered for its name; an IPv6
// address carrying such an IPv4 address through NAT64 — the well-known
// prefix, and the prefix the network's own resolver names, which sits in
// global unicast where only the rule WITH the network's prefixes reads the
// address it carries; an address under a prefix the resolver names in
// unique-local space, whatever it carries, since httpx takes no resolver's
// word that would open a network off the internet; and a name answering one
// public and one private address, which its owner can point either way
// between the judgment and the next lookup. The refusal is
// httpx.ErrNotPublic, naming git.import_allow_private — which pins the
// answers as they come — and a resolver that cannot say which prefixes its
// network translates through, for an address whose verdict they decide, fails
// the resolve rather than judging on a guess.
//
// Mutation: judge the answers by the rule without the network's discovered
// prefixes, and the DNS64 row goes red; count a prefix the resolver names
// wherever it lies, and the unique-local DNS64 row does; pin a private answer
// without git.import_allow_private, and every refused row does.
func TestARemoteResolvingPrivateIsRefused(t *testing.T) {
	t.Parallel()
	const named = "https://" + remoteHost + "/team/r.git"
	// The network's own NAT64 prefix, 2606:4700:64::/96: its resolver
	// synthesizes ipv4only.arpa's 192.0.0.170 into it.
	dns64 := addrs("2606:4700:64::c000:aa")
	for _, tc := range []struct {
		name, url string
		answers   []netip.Addr // the remote's host's addresses
		nat64     []netip.Addr // ipv4only.arpa's AAAA answers
		pinned    []netip.Addr // what an allowed private remote is pinned to
	}{
		{name: "a private address", url: named, answers: addrs("10.0.0.5")},
		{name: "loopback", url: named, answers: addrs("127.0.0.1")},
		{name: "the cloud metadata address", url: named, answers: addrs("169.254.169.254")},
		{name: "a unique-local IPv6 address", url: named, answers: addrs("fd00::5")},
		{name: "a public and a private address", url: named, answers: addrs("104.16.0.1", "10.0.0.5")},
		{name: "the well-known NAT64 prefix carrying a private address", url: named,
			answers: addrs("64:ff9b::a00:5")},
		{name: "the network's own NAT64 prefix carrying a private address", url: named,
			answers: addrs("2606:4700:64::a00:5"), nat64: dns64},
		// fd00:64::/96, named by the network's resolver, carrying
		// 104.16.0.1: a resolver that could have this counted could call
		// public any host of the unique-local network whose low 32 bits read
		// as a public IPv4 address.
		{name: "a unique-local NAT64 prefix carrying a public address", url: named,
			answers: addrs("fd00:64::6810:1"), nat64: addrs("fd00:64::c000:aa")},
		{name: "a private address literal", url: "https://10.0.0.5/r.git", pinned: addrs("10.0.0.5")},
		{name: "an IPv6 loopback literal", url: "https://[::1]/r.git", pinned: addrs("::1")},
		{name: "a private address written in IPv6", url: "https://[::ffff:10.0.0.5]/r.git", pinned: addrs("10.0.0.5")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dns := &resolver{answers: map[string][]netip.Addr{remoteHost: tc.answers, ipv4onlyArpa: tc.nat64}}
			env := gitproc.RemoteEnv{Resolver: dns}
			r, err := gitproc.ResolveRemoteWith(t.Context(), env, tc.url, false)
			if !errors.Is(err, httpx.ErrNotPublic) || !strings.Contains(err.Error(), "git.import_allow_private") {
				t.Fatalf("resolved to %v, %v: want httpx.ErrNotPublic naming git.import_allow_private", r, err)
			}
			// git.import_allow_private pins the answers as they come.
			allowed := resolve(t, env, tc.url, true)
			want := tc.pinned
			if want == nil {
				want = tc.answers
			}
			if got := allowed.Addrs(); !slices.Equal(got, want) {
				t.Errorf("allowed private, it is pinned to %v, want %v", got, want)
			}
		})
	}

	for _, tc := range []struct {
		name           string
		answers, nat64 []netip.Addr
	}{
		{name: "a public address", answers: addrs("104.16.0.1")},
		{name: "a public address of each family, in the order answered",
			answers: addrs("2606:4700::1", "104.16.0.1")},
		{name: "the well-known NAT64 prefix carrying a public address", answers: addrs("64:ff9b::6810:1")},
		{name: "the network's own NAT64 prefix carrying a public address",
			answers: addrs("2606:4700:64::6810:1"), nat64: dns64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dns := &resolver{answers: map[string][]netip.Addr{remoteHost: tc.answers, ipv4onlyArpa: tc.nat64}}
			r := resolve(t, gitproc.RemoteEnv{Resolver: dns}, named, false)
			if got := r.Addrs(); !slices.Equal(got, tc.answers) {
				t.Errorf("pinned to %v, want %v", got, tc.answers)
			}
		})
	}

	// A resolver that cannot say which NAT64 prefixes its network translates
	// through is not trusted with a verdict they decide: an address in global
	// unicast, which a prefix the network names could hold, fails the
	// resolve with the resolver's own error, and nothing is judged on a guess.
	failing := &net.DNSError{Err: "server misbehaving", Name: ipv4onlyArpa, IsTemporary: true}
	dns := &resolver{answers: map[string][]netip.Addr{remoteHost: addrs("2606:4700::1")},
		fails: map[string]error{ipv4onlyArpa: failing}}
	r, err := gitproc.ResolveRemoteWith(t.Context(), gitproc.RemoteEnv{Resolver: dns}, named, false)
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || dnsErr != failing || errors.Is(err, httpx.ErrNotPublic) {
		t.Errorf("a failed discovery resolved to %v, %v: want the resolver's own error", r, err)
	}
}

// A REMOTE'S URL IS READ ONE WAY AND WRITTEN BACK THAT WAY: what git is handed
// is the URL as the engine parsed it — https, a lowercase name of letters,
// digits and hyphens whose last label begins with a letter or an address in
// its one written form, the default port left out — because two parsers that
// read one URL differently are how a check of one host becomes a dial of
// another. Anything another parser could read as a different host (a trailing
// dot, an IPv4 address in octal, hex or fewer than four parts, a name libcurl
// would turn into punycode, a zone), any other scheme, and what git would
// write into its children's arguments (a user, a password) is refused
// ErrRemote — quoting no password.
func TestARemoteURLIsWrittenBackOneWay(t *testing.T) {
	t.Parallel()
	env := gitproc.RemoteEnv{Resolver: publicDNS()}
	for given, want := range map[string]string{
		"https://Git.Example.COM/team/r.git":       "https://git.example.com/team/r.git",
		"https://git.example.com:443/r.git":        "https://git.example.com/r.git",
		"https://git.example.com:/r.git":           "https://git.example.com/r.git",
		"https://git.example.com:8443/r.git":       "https://git.example.com:8443/r.git",
		"https://git.example.com":                  "https://git.example.com",
		"https://git.example.com/a%20b.git":        "https://git.example.com/a%20b.git",
		"https://xn--gt-fia.example.com/r.git":     "https://xn--gt-fia.example.com/r.git",
		"https://104.16.0.1/r.git":                 "https://104.16.0.1/r.git",
		"https://[2606:4700:0:0:0:0:0:1]/r.git":    "https://[2606:4700::1]/r.git",
		"https://[2606:4700::1]:8443/r.git":        "https://[2606:4700::1]:8443/r.git",
		"https://[2606:4700::ABCD]:443/team/r.git": "https://[2606:4700::abcd]/team/r.git",
	} {
		if got := resolve(t, env, given, false).URL(); got != want {
			t.Errorf("%s is written back %s, want %s", given, got, want)
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
		r, err := gitproc.ResolveRemoteWith(t.Context(), env, given, true)
		if !errors.Is(err, gitproc.ErrRemote) {
			t.Errorf("%q resolved to %v, %v: want ErrRemote", given, r, err)
			continue
		}
		if strings.Contains(err.Error(), password) {
			t.Errorf("%q's refusal quotes its password: %v", given, err)
		}
	}
}

// A REMOTE BEHIND THE PROCESS'S PROXY IS DIALED THROUGH IT, AS ITSELF: the
// proxy the process names for the URL resolves the host, so nothing is
// resolved here — no lookup, no pin, no rule, git.import_allow_private
// changing nothing — and git is handed the proxy as libcurl reads the one Go
// read: a SOCKS5 proxy as socks5h, which hands the proxy the name rather than
// resolving it on this node, where no pin covers it. A proxy neither reads is
// refused, naming the variable that set it, and a proxy's password prints
// nowhere a log line reads a remote.
func TestARemoteBehindAProxyIsDialedThroughIt(t *testing.T) {
	t.Parallel()
	const password = "pr0xy-passw0rd"
	for given, want := range map[string]string{
		"http://proxy.example.com:3128":                         "http://proxy.example.com:3128",
		"https://proxy.example.com:3129":                        "https://proxy.example.com:3129",
		"http://user:" + password + "@proxy.example.com:3128":   "http://user:" + password + "@proxy.example.com:3128",
		"socks5://proxy.example.com:1080":                       "socks5h://proxy.example.com:1080",
		"socks5h://proxy.example.com:1080":                      "socks5h://proxy.example.com:1080",
		"socks5://user:" + password + "@proxy.example.com:1080": "socks5h://user:" + password + "@proxy.example.com:1080",
	} {
		dns := &resolver{fallback: addrs("10.0.0.5")}
		proxy, err := url.Parse(given)
		if err != nil {
			t.Fatal(err)
		}
		env := gitproc.RemoteEnv{Resolver: dns, Proxy: func(*url.URL) (*url.URL, error) { return proxy, nil }}
		r := resolve(t, env, "https://"+remoteHost+"/r.git", false)
		if asked := dns.lookups(); len(asked) > 0 || len(r.Addrs()) > 0 {
			t.Errorf("through %s: the resolver was asked %v and the remote pinned to %v: the proxy resolves the host",
				given, asked, r.Addrs())
		}
		// The stand-in prints its environment on stdout, which a remote run
		// hands on with the proxy's password taken out.
		want = strings.ReplaceAll(want, password, redact.Marker+"credential]")
		config, _ := remoteConfig(t, r)
		if wantConfig := slices.Concat(baseConfig, remoteBase, []gitproc.KV{{Key: "http.proxy", Value: want}}); !slices.Equal(config, wantConfig) {
			t.Errorf("through %s: configured %v, want %v", given, config, wantConfig)
		}
		for _, printed := range []string{r.String(), fmt.Sprintf("%v", r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r)} {
			if strings.Contains(printed, password) || !strings.Contains(printed, "proxy.example.com") {
				t.Errorf("through %s: the remote prints %q: want the proxy without its password", given, printed)
			}
		}
	}
	for given, says := range map[string]string{
		"ftp://proxy.example.com:21": "HTTPS_PROXY",
		"http://":                    "HTTPS_PROXY",
	} {
		proxy, err := url.Parse(given)
		if err != nil {
			t.Fatal(err)
		}
		env := gitproc.RemoteEnv{Resolver: publicDNS(), Proxy: func(*url.URL) (*url.URL, error) { return proxy, nil }}
		r, err := gitproc.ResolveRemoteWith(t.Context(), env, "https://"+remoteHost+"/r.git", false)
		if !errors.Is(err, gitproc.ErrRemote) || !strings.Contains(err.Error(), says) {
			t.Errorf("through %s: %v, %v: want ErrRemote naming %s", given, r, err, says)
		}
	}
	unreadable := errors.New("invalid proxy address")
	env := gitproc.RemoteEnv{Resolver: publicDNS(), Proxy: func(*url.URL) (*url.URL, error) { return nil, unreadable }}
	if r, err := gitproc.ResolveRemoteWith(t.Context(), env, "https://"+remoteHost+"/r.git", false); !errors.Is(err, unreadable) ||
		!strings.Contains(err.Error(), "HTTPS_PROXY") {
		t.Errorf("an unreadable proxy: %v, %v: want its error, naming HTTPS_PROXY", r, err)
	}
}

// A REMOTE RUN IS CONFIGURED BY THE POOL, AFTER THE BASE AND BEFORE THE CALL
// SITE: https alone; no redirect, bundle URI or submodule followed; TCP's
// keepalive watching the peer, and nothing bounding the leg's speed; the host
// pinned to every address the rule judged, in the order answered, and never
// with the '+' that would let the entry lapse into a lookup; the process's
// certificate authorities; and the header — every one in the process's
// environment and none in its arguments, which are exactly the call's. A call
// site that names one of those keys itself, in any case, is refused ErrConfig
// and pointed at Cmd.Remote, as a base key is, and one that names libcurl's
// speed bound is refused as every key off the allowlist is.
//
// Mutation: write the remote's keys after the call site's, and the order is
// red; drop the pin, and so is the configuration.
func TestARemoteRunIsConfiguredByThePool(t *testing.T) {
	t.Parallel()
	secret := secretHex(t, "ghs_")
	dns := &resolver{answers: map[string][]netip.Addr{remoteHost: addrs("2606:4700::1", "104.16.0.1")}}
	env := gitproc.RemoteEnv{Resolver: dns, CAFile: "/etc/crewlet/ca.pem", CADirs: "/etc/crewlet/ca.d"}
	r, err := resolve(t, env, "https://"+remoteHost+":8443/r.git", false).WithHeader("Authorization: Bearer " + secret)
	if err != nil {
		t.Fatal(err)
	}
	callSite := gitproc.KV{Key: "core.quotePath", Value: "false"}
	config, args := remoteConfig(t, r, callSite)
	want := slices.Concat(baseConfig, remoteBase, []gitproc.KV{
		{Key: "http.curloptResolve", Value: remoteHost + ":8443:[2606:4700::1],104.16.0.1"},
		{Key: "http.sslCAInfo", Value: "/etc/crewlet/ca.pem"},
		{Key: "http.sslCAPath", Value: "/etc/crewlet/ca.d"},
		// The stand-in prints its environment on stdout, which a remote run
		// with a secret hands on with the secret taken out: the header is
		// there, its value is not.
		{Key: "http.extraHeader", Value: "Authorization: " + redact.Marker + "credential]"},
		callSite,
	})
	if !slices.Equal(config, want) {
		t.Errorf("a remote run is configured\n  %v\nwant\n  %v", config, want)
	}
	if wantArgs := []string{"ls-remote", r.URL()}; !slices.Equal(args, wantArgs) {
		t.Errorf("a remote run's arguments are %q, want exactly the call's %q", args, wantArgs)
	}

	// A literal is no name: nothing resolves it, so nothing is pinned.
	literal := resolve(t, gitproc.RemoteEnv{Resolver: publicDNS()}, "https://104.16.0.1/r.git", false)
	if config, _ := remoteConfig(t, literal); !slices.Equal(config, slices.Concat(baseConfig, remoteBase)) {
		t.Errorf("a remote at an address literal is configured %v, want the base and the remote's keys alone", config)
	}

	p := fakePool(t, "exit 0", gitproc.Options{})
	for _, key := range []string{
		"protocol.https.allow", "http.followRedirects", "transfer.bundleURI", "fetch.recurseSubmodules",
		"http.keepAliveIdle", "http.keepAliveInterval", "http.keepAliveCount", "http.curloptResolve", "http.proxy",
		"http.sslCAInfo", "http.sslCAPath", "http.extraHeader", "HTTP.ExtraHeader", "HTTP.CURLOPTRESOLVE",
	} {
		_, err := p.Run(t.Context(), gitproc.Cmd{
			Args: []string{"ls-remote", r.URL()}, Remote: &r, Config: []gitproc.KV{{Key: key, Value: "x"}},
			Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: gitproc.RemoteDeadline(0, 0),
		})
		if !errors.Is(err, gitproc.ErrConfig) || !strings.Contains(err.Error(), "Cmd.Remote") {
			t.Errorf("a call site setting %s: %v, want ErrConfig pointing at Cmd.Remote", key, err)
		}
	}
	for _, key := range []string{"http.lowSpeedLimit", "http.lowSpeedTime"} {
		_, err := p.Run(t.Context(), gitproc.Cmd{
			Args: []string{"ls-remote", r.URL()}, Remote: &r, Config: []gitproc.KV{{Key: key, Value: "1"}},
			Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: gitproc.RemoteDeadline(0, 0),
		})
		if !errors.Is(err, gitproc.ErrConfig) {
			t.Errorf("a call site bounding a remote's speed with %s: %v, want ErrConfig", key, err)
		}
	}
}

// THE PROCESS'S CERTIFICATE AUTHORITIES ARE GIT'S: SSL_CERT_FILE is
// http.sslCAInfo and the directory SSL_CERT_DIR names is http.sslCAPath, so git
// trusts what the engine's own clients trust — and an SSL_CERT_DIR naming
// several directories, which crypto/x509 reads as a list and libcurl as one
// directory, is refused naming the variable, rather than leaving git trusting
// none of them.
func TestTheProcessAuthoritiesAreGits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, file, dirs string
		want             []gitproc.KV
	}{
		{name: "neither set", want: nil},
		{name: "a file", file: "/etc/ca.pem", want: []gitproc.KV{{Key: "http.sslCAInfo", Value: "/etc/ca.pem"}}},
		{name: "a directory", dirs: "/etc/ca.d", want: []gitproc.KV{{Key: "http.sslCAPath", Value: "/etc/ca.d"}}},
		{name: "a directory among empty entries", dirs: ":/etc/ca.d:",
			want: []gitproc.KV{{Key: "http.sslCAPath", Value: "/etc/ca.d"}}},
		{name: "a file and a directory", file: "/etc/ca.pem", dirs: "/etc/ca.d", want: []gitproc.KV{
			{Key: "http.sslCAInfo", Value: "/etc/ca.pem"}, {Key: "http.sslCAPath", Value: "/etc/ca.d"},
		}},
	} {
		env := gitproc.RemoteEnv{Resolver: publicDNS(), CAFile: tc.file, CADirs: tc.dirs}
		config, _ := remoteConfig(t, resolve(t, env, "https://104.16.0.1/r.git", false))
		if want := slices.Concat(baseConfig, remoteBase, tc.want); !slices.Equal(config, want) {
			t.Errorf("%s: configured %v, want %v", tc.name, config, want)
		}
	}
	env := gitproc.RemoteEnv{Resolver: publicDNS(), CADirs: "/etc/ca.d:/usr/local/share/ca.d"}
	if r, err := gitproc.ResolveRemoteWith(t.Context(), env, "https://104.16.0.1/r.git", false); !errors.Is(err, gitproc.ErrRemote) ||
		!strings.Contains(err.Error(), "SSL_CERT_DIR") {
		t.Errorf("two directories of authorities: %v, %v: want ErrRemote naming SSL_CERT_DIR", r, err)
	}
}

// A REMOTE'S HEADER IS ONE HEADER LINE, AND A REMOTE PRINTS WITHOUT IT: a
// name of HTTP's token characters and a value of one line — a carriage return
// or a line feed would write a second header nobody meant git to send — or
// ErrRemote, quoting nothing of the value; a header only for a remote
// ResolveRemote built; and a Remote printed as a log line would print it, by
// %v, %+v or %#v, names its URL and the addresses it is pinned to and says it
// carries a header, never the header itself.
func TestARemotesHeaderIsOneLineAndPrintsNowhere(t *testing.T) {
	t.Parallel()
	secret := secretHex(t, "glpat-")
	r := resolve(t, gitproc.RemoteEnv{Resolver: publicDNS()}, "https://"+remoteHost+"/r.git", false)
	for name, header := range map[string]string{
		"no colon":            "Bearer " + secret,
		"an empty name":       ": Bearer " + secret,
		"a name with a space": "Authorization Header: Bearer " + secret,
		"no value":            "Authorization:  ",
		"a line feed":         "Authorization: Bearer " + secret + "\nX-Planted: 1",
		"a carriage return":   "Authorization: Bearer " + secret + "\rX-Planted: 1",
		"a NUL":               "Authorization: Bearer " + secret + "\x00",
		"a control character": "Authorization: Bearer " + secret + "\x7f",
	} {
		got, err := r.WithHeader(header)
		if !errors.Is(err, gitproc.ErrRemote) {
			t.Errorf("%s: %v, %v: want ErrRemote", name, got, err)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the refusal quotes the value: %v", name, err)
		}
	}
	if got, err := (gitproc.Remote{}).WithHeader("Authorization: Bearer " + secret); !errors.Is(err, gitproc.ErrRemote) {
		t.Errorf("a header for a remote ResolveRemote did not build: %v, %v: want ErrRemote", got, err)
	}

	with, err := r.WithHeader("Authorization:\tBearer " + secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, printed := range []string{with.String(), fmt.Sprintf("%v", with), fmt.Sprintf("%+v", with),
		fmt.Sprintf("%#v", with), fmt.Sprint(&with)} {
		if strings.Contains(printed, secret) || !strings.Contains(printed, "https://"+remoteHost+"/r.git") ||
			!strings.Contains(printed, "104.16.0.1") || !strings.Contains(printed, "header") {
			t.Errorf("a remote prints %q: want its URL, its address and that it has a header, never the header", printed)
		}
	}
}

// A REMOTE RUN'S LINES NAME NONE OF ITS SECRETS. git never prints the header,
// but the remote, which holds it, may echo it — in the remote: lines git
// relays, the body of an HTTP error — so the header's value, the credential
// after its scheme and the user and password a Basic credential encodes are
// taken out of stderr's tail and of stdout, a line at a time however the
// writes split it, and out of the error the run returns: before the observer,
// a log line or a caller reads any of them.
//
// Mutation: hand stdout on unredacted, and the stdout rows go red; take the
// secrets out of stderr's tail once it is cut rather than as each line
// arrives, and nothing changes here — the tail is never cut — so the sink's
// own case is the line a run ends without a line feed.
func TestARemoteRunsLinesNameNoneOfItsSecrets(t *testing.T) {
	t.Parallel()
	password := secretHex(t, "pw-")
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + password))
	bearer := secretHex(t, "ghp_")
	for _, tc := range []struct {
		name, value string
		echoed      []string // what the remote echoes, each a secret
	}{
		{"bearer", "Bearer " + bearer, []string{"Bearer " + bearer, bearer}},
		{"basic", "Basic " + basic, []string{"Basic " + basic, basic, "x-access-token:" + password, password}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := resolve(t, gitproc.RemoteEnv{Resolver: publicDNS()}, "https://"+remoteHost+"/r.git", false).
				WithHeader("Authorization: " + tc.value)
			if err != nil {
				t.Fatal(err)
			}
			var body strings.Builder
			for _, s := range tc.echoed {
				// stderr: a whole line; stdout: one line written in two
				// pieces, split inside the secret.
				fmt.Fprintf(&body, "printf 'remote: this host was sent %%s\\n' %s >&2\n", quote(s))
				half := len(s) / 2
				fmt.Fprintf(&body, "printf 'heard %%s' %s; printf '%%s\\n' %s\n", quote(s[:half]), quote(s[half:]))
			}
			fmt.Fprintf(&body, "printf 'fatal: the last line names %%s' %s >&2\nexit 128\n", quote(tc.echoed[0]))
			res, err := remoteRun(t, body.String(), r)
			var exit *gitproc.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the stand-in's exit: %v, want an *ExitError", err)
			}
			for what, text := range map[string]string{
				"stderr": res.Stderr, "stdout": string(res.Stdout), "the error": err.Error(),
			} {
				for _, s := range tc.echoed {
					if strings.Contains(text, s) {
						t.Errorf("%s names %q:\n%s", what, s, text)
					}
				}
			}
			if !strings.Contains(res.Stderr, "remote: this host was sent "+redact.Marker) ||
				!strings.Contains(string(res.Stdout), "heard "+redact.Marker) ||
				!strings.Contains(err.Error(), "the last line names "+redact.Marker) {
				t.Errorf("the lines do not say where a secret was taken out:\nstderr %s\nstdout %s\nerror %v",
					res.Stderr, res.Stdout, err)
			}
		})
	}
}

// A REMOTE RUN IS HELD TO ITS REMOTE before any process starts: a fetch, an
// ls-remote and a push carry the Cmd.Remote ResolveRemote built — the zero
// Remote is none — and nothing else carries one; it waits in the transfer
// lane and carries no Stall, since the pool's watchdog cannot see its leg
// move, and TCP's keepalive watches its peer; and git's remote, the first
// argument that is no option (or the first after "--"), is the Remote's URL
// exactly, with no other argument naming a URL or a remote helper — an option
// whose value stands apart would be read as the remote, so each is written
// --name=value.
//
// The git here is a stand-in that only counts its runs: a refusal this case
// failed to see would otherwise be a real git dialing the public address its
// remote is pinned to.
//
// Mutation: read the remote from the last argument, or let a second URL or a
// Stall through, and the rows naming one go red.
func TestARemoteRunIsHeldToItsRemote(t *testing.T) {
	t.Parallel()
	ran := filepath.Join(t.TempDir(), "ran")
	p := fakePool(t, "printf '%s\\n' \"$1\" >> "+quote(ran), gitproc.Options{})
	runs := func() int {
		body, err := os.ReadFile(ran)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(body), "\n")
	}
	r := resolve(t, gitproc.RemoteEnv{Resolver: publicDNS()}, "https://"+remoteHost+"/r.git", false)
	other := resolve(t, gitproc.RemoteEnv{Resolver: publicDNS()}, "https://other.example.com/r.git", false)
	remote := func(args ...string) gitproc.Cmd {
		return gitproc.Cmd{Args: args, Remote: &r, Class: gitproc.ClassTransfer, Share: suiteShare,
			Deadline: gitproc.RemoteDeadline(0, 0)}
	}
	refused := map[string]gitproc.Cmd{
		"a fetch with no remote": {Args: []string{"fetch", r.URL()}, Class: gitproc.ClassTransfer, Share: suiteShare,
			Deadline: gitproc.RemoteDeadline(0, 0)},
		"an ls-remote with no remote": {Args: []string{"ls-remote", r.URL()}, Class: gitproc.ClassTransfer,
			Share: suiteShare, Deadline: gitproc.RemoteDeadline(0, 0)},
		"a push with no remote": {Args: []string{"push", r.URL(), "main"}, Class: gitproc.ClassTransfer,
			Share: suiteShare, Deadline: gitproc.RemoteDeadline(0, 0)},
		"a remote on another subcommand": remote("upload-pack", "--stateless-rpc", "/srv/r.git"),
		"the zero remote": {Args: []string{"fetch", ""}, Remote: &gitproc.Remote{}, Class: gitproc.ClassTransfer,
			Share: suiteShare, Deadline: gitproc.RemoteDeadline(0, 0)},
		"a remote in the read lane": {Args: []string{"ls-remote", r.URL()}, Remote: &r, Class: gitproc.ClassRead,
			Deadline: gitproc.ReadDeadline},
		"another remote's URL":        remote("fetch", other.URL()),
		"the URL as it was given":     remote("fetch", "https://GIT.example.com:443/r.git"),
		"no remote at all":            remote("fetch", "--quiet"),
		"an option's value apart":     remote("fetch", "--depth", "1", r.URL()),
		"a second URL":                remote("fetch", r.URL(), other.URL()),
		"a URL in an option":          remote("push", "--repo="+other.URL(), r.URL(), "main"),
		"a remote helper":             remote("fetch", r.URL(), "ext::sh -c id"),
		"a remote helper as remote":   remote("ls-remote", "ext::sh -c id"),
		"the remote after a stray --": remote("fetch", "--quiet", "--"),
	}
	stalled := remote("fetch", r.URL())
	stalled.Stall = gitproc.TransferStall
	refused["a stall watchdog"] = stalled
	for name, c := range refused {
		if _, err := p.Run(t.Context(), c); !errors.Is(err, gitproc.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if n := runs(); n != 0 {
		t.Fatalf("%d git processes ran for refused remote runs", n)
	}

	accepted := map[string]gitproc.Cmd{
		"a fetch":                  remote("fetch", "--quiet", "--prune", r.URL(), "+refs/heads/*:refs/heads/*"),
		"a fetch after --":         remote("fetch", "--quiet", "--", r.URL(), "+refs/tags/*:refs/tags/*"),
		"an option with its value": remote("fetch", "--depth=1", r.URL()),
		"a listing":                remote("ls-remote", "--symref", r.URL()),
		"a push":                   remote("push", "--porcelain", "--force", r.URL(), "refs/heads/main:refs/heads/main"),
	}
	for name, c := range accepted {
		if _, err := p.Run(t.Context(), c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := runs(); n != len(accepted) {
		t.Errorf("%d git processes ran for %d remote runs", n, len(accepted))
	}
}

// tlsRemote serves h over TLS for remoteHost: the remote a git process dials
// it at — the name resolved to the server's own address, which only
// git.import_allow_private lets a run dial, since it is loopback — with the
// file of the authority that signed the server's certificate as the
// process's SSL_CERT_FILE, and the server.
func tlsRemote(t *testing.T, path string, h http.Handler) (gitproc.Remote, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	at, err := netip.ParseAddrPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dns := &resolver{answers: map[string][]netip.Addr{remoteHost: {at.Addr()}}}
	r := resolve(t, gitproc.RemoteEnv{Resolver: dns, CAFile: ca},
		fmt.Sprintf("https://%s:%d%s", remoteHost, at.Port(), path), true)
	return r, srv
}

// fetchFrom fetches r's branches into dir through p.
func fetchFrom(t *testing.T, p *gitproc.Pool, r gitproc.Remote, dir string) (gitproc.Result, error) {
	t.Helper()
	return p.Run(t.Context(), gitproc.Cmd{
		Dir: dir, Args: []string{"fetch", "--quiet", r.URL(), "+refs/heads/*:refs/heads/*"}, Remote: &r,
		Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: gitproc.RemoteDeadline(1<<20, 0),
	})
}

// A REMOTE RUN DIALS THE ADDRESS THE ENGINE JUDGED, AND NOTHING ELSE ASKS DNS:
// a repository at git.example.com — a name no DNS answers with this case's
// server — is fetched whole and its refs listed, over TLS verified against the
// authority the process names, at the address the run was pinned to, on every
// git a git node may run.
//
// Mutation: leave http.curloptResolve out, and libcurl looks the name up
// itself and reaches no server of this case's; leave http.sslCAInfo out, and
// the server's certificate is one git does not trust.
func TestARemoteRunDialsTheAddressItWasPinnedTo(t *testing.T) {
	t.Parallel()
	onNodeGits(t, dialsThePinOn)
}

// dialsThePinOn is TestARemoteRunDialsTheAddressItWasPinnedTo on the git at
// git: the repository served as files (git's dumb protocol), which only an
// https remote run can reach.
func dialsThePinOn(t *testing.T, git string) {
	c := gittest.NewClient(t)
	wt := c.Init(t)
	c.Commit(t, wt, map[string]string{"a": "a\n"}, "first")
	main := c.Commit(t, wt, map[string]string{"b/c": "c\n"}, "second")
	served := gittest.Empty(t)
	gittest.Pack(t, c, served, wt, main)
	gittest.SetRefs(t, served, map[string]string{"refs/heads/main": main})
	c.Run(t, "", "--git-dir="+served, "update-server-info")
	var asked atomic.Int64
	files := http.StripPrefix("/r.git/", http.FileServer(http.Dir(served)))
	r, _ := tlsRemote(t, "/r.git", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		asked.Add(1)
		files.ServeHTTP(w, req)
	}))

	p := poolOn(t, git, gitproc.Options{})
	into := gittest.Empty(t)
	if res, err := fetchFrom(t, p, r, into); err != nil {
		t.Fatalf("fetching from %v: %v\n%s", r, err, res.Stderr)
	}
	if got := strings.TrimSpace(c.Run(t, "", "--git-dir="+into, "rev-parse", "refs/heads/main")); got != main {
		t.Errorf("the fetch moved main to %s, want %s", got, main)
	}
	c.Run(t, "", "--git-dir="+into, "fsck", "--strict", "--no-dangling")

	res, err := p.Run(t.Context(), gitproc.Cmd{
		Args: []string{"ls-remote", r.URL()}, Remote: &r,
		Class: gitproc.ClassTransfer, Share: suiteShare, Deadline: gitproc.RemoteDeadline(0, 0),
	})
	if err != nil {
		t.Fatalf("listing %v: %v\n%s", r, err, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), main+"\trefs/heads/main\n") {
		t.Errorf("the listing is %q, want main at %s", res.Stdout, main)
	}
	if asked.Load() == 0 {
		t.Error("the server was asked nothing")
	}
}

// A REDIRECT IS NOT FOLLOWED: the host a remote run dials is the one the rule
// judged and the run pinned, so a remote that answers with another host's
// address — whatever that host is — ends the run, and the other host is
// never asked anything.
//
// Mutation: http.followRedirects=true (or git's own default, initial), and the
// second server is asked for the refs the first sent the run to.
func TestARedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	onNodeGits(t, redirectOn)
}

// redirectOn is TestARedirectIsNotFollowed on the git at git.
func redirectOn(t *testing.T, git string) {
	var first, second atomic.Int64
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		second.Add(1)
		http.NotFound(w, req)
	}))
	t.Cleanup(elsewhere.Close)
	r, _ := tlsRemote(t, "/r.git", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		first.Add(1)
		http.Redirect(w, req, elsewhere.URL+req.URL.RequestURI(), http.StatusFound)
	}))

	res, err := fetchFrom(t, poolOn(t, git, gitproc.Options{}), r, gittest.Empty(t))
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("a fetch answered with a redirect: %v, want an *ExitError\n%s", err, res.Stderr)
	}
	if first.Load() == 0 {
		t.Fatalf("the remote was never asked, so the run did not reach it: %s", res.Stderr)
	}
	if n := second.Load(); n != 0 {
		t.Errorf("the host the remote redirected to was asked %d times: a run dials its remote's host alone", n)
	}
}

// A QUIET REMOTE IS WAITED ON. A remote run's connection is ended when its
// peer stops answering — TCP's keepalive probes left unanswered a stall after
// the last thing it heard — and never for what it sends: a git server counting
// and compressing what a large fetch wants, or indexing and checking what a
// push sent, may send nothing but a keepalive every five seconds, or nothing
// at all, for minutes, and a run ended for that would be ended on every
// attempt. A remote that answers a fetch's request and then sends NOTHING —
// no progress, no keepalive, the quietest a working server is — for two and a
// half times the stall, scaled to six seconds so the probes go out every
// second after three seconds of quiet (and are answered), is waited on, and
// the pack it then sends is fetched whole, on every git a git node may run.
//
// Mutation: hand libcurl back its low-speed check (http.lowSpeedLimit 1,
// http.lowSpeedTime the stall), and the run is ended — curl 28, "Operation
// too slow" — a few seconds past the stall, while the remote still holds it.
func TestAQuietRemoteIsWaitedOn(t *testing.T) {
	t.Parallel()
	onNodeGits(t, quietRemoteOn)
}

// quietStall is the stall TestAQuietRemoteIsWaitedOn scales its pool to: six
// seconds, whose half and the third of the rest — the keepalive's quiet before
// its first probe, and its interval — are whole seconds, as git reads them.
const quietStall = 6 * time.Second

// quietRemoteOn is TestAQuietRemoteIsWaitedOn on the git at git.
func quietRemoteOn(t *testing.T, git string) {
	c := gittest.NewClient(t)
	wt := c.Init(t)
	tip := c.Commit(t, wt, map[string]string{"a": "a\n"}, "first")
	served := gittest.Empty(t)
	checksum := gittest.Pack(t, c, served, wt, tip)
	pack, err := os.ReadFile(filepath.Join(served, "objects", "pack", "pack-"+checksum+".pack"))
	if err != nil {
		t.Fatal(err)
	}
	hold := 5 * quietStall / 2
	r, _ := tlsRemote(t, "/r.git", quietServer(tip, pack, hold))
	p := poolOn(t, git, gitproc.Options{})
	gitproc.SetRemoteStall(p, quietStall)

	into := gittest.Empty(t)
	began := time.Now()
	res, err := fetchFrom(t, p, r, into)
	took := time.Since(began)
	if err != nil {
		t.Fatalf("a fetch from a remote quiet for %v was ended after %v: %v\n%s", hold, took, err, res.Stderr)
	}
	if took < hold {
		t.Fatalf("the fetch returned after %v, inside the remote's %v of quiet: it never waited", took, hold)
	}
	if got := strings.TrimSpace(c.Run(t, "", "--git-dir="+into, "rev-parse", "refs/heads/main")); got != tip {
		t.Errorf("the fetch moved main to %s, want %s", got, tip)
	}
	c.Run(t, "", "--git-dir="+into, "fsck", "--strict", "--no-dangling")
}

// quietServer is a git server — smart HTTP at protocol version 0, its one
// ref, main, naming tip and its objects pack — that works quietly before it
// answers: a fetch's request is answered NAK at once, then nothing at all for
// hold, then the pack over side band 1, as upload-pack answers once
// pack-objects has counted and compressed with no progress asked for and no
// keepalive configured.
func quietServer(tip string, pack []byte, hold time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/r.git/info/refs":
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = io.WriteString(w, pkt("# service=git-upload-pack\n")+"0000"+
				pkt(tip+" refs/heads/main\x00side-band-64k ofs-delta no-progress\n")+"0000")
		case req.Method == http.MethodPost && req.URL.Path == "/r.git/git-upload-pack":
			_, _ = io.Copy(io.Discard, req.Body)
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			_, _ = io.WriteString(w, pkt("NAK\n"))
			w.(http.Flusher).Flush()
			select {
			case <-req.Context().Done():
				return
			case <-time.After(hold):
			}
			// Side band 1, in packets of at most side-band-64k's 65 520
			// bytes: the length, the band, the data.
			for rest := pack; len(rest) > 0; {
				n := min(len(rest), 65520-5)
				_, _ = fmt.Fprintf(w, "%04x\x01", n+5)
				_, _ = w.Write(rest[:n])
				rest = rest[n:]
			}
			_, _ = io.WriteString(w, "0000")
		default:
			http.NotFound(w, req)
		}
	}
}

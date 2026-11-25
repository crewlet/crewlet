package hostreach

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// reachCopy is reach.js written into dir as a .cjs, which node loads as
// CommonJS whatever package.json lies above it.
func reachCopy(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reach.cjs")
	if err := os.WriteFile(path, []byte(reachJS), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// drive runs testdata/drive.cjs under node with req, in an environment of
// its own, so reach.js exports its functions rather than run, and decodes its
// answer into out.
func drive(t *testing.T, node, reach string, req, out any) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := filepath.Abs(filepath.Join("testdata", "drive.cjs"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), node, driver, reach)
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(body)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	answer, err := cmd.Output()
	if err != nil {
		t.Fatalf("the driver under %s: %v\n%s%s", node, err, answer, stderr.String())
	}
	if err := json.Unmarshal(answer, out); err != nil {
		t.Fatalf("the driver answered %q: %v", answer, err)
	}
}

// fixture is a testdata file's text.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// parsed is the driver's answer to a parse.
type parsed struct {
	Routes            []string `json:"routes"`
	MaxAddresses      int      `json:"maxAddresses"`
	MaxProxyAddresses int      `json:"maxProxyAddresses"`
	LoopbackTargets   []string `json:"loopbackTargets"`
	Gateways          []string `json:"gateways"`
	Addresses         []struct {
		Route   string `json:"route"`
		Address string `json:"address"`
	} `json:"addresses"`
	Proxies []struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"proxies"`
}

// checked is the driver's answer to a check: the box's word, what check
// threw, every connection it made, and the fake clock when it answered.
type checked struct {
	Word  string `json:"word"`
	Threw string `json:"threw"`
	Dials []struct {
		Host      string   `json:"host"`
		Port      int      `json:"port"`
		Sent      []string `json:"sent"`
		Made      int      `json:"made"`
		Destroyed int      `json:"destroyed"`
	} `json:"dials"`
	Now int `json:"now"`
}

// dialed is every host the check connected to, in order.
func (c checked) dialed() []string {
	var out []string
	for _, d := range c.Dials {
		out = append(out, d.Host)
	}
	return out
}

// boxTokens is the token the cases hand the box for each route: its name,
// repeated, so a dial's first write names the route it was dialed under.
func boxTokens() string {
	var pairs []string
	for _, route := range routes {
		pairs = append(pairs, string(route)+":"+strings.ToUpper(string(route)))
	}
	return strings.Join(pairs, ",")
}

// handOverFor is what the engine hands a box's half in a case, beyond what
// lies past a rootless runtime's gateway, with extra over it.
func handOverFor(beyond string, extra map[string]string) map[string]string {
	env := map[string]string{
		PortEnv: "4711", TokensEnv: boxTokens(), BeyondEnv: beyond,
		RouteEnv:  strconv.FormatInt(RouteBound.Milliseconds(), 10),
		AnswerEnv: strconv.FormatInt(AnswerBound.Milliseconds(), 10),
	}
	maps.Copy(env, extra)
	return env
}

// A box whose files name no gateway, no hosts entry and no socket, in the
// whole user namespace: it dials its own loopback alone.
const (
	noRoutes    = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	identityMap = "         0          0 4294967295\n"
	noSockets   = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
)

// bareBox is the files of a box that names nothing past its loopback, over
// which a case writes its own.
func bareBox(over map[string]any) map[string]any {
	files := map[string]any{"/proc/net/route": noRoutes, "/proc/net/ipv6_route": "", "/etc/hosts": "",
		"/proc/self/uid_map": identityMap, "/proc/net/tcp": noSockets, "/proc/net/udp": noSockets}
	maps.Copy(files, over)
	return files
}

// THE BOX'S HALF FINDS EVERY ROUTE AND PROXY IT HAS: both tables' gateways (an
// IPv4 one in the kernel's byte order, an IPv6 link-local one with its zone,
// reject routes on lo aside); every address its hosts file names, Docker's
// extras among them, a comment aside; what lies past a rootless gateway; each
// once, under the first route naming it, in host_network's order; each proxy
// read as Chromium reads it (scheme and user dropped, port 80 where none is,
// a trailing slash a twin, no_proxy ignored); and past a cap it says too_many
// and dials nothing, at it it dials them all.
//
// Mutations, each red: reverse the byte order; drop the zone; dial a shared
// address under its later route; read only the runtime's two names; honour a
// proxy's scheme; keep the user; skip a proxy under no_proxy; drop silently.
func TestTheBoxsHalfFindsEveryRouteAndProxyItHas(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		reach := reachCopy(t)
		var got parsed
		drive(t, env.Node, reach, map[string]any{"op": "parse", "hosts": fixture(t, "hosts"),
			"route4": fixture(t, "route"), "route6": fixture(t, "ipv6_route"),
			"beyond": []string{"10.0.2.2", "fd00::2", "192.168.1.1"},
			"env": map[string]string{"https_proxy": "socks5://u:p@127.0.0.1:38181/", "HTTP_PROXY": "proxy.example.com",
				"all_proxy": "http://[::1]:3128", "ALL_PROXY": "http://[::1]:3128/", "no_proxy": "*"}}, &got)

		if want := []string{"loopback", "gateway", "host_alias", "rootless_gateway", "proxy"}; !slices.Equal(
			append(slices.Clone(got.Routes), "proxy"), want) || !slices.Equal(want, causeNames(routes)) {
			t.Errorf("reach.js dials under %q and then proxy, the engine names %q: want both %q", got.Routes,
				routes, want)
		}
		if got.MaxAddresses != maxAddresses || got.MaxProxyAddresses != maxProxyAddresses ||
			2*len(got.LoopbackTargets) != proxyAsks {
			t.Errorf("reach.js caps %d addresses and %d proxy addresses, each asked twice for %q, the engine says "+
				"%d and %d, each asked %d times", got.MaxAddresses, got.MaxProxyAddresses, got.LoopbackTargets,
				maxAddresses, maxProxyAddresses, proxyAsks)
		}
		var addresses []string
		for _, a := range got.Addresses {
			addresses = append(addresses, a.Route+" "+a.Address)
		}
		want := []string{
			"loopback 127.0.0.1", "loopback ::1",
			"gateway 172.17.0.1", "gateway 192.168.1.1",
			"gateway fe80:0000:0000:0000:0000:0000:0000:0001%eth0", "gateway 2001:0db8:0000:0000:0000:0000:0000:0001",
			"host_alias fe00::", "host_alias ff00::", "host_alias ff02::1", "host_alias ff02::2",
			"host_alias 172.17.0.2",
			"rootless_gateway 10.0.2.2", "rootless_gateway fd00::2",
		}
		if !slices.Equal(addresses, want) {
			t.Errorf("the box dials\n%s\nwant\n%s", strings.Join(addresses, "\n"), strings.Join(want, "\n"))
		}
		var proxies []string
		for _, p := range got.Proxies {
			proxies = append(proxies, net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
		}
		if want := []string{"proxy.example.com:80", "127.0.0.1:38181", "[::1]:3128"}; !slices.Equal(proxies, want) {
			t.Errorf("the box's proxies are %q, want %q", proxies, want)
		}

		many := func(n int) string {
			var b strings.Builder
			for i := range n {
				fmt.Fprintf(&b, "10.1.%d.%d\tname%d\n", i/250, i%250+1, i)
			}
			return b.String()
		}
		proxyAt := func(n int) []string {
			var out []string
			for i := range n {
				out = append(out, fmt.Sprintf("10.2.0.%d", i+1))
			}
			return out
		}
		open := map[string]any{}
		for _, at := range proxyAt(maxProxyAddresses) {
			open[at] = "proxy-open"
		}
		for name, c := range map[string]struct {
			hosts   string
			proxies int
			word    string
			dials   int
		}{
			"addresses at the cap":   {hosts: many(maxAddresses - 2), word: "checked", dials: maxAddresses},
			"addresses past the cap": {hosts: many(maxAddresses - 1), word: "too_many"},
			"proxy addresses at the cap": {proxies: maxProxyAddresses, word: "checked",
				dials: 2 + proxyAsks*maxProxyAddresses},
			"proxy addresses past it": {proxies: maxProxyAddresses + 1, word: "too_many"},
		} {
			var got checked
			proxy := map[string]string{"no_proxy": "*"}
			if c.proxies > 0 {
				proxy["HTTP_PROXY"] = "proxy.example.com:3128"
			}
			drive(t, env.Node, reach, map[string]any{"op": "check", "env": handOverFor("", proxy),
				"files": bareBox(map[string]any{"/etc/hosts": c.hosts}), "plan": open,
				"lookups": map[string]any{"proxy.example.com": proxyAt(c.proxies)}}, &got)
			if got.Word != c.word || len(got.Dials) != c.dials {
				t.Errorf("%s: the box says %q after %d dials, want %q after %d", name, got.Word, len(got.Dials), c.word,
					c.dials)
			}
		}
	})
}

// causeNames is each cause as its word.
func causeNames(causes []browser.EnsureCause) []string {
	var out []string
	for _, c := range causes {
		out = append(out, string(c))
	}
	return out
}

// A BOX IN A USER NAMESPACE DIALS BEYOND ITS GATEWAY: what the engine hands it
// past a rootless runtime's gateway is dialed, under rootless_gateway, by a box
// whose uid_map is not the whole identity map, or cannot be read, and by no
// box in the whole one; and a box that cannot read a routing table or its
// hosts file, the IPv6 table or the hosts file absent aside, says unreadable
// and dials nothing.
//
// Mutations, each red: invert the identity test; read an unreadable map as
// the identity; read an unreadable table, or hosts file, as empty.
func TestABoxInAUserNamespaceDialsBeyondItsGateway(t *testing.T) {
	t.Parallel()
	const beyond = "10.0.2.2,fd00::2,192.168.1.1"
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		reach := reachCopy(t)
		check := func(over map[string]any, absent ...string) checked {
			t.Helper()
			var got checked
			drive(t, env.Node, reach, map[string]any{"op": "check", "env": handOverFor(beyond, nil),
				"files": bareBox(over), "absent": absent}, &got)
			return got
		}
		if got := check(nil); got.Word != "checked" || !slices.Equal(got.dialed(), []string{"127.0.0.1", "::1"}) {
			t.Errorf("a box in the whole identity map says %q after dialing %q, want checked after its loopback alone",
				got.Word, got.dialed())
		}
		for name, uidMap := range map[string]any{"a rootless runtime's map": "         0       1000          1\n" +
			"         1     100000      65536\n", "an unreadable map": nil} {
			got := check(map[string]any{"/proc/self/uid_map": uidMap})
			want := []string{"127.0.0.1", "::1", "10.0.2.2", "fd00::2", "192.168.1.1"}
			if got.Word != "checked" || !slices.Equal(got.dialed(), want) {
				t.Errorf("%s: the box says %q after dialing %q, want checked after %q", name, got.Word, got.dialed(), want)
			}
			for _, d := range got.Dials[2:] {
				if len(d.Sent) > 0 || d.Made >= 0 {
					t.Errorf("%s: %s, refused, was sent %q", name, d.Host, d.Sent)
				}
			}
		}
		for name, c := range map[string]struct {
			over   map[string]any
			absent string
			word   string
		}{
			"an unreadable IPv4 table": {over: map[string]any{"/proc/net/route": nil}, word: "unreadable"},
			"an unreadable IPv6 table": {over: map[string]any{"/proc/net/ipv6_route": nil}, word: "unreadable"},
			"an absent IPv4 table":     {absent: "/proc/net/route", word: "unreadable"},
			"an absent IPv6 table":     {absent: "/proc/net/ipv6_route", word: "checked"},
			"an unreadable hosts file": {over: map[string]any{"/etc/hosts": nil}, word: "unreadable"},
			"an absent hosts file":     {absent: "/etc/hosts", word: "checked"},
		} {
			got := check(c.over, c.absent)
			if got.Word != c.word || (c.word == "unreadable" && len(got.Dials) > 0) {
				t.Errorf("%s: the box says %q after %d dials, want %q", name, got.Word, len(got.Dials), c.word)
			}
		}
	})
}

// A MADE CONNECTION IS HELD UNTIL IT IS ANSWERED OR CLOSED: a dial that is
// never made leads nowhere at RouteBound; one made survives RouteBound, and
// is settled by the engine's answer, or its close, or held at AnswerBound,
// which the box says; a proxy's name still unresolved at AnswerBound is held
// and never dialed; and a tunnel a proxy refuses is not sent the token, one
// it opens is, its status line's reason phrase there or not.
//
// Mutations, each red: settle a made dial at RouteBound; read a lookup still
// pending as leading nowhere; send the token through a refused tunnel; take
// a tunnel's answer only with a reason phrase.
func TestAMadeConnectionIsHeldUntilItIsAnsweredOrClosed(t *testing.T) {
	t.Parallel()
	route, answer := int(RouteBound.Milliseconds()), int(AnswerBound.Milliseconds())
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		reach := reachCopy(t)
		check := func(extra map[string]string, plan, lookups map[string]any) checked {
			t.Helper()
			var got checked
			drive(t, env.Node, reach, map[string]any{"op": "check", "env": handOverFor("", extra),
				"files": bareBox(nil), "plan": plan, "lookups": lookups}, &got)
			if got.Threw != "" {
				t.Fatalf("the check threw %q", got.Threw)
			}
			return got
		}
		for name, c := range map[string]struct {
			behaviour       string
			word            string
			made, destroyed int
		}{
			"never made":           {behaviour: "silent", word: "checked", made: -1, destroyed: route},
			"answered":             {behaviour: "engine", word: "checked", made: 0, destroyed: 0},
			"closed":               {behaviour: "made-close", word: "checked", made: 0, destroyed: 0},
			"made, never answered": {behaviour: "made-silent", word: "held", made: 0, destroyed: answer},
		} {
			got := check(nil, map[string]any{"127.0.0.1": c.behaviour}, nil)
			d := got.Dials[0]
			if got.Word != c.word || d.Host != "127.0.0.1" || d.Made != c.made || d.Destroyed != c.destroyed {
				t.Errorf("%s: the box says %q, its dial made at %d and settled at %d; want %q, %d and %d", name,
					got.Word, d.Made, d.Destroyed, c.word, c.made, c.destroyed)
			}
		}
		pending := check(map[string]string{"HTTP_PROXY": "proxy.example.com:3128"}, nil,
			map[string]any{"proxy.example.com": "pending"})
		if pending.Word != "held" || pending.Now != answer || slices.Contains(pending.dialed(), "proxy.example.com") {
			t.Errorf("a proxy name still unresolved: the box says %q at %d after dialing %q, want held at %d", pending.Word,
				pending.Now, pending.dialed(), answer)
		}
		for behaviour, sendsToken := range map[string]bool{"proxy-refuse": false, "proxy-open": true,
			"proxy-terse": true} {
			// 10.9.9.9 is no address of this host's (handOverFor hands none), so
			// a proxy that refuses is asked nowhere else.
			got := check(map[string]string{"https_proxy": "http://10.9.9.9:3128"},
				map[string]any{"10.9.9.9": behaviour}, nil)
			tunnels := 0
			for _, d := range got.Dials {
				if d.Host != "10.9.9.9" || !strings.HasPrefix(d.Sent[0], "CONNECT ") {
					continue
				}
				tunnels++
				if sent := slices.Contains(d.Sent, "PROXY\n"); sent != sendsToken {
					t.Errorf("a tunnel %s answered was sent %q", behaviour, d.Sent)
				}
			}
			if got.Word != "checked" || tunnels != proxyAsks/2 {
				t.Errorf("through %s the box says %q after %d tunnels, want checked after %d", behaviour, got.Word,
					tunnels, proxyAsks/2)
			}
		}
		var none checked
		drive(t, env.Node, reach, map[string]any{"op": "check", "env": map[string]string{PortEnv: "4711"},
			"files": bareBox(nil)}, &none)
		if none.Threw == "" || none.Word != "" {
			t.Errorf("a box handed no tokens or bounds says %q, threw %q: want it to throw, saying no word",
				none.Word, none.Threw)
		}
	})
}

// socketRow is a row of /proc/net's tcp and udp tables for a socket at local
// (an address as the kernel prints it, and a port) in state st, inode inode.
func socketRow(local, st, inode string) string {
	return "   0: " + local + " 00000000:0000 " + st + " 00000000:00000000 00:00000000 00000000  1000        0 " +
		inode + " 1 0000000000000000 100 0 0 10 0\n"
}

// A SOCKET BOUND ON THE BOX'S OWN LOOPBACK FROM OUTSIDE IT IS FOUND: a TCP
// listener or an unconnected UDP socket on 127.0.0.1, ::1 or
// ::ffff:127.0.0.1, listed in both reads of the box's tables and held by no
// thread of a process it can see, as pasta's -T and -U bind theirs, makes the
// box say forwarded; one a process of the box holds, by a thread beside its
// exited main thread too, one on another address (the runtime's resolver at
// 127.0.0.11, every address), a connected one, one gone by the second read,
// or none, leaves it checked; an unreadable table says unreadable.
//
// Mutations, each red: read no UDP table; skip the IPv6 tables; count a held
// socket as forwarded; count one gone by the second read; read an unreadable
// table as empty; read a process's own descriptors, its main thread's.
func TestASocketBoundOnTheBoxsLoopbackFromOutsideItIsFound(t *testing.T) {
	t.Parallel()
	const v6Loopback, v4Mapped = "00000000000000000000000001000000", "0000000000000000FFFF00000100007F"
	// Process 23's main thread has exited: its own descriptors, and its main
	// thread's, list nothing, while thread 24 holds the table on.
	held := map[string]any{
		"dirs": map[string]any{
			"/proc":               []string{"1", "17", "23", "self", "net"},
			"/proc/1/fd":          []string{"0", "1", "2"},
			"/proc/1/task":        []string{"1"},
			"/proc/1/task/1/fd":   []string{"0", "1", "2"},
			"/proc/17/fd":         []string{"0", "3", "4"},
			"/proc/17/task":       []string{"17"},
			"/proc/17/task/17/fd": []string{"0", "3", "4"},
			"/proc/23/task":       []string{"23", "24"},
			"/proc/23/task/24/fd": []string{"5"},
		},
		"links": map[string]any{
			"/proc/1/fd/0":          "/dev/null",
			"/proc/1/task/1/fd/0":   "/dev/null",
			"/proc/17/fd/3":         "socket:[5001]",
			"/proc/17/task/17/fd/3": "socket:[5001]",
			"/proc/17/fd/4":         "pipe:[5002]",
			"/proc/17/task/17/fd/4": "pipe:[5002]",
			"/proc/23/task/24/fd/5": "socket:[5003]",
		},
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		reach := reachCopy(t)
		for name, c := range map[string]struct {
			files map[string]any
			word  string
		}{
			"no socket": {word: "checked"},
			"a TCP listener on 127.0.0.1, held by none": {word: "forwarded",
				files: map[string]any{"/proc/net/tcp": noSockets + socketRow("0100007F:1538", "0A", "6001")}},
			"a TCP listener on ::1, held by none": {word: "forwarded",
				files: map[string]any{"/proc/net/tcp6": noSockets + socketRow(v6Loopback+":1538", "0A", "6001")}},
			"a TCP listener on ::ffff:127.0.0.1, held by none": {word: "forwarded",
				files: map[string]any{"/proc/net/tcp6": noSockets + socketRow(v4Mapped+":1538", "0A", "6001")}},
			"a UDP socket on 127.0.0.1, held by none": {word: "forwarded",
				files: map[string]any{"/proc/net/udp": noSockets + socketRow("0100007F:0035", "07", "6001")}},
			"a UDP socket on ::1, held by none": {word: "forwarded",
				files: map[string]any{"/proc/net/udp6": noSockets + socketRow(v6Loopback+":0035", "07", "6001")}},
			"a TCP listener on 127.0.0.1 a process of the box holds": {word: "checked",
				files: map[string]any{"/proc/net/tcp": noSockets + socketRow("0100007F:1538", "0A", "5001")}},
			"a TCP listener on 127.0.0.1 a thread holds beside its exited main thread": {word: "checked",
				files: map[string]any{"/proc/net/tcp": noSockets + socketRow("0100007F:1538", "0A", "5003")}},
			"the runtime's resolver on 127.0.0.11, held by none": {word: "checked",
				files: map[string]any{"/proc/net/udp": noSockets + socketRow("0B00007F:0035", "07", "6001")}},
			"a listener on every address, held by none": {word: "checked",
				files: map[string]any{"/proc/net/tcp": noSockets + socketRow("00000000:1538", "0A", "6001")}},
			"a connection on 127.0.0.1, held by none": {word: "checked",
				files: map[string]any{"/proc/net/tcp": noSockets + socketRow("0100007F:1538", "01", "6001")}},
			"a listener gone by the second read": {word: "checked", files: map[string]any{"/proc/net/tcp": []string{
				noSockets + socketRow("0100007F:1538", "0A", "6001"), noSockets}}},
			"an unreadable TCP table":  {word: "unreadable", files: map[string]any{"/proc/net/tcp": nil}},
			"an unreadable UDP6 table": {word: "unreadable", files: map[string]any{"/proc/net/udp6": nil}},
		} {
			var got checked
			req := map[string]any{"op": "check", "env": handOverFor("", nil), "files": bareBox(c.files)}
			maps.Copy(req, held)
			drive(t, env.Node, reach, req, &got)
			if got.Word != c.word {
				t.Errorf("%s: the box says %q, want %q", name, got.Word, c.word)
			}
		}
	})
}

// A PROXY OF THIS HOST THAT REFUSES EVERY LOOPBACK ASK IS ASKED ELSEWHERE: a
// proxy is asked for 127.0.0.1, localhost and [::1] at the check's port, with
// a GET and a tunnel; one at an address of this host's that refused all six
// is asked for TEST-NET-1 at that port, and refusing it alike makes the box
// say proxy_refused, where one that dials it (no answer within RouteBound)
// leaves it checked; one elsewhere is never asked elsewhere. A proxy whose
// name resolves to nothing, or none of whose addresses takes a connection, is
// proxy_down; one whose second address takes one is not.
//
// Mutations, each red: drop the [::1] asks; ask a proxy elsewhere; read a
// proxy that refused alike, or one that is down, as checked; judge a proxy
// down by one address; read a status line with no reason phrase as none.
func TestAProxyOfThisHostThatRefusesEveryLoopbackAskIsAskedElsewhere(t *testing.T) {
	t.Parallel()
	route := int(RouteBound.Milliseconds())
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		reach := reachCopy(t)
		check := func(extra map[string]string, plan, lookups map[string]any) checked {
			t.Helper()
			var got checked
			drive(t, env.Node, reach, map[string]any{"op": "check", "env": handOverFor("", extra),
				"files": bareBox(nil), "plan": plan, "lookups": lookups}, &got)
			if got.Threw != "" {
				t.Fatalf("the check threw %q", got.Threw)
			}
			return got
		}
		// asked is each request the box made of the proxy at host, by its
		// first line.
		asked := func(got checked, host string) []string {
			var out []string
			for _, d := range got.Dials {
				if d.Host == host && len(d.Sent) > 0 {
					first, _, _ := strings.Cut(d.Sent[0], "\r\n")
					out = append(out, first)
				}
			}
			slices.Sort(out)
			return out
		}
		loopbackAsks := []string{"CONNECT 127.0.0.1:4711 HTTP/1.1", "CONNECT [::1]:4711 HTTP/1.1",
			"CONNECT localhost:4711 HTTP/1.1", "GET http://127.0.0.1:4711/PROXY HTTP/1.1",
			"GET http://[::1]:4711/PROXY HTTP/1.1", "GET http://localhost:4711/PROXY HTTP/1.1"}
		elsewhereAsks := []string{"CONNECT 192.0.2.1:4711 HTTP/1.1", "GET http://192.0.2.1:4711/PROXY HTTP/1.1"}
		ours := map[string]string{"HTTPS_PROXY": "http://10.9.9.9:3128", HostEnv: "172.17.0.1,10.9.9.9"}
		for name, c := range map[string]struct {
			extra     map[string]string
			host      string // the proxy as the box dials it: 10.9.9.9 where empty
			behaviour string
			word      string
			asks      []string
		}{
			"a proxy of this host that refuses alike": {extra: ours, behaviour: "proxy-refuse", word: "proxy_refused",
				asks: slices.Concat(loopbackAsks, elsewhereAsks)},
			"a proxy of this host that refuses alike, tersely": {extra: ours, behaviour: "proxy-refuse-terse",
				word: "proxy_refused", asks: slices.Concat(loopbackAsks, elsewhereAsks)},
			"a proxy of this host that refuses loopback destinations": {extra: ours, behaviour: "proxy-dest",
				word: "checked", asks: slices.Concat(loopbackAsks, elsewhereAsks)},
			"a proxy elsewhere that refuses alike": {extra: map[string]string{"HTTPS_PROXY": "http://10.9.9.9:3128"},
				behaviour: "proxy-refuse", word: "checked", asks: loopbackAsks},
			"a proxy of this host, spelled otherwise": {extra: map[string]string{
				"HTTPS_PROXY": "http://[fd00:0:0:0:0:0:0:9]:3128", HostEnv: "fd00::9"}, host: "fd00:0:0:0:0:0:0:9",
				behaviour: "proxy-refuse", word: "proxy_refused", asks: slices.Concat(loopbackAsks, elsewhereAsks)},
		} {
			host := cmp.Or(c.host, "10.9.9.9")
			got := check(c.extra, map[string]any{host: c.behaviour}, nil)
			want := slices.Sorted(slices.Values(c.asks))
			if got.Word != c.word || !slices.Equal(asked(got, host), want) {
				t.Errorf("%s: the box says %q after asking %q, want %q after %q", name, got.Word, asked(got, host),
					c.word, want)
			}
			if c.behaviour == "proxy-dest" && got.Now != route {
				t.Errorf("%s: the box answered at %d ms, want the elsewhere asks settled at RouteBound, %d", name,
					got.Now, route)
			}
		}
		for name, c := range map[string]struct {
			extra   map[string]string
			lookups map[string]any
		}{
			"a name that resolves to nothing": {extra: map[string]string{"HTTP_PROXY": "proxy.invalid:3128"},
				lookups: map[string]any{"proxy.invalid": "error"}},
			"an address that takes no connection": {extra: map[string]string{"HTTP_PROXY": "http://10.9.9.8:3199"}},
			"a name none of whose addresses takes one": {extra: map[string]string{"HTTP_PROXY": "proxy.example.com:3128"},
				lookups: map[string]any{"proxy.example.com": []string{"2001:db8::8", "10.9.9.8"}}},
		} {
			if got := check(c.extra, nil, c.lookups); got.Word != "proxy_down" {
				t.Errorf("%s: the box says %q, want proxy_down", name, got.Word)
			}
		}
		// Chromium goes on to a proxy's next address, as to an IPv4 one past an
		// IPv6 one the box cannot reach.
		if got := check(map[string]string{"HTTP_PROXY": "proxy.example.com:3128"},
			map[string]any{"10.9.9.7": "proxy-open"},
			map[string]any{"proxy.example.com": []string{"2001:db8::8", "10.9.9.7"}}); got.Word != "checked" {
			t.Errorf("a proxy whose second address takes a connection: the box says %q, want checked", got.Word)
		}
	})
}

// THE ENGINE AND THE BOX READ A ROUTING TABLE ALIKE: the engine's default
// gateways, which it hands every box, are the box's own reading of the same
// tables, a link-local next hop too, which the engine hands with no interface.
//
// Mutations, each red: reverse either side's byte order; drop a link-local
// next hop.
func TestTheEngineAndTheBoxReadARoutingTableAlike(t *testing.T) {
	t.Parallel()
	engine, err := engineGateways(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		var got parsed
		drive(t, env.Node, reachCopy(t), map[string]any{"op": "parse", "route4": fixture(t, "route"),
			"route6": fixture(t, "ipv6_route")}, &got)
		var box []string
		for _, g := range got.Gateways {
			address, _, _ := strings.Cut(g, "%")
			if ip := net.ParseIP(address); ip != nil {
				box = append(box, ip.String())
			} else {
				t.Errorf("the box reads a gateway %q, which is no address", g)
			}
		}
		if !slices.Equal(engine, box) || len(engine) != 4 {
			t.Errorf("the engine reads %q, the box %q from the same tables (an interface aside), want four alike",
				engine, box)
		}
	})
}

// forwardProxy is a permissive HTTP forward proxy on 127.0.0.1, as one that
// does not refuse loopback destinations is: a GET in absolute form is fetched
// from its host, a CONNECT tunnelled. Its address is answered; the case's end
// stops it.
func forwardProxy(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = l.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { forward(conn) })
		}
	})
	return l.Addr().String()
}

// forward carries one request a proxy's client made to where it names.
func forward(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	req, err := http.ReadRequest(r)
	if err != nil {
		return
	}
	host := req.Host
	if req.Method != http.MethodConnect {
		host = req.URL.Host
	}
	up, err := net.Dial("tcp", host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer func() { _ = up.Close() }()
	if req.Method == http.MethodConnect {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(up, r)
			close(done)
		}()
		_, _ = io.Copy(conn, up)
		_ = conn.Close()
		<-done
		return
	}
	_, _ = fmt.Fprintf(up, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", req.URL.RequestURI(), host)
	_, _ = io.Copy(conn, up)
}

// THE BOX'S HALF DIALS WHAT IT FINDS AND SAYS ITS KERNEL, run for real under
// sh on this host, which is the box: on Linux its loopback and a permissive
// proxy carry their own routes' tokens to the engine, and its line names this
// kernel's boot id; off Linux, with no /proc, it says it read neither its
// boot id nor its routing table; and with no node on its PATH it says
// no_node. Run as `sh -c`, not the login shell Command starts, which on this
// host would put the machine's own PATH back over the case's.
//
// Mutations, each red: dial under the wrong route's token; skip the proxies;
// drop the boot id's read.
func TestTheBoxsHalfDialsWhatItFindsAndSaysItsKernel(t *testing.T) {
	t.Parallel()
	kernel := testKernel
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(bootIDFile)
		if err != nil {
			t.Fatal(err)
		}
		kernel = Kernel(strings.TrimSpace(string(raw)))
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		proxy := forwardProxy(t)
		half := func(path string) outcome {
			t.Helper()
			o, err := realChecker(nil).run(t.Context(), Request{
				Kernel: func() (Kernel, error) { return kernel, nil },
				Run: func(ctx context.Context, _ string, handed map[string]string, _ time.Duration) (string, int, error) {
					cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script())
					cmd.Env = []string{"PATH=" + path, "HTTP_PROXY=http://" + proxy}
					for k, v := range handed {
						cmd.Env = append(cmd.Env, k+"="+v)
					}
					out, err := cmd.Output()
					if exit, ok := errors.AsType[*exec.ExitError](err); ok {
						return string(out), exit.ExitCode(), nil
					}
					return string(out), 0, err
				},
			})
			if err != nil {
				t.Fatalf("the check: %v", err)
			}
			return o
		}
		o := half(filepath.Dir(env.Node) + string(os.PathListSeparator) + "/usr/bin:/bin")
		line, state := parseLine(o.stdout)
		switch {
		case state != lineRead:
			t.Errorf("the box's half printed %q (exit %d), which is no line of its grammar", o.stdout, o.exit)
		case runtime.GOOS != "linux" && (line.kernel != "unreadable" || line.word != wordUnreadable):
			t.Errorf("off Linux the box's half says %+v, want its boot id and routing table unreadable", line)
		case runtime.GOOS != "linux":
		case line.kernel != string(kernel):
			t.Errorf("the box's half says kernel %q, want this host's boot id %q", line.kernel, kernel)
		case !o.arrived[browser.CauseLoopback] || !o.arrived[browser.CauseProxy] || len(o.arrived) != 2:
			t.Errorf("the box's half carried %v to the engine, want loopback and proxy", o.arrived)
		case o.disturbance != "":
			t.Errorf("the port was disturbed by %s", o.disturbance)
		}
		bare := half(t.TempDir())
		if line, state := parseLine(bare.stdout); state != lineRead || line.word != wordNoNode ||
			(runtime.GOOS == "linux" && line.kernel != string(kernel)) {
			t.Errorf("with no node on its PATH the box's half printed %q, want no_node on this kernel", bare.stdout)
		}
	})
}

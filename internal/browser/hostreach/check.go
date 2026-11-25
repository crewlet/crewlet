package hostreach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Request is what [Check] is handed of one launch's box.
type Request struct {
	// Kernel is the kernel of the host the box's runtime runs on, the
	// engine's own for a container box ([EngineKernel]).
	Kernel func() (Kernel, error)
	// Run runs command in the box with env over the run's environment,
	// bounded by timeout and by ctx, and answers what it printed, its exit
	// status, and an error where it could not be run.
	Run func(ctx context.Context, command string, env map[string]string,
		timeout time.Duration) (stdout string, exit int, err error)
}

// Verdict is what [Check] came to.
type Verdict struct {
	// Off is true only where the box may run a browser.
	Off bool
	// Reason and Cause say why it may not: host_network with the cause, or
	// node_missing not_on_path for a box with no node to run the check.
	Reason browser.EnsureReason
	Cause  browser.EnsureCause
	// Detail is what to change, for the operator's log: never anything the
	// box printed or sent.
	Detail string
}

// checker is [Check] with its seams: how it listens, reads the engine's own
// routing tables, lists its own addresses and connects to its own listeners.
type checker struct {
	listen func(network, address string) (net.Listener, error)
	read   func(name string) ([]byte, error)
	addrs  func() ([]net.Addr, error)
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
}

// Check asks a box whether it may run a seat's browser: that it runs on the
// engine's own kernel and that none of its routes reaches a listener on the
// engine's loopback (the package doc). Its error is only ctx's own; every
// other answer is a verdict, and only one that found the box off its host's
// loopback leaves it its browser.
func Check(ctx context.Context, r Request) (Verdict, error) {
	return checker{listen: net.Listen, read: os.ReadFile, addrs: net.InterfaceAddrs,
		dial: (&net.Dialer{}).DialContext}.check(ctx, r)
}

func (c checker) check(ctx context.Context, r Request) (Verdict, error) {
	o, err := c.run(ctx, r)
	if err != nil {
		return Verdict{}, err
	}
	return decide(o), nil
}

// run asks the box and gathers what the check learnt; its error is ctx's own.
func (c checker) run(ctx context.Context, r Request) (outcome, error) {
	var o outcome
	switch {
	case r.Kernel == nil:
		o.unasked = "the box's handle cannot say which kernel its runtime runs on"
		return o, nil
	case r.Run == nil:
		o.unasked = "the check was handed no way to run its command in the box"
		return o, nil
	}
	if o.engine, o.kernelErr = r.Kernel(); o.kernelErr != nil {
		return o, nil
	}
	var beyond, host []string
	if beyond, o.tablesErr = engineGateways(c.read); o.tablesErr != nil {
		return o, nil
	}
	if host, o.addrsErr = engineAddresses(c.addrs); o.addrsErr != nil {
		return o, nil
	}
	p, err := c.open(mint())
	if err != nil {
		o.listenErr = err
		return o, nil
	}
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	p.serve(cctx)
	o.stdout, o.exit, o.runErr = r.Run(cctx, Command(), handOver(p, beyond, host), CheckTimeout)
	switch {
	case ctx.Err() != nil:
	case cctx.Err() != nil:
		o.timedOut = true
	default:
		if err := p.drain(cctx, c.dial); errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			o.timedOut = true
		} else if err != nil && ctx.Err() == nil {
			o.undrained = err
		}
	}
	o.arrived, o.disturbance = p.shut()
	return o, ctx.Err()
}

// mint is a check's tokens: 128 random bits for each route, and the engine's
// own.
func mint() tokens {
	t := tokens{route: map[browser.EnsureCause]string{}, drain: randomToken()}
	for _, route := range routes {
		t.route[route] = randomToken()
	}
	return t
}

// randomToken is 128 random bits in hex. crypto/rand's Read never fails: it
// ends the program rather than answer an error.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// handOver is what the engine hands the box's half: the port, each route's
// token, the bounds, the half's own deadline, what lies past a rootless
// runtime's gateway (slirp4netns's and RootlessKit's gateway and this
// engine's own default gateways), and this host's own addresses, which tell
// a proxy that may share its loopback.
func handOver(p *port, gateways, host []string) map[string]string {
	pairs := make([]string, 0, len(routes))
	for _, route := range routes {
		pairs = append(pairs, string(route)+":"+p.tokens.route[route])
	}
	return map[string]string{
		PortEnv:     strconv.Itoa(p.number),
		TokensEnv:   strings.Join(pairs, ","),
		RouteEnv:    strconv.FormatInt(RouteBound.Milliseconds(), 10),
		AnswerEnv:   strconv.FormatInt(AnswerBound.Milliseconds(), 10),
		DeadlineEnv: strconv.FormatInt(CheckTimeout.Milliseconds(), 10),
		BeyondEnv:   strings.Join(slices.Concat(slirpGateways, gateways), ","),
		HostEnv:     strings.Join(host, ","),
	}
}

// outcome is everything the check learnt, for [decide] to judge.
type outcome struct {
	// unasked is why the box could not be asked at all: a request missing a
	// function.
	unasked string
	// kernelErr is the engine's own kernel unread, or one no box shares
	// ([ErrNoSharedKernel]); tablesErr its routing tables unread; addrsErr
	// its addresses unlisted; listenErr its loopback refusing the check's
	// listener. Each is decided before anything runs in the box.
	kernelErr, tablesErr, addrsErr, listenErr error
	engine                                    Kernel
	// arrived is each route whose token reached the engine; disturbance is
	// what else first reached its port, "" for nothing; undrained is the
	// engine's own connection to its listener failing.
	arrived     map[browser.EnsureCause]bool
	disturbance string
	undrained   error
	// timedOut says the check did not finish within CheckTimeout.
	timedOut bool
	stdout   string
	exit     int
	runErr   error
}

// The withheld verdicts' details.
const (
	remedyLoopback = "the box reached this engine's loopback at its own: it shares this host's network " +
		"namespace. Take the container off it (providers.sandbox.local.network or run_args, or the runtime's " +
		"own setting, such as containers.conf's netns); the seat's next fresh box has a browser"
	remedyGateway = "the box reached this engine's loopback at its default gateway: remove what leads the " +
		"gateway there (slirp4netns's allow_host_loopback, pasta's --map-gw or --map-host-loopback, in " +
		"local.network, run_args or containers.conf, or a relay on the gateway); the seat's next fresh box has " +
		"a browser"
	remedyHostAlias = "the box reached this engine's loopback at an address its hosts file names: remove what " +
		"leads that address there (a relay on it, pasta's --map-host-loopback naming it, or an --add-host in " +
		"local.run_args naming a forwarded address)"
	remedyRootlessGateway = "the box, in a rootless runtime's user namespace, reached this engine's loopback " +
		"beyond its own gateway: keep that runtime's network off the host's loopback (Docker's " +
		"DOCKERD_ROOTLESS_ROOTLESSKIT_DISABLE_HOST_LOOPBACK at its default, true, then restart the daemon; for " +
		"Podman, no allow_host_loopback, --map-gw or --map-host-loopback in containers.conf)"
	remedyProxy = "a proxy the box's browser goes through (HTTP_PROXY, HTTPS_PROXY or ALL_PROXY) carried the " +
		"check to this engine's loopback: have it refuse the boxes' loopback destinations, by their resolved " +
		"address and at every port (squid's http_access deny to_localhost, ahead of every rule that allows " +
		"them), or name one that does not run on this host"
	remedyAnotherKernel = "the box runs on another kernel than this engine's: its runtime runs boxes in a " +
		"virtual machine, gives each a kernel of its own (gVisor, Kata Containers), or runs on another machine " +
		"(DOCKER_HOST, CONTAINER_HOST or a docker context naming a remote daemon); run the seat's boxes with " +
		"Docker Engine or Podman on this host's kernel"
	remedyInitNet = "the box is in the network namespace of the machine its runtime runs on: take the " +
		"container off the host's network (providers.sandbox.local.network or run_args, or the runtime's own " +
		"setting, such as containers.conf's netns); the seat's next fresh box has a browser"
	remedyUnreadableKernel = "the box could not read its kernel's boot id, so it does not show it runs on this " +
		"engine's kernel: let the box read " + bootIDFile
	remedyDisturbed = "something on this host other than the box connected to the check's port on this " +
		"engine's loopback (%s), so the check could not tell the box's routes from it; the next launch checks " +
		"again"
	remedyHeld = "a connection the box made for the check was held open without an answer for %s: something " +
		"in the box listens on the check's port, or a proxy did not answer; the next launch checks again on " +
		"another port"
	remedyTooMany = "the box names more than %d addresses in its hosts file and routing tables, or more than %d " +
		"for its proxies, more than the check dials"
	remedyUnreadable = "the box could not read its routing tables, its hosts file or its socket tables, so it " +
		"could not name every way out it has: let the box read " + routeFile + ", /etc/hosts and /proc/net/tcp " +
		"and udp"
	remedyForwarded = "a socket bound on the box's own loopback is held by no process the check can see in the " +
		"box: something outside the box binds it there, such as pasta's -T or -U (local.network, run_args or " +
		"containers.conf's pasta_options) or a container sharing the box's network. Remove it, and run every " +
		"process in the box as its user; the seat's next fresh box has a browser"
	remedyProxyDown = "a proxy the box's browser goes through (HTTP_PROXY, HTTPS_PROXY or ALL_PROXY) names no " +
		"address the box could connect to, so the check could not ask it, and the browser would go through it " +
		"unasked once it answers: correct its address or start it; the next launch checks again"
	remedyProxyRefused = "a proxy of this host the box's browser goes through refused every request the check " +
		"made of it, to loopback and elsewhere alike, so the check could not tell whether it refuses loopback " +
		"destinations or only the check's port: have it refuse the boxes' loopback destinations, by their " +
		"resolved address and at every port, and carry their requests to ports 1024 to 65535 (squid's default " +
		"Safe_ports), or name one that does not run on this host"
	remedyNoNode = "the box's login shell finds no node, which the launch's check of the box's network runs " +
		"under: install Node.js in the image, on the login PATH"
)

// arrivedSaid is what to change for each route a token arrived by.
var arrivedSaid = map[browser.EnsureCause]string{
	browser.CauseLoopback:        remedyLoopback,
	browser.CauseGateway:         remedyGateway,
	browser.CauseHostAlias:       remedyHostAlias,
	browser.CauseRootlessGateway: remedyRootlessGateway,
	browser.CauseProxy:           remedyProxy,
}

// decide judges an outcome, in order: what the engine decided before anything
// ran; the box on another kernel, then in its machine's initial network
// namespace; a route that arrived, the first in [routes]' order; the port
// disturbed; the check unfinished, unrun or unreadable; then the box's word,
// which only checked leaves the box its browser.
func decide(o outcome) Verdict {
	switch {
	case o.unasked != "":
		return withheld(browser.CauseUnchecked, o.unasked)
	case errors.Is(o.kernelErr, ErrNoSharedKernel):
		return withheld(browser.CauseAnotherKernel, o.kernelErr.Error())
	case o.kernelErr != nil:
		return withheld(browser.CauseUnchecked, "this engine could not read its own kernel's boot id: "+
			o.kernelErr.Error())
	case o.tablesErr != nil:
		return withheld(browser.CauseUnchecked, "this engine could not read its own routing table, whose "+
			"default gateways it hands the box: "+o.tablesErr.Error())
	case o.addrsErr != nil:
		return withheld(browser.CauseUnchecked, "this engine could not list its own addresses, which it hands "+
			"the box: "+o.addrsErr.Error())
	case o.listenErr != nil:
		return withheld(browser.CauseUnchecked, "this engine could not listen on its loopback for the check: "+
			o.listenErr.Error())
	}
	line, state := parseLine(o.stdout)
	if state == lineRead && line.kernel != string(o.engine) {
		if line.kernel == string(wordUnreadable) {
			return withheld(browser.CauseAnotherKernel, remedyUnreadableKernel)
		}
		return withheld(browser.CauseAnotherKernel, remedyAnotherKernel)
	}
	if state == lineRead && line.net == initNetNS {
		return withheld(browser.CauseLoopback, remedyInitNet)
	}
	for _, route := range routes {
		if o.arrived[route] {
			return withheld(route, arrivedSaid[route])
		}
	}
	switch {
	case o.disturbance != "":
		return withheld(browser.CauseUnchecked, fmt.Sprintf(remedyDisturbed, o.disturbance))
	case o.undrained != nil:
		return withheld(browser.CauseUnchecked, "this engine could not connect to its own listener to finish "+
			"the check: "+o.undrained.Error())
	case o.timedOut:
		return withheld(browser.CauseUnchecked, fmt.Sprintf("the check of the box's network answered nothing "+
			"within %s: give the box more CPU and memory", statelog.Spoken(CheckTimeout)))
	case o.runErr != nil && o.stdout == "":
		return withheld(browser.CauseUnchecked, "the check of the box's network could not run: "+o.runErr.Error())
	case state == lineNone && o.exit != 0:
		return withheld(browser.CauseUnchecked, fmt.Sprintf("the check of the box's network exited %d and "+
			"printed no line", o.exit))
	case state == lineNone:
		return withheld(browser.CauseUnchecked, "the check of the box's network printed no line")
	case state == lineOffGrammar:
		return withheld(browser.CauseUnchecked, "the check of the box's network printed a line this engine "+
			"does not read")
	}
	switch line.word {
	case wordChecked:
		return Verdict{Off: true}
	case wordNoNode:
		return Verdict{Reason: browser.ReasonNodeMissing, Cause: browser.CauseNotOnPath, Detail: remedyNoNode}
	case wordHeld:
		return withheld(browser.CauseUnchecked, fmt.Sprintf(remedyHeld, statelog.Spoken(AnswerBound)))
	case wordTooMany:
		return withheld(browser.CauseUnchecked, fmt.Sprintf(remedyTooMany, maxAddresses, maxProxyAddresses))
	case wordForwarded:
		return withheld(browser.CauseUnchecked, remedyForwarded)
	case wordProxyDown:
		return withheld(browser.CauseUnchecked, remedyProxyDown)
	case wordProxyRefused:
		return withheld(browser.CauseUnchecked, remedyProxyRefused)
	}
	// unreadable, the one word of the grammar's left.
	return withheld(browser.CauseUnchecked, remedyUnreadable)
}

// withheld is the box's browser withheld as host_network, for cause.
func withheld(cause browser.EnsureCause, detail string) Verdict {
	return Verdict{Reason: browser.ReasonHostNetwork, Cause: cause, Detail: detail}
}

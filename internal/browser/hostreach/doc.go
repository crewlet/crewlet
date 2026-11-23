// Package hostreach is the launch's check that a browser seat's box is off
// its host's network: that the box runs on the engine's own
// kernel, and that no route it has carries a connection to the engine's
// loopback. internal/sandbox runs it in every container box at every launch,
// before the box's helper is written, so it judges the box as it is: a kept
// box made on its host's network, and a runtime whose own configuration or
// virtual machine leads a box to the host's loopback by no setting config
// reads.
//
// # The kernel
//
// A box shows its kernel by its boot id (/proc/sys/kernel/random/boot_id),
// which the kernel draws at boot and no namespace changes: a box on the
// engine's kernel reads the engine's own, and a box on any other cannot know
// it. The rule names the one kernel it admits ([EngineKernel]), so every
// runtime that runs its boxes in a virtual machine (Docker Desktop, Podman's
// machine, OrbStack, Colima, Rancher Desktop, every runtime on macOS), every
// one that gives a box a kernel of its own (gVisor, Kata Containers) and
// every one on another machine is withheld without being named, as is a box
// that cannot read its boot id. An engine on another system than Linux has no
// kernel a box can share; nor has one under WSL, whose kernel every WSL
// distro shares (systemd-detect-virt(1)), Docker Desktop's and Podman's
// machine's among them, which lead a box's name for the host to Windows' own
// loopback. The engine tells WSL by its kernel's release naming Microsoft or
// WSL in any case, as systemd's detect_container does but for the case (a WSL
// 2 kernel of 4.19 spells it lower), and by WSL's interop entries and its
// /run/WSL, which a kernel named neither still leaves. Both are withheld
// before anything runs in the box ([ErrNoSharedKernel]).
//
// The box says the inode of its network namespace too. Kernels with fixed
// inodes for their initial namespaces give the initial network namespace
// 0xEFFFFFF9, and older ones number every namespace from 0xF0000000 up, so a
// box in that namespace shares its machine's network, whatever namespace the
// engine runs in: withheld as loopback.
//
// # The routes
//
// For the check alone the engine listens on its loopback, 127.0.0.1 and ::1
// on one port chosen afresh, so a forwarder that carries one family to the
// other still arrives. The box dials that port by every route that can lead
// there, each with a token of its own, 128 random bits a launch, under the
// first route that names an address:
//
//   - loopback: 127.0.0.1 and ::1, the host's own where the box shares its
//     network namespace, by a setting or by the runtime's configuration;
//   - gateway: each default gateway in the box's routing tables, which
//     slirp4netns's allow_host_loopback, pasta's --map-gw and a relay lead
//     there;
//   - host_alias: every address the box's hosts file names, the runtime's
//     name for the host among them, which a virtual machine, a relay or
//     pasta's --map-host-loopback naming it leads there;
//   - rootless_gateway: in a user namespace of its own, as every rootless
//     runtime's box is (its uid_map is not the whole identity map, or cannot
//     be read), what lies beyond its own gateway: 10.0.2.2 and fd00::2,
//     slirp4netns's and RootlessKit's gateway, which their host-loopback
//     setting leads there, and this host's default gateways, which pasta
//     copies into a rootless network namespace and --map-gw leads there. The
//     engine hands every box these, and only such a box dials them, so a
//     rootful host pays no bound for addresses that lead out to its LAN;
//   - proxy: every value of http_proxy, https_proxy and all_proxy, either
//     case, read as the box's Chromium reads its environment (the scheme and
//     any user dropped, an HTTP proxy at the host and port named, 80 where
//     none is), asked for http://127.0.0.1:<port>/, http://localhost:<port>/
//     and http://[::1]:<port>/ and for a tunnel to each, since a page names
//     a host the proxy resolves or either address.
//
// The box reads its hosts file and routing tables itself, never a resolver,
// and resolves only a proxy's name, as its browser would.
//
// A proxy is asked at the check's port, which its own rules may refuse
// whatever the destination. So a proxy at one of this host's addresses (the
// engine hands them), which may share its loopback, that refused every ask is
// asked for TEST-NET-1 at the same port: refused alike, it refused the port,
// and the check cannot tell whether it refuses loopback destinations. A proxy
// elsewhere has a loopback of its own. A refusal comes before a proxy dials,
// so one that has not answered within [RouteBound] is dialing: not refused.
//
// Pasta's -T and -U bind the box's own loopback from outside the box, as a
// container sharing its network does, where no dial of the check's port need
// find them. So the box lists every socket bound on 127.0.0.1 and ::1 in its
// socket tables (TCP listening, UDP unconnected), twice, around reading which
// sockets each process it can see holds, thread by thread, since a process's
// own descriptor listing is its main thread's and empties once that thread
// exits: one listed both times and held by none was bound from outside it.
//
// # The verdict
//
// The engine reads a connection's first line, a token or a proxy's GET of
// /<token>, records the route, then answers ok. The box settles a dial by
// that answer, by any other answer or close, or as held when a made
// connection hears none in [AnswerBound]; a dial not made within [RouteBound]
// leads nowhere. Once the check's command ends the engine connects to each of
// its listeners last: a listener's queue is first in, first out, so every
// connection a route made is read before the verdict, however slowly the
// engine got to it. A connection carrying none of the check's tokens, more
// than 128 at once, or an accept that fails means something else reached
// the port, and the check withholds as unchecked, never passes. Which token
// arrived names the route, so nothing the box sends reaches the operator's
// log; its one line says only which boot id and namespace it read, and a
// word: checked; unreadable, a table or its hosts file unread; too_many;
// forwarded, a socket bound from outside it; held; proxy_down, a proxy that
// resolved to no address or none it could connect to, which its browser
// would go through unasked once it answers; proxy_refused; or no_node.
// [Check] decides in that order: another kernel, the machine's network
// namespace, a route that arrived, the port disturbed, a check that did not
// finish within [CheckTimeout], could not run or printed no line it reads,
// then the line's word: anything but checked is unchecked, and no_node is
// node_missing, before the helper is written. Only checked, on the engine's
// kernel, with nothing arrived or disturbed, leaves the box its browser.
//
// # Bounds
//
// [RouteBound] is how long a dial may go unconnected before the box settles
// it as leading nowhere: a connection through a forwarder on this kernel
// arrives in milliseconds (6 to 19 ms through two userspace relays, measured
// 2026-11-23), and the bound holds the kernel's first retransmission of a lost
// SYN, at one second, with half a second for its answer. A host whose firewall
// drops a box's packets to it, as Ubuntu's ufw does by default, waits it out
// on every launch; a proxy of this host that refuses loopback destinations
// costs it once more. [AnswerBound] holds a loaded engine host or proxy
// hundreds of times over, and bounds what a peer holding the check's port
// costs. [CheckTimeout] holds node's cold start (about a second) and the
// bounds with room; the box's half ends itself at it too, since a runtime's
// exec client that is killed leaves its command running in the box.
//
// # What the docs require
//
// The check finds a route by a connection that arrives, a socket the box
// lists or the namespace it says, as a page in the box's browser would reach
// one. The docs state the rule's requirements rather than what the check
// finds: the engine in its host's network namespace, beside the runtime; a
// proxy of this host that refuses loopback destinations by their resolved
// address at every port; and no forwarder or mapping to the host's loopback,
// for any port or address. A box cannot make the engine find a route it does
// not have, nor show a kernel it does not run on; its image, its setup and
// its runs are its operator's.
package hostreach

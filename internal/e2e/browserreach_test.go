package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	seatbrowser "github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/browser/hostreach"
	"github.com/crewlet/crewlet/internal/imagetest"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// relayAlias is the address the relayed case names as the runtime's name for
// the host: a loopback of the box's own, where the relay listens.
const relayAlias = "127.0.0.2"

// relayJS is the relay a virtual machine's name for its host stands for: it
// listens on the box's relayAlias at the port it is handed, carries each
// connection over /relay/sock, which the case serves, and once listening says
// ready over that socket.
const relayJS = `const net = require("net")
const server = net.createServer((c) => {
  const up = net.connect("/relay/sock")
  c.pipe(up)
  up.pipe(c)
  c.on("error", () => up.destroy())
  up.on("error", () => c.destroy())
})
server.listen(Number(process.argv[1]), "` + relayAlias + `", () => {
  const hello = net.connect("/relay/sock", () => hello.end("ready\n"))
})
`

// sidecarJS is a container sharing a box's network, its processes out of the
// box's sight: it listens on the box's 127.0.0.1, as pasta's -T binds a port
// there from outside the box, and once listening says ready over /relay/sock.
const sidecarJS = `const net = require("net")
net.createServer(() => {}).listen(5201, "127.0.0.1", () => {
  const hello = net.connect("/relay/sock", () => hello.end("ready\n"))
})
`

// A BROWSER IS WITHHELD WHERE ITS BOX REACHES ITS HOST'S LOOPBACK: the launch's
// own step in real container boxes. On the default bridge the browser is
// readied. On the host's network it is withheld host_network loopback where
// the box shares this process's network namespace, as a rootful runtime's
// does. Where the runtime's name for the host leads to this host's loopback,
// as a virtual machine's does (a relay in the box over a socket the case
// serves), host_network host_alias, a browser the box ran before stopped.
// Where a container sharing the box's network binds its loopback, unchecked.
// No box withheld first is written the helper. Mutations, each red: the half
// reading no hosts file or socket tables; arrivals ignored; the check after
// the helper's write or skipped; the stop skipped; on a host whose firewall
// lets a box reach it, the engine listening past its loopback.
func TestABrowserIsWithheldWhereItsBoxReachesItsHostsLoopback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a container runs Linux, and the newest Chromium pin is this host's build")
	}
	rt := imagetest.Require(t)
	logs.attribute(t)
	floor := prereqtest.ReadPins(t, filepath.Join(sourcetree.Root(t), "scripts", "node-pin.env"))["NODE_FLOOR_VERSION"]
	if floor == "" {
		t.Fatal("scripts/node-pin.env names no NODE_FLOOR_VERSION")
	}
	image := browserBoxImage(t, floor)
	newest := chromiumtest.Newest(t)
	mounts := []string{"-v", filepath.Join(boxhelpertest.Tree(t), "npm-cache") + ":" + boxNPMCache + ":ro",
		"-v", chromiumtest.Unpacked(t, newest) + ":" + boxChromium + ":ro"}
	registry := map[string]string{"NPM_CONFIG_REGISTRY": "http://127.0.0.1:1/"}
	// Not parallel, as the case beside it: one Chromium at a time.

	t.Run("on the bridge", func(t *testing.T) {
		box := browserBox(t, rt, image, "", mounts)
		if got := ensureIn(t, box, seatbrowser.SandboxWaived, registry); got.Record.State != seatbrowser.EnsureReady {
			t.Errorf("on the bridge the browser is %+v (%s), want ready", got.Record, got.Detail)
		}
	})

	t.Run("on its host's network", func(t *testing.T) {
		box := browserBox(t, rt, image, "host", mounts)
		ours, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		res, err := box.Exec(t.Context(), "readlink /proc/self/ns/net", sandbox.ExecOptions{})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("reading the box's network namespace: %v, %+v", err, res)
		}
		shared := strings.TrimSpace(res.Stdout) == ours
		got := ensureIn(t, box, seatbrowser.SandboxWaived, registry)
		switch r := got.Record; {
		case shared && (r.Reason != seatbrowser.ReasonHostNetwork || r.Cause != seatbrowser.CauseLoopback):
			t.Errorf("in this host's network namespace the browser is %+v (%s), want host_network loopback", r,
				got.Detail)
		case !shared && r.Cause == seatbrowser.CauseLoopback:
			t.Errorf("in a namespace of the runtime's own (%s, this host's %s) the browser is withheld as loopback, "+
				"which is not this host's", res.Stdout, ours)
		}
		if shared {
			helperUnwritten(t, box)
		}
	})

	t.Run("its host's name relayed to the host's loopback", func(t *testing.T) {
		dir := socketDir(t)
		relay := serveRelay(t, filepath.Join(dir, "sock"))
		box := browserBox(t, rt, image, "none", slices.Concat(mounts,
			[]string{"--add-host", "host.docker.internal:" + relayAlias, "-v", dir + ":/relay"}))
		got := ensureIn(t, relayedBox{Sandbox: box, rt: rt, relay: relay}, seatbrowser.SandboxWaived, registry)
		if r := got.Record; r.Reason != seatbrowser.ReasonHostNetwork || r.Cause != seatbrowser.CauseHostAlias {
			t.Errorf("with the host's name relayed to its loopback the browser is %+v (%s), want host_network "+
				"host_alias", r, got.Detail)
		}
		helperUnwritten(t, box)
	})

	t.Run("a browser it ran before, its host's name relayed since", func(t *testing.T) {
		dir := socketDir(t)
		relay := serveRelay(t, filepath.Join(dir, "sock"))
		box := browserBox(t, rt, image, "none", slices.Concat(mounts,
			[]string{"--add-host", "host.docker.internal:" + relayAlias, "-v", dir + ":/relay"}))
		if got := ensureIn(t, box, seatbrowser.SandboxWaived, registry); got.Record.State != seatbrowser.EnsureReady {
			t.Fatalf("before the relay the browser is %+v (%s), want ready", got.Record, got.Detail)
		}
		got := ensureIn(t, relayedBox{Sandbox: box, rt: rt, relay: relay}, seatbrowser.SandboxWaived, registry)
		if r := got.Record; r.Reason != seatbrowser.ReasonHostNetwork || r.Cause != seatbrowser.CauseHostAlias ||
			strings.Contains(got.Detail, "could not be stopped") {
			t.Errorf("with the host's name relayed since, the browser is %+v (%s), want host_network host_alias, "+
				"its browser stopped", r, got.Detail)
		}
		const said = `const s = require(process.env.HOME + "/.crewlet/browser-box/supervisor.json")
let alive = true
try { process.kill(s.pid, 0) } catch { alive = false }
console.log(s.state + " " + alive)`
		res, err := box.Exec(t.Context(), "node -e "+shellword.Quote(said), sandbox.ExecOptions{})
		if err != nil || strings.TrimSpace(res.Stdout) != "stopped false" {
			t.Errorf("the box's supervisor says %q (%v, exit %d), want stopped and gone", res.Stdout, err, res.ExitCode)
		}
	})

	t.Run("its loopback bound from outside it", func(t *testing.T) {
		dir := socketDir(t)
		hello := serveRelay(t, filepath.Join(dir, "sock"))
		box := browserBox(t, rt, image, "", mounts)
		name := "crewlet-e2e-sidecar-" + box.ID()
		run := exec.CommandContext(t.Context(), rt, "run", "-d", "--rm", "--name", name,
			"--network", "container:"+sandbox.ContainerPrefix+box.ID(), "-v", dir+":/relay", image, "node", "-e",
			sidecarJS)
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatalf("starting the sidecar: %v: %s", err, out)
		}
		t.Cleanup(func() { _ = exec.CommandContext(context.WithoutCancel(t.Context()), rt, "rm", "-f", name).Run() })
		select {
		case <-hello.ready:
		case <-t.Context().Done():
			t.Fatal("the sidecar never said it listens")
		}
		got := ensureIn(t, box, seatbrowser.SandboxWaived, registry)
		if r := got.Record; r.Reason != seatbrowser.ReasonHostNetwork || r.Cause != seatbrowser.CauseUnchecked ||
			!strings.Contains(got.Detail, "held by no process the check can see") {
			t.Errorf("with its loopback bound from outside it the browser is %+v (%s), want host_network "+
				"unchecked, the socket named", r, got.Detail)
		}
		helperUnwritten(t, box)
	})
}

// socketDir is a short directory for a socket the case serves a box, removed
// at the case's end.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "crelay") //nolint:usetesting // t.TempDir is named for the test, past a socket path's bound
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// helperUnwritten fails t if box holds anything at the helper's folder.
func helperUnwritten(t *testing.T, box sandbox.Sandbox) {
	t.Helper()
	folder := boxhelper.Folder(box.Home())
	res, err := box.Exec(t.Context(), "[ ! -e "+shellword.Quote(folder)+" ]", sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		t.Errorf("a withheld box holds %s (%v, exit %d)", folder, err, res.ExitCode)
	}
}

// relay is the case's end of a relay in a box: the port each connection is
// carried on to, set once the box's check is handed it, and a channel closed
// once the relay in the box says it listens.
type relay struct {
	mu    sync.Mutex
	port  string
	ready chan struct{}
}

// to sets the engine's port the relay carries each connection to.
func (r *relay) to(port string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.port = port
}

// serveRelay serves socket for a box's relay: its first connection the relay's
// hello, every later one carried to the engine's port on 127.0.0.1. The case's
// end stops it.
func serveRelay(t *testing.T, socket string) *relay {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{ready: make(chan struct{})}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = l.Close()
		wg.Wait()
	})
	wg.Go(func() {
		hello, err := l.Accept()
		if err != nil {
			return
		}
		said, _ := bufio.NewReader(hello).ReadString('\n')
		_ = hello.Close()
		if said == "ready\n" {
			close(r.ready)
		}
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { r.carry(conn) })
		}
	})
	return r
}

// carry pipes one relayed connection to the engine's port, each way.
func (r *relay) carry(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r.mu.Lock()
	port := r.port
	r.mu.Unlock()
	up, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(up, conn)
		close(done)
	}()
	_, _ = io.Copy(conn, up)
	_ = conn.Close()
	<-done
}

// relayedBox is a container box whose check finds a relay on its runtime's
// name for the host: when the launch's check comes, it starts the relay in
// the box on the port the engine handed, waits for the relay's hello, then
// runs the check.
type relayedBox struct {
	sandbox.Sandbox
	rt    string
	relay *relay
}

// EngineKernel is the box's own: a container box's runtime runs on this host.
func (b relayedBox) EngineKernel() (hostreach.Kernel, error) {
	host, ok := b.Sandbox.(interface {
		EngineKernel() (hostreach.Kernel, error)
	})
	if !ok {
		return "", errors.New("the relayed box's own handle cannot say its runtime's kernel")
	}
	return host.EngineKernel()
}

func (b relayedBox) Exec(ctx context.Context, cmd string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	if cmd != hostreach.Command() {
		return b.Sandbox.Exec(ctx, cmd, opts)
	}
	port := opts.Env[hostreach.PortEnv]
	b.relay.to(port)
	start := exec.CommandContext(ctx, b.rt, "exec", "-d", sandbox.ContainerPrefix+b.ID(), "node", "-e", relayJS, port)
	if out, err := start.CombinedOutput(); err != nil {
		return sandbox.ExecResult{}, fmt.Errorf("starting the relay in the box: %w: %s", err, out)
	}
	select {
	case <-b.relay.ready:
	case <-ctx.Done():
		return sandbox.ExecResult{}, ctx.Err()
	}
	return b.Sandbox.Exec(ctx, cmd, opts)
}

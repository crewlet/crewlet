package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	seatbrowser "github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/imagetest"
	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// browserBoxCacheEnv names the directory CI keeps the browser box image in
// between runs, as a tar (ci.yml's `gates` job). Unset, as on a workstation,
// an image the runtime does not hold is built.
const browserBoxCacheEnv = "CREWLET_BROWSERBOX_CACHE"

// Where a browser box holds what the case mounts into it, read-only: the suite
// tree's npm cache, which the image's global npmrc names, and the newest
// Chromium pin's build, which the image's PATH holds as chromium.
const (
	boxNPMCache = "/var/cache/crewlet-npm"
	boxChromium = "/opt/crewlet-test-chromium"
)

// browserBoxDockerfile is the image a browser seat's box runs in here: the
// floor Node's slim Debian 13, Debian's stable release and one the locked
// Playwright's installer supports, with the npm it bundles, Chromium's
// libraries and fonts as that installer lists them, a global npmrc naming
// boxNPMCache and a chromium on PATH linked to boxChromium, and no browser and
// no tree of its own. Its text names the floor Node and the tree's installed
// digest, so a bump of either builds it again (internal/imagetest's "The tag
// names the text"). apt retries each fetch five times, as
// internal/ssh/sshdtest's images do and for their reason.
const browserBoxDockerfile = `FROM node:%[1]s-trixie-slim
LABEL crewlet.playwright-tree.installed=%[2]s
COPY playwright-core /tmp/playwright-core
RUN printf 'Acquire::Retries "5";\n' >/etc/apt/apt.conf.d/80crewlet-retries && \
    DEBIAN_FRONTEND=noninteractive node /tmp/playwright-core/cli.js install-deps chromium && \
    rm -rf /tmp/playwright-core /var/lib/apt/lists/* && \
    npm config set --location=global cache %[3]s && ln -s %[4]s/chrome /usr/local/bin/chromium
`

// browserBoxImage is the image's tag once the runtime holds it, built on
// floor, the floor Node's release. Its build copies the suite tree's
// playwright-core to run the installer, then removes it.
func browserBoxImage(t *testing.T, floor string) string {
	t.Helper()
	installed, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "internal", "browser", "boxhelper", "tree",
		"installed.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	return imagetest.Provide(t, imagetest.Image{Repo: "crewlet-e2e-browserbox", Name: "node:" + floor + "-trixie-slim",
		Text:    fmt.Sprintf(browserBoxDockerfile, floor, strings.TrimSpace(string(installed)), boxNPMCache, boxChromium),
		Context: filepath.Join(boxhelpertest.Tree(t), "node_modules"), CacheEnv: browserBoxCacheEnv})
}

// seccompDenyingUserNamespaces refuses what Chromium's own sandbox starts
// from, a user namespace, and allows the rest: unshare, and a clone asking for
// CLONE_NEWUSER (0x10000000), fail with EPERM (1), and clone3, whose flags a
// filter cannot read, with ENOSYS (38), so glibc falls back to clone, as
// Docker's default profile has it. A profile of the case's own, since Docker's
// default refuses the sandbox and Podman's allows it.
const seccompDenyingUserNamespaces = `{
  "defaultAction": "SCMP_ACT_ALLOW",
  "syscalls": [
    {"names": ["unshare"], "action": "SCMP_ACT_ERRNO", "errnoRet": 1},
    {"names": ["clone"], "action": "SCMP_ACT_ERRNO", "errnoRet": 1,
      "args": [{"index": 0, "value": 268435456, "valueTwo": 268435456, "op": "SCMP_CMP_MASKED_EQ"}]},
    {"names": ["clone3"], "action": "SCMP_ACT_ERRNO", "errnoRet": 38}
  ]
}`

// boxShown is every cause a box shows for a sandbox that would not start, read
// from its own kernel files, the first that holds being the host's: root (a
// rootless runtime's box), Ubuntu's AppArmor restriction (GitHub's runners
// keep it), user namespaces off, or a seccomp filter. Never launcher, a browser
// run with --no-sandbox, nor unknown, since the profile is a filter.
var boxShown = []seatbrowser.EnsureCause{seatbrowser.CauseRoot, seatbrowser.CauseAppArmor,
	seatbrowser.CauseUserns, seatbrowser.CauseSeccomp}

// pageFence opens a page's words as a box's browser hands them to a model: a
// page in a seat's own browser, at external trust, no author named.
var pageFence = regexp.MustCompile(`^\[untrusted:([A-Z2-7]{16}) source=browser_page trust=external authors=unknown\]\n`)

// pageMarker is what the case's page says, which its snapshot must carry.
const pageMarker = "A page this case wrote"

// A BROWSER SEAT'S BOX RUNS ITS BROWSER IN A CONTAINER: the launch's own step,
// in a real container box of a floor-Node image with no browser or tree,
// readies the browser from the image's npm cache and the mounted newest
// Chromium pin, and its server, run in the box as a harness runs it, drives a
// page and saves a screenshot in the workspace; ensure runs with the run's
// environment alone, which names the cache or empties it; a required sandbox
// the box refuses is withheld with a cause the box shows (apparmor under
// Ubuntu's restriction, as on GitHub's runners; seccomp, the case's own
// profile, only where nothing refuses first). The newest pin alone: a build's
// own launch is boxhelper's subject on both (boxhelpertest.Each), this case's
// the cell, which another build reruns unchanged. Mutations, each red: the
// step's Env not passed; chromiumSandbox dropped; the server rooted at home.
func TestABrowserSeatsBoxRunsItsBrowserInAContainer(t *testing.T) {
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
	chromium := chromiumtest.Version(t, newest)
	major, ok := seatbrowser.ChromiumMajor(chromium)
	outdated, err := seatbrowser.Outdated(chromium)
	if !ok || err != nil {
		t.Fatalf("the newest pin answered %s, which is no Chromium version (%v)", chromium, err)
	}
	mounts := []string{"-v", filepath.Join(boxhelpertest.Tree(t), "npm-cache") + ":" + boxNPMCache + ":ro",
		"-v", chromiumtest.Unpacked(t, newest) + ":" + boxChromium + ":ro"}
	// A registry on a port nothing in the box listens on, in every run's
	// environment: a box that misses its cache fails at once. Each box is on
	// no network, so nothing it does leaves it, even a step that dropped the
	// run's environment and its refused registry.
	registry := map[string]string{"NPM_CONFIG_REGISTRY": "http://127.0.0.1:1/"}
	// Not parallel: one Chromium beside nothing else, as chromiumtest's cases
	// run, so ensure's bounds measure the box and never a loaded shard.

	t.Run("the image's cache, waived", func(t *testing.T) {
		box := browserBox(t, rt, image, "none", mounts)
		want := types.ExecutorBrowser{State: seatbrowser.EnsureReady, Chromium: chromium, ChromiumMajor: major,
			Outdated: outdated, Build: seatbrowser.BuildUntested, Sandbox: seatbrowser.SandboxWaived, Node: floor,
			Helper: boxhelper.Version(), Pin: seatbrowser.PlaywrightMCPRelease}
		first := ensureIn(t, box, seatbrowser.SandboxWaived, registry)
		if first.Record != want || first.Supervisor != seatbrowser.SupervisorStarted || first.Server == nil {
			t.Fatalf("the first launch's browser is %+v (%s), supervisor %q, server %v; want %+v, started, a server",
				first.Record, first.Detail, first.Supervisor, first.Server, want)
		}
		second := ensureIn(t, box, seatbrowser.SandboxWaived, registry)
		if second.Record != want || second.Supervisor != seatbrowser.SupervisorFound || second.Server == nil {
			t.Fatalf("the next launch's browser is %+v (%s), supervisor %q; want %+v, found", second.Record,
				second.Detail, second.Supervisor, want)
		}

		m := shimIn(t, rt, box, *second.Server)
		m.Initialize("2025-11-25", map[string]any{})
		for _, tool := range m.ListTools() {
			if tool.Name == "browser_run_code_unsafe" {
				t.Errorf("the model is shown %s, which runs code in the supervisor", tool.Name)
			}
		}
		page := "<title>" + pageMarker + "</title><p>" + pageMarker + "</p>"
		toolActed(t, m.Call("browser_navigate",
			map[string]any{"url": "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(page))}),
			"browser_navigate")
		snapshot := m.Call("browser_snapshot", nil)
		toolActed(t, snapshot, "browser_snapshot")
		if !fencedPage(t, snapshot) {
			t.Errorf("the snapshot carries %q in no page's fence: %.600q", pageMarker, snapshot.Text())
		}
		toolActed(t, m.Call("browser_take_screenshot", map[string]any{"filename": "shot.png"}),
			"browser_take_screenshot")
		shot := path.Join(box.Home(), sandbox.WorkspaceSubdir, "shot.png")
		if png, err := box.ReadFile(t.Context(), shot); err != nil || !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
			t.Errorf("%s, the screenshot the model named, reads %d bytes (%v), want a PNG in the run's workspace", shot,
				len(png), err)
		}
	})

	t.Run("the run's environment and its npmrc name the cache", func(t *testing.T) {
		box := browserBox(t, rt, image, "none", mounts)
		// An empty global npmrc keeps the image's out: only the run's
		// environment, read by the setup's npmrc, names the cache.
		env := maps.Clone(registry)
		env["NPM_CONFIG_GLOBALCONFIG"] = path.Join(box.Home(), "empty-npmrc")
		env["CREWLET_TEST_NPM_CACHE"] = boxNPMCache
		if err := box.WriteFile(t.Context(), env["NPM_CONFIG_GLOBALCONFIG"], nil); err != nil {
			t.Fatal(err)
		}
		setup := []sandbox.SetupStep{{Name: "npmrc", Files: map[string]string{".npmrc": "cache=${CREWLET_TEST_NPM_CACHE}\n"}}}
		if err := sandbox.ApplySetup(t.Context(), box, setup, env); err != nil {
			t.Fatal(err)
		}
		if got := ensureIn(t, box, seatbrowser.SandboxWaived, env); got.Record.State != seatbrowser.EnsureReady {
			t.Errorf("with the cache named by the run's environment alone, the browser is %+v (%s), want ready",
				got.Record, got.Detail)
		}
	})

	t.Run("no cache", func(t *testing.T) {
		box := browserBox(t, rt, image, "none", mounts)
		env := maps.Clone(registry)
		env["NPM_CONFIG_CACHE"] = path.Join(box.Home(), "npm-cache")
		got := ensureIn(t, box, seatbrowser.SandboxWaived, env)
		if r := got.Record; r.State != seatbrowser.EnsureWithheld || r.Reason != seatbrowser.ReasonPlaywrightMissing ||
			r.Cause != seatbrowser.CauseRegistryUnreachable || got.Server != nil {
			t.Errorf("with an empty cache of its own, the browser is %+v (%s), server %v; want withheld %s %s",
				r, got.Detail, got.Server, seatbrowser.ReasonPlaywrightMissing, seatbrowser.CauseRegistryUnreachable)
		}
	})

	t.Run("required, denied", func(t *testing.T) {
		profile := filepath.Join(t.TempDir(), "seccomp.json")
		if err := os.WriteFile(profile, []byte(seccompDenyingUserNamespaces), 0o644); err != nil {
			t.Fatal(err)
		}
		box := browserBox(t, rt, image, "none", append(slices.Clone(mounts), "--security-opt", "seccomp="+profile))
		got := ensureIn(t, box, seatbrowser.SandboxRequired, registry)
		if r := got.Record; r.State != seatbrowser.EnsureWithheld ||
			r.Reason != seatbrowser.ReasonChromiumSandboxUnavailable || !slices.Contains(boxShown, r.Cause) ||
			got.Server != nil {
			t.Errorf("with a required sandbox the box refuses, the browser is %+v (%s), server %v; want "+
				"withheld %s with a cause the box shows", r, got.Detail, got.Server,
				seatbrowser.ReasonChromiumSandboxUnavailable)
		}
	})
}

// browserBox is a fresh box of image in the container cell, on network ("" the
// runtime's default bridge), runArgs on its run's command line, made for a
// turn as a launch makes one. The case's end removes it.
func browserBox(t *testing.T, rt, image, network string, runArgs []string) sandbox.Sandbox {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Container, StateDir: t.TempDir(),
		Image: image, Runtime: rt, Network: network, RunArgs: runArgs})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{Placement: sandbox.Container, TurnID: uuid.NewString()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = box.Close(context.Background()) })
	return box
}

// ensureIn is the launch's own browser step in box for a launch of its own,
// its cell's sandbox s and the run's environment env, failing t on an error,
// which only the case's own end gives.
func ensureIn(t *testing.T, box sandbox.Sandbox, s seatbrowser.ChromiumSandbox,
	env map[string]string) sandbox.BrowserOutcome {
	t.Helper()
	out, err := sandbox.EnsureBrowser(t.Context(), box, sandbox.BrowserStep{Placement: sandbox.Container,
		Browser: sandbox.BrowserLaunch{Sandbox: s}, Env: env, LaunchID: uuid.NewString()})
	if err != nil {
		t.Fatalf("EnsureBrowser: %v", err)
	}
	return out
}

// shimIn starts server, the step's rendering, in box as its harness starts it
// there, over `<runtime> exec -i`, and answers an MCP client of it. The case's
// end kills the exec client, whose pipes then close, and reaps it.
func shimIn(t *testing.T, rt string, box sandbox.Sandbox, server sandbox.MCPServer) *boxhelpertest.MCP {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), rt,
		slices.Concat([]string{"exec", "-i", sandbox.ContainerPrefix + box.ID(), server.Command}, server.Args)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the browser's server in the box: %v", err)
	}
	m := boxhelpertest.MCPOver(t, stdin, stdout)
	// The client's reads end before the reap closes its pipe.
	t.Cleanup(func() {
		m.Close()
		<-m.Closed()
		if err := cmd.Wait(); err != nil && t.Failed() {
			t.Logf("the browser's server ended %v, saying: %s", err, stderr.String())
		}
	})
	return m
}

// toolActed fails t unless r answers a call that did what it was asked.
func toolActed(t *testing.T, r boxhelpertest.Result, what string) {
	t.Helper()
	if r.Error != nil || r.IsError {
		t.Fatalf("%s was answered %+v %.600q", what, r.Error, r.Text())
	}
}

// fencedPage reports whether r carries pageMarker inside a page's fence, each
// of its text items one fence wrapping the whole item.
func fencedPage(t *testing.T, r boxhelpertest.Result) bool {
	t.Helper()
	found := false
	for _, c := range r.Content {
		if c.Type != "text" {
			continue
		}
		m := pageFence.FindStringSubmatch(c.Text)
		if m == nil {
			t.Errorf("a text item is not fenced as a page's words: %.300q", c.Text)
			continue
		}
		body := provenance.Unfence(provenance.Boundary(m[1]), c.Text)
		found = found || (body != c.Text && strings.Contains(body, pageMarker))
	}
	return found
}

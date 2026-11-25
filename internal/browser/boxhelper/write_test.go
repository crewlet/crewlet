package boxhelper_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
)

// recorder is a box that keeps every file written into it, in order, and
// refuses the one call numbered refuseAt (from 0) with refusal, if it is set,
// taking every call after it.
type recorder struct {
	calls    int
	paths    []string
	bodies   map[string][]byte
	refuseAt int
	refusal  error
}

func (r *recorder) WriteFile(_ context.Context, path string, content []byte) error {
	call := r.calls
	r.calls++
	if r.refusal != nil && call == r.refuseAt {
		return r.refusal
	}
	if r.bodies == nil {
		r.bodies = map[string][]byte{}
	}
	r.paths = append(r.paths, path)
	r.bodies[path] = bytes.Clone(content)
	return nil
}

// THE BOX IS WRITTEN THE EMBEDDED HELPER, THEN A CONFIG NAMING IT: Files in
// order into .crewlet/browser-box, the committed manifest and lock as
// tree-package.json and tree-package-lock.json, then config.json naming the
// Version, release, digests, floors, sandbox and the live view's blocks, each
// its constant's, and bounds that nest: npm's fetches in the install's, a
// launch in the supervisor's wait, both in ensure's deadline, under
// EnsureTimeout and a takeover; a quiet capture no sooner than a watch slot;
// a state within a relay.sock header; the oversize capture smaller.
//
// Mutations, each red: config.json first; the digest over the lock alone; the
// lock as tree-package.json; npm_floor from MinNodeMajor; a 30 s fetch
// timeout; ensure's deadline EnsureTimeout; quiet_ms a literal 50.
func TestTheBoxIsWrittenTheEmbeddedHelperThenAConfigNamingIt(t *testing.T) {
	t.Parallel()
	const home = "/home/user"
	folder := home + "/.crewlet/browser-box"
	if got := boxhelper.Folder(home); got != folder {
		t.Fatalf("Folder(%q) is %q, want %q", home, got, folder)
	}
	box := &recorder{}
	if err := boxhelper.Write(t.Context(), box, home, boxhelper.Settings{Sandbox: browser.SandboxWaived}); err != nil {
		t.Fatal(err)
	}
	manifest, lock := committedTree(t)
	var want []string
	for _, f := range boxhelper.Files() {
		want = append(want, folder+"/"+f.Name)
		if !bytes.Equal(box.bodies[folder+"/"+f.Name], f.Body) {
			t.Errorf("%s is not written byte for byte", f.Name)
		}
	}
	want = append(want, folder+"/tree-package.json", folder+"/tree-package-lock.json", folder+"/config.json")
	if !slices.Equal(box.paths, want) {
		t.Fatalf("Write wrote %q, want %q in that order", box.paths, want)
	}
	for name, body := range map[string][]byte{"tree-package.json": manifest, "tree-package-lock.json": lock} {
		if !bytes.Equal(box.bodies[folder+"/"+name], body) {
			t.Errorf("%s is not the committed tree/%s byte for byte", name, strings.TrimPrefix(name, "tree-"))
		}
	}

	var pkg struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(manifest, &pkg); err != nil {
		t.Fatal(err)
	}
	m, l := sha256.Sum256(manifest), sha256.Sum256(lock)
	digest := sha256.Sum256([]byte(hex.EncodeToString(m[:]) + "\n" + hex.EncodeToString(l[:]) + "\n"))
	raw := box.bodies[folder+"/config.json"]
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("config.json %q: %v", raw, err)
	}
	for key, want := range map[string]any{
		"v": 1.0, "helper": boxhelper.Version(), "pin": pkg.Dependencies["@playwright/mcp"],
		"pin_digest": hex.EncodeToString(digest[:])[:12], "installed_digest": committedInstalled(t),
		"node_floor": float64(boxhelper.MinNodeMajor), "npm_floor": float64(boxhelper.MinNPMMajor),
		"sandbox": "waived",
	} {
		if got := config[key]; got != want {
			t.Errorf("config.json's %s is %v, want %v", key, got, want)
		}
	}

	// count is the whole number object holds at key, failing t on anything
	// else.
	count := func(object map[string]any, key string) int {
		n, ok := object[key].(float64)
		if !ok || n < 0 || n != math.Trunc(n) {
			t.Fatalf("config.json's %s is %v in %v, not a whole number", key, object[key], object)
		}
		return int(n)
	}
	install, _ := config["install"].(map[string]any)
	timeout, fetch := count(install, "timeout_ms"), count(install, "fetch_timeout_ms")
	retries, wait := count(install, "fetch_retries"), count(install, "fetch_retry_wait_ms")
	grace, deadline := count(config, "stop_grace_ms"), count(config, "ensure_deadline_ms")
	launch, supervisor := count(config, "launch_timeout_ms"), count(config, "supervisor_wait_ms")
	if timeout == 0 || fetch == 0 || wait == 0 || grace == 0 || launch == 0 {
		t.Errorf("config.json's bounds are %v, stop_grace_ms %d and launch_timeout_ms %d: each is a time, above "+
			"zero", install, grace, launch)
	}
	if tries := (retries+1)*fetch + retries*wait; tries > timeout {
		t.Errorf("npm's own tries of a silent registry take %d ms, past install.timeout_ms %d: it would read as "+
			"timeout, never as the registry_unreachable npm says", tries, timeout)
	}
	if launch >= supervisor {
		t.Errorf("launch_timeout_ms %d is not under supervisor_wait_ms %d: a hung launch would read as the "+
			"supervisor's silence, not its own timeout", launch, supervisor)
	}
	if steps := timeout + grace + supervisor; steps > deadline {
		t.Errorf("an install's bound, its grace and the supervisor's wait take %d ms, past ensure_deadline_ms %d",
			steps, deadline)
	}
	engine := int(boxhelper.EnsureTimeout / time.Millisecond)
	if takeover := count(config, "lock_takeover_ms"); deadline >= engine || takeover <= engine {
		t.Errorf("ensure_deadline_ms %d and lock_takeover_ms %d, with EnsureTimeout %d ms: ensure answers before "+
			"the engine's bound, and a lock is taken over only once a whole ensure is past it", deadline, takeover,
			engine)
	}

	frames, _ := config["frames"].(map[string]any)
	state, _ := config["state"].(map[string]any)
	relay, _ := config["relay"].(map[string]any)
	for block, want := range map[string]map[string]any{
		"frames": {"quality": browser.FrameQuality, "max_width": browser.MaxFrameWidth,
			"max_height": browser.MaxFrameHeight, "max_bytes": browser.MaxFrameBytes,
			"meta_bytes": browser.MaxFrameMetaBytes, "jpeg_header_bytes": browser.JPEGHeaderBytes,
			"device_max": browser.MaxDeviceSize, "scale_min": browser.MinPageScale, "scale_max": browser.MaxPageScale,
			"watch_fps": browser.WatchFPS, "flow_major": live.ScreencastFlowMajor, "quiet_ms": live.QuietMS,
			"oversize_quality": live.OversizeQuality, "oversize_scale": live.OversizeScale},
		"state": {"max_bytes": browser.MaxStateBytes, "max_tabs": browser.MaxTabs,
			"title_chars": browser.MaxTitleChars, "url_bytes": browser.MaxURLBytes, "title_ms": live.TitleMS},
		"relay": {"header_bytes": live.MaxHeaderBytes},
	} {
		got, _ := config[block].(map[string]any)
		for key, v := range want {
			if f, ok := got[key].(float64); !ok || f != toFloat(v) {
				t.Errorf("config.json's %s.%s is %v, want %v, its constant's", block, key, got[key], v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("config.json's %s holds %d keys, want the %d its constants name", block, len(got), len(want))
		}
	}
	if slot := 1000 / count(frames, "watch_fps"); count(frames, "quiet_ms") < slot {
		t.Errorf("frames.quiet_ms %d is under one watch slot, %d ms: a capture would race the screencast's own last "+
			"frame", count(frames, "quiet_ms"), slot)
	}
	if header := count(relay, "header_bytes"); header <= count(state, "max_bytes") || header < count(frames, "meta_bytes") {
		t.Errorf("relay.header_bytes %d holds no state of %d bytes with its record's own fields, or no frame's "+
			"metadata", header, count(state, "max_bytes"))
	}
	if count(frames, "oversize_quality") >= count(frames, "quality") || toFloat(frames["oversize_scale"]) >= 1 {
		t.Errorf("the oversize capture is at quality %v and scale %v: a frame past its bound is replaced by a "+
			"smaller one", frames["oversize_quality"], frames["oversize_scale"])
	}
}

// toFloat is a constant as encoding/json decodes it.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	return math.NaN()
}

// A WRITE THE BOX REFUSES REACHES THE CALLER AS ITSELF: whichever file of
// Write's the box refuses, the call stops there, writing nothing after it,
// and returns the refusal wrapped, so errors.Is still tells it (a backend on
// the engine host refuses a path holding anything but a regular file with a
// sentinel of its own) and its words name the path refused.
//
// Mutations, each red: wrap with %v; go on writing after a refusal.
func TestAWriteTheBoxRefusesReachesTheCallerAsItself(t *testing.T) {
	t.Parallel()
	refusal := errors.New("the stand-in box refuses this path")
	settings := boxhelper.Settings{Sandbox: browser.SandboxRequired}
	whole := &recorder{}
	if err := boxhelper.Write(t.Context(), whole, "/home/user", settings); err != nil ||
		len(whole.paths) != len(boxhelper.Files())+3 {
		t.Fatalf("Write wrote %q (%v), want %d files", whole.paths, err, len(boxhelper.Files())+3)
	}
	for at, path := range whole.paths {
		box := &recorder{refuseAt: at, refusal: refusal}
		err := boxhelper.Write(t.Context(), box, "/home/user", settings)
		if !errors.Is(err, refusal) || err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("Write refused at %s returned %v, want the refusal, wrapped, naming the path", path, err)
		}
		if !slices.Equal(box.paths, whole.paths[:at]) {
			t.Errorf("Write refused at %s wrote %q, want %q and nothing after", path, box.paths, whole.paths[:at])
		}
	}
}

// A SANDBOX NOBODY RESOLVED IS REFUSED, NEVER READ AS EITHER: Write refuses
// settings whose sandbox is unset or off the list, naming the setting, before
// it writes a single file, so a launch that skipped resolving its cell's
// chromium_sandbox never runs a box's browser on a guess.
//
// Mutation: read an unset sandbox as required (the zero row writes a box).
func TestASandboxNobodyResolvedIsRefusedNeverReadAsEither(t *testing.T) {
	t.Parallel()
	for _, sandbox := range []browser.ChromiumSandbox{"", "off", "Required"} {
		box := &recorder{}
		err := boxhelper.Write(t.Context(), box, "/home/user", boxhelper.Settings{Sandbox: sandbox})
		if err == nil || !strings.Contains(err.Error(), "chromium_sandbox") || len(box.paths) > 0 {
			t.Errorf("Write with sandbox %q returned %v and wrote %q, want a refusal naming chromium_sandbox and "+
				"nothing written", sandbox, err, box.paths)
		}
	}
}

// A CONFIG OF ANOTHER LIVE SHAPE IS REFUSED WHOLE: readConfig refuses a
// config.json whose frames, state or relay block is missing, or holds a key
// missing, of another type, or past its range, naming the key, so a box
// never runs its live view on a bound it guessed; the engine's own is read.
// On each Node, whose JSON and numbers the helper reads them with.
//
// Mutation: read frames without holding it to its shape.
func TestAConfigOfAnotherLiveShapeIsRefusedWhole(t *testing.T) {
	cases := []struct {
		block, key string
		value      any // nil leaves the key out
	}{
		{"frames", "", nil}, {"state", "", nil}, {"relay", "", nil},
		{"frames", "quality", 0}, {"frames", "quality", 101}, {"frames", "quality", "70"},
		{"frames", "max_width", 0}, {"frames", "max_height", nil}, {"frames", "max_bytes", 0},
		{"frames", "meta_bytes", -1}, {"frames", "jpeg_header_bytes", 3}, {"frames", "device_max", 0},
		{"frames", "scale_min", 0}, {"frames", "scale_max", 0.05}, {"frames", "watch_fps", 0},
		{"frames", "watch_fps", 61}, {"frames", "flow_major", 0}, {"frames", "quiet_ms", 0},
		{"frames", "oversize_quality", nil}, {"frames", "oversize_scale", 1.5}, {"frames", "oversize_scale", 0},
		{"state", "max_bytes", 0}, {"state", "max_tabs", 1001}, {"state", "title_chars", -1},
		{"state", "url_bytes", nil}, {"state", "title_ms", 0}, {"relay", "header_bytes", 1},
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		probe := box.Probe(t)
		type answer struct {
			OK      bool   `json:"ok"`
			Refused string `json:"refused"`
		}
		var read answer
		probe.Do(map[string]any{"op": "config_read"}, &read)
		if !read.OK {
			t.Fatalf("the engine's own config.json was refused: %s", read.Refused)
		}
		engine := box.Config(t)
		for _, c := range cases {
			box.SetConfig(t, engine)
			config := box.Config(t)
			named := c.block
			if c.key == "" {
				delete(config, c.block)
			} else {
				named += "." + c.key
				block := config[c.block].(map[string]any)
				if c.value == nil {
					delete(block, c.key)
				} else {
					block[c.key] = c.value
				}
			}
			box.SetConfig(t, config)
			read := answer{}
			probe.Do(map[string]any{"op": "config_read"}, &read)
			if read.OK || !strings.Contains(read.Refused, " has "+named+" ") {
				t.Errorf("config.json with %s %v was answered %+v, want it refused naming %s", named, c.value, read, named)
			}
		}
		box.SetConfig(t, engine)
	})
}

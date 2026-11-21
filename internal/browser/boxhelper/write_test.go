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

	"github.com/crewlet/crewlet/internal/browser/boxhelper"
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

// THE BOX IS WRITTEN THE EMBEDDED HELPER, THEN A CONFIG NAMING IT: Write puts
// every module of Files into the box's .crewlet/browser-box in Files' order,
// then the committed manifest and lock as tree-package.json and
// tree-package-lock.json, each byte for byte, and config.json last, naming
// the helper's Version, the manifest's release, the 12 digits of SHA-256 over
// the two files' SHA-256s the tree's name carries, the committed digest of a
// whole install, npm's floor, and bounds whose fetches end inside the
// install's, each read here from the committed files.
//
// Mutations, each red: write config.json first; the digest over the lock
// alone; the lock written as tree-package.json; npm_floor from MinNodeMajor; a
// fetch timeout of 30 s (two tries no longer fit in the install's 60).
func TestTheBoxIsWrittenTheEmbeddedHelperThenAConfigNamingIt(t *testing.T) {
	t.Parallel()
	const home = "/home/user"
	folder := home + "/.crewlet/browser-box"
	if got := boxhelper.Folder(home); got != folder {
		t.Fatalf("Folder(%q) is %q, want %q", home, got, folder)
	}
	box := &recorder{}
	if err := boxhelper.Write(t.Context(), box, home); err != nil {
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
		"npm_floor": float64(boxhelper.MinNPMMajor),
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
	if timeout == 0 || fetch == 0 || wait == 0 || count(config, "stop_grace_ms") == 0 {
		t.Errorf("config.json's bounds are %v and stop_grace_ms %v: each is a time, above zero", install,
			config["stop_grace_ms"])
	}
	if tries := (retries+1)*fetch + retries*wait; tries > timeout {
		t.Errorf("npm's own tries of a silent registry take %d ms, past install.timeout_ms %d: it would read as "+
			"timeout, never as the registry_unreachable npm says", tries, timeout)
	}
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
	whole := &recorder{}
	if err := boxhelper.Write(t.Context(), whole, "/home/user"); err != nil ||
		len(whole.paths) != len(boxhelper.Files())+3 {
		t.Fatalf("Write wrote %q (%v), want %d files", whole.paths, err, len(boxhelper.Files())+3)
	}
	for at, path := range whole.paths {
		box := &recorder{refuseAt: at, refusal: refusal}
		err := boxhelper.Write(t.Context(), box, "/home/user")
		if !errors.Is(err, refusal) || err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("Write refused at %s returned %v, want the refusal, wrapped, naming the path", path, err)
		}
		if !slices.Equal(box.paths, whole.paths[:at]) {
			t.Errorf("Write refused at %s wrote %q, want %q and nothing after", path, box.paths, whole.paths[:at])
		}
	}
}

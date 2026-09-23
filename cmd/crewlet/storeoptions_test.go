package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// EVERY COMMAND OPENS THE REPLICATED ESTATE TIER A NAMES, AND NO OTHER.
//
// store.replicated_path moves the second database off store.path's directory,
// and a command that opened the store without it would derive the default
// beside store.path instead — locking, creating and migrating a database the
// engine never reads, and answering from it. So each command that opens the
// store while the engine is stopped runs here against a Tier A that names both
// files apart, and after each one the configured file is there and the
// derived one is not.
func TestEveryStoreCommandOpensTheConfiguredReplicatedEstate(t *testing.T) {
	dir := t.TempDir()
	nodePath := filepath.Join(dir, "node", "crewlet.db")
	replicated := filepath.Join(dir, "elsewhere", "rep.db")
	stray := store.ReplicatedPath(nodePath, "")
	if stray == replicated {
		t.Fatalf("the derived path %s is the configured one, so nothing here could fail", stray)
	}
	// The directories exist so a stray file has somewhere to land: a
	// command that went wrong must be able to leave the evidence.
	for _, d := range []string{filepath.Dir(nodePath), filepath.Dir(replicated)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	key := make([]byte, 32)
	key[0] = 1
	cfg := filepath.Join(dir, "crewlet.yaml")
	body := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n  replicated_path: %s\n"+
		"secrets:\n  active_key_id: k1\n  keys:\n    - id: k1\n      material: %q\n",
		nodePath, replicated, base64.StdEncoding.EncodeToString(key))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}

	for _, step := range []struct {
		name string
		args []string
		// fails is whether the command exits non-zero on this store for a
		// reason of its own, after it opened it.
		fails bool
		// says is what its output must name, when anything.
		says string
	}{
		// Pending is the read that opens each estate apart, so it is the
		// one most able to name the wrong file — and it reports the path.
		{name: "migrate -check", args: []string{"migrate", "-config", cfg, "-check"},
			fails: true, says: replicated},
		{name: "migrate", args: []string{"migrate", "-config", cfg}, says: replicated},
		// openConfigStore, which `crewlet run` also boots through when it
		// is given no Tier B file.
		{name: "config revisions", args: []string{"config", "revisions", "-config", cfg}},
		{name: "secrets set", args: []string{"secrets", "set", "TOKEN", "-value", "sk-not-real",
			"-config", cfg}},
		{name: "secrets list", args: []string{"secrets", "list", "-config", cfg}, says: "TOKEN"},
		// An empty corpus fails the measurement, which is after the open.
		{name: "search eval", args: []string{"search", "eval", "-config", cfg}, fails: true},
	} {
		out, errs, err := cli(t, step.args...)
		if (err != nil) != step.fails {
			t.Fatalf("%s: err = %v, want failing %v (stdout %q, stderr %q)",
				step.name, err, step.fails, out, errs)
		}
		if step.says != "" && !strings.Contains(out, step.says) {
			t.Errorf("%s: the output does not name %s: %q", step.name, step.says, out)
		}
		if _, err := os.Stat(stray); err == nil {
			t.Fatalf("%s opened a replicated estate at %s, beside store.path, where Tier A "+
				"names %s", step.name, stray, replicated)
		}
		if step.name != "migrate -check" {
			if _, err := os.Stat(replicated); err != nil {
				t.Fatalf("%s: the configured replicated estate is not there: %v", step.name, err)
			}
		}
	}
}

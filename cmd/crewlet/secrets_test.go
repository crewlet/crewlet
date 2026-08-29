package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// bootstrapWithKeyring writes a Tier A config with a real keyring and a
// store path, and returns the config path.
func bootstrapWithKeyring(t *testing.T, keys ...string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	fmt.Fprintf(&b, "node:\n  id: cli-test\nstore:\n  path: %s\n",
		filepath.Join(dir, "index.db"))
	fmt.Fprintf(&b, "secrets:\n  active_key_id: %s\n  keys:\n", keys[0])
	for i, id := range keys {
		key := make([]byte, 32)
		key[0] = byte(i + 1)
		fmt.Fprintf(&b, "    - id: %s\n      material: %q\n",
			id, base64.StdEncoding.EncodeToString(key))
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	return path
}

// secretsCmd runs one `crewlet secrets` invocation.
func secretsCmd(t *testing.T, cfg string, args ...string) (string, string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	full := append([]string{"secrets"}, args...)
	full = append(full, "-config", cfg)
	err := run(full, &out, &errs)
	return out.String(), errs.String(), err
}

// A SECRET ROUND TRIPS THROUGH THE CLI, which is the whole surface: set,
// list, get, unset.
func TestASecretRoundTripsThroughTheCLI(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")

	if _, errs, err := secretsCmd(t, cfg, "set", "TOKEN", "-value", "sk-not-real"); err != nil {
		t.Fatalf("set: %v (%s)", err, errs)
	}
	out, errs, err := secretsCmd(t, cfg, "get", "TOKEN", "-reveal")
	if err != nil {
		t.Fatalf("get: %v (%s)", err, errs)
	}
	// NO TRAILING NEWLINE: this is piped into another command, where a
	// newline would be part of the token.
	if out != "sk-not-real" {
		t.Fatalf("get printed %q", out)
	}

	list, _, err := secretsCmd(t, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, "TOKEN") || !strings.Contains(list, "k1") {
		t.Fatalf("list = %q", list)
	}
	// A LISTING CARRIES NO VALUES. It is read to answer "is X set", and one
	// that printed plaintext would put a company's credentials in a
	// scrollback buffer.
	if strings.Contains(list, "sk-not-real") {
		t.Fatal("the listing printed a secret's value")
	}

	if out, _, err := secretsCmd(t, cfg, "unset", "TOKEN"); err != nil {
		t.Fatalf("unset: %v", err)
	} else if !strings.Contains(out, "removed") {
		t.Fatalf("unset said %q", out)
	}
	if _, _, err := secretsCmd(t, cfg, "get", "TOKEN", "-reveal"); err == nil {
		t.Fatal("a removed secret was still readable")
	}
}

// READ-BACK IS BREAK-GLASS. There is no HTTP route that returns a value, and
// this refuses without an explicit flag for the same reason: the common need
// is "is X set", which the listing answers without putting a credential into
// a terminal and a screen-share.
func TestGettingASecretNeedsTheRevealFlag(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "TOKEN", "-value", "sk-not-real"); err != nil {
		t.Fatalf("set: %v (%s)", err, errs)
	}
	out, _, err := secretsCmd(t, cfg, "get", "TOKEN")
	if err == nil {
		t.Fatalf("get printed %q without -reveal", out)
	}
	if strings.Contains(out, "sk-not-real") || strings.Contains(err.Error(), "sk-not-real") {
		t.Fatal("the refusal leaked the value it refused to print")
	}
	if !strings.Contains(err.Error(), "-reveal") {
		t.Errorf("the refusal %q does not say how to proceed", err)
	}
}

// AN EMPTY VALUE IS A VALUE — clearing a token without removing the row —
// so `-value ""` must be distinguishable from omitting the flag, which
// reads stdin.
func TestAnEmptyValueIsStoredRatherThanReadFromStdin(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, cfg, "set", "TOKEN", "-value", ""); err != nil {
		t.Fatalf("set: %v (%s)", err, errs)
	}
	out, _, err := secretsCmd(t, cfg, "get", "TOKEN", "-reveal")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out != "" {
		t.Fatalf("get printed %q, want the empty value that was set", out)
	}
}

// NO KEYRING IS AN ERROR THAT SAYS WHAT TO DO, not a store that silently
// stores plaintext.
func TestWithoutAKeyringTheCommandSaysHowToGetOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n",
		filepath.Join(dir, "index.db"))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	_, _, err := secretsCmd(t, path, "list")
	if err == nil {
		t.Fatal("a store with no keyring was opened")
	}
	if !strings.Contains(err.Error(), "keygen") {
		t.Errorf("error %q does not say how to get a keyring", err)
	}
}

// KEYGEN NEEDS NOTHING — no store, no keyring, not even a config. It is what
// an operator runs BEFORE any of this exists.
func TestKeygenEmitsAPasteableKey(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	if err := run([]string{"secrets", "keygen"}, &out, &errs); err != nil {
		t.Fatalf("keygen: %v (%s)", err, errs.String())
	}
	// THE KEY IS THE FIRST LINE, so `crewlet secrets keygen | head -1`
	// yields something pasteable; the snippet below it is guidance.
	first := strings.SplitN(out.String(), "\n", 2)[0]
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(first))
	if err != nil {
		t.Fatalf("keygen's first line %q is not base64: %v", first, err)
	}
	if len(raw) != 32 {
		t.Fatalf("keygen emitted %d bytes, want a 32-byte AES-256 key", len(raw))
	}

	// TWICE IS TWO KEYS. A generator that repeated itself would give every
	// deployment that ran it the same key.
	var again bytes.Buffer
	if err := run([]string{"secrets", "keygen"}, &again, &errs); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if strings.SplitN(again.String(), "\n", 2)[0] == first {
		t.Fatal("keygen emitted the same key twice")
	}
}

// A REKEY MOVES THE STALE ROWS AND NAMES THEM, which is the last chance to
// see what is now safe to retire the old key over.
func TestRekeyReportsWhatItMoved(t *testing.T) {
	first := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, first, "set", "TOKEN", "-value", "v"); err != nil {
		t.Fatalf("set: %v (%s)", err, errs)
	}

	// The same store, now under a keyring whose active key is k2 and which
	// still holds k1 — the shape of an online rotation.
	rotated := rekeyedConfig(t, first, "k2", "k1")
	out, errs, err := secretsCmd(t, rotated, "rekey")
	if err != nil {
		t.Fatalf("rekey: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "TOKEN") {
		t.Fatalf("rekey said %q, want it to name what moved", out)
	}

	// The value survives, and a second pass is a no-op — which is what
	// makes the command safe to re-run after a partial failure.
	if got, _, err := secretsCmd(t, rotated, "get", "TOKEN", "-reveal"); err != nil || got != "v" {
		t.Fatalf("after rekey get = %q, %v", got, err)
	}
	again, _, err := secretsCmd(t, rotated, "rekey")
	if err != nil {
		t.Fatalf("second rekey: %v", err)
	}
	if !strings.Contains(again, "already sealed") {
		t.Fatalf("the second rekey said %q, want a no-op", again)
	}
}

// rekeyedConfig rewrites a bootstrap to make another of its keys active,
// keeping the store path so the same rows are read.
func rekeyedConfig(t *testing.T, from, active string, others ...string) string {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatalf("read %s: %v", from, err)
	}
	storePath := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "path:") {
			storePath = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "path:"))
		}
	}
	if storePath == "" {
		t.Fatal("the source bootstrap named no store path")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "node:\n  id: cli-test\nstore:\n  path: %s\n", storePath)
	fmt.Fprintf(&b, "secrets:\n  active_key_id: %s\n  keys:\n", active)
	// k1 keeps the material bootstrapWithKeyring gave it (index 0), so what
	// it sealed still opens; the new key is fresh.
	for _, id := range append([]string{active}, others...) {
		key := make([]byte, 32)
		if id == "k1" {
			key[0] = 1
		} else {
			key[0] = 9
		}
		fmt.Fprintf(&b, "    - id: %s\n      material: %q\n",
			id, base64.StdEncoding.EncodeToString(key))
	}
	path := filepath.Join(filepath.Dir(from), "rotated.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write rotated bootstrap: %v", err)
	}
	return path
}

func TestTheSecretsSubcommandsAreChecked(t *testing.T) {
	t.Parallel()
	cfg := bootstrapWithKeyring(t, "k1")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"an unknown subcommand", []string{"nonesuch"}},
		{"set with no name", []string{"set", "-value", "v"}},
		{"get with no name", []string{"get"}},
		{"unset with no name", []string{"unset"}},
		{"two names at once", []string{"get", "A", "B"}},
	} {
		if _, _, err := secretsCmd(t, cfg, tc.args...); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// A PIPED SECRET LOSES ITS TRAILING NEWLINE and nothing else. `echo secret
// | crewlet secrets set X` is how this is used, and a token carrying a
// newline fails at the vendor with a 401 that names neither the newline nor
// this command. Stripping more would be wrong the other way: a secret may
// legitimately end in whitespace, and altering it silently is a failure
// nobody can see.
func TestAPipedSecretLosesExactlyOneTrailingNewline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"sk-token\n", "sk-token"},
		{"sk-token", "sk-token"},
		{"sk-token\n\n", "sk-token\n"},
		{"sk-token \n", "sk-token "},
		{"line-one\nline-two\n", "line-one\nline-two"},
		{"\n", ""},
		{"", ""},
	} {
		got, err := secretFromReader(strings.NewReader(tc.in))
		if err != nil {
			t.Fatalf("secretFromReader(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("secretFromReader(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// THE SNIPPET REFERENCES A VARIABLE rather than inlining the key, because
// config.yaml is the file people commit — pasting a raw key into it is the
// single most likely way it ends up in a repository.
func TestKeygenHandsOverAPasteableSnippetThatDoesNotInlineTheKey(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	if err := run([]string{"secrets", "keygen", "-key-id", "prod-2"}, &out, &errs); err != nil {
		t.Fatalf("keygen: %v (%s)", err, errs.String())
	}
	body := out.String()
	for _, want := range []string{
		"active_key_id: prod-2",
		"id: prod-2",
		`material: "${CREWLET_SECRET_PROD_2}"`,
		"export CREWLET_SECRET_PROD_2=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the snippet does not contain %q:\n%s", want, body)
		}
	}
	// The key appears on its own line and in the export, never inside the
	// YAML — which is the whole point of the reference form.
	key := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
	yaml := body[strings.Index(body, "secrets:"):strings.Index(body, "  export")]
	if strings.Contains(yaml, key) {
		t.Fatal("the generated key was inlined into the config snippet")
	}
}

func TestKeygenNeedsAKeyID(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	if err := run([]string{"secrets", "keygen", "-key-id", "  "}, &out, &errs); err == nil {
		t.Fatal("a blank key id was accepted")
	}
	if err := run([]string{"secrets", "keygen", "extra"}, &out, &errs); err == nil {
		t.Fatal("a positional argument was accepted")
	}
}

// A DRY RUN MUST NOT DECRYPT ANYTHING. It reports from the denormalised key
// id column, which is there for exactly this — a preview that opened every
// row would be a bigger exposure than the pass it previews.
func TestARekeyDryRunReportsWithoutWriting(t *testing.T) {
	first := bootstrapWithKeyring(t, "k1")
	if _, errs, err := secretsCmd(t, first, "set", "TOKEN", "-value", "v"); err != nil {
		t.Fatalf("set: %v (%s)", err, errs)
	}
	rotated := rekeyedConfig(t, first, "k2", "k1")

	out, _, err := secretsCmd(t, rotated, "rekey", "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "TOKEN") || !strings.Contains(out, "would be re-sealed") {
		t.Fatalf("dry run said %q", out)
	}
	if strings.Contains(out, "\nv\n") {
		t.Fatal("the dry run printed a value")
	}

	// NOTHING MOVED: a real rekey afterwards still has work to do.
	real, _, err := secretsCmd(t, rotated, "rekey")
	if err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if !strings.Contains(real, "TOKEN") {
		t.Fatal("the dry run wrote after all — the real pass found nothing to move")
	}
}

// PROVENANCE IS RECORDED, so "where did this credential come from" has an
// answer months later.
func TestTheProvenanceOfAWriteIsRecorded(t *testing.T) {
	cfg := bootstrapWithKeyring(t, "k1")
	t.Setenv("CREWLET_OPERATOR", "sam")
	if _, errs, err := secretsCmd(t, cfg, "set", "TOKEN",
		"-value", "v", "-source", "gitlab-provision"); err != nil {
		t.Fatalf("set: %v (%s)", err, errs)
	}
	list, _, err := secretsCmd(t, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, "sam") || !strings.Contains(list, "gitlab-provision") {
		t.Fatalf("listing = %q, want the operator and the source", list)
	}
}

// A LOCKED STORE IS ROUTED THROUGH THE RUNNING NODE, not refused.
//
// internal/store proves the exclusion itself, across a real process boundary
// — it has to, since a second PROCESS is the whole property and a Go test
// shares one. What this side owns is what the lock MEANS here: the engine is
// up, so the fleet's store is reachable through its API, and that is the only
// route that puts a value on every node.
func TestALockedStoreIsRoutedThroughTheRunningNode(t *testing.T) {
	t.Parallel()
	boot := bootWithAPI(t, "127.0.0.1", 8080, "ops-token")
	locked := fmt.Errorf("open store: %w: /var/lib/crewlet/index.db is held by "+
		"pid 41 on host-a since 2026-01-01T00:00:00Z", store.ErrLocked)

	target, err := throughTheRunningNode(boot, "crewlet.yaml", locked)
	if err != nil {
		t.Fatalf("a locked store was refused instead of routed: %v", err)
	}
	if !target.fleet {
		t.Fatal("the target is not marked fleet-wide, so `secrets set` would " +
			"tell an operator their rotation reached one node")
	}
	if !strings.Contains(target.where, "127.0.0.1:8080") {
		t.Errorf("where = %q, want the node it writes through", target.where)
	}
}

// AND WHEN THE NODE CANNOT BE REACHED, both facts are in the refusal.
//
// The lock alone reads as "you are blocked, with no way forward"; the API's
// own complaint alone reads as though the local store were never an option.
// An operator needs to know that the engine is holding the file AND why the
// route around it did not work.
func TestARoutedStoreThatCannotReachTheNodeSaysBothWhy(t *testing.T) {
	t.Parallel()
	// api.port 0 is a node that serves no HTTP at all, which is the one
	// shape where a running engine genuinely cannot be written through.
	boot := bootWithAPI(t, "127.0.0.1", 0, "ops-token")
	locked := fmt.Errorf("%w: held by pid 41", store.ErrLocked)

	_, err := throughTheRunningNode(boot, "crewlet.yaml", locked)
	if err == nil {
		t.Fatal("a node with no HTTP surface was accepted as a write target")
	}
	if !errors.Is(err, store.ErrLocked) {
		t.Fatalf("the sentinel was lost, so a caller can no longer tell this "+
			"from any other failure: %v", err)
	}
	for _, want := range []string{
		"pid 41",       // the original, still there to act on
		"crewlet.yaml", // which node
		"api.port",     // why the route around it failed
		"crewlet run",  // what is holding the file
		"environment",  // the way out that needs no downtime
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal omits %q, so an operator is told they are "+
				"blocked and not what to do: %v", want, err)
		}
	}
}

// EVERY OTHER FAILURE KEEPS ITS OWN MESSAGE AND IS NOT ROUTED. Telling an
// operator to reach the API when the real problem is a missing keyring or an
// unreadable path replaces an accurate message with a connection refused.
func TestOnlyALockedStoreIsRoutedThroughTheAPI(t *testing.T) {
	t.Parallel()
	boot := bootWithAPI(t, "127.0.0.1", 8080, "ops-token")
	other := errors.New("the keyring cannot open this store")

	target, err := throughTheRunningNode(boot, "crewlet.yaml", other)
	if target != nil {
		t.Fatal("an unrelated failure was answered with an API client, so a " +
			"broken node would report a connection error instead of its fault")
	}
	if !errors.Is(err, other) {
		t.Fatalf("the original error was replaced: %v", err)
	}
	if strings.Contains(err.Error(), "crewlet run") {
		t.Fatalf("an unrelated failure was answered with the lock's "+
			"remediation: %v", err)
	}
}

// EVERY SECRET-STORE OPENER ROUTES A LOCKED STORE.
//
// Derived from the source rather than driven through the CLI, because the
// case that would drive it needs a second OS PROCESS holding the file — which
// internal/store tests and a Go test here cannot. What can go wrong on this
// side is a path that opens the store and returns store.ErrLocked raw, and
// the symptom is a rotation an operator is told to stop the engine for when
// the engine is exactly what should have carried it.
func TestOpeningTheSecretStoreAlwaysRoutesALockedOne(t *testing.T) {
	t.Parallel()
	const file = "secrets.go"
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var checked bool
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "openSecretStore" {
			continue
		}
		checked = true
		var routes bool
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "throughTheRunningNode" {
				routes = true
			}
			return true
		})
		if !routes {
			t.Errorf("openSecretStore returns the store's error without "+
				"throughTheRunningNode at %s — a locked store would be "+
				"reported with no route to the fleet", fset.Position(fn.Pos()))
		}
	}
	if !checked {
		t.Fatal("openSecretStore is gone from secrets.go, so this guard is " +
			"asserting nothing — point it at whatever replaced it")
	}
}

// EVERY COMMAND THAT OPENS THE STORE ANSWERS A LOCKED ONE WITH ITS OWN WAY
// OUT.
//
// `secrets` routes through the API, `config import` points at PUT /config,
// and `migrate` has no route around it because a schema change under a live
// engine is precisely what the lock exists to prevent. Three different
// remedies, so a shared "the file is locked" message would be wrong for at
// least two of them — which is why this asserts the wrapper carries the
// caller's own text rather than a fixed one.
func TestALockedStoreIsAnsweredInEachCommandsOwnTerms(t *testing.T) {
	t.Parallel()
	locked := fmt.Errorf("%w: /var/lib/crewlet/index.db is held by pid 41",
		store.ErrLocked)

	got := engineHoldsTheStore(locked, "crewlet.yaml", "Do the specific thing.")
	if !errors.Is(got, store.ErrLocked) {
		t.Fatalf("the sentinel was lost: %v", got)
	}
	for _, want := range []string{
		"pid 41",                 // the original, still there to act on
		"crewlet.yaml",           // which node
		"Do the specific thing.", // the caller's own remedy
	} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("the refusal omits %q: %v", want, got)
		}
	}
}

// AND EVERY OTHER FAILURE KEEPS ITS OWN MESSAGE. Telling an operator the
// engine is holding the file when the real problem is an unreadable path
// sends them to fix something that is not broken.
func TestOnlyALockedStoreGetsARemediation(t *testing.T) {
	t.Parallel()
	other := errors.New("permission denied")
	got := engineHoldsTheStore(other, "crewlet.yaml", "Do the specific thing.")
	if !errors.Is(got, other) {
		t.Fatalf("the original error was replaced: %v", got)
	}
	if strings.Contains(got.Error(), "Do the specific thing.") {
		t.Fatalf("an unrelated failure was answered with the lock's "+
			"remediation: %v", got)
	}
}

// EVERY STORE OPENER IN THE CLI ROUTES OR EXPLAINS A LOCKED STORE.
//
// Derived from the source, because driving it needs a second OS PROCESS
// holding the file. The failure mode of a missed call site is silent and
// permanent: an operator is told "the database is open in another process"
// with no idea that they are the second process, on a command that had a
// perfectly good way forward.
func TestEveryStoreOpenerAnswersALockedStore(t *testing.T) {
	t.Parallel()
	// Each opener, and the function that must appear in it. `secrets.go`
	// routes to the API; the other two explain.
	openers := map[string]map[string]string{
		"secrets.go":    {"openSecretStore": "throughTheRunningNode"},
		"config_cmd.go": {"openConfigStore": "engineHoldsTheStore"},
		"ops.go":        {"runMigrate": "engineHoldsTheStore"},
	}
	for file, want := range openers {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for fnName, needs := range want {
			found, handles := false, false
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != fnName {
					continue
				}
				found = true
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == needs {
						handles = true
					}
					return true
				})
			}
			if !found {
				t.Errorf("%s is gone from %s, so this guard asserts nothing "+
					"about it — point it at whatever replaced it", fnName, file)
				continue
			}
			if !handles {
				t.Errorf("%s in %s opens the store without %s: a locked one "+
					"would be reported with no way forward", fnName, file, needs)
			}
		}
	}
}

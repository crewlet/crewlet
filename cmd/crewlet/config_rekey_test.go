package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// bootstrapWithKeys writes a Tier A naming a store and a keyring.
//
// The material is derived from the id rather than random, so a SECOND
// bootstrap listing the same ids in a different order — which is how a
// rotation is spelled — still opens what the first one sealed.
func bootstrapWithKeys(t *testing.T, dir string, keys ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "node:\n  id: cli-test\nstore:\n  path: %s\n",
		filepath.Join(dir, "index.db"))
	if len(keys) > 0 {
		fmt.Fprintf(&b, "secrets:\n  active_key_id: %s\n  keys:\n", keys[0])
		for _, id := range keys {
			material := make([]byte, 32)
			copy(material, id)
			fmt.Fprintf(&b, "    - id: %s\n      material: %q\n",
				id, base64.StdEncoding.EncodeToString(material))
		}
	}
	path := filepath.Join(dir, "config-"+strings.Join(keys, "-")+".yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	return path
}

// A KEYRING ROTATION HAS TWO HALVES and this is the one that was missing.
//
// `crewlet secrets rekey` moves the secret store. The company document is
// sealed with the SAME keyring and may carry literals of its own, and nothing
// moved it — so an operator who followed the documented rotation, saw the
// secrets half report success and dropped the retired key had just made their
// configuration unreadable on every node, at the next apply.
func TestConfigRekeyMovesTheDocumentOntoTheActiveKey(t *testing.T) {
	dir := t.TempDir()
	old := bootstrapWithKeys(t, dir, "k1")
	company := companyFile(t, dir, "company.yaml", nil)
	if _, errs, err := configCmd(t, old, "import", company); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	if got, _ := activeKeyOf(t, old); got != "k1" {
		t.Fatalf("imported under %q, want k1", got)
	}

	// The rotation: k2 active, k1 retained so the old document still opens.
	rotated := bootstrapWithKeys(t, dir, "k2", "k1")
	out, errs, err := configCmd(t, rotated, "rekey")
	if err != nil {
		t.Fatalf("rekey: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "k2") || !strings.Contains(out, "k1") {
		t.Errorf("the output does not say what moved where: %q", out)
	}
	if got, _ := activeKeyOf(t, rotated); got != "k2" {
		t.Fatalf("after the rekey the document is under %q, want k2", got)
	}
	// AND IT STILL OPENS — the point of the whole exercise. Read through
	// a keyring holding ONLY the new key, which is the state the operator
	// reaches after dropping the retired one.
	only := bootstrapWithKeys(t, dir, "k2")
	if _, errs, err = configCmd(t, only, "show"); err != nil {
		t.Fatalf("after dropping k1 the config no longer reads: %v (%s)", err, errs)
	}
}

// A SECOND RUN IS A NO-OP, which is what makes this safe in a deploy script.
func TestConfigRekeyIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapWithKeys(t, dir, "k1")
	if _, errs, err := configCmd(t, cfg, "import",
		companyFile(t, dir, "company.yaml", nil)); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	before := revisionCount(t, cfg)

	out, errs, err := configCmd(t, cfg, "rekey")
	if err != nil {
		t.Fatalf("rekey: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "already sealed") {
		t.Errorf("output = %q, want it to say nothing needed moving", out)
	}
	if after := revisionCount(t, cfg); after != before {
		t.Errorf("a no-op rekey wrote %d revisions", after-before)
	}
}

// A DRY RUN REPORTS WITHOUT WRITING, and reads the denormalised key id rather
// than decrypting — so previewing a rotation is not a bigger exposure than
// the pass it previews.
func TestConfigRekeyDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	old := bootstrapWithKeys(t, dir, "k1")
	if _, errs, err := configCmd(t, old, "import",
		companyFile(t, dir, "company.yaml", nil)); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	rotated := bootstrapWithKeys(t, dir, "k2", "k1")
	before := revisionCount(t, rotated)

	out, errs, err := configCmd(t, rotated, "rekey", "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "would be re-sealed") {
		t.Errorf("output = %q", out)
	}
	if after := revisionCount(t, rotated); after != before {
		t.Error("a dry run wrote a revision")
	}
	if got, _ := activeKeyOf(t, rotated); got != "k1" {
		t.Errorf("a dry run moved the document to %q", got)
	}
}

// plantPlaintextRevision stores the fixture company's settings as an UNSEALED
// active revision, directly in the store dir holds.
//
// No command writes one any more — every node holds a keyring and every write
// path seals — so the only plaintext revision a store can carry is one an
// older build left there, and a test reaches that state the way it arose: by
// a write that did not seal.
func plantPlaintextRevision(t *testing.T, dir string) {
	t.Helper()
	company, err := config.LoadCompany(companyFile(t, dir, "company.yaml", nil))
	if err != nil {
		t.Fatalf("load the fixture company: %v", err)
	}
	document, err := json.Marshal(config.SettingsOf(company).Company())
	if err != nil {
		t.Fatalf("encode the fixture company: %v", err)
	}
	db, err := store.Open(t.Context(), filepath.Join(dir, "index.db"), store.Options{})
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Configs().InsertActive(t.Context(), store.Revision{
		Source: "file", CreatedBy: "an-older-build", CreatedByKind: "system",
		Summary: "written before the keyring was required", Payload: document,
	}); err != nil {
		t.Fatalf("plant the plaintext revision: %v", err)
	}
	if secrets.Sealed(document) {
		t.Fatal("the planted revision is sealed, so no case below is about plaintext")
	}
}

// A PLAINTEXT DOCUMENT IS NOT SILENTLY SEALED BY A REKEY.
//
// "Rotate the key this is under" and "start encrypting this at all" are
// different decisions, and an operator running a rotation script has not
// asked for the second.
func TestConfigRekeyRefusesAPlaintextRevisionAndNamesTheFix(t *testing.T) {
	dir := t.TempDir()
	plantPlaintextRevision(t, dir)
	keyed := bootstrapWithKeys(t, dir, "k1")

	_, _, err := configCmd(t, keyed, "rekey")
	if err == nil {
		t.Fatal("a plaintext revision was silently sealed by a rekey")
	}
	if !strings.Contains(err.Error(), "config seal") {
		t.Errorf("the refusal does not name the command that does it: %v", err)
	}
}

// SEAL IS THE ONE-TIME MIGRATION off plaintext-at-rest: a store written before
// the keyring was required holds a plaintext revision, and nothing else
// re-seals it — `import` only seals what it imports.
func TestConfigSealEncryptsAPlaintextRevision(t *testing.T) {
	dir := t.TempDir()
	plantPlaintextRevision(t, dir)
	keyed := bootstrapWithKeys(t, dir, "k1")
	if _, sealed := activeKeyOf(t, keyed); sealed {
		t.Fatal("the planted revision reads as sealed")
	}

	if _, errs, err := configCmd(t, keyed, "seal"); err != nil {
		t.Fatalf("seal: %v (%s)", err, errs)
	}
	got, sealed := activeKeyOf(t, keyed)
	if !sealed || got != "k1" {
		t.Fatalf("after seal the revision is under %q (sealed=%t), want k1", got, sealed)
	}
	// AND IT STILL READS.
	if _, errs, err := configCmd(t, keyed, "show"); err != nil {
		t.Fatalf("the sealed revision no longer reads: %v (%s)", err, errs)
	}
}

// SEALING TWICE IS A NO-OP rather than an envelope inside an envelope.
func TestConfigSealIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	keyed := bootstrapWithKeys(t, dir, "k1")
	if _, errs, err := configCmd(t, keyed, "import",
		companyFile(t, dir, "company.yaml", nil)); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	before := revisionCount(t, keyed)

	out, errs, err := configCmd(t, keyed, "seal")
	if err != nil {
		t.Fatalf("seal: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "already sealed") {
		t.Errorf("output = %q", out)
	}
	if after := revisionCount(t, keyed); after != before {
		t.Error("a no-op seal wrote a revision")
	}
}

// WITHOUT A KEYRING EVERY CONFIG COMMAND SAYS HOW TO GET ONE, rather than
// failing on a nil cipher somewhere further in — `import` included, which
// used to store a plaintext revision on a node with no keyring and is now
// refused like the rest, because every node holds one.
func TestEveryConfigCommandWithoutAKeyringNamesTheRemedy(t *testing.T) {
	dir := t.TempDir()
	plain := bootstrapWithKeys(t, dir)
	for _, args := range [][]string{
		{"import", companyFile(t, dir, "company.yaml", nil)},
		{"show"}, {"seal"}, {"rekey"},
	} {
		_, _, err := configCmd(t, plain, args...)
		if err == nil {
			t.Fatalf("%s ran with no keyring", args[0])
		}
		for _, says := range []string{"secrets.keys", "keygen"} {
			if !strings.Contains(err.Error(), says) {
				t.Errorf("%s: the refusal does not say %q: %v", args[0], says, err)
			}
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "index.db")); statErr == nil {
		t.Error("a command refused for its keyring still created the store")
	}
}

// activeKeyOf reads which key sealed the active revision, via `config
// revisions` plus the export path — the only reader a test has that does not
// reach into the store directly.
func activeKeyOf(t *testing.T, cfg string) (string, bool) {
	t.Helper()
	ctx := t.Context()
	cs, closeStore, err := openConfigStore(ctx, cfg)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer closeStore()
	rev, err := revisionOrActive(ctx, cs, "")
	if err != nil {
		t.Fatalf("active revision: %v", err)
	}
	return secrets.EnvelopeKeyIDOf(rev.Payload)
}

func revisionCount(t *testing.T, cfg string) int {
	t.Helper()
	var out bytes.Buffer
	if err := run([]string{"config", "revisions", "-config", cfg}, &out, &out); err != nil {
		t.Fatalf("revisions: %v (%s)", err, out.String())
	}
	return strings.Count(out.String(), "\n")
}

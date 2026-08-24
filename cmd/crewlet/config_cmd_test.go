package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliCompanyDoc = `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
roles:
  - name: CEO
    handle: ceo
    llm: main
`

// companyFile writes a company document, with an optional amendment.
func companyFile(t *testing.T, dir, name string, amend func(string) string) string {
	t.Helper()
	doc := cliCompanyDoc
	if amend != nil {
		doc = amend(doc)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// bootstrapForStore writes a Tier A config naming a store in dir.
func bootstrapForStore(t *testing.T, dir string) string {
	t.Helper()
	body := fmt.Sprintf("node:\n  id: cli-test\nstore:\n  path: %s\n",
		filepath.Join(dir, "index.db"))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	return path
}

func configCmd(t *testing.T, cfg string, args ...string) (string, string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	full := append([]string{"config"}, args...)
	full = append(full, "-config", cfg)
	err := run(full, &out, &errs)
	return out.String(), errs.String(), err
}

// AN IMPORT IS IDEMPOTENT BY CONTENT, the same rule the boot seed follows:
// an unchanged file writes nothing, an edited one writes once. Silently
// ignoring an edited file would be the worst of the three — an operator
// changes a config, runs the command, and nothing happens.
func TestImportingIsIdempotentByContent(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	company := companyFile(t, dir, "company.yaml", nil)

	first, errs, err := configCmd(t, cfg, "import", company)
	if err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	if !strings.Contains(first, "imported") {
		t.Fatalf("import said %q", first)
	}

	again, _, err := configCmd(t, cfg, "import", company)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !strings.Contains(again, "nothing imported") {
		t.Fatalf("the second import said %q, want a no-op", again)
	}

	// An EDIT writes once more, and the new revision becomes active.
	edited := companyFile(t, dir, "company.yaml", func(doc string) string {
		return strings.Replace(doc, "name: Nimbus", "name: Nimbus Two", 1)
	})
	if out, _, err := configCmd(t, cfg, "import", edited); err != nil {
		t.Fatalf("edited import: %v", err)
	} else if !strings.Contains(out, "imported") {
		t.Fatalf("the edited import said %q", out)
	}
	shown, _, err := configCmd(t, cfg, "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(shown, "Nimbus Two") {
		t.Fatalf("show printed the old revision: %q", shown)
	}
}

// A BROKEN DOCUMENT IS REFUSED BEFORE IT IS STORED. A revision that cannot
// be built is one every node in the fleet refuses, one after another, each
// reporting its own failure — a fleet-wide incident from a typo that could
// have been caught here.
func TestABrokenDocumentIsNeverStored(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	broken := companyFile(t, dir, "broken.yaml", func(doc string) string {
		// A seat whose named provider does not exist.
		return strings.Replace(doc, "llm: main", "llm: nonesuch", 1)
	})
	if _, _, err := configCmd(t, cfg, "import", broken); err == nil {
		t.Fatal("a config naming a missing provider was imported")
	}
	if out, _, err := configCmd(t, cfg, "revisions"); err != nil {
		t.Fatalf("revisions: %v", err)
	} else if !strings.Contains(out, "no revisions") {
		t.Fatalf("the refused import left something behind: %q", out)
	}
}

// THE ACTIVE REVISION IS MARKED, because "which is running" is the question
// this list is opened to answer and an id alone cannot answer it.
func TestTheRevisionListingMarksTheActiveOne(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	for i, name := range []string{"Nimbus One", "Nimbus Two"} {
		path := companyFile(t, dir, fmt.Sprintf("c%d.yaml", i), func(doc string) string {
			return strings.Replace(doc, "name: Nimbus", "name: "+name, 1)
		})
		if _, _, err := configCmd(t, cfg, "import", path); err != nil {
			t.Fatalf("import %s: %v", name, err)
		}
	}
	out, _, err := configCmd(t, cfg, "revisions")
	if err != nil {
		t.Fatalf("revisions: %v", err)
	}
	stars := strings.Count(out, "*")
	if stars != 1 {
		t.Fatalf("%d revisions are marked active:\n%s", stars, out)
	}
}

// A DIFF IS ALWAYS REDACTED, with no flag to turn it off: a diff is what an
// operator pastes into a ticket to ask whether a change looks right, and
// that is the single most likely way a credential leaves the machine.
func TestADiffIsRedactedAndNamesWhatMoved(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	first := companyFile(t, dir, "one.yaml", nil)
	if _, _, err := configCmd(t, cfg, "import", first); err != nil {
		t.Fatalf("import: %v", err)
	}
	firstID := activeRevisionID(t, cfg)

	// A CHANGE IN THE MIDDLE of the document, so the elision has something
	// to elide on both sides — the company name is the first line, where a
	// common prefix cannot exist.
	second := companyFile(t, dir, "two.yaml", func(doc string) string {
		return strings.Replace(doc, "model: claude-sonnet-5", "model: claude-opus-5", 1)
	})
	if _, _, err := configCmd(t, cfg, "import", second); err != nil {
		t.Fatalf("second import: %v", err)
	}

	out, _, err := configCmd(t, cfg, "diff", firstID)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(out, "claude-opus-5") || !strings.Contains(out, "claude-sonnet-5") {
		t.Fatalf("the diff does not show the change:\n%s", out)
	}
	// THE UNCHANGED BULK IS ELIDED ON BOTH SIDES, which is what an
	// operator opened a diff to avoid. Both markers, because the change is
	// one line in the middle of a long document: asserting only one lets
	// half the elision be dropped while the test still passes.
	for _, marker := range []string{"identical line(s) above", "identical line(s) below"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("the diff is missing %q, so it printed the "+
				"unchanged bulk:\n%s", marker, out)
		}
	}
	// AND THE CHANGE ITSELF IS SMALL. A diff that elides nothing and one
	// that elides everything both "contain" a marker.
	if changed := strings.Count(out, "\n-") + strings.Count(out, "\n+"); changed > 6 {
		t.Fatalf("the diff shows %d changed lines for a one-line edit:\n%s",
			changed, out)
	}
}

// IDENTICAL REVISIONS SAY SO rather than printing an empty diff, which
// reads as a broken command.
func TestDiffingARevisionAgainstItselfSaysSo(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	if _, _, err := configCmd(t, cfg, "import", companyFile(t, dir, "one.yaml", nil)); err != nil {
		t.Fatalf("import: %v", err)
	}
	out, _, err := configCmd(t, cfg, "diff", activeRevisionID(t, cfg))
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(out, "identical") {
		t.Fatalf("diff said %q", out)
	}
}

// RE-ACTIVATING THE CURRENT REVISION IS NOT A NO-OP: the pointer is
// append-only, so it mints a new epoch every node is watching — which is how
// a rotated secret reaches a running fleet without a restart.
func TestReactivatingTheCurrentRevisionMintsANewEpoch(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	if _, _, err := configCmd(t, cfg, "import", companyFile(t, dir, "one.yaml", nil)); err != nil {
		t.Fatalf("import: %v", err)
	}
	id := activeRevisionID(t, cfg)

	first, _, err := configCmd(t, cfg, "activate", id)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	second, _, err := configCmd(t, cfg, "activate", id)
	if err != nil {
		t.Fatalf("second activate: %v", err)
	}
	if first == second {
		t.Fatalf("re-activating produced the same epoch twice: %q", first)
	}
}

func TestActivatingAnUnknownRevisionIsRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	if _, _, err := configCmd(t, cfg, "activate", "no-such-revision"); err == nil {
		t.Fatal("an unknown revision was activated")
	}
}

// activeRevisionID reads the marked row out of the listing.
func activeRevisionID(t *testing.T, cfg string) string {
	t.Helper()
	out, _, err := configCmd(t, cfg, "revisions")
	if err != nil {
		t.Fatalf("revisions: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("cannot read a revision id out of %q", line)
		}
		return fields[1]
	}
	t.Fatalf("no active revision in:\n%s", out)
	return ""
}

func TestTheConfigSubcommandsAreChecked(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"an unknown subcommand", []string{"nonesuch"}},
		{"import with no file", []string{"import"}},
		{"diff with no revision", []string{"diff"}},
		{"activate with no revision", []string{"activate"}},
		{"two arguments", []string{"diff", "a", "b"}},
		{"export with nothing active", []string{"export"}},
		{"a non-positive limit", []string{"revisions", "-limit", "0"}},
	} {
		if _, _, err := configCmd(t, cfg, tc.args...); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

// A LITERAL CREDENTIAL IS MASKED in show and diff. The `${VAR}` form above
// is deliberately left visible — it names a credential rather than being one
// — so proving redaction needs a config that inlines the real thing, which
// is what an operator who has not adopted the secret store has.
func TestALiteralCredentialIsMaskedEverywhereItIsPrinted(t *testing.T) {
	dir := t.TempDir()
	cfg := bootstrapForStore(t, dir)
	literal := companyFile(t, dir, "literal.yaml", func(doc string) string {
		return strings.Replace(doc, `["${ANTHROPIC_API_KEY}"]`,
			`["sk-ant-a-real-looking-literal"]`, 1)
	})
	if _, errs, err := configCmd(t, cfg, "import", literal); err != nil {
		t.Fatalf("import: %v (%s)", err, errs)
	}
	firstID := activeRevisionID(t, cfg)

	// show is redacted...
	shown, _, err := configCmd(t, cfg, "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if strings.Contains(shown, "sk-ant-a-real-looking-literal") {
		t.Errorf("show printed the literal credential:\n%s", shown)
	}

	// ...and so is a diff, on BOTH sides, with no flag to turn it off.
	changed := companyFile(t, dir, "changed.yaml", func(doc string) string {
		doc = strings.Replace(doc, `["${ANTHROPIC_API_KEY}"]`,
			`["sk-ant-a-second-literal"]`, 1)
		return strings.Replace(doc, "name: Nimbus", "name: Nimbus Two", 1)
	})
	if _, _, err := configCmd(t, cfg, "import", changed); err != nil {
		t.Fatalf("second import: %v", err)
	}
	diff, _, err := configCmd(t, cfg, "diff", firstID)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, leaked := range []string{"sk-ant-a-real-looking-literal", "sk-ant-a-second-literal"} {
		if strings.Contains(diff, leaked) {
			t.Errorf("the diff leaked %s:\n%s", leaked, diff)
		}
	}

	// EXPORT WITHOUT -redact IS THE DELIBERATE ACT that yields the real
	// values — otherwise there would be no way to get a config back out.
	full, _, err := configCmd(t, cfg, "export", "-revision", firstID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(full, "sk-ant-a-real-looking-literal") {
		t.Fatalf("an unredacted export did not carry the credential:\n%s", full)
	}
}

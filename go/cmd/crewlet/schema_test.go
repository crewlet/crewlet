package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// THE CHECKED-IN SCHEMAS ARE REGENERATED AND COMPARED, which is the whole
// reason they can be trusted.
//
// They are a public, editor-facing artifact — the `# yaml-language-server:
// $schema=` modeline at the top of every shipped config resolves to them —
// and nothing at runtime reads them, so a stale one fails silently and
// permanently: an author gets no red underline for a field that does not
// exist, and one for a field that does.
func TestTheCheckedInSchemasAreWhatTheGeneratorEmits(t *testing.T) {
	t.Parallel()
	for _, tier := range []string{"bootstrap", "company"} {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()
			var out, errs bytes.Buffer
			if err := run([]string{"schema", tier}, &out, &errs); err != nil {
				t.Fatalf("schema %s: %v (%s)", tier, err, errs.String())
			}
			path := filepath.Join("..", "..", "schema", tier+".schema.json")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("%s is stale.\n\nRegenerate it:\n"+
					"  go run ./cmd/crewlet schema %s -o schema/%s.schema.json\n",
					path, tier, tier)
			}
		})
	}
}

// The tier is positional and the flag order must not matter: Go's flag
// package stops at the first non-flag argument, so parsing before peeling
// the tier off silently writes to stdout on the natural spelling.
func TestBothArgumentOrdersWriteTheFile(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"schema", "company", "-o", ""},
		{"schema", "-o", "", "company"},
	} {
		path := filepath.Join(t.TempDir(), "out.json")
		args[slicesIndex(args, "")] = path
		var out, errs bytes.Buffer
		if err := run(args, &out, &errs); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, errs.String())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v wrote nothing: %v", args, err)
		}
		if len(body) == 0 || body[0] != '{' {
			t.Fatalf("%v wrote %d bytes that are not a JSON document", args, len(body))
		}
	}
}

func slicesIndex(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func TestAnUnknownTierIsRefused(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	if err := run([]string{"schema", "nonesuch"}, &out, &errs); err == nil {
		t.Fatal("an unknown tier was accepted")
	}
	if err := run([]string{"schema", "company", "bootstrap"}, &out, &errs); err == nil {
		t.Fatal("two tiers at once were accepted")
	}
}

// NAMING NO TIER IS THE COMPANY TIER, because it is the one an author
// edits — Tier A is a handful of URLs written once.
func TestNoTierIsTheCompanyTier(t *testing.T) {
	t.Parallel()
	var bare, named, errs bytes.Buffer
	if err := run([]string{"schema"}, &bare, &errs); err != nil {
		t.Fatalf("bare schema: %v (%s)", err, errs.String())
	}
	if err := run([]string{"schema", "company"}, &named, &errs); err != nil {
		t.Fatalf("schema company: %v (%s)", err, errs.String())
	}
	if !bytes.Equal(bare.Bytes(), named.Bytes()) {
		t.Fatal("the bare form emitted something other than the company tier")
	}
}

package config

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// testLogger writes structured lines into a buffer so a test can assert
// what a warning DOES and DOES NOT carry — the shadowed-secret warning's
// whole contract is that it names variables and never their values.
func testLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// mustCompany parses a Tier B document that the test expects to be valid.
func mustCompany(t *testing.T, doc string) *Company {
	t.Helper()
	cfg, err := ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("expected a valid company config, got:\n%v", err)
	}
	return cfg
}

// rejects asserts that a document fails validation and that the message
// points at the field path an operator can search their file for. The path
// is the assertion that matters: "invalid value" with no location is the
// error this package exists to avoid producing.
func rejects(t *testing.T, doc, wantPath string) error {
	t.Helper()
	_, err := ParseCompany([]byte(doc))
	if err == nil {
		t.Fatalf("expected %s to be rejected", wantPath)
	}
	if !strings.Contains(err.Error(), wantPath) {
		t.Fatalf("error must name %q; got:\n%v", wantPath, err)
	}
	return err
}

// rejectsBootstrap is [rejects] for Tier A.
func rejectsBootstrap(t *testing.T, doc, wantPath string) error {
	t.Helper()
	_, err := ParseBootstrap([]byte(doc), EnvOnly())
	if err == nil {
		t.Fatalf("expected %s to be rejected", wantPath)
	}
	if !strings.Contains(err.Error(), wantPath) {
		t.Fatalf("error must name %q; got:\n%v", wantPath, err)
	}
	return err
}

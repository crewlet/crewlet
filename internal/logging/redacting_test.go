package logging

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// token is a credential in a shape internal/redact knows: a GitHub token.
var token = "ghp_" + strings.Repeat("b", 36)

// EVERY LOG LINE IS REDACTED, the promise the log file's mode is chosen
// beside (file.go, [FileMode]): a credential in the message, a string
// attribute, an error, a bound attribute or a group nested inside a group
// reaches neither the console nor the file, and no record is dropped for
// carrying one.
//
// Mutation: install the destination handler unwrapped (drop redacting in
// install), or stop recursing into groups in redactAttr, and the token is
// written.
func TestEveryLogLineIsRedacted(t *testing.T) {
	t.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	var console, file bytes.Buffer
	install(settings{level: slog.LevelInfo, format: FormatJSON, console: &console,
		file: FileSink{Writer: &file, Format: FormatText}})

	log := Get("redaction").With("bound", "cloned "+token)
	log.Info("clone_failed "+token,
		"url", "https://x-access-token:"+token+"@example.com/repo.git",
		"error", errors.New("remote said: bad credentials "+token),
		slog.Group("outer", slog.Group("inner", slog.String("header", "Authorization: "+token))))
	log.Info("clean_line", "seat", "eng.alice")

	for name, out := range map[string]string{"console": console.String(), "file": file.String()} {
		if strings.Contains(out, token) || strings.Contains(out, strings.Repeat("b", 36)) {
			t.Errorf("the %s carries the token:\n%s", name, out)
		}
		if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != 2 {
			t.Errorf("the %s holds %d lines, want both records:\n%s", name, lines, out)
		}
		for _, kept := range []string{"clone_failed", "outer", "inner", "header", "clean_line",
			"eng.alice", "example.com"} {
			if !strings.Contains(out, kept) {
				t.Errorf("the %s lost %q, which carried no credential:\n%s", name, kept, out)
			}
		}
	}
}

// A CLEAN RECORD IS HANDED ON AS IT WAS: redaction rewrites what it must and
// nothing else, so a number stays a number for the shipper parsing it.
func TestARedactedRecordKeepsItsValuesKinds(t *testing.T) {
	t.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	var out bytes.Buffer
	install(settings{level: slog.LevelInfo, format: FormatJSON, console: &out})
	Get("redaction").Info("counted", "epoch", 7, "ok", true)
	for _, want := range []string{`"epoch":7`, `"ok":true`, `"msg":"counted"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the line lost %s: %s", want, out.String())
		}
	}
}

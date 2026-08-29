package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// THE FILE'S LOGGING SETTINGS REACH THE PROCESS.
//
// There was a `debug: true` boolean here that nothing in the tree ever read:
// the quickstart told an operator to write it and the deployment guide said
// it "raises the log level to DEBUG", and for the life of the field it
// changed nothing. A boolean nobody consults looks identical to a boolean
// that works, which is why this is asserted rather than left to the CLI
// wiring that consumes it. The boolean is now gone — `logging.level` says
// everything it said — but the assertion it was missing is not.
func TestLogSettingsPrecedence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		yaml       string
		wantLevel  slog.Level
		wantFormat logging.Format
	}{
		{
			"nothing said", "",
			slog.LevelInfo, logging.FormatConsole,
		},
		{
			"debug", "logging:\n  level: debug\n",
			slog.LevelDebug, logging.FormatConsole,
		},
		{
			"an explicit level", "logging:\n  level: warn\n",
			slog.LevelWarn, logging.FormatConsole,
		},
		{
			"an explicit format", "logging:\n  format: json\n",
			slog.LevelInfo, logging.FormatJSON,
		},
		{
			"both", "logging:\n  level: error\n  format: text\n",
			slog.LevelError, logging.FormatText,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot, err := ParseBootstrap([]byte(tc.yaml), EnvOnly())
			if err != nil {
				t.Fatalf("expected a valid Tier A document, got: %v", err)
			}
			level, format := boot.LogSettings()
			if level != tc.wantLevel {
				t.Errorf("level = %v, want %v", level, tc.wantLevel)
			}
			if format != tc.wantFormat {
				t.Errorf("format = %v, want %v", format, tc.wantFormat)
			}
		})
	}
}

// A TYPO IN THE FILE IS REFUSED, unlike the same typo in the `-log-level`
// flag, which resolves to info so that a bad level can never be why a company
// will not boot. The asymmetry is the point: a flag is typed by someone
// watching the process start, and a file is written once and deployed for
// months, so `level: dbug` silently running at info is the same failure this
// whole change exists to remove.
func TestLoggingValidatorRejections(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, yaml, path string
	}{
		{"unknown level", "logging:\n  level: dbug\n", "logging.level"},
		// "warning" is accepted by the FLAG parser and deliberately not by
		// the file: two spellings of one level in an editor's completion
		// list is a choice nobody benefits from making.
		{"the warning alias", "logging:\n  level: warning\n", "logging.level"},
		{"unknown format", "logging:\n  format: pretty\n", "logging.format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.yaml, tc.path)
			if !errors.Is(err, ErrUnknownValue) {
				t.Fatalf("want %v, got %v", ErrUnknownValue, err)
			}
		})
	}
}

// EVERY LEVEL AND FORMAT THE FILE ACCEPTS IS ONE THE ENGINE CAN INSTALL.
// The validator and the schema both read logging.Levels / logging.Formats,
// so a value added to either set without a handler behind it would pass
// validation and then log in a shape nothing produces.
func TestEveryDeclaredLevelAndFormatIsUsable(t *testing.T) {
	t.Parallel()
	for _, level := range logging.Levels {
		boot := DefaultBootstrap()
		boot.Logging.Level = level
		if err := boot.Validate(); err != nil {
			t.Errorf("level %q is in the closed set but refused: %v", level, err)
		}
		if got, _ := boot.LogSettings(); got != level.Slog() {
			t.Errorf("level %q resolved to %v", level, got)
		}
	}
	for _, format := range logging.Formats {
		boot := DefaultBootstrap()
		boot.Logging.Format = format
		if err := boot.Validate(); err != nil {
			t.Errorf("format %q is in the closed set but refused: %v", format, err)
		}
		if _, got := boot.LogSettings(); got != format {
			t.Errorf("format %q resolved to %v", format, got)
		}
	}
}

// THE RETIRED `debug:` KEY EXPLAINS ITSELF. The loader refuses anything it
// does not define, which is right — a misspelled setting that decoded to
// nothing is how a company boots with half its config silently absent. But
// this project's own quickstart and example told operators to write `debug:`,
// so reporting it as a spelling mistake sends them looking for a typo that
// is not there. It names the line that replaces it instead.
func TestTheRetiredDebugKeyNamesItsReplacement(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{"debug: true\n", "debug: false\n"} {
		err := rejectsBootstrap(t, doc, "logging:")
		if !errors.Is(err, ErrUnknownField) {
			t.Errorf("%q: want %v, got %v", doc, ErrUnknownField, err)
		}
		// The generic message would send them hunting for a typo.
		if strings.Contains(err.Error(), "check the spelling") {
			t.Errorf("%q was reported as a misspelling: %v", doc, err)
		}
		if !strings.Contains(err.Error(), "level: debug") {
			t.Errorf("%q: the error does not say what to write: %v", doc, err)
		}
	}
}

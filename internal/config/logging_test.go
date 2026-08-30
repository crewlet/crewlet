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

// AND THE HINT DOES NOT LEAK INTO THE OTHER TIER. `debug` was retired from
// Tier A; a company document never had one. Both tiers and every nested
// sub-document share one decode-error translation, so an ungated table would
// answer a `debug:` in company.yaml with advice about a `logging:` block that
// does not exist there — sending its author to edit a file they are not in.
func TestTheRetiredKeyHintIsScopedToItsTier(t *testing.T) {
	t.Parallel()
	err := rejects(t, "name: Acme\ndebug: true\n", "line 2")
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("want %v, got %v", ErrUnknownField, err)
	}
	if strings.Contains(err.Error(), "logging:") {
		t.Errorf("a company document was given Tier A's advice: %v", err)
	}
	if !strings.Contains(err.Error(), "check the spelling") {
		t.Errorf("an ordinary unknown key must read as one: %v", err)
	}
}

// THE RETIRED STORE DRIVER NAMES ITS REPLACEMENT TOO, and there is no
// replacement to name — which is exactly why the message has to exist.
//
// `store.driver` shipped in the quickstart, in examples/nimbus.config.yaml and
// in the deployment guide, so an operator upgrading has it written down. The
// generic "check the spelling" would send them looking for a typo in a key
// this project told them to write, and the only true answer — Turso is the
// database now, delete the line — is one nothing else says.
//
// Both values are covered: `sqlite` is the one that used to select the driver
// that no longer exists, and `turso` is the one that is still correct as a
// VALUE and still has to be refused as a KEY, because a field nothing reads is
// how a config comes to mean something it does not.
func TestTheRetiredStoreDriverKeyNamesItsReplacement(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		"store:\n  driver: sqlite\n",
		"store:\n  driver: turso\n",
	} {
		err := rejectsBootstrap(t, doc, "Turso is the database")
		if !errors.Is(err, ErrUnknownField) {
			t.Errorf("%q: want %v, got %v", doc, ErrUnknownField, err)
		}
		if strings.Contains(err.Error(), "check the spelling") {
			t.Errorf("%q was reported as a misspelling: %v", doc, err)
		}
		if !strings.Contains(err.Error(), "Delete the line") {
			t.Errorf("%q: the error does not say what to do: %v", doc, err)
		}
	}
}

// AND IT IS SCOPED TO ITS BLOCK, not to the word.
//
// The retired table is keyed on `Store.driver` rather than on `driver`, for
// the same reason the retired keys are tracked per TIER one level up: `driver` under
// `store:` was the storage engine and is retired, while a `driver:` typed
// under `stream:` never existed there at all. An ungated table would answer
// the second with advice about the first, sending its author to edit a block
// they are not in.
func TestTheRetiredDriverHintIsScopedToItsBlock(t *testing.T) {
	t.Parallel()
	err := rejectsBootstrap(t, "stream:\n  driver: nats\n", "line 2")
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("want %v, got %v", ErrUnknownField, err)
	}
	if strings.Contains(err.Error(), "Turso is the database") {
		t.Errorf("a stream key was given the store's advice: %v", err)
	}
	if !strings.Contains(err.Error(), "check the spelling") {
		t.Errorf("an ordinary unknown key must read as one: %v", err)
	}
}

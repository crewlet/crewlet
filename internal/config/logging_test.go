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
	err := rejects(t, "name: Acme\ndebug: true\n", "debug: unknown field")
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
	err := rejectsBootstrap(t, "stream:\n  driver: nats\n", "stream.driver: unknown field")
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

// THE LOG FILE BLOCK IS REFUSED WHERE IT SAYS NOTHING USABLE, by the same
// rule the level and the format follow: a file is written once and deployed
// for months, so a value that quietly did something else would run that way
// for as long as nobody looked.
func TestLogFileValidatorRejections(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, yaml, path string
		kind             error
	}{
		{
			"an unknown shape for the file",
			"logging:\n  file:\n    path: /tmp/c.log\n    format: pretty\n",
			"logging.file.format", ErrUnknownValue,
		},
		// A FILE THAT ROTATES EVERY ZERO BYTES IS NOT A SETTING. It is
		// also the zero value of `max_size_mb`, which is why the field is
		// a pointer: read as "unset" this would silently become 100 MB
		// and the operator would have no way to tell.
		{
			"a zero rotation size",
			"logging:\n  file:\n    path: /tmp/c.log\n    max_size_mb: 0\n",
			"logging.file.max_size_mb", ErrOutOfRange,
		},
		{
			"a negative rotation size",
			"logging:\n  file:\n    path: /tmp/c.log\n    max_size_mb: -5\n",
			"logging.file.max_size_mb", ErrOutOfRange,
		},
		{
			"a negative backup count",
			"logging:\n  file:\n    path: /tmp/c.log\n    max_backups: -1\n",
			"logging.file.max_backups", ErrOutOfRange,
		},
		// A SHAPE OR A CAP WITH NO FILE TO WRITE reads in review as a node
		// that logs to a file and writes nothing at all — the retired
		// `debug: true` failure, one block down.
		{
			"a shape with nothing to write it to",
			"logging:\n  file:\n    format: json\n",
			"logging.file.path", ErrMissing,
		},
		{
			"caps with nothing to cap",
			"logging:\n  file:\n    max_size_mb: 10\n    max_backups: 2\n",
			"logging.file.path", ErrMissing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.yaml, tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// AND ZERO BACKUPS IS ACCEPTED, because it IS a setting: cap the disk at one
// file. It is the one number here whose zero value is meaningful, which is
// the whole reason `max_backups` is a pointer rather than an int.
func TestZeroBackupsIsASettingRatherThanAnAbsence(t *testing.T) {
	t.Parallel()
	boot, err := ParseBootstrap(
		[]byte("logging:\n  file:\n    path: /tmp/c.log\n    max_backups: 0\n"), EnvOnly())
	if err != nil {
		t.Fatalf("max_backups: 0 was refused: %v", err)
	}
	settings, ok := boot.Logging.File.LogFileSettings()
	if !ok {
		t.Fatal("a block with a path reported no file")
	}
	if settings.Open.MaxBackups == nil {
		t.Fatal("an explicit 0 reached the sink as \"nothing was said\"")
	}
	if *settings.Open.MaxBackups != 0 {
		t.Errorf("max_backups = %d, want 0", *settings.Open.MaxBackups)
	}
}

// NO FILE IS THE DEFAULT, and an empty path is how a block says so. Every
// deployment whose platform already captures stderr writes no file, and that
// has to be what a document saying nothing means.
func TestNoLogFileIsTheDefault(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, yaml string }{
		{"nothing at all", "{}\n"},
		{"a logging block with no file", "logging:\n  level: debug\n"},
		{"an empty file block", "logging:\n  file: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot, err := ParseBootstrap([]byte(tc.yaml), EnvOnly())
			if err != nil {
				t.Fatalf("expected a valid document, got: %v", err)
			}
			if _, ok := boot.Logging.File.LogFileSettings(); ok {
				t.Error("a document that named no file asked for one")
			}
		})
	}
}

// THE WHOLE BLOCK REACHES THE SINK, caps and shape included — the one thing
// between an operator's document and what internal/logging actually opens.
func TestLogFileOptionsCarryTheWholeBlock(t *testing.T) {
	t.Parallel()
	boot, err := ParseBootstrap([]byte("logging:\n  file:\n"+
		"    path: /var/log/crewlet/crewlet.log\n    format: json\n"+
		"    max_size_mb: 25\n    max_backups: 3\n"), EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid document, got: %v", err)
	}
	settings, ok := boot.Logging.File.LogFileSettings()
	if !ok {
		t.Fatal("a block with a path reported no file")
	}
	if settings.Open.Path != "/var/log/crewlet/crewlet.log" {
		t.Errorf("path = %q", settings.Open.Path)
	}
	if settings.Sink.Format != logging.FormatJSON {
		t.Errorf("format = %q, want json", settings.Sink.Format)
	}
	if settings.Open.MaxSizeMB == nil || *settings.Open.MaxSizeMB != 25 {
		t.Errorf("max_size_mb = %v, want 25", settings.Open.MaxSizeMB)
	}
	if settings.Open.MaxBackups == nil || *settings.Open.MaxBackups != 3 {
		t.Errorf("max_backups = %v, want 3", settings.Open.MaxBackups)
	}
}

// EVERY FORMAT THE FILE BLOCK ACCEPTS IS ONE THE ENGINE CAN INSTALL — the
// same assertion the process-wide format gets, one level down, because the
// file sink has a shape of its own to drift.
func TestEveryDeclaredFormatIsUsableForTheFile(t *testing.T) {
	t.Parallel()
	for _, format := range logging.Formats {
		boot := DefaultBootstrap()
		boot.Logging.File.Path = "/tmp/crewlet.log"
		boot.Logging.File.Format = format
		if err := boot.Validate(); err != nil {
			t.Errorf("file format %q is in the closed set but refused: %v", format, err)
		}
		if got, _ := boot.Logging.File.LogFileSettings(); got.Sink.Format != format {
			t.Errorf("file format %q resolved to %v", format, got.Sink.Format)
		}
	}
}

// A `${VAR}` PATH RESOLVES like every other Tier A string, so a container
// hands the node its log path the same way it hands it a node id.
func TestALogFilePathTakesAnEnvReference(t *testing.T) {
	t.Setenv("CREWLET_TEST_LOG_PATH", "/var/log/crewlet/from-env.log")
	boot, err := ParseBootstrap(
		[]byte("logging:\n  file:\n    path: \"${CREWLET_TEST_LOG_PATH}\"\n"), EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid document, got: %v", err)
	}
	settings, ok := boot.Logging.File.LogFileSettings()
	if !ok {
		t.Fatal("a block with a path reported no file")
	}
	if settings.Open.Path != "/var/log/crewlet/from-env.log" {
		t.Errorf("path = %q, want the resolved value", settings.Open.Path)
	}
}

// `stderr: false` WITH NO FILE IS A NODE THAT LOGS NOWHERE, and it is
// refused by name. The field is a statement about the file taking the stream
// over, not a request for silence — and an operator told what is wrong with
// their document can fix it where one whose setting was quietly overridden
// cannot.
func TestSilencingStderrWithNoFileIsRefused(t *testing.T) {
	t.Parallel()
	err := rejectsBootstrap(t, "logging:\n  stderr: false\n", "logging.stderr")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want %v, got %v", ErrConflict, err)
	}
}

// AND WITH A FILE IT IS ACCEPTED, because that is the deployment it exists
// for: a durable log the platform is not also capturing twice.
func TestSilencingStderrBesideAFileIsAccepted(t *testing.T) {
	t.Parallel()
	boot, err := ParseBootstrap([]byte(
		"logging:\n  stderr: false\n  file:\n    path: /var/log/crewlet/crewlet.log\n"),
		EnvOnly())
	if err != nil {
		t.Fatalf("stderr: false beside a file was refused: %v", err)
	}
	if boot.Logging.StderrEnabled() {
		t.Error("an explicit `stderr: false` reached the engine as \"nothing was said\"")
	}
}

// UNSET IS ON, which is what every deployment without a log file must have.
// `false` is the zero value of its own type, which is why the field is a
// pointer: read as "unset" it would silence every node that wrote it at all.
func TestStderrDefaultsToOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, yaml string
		want       bool
	}{
		{"nothing at all", "{}\n", true},
		{"a logging block that says nothing about it", "logging:\n  level: debug\n", true},
		{"an explicit true", "logging:\n  stderr: true\n", true},
		{
			"an explicit false beside a file",
			"logging:\n  stderr: false\n  file:\n    path: /tmp/c.log\n", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot, err := ParseBootstrap([]byte(tc.yaml), EnvOnly())
			if err != nil {
				t.Fatalf("expected a valid document, got: %v", err)
			}
			if got := boot.Logging.StderrEnabled(); got != tc.want {
				t.Errorf("StderrEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// THE FILE'S OWN LEVEL REACHES THE SINK, converted once here at the edge so
// nothing below this package sees an operator's spelling.
func TestTheFileLevelIsConvertedAtTheEdge(t *testing.T) {
	t.Parallel()
	boot, err := ParseBootstrap([]byte(
		"logging:\n  level: warn\n  file:\n    path: /tmp/c.log\n    level: debug\n"),
		EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid document, got: %v", err)
	}
	settings, ok := boot.Logging.File.LogFileSettings()
	if !ok {
		t.Fatal("a block with a path reported no file")
	}
	if settings.Sink.Level == nil {
		t.Fatal("an explicit file level reached the sink as \"nothing was said\"")
	}
	if *settings.Sink.Level != slog.LevelDebug {
		t.Errorf("file level = %v, want debug", *settings.Sink.Level)
	}
	// And the process level is untouched by it: the two are separate
	// decisions, and a file level that moved `logging.level` would make
	// `-log-level` argue with the document.
	if level, _ := boot.LogSettings(); level != slog.LevelWarn {
		t.Errorf("the file's level moved the process level to %v", level)
	}
}

// AN UNSET FILE LEVEL IS "FOLLOW THE PROCESS", carried as nil rather than as
// a guess at what the process level happens to be right now — the flags are
// layered on afterwards, so a value resolved here would be the file's, not
// the invocation's.
func TestAnUnsetFileLevelStaysUnset(t *testing.T) {
	t.Parallel()
	boot, err := ParseBootstrap([]byte(
		"logging:\n  level: warn\n  file:\n    path: /tmp/c.log\n"), EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid document, got: %v", err)
	}
	settings, _ := boot.Logging.File.LogFileSettings()
	if settings.Sink.Level != nil {
		t.Errorf("an unset file level resolved to %v instead of following the process",
			*settings.Sink.Level)
	}
}

// AND AN UNKNOWN FILE LEVEL IS REFUSED, like every other closed set in a
// file — the flag path is the one that may not fail, not this one.
func TestAnUnknownFileLevelIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, yaml string }{
		{"a typo", "logging:\n  file:\n    path: /tmp/c.log\n    level: dbug\n"},
		// "warning" is accepted by the flag parser and deliberately not
		// by a file, here exactly as one block up.
		{"the warning alias", "logging:\n  file:\n    path: /tmp/c.log\n    level: warning\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.yaml, "logging.file.level")
			if !errors.Is(err, ErrUnknownValue) {
				t.Fatalf("want %v, got %v", ErrUnknownValue, err)
			}
		})
	}
}

// A LEVEL WITH NO FILE TO WRITE IT TO is the same mistake as a shape with no
// file, and is refused the same way.
func TestALogFileLevelWithNoPathIsRefused(t *testing.T) {
	t.Parallel()
	err := rejectsBootstrap(t, "logging:\n  file:\n    level: debug\n", "logging.file.path")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want %v, got %v", ErrMissing, err)
	}
}

// AN EXPLICIT `info` FILE LEVEL IS A SETTING, not an absence — the case the
// pointer in [logging.FileSink] exists for, at the config edge.
//
// slog.LevelInfo is 0, so a conversion that reported an explicit `info` as
// nil would silently give a `logging.level: warn` node a warn file. Every
// other level in this suite is non-zero and survives that mutation.
func TestAnExplicitInfoFileLevelSurvivesTheConversion(t *testing.T) {
	t.Parallel()
	boot, err := ParseBootstrap([]byte(
		"logging:\n  level: warn\n  file:\n    path: /tmp/c.log\n    level: info\n"),
		EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid document, got: %v", err)
	}
	settings, _ := boot.Logging.File.LogFileSettings()
	if settings.Sink.Level == nil {
		t.Fatal("an explicit `info` reached the sink as \"nothing was said\"")
	}
	if *settings.Sink.Level != slog.LevelInfo {
		t.Errorf("file level = %v, want info", *settings.Sink.Level)
	}
}

// THE ROTATION CAPS ARE BOUNDED AT BOTH ENDS, and the ceiling is the half
// that is not obvious.
//
// `max_size_mb` is held in BYTES by the sink, so a value at or above 1<<43 MB
// wraps int64 negative and the file rotates on EVERY line — the exact inverse
// of the enormous number an operator wrote, and silent, because rotating is
// what success looks like. `max_backups` is a rename count per rotation, so a
// huge one stalls the rotation rather than keeping more history. The
// deployment guide tells operators running logrotate(8) to write "a size this
// node will never reach", so the ceiling is what keeps that advice from
// having a cliff at the end of it.
func TestTheRotationCapsAreBoundedAbove(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, yaml, path string
	}{
		{
			"a size that wraps the byte arithmetic",
			"logging:\n  file:\n    path: /tmp/c.log\n    max_size_mb: 8796093022208\n",
			"logging.file.max_size_mb",
		},
		{
			"a size one past the ceiling",
			"logging:\n  file:\n    path: /tmp/c.log\n    max_size_mb: 1073741825\n",
			"logging.file.max_size_mb",
		},
		{
			"a backup count that would stall the rotation",
			"logging:\n  file:\n    path: /tmp/c.log\n    max_backups: 1001\n",
			"logging.file.max_backups",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.yaml, tc.path)
			if !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("want %v, got %v", ErrOutOfRange, err)
			}
		})
	}
}

// AND THE CEILINGS THEMSELVES ARE ACCEPTED, so the bound is a range rather
// than an off-by-one that refuses the value it documents.
func TestTheRotationCapCeilingsAreThemselvesValid(t *testing.T) {
	t.Parallel()
	boot := DefaultBootstrap()
	boot.Logging.File.Path = "/tmp/crewlet.log"
	size, backups := logging.MaxSizeMBCeiling, logging.MaxBackupsCeiling
	boot.Logging.File.MaxSizeMB, boot.Logging.File.MaxBackups = &size, &backups
	if err := boot.Validate(); err != nil {
		t.Errorf("the documented ceilings are refused by the validator: %v", err)
	}
}

// AN UNRESOLVED `${VAR}` IN THE LOG FILE PATH STOPS THE BOOT.
//
// An unresolved reference expands to the empty string, and empty is a
// legitimate SETTING for this field — "write no file". So without this,
// `path: "${LOG_PATH}"` with the variable unset does not fail and does not
// look wrong; it silently becomes the deployment that asked for no durable
// log at all, which is the precise failure the whole surface is arranged
// against. Every other Tier A field catches it on its own terms (an empty
// store.path is refused as a missing store); this one cannot.
func TestAnUnresolvedLogFilePathIsRefused(t *testing.T) {
	_, err := ParseBootstrap(
		[]byte("logging:\n  file:\n    path: \"${CREWLET_TEST_UNSET_LOG_PATH}\"\n"),
		EnvOnly())
	if err == nil {
		t.Fatal("a log file named by an unanswered variable was accepted")
	}
	if !strings.Contains(err.Error(), "logging.file.path") {
		t.Errorf("the error does not name the field: %v", err)
	}
	if !strings.Contains(err.Error(), "CREWLET_TEST_UNSET_LOG_PATH") {
		t.Errorf("the error does not name the variable to set: %v", err)
	}
}

// AND A RESOLVED ONE IS FINE, or the guard above would refuse every
// container that templates its log path — the ordinary case.
func TestAResolvedLogFilePathIsAccepted(t *testing.T) {
	t.Setenv("CREWLET_TEST_SET_LOG_PATH", "/var/log/crewlet/crewlet.log")
	boot, err := ParseBootstrap(
		[]byte("logging:\n  file:\n    path: \"${CREWLET_TEST_SET_LOG_PATH}\"\n"),
		EnvOnly())
	if err != nil {
		t.Fatalf("a resolved path was refused: %v", err)
	}
	settings, ok := boot.Logging.File.LogFileSettings()
	if !ok || settings.Open.Path != "/var/log/crewlet/crewlet.log" {
		t.Errorf("path = %q, ok = %v", settings.Open.Path, ok)
	}
}

// A DOCUMENT THAT NAMES NO FILE AT ALL stays legitimate — the guard is about
// a reference that went unanswered, never about the absence of the block.
func TestNoLogFileBlockIsStillFine(t *testing.T) {
	t.Parallel()
	if _, err := ParseBootstrap([]byte("logging:\n  level: debug\n"), EnvOnly()); err != nil {
		t.Fatalf("a document with no log file was refused: %v", err)
	}
}

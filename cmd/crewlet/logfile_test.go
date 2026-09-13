package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
)

// THE FLAG OVERRIDES THE FILE ONLY WHERE IT SPOKE — the same rule
// [logSettings] follows, and it matters in BOTH directions here. `-log-file`
// carries the empty string as its default whether or not anyone typed it, so
// applying it unconditionally would make `logging.file.path` in a
// deployment's own document dead on arrival; and an EXPLICIT `-log-file ""`
// has to be how a node with a file configured is asked to write none for one
// run.
func TestLogFileSettingsOverrideTheFileOnlyWhenGiven(t *testing.T) {
	t.Parallel()
	const withFile = "logging:\n  file:\n    path: /var/log/crewlet/crewlet.log\n" +
		"    format: json\n    max_size_mb: 20\n    max_backups: 2\n"

	for _, tc := range []struct {
		name, yaml string
		args       []string
		wantPath   string
		wantFormat logging.Format
	}{
		{"neither says anything", "", nil, "", ""},
		{"the file alone names a path", withFile, nil,
			"/var/log/crewlet/crewlet.log", logging.FormatJSON},
		{"the flag alone names a path", "", []string{"-log-file", "/tmp/one-run.log"},
			"/tmp/one-run.log", ""},
		// The flag replaces the PATH and leaves the shape and the caps
		// alone: those describe the disk this deployment runs on rather
		// than this invocation.
		{"the flag beats the file's path", withFile, []string{"-log-file", "/tmp/one-run.log"},
			"/tmp/one-run.log", logging.FormatJSON},
		// AN EXPLICIT EMPTY VALUE IS AN INSTRUCTION, not silence.
		{"an explicit empty flag turns the file off", withFile, []string{"-log-file", ""},
			"", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot, err := config.ParseBootstrap([]byte(tc.yaml), config.EnvOnly())
			if err != nil {
				t.Fatalf("expected a valid Tier A document, got: %v", err)
			}
			fs, _, _, logFile, _ := runFlags(t, tc.args...)
			settings, _, err := logFileSettings(boot, fs, logFile)
			if err != nil {
				t.Fatalf("logFileSettings: %v", err)
			}
			if settings.Open.Path != tc.wantPath {
				t.Errorf("path = %q, want %q", settings.Open.Path, tc.wantPath)
			}
			if settings.Sink.Format != tc.wantFormat {
				t.Errorf("format = %q, want %q", settings.Sink.Format, tc.wantFormat)
			}
		})
	}
}

// THE CAPS STAY THE FILE'S. `-log-file` moves where one run writes; how much
// disk the deployment is willing to give it is not an invocation property,
// which is the same line `-roles` and `-api-host` are drawn along.
func TestTheFlagMovesThePathAndNotTheCaps(t *testing.T) {
	t.Parallel()
	boot, err := config.ParseBootstrap([]byte(
		"logging:\n  file:\n    path: /var/log/crewlet/crewlet.log\n"+
			"    max_size_mb: 20\n    max_backups: 0\n"), config.EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid Tier A document, got: %v", err)
	}
	fs, _, _, logFile, _ := runFlags(t, "-log-file", "/tmp/one-run.log")
	settings, _, err := logFileSettings(boot, fs, logFile)
	if err != nil {
		t.Fatalf("logFileSettings: %v", err)
	}

	if settings.Open.MaxSizeMB == nil || *settings.Open.MaxSizeMB != 20 {
		t.Errorf("max_size_mb = %v, want the file's 20", settings.Open.MaxSizeMB)
	}
	if settings.Open.MaxBackups == nil || *settings.Open.MaxBackups != 0 {
		t.Errorf("max_backups = %v, want the file's explicit 0", settings.Open.MaxBackups)
	}
}

// ATTACHING IS A SECOND DESTINATION AND A TEARDOWN.
//
// Not parallel: it reconfigures the process-wide logger. See [TestMain].
func TestAttachingTheLogFileAddsASinkAndDetachesIt(t *testing.T) {
	var console bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() {
		logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
	})

	path := filepath.Join(t.TempDir(), "crewlet.log")
	detach, err := attachLogFile(config.LogFileSettings{
		Open: logging.FileOptions{Path: path},
		Sink: logging.FileSink{Format: logging.FormatJSON},
	}, true, &console)
	if err != nil {
		t.Fatalf("attachLogFile: %v", err)
	}

	logging.Get("probe").Info("while_attached")
	detach()
	logging.Get("probe").Info("after_detach")

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}
	if !strings.Contains(string(body), "while_attached") {
		t.Errorf("the line did not reach the file: %q", body)
	}
	// THE TEARDOWN DETACHES, so a line emitted after it goes to the
	// console sink alone. That it detaches BEFORE closing is the other
	// half, and it is asserted where the consequence is visible:
	// [logging.TestWritingAfterCloseIsRefused] covers what a sink closed
	// while still attached does about the lines it is dropping.
	if strings.Contains(string(body), "after_detach") {
		t.Errorf("a detached file still received a line: %q", body)
	}
	if !strings.Contains(console.String(), "after_detach") {
		t.Errorf("detaching the file silenced the console: %q", console.String())
	}
	// The console keeps everything throughout: a file is added to stderr,
	// never instead of it.
	if !strings.Contains(console.String(), "while_attached") {
		t.Errorf("attaching a file silenced the console: %q", console.String())
	}
	// And the file says which file it is, so an operator reading a
	// relative path back is not guessing at which copy they opened.
	if !strings.Contains(string(body), "log_file_opened") {
		t.Errorf("the file does not name itself: %q", body)
	}
}

// A PATH THAT CANNOT BE OPENED FAILS THE COMMAND.
//
// Every other bad logging value in this binary resolves to a default,
// because a misspelled log level must never be why a company will not boot.
// A path is not one of those: an operator who configured a durable record
// and silently did not get one has nothing pointing at why — which is the
// retired `debug: true` field's whole biography.
func TestAnUnopenableLogFileFailsTheCommand(t *testing.T) {
	// A file where a directory has to be: every path under it is refused
	// by the kernel, on every platform this ships to.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := attachLogFile(config.LogFileSettings{
		Open: logging.FileOptions{Path: filepath.Join(blocker, "crewlet.log")},
	}, true, io.Discard)
	if err == nil {
		t.Fatal("an unopenable path was accepted")
	}
	if !strings.Contains(err.Error(), "crewlet.log") {
		t.Errorf("the error does not name the path an operator has to fix: %v", err)
	}
}

// NO PATH IS NOT A FAILURE, and its teardown is still safe to call — every
// run that configures no file takes this path.
func TestNoLogFileAttachesNothing(t *testing.T) {
	detach, err := attachLogFile(config.LogFileSettings{}, true, io.Discard)
	if err != nil {
		t.Fatalf("an absent log file was treated as a mistake: %v", err)
	}
	detach()
}

// $CREWLET_LOG_FILE IS THE OPERATOR COMMANDS' LEVER, the third sibling of
// $CREWLET_LOG_LEVEL and $CREWLET_LOG_FORMAT. These commands take no logging
// flags, so a CI step that wants `crewlet validate` in the same durable
// record as the node it is checking has no other way to ask.
//
// Not parallel: it reconfigures the process-wide logger. See [TestMain].
func TestTheOperatorCommandsWriteTheEnvironmentsLogFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.log")
	t.Setenv("CREWLET_LOG_FILE", path)
	t.Setenv("CREWLET_LOG_LEVEL", "warn")

	var out, errOut bytes.Buffer
	_ = run([]string{"validate", unresolvedBootstrap(t)}, &out, &errOut)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the environment's log file was never opened: %v", err)
	}
	if !strings.Contains(string(body), "NOT_SET_ANYWHERE") {
		t.Errorf("the command's warning did not reach the file: %q", body)
	}
	// The command's own stdout/stderr are untouched: the log file is a
	// second copy of the PROCESS's telemetry, not a redirect of what a
	// command prints for its caller.
	if strings.Contains(errOut.String(), "NOT_SET_ANYWHERE") {
		t.Errorf("a log line was written into the writer the command was handed: %q",
			errOut.String())
	}
}

// AND `crewlet run` DOES NOT ANSWER IT. Its file comes from Tier A and
// `-log-file`, exactly as its level comes from `logging.level` and
// `-log-level` rather than from $CREWLET_LOG_LEVEL: a node's destinations
// belong to the document that describes the deployment, where they can be
// reviewed, and an environment variable that silently redirected a whole
// company's log would be a second configuration surface with no history.
//
// Not parallel: t.Setenv.
func TestRunIgnoresTheEnvironmentsLogFile(t *testing.T) {
	t.Setenv("CREWLET_LOG_FILE", filepath.Join(t.TempDir(), "ops.log"))

	boot, err := config.ParseBootstrap(nil, config.EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid Tier A document, got: %v", err)
	}
	fs, _, _, logFile, _ := runFlags(t)
	settings, _, err := logFileSettings(boot, fs, logFile)
	if err != nil {
		t.Fatalf("logFileSettings: %v", err)
	}
	if settings.Open.Path != "" {
		t.Errorf("`crewlet run` took its log file from the environment: %q",
			settings.Open.Path)
	}
}

// AND A BROKEN $CREWLET_LOG_FILE FAILS THE COMMAND rather than running
// without the record it was asked to keep.
func TestABrokenEnvironmentLogFileFailsTheCommand(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREWLET_LOG_FILE", filepath.Join(blocker, "ops.log"))

	var out, errOut bytes.Buffer
	if err := run([]string{"version"}, &out, &errOut); err == nil {
		t.Fatal("a command ran on with a log file it could not open")
	}
}

// `-log-file ""` AGAINST `logging.stderr: false` IS REFUSED.
//
// Neither layer can see this on its own: the Tier A validator checks
// `stderr: false` against the file THAT DOCUMENT names, and the flag takes
// that file away afterwards. Both halves are known here and nowhere else, so
// this is where a node that would write its log nowhere is stopped.
func TestRemovingTheOnlyDestinationIsRefused(t *testing.T) {
	t.Parallel()
	boot, err := config.ParseBootstrap([]byte(
		"logging:\n  stderr: false\n  file:\n    path: /var/log/crewlet/crewlet.log\n"),
		config.EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid Tier A document, got: %v", err)
	}
	fs, _, _, logFile, _ := runFlags(t, "-log-file", "")
	if _, _, err = logFileSettings(boot, fs, logFile); err == nil {
		t.Fatal("a node with neither destination was accepted")
	}
	if !strings.Contains(err.Error(), "logging.stderr") {
		t.Errorf("the error does not name the field an operator has to change: %v", err)
	}
}

// AND THE SAME FLAG IS FINE WHEN STDERR IS STILL THERE — the ordinary way to
// turn a Tier A log file off for one run.
func TestRemovingTheFileIsFineWhileStderrRemains(t *testing.T) {
	t.Parallel()
	boot, err := config.ParseBootstrap([]byte(
		"logging:\n  file:\n    path: /var/log/crewlet/crewlet.log\n"), config.EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid Tier A document, got: %v", err)
	}
	fs, _, _, logFile, _ := runFlags(t, "-log-file", "")
	settings, stderr, err := logFileSettings(boot, fs, logFile)
	if err != nil {
		t.Fatalf("logFileSettings: %v", err)
	}
	if settings.Open.Path != "" {
		t.Errorf("the file was not removed: %q", settings.Open.Path)
	}
	if !stderr {
		t.Error("stderr was reported off for a document that never said so")
	}
}

// THE FILE'S OWN LEVEL SURVIVES THE FLAG, like its shape and its caps: a
// `-log-file` moves where this run writes, never how loud the durable record
// is, which is a property of the deployment.
func TestTheFlagMovesThePathAndNotTheLevel(t *testing.T) {
	t.Parallel()
	boot, err := config.ParseBootstrap([]byte(
		"logging:\n  level: warn\n  file:\n    path: /var/log/crewlet/crewlet.log\n"+
			"    level: debug\n"), config.EnvOnly())
	if err != nil {
		t.Fatalf("expected a valid Tier A document, got: %v", err)
	}
	fs, _, _, logFile, _ := runFlags(t, "-log-file", "/tmp/one-run.log")
	settings, _, err := logFileSettings(boot, fs, logFile)
	if err != nil {
		t.Fatalf("logFileSettings: %v", err)
	}
	if settings.Sink.Level == nil || *settings.Sink.Level != slog.LevelDebug {
		t.Errorf("file level = %v, want the document's debug", settings.Sink.Level)
	}
}

// ATTACHING WITH STDERR OFF SILENCES THE CONSOLE, AND THE TEARDOWN RESTORES
// IT BEFORE DETACHING THE FILE — a shutdown between the two would otherwise
// log its own drain nowhere.
//
// Not parallel: it reconfigures the process-wide logger. See [TestMain].
func TestAttachingWithStderrOffSilencesAndRestoresTheConsole(t *testing.T) {
	var console bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() {
		logging.SetConsole(true)
		logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
	})

	path := filepath.Join(t.TempDir(), "crewlet.log")
	detach, err := attachLogFile(config.LogFileSettings{
		Open: logging.FileOptions{Path: path},
		Sink: logging.FileSink{Format: logging.FormatText},
	}, false, &console)
	if err != nil {
		t.Fatalf("attachLogFile: %v", err)
	}

	logging.Get("probe").Info("while_silenced")
	detach()
	logging.Get("probe").Info("after_detach")

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}
	if !strings.Contains(string(body), "while_silenced") {
		t.Errorf("the line did not reach the file: %q", body)
	}
	if strings.Contains(console.String(), "while_silenced") {
		t.Errorf("stderr was not silenced: %q", console.String())
	}
	// The one line stderr DOES keep on this path: where the log went.
	// Without it an operator who switched stderr off sees a terminal that
	// says nothing at all and no way to find out why.
	if !strings.Contains(console.String(), path) {
		t.Errorf("stderr was silenced without saying where the log went: %q",
			console.String())
	}
	if !strings.Contains(console.String(), "after_detach") {
		t.Errorf("the teardown left the process logging nowhere: %q", console.String())
	}
	// AND IT RESTORES THE CONSOLE BEFORE DETACHING THE FILE, not the other
	// way round. Detaching first leaves a moment with no destination at
	// all, which the logging package's backstop rescues — by reopening the
	// console and WARNING about it. A clean shutdown must not produce that
	// warning, and it is the only thing that tells the two orders apart.
	if strings.Contains(console.String(), "console_kept_open") {
		t.Errorf("the teardown detached the file before restoring the console, "+
			"so a clean shutdown warned about logging nowhere: %q", console.String())
	}
}

// AND THE HANDOVER SURVIVES A QUIET LEVEL, which is the case that made it a
// bug rather than a nicety.
//
// The `log_file_opened` record is ordinary telemetry at info, so on a
// `logging.level: warn` node — an entirely normal production setting — it is
// filtered. Paired with `logging.stderr: false` that left `crewlet run`
// printing NOTHING AT ALL on a terminal, with no way for the operator to
// discover their log was in a file. The handover therefore goes straight to
// stderr rather than through the logger, exactly as the sink's own failure
// report does: a logger cannot announce that the logger is going quiet.
//
// Not parallel: it reconfigures the process-wide logger. See [TestMain].
func TestTheHandoverReachesStderrAtEveryLevel(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		t.Run(level.String(), func(t *testing.T) {
			var console bytes.Buffer
			logging.Configure(level, logging.FormatText, &console)
			t.Cleanup(func() {
				logging.SetConsole(true)
				logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
			})

			path := filepath.Join(t.TempDir(), "crewlet.log")
			detach, err := attachLogFile(config.LogFileSettings{
				Open: logging.FileOptions{Path: path},
				Sink: logging.FileSink{Format: logging.FormatText},
			}, false, &console)
			if err != nil {
				t.Fatalf("attachLogFile: %v", err)
			}
			t.Cleanup(detach)

			if !strings.Contains(console.String(), path) {
				t.Errorf("at %v the terminal went silent without naming the log "+
					"file: %q", level, console.String())
			}
		})
	}
}

// AND IT IS NOT PRINTED WHEN STDERR STAYS ON, or every ordinary run with a
// log file would carry a line about a handover that never happened.
func TestNoHandoverNoticeWhenStderrStaysOn(t *testing.T) {
	var console bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() {
		logging.Configure(slog.LevelError+1, logging.FormatText, io.Discard)
	})

	path := filepath.Join(t.TempDir(), "crewlet.log")
	detach, err := attachLogFile(config.LogFileSettings{
		Open: logging.FileOptions{Path: path},
		Sink: logging.FileSink{Format: logging.FormatText},
	}, true, &console)
	if err != nil {
		t.Fatalf("attachLogFile: %v", err)
	}
	t.Cleanup(detach)

	if strings.Contains(console.String(), "carries no further log lines") {
		t.Errorf("a run that kept stderr was told it was losing it: %q",
			console.String())
	}
}

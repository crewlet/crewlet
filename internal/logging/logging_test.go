package logging_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// packageLogger is how nearly every subsystem obtains one: a package-level
// var, evaluated at init — before main has parsed a single flag.
var packageLogger = logging.Get("subsystem")

// A LATE Configure REACHES A LOGGER OBTAINED AT INIT. Without this,
// `-log-level debug` would reach nothing: every `var log = logging.Get(...)`
// in the tree runs before the flag is read, so the only lines affected would
// be the handful emitted by loggers built afterwards — and every subsystem
// would keep logging at info with nothing saying why.
func TestConfiguringLateReachesALoggerBoundEarly(t *testing.T) {
	var buf bytes.Buffer
	logging.Configure(slog.LevelDebug, logging.FormatText, &buf)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, &bytes.Buffer{}) })

	packageLogger.Debug("a_debug_line", "k", "v")
	if !strings.Contains(buf.String(), "a_debug_line") {
		t.Fatalf("the init-time logger did not follow the reconfiguration: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "component=subsystem") {
		t.Errorf("the component attribute was lost: %q", buf.String())
	}
}

// AND THE LEVEL IS HONOURED in the other direction: raising it silences a
// logger that was already handed out.
func TestRaisingTheLevelSilencesALoggerBoundEarly(t *testing.T) {
	var buf bytes.Buffer
	logging.Configure(slog.LevelWarn, logging.FormatText, &buf)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, &bytes.Buffer{}) })

	packageLogger.Info("an_info_line")
	if strings.Contains(buf.String(), "an_info_line") {
		t.Fatalf("an info line survived a warn level: %q", buf.String())
	}
	packageLogger.Warn("a_warn_line")
	if !strings.Contains(buf.String(), "a_warn_line") {
		t.Fatalf("a warning was suppressed: %q", buf.String())
	}
}

// THE FORMAT FOLLOWS TOO, which is what makes `-log-format json` work for
// the subsystems rather than only for the CLI's own lines.
func TestTheFormatFollowsAReconfiguration(t *testing.T) {
	var buf bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatJSON, &buf)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, &bytes.Buffer{}) })

	packageLogger.Info("a_json_line", "k", "v")
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "{") {
		t.Fatalf("the line is not JSON: %q", buf.String())
	}
}

// DERIVED LOGGERS DO NOT LEAK INTO EACH OTHER. slog hands the same handler
// to every `With` on a shared package logger, so appending an op in place
// would put one derivation's attributes on another's lines whenever the
// backing array had spare capacity.
func TestDerivedLoggersDoNotShareAttributes(t *testing.T) {
	var buf bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &buf)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, &bytes.Buffer{}) })

	base := logging.Get("shared")
	left := base.With("side", "left")
	right := base.With("side", "right")

	left.Info("left_line")
	right.Info("right_line")

	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		switch {
		case strings.Contains(line, "left_line"):
			if strings.Contains(line, "right") {
				t.Errorf("the left logger carried the right one's attribute: %q", line)
			}
		case strings.Contains(line, "right_line"):
			if strings.Contains(line, "left") {
				t.Errorf("the right logger carried the left one's attribute: %q", line)
			}
		}
	}
}

// A GROUP SURVIVES the forwarding, which is the other half of the handler
// contract — a logger that dropped groups would flatten structured context
// onto the top level and silently collide keys.
func TestAGroupSurvivesTheForwarding(t *testing.T) {
	var buf bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &buf)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, &bytes.Buffer{}) })

	logging.Get("grouped").WithGroup("turn").Info("a_line", "id", "t1")
	if !strings.Contains(buf.String(), "turn.id=t1") {
		t.Fatalf("the group was dropped: %q", buf.String())
	}
}

// CONFIGURE RACES A LOGGING GOROUTINE in any real process: an apply
// reconfigures nothing, but the CLI configures while background work is
// already running, and the seat host logs from several goroutines.
func TestConcurrentLoggingAndReconfigurationDoNotRace(t *testing.T) {
	logging.Configure(slog.LevelInfo, logging.FormatText, &syncBuffer{})
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, &bytes.Buffer{}) })

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				packageLogger.Info("a_line", "k", "v")
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			logging.Configure(slog.LevelInfo, logging.FormatText, &syncBuffer{})
		}
	})
	wg.Wait()
}

// syncBuffer is a writer safe for the concurrent case above; slog's handlers
// serialise their own writes but two handlers over one buffer do not.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func TestParseLevelAndFormat(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError, "info": slog.LevelInfo,
		// A TYPO IS INFO, never a refusal: a bad log level must never be
		// the reason a company will not boot.
		"nonesuch": slog.LevelInfo, "": slog.LevelInfo, "  ": slog.LevelInfo,
	} {
		if got := logging.ParseLevel(name); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", name, got, want)
		}
	}
	for name, want := range map[string]logging.Format{
		"text": logging.FormatText, "TEXT": logging.FormatText,
		"json": logging.FormatJSON, "JSON": logging.FormatJSON,
		"console": logging.FormatConsole, " Console ": logging.FormatConsole,
		// A TYPO IS THE DEFAULT, exactly as it is for the level above.
		// It used to be JSON, which meant `-log-format tekst` silently
		// handed an operator a format they had not asked for and could
		// not read — a fallback that is only "safe" in one direction. A
		// config file's logging.format is a closed set and is REFUSED
		// on a typo; this is the flag path, where nothing may fail.
		"nonesuch": logging.FormatConsole, "": logging.FormatConsole,
	} {
		if got := logging.ParseFormat(name); got != want {
			t.Errorf("ParseFormat(%q) = %v, want %v", name, got, want)
		}
	}
}

// SETVERBOSITY KEEPS THE DESTINATION, which is the whole reason it exists
// beside Configure.
//
// A command's flags say how loud it should be; they do not say where a
// process's logs go. Collapsing the two let `crewlet`'s own run() install the
// writer it had been handed as the process-wide sink — harmless with one
// invocation per process, and under `go test` a global pointing at whichever
// parallel test configured it last.
func TestSetVerbosityChangesTheLevelAndNotTheDestination(t *testing.T) {
	var installed, other bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &installed)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetVerbosity(slog.LevelDebug, logging.FormatJSON)
	logging.Get("probe").Debug("after_set_verbosity")

	if !strings.Contains(installed.String(), "after_set_verbosity") {
		t.Fatalf("the line did not reach the installed destination: %q",
			installed.String())
	}
	// THE LEVEL MOVED. Debug is below the level Configure installed, so a
	// SetVerbosity that did nothing would have dropped the line entirely
	// and the assertion above would be the only thing failing — which
	// would read as a destination problem.
	if !strings.Contains(installed.String(), `"msg"`) {
		t.Errorf("the format did not change to JSON: %q", installed.String())
	}
	if other.Len() != 0 {
		t.Errorf("something reached a writer nothing installed: %q", other.String())
	}
}

// A LOG FILE IS A SECOND DESTINATION, NEVER A REPLACEMENT.
//
// stderr is the only sink that exists before the config naming the file has
// been read, it is what a container platform captures, and it is where a
// boot failure is watched. A node that went quiet there the moment a path
// was configured would look exactly like one that had stopped.
func TestAFileSinkDoesNotSilenceTheConsoleOne(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(&file, "")
	logging.Get("both").Info("a_line", "k", "v")
	logging.SetFile(nil, "")

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		if !strings.Contains(got, "a_line") {
			t.Errorf("the %s sink did not get the line: %q", name, got)
		}
	}
}

// AND IT CARRIES ITS OWN SHAPE. This is the whole reason the second
// destination is a second handler rather than an io.MultiWriter: a person
// watching a boot wants columns while the shipper reading the file wants
// json, and one writer cannot be both.
func TestTheFileSinkCarriesItsOwnFormat(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(&file, logging.FormatJSON)
	logging.Get("split").Info("a_line")
	logging.SetFile(nil, "")

	if !strings.HasPrefix(strings.TrimSpace(file.String()), "{") {
		t.Errorf("the file is not JSON: %q", file.String())
	}
	if strings.HasPrefix(strings.TrimSpace(console.String()), "{") {
		t.Errorf("the file's format reached the console sink: %q", console.String())
	}
}

// A FILE GIVEN NO SHAPE FOLLOWS THE PROCESS'S, which is what makes
// `-log-format json` mean one thing rather than two.
func TestAFileWithNoFormatFollowsTheProcessFormat(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(&file, "")
	logging.SetVerbosity(slog.LevelInfo, logging.FormatJSON)
	logging.Get("follow").Info("a_line")
	logging.SetFile(nil, "")

	if !strings.HasPrefix(strings.TrimSpace(file.String()), "{") {
		t.Errorf("the file did not follow the process format: %q", file.String())
	}
}

// SetVerbosity KEEPS THE FILE. The level and the shape are invocation
// properties and the destinations are not, so turning a running node's
// verbosity up must not quietly detach the durable copy of its log.
func TestSetVerbosityKeepsTheFileDestination(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(&file, logging.FormatText)
	logging.SetVerbosity(slog.LevelDebug, logging.FormatText)
	logging.Get("kept").Debug("a_debug_line")
	logging.SetFile(nil, "")

	if !strings.Contains(file.String(), "a_debug_line") {
		t.Errorf("the file destination was lost to a verbosity change: %q", file.String())
	}
}

// AND Configure CLEARS IT, because Configure is the statement of what this
// process's destinations ARE rather than an adjustment to them — a TestMain
// pointing the process at io.Discard must not leave a previous test's file
// attached.
func TestConfigureClearsTheFileDestination(t *testing.T) {
	var first, file, second bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &first)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })
	logging.SetFile(&file, logging.FormatText)

	logging.Configure(slog.LevelInfo, logging.FormatText, &second)
	logging.Get("cleared").Info("after_reconfigure")

	if strings.Contains(file.String(), "after_reconfigure") {
		t.Errorf("the file survived a Configure: %q", file.String())
	}
	if !strings.Contains(second.String(), "after_reconfigure") {
		t.Errorf("the new console destination did not get the line: %q", second.String())
	}
}

// REMOVING THE FILE LEAVES THE CONSOLE ALONE, which is what the CLI's
// teardown does before closing the descriptor.
func TestRemovingTheFileLeavesTheConsoleSink(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(&file, logging.FormatText)
	logging.SetFile(nil, "")
	logging.Get("detached").Info("after_detach")

	if strings.Contains(file.String(), "after_detach") {
		t.Errorf("a detached file still received a line: %q", file.String())
	}
	if !strings.Contains(console.String(), "after_detach") {
		t.Errorf("detaching the file silenced the console: %q", console.String())
	}
}

// ONE LEVEL, BOTH DESTINATIONS. There is deliberately no per-sink level:
// "was this line written" must not depend on which file you look in, and
// [lazy.Enabled] answers for the whole tree from the root handler's level
// alone — a fan-out that disagreed with its children would filter different
// lines depending on how a call site was spelled.
func TestBothDestinationsShareOneLevel(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelWarn, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(&file, logging.FormatText)
	log := logging.Get("levelled")
	if log.Enabled(t.Context(), slog.LevelInfo) {
		t.Error("info is enabled at a warn level with a file attached")
	}
	log.Info("an_info_line")
	log.Warn("a_warn_line")
	logging.SetFile(nil, "")

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		if strings.Contains(got, "an_info_line") {
			t.Errorf("the %s sink took a line below the level: %q", name, got)
		}
		if !strings.Contains(got, "a_warn_line") {
			t.Errorf("the %s sink dropped a line at the level: %q", name, got)
		}
	}
}

// DERIVED LOGGERS STILL DO NOT LEAK, with two destinations under them. The
// fan-out derives each child into a NEW slice for the same reason
// [lazy.with] copies its ops: slog hands one handler to every logger derived
// from it.
func TestDerivedLoggersDoNotShareAttributesAcrossSinks(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })
	logging.SetFile(&file, logging.FormatText)
	t.Cleanup(func() { logging.SetFile(nil, "") })

	base := logging.Get("shared")
	base.With("side", "left").Info("left_line")
	base.With("side", "right").Info("right_line")

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		for line := range strings.SplitSeq(strings.TrimSpace(got), "\n") {
			switch {
			case strings.Contains(line, "left_line") && strings.Contains(line, "right"):
				t.Errorf("%s: the left logger carried the right one's attribute: %q", name, line)
			case strings.Contains(line, "right_line") && strings.Contains(line, "left"):
				t.Errorf("%s: the right logger carried the left one's attribute: %q", name, line)
			}
		}
	}
}

// A SICK SINK DOES NOT SILENCE THE OTHER ONE. A full log disk must cost the
// durable copy and nothing else: the fan-out tries every destination and
// joins what failed rather than returning on the first.
func TestAFailingDestinationDoesNotStopTheOthers(t *testing.T) {
	var console bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(brokenWriter{}, logging.FormatText)
	logging.Get("resilient").Info("a_line")
	logging.SetFile(nil, "")

	if !strings.Contains(console.String(), "a_line") {
		t.Errorf("a failing file sink took the console sink down with it: %q",
			console.String())
	}
}

// brokenWriter is a destination that is always full.
type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A GROUP AND ITS ATTRIBUTES REACH BOTH DESTINATIONS. WithGroup and
// WithAttrs are derived per child, so a fan-out that forwarded only to the
// first would lose structure on the file and nothing would say so.
func TestGroupsAndAttributesReachEveryDestination(t *testing.T) {
	var console, file bytes.Buffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &console)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatText, io.Discard) })

	logging.SetFile(&file, logging.FormatText)
	logging.Get("grouped").With("node", "n1").WithGroup("turn").Info("a_line", "id", "t1")
	logging.SetFile(nil, "")

	for name, got := range map[string]string{"console": console.String(), "file": file.String()} {
		for _, want := range []string{"component=grouped", "node=n1", "turn.id=t1"} {
			if !strings.Contains(got, want) {
				t.Errorf("the %s sink is missing %s: %q", name, want, got)
			}
		}
	}
}

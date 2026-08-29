package logging_test

import (
	"bytes"
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

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				packageLogger.Info("a_line", "k", "v")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			logging.Configure(slog.LevelInfo, logging.FormatText, &syncBuffer{})
		}
	}()
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
		"json": logging.FormatJSON, "nonesuch": logging.FormatJSON,
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

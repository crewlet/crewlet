package main

import (
	"flag"
	"io"
	"log/slog"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
)

// runFlags rebuilds the three logging flags `crewlet run` declares, with the
// same names and the same defaults, and parses args through them.
//
// The DEFAULTS ARE THE POINT: a flag carries its default whether or not
// anyone typed it, so a test that constructed the values directly would
// never exercise the thing [logSettings] exists to get right.
func runFlags(t *testing.T, args ...string) (*flag.FlagSet, string, string, bool) {
	t.Helper()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	level := fs.String("log-level", "info", "")
	format := fs.String("log-format", "console", "")
	debug := fs.Bool("debug", false, "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return fs, *level, *format, *debug
}

// THE COMMAND LINE WINS, BUT ONLY WHERE IT SPOKE.
//
// This is the whole bug, at the layer that had it: `debug: true` in Tier A
// was never read, and applying the flags unconditionally instead would have
// replaced that silence with a different one — every node pinned at the
// flag's own default, and `logging.level: warn` in the file dead on arrival.
func TestLogSettingsFlagsOverrideTheFileOnlyWhenGiven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		yaml       string
		args       []string
		wantLevel  slog.Level
		wantFormat logging.Format
	}{
		{
			"neither says anything", "", nil,
			slog.LevelInfo, logging.FormatConsole,
		},
		// The regression. Nothing on the command line, `debug: true` in
		// the file: the file has to be what decides.
		{
			"the file alone asks for debug", "debug: true\n", nil,
			slog.LevelDebug, logging.FormatConsole,
		},
		{
			"the file alone asks for json",
			"logging:\n  format: json\n", nil,
			slog.LevelInfo, logging.FormatJSON,
		},
		{
			"the file alone asks for warn",
			"logging:\n  level: warn\n", nil,
			slog.LevelWarn, logging.FormatConsole,
		},
		// An explicit flag beats the file, including when what it asks
		// for happens to equal the flag's own default.
		{
			"an explicit level beats the file",
			"debug: true\n", []string{"-log-level", "info"},
			slog.LevelInfo, logging.FormatConsole,
		},
		{
			"an explicit format beats the file",
			"logging:\n  format: json\n", []string{"-log-format", "text"},
			slog.LevelInfo, logging.FormatText,
		},
		// -debug only ever RAISES: it is the shorthand for asking for
		// debug, not a switch that turns the file's setting off.
		{
			"the debug flag beats a quieter file",
			"logging:\n  level: error\n", []string{"-debug"},
			slog.LevelDebug, logging.FormatConsole,
		},
		{
			"the debug flag beats an explicit level flag",
			"", []string{"-log-level", "warn", "-debug"},
			slog.LevelDebug, logging.FormatConsole,
		},
		{
			"a false debug flag does not silence the file",
			"debug: true\n", []string{"-debug=false"},
			slog.LevelDebug, logging.FormatConsole,
		},
		// The file's two ways of saying it, together, through the flags.
		{
			"file format and flag level",
			"logging:\n  level: warn\n  format: json\n", []string{"-log-level", "debug"},
			slog.LevelDebug, logging.FormatJSON,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot, err := config.ParseBootstrap([]byte(tc.yaml), config.EnvOnly())
			if err != nil {
				t.Fatalf("expected a valid Tier A document, got: %v", err)
			}
			fs, level, format, debug := runFlags(t, tc.args...)
			gotLevel, gotFormat := logSettings(boot, fs, level, format, debug)
			if gotLevel != tc.wantLevel {
				t.Errorf("level = %v, want %v", gotLevel, tc.wantLevel)
			}
			if gotFormat != tc.wantFormat {
				t.Errorf("format = %v, want %v", gotFormat, tc.wantFormat)
			}
		})
	}
}

// BEFORE THE FILE IS OPEN, THE FLAGS ARE ALL THERE IS. `-debug` is turned on
// most often to watch the config load itself fail, so the first SetVerbosity
// has to happen before it — this is the level that call uses.
func TestFlagLogLevelIsTheShorthandThenTheFlag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		level string
		debug bool
		want  slog.Level
	}{
		{"info", false, slog.LevelInfo},
		{"warn", false, slog.LevelWarn},
		{"warn", true, slog.LevelDebug},
		{"nonesuch", false, slog.LevelInfo},
	} {
		if got := flagLogLevel(tc.level, tc.debug); got != tc.want {
			t.Errorf("flagLogLevel(%q, %v) = %v, want %v",
				tc.level, tc.debug, got, tc.want)
		}
	}
}

// THE OPERATOR COMMANDS TAKE NO FLAGS, so the environment is the only lever
// over their output. $CREWLET_LOG_FORMAT is the sibling of
// $CREWLET_LOG_LEVEL: a CI step that ships a `crewlet migrate` run to a
// collector needs json out of it exactly as much as out of `crewlet run`.
func TestOperatorLogFormat(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want logging.Format
	}{
		{"", logging.FormatConsole},
		{"json", logging.FormatJSON},
		{"TEXT", logging.FormatText},
		{" console ", logging.FormatConsole},
		// Never a refusal, and never a shape nobody asked for: a typo
		// lands on the default, as it does for the level.
		{"jsonn", logging.FormatConsole},
	} {
		t.Run("CREWLET_LOG_FORMAT="+tc.set, func(t *testing.T) {
			t.Setenv("CREWLET_LOG_FORMAT", tc.set)
			if got := operatorLogFormat(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

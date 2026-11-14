package prereqtest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// A MISSING PREREQUISITE FAILS UNDER MAKE AND SKIPS ELSEWHERE, in the same
// words, through every door this package has: [prereqtest.Missing] itself, a
// program [prereqtest.OnPath] cannot find, and a variable
// [prereqtest.Named] finds unset. With CREWLET_REQUIRE_PREREQUISITES=1, which
// the make targets that run the suite set, each FAILS naming its remedy;
// unset, each SKIPS naming the same remedy; any other value is a mistake that
// fails naming the variable, never read as either answer.
//
// Not parallel: it sets the variables the helpers read.
//
// Mutation: read the variable as set whenever it is non-empty, and the
// "true" case fails with the remedy rather than naming the variable — red.
func TestAMissingPrerequisiteFailsUnderMakeAndSkipsElsewhere(t *testing.T) {
	const remedy = "run the one remedy the case names"
	doors := []struct {
		name string
		call func(testing.TB)
	}{
		{"Missing", func(tb testing.TB) { prereqtest.Missing(tb, "the thing is absent", remedy) }},
		{"OnPath", func(tb testing.TB) {
			prereqtest.OnPath(tb, "crewlet-prereqtest-absent", "a case runs under it", remedy)
		}},
		{"Named", func(tb testing.TB) {
			prereqtest.Named(tb, "CREWLET_PREREQTEST_UNSET", "a program a case runs", remedy)
		}},
	}
	for _, setting := range []struct {
		name, value string
		fatal, skip string // a fragment of the failure or the skip
	}{
		{name: "required", value: "1", fatal: remedy},
		{name: "unset", value: "", skip: remedy},
		{name: "a requirement that is no setting", value: "true", fatal: prereqtest.RequireEnv + `="true"`},
	} {
		for _, door := range doors {
			t.Run(setting.name+"/"+door.name, func(t *testing.T) {
				t.Setenv(prereqtest.RequireEnv, setting.value)
				t.Setenv("PATH", t.TempDir())
				t.Setenv("CREWLET_PREREQTEST_UNSET", "")
				got := prereqtest.Run(t, door.call)
				switch {
				case setting.fatal != "":
					if !strings.Contains(got.Fatal, setting.fatal) || got.Skip != "" {
						t.Errorf("failed %q, skipped %q; want a failure saying %q", got.Fatal, got.Skip, setting.fatal)
					}
				default:
					if !strings.Contains(got.Skip, setting.skip) || got.Fatal != "" {
						t.Errorf("failed %q, skipped %q; want a skip saying %q", got.Fatal, got.Skip, setting.skip)
					}
				}
			})
		}
	}
	// THE SAME WORDS EITHER WAY: what is absent and its remedy open both the
	// failure and the skip, so a contributor reads one sentence whichever way
	// the run went.
	t.Run("same words", func(t *testing.T) {
		t.Setenv(prereqtest.RequireEnv, "1")
		fatal := prereqtest.Run(t, doors[0].call).Fatal
		t.Setenv(prereqtest.RequireEnv, "")
		skip := prereqtest.Run(t, doors[0].call).Skip
		const head = "the thing is absent: " + remedy + "."
		if !strings.HasPrefix(fatal, head) || !strings.HasPrefix(skip, head) {
			t.Errorf("failure %q and skip %q do not both open with %q", fatal, skip, head)
		}
	})
}

// A PROGRAM ON PATH IS HANDED BACK where PATH finds it, and nothing about the
// requirement is read for it: present is present under every setting.
//
// Mutation: hand back the name rather than the path PATH found, and the case
// gets "crewlet-prereqtest-present" — red.
func TestAProgramOnPathIsHandedBack(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "crewlet-prereqtest-present")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv(prereqtest.RequireEnv, "true")
	var got string
	ended := prereqtest.Run(t, func(tb testing.TB) {
		got = prereqtest.OnPath(tb, "crewlet-prereqtest-present", "a case runs under it", "no remedy")
	})
	if ended != (prereqtest.Ended{}) || got != program {
		t.Errorf("OnPath = %q (ended %+v), want %s", got, ended, program)
	}
}

// A NAMED PREREQUISITE IS AN ABSOLUTE PATH TO AN EXECUTABLE FILE, or the
// variable naming it is named in the failure — under every setting of the
// requirement, because a value somebody set is a mistake to correct and never
// a reason to skip: a relative path, a file that is not there, a directory and
// a file nobody may execute each fail, and an executable file comes back.
//
// Not parallel: it sets the variable it reads.
//
// Mutation: drop the executable-bit check, and the 0644 file comes back as
// though it would run — red.
func TestANamedPrerequisiteIsAnAbsolutePathToAnExecutable(t *testing.T) {
	const env = "CREWLET_PREREQTEST_PROGRAM"
	dir := t.TempDir()
	executable := filepath.Join(dir, "program")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	unexecutable := filepath.Join(dir, "unexecutable")
	if err := os.WriteFile(unexecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, value string
		fatal       string // a fragment of the failure; "" is the program back
	}{
		{"relative", "bin/program", "not an absolute path"},
		{"missing", filepath.Join(dir, "absent"), "names nothing this case can run"},
		{"a directory", dir, "not an executable file"},
		{"not executable", unexecutable, "not an executable file"},
		{"an executable", executable, ""},
	} {
		for _, require := range []string{"1", ""} {
			t.Run(tc.name+"/required="+require, func(t *testing.T) {
				t.Setenv(prereqtest.RequireEnv, require)
				t.Setenv(env, tc.value)
				var got string
				ended := prereqtest.Run(t, func(tb testing.TB) {
					got = prereqtest.Named(tb, env, "a program a case runs", "the remedy")
				})
				if tc.fatal == "" {
					if ended != (prereqtest.Ended{}) || got != tc.value {
						t.Errorf("Named = %q (ended %+v), want %s", got, ended, tc.value)
					}
					return
				}
				if !strings.Contains(ended.Fatal, tc.fatal) || !strings.Contains(ended.Fatal, env) || ended.Skip != "" {
					t.Errorf("failed %q, skipped %q; want a failure naming %s and saying %q",
						ended.Fatal, ended.Skip, env, tc.fatal)
				}
			})
		}
	}
}

// RUN REPORTS HOW A HELPER ENDED THE CASE: the failure it reported, the skip
// it took, or neither when it returned — and, as the real testing.T does,
// nothing after a Fatal or a Skip runs, so a helper that "fails" and carries
// on is not mistaken for one that stops.
//
// Mutation: return from the recorder's Fatalf without runtime.Goexit, and the
// statement after it runs — red.
func TestRunReportsHowAHelperEndedTheCase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		end   func(testing.TB)
		want  prereqtest.Ended
		after bool // the statement after end runs
	}{
		{"fatal", func(tb testing.TB) { tb.Fatalf("failed %d", 1) }, prereqtest.Ended{Fatal: "failed 1"}, false},
		{"fatal without a format", func(tb testing.TB) { tb.Fatal("failed", 2) }, prereqtest.Ended{Fatal: "failed2"}, false},
		{"skip", func(tb testing.TB) { tb.Skipf("skipped %d", 3) }, prereqtest.Ended{Skip: "skipped 3"}, false},
		{"skip without a format", func(tb testing.TB) { tb.Skip("skipped") }, prereqtest.Ended{Skip: "skipped"}, false},
		{"neither", func(testing.TB) {}, prereqtest.Ended{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			after := false
			got := prereqtest.Run(t, func(tb testing.TB) {
				tc.end(tb)
				after = true
			})
			if got != tc.want || after != tc.after {
				t.Errorf("Run = %+v and the statement after it ran: %v; want %+v and %v", got, after, tc.want, tc.after)
			}
		})
	}
}

// READPINS REFUSES A LINE THAT IS NEITHER A COMMENT NOR AN ASSIGNMENT, a name
// set twice, and an assignment with space or a carriage return around it — an
// indented one, which the recipes reading the same file never read, or one
// whose value they would read with its trailing blank — so it never reads a
// value they would not; what it hands back is every NAME=value line, comments
// and blank lines skipped.
//
// Mutations: skip the duplicate check, and the file setting NODE twice reads
// as its last value — red; trim each line before reading it, and the
// indented assignment is read — red.
func TestReadPinsRefusesALineThatIsNeitherACommentNorAnAssignment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good.env", "# a pin file\n\n  # an indented comment\nNODE_VERSION=2.56.0\nNODE_SHA256=abc\n")
	var pins map[string]string
	if ended := prereqtest.Run(t, func(tb testing.TB) { pins = prereqtest.ReadPins(tb, good) }); ended != (prereqtest.Ended{}) {
		t.Fatalf("ReadPins of a good file ended %+v", ended)
	}
	if len(pins) != 2 || pins["NODE_VERSION"] != "2.56.0" || pins["NODE_SHA256"] != "abc" {
		t.Errorf("ReadPins = %v, want NODE_VERSION=2.56.0 and NODE_SHA256=abc", pins)
	}
	for _, tc := range []struct{ name, body, fatal string }{
		{"neither", "# a pin file\nNODE_VERSION 2.56.0\n", "neither a comment nor NAME=value"},
		{"twice", "NODE_VERSION=2.56.0\nNODE_VERSION=2.57.0\n", "sets NODE_VERSION twice"},
		{"indented", "NODE_VERSION=2.56.0\n  NODE_SHA256=abc\n", "has space or a carriage return around it"},
		{"trailing", "NODE_VERSION=2.56.0 \n", "has space or a carriage return around it"},
		{"crlf", "NODE_VERSION=2.56.0\r\n", "has space or a carriage return around it"},
		{"absent", "", "no such file"},
	} {
		path := filepath.Join(dir, "absent.env")
		if tc.body != "" {
			path = write(tc.name+".env", tc.body)
		}
		ended := prereqtest.Run(t, func(tb testing.TB) { prereqtest.ReadPins(tb, path) })
		if !strings.Contains(ended.Fatal, tc.fatal) {
			t.Errorf("%s: ReadPins ended %+v, want a failure saying %q", tc.name, ended, tc.fatal)
		}
	}
}

package executor_test

import (
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A GEMINI CLI RUN RECORDS ITS GUARD BEFORE ITS HARNESS: a run signed in with
// a key, in a box that loads no other server, records `unshadowed yes` before
// its release, already there as its harness starts; a run signed in by a
// login, whose remote admin controls can add servers, records none.
//
// Mutation: run the guard after the harness, write the line after the
// release, or write it whatever the helper said, and a case goes red.
func TestAGeminiRunRecordsItsGuardBeforeItsHarness(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env        map[string]string
		unshadowed bool
	}{
		"a key":   {map[string]string{"GEMINI_API_KEY": "key-for-tests"}, true},
		"a login": {map[string]string{"GOOGLE_GENAI_USE_GCA": "true"}, false},
	} {
		probe := t.TempDir() + "/guarded-before"
		runner, b := geminiBoxAt(t, geminiCapturedRelease, "")
		record := executor.PathsFor(b).SessionRecord()
		writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/gemini",
			"#!/bin/sh\ncase \"$1\" in --version) echo "+shellword.Quote(geminiCapturedRelease)+"; exit 0;; esac\n"+
				"case ${GEMINI_WRITE_SYSTEM_MD:-false} in false|0) ;; *) echo prompt > \"$GEMINI_WRITE_SYSTEM_MD\"; exit 42;; esac\n"+
				"grep -qx 'unshadowed yes' "+shellword.Quote(record)+" && : > "+shellword.Quote(probe)+"\n"+
				geminiCarryStandIn(t))
		runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin", Env: tc.env})
		text := boxFile(t, b, record)
		at, release := strings.Index(text, "\nunshadowed yes\n"), strings.Index(text, "\nrelease ")
		_, err := os.Stat(probe)
		before := err == nil
		switch {
		case tc.unshadowed && (at < 0 || release < at || !before):
			t.Errorf("%s: the record reads\n%s\nseen by the harness %t; want `unshadowed yes` before the release, "+
				"as the harness starts", name, text, before)
		case !tc.unshadowed && (at >= 0 || before):
			t.Errorf("%s: the record reads\n%s\nwant no `unshadowed` line", name, text)
		}
	}
}

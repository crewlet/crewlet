package executor_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// Session ids as a minting harness spells them: Kimi's are `session_<uuid>`.
const (
	minted      = "session_0b9c1d3e-5f70-4a8b-9c1d-2e3f4a5b6c7d"
	mintedOther = "session_7d6c5b4a-3f2e-4d1c-8b9a-07f5e3d1c9b0"
)

// mintingStart starts a run of a minting harness in a box of its own and
// writes what its script would have left: its record of the session, and its
// report where report is set.
func mintingStart(t *testing.T, b *sandbox.FakeSandbox, runner *executor.Runner, req sandbox.RunRequest,
	record, report string) sandbox.RunHandle {

	t.Helper()
	handle, err := runner.Start(t.Context(), b, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	p := paths(b)
	if err := b.WriteFile(t.Context(), p.SessionRecord(), []byte(record)); err != nil {
		t.Fatalf("writing the record: %v", err)
	}
	if err := b.WriteFile(t.Context(), p.SessionReport(), []byte(report)); err != nil {
		t.Fatalf("writing the report: %v", err)
	}
	return handle
}

// A RUN KILLED BEFORE ITS REPORT IS SESSIONLESS: a minting harness names no
// session as its run starts, and its session is the one its script reports,
// never an id its record names that its harness never reported.
//
// Mutation: name a session at a minting run's start, or read the record's id
// where no report came, and this goes red.
func TestAMintingRunKilledBeforeItsReportIsSessionless(t *testing.T) {
	t.Parallel()
	runner := executor.New(executor.Minter{})
	if !runner.MintsSession() {
		t.Fatal("a minting harness's runner says it does not mint its session")
	}
	b := box(t, runner)
	handle := mintingStart(t, b, runner, sandbox.RunRequest{Brief: "begin"},
		"id "+minted+"\ncontinued no\nwhy \nrelease 1.0.0\n", "")
	if handle.SessionID != "" || handle.FreshSessionID != "" {
		t.Fatalf("a minting run's start named %q and %q, want no session", handle.SessionID, handle.FreshSessionID)
	}
	got, err := runner.ReportedSession(t.Context(), b, handle)
	if err != nil || got != "" {
		t.Fatalf("a run killed before its report collected session %q (%v), want none", got, err)
	}
}

// A REPORTED ID RESUMES: the id a fresh run reported is its session, and a
// later run continuing it names it at its start and works in it, whether its
// script reports it again or reports nothing.
//
// Mutation: drop the report, or have the resume name any other session, and
// this goes red.
func TestAMintingRunsReportedSessionResumes(t *testing.T) {
	t.Parallel()
	runner := executor.New(executor.Minter{})
	b := box(t, runner)
	first := mintingStart(t, b, runner, sandbox.RunRequest{Brief: "begin"}, "continued no\n", minted+"\n")
	reported, err := runner.ReportedSession(t.Context(), b, first)
	if err != nil || reported != minted {
		t.Fatalf("the fresh run's session is %q (%v), want the reported %s", reported, err, minted)
	}
	for name, report := range map[string]string{"reported again": reported + "\n", "reporting nothing": ""} {
		resumed := mintingStart(t, b, runner, sandbox.RunRequest{Brief: "carry on", Resume: reported},
			"continued yes\n", report)
		if resumed.SessionID != reported {
			t.Fatalf("%s: the resume's start named %q, want %s", name, resumed.SessionID, reported)
		}
		got, err := runner.ReportedSession(t.Context(), b, resumed)
		if err != nil || got != reported {
			t.Errorf("%s: the resume's session is %q (%v), want %s", name, got, err, reported)
		}
	}
}

// A REPORT THE ENGINE CANNOT TRUST IS REFUSED, failing the run closed: a resume
// reports nothing new, a resume's fall back never reports the session it
// fell back from, and a report is one id on one line.
//
// Mutation: take a resume's differing report, a fall back's continued id, or a
// report of two lines, and a case goes red.
func TestAMintingRunsUntrustedReportIsRefused(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ record, report string }{
		"a resume reporting another session":  {"continued yes\n", mintedOther + "\n"},
		"a fall back reporting the continued": {"continued no\n", minted + "\n"},
		"two ids":                             {"continued no\n", mintedOther + "\n" + minted + "\n"},
		"no id a harness names":               {"continued no\n", "../session\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.New(executor.Minter{})
			b := box(t, runner)
			handle := mintingStart(t, b, runner, sandbox.RunRequest{Brief: "carry on", Resume: minted},
				c.record, c.report)
			if got, err := runner.ReportedSession(t.Context(), b, handle); !errors.Is(err, executor.ErrSessionReport) {
				t.Fatalf("the report was read as session %q (%v), want it refused", got, err)
			}
		})
	}
}

// A REPORT IS FOUND WHOLE OR NOT AT ALL: a minting script stages the id and
// renames it over the report, so a reader never takes half an id.
//
// Mutation: write the report in place, and this goes red.
func TestAMintingScriptStagesItsReportThenRenamesIt(t *testing.T) {
	t.Parallel()
	p := executor.Paths{Home: "/box"}
	script := executor.ReportSession(p, "id")
	staged, report := sandbox.ShellQuote(executor.SessionReportStaging(p)), sandbox.ShellQuote(p.SessionReport())
	if !strings.HasPrefix(script, `printf '%s\n' "$id" > `+staged+" && ") ||
		!strings.HasSuffix(script, "mv -f "+staged+" "+report) {
		t.Fatalf("the report is not staged and renamed into place:\n%s", script)
	}
}

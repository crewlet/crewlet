package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/plane"
	"github.com/crewlet/crewlet/internal/provision"
)

const gitlabCompanyDoc = `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.example.com
    signing_secret: "${GITLAB_SIGNING_SECRET}"
    provisioning:
      group: nimbus
roles:
  - name: SWE
    handle: swe
    llm: main
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "${GITLAB_TOKEN_SWE}"
  - name: PM
    handle: pm
    llm: main
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "glpat-managed-by-hand"
`

func gitlabCompanyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(gitlabCompanyDoc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func provisionCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	err := run(append([]string{"gitlab", "provision"}, args...), &out, &errs)
	return out.String(), errs.String(), err
}

// A DRY RUN PRINTS THE PLAN AND TOUCHES NOTHING — and it is the SAME plan
// the real run uses, so it cannot disagree with what would happen.
func TestADryRunReportsWithoutReachingTheVendor(t *testing.T) {
	t.Parallel()
	company := gitlabCompanyFile(t)
	out, errs, err := provisionCmd(t, company, "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "swe") || !strings.Contains(out, "GITLAB_TOKEN_SWE") {
		t.Errorf("the plan does not name the provisionable seat:\n%s", out)
	}
	if !strings.Contains(out, "nothing was created") {
		t.Errorf("the dry run does not say it did nothing:\n%s", out)
	}
	// THE SEAT MANAGED BY HAND IS REPORTED, not silently dropped: it will
	// keep being skipped, and an operator has to know which.
	if !strings.Contains(out, "pm") || !strings.Contains(out, "a literal") {
		t.Errorf("the literal seat is not reported:\n%s", out)
	}
	// AND ITS CREDENTIAL DOES NOT APPEAR. This output is pasted into
	// tickets.
	if strings.Contains(out, "glpat-managed-by-hand") {
		t.Errorf("the report leaked a credential:\n%s", out)
	}
}

// A DRY RUN NEEDS NO SINK, because it mints nothing — requiring one would
// make the safe command harder to run than the dangerous one.
func TestADryRunNeedsNoSink(t *testing.T) {
	t.Parallel()
	if _, _, err := provisionCmd(t, gitlabCompanyFile(t), "-dry-run"); err != nil {
		t.Fatalf("a dry run was refused for having no sink: %v", err)
	}
}

// A REAL RUN WITH NOWHERE TO PUT WHAT IT MINTS IS REFUSED BEFORE IT TOUCHES
// ANYTHING. The alternative creates live credentials at the vendor and
// prints none of them, and every one has to be found and revoked by hand.
func TestARealRunWithoutASinkIsRefused(t *testing.T) {
	t.Parallel()
	_, _, err := provisionCmd(t, gitlabCompanyFile(t), "-admin-token", "t")
	if err == nil {
		t.Fatal("a run with no sink was accepted")
	}
	for _, want := range []string{"-secret-store", "-env-file", "-print"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not offer %s: %v", want, err)
		}
	}
}

// TWO SINKS ARE REFUSED RATHER THAN ORDERED: writing to both doubles the
// copies of a live credential, and a precedence rule would put it somewhere
// the operator did not ask for.
func TestTwoSinksAreRefused(t *testing.T) {
	t.Parallel()
	_, _, err := provisionCmd(t, gitlabCompanyFile(t),
		"-print", "-env-file", filepath.Join(t.TempDir(), ".env"),
		"-admin-token", "t")
	if err == nil {
		t.Fatal("two sinks at once were accepted")
	}
	if !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("the refusal is unclear: %v", err)
	}
}

// A RUN THAT RECORDED SOMETHING SAYS WHAT IS LEFT TO DO.
//
// "Recorded in the encrypted secret store" reads as finished, and it is not:
// the engine resolves through a snapshot rebuilt on apply, so a value written
// there reaches a running process at the next apply and not before. The
// answer differs per sink and only one of the three is "source a file", so
// the sink is asked rather than the report guessing.
//
// A run that changed nothing says nothing. An operator who is told to restart
// after every no-op run learns to skip the line, and then misses the run that
// meant it.
func TestARunReportsWhatIsLeftToDoOnlyWhenItRecordedSomething(t *testing.T) {
	t.Parallel()
	sink, err := provision.NewPrintSink(io.Discard)
	if err != nil {
		t.Fatalf("NewPrintSink: %v", err)
	}

	var recorded bytes.Buffer
	printResult(&recorded, &gitlab.Result{Rotated: []string{"swe"}, Recorded: 1}, sink)
	if !strings.Contains(recorded.String(), "Next:") {
		t.Errorf("a run that minted a credential did not say what is left to "+
			"do:\n%s", recorded.String())
	}
	if !strings.Contains(recorded.String(), sink.NextStep()) {
		t.Errorf("the report did not ask the sink:\n%s", recorded.String())
	}

	var unchanged bytes.Buffer
	printResult(&unchanged, &gitlab.Result{Kept: []string{"swe"}}, sink)
	if strings.Contains(unchanged.String(), "Next:") {
		t.Errorf("a run that changed nothing told the operator to act:\n%s",
			unchanged.String())
	}
}

// A DRY RUN SAYS WHAT IT WOULD DO TO THE SIGNING SECRET.
//
// The most consequential thing a provisioning run can do is replace the key
// a working hook signs with — every delivery in flight then fails
// verification until the new value reaches the engine. A dry run that
// reported the seat plan and said nothing about that was silent about the
// one outcome an operator most needs warning of.
func TestADryRunSaysWhatItWouldDoToTheSigningSecret(t *testing.T) {
	company := gitlabCompanyFile(t)
	for _, tc := range []struct {
		name  string
		args  []string
		setup func(t *testing.T)
		want  string
	}{
		{
			name: "no public url leaves it alone",
			args: []string{"-dry-run"},
			want: "untouched",
		},
		{
			name: "an unset reference will be minted",
			args: []string{"-dry-run", "-public-url", "https://crewlet.example.com"},
			want: "WILL BE MINTED into GITLAB_SIGNING_SECRET",
		},
		{
			name: "a resolved one is reused",
			args: []string{"-dry-run", "-public-url", "https://crewlet.example.com"},
			setup: func(t *testing.T) {
				t.Setenv("GITLAB_SIGNING_SECRET",
					"whsec_ZTJlLXRlc3Qtc2lnbmluZy1rZXktMzItYnl0ZXMhISE=")
			},
			want: "reused",
		},
		{
			// The warning that has to be loud: -rotate re-points the hook
			// at a key the running engine does not hold yet.
			name: "rotate replaces a working one",
			args: []string{"-dry-run", "-public-url", "https://crewlet.example.com", "-rotate"},
			setup: func(t *testing.T) {
				t.Setenv("GITLAB_SIGNING_SECRET",
					"whsec_ZTJlLXRlc3Qtc2lnbmluZy1rZXktMzItYnl0ZXMhISE=")
			},
			want: "WILL BE ROTATED into GITLAB_SIGNING_SECRET",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			out, errs, err := provisionCmd(t, append([]string{company}, tc.args...)...)
			if err != nil {
				t.Fatalf("dry run: %v (%s)", err, errs)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("the plan does not say %q:\n%s", tc.want, out)
			}
			if !strings.Contains(out, "nothing was created") {
				t.Errorf("the dry run does not say it did nothing:\n%s", out)
			}
		})
	}
}

// THE PRINT SINK'S DESTINATION IS PER RUN, NOT PER PROCESS.
//
// It used to be per process: each provision command assigned the writer to a
// package variable just before opening its sink, and open read it back. Two
// runs in one process therefore shared one destination and raced on the
// pointer getting there, which is what the race detector reported once the
// CLI's own tests started running in parallel. Opening two sinks here is the
// whole assertion — under the old shape neither would have had a writer at
// all, because nothing outside a command ever set the variable.
func TestEachRunPrintsToTheWriterItWasGiven(t *testing.T) {
	t.Parallel()
	openPrintSink := func(t *testing.T, w io.Writer) provision.TokenSink {
		t.Helper()
		fs := flag.NewFlagSet("provision", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		sinks := addSinkFlags(fs)
		if err := fs.Parse([]string{"-print"}); err != nil {
			t.Fatalf("parse: %v", err)
		}
		sink, closeSink, err := sinks.open(t.Context(), w)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(closeSink)
		return sink
	}

	var first, second bytes.Buffer
	runs := []struct {
		out  *bytes.Buffer
		sink provision.TokenSink
		name string
	}{
		{out: &first, sink: openPrintSink(t, &first), name: "CREWLET_GITLAB_TOKEN_CEO"},
		{out: &second, sink: openPrintSink(t, &second), name: "CREWLET_GITLAB_TOKEN_CTO"},
	}
	for _, run := range runs {
		if err := run.sink.Record(t.Context(), run.name, "glpat-"+run.name); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	for i, run := range runs {
		other := runs[1-i]
		if !strings.Contains(run.out.String(), run.name) {
			t.Errorf("the run's own credential did not reach its writer: %q", run.out.String())
		}
		if strings.Contains(run.out.String(), other.name) {
			t.Errorf("one run's credential was printed into the other run's "+
				"output, so an operator is handed a secret they did not mint: %q",
				run.out.String())
		}
	}
}

// THE SEATS' OWN TOKENS ARE WHAT THIS RUN MINTS, so it cannot bootstrap
// itself from them — the refusal says so rather than failing at the first
// 401.
func TestARunWithoutAnAdministratorTokenSaysWhy(t *testing.T) {
	t.Parallel()
	_, _, err := provisionCmd(t, gitlabCompanyFile(t), "-print")
	if err == nil {
		t.Fatal("a run with no administrator token was accepted")
	}
	if !strings.Contains(err.Error(), "GITLAB_ADMIN_TOKEN") {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
}

func TestProvisionNeedsExactlyOneCompanyDocument(t *testing.T) {
	t.Parallel()
	company := gitlabCompanyFile(t)
	for _, args := range [][]string{
		{},
		{company, company},
		{"-dry-run"},
	} {
		if _, _, err := provisionCmd(t, args...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

func TestAnUnknownIntegrationCommandIsRefused(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	if err := run([]string{"gitlab", "nonesuch"}, &out, &errs); err == nil {
		t.Fatal("an unknown gitlab command was accepted")
	}
}

const chatCompanyDoc = `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
integrations:
  mattermost:
    enabled: true
    url: https://chat.example.com
    team: nimbus
    provisioning:
      username_prefix: "agent-"
      channels: [general]
roles:
  - name: CEO
    handle: ceo
    llm: main
    integrations:
      mattermost:
        bot_token: "${MM_TOKEN_CEO}"
        channel: leadership
`

// THE MATTERMOST COMMAND SHARES THE SAME SAFETY RULES: a dry run reaches
// nothing, and a real run with nowhere to put what it mints is refused
// before it touches the instance.
func TestTheMattermostCommandSharesTheProvisioningRules(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(chatCompanyDoc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	chat := func(args ...string) (string, error) {
		var out, errs bytes.Buffer
		err := run(append([]string{"mattermost", "provision"}, args...), &out, &errs)
		return out.String(), err
	}

	out, err := chat(path, "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(out, "ceo") || !strings.Contains(out, "MM_TOKEN_CEO") {
		t.Errorf("the plan does not name the seat:\n%s", out)
	}
	if !strings.Contains(out, "nothing was created") {
		t.Errorf("the dry run does not say it did nothing:\n%s", out)
	}

	if _, err := chat(path, "-admin-token", "t"); err == nil {
		t.Fatal("a run with no sink was accepted")
	}
	if _, err := chat(path, "-print"); err == nil {
		t.Fatal("a run with no administrator token was accepted")
	} else if !strings.Contains(err.Error(), "MATTERMOST_ADMIN_TOKEN") {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
}

// --- the Plane report's people table --------------------------------------

// THE TABLE IS FOR PEOPLE, and its count must not read as a workspace
// census.
//
// It is the member list as the run FOUND it, before it created anything —
// right for people, because a run never creates one, and wrong as a total:
// printed under a line saying eight service accounts were created, a
// "Workspace members (2)" reads as a run that half-failed. Measured against
// a real instance whose API reported ten.
func TestThePeopleTableLeavesOutTheServiceAccounts(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printMembers(&out, []plane.Account{
		{Username: "founder", ID: "912601e1", Email: "founder@example.com"},
		{Username: "bot_user_c168db5c", ID: "f455f222", Email: "bot@example.com", IsBot: true},
	})

	text := out.String()
	if !strings.Contains(text, "(1)") {
		t.Errorf("the count includes the service accounts:\n%s", text)
	}
	if strings.Contains(text, "bot_user_c168db5c") {
		t.Errorf("a service account reached the people table:\n%s", text)
	}
	if !strings.Contains(text, "912601e1") {
		t.Errorf("the person's id — the whole point of the table — is missing:\n%s", text)
	}
}

// A WORKSPACE WITH NO PEOPLE PRINTS NO TABLE, rather than a heading over
// nothing.
func TestThePeopleTableIsSkippedWhenThereAreNone(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printMembers(&out, []plane.Account{{Username: "bot", ID: "x", IsBot: true}})
	if out.Len() != 0 {
		t.Errorf("printed a table with no people in it:\n%s", out.String())
	}
}

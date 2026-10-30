package main

import (
	"bytes"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/slack"
)

const gitlabCompanyDoc = `
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    main:
      model: claude-sonnet-5
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
// ANYTHING. The alternative creates live credentials at the third-party app and
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
	if !printsANextStep(recorded.String()) {
		t.Errorf("a run that minted a credential did not say what is left to "+
			"do:\n%s", recorded.String())
	}
	if !strings.Contains(recorded.String(), sink.NextStep()) {
		t.Errorf("the report did not ask the sink:\n%s", recorded.String())
	}

	var unchanged bytes.Buffer
	printResult(&unchanged, &gitlab.Result{Kept: []string{"swe"}}, sink)
	if printsANextStep(unchanged.String()) {
		t.Errorf("a run that changed nothing told the operator to act:\n%s",
			unchanged.String())
	}
}

// A CHAT RUN SAYS WHAT IS LEFT TO DO AS A GITLAB RUN DOES — Mattermost's and
// Slack's reports stopped at where the values went, so an operator who chose
// -secret-store was told nothing while every new bot stayed offline until the
// engine's next apply. Only a run that recorded something says it.
func TestAChatRunReportsWhatIsLeftToDoOnlyWhenItRecordedSomething(t *testing.T) {
	t.Parallel()
	sink, err := provision.NewPrintSink(io.Discard)
	if err != nil {
		t.Fatalf("NewPrintSink: %v", err)
	}
	reports := map[string]func(w io.Writer, recorded int){
		"slack": func(w io.Writer, recorded int) {
			printSlackResult(w, &slack.Result{Recorded: recorded}, "slack-apps.json", sink)
		},
	}
	for name, report := range reports {
		var recorded, unchanged bytes.Buffer
		report(&recorded, 1)
		if !strings.Contains(recorded.String(), "Next: "+sink.NextStep()) {
			t.Errorf("%s: a run that recorded a credential did not say what is "+
				"left to do:\n%s", name, recorded.String())
		}
		report(&unchanged, 0)
		if printsANextStep(unchanged.String()) {
			t.Errorf("%s: a run that recorded nothing told the operator to act:\n%s",
				name, unchanged.String())
		}
	}
}

// printsANextStep is whether a report has a Next: LINE — the step the run
// asks for, which [printNextStep] prints at the start of its own line. A
// line that only mentions one, as the kept line does for a run with the
// command's minting flag, asks for nothing.
func printsANextStep(report string) bool {
	for line := range strings.Lines(report) {
		if strings.HasPrefix(line, "Next: ") {
			return true
		}
	}
	return false
}

// THE KEPT LINE NAMES THE COMMAND'S OWN FLAG, AND NO RESTART.
//
// GitLab, Mattermost and Slack share the line a re-run prints for the seats
// it left alone, and it named -rotate for all three: Slack has no such flag
// (it mints afresh with -reinstall), so that operator was handed a flag the
// command refuses. It also said to restart the engine after, which is the
// step for -env-file and -print alone — with -secret-store a reload of a
// running node is what the engine needs, and the run that mints says so on
// its Next: line.
func TestTheKeptLineNamesTheCommandsOwnFlag(t *testing.T) {
	t.Parallel()
	sink, err := provision.NewPrintSink(io.Discard)
	if err != nil {
		t.Fatalf("NewPrintSink: %v", err)
	}
	kept := []string{"swe"}
	reports := []struct {
		name, flag, other string
		report            func(w io.Writer)
	}{
		{"gitlab", "-rotate", "-reinstall", func(w io.Writer) {
			printResult(w, &gitlab.Result{Kept: kept}, sink)
		}},
		{"slack", "-reinstall", "-rotate", func(w io.Writer) {
			printSlackResult(w, &slack.Result{Kept: kept}, "slack-apps.json", sink)
		}},
	}
	for _, c := range reports {
		var out bytes.Buffer
		c.report(&out)
		got := out.String()
		if !strings.Contains(got, "Left 1 seat(s) alone") {
			t.Fatalf("%s: the report does not say it kept the seat:\n%s", c.name, got)
		}
		if !strings.Contains(got, "pass "+c.flag+" to mint fresh ones") {
			t.Errorf("%s: the kept line does not name %s, the command's own "+
				"flag:\n%s", c.name, c.flag, got)
		}
		if strings.Contains(got, c.other) {
			t.Errorf("%s: the kept line names %s, another command's flag:\n%s",
				c.name, c.other, got)
		}
		if strings.Contains(got, "restart") {
			t.Errorf("%s: the kept line asks for a restart, which -secret-store "+
				"does not need:\n%s", c.name, got)
		}
		if printsANextStep(got) {
			t.Errorf("%s: a run that kept every seat asked for a next step:\n%s",
				c.name, got)
		}
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

// -rotate SAYS EVERYTHING IT REPLACES. The dry run above plans the signing
// secret ROTATED under it, and the flag's own help named only the seats'
// tokens — so an operator rotating tokens on a schedule learned from the plan,
// or from the hook that stopped verifying, that the key went too.
func TestTheRotateFlagNamesTheSigningSecretItReplaces(t *testing.T) {
	t.Parallel()
	_, errs, err := cliIn(t, noEnviron, "gitlab", "provision", "-h")
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("gitlab provision -h = %v, want the help", err)
	}
	_, help, found := strings.Cut(errs, "  -rotate\n")
	help, _, _ = strings.Cut(help, "\n")
	if !found || !strings.Contains(help, "token for every seat") ||
		!strings.Contains(help, "signing secret") {
		t.Errorf("-rotate's help does not name both things it replaces: %q\n%s", help, errs)
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
		companySecrets, err := openCompanyStore(noEnviron, sinks.store,
			"crewlet gitlab provision")
		if err != nil {
			t.Fatalf("open the run's store: %v", err)
		}
		t.Cleanup(companySecrets.Close)
		sink, err := sinks.open(t.Context(), companySecrets, w)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
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
  sandbox: {fake: true}
  llm:
    main:
      model: claude-sonnet-5
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

// `crewlet mattermost provision` IS RETIRED, AND SAYS WHERE ITS WORK WENT.
// Each agent's bot is per-seat access now, managed from the integration's
// Agents tab and applied by the reconcile loop, so a command line copied from
// an older guide is told that rather than that the command never existed.
func TestTheRetiredMattermostProvisionSaysWhereItsWorkWent(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	err := run([]string{"mattermost", "provision", "company.yaml"}, &out, &errs)
	if err == nil || errors.Is(err, errUnknownSub) ||
		!strings.Contains(err.Error(), "Agents tab") ||
		!strings.Contains(err.Error(), "crewlet mattermost doctor") {
		t.Fatalf("err = %v, want the retirement and where the work went", err)
	}
	out.Reset()
	errs.Reset()
	if err := run([]string{"mattermost"}, &out, &errs); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("usage: %v", err)
	}
	if usage := errs.String(); !strings.Contains(usage, "crewlet mattermost doctor") ||
		!strings.Contains(usage, "is retired") {
		t.Errorf("the usage does not list doctor and the retirement:\n%s", usage)
	}
}

// WHERE PEOPLE BROWSE REACHES THE DOCTOR from the flag, or from the variable
// the compose stack sets the Site URL from — the one fact about browsers the
// engine's own configuration cannot give it. Here the server's Site URL is
// localhost and people browse another address: told nothing, the doctor can
// only judge the Site URL on its own; told by either, it fails it.
//
// Not parallel: it sets the process environment.
func TestTheDoctorIsToldWherePeopleBrowse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/system/ping":
			w.Write([]byte(`{"status":"OK"}`))
		case "/api/v4/config/client":
			w.Write([]byte(`{"SiteURL":"http://localhost:8065"}`))
		case "/api/v4/users/me":
			w.Write([]byte(`{"id":"user-ceo","username":"agent-ceo"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"not here"}`))
		}
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "company.yaml")
	doc := strings.Replace(chatCompanyDoc, "url: https://chat.example.com", "url: "+server.URL, 1)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("MM_TOKEN_CEO", "tok-ceo")
	siteLine := func(args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		// The run fails whatever it is told, because this stand-in
		// upgrades no socket; only the site url line is the question.
		_ = run(append([]string{"mattermost", "doctor", path}, args...), &out, &errs)
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.Contains(line, "site url") {
				return line
			}
		}
		t.Fatalf("no site url line in:\n%s\n%s", out.String(), errs.String())
		return ""
	}

	// An address refused at once, so the browser probe sent there fails
	// fast rather than waiting out a dial timeout.
	const browsed = "http://127.0.0.1:1"
	t.Setenv("MATTERMOST_PUBLIC_URL", "")
	if line := siteLine(); !strings.HasPrefix(line, "ok") || !strings.Contains(line, "-browser-url") {
		t.Errorf("told nothing: %s", line)
	}
	if line := siteLine("-browser-url", browsed); !strings.HasPrefix(line, "FAIL") ||
		!strings.Contains(line, browsed) {
		t.Errorf("told by the flag: %s", line)
	}
	t.Setenv("MATTERMOST_PUBLIC_URL", browsed)
	if line := siteLine(); !strings.HasPrefix(line, "FAIL") || !strings.Contains(line, browsed) {
		t.Errorf("told by MATTERMOST_PUBLIC_URL: %s", line)
	}
	// The flag outranks the variable, as every flag here does.
	if line := siteLine("-browser-url", "http://localhost:8065"); !strings.HasPrefix(line, "ok") {
		t.Errorf("the flag did not outrank the variable: %s", line)
	}
}

// EVERY VENDOR THE BINARY ROUTES HAS COMMANDS, AND EVERY VENDOR WITH
// COMMANDS IS ROUTED.
//
// run()'s case list and [vendorCommands] are two hand-written lists naming
// the same thing, and a vendor in one and not the other fails in a way
// nobody sees: added to run() alone, `crewlet <vendor>` answers "no commands
// for" instead of a usage line; added to the table alone, the whole vendor
// is unreachable and the table reads as though it works.
func TestEveryRoutedVendorHasCommands(t *testing.T) {
	t.Parallel()

	routed := map[string]bool{}
	for _, cmd := range dispatchedCommands(t) {
		if _, ok := vendorCommands[cmd]; ok {
			routed[cmd] = true
		}
	}
	for vendor := range vendorCommands {
		if !routed[vendor] {
			t.Errorf("vendorCommands has %q but run() never dispatches to "+
				"runIntegration for it, so none of its commands can be reached",
				vendor)
		}
	}
	// The other direction: a vendor run() routes with nothing behind it.
	// Read off the same case clause, so a vendor added there is caught
	// whether or not anyone remembered this test.
	for _, cmd := range dispatchedCommands(t) {
		if !isVendorCase(t, cmd) {
			continue
		}
		if len(vendorCommands[cmd]) == 0 {
			t.Errorf("run() dispatches %q to runIntegration but vendorCommands "+
				"has no entry, so every invocation answers an internal error", cmd)
		}
	}
}

// isVendorCase reports whether a command in run() is one of the vendor
// cases — the clause whose body calls runIntegration.
func isVendorCase(t *testing.T, command string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing main.go: %v", err)
	}
	var vendor bool
	ast.Inspect(file, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		var calls bool
		ast.Inspect(clause, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "runIntegration" {
				calls = true
			}
			return true
		})
		if !calls {
			return true
		}
		for _, expr := range clause.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquoting %s: %v", lit.Value, err)
			}
			if value == command {
				vendor = true
			}
		}
		return true
	})
	return vendor
}

// A VENDOR'S USAGE NAMES THE COMMANDS IT ACTUALLY HAS — no more.
//
// The usage text used to be written by hand beside the dispatch switch, and
// it opened with a `provision` line for every vendor. Confluence has an
// import and no provision, so `crewlet confluence` advertised a command that
// answers "unknown confluence command" when you run it.
func TestVendorUsageNamesOnlyRealCommands(t *testing.T) {
	t.Parallel()

	for vendor, commands := range vendorCommands {
		var out bytes.Buffer
		if err := runIntegration(invocation{environ: noEnviron}, vendor, nil, io.Discard, &out); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s: bare invocation returned %v, want the help sentinel", vendor, err)
		}
		usage := out.String()
		for _, command := range commands {
			if !strings.Contains(usage, "crewlet "+vendor+" "+command.sub) {
				t.Errorf("%s: usage never names %q:\n%s", vendor, command.sub, usage)
			}
			if command.run == nil {
				// An advertised row with nothing behind it is the same
				// defect one level in: usage names it, and running it
				// panics instead of answering.
				t.Errorf("%s: %q is advertised and runs nothing", vendor, command.sub)
				continue
			}
			// AND IT REACHES THAT RUNNER. `-h` makes every subcommand
			// print its flags and stop, which is the cheapest proof that
			// the dispatch found it rather than falling through.
			if err := runIntegration(invocation{environ: noEnviron}, vendor, []string{command.sub, "-h"},
				io.Discard, io.Discard); errors.Is(err, errUnknownSub) {
				t.Errorf("%s: usage names %q but dispatch does not reach it",
					vendor, command.sub)
			}
		}
		// AN UNKNOWN SUBCOMMAND IS AN ERROR, not a usage dump: a typo
		// answered with help reads as though the command was accepted.
		if err := runIntegration(invocation{environ: noEnviron}, vendor, []string{"nonesuch"},
			io.Discard, io.Discard); !errors.Is(err, errUnknownSub) {
			t.Errorf("%s: an unknown subcommand returned %v, want an unknown-command error",
				vendor, err)
		}
		// Every subcommand spelled in the usage must be dispatchable.
		for line := range strings.SplitSeq(strings.TrimSpace(usage), "\n") {
			// "usage: crewlet <vendor> <sub> …", and the continuation
			// lines are the same without the lead — so the subcommand is
			// found relative to "crewlet", not from the start of the line.
			fields := strings.Fields(line)
			at := slices.Index(fields, "crewlet")
			if at < 0 || len(fields) < at+3 {
				t.Fatalf("%s: unreadable usage line %q", vendor, line)
			}
			sub := fields[at+2]
			if !slices.ContainsFunc(commands, func(c vendorCommand) bool {
				return c.sub == sub
			}) {
				t.Errorf("%s: usage advertises %q, which dispatches to nothing:\n%s",
					vendor, sub, usage)
			}
		}
	}
}

// A REFUSAL NAMES WHAT TO CHANGE, and there are two different answers.
//
// The message said "no -public-url" and named nothing else, which was already
// only one of the ways to reach it: the value comes from the flag OR from the
// deployment's own `api.external_url`. There used to be a THIRD — the address
// was a Tier B field, so it could be SET and still resolve to nothing, a whole
// ${VAR} this process could not see becoming "" rather than the text of the
// variable — and moving it to Tier A, which resolves before it decodes,
// removed that state rather than the message for it.
func TestTheMissingPublicBaseRefusalNamesTheRightThing(t *testing.T) {
	t.Parallel()
	t.Run("no Tier A was read at all", func(t *testing.T) {
		got := noPublicBase(nil)
		if !strings.Contains(got, "-public-url") || !strings.Contains(got, "-config") {
			t.Errorf("the refusal names only one of the two sources: %q", got)
		}
	})
	t.Run("a Tier A that sets no address", func(t *testing.T) {
		boot := config.DefaultBootstrap()
		got := noPublicBase(&boot)
		if !strings.Contains(got, "api.external_url") {
			t.Errorf("the refusal does not name the field it came from: %q", got)
		}
		if !strings.Contains(got, "-public-url") {
			t.Errorf("the refusal does not name the one-off override: %q", got)
		}
	})
	t.Run("the flag wins only when it was typed", func(t *testing.T) {
		boot := config.DefaultBootstrap()
		boot.API.ExternalURL = "https://from-the-file.example.com/"
		if got := webhookBase("", &boot); got != "https://from-the-file.example.com" {
			t.Errorf("an untyped flag took the deployment's address as %q", got)
		}
		if got := webhookBase("https://typed.example.com/", &boot); got != "https://typed.example.com" {
			t.Errorf("a typed flag did not win: %q", got)
		}
		if got := webhookBase("", nil); got != "" {
			t.Errorf("no flag and no Tier A produced %q, want empty", got)
		}
		if got := webhookBaseKey(nil); got != "api.external_url" {
			t.Errorf("with no Tier A a pass is told to set %q, want the "+
				"key of a deployment with no public listener", got)
		}
	})
}

// BEHIND A PUBLIC LISTENER A VENDOR IS HANDED THE PUBLIC ADDRESS, and a refusal
// names the key that address is read from.
//
// Once `api.public` is set every webhook route is served only on that listener
// and `api.external_url` answers 404 for each of them — so a command that built
// its hooks, manifests and redirects on the person's address registered every
// one of them where nothing delivers, while the reconcile loop, reading the
// vendors' address, re-pointed them on its next tick. And a Tier A with a public
// listener and no address of its own was told to set `api.external_url`, which
// was set, and which nothing building a vendor URL reads.
func TestBehindAPublicListenerAVendorIsHandedThePublicAddress(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	boot.API.ExternalURL = "https://admin.example.com/"
	boot.API.Public = config.APIPublic{Port: 8443, ExternalURL: "https://hooks.example.com/"}

	if got := webhookBase("", &boot); got != "https://hooks.example.com" {
		t.Errorf("an untyped flag built vendor URLs on %q, want the public "+
			"listener's address", got)
	}
	if got := webhookBase("https://typed.example.com", &boot); got != "https://typed.example.com" {
		t.Errorf("a typed flag did not win over the public listener: %q", got)
	}
	if got := webhookBaseKey(&boot); got != "api.public.external_url" {
		t.Errorf("a pass with no address is told to set %q, want the public "+
			"listener's key", got)
	}

	// THE PUBLIC LISTENER WITH NO ADDRESS OF ITS OWN, which Tier A allows
	// off an ingress node: there is no vendor address at all — never the
	// person's borrowed — and the refusal names the key that is missing.
	boot.API.Public.ExternalURL = ""
	if got := webhookBase("", &boot); got != "" {
		t.Errorf("a public listener with no address built vendor URLs on %q, "+
			"want none", got)
	}
	got := noPublicBase(&boot)
	if !strings.Contains(got, "`api.public.external_url`") {
		t.Errorf("the refusal does not name the key the address is read from: %q", got)
	}
	if strings.Contains(got, "`api.external_url`") {
		t.Errorf("the refusal names the person's address, which is set and "+
			"which no vendor URL is built on: %q", got)
	}
}

// THE DOCUMENT DECIDES WHERE ACCOUNTS ARE OWNED, and the flag overrides it
// for one invocation.
//
// `-mode` used to be the only source, which made it a fact only the person
// who typed it knew. The engine provisions the same company from the same
// document and reads no flags, so a company whose accounts are
// instance-owned had the command line doing one thing and the loop another —
// minting through a group that does not own the account, and deleting down a
// route that answers 404 as success.
func TestTheServiceAccountModeComesFromTheDocumentUnlessTheFlagSaysOtherwise(t *testing.T) {
	t.Parallel()
	instance := &config.GitLab{Provisioning: &config.GitLabProvisioning{
		Group: "nimbus", Mode: config.GitLabModeInstance,
	}}
	plain := &config.GitLab{Provisioning: &config.GitLabProvisioning{Group: "nimbus"}}

	if got := accountMode(instance, false, ""); got != gitlab.ModeInstance {
		t.Errorf("an untyped flag over an instance-owned company chose %q", got)
	}
	if got := accountMode(instance, true, string(gitlab.ModeGroup)); got != gitlab.ModeGroup {
		t.Errorf("-mode group over an instance-owned company chose %q", got)
	}
	if got := accountMode(plain, false, ""); got != gitlab.ModeGroup {
		t.Errorf("a company that says nothing chose %q, want the group route", got)
	}
	if got := accountMode(nil, false, ""); got != gitlab.ModeGroup {
		t.Errorf("no gitlab block at all chose %q, want the group route", got)
	}
}

// outrankRoute is a store a provisioning run reads through that holds held:
// the args that send the run there and the environment it runs in, and the
// node, where the route is one.
type outrankRoute struct {
	name string
	open func(t *testing.T, held map[string]string) (args []string, node *revealNode)
	// says is what a refusal names the store by.
	says func(args []string, node *revealNode) string
}

var outrankRoutes = []outrankRoute{
	{
		name: "through a running node",
		open: func(t *testing.T, held map[string]string) ([]string, *revealNode) {
			node := newRevealNode(t, held)
			return []string{"-api", node.server.URL,
				"-config", filepath.Join(t.TempDir(), "absent.yaml")}, node
		},
		says: func(_ []string, node *revealNode) string {
			return "read through the running node at " + node.server.URL
		},
	},
	{
		name: "on the fleet's store, with the engine stopped",
		open: func(t *testing.T, held map[string]string) ([]string, *revealNode) {
			cfg := bootstrapWithKeyring(t, "k1")
			for name, value := range held {
				if _, errs, err := secretsCmd(t, cfg, "set", name, "-value", value); err != nil {
					t.Fatalf("seed %s: %v (%s)", name, err, errs)
				}
			}
			return []string{"-config", cfg}, nil
		},
		says: func(args []string, _ *revealNode) string {
			boot, err := loadBootstrapForStore(args[len(args)-1])
			if err != nil {
				return err.Error()
			}
			return "the fleet's store, reached through this node's broker on " +
				boot.Stream.StoreDir + " — this node's engine is stopped"
		},
	},
}

// A SINK THE COMPANY'S STORE OUTRANKS IS REFUSED BEFORE THE VENDOR, where the
// store holds a name the run can mint into.
//
// The engine resolves every ${VAR} from the store before its environment, so a
// value an -env-file or -print run mints for a name the store holds is never
// read: the vendor holds the new value and the engine goes on with the stored
// one — a seat's earlier token retired under it, a hook re-keyed with a key
// the engine does not hold. So each provisioner, through a running node and on
// the fleet's store reached directly with the engine stopped alike, refuses
// such a run naming the store, the names, the sink and -secret-store, and:
//
//   - no vendor is reached — every vendor address here fails the case on any
//     request, and Slack's run, whose vendor is no address this case can
//     name, writes no ledger, which its first step at Slack would;
//   - nothing is printed but the run's route line: not its plan;
//   - no env file is written, and nothing is written through the node.
//
// Mutation: let refuseOutranked through, and every row goes red; leave the
// seats' tokens out of what GitLab, Mattermost or Slack can mint into, or the
// flag for a fresh value out of a webhook secret's, and that row does; count
// Slack's signing secret only where an install runs, and the -no-install row
// does.
func TestASinkTheStoreOutranksIsRefusedBeforeTheVendor(t *testing.T) {
	t.Parallel()
	const public = "-public-url=https://crewlet.example.com"
	for _, tc := range []struct {
		name   string
		args   []string
		held   map[string]string
		vendor string
	}{
		{"gitlab provision, a seat's token",
			[]string{"gitlab", "provision", "-admin-token=glpat-admin", public},
			map[string]string{"GITLAB_TOKEN_CEO": "glpat-ceo"}, "GitLab"},
		{"gitlab provision -rotate, the signing secret",
			[]string{"gitlab", "provision", "-admin-token=glpat-admin", public, "-rotate"},
			map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret}, "GitLab"},
		{"github provision -recreate-webhooks",
			[]string{"github", "provision", public, "-recreate-webhooks"},
			map[string]string{"GITHUB_WEBHOOK_SECRET": "a-stored-secret"}, "GitHub"},
		{"jira provision -recreate-webhook",
			[]string{"jira", "provision", public, "-recreate-webhook"},
			map[string]string{"JIRA_WEBHOOK_SECRET": "a-stored-secret"}, "Jira"},
		{"confluence provision -recreate-webhooks",
			[]string{"confluence", "provision", public, "-recreate-webhooks"},
			map[string]string{"CONFLUENCE_WEBHOOK_SECRET": "a-stored-secret"}, "Confluence"},
		{"slack provision, a bot's token",
			[]string{"slack", "provision", public},
			map[string]string{"SLACK_BOT_TOKEN_CEO": "xoxb-ceo"}, "Slack"},
		{"slack provision, a signing secret",
			[]string{"slack", "provision", public},
			map[string]string{"SLACK_SIGNING_SECRET_CEO": "a-stored-secret"}, "Slack"},
		{"slack provision -no-install, a signing secret",
			[]string{"slack", "provision", public, "-no-install"},
			map[string]string{"SLACK_SIGNING_SECRET_CEO": "a-stored-secret"}, "Slack"},
	} {
		for _, route := range outrankRoutes {
			for _, sinkName := range []string{"-print", "-env-file"} {
				t.Run(tc.name+", "+route.name+", "+sinkName, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					envFile := filepath.Join(dir, "minted.env")
					ledger := filepath.Join(dir, "slack-apps.json")
					sink, says := []string{"-print"}, "this run's output (-print)"
					if sinkName == "-env-file" {
						sink, says = []string{"-env-file", envFile}, "the env file "+envFile
					}
					storeArgs, node := route.open(t, maps.Clone(tc.held))
					args := append(slices.Clone(tc.args[:2]),
						companyAt(t, readerCompanyDoc, untouchedVendor(t)))
					args = append(append(args, tc.args[2:]...), sink...)
					args = append(append(args, "-ledger", ledger), storeArgs...)
					if tc.args[0] != "slack" {
						args = slices.DeleteFunc(args, func(arg string) bool {
							return arg == "-ledger" || arg == ledger
						})
					}

					out, errs, err := cli(t, args...)
					if !errors.Is(err, errOutrankedSink) {
						t.Fatalf("err = %v, want the outranked-sink refusal\n%s%s", err, out, errs)
					}
					want := []string{"crewlet " + tc.args[0] + " provision", says,
						route.says(storeArgs, node), "-secret-store",
						"Nothing was sent to " + tc.vendor + "."}
					for name := range tc.held {
						want = append(want, "${"+name+"}")
					}
					for _, w := range want {
						if !strings.Contains(err.Error(), w) {
							t.Errorf("the refusal omits %q: %v", w, err)
						}
					}
					if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 ||
						!strings.Contains(lines[0], "fleet's store") &&
							!strings.Contains(lines[0], "running node") || errs != "" {
						t.Errorf("a refused run printed more than its route line:\n%s%s", out, errs)
					}
					for _, path := range []string{envFile, ledger} {
						if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
							t.Errorf("a refused run wrote %s: %v", path, statErr)
						}
					}
					if node != nil {
						node.mu.Lock()
						stored := len(node.stored)
						node.mu.Unlock()
						if stored != 0 {
							t.Errorf("a refused run wrote %d value(s) through the node", stored)
						}
					}
				})
			}
		}
	}
}

// reachedVendor is a vendor that refuses every request with a 401, and how
// many it has had: a run that reached it was not refused before it.
func reachedVendor(t *testing.T) (string, func() int) {
	t.Helper()
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"401 Unauthorized"}`)
	}))
	t.Cleanup(server.Close)
	return server.URL, func() int {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
}

// A RUN THE STORE DOES NOT OUTRANK GOES ON TO THE VENDOR: one whose store holds
// nothing it can mint into — a webhook secret that resolves, registered as it
// is; a secret it would mint with no hook to register, for want of an address
// or, on GitHub, of a provisioning block naming where; a Slack run under
// -no-install, which installs nothing and so records no bot token; a run whose
// store holds none of its names — one whose sink is the store itself, which
// reads each value back and records over it, and one with no store at all,
// which nothing outranks. No case's run reaches Slack ([unreachableSlack]), so
// a Slack run is seen going on to the step after the store: the deployment
// address it was not given.
//
// Mutation: count a webhook secret as one the run mints whatever its flag,
// or wherever it registers no hook — no address, or no GitHub provisioning
// block — and that GitHub row is refused; count a bot token under -no-install,
// and Slack's is; ask a -secret-store run, and its row is; refuse on a name the
// store does not hold, and the empty store's row is.
func TestARunTheStoreDoesNotOutrankGoesOnToTheVendor(t *testing.T) {
	t.Parallel()
	const public = "-public-url=https://crewlet.example.com"
	signing := map[string]string{"GITLAB_SIGNING_SECRET": storedSigningSecret}
	const githubProvisioning = "    provisioning:\n      org: nimbus\n"
	unprovisioned := strings.Replace(readerCompanyDoc,
		"\"${GITHUB_WEBHOOK_SECRET}\"\n"+githubProvisioning, "\"${GITHUB_WEBHOOK_SECRET}\"\n", 1)
	if unprovisioned == readerCompanyDoc {
		t.Fatal("readerCompanyDoc's github block has no provisioning block to leave out")
	}
	for _, tc := range []struct {
		name  string
		args  []string
		held  map[string]string
		route bool
		// doc is the company the run reads, where it is not readerCompanyDoc.
		doc string
		// stop is what a Slack run stops at past the store: no case's run
		// reaches Slack ([unreachableSlack]), so this is how one is seen to
		// have gone on.
		stop string
	}{
		{name: "github provision, a stored webhook secret it registers as it is",
			args: []string{"github", "provision", "-print", public},
			held: map[string]string{"GITHUB_WEBHOOK_SECRET": "a-stored-secret"}, route: true},
		{name: "github provision -recreate-webhooks with no address to register a hook at",
			args: []string{"github", "provision", "-print", "-recreate-webhooks"},
			held: map[string]string{"GITHUB_WEBHOOK_SECRET": "a-stored-secret"}, route: true},
		{name: "github provision -recreate-webhooks with no provisioning block to register a hook on",
			args: []string{"github", "provision", "-print", public, "-recreate-webhooks"},
			held: map[string]string{"GITHUB_WEBHOOK_SECRET": "a-stored-secret"}, route: true,
			doc: unprovisioned},
		{name: "slack provision -no-install, a bot's token held",
			args: []string{"slack", "provision", "-print", "-no-install"},
			held: map[string]string{"SLACK_BOT_TOKEN_CEO": "xoxb-ceo"}, route: true,
			stop: "no address for this deployment"},
		{name: "gitlab provision, a stored signing secret it reuses",
			args: []string{"gitlab", "provision", "-print", "-admin-token=glpat-admin", public},
			held: signing, route: true},
		{name: "gitlab provision, a store holding none of its names",
			args: []string{"gitlab", "provision", "-env-file", "ENVFILE", "-admin-token=glpat-admin",
				public}, held: map[string]string{"UNRELATED": "x"}, route: true},
		{name: "gitlab provision -secret-store, its seat's token held",
			args: []string{"gitlab", "provision", "-secret-store", "-admin-token=glpat-admin", public},
			held: map[string]string{"GITLAB_TOKEN_CEO": "glpat-ceo"}, route: true},
		{name: "gitlab provision with no store",
			args: []string{"gitlab", "provision", "-print", "-admin-token=glpat-admin", public}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vendor, reached := reachedVendor(t)
			doc := tc.doc
			if doc == "" {
				doc = readerCompanyDoc
			}
			args := append(slices.Clone(tc.args[:2]), companyAt(t, doc, vendor))
			for _, arg := range tc.args[2:] {
				if arg == "ENVFILE" {
					arg = filepath.Join(t.TempDir(), "minted.env")
				}
				args = append(args, arg)
			}
			if tc.route {
				storeArgs, _ := outrankRoutes[0].open(t, maps.Clone(tc.held))
				args = append(args, storeArgs...)
			} else {
				args = append(args, "-config", filepath.Join(t.TempDir(), "absent.yaml"))
			}
			out, errs, err := cli(t, args...)
			if errors.Is(err, errOutrankedSink) {
				t.Fatalf("a run the store does not outrank was refused: %v\n%s%s", err, out, errs)
			}
			if tc.stop != "" {
				if err == nil || !strings.Contains(err.Error(), tc.stop) {
					t.Errorf("err = %v, want the run to go on to %q\n%s%s", err, tc.stop, out, errs)
				}
				return
			}
			if reached() == 0 {
				t.Errorf("the run never reached the vendor: %v\n%s%s", err, out, errs)
			}
		})
	}
}

// A SLACK RUN WHOSE SINK IS NO STORE ONLY LISTS THE STORE: what it needs of it
// is which names it holds, so through a running node it takes the listing and
// reveals nothing, says so on its first line, and goes on where the store holds
// none of its names. With no token for the node it is told of the token a
// listing takes, which a machine token can be; a listing the node refuses is
// shown the node's refusal. Neither is told anything of the Tier A token a
// reveal takes.
//
// Mutation: route it as a run that reveals, and the no-token refusal names a
// reveal; hand a refused listing the reveal's remedy, and that refusal does;
// read its names' values, and the node is asked for them.
func TestASlackRunWhoseSinkIsNoStoreOnlyListsTheStore(t *testing.T) {
	t.Parallel()
	node := newRevealNode(t, map[string]string{"UNRELATED": "x"})
	company := companyAt(t, readerCompanyDoc, untouchedVendor(t))
	args := []string{"slack", "provision", company, "-print",
		"-public-url", "https://crewlet.example.com", "-api", node.server.URL,
		"-config", filepath.Join(t.TempDir(), "absent.yaml"),
		"-ledger", filepath.Join(t.TempDir(), "slack-apps.json")}

	out, errs, err := cliIn(t, tokenEnviron("a-machine-token"), args...)
	if errors.Is(err, errOutrankedSink) {
		t.Fatalf("a run the store holds none of the names of was refused: %v\n%s%s", err, out, errs)
	}
	first, _, _ := strings.Cut(out, "\n")
	if want := "checking which names the running node at " + node.server.URL +
		" holds: this command reads no stored value, so none was revealed."; first != want {
		t.Errorf("the first line is %q, want %q", first, want)
	}
	if asked, gets := node.batches(); len(asked) != 0 || len(gets) != 0 || node.listed() != 1 {
		t.Errorf("listed %d time(s), and asked %v and %v: want the one listing and no reveal",
			node.listed(), asked, gets)
	}

	requests := node.requests()
	_, _, err = cliIn(t, noEnviron, args...)
	if err == nil || !strings.Contains(err.Error(), "an API token or an access token") ||
		strings.Contains(err.Error(), "reveal") {
		t.Errorf("with no token the run was told %v, want the token a listing takes", err)
	}
	if node.requests() != requests {
		t.Error("a run with no token reached the node")
	}

	refusing := newRevealNode(t, nil)
	refusing.refuseListing = http.StatusForbidden
	args[slices.Index(args, "-api")+1] = refusing.server.URL
	_, _, err = cliIn(t, tokenEnviron("a-machine-token"), args...)
	if err == nil || !strings.Contains(err.Error(), "403") ||
		strings.Contains(err.Error(), "reveal") {
		t.Errorf("a refused listing was told %v, want the node's refusal and no reveal", err)
	}
}

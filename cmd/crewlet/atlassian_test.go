package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `crewlet atlassian provision`, driven through run() the way an operator
// types it.
//
// Every case here is a refusal or a report, and none of them reaches
// Atlassian: what is under test is the boundary — which companies this
// command will act on at all, and what it says to the ones it will not.

const atlassianCompanyDoc = `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
integrations:
  jira:
    cloud_id: acme-cloud
    site_url: https://acme.atlassian.net
    token: "${JIRA_ADMIN_TOKEN}"
    email: "${JIRA_ADMIN_EMAIL}"
  confluence:
    cloud_id: acme-cloud
    site_url: https://acme.atlassian.net
    token: "${CONFLUENCE_ADMIN_TOKEN}"
    email: "${CONFLUENCE_ADMIN_EMAIL}"
  atlassian:
    org_id: org-1
roles:
  - name: SWE
    handle: swe
    llm: main
    integrations:
      jira: { project: ENG }
      confluence: { space: ENG }
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ATLASSIAN_EMAIL_SWE}"
        JIRA_API_TOKEN: "${ATLASSIAN_TOKEN_SWE}"
        CONFLUENCE_USERNAME: "${ATLASSIAN_EMAIL_SWE}"
        CONFLUENCE_API_TOKEN: "${ATLASSIAN_TOKEN_SWE}"
  - name: Tech Writer
    handle: writer
    llm: main
    integrations:
      atlassian: { products: [confluence] }
    mcp_env:
      atlassian:
        CONFLUENCE_USERNAME: "${ATLASSIAN_EMAIL_WRITER}"
        CONFLUENCE_API_TOKEN: "${ATLASSIAN_TOKEN_WRITER}"
  - name: PM
    handle: pm
    llm: main
    mcp_env:
      atlassian:
        JIRA_USERNAME: pm@example.com
        JIRA_API_TOKEN: ATATT-managed-by-hand
`

// atlassianCompanyFile writes a company document, optionally with one
// substitution so a case can vary a single line.
func atlassianCompanyFile(t *testing.T, replacements ...string) string {
	t.Helper()
	doc := atlassianCompanyDoc
	for i := 0; i+1 < len(replacements); i += 2 {
		if !strings.Contains(doc, replacements[i]) {
			t.Fatalf("fixture does not contain %q", replacements[i])
		}
		doc = strings.Replace(doc, replacements[i], replacements[i+1], 1)
	}
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// atlassianCmd runs the command with a bootstrap path that does not exist,
// which forces the environment-only resolver — the test machine has no
// secret store, and a run that tried to open one would prompt.
func atlassianCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errs bytes.Buffer
	args = append([]string{"atlassian", "provision"}, args...)
	args = append(args, "-config", filepath.Join(t.TempDir(), "absent.yaml"))
	err := run(args, &out, &errs)
	return out.String(), errs.String(), err
}

func TestAnAtlassianDryRunReportsWithoutReachingAtlassian(t *testing.T) {
	t.Parallel()
	out, errs, err := atlassianCmd(t, atlassianCompanyFile(t), "-dry-run")
	if err != nil {
		t.Fatalf("dry run: %v (%s)", err, errs)
	}
	if !strings.Contains(out, "swe") || !strings.Contains(out, "ATLASSIAN_TOKEN_SWE") {
		t.Errorf("the plan does not name the provisionable seat:\n%s", out)
	}
	// The per-seat product list is in the plan, because it is what the run
	// will BUY: a licence is billable, and an operator has to see the bill
	// before the run rather than after it.
	if !strings.Contains(out, "Jira+Confluence") || !strings.Contains(out, "writer") {
		t.Errorf("the plan does not show what each seat is licensed for:\n%s", out)
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
	if strings.Contains(out, "ATATT-managed-by-hand") {
		t.Errorf("THE REPORT LEAKED A CREDENTIAL:\n%s", out)
	}
}

func TestAnAtlassianDryRunNeedsNoSinkAndNoOrganizationKey(t *testing.T) {
	t.Parallel()
	// It mints nothing, so it has nothing to record — and opening a sink is
	// not free: the -env-file one creates the file, and the -secret-store
	// one probes the store's lock and may reach a running node's API.
	if _, errs, err := atlassianCmd(t, atlassianCompanyFile(t), "-dry-run"); err != nil {
		t.Fatalf("a dry run demanded a sink: %v (%s)", err, errs)
	}
}

func TestARealAtlassianRunWithNoSinkIsRefusedBeforeItTouchesAnything(t *testing.T) {
	t.Parallel()
	// A run with nowhere to put what it mints would create live
	// credentials at Atlassian and print none of them, which is the worst
	// outcome available: every one has to be found and revoked by hand.
	_, _, err := atlassianCmd(t, atlassianCompanyFile(t))
	if err == nil {
		t.Fatal("a run with no sink was accepted")
	}
	for _, flag := range []string{"-secret-store", "-env-file", "-print"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("the refusal does not name %s: %v", flag, err)
		}
	}
}

func TestTwoSinksAreRefusedRatherThanOrdered(t *testing.T) {
	t.Parallel()
	// Writing to two places doubles the number of copies of a live
	// credential, and picking one by precedence would put it somewhere the
	// operator did not ask for.
	_, _, err := atlassianCmd(t, atlassianCompanyFile(t), "-print",
		"-env-file", filepath.Join(t.TempDir(), ".env"))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("err = %v", err)
	}
}

func TestACompanyWithNoAtlassianBlockIsRefusedByName(t *testing.T) {
	t.Parallel()
	company := atlassianCompanyFile(t, "  atlassian:\n    org_id: org-1\n", "")
	_, _, err := atlassianCmd(t, company, "-dry-run")
	if err == nil {
		t.Fatal("a company with no organization block was accepted")
	}
	if !strings.Contains(err.Error(), "integrations.atlassian") {
		t.Errorf("the refusal does not name the block to add: %v", err)
	}
	// And it says where the value comes from, because the org id is not
	// somewhere an operator has ever had to look before.
	if !strings.Contains(err.Error(), "admin.atlassian.com") {
		t.Errorf("the refusal does not say where org_id comes from: %v", err)
	}
}

// atlassianDataCentreDoc is a company on Data Center throughout.
//
// A company that is half Cloud and half Data Center never reaches this
// command at all: the config validator refuses it, because a licence is
// granted on a SITE and one product would be licensed somewhere the other
// cannot reach.
const atlassianDataCentreDoc = `
name: Nimbus
providers:
  llm:
    main:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
integrations:
  jira:
    url: https://jira.internal.example.com
    token: "${JIRA_ADMIN_TOKEN}"
    webhook_secret: "${JIRA_WEBHOOK_SECRET}"
  atlassian:
    org_id: org-1
roles:
  - name: SWE
    handle: swe
    llm: main
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ATLASSIAN_EMAIL_SWE}"
        JIRA_API_TOKEN: "${ATLASSIAN_TOKEN_SWE}"
`

func TestADataCentreCompanyIsRefusedAndPointedAtTheCommandThatServesIt(t *testing.T) {
	t.Parallel()
	// Service accounts are a Cloud-only capability: there is no
	// organization admin API on Data Center, and a personal access token
	// can only be minted for the calling user. Two commands that look
	// interchangeable and are not is worse than one that says no.
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(atlassianDataCentreDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := atlassianCmd(t, path, "-dry-run")
	if err == nil {
		t.Fatal("a Data Center company was accepted")
	}
	if !strings.Contains(err.Error(), "integrations.jira.url") {
		t.Errorf("the refusal does not name the field that decided it: %v", err)
	}
	if !strings.Contains(err.Error(), "crewlet jira provision") {
		t.Errorf("the refusal does not name where those operators go: %v", err)
	}
}

func TestACompanyHalfCloudAndHalfDataCentreIsRefusedByTheValidator(t *testing.T) {
	t.Parallel()
	// The other half of the same rule, and it fires EARLIER — at load,
	// before this command has resolved anything — because it is a config
	// mistake rather than a deployment this command cannot serve.
	company := atlassianCompanyFile(t,
		"  jira:\n    cloud_id: acme-cloud",
		"  jira:\n    url: https://jira.internal.example.com\n    webhook_secret: \"${S}\"")
	_, _, err := atlassianCmd(t, company, "-dry-run")
	if err == nil {
		t.Fatal("a half-Cloud company was accepted")
	}
	if !strings.Contains(err.Error(), "half Cloud and half Data Center") {
		t.Errorf("err = %v", err)
	}
}

func TestAnUnresolvedOrganizationIDIsRefusedRatherThanSentEmpty(t *testing.T) {
	t.Parallel()
	// It is the organization every service account is created in, so an
	// empty one would create accounts somewhere nobody named — or, more
	// likely, fail with a 404 that says nothing at all about the missing
	// variable. The reference below is deliberately one nothing exports.
	company := atlassianCompanyFile(t,
		"org_id: org-1", `org_id: "${ATLASSIAN_ORG_ID_NOTHING_EXPORTS}"`)
	_, _, err := atlassianCmd(t, company, "-dry-run")
	if err == nil || !strings.Contains(err.Error(), "org_id") {
		t.Fatalf("err = %v", err)
	}
}

func TestARunWithNoOrganizationKeyNamesTheVariableAndItsShape(t *testing.T) {
	// NOT PARALLEL: it clears the operator's own credential out of the
	// environment, which every other case in this file shares.
	// THE WALL EVERY FIRST RUN HITS. The key has to be created WITHOUT
	// scopes, and a scoped one is refused with a flat 403 that reads as
	// "you do not have permission".
	t.Setenv("ATLASSIAN_ORG_API_KEY", "")
	t.Setenv("ATLASSIAN_ADMIN_TOKEN", "")
	_, _, err := atlassianCmd(t, atlassianCompanyFile(t), "-print")
	if err == nil {
		t.Fatal("a run with no organization key was accepted")
	}
	if !strings.Contains(err.Error(), "ATLASSIAN_ORG_API_KEY") {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
	if !strings.Contains(err.Error(), "WITHOUT scopes") {
		t.Errorf("the refusal does not name the key's shape: %v", err)
	}
	// It also says why the run cannot bootstrap itself from the seats'
	// own credentials, which is the obvious question.
	if !strings.Contains(err.Error(), "MINTS") {
		t.Errorf("the refusal does not explain why a seat token will not do: %v", err)
	}
}

func TestTheCommandNeedsExactlyOneCompanyDocument(t *testing.T) {
	t.Parallel()
	if _, _, err := atlassianCmd(t); err == nil {
		t.Error("a run with no company document was accepted")
	}
	if _, _, err := atlassianCmd(t, atlassianCompanyFile(t), "another.yaml"); err == nil {
		t.Error("a run naming two documents was accepted")
	}
}

func TestADryRunDescribesTheRunTheFlagsAskedFor(t *testing.T) {
	t.Parallel()
	// -handles is passed to the run, so a plan printed un-narrowed
	// describes a run that will not happen — and a dry run that lists every
	// seat before provisioning one is the opposite of what the flag
	// promises. A handle that matched nothing used to print the full plan,
	// an empty result and exit 0, which reads as a healthy no-op on a seat
	// still holding its old credential.
	dir := t.TempDir()
	path := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(path, []byte(twoSeatAtlassianCompany), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	err := run([]string{"atlassian", "provision", path, "-dry-run",
		"-handles", "agent-one,nobody"}, &out, &errs)
	if err != nil {
		t.Fatalf("%v (%s)", err, errs.String())
	}
	got := out.String()
	if !strings.Contains(got, "1 seat(s) to provision") {
		t.Errorf("the plan was not narrowed to -handles:\n%s", got)
	}
	if strings.Contains(got, "agent-two") {
		t.Errorf("a seat the run will not touch is listed:\n%s", got)
	}
	if !strings.Contains(got, `named "nobody"`) {
		t.Errorf("a handle that matched no seat went unreported:\n%s", got)
	}
}

func TestADryRunNamesWhatDecommissionWouldDelete(t *testing.T) {
	t.Parallel()
	// -decommission is the one irreversible verb here and the seat plan
	// does not describe it: the dry run printed the same words whether the
	// real run would delete nothing or every account in the organization.
	dir := t.TempDir()
	path := filepath.Join(dir, "company.yaml")
	if err := os.WriteFile(path, []byte(twoSeatAtlassianCompany), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if err := run([]string{"atlassian", "provision", path, "-dry-run", "-decommission"},
		&out, &errs); err != nil {
		t.Fatalf("%v (%s)", err, errs.String())
	}
	got := out.String()
	if !strings.Contains(got, "will KEEP") || !strings.Contains(got, "no restore") {
		t.Fatalf("the sweep is invisible in a dry run:\n%s", got)
	}
	// The keep-set is every AGENT SEAT the chart has, not the seats this
	// run provisions — that difference is the whole safety property.
	for _, name := range []string{"crewlet agent one", "crewlet agent two"} {
		if !strings.Contains(got, name) {
			t.Errorf("the keep-set omits %q:\n%s", name, got)
		}
	}
	// And a run without the flag says nothing about it.
	var plain bytes.Buffer
	if err := run([]string{"atlassian", "provision", path, "-dry-run"}, &plain, &errs); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "will KEEP") {
		t.Error("the keep-set printed for a run that is not sweeping")
	}
}

// twoSeatAtlassianCompany is the smallest chart with two provisionable
// Atlassian seats, for the narrowing and sweep-preview cases.
const twoSeatAtlassianCompany = `name: Acme
integrations:
  atlassian:
    org_id: org-1
  jira:
    cloud_id: cloud-1
    token: "org-read-token"
roles:
  - name: Agent One
    goal: one
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ATLASSIAN_EMAIL_ONE}"
        JIRA_API_TOKEN: "${ATLASSIAN_TOKEN_ONE}"
  - name: Agent Two
    goal: two
    mcp_env:
      atlassian:
        JIRA_USERNAME: "${ATLASSIAN_EMAIL_TWO}"
        JIRA_API_TOKEN: "${ATLASSIAN_TOKEN_TWO}"
`

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/plane"
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

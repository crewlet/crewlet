package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

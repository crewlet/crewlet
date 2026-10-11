package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// A SEAT GITHUB NEVER ANSWERED FOR IS NOT PRINTED AS REFUSED.
//
// The run returns an error for it and the command prints the seats beside
// that error. Listed under REFUSED, a dial that failed read as GitHub turning
// the credential down, and sent the operator to look at an account over a
// request that never got there — the same misfiling the reconcile itself
// stopped making.
func TestASeatGitHubNeverAnsweredForIsNotPrintedAsRefused(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printGitHubResult(&out, &github.Result{
		Login: "crewlet-engine",
		Seats: []github.SeatIdentity{
			{
				Handle: "analyst", Outcome: github.IdentityUnanswered,
				Reason: "github: /user: dial tcp: connect: connection refused",
			},
			{
				Handle: "ops", Outcome: github.IdentityRefused,
				Reason: "github: GET /user: 401: Bad credentials",
			},
		},
	})
	lineOf := func(handle string) string {
		for line := range strings.SplitSeq(out.String(), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), handle+" ") {
				return line
			}
		}
		t.Fatalf("no line for %s in:\n%s", handle, out.String())
		return ""
	}
	if line := lineOf("analyst"); strings.Contains(line, "REFUSED") ||
		!strings.Contains(line, "NO ANSWER") {
		t.Errorf("the unanswered seat printed as %q", line)
	}
	if line := lineOf("ops"); !strings.Contains(line, "REFUSED") {
		t.Errorf("the refused seat printed as %q", line)
	}
}

// AND A HOOK TARGET GITHUB NEVER ANSWERED ABOUT IS NEITHER SKIPPED NOR NOT
// HOOKED. "Skipped" is what an archived repository prints and "NOT HOOKED" a
// refusal somebody at GitHub has to widen; this one is a request that never
// got there, beside the error the run exits with.
func TestAHookGitHubNeverAnsweredAboutIsNotPrintedAsSkippedOrBlocked(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printGitHubResult(&out, &github.Result{
		Login: "crewlet-engine",
		Hooks: []github.HookState{
			{
				Target:  github.Target{Owner: "acme", Repo: "web"},
				Outcome: github.HookUnanswered,
				Detail:  "github: /repos/acme/web: EOF",
			},
			{
				Target:  github.Target{Owner: "acme", Repo: "old"},
				Outcome: github.HookSkipped,
				Detail:  "archived, so it emits no events — no hook registered",
			},
		},
	})
	var web, old string
	for line := range strings.SplitSeq(out.String(), "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "acme/web "):
			web = line
		case strings.HasPrefix(strings.TrimSpace(line), "acme/old "):
			old = line
		}
	}
	if !strings.Contains(web, "NO ANSWER") || strings.Contains(web, "skipped") ||
		strings.Contains(web, "NOT HOOKED") {
		t.Errorf("the unanswered target printed as %q", web)
	}
	if !strings.Contains(old, "skipped") {
		t.Errorf("the archived target printed as %q", old)
	}
}

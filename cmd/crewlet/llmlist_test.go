package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// `crewlet llm list` NAMES AN ENTRY SIGNED IN BY ITS OWN ENV AS `env`, in the
// SIGN-IN column beside the other routes' words, which are unchanged: the
// column is parsed (crewlet-operator reads it), and an env-mode entry, whose
// cloud switch or gateway token is its whole sign-in, read `none` before the
// mode existed. The first route the doctor's sign-in line names is the word,
// so the two commands cannot disagree.
//
// Mutation: report the env-mode route under another word in signIn, and the
// env row is red; reorder or rename the columns, and every row is.
func TestLLMListNamesAnEnvSignInAsEnv(t *testing.T) {
	t.Parallel()
	entry := func(key, lines string) string {
		return fmt.Sprintf("    %s:\n      agent: claude-code\n      model: sonnet\n      state_dir: %q\n%s", key, t.TempDir(), lines)
	}
	flags := doctorCompany(t,
		entry("gateway", "      auth: {mode: env}\n"+
			"      env: {ANTHROPIC_AUTH_TOKEN: gw-test-token, ANTHROPIC_BASE_URL: \"https://gateway.example\"}\n")+
			entry("plan", "      auth: {token: fake-headless-token}\n")+
			entry("nothing", "      auth: {mode: subscription}\n"))
	var out, notes bytes.Buffer
	if err := runLLM(noEnviron, append([]string{"list"}, flags...), &out, &notes); err != nil {
		t.Fatalf("llm list: %v\n%s%s", err, out.String(), notes.String())
	}
	rows := map[string][]string{}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "PROVIDER AGENT MODEL SIGN-IN STATE DIR" {
		t.Fatalf("header = %q", lines[0])
	}
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		rows[fields[0]] = fields
	}
	for key, want := range map[string]string{"gateway": "env", "plan": "token", "nothing": "none"} {
		row, ok := rows[key]
		if !ok {
			t.Errorf("no row for %s:\n%s", key, out.String())
			continue
		}
		if got := row[3]; got != want {
			t.Errorf("%s: SIGN-IN = %q, want %q:\n%s", key, got, want, out.String())
		}
	}
}
